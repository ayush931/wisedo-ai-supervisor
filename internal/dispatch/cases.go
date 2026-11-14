package dispatch

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/cases"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/outcome"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/retrieval"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/tasks"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const (
	// CaseSignal carries one cases.WorkflowEvent from the case outbox.
	CaseSignal = "case-event"
	// deadlineRecheck is the shortest wait after a deadline the database did not yet accept,
	// so a workflow clock ahead of the database clock cannot spin activities.
	deadlineRecheck = time.Minute
	caseRelayBatch  = 100
)

// caseSignalsPerRun bounds history: after this many processed signals the workflow continues
// as new, carrying only its reference (WIS-273: after 1000 signals). A variable only so a test
// can lower it.
var caseSignalsPerRun = 1000

// caseActivity is the WIS-273 transient bound: at most 3 attempts with 1 s then 2 s backoff,
// and no retry multiplication inside the activities themselves.
var caseActivity = workflow.ActivityOptions{
	StartToCloseTimeout: 30 * time.Second,
	RetryPolicy:         &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: 2 * time.Second, MaximumAttempts: 3},
}

// admissionBusy is the InvestigateCase error type for a site or worker at its investigation bound.
const admissionBusy = "admission_busy"

// admissionWait is the wait before the n-th consecutive deferred investigation is tried again:
// 5 s doubling to at most 60 s. Each case keeps its own backoff, so a site with many waiting
// cases holds at most its two places and other sites are admitted as places free up.
func admissionWait(n int) time.Duration {
	d := 5 * time.Second
	for i := 1; i < n && d < time.Minute; i++ {
		d *= 2
	}
	return min(d, time.Minute)
}

var agentActivity = workflow.ActivityOptions{
	StartToCloseTimeout: 4 * time.Minute,
	HeartbeatTimeout:    30 * time.Second,
	RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 2, NonRetryableErrorTypes: []string{
		"scope", "budget", "canceled", "tool_rejected", "model_not_pinned",
	}},
}

