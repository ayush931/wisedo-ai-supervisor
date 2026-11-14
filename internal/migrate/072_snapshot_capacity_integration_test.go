//go:build integration

package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestWIS621SnapshotCapacityMigration applies 072 over a database that already holds a snapshot and
// proves the stored body bound: before 072 a 100 KiB body is refused, after it the same body is
// stored, a body of exactly 8 MiB is stored and one byte more is refused even when application code
// is bypassed. The existing snapshot is untouched.
func TestWIS621SnapshotCapacityMigration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := migratorDB(t)
	if err := applyThrough(ctx, db, 71); err != nil {
		t.Fatal(err)
	}
	tenant, site := uuid.NewString(), uuid.NewString()
	seed, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Rollback()
	for _, q := range []string{`SELECT set_config('supervisor.tenant_id',$1,true)`, `INSERT INTO supervisor.tenants(id,name) VALUES($1,'snapshot capacity')`} {
		if _, err = seed.ExecContext(ctx, q, tenant); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = seed.ExecContext(ctx, `INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'site')`, tenant, site); err != nil {
		t.Fatal(err)
	}
	if err = seed.Commit(); err != nil {
		t.Fatal(err)
	}
	// body returns a canonical snapshot object of exactly size bytes.
	body := func(size int) string {
		head := `{"schema_version":1,"tenant_id":"` + tenant + `","site_id":"` + site + `","inventory_revision":0,"pad":"`
		return head + strings.Repeat("x", size-len(head)-2) + `"}`
	}
	insert := func(size int) (string, error) {
		id, contents := uuid.NewString(), body(size)
		sum := sha256.Sum256([]byte(contents))
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return "", err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
			return "", err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_process_snapshots(tenant_id,site_id,snapshot_id,inventory_revision,sha256,canonical_contents,reason,actor_issuer,actor_subject,auth_time)
 VALUES($1,$2,$3,0,$4,$5,'capacity probe','https://idp.test','u',clock_timestamp())`, tenant, site, id, hex.EncodeToString(sum[:]), contents); err != nil {
			return "", err
		}
		return id, tx.Commit()
	}
	checkViolation := func(err error) bool {
		var pg *pgconn.PgError
		return errors.As(err, &pg) && pg.Code == "23514" && pg.ConstraintName == "knowledge_process_snapshots_canonical_contents_check"
	}
	existing, err := insert(1 << 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = insert(100 << 10); !checkViolation(err) {
		t.Fatalf("before 072 a 100 KiB body: %v", err)
	}
	if err = Apply(ctx, db); err != nil {
		t.Fatalf("072 over an existing snapshot: %v", err)
	}
	if err = Check(ctx, db); err != nil {
		t.Fatal(err)
	}
	read, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var size int
	if _, err = read.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err == nil {
		err = read.QueryRowContext(ctx, `SELECT octet_length(canonical_contents) FROM supervisor.knowledge_process_snapshots WHERE tenant_id=$1 AND snapshot_id=$2`, tenant, existing).Scan(&size)
	}
	if err = errors.Join(err, read.Rollback()); err != nil || size != 1<<10 {
		t.Fatalf("existing snapshot after 072: %d bytes (%v)", size, err)
	}
	for _, tc := range []struct {
		name    string
		size    int
		refused bool
	}{
		{"100 KiB body", 100 << 10, false},
		{"exactly 8 MiB", 8 << 20, false},
		{"8 MiB plus one byte", 8<<20 + 1, true},
	} {
		if _, err = insert(tc.size); tc.refused != checkViolation(err) || !tc.refused && err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}
