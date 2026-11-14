//go:build integration

package ingestion

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestRevokedSourceImportRecordsNoFurtherRows proves the importer side of the WIS-272
// revocation transaction: rows are recorded while the import runs, and once a revocation has
// cancelled the job (under the job row lock) the importer records nothing more for it.
func TestRevokedSourceImportRecordsNoFurtherRows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL admin/runtime URLs required")
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	runtime, err := sql.Open("pgx", runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err = migrate.Apply(ctx, admin); err != nil {
		t.Fatal(err)
	}
	tenant, site, sourceID, jobID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	// Seed a source head and a running job directly; the registry and import flows are
	// covered by the HTTP suites. Replica mode skips the unrelated foreign keys of the fixture.
	tx, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`SET LOCAL session_replication_role = replica`, nil},
		{`SELECT set_config('supervisor.tenant_id',$1,true)`, []any{tenant}},
		{`INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic import cancel')`, []any{tenant}},
		{`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'import site')`, []any{tenant, site}},
		{`INSERT INTO supervisor.operational_source_heads(tenant_id,site_id,source_id,current_revision) VALUES($1,$2,$3,1)`, []any{tenant, site, sourceID}},
		{`INSERT INTO supervisor.operational_import_jobs(tenant_id,site_id,id,source_id,format,file_sha256,request_sha256,idempotency_key,actor_issuer,actor_subject,source_revision,mapping_revision,start_sequence,row_count,status) VALUES($1,$2,$3,$4,'csv',repeat('a',64),repeat('b',64),'k','https://issuer.invalid','subject',1,1,1,3,'running')`, []any{tenant, site, jobID, sourceID}},
	} {
		if _, err = tx.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s := New(runtime)
	sc := Scope{TenantID: tenant, SiteID: site}
	if err = s.recordRows(ctx, sc, jobID, []ImportRow{{Index: 1, Status: "accepted", SourceSequence: 1}}); err != nil {
		t.Fatalf("running job refused a row: %v", err)
	}
	// The revocation transaction cancels the job (as operational.CancelImports does).
	if _, err = admin.ExecContext(ctx, `DO $$ BEGIN PERFORM set_config('supervisor.tenant_id','`+tenant+`',true);
 UPDATE supervisor.operational_import_jobs SET status='cancelled',accepted_rows=1,finished_at=clock_timestamp() WHERE id='`+jobID+`'; END $$`); err != nil {
		t.Fatal(err)
	}
	if err = s.recordRows(ctx, sc, jobID, []ImportRow{{Index: 2, Status: "accepted", SourceSequence: 2}}); !errors.Is(err, ErrImportCancelled) {
		t.Fatalf("cancelled job accepted a row: %v", err)
	}
	var rows int
	if err = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.operational_import_rows WHERE job_id=$1`, jobID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows after cancellation=%d err=%v", rows, err)
	}
	// A cancelled job is final: it can never be finished as completed afterwards.
	if _, err = admin.ExecContext(ctx, `DO $$ BEGIN PERFORM set_config('supervisor.tenant_id','`+tenant+`',true);
 UPDATE supervisor.operational_import_jobs SET status='completed' WHERE id='`+jobID+`'; END $$`); err == nil {
		t.Fatal("a cancelled import job was reopened")
	}
}
