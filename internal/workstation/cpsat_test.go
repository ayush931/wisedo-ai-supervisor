package workstation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func solverFixture(t *testing.T, output string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "solver")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' '"+output+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPlanCPsatAcceptsValidatedOptimalSchedule(t *testing.T) {
	output := `{"schema_version":1,"method":"ortools_cp_sat","status":"OPTIMAL","is_optimized":true,"makespan_seconds":5400,"assignments":[{"job_order_id":"ed6ceeb6-23a2-41e9-8f25-d88aedf78b58","operation_id":"5d4a1a15-ae68-42a7-81e0-1c607169e369","sequence":1,"workstation_id":"7ee1e85e-7683-46b2-815e-38ce4d4681d1","starts_at":"2026-10-20T10:00:00Z","ends_at":"2026-10-20T11:00:00Z","duration_seconds":3600,"duration_basis":"standard_cycle"},{"job_order_id":"ed6ceeb6-23a2-41e9-8f25-d88aedf78b58","operation_id":"605ff7bd-07a9-4017-a8c5-bb01d918bb7f","sequence":2,"workstation_id":"e0ff2d73-ae44-4fe1-918f-efc2a419da92","starts_at":"2026-10-20T11:00:00Z","ends_at":"2026-10-20T11:30:00Z","duration_seconds":1800,"duration_basis":"standard_cycle"}]}`
	plan, err := PlanCPsat(context.Background(), planningRequest(), solverFixture(t, output))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Method != "ortools_cp_sat" || !plan.IsOptimized || plan.SolverStatus != "OPTIMAL" || len(plan.Assignments) != 2 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestPlanCPsatMapsNoResultAndRejectsInvalidSchedule(t *testing.T) {
	request := planningRequest()
	for _, test := range []struct {
		name, output string
		want         error
	}{
		{"infeasible", `{"schema_version":1,"method":"ortools_cp_sat","status":"INFEASIBLE","is_optimized":false,"assignments":[]}`, ErrNoFeasibleSchedule},
		{"unknown", `{"schema_version":1,"method":"ortools_cp_sat","status":"UNKNOWN","is_optimized":false,"assignments":[]}`, ErrSolverUnknown},
		{"wrong station", `{"schema_version":1,"method":"ortools_cp_sat","status":"OPTIMAL","is_optimized":true,"makespan_seconds":5400,"assignments":[{"job_order_id":"ed6ceeb6-23a2-41e9-8f25-d88aedf78b58","operation_id":"5d4a1a15-ae68-42a7-81e0-1c607169e369","sequence":1,"workstation_id":"e0ff2d73-ae44-4fe1-918f-efc2a419da92","starts_at":"2026-10-20T10:00:00Z","ends_at":"2026-10-20T11:00:00Z","duration_seconds":3600,"duration_basis":"standard_cycle"},{"job_order_id":"ed6ceeb6-23a2-41e9-8f25-d88aedf78b58","operation_id":"605ff7bd-07a9-4017-a8c5-bb01d918bb7f","sequence":2,"workstation_id":"e0ff2d73-ae44-4fe1-918f-efc2a419da92","starts_at":"2026-10-20T11:00:00Z","ends_at":"2026-10-20T11:30:00Z","duration_seconds":1800,"duration_basis":"standard_cycle"}]}`, ErrSolverOutput},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := PlanCPsat(context.Background(), request, solverFixture(t, test.output))
			if !errors.Is(err, test.want) {
				t.Fatalf("PlanCPsat error = %v, want %v", err, test.want)
			}
		})
	}
}

// scriptFixture writes an executable shell script as the solver.
func scriptFixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "solver")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

const optimalOutput = `{"schema_version":1,"method":"ortools_cp_sat","status":"OPTIMAL","is_optimized":true,"makespan_seconds":5400,"assignments":[{"job_order_id":"ed6ceeb6-23a2-41e9-8f25-d88aedf78b58","operation_id":"5d4a1a15-ae68-42a7-81e0-1c607169e369","sequence":1,"workstation_id":"7ee1e85e-7683-46b2-815e-38ce4d4681d1","starts_at":"2026-10-20T10:00:00Z","ends_at":"2026-10-20T11:00:00Z","duration_seconds":3600,"duration_basis":"standard_cycle"},{"job_order_id":"ed6ceeb6-23a2-41e9-8f25-d88aedf78b58","operation_id":"605ff7bd-07a9-4017-a8c5-bb01d918bb7f","sequence":2,"workstation_id":"e0ff2d73-ae44-4fe1-918f-efc2a419da92","starts_at":"2026-10-20T11:00:00Z","ends_at":"2026-10-20T11:30:00Z","duration_seconds":1800,"duration_basis":"standard_cycle"}]}`

// Rule 01 CP-SAT exception: a missing or failing solver is ErrSolverUnavailable and manual
// planning is unaffected. Review finding: a removed absolute path or a non-zero exit used to
// surface as ErrSolverOutput. The configured path is cleared so the result does not depend
// on the runtime image's SUPERVISOR_WORKSTATION_CPSAT_PATH.
func TestPlanCPsatMissingOrFailingSolverIsUnavailable(t *testing.T) {
	t.Setenv("SUPERVISOR_WORKSTATION_CPSAT_PATH", "")
	for _, test := range []struct{ name, path string }{
		{"not configured", ""},
		{"removed binary", filepath.Join(t.TempDir(), "absent")},
		{"not executable", func() string {
			path := filepath.Join(t.TempDir(), "solver")
			if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}()},
		{"non-zero exit", scriptFixture(t, "exit 3")},
		{"crash after partial output", scriptFixture(t, "printf '{\\\"schema_version\\\":1'; kill -9 $$")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := PlanCPsat(context.Background(), planningRequest(), test.path); !errors.Is(err, ErrSolverUnavailable) {
				t.Fatalf("PlanCPsat error = %v, want ErrSolverUnavailable", err)
			}
		})
	}
	if _, err := PlanGreedy(planningRequest()); err != nil {
		t.Fatalf("manual planner unavailable: %v", err)
	}
}

