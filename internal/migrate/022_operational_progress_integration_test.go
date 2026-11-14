//go:build integration

package migrate

import (
	"context"
	"testing"
)

func TestOperationalProgressMigrationCatalog(t *testing.T) {
	db := newOperationalMigrationDB(t)
	ctx := context.Background()
	if err := Check(ctx, db); err != nil {
		t.Fatal(err)
	}
	assertGrants(t, db, appendOnly, "operational_source_progress_heads", "operational_source_progress_revisions")
	var version, policies, immutable, headUpdateColumns int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM public.supervisor_schema_versions WHERE version=22),(SELECT count(*) FROM pg_policies WHERE schemaname='supervisor' AND tablename LIKE 'operational_source_progress_%' AND policyname='tenant_scope'),(SELECT count(*) FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='supervisor' AND c.relname='operational_source_progress_revisions' AND NOT t.tgisinternal AND t.tgname='operational_source_progress_revision_immutable'),(SELECT count(*) FROM information_schema.column_privileges WHERE grantee='supervisor_runtime' AND table_schema='supervisor' AND table_name='operational_source_progress_heads' AND privilege_type='UPDATE')`).Scan(&version, &policies, &immutable, &headUpdateColumns); err != nil {
		t.Fatal(err)
	}
	if version != 1 || policies != 2 || immutable != 1 || headUpdateColumns != 2 {
		t.Fatalf("migration invariants: version=%d policies=%d immutable=%d head update columns=%d", version, policies, immutable, headUpdateColumns)
	}
}
