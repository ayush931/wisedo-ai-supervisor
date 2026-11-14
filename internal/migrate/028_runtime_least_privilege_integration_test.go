//go:build integration

package migrate

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// runtimeDB connects as the restricted runtime role to the database behind admin.
func runtimeDB(t *testing.T, admin *sql.DB) *sql.DB {
	t.Helper()
	raw := os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if raw == "" {
		t.Fatal("BLOCKED: restricted PostgreSQL URL required")
	}
	var name string
	if err := admin.QueryRow(`SELECT current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
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
	return db
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// The runtime role must not be able to raise a membership by shadowing the catalog that the
// membership guard trigger reads.
func TestRuntimeCannotShadowMembershipGuard(t *testing.T) {
	admin := newOperationalMigrationDB(t)
	ctx := context.Background()
	runtime := runtimeDB(t, admin)
	tenant, site := createSite(t, admin)
	subject := uuid.NewString()

	tx, err := admin.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,'https://idp.example','`+subject+`','viewer')`, tenant, site); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Layer 1: runtime has no TEMP privilege at all.
	if _, err = runtime.ExecContext(ctx, `CREATE TEMP TABLE probe(x int)`); sqlState(err) != "42501" {
		t.Fatalf("runtime created a temporary table: %v", err)
	}

	// Layer 2: even with TEMP granted, the guard reads the real catalog.
	var name string
	if err = admin.QueryRow(`SELECT current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if _, err = admin.Exec(`GRANT TEMPORARY ON DATABASE ` + name + ` TO supervisor_runtime`); err != nil {
		t.Fatal(err)
	}
	conn, err := runtime.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rtx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rtx.Rollback()
	for _, statement := range []string{
		`SELECT set_config('supervisor.tenant_id','` + tenant + `',true)`,
		`CREATE TEMP TABLE pg_class(oid oid, relowner oid)`,
		`INSERT INTO pg_temp.pg_class VALUES ('supervisor.memberships'::regclass, (SELECT r.oid FROM pg_catalog.pg_roles r WHERE r.rolname=current_user))`,
	} {
		if _, err = rtx.ExecContext(ctx, statement); err != nil {
			t.Fatalf("exploit setup %q: %v", statement, err)
		}
	}
	_, err = rtx.ExecContext(ctx, `UPDATE supervisor.memberships SET role='owner' WHERE subject=$1`, subject)
	if sqlState(err) != "42501" {
		t.Fatalf("runtime raised a membership through a shadowed pg_class: %v", err)
	}
	_ = rtx.Rollback()

	var role string
	if err = admin.QueryRowContext(ctx, `SELECT role FROM supervisor.memberships WHERE subject=$1`, subject).Scan(&role); err != nil || role != "viewer" {
		t.Fatalf("membership role=%q err=%v", role, err)
	}
}