// The solver sees a cleared environment: no parent credentials or connection strings.
func TestPlanCPsatClearsTheSolverEnvironment(t *testing.T) {
	t.Setenv("SUPERVISOR_DATABASE_URL", "postgres://secret@db/supervisor")
	t.Setenv("SUPERVISOR_OIDC_CLIENT_SECRET", "secret")
	solver := scriptFixture(t, `if [ -n "$SUPERVISOR_DATABASE_URL$SUPERVISOR_OIDC_CLIENT_SECRET$HOME" ]; then exit 7; fi; printf '%s\n' '`+optimalOutput+`'`)
	if _, err := PlanCPsat(context.Background(), planningRequest(), solver); err != nil {
		t.Fatalf("solver saw the parent environment: %v", err)
	}
}

// A solver that ignores its own time limit is killed at the call deadline and reported as
// having found no result within its budget.
func TestPlanCPsatKillsAnOverrunningSolver(t *testing.T) {
	previous := cpsatLimit
	cpsatLimit = 200 * time.Millisecond
	t.Cleanup(func() { cpsatLimit = previous })
	started := time.Now()
	_, err := PlanCPsat(context.Background(), planningRequest(), scriptFixture(t, "exec sleep 30"))
	if !errors.Is(err, ErrSolverUnknown) {
		t.Fatalf("PlanCPsat error = %v, want ErrSolverUnknown", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("overrunning solver ran for %s", elapsed)
	}
}

// Output beyond the request body limit is refused rather than buffered.
func TestPlanCPsatRejectsOversizedOutput(t *testing.T) {
	solver := scriptFixture(t, "head -c 2000000 /dev/zero | tr '\\0' ' '")
	if _, err := PlanCPsat(context.Background(), planningRequest(), solver); !errors.Is(err, ErrSolverOutput) {
		t.Fatalf("PlanCPsat error = %v, want ErrSolverOutput", err)
	}
}

func TestPlanCPsatRejectsFractionalSecondInputs(t *testing.T) {
	request := planningRequest()
	request.StartsAt = "2026-10-20T06:00:00.5-04:00"
	if _, err := PlanCPsat(context.Background(), request, "unused"); !errors.Is(err, ErrSolverTimePrecision) {
		t.Fatalf("PlanCPsat error = %v, want whole-second precision error", err)
	}
}

// WIS-699: the solver runs under resource limits set by the confinement step: CPU time, no core
// dump, no file writes and few descriptors. The fixture solver checks them as the real one runs.
func TestPlanCPsatRunsTheSolverUnderLimits(t *testing.T) {
	solver := scriptFixture(t, `[ "$(ulimit -t)" = "8" ] && [ "$(ulimit -c)" = "0" ] && [ "$(ulimit -f)" = "0" ] && [ "$(ulimit -n)" = "64" ] || exit 7
printf '%s\n' '`+optimalOutput+`'`)
	if _, err := PlanCPsat(context.Background(), planningRequest(), solver); err != nil {
		t.Fatalf("solver did not run under the limits: %v", err)
	}
}

// WIS-699: LD_LIBRARY_PATH is no longer passed on; the solver finds OR-Tools through its RPATH.
func TestPlanCPsatDoesNotForwardLibraryPath(t *testing.T) {
	t.Setenv("LD_LIBRARY_PATH", "/tmp/planted-libraries")
	solver := scriptFixture(t, `[ -z "$LD_LIBRARY_PATH" ] || exit 7
printf '%s\n' '`+optimalOutput+`'`)
	if _, err := PlanCPsat(context.Background(), planningRequest(), solver); err != nil {
		t.Fatalf("solver saw LD_LIBRARY_PATH: %v", err)
	}
}

// WIS-699: the confinement step never runs a solver unconfined or by a relative path: both exit
// with its own code before anything is executed.
func TestCPSATChildRefusesBeforeExecuting(t *testing.T) {
	executed := false
	exec := func(string, []string, []string) error { executed = true; return nil }
	if code := runCPSATChild([]string{"relative/solver"}, nil, exec); code != cpsatChildExitUnconfined || executed {
		t.Fatalf("relative path: exit %d executed %v", code, executed)
	}
	if code := runCPSATChild(nil, nil, exec); code != cpsatChildExitUnconfined || executed {
		t.Fatalf("no path: exit %d executed %v", code, executed)
	}
}

// WIS-699 verification: the real solver refuses "downtimes": null ("request outside supported
// bounds"), so a request built with a nil slice must reach it as an empty array.
func TestPlanCPsatSendsNoDowntimesAsAnArray(t *testing.T) {
	request := planningRequest()
	request.Downtimes = nil
	solver := scriptFixture(t, `grep -q '"downtimes":\[\]' || exit 7
printf '%s\n' '`+optimalOutput+`'`)
	if _, err := PlanCPsat(context.Background(), request, solver); err != nil {
		t.Fatalf("nil downtimes did not reach the solver as []: %v", err)
	}
}
