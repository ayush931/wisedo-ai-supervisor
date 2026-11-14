//go:build integration

package knowledge

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/google/uuid"
)

// WIS-270 Decision "reason:1..2000chars". A database populated at the baseline before the
// reason migration (reasons capped at 1000 characters) upgrades through it as a non-superuser
// owner, keeps every existing review, revocation and resolution row byte-for-byte, and then
// stores 2000 but refuses 2001 characters, also at the database for a direct runtime-role
// insert (SQLSTATE 23514).
func TestPostgresDecisionReasonUpgradeTo2000Characters(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: real PostgreSQL URLs required")
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	name := "reason_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	secret := make([]byte, 24)
	if _, err = rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	password := hex.EncodeToString(secret)
	if _, err = admin.ExecContext(ctx, `CREATE ROLE `+name+` LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '`+password+`'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 60*time.Second)
		defer stop()
		if _, e := admin.ExecContext(cleanup, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); e != nil {
			t.Error(e)
		}
		if _, e := admin.ExecContext(cleanup, `DROP ROLE IF EXISTS `+name); e != nil {
			t.Error(e)
		}
	})
	if _, err = admin.ExecContext(ctx, `CREATE DATABASE `+name+` OWNER `+name); err != nil {
		t.Fatal(err)
	}
	connect := func(raw string, asOwner bool) *sql.DB {
		t.Helper()
		u, e := url.Parse(raw)
		if e != nil {
			t.Fatal(e)
		}
		u.Path = "/" + name
		if asOwner {
			u.User = url.UserPassword(name, password)
		}
		db, e := sql.Open("pgx", u.String())
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	owner, runtime := connect(adminURL, true), connect(runtimeURL, false)

	// The baseline is every migration before the reason migration, found by file name, so
	// migrations appended after it never affect this test. A migration inserted before it only
	// requires the raw seed SQL below to stay valid at the new baseline: every pre-upgrade row is
	// written with raw SQL (no service), using only tables and columns that exist at the baseline.
	files, err := filepath.Glob("../migrate/[0-9][0-9][0-9]_*.sql")
	if err != nil {
		t.Fatal(err)
	}
	reasonVersion := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_knowledge_decision_reason_2000.sql") {
			if reasonVersion, err = strconv.Atoi(filepath.Base(path)[:3]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if reasonVersion < 2 {
		t.Fatalf("decision reason migration not found in %v", files)
	}
	tx, err := owner.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	must := func(q string, args ...any) {
		t.Helper()
		if _, e := tx.ExecContext(ctx, q, args...); e != nil {
			tx.Rollback()
			t.Fatal(e)
		}
	}
	must(`CREATE TABLE public.supervisor_schema_versions(version integer PRIMARY KEY,sha256 text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now()); GRANT SELECT ON public.supervisor_schema_versions TO supervisor_runtime`)
	for i, path := range files[:reasonVersion-1] {
		// Apply compares recorded versions positionally, so the baseline must be exactly 1..N.
		if version, e := strconv.Atoi(filepath.Base(path)[:3]); e != nil || version != i+1 {
			tx.Rollback()
			t.Fatalf("baseline migration %s is not version %d", path, i+1)
		}
		body, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		must(string(body))
		// 001 (a pg_dump body) turns row_security off for the session. The tables are empty, so
		// RLS-filtered validation scans are exact; Apply's guard only matters once rows exist.
		must(`SET row_security = on`)
		sum := sha256.Sum256(body)
		must(`INSERT INTO public.supervisor_schema_versions(version,sha256) VALUES($1,$2)`, i+1, hex.EncodeToString(sum[:]))
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}

	sc := Scope{uuid.NewString(), uuid.NewString()}
	sum := sha256.Sum256([]byte(uuid.NewString()))
	actor := Actor{"https://reason-upgrade.invalid", uuid.NewString(), hex.EncodeToString(sum[:])}
	legacy := legacyEvidence{t, ctx, owner, actor}
	run := func(q string, args ...any) func(*sql.Tx) error {
		return func(x *sql.Tx) error { _, e := x.ExecContext(ctx, q, args...); return e }
	}
	legacy.exec(sc,
		run(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'reason upgrade')`, sc.TenantID),
		run(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'reason upgrade')`, sc.TenantID, sc.SiteID),
		run(`INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'fixture',now(),now(),now()+interval '1 hour',now())`, actor.SessionHash, actor.Issuer, actor.Subject),
		run(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, sc.TenantID, sc.SiteID, actor.Issuer, actor.Subject))
	source, entity, authority := uuid.NewString(), uuid.NewString(), uuid.NewString()
	legacy.source(sc, source, "manual", "Synthetic process evidence", 1)
	legacy.exec(sc,
		run(`INSERT INTO supervisor.evidence_authority_heads(tenant_id,site_id,source_id,current_revision) VALUES($1,$2,$3,1)`, sc.TenantID, sc.SiteID, source),
		run(`INSERT INTO supervisor.evidence_authority_decisions(tenant_id,site_id,source_id,revision,decision_id,source_revision,source_sha256,source_access_epoch,verdict,reason,actor_issuer,actor_subject,auth_time)
 SELECT r.tenant_id,r.site_id,r.source_id,1,$4,r.revision,r.sha256,h.access_epoch,'designate','Owner validated source',$5,$6,now()
 FROM supervisor.evidence_revisions r JOIN supervisor.evidence_heads h USING(tenant_id,site_id,source_id) WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.source_id=$3 AND r.revision=1`, sc.TenantID, sc.SiteID, source, authority, actor.Issuer, actor.Subject),
		run(`INSERT INTO supervisor.knowledge_entities(tenant_id,site_id,id,entity_type,payload,source_id,source_revision,actor_issuer,actor_subject)
 VALUES($1,$2,$3,'operation',jsonb_build_object('entity_id',$3::uuid::text,'type','operation','name','Reason operation','source_id',$4::uuid::text,'source_revision',1),$4,1,$5,$6)`, sc.TenantID, sc.SiteID, entity, source, actor.Issuer, actor.Subject),
		run(`INSERT INTO supervisor.knowledge_entity_heads(tenant_id,site_id,id,current_revision) VALUES($1,$2,$3,1)`, sc.TenantID, sc.SiteID, entity),
		run(`INSERT INTO supervisor.knowledge_entity_revisions(tenant_id,site_id,id,revision,entity_type,payload,source_id,source_revision,actor_issuer,actor_subject)
 SELECT tenant_id,site_id,id,1,entity_type,payload,source_id,source_revision,actor_issuer,actor_subject FROM supervisor.knowledge_entities WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, entity))
	span := legacy.span(sc, source, 0, 9)
	claim := func() string {
		t.Helper()
		id := uuid.NewString()
		legacy.exec(sc,
			run(`INSERT INTO supervisor.knowledge_claim_heads(tenant_id,site_id,id,current_revision) VALUES($1,$2,$3,1)`, sc.TenantID, sc.SiteID, id),
			run(`INSERT INTO supervisor.knowledge_claim_revisions(tenant_id,site_id,claim_id,revision,subject_id,predicate,payload,actor_issuer,actor_subject)
 VALUES($1,$2,$3,1,$4,'actual_step',jsonb_build_object('claim_id',$3::uuid::text,'subject_id',$4::uuid::text,'predicate','actual_step','subject_revision',1,'source_span_ids',jsonb_build_array($7::uuid::text)),$5,$6)`, sc.TenantID, sc.SiteID, id, entity, actor.Issuer, actor.Subject, span),
			run(`INSERT INTO supervisor.knowledge_claim_entity_refs(tenant_id,site_id,claim_id,claim_revision,subject_id,subject_revision) VALUES($1,$2,$3,1,$4,1)`, sc.TenantID, sc.SiteID, id, entity),
			run(`INSERT INTO supervisor.knowledge_claim_spans(tenant_id,site_id,claim_id,claim_revision,span_id,source_id,source_revision) VALUES($1,$2,$3,1,$4,$5,1)`, sc.TenantID, sc.SiteID, id, span, source))
		return id
	}
	// 1000 three-byte characters: the old maximum, 3000 bytes.
	legacy1000 := strings.Repeat("界", 1000)
	rejected, approved, review := claim(), claim(), uuid.NewString()
	left, right := claim(), claim()
	if right < left {
		left, right = right, left
	}
	conflict := uuid.NewString()
	legacy.exec(sc,
		run(`INSERT INTO supervisor.knowledge_claim_reviews(tenant_id,site_id,review_id,claim_id,claim_revision,verdict,reason,actor_issuer,actor_subject,auth_time) VALUES($1,$2,gen_random_uuid(),$3,1,'reject',$4,$5,$6,now())`, sc.TenantID, sc.SiteID, rejected, legacy1000, actor.Issuer, actor.Subject),
		run(`INSERT INTO supervisor.knowledge_claim_reviews(tenant_id,site_id,review_id,claim_id,claim_revision,verdict,reason,actor_issuer,actor_subject,auth_time,authority_decision_id,authority_source_id,authority_source_revision,authority_revision) VALUES($1,$2,$3,$4,1,'approve','Approved before upgrade',$5,$6,now(),$7,$8,1,1)`, sc.TenantID, sc.SiteID, review, approved, actor.Issuer, actor.Subject, authority, source),
		run(`INSERT INTO supervisor.knowledge_review_revocations(tenant_id,site_id,revocation_id,review_id,claim_id,claim_revision,reason,actor_issuer,actor_subject,auth_time) VALUES($1,$2,gen_random_uuid(),$3,$4,1,$5,$6,$7,now())`, sc.TenantID, sc.SiteID, review, approved, legacy1000, actor.Issuer, actor.Subject),
		run(`INSERT INTO supervisor.knowledge_conflicts(tenant_id,site_id,id,left_claim_id,left_revision,right_claim_id,right_revision,kind,effective_from) VALUES($1,$2,$3,$4,1,$5,1,'text_review','2026-09-27T00:00:00Z')`, sc.TenantID, sc.SiteID, conflict, left, right),
		run(`INSERT INTO supervisor.knowledge_resolution_heads(tenant_id,site_id,conflict_id,current_revision) VALUES($1,$2,$3,1)`, sc.TenantID, sc.SiteID, conflict),
		run(`INSERT INTO supervisor.knowledge_resolution_decisions(tenant_id,site_id,conflict_id,revision,resolution_id,decision,reason,actor_issuer,actor_subject,auth_time) VALUES($1,$2,$3,1,gen_random_uuid(),'neither',$4,$5,$6,now())`, sc.TenantID, sc.SiteID, conflict, legacy1000, actor.Issuer, actor.Subject))

	// The baseline database still refuses what the new Go limit admits, so the upgrade is what
	// changes it: an otherwise valid next resolution revision with 1001 characters hits the CHECK.
	sqlState := func(e error) string {
		var state interface{ SQLState() string }
		if errors.As(e, &state) {
			return state.SQLState()
		}
		return ""
	}
	{
		x, e := owner.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = x.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); e != nil {
			t.Fatal(e)
		}
		_, e = x.ExecContext(ctx, `INSERT INTO supervisor.knowledge_resolution_decisions(tenant_id,site_id,conflict_id,revision,resolution_id,decision,reason,actor_issuer,actor_subject,auth_time) VALUES($1,$2,$3,2,gen_random_uuid(),'withdraw',$4,$5,$6,now())`, sc.TenantID, sc.SiteID, conflict, legacy1000+"界", actor.Issuer, actor.Subject)
		x.Rollback()
		if sqlState(e) != "23514" {
			t.Fatalf("baseline database accepted or misreported a 1001-character reason: %v", e)
		}
	}

	type row struct{ table, reason string }
	snapshot := func() map[string]row {
		t.Helper()
		out := map[string]row{}
		x, e := runtime.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer x.Rollback()
		if _, e = x.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); e != nil {
			t.Fatal(e)
		}
		rows, e := x.QueryContext(ctx, `SELECT 'review',review_id::text,reason FROM supervisor.knowledge_claim_reviews
 UNION ALL SELECT 'revocation',revocation_id::text,reason FROM supervisor.knowledge_review_revocations
 UNION ALL SELECT 'resolution',resolution_id::text,reason FROM supervisor.knowledge_resolution_decisions`)
		if e != nil {
			t.Fatal(e)
		}
		defer rows.Close()
		for rows.Next() {
			var r row
			var id string
			if e = rows.Scan(&r.table, &id, &r.reason); e != nil {
				t.Fatal(e)
			}
			out[id] = r
		}
		if e = rows.Err(); e != nil {
			t.Fatal(e)
		}
		return out
	}
	before := snapshot()
	if len(before) != 4 {
		t.Fatalf("populated baseline fixture has %d decision rows, want 2 reviews, 1 revocation, 1 resolution", len(before))
	}

	if err = migrate.Apply(ctx, owner); err != nil {
		t.Fatalf("populated baseline upgrade: %v", err)
	}
	if err = migrate.Apply(ctx, owner); err != nil {
		t.Fatalf("repeat upgrade: %v", err)
	}
	if err = migrate.Check(ctx, owner); err != nil {
		t.Fatal(err)
	}
	after := snapshot()
	if len(after) != len(before) {
		t.Fatalf("upgrade changed row count %d -> %d", len(before), len(after))
	}
	for id, want := range before {
		if after[id] != want {
			t.Fatalf("upgrade changed %s %s", want.table, id)
		}
	}
	var unforced int
	if err = owner.QueryRowContext(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='supervisor' AND c.relname IN ('knowledge_claim_reviews','knowledge_review_revocations','knowledge_resolution_decisions') AND c.relrowsecurity AND c.relforcerowsecurity`).Scan(&unforced); err != nil || unforced != 3 {
		t.Fatalf("forced RLS not restored on decision tables: %d %v", unforced, err)
	}

	// After the upgrade the schema is current, so the services build a fresh conflict and store
	// the 2000-character maximum on it; 2001 characters are refused (Go validation or CHECK).
	service, ev := New(runtime), evidence.New(runtime)
	ea := evidence.Actor{Issuer: actor.Issuer, Subject: actor.Subject, SessionHash: actor.SessionHash}
	es := evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}
	fresh := evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Reason upgrade source", Text: "Synthetic process evidence"}
	if _, err = ev.CreateOrRevise(ctx, ea, es, fresh, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	freshSpan, err := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: fresh.SourceID, Revision: 1, Start: 0, End: 9}, uuid.NewString(), 1)
	if err != nil {
		t.Fatal(err)
	}
	contested := uuid.NewString()
	if _, err = service.CreateEntity(ctx, actor, sc, EntityInput{EntityID: contested, Type: "operation", Name: "Reason operation", SourceID: fresh.SourceID, SourceRevision: 1}, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	var first string
	for _, value := range []string{`"First instruction"`, `"Second instruction"`} {
		in := ClaimInput{ClaimID: uuid.NewString(), SubjectID: contested, SubjectRevision: 1, Predicate: "actual_step", Literal: &Literal{Type: "string", Value: json.RawMessage(value)}, EffectiveFrom: "2026-09-27T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{freshSpan.ID}}
		if _, err = service.CreateClaim(ctx, actor, sc, in, uuid.NewString(), 0); err != nil {
			t.Fatal(err)
		}
		if first == "" {
			first = in.ClaimID
		}
	}
	page, err := service.ListConflicts(ctx, actor, sc, first, 1, 100, "")
	if err != nil || len(page.Conflicts) != 1 {
		t.Fatalf("conflict fixture=%+v %v", page, err)
	}
	maximum := strings.Repeat("界", 2000)
	if _, err = service.DecideResolution(ctx, actor, sc, page.Conflicts[0].ID, ResolutionInput{Decision: "neither", Reason: maximum + "界"}, uuid.NewString(), 0); err == nil {
		t.Fatal("service stored a 2001-character resolution reason")
	}
	if _, err = service.DecideResolution(ctx, actor, sc, page.Conflicts[0].ID, ResolutionInput{Decision: "neither", Reason: maximum}, uuid.NewString(), 0); err != nil {
		t.Fatalf("2000-character resolution reason after upgrade: %v", err)
	}

	// The database itself enforces 1..2000 characters for a direct runtime insert. 2000
	// characters pass the CHECK and reach the dangling foreign key (23503); 2001 and the empty
	// string stop at the CHECK (23514).
	inserts := map[string]string{
		"knowledge_claim_reviews":        `INSERT INTO supervisor.knowledge_claim_reviews(tenant_id,site_id,review_id,claim_id,claim_revision,verdict,reason,actor_issuer,actor_subject,auth_time) VALUES($1,$2,gen_random_uuid(),gen_random_uuid(),1,'reject',$3,'https://reason-upgrade.invalid','direct',now())`,
		"knowledge_review_revocations":   `INSERT INTO supervisor.knowledge_review_revocations(tenant_id,site_id,revocation_id,review_id,claim_id,claim_revision,reason,actor_issuer,actor_subject,auth_time) VALUES($1,$2,gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),1,$3,'https://reason-upgrade.invalid','direct',now())`,
		"knowledge_resolution_decisions": `INSERT INTO supervisor.knowledge_resolution_decisions(tenant_id,site_id,conflict_id,revision,resolution_id,decision,reason,actor_issuer,actor_subject,auth_time) VALUES($1,$2,gen_random_uuid(),1,gen_random_uuid(),'neither',$3,'https://reason-upgrade.invalid','direct',now())`,
	}
	for table, insert := range inserts {
		for _, c := range []struct {
			reason, state string
		}{{maximum, "23503"}, {maximum + "界", "23514"}, {"", "23514"}} {
			x, e := runtime.BeginTx(ctx, nil)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = x.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); e != nil {
				t.Fatal(e)
			}
			if _, e = x.ExecContext(ctx, insert, sc.TenantID, sc.SiteID, c.reason); e == nil {
				e = x.Commit()
			} else {
				x.Rollback()
			}
			var state interface{ SQLState() string }
			if !errors.As(e, &state) || state.SQLState() != c.state {
				t.Fatalf("%s reason of %d characters: want SQLSTATE %s, got %v", table, len([]rune(c.reason)), c.state, e)
			}
		}
	}
}