// SupervisorCaseWorkflow owns the scheduling of one case: it wakes on outbox signals and on
// the human-wait timer, and does nothing else. PostgreSQL owns business state: every decision
// re-reads the case through an activity, every signal is deduplicated by event ID in the
// product database, and the deadline escalation is fenced on the case revision. The workflow
// ends when the case is terminal; a later signal starts a fresh run that ends at once.
func SupervisorCaseWorkflow(ctx workflow.Context, ref cases.WorkflowRef) error {
	actx := workflow.WithActivityOptions(ctx, caseActivity)
	signals := workflow.GetSignalChannel(ctx, CaseSignal)
	// drain records every delivery already queued, so the ledger stays complete when the run
	// ends or continues as new (an undrained signal is lost: the relay already marked it
	// delivered).
	drain := func() error {
		var event cases.WorkflowEvent
		for signals.ReceiveAsync(&event) {
			var duplicate bool
			if err := workflow.ExecuteActivity(actx, "RecordCaseEvent", ref, event).Get(ctx, &duplicate); err != nil {
				return caseRunError(err)
			}
		}
		return nil
	}
	// Each run first records every outbox delivery of the case, so a signal lost with a failed
	// run is still recorded. Versioned: histories recorded before this step replay unchanged.
	if workflow.GetVersion(ctx, "reconcile-case-outbox", workflow.DefaultVersion, 1) == 1 {
		var added int
		if err := workflow.ExecuteActivity(actx, "ReconcileCaseEvents", ref).Get(ctx, &added); err != nil {
			return caseRunError(err)
		}
	}
	processed := 0
	var recheck time.Time
	var investigated cases.WorkflowStatus // the case state and revision the agent last ran for
	var deferredUntil time.Time           // a deferred investigation is not tried again before this
	deferrals := 0
	for {
		var st cases.WorkflowStatus
		if err := workflow.ExecuteActivity(actx, "ReadCaseStatus", ref).Get(ctx, &st); err != nil {
			return caseRunError(err)
		}
		// An investigation that left the case unchanged is not repeated at once: the workflow waits
		// for the next signal instead of spinning on a replayed terminal run. Versioned: a history
		// recorded before this rule re-ran the activity and replays unchanged.
		again := st.State == investigated.State && st.Revision == investigated.Revision
		if workflow.GetVersion(ctx, "agent-investigation", workflow.DefaultVersion, 1) == 1 && st.State == cases.Investigating && st.AgentEnabled &&
			(!again || workflow.GetVersion(ctx, "agent-investigation-wait", workflow.DefaultVersion, 1) == workflow.DefaultVersion) &&
			!workflow.Now(ctx).Before(deferredUntil) {
			agctx := workflow.WithActivityOptions(ctx, agentActivity)
			investigation := InvestigateRef{Tenant: ref.TenantID, Site: ref.SiteID, Case: ref.CaseID, Revision: st.Revision}
			var result agent.Result
			if err := workflow.ExecuteActivity(agctx, "InvestigateCase", investigation).Get(ctx, &result); err != nil {
				// A site or worker at its investigation bound defers the case: it stays investigating
				// and is tried again after a backoff, while signals are still recorded. Versioned: only
				// a history that met this error takes the new branch.
				var appErr *temporal.ApplicationError
				if errors.As(err, &appErr) && appErr.Type() == admissionBusy &&
					workflow.GetVersion(ctx, "investigation-admission", workflow.DefaultVersion, 1) == 1 {
					deferrals++
					deferredUntil = workflow.Now(ctx).Add(admissionWait(deferrals))
					// A deferral adds history like a signal does, so it counts toward continue-as-new.
					processed++
					continue
				}
				return caseRunError(err)
			}
			investigated, deferrals, deferredUntil = st, 0, time.Time{}
			continue
		}
		// A deferral only waits for a place while this worker could still investigate the case. If
		// the case moved on or the agent is off here, the pending retry is dropped: a timer re-armed
		// at zero would otherwise spin without bound (WIS-273 review B1).
		if st.State != cases.Investigating || !st.AgentEnabled {
			deferrals, deferredUntil = 0, time.Time{}
		}
		if st.Terminal {
			// A signal arriving later starts a fresh run that ends here too.
			return drain()
		}
		if processed >= caseSignalsPerRun {
			if err := drain(); err != nil {
				return err
			}
			return workflow.NewContinueAsNewError(ctx, SupervisorCaseWorkflow, ref)
		}
		selector := workflow.NewSelector(ctx)
		var event cases.WorkflowEvent
		received := false
		selector.AddReceive(signals, func(c workflow.ReceiveChannel, _ bool) {
			c.Receive(ctx, &event)
			received = true
		})
		fired := false
		cancelTimer := func() {}
		retry := false
		cancelRetry := func() {}
		if !deferredUntil.IsZero() {
			var rctx workflow.Context
			rctx, cancelRetry = workflow.WithCancel(ctx)
			selector.AddFuture(workflow.NewTimer(rctx, max(deferredUntil.Sub(workflow.Now(ctx)), 0)), func(f workflow.Future) { retry = f.Get(rctx, nil) == nil })
		}
		if st.WaitingOnHuman {
			deadline := st.WaitDeadline
			if recheck.After(deadline) {
				deadline = recheck
			}
			wait := deadline.Sub(workflow.Now(ctx))
			if wait < 0 {
				wait = 0
			}
			var tctx workflow.Context
			tctx, cancelTimer = workflow.WithCancel(ctx)
			selector.AddFuture(workflow.NewTimer(tctx, wait), func(f workflow.Future) { fired = f.Get(tctx, nil) == nil })
		}
		selector.Select(ctx)
		cancelTimer()
		cancelRetry()
		if retry {
			continue
		}
		if received {
			var duplicate bool
			if err := workflow.ExecuteActivity(actx, "RecordCaseEvent", ref, event).Get(ctx, &duplicate); err != nil {
				return caseRunError(err)
			}
			processed++
			continue
		}
		if fired {
			var escalated bool
			if err := workflow.ExecuteActivity(actx, "EscalateCaseOnDeadline", ref, st.Revision).Get(ctx, &escalated); err != nil {
				return caseRunError(err)
			}
			recheck = time.Time{}
			if !escalated {
				recheck = workflow.Now(ctx).Add(deadlineRecheck)
			}
		}
	}
}

