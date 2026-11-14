//go:build integration

package migrate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestAgentMachineStateRefsUpgradeWithExistingRefs applies 065 as the non-superuser migration
// role over a database that already holds captured run refs, as any deployed database does. The
// replaced kind CHECK validates existing rows by a direct table scan, which forced RLS does not
// intercept (unlike the FK validations relaxedForRLS exists for), so no relaxation is needed; this
// test keeps that true.
func TestWIS276AgentMachineStateRefsUpgradeWithExistingRefs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratorDB(t)
	if err := applyThrough(ctx, db, 64); err != nil {
		t.Fatal(err)
	}
	tenant, site, run := uuid.NewString(), uuid.NewString(), uuid.NewString()
	insert := func(statements ...string) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
			return err
		}
		for _, s := range statements {
			if _, err = tx.ExecContext(ctx, s); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	if err := insert(
		`INSERT INTO supervisor.tenants(id,name) VALUES('`+tenant+`','machine state upgrade')`,
		`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES('`+tenant+`','`+site+`','site')`,
		`INSERT INTO supervisor.agent_runs(tenant_id,site_id,id,request_kind,conversation_id,request_id,trace_id,provider_name,provider_actual,model,model_revision,prompt_version,tool_version,policy_revision,invocations_left,tool_calls_left,requests_left,tokens_left,cost_left_microusd)
 VALUES('`+tenant+`','`+site+`','`+run+`','conversation','`+uuid.NewString()+`','`+uuid.NewString()+`','`+strings.Repeat("a", 32)+`','fixture',false,'m','r','p','t',1,1,1,1,1,1)`,
		`INSERT INTO supervisor.agent_run_refs(tenant_id,site_id,run_id,kind,ref_id,revision,sha256) VALUES('`+tenant+`','`+site+`','`+run+`','measurement','`+uuid.NewString()+`',1,'`+strings.Repeat("b", 64)+`')`,
	); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("065 upgrade over existing run refs: %v", err)
	}
	if err := Check(ctx, db); err != nil {
		t.Fatal(err)
	}
	var forced bool
	if err := db.QueryRowContext(ctx, `SELECT relforcerowsecurity FROM pg_class WHERE oid='supervisor.agent_run_refs'::regclass`).Scan(&forced); err != nil || !forced {
		t.Fatalf("agent_run_refs must stay FORCE ROW LEVEL SECURITY after 065: forced=%v err=%v", forced, err)
	}
	ref := func(kind string) error {
		return insert(`INSERT INTO supervisor.agent_run_refs(tenant_id,site_id,run_id,kind,ref_id,revision,sha256) VALUES('` + tenant + `','` + site + `','` + run + `','` + kind + `','` + uuid.NewString() + `',1,'` + strings.Repeat("c", 64) + `')`)
	}
	if err := ref("machine_state"); err != nil {
		t.Fatalf("065 must accept a machine_state ref: %v", err)
	}
	if err := ref("operator_report"); err == nil || !strings.Contains(err.Error(), "agent_run_refs_kind_check") {
		t.Fatalf("an unlisted ref kind must still be refused: %v", err)
	}
}
