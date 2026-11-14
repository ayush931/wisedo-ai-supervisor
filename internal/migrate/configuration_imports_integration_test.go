//go:build integration

package migrate

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestConfigurationImportsMigration(t *testing.T) {
	raw := os.Getenv("SUPERVISOR_TEST_ADMIN_URL")
	if raw == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL admin URL required")
	}
	db, err := sql.Open("pgx", raw)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err = Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	var rls, forced bool
	if err = db.QueryRowContext(ctx, `SELECT c.relrowsecurity,c.relforcerowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='supervisor' AND c.relname='configuration_imports'`).Scan(&rls, &forced); err != nil {
		t.Fatal(err)
	}
	if !rls || !forced {
		t.Fatalf("configuration_imports RLS enabled=%t forced=%t", rls, forced)
	}
	var policies, privileges, triggers, version int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM pg_policies WHERE schemaname='supervisor' AND tablename='configuration_imports' AND policyname='tenant_scope'`).Scan(&policies); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.table_privileges WHERE table_schema='supervisor' AND table_name='configuration_imports' AND grantee='supervisor_runtime' AND privilege_type IN ('SELECT','INSERT')`).Scan(&privileges); err != nil {
		t.Fatal(err)
	}
	var forbidden int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.table_privileges WHERE table_schema='supervisor' AND table_name='configuration_imports' AND grantee='supervisor_runtime' AND privilege_type IN ('UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER')`).Scan(&forbidden); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace JOIN pg_proc p ON p.oid=t.tgfoid WHERE n.nspname='supervisor' AND c.relname='configuration_imports' AND p.proname='evidence_immutable' AND NOT t.tgisinternal`).Scan(&triggers); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM public.supervisor_schema_versions WHERE version=54`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if policies != 1 || privileges != 2 || forbidden != 0 || triggers != 1 || version != 1 {
		t.Fatalf("migration contract policies=%d select-insert=%d forbidden=%d immutable=%d version=%d", policies, privileges, forbidden, triggers, version)
	}
}
