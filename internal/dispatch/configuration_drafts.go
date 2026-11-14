package dispatch

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/google/uuid"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

type ConfigurationDraftRef struct{ Tenant, Site, Session, Run string }

const configurationDraftDeadline = agent.InterviewTurnScheduleToClose + time.Minute

func ConfigurationDraftWorkflow(ctx workflow.Context, ref ConfigurationDraftRef) error {
	activityOptions := agentActivity
	activityOptions.ScheduleToStartTimeout = time.Minute
	activityOptions.ScheduleToCloseTimeout = agent.InterviewTurnScheduleToClose
	activityCtx, cancelActivity := workflow.WithCancel(workflow.WithActivityOptions(ctx, activityOptions))
	timerCtx, cancelTimer := workflow.WithCancel(ctx)
	defer cancelTimer()
	resultFuture := workflow.ExecuteActivity(activityCtx, "RunConfigurationDraft", ref)
	deadline := workflow.NewTimer(timerCtx, configurationDraftDeadline)
	selector := workflow.NewSelector(ctx)
	var result agent.ConfigurationDraftResult
	var activityErr error
	timedOut := false
	selector.AddFuture(resultFuture, func(f workflow.Future) { activityErr = f.Get(activityCtx, &result) })
	selector.AddFuture(deadline, func(f workflow.Future) { timedOut = f.Get(ctx, nil) == nil })
	selector.Select(ctx)
	cancelActivity()
	cancelTimer()
	if !timedOut && activityErr == nil {
		return nil
	}
	finalizerCtx, cancelFinalizer := workflow.NewDisconnectedContext(ctx)
	defer cancelFinalizer()
	finalizerCtx = workflow.WithActivityOptions(finalizerCtx, caseActivity)
	return workflow.ExecuteActivity(finalizerCtx, "FinalizeConfigurationDraftFailure", ref).Get(finalizerCtx, &result)
}

func (a *Activities) RunConfigurationDraft(ctx context.Context, ref ConfigurationDraftRef) (agent.ConfigurationDraftResult, error) {
	runDraft := a.draftConfiguration
	if runDraft == nil && a.Agent != nil {
		runDraft = a.Agent.DraftConfiguration
	}
	if runDraft == nil {
		return agent.ConfigurationDraftResult{}, temporal.NewNonRetryableApplicationError("agent runtime disabled", "disabled", nil)
	}
	if err := a.scope(ctx, ref.Tenant); err != nil {
		return agent.ConfigurationDraftResult{}, err
	}
	stopHeartbeat := startActivityHeartbeat(ctx)
	defer stopHeartbeat()
	out, err := runDraft(ctx, agent.ConfigurationDraftRef{TenantID: ref.Tenant, SiteID: ref.Site, SessionID: ref.Session, RunID: ref.Run})
	switch {
	case errors.Is(err, agent.ErrInvalidRequest), errors.Is(err, agent.ErrConversationDisabled):
		return agent.ConfigurationDraftResult{}, temporal.NewNonRetryableApplicationError("configuration draft unavailable", "scope", nil)
	case errors.Is(err, agent.ErrBudgetExhausted), errors.Is(err, agent.ErrContextOverflow):
		return agent.ConfigurationDraftResult{}, temporal.NewNonRetryableApplicationError("configuration draft budget exhausted", "budget", nil)
	case errors.Is(err, agent.ErrRunCanceled):
		return agent.ConfigurationDraftResult{}, temporal.NewNonRetryableApplicationError("configuration draft canceled", "canceled", nil)
	}
	var toolErr *agent.ToolError
	if errors.As(err, &toolErr) && toolErr.Fatal {
		return agent.ConfigurationDraftResult{}, temporal.NewNonRetryableApplicationError("configuration draft tool rejected", "tool_rejected", nil)
	}
	return out, err
}

// FinalizeConfigurationDraftFailure closes a run its activity could not finish. It skips the active-tenant check on
// purpose, as FinalizeChatAnswerFailure explains (WIS-699).
func (a *Activities) FinalizeConfigurationDraftFailure(ctx context.Context, ref ConfigurationDraftRef) (agent.ConfigurationDraftResult, error) {
	tenant, err := uuid.Parse(ref.Tenant)
	if err != nil {
		return agent.ConfigurationDraftResult{}, temporal.NewNonRetryableApplicationError("invalid scope", "scope", nil)
	}
	ref.Tenant = tenant.String()
	if len(a.Allow) > 0 && !a.Allow[ref.Tenant] {
		return agent.ConfigurationDraftResult{}, temporal.NewNonRetryableApplicationError("unprovisioned scope", "scope", nil)
	}
	return agent.FinalizeConfigurationDraftFailure(ctx, agent.NewStore(a.Store.DB), agent.ConfigurationDraftRef{TenantID: ref.Tenant, SiteID: ref.Site, SessionID: ref.Session, RunID: ref.Run})
}

type pendingConfigurationDraft struct{ outboxID, site, session, run string }

func RelayConfigurationDrafts(ctx context.Context, db *sql.DB, c client.Client, cfg config.Config, tenants []string) (int, error) {
	n := 0
	var failed []error
	for _, tenant := range tenants {
		id, err := uuid.Parse(tenant)
		if err != nil {
			failed = append(failed, err)
			continue
		}
		tenant = id.String()
		rows, err := pendingConfigurationDrafts(ctx, db, tenant)
		if err != nil {
			failed = append(failed, err)
			continue
		}
		for _, row := range rows {
			workflowID := "configuration-draft:" + tenant + ":" + row.run
			_, err = c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: workflowID, TaskQueue: cfg.TemporalQueue, WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE, WorkflowExecutionErrorWhenAlreadyStarted: true}, ConfigurationDraftWorkflow, ConfigurationDraftRef{Tenant: tenant, Site: row.site, Session: row.session, Run: row.run})
			var duplicate *serviceerror.WorkflowExecutionAlreadyStarted
			if err != nil && !errors.As(err, &duplicate) {
				failed = append(failed, err)
				continue
			}
			if err = markConfigurationDraftDelivered(ctx, db, tenant, row.outboxID); err != nil {
				failed = append(failed, err)
				continue
			}
			n++
		}
	}
	return n, errors.Join(failed...)
}

func pendingConfigurationDrafts(ctx context.Context, db *sql.DB, tenant string) ([]pendingConfigurationDraft, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id::text,session_id::text,run_id::text,site_id::text FROM supervisor.configuration_draft_request_outbox WHERE tenant_id=$1 AND delivered_at IS NULL ORDER BY id LIMIT 100`, tenant)
	if err != nil {
		return nil, err
	}
	out := []pendingConfigurationDraft{}
	for rows.Next() {
		var row pendingConfigurationDraft
		if err = rows.Scan(&row.outboxID, &row.session, &row.run, &row.site); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, row)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func markConfigurationDraftDelivered(ctx context.Context, db *sql.DB, tenant, id string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE supervisor.configuration_draft_request_outbox SET delivered_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2 AND delivered_at IS NULL`, tenant, id); err != nil {
		return err
	}
	return tx.Commit()
}
