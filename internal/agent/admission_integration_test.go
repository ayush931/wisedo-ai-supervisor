//go:build integration

package agent_test

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/dispatch"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
)

// plantRunningRun records a running case run for another case of the site, started ageSeconds
// ago. The case row is not this test's concern, so foreign keys are skipped for this row only.
func (w *world) plantRunningRun(t *testing.T, site string, ageSeconds int) string {
	t.Helper()
	tx, err := w.control.BeginTx(w.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	id := uuid.NewString()
	for _, q := range []string{`SET LOCAL session_replication_role = replica`,
		`INSERT INTO supervisor.agent_runs(tenant_id,site_id,id,request_kind,case_id,case_revision,trace_id,provider_name,provider_actual,model,model_revision,prompt_version,tool_version,policy_revision,
 invocations_left,tool_calls_left,requests_left,tokens_left,cost_left_microusd,started_at)
 VALUES('` + w.tenant + `','` + site + `','` + id + `','case','` + uuid.NewString() + `',1,'` + agent.NewTraceID() + `','fixture',false,'fixture-model','sha256:fixture','p','t',1,3,12,6,24000,0,clock_timestamp()-make_interval(secs=>` + strconv.Itoa(ageSeconds) + `))`} {
		if _, err = tx.ExecContext(w.ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return id
}

// endRun cancels a run opened only to drive tool calls directly, so it stops counting as a live
// investigation of the site (WIS-273 admission).
func (w *world) endRun(t *testing.T, sc agent.ToolServerContext) {
	t.Helper()
	rs := agent.RunScope{TenantID: sc.TenantID, SiteID: sc.SiteID}
	if err := w.store.InTx(w.ctx, rs, func(tx *sql.Tx) error {
		return agent.FinishRunTx(w.ctx, tx, rs, sc.RunID, agent.Finish{State: agent.RunCanceled})
	}); err != nil {
		t.Fatal(err)
	}
}

func (w *world) caseRuns(t *testing.T) int {
	t.Helper()
	var n int
	if err := w.control.QueryRowContext(w.ctx, `SELECT count(*) FROM supervisor.agent_runs WHERE tenant_id=$1 AND case_id=$2`, w.tenant, w.caseID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestWIS273InvestigationAdmissionPerSite: a site with MaxActivePerSite running investigations
// refuses a new one with ErrSiteBusy and creates nothing, and the activity reports it as the
// non-retryable admission_busy type the case workflow defers on. Another site's investigations
// never count, and a run past its wall time no longer holds a place.
func TestWIS273InvestigationAdmissionPerSite(t *testing.T) {
	w := newWorld(t)
	busySite := uuid.NewString()
	w.exec(t, `INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'busy neighbour')`, w.tenant, busySite)
	w.plantRunningRun(t, busySite, 0)
	w.plantRunningRun(t, busySite, 0)
	rev := w.fresh(t)
	if res := w.investigate(t, w.runtime(script(finalStep(t, w.result("success"), w.clarify(rev))), nil), rev); res.RunID == "" || w.caseRuns(t) != 1 {
		t.Fatalf("another site's runs must not count: %+v runs=%d", res, w.caseRuns(t))
	}

	first := w.plantRunningRun(t, w.site, 0)
	w.plantRunningRun(t, w.site, 0)
	rev = w.fresh(t)
	rt := w.runtime(script(finalStep(t, w.result("success"), w.clarify(rev))), nil)
	_, err := rt.Investigate(w.ctx, agent.InvestigateRef{TenantID: w.tenant, SiteID: w.site, CaseID: w.caseID}, rev)
	if !errors.Is(err, agent.ErrSiteBusy) || w.caseRuns(t) != 1 {
		t.Fatalf("full site: err=%v runs=%d, want ErrSiteBusy and no new run", err, w.caseRuns(t))
	}
	_, err = activityFor(w, rt, w.tenant).InvestigateCase(w.ctx, dispatch.InvestigateRef{Tenant: w.tenant, Site: w.site, Case: w.caseID, Revision: rev})
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || appErr.Type() != "admission_busy" || !appErr.NonRetryable() || w.caseRuns(t) != 1 {
		t.Fatalf("activity: err=%v, want non-retryable admission_busy and no new run", err)
	}
	w.exec(t, `UPDATE supervisor.agent_runs SET started_at=started_at-interval '10 minutes' WHERE tenant_id=$1 AND id=$2`, w.tenant, first)
	res := w.investigate(t, rt, rev)
	if got := w.row(t, res.RunID); got.status != "needs_input" || w.caseRuns(t) != 2 {
		t.Fatalf("a run past its wall time must not hold a place: %+v runs=%d", got, w.caseRuns(t))
	}
}

// TestWIS273OwnRunningRunResumesWhenSiteIsFull: admission applies to new runs only, so a crashed
// investigation of this case at this revision is resumed rather than deferred behind others.
func TestWIS273OwnRunningRunResumesWhenSiteIsFull(t *testing.T) {
	w := newWorld(t)
	_, _ = w.openRun(t, agent.DefaultLimits()) // this case's running run at the current revision
	w.plantRunningRun(t, w.site, 0)
	w.plantRunningRun(t, w.site, 0)
	rt := w.runtime(script(finalStep(t, w.result("success"), w.clarify(w.revision))), nil)
	res := w.investigate(t, rt, w.revision)
	if res.RunID == "" || w.caseRuns(t) != 1 {
		t.Fatalf("resume: %+v runs=%d", res, w.caseRuns(t))
	}
}

// TestWIS273InvestigationAdmissionPerWorker: a worker already running MaxActivePerProcess
// investigations refuses another with ErrWorkerBusy before reading or creating anything, and
// admits it once a place is released.
func TestWIS273InvestigationAdmissionPerWorker(t *testing.T) {
	w := newWorld(t)
	rev := w.fresh(t)
	rt := w.runtime(script(finalStep(t, w.result("success"), w.clarify(rev))), nil)
	rt.Limits.MaxActivePerProcess = 1
	release, err := agent.HoldWorkerPlace(rt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = rt.Investigate(w.ctx, agent.InvestigateRef{TenantID: w.tenant, SiteID: w.site, CaseID: w.caseID}, rev); !errors.Is(err, agent.ErrWorkerBusy) || w.caseRuns(t) != 0 {
		t.Fatalf("full worker: err=%v runs=%d", err, w.caseRuns(t))
	}
	release()
	if res := w.investigate(t, rt, rev); res.RunID == "" || w.caseRuns(t) != 1 {
		t.Fatalf("after release: %+v", res)
	}
}

// TestWIS273StaleResumeDoesNotOutliveItsWallTime (review M1): admitSite stops counting a run
// WallTime after it started, so a resumed run must not still execute after that. This case's own
// running run, started past the wall time, resumes while two fresh runs fill the site: it ends on
// the deadline path without a model call instead of running as an uncounted third investigation.
func TestWIS273StaleResumeDoesNotOutliveItsWallTime(t *testing.T) {
	w := newWorld(t)
	sc, _ := w.openRun(t, agent.DefaultLimits())
	w.exec(t, `UPDATE supervisor.agent_runs SET started_at=started_at-interval '5 minutes' WHERE tenant_id=$1 AND id=$2`, w.tenant, sc.RunID)
	w.plantRunningRun(t, w.site, 0)
	w.plantRunningRun(t, w.site, 0)
	res := w.investigate(t, w.runtime(script(finalStep(t, w.result("success"), w.clarify(w.revision))), nil), w.revision)
	got := w.row(t, res.RunID)
	if res.RunID != sc.RunID || got.modelCalls != 0 || got.status != recovery.StatusNeedsInput || got.reasons != agent.CodeDeadlineExceeded {
		t.Fatalf("stale resume: run=%s (want %s) %+v, want no model call and %s", res.RunID, sc.RunID, got, agent.CodeDeadlineExceeded)
	}
}

// blocking holds every model call until released, so a run stays running while a test looks at
// the site.
type blocking struct{ release chan struct{} }

func (b blocking) Name() string { return agent.ProviderFixture }
func (b blocking) Actual() bool { return false }
func (b blocking) Complete(ctx context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return agent.ModelResponse{}, ctx.Err()
}

// TestWIS273SiteAdmissionIsSerialized (review M2): the per-site advisory lock means no two
// admissions are ever between the site count and the run insert at once. Two investigations of the
// site start together; the hook in that window waits for the other to arrive. With the lock the
// second cannot arrive while the first holds it, so the window never holds both. Without the lock
// both pass the count and overlap there, and the site could take more runs than its bound.
func TestWIS273SiteAdmissionIsSerialized(t *testing.T) {
	w := newWorld(t)
	w.plantRunningRun(t, w.site, 0) // one place of two left
	rev := w.fresh(t)
	model := blocking{release: make(chan struct{})}
	defer close(model.release)
	rt := w.runtime(model, nil)
	var mu sync.Mutex
	inside, arrivals, overlap := 0, 0, false
	agent.SetAfterSiteAdmission(rt, func() {
		mu.Lock()
		inside++
		arrivals++
		mu.Unlock()
		defer func() { mu.Lock(); inside--; mu.Unlock() }()
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			mu.Lock()
			both := inside == 2
			overlap = overlap || both
			mu.Unlock()
			if both {
				return
			}
		}
	})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			ctx, cancel := context.WithTimeout(w.ctx, 20*time.Second)
			defer cancel()
			_, err := rt.Investigate(ctx, agent.InvestigateRef{TenantID: w.tenant, SiteID: w.site, CaseID: w.caseID}, rev)
			errs <- err
		}()
	}
	// The admitted call blocks in the model; the other returns refused.
	var err error
	select {
	case err = <-errs:
	case <-time.After(15 * time.Second):
		t.Fatal("neither investigation returned")
	}
	mu.Lock()
	defer mu.Unlock()
	if overlap || arrivals != 1 || !errors.Is(err, agent.ErrSiteBusy) || w.caseRuns(t) != 1 {
		t.Fatalf("overlap=%v arrivals=%d err=%v runs=%d; want one admission at a time, the second ErrSiteBusy and one run", overlap, arrivals, err, w.caseRuns(t))
	}
}
