//go:build integration

package knowledge

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/google/uuid"
)

func TestPostgresPopulatedConflictUpgrade(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("real PostgreSQL URLs required")
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	name := "conflict_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	var super, bypass bool
	if err = owner.QueryRowContext(ctx, `SELECT rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&super, &bypass); err != nil || super || bypass {
		t.Fatalf("migration owner super=%v bypass=%v err=%v", super, bypass, err)
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
	files, err := filepath.Glob("../migrate/00[1-8]_*.sql")
	if err != nil || len(files) != 8 {
		t.Fatalf("008 baseline: %v %v", files, err)
	}
	for i, path := range files {
		body, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		must(string(body))
		sum := sha256.Sum256(body)
		must(`INSERT INTO public.supervisor_schema_versions(version,sha256) VALUES($1,$2)`, i+1, hex.EncodeToString(sum[:]))
	}
	// The current service takes the site lock through lock_member_site (079, WIS-699). Install
	// that function alone, unrecorded, so the service can populate the 008 schema; the upgrade
	// below applies 079 for real (CREATE OR REPLACE).
	lock, err := os.ReadFile("../migrate/079_member_site_lock.sql")
	if err != nil {
		t.Fatal(err)
	}
	must(string(lock))
	must(`SET row_security=on`)
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(uuid.NewString()))
	actor := Actor{"https://conflict-upgrade.invalid", uuid.NewString(), hex.EncodeToString(sum[:])}
	if _, err = owner.ExecContext(ctx, `INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'fixture',now(),now(),now()+interval '1 hour',now())`, actor.SessionHash, actor.Issuer, actor.Subject); err != nil {
		t.Fatal(err)
	}
	type fixture struct {
		scope    Scope
		claims   []string
		payloads map[string]string
	}
	fixtures := make([]fixture, 2)
	service, legacy := New(runtime), legacyEvidence{t, ctx, owner, actor}
	for tenant := range fixtures {
		f := &fixtures[tenant]
		f.scope = Scope{uuid.NewString(), uuid.NewString()}
		f.payloads = map[string]string{}
		tx, err = owner.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		must(`SELECT set_config('supervisor.tenant_id',$1,true)`, f.scope.TenantID)
		must(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'conflict upgrade')`, f.scope.TenantID)
		must(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'conflict upgrade')`, f.scope.TenantID, f.scope.SiteID)
		must(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, f.scope.TenantID, f.scope.SiteID, actor.Issuer, actor.Subject)
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		for _, scenario := range []string{"conflict", "equivalent", "disjoint", "stale", "revoked"} {
			source := struct{ SourceID string }{uuid.NewString()}
			legacy.source(f.scope, source.SourceID, "manual", "Synthetic process evidence", 1)
			entity := uuid.NewString()
			if _, e := service.CreateEntity(ctx, actor, f.scope, EntityInput{EntityID: entity, Type: "operation", Name: "Legacy operation", SourceID: source.SourceID, SourceRevision: 1}, uuid.NewString(), 0); e != nil {
				t.Fatal(e)
			}
			span := struct{ ID string }{legacy.span(f.scope, source.SourceID, 0, 9)}
			tx, err = owner.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			must(`SELECT set_config('supervisor.tenant_id',$1,true)`, f.scope.TenantID)
			for side := 0; side < 2; side++ {
				id := uuid.NewString()
				from, end := "2026-09-27T00:00:00Z", "2026-09-27T02:00:00Z"
				literal := &Literal{Type: "decimal", Value: json.RawMessage(`"10"`)}
				predicate := "capacity_per_hour"
				if side == 1 {
					literal.Value = json.RawMessage(`"20"`)
					from = "2026-09-27T01:00:00Z"
				}
				if scenario == "equivalent" {
					predicate = "objective"
					literal.Type = "string"
					literal.Value = json.RawMessage(`"same"`)
					if side == 1 {
						literal.Value = json.RawMessage(`"s\u0061me"`)
					}
				}
				if scenario == "disjoint" && side == 1 {
					from = "2026-09-27T02:00:00Z"
					end = "2026-09-27T03:00:00Z"
				}
				in := ClaimInput{ClaimID: id, SubjectID: entity, SubjectRevision: 1, Predicate: predicate, Literal: literal, EffectiveFrom: from, EffectiveTo: &end, EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
				body, e := json.Marshal(in)
				if e != nil {
					t.Fatal(e)
				}
				must(`INSERT INTO supervisor.knowledge_claim_heads(tenant_id,site_id,id,current_revision) VALUES($1,$2,$3,1)`, f.scope.TenantID, f.scope.SiteID, id)
				must(`INSERT INTO supervisor.knowledge_claim_revisions(tenant_id,site_id,claim_id,revision,subject_id,predicate,payload,actor_issuer,actor_subject) VALUES($1,$2,$3,1,$4,$5,$6,$7,$8)`, f.scope.TenantID, f.scope.SiteID, id, entity, predicate, string(body), actor.Issuer, actor.Subject)
				must(`INSERT INTO supervisor.knowledge_claim_entity_refs(tenant_id,site_id,claim_id,claim_revision,subject_id,subject_revision) VALUES($1,$2,$3,1,$4,1)`, f.scope.TenantID, f.scope.SiteID, id, entity)
				must(`INSERT INTO supervisor.knowledge_claim_spans(tenant_id,site_id,claim_id,claim_revision,span_id,source_id,source_revision) VALUES($1,$2,$3,1,$4,$5,1)`, f.scope.TenantID, f.scope.SiteID, id, span.ID, source.SourceID)
				var persisted string
				if e = tx.QueryRowContext(ctx, `SELECT payload::text FROM supervisor.knowledge_claim_revisions WHERE tenant_id=$1 AND claim_id=$2`, f.scope.TenantID, id).Scan(&persisted); e != nil {
					t.Fatal(e)
				}
				f.payloads[id] = persisted
				if scenario == "conflict" {
					f.claims = append(f.claims, id)
				}
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if scenario == "stale" {
				legacy.source(f.scope, source.SourceID, "manual", "Changed synthetic evidence", 2)
			}
			if scenario == "revoked" {
				legacy.revoke(f.scope, source.SourceID)
			}
		}
	}
	if err = migrate.Apply(ctx, owner); err != nil {
		t.Fatalf("populated 008 upgrade: %v", err)
	}
	if err = migrate.Apply(ctx, owner); err != nil {
		t.Fatalf("repeat upgrade: %v", err)
	}
	for _, f := range fixtures {
		tx, err = runtime.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		must(`SELECT set_config('supervisor.tenant_id',$1,true)`, f.scope.TenantID)
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_conflicts`).Scan(&count); err != nil || count != 1 {
			tx.Rollback()
			t.Fatalf("backfilled scoped conflicts=%d want=1 err=%v", count, err)
		}
		var left, right, kind string
		var lr, rr int64
		var from, to time.Time
		err = tx.QueryRowContext(ctx, `SELECT left_claim_id,left_revision,right_claim_id,right_revision,kind,effective_from,effective_to FROM supervisor.knowledge_conflicts`).Scan(&left, &lr, &right, &rr, &kind, &from, &to)
		sort.Strings(f.claims)
		if err != nil || left != f.claims[0] || right != f.claims[1] || lr != 1 || rr != 1 || kind != "value_conflict" || from.UTC().Format(time.RFC3339) != "2026-09-27T01:00:00Z" || to.UTC().Format(time.RFC3339) != "2026-09-27T02:00:00Z" {
			t.Fatalf("wrong backfilled exact pair: %s/%d %s/%d want=%v %s from=%s to=%s %v", left, lr, right, rr, f.claims, kind, from.Format(time.RFC3339), to.Format(time.RFC3339), err)
		}
		for id, want := range f.payloads {
			var body string
			if err = tx.QueryRowContext(ctx, `SELECT payload::text FROM supervisor.knowledge_claim_revisions WHERE claim_id=$1`, id).Scan(&body); err != nil || body != want {
				t.Fatalf("legacy payload mutated id=%s err=%v", id, err)
			}
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	var unforced int
	if err = owner.QueryRowContext(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='supervisor' AND c.relkind='r' AND (NOT c.relrowsecurity OR NOT c.relforcerowsecurity) AND (c.relname LIKE 'knowledge_%' OR c.relname IN ('evidence_heads','evidence_revocations'))`).Scan(&unforced); err != nil || unforced != 0 {
		t.Fatalf("backfill RLS not restored count=%d err=%v", unforced, err)
	}
	if err = migrate.Check(ctx, owner); err != nil {
		t.Fatal(err)
	}
	t.Log("populated 008 -> 009: non-superuser owner, two tenants, exact conflicts, semantic equality, temporal/provenance exclusions and immutable payloads verified")
}
