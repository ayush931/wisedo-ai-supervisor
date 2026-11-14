package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

func TestInterviewTurnWorkflowFinalizesActivityFailure(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	ref := InterviewTurnRef{Tenant: "tenant", Site: "site", Session: "session", Run: "run"}
	var turns, finalizations int
	env.RegisterWorkflow(InterviewTurnWorkflow)
	env.RegisterActivityWithOptions(func(context.Context, InterviewTurnRef) (agent.InterviewTurnResult, error) {
		turns++
		return agent.InterviewTurnResult{}, temporal.NewNonRetryableApplicationError("agent disabled", "disabled", nil)
	}, activity.RegisterOptions{Name: "RunInterviewTurn"})
	env.RegisterActivityWithOptions(func(context.Context, InterviewTurnRef) (agent.InterviewTurnResult, error) {
		finalizations++
		return agent.InterviewTurnResult{State: agent.RunCompleted, OutcomeStatus: "needs_input"}, nil
	}, activity.RegisterOptions{Name: "FinalizeInterviewTurnFailure"})
	env.ExecuteWorkflow(InterviewTurnWorkflow, ref)
	if err := env.GetWorkflowError(); err != nil || turns != 1 || finalizations != 1 {
		t.Fatalf("workflow error=%v RunInterviewTurn calls=%d finalizer calls=%d", err, turns, finalizations)
	}
}

func TestInterviewTurnWorkflowReturnsFinalizerFailure(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	ref := InterviewTurnRef{Tenant: "tenant", Site: "site", Session: "session", Run: "run"}
	env.RegisterWorkflow(InterviewTurnWorkflow)
	env.RegisterActivityWithOptions(func(context.Context, InterviewTurnRef) (agent.InterviewTurnResult, error) {
		return agent.InterviewTurnResult{}, temporal.NewNonRetryableApplicationError("agent disabled", "disabled", nil)
	}, activity.RegisterOptions{Name: "RunInterviewTurn"})
	env.RegisterActivityWithOptions(func(context.Context, InterviewTurnRef) (agent.InterviewTurnResult, error) {
		return agent.InterviewTurnResult{}, errors.New("database unavailable")
	}, activity.RegisterOptions{Name: "FinalizeInterviewTurnFailure"})
	env.ExecuteWorkflow(InterviewTurnWorkflow, ref)
	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("workflow hid a failed terminal finalization")
	}
}

func TestInterviewTurnWorkflowFinalizesAfterActivityDeadline(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	ref := InterviewTurnRef{Tenant: "tenant", Site: "site", Session: "session", Run: "run"}
	var finalized int
	env.RegisterWorkflow(InterviewTurnWorkflow)
	env.RegisterActivityWithOptions(func(context.Context, InterviewTurnRef) (agent.InterviewTurnResult, error) {
		return agent.InterviewTurnResult{}, errors.New("unexpected immediate activity")
	}, activity.RegisterOptions{Name: "RunInterviewTurn"})
	env.RegisterActivityWithOptions(func(context.Context, InterviewTurnRef) (agent.InterviewTurnResult, error) {
		finalized++
		return agent.InterviewTurnResult{State: agent.RunCompleted, OutcomeStatus: "needs_input"}, nil
	}, activity.RegisterOptions{Name: "FinalizeInterviewTurnFailure"})
	env.OnActivity("RunInterviewTurn", mock.Anything, ref).After(interviewTurnDeadline+time.Minute).Return(agent.InterviewTurnResult{}, nil).Once()
	env.ExecuteWorkflow(InterviewTurnWorkflow, ref)
	if err := env.GetWorkflowError(); err != nil || finalized != 1 {
		t.Fatalf("deadline finalizer calls=%d workflow error=%v", finalized, err)
	}
	env.AssertExpectations(t)
}

func TestInterviewTurnWorkflowAllowsActivityPastScheduleToCloseMargin(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	ref := InterviewTurnRef{Tenant: "tenant", Site: "site", Session: "session", Run: "run"}
	var finalized int
	env.RegisterWorkflow(InterviewTurnWorkflow)
	env.RegisterActivityWithOptions(func(context.Context, InterviewTurnRef) (agent.InterviewTurnResult, error) {
		return agent.InterviewTurnResult{}, errors.New("unexpected immediate activity")
	}, activity.RegisterOptions{Name: "RunInterviewTurn"})
	env.RegisterActivityWithOptions(func(context.Context, InterviewTurnRef) (agent.InterviewTurnResult, error) {
		finalized++
		return agent.InterviewTurnResult{State: agent.RunCompleted, OutcomeStatus: "needs_input"}, nil
	}, activity.RegisterOptions{Name: "FinalizeInterviewTurnFailure"})
	env.OnActivity("RunInterviewTurn", mock.Anything, ref).After(agent.InterviewTurnScheduleToClose+30*time.Second).Return(agent.InterviewTurnResult{State: agent.RunCompleted}, nil).Once()
	env.ExecuteWorkflow(InterviewTurnWorkflow, ref)
	if err := env.GetWorkflowError(); err != nil || finalized != 0 {
		t.Fatalf("activity was finalized before its deadline margin: finalizer calls=%d workflow error=%v", finalized, err)
	}
	env.AssertExpectations(t)
}
