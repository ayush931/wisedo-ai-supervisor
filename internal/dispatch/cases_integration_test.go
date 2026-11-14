//go:build integration

package dispatch

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/cases"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

func TestAgentWorkflowReplaysHistoryWithoutVersionMarker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	address := os.Getenv("SUPERVISOR_TEST_TEMPORAL_ADDRESS")
	if address == "" {
		t.Fatal("BLOCKED: disposable Temporal address is required")
	}
	c, err := client.Dial(client.Options{HostPort: address, Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	queue, id := "agent-replay-test-"+uuid.NewString(), "agent-replay-"+uuid.NewString()
	w := worker.New(c, queue, worker.Options{})
	w.RegisterWorkflowWithOptions(legacyCaseWorkflow, workflow.RegisterOptions{Name: "SupervisorCaseWorkflow"})
	w.RegisterActivityWithOptions(func(context.Context, cases.WorkflowRef) (int, error) { return 0, nil }, activity.RegisterOptions{Name: "ReconcileCaseEvents"})
	w.RegisterActivityWithOptions(func(context.Context, cases.WorkflowRef) (cases.WorkflowStatus, error) {
		return cases.WorkflowStatus{State: cases.Resolved, Revision: 4, Terminal: true}, nil
	}, activity.RegisterOptions{Name: "ReadCaseStatus"})
	if err = w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: queue}, "SupervisorCaseWorkflow", caseRef())
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Get(ctx, nil); err != nil {
		t.Fatal(err)
	}
	history := &historypb.History{}
	iter := c.GetWorkflowHistory(ctx, id, run.GetRunID(), false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iter.HasNext() {
		event, e := iter.Next()
		if e != nil {
			t.Fatal(e)
		}
		history.Events = append(history.Events, event)
	}
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(SupervisorCaseWorkflow)
	if err = replayer.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatalf("pre-agent history replay: %v", err)
	}
}

// WIS-275 review: a history recorded by 94bbf3f, where an investigation that left the case
// unchanged was re-run at once, still replays after the agent-investigation-wait change.
func TestAgentWorkflowReplaysRepeatedInvestigationHistory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	address := os.Getenv("SUPERVISOR_TEST_TEMPORAL_ADDRESS")
	if address == "" {
		t.Fatal("BLOCKED: disposable Temporal address is required")
	}
	c, err := client.Dial(client.Options{HostPort: address, Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	queue, id := "agent-replay-test-"+uuid.NewString(), "agent-replay-"+uuid.NewString()
	w := worker.New(c, queue, worker.Options{})
	w.RegisterWorkflowWithOptions(spinningAgentCaseWorkflow, workflow.RegisterOptions{Name: "SupervisorCaseWorkflow"})
	w.RegisterActivityWithOptions(func(context.Context, cases.WorkflowRef) (int, error) { return 0, nil }, activity.RegisterOptions{Name: "ReconcileCaseEvents"})
	var reads, investigations atomic.Int32
	w.RegisterActivityWithOptions(func(context.Context, cases.WorkflowRef) (cases.WorkflowStatus, error) {
		if reads.Add(1) <= 2 {
			return cases.WorkflowStatus{State: cases.Investigating, Revision: 2, AgentEnabled: true}, nil
		}
		return cases.WorkflowStatus{State: cases.Resolved, Revision: 4, Terminal: true}, nil
	}, activity.RegisterOptions{Name: "ReadCaseStatus"})
	w.RegisterActivityWithOptions(func(context.Context, InvestigateRef) (agent.Result, error) {
		investigations.Add(1)
		return agent.Result{State: agent.RunCanceled}, nil
	}, activity.RegisterOptions{Name: "InvestigateCase"})
	if err = w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: queue}, "SupervisorCaseWorkflow", caseRef())
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Get(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if investigations.Load() != 2 {
		t.Fatalf("recorded history has %d investigations, want the 94bbf3f repeat (2)", investigations.Load())
	}
	history := &historypb.History{}
	iter := c.GetWorkflowHistory(ctx, id, run.GetRunID(), false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iter.HasNext() {
		event, e := iter.Next()
		if e != nil {
			t.Fatal(e)
		}
		history.Events = append(history.Events, event)
	}
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(SupervisorCaseWorkflow)
	if err = replayer.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatalf("94bbf3f repeated-investigation history replay: %v", err)
	}
}

