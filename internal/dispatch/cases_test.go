package dispatch

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/cases"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// fakeCase is the business state the activities would read from PostgreSQL. The test
// environment skips time, so the 24 h human wait is exercised exactly, without a wall clock.
type fakeCase struct {
	env       *testsuite.TestWorkflowEnvironment
	status    cases.WorkflowStatus
	recorded  []cases.WorkflowEvent
	seen      map[string]bool
	escalated []time.Time
	// delivered are outbox rows the relay delivered to an earlier run that failed before
	// recording them; ReconcileCaseEvents copies the unrecorded ones into the ledger (seen).
	delivered []cases.WorkflowEvent
	// acceptAfter is when the "database" accepts the deadline; before it escalation is fenced.
	acceptAfter time.Time
	// reads counts ReadCaseStatus activities, so a spinning workflow is visible.
	reads int
}

func newFakeCase(t *testing.T, status cases.WorkflowStatus) (*testsuite.TestWorkflowEnvironment, *fakeCase) {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	f := &fakeCase{env: env, status: status, seen: map[string]bool{}}
	env.RegisterWorkflow(SupervisorCaseWorkflow)
	env.RegisterActivityWithOptions(func(context.Context, cases.WorkflowRef) (cases.WorkflowStatus, error) {
		f.reads++
		return f.status, nil
	}, activity.RegisterOptions{Name: "ReadCaseStatus"})
	env.RegisterActivityWithOptions(func(_ context.Context, _ cases.WorkflowRef, ev cases.WorkflowEvent) (bool, error) {
		duplicate := f.seen[ev.EventID]
		f.seen[ev.EventID] = true
		f.recorded = append(f.recorded, ev)
		if ev.Kind == "cancel" {
			f.status = cases.WorkflowStatus{State: cases.Canceled, Revision: ev.Revision, Terminal: true}
		}
		return duplicate, nil
	}, activity.RegisterOptions{Name: "RecordCaseEvent"})
	env.RegisterActivityWithOptions(func(context.Context, cases.WorkflowRef) (int, error) {
		added := 0
		for _, ev := range f.delivered {
			if !f.seen[ev.EventID] {
				f.seen[ev.EventID] = true
				added++
			}
		}
		return added, nil
	}, activity.RegisterOptions{Name: "ReconcileCaseEvents"})
	env.RegisterActivityWithOptions(func(_ context.Context, _ cases.WorkflowRef, revision int64) (bool, error) {
		now := env.Now()
		if revision != f.status.Revision || !f.status.WaitingOnHuman || now.Before(f.acceptAfter) {
			return false, nil
		}
		f.escalated = append(f.escalated, now)
		f.status = cases.WorkflowStatus{State: cases.Escalated, Revision: revision + 1}
		return true, nil
	}, activity.RegisterOptions{Name: "EscalateCaseOnDeadline"})
	return env, f
}

func caseRef() cases.WorkflowRef {
	return cases.WorkflowRef{TenantID: uuid.NewString(), SiteID: uuid.NewString(), CaseID: uuid.NewString()}
}

func (f *fakeCase) signalAt(after time.Duration, ev cases.WorkflowEvent) {
	f.env.RegisterDelayedCallback(func() { f.env.SignalWorkflow(CaseSignal, ev) }, after)
}

// WIS-273: a case waiting on a person escalates when the 24 h wait ends, not earlier, and a
// signal arriving during the wait does not restart it.
func TestCaseWorkflowEscalatesAtTheHumanWaitDeadline(t *testing.T) {
	start := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	env, f := newFakeCase(t, cases.WorkflowStatus{})
	env.SetStartTime(start)
	f.status = cases.WorkflowStatus{State: cases.NeedsInput, Revision: 3, WaitingOnHuman: true, WaitDeadline: start.Add(cases.HumanWait)}
	f.signalAt(time.Hour, cases.WorkflowEvent{EventID: uuid.NewString(), Kind: "signal", Revision: 3})
	f.signalAt(cases.HumanWait+2*time.Hour, cases.WorkflowEvent{EventID: uuid.NewString(), Kind: "cancel", Revision: 5})
	env.ExecuteWorkflow(SupervisorCaseWorkflow, caseRef())
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow: completed=%v err=%v", env.IsWorkflowCompleted(), env.GetWorkflowError())
	}
	if len(f.escalated) != 1 || !f.escalated[0].Equal(start.Add(cases.HumanWait)) {
		t.Fatalf("escalations at %v, want exactly one at %v", f.escalated, start.Add(cases.HumanWait))
	}
	if len(f.recorded) != 2 {
		t.Fatalf("recorded %d events, want 2", len(f.recorded))
	}
}

