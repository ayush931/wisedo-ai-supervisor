//go:build integration

package migrate

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// migratorDB creates a disposable database owned by a fresh NOSUPERUSER NOBYPASSRLS role, the
// shape deploy/supervisor/bootstrap.sh gives supervisor_migrator, and connects as that role.
// The admin URL is a superuser, so tests that migrate through it cannot see RLS failures that
// only the real migration role hits.
func migratorDB(t *testing.T) *sql.DB {
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
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	role, name, password := "migrator_"+suffix, "migrator_db_"+suffix, uuid.NewString()
	for _, statement := range []string{
		"CREATE ROLE " + role + " LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE PASSWORD '" + password + "'",
		"CREATE DATABASE " + name + " OWNER " + role,
		"REVOKE CONNECT ON DATABASE " + name + " FROM PUBLIC",
		"GRANT CONNECT ON DATABASE " + name + " TO " + role + ", supervisor_runtime",
	} {
		if _, err = admin.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	t.Cleanup(func() {
		if _, e := admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)"); e != nil {
			t.Error(e)
		}
		if _, e := admin.Exec("DROP ROLE IF EXISTS " + role); e != nil {
			t.Error(e)
		}
	})
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)
	u.Path = "/" + name
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// assertTenantTablesForced checks the catalog rather than a fixed count: every supervisor table
// with a tenant_id column has RLS enabled and forced, at least one policy, and is not owned by
// the runtime role.
func assertTenantTablesForced(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`SELECT c.relname FROM pg_catalog.pg_class c
 JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 JOIN pg_catalog.pg_attribute a ON a.attrelid=c.oid AND a.attname='tenant_id' AND NOT a.attisdropped
 WHERE n.nspname='supervisor' AND c.relkind='r'
   AND (NOT c.relrowsecurity OR NOT c.relforcerowsecurity
        OR c.relowner=(SELECT oid FROM pg_catalog.pg_roles WHERE rolname='supervisor_runtime')
        OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_policy p WHERE p.polrelid=c.oid))
 ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var bad []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		bad = append(bad, name)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Fatalf("tenant tables without forced RLS, a policy or with runtime ownership: %v", bad)
	}
	var tenantTables int
	if err = db.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_schema='supervisor' AND column_name='tenant_id'`).Scan(&tenantTables); err != nil || tenantTables < 79 { // 79 tables carry tenant_id at 025; supervisor.tenants is keyed by id
		t.Fatalf("expected the tenant tables to exist, found %d: %v", tenantTables, err)
	}
}

func TestMigrationRoleFreshInstall(t *testing.T) {
	db := migratorDB(t)
	ctx := context.Background()
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("fresh install as the migration role: %v", err)
	}
	if err := Check(ctx, db); err != nil {
		t.Fatal(err)
	}
	assertTenantTablesForced(t, db)
}

func TestMigrationRoleUpgradeFrom022(t *testing.T) {
	db := migratorDB(t)
	ctx := context.Background()
	if err := applyThrough(ctx, db, 22); err != nil {
		t.Fatal(err)
	}
	if err := Check(ctx, db); err == nil || err.Error() != "schema upgrade required" {
		t.Fatalf("a database at 022 must report that an upgrade is required: %v", err)
	}
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("upgrade from 022 as the migration role: %v", err)
	}
	if err := Check(ctx, db); err != nil {
		t.Fatal(err)
	}
	assertTenantTablesForced(t, db)
}

// A database that a superuser migrated with the original 023 body stays valid; any other
// checksum is still rejected.
func TestLegacyChecksumAccepted(t *testing.T) {
	db := newOperationalMigrationDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `UPDATE public.supervisor_schema_versions SET sha256='ed9b438984a2996666c0e78a87b2a8168a82053caddeaedc4726e4fb69265f9e' WHERE version=23`); err != nil {
		t.Fatal(err)
	}
	if err := Check(ctx, db); err != nil {
		t.Fatalf("legacy 023 checksum rejected: %v", err)
	}
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("re-apply over legacy 023: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE public.supervisor_schema_versions SET sha256=repeat('0',64) WHERE version=23`); err != nil {
		t.Fatal(err)
	}
	wantMismatch(t, Check(ctx, db), "unknown 023 checksum")
	if _, err := db.ExecContext(ctx, `UPDATE public.supervisor_schema_versions SET sha256=$1 WHERE version=23`, checksum(migrations[22].sql)); err != nil {
		t.Fatal(err)
	}
	if err := Check(ctx, db); err != nil {
		t.Fatalf("current 023 checksum rejected: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE public.supervisor_schema_versions SET sha256='ed9b438984a2996666c0e78a87b2a8168a82053caddeaedc4726e4fb69265f9e' WHERE version=22`); err != nil {
		t.Fatal(err)
	}
	wantMismatch(t, Check(ctx, db), "legacy 023 checksum on version 22")
}