// spinningAgentCaseWorkflow is the 94bbf3f agent loop: after InvestigateCase it always re-read
// the case and, if still investigating, investigated again.
func spinningAgentCaseWorkflow(ctx workflow.Context, ref cases.WorkflowRef) error {
	actx := workflow.WithActivityOptions(ctx, caseActivity)
	if workflow.GetVersion(ctx, "reconcile-case-outbox", workflow.DefaultVersion, 1) == 1 {
		var added int
		if err := workflow.ExecuteActivity(actx, "ReconcileCaseEvents", ref).Get(ctx, &added); err != nil {
			return err
		}
	}
	for {
		var st cases.WorkflowStatus
		if err := workflow.ExecuteActivity(actx, "ReadCaseStatus", ref).Get(ctx, &st); err != nil {
			return err
		}
		if workflow.GetVersion(ctx, "agent-investigation", workflow.DefaultVersion, 1) == 1 && st.State == cases.Investigating && st.AgentEnabled {
			var result agent.Result
			investigation := InvestigateRef{Tenant: ref.TenantID, Site: ref.SiteID, Case: ref.CaseID, Revision: st.Revision}
			if err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, agentActivity), "InvestigateCase", investigation).Get(ctx, &result); err != nil {
				return err
			}
			continue
		}
		if st.Terminal {
			return nil
		}
		return errors.New("replay fixture requires a terminal case")
	}
}

// legacyCaseWorkflow is the pre-slice-2 workflow shape: it has the outbox version marker but
// no agent-investigation marker or activity.
func legacyCaseWorkflow(ctx workflow.Context, ref cases.WorkflowRef) error {
	actx := workflow.WithActivityOptions(ctx, caseActivity)
	if workflow.GetVersion(ctx, "reconcile-case-outbox", workflow.DefaultVersion, 1) == 1 {
		var added int
		if err := workflow.ExecuteActivity(actx, "ReconcileCaseEvents", ref).Get(ctx, &added); err != nil {
			return err
		}
	}
	var status cases.WorkflowStatus
	if err := workflow.ExecuteActivity(actx, "ReadCaseStatus", ref).Get(ctx, &status); err != nil {
		return err
	}
	if !status.Terminal {
		return errors.New("legacy replay fixture requires terminal case")
	}
	return nil
}

