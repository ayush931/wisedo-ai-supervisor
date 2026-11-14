package dispatch

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
)

// WIS-625 slice B1: a queued scan job is read by the vision runtime in one activity. A deadline or
// a lost worker quarantines the job (EXTRACTION_FAILED) and clears its image; nothing is retried
// against the model, because each page's call is budgeted once.
//
// WIS-717: a full vision queue is not a failure of the page. The worker's vision slots are shared
// by every tenant, so another tenant's scans filling them used to quarantine this page and delete
// its image without the model ever reading it. RunScan now reports a full queue as admission_busy
// before any model call is made, and the workflow waits (admissionWait, as investigations do) and
// tries again, for at most scanBusyLimit, before it quarantines.

type ScanRef struct{ Tenant, Site, Job, Run string }

const (
	scanScheduleToStart = time.Minute
	scanScheduleToClose = 4 * time.Minute
	scanDeadline        = scanScheduleToClose + time.Minute
	// scanBusyLimit bounds the total time a page waits for the worker's vision capacity.
	scanBusyLimit = 30 * time.Minute
)

func ScanWorkflow(ctx workflow.Context, ref ScanRef) error {
	started := workflow.Now(ctx)
	for deferrals := 0; ; {
		timedOut, scanErr := runScanAttempt(ctx, ref)
		if !timedOut && scanErr == nil {
			return nil
		}
		var appErr *temporal.ApplicationError
		if timedOut || !errors.As(scanErr, &appErr) || appErr.Type() != admissionBusy {
			break
		}
		deferrals++
		wait := admissionWait(deferrals)
		if workflow.Now(ctx).Add(wait).Sub(started) > scanBusyLimit || workflow.Sleep(ctx, wait) != nil {
			break
		}
	}
	var result agent.ScanResult
	finalizerCtx, cancelFinalizer := workflow.NewDisconnectedContext(ctx)
	defer cancelFinalizer()
	finalizerCtx = workflow.WithActivityOptions(finalizerCtx, caseActivity)
	return workflow.ExecuteActivity(finalizerCtx, "FinalizeScanFailure", ref).Get(finalizerCtx, &result)
}

// runScanAttempt runs RunScan once under the per-attempt deadline; it reports whether the deadline
// fired first and the activity's error.
func runScanAttempt(ctx workflow.Context, ref ScanRef) (bool, error) {
	opts := agentActivity
	opts.ScheduleToStartTimeout, opts.ScheduleToCloseTimeout = scanScheduleToStart, scanScheduleToClose
	scanCtx, cancelScan := workflow.WithCancel(workflow.WithActivityOptions(ctx, opts))
	defer cancelScan()
	timerCtx, cancelTimer := workflow.WithCancel(ctx)
	defer cancelTimer()
	scan := workflow.ExecuteActivity(scanCtx, "RunScan", ref)
	deadline := workflow.NewTimer(timerCtx, scanDeadline)
	var result agent.ScanResult
	var scanErr error
	timedOut := false
	selector := workflow.NewSelector(ctx)
	selector.AddFuture(scan, func(f workflow.Future) { scanErr = f.Get(scanCtx, &result) })
	selector.AddFuture(deadline, func(f workflow.Future) { timedOut = f.Get(ctx, nil) == nil })
	selector.Select(ctx)
	return timedOut, scanErr
}

func (a *Activities) RunScan(ctx context.Context, ref ScanRef) (agent.ScanResult, error) {
	if a.Vision == nil {
		return agent.ScanResult{}, temporal.NewNonRetryableApplicationError("vision model not configured", "disabled", nil)
	}
	if err := a.scope(ctx, ref.Tenant); err != nil {
		return agent.ScanResult{}, err
	}
	stop := startActivityHeartbeat(ctx)
	defer stop()
	out, err := a.Vision.ExtractScan(ctx, agent.ScanRef{TenantID: ref.Tenant, SiteID: ref.Site, JobID: ref.Job, RunID: ref.Run})
	switch {
	case errors.Is(err, agent.ErrInvalidRequest):
		return agent.ScanResult{}, temporal.NewNonRetryableApplicationError("scan unavailable", "scope", nil)
	case errors.Is(err, agent.ErrQueueFull):
		// Nothing was sent: the page waits in the workflow, not in a retry against a full queue.
		return agent.ScanResult{}, temporal.NewNonRetryableApplicationError("vision model busy", admissionBusy, nil)
	}
	return out, err
}

