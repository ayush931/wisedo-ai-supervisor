//go:build integration && pilotoutcome

package pilot_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/outcome"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plantcontext"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/tasks"
	"github.com/google/uuid"
)

// TestWIS631PilotOutcomeMeasuredHTTPPostgres is step 10 measured for real: it runs the whole
// pilot, then lets real time pass until two complete 15-minute windows have followed each task's
// completion, reporting their production as it happens. The outcome is decided by the server on
// its own clock; nothing is backdated. Chakan recovers (100 of 100 parts a window) and must be
// observed_improvement; Pune does not (80 of 100) and must be no_improvement. Neither result
// claims the task caused anything: it is threshold recovery (rule 06).
//
// It takes 32 to 50 minutes, so it runs only with the pilotoutcome build tag:
// make supervisor-integration CASE=onboarding-pilot-outcome.
func TestWIS631PilotOutcomeMeasuredHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	env, runs := runPilot(t, ctx)

	// Every run completed its task within the same minute; the first window that starts at or after
	// the later completion is common to both, so one schedule serves both tenants.
	var completedAt time.Time
	for _, r := range runs {
		var at time.Time
		if err := env.admin.QueryRowContext(ctx, `SELECT max(created_at) FROM supervisor.task_completions WHERE tenant_id=$1 AND task_id=$2`, r.tenantID, r.outcome.taskID).Scan(&at); err != nil {
			t.Fatal(err)
		}
		if at.After(completedAt) {
			completedAt = at
		}
	}
	windows := outcome.Candidates(completedAt)
	if len(windows) < outcome.Needed {
		t.Fatalf("only %d candidate windows after %s", len(windows), completedAt)
	}
	heartbeats := func() {
		for _, r := range runs {
			func() {
				defer recoverInto(t, r)
				r.outcome.counts.heartbeat(ctx)
				r.outcome.machine.heartbeat(ctx)
			}()
		}
	}
	for i := 0; i < outcome.Needed; i++ {
		end, _ := time.Parse(time.RFC3339, windows[i].End)
		// Keep every source live while the window runs (the server trusts a heartbeat for 120 s).
		for time.Now().Before(end) {
			heartbeats()
			select {
			case <-ctx.Done():
				t.Fatal("timed out waiting for the post-completion windows")
			case <-time.After(min(60*time.Second, time.Until(end)+time.Second)):
			}
		}
		// The window has ended: each factory reports what it made in it, then marks it complete.
		for _, r := range runs {
			func() {
				defer recoverInto(t, r)
				o, c := r.outcome, r.outcome.counts
				delta := map[string]any{"order_id": o.orderID, "asset_id": o.lineID, "interval_start": windows[i].Start, "interval_end": windows[i].End, "good_units": r.f.recoveredUnits, "unit": "count"}
				c.send(ctx, map[string]any{"external_key": "post-delta-" + strconv.Itoa(i), "kind": "production_delta", "event_at": end.Format(time.RFC3339), "asset_external_key": "bracket-line", "order_external_key": "order-1", "payload": delta})
				c.send(ctx, map[string]any{"external_key": "post-watermark-" + strconv.Itoa(i), "kind": "watermark", "event_at": time.Now().UTC().Format(time.RFC3339Nano),
					"payload": map[string]any{"complete_through": windows[i].End, "through_sequence": c.sequence}})
			}()
		}
	}
	// A window may be READY only plantcontext.EvaluationDelay after it ends (late data still
	// counts until then), so the sources stay live until the last window has settled.
	last, _ := time.Parse(time.RFC3339, windows[outcome.Needed-1].End)
	settled := last.Add(plantcontext.EvaluationDelay + 5*time.Second)
	for time.Now().Before(settled) {
		heartbeats()
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for the last window to settle")
		case <-time.After(min(60*time.Second, time.Until(settled)+time.Second)):
		}
	}
	heartbeats()

	want := map[string]outcome.Status{pilotFactories[0].name: outcome.ObservedImprovement, pilotFactories[1].name: outcome.NoImprovement}
	for _, r := range runs {
		observed, err := tasks.NewService(env.db.DB).ObserveOutcomes(ctx, r.tenantID, 10)
		if err != nil || len(observed) != 1 || observed[0].Status != want[r.f.name] {
			t.Fatalf("%s observer: %+v (%v), want %s", r.f.name, observed, err, want[r.f.name])
		}
		var status, rule string
		var windowsStored int
		if err = env.admin.QueryRowContext(ctx, `SELECT status,rule_version,jsonb_array_length(windows) FROM supervisor.outcome_observations WHERE tenant_id=$1 AND task_id=$2`, r.tenantID, r.outcome.taskID).Scan(&status, &rule, &windowsStored); err != nil {
			t.Fatalf("%s stored observation: %v", r.f.name, err)
		}
		if status != string(want[r.f.name]) || rule != outcome.RuleVersion || windowsStored != outcome.Needed {
			t.Fatalf("%s stored %s (%s) over %d windows", r.f.name, status, rule, windowsStored)
		}
		var task struct {
			Outcome *struct {
				Status string `json:"status"`
			} `json:"outcome"`
		}
		// The operator's session went idle while the windows ran (the server ends it), so they sign
		// in again to read the result.
		r.operator = env.signIn(t, r, r.operator.subject)
		func() {
			defer recoverInto(t, r)
			r.call(r.operator, "GET", "/tasks/"+r.outcome.taskID, nil, -1, 200, &task)
		}()
		if task.Outcome == nil || task.Outcome.Status != string(want[r.f.name]) {
			t.Fatalf("%s task shows outcome %+v", r.f.name, task.Outcome)
		}
	}
	// The observation is the other tenant's business too: it stays the non-disclosing 404.
	for i, r := range runs {
		env.probe(t, runs[1-i], r, probe{step: "outcome", path: "/tasks/" + r.outcome.taskID, ids: []string{r.outcome.taskID}})
	}
}

// signIn gives an existing member a new session with a recent sign-in, as the fixtures seed one.
func (e *pilotEnv) signIn(t *testing.T, r *pilotRun, subject string) actor {
	t.Helper()
	a := actor{token: uuid.NewString(), csrf: uuid.NewString(), subject: subject}
	sum := sha256.Sum256([]byte(a.token))
	a.hash = hex.EncodeToString(sum[:])
	if _, err := e.admin.Exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, a.hash, subject, e.issuer, a.csrf); err != nil {
		t.Fatalf("%s sign-in: %v", r.f.name, err)
	}
	return a
}

// recoverInto turns a run helper's failure into a test failure on the test goroutine.
func recoverInto(t *testing.T, r *pilotRun) {
	t.Helper()
	if p := recover(); p != nil {
		if f, ok := p.(failure); ok {
			t.Fatalf("%s: %s", r.f.name, f.msg)
		}
		panic(p)
	}
}