// caseWorkflowRetry restarts a run that failed after its activities used their attempts (for
// example a database outage longer than the activity backoff), so the human-wait timer is
// re-armed from PostgreSQL instead of being lost. There is no attempt limit: the run reads the
// case again and stops once it is terminal. A variable only so a test can shorten it.
var caseWorkflowRetry = &temporal.RetryPolicy{InitialInterval: 10 * time.Second, BackoffCoefficient: 2, MaximumInterval: 5 * time.Minute}

// caseRunError ends a run on a scope or contract failure without a workflow retry (the case or
// tenant is not available to this worker); any other activity failure fails the run so
// caseWorkflowRetry restarts it.
func caseRunError(err error) error {
	var app *temporal.ApplicationError
	if errors.As(err, &app) {
		switch app.Type() {
		case "scope", "budget", "canceled", "tool_rejected", "model_not_pinned":
			return temporal.NewNonRetryableApplicationError("case activity is not retryable", app.Type(), err)
		}
	}
	return err
}

// caseError maps a case service failure to a Temporal outcome: a scope or contract failure is
// never retried; anything else (storage) is.
func caseError(err error) error {
	if errors.Is(err, cases.ErrNotFound) || errors.Is(err, cases.ErrInvalid) {
		return temporal.NewNonRetryableApplicationError("case unavailable", "scope", nil)
	}
	return err
}

func (a *Activities) caseScope(ctx context.Context, ref cases.WorkflowRef) error {
	return a.scope(ctx, ref.TenantID)
}

// ReadCaseStatus reads the case for its workflow.
func (a *Activities) ReadCaseStatus(ctx context.Context, ref cases.WorkflowRef) (cases.WorkflowStatus, error) {
	if err := a.caseScope(ctx, ref); err != nil {
		return cases.WorkflowStatus{}, err
	}
	st, err := a.Cases.WorkflowStatus(ctx, ref)
	st.AgentEnabled = a.agentEnabled()
	return st, caseError(err)
}

// agentEnabled is whether this worker's built agent runtime investigates cases: nil when no
// provider is configured or the runtime could not be built (WIS-636).
func (a *Activities) agentEnabled() bool {
	return a.Agent != nil && a.Agent.Options.ModeEnabled
}

// RecordCaseEvent records one processed signal and reports whether it was a duplicate.
func (a *Activities) RecordCaseEvent(ctx context.Context, ref cases.WorkflowRef, event cases.WorkflowEvent) (bool, error) {
	if err := a.caseScope(ctx, ref); err != nil {
		return false, err
	}
	duplicate, err := a.Cases.RecordWorkflowEvent(ctx, ref, event)
	return duplicate, caseError(err)
}

// ReconcileCaseEvents records the case's outbox deliveries that are missing from the ledger.
func (a *Activities) ReconcileCaseEvents(ctx context.Context, ref cases.WorkflowRef) (int, error) {
	if err := a.caseScope(ctx, ref); err != nil {
		return 0, err
	}
	added, err := a.Cases.ReconcileWorkflowEvents(ctx, ref)
	return added, caseError(err)
}

// EscalateCaseOnDeadline escalates a case still waiting on a person at the given revision.
func (a *Activities) EscalateCaseOnDeadline(ctx context.Context, ref cases.WorkflowRef, revision int64) (bool, error) {
	if err := a.caseScope(ctx, ref); err != nil {
		return false, err
	}
	escalated, err := a.Cases.EscalateOnDeadline(ctx, ref, revision)
	return escalated, caseError(err)
}