// TestCaseWorkflowRealTemporalPostgres runs SupervisorCaseWorkflow on a real Temporal service
// and PostgreSQL (WIS-273): the relay starts and signals the workflow from the outbox, two
// workers share the queue, a case waiting on a person past its 24 h deadline is escalated
// once, a duplicate signal has no second effect, cancellation ends the workflow, the recorded
// history replays deterministically, and without Temporal the outbox stays queued.
func TestCaseWorkflowRealTemporalPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	adminURL, runtimeURL, address := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL"), os.Getenv("SUPERVISOR_TEST_TEMPORAL_ADDRESS")
	if adminURL == "" || runtimeURL == "" || address == "" {
		t.Fatal("BLOCKED: real PostgreSQL and Temporal service addresses are required")
	}
	root, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	database := "case_workflow_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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

	// A case waiting on a person (needs_input) whose wait began 25 h ago, with its start and
	// signal deliveries queued in the outbox. The rows behind it are planted with FK triggers
	// off: the trigger path is covered by the case HTTP suite.
	tenant, site, otherTenant := uuid.NewString(), uuid.NewString(), uuid.NewString()
	line, order, contextID, sourceID, caseID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	startEvent, signalEvent, cancelEvent := uuid.NewString(), uuid.NewString(), uuid.NewString()
	digest := strings.Repeat("a", 64)
	plant := func(statements ...string) {
		t.Helper()
		tx, e := control.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		for _, q := range append([]string{`SET LOCAL session_replication_role = replica`, `SELECT set_config('supervisor.tenant_id','` + tenant + `',true)`}, statements...) {
			if _, e = tx.ExecContext(ctx, q); e != nil {
				t.Fatalf("%s: %v", q, e)
			}
		}
		if e = tx.Commit(); e != nil {
			t.Fatal(e)
		}
	}
	q := func(s string) string { return "'" + s + "'" }
	snapshot := `{"readiness":"NOT_READY","digest":"` + digest + `","context_id":"` + contextID + `","order_id":"` + order + `","asset_id":"` + line + `","site_id":"` + site + `"}`
	plant(
		`INSERT INTO supervisor.tenants(id,name) VALUES(`+q(tenant)+`,'case workflow test')`,
		`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES(`+q(tenant)+`,`+q(site)+`,'case workflow site')`,
		`INSERT INTO supervisor.knowledge_entity_heads(tenant_id,site_id,id,current_revision) VALUES(`+q(tenant)+`,`+q(site)+`,`+q(line)+`,1),(`+q(tenant)+`,`+q(site)+`,`+q(order)+`,1)`,
		`INSERT INTO supervisor.plant_contexts(tenant_id,site_id,id,order_id,asset_id,window_start,window_end,process_snapshot_id,process_snapshot_sha256,readiness,snapshot,digest,actor_issuer,actor_subject,evaluated_at) VALUES(`+q(tenant)+`,`+q(site)+`,`+q(contextID)+`,`+q(order)+`,`+q(line)+`,'2026-10-20T06:00:00Z','2026-10-20T06:15:00Z',`+q(uuid.NewString())+`,`+q(strings.Repeat("d", 64))+`,'NOT_READY','`+snapshot+`',`+q(digest)+`,'test','operator','2026-10-20T06:15:00Z')`,
		`INSERT INTO supervisor.operational_event_records(tenant_id,site_id,source_id,source_revision,mapping_revision,source_sequence,external_key,revision,event_kind,event_at,payload,payload_sha256,event_sha256,batch_id) VALUES(`+q(tenant)+`,`+q(site)+`,`+q(sourceID)+`,1,1,1,'delta-1',1,'production_delta','2026-10-20T06:15:00Z','{}',`+q(strings.Repeat("e", 64))+`,`+q(strings.Repeat("e", 64))+`,`+q(uuid.NewString())+`)`,
		`INSERT INTO supervisor.cases(tenant_id,site_id,id,line_id,order_id,exception_kind,episode,revision,context_snapshot_id,context_revision,context_digest,state,detected_at,latest_window_end,workflow_id) VALUES(`+q(tenant)+`,`+q(site)+`,`+q(caseID)+`,`+q(line)+`,`+q(order)+`,'production_shortfall',1,2,`+q(contextID)+`,1,`+q(digest)+`,'needs_input',now()-interval '26 hours',now()-interval '26 hours','case:`+tenant+`:`+caseID+`')`,
		`INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,actor_issuer,actor_subject,created_at) VALUES(`+q(tenant)+`,`+q(site)+`,`+q(caseID)+`,1,'none','detected','trigger','svc','ingestion',now()-interval '26 hours')`,
		`INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,reason,actor_issuer,actor_subject,created_at) VALUES(`+q(tenant)+`,`+q(site)+`,`+q(caseID)+`,2,'detected','needs_input','evidence_missing','MISSING_ACTUALS','svc','case-workflow',now()-interval '25 hours')`,
		`INSERT INTO supervisor.case_trigger_evidence(tenant_id,site_id,case_id,source_id,source_sequence) VALUES(`+q(tenant)+`,`+q(site)+`,`+q(caseID)+`,`+q(sourceID)+`,1)`,
		`INSERT INTO supervisor.case_outbox(tenant_id,site_id,id,case_id,case_revision,kind,event_id) VALUES(`+q(tenant)+`,`+q(site)+`,`+q(uuid.NewString())+`,`+q(caseID)+`,1,'start',`+q(startEvent)+`),(`+q(tenant)+`,`+q(site)+`,`+q(uuid.NewString())+`,`+q(caseID)+`,2,'signal',`+q(signalEvent)+`)`,
	)
	// A second case (another order) waiting on a person for only one hour: not escalated yet.
	recentOrder, recentCase, recentEvent := uuid.NewString(), uuid.NewString(), uuid.NewString()
	recentContext := uuid.NewString()
	recentSnapshot := `{"readiness":"NOT_READY","digest":"` + digest + `","context_id":"` + recentContext + `","order_id":"` + recentOrder + `","asset_id":"` + line + `","site_id":"` + site + `"}`
	plant(
		`INSERT INTO supervisor.knowledge_entity_heads(tenant_id,site_id,id,current_revision) VALUES(`+q(tenant)+`,`+q(site)+`,`+q(recentOrder)+`,1)`,
		`INSERT INTO supervisor.plant_contexts(tenant_id,site_id,id,order_id,asset_id,window_start,window_end,process_snapshot_id,process_snapshot_sha256,readiness,snapshot,digest,actor_issuer,actor_subject,evaluated_at) VALUES(`+q(tenant)+`,`+q(site)+`,`+q(recentContext)+`,`+q(recentOrder)+`,`+q(line)+`,'2026-10-20T06:00:00Z','2026-10-20T06:15:00Z',`+q(uuid.NewString())+`,`+q(strings.Repeat("d", 64))+`,'NOT_READY','`+recentSnapshot+`',`+q(digest)+`,'test','operator','2026-10-20T06:15:00Z')`,
		`INSERT INTO supervisor.cases(tenant_id,site_id,id,line_id,order_id,exception_kind,episode,revision,context_snapshot_id,context_revision,context_digest,state,detected_at,latest_window_end,workflow_id) VALUES(`+q(tenant)+`,`+q(site)+`,`+q(recentCase)+`,`+q(line)+`,`+q(recentOrder)+`,'production_shortfall',1,2,`+q(recentContext)+`,1,`+q(digest)+`,'needs_input',now()-interval '2 hours',now()-interval '2 hours','case:`+tenant+`:`+recentCase+`')`,
		`INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,actor_issuer,actor_subject,created_at) VALUES(`+q(tenant)+`,`+q(site)+`,`+q(recentCase)+`,1,'none','detected','trigger','svc','ingestion',now()-interval '2 hours')`,
		`INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,actor_issuer,actor_subject,created_at) VALUES(`+q(tenant)+`,`+q(site)+`,`+q(recentCase)+`,2,'detected','needs_input','evidence_missing','svc','case-workflow',now()-interval '1 hour')`,
		`INSERT INTO supervisor.case_trigger_evidence(tenant_id,site_id,case_id,source_id,source_sequence) VALUES(`+q(tenant)+`,`+q(site)+`,`+q(recentCase)+`,`+q(sourceID)+`,1)`,
		`INSERT INTO supervisor.case_outbox(tenant_id,site_id,id,case_id,case_revision,kind,event_id) VALUES(`+q(tenant)+`,`+q(site)+`,`+q(uuid.NewString())+`,`+q(recentCase)+`,1,'start',`+q(recentEvent)+`)`,
	)
	// A third case, past its deadline at revision 4 and with no workflow: escalation is fenced
	// on the exact revision the caller observed.
	fencedOrder, fencedCase, fencedContext := uuid.NewString(), uuid.NewString(), uuid.NewString()
	fencedSnapshot := `{"readiness":"NOT_READY","digest":"` + digest + `","context_id":"` + fencedContext + `","order_id":"` + fencedOrder + `","asset_id":"` + line + `","site_id":"` + site + `"}`
	transition := func(revision int, from, to, ago string) string {
		return `INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,actor_issuer,actor_subject,created_at) VALUES(` + q(tenant) + `,` + q(site) + `,` + q(fencedCase) + `,` + strconv.Itoa(revision) + `,'` + from + `','` + to + `','test','svc','case-workflow',now()-interval '` + ago + `')`
	}
	plant(
		`INSERT INTO supervisor.knowledge_entity_heads(tenant_id,site_id,id,current_revision) VALUES(`+q(tenant)+`,`+q(site)+`,`+q(fencedOrder)+`,1)`,
		`INSERT INTO supervisor.plant_contexts(tenant_id,site_id,id,order_id,asset_id,window_start,window_end,process_snapshot_id,process_snapshot_sha256,readiness,snapshot,digest,actor_issuer,actor_subject,evaluated_at) VALUES(`+q(tenant)+`,`+q(site)+`,`+q(fencedContext)+`,`+q(fencedOrder)+`,`+q(line)+`,'2026-10-20T06:00:00Z','2026-10-20T06:15:00Z',`+q(uuid.NewString())+`,`+q(strings.Repeat("d", 64))+`,'NOT_READY','`+fencedSnapshot+`',`+q(digest)+`,'test','operator','2026-10-20T06:15:00Z')`,
		`INSERT INTO supervisor.cases(tenant_id,site_id,id,line_id,order_id,exception_kind,episode,revision,context_snapshot_id,context_revision,context_digest,state,detected_at,latest_window_end,workflow_id) VALUES(`+q(tenant)+`,`+q(site)+`,`+q(fencedCase)+`,`+q(line)+`,`+q(fencedOrder)+`,'production_shortfall',1,4,`+q(fencedContext)+`,1,`+q(digest)+`,'needs_input',now()-interval '30 hours',now()-interval '30 hours','case:`+tenant+`:`+fencedCase+`')`,
		transition(1, "none", "detected", "30 hours"),
		transition(2, "detected", "needs_input", "29 hours"),
		// The clarification is a person's, not the workflow's.
		`INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,reason,actor_issuer,actor_subject,created_at) VALUES(`+q(tenant)+`,`+q(site)+`,`+q(fencedCase)+`,3,'needs_input','investigating','clarified','Stop reason supplied by the line lead','test','operator',now()-interval '28 hours')`,
		transition(4, "investigating", "needs_input", "25 hours"),
		`INSERT INTO supervisor.case_trigger_evidence(tenant_id,site_id,case_id,source_id,source_sequence) VALUES(`+q(tenant)+`,`+q(site)+`,`+q(fencedCase)+`,`+q(sourceID)+`,1)`,
	)
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if e := control.QueryRowContext(ctx, query, args...).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}

	c, err := client.Dial(client.Options{HostPort: address, Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cfg := config.Config{TemporalQueue: "case-workflow-test-" + uuid.NewString(), Tenants: []string{tenant}}
	for i := 0; i < 2; i++ {
		w, err := NewWorker(c, db, cfg)
		if err == nil {
			err = w.Start()
		}
		if err != nil {
			t.Fatal(err)
		}
		defer w.Stop()
	}
	waitFor := func(what string, done func() bool) {
		t.Helper()
		for !done() {
			if ctx.Err() != nil {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	// Review finding: a run whose activities exhausted their attempts (a database outage longer
	// than the activity backoff) failed for good and the 24 h escalation was never re-armed. The
	// runtime role loses read access to cases, so every run fails; a retried run must start, and
	// once access returns the case is still escalated.
	previousRetry := caseWorkflowRetry
	caseWorkflowRetry = &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 1, MaximumInterval: time.Second}
	t.Cleanup(func() { caseWorkflowRetry = previousRetry })
	if _, err = control.ExecContext(ctx, `REVOKE SELECT ON supervisor.cases FROM supervisor_runtime`); err != nil {
		t.Fatal(err)
	}
	// One tenant's failure must not hold back another's deliveries: an unusable tenant entry
	// comes first in the relay's list.
	relayCfg := cfg
	relayCfg.Tenants = []string{"not-a-tenant", tenant}
	if n, err := RelayCases(ctx, db, c, relayCfg, relayCfg.Tenants); err == nil || n != 3 {
		t.Fatalf("relay with a failing tenant delivered %d: %v", n, err)
	}
	if pending := count(`SELECT count(*) FROM supervisor.case_outbox WHERE delivered_at IS NULL`); pending != 0 {
		t.Fatalf("%d outbox rows still pending after relay", pending)
	}
	escalatingID := cases.WorkflowID(tenant, caseID)
	firstRun := ""
	waitFor("a retried run after the outage", func() bool {
		d, e := c.DescribeWorkflowExecution(ctx, escalatingID, "")
		if e != nil {
			return false
		}
		run := d.GetWorkflowExecutionInfo().GetExecution().GetRunId()
		if firstRun == "" {
			firstRun = run
		}
		return run != firstRun
	})
	if _, err = control.ExecContext(ctx, `GRANT SELECT ON supervisor.cases TO supervisor_runtime`); err != nil {
		t.Fatal(err)
	}
	waitFor("deadline escalation", func() bool {
		return count(`SELECT count(*) FROM supervisor.cases WHERE id=$1 AND state='escalated' AND revision=3`, caseID) == 1
	})
	var event, reason, issuer string
	if err = control.QueryRowContext(ctx, `SELECT event,reason,actor_issuer FROM supervisor.case_transitions WHERE case_id=$1 AND revision=3`, caseID).Scan(&event, &reason, &issuer); err != nil ||
		event != "wait_deadline" || reason != "HUMAN_WAIT_DEADLINE" || issuer != cases.WorkflowActor.Issuer {
		t.Fatalf("escalation transition: %s %s %s %v", event, reason, issuer, err)
	}
	waitFor("both deliveries recorded", func() bool {
		return count(`SELECT count(*) FROM supervisor.case_workflow_events WHERE case_id=$1`, caseID) == 2
	})
	// The recent case's workflow ran (its delivery is recorded) and left it waiting: its
	// deadline is 23 h away, and an early escalation attempt is fenced off.
	waitFor("recent case delivery recorded", func() bool {
		return count(`SELECT count(*) FROM supervisor.case_workflow_events WHERE case_id=$1`, recentCase) == 1
	})
	if n := count(`SELECT count(*) FROM supervisor.cases WHERE id=$1 AND state='needs_input' AND revision=2`, recentCase); n != 1 {
		t.Fatal("a case inside its 24 h wait was escalated")
	}
	recentRef := cases.WorkflowRef{TenantID: tenant, SiteID: site, CaseID: recentCase}
	if escalated, err := cases.NewService(db.DB).EscalateOnDeadline(ctx, recentRef, 2); err != nil || escalated {
		t.Fatalf("early escalation: %v %v", escalated, err)
	}
	fencedRef := cases.WorkflowRef{TenantID: tenant, SiteID: site, CaseID: fencedCase}
	if escalated, err := cases.NewService(db.DB).EscalateOnDeadline(ctx, fencedRef, 2); err != nil || escalated {
		t.Fatalf("escalation at a stale revision: %v %v", escalated, err)
	}
	if escalated, err := cases.NewService(db.DB).EscalateOnDeadline(ctx, fencedRef, 4); err != nil || !escalated {
		t.Fatalf("escalation at the current revision: %v %v", escalated, err)
	}
	if escalated, err := cases.NewService(db.DB).EscalateOnDeadline(ctx, fencedRef, 4); err != nil || escalated {
		t.Fatalf("repeated escalation: %v %v", escalated, err)
	}

	// A duplicate delivery of the same event, then cancellation: the workflow ends, and the
	// duplicate left no second record.
	workflowID := cases.WorkflowID(tenant, caseID)
	if err = c.SignalWorkflow(ctx, workflowID, "", CaseSignal, cases.WorkflowEvent{EventID: signalEvent, Kind: "signal", Revision: 2}); err != nil {
		t.Fatal(err)
	}
	plant(
		`INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,reason,actor_issuer,actor_subject) VALUES(`+q(tenant)+`,`+q(site)+`,`+q(caseID)+`,4,'escalated','canceled','cancel','Duplicate alert','https://idp.invalid','lead')`,
		`UPDATE supervisor.cases SET state='canceled',revision=4 WHERE id=`+q(caseID),
		`INSERT INTO supervisor.case_outbox(tenant_id,site_id,id,case_id,case_revision,kind,event_id) VALUES(`+q(tenant)+`,`+q(site)+`,`+q(uuid.NewString())+`,`+q(caseID)+`,4,'cancel',`+q(cancelEvent)+`)`,
	)
	if n, err := RelayCases(ctx, db, c, cfg, cfg.Tenants); err != nil || n != 1 {
		t.Fatalf("cancel relay delivered %d: %v", n, err)
	}
	run := c.GetWorkflow(ctx, workflowID, "")
	if err = run.Get(ctx, nil); err != nil {
		t.Fatalf("workflow did not end cleanly: %v", err)
	}
	if n := count(`SELECT count(*) FROM supervisor.case_workflow_events WHERE case_id=$1`, caseID); n != 3 {
		t.Fatalf("workflow events = %d, want start, signal and cancel once each", n)
	}
	if n := count(`SELECT count(*) FROM supervisor.case_transitions WHERE case_id=$1 AND event='wait_deadline'`, caseID); n != 1 {
		t.Fatalf("escalated %d times", n)
	}

	// The persisted history replays against the current workflow code.
	history := &historypb.History{}
	iter := c.GetWorkflowHistory(ctx, workflowID, run.GetRunID(), false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iter.HasNext() {
		e, err := iter.Next()
		if err != nil {
			t.Fatal(err)
		}
		history.Events = append(history.Events, e)
	}
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(SupervisorCaseWorkflow)
	if err = replayer.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatalf("history replay: %v", err)
	}

	// Without Temporal a delivery stays queued, never lost or marked delivered.
	plant(`INSERT INTO supervisor.case_outbox(tenant_id,site_id,id,case_id,case_revision,kind,event_id) VALUES(` + q(tenant) + `,` + q(site) + `,` + q(uuid.NewString()) + `,` + q(caseID) + `,4,'signal',` + q(uuid.NewString()) + `)`)
	down, err := client.NewLazyClient(client.Options{HostPort: "127.0.0.1:1", Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	defer down.Close()
	short, stop := context.WithTimeout(ctx, 5*time.Second)
	if _, err = RelayCases(short, db, down, cfg, cfg.Tenants); err == nil {
		t.Fatal("relay reported success without Temporal")
	}
	stop()
	if pending := count(`SELECT count(*) FROM supervisor.case_outbox WHERE delivered_at IS NULL`); pending != 1 {
		t.Fatalf("pending deliveries without Temporal = %d, want 1", pending)
	}

	// Tenant isolation: the service cannot read the case through another tenant's reference.
	if _, err = cases.NewService(db.DB).WorkflowStatus(ctx, cases.WorkflowRef{TenantID: otherTenant, SiteID: site, CaseID: caseID}); !errors.Is(err, cases.ErrNotFound) {
		t.Fatalf("cross-tenant workflow read: %v", err)
	}
}
