package dispatch

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/google/uuid"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// ChatAnswerRef names one WIS-517 chat answer run.
type ChatAnswerRef struct{ Tenant, Site, Run string }

// ChatAnswerWorkflow runs one captured chat answer with a hard deadline. If the activity fails
// or the deadline passes, a disconnected finalizer records the reply as failed, so a reply never
// stays pending.
func ChatAnswerWorkflow(ctx workflow.Context, ref ChatAnswerRef) error {
	turnActivity := agentActivity
	turnActivity.ScheduleToStartTimeout = interviewTurnScheduleToStart
	turnActivity.ScheduleToCloseTimeout = agent.InterviewTurnScheduleToClose
	turnCtx, cancelTurn := workflow.WithCancel(workflow.WithActivityOptions(ctx, turnActivity))
	timerCtx, cancelTimer := workflow.WithCancel(ctx)
	defer cancelTimer()
	turn := workflow.ExecuteActivity(turnCtx, "RunChatAnswer", ref)
	deadline := workflow.NewTimer(timerCtx, interviewTurnDeadline)
	selector := workflow.NewSelector(ctx)
	var result agent.ChatAnswerResult
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
	return workflow.ExecuteActivity(finalizerCtx, "FinalizeChatAnswerFailure", ref).Get(finalizerCtx, &result)
}

func (a *Activities) RunChatAnswer(ctx context.Context, ref ChatAnswerRef) (agent.ChatAnswerResult, error) {
	answer := a.chatAnswer
	if answer == nil && a.Agent != nil {
		answer = a.Agent.ChatAnswer
	}
	if answer == nil {
		return agent.ChatAnswerResult{}, temporal.NewNonRetryableApplicationError("agent runtime disabled", "disabled", nil)
	}
	if err := a.scope(ctx, ref.Tenant); err != nil {
		return agent.ChatAnswerResult{}, err
	}
	stopHeartbeat := startActivityHeartbeat(ctx)
	defer stopHeartbeat()
	out, err := answer(ctx, agent.ChatAnswerRef{TenantID: ref.Tenant, SiteID: ref.Site, RunID: ref.Run})
	switch {
	case errors.Is(err, agent.ErrInvalidRequest), errors.Is(err, agent.ErrConversationDisabled):
		return agent.ChatAnswerResult{}, temporal.NewNonRetryableApplicationError("chat answer unavailable", "scope", nil)
	case errors.Is(err, agent.ErrBudgetExhausted), errors.Is(err, agent.ErrContextOverflow):
		return agent.ChatAnswerResult{}, temporal.NewNonRetryableApplicationError("chat answer budget exhausted", "budget", nil)
	case errors.Is(err, agent.ErrRunCanceled):
		return agent.ChatAnswerResult{}, temporal.NewNonRetryableApplicationError("chat answer canceled", "canceled", nil)
	}
	return out, err
}

// FinalizeChatAnswerFailure closes a run its activity could not finish. Like every Finalize*Failure
// activity it deliberately skips the active-tenant check that scope applies (WIS-699): when a
// tenant is deactivated the run activity refuses, and this finalizer is what then closes the run
// refused and the reply failed. It only moves a running run to a terminal state, with no model
// call, source read or content, so refusing here would only leave the run running forever
// (pinned by TestChatAnsweringPostgres).
func (a *Activities) FinalizeChatAnswerFailure(ctx context.Context, ref ChatAnswerRef) (agent.ChatAnswerResult, error) {
	tenant, err := uuid.Parse(ref.Tenant)
	if err != nil {
		return agent.ChatAnswerResult{}, temporal.NewNonRetryableApplicationError("invalid scope", "scope", nil)
	}
	ref.Tenant = tenant.String()
	if len(a.Allow) > 0 && !a.Allow[ref.Tenant] {
		return agent.ChatAnswerResult{}, temporal.NewNonRetryableApplicationError("unprovisioned scope", "scope", nil)
	}
	return agent.FinalizeChatAnswerFailure(ctx, agent.NewStore(a.Store.DB), agent.ChatAnswerRef{TenantID: ref.Tenant, SiteID: ref.Site, RunID: ref.Run})
}

// RelayChatAnswers starts one workflow per undelivered chat answer turn, then records delivery.
// A turn is marked delivered only after Temporal accepts (or already has) the workflow.
func RelayChatAnswers(ctx context.Context, db *sql.DB, c client.Client, cfg config.Config, tenants []string) (int, error) {
	n := 0
	var failed []error
	for _, tenant := range tenants {
		id, err := uuid.Parse(tenant)
		if err != nil {
			failed = append(failed, err)
			continue
		}
		tenant = id.String()
		rows, err := pendingChatAnswers(ctx, db, tenant)
		if err != nil {
			failed = append(failed, err)
			continue
		}
		for _, row := range rows {
			_, err = c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "chat-answer:" + tenant + ":" + row.run, TaskQueue: cfg.TemporalQueue, WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE, WorkflowExecutionErrorWhenAlreadyStarted: true},
				ChatAnswerWorkflow, ChatAnswerRef{Tenant: tenant, Site: row.site, Run: row.run})
			var duplicate *serviceerror.WorkflowExecutionAlreadyStarted
			if err != nil && !errors.As(err, &duplicate) {
				failed = append(failed, err)
				continue
			}
			if err = markChatAnswerDelivered(ctx, db, tenant, row.site, row.run); err != nil {
				failed = append(failed, err)
				continue
			}
			n++
		}
	}
	return n, errors.Join(failed...)
}

type pendingChatAnswer struct{ site, run string }

func pendingChatAnswers(ctx context.Context, db *sql.DB, tenant string) ([]pendingChatAnswer, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT site_id::text,run_id::text FROM supervisor.chat_answer_turns WHERE tenant_id=$1 AND delivered_at IS NULL ORDER BY created_at,run_id LIMIT 100`, tenant)
	if err != nil {
		return nil, err
	}
	out := []pendingChatAnswer{}
	for rows.Next() {
		var p pendingChatAnswer
		if err = rows.Scan(&p.site, &p.run); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out = append(out, p)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func markChatAnswerDelivered(ctx context.Context, db *sql.DB, tenant, site, run string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE supervisor.chat_answer_turns SET delivered_at=clock_timestamp() WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3 AND delivered_at IS NULL`, tenant, site, run); err != nil {
		return err
	}
	return tx.Commit()
}
