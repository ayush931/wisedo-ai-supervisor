package dispatch

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

func TestRunChatAnswerHeartbeatsDuringSlowRuntime(t *testing.T) {
	registerActiveTenantDriver.Do(func() { sql.Register("dispatch-active-tenant-test", activeTenantDriver{}) })
	db, err := sql.Open("dispatch-active-tenant-test", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	previousActivityOptions := agentActivity
	agentActivity.HeartbeatTimeout = 11 * time.Second
	t.Cleanup(func() { agentActivity = previousActivityOptions })

	tenant := uuid.NewString()
	var calls, heartbeats, finalizations atomic.Int32
	a := &Activities{
		Store: &store.Store{DB: db}, Allow: map[string]bool{tenant: true},
		chatAnswer: func(ctx context.Context, _ agent.ChatAnswerRef) (agent.ChatAnswerResult, error) {
			calls.Add(1)
			timer := time.NewTimer(12 * time.Second)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return agent.ChatAnswerResult{}, ctx.Err()
			case <-timer.C:
				return agent.ChatAnswerResult{State: agent.RunCompleted}, nil
			}
		},
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.SetTestTimeout(35 * time.Second)
	env.SetOnActivityHeartbeatListener(func(_ *activity.Info, _ converter.EncodedValues) { heartbeats.Add(1) })
	env.RegisterWorkflow(ChatAnswerWorkflow)
	env.RegisterActivityWithOptions(a.RunChatAnswer, activity.RegisterOptions{Name: "RunChatAnswer"})
	env.RegisterActivityWithOptions(func(context.Context, ChatAnswerRef) (agent.ChatAnswerResult, error) {
		finalizations.Add(1)
		return agent.ChatAnswerResult{State: agent.RunCompleted}, nil
	}, activity.RegisterOptions{Name: "FinalizeChatAnswerFailure"})
	ref := ChatAnswerRef{Tenant: tenant, Site: uuid.NewString(), Run: uuid.NewString()}
	env.ExecuteWorkflow(ChatAnswerWorkflow, ref)
	if err := env.GetWorkflowError(); err != nil || calls.Load() != 1 || heartbeats.Load() == 0 || finalizations.Load() != 0 {
		t.Fatalf("workflow error=%v calls=%d heartbeats=%d finalizations=%d", err, calls.Load(), heartbeats.Load(), finalizations.Load())
	}
}

func TestChatAnswerWorkflowFinalizesActivityFailure(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	ref := ChatAnswerRef{Tenant: "tenant", Site: "site", Run: "run"}
	var runs, finalizations int
	env.RegisterWorkflow(ChatAnswerWorkflow)
	env.RegisterActivityWithOptions(func(context.Context, ChatAnswerRef) (agent.ChatAnswerResult, error) {
		runs++
		return agent.ChatAnswerResult{}, temporal.NewNonRetryableApplicationError("agent disabled", "disabled", nil)
	}, activity.RegisterOptions{Name: "RunChatAnswer"})
	env.RegisterActivityWithOptions(func(context.Context, ChatAnswerRef) (agent.ChatAnswerResult, error) {
		finalizations++
		return agent.ChatAnswerResult{State: agent.RunCompleted}, nil
	}, activity.RegisterOptions{Name: "FinalizeChatAnswerFailure"})
	env.ExecuteWorkflow(ChatAnswerWorkflow, ref)
	if err := env.GetWorkflowError(); err != nil || runs != 1 || finalizations != 1 {
		t.Fatalf("workflow error=%v RunChatAnswer calls=%d finalizer calls=%d", err, runs, finalizations)
	}
}

func TestChatAnswerWorkflowFinalizesAfterDeadline(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	ref := ChatAnswerRef{Tenant: "tenant", Site: "site", Run: "run"}
	var finalizations int
	env.RegisterWorkflow(ChatAnswerWorkflow)
	env.RegisterActivityWithOptions(func(context.Context, ChatAnswerRef) (agent.ChatAnswerResult, error) {
		return agent.ChatAnswerResult{}, errors.New("unexpected immediate activity")
	}, activity.RegisterOptions{Name: "RunChatAnswer"})
	env.RegisterActivityWithOptions(func(context.Context, ChatAnswerRef) (agent.ChatAnswerResult, error) {
		finalizations++
		return agent.ChatAnswerResult{State: agent.RunCompleted}, nil
	}, activity.RegisterOptions{Name: "FinalizeChatAnswerFailure"})
	env.OnActivity("RunChatAnswer", mock.Anything, ref).After(agent.InterviewTurnScheduleToClose+2*time.Minute).Return(agent.ChatAnswerResult{}, nil).Once()
	env.ExecuteWorkflow(ChatAnswerWorkflow, ref)
	if err := env.GetWorkflowError(); err != nil || finalizations != 1 {
		t.Fatalf("deadline finalizer calls=%d workflow error=%v", finalizations, err)
	}
	env.AssertExpectations(t)
}