// RelayCases delivers undelivered case outbox rows to their workflows with signal-with-start
// and marks each delivered only after Temporal accepted it. Without Temporal the rows stay
// pending (the case is queued, not lost); a row delivered twice is deduplicated by event ID.
func RelayCases(ctx context.Context, db *store.Store, c client.Client, cfg config.Config, tenants []string) (int, error) {
	service := cases.NewService(db.DB)
	n := 0
	var failed []error
	for _, tenant := range tenants {
		delivered, err := relayTenant(ctx, service, c, cfg, tenant)
		n += delivered
		if err != nil {
			// One tenant's failure never holds back another tenant's deliveries.
			failed = append(failed, err)
		}
	}
	return n, errors.Join(failed...)
}

func relayTenant(ctx context.Context, service *cases.Service, c client.Client, cfg config.Config, tenant string) (int, error) {
	n := 0
	rows, err := service.PendingOutbox(ctx, tenant, caseRelayBatch)
	if err != nil {
		return n, err
	}
	for _, row := range rows {
		id := cases.WorkflowID(row.Ref.TenantID, row.Ref.CaseID)
		if _, err = c.SignalWithStartWorkflow(ctx, id, CaseSignal, row.Event, client.StartWorkflowOptions{ID: id, TaskQueue: cfg.TemporalQueue, RetryPolicy: caseWorkflowRetry}, SupervisorCaseWorkflow, row.Ref); err != nil {
			return n, err
		}
		if err = service.MarkDelivered(ctx, row); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ExecuteApproved carries closed approval decisions to their cases for every provisioned tenant
// (WIS-277): with task execution enabled an approved proposal is consumed into one internal task;
// a rejected, revoked, expired or refused one returns the case to needs_input.
func ExecuteApproved(ctx context.Context, db *store.Store, cfg config.Config, tenants []string) (int, error) {
	// The executor re-evaluates every bound evidence revision with the agent runtime's rules.
	service := cases.NewService(db.DB).WithBoundEvidence(agent.DispatchEvidenceCode)
	n := 0
	var failed []error
	for _, tenant := range tenants {
		done, err := service.ExecuteApproved(ctx, tenant, cfg.TaskExecution, caseRelayBatch)
		for _, e := range done {
			if e.Outcome != cases.ExecutionWaiting {
				n++
			}
		}
		if err != nil {
			// One tenant's failure never holds back another tenant's execution.
			failed = append(failed, err)
		}
	}
	return n, errors.Join(failed...)
}

// ObserveOutcomes decides the outcome of every completed task's current completion for every
// provisioned tenant once its post-completion windows allow (WIS-277). It only measures; a
// supervisor closes the case.
func ObserveOutcomes(ctx context.Context, db *store.Store, tenants []string) (int, error) {
	service := tasks.NewService(db.DB)
	n := 0
	var failed []error
	for _, tenant := range tenants {
		done, err := service.ObserveOutcomes(ctx, tenant, caseRelayBatch)
		for _, o := range done {
			if o.Status != outcome.Pending {
				n++
			}
		}
		if err != nil {
			// One tenant's failure never holds back another tenant's observation.
			failed = append(failed, err)
		}
	}
	return n, errors.Join(failed...)
}

// Evaluator evaluates newly created plant contexts of every provisioned tenant against the
// shortfall rule, opening or joining cases through the checked trigger path. The worker runs it
// on its own loop, outside Tick, so an approved action never waits for another tenant's
// evaluation (WIS-718). Each pass gives every tenant the same Budget, starting from a different
// tenant each pass, and each tenant resumes where its previous pass stopped. A context whose
// evaluation fails is retried on a later pass, and one tenant's failure does not stop the others.
// It is not safe for concurrent passes; the worker runs one at a time.
type Evaluator struct {
	DB     *store.Store
	Budget cases.EvaluationBudget
	passes int
	next   map[string]*cases.EvaluationCursor
}

// NewEvaluator returns an evaluator with the default per-tenant budget.
func NewEvaluator(db *store.Store) *Evaluator {
	return &Evaluator{DB: db, Budget: cases.DefaultEvaluationBudget}
}

// Pass discovers the active tenants (within allow) and evaluates each within its budget.
func (e *Evaluator) Pass(ctx context.Context, allow []string) (int, error) {
	tenants, err := Tenants(ctx, e.DB, allow)
	if err != nil {
		return 0, err
	}
	return e.Evaluate(ctx, tenants)
}

// Evaluate runs one pass over the given tenants.
func (e *Evaluator) Evaluate(ctx context.Context, tenants []string) (int, error) {
	service := cases.NewService(e.DB.DB)
	next := make(map[string]*cases.EvaluationCursor, len(tenants))
	n := 0
	var failed []error
	for i := range tenants {
		tenant := tenants[(i+e.passes)%len(tenants)]
		done, after, err := service.EvaluatePass(ctx, tenant, cases.EvaluatorActor, e.Budget, e.next[tenant])
		next[tenant] = after
		n += len(done)
		if err != nil {
			// One tenant's failure never holds back another tenant's evaluation.
			failed = append(failed, err)
		}
	}
	e.next = next
	e.passes++
	return n, errors.Join(failed...)
}

// StartDetectedCases moves each provisioned tenant's detected cases with a READY bound context to
// investigating (WIS-636). The scope is enabled exactly when the case workflow would investigate:
// agentEnabled is Worker.AgentEnabled, from the built runtime, so a configured runtime that could
// not be built leaves cases detected. With the agent off, cases stay detected and move on the
// first tick after it is turned on.
func StartDetectedCases(ctx context.Context, db *store.Store, enabled bool, tenants []string) (int, error) {
	service := cases.NewService(db.DB)
	n := 0
	var failed []error
	for _, tenant := range tenants {
		moved, err := service.StartDetected(ctx, tenant, enabled, caseRelayBatch)
		n += moved
		if err != nil {
			// One tenant's failure never holds back another tenant's cases.
			failed = append(failed, err)
		}
	}
	return n, errors.Join(failed...)
}

// embedBatch bounds how many evidence spans one pass embeds per tenant in one request (WIS-627).
// A CPU endpoint was measured at ~4s for one short text; at that rate 8 spans take ~32s, about
// half of the 60s default SUPERVISOR_EMBEDDINGS_INDEX_TIMEOUT. Passes run back to back.
const embedBatch = 8

// EmbedSpans embeds, per tenant, the evidence spans that have no embedding for the configured
// model revision yet, when semantic retrieval is on. An endpoint failure is logged as a safe class:
// an unreachable endpoint is retried next tick, a refused span is deferred with backoff. Until a
// span is embedded, chat ranks that site's question by words.
func EmbedSpans(ctx context.Context, db *store.Store, tenants []string) (int, error) {
	c := retrieval.Default()
	if c == nil {
		return 0, nil
	}
	n := 0
	var failed []error
	for _, tenant := range tenants {
		done, err := retrieval.EmbedPending(ctx, db.DB, c, tenant, embedBatch)
		n += done
		if err != nil {
			if errors.Is(err, retrieval.ErrTimeout) {
				log.Printf("embedding endpoint timed out after %s (SUPERVISOR_EMBEDDINGS_INDEX_TIMEOUT) for tenant %s; spans retried next tick", c.IndexTimeout(), tenant)
				continue
			}
			if errors.Is(err, retrieval.ErrEndpoint) || errors.Is(err, retrieval.ErrDimension) {
				log.Printf("embedding endpoint unreachable or refused spans for tenant %s; refused spans deferred, the rest retried next tick", tenant)
				continue
			}
			failed = append(failed, err)
		}
	}
	return n, errors.Join(failed...)
}
