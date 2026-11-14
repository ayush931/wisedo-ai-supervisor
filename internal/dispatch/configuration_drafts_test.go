package dispatch

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync"
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

type activeTenantDriver struct{}
type activeTenantConn struct{}
type activeTenantRows struct{ done bool }

func (activeTenantDriver) Open(string) (driver.Conn, error) { return activeTenantConn{}, nil }
func (activeTenantConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepared query")
}
func (activeTenantConn) Close() error              { return nil }
func (activeTenantConn) Begin() (driver.Tx, error) { return nil, errors.New("unexpected transaction") }
func (activeTenantConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &activeTenantRows{}, nil
}
func (*activeTenantRows) Columns() []string { return []string{"exists"} }
func (*activeTenantRows) Close() error      { return nil }
func (r *activeTenantRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = true
	return nil
}

var registerActiveTenantDriver sync.Once

func TestRunConfigurationDraftHeartbeatsDuringSlowRuntime(t *testing.T) {
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
		Store: &store.Store{DB: db},
		Allow: map[string]bool{tenant: true},
		draftConfiguration: func(ctx context.Context, _ agent.ConfigurationDraftRef) (agent.ConfigurationDraftResult, error) {
			calls.Add(1)
			timer := time.NewTimer(12 * time.Second)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return agent.ConfigurationDraftResult{}, ctx.Err()
			case <-timer.C:
				return agent.ConfigurationDraftResult{State: agent.RunCompleted}, nil
			}
		},
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.SetTestTimeout(35 * time.Second)
	env.SetOnActivityHeartbeatListener(func(_ *activity.Info, _ converter.EncodedValues) { heartbeats.Add(1) })
	env.RegisterWorkflow(ConfigurationDraftWorkflow)
	env.RegisterActivityWithOptions(a.RunConfigurationDraft, activity.RegisterOptions{Name: "RunConfigurationDraft"})
	env.RegisterActivityWithOptions(func(context.Context, ConfigurationDraftRef) (agent.ConfigurationDraftResult, error) {
		finalizations.Add(1)
		return agent.ConfigurationDraftResult{State: agent.RunCompleted}, nil
	}, activity.RegisterOptions{Name: "FinalizeConfigurationDraftFailure"})
	ref := ConfigurationDraftRef{Tenant: tenant, Site: uuid.NewString(), Session: uuid.NewString(), Run: uuid.NewString()}
	env.ExecuteWorkflow(ConfigurationDraftWorkflow, ref)
	if err := env.GetWorkflowError(); err != nil || calls.Load() != 1 || heartbeats.Load() == 0 || finalizations.Load() != 0 {
		t.Fatalf("workflow error=%v calls=%d heartbeats=%d finalizations=%d", err, calls.Load(), heartbeats.Load(), finalizations.Load())
	}
}

func TestConfigurationDraftWorkflowFinalizesActivityFailure(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	ref := ConfigurationDraftRef{Tenant: "tenant", Site: "site", Session: "session", Run: "run"}
	var runs, finalizations int
	env.RegisterWorkflow(ConfigurationDraftWorkflow)
	env.RegisterActivityWithOptions(func(context.Context, ConfigurationDraftRef) (agent.ConfigurationDraftResult, error) {
		runs++
		return agent.ConfigurationDraftResult{}, temporal.NewNonRetryableApplicationError("agent disabled", "disabled", nil)
	}, activity.RegisterOptions{Name: "RunConfigurationDraft"})
	env.RegisterActivityWithOptions(func(context.Context, ConfigurationDraftRef) (agent.ConfigurationDraftResult, error) {
		finalizations++
		return agent.ConfigurationDraftResult{State: agent.RunCompleted, OutcomeStatus: "needs_input"}, nil
	}, activity.RegisterOptions{Name: "FinalizeConfigurationDraftFailure"})
	env.ExecuteWorkflow(ConfigurationDraftWorkflow, ref)
	if err := env.GetWorkflowError(); err != nil || runs != 1 || finalizations != 1 {
		t.Fatalf("workflow error=%v run calls=%d finalizer calls=%d", err, runs, finalizations)
	}
}

func TestConfigurationDraftWorkflowReturnsFinalizerFailure(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	ref := ConfigurationDraftRef{Tenant: "tenant", Site: "site", Session: "session", Run: "run"}
	env.RegisterWorkflow(ConfigurationDraftWorkflow)
	env.RegisterActivityWithOptions(func(context.Context, ConfigurationDraftRef) (agent.ConfigurationDraftResult, error) {
		return agent.ConfigurationDraftResult{}, temporal.NewNonRetryableApplicationError("agent disabled", "disabled", nil)
	}, activity.RegisterOptions{Name: "RunConfigurationDraft"})
	env.RegisterActivityWithOptions(func(context.Context, ConfigurationDraftRef) (agent.ConfigurationDraftResult, error) {
		return agent.ConfigurationDraftResult{}, errors.New("database unavailable")
	}, activity.RegisterOptions{Name: "FinalizeConfigurationDraftFailure"})
	env.ExecuteWorkflow(ConfigurationDraftWorkflow, ref)
	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("workflow hid a failed terminal finalization")
	}
}

func TestConfigurationDraftWorkflowFinalizesAfterDeadline(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	ref := ConfigurationDraftRef{Tenant: "tenant", Site: "site", Session: "session", Run: "run"}
	var finalizations int
	env.RegisterWorkflow(ConfigurationDraftWorkflow)
	env.RegisterActivityWithOptions(func(context.Context, ConfigurationDraftRef) (agent.ConfigurationDraftResult, error) {
		return agent.ConfigurationDraftResult{}, errors.New("unexpected immediate activity")
	}, activity.RegisterOptions{Name: "RunConfigurationDraft"})
	env.RegisterActivityWithOptions(func(context.Context, ConfigurationDraftRef) (agent.ConfigurationDraftResult, error) {
		finalizations++
		return agent.ConfigurationDraftResult{State: agent.RunCompleted, OutcomeStatus: "needs_input"}, nil
	}, activity.RegisterOptions{Name: "FinalizeConfigurationDraftFailure"})
	env.OnActivity("RunConfigurationDraft", mock.Anything, ref).After(configurationDraftDeadline+time.Second).Return(agent.ConfigurationDraftResult{}, nil).Once()
	env.ExecuteWorkflow(ConfigurationDraftWorkflow, ref)
	if err := env.GetWorkflowError(); err != nil || finalizations != 1 {
		t.Fatalf("deadline finalizer calls=%d workflow error=%v", finalizations, err)
	}
	env.AssertExpectations(t)
}