// FinalizeScanFailure closes a run its activity could not finish. It skips the active-tenant check on
// purpose, as FinalizeChatAnswerFailure explains (WIS-699).
func (a *Activities) FinalizeScanFailure(ctx context.Context, ref ScanRef) (agent.ScanResult, error) {
	tenant, err := uuid.Parse(ref.Tenant)
	if err != nil {
		return agent.ScanResult{}, temporal.NewNonRetryableApplicationError("invalid scope", "scope", nil)
	}
	ref.Tenant = tenant.String()
	if len(a.Allow) > 0 && !a.Allow[ref.Tenant] {
		return agent.ScanResult{}, temporal.NewNonRetryableApplicationError("unprovisioned scope", "scope", nil)
	}
	return agent.FinalizeScanFailure(ctx, agent.NewStore(a.Store.DB), agent.ScanRef{TenantID: ref.Tenant, SiteID: ref.Site, JobID: ref.Job, RunID: ref.Run})
}

// visionRuntime is the runtime OCR runs on: the pinned local vision model, or the deterministic
// fixture outside production. nil without a vision model: scans are refused at upload.
func visionRuntime(cfg config.Config, db *store.Store) (*agent.Runtime, error) {
	a := cfg.Agent
	if a.VisionModel == "" || !a.ModeEnabled {
		return nil, nil
	}
	var p agent.Provider
	switch a.Provider {
	case config.AgentProviderFixture:
		// A fixture script may script the page transcription (its "ocr" purpose, WIS-643); without
		// one, or without that purpose, every page is quarantined, as before.
		if a.FixtureScript == "" {
			p = agent.NewFixtureProvider()
		} else {
			s, err := agent.LoadFixtureProvider(a.FixtureScript)
			if err != nil {
				return nil, err
			}
			p = s
		}
	case config.AgentProviderLocal:
		m, err := agent.NewVisionModel(a, slog.Default(), time.Now)
		if err != nil {
			return nil, err
		}
		p = m
	default:
		return nil, nil
	}
	limits := agent.ExtractionLimits()
	s := agent.NewStore(db.DB)
	return &agent.Runtime{Store: s, Provider: p, Tools: &agent.Tools{Store: s}, Slots: agent.NewSlots(1, a.MaxQueued), Limits: limits,
		Model: a.VisionModel, ModelRevision: a.VisionModelRevision, Options: agent.LoaderOptions{ModeEnabled: a.ModeEnabled}, Logger: slog.Default()}, nil
}

func RelayScans(ctx context.Context, db *sql.DB, c client.Client, cfg config.Config, tenants []string) (int, error) {
	n := 0
	var failed []error
	for _, tenant := range tenants {
		id, err := uuid.Parse(tenant)
		if err != nil {
			failed = append(failed, err)
			continue
		}
		tenant = id.String()
		rows, err := pendingScans(ctx, db, tenant)
		if err != nil {
			failed = append(failed, err)
			continue
		}
		for _, row := range rows {
			_, err = c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "source-scan:" + tenant + ":" + row.Run, TaskQueue: cfg.TemporalQueue,
				WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE, WorkflowExecutionErrorWhenAlreadyStarted: true}, ScanWorkflow, row.ScanRef)
			var duplicate *serviceerror.WorkflowExecutionAlreadyStarted
			if err != nil && !errors.As(err, &duplicate) {
				failed = append(failed, err)
				continue
			}
			if err = markScanDelivered(ctx, db, tenant, row.outbox); err != nil {
				failed = append(failed, err)
				continue
			}
			n++
		}
	}
	return n, errors.Join(failed...)
}

type pendingScan struct {
	ScanRef
	outbox string
}

func pendingScans(ctx context.Context, db *sql.DB, tenant string) ([]pendingScan, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id::text,site_id::text,job_id::text,run_id::text FROM supervisor.document_scan_outbox WHERE tenant_id=$1 AND delivered_at IS NULL ORDER BY id LIMIT 100`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pendingScan
	for rows.Next() {
		p := pendingScan{ScanRef: ScanRef{Tenant: tenant}}
		if err = rows.Scan(&p.outbox, &p.Site, &p.Job, &p.Run); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func markScanDelivered(ctx context.Context, db *sql.DB, tenant, id string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE supervisor.document_scan_outbox SET delivered_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2 AND delivered_at IS NULL`, tenant, id); err != nil {
		return err
	}
	return tx.Commit()
}
