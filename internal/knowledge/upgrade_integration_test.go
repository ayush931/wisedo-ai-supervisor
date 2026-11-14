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

// This must use an actual non-superuser login: SET ROLE on a superuser connection
// would not exercise the migration owner's startup permissions and FORCE RLS.
func TestPostgresPopulatedEntityRevisionUpgrade(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	adminURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL")
	runtimeURL := os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("real PostgreSQL URLs required")
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	name := "entity_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	// Keep admin open through the database/role cleanup registered above.
	if _, err = admin.ExecContext(ctx, `CREATE DATABASE `+name+` OWNER `+name); err != nil {
		t.Fatal(err)
	}
	connect := func(raw string, owner bool) *sql.DB {
		t.Helper()
		u, e := url.Parse(raw)
		if e != nil {
			t.Fatal(e)
		}
		u.Path = "/" + name
		if owner {
			u.User = url.UserPassword(name, password)
		}
		db, e := sql.Open("pgx", u.String())
		if e != nil {
			t.Fatal(e)
		}
		if e = db.PingContext(ctx); e != nil {
			db.Close()
			t.Fatal(e)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	owner := connect(adminURL, true)
	var super, bypass bool
	if err = owner.QueryRowContext(ctx, `SELECT rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&super, &bypass); err != nil || super || bypass {
		t.Fatalf("owner flags: %v %v %v", super, bypass, err)
	}
	files, err := filepath.Glob("../migrate/00[1-6]_*.sql")
	if err != nil || len(files) != 6 {
		t.Fatalf("legacy migrations: %v %v", files, err)
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
	for i, path := range files {
		body, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		must(string(body))
		sum := sha256.Sum256(body)
		must(`INSERT INTO public.supervisor_schema_versions(version,sha256) VALUES($1,$2)`, i+1, hex.EncodeToString(sum[:]))
	}
	must(`SET row_security=on`)
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	type fixture struct {
		scope                              Scope
		entity, other, source, span, claim string
		body, entityBody, receipt          string
	}
	fixtures := make([]fixture, 2)
	sessionSum := sha256.Sum256([]byte(uuid.NewString()))
	actor := Actor{Issuer: "upgrade", Subject: "operator", SessionHash: hex.EncodeToString(sessionSum[:])}
	if _, err = owner.ExecContext(ctx, `INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'fixture',now(),now(),now()+interval '8 hours',now())`, actor.SessionHash, actor.Subject, actor.Issuer); err != nil {
		t.Fatal(err)
	}
	for i := range fixtures {
		f := &fixtures[i]
		f.scope = Scope{uuid.NewString(), uuid.NewString()}
		f.entity = uuid.NewString()
		f.other = uuid.NewString()
		f.source = uuid.NewString()
		f.span = uuid.NewString()
		f.claim = uuid.NewString()
		tx, err = owner.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		must(`SELECT set_config('supervisor.tenant_id',$1,true)`, f.scope.TenantID)
		must(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'upgrade tenant')`, f.scope.TenantID)
		must(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'upgrade site')`, f.scope.TenantID, f.scope.SiteID)
		must(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, f.scope.TenantID, f.scope.SiteID, actor.Issuer, actor.Subject)
		must(`INSERT INTO supervisor.evidence_heads(tenant_id,site_id,source_id,current_revision,state_revision) VALUES($1,$2,$3,1,1)`, f.scope.TenantID, f.scope.SiteID, f.source)
		must(`INSERT INTO supervisor.evidence_revisions(tenant_id,site_id,source_id,revision,kind,title,body,sha256,byte_length,rune_length,actor_issuer,actor_subject) VALUES($1,$2,$3,1,'manual','Legacy source','Old source',encode(sha256(convert_to('Old source','UTF8')),'hex'),10,10,'upgrade','operator')`, f.scope.TenantID, f.scope.SiteID, f.source)
		must(`INSERT INTO supervisor.evidence_spans(tenant_id,site_id,id,source_id,revision,start_rune,end_rune,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,1,0,3,'upgrade','operator')`, f.scope.TenantID, f.scope.SiteID, f.span, f.source)
		for _, id := range []string{f.entity, f.other} {
			payload, e := json.Marshal(EntityInput{EntityID: id, Type: "operation", Name: "Legacy operation", SourceID: f.source, SourceRevision: 1})
			if e != nil {
				t.Fatal(e)
			}
			must(`INSERT INTO supervisor.knowledge_entities(tenant_id,site_id,id,entity_type,payload,source_id,source_revision,actor_issuer,actor_subject) VALUES($1,$2,$3,'operation',$4,$5,1,'upgrade','operator')`, f.scope.TenantID, f.scope.SiteID, id, string(payload), f.source)
		}
		// Deliberately omit new revision pins: this is the persisted 006 wire shape.
		legacy := struct {
			ClaimID        string   `json:"claim_id"`
			SubjectID      string   `json:"subject_id"`
			Predicate      string   `json:"predicate"`
			ObjectEntityID string   `json:"object_entity_id,omitempty"`
			EffectiveFrom  string   `json:"effective_from"`
			EvidenceClass  string   `json:"evidence_class"`
			SourceSpanIDs  []string `json:"source_span_ids"`
		}{f.claim, f.entity, "precedes", f.other, "2026-09-27T00:00:00Z", "report", []string{f.span}}
		payload, e := json.Marshal(legacy)
		if e != nil {
			t.Fatal(e)
		}
		envelope, e := json.Marshal(struct {
			Op       string
			Input    any
			Expected int64
		}{"claim", legacy, 0})
		if e != nil {
			t.Fatal(e)
		}
		sum := sha256.Sum256(envelope)
		f.receipt = hex.EncodeToString(sum[:])
		must(`INSERT INTO supervisor.knowledge_claim_heads(tenant_id,site_id,id,current_revision) VALUES($1,$2,$3,1)`, f.scope.TenantID, f.scope.SiteID, f.claim)
		must(`INSERT INTO supervisor.knowledge_claim_revisions(tenant_id,site_id,claim_id,revision,subject_id,predicate,object_entity_id,payload,actor_issuer,actor_subject) VALUES($1,$2,$3,1,$4,'precedes',$5,$6,'upgrade','operator')`, f.scope.TenantID, f.scope.SiteID, f.claim, f.entity, f.other, string(payload))
		must(`INSERT INTO supervisor.knowledge_claim_spans(tenant_id,site_id,claim_id,claim_revision,span_id,source_id,source_revision) VALUES($1,$2,$3,1,$4,$5,1)`, f.scope.TenantID, f.scope.SiteID, f.claim, f.span, f.source)
		must(`INSERT INTO supervisor.knowledge_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_hash,operation,target_id,revision) VALUES($1,$2,'upgrade','operator','legacy-key',$3,'claim',$4,1)`, f.scope.TenantID, f.scope.SiteID, f.receipt, f.claim)
		if err = tx.QueryRowContext(ctx, `SELECT payload::text FROM supervisor.knowledge_claim_revisions WHERE tenant_id=$1 AND claim_id=$2`, f.scope.TenantID, f.claim).Scan(&f.body); err != nil {
			t.Fatal(err)
		}
		if err = tx.QueryRowContext(ctx, `SELECT payload::text FROM supervisor.knowledge_entities WHERE tenant_id=$1 AND id=$2`, f.scope.TenantID, f.entity).Scan(&f.entityBody); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	checkService := func(runtime *sql.DB) {
		service := New(runtime)
		for _, f := range fixtures {
			input := ClaimInput{ClaimID: f.claim, SubjectID: f.entity, SubjectRevision: 1, Predicate: "precedes", ObjectEntityID: f.other, ObjectRevision: 1, EffectiveFrom: "2026-09-27T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{f.span}}
			out, e := service.CreateClaim(ctx, actor, f.scope, input, "legacy-key", 0)
			if e != nil || out.Revision != 1 || out.SubjectRevision != 1 || out.ObjectRevision != 1 {
				t.Fatalf("legacy receipt replay: %+v %v", out, e)
			}
			changed := input
			changed.EvidenceClass = "hypothesis"
			if _, e = service.CreateClaim(ctx, actor, f.scope, changed, "legacy-key", 0); !errors.Is(e, ErrConflict) {
				t.Fatalf("legacy changed payload accepted: %v", e)
			}
			changed = input
			if _, e = service.CreateEntity(ctx, actor, f.scope, EntityInput{EntityID: f.entity, Type: "operation", Name: "Revised operation", SourceID: f.source, SourceRevision: 1}, "entity-revision-two", 1); e != nil {
				t.Fatal(e)
			}
			changed.SubjectRevision = 2
			if _, e = service.CreateClaim(ctx, actor, f.scope, changed, "legacy-key", 0); !errors.Is(e, ErrConflict) {
				t.Fatalf("legacy changed pins accepted: %v", e)
			}
			entity, e := service.ReadEntity(ctx, actor, f.scope, f.entity, 1)
			if e != nil || entity.Revision != 1 || entity.CurrentRevision != 2 || entity.SourcesCurrent {
				t.Fatalf("backfilled entity read: %+v %v", entity, e)
			}
			out, e = service.CreateClaim(ctx, actor, f.scope, input, "legacy-key", 0)
			if e != nil || out.Revision != 1 || out.SubjectRevision != 1 || out.SourcesCurrent {
				t.Fatalf("legacy replay changed after entity revision: %+v %v", out, e)
			}
			ev := evidence.New(runtime)
			if _, e = ev.Revoke(ctx, evidence.Actor{Issuer: actor.Issuer, Subject: actor.Subject, SessionHash: actor.SessionHash}, evidence.Scope{TenantID: f.scope.TenantID, SiteID: f.scope.SiteID}, f.source, "Legacy source withdrawn", uuid.NewString(), 1); e != nil {
				t.Fatal(e)
			}
			if _, e = service.CreateClaim(ctx, actor, f.scope, input, "legacy-key", 0); !errors.Is(e, ErrNotFound) {
				t.Fatalf("revoked legacy replay visible: %v", e)
			}
			if _, e = service.ReadEntity(ctx, actor, f.scope, f.entity, 1); !errors.Is(e, ErrNotFound) {
				t.Fatalf("revoked backfilled entity visible: %v", e)
			}
		}
	}
	if err = migrate.Apply(ctx, owner); err != nil {
		t.Fatalf("populated owner upgrade: %v", err)
	}
	if err = migrate.Apply(ctx, owner); err != nil {
		t.Fatalf("repeat upgrade: %v", err)
	}
	if err = migrate.Check(ctx, owner); err != nil {
		t.Fatal(err)
	}
	runtime := connect(runtimeURL, false)
	for _, f := range fixtures {
		tx, err = runtime.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		must(`SELECT set_config('supervisor.tenant_id',$1,true)`, f.scope.TenantID)
		var count int
		for _, table := range []string{"knowledge_entity_heads", "knowledge_entity_revisions"} {
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.`+table).Scan(&count); err != nil || count != 2 {
				t.Fatalf("%s scoped rows=%d: %v", table, count, err)
			}
		}
		var body, entityBody, hash, sourceHash string
		var subjectRevision, objectRevision int64
		err = tx.QueryRowContext(ctx, `SELECT c.payload::text,e.payload::text,r.request_hash,s.sha256,p.subject_revision,p.object_revision FROM supervisor.knowledge_claim_revisions c JOIN supervisor.knowledge_claim_entity_refs p ON (p.tenant_id,p.site_id,p.claim_id,p.claim_revision)=(c.tenant_id,c.site_id,c.claim_id,c.revision) JOIN supervisor.knowledge_entity_revisions e ON (e.tenant_id,e.site_id,e.id,e.revision)=(p.tenant_id,p.site_id,p.subject_id,p.subject_revision) JOIN supervisor.knowledge_receipts r ON (r.tenant_id,r.site_id,r.target_id)=(c.tenant_id,c.site_id,c.claim_id) JOIN supervisor.evidence_revisions s ON (s.tenant_id,s.site_id,s.source_id,s.revision)=(e.tenant_id,e.site_id,e.source_id,e.source_revision) WHERE c.claim_id=$1`, f.claim).Scan(&body, &entityBody, &hash, &sourceHash, &subjectRevision, &objectRevision)
		sourceSum := sha256.Sum256([]byte("Old source"))
		if err != nil || body != f.body || entityBody != f.entityBody || hash != f.receipt || sourceHash != hex.EncodeToString(sourceSum[:]) || subjectRevision != 1 || objectRevision != 1 {
			t.Fatalf("backfill changed provenance: err=%v claim=%t entity=%t receipt=%t pins=%d/%d", err, body == f.body, entityBody == f.entityBody, hash == f.receipt, subjectRevision, objectRevision)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	checkService(runtime)
	var forced int
	if err = owner.QueryRowContext(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='supervisor' AND c.relname IN ('knowledge_entities','knowledge_claim_revisions','knowledge_entity_heads','knowledge_entity_revisions','knowledge_claim_entity_refs') AND c.relrowsecurity AND c.relforcerowsecurity`).Scan(&forced); err != nil || forced != 5 {
		t.Fatalf("RLS restored count=%d: %v", forced, err)
	}
	t.Log(fmt.Sprintf("populated 006 -> 007: two tenants, non-superuser owner, immutable payloads, hashes, pins and runtime RLS verified"))
}
