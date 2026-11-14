//go:build integration

package migrate

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestWIS702LimitProfileUpgrade preserves populated run history under the non-superuser migrator.
func TestWIS702LimitProfileUpgrade(t *testing.T) {
	db := migratorDB(t)
	ctx := context.Background()
	if err := applyThrough(ctx, db, 81); err != nil {
		t.Fatal(err)
	}
	tenant, site, run := uuid.NewString(), uuid.NewString(), uuid.NewString()
	scoped := func(fn func(*sql.Tx) error) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
			return err
		}
		if err = fn(tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err := scoped(func(tx *sql.Tx) error {
		for _, step := range []struct {
			q    string
			args []any
		}{
			{`INSERT INTO supervisor.tenants(id,name) VALUES($1,'profile upgrade')`, []any{tenant}},
			{`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'site')`, []any{tenant, site}},
			{`INSERT INTO supervisor.agent_runs(tenant_id,site_id,id,request_kind,conversation_id,request_id,trace_id,provider_name,provider_actual,model,model_revision,prompt_version,tool_version,policy_revision,invocations_left,tool_calls_left,requests_left,tokens_left,cost_left_microusd)
 VALUES($1,$2,$3,'conversation',gen_random_uuid(),gen_random_uuid(),$4,'local-openai-compatible',true,'unsloth/Qwen3.5-9B-GGUF:Q4_K_M','sha256:03b74727a860a56338e042c4420bb3f04b2fec5734175f4cb9fa853daf52b7e8','p','t',1,3,12,6,24000,1000000)`, []any{tenant, site, run, strings.Repeat("a", 32)}},
		} {
			if _, err := tx.ExecContext(ctx, step.q, step.args...); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := Check(ctx, db); err != nil {
		t.Fatal(err)
	}
	assertTenantTablesForced(t, db)
	if err := scoped(func(tx *sql.Tx) error {
		var profile string
		var version, tokens int
		if err := tx.QueryRowContext(ctx, `SELECT limit_profile_id,limit_profile_version,tokens_left FROM supervisor.agent_runs WHERE id=$1`, run).Scan(&profile, &version, &tokens); err != nil {
			return err
		}
		if profile != "default" || version != 1 || tokens != 24000 {
			t.Fatalf("old provenance changed: %s/%d tokens=%d", profile, version, tokens)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Even the owner cannot rewrite the model or the recorded profile of a running run.
	for _, assignment := range []string{"limit_profile_version=2", "model_revision='changed'", "limit_profile_id='wis702-local-qwen35-9b'"} {
		err := scoped(func(tx *sql.Tx) error {
			_, e := tx.ExecContext(ctx, `UPDATE supervisor.agent_runs SET `+assignment+` WHERE id=$1`, run)
			return e
		})
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "23514" {
			t.Fatalf("rewrite %s: %v", assignment, err)
		}
	}
}
