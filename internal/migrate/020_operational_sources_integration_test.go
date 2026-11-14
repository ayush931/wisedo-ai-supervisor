//go:build integration

package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/operational"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func newOperationalMigrationDB(t *testing.T) *sql.DB {
	t.Helper()
	raw := os.Getenv("SUPERVISOR_TEST_ADMIN_URL")
	if raw == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL URL required")
	}
	admin, err := sql.Open("pgx", raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	name := "operational_migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, e := admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	})
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err = Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db
}

func createSite(t *testing.T, db *sql.DB) (string, string) {
	t.Helper()
	tenant, site := uuid.NewString(), uuid.NewString()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'migration integrity test')`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'migration integrity test')`, tenant, site); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return tenant, site
}

func setTenant(t *testing.T, tx *sql.Tx, tenant string) {
	t.Helper()
	if _, err := tx.Exec(`SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		t.Fatal(err)
	}
}

func emptyMappingDigest() string {
	sum := sha256.Sum256([]byte("[]"))
	return hex.EncodeToString(sum[:])
}

func mappingDigest(t *testing.T, rows any) string {
	t.Helper()
	body, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func insertMapHeaders(t *testing.T, tx *sql.Tx, tenant, site, source, digest string) {
	t.Helper()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO supervisor.operational_source_heads(tenant_id,site_id,source_id,current_revision) VALUES($1,$2,$3,1)`, []any{tenant, site, source}},
		{`INSERT INTO supervisor.operational_source_mapping_heads(tenant_id,site_id,source_id,current_revision) VALUES($1,$2,$3,1)`, []any{tenant, site, source}},
		{`INSERT INTO supervisor.operational_source_mapping_revisions(tenant_id,site_id,source_id,revision,mapping_sha256,actor_issuer,actor_subject) VALUES($1,$2,$3,1,$4,'test','operator')`, []any{tenant, site, source, digest}},
		{`INSERT INTO supervisor.operational_source_revisions(tenant_id,site_id,source_id,revision,source_type,authority_scope,mapping_revision,mapping_sha256,enabled,access_policy_revision,freshness_seconds,clock_skew_seconds,config_sha256,actor_issuer,actor_subject,auth_time) VALUES($1,$2,$3,1,'manual','site',1,$4,true,1,120,30,$5,'test','operator',clock_timestamp())`, []any{tenant, site, source, digest, strings.Repeat("a", 64)}},
	} {
		if _, err := tx.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
}

// tableGrants is what the restricted runtime role may do to one supervisor table.
type tableGrants struct {
	Forced, RuntimeOwns, Select, Insert, Update, Delete bool
	TenantPolicies                                      int
}

// appendOnly is the expected profile of a tenant table the runtime may only read and append to.
var appendOnly = tableGrants{Forced: true, Select: true, Insert: true, TenantPolicies: 1}

func runtimeGrants(t *testing.T, db *sql.DB, table string) tableGrants {
	t.Helper()
	var g tableGrants
	if err := db.QueryRow(`SELECT c.relrowsecurity AND c.relforcerowsecurity,c.relowner=r.oid,
 has_table_privilege(r.oid,c.oid,'SELECT'),has_table_privilege(r.oid,c.oid,'INSERT'),has_table_privilege(r.oid,c.oid,'UPDATE'),has_table_privilege(r.oid,c.oid,'DELETE'),
 (SELECT count(*) FROM pg_policies p WHERE p.schemaname='supervisor' AND p.tablename=c.relname AND p.policyname='tenant_scope')
 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace CROSS JOIN pg_roles r
 WHERE n.nspname='supervisor' AND c.relname=$1 AND r.rolname='supervisor_runtime'`, table).Scan(&g.Forced, &g.RuntimeOwns, &g.Select, &g.Insert, &g.Update, &g.Delete, &g.TenantPolicies); err != nil {
		t.Fatalf("%s: %v", table, err)
	}
	return g
}

func assertGrants(t *testing.T, db *sql.DB, want tableGrants, tables ...string) {
	t.Helper()
	for _, table := range tables {
		if got := runtimeGrants(t, db, table); got != want {
			t.Errorf("%s runtime grants = %+v, want %+v", table, got, want)
		}
	}
}

