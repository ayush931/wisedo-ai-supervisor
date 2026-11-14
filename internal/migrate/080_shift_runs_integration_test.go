//go:build integration

package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestWIS701ShiftRunsUpgradeFrom079AndGuards: 080 upgrades a populated 079 database as the
// non-superuser migration owner without backfilling runs, and the runtime role can only start a
// run on the approved revision and end it once, citing a summary by its exact digest and a
// handover drafted from it. Start columns, deletes, receipts and audit are immutable for it.
func TestWIS701ShiftRunsUpgradeFrom079AndGuards(t *testing.T) {
	db := migratorDB(t)
	ctx := context.Background()
	if err := applyThrough(ctx, db, 79); err != nil {
		t.Fatal(err)
	}
	tenant, site, other := uuid.NewString(), uuid.NewString(), uuid.NewString()
	planA, planB := uuid.NewString(), uuid.NewString()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := tx.ExecContext(ctx, q, args...); e != nil {
			t.Fatalf("%s: %v", q, e)
		}
	}
	exec(`SELECT set_config('supervisor.tenant_id',$1,true)`, tenant)
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'upgrade')`, tenant)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'site')`, tenant, site)
	// Two approved plans (empty assignment lists), each with summary 1; plan A also has
	// summary 2 and handovers 1 (from summary 1) and 2 (from summary 2). Canonical bodies are
	// built in SQL so their digests check.
	for i, plan := range []string{planA, planB} {
		starts, ends := []string{"2026-11-10T06:00:00Z", "2026-11-10T14:00:00Z"}[i], []string{"2026-11-10T14:00:00Z", "2026-11-10T22:00:00Z"}[i]
		line := uuid.NewString()
		exec(`INSERT INTO supervisor.shift_plan_heads(tenant_id,site_id,id,line_id,starts_at,ends_at,current_revision,state_revision) VALUES($1,$2,$3,$4,$5,$6,1,1)`, tenant, site, plan, line, starts, ends)
		exec(`WITH p AS (SELECT jsonb_build_object('schema_version',1,'line_id',$4::text,'starts_at',$5::text,'ends_at',$6::text,'timezone','UTC','assignments','[]'::jsonb) AS body),
 c AS (SELECT body,jsonb_build_object('schema','ShiftPlanBodyV1','schema_version',1,'tenant_id',$1::text,'site_id',$2::text,'body',body)::text AS t FROM p)
 INSERT INTO supervisor.shift_plan_revisions(tenant_id,site_id,plan_id,revision,line_id,starts_at,ends_at,timezone,payload,canonical_payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject)
 SELECT $1::uuid,$2::uuid,$3::uuid,1,$4::uuid,$5::timestamptz,$6::timestamptz,'UTC',body,t,encode(sha256(convert_to(t,'UTF8')),'hex'),1,'i','lead' FROM c`, tenant, site, plan, line, starts, ends)
		exec(`INSERT INTO supervisor.shift_plan_approvals(tenant_id,site_id,plan_id,revision,canonical_payload_sha256,approver_issuer,approver_subject,policy_revision,auth_time)
 SELECT tenant_id,site_id,plan_id,1,canonical_payload_sha256,'i','approver',1,clock_timestamp() FROM supervisor.shift_plan_revisions WHERE tenant_id=$1 AND plan_id=$2`, tenant, plan)
		exec(`UPDATE supervisor.shift_plan_heads SET approved_revision=1,state_revision=2 WHERE tenant_id=$1 AND id=$2`, tenant, plan)
		exec(`WITH c AS (SELECT jsonb_build_object('plan_id',$3::text,'plan_revision',1) AS body),
 e AS (SELECT body,jsonb_build_object('schema','ShiftSummaryV1','schema_version',1,'tenant_id',$1::text,'site_id',$2::text,'body',body)::text AS t FROM c)
 INSERT INTO supervisor.shift_summaries(tenant_id,site_id,plan_id,revision,plan_revision,payload,canonical_payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject)
 SELECT $1::uuid,$2::uuid,$3::uuid,1,1,body,t,encode(sha256(convert_to(t,'UTF8')),'hex'),1,'i','lead' FROM e`, tenant, site, plan)
	}
	exec(`WITH c AS (SELECT jsonb_build_object('plan_id',$3::text,'plan_revision',1,'late',true) AS body),
 e AS (SELECT body,jsonb_build_object('schema','ShiftSummaryV1','schema_version',1,'tenant_id',$1::text,'site_id',$2::text,'body',body)::text AS t FROM c)
 INSERT INTO supervisor.shift_summaries(tenant_id,site_id,plan_id,revision,plan_revision,payload,canonical_payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject)
 SELECT $1::uuid,$2::uuid,$3::uuid,2,1,body,t,encode(sha256(convert_to(t,'UTF8')),'hex'),1,'i','lead' FROM e`, tenant, site, planA)
	for revision := 1; revision <= 2; revision++ {
		exec(`WITH c AS (SELECT jsonb_build_object('plan_id',$3::text,'summary_revision',$4::int) AS body),
 e AS (SELECT body,jsonb_build_object('schema','ShiftHandoverV1','schema_version',1,'tenant_id',$1::text,'site_id',$2::text,'body',body)::text AS t FROM c)
 INSERT INTO supervisor.shift_handovers(tenant_id,site_id,plan_id,revision,summary_revision,payload,canonical_payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject)
 SELECT $1::uuid,$2::uuid,$3::uuid,$4::int,$4::int,body,t,encode(sha256(convert_to(t,'UTF8')),'hex'),1,'i','lead' FROM e`, tenant, site, planA, revision)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = Apply(ctx, db); err != nil {
		t.Fatalf("upgrade populated 079 as the migration owner: %v", err)
	}
	if err = Check(ctx, db); err != nil {
		t.Fatal(err)
	}
	assertTenantTablesForced(t, db)

	runtime := runtimeDB(t, db)
	// step runs one statement as the runtime role in tenant scope and returns its SQLSTATE ("" on success).
	// stepOn runs one statement in tenant scope on conn and returns its SQLSTATE ("" on success).
	stepOn := func(conn *sql.DB, scope, q string, args ...any) string {
		t.Helper()
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, scope); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, q, args...); err != nil {
			if i := strings.Index(err.Error(), "SQLSTATE "); i >= 0 {
				return err.Error()[i+9 : i+14]
			}
			t.Fatalf("%s: %v", q, err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return ""
	}
	count := func(scope, q string, args ...any) int {
		t.Helper()
		tx, err := runtime.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, scope); err != nil {
			t.Fatal(err)
		}
		var n int
		if err = tx.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(tenant, `SELECT count(*) FROM supervisor.shift_runs`); n != 0 {
		t.Fatalf("080 backfilled %d runs", n)
	}
	if n := count(tenant, `SELECT count(*) FROM supervisor.shift_summaries`); n != 3 {
		t.Fatalf("summaries after 080: %d", n)
	}

	insert := `INSERT INTO supervisor.shift_runs(tenant_id,site_id,plan_id,started_plan_revision,started_by_issuer,started_by_subject,start_auth_time) VALUES($1,$2,$3,$4,'i','lead',clock_timestamp())`
	// endRun takes the cited digest ($4), handover revision ($5), ended plan revision ($6) and summary revision ($7).
	endRun := `UPDATE supervisor.shift_runs SET ended_plan_revision=$6,ended_by_issuer='i',ended_by_subject='lead',end_auth_time=clock_timestamp(),ended_at=clock_timestamp(),summary_revision=$7,summary_sha256=$4,handover_revision=$5 WHERE tenant_id=$1 AND site_id=$2 AND plan_id=$3`
	digest := func(plan string, revision int) string {
		t.Helper()
		tx, err := runtime.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		var d string
		if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
			t.Fatal(err)
		}
		if err = tx.QueryRowContext(ctx, `SELECT canonical_payload_sha256 FROM supervisor.shift_summaries WHERE plan_id=$1 AND revision=$2`, plan, revision).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	digestA, digestB := digest(planA, 2), digest(planB, 1)
	// owner cases run as the migration owner, which holds every privilege on the table, so
	// the guard itself (not a missing runtime grant) decides them.
	for _, c := range []struct {
		name, scope, q string
		args           []any
		want           string
		owner          bool
	}{
		{"insert under another tenant's scope", other, insert, []any{tenant, site, planA, 1}, "42501", false},
		{"start on an unapproved revision", tenant, insert, []any{tenant, site, planA, 2}, "23514", false},
		{"start already ended", tenant, `INSERT INTO supervisor.shift_runs(tenant_id,site_id,plan_id,started_plan_revision,started_by_issuer,started_by_subject,start_auth_time,ended_at) VALUES($1,$2,$3,1,'i','lead',now(),now())`, []any{tenant, site, planA}, "23514", false},
		{"start plan A", tenant, insert, []any{tenant, site, planA, 1}, "", false},
		{"start plan A twice", tenant, insert, []any{tenant, site, planA, 1}, "23505", false},
		{"rewrite a start column (no runtime grant)", tenant, `UPDATE supervisor.shift_runs SET started_by_subject='someone' WHERE plan_id=$1`, []any{planA}, "42501", false},
		{"end citing another plan's digest", tenant, endRun, []any{tenant, site, planA, digestB, 2, 1, 2}, "23514", false},
		{"end citing a handover drafted from another summary", tenant, endRun, []any{tenant, site, planA, digestA, 1, 1, 2}, "23514", false},
		{"end citing a handover that does not exist", tenant, endRun, []any{tenant, site, planA, digestA, 3, 1, 2}, "23514", false},
		{"end on a revision other than the approved one", tenant, endRun, []any{tenant, site, planA, digestA, 2, 2, 2}, "23514", false},
		// A valid end that also rewrites a start column: only the start-column check refuses it.
		{"end that rewrites a start column (owner: guard)", tenant, strings.Replace(endRun, "SET ", "SET started_by_subject='someone',", 1), []any{tenant, site, planA, digestA, 2, 1, 2}, "23514", true},
		{"end plan A", tenant, endRun, []any{tenant, site, planA, digestA, 2, 1, 2}, "", false},
		{"end plan A twice", tenant, endRun, []any{tenant, site, planA, digestA, 2, 1, 2}, "23514", false},
		{"delete a run (no runtime grant)", tenant, `DELETE FROM supervisor.shift_runs WHERE plan_id=$1`, []any{planA}, "42501", false},
		{"delete a run (owner: guard)", tenant, `DELETE FROM supervisor.shift_runs WHERE plan_id=$1`, []any{planA}, "42501", true},
		{"receipt", tenant, `INSERT INTO supervisor.shift_run_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_sha256,operation,plan_id,response) VALUES($1,$2,'i','lead','k',repeat('a',64),'start',$3::uuid,jsonb_build_object('plan_id',$3::text))`, []any{tenant, site, planA}, "", false},
		{"receipt whose response names another plan", tenant, `INSERT INTO supervisor.shift_run_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_sha256,operation,plan_id,response) VALUES($1,$2,'i','lead','k2',repeat('a',64),'start',$3::uuid,jsonb_build_object('plan_id',$4::text))`, []any{tenant, site, planA, planB}, "23514", false},
		{"audit", tenant, `INSERT INTO supervisor.shift_run_audit(tenant_id,site_id,actor_issuer,actor_subject,plan_id,event_type) VALUES($1,$2,'i','lead',$3,'shift_run.started')`, []any{tenant, site, planA}, "", false},
		{"update a receipt", tenant, `UPDATE supervisor.shift_run_receipts SET operation='end'`, nil, "42501", false},
		{"delete a receipt", tenant, `DELETE FROM supervisor.shift_run_receipts`, nil, "42501", false},
		{"update audit", tenant, `UPDATE supervisor.shift_run_audit SET event_type='shift_run.read'`, nil, "42501", false},
		{"delete audit", tenant, `DELETE FROM supervisor.shift_run_audit`, nil, "42501", false},
	} {
		conn := runtime
		if c.owner {
			conn = db
		}
		if got := stepOn(conn, c.scope, c.q, c.args...); got != c.want {
			t.Errorf("%s: SQLSTATE %q, want %q", c.name, got, c.want)
		}
	}
	if n := count(other, `SELECT count(*) FROM supervisor.shift_runs`); n != 0 {
		t.Fatalf("another tenant sees %d runs", n)
	}
	if n := count(tenant, `SELECT count(*) FROM supervisor.shift_runs WHERE ended_at IS NOT NULL AND summary_sha256=$1`, digestA); n != 1 {
		t.Fatalf("ended runs %d", n)
	}
}