// A deadline the database has not accepted yet (fenced escalation returns false) is retried
// no sooner than deadlineRecheck later, never in a tight loop.
func TestCaseWorkflowRechecksAFencedDeadline(t *testing.T) {
	start := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	env, f := newFakeCase(t, cases.WorkflowStatus{})
	env.SetStartTime(start)
	f.status = cases.WorkflowStatus{State: cases.AwaitingApproval, Revision: 7, WaitingOnHuman: true, WaitDeadline: start.Add(cases.HumanWait)}
	f.acceptAfter = start.Add(cases.HumanWait + 30*time.Second)
	f.signalAt(cases.HumanWait+time.Hour, cases.WorkflowEvent{EventID: uuid.NewString(), Kind: "cancel", Revision: 9})
	env.ExecuteWorkflow(SupervisorCaseWorkflow, caseRef())
	if env.GetWorkflowError() != nil {
		t.Fatal(env.GetWorkflowError())
	}
	if want := start.Add(cases.HumanWait + deadlineRecheck); len(f.escalated) != 1 || !f.escalated[0].Equal(want) {
		t.Fatalf("escalations at %v, want one at %v", f.escalated, want)
	}
}

// Duplicate signals are passed to the database ledger, which reports them; a case that is not
// waiting on a person never escalates; a terminal case ends the workflow.
func TestCaseWorkflowDeduplicatesAndEndsWhenTerminal(t *testing.T) {
	env, f := newFakeCase(t, cases.WorkflowStatus{State: cases.Detected, Revision: 1})
	// A signal delivered to a failed earlier run is not redelivered; the run records it from
	// the outbox.
	lost := cases.WorkflowEvent{EventID: uuid.NewString(), Kind: "signal", Revision: 1}
	f.delivered = []cases.WorkflowEvent{lost}
	dup := cases.WorkflowEvent{EventID: uuid.NewString(), Kind: "start", Revision: 1}
	f.signalAt(time.Minute, dup)
	f.signalAt(2*time.Minute, dup)
	f.signalAt(72*time.Hour, cases.WorkflowEvent{EventID: uuid.NewString(), Kind: "cancel", Revision: 2})
	env.ExecuteWorkflow(SupervisorCaseWorkflow, caseRef())
	if env.GetWorkflowError() != nil {
		t.Fatal(env.GetWorkflowError())
	}
	if !f.seen[lost.EventID] {
		t.Fatal("the run did not record the delivery lost with a failed run")
	}
	if len(f.recorded) != 3 || len(f.seen) != 3 || len(f.escalated) != 0 {
		t.Fatalf("recorded=%d distinct=%d escalated=%d", len(f.recorded), len(f.seen), len(f.escalated))
	}
	ended, g := newFakeCase(t, cases.WorkflowStatus{State: cases.Resolved, Revision: 9, Terminal: true})
	ended.ExecuteWorkflow(SupervisorCaseWorkflow, caseRef())
	if !ended.IsWorkflowCompleted() || ended.GetWorkflowError() != nil || len(g.recorded) != 0 {
		t.Fatalf("terminal case: err=%v recorded=%d", ended.GetWorkflowError(), len(g.recorded))
	}
}

func TestCaseWorkflowDoesNotScheduleAgentWhenFlagUnset(t *testing.T) {
	env, f := newFakeCase(t, cases.WorkflowStatus{State: cases.Investigating, Revision: 2})
	calls := 0
	env.RegisterActivityWithOptions(func(context.Context, InvestigateRef) (agent.Result, error) {
		calls++
		return agent.Result{}, nil
	}, activity.RegisterOptions{Name: "InvestigateCase"})
	f.signalAt(time.Minute, cases.WorkflowEvent{EventID: uuid.NewString(), Kind: "cancel", Revision: 3})
	env.ExecuteWorkflow(SupervisorCaseWorkflow, caseRef())
	if env.GetWorkflowError() != nil || calls != 0 {
		t.Fatalf("workflow err=%v agent calls=%d, want no scheduled activity", env.GetWorkflowError(), calls)
	}
}