func TestOperationalSourceMigrationCatalog(t *testing.T) {
	db := newOperationalMigrationDB(t)
	ctx := context.Background()
	// newOperationalMigrationDB applied every migration; a second Apply must be a no-op.
	if err := Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := Check(ctx, db); err != nil {
		t.Fatal(err)
	}
	assertGrants(t, db, appendOnly, "operational_source_heads", "operational_source_mapping_heads", "operational_source_mapping_revisions", "operational_source_mappings", "operational_source_revisions", "operational_source_review_heads", "operational_source_reviews", "operational_source_receipts", "operational_source_audit")
	var targetFK, immutable, revisionGuard, revision20 bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(
 SELECT 1 FROM pg_constraint c JOIN pg_class src ON src.oid=c.conrelid JOIN pg_namespace sn ON sn.oid=src.relnamespace
 JOIN pg_class dst ON dst.oid=c.confrelid JOIN pg_namespace dn ON dn.oid=dst.relnamespace
 WHERE c.contype='f' AND sn.nspname='supervisor' AND src.relname='operational_source_mappings'
 AND dn.nspname='supervisor' AND dst.relname='knowledge_entity_revisions' AND cardinality(c.conkey)=4),
 EXISTS(SELECT 1 FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='supervisor' AND c.relname='operational_source_revisions' AND t.tgname='operational_source_revision_immutable' AND NOT t.tgisinternal),
 EXISTS(SELECT 1 FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='supervisor' AND c.relname='operational_source_heads' AND t.tgname='operational_source_head_guard' AND NOT t.tgisinternal),
 EXISTS(SELECT 1 FROM public.supervisor_schema_versions WHERE version=20)`).Scan(&targetFK, &immutable, &revisionGuard, &revision20); err != nil {
		t.Fatal(err)
	}
	if !targetFK || !immutable || !revisionGuard || !revision20 {
		t.Fatalf("migration catalog invariant failed: targetFK=%v immutable=%v revisionGuard=%v version20=%v", targetFK, immutable, revisionGuard, revision20)
	}
	var version int
	var coreSHA256, canExecute bool
	if err := db.QueryRowContext(ctx, `SELECT current_setting('server_version_num')::integer,
 to_regprocedure('pg_catalog.sha256(bytea)') IS NOT NULL,
 has_function_privilege(current_user,'pg_catalog.sha256(bytea)','EXECUTE')`).Scan(&version, &coreSHA256, &canExecute); err != nil {
		t.Fatal(err)
	}
	if version < 170000 || !coreSHA256 || !canExecute {
		t.Fatalf("migration role lacks PostgreSQL 17 core SHA-256: version=%d available=%v", version, coreSHA256)
	}
}

func TestOperationalMappingDigestIsEnforcedAtCommit(t *testing.T) {
	db := newOperationalMigrationDB(t)
	tenant, site := createSite(t, db)
	t.Run("empty-map-has-canonical-digest", func(t *testing.T) {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		setTenant(t, tx, tenant)
		insertMapHeaders(t, tx, tenant, site, uuid.NewString(), emptyMappingDigest())
		if err = tx.Commit(); err != nil {
			t.Fatalf("canonical empty mapping rejected: %v", err)
		}
	})
	t.Run("arbitrary-digest-rejected-at-commit", func(t *testing.T) {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		setTenant(t, tx, tenant)
		insertMapHeaders(t, tx, tenant, site, uuid.NewString(), strings.Repeat("0", 64))
		if err = tx.Commit(); err == nil {
			t.Fatal("arbitrary empty-map digest committed")
		}
	})
	t.Run("missing-mapping-row-rejected-at-commit", func(t *testing.T) {
		intended := []struct {
			ExternalType   string `json:"external_type"`
			ExternalKey    string `json:"external_key"`
			EntityID       string `json:"entity_id"`
			EntityRevision int64  `json:"entity_revision"`
		}{{"asset", "press-01", uuid.NewString(), 1}}
		digest := mappingDigest(t, intended)
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		setTenant(t, tx, tenant)
		insertMapHeaders(t, tx, tenant, site, uuid.NewString(), digest)
		if err = tx.Commit(); err == nil {
			t.Fatal("mapping digest for omitted row committed")
		}
	})
	t.Run("canonical-row-hash-and-late-insert", func(t *testing.T) {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		setTenant(t, tx, tenant)
		entity1, entity2 := uuid.NewString(), uuid.NewString()
		seedOperationalAsset(t, tx, tenant, site, entity1)
		seedOperationalAsset(t, tx, tenant, site, entity2)
		source := uuid.NewString()
		key := "press<&>\u2028line"
		mapping := operational.Mapping{ExternalType: "asset", ExternalKey: key, EntityID: entity1, EntityRevision: 1}
		insertMapHeaders(t, tx, tenant, site, source, mappingDigest(t, []operational.Mapping{mapping}))
		if _, err = tx.Exec(`INSERT INTO supervisor.operational_source_mappings(tenant_id,site_id,source_id,mapping_revision,external_type,external_key,entity_id,entity_revision) VALUES($1,$2,$3,1,'asset',$4,$5,1)`, tenant, site, source, key, entity1); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatalf("Go canonical mapping digest rejected: %v", err)
		}

		tx, err = db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		setTenant(t, tx, tenant)
		if _, err = tx.Exec(`INSERT INTO supervisor.operational_source_mappings(tenant_id,site_id,source_id,mapping_revision,external_type,external_key,entity_id,entity_revision) VALUES($1,$2,$3,1,'asset','press-02',$4,1)`, tenant, site, source, entity2); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err == nil {
			t.Fatal("late row changed an already-digested mapping revision")
		}
	})
}

func seedOperationalAsset(t *testing.T, tx *sql.Tx, tenant, site, entity string) {
	t.Helper()
	seedAsset(t, tx, tenant, site, entity, "machine")
}

func seedAsset(t *testing.T, tx *sql.Tx, tenant, site, entity, kind string) {
	t.Helper()
	evidence := uuid.NewString()
	payload, err := json.Marshal(struct {
		EntityID       string `json:"entity_id"`
		Type           string `json:"type"`
		AssetKind      string `json:"asset_kind"`
		SourceID       string `json:"source_id"`
		SourceRevision int64  `json:"source_revision"`
	}{entity, "asset", kind, evidence, 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO supervisor.evidence_heads(tenant_id,site_id,source_id,current_revision,state_revision) VALUES($1,$2,$3,1,1)`, []any{tenant, site, evidence}},
		{`INSERT INTO supervisor.evidence_revisions(tenant_id,site_id,source_id,revision,kind,title,body,sha256,byte_length,rune_length,actor_issuer,actor_subject) VALUES($1,$2,$3,1,'manual','asset','mapping test evidence',encode(sha256(convert_to('mapping test evidence','UTF8')),'hex'),21,21,'test','operator')`, []any{tenant, site, evidence}},
		{`INSERT INTO supervisor.knowledge_entities(tenant_id,site_id,id,entity_type,payload,source_id,source_revision,actor_issuer,actor_subject) VALUES($1,$2,$3,'asset',$4,$5,1,'test','operator')`, []any{tenant, site, entity, string(payload), evidence}},
		{`INSERT INTO supervisor.knowledge_entity_heads(tenant_id,site_id,id,current_revision) VALUES($1,$2,$3,1)`, []any{tenant, site, entity}},
		{`INSERT INTO supervisor.knowledge_entity_revisions(tenant_id,site_id,id,revision,entity_type,payload,source_id,source_revision,actor_issuer,actor_subject) VALUES($1,$2,$3,1,'asset',$4,$5,1,'test','operator')`, []any{tenant, site, entity, string(payload), evidence}},
	} {
		if _, err = tx.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOperationalReceiptsAndAuditBindToOperationRevision(t *testing.T) {
	db := newOperationalMigrationDB(t)
	tenant, site := createSite(t, db)
	source := uuid.NewString()
	configHash := strings.Repeat("a", 64)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	setTenant(t, tx, tenant)
	insertMapHeaders(t, tx, tenant, site, source, emptyMappingDigest())
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// A second source revision makes revision 2 resolve in the source table only.
	tx, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	setTenant(t, tx, tenant)
	if _, err = tx.Exec(`UPDATE supervisor.operational_source_heads SET current_revision=2 WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, tenant, site, source); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO supervisor.operational_source_revisions(tenant_id,site_id,source_id,revision,source_type,authority_scope,mapping_revision,mapping_sha256,enabled,access_policy_revision,freshness_seconds,clock_skew_seconds,config_sha256,actor_issuer,actor_subject,auth_time) VALUES($1,$2,$3,2,'manual','site',1,$4,true,1,120,30,$5,'test','operator',clock_timestamp())`, tenant, site, source, emptyMappingDigest(), configHash); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}

	assertRejected := func(name, query string, args ...any) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			tx, e := db.Begin()
			if e != nil {
				t.Fatal(e)
			}
			setTenant(t, tx, tenant)
			if _, e = tx.Exec(query, args...); e == nil {
				e = tx.Commit()
			} else {
				_ = tx.Rollback()
			}
			if e == nil {
				t.Fatal("mismatched/orphan reference was accepted")
			}
		})
	}
	if _, err = db.Exec(`INSERT INTO supervisor.operational_source_audit(tenant_id,site_id,actor_issuer,actor_subject,source_id,revision,event_type,source_revision,mapping_revision) VALUES($1,$2,'test','operator',$3,1,'mapping.read',2,1)`, tenant, site, source); err != nil {
		t.Fatalf("valid mapping audit with divergent source revision rejected: %v", err)
	}
	var sourceRevision, mappingRevision int64
	if err = db.QueryRow(`SELECT source_revision,mapping_revision FROM supervisor.operational_source_audit WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND event_type='mapping.read'`, tenant, site, source).Scan(&sourceRevision, &mappingRevision); err != nil || sourceRevision != 2 || mappingRevision != 1 {
		t.Fatalf("audit lost source/mapping revision types: source=%d mapping=%d err=%v", sourceRevision, mappingRevision, err)
	}
	tx, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	setTenant(t, tx, tenant)
	for _, q := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO supervisor.operational_source_review_heads(tenant_id,site_id,source_id,current_revision) VALUES($1,$2,$3,1)`, []any{tenant, site, source}},
		{`INSERT INTO supervisor.operational_source_reviews(tenant_id,site_id,source_id,revision,decision_id,source_revision,config_sha256,mapping_revision,mapping_sha256,verdict,reason,actor_issuer,actor_subject,auth_time) VALUES($1,$2,$3,1,$4,2,$5,1,$6,'approved','verified','test','operator',clock_timestamp())`, []any{tenant, site, source, uuid.NewString(), configHash, emptyMappingDigest()}},
		{`INSERT INTO supervisor.operational_source_audit(tenant_id,site_id,actor_issuer,actor_subject,source_id,revision,event_type,source_revision,review_revision) VALUES($1,$2,'test','operator',$3,1,'review.decided',2,1)`, []any{tenant, site, source}},
	} {
		if _, err = tx.Exec(q.query, q.args...); err != nil {
			_ = tx.Rollback()
			t.Fatalf("valid review audit rejected: %v", err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	assertRejected("source-receipt-orphan", `INSERT INTO supervisor.operational_source_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_sha256,operation,source_id,revision) VALUES($1,$2,'test','operator','orphan-source',$3,'source',$4,99)`, strings.Repeat("b", 64), source)
	assertRejected("review-receipt-matches-only-source-revision", `INSERT INTO supervisor.operational_source_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_sha256,operation,source_id,revision) VALUES($1,$2,'test','operator','wrong-review',$3,'review',$4,2)`, strings.Repeat("b", 64), source)
	assertRejected("source-audit-orphan", `INSERT INTO supervisor.operational_source_audit(tenant_id,site_id,actor_issuer,actor_subject,source_id,revision,event_type) VALUES($1,$2,'test','operator',$3,99,'source.read')`, source)
	assertRejected("mapping-audit-revision-type-mismatch", `INSERT INTO supervisor.operational_source_audit(tenant_id,site_id,actor_issuer,actor_subject,source_id,revision,event_type,source_revision,mapping_revision) VALUES($1,$2,'test','operator',$3,1,'mapping.read',99,1)`, source)
	assertRejected("review-audit-source-revision-mismatch", `INSERT INTO supervisor.operational_source_audit(tenant_id,site_id,actor_issuer,actor_subject,source_id,revision,event_type,source_revision,review_revision) VALUES($1,$2,'test','operator',$3,1,'review.read',1,1)`, source)
	assertRejected("review-audit-matches-only-source-revision", `INSERT INTO supervisor.operational_source_audit(tenant_id,site_id,actor_issuer,actor_subject,source_id,revision,event_type) VALUES($1,$2,'test','operator',$3,2,'review.read')`, source)
	assertRejected("replay-audit-operation-mismatch", `INSERT INTO supervisor.operational_source_audit(tenant_id,site_id,actor_issuer,actor_subject,source_id,revision,operation,event_type) VALUES($1,$2,'test','operator',$3,2,'review','mutation.replayed')`, source)
	assertRejected("replay-audit-requires-operation", `INSERT INTO supervisor.operational_source_audit(tenant_id,site_id,actor_issuer,actor_subject,source_id,revision,event_type) VALUES($1,$2,'test','operator',$3,2,'mutation.replayed')`, source)
}
