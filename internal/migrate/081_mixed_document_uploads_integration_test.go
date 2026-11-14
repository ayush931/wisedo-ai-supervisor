//go:build integration

package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// WIS-645 slice 2: 081 upgrades a populated 080 database as the non-superuser migration owner. A
// scan job queued before it has no upload and still finishes. As the runtime role, a mixed upload
// and its page jobs can be stored; the link and the upload row never change; another tenant
// neither reads the upload nor links a job to it.
func TestMixedDocumentUploadsUpgradeFrom080(t *testing.T) {
	db := migratorDB(t)
	ctx := context.Background()
	if err := applyThrough(ctx, db, 80); err != nil {
		t.Fatal(err)
	}
	tenant, other, site, otherSite, oldRun, oldJob := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	run := func(id string) string {
		return `INSERT INTO supervisor.agent_runs(tenant_id,site_id,id,request_kind,conversation_id,request_id,trace_id,provider_name,provider_actual,model,model_revision,prompt_version,tool_version,policy_revision,invocations_left,tool_calls_left,requests_left,tokens_left,cost_left_microusd)
 VALUES($1,$2,'` + id + `','extraction',gen_random_uuid(),gen_random_uuid(),'` + strings.Repeat("a", 32) + `','fixture',false,'vision','r','p','t',1,1,1,1,1,1)`
	}
	job := `INSERT INTO supervisor.document_scan_jobs(tenant_id,site_id,job_id,uploader_issuer,uploader_subject,idempotency_key,title,original_sha256,media_type,width,height,image,run_id`
	exec := func(conn interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	}, q string, args ...any) error {
		_, err := conn.ExecContext(ctx, q, args...)
		return err
	}
	seed, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Rollback()
	for _, step := range []struct {
		q    string
		args []any
	}{
		{`SELECT set_config('supervisor.tenant_id',$1,true)`, []any{tenant}},
		{`INSERT INTO supervisor.tenants(id,name) VALUES($1,'upgrade')`, []any{tenant}},
		{`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'plant')`, []any{tenant, site}},
		{run(oldRun), []any{tenant, site}},
		{job + `) VALUES($1,$2,$3,'i','s','k-old','Photo',repeat('c',64),'image/png',10,10,'\x89'::bytea,$4)`, []any{tenant, site, oldJob, oldRun}},
	} {
		if err = exec(seed, step.q, step.args...); err != nil {
			t.Fatalf("%s: %v", step.q, err)
		}
	}
	if err = seed.Commit(); err != nil {
		t.Fatal(err)
	}
	// A real second tenant and site, so its link below can fail only on the upload it names.
	seedOther, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer seedOther.Rollback()
	for _, q := range []string{`SELECT set_config('supervisor.tenant_id',$1,true)`, `INSERT INTO supervisor.tenants(id,name) VALUES($1,'other')`} {
		if err = exec(seedOther, q, other); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err = exec(seedOther, `INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'other plant')`, other, otherSite); err != nil {
		t.Fatal(err)
	}
	otherRun := uuid.NewString()
	if err = exec(seedOther, run(otherRun), other, otherSite); err != nil {
		t.Fatal(err)
	}
	if err = seedOther.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = Apply(ctx, db); err != nil {
		t.Fatalf("upgrade populated 080 as the migration owner: %v", err)
	}
	if err = Check(ctx, db); err != nil {
		t.Fatal(err)
	}
	assertTenantTablesForced(t, db)

	rt := runtimeDB(t, db)
	in := func(tenantID string, fn func(*sql.Tx) error) error {
		tx, err := rt.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenantID); err != nil {
			return err
		}
		if err = fn(tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	// A job queued before 081 has no upload and still finishes.
	if err = in(tenant, func(tx *sql.Tx) error {
		var link sql.NullString
		if e := tx.QueryRowContext(ctx, `SELECT upload_id::text FROM supervisor.document_scan_jobs WHERE job_id=$1`, oldJob).Scan(&link); e != nil || link.Valid {
			t.Fatalf("pre-081 job upload_id %v %v", link, e)
		}
		return exec(tx, `UPDATE supervisor.document_scan_jobs SET state='quarantined',quarantine_reason='PAGE_ILLEGIBLE',image=NULL,finished_at=now() WHERE job_id=$1`, oldJob)
	}); err != nil {
		t.Fatalf("finish a pre-081 job: %v", err)
	}

	// The runtime stores a mixed upload: its typed source, the upload row and a page job linked to it.
	source, upload, pageRun, pageJob := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	if err = in(tenant, func(tx *sql.Tx) error {
		for _, q := range []struct {
			q    string
			args []any
		}{
			{`INSERT INTO supervisor.evidence_heads(tenant_id,site_id,source_id,current_revision,state_revision) VALUES($1,$2,$3,1,1)`, []any{tenant, site, source}},
			{`INSERT INTO supervisor.evidence_revisions(tenant_id,site_id,source_id,revision,kind,title,body,sha256,byte_length,rune_length,actor_issuer,actor_subject,independence_group_id) VALUES($1,$2,$3,1,'document','Receiving SOP','typed',encode(sha256('typed'),'hex'),5,5,'i','s',$3)`, []any{tenant, site, source}},
			{`INSERT INTO supervisor.document_uploads(tenant_id,site_id,upload_id,uploader_issuer,uploader_subject,idempotency_key,title,original_sha256,typed_source_id,page_count) VALUES($1,$2,$3,'i','s','k-mixed','Receiving SOP',repeat('d',64),$4,3)`, []any{tenant, site, upload, source}},
			{run(pageRun), []any{tenant, site}},
			{job + `,upload_id) VALUES($1,$2,$3,'i','s','k-mixed#2','Receiving SOP (page 2 of 3)',repeat('e',64),'image/png',10,10,'\x89'::bytea,$4,$5)`, []any{tenant, site, pageJob, pageRun, upload}},
		} {
			if e := exec(tx, q.q, q.args...); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("store a mixed upload as the runtime: %v", err)
	}

	for name, c := range map[string]struct {
		tenant, q, want string
		args            []any
	}{
		// The runtime may update only a job's state columns; upload_id is not among them.
		"the runtime cannot change the link": {tenant, `UPDATE supervisor.document_scan_jobs SET upload_id=NULL,state='quarantined',quarantine_reason='PAGE_ILLEGIBLE',image=NULL,finished_at=now() WHERE job_id=$1`, "42501", []any{pageJob}},
		"an upload is never updated":         {tenant, `UPDATE supervisor.document_uploads SET title='Renamed' WHERE upload_id=$1`, "42501", []any{upload}},
		"an upload is never deleted":         {tenant, `DELETE FROM supervisor.document_uploads WHERE upload_id=$1`, "42501", []any{upload}},
		"another tenant cannot link to it":   {other, job + `,upload_id) VALUES($1,$2,gen_random_uuid(),'i','s','k-x','X',repeat('f',64),'image/png',10,10,'\x89'::bytea,$3,$4)`, "23503", []any{other, otherSite, otherRun, upload}},
	} {
		t.Run(name, func(t *testing.T) {
			if e := in(c.tenant, func(tx *sql.Tx) error { return exec(tx, c.q, c.args...) }); sqlState(e) != c.want {
				t.Fatalf("got %v (state %q), want %s", e, sqlState(e), c.want)
			}
		})
	}
	// Even the table owner, which may update any column, cannot move a job to another upload: the
	// guard trigger refuses it.
	owner, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE supervisor.document_scan_jobs SET upload_id=NULL,state='quarantined',quarantine_reason='PAGE_ILLEGIBLE',image=NULL,finished_at=now() WHERE job_id=$1`, pageJob); sqlState(err) != "23514" {
		t.Fatalf("owner moved a job off its upload: %v", err)
	}
	_ = owner.Rollback()
	// 081 redefines 078's guard: presented_sha256 (WIS-625) stays immutable beside upload_id.
	owner, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ExecContext(ctx, `UPDATE supervisor.document_scan_jobs SET presented_sha256=repeat('9',64),state='quarantined',quarantine_reason='PAGE_ILLEGIBLE',image=NULL,finished_at=now() WHERE job_id=$1`, pageJob); sqlState(err) != "23514" {
		t.Fatalf("owner changed a job's presented digest after 081: %v", err)
	}
	_ = owner.Rollback()
	// Control: the same insert for the other tenant without the foreign upload succeeds, so the
	// refusal above is the upload link and nothing else.
	if err = in(other, func(tx *sql.Tx) error {
		return exec(tx, job+`) VALUES($1,$2,gen_random_uuid(),'i','s','k-y','Y',repeat('f',64),'image/png',10,10,'\x89'::bytea,$3)`, other, otherSite, otherRun)
	}); err != nil {
		t.Fatalf("control insert: %v", err)
	}
	// The job can still finish with its link unchanged.
	if err = in(tenant, func(tx *sql.Tx) error {
		return exec(tx, `UPDATE supervisor.document_scan_jobs SET state='quarantined',quarantine_reason='PAGE_ILLEGIBLE',image=NULL,finished_at=now() WHERE job_id=$1`, pageJob)
	}); err != nil {
		t.Fatalf("finish a linked job: %v", err)
	}
	var seen int
	if err = in(other, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.document_uploads`).Scan(&seen)
	}); err != nil || seen != 0 {
		t.Fatalf("another tenant reads %d uploads (%v)", seen, err)
	}
}
