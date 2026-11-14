//go:build integration

package knowledge

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/google/uuid"
)

func TestPostgresKnowledgeInventoryRevision(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("real PostgreSQL URLs required")
	}
	control, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })
	name := "inventory_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	secret := make([]byte, 24)
	if _, err = rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	password := hex.EncodeToString(secret)
	if _, err = control.ExecContext(ctx, `CREATE ROLE `+name+` LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '`+password+`'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 60*time.Second)
		defer stop()
		if _, e := control.ExecContext(cleanup, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); e != nil {
			t.Error(e)
		}
		if _, e := control.ExecContext(cleanup, `DROP ROLE IF EXISTS `+name); e != nil {
			t.Error(e)
		}
	})
	if _, err = control.ExecContext(ctx, `CREATE DATABASE `+name+` OWNER `+name); err != nil {
		t.Fatal(err)
	}
	connect := func(raw string, isOwner bool) *sql.DB {
		t.Helper()
		u, e := url.Parse(raw)
		if e != nil {
			t.Fatal(e)
		}
		u.Path = "/" + name
		if isOwner {
			u.User = url.UserPassword(name, password)
		}
		db, e := sql.Open("pgx", u.String())
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	owner, runtime, admin := connect(adminURL, true), connect(runtimeURL, false), connect(adminURL, false)
	var super, bypass bool
	if err = owner.QueryRowContext(ctx, `SELECT rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&super, &bypass); err != nil || super || bypass {
		t.Fatalf("restricted migration owner=%t/%t %v", super, bypass, err)
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
	must(`CREATE TABLE public.supervisor_schema_versions(version integer PRIMARY KEY,sha256 text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())`)
	for version := 1; version <= 12; version++ {
		files, e := filepath.Glob(fmt.Sprintf("../migrate/%03d_*.sql", version))
		if e != nil || len(files) != 1 {
			t.Fatalf("legacy migration%d=%v %v", version, files, e)
		}
		body, e := os.ReadFile(files[0])
		if e != nil {
			t.Fatal(e)
		}
		must(string(body))
		sum := sha256.Sum256(body)
		must(`INSERT INTO public.supervisor_schema_versions(version,sha256) VALUES($1,$2)`, version, hex.EncodeToString(sum[:]))
	}
	must(`SET row_security=on`)
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	actor := Actor{"https://inventory.invalid", uuid.NewString(), strings.ReplaceAll(uuid.NewString(), "-", "") + strings.ReplaceAll(uuid.NewString(), "-", "")}
	if _, err = owner.ExecContext(ctx, `INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'fixture',now(),now(),now()+interval '1 hour',now())`, actor.SessionHash, actor.Issuer, actor.Subject); err != nil {
		t.Fatal(err)
	}
	sc, foreign := Scope{uuid.NewString(), uuid.NewString()}, Scope{uuid.NewString(), uuid.NewString()}
	seedScope := func(s Scope) {
		t.Helper()
		tx, err = owner.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		must(`SELECT set_config('supervisor.tenant_id',$1,true)`, s.TenantID)
		must(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'inventory tenant')`, s.TenantID)
		must(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'inventory site')`, s.TenantID, s.SiteID)
		must(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, s.TenantID, s.SiteID, actor.Issuer, actor.Subject)
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	ev, service := evidence.New(runtime), New(runtime)
	ea := evidence.Actor{Issuer: actor.Issuer, Subject: actor.Subject, SessionHash: actor.SessionHash}
	source := evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Inventory source", Text: "Original synthetic process"}
	for _, s := range []Scope{sc, foreign} {
		seedScope(s)
		in := source
		if s == foreign {
			in.SourceID = uuid.NewString()
		}
		legacyEvidence{t, ctx, owner, actor}.source(s, in.SourceID, in.Kind, in.Text, 1) // pre-039 schema
	}
	if err = migrate.Apply(ctx, owner); err != nil {
		t.Fatalf("populated restricted-owner upgrade=%v", err)
	}
	if err = migrate.Apply(ctx, owner); err != nil {
		t.Fatal(err)
	}
	epoch := func(s Scope) int64 {
		t.Helper()
		out, e := service.ReadInventory(ctx, actor, s)
		if e != nil || out.SchemaVersion != 1 {
			t.Fatalf("inventory=%+v %v", out, e)
		}
		return out.Revision
	}
	initial := epoch(sc)
	otherInitial := epoch(foreign)
	if initial < 0 || otherInitial < 0 || epoch(sc) != initial {
		t.Fatal("inventory baseline unstable")
	}
	fresh := Scope{uuid.NewString(), uuid.NewString()}
	seedScope(fresh)
	if got := epoch(fresh); got != 0 {
		t.Fatalf("new empty site revision=%d want0", got)
	}
	es := evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}
	changed := func(name string, operation func() error) {
		t.Helper()
		before := epoch(sc)
		if e := operation(); e != nil {
			t.Fatalf("%s: %v", name, e)
		}
		after := epoch(sc)
		if after <= before {
			t.Fatalf("%s did not advance revision: %d -> %d", name, before, after)
		}
		if epoch(foreign) != otherInitial {
			t.Fatalf("%s changed other tenant", name)
		}
	}
	stable := func(name string, operation func() error) {
		t.Helper()
		before := epoch(sc)
		if e := operation(); e != nil {
			t.Fatalf("%s: %v", name, e)
		}
		if after := epoch(sc); after != before {
			t.Fatalf("%s changed epoch: %d -> %d", name, before, after)
		}
	}
	entity := EntityInput{EntityID: uuid.NewString(), Type: "operation", Name: "Inventory operation", SourceID: source.SourceID, SourceRevision: 1}
	entityKey := uuid.NewString()
	changed("entity creation", func() error { _, e := service.CreateEntity(ctx, actor, sc, entity, entityKey, 0); return e })
	stable("entity replay", func() error { _, e := service.CreateEntity(ctx, actor, sc, entity, entityKey, 0); return e })
	stable("entity read", func() error { _, e := service.ReadEntity(ctx, actor, sc, entity.EntityID, 1); return e })
	changed("entity revision", func() error {
		entity.Name = "Revised operation"
		_, e := service.CreateEntity(ctx, actor, sc, entity, uuid.NewString(), 1)
		return e
	})
	var span evidence.Span
	changed("source span", func() error {
		var e error
		span, e = ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: source.SourceID, Revision: 1, Start: 0, End: 9}, uuid.NewString(), 1)
		return e
	})
	claim := ClaimInput{ClaimID: uuid.NewString(), SubjectID: entity.EntityID, SubjectRevision: 2, Predicate: "actual_step", Literal: &Literal{Type: "string", Value: json.RawMessage(`"Initial instruction"`)}, EffectiveFrom: "2026-09-27T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
	claimKey := uuid.NewString()
	changed("claim creation", func() error { _, e := service.CreateClaim(ctx, actor, sc, claim, claimKey, 0); return e })
	stable("claim replay", func() error { _, e := service.CreateClaim(ctx, actor, sc, claim, claimKey, 0); return e })
	changed("claim revision", func() error {
		claim.Literal.Value = json.RawMessage(`"Revised instruction"`)
		_, e := service.CreateClaim(ctx, actor, sc, claim, uuid.NewString(), 1)
		return e
	})
	authority := evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Source confirmed"}
	authorityKey := uuid.NewString()
	changed("authority designation", func() error {
		_, e := ev.DecideAuthority(ctx, ea, es, source.SourceID, authority, authorityKey, 0)
		return e
	})
	stable("authority replay", func() error {
		_, e := ev.DecideAuthority(ctx, ea, es, source.SourceID, authority, authorityKey, 0)
		return e
	})
	reviewInput := ReviewInput{ClaimRevision: 2, Verdict: "approve", Reason: "Human confirmed instruction"}
	reviewKey := uuid.NewString()
	var review Review
	changed("review creation", func() error {
		var e error
		review, e = service.DecideReview(ctx, actor, sc, claim.ClaimID, reviewInput, reviewKey, 2)
		return e
	})
	stable("review read", func() error { _, e := service.ReadReview(ctx, actor, sc, claim.ClaimID, 2); return e })
	stable("review replay", func() error {
		_, e := service.DecideReview(ctx, actor, sc, claim.ClaimID, reviewInput, reviewKey, 2)
		return e
	})
	withdrawal := RevocationInput{ClaimRevision: 2, ReviewID: review.ReviewID, Reason: "Withdraw reviewed instruction"}
	withdrawalKey := uuid.NewString()
	changed("review revocation", func() error {
		_, e := service.RevokeReview(ctx, actor, sc, claim.ClaimID, withdrawal, withdrawalKey, 2)
		return e
	})
	stable("review revocation replay", func() error {
		_, e := service.RevokeReview(ctx, actor, sc, claim.ClaimID, withdrawal, withdrawalKey, 2)
		return e
	})
	peer := claim
	peer.ClaimID = uuid.NewString()
	peer.Literal = &Literal{Type: "string", Value: json.RawMessage(`"Conflicting instruction"`)}
	changed("claim and conflict creation", func() error { _, e := service.CreateClaim(ctx, actor, sc, peer, uuid.NewString(), 0); return e })
	page, err := service.ListConflicts(ctx, actor, sc, claim.ClaimID, 2, 100, "")
	if err != nil || len(page.Conflicts) != 1 {
		t.Fatalf("conflict fixture=%+v %v", page, err)
	}
	conflict := page.Conflicts[0]
	decision := "prefer_left"
	if conflict.Right.ClaimID == claim.ClaimID {
		decision = "prefer_right"
	}
	resolution := ResolutionInput{Decision: decision, Reason: "Explicit inventory fixture resolution"}
	resolutionKey := uuid.NewString()
	changed("resolution creation", func() error {
		_, e := service.DecideResolution(ctx, actor, sc, conflict.ID, resolution, resolutionKey, 0)
		return e
	})
	stable("resolution replay", func() error {
		_, e := service.DecideResolution(ctx, actor, sc, conflict.ID, resolution, resolutionKey, 0)
		return e
	})
	stable("resolution read", func() error { _, e := service.ReadResolution(ctx, actor, sc, conflict.ID, 0); return e })
	changed("resolution withdrawal", func() error {
		_, e := service.DecideResolution(ctx, actor, sc, conflict.ID, ResolutionInput{Decision: "withdraw", Reason: "Withdraw resolution"}, uuid.NewString(), 1)
		return e
	})

	// The epoch changes in the same transaction as its dependency and audit row.
	beforeRollback := epoch(sc)
	blocker, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err = blocker.ExecContext(ctx, `LOCK TABLE supervisor.knowledge_audit IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	rollbackEntity := entity
	rollbackEntity.EntityID = uuid.NewString()
	rollbackKey := uuid.NewString()
	short, stop := context.WithTimeout(ctx, 150*time.Millisecond)
	_, err = service.CreateEntity(short, actor, sc, rollbackEntity, rollbackKey, 0)
	stop()
	if err == nil {
		t.Fatal("audit lock did not abort mutation")
	}
	if err = blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	if after := epoch(sc); after != beforeRollback {
		t.Fatalf("rolled back mutation changed epoch: %d -> %d", beforeRollback, after)
	}
	var rows int
	if err = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_entities WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, rollbackEntity.EntityID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rolled back entity rows=%d %v", rows, err)
	}
	changed("retry after audit rollback", func() error { _, e := service.CreateEntity(ctx, actor, sc, rollbackEntity, rollbackKey, 0); return e })

	// Privileged retention remains scoped and invalidates the same site epoch.
	unusedSpan, err := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: source.SourceID, Revision: 1, Start: 10, End: 19}, uuid.NewString(), 1)
	if err != nil {
		t.Fatal(err)
	}
	beforeDenied := epoch(sc)
	wrongScope, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = wrongScope.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, foreign.TenantID); err != nil {
		t.Fatal(err)
	}
	_, err = wrongScope.ExecContext(ctx, `DELETE FROM supervisor.evidence_spans WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, unusedSpan.ID)
	wrongScope.Rollback()
	requireSQLState(t, err, "42501")
	if epoch(sc) != beforeDenied {
		t.Fatal("denied retention changed epoch")
	}
	changed("scoped privileged retention", func() error {
		retention, e := owner.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		defer retention.Rollback()
		if _, e = retention.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); e != nil {
			return e
		}
		if _, e = retention.ExecContext(ctx, `DELETE FROM supervisor.evidence_spans WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, unusedSpan.ID); e != nil {
			return e
		}
		return retention.Commit()
	})

	authority.Verdict, authority.Reason = "withdraw", "Withdraw source designation"
	changed("authority withdrawal", func() error {
		_, e := ev.DecideAuthority(ctx, ea, es, source.SourceID, authority, uuid.NewString(), 1)
		return e
	})
	source.Text = "Revised synthetic process"
	sourceKey := uuid.NewString()
	changed("source revision", func() error { _, e := ev.CreateOrRevise(ctx, ea, es, source, sourceKey, 1); return e })
	stable("source revision replay", func() error { _, e := ev.CreateOrRevise(ctx, ea, es, source, sourceKey, 1); return e })
	revokeKey := uuid.NewString()
	changed("source revocation", func() error {
		_, e := ev.Revoke(ctx, ea, es, source.SourceID, "Evidence removed", revokeKey, 2)
		return e
	})
	stable("source revocation replay", func() error {
		_, e := ev.Revoke(ctx, ea, es, source.SourceID, "Evidence removed", revokeKey, 2)
		return e
	})

	for _, statement := range []string{
		`UPDATE supervisor.knowledge_inventory_revisions SET revision=revision+1`,
		`DELETE FROM supervisor.knowledge_inventory_revisions`,
		`INSERT INTO supervisor.knowledge_inventory_revisions(tenant_id,site_id,revision) VALUES($1,$2,9000)`,
		`SELECT supervisor.advance_knowledge_inventory()`,
	} {
		attempt, e := runtime.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = attempt.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); e != nil {
			t.Fatal(e)
		}
		var args []any
		if strings.Contains(statement, "VALUES") {
			args = []any{sc.TenantID, sc.SiteID}
		}
		_, e = attempt.ExecContext(ctx, statement, args...)
		attempt.Rollback()
		requireSQLState(t, e, "42501")
	}
	isolated, err := runtime.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer isolated.Rollback()
	if _, err = isolated.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, foreign.TenantID); err != nil {
		t.Fatal(err)
	}
	if err = isolated.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_inventory_revisions WHERE tenant_id=$1`, sc.TenantID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("cross-tenant epoch visible rows=%d %v", rows, err)
	}
	isolated.Rollback()
	if err = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_inventory_revisions WHERE (tenant_id=$1 AND site_id=$2) OR (tenant_id=$3 AND site_id=$4)`, sc.TenantID, sc.SiteID, foreign.TenantID, foreign.SiteID).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("populated tenant backfill rows=%d %v", rows, err)
	}
	var rls, force bool
	if err = owner.QueryRowContext(ctx, `SELECT relrowsecurity,relforcerowsecurity FROM pg_class WHERE oid='supervisor.knowledge_inventory_revisions'::regclass`).Scan(&rls, &force); err != nil || !rls || !force {
		t.Fatalf("inventory RLS=%t FORCE=%t %v", rls, force, err)
	}
	if err = migrate.Check(ctx, owner); err != nil {
		t.Fatal(err)
	}
}
