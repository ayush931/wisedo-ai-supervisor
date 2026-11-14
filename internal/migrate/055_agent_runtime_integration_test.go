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

// TestAgentRuntimeUpgradeFrom054AndGuards upgrades a populated 054 database to 055 and proves,
// as the runtime role, the WIS-275 storage boundary: forced RLS with cross-tenant zero rows,
// composite FKs to the tenant's own case, column-limited UPDATE, the case/conversation shape,
// one run per case revision, terminal runs and settled calls frozen, and append-only history.
func TestAgentRuntimeUpgradeFrom054AndGuards(t *testing.T) {
	raw := os.Getenv("SUPERVISOR_TEST_ADMIN_URL")
	if raw == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL URL required")
	}
	root, err := sql.Open("pgx", raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	name := "agent_runtime_migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = root.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, e := root.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	})
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	admin, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	ctx := context.Background()
	if err = applyThrough(ctx, admin, 54); err != nil {
		t.Fatal(err)
	}
	c := seedApprovalCase(t, admin) // populated before 055
	if err = Apply(ctx, admin); err != nil {
		t.Fatalf("upgrade 054 -> 055: %v", err)
	}
	if err = Check(ctx, admin); err != nil {
		t.Fatal(err)
	}
	assertTenantTablesForced(t, admin)
	// Column-level UPDATE grants do not show as a table privilege, so every table has the
	// append-only table profile; the granted columns are checked next.
	assertGrants(t, admin, appendOnly, "agent_runs", "agent_run_refs", "agent_steps", "agent_model_calls", "agent_tool_calls", "agent_artifacts")
	for table, want := range map[string]string{
		"agent_runs":        "artifact_id,cost_left_microusd,ended_at,invocations_left,outcome_reasons,outcome_status,proposal_id,requests_left,revocation_reason,revoked_after_dispatch,state,tokens_left,tool_calls_left",
		"agent_model_calls": "charged_microusd,class,ended_at,http_status,input_tokens,output_tokens,reason_code,response_sha256,status,wall_ms",
	} {
		var got string
		if err = admin.QueryRow(`SELECT string_agg(column_name,',' ORDER BY column_name) FROM information_schema.column_privileges WHERE grantee='supervisor_runtime' AND table_schema='supervisor' AND table_name=$1 AND privilege_type='UPDATE'`, table).Scan(&got); err != nil || got != want {
			t.Fatalf("%s runtime UPDATE columns = %q (%v), want %q", table, got, err, want)
		}
	}

	runtime := runtimeDB(t, admin)
	as := func(tenant string, statements ...string) func(args ...[]any) error {
		return func(args ...[]any) error {
			tx, e := runtime.Begin()
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback()
			setTenant(t, tx, tenant)
			for i, s := range statements {
				if _, e = tx.Exec(s, args[i]...); e != nil {
					return e
				}
			}
			return tx.Commit()
		}
	}
	countAs := func(tenant, q string, args ...any) int {
		t.Helper()
		tx, e := runtime.Begin()
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		setTenant(t, tx, tenant)
		var n int
		if e = tx.QueryRow(q, args...).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	trace := strings.Repeat("a1", 16)
	insertRun := `INSERT INTO supervisor.agent_runs(tenant_id,site_id,id,request_kind,case_id,case_revision,conversation_id,request_id,trace_id,provider_name,provider_actual,model,model_revision,prompt_version,tool_version,policy_revision,invocations_left,tool_calls_left,requests_left,tokens_left,cost_left_microusd)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'deterministic-fixture',false,'fixture-model','sha256:fixture','p1','t1',1,3,12,6,24000,1000000)`
	caseRun := func(tenant, site, id, caseID string) []any {
		return []any{tenant, site, id, "case", caseID, 3, nil, nil, trace}
	}
	run := uuid.NewString()
	if err = as(c.tenant, insertRun)(caseRun(c.tenant, c.site, run, c.caseID)); err != nil {
		t.Fatalf("case run for the tenant's own case: %v", err)
	}

	// Forced RLS: another tenant's runtime transaction sees zero rows, and cannot attach a run
	// to this tenant's case through the composite FK.
	other, otherSite := createSite(t, admin)
	if n := countAs(c.tenant, `SELECT count(*) FROM supervisor.agent_runs WHERE id=$1`, run); n != 1 {
		t.Fatalf("own tenant sees %d runs", n)
	}
	if n := countAs(other, `SELECT count(*) FROM supervisor.agent_runs WHERE id=$1`, run); n != 0 {
		t.Fatalf("cross-tenant runtime read saw %d agent_runs rows", n)
	}
	wantState(t, as(other, insertRun)(caseRun(other, otherSite, uuid.NewString(), c.caseID)), "23503", "run bound to another tenant's case")
	wantState(t, as(other, insertRun)(caseRun(c.tenant, c.site, uuid.NewString(), c.caseID)), "42501", "row written into another tenant")

	// Request shape and uniqueness.
	wantState(t, as(c.tenant, insertRun)([]any{c.tenant, c.site, uuid.NewString(), "conversation", c.caseID, 3, uuid.NewString(), uuid.NewString(), trace}), "23514", "conversation run that names a case")
	wantState(t, as(c.tenant, insertRun)([]any{c.tenant, c.site, uuid.NewString(), "case", nil, nil, nil, nil, trace}), "23514", "case run without a case")
	wantState(t, as(c.tenant, insertRun)(caseRun(c.tenant, c.site, uuid.NewString(), c.caseID)), "23505", "second run for one case revision")
	if err = as(c.tenant, insertRun)([]any{c.tenant, c.site, uuid.NewString(), "conversation", nil, nil, uuid.NewString(), uuid.NewString(), trace}); err != nil {
		t.Fatalf("conversation run without a case: %v", err)
	}

	// Column-limited UPDATE: counters move, identity and provenance columns do not.
	scoped := []any{c.tenant, c.site, run}
	if err = as(c.tenant, `UPDATE supervisor.agent_runs SET tokens_left=tokens_left-100 WHERE tenant_id=$1 AND site_id=$2 AND id=$3`)(scoped); err != nil {
		t.Fatalf("granted counter update: %v", err)
	}
	for _, column := range []string{"trace_id='" + strings.Repeat("b2", 16) + "'", "provider_actual=true", "model='other-model'", "case_revision=4", "policy_revision=2"} {
		wantState(t, as(c.tenant, `UPDATE supervisor.agent_runs SET `+column+` WHERE tenant_id=$1 AND site_id=$2 AND id=$3`)(scoped), "42501", "runtime UPDATE of "+column)
	}
	wantState(t, as(c.tenant, `UPDATE supervisor.agent_runs SET tokens_left=-1 WHERE tenant_id=$1 AND site_id=$2 AND id=$3`)(scoped), "23514", "negative token counter")

	// Model calls settle once.
	insertCall := `INSERT INTO supervisor.agent_model_calls(tenant_id,site_id,run_id,seq,attempt,span_id,provider_actual,model,model_revision,request_sha256,reserved_tokens,reserved_microusd,cost_basis) VALUES($1,$2,$3,1,1,$4,false,'fixture-model','sha256:fixture',$5,3000,0,'local')`
	if err = as(c.tenant, insertCall)([]any{c.tenant, c.site, run, strings.Repeat("c3", 8), strings.Repeat("d", 64)}); err != nil {
		t.Fatal(err)
	}
	settleCharged := func(micros string) string {
		return `UPDATE supervisor.agent_model_calls SET status='completed',input_tokens=1,output_tokens=1,charged_microusd=` + micros + `,ended_at=clock_timestamp() WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3`
	}
	wantState(t, as(c.tenant, settleCharged("5"))(scoped), "23514", "local call charged")
	settle := settleCharged("0")
	if err = as(c.tenant, settle)(scoped); err != nil {
		t.Fatalf("settle: %v", err)
	}
	wantState(t, as(c.tenant, settle)(scoped), "23514", "second settlement of one call")

	// Append-only trace and artifacts; an artifact binds only to its own run.
	insertStep := `INSERT INTO supervisor.agent_steps(tenant_id,site_id,run_id,seq,span_id,role,hop,envelope_sha256,status,started_at,ended_at) VALUES($1,$2,$3,1,$4,'investigation',1,$5,'completed',clock_timestamp(),clock_timestamp())`
	if err = as(c.tenant, insertStep)([]any{c.tenant, c.site, run, strings.Repeat("e4", 8), strings.Repeat("f", 64)}); err != nil {
		t.Fatal(err)
	}
	wantState(t, as(c.tenant, `UPDATE supervisor.agent_steps SET status='failed' WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3`)(scoped), "42501", "rewriting a trace step")
	wantState(t, as(c.tenant, `DELETE FROM supervisor.agent_steps WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3`)(scoped), "42501", "deleting a trace step")
	conversation := uuid.NewString()
	if err = as(c.tenant, insertRun)([]any{c.tenant, c.site, conversation, "conversation", nil, nil, uuid.NewString(), uuid.NewString(), trace}); err != nil {
		t.Fatal(err)
	}
	foreignArtifact := uuid.NewString()
	if err = as(c.tenant, `INSERT INTO supervisor.agent_artifacts(tenant_id,site_id,id,run_id,schema,body,sha256) VALUES($1,$2,$3,$4,'SpecialistOutputV1','{}',$5)`)([]any{c.tenant, c.site, foreignArtifact, conversation, strings.Repeat("0", 64)}); err != nil {
		t.Fatal(err)
	}
	wantState(t, as(c.tenant, `UPDATE supervisor.agent_runs SET artifact_id=$4 WHERE tenant_id=$1 AND site_id=$2 AND id=$3`)(append(scoped, foreignArtifact)), "23503", "run citing another run's artifact")

	// A terminal run is frozen: no counter, outcome or state change afterwards.
	if err = as(c.tenant, `UPDATE supervisor.agent_runs SET state='degraded',outcome_status='escalated',outcome_reasons=ARRAY['PROVIDER_FAILED'],ended_at=clock_timestamp() WHERE tenant_id=$1 AND site_id=$2 AND id=$3`)(scoped); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	for _, set := range []string{"tokens_left=0", "state='running',ended_at=NULL", "outcome_status='proposed'"} {
		wantState(t, as(c.tenant, `UPDATE supervisor.agent_runs SET `+set+` WHERE tenant_id=$1 AND site_id=$2 AND id=$3`)(scoped), "23514", "terminal run "+set)
	}
}
