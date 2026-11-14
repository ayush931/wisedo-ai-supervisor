package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

// scanEnv runs ScanWorkflow with a scripted RunScan: each call returns the next error (nil ends
// the script with a success). The test environment skips time, so waits are exercised exactly.
func scanEnv(t *testing.T, script func(call int) error) (*testsuite.TestWorkflowEnvironment, *int, *int) {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	runs, finals := 0, 0
	env.RegisterWorkflow(ScanWorkflow)
	env.RegisterActivityWithOptions(func(context.Context, ScanRef) (agent.ScanResult, error) {
		runs++
		return agent.ScanResult{}, script(runs)
	}, activity.RegisterOptions{Name: "RunScan"})
	env.RegisterActivityWithOptions(func(context.Context, ScanRef) (agent.ScanResult, error) {
		finals++
		return agent.ScanResult{}, nil
	}, activity.RegisterOptions{Name: "FinalizeScanFailure"})
	return env, &runs, &finals
}

func busy() error {
	return temporal.NewNonRetryableApplicationError("vision model busy", admissionBusy, nil)
}

// WIS-717: a full vision queue (another tenant's scans holding the worker's slots) must not
// quarantine the page. The workflow waits and tries again; before the fix the first busy answer
// ran FinalizeScanFailure, which quarantined the job and cleared its image.
func TestScanWorkflowWaitsWhileTheVisionQueueIsFull(t *testing.T) {
	env, runs, finals := scanEnv(t, func(call int) error {
		if call <= 3 {
			return busy()
		}
		return nil
	})
	env.ExecuteWorkflow(ScanWorkflow, ScanRef{Tenant: "t", Site: "s", Job: "j", Run: "r"})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow: completed=%v err=%v", env.IsWorkflowCompleted(), env.GetWorkflowError())
	}
	if *runs != 4 || *finals != 0 {
		t.Fatalf("RunScan calls=%d FinalizeScanFailure calls=%d, want 4 and 0", *runs, *finals)
	}
}

// The wait is bounded: a queue that stays full for scanBusyLimit ends in the quarantine finalizer,
// once, rather than a workflow that never finishes.
func TestScanWorkflowQuarantinesAfterTheBusyLimit(t *testing.T) {
	env, runs, finals := scanEnv(t, func(int) error { return busy() })
	start := env.Now()
	env.ExecuteWorkflow(ScanWorkflow, ScanRef{Tenant: "t", Site: "s", Job: "j", Run: "r"})
	if !env.IsWorkflowCompleted() || *finals != 1 {
		t.Fatalf("completed=%v finalizer calls=%d, want completed and 1", env.IsWorkflowCompleted(), *finals)
	}
	if waited := env.Now().Sub(start); waited > scanBusyLimit || waited < scanBusyLimit-time.Minute {
		t.Fatalf("waited %v before quarantining, want about %v", waited, scanBusyLimit)
	}
	if *runs < 20 {
		t.Fatalf("RunScan calls=%d: the workflow gave up before the limit", *runs)
	}
}

// Other failures keep the WIS-625 behaviour: no retry against the model, straight to the finalizer.
func TestScanWorkflowFinalizesOtherFailuresAtOnce(t *testing.T) {
	env, runs, finals := scanEnv(t, func(int) error {
		return temporal.NewNonRetryableApplicationError("scan unavailable", "scope", nil)
	})
	env.ExecuteWorkflow(ScanWorkflow, ScanRef{Tenant: "t", Site: "s", Job: "j", Run: "r"})
	if *runs != 1 || *finals != 1 {
		t.Fatalf("RunScan calls=%d FinalizeScanFailure calls=%d, want 1 and 1", *runs, *finals)
	}
}
