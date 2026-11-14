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
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

type InterviewTurnRef struct{ Tenant, Site, Session, Run string }

const (
	interviewTurnScheduleToStart = time.Minute
	interviewTurnDeadline        = agent.InterviewTurnScheduleToClose + time.Minute
)

func InterviewTurnWorkflow(ctx workflow.Context, ref InterviewTurnRef) error {
	turnActivity := agentActivity
	turnActivity.ScheduleToStartTimeout = interviewTurnScheduleToStart
	turnActivity.ScheduleToCloseTimeout = agent.InterviewTurnScheduleToClose
	turnCtx, cancelTurn := workflow.WithCancel(workflow.WithActivityOptions(ctx, turnActivity))
	timerCtx, cancelTimer := workflow.WithCancel(ctx)
	defer cancelTimer()
	turn := workflow.ExecuteActivity(turnCtx, "RunInterviewTurn", ref)
	deadline := workflow.NewTimer(timerCtx, interviewTurnDeadline)
	selector := workflow.NewSelector(ctx)
	var result agent.InterviewTurnResult
	var turnErr error
	timedOut := false
	selector.AddFuture(turn, func(f workflow.Future) { turnErr = f.Get(turnCtx, &result) })
	selector.AddFuture(deadline, func(f workflow.Future) { timedOut = f.Get(ctx, nil) == nil })
	selector.Select(ctx)
	cancelTurn()
	cancelTimer()
	if !timedOut && turnErr == nil {
		return nil
	}
	finalizerCtx, cancelFinalizer := workflow.NewDisconnectedContext(ctx)
	defer cancelFinalizer()
	finalizerCtx = workflow.WithActivityOptions(finalizerCtx, caseActivity)
	return workflow.ExecuteActivity(finalizerCtx, "FinalizeInterviewTurnFailure", ref).Get(finalizerCtx, &result)
}

func (a *Activities) RunInterviewTurn(ctx context.Context, ref InterviewTurnRef) (agent.InterviewTurnResult, error) {
	if a.Agent == nil {
		return agent.InterviewTurnResult{}, temporal.NewNonRetryableApplicationError("agent runtime disabled", "disabled", nil)
	}
	if err := a.scope(ctx, ref.Tenant); err != nil {
		return agent.InterviewTurnResult{}, err
	}
	stopHeartbeat := startActivityHeartbeat(ctx)
	defer stopHeartbeat()
	out, err := a.Agent.InterviewTurn(ctx, agent.InterviewTurnRef{TenantID: ref.Tenant, SiteID: ref.Site, SessionID: ref.Session, RunID: ref.Run})
	switch {
	case errors.Is(err, agent.ErrInvalidRequest), errors.Is(err, agent.ErrConversationDisabled):
		return agent.InterviewTurnResult{}, temporal.NewNonRetryableApplicationError("interview turn unavailable", "scope", nil)
	case errors.Is(err, agent.ErrBudgetExhausted), errors.Is(err, agent.ErrContextOverflow):
		return agent.InterviewTurnResult{}, temporal.NewNonRetryableApplicationError("interview turn budget exhausted", "budget", nil)
	case errors.Is(err, agent.ErrRunCanceled):
		return agent.InterviewTurnResult{}, temporal.NewNonRetryableApplicationError("interview turn canceled", "canceled", nil)
	}
	return out, err
}

func startActivityHeartbeat(ctx context.Context) func() {
	heartbeatCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				activity.RecordHeartbeat(heartbeatCtx)
			}
		}
	}()
	return func() { stop(); <-done }
}

// FinalizeInterviewTurnFailure closes a run its activity could not finish. It skips the active-tenant check on
// purpose, as FinalizeChatAnswerFailure explains (WIS-699).
func (a *Activities) FinalizeInterviewTurnFailure(ctx context.Context, ref InterviewTurnRef) (agent.InterviewTurnResult, error) {
	tenant, err := uuid.Parse(ref.Tenant)
	if err != nil {
		return agent.InterviewTurnResult{}, temporal.NewNonRetryableApplicationError("invalid scope", "scope", nil)
	}
	ref.Tenant = tenant.String()
	if len(a.Allow) > 0 && !a.Allow[ref.Tenant] {
		return agent.InterviewTurnResult{}, temporal.NewNonRetryableApplicationError("unprovisioned scope", "scope", nil)
	}
	return agent.FinalizeInterviewTurnFailure(ctx, agent.NewStore(a.Store.DB), agent.InterviewTurnRef{TenantID: ref.Tenant, SiteID: ref.Site, SessionID: ref.Session, RunID: ref.Run})
}

type pendingInterviewTurn struct{ outboxID, site, session, run string }

func RelayInterviewTurns(ctx context.Context, db *sql.DB, c client.Client, cfg config.Config, tenants []string) (int, error) {
	n := 0
	var failed []error
	for _, tenant := range tenants {
		id, err := uuid.Parse(tenant)
		if err != nil {
			failed = append(failed, err)
			continue
		}
		tenant = id.String()
		rows, err := pendingTurns(ctx, db, tenant)
		if err != nil {
			failed = append(failed, err)
			continue
		}
		for _, row := range rows {
			wfID := "interview-turn:" + tenant + ":" + row.run
			_, err = c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: wfID, TaskQueue: cfg.TemporalQueue, WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE, WorkflowExecutionErrorWhenAlreadyStarted: true}, InterviewTurnWorkflow, InterviewTurnRef{Tenant: tenant, Site: row.site, Session: row.session, Run: row.run})
			var duplicate *serviceerror.WorkflowExecutionAlreadyStarted
			if err != nil && !errors.As(err, &duplicate) {
				failed = append(failed, err)
				continue
			}
			if err = markTurnDelivered(ctx, db, tenant, row.outboxID); err != nil {
				failed = append(failed, err)
				continue
			}
			n++
		}
	}
	return n, errors.Join(failed...)
}

func pendingTurns(ctx context.Context, db *sql.DB, tenant string) ([]pendingInterviewTurn, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id::text,session_id::text,run_id::text,site_id::text FROM supervisor.interview_agent_turn_outbox WHERE tenant_id=$1 AND delivered_at IS NULL ORDER BY id LIMIT 100`, tenant)
	if err != nil {
		return nil, err
	}
	type pending struct{ id, session, run, site string }
	all := []pending{}
	for rows.Next() {
		var x pending
		if err = rows.Scan(&x.id, &x.session, &x.run, &x.site); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, x)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	out := make([]pendingInterviewTurn, 0, len(all))
	for _, x := range all {
		out = append(out, pendingInterviewTurn{outboxID: x.id, site: x.site, session: x.session, run: x.run})
	}
	return out, nil
}

func markTurnDelivered(ctx context.Context, db *sql.DB, tenant, id string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE supervisor.interview_agent_turn_outbox SET delivered_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2 AND delivered_at IS NULL`, tenant, id)
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil {
		return e
	} else if n != 1 {
		return nil
	}
	return tx.Commit()
}
