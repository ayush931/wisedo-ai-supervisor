//go:build integration

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestOnboardingGatePostgres runs a small onboarding manifest end to end against a disposable
// database through the test-only path (never PASS: at best NOT_QUALIFIED, exit 2), then the
// negative controls: a frozen-threshold breach and a loosened RLS policy each exit 1 with a FAIL
// report, and a missing or wrong migration credential is BLOCKED with exit 2 and no report.
func TestOnboardingGatePostgres(t *testing.T) {
	env := map[string]string{}
	for _, k := range []string{"SUPERVISOR_TEST_ADMIN_URL", "SUPERVISOR_TEST_DATABASE_URL", "SUPERVISOR_MIGRATION_PASSWORD"} {
		if env[k] = os.Getenv(k); env[k] == "" {
			t.Fatalf("BLOCKED: %s is required (real PostgreSQL)", k)
		}
	}
	// One build of cmd/supervisor shared by every run; each run provisions through it.
	bin := filepath.Join(t.TempDir(), "supervisor")
	build := exec.Command("go", "build", "-o", bin, "./cmd/supervisor")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build cmd/supervisor: %v\n%s", err, out)
	}
	env["SUPERVISOR_LOAD_SUPERVISOR_BIN"] = bin
	env["SUPERVISOR_ENVIRONMENT"] = "local"
	small := func(mutate func(map[string]any)) string {
		return writeVariant(t, func(m map[string]any) {
			m["tenants"], m["sites_per_tenant"], m["workstations_per_site"] = 2, 2, 2
			m["intake_batches_per_site"], m["events_per_batch"], m["station_records_per_site"], m["concurrency"] = 5, 5, 6, 2
			m["thresholds"].(map[string]any)["queue_depth_max"] = 100 // 20% of this run's 100 events
			m["thresholds"].(map[string]any)["max_rss_mib"] = 4096    // the race detector multiplies memory
			if mutate != nil {
				mutate(m)
			}
		})
	}
	type report struct {
		Result   string           `json:"result"`
		Failures []string         `json:"failures"`
		Blockers []string         `json:"qualification_blockers"`
		Results  OnboardingResult `json:"results"`
		Probes   []Probe          `json:"probes"`
	}
	runGate := func(manifest string, env map[string]string, opts onboardingOptions) (int, string, *report) {
		out := t.TempDir()
		var stdout, stderr bytes.Buffer
		opts.testOnly = true
		code := runOnboarding(manifest, out, func(k string) string { return env[k] }, &stdout, &stderr, opts)
		entries, _ := os.ReadDir(out)
		if len(entries) == 0 {
			return code, stderr.String(), nil
		}
		raw, err := os.ReadFile(filepath.Join(out, entries[0].Name()))
		if err != nil {
			t.Fatal(err)
		}
		var r report
		if err = json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		t.Logf("exit %d\n%s%s", code, stdout.String(), stderr.String())
		return code, stderr.String(), &r
	}

	// Clean run: every threshold met, still NOT_QUALIFIED because the path is test-only.
	code, stderr, r := runGate(small(nil), env, onboardingOptions{})
	if code != 2 || r == nil || r.Result != "NOT_QUALIFIED" || len(r.Failures) != 0 || !containsText(r.Blockers, "test-only") {
		t.Fatalf("clean small run: exit %d stderr %q report %+v", code, stderr, r)
	}
	res := r.Results
	if res.Intake.Count != 20 || res.StationWrites.Count != 24 || res.Provision.Count != 2 || res.EventsIn202 != 100 || res.EventsAccepted != 100 || res.EventsFolded != 100 || res.EventsQuarantine != 0 ||
		res.QueueResidual != 0 || res.CrossTenantRows != 0 || res.IsolationChecks < 2 || len(r.Probes) != 3 || res.MaxRSSMiB <= 0 {
		t.Fatalf("clean small run measured %+v probes %+v", res, r.Probes)
	}

	// Breach: the same workload against an impossible frozen p95 fails, it is never passed.
	breach := small(func(m map[string]any) { m["thresholds"].(map[string]any)["intake_p95_ms"] = 0.001 })
	code, stderr, r = runGate(breach, env, onboardingOptions{})
	if code != 1 || r == nil || r.Result != "FAIL" || len(r.Failures) != 1 || !strings.Contains(r.Failures[0], "intake p95") {
		t.Fatalf("breach run: exit %d stderr %q report %+v", code, stderr, r)
	}

	// Cross-tenant negative control: a permissive policy lets the runtime role read every
	// tenant's station records. The isolation check must see the rows and FAIL; the policy is
	// dropped again after the probes.
	restored := false
	leak := func(ctx context.Context, admin *sql.DB) (func() error, error) {
		if _, err := admin.ExecContext(ctx, `CREATE POLICY load_negative_control ON supervisor.station_records FOR SELECT TO supervisor_runtime USING (true)`); err != nil {
			return nil, err
		}
		return func() error {
			restored = true
			_, err := admin.ExecContext(context.Background(), `DROP POLICY load_negative_control ON supervisor.station_records`)
			return err
		}, nil
	}
	code, stderr, r = runGate(small(nil), env, onboardingOptions{afterSetup: leak})
	if code != 1 || r == nil || r.Result != "FAIL" || r.Results.CrossTenantRows == 0 || !containsText(r.Failures, "cross-tenant rows visible") || !restored {
		t.Fatalf("RLS leak run: exit %d stderr %q restored %v report %+v", code, stderr, restored, r)
	}

	// BLOCKED: no migration credential, and a wrong one (the migrate step cannot run).
	for name, change := range map[string][2]string{"missing migration password": {"SUPERVISOR_MIGRATION_PASSWORD", ""}, "wrong migration password": {"SUPERVISOR_MIGRATION_PASSWORD", "wrong"}} {
		blockedEnv := map[string]string{}
		for k, v := range env {
			blockedEnv[k] = v
		}
		blockedEnv[change[0]] = change[1]
		code, stderr, r = runGate(small(nil), blockedEnv, onboardingOptions{})
		if code != 2 || r != nil || !strings.HasPrefix(stderr, "BLOCKED: ") {
			t.Fatalf("%s: exit %d stderr %q report %+v", name, code, stderr, r)
		}
	}
}
