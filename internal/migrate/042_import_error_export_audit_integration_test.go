//go:build integration

package migrate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// WIS-272 import error export audit: 042 upgrades a populated database as the non-superuser
// migration owner, keeps existing audit rows, and the runtime role can append an export row
// only with an in-scope job and a row count; other event types cannot carry export fields.
func TestImportErrorExportAuditUpgrade(t *testing.T) {
	db := migratorDB(t)
	ctx := context.Background()
	if err := applyThrough(ctx, db, 41); err != nil {
		t.Fatal(err)
	}
	tenant, site := createSite(t, db)
	_, otherSite := createSite(t, db)
	source, job := uuid.NewString(), uuid.NewString()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	setTenant(t, tx, tenant)
	insertMapHeaders(t, tx, tenant, site, source, emptyMappingDigest())
	// A pre-042 audit row and a finished import job.
	if _, err = tx.Exec(`INSERT INTO supervisor.operational_source_audit(tenant_id,site_id,actor_issuer,actor_subject,source_id,revision,event_type,source_revision) VALUES($1,$2,'test','operator',$3,1,'source.read',1)`, tenant, site, source); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO supervisor.operational_import_jobs(tenant_id,site_id,id,source_id,format,file_sha256,request_sha256,idempotency_key,actor_issuer,actor_subject,source_revision,mapping_revision,start_sequence,row_count,status,finished_at) VALUES($1,$2,$3,$4,'csv',$5,$5,'pre-042','test','operator',1,1,1,3,'partial',clock_timestamp())`, tenant, site, job, source, strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = Apply(ctx, db); err != nil {
		t.Fatalf("upgrade 041 -> 042 as the migration role: %v", err)
	}
	if err = Check(ctx, db); err != nil {
		t.Fatal(err)
	}
	assertTenantTablesForced(t, db)
	runtime := runtimeDB(t, db)

	insert := func(siteID, event, jobID string, rows any) error {
		t.Helper()
		rtx, e := runtime.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer rtx.Rollback()
		if _, e = rtx.Exec(`SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); e != nil {
			t.Fatal(e)
		}
		if _, e = rtx.Exec(`INSERT INTO supervisor.operational_source_audit(tenant_id,site_id,actor_issuer,actor_subject,source_id,revision,event_type,source_revision,job_id,exported_rows) VALUES($1,$2,'test','viewer',$3,1,$4,1,NULLIF($5,'')::uuid,$6)`, tenant, siteID, source, event, jobID, rows); e != nil {
			return e
		}
		return rtx.Commit()
	}
	// Same tenant, a different site with its own real job: the composite (tenant,site,job) FK
	// is what rejects pairing this site's job with the other site.
	site2, job2 := uuid.NewString(), uuid.NewString()
	stx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	setTenant(t, stx, tenant)
	if _, err = stx.Exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'second site')`, tenant, site2); err != nil {
		t.Fatal(err)
	}
	source2 := uuid.NewString()
	insertMapHeaders(t, stx, tenant, site2, source2, emptyMappingDigest())
	if _, err = stx.Exec(`INSERT INTO supervisor.operational_import_jobs(tenant_id,site_id,id,source_id,format,file_sha256,request_sha256,idempotency_key,actor_issuer,actor_subject,source_revision,mapping_revision,start_sequence,row_count,status,finished_at) VALUES($1,$2,$3,$4,'csv',$5,$5,'site2','test','operator',1,1,1,1,'completed',clock_timestamp())`, tenant, site2, job2, source2, strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	if err = stx.Commit(); err != nil {
		t.Fatal(err)
	}
	var pgErr *pgconn.PgError
	if e := insert(site, "import.errors_exported", job2, 2); !errors.As(e, &pgErr) || pgErr.Code != "23503" || pgErr.ConstraintName != "operational_source_audit_job_fkey" {
		t.Fatalf("other site's job: %v", e)
	}
	if err = insert(site, "import.errors_exported", job, 2); err != nil {
		t.Fatalf("runtime export audit: %v", err)
	}
	for name, c := range map[string]struct {
		site, event, job string
		rows             any
	}{
		"export without job":                 {site, "import.errors_exported", "", 2},
		"export without row count":           {site, "import.errors_exported", job, nil},
		"export of unknown job":              {site, "import.errors_exported", uuid.NewString(), 2},
		"export under another tenant's site": {otherSite, "import.errors_exported", job, 2},
		"negative row count":                 {site, "import.errors_exported", job, -1},
		"read carrying a job":                {site, "source.read", job, 2},
		"read carrying a row count":          {site, "source.read", "", 2},
	} {
		if e := insert(c.site, c.event, c.job, c.rows); e == nil {
			t.Errorf("%s: runtime insert accepted", name)
		}
	}

	var kept, exports int
	rtx, err := runtime.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rtx.Rollback()
	if _, err = rtx.Exec(`SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		t.Fatal(err)
	}
	if err = rtx.QueryRow(`SELECT count(*) FILTER (WHERE event_type='source.read' AND job_id IS NULL AND exported_rows IS NULL),count(*) FILTER (WHERE event_type='import.errors_exported' AND job_id=$2 AND exported_rows=2) FROM supervisor.operational_source_audit WHERE tenant_id=$1`, tenant, job).Scan(&kept, &exports); err != nil {
		t.Fatal(err)
	}
	if kept != 1 || exports != 1 {
		t.Fatalf("audit rows: pre-042 kept=%d exports=%d", kept, exports)
	}
}
