//go:build integration

package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestWIS614AgentRunSpansMigration applies 071 over a database that already holds runs and
// evidence, then proves the storage boundary of captured document passages: forced RLS and the
// append-only runtime grants, a passage must be a span of the source revision it names, at most
// eight passages per run, rows are immutable, and another tenant sees none of them.
func TestWIS614AgentRunSpansMigration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratorDB(t)
	if err := applyThrough(ctx, db, 70); err != nil {
		t.Fatal(err)
	}
	tenant, site, run := uuid.NewString(), uuid.NewString(), uuid.NewString()
	source, other, span, otherSpan := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	asTenant := func(id string, statements ...string) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, id); err != nil {
			return err
		}
		for _, s := range statements {
			if _, err = tx.ExecContext(ctx, s); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	evidence := func(sourceID, spanID string) []string {
		body := "Steel coil is received at Gate 2."
		sum := sha256.Sum256([]byte(body))
		return []string{
			`INSERT INTO supervisor.evidence_heads(tenant_id,site_id,source_id,current_revision,state_revision) VALUES('` + tenant + `','` + site + `','` + sourceID + `',1,1)`,
			`INSERT INTO supervisor.evidence_revisions(tenant_id,site_id,source_id,revision,kind,title,body,sha256,byte_length,rune_length,actor_issuer,actor_subject,authority_class,independence_group_id)
 VALUES('` + tenant + `','` + site + `','` + sourceID + `',1,'document','SOP','` + body + `','` + hex.EncodeToString(sum[:]) + `',33,33,'https://idp.test','u','reported','` + sourceID + `')`,
			`INSERT INTO supervisor.evidence_spans(tenant_id,site_id,id,source_id,revision,start_rune,end_rune,actor_issuer,actor_subject) VALUES('` + tenant + `','` + site + `','` + spanID + `','` + sourceID + `',1,0,33,'https://idp.test','u')`,
		}
	}
	seed := []string{
		`INSERT INTO supervisor.tenants(id,name) VALUES('` + tenant + `','run spans')`,
		`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES('` + tenant + `','` + site + `','site')`,
		`INSERT INTO supervisor.agent_runs(tenant_id,site_id,id,request_kind,conversation_id,request_id,trace_id,provider_name,provider_actual,model,model_revision,prompt_version,tool_version,policy_revision,invocations_left,tool_calls_left,requests_left,tokens_left,cost_left_microusd)
 VALUES('` + tenant + `','` + site + `','` + run + `','conversation','` + uuid.NewString() + `','` + uuid.NewString() + `','` + strings.Repeat("a", 32) + `','fixture',false,'m','r','p','t',1,1,1,1,1,1)`,
	}
	seed = append(seed, evidence(source, span)...)
	seed = append(seed, evidence(other, otherSpan)...)
	if err := asTenant(tenant, seed...); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("071 over existing runs and evidence: %v", err)
	}
	if err := Check(ctx, db); err != nil {
		t.Fatal(err)
	}
	assertGrants(t, db, appendOnly, "agent_run_spans")

	passage := func(ordinal, spanID, sourceID string) string {
		return `INSERT INTO supervisor.agent_run_spans(tenant_id,site_id,run_id,ordinal,span_id,source_id,source_revision) VALUES('` + tenant + `','` + site + `','` + run + `',` + ordinal + `,'` + spanID + `','` + sourceID + `',1)`
	}
	if err := asTenant(tenant, passage("1", span, source)); err != nil {
		t.Fatalf("a span of its own source revision is a valid passage: %v", err)
	}
	code := func(err error) string {
		var pg *pgconn.PgError
		if errors.As(err, &pg) {
			return pg.Code
		}
		return ""
	}
	for name, c := range map[string]struct {
		statement, code string
	}{
		"span of another source":   {passage("2", otherSpan, source), "23503"},
		"ninth passage":            {passage("9", otherSpan, other), "23514"},
		"ordinal used twice":       {passage("1", otherSpan, other), "23505"},
		"rewrite a passage":        {`UPDATE supervisor.agent_run_spans SET ordinal=2 WHERE run_id='` + run + `'`, "42501"},
		"same span captured twice": {passage("3", span, source), "23505"},
	} {
		if err := asTenant(tenant, c.statement); code(err) != c.code {
			t.Errorf("%s: %v, want SQLSTATE %s", name, err, c.code)
		}
	}
	var seen int
	err := func() error {
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, uuid.NewString()); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.agent_run_spans`).Scan(&seen)
	}()
	if err != nil || seen != 0 {
		t.Fatalf("another tenant saw %d passages (%v)", seen, err)
	}
}