func TestCaseWorkflowDoesNotScheduleAgentForDefaultVersion(t *testing.T) {
	env, f := newFakeCase(t, cases.WorkflowStatus{State: cases.Investigating, Revision: 2, AgentEnabled: true})
	env.OnGetVersion("agent-investigation", workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
	calls := 0
	env.RegisterActivityWithOptions(func(context.Context, InvestigateRef) (agent.Result, error) {
		calls++
		return agent.Result{}, nil
	}, activity.RegisterOptions{Name: "InvestigateCase"})
	f.signalAt(time.Minute, cases.WorkflowEvent{EventID: uuid.NewString(), Kind: "cancel", Revision: 3})
	env.ExecuteWorkflow(SupervisorCaseWorkflow, caseRef())
	if env.GetWorkflowError() != nil || calls != 0 {
		t.Fatalf("workflow err=%v agent calls=%d, want no activity for DefaultVersion", env.GetWorkflowError(), calls)
	}
}

func TestCaseWorkflowSchedulesAgentOnlyForEnabledInvestigation(t *testing.T) {
	env, f := newFakeCase(t, cases.WorkflowStatus{State: cases.Investigating, Revision: 2, AgentEnabled: true})
	calls := 0
	env.RegisterActivityWithOptions(func(_ context.Context, ref InvestigateRef) (agent.Result, error) {
		calls++
		if ref.Revision != 2 {
			t.Fatalf("investigated revision %d, want 2", ref.Revision)
		}
		f.status = cases.WorkflowStatus{State: cases.NeedsInput, Revision: 3, WaitingOnHuman: true, WaitDeadline: env.Now().Add(cases.HumanWait)}
		return agent.Result{RunID: uuid.NewString(), State: agent.RunCompleted}, nil
	}, activity.RegisterOptions{Name: "InvestigateCase"})
	f.signalAt(time.Minute, cases.WorkflowEvent{EventID: uuid.NewString(), Kind: "cancel", Revision: 4})
	env.ExecuteWorkflow(SupervisorCaseWorkflow, caseRef())
	if env.GetWorkflowError() != nil || calls != 1 {
		t.Fatalf("workflow err=%v agent calls=%d, want one activity", env.GetWorkflowError(), calls)
	}
}

// WIS-275 review: an investigation that leaves the case investigating at the same revision (a
// replayed canceled or degraded run) must not re-run the activity at once; the workflow waits for
// the next signal. A later revision is investigated again.
func TestCaseWorkflowWaitsWhenInvestigationLeavesCaseUnchanged(t *testing.T) {
	env, f := newFakeCase(t, cases.WorkflowStatus{State: cases.Investigating, Revision: 2, AgentEnabled: true})
	env.SetTestTimeout(10 * time.Second)
	var revisions []int64
	env.RegisterActivityWithOptions(func(_ context.Context, ref InvestigateRef) (agent.Result, error) {
		revisions = append(revisions, ref.Revision)
		if len(revisions) > 5 {
			return agent.Result{}, temporal.NewNonRetryableApplicationError("spinning", "scope", nil)
		}
		return agent.Result{RunID: uuid.NewString(), State: agent.RunCanceled, Replayed: true}, nil
	}, activity.RegisterOptions{Name: "InvestigateCase"})
	// A person clarifies (revision 3, still investigating), then cancels.
	env.RegisterDelayedCallback(func() {
		f.status = cases.WorkflowStatus{State: cases.Investigating, Revision: 3, AgentEnabled: true}
		env.SignalWorkflow(CaseSignal, cases.WorkflowEvent{EventID: uuid.NewString(), Kind: "signal", Revision: 3})
	}, time.Minute)
	f.signalAt(time.Hour, cases.WorkflowEvent{EventID: uuid.NewString(), Kind: "cancel", Revision: 4})
	env.ExecuteWorkflow(SupervisorCaseWorkflow, caseRef())
	if env.GetWorkflowError() != nil || len(revisions) != 2 || revisions[0] != 2 || revisions[1] != 3 {
		t.Fatalf("workflow err=%v investigated revisions=%v, want [2 3]", env.GetWorkflowError(), revisions)
	}
}

// After caseSignalsPerRun processed signals the workflow continues as new, carrying only its
// reference, so history stays bounded. The bound is lowered here: the SDK test environment
// buffers at most 1000 delayed callbacks (internal_workflow_testsuite.go callbackChannel) and
// deadlocks beyond that, which is a harness limit, not workflow behavior.
func TestCaseWorkflowContinuesAsNewAfterTheSignalBound(t *testing.T) {
	previous := caseSignalsPerRun
	caseSignalsPerRun = 5
	t.Cleanup(func() { caseSignalsPerRun = previous })
	env, f := newFakeCase(t, cases.WorkflowStatus{State: cases.Investigating, Revision: 2})
	for i := 0; i < caseSignalsPerRun+3; i++ {
		f.signalAt(time.Duration(i+1)*time.Second, cases.WorkflowEvent{EventID: uuid.NewString(), Kind: "signal", Revision: 2})
	}
	ref := caseRef()
	env.ExecuteWorkflow(SupervisorCaseWorkflow, ref)
	var next *workflow.ContinueAsNewError
	if err := env.GetWorkflowError(); !errors.As(err, &next) {
		t.Fatalf("workflow error = %v, want continue-as-new", err)
	}
	if len(f.recorded) != caseSignalsPerRun {
		t.Fatalf("recorded %d signals before continuing, want %d", len(f.recorded), caseSignalsPerRun)
	}
}

// Activities refuse a reference for a tenant this worker was not provisioned for, without
// touching the database.
func TestCaseActivitiesRefuseUnprovisionedTenants(t *testing.T) {
	a := &Activities{Allow: map[string]bool{uuid.NewString(): true}}
	ref := caseRef()
	if _, err := a.ReadCaseStatus(context.Background(), ref); err == nil {
		t.Fatal("read for an unprovisioned tenant")
	}
	if _, err := a.RecordCaseEvent(context.Background(), ref, cases.WorkflowEvent{}); err == nil {
		t.Fatal("record for an unprovisioned tenant")
	}
	if _, err := a.EscalateCaseOnDeadline(context.Background(), ref, 1); err == nil {
		t.Fatal("escalation for an unprovisioned tenant")
	}
}

func TestActivityDiscoveryErrorIsRetryable(t *testing.T) {
	db, err := sql.Open("pgx", "postgresql://invalid.invalid/supervisor")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	a := &Activities{Store: &store.Store{DB: db}}
	if err = a.caseScope(context.Background(), caseRef()); err == nil {
		t.Fatal("closed database discovery error was accepted")
	} else {
		var app *temporal.ApplicationError
		if errors.As(err, &app) && app.NonRetryable() {
			t.Fatalf("discovery error was made non-retryable: %v", err)
		}
	}
}

// Review finding: signals already buffered when the bound is reached were dropped by
// continue-as-new (the relay had already marked them delivered). They are recorded first.
func TestCaseWorkflowRecordsBufferedSignalsBeforeContinuingAsNew(t *testing.T) {
	previous := caseSignalsPerRun
	caseSignalsPerRun = 5
	t.Cleanup(func() { caseSignalsPerRun = previous })
	env, f := newFakeCase(t, cases.WorkflowStatus{State: cases.Investigating, Revision: 2})
	sent := map[string]bool{}
	for i := 0; i < caseSignalsPerRun+3; i++ {
		ev := cases.WorkflowEvent{EventID: uuid.NewString(), Kind: "signal", Revision: 2}
		sent[ev.EventID] = true
		f.signalAt(time.Second, ev) // all arrive together, so three are buffered at the bound
	}
	env.ExecuteWorkflow(SupervisorCaseWorkflow, caseRef())
	var next *workflow.ContinueAsNewError
	if err := env.GetWorkflowError(); !errors.As(err, &next) {
		t.Fatalf("workflow error = %v, want continue-as-new", err)
	}
	if len(f.recorded) != len(sent) {
		t.Fatalf("recorded %d of %d signals before continuing as new", len(f.recorded), len(sent))
	}
	for _, ev := range f.recorded {
		if !sent[ev.EventID] {
			t.Fatalf("recorded an unknown event %s", ev.EventID)
		}
	}
}

// A scope failure (the case or tenant is unavailable to this worker) ends the run without a
// workflow retry; any other activity failure fails the run so the workflow retry policy
// restarts it and re-arms the human-wait timer.
func TestCaseWorkflowFailureKinds(t *testing.T) {
	for _, tc := range []struct {
		name         string
		err          error
		nonRetryable bool
	}{
		{"scope failure", temporal.NewNonRetryableApplicationError("case unavailable", "scope", nil), true},
		{"storage outage", errors.New("database unavailable"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestWorkflowEnvironment()
			env.RegisterWorkflow(SupervisorCaseWorkflow)
			env.RegisterActivityWithOptions(func(context.Context, cases.WorkflowRef) (cases.WorkflowStatus, error) {
				return cases.WorkflowStatus{}, tc.err
			},
				activity.RegisterOptions{Name: "ReadCaseStatus"})
			env.RegisterActivityWithOptions(func(context.Context, cases.WorkflowRef) (int, error) { return 0, nil },
				activity.RegisterOptions{Name: "ReconcileCaseEvents"})
			env.ExecuteWorkflow(SupervisorCaseWorkflow, caseRef())
			err := env.GetWorkflowError()
			if err == nil {
				t.Fatal("workflow succeeded despite a failing activity")
			}
			// The server retries a failed run unless the run's own (outermost) failure is a
			// non-retryable application error; an ActivityError there is retried even when the
			// activity's cause is non-retryable. So inspect only the cause directly under the
			// WorkflowExecutionError, never errors.As down the chain (review finding: that
			// passed with caseRunError reduced to `return err`).
			var run *temporal.WorkflowExecutionError
			if !errors.As(err, &run) {
				t.Fatalf("workflow error %T, want a WorkflowExecutionError", err)
			}
			app, ok := errors.Unwrap(run).(*temporal.ApplicationError)
			got := ok && app.NonRetryable() && app.Type() == "scope"
			if got != tc.nonRetryable {
				t.Fatalf("non-retryable scope failure = %v, want %v (err %v)", got, tc.nonRetryable, err)
			}
		})
	}
}

// WIS-273 runtime bounds: a site or worker at its investigation bound defers the case instead of
// failing the run. It is tried again after 5 s, then 10 s, still recording signals meanwhile, and
// a successful attempt resets the backoff.
func TestCaseWorkflowDefersInvestigationWhenAdmissionIsBusy(t *testing.T) {
	env, f := newFakeCase(t, cases.WorkflowStatus{State: cases.Investigating, Revision: 2, AgentEnabled: true})
	env.SetTestTimeout(10 * time.Second)
	var at []time.Time
	env.RegisterActivityWithOptions(func(_ context.Context, ref InvestigateRef) (agent.Result, error) {
		at = append(at, env.Now())
		if len(at) <= 2 {
			return agent.Result{}, temporal.NewNonRetryableApplicationError("investigation deferred", admissionBusy, nil)
		}
		f.status = cases.WorkflowStatus{State: cases.NeedsInput, Revision: 3, WaitingOnHuman: true, WaitDeadline: env.Now().Add(cases.HumanWait)}
		return agent.Result{RunID: uuid.NewString(), State: agent.RunCompleted}, nil
	}, activity.RegisterOptions{Name: "InvestigateCase"})
	queued := cases.WorkflowEvent{EventID: uuid.NewString(), Kind: "signal", Revision: 2}
	f.signalAt(2*time.Second, queued)
	f.signalAt(time.Hour, cases.WorkflowEvent{EventID: uuid.NewString(), Kind: "cancel", Revision: 4})
	env.ExecuteWorkflow(SupervisorCaseWorkflow, caseRef())
	if err := env.GetWorkflowError(); err != nil || len(at) != 3 {
		t.Fatalf("workflow err=%v attempts=%d, want 3 attempts and no failure", err, len(at))
	}
	if gap1, gap2 := at[1].Sub(at[0]), at[2].Sub(at[1]); gap1 != 5*time.Second || gap2 != 10*time.Second {
		t.Fatalf("retry gaps %v and %v, want 5s and 10s", gap1, gap2)
	}
	if !f.seen[queued.EventID] {
		t.Fatal("a signal delivered during the deferral was not recorded")
	}
}

// A case canceled while its investigation is deferred ends without another attempt.
func TestCaseWorkflowDeferredInvestigationStopsOnCancel(t *testing.T) {
	env, f := newFakeCase(t, cases.WorkflowStatus{State: cases.Investigating, Revision: 2, AgentEnabled: true})
	env.SetTestTimeout(10 * time.Second)
	calls := 0
	env.RegisterActivityWithOptions(func(context.Context, InvestigateRef) (agent.Result, error) {
		calls++
		return agent.Result{}, temporal.NewNonRetryableApplicationError("investigation deferred", admissionBusy, nil)
	}, activity.RegisterOptions{Name: "InvestigateCase"})
	f.signalAt(2*time.Second, cases.WorkflowEvent{EventID: uuid.NewString(), Kind: "cancel", Revision: 3})
	env.ExecuteWorkflow(SupervisorCaseWorkflow, caseRef())
	if err := env.GetWorkflowError(); err != nil || calls != 1 || f.status.State != cases.Canceled {
		t.Fatalf("workflow err=%v calls=%d state=%s, want one attempt and a clean end", err, calls, f.status.State)
	}
}

func TestAdmissionWaitBacksOffToOneMinute(t *testing.T) {
	for n, want := range map[int]time.Duration{1: 5 * time.Second, 2: 10 * time.Second, 3: 20 * time.Second, 4: 40 * time.Second, 5: time.Minute, 50: time.Minute} {
		if got := admissionWait(n); got != want {
			t.Errorf("admissionWait(%d)=%v want %v", n, got, want)
		}
	}
}

// WIS-273 review B1: a case deferred by admission whose worker then reports the agent off must
// wait for a signal, not re-arm a zero-length retry timer forever. Before the fix this run made
// close to a million ReadCaseStatus calls before the cancel arrived.
func TestCaseWorkflowDeferredCaseDoesNotSpinWhenAgentTurnsOff(t *testing.T) {
	env, f := newFakeCase(t, cases.WorkflowStatus{State: cases.Investigating, Revision: 2, AgentEnabled: true})
	env.SetTestTimeout(10 * time.Second)
	calls := 0
	env.RegisterActivityWithOptions(func(context.Context, InvestigateRef) (agent.Result, error) {
		calls++
		f.status.AgentEnabled = false // this worker's agent mode is now off
		return agent.Result{}, temporal.NewNonRetryableApplicationError("investigation deferred", admissionBusy, nil)
	}, activity.RegisterOptions{Name: "InvestigateCase"})
	f.signalAt(time.Minute, cases.WorkflowEvent{EventID: uuid.NewString(), Kind: "cancel", Revision: 3})
	env.ExecuteWorkflow(SupervisorCaseWorkflow, caseRef())
	if err := env.GetWorkflowError(); err != nil || calls != 1 || f.reads > 5 || f.status.State != cases.Canceled {
		t.Fatalf("workflow err=%v investigations=%d status reads=%d state=%s, want one attempt, a handful of reads and a clean end", err, calls, f.reads, f.status.State)
	}
}

// WIS-273 review m2: deferrals add history, so a long deferral continues as new instead of
// growing one run toward the history limit.
func TestCaseWorkflowDeferralsCountTowardContinueAsNew(t *testing.T) {
	env, _ := newFakeCase(t, cases.WorkflowStatus{State: cases.Investigating, Revision: 2, AgentEnabled: true})
	env.SetTestTimeout(30 * time.Second)
	calls := 0
	env.RegisterActivityWithOptions(func(context.Context, InvestigateRef) (agent.Result, error) {
		calls++
		return agent.Result{}, temporal.NewNonRetryableApplicationError("investigation deferred", admissionBusy, nil)
	}, activity.RegisterOptions{Name: "InvestigateCase"})
	env.ExecuteWorkflow(SupervisorCaseWorkflow, caseRef())
	var can *workflow.ContinueAsNewError
	if err := env.GetWorkflowError(); !errors.As(err, &can) || calls != caseSignalsPerRun {
		t.Fatalf("workflow err=%v deferrals=%d, want continue-as-new after %d", err, calls, caseSignalsPerRun)
	}
}
