//go:build integration

package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestWorkerTenantDiscoveryMigrationCatalog(t *testing.T) {
	if len(migrations) < 53 || migrations[52].version != 53 || migrations[52].sql != workerTenantDiscoverySchema {
		t.Fatal("worker tenant-discovery migration 053 moved or changed in the immutable catalog")
	}
}

func TestWorkerTenantDiscoveryMigrationUpgradeFrom052(t *testing.T) {
	db := migratorDB(t)
	ctx := context.Background()
	if err := applyThrough(ctx, db, 52); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := Check(ctx, db); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT to_regprocedure('supervisor.active_tenant_ids()') IS NOT NULL AND EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='supervisor' AND table_name='tenants' AND column_name='deactivated_at')`).Scan(&exists); err != nil || !exists {
		t.Fatalf("migration 053 upgrade missing schema: exists=%v err=%v", exists, err)
	}
}

func TestActiveTenantIDsUnderNonBypassMigrator(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	migrator := migratorDB(t)
	if err := Apply(ctx, migrator); err != nil {
		t.Fatal(err)
	}
	runtime := runtimeDB(t, migrator)
	admin := activeTenantAdminDB(t, migrator)
	active, deactivated, bogus := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, id := range []string{active, deactivated} {
		tx, err := migrator.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, id); err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.tenants(id,name) VALUES($1,'tenant discovery integration')`, id)
		}
		if err == nil && id == deactivated {
			_, err = tx.ExecContext(ctx, `UPDATE supervisor.tenants SET deactivated_at=now() WHERE id=$1`, id)
		}
		if err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("TestActiveTenantIDsOwnerIsNotPrivileged", func(t *testing.T) {
		var owner, currentUser string
		var superuser, bypass, definer, pinned bool
		err := migrator.QueryRowContext(ctx, `SELECT pg_get_userbyid(p.proowner),current_user,r.rolsuper,r.rolbypassrls,p.prosecdef,
			COALESCE(array_to_string(p.proconfig,','),'') LIKE '%search_path=pg_catalog, pg_temp%'
			FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_roles r ON r.oid=p.proowner
			WHERE p.oid='supervisor.active_tenant_ids()'::regprocedure`).Scan(&owner, &currentUser, &superuser, &bypass, &definer, &pinned)
		if err != nil || owner != currentUser || superuser || bypass || !definer || !pinned {
			t.Fatalf("owner=%q current=%q superuser=%v bypass=%v definer=%v pinned=%v err=%v", owner, currentUser, superuser, bypass, definer, pinned, err)
		}
	})

	t.Run("active results contain only the non-deactivated tenant", func(t *testing.T) {
		rows, err := runtime.QueryContext(ctx, `SELECT tenant_id::text FROM supervisor.active_tenant_ids() AS active(tenant_id) ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			got = append(got, id)
		}
		if err = rows.Err(); err != nil || !slices.Equal(got, []string{active}) {
			t.Fatalf("active IDs=%v err=%v", got, err)
		}
	})

	t.Run("TestActiveTenantIDsRejectsArguments", func(t *testing.T) {
		if _, err := runtime.ExecContext(ctx, `SELECT supervisor.active_tenant_ids('ignored')`); sqlState(err) != "42883" {
			t.Fatalf("argument call SQLSTATE=%s err=%v", sqlState(err), err)
		}
	})

	t.Run("TestActiveTenantIDsNotExecutableByPublic", func(t *testing.T) {
		var publicExec, runtimeExec bool
		if err := migrator.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_catalog.pg_proc p CROSS JOIN LATERAL pg_catalog.aclexplode(COALESCE(p.proacl, pg_catalog.acldefault('f',p.proowner))) a
			WHERE p.oid='supervisor.active_tenant_ids()'::regprocedure AND a.grantee=0 AND a.privilege_type='EXECUTE'),
			has_function_privilege('supervisor_runtime','supervisor.active_tenant_ids()','EXECUTE')`).Scan(&publicExec, &runtimeExec); err != nil {
			t.Fatal(err)
		}
		if publicExec || !runtimeExec {
			t.Fatalf("PUBLIC execute=%v runtime execute=%v", publicExec, runtimeExec)
		}
		role := "active_fn_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		password := uuid.NewString()
		if _, err := admin.ExecContext(ctx, fmt.Sprintf("CREATE ROLE %s LOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS PASSWORD '%s'", role, password)); err != nil {
			t.Fatal(err)
		}
		defer func() { _, _ = admin.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+role) }()
		if _, err := admin.ExecContext(ctx, `DO $$ BEGIN EXECUTE format('GRANT CONNECT ON DATABASE %I TO %I',current_database(),'`+role+`'); EXECUTE format('GRANT USAGE ON SCHEMA supervisor TO %I','`+role+`'); END $$`); err != nil {
			t.Fatal(err)
		}
		raw := os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		var dbName string
		if err = migrator.QueryRowContext(ctx, `SELECT current_database()`).Scan(&dbName); err != nil {
			t.Fatal(err)
		}
		u.Path = "/" + dbName
		u.User = url.UserPassword(role, password)
		unprivileged, err := sql.Open("pgx", u.String())
		if err != nil {
			t.Fatal(err)
		}
		_, callErr := unprivileged.ExecContext(ctx, `SELECT supervisor.active_tenant_ids()`)
		_ = unprivileged.Close()
		if sqlState(callErr) != "42501" {
			t.Fatalf("unprivileged function call SQLSTATE=%s err=%v", sqlState(callErr), callErr)
		}
	})

	t.Run("TestActiveTenantIDsIgnoresCallerSearchPath", func(t *testing.T) {
		if _, err := admin.ExecContext(ctx, `DO $$ BEGIN EXECUTE format('GRANT TEMPORARY ON DATABASE %I TO supervisor_runtime',current_database()); END $$`); err != nil {
			t.Fatal(err)
		}
		conn, err := runtime.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		for _, q := range []string{
			`CREATE TEMP TABLE tenants(id uuid,deactivated_at timestamptz)`,
			`INSERT INTO pg_temp.tenants VALUES('` + bogus + `',NULL)`,
			`SET LOCAL search_path=pg_temp,supervisor`,
		} {
			if _, err = tx.ExecContext(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT tenant_id::text FROM supervisor.active_tenant_ids() AS active(tenant_id) ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			got = append(got, id)
		}
		if err = rows.Err(); err != nil || !slices.Equal(got, []string{active}) {
			t.Fatalf("shadowed result=%v err=%v", got, err)
		}
	})

	t.Run("TestRuntimeTenantsSelectStillScoped", func(t *testing.T) {
		var unscoped int
		if err := runtime.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.tenants`).Scan(&unscoped); err != nil || unscoped != 0 {
			t.Fatalf("unscoped tenant rows=%d err=%v", unscoped, err)
		}
		tx, err := runtime.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, active); err != nil {
			t.Fatal(err)
		}
		rows, err := tx.QueryContext(ctx, `SELECT * FROM supervisor.tenants ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		var scoped []string
		for rows.Next() {
			var id, name string
			var deactivatedAt sql.NullTime
			if err = rows.Scan(&id, &name, &deactivatedAt); err != nil {
				t.Fatal(err)
			}
			scoped = append(scoped, id)
		}
		_ = rows.Close()
		if err = rows.Err(); err != nil || !slices.Equal(scoped, []string{active}) {
			t.Fatalf("scoped tenant rows=%v err=%v", scoped, err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("TestRuntimeCannotUpdateDeactivatedAt", func(t *testing.T) {
		for _, value := range []string{"now()", "NULL"} {
			tx, err := runtime.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, active); err != nil {
				t.Fatal(err)
			}
			_, err = tx.ExecContext(ctx, `UPDATE supervisor.tenants SET deactivated_at=`+value+` WHERE id=$1`, active)
			_ = tx.Rollback()
			if sqlState(err) != "42501" {
				t.Fatalf("runtime UPDATE with %s SQLSTATE=%s err=%v", value, sqlState(err), err)
			}
		}
	})
}

func activeTenantAdminDB(t *testing.T, migrator *sql.DB) *sql.DB {
	t.Helper()
	raw := os.Getenv("SUPERVISOR_TEST_ADMIN_URL")
	var database string
	if err := migrator.QueryRow(`SELECT current_database()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + database
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
