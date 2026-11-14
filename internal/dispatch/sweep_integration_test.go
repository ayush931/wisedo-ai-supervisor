//go:build integration

package dispatch

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/cases"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
	"go.temporal.io/sdk/client"
)

// WIS-636 (PR #232 review): one Tick with an agent-enabled worker moves a detected case with a
// READY context to investigating and relays the move's signal in the same tick, because the
// sweep runs before the case relay.
func TestTickStartsDetectedCaseAndRelaysItsSignal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	adminURL, runtimeURL, address := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL"), os.Getenv("SUPERVISOR_TEST_TEMPORAL_ADDRESS")
	if adminURL == "" || runtimeURL == "" || address == "" {
		t.Fatal("BLOCKED: real PostgreSQL and Temporal service addresses are required")
	}
	root, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	database := "case_sweep_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = root.ExecContext(ctx, "CREATE DATABASE "+database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if _, e := root.ExecContext(cleanup, "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
		root.Close()
	})
	inDatabase := func(raw string) string {
		u, e := url.Parse(raw)
		if e != nil {
			t.Fatal(e)
		}
		u.Path = "/" + database
		return u.String()
	}
	control, err := sql.Open("pgx", inDatabase(adminURL))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if err = migrate.Apply(ctx, control); err != nil {
		t.Fatal(err)
	}
	db, err := store.New(inDatabase(runtimeURL))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// A detected case at revision 1 bound to a READY context, with its start queued. The rows are
	// planted with FK triggers off: the trigger path is covered by the case HTTP suite.
	tenant, site, line, order, contextID, sourceID, caseID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	digest := strings.Repeat("a", 64)
	snapshot := `{"readiness":"READY","digest":"` + digest + `","context_id":"` + contextID + `","order_id":"` + order + `","asset_id":"` + line + `","site_id":"` + site + `","actual_good_units_exact":"80","deficit_good_units_exact":"20"}`
	tx, err := control.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	for _, s := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO supervisor.tenants(id,name) VALUES($1,'case sweep test')`, []any{tenant}},
		{`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'case sweep site')`, []any{tenant, site}},
		{`INSERT INTO supervisor.knowledge_entity_heads(tenant_id,site_id,id,current_revision) VALUES($1,$2,$3,1),($1,$2,$4,1)`, []any{tenant, site, line, order}},
		{`INSERT INTO supervisor.plant_contexts(tenant_id,site_id,id,order_id,asset_id,window_start,window_end,process_snapshot_id,process_snapshot_sha256,readiness,snapshot,digest,actor_issuer,actor_subject,evaluated_at) VALUES($1,$2,$3,$4,$5,'2026-10-20T06:00:00Z','2026-10-20T06:15:00Z',gen_random_uuid(),repeat('d',64),'READY',$6::jsonb,$7,'test','operator','2026-10-20T06:15:00Z')`, []any{tenant, site, contextID, order, line, snapshot, digest}},
		{`INSERT INTO supervisor.cases(tenant_id,site_id,id,line_id,order_id,exception_kind,episode,revision,context_snapshot_id,context_revision,context_digest,state,detected_at,latest_window_end,workflow_id) VALUES($1,$2,$3,$4,$5,'production_shortfall',1,1,$6,1,$7,'detected',now(),now(),$8)`, []any{tenant, site, caseID, line, order, contextID, digest, "case:" + tenant + ":" + caseID}},
		{`INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,actor_issuer,actor_subject) VALUES($1,$2,$3,1,'none','detected','trigger','svc','ingestion')`, []any{tenant, site, caseID}},
		{`INSERT INTO supervisor.case_trigger_evidence(tenant_id,site_id,case_id,source_id,source_sequence) VALUES($1,$2,$3,$4,1)`, []any{tenant, site, caseID, sourceID}},
		{`INSERT INTO supervisor.case_outbox(tenant_id,site_id,id,case_id,case_revision,kind,event_id) VALUES($1,$2,gen_random_uuid(),$3,1,'start',gen_random_uuid())`, []any{tenant, site, caseID}},
	} {
		if _, err = tx.ExecContext(ctx, s.q, s.args...); err != nil {
			t.Fatalf("%s: %v", s.q, err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}

	c, err := client.Dial(client.Options{HostPort: address, Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// The worker is built (to learn AgentEnabled from its runtime) but not started, so nothing
	// else moves the case after the tick.
	cfg := config.Config{TemporalQueue: "case-sweep-test-" + uuid.NewString(), Tenants: []string{tenant},
		Agent: config.Agent{Provider: config.AgentProviderFixture, ModeEnabled: true}}
	w, err := NewWorker(c, db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !w.AgentEnabled {
		t.Fatal("fixture runtime with agent mode on is not enabled")
	}
	if _, err = Tick(ctx, db, c, cfg, w.AgentEnabled); err != nil {
		t.Fatal(err)
	}
	var state, event, issuer string
	var revision int64
	if err = control.QueryRowContext(ctx, `SELECT c.state,c.revision,t.event,t.actor_issuer FROM supervisor.cases c JOIN supervisor.case_transitions t ON (t.tenant_id,t.site_id,t.case_id,t.revision)=(c.tenant_id,c.site_id,c.id,c.revision) WHERE c.id=$1`, caseID).Scan(&state, &revision, &event, &issuer); err != nil {
		t.Fatal(err)
	}
	if state != string(cases.Investigating) || revision != 2 || event != string(cases.ContextReady) || issuer != cases.SweepActor.Issuer {
		t.Fatalf("after one tick: %s r%d by %s/%s, want investigating r2 context_ready by the sweep", state, revision, event, issuer)
	}
	var delivered int
	if err = control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.case_outbox WHERE case_id=$1 AND case_revision=2 AND kind='signal' AND delivered_at IS NOT NULL`, caseID).Scan(&delivered); err != nil {
		t.Fatal(err)
	}
	if delivered != 1 {
		t.Fatalf("the move's signal was delivered %d times in the same tick, want 1", delivered)
	}
}
