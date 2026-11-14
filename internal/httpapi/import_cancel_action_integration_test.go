//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// cancelReply decodes a job or a problem: status is the job's state or the problem's HTTP status.
type cancelReply struct {
	ID       string `json:"job_id"`
	Status   any    `json:"status"`
	Accepted int    `json:"accepted_rows"`
	Replayed bool   `json:"replayed"`
	Code     string `json:"code"`
}

func decodeCancel(t *testing.T, w *httptest.ResponseRecorder) cancelReply {
	t.Helper()
	var v cancelReply
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("HTTP %d undecodable body %q: %v", w.Code, w.Body.String(), err)
	}
	return v
}

func (v *importRace) jobStatus(ctx context.Context, jobID string) string {
	v.t.Helper()
	var status string
	if err := v.control.QueryRowContext(ctx, `SELECT status FROM supervisor.operational_import_jobs WHERE id=$1`, jobID).Scan(&status); err != nil {
		v.t.Fatal(err)
	}
	return status
}

// WIS-719 follow-up: an owner or supervisor ends a running import (one a crashed process left
// holding its source) through POST .../operational-ingestion-jobs/{job}/cancel. Operators,
// reviewers and viewers are refused, another tenant gets the non-disclosing 404, a same-key retry
// replays, a finished job is a lifecycle conflict, and the source takes new intake afterwards.
func TestImportCancelActionPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	v := setupImportRace(t, ctx)
	jobID := v.seedRunningJob(ctx, "crashed", importRaceFile(3), 3, 0)

	refused := []struct {
		name, who, site, job, key, body string
		status                          int
		code                            string
	}{
		{"operator may import but not cancel", "operator", v.site, jobID, "c-op", `{}`, 403, "PERMISSION_DENIED"},
		{"reviewer role", "reviewer", v.site, jobID, "c-rev", `{}`, 403, "PERMISSION_DENIED"},
		{"viewer", "viewer", v.site, jobID, "c-view", `{}`, 403, "PERMISSION_DENIED"},
		{"other tenant on this site's path", "foreign", v.site, jobID, "c-f1", `{}`, 404, "NOT_FOUND"},
		{"other tenant's own site with this job", "foreign", v.foreignSite, jobID, "c-f2", `{}`, 404, "NOT_FOUND"},
		{"unknown job", "owner", v.site, uuid.NewString(), "c-unknown", `{}`, 404, "NOT_FOUND"},
		{"unknown body member", "owner", v.site, jobID, "c-body", `{"reason":"x"}`, 422, "INVALID_REQUEST"},
		{"missing idempotency key", "owner", v.site, jobID, "", `{}`, 422, "INVALID_REQUEST"},
		{"null body (the contract says exactly {})", "owner", v.site, jobID, "c-null", `null`, 422, "INVALID_REQUEST"},
		{"array body", "owner", v.site, jobID, "c-arr", `[]`, 422, "INVALID_REQUEST"},
	}
	for _, c := range refused {
		w := v.cancel(ctx, c.who, c.site, c.job, c.key, c.body)
		if got := decodeCancel(t, w); w.Code != c.status || got.Code != c.code {
			t.Fatalf("%s: HTTP %d %s, want %d %s", c.name, w.Code, w.Body.String(), c.status, c.code)
		}
		if st := v.jobStatus(ctx, jobID); st != "running" {
			t.Fatalf("%s: job became %s", c.name, st)
		}
	}
	var receipts int
	if err := v.control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.operational_import_cancellations`).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("refused cancels left %d receipts (err=%v)", receipts, err)
	}

	// The owner cancels; the receipt names the owner and the key.
	w := v.cancel(ctx, "owner", v.site, jobID, "c-own", `{}`)
	if got := decodeCancel(t, w); w.Code != 200 || got.ID != jobID || got.Status != "cancelled" || got.Replayed {
		t.Fatalf("owner cancel: HTTP %d %s, want 200 cancelled", w.Code, w.Body.String())
	}
	var key string
	if err := v.control.QueryRowContext(ctx, `SELECT c.idempotency_key FROM supervisor.operational_import_cancellations c JOIN supervisor.memberships m ON (m.tenant_id,m.site_id,m.issuer,m.subject)=(c.tenant_id,c.site_id,c.actor_issuer,c.actor_subject) WHERE c.job_id=$1 AND m.role='owner'`, jobID).Scan(&key); err != nil || key != "c-own" {
		t.Fatalf("cancel receipt key=%q err=%v, want the owner's c-own", key, err)
	}
	// The receipt is append-only, even for the migration owner.
	if _, err := v.control.ExecContext(ctx, `UPDATE supervisor.operational_import_cancellations SET idempotency_key='rewritten' WHERE job_id=$1`, jobID); err == nil {
		t.Fatal("a cancel receipt was rewritten; want the immutable trigger to refuse it")
	}
	// Same key: a replay, not a second cancel and not a conflict.
	w = v.cancel(ctx, "owner", v.site, jobID, "c-own", `{}`)
	if got := decodeCancel(t, w); w.Code != 200 || got.ID != jobID || got.Status != "cancelled" || !got.Replayed {
		t.Fatalf("owner replay: HTTP %d %s, want 200 replayed", w.Code, w.Body.String())
	}
	// A new key on the now-cancelled job is a lifecycle conflict.
	if w = v.cancel(ctx, "supervisor", v.site, jobID, "c-sup", `{}`); w.Code != 409 || decodeCancel(t, w).Code != "IMPORT_NOT_RUNNING" {
		t.Fatalf("cancel of a cancelled job: HTTP %d %s, want 409 IMPORT_NOT_RUNNING", w.Code, w.Body.String())
	}

	// The source takes new intake at once: a live batch, then a whole import that completes.
	if w = v.batch(ctx, "live-after-cancel", v.liveBatch(ctx, "live-after-cancel")); w.Code != 202 {
		t.Fatalf("live batch after cancel: HTTP %d %s, want 202", w.Code, w.Body.String())
	}
	up := v.upload(ctx, "after-cancel", importRaceFile(2))
	done := decodeCancel(t, up)
	if up.Code != 202 || done.Status != "completed" {
		t.Fatalf("import after cancel: HTTP %d %s, want 202 completed", up.Code, up.Body.String())
	}
	// Cancelling that finished job is the same lifecycle conflict.
	if w = v.cancel(ctx, "owner", v.site, done.ID, "c-finished", `{}`); w.Code != 409 || decodeCancel(t, w).Code != "IMPORT_NOT_RUNNING" {
		t.Fatalf("cancel of a completed job: HTTP %d %s, want 409 IMPORT_NOT_RUNNING", w.Code, w.Body.String())
	}
	if st := v.jobStatus(ctx, done.ID); st != "completed" {
		t.Fatalf("completed job became %s", st)
	}

	// The supervisor may cancel too. The owner's key, reused on another job, is an idempotency conflict.
	second := v.seedRunningJob(ctx, "crashed-2", importRaceFile(4), 4, 0)
	if w = v.cancel(ctx, "owner", v.site, second, "c-own", `{}`); w.Code != 409 || decodeCancel(t, w).Code != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("owner key reused on another job: HTTP %d %s, want 409 IDEMPOTENCY_CONFLICT", w.Code, w.Body.String())
	}
	if w = v.cancel(ctx, "supervisor", v.site, second, "c-sup-2", `{}`); w.Code != 200 || decodeCancel(t, w).Status != "cancelled" {
		t.Fatalf("supervisor cancel: HTTP %d %s, want 200 cancelled", w.Code, w.Body.String())
	}
}

// WIS-719 follow-up: a cancel sent while an import chunk is in flight waits for that chunk, so
// every stored event keeps its outcome, and the rows the import never reached are exported as
// unprocessed. The chunk is paused at its first write; the cancel must not end the job until the
// chunk commits.
func TestImportCancelActionWaitsForAnInFlightChunkPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	v := setupImportRace(t, ctx)
	gate, err := v.control.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback()
	if _, err = gate.ExecContext(ctx, `LOCK TABLE supervisor.operational_event_batches IN SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	uploaded := make(chan *httptest.ResponseRecorder, 1)
	go func() { uploaded <- v.upload(ctx, "inflight", importRaceFile(150)) }()
	v.waitForWaiters(ctx, waitChunkInsert, 1)
	var jobID string
	if err = v.control.QueryRowContext(ctx, `SELECT id::text FROM supervisor.operational_import_jobs WHERE tenant_id=$1 AND idempotency_key='inflight'`, v.tenant).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	cancelled := make(chan *httptest.ResponseRecorder, 1)
	go func() { cancelled <- v.cancel(ctx, "owner", v.site, jobID, "c-inflight", `{}`) }()
	// The cancel queues behind the chunk's site lock (a chunk holds it, then the job FOR SHARE).
	v.waitForWaiters(ctx, "%lock_member_site%", 1)
	for deadline := time.Now().Add(300 * time.Millisecond); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if st := v.jobStatus(ctx, jobID); st != "running" {
			_ = gate.Commit()
			t.Fatalf("job became %s while its chunk was in flight", st)
		}
	}
	if err = gate.Commit(); err != nil {
		t.Fatal(err)
	}
	cw, uw := <-cancelled, <-uploaded
	if got := decodeCancel(t, cw); cw.Code != 200 || got.Status != "cancelled" || got.Accepted != 100 {
		t.Fatalf("cancel: HTTP %d %.300s, want 200 cancelled with the in-flight chunk's 100 rows", cw.Code, cw.Body.String())
	}
	if got := decodeCancel(t, uw); uw.Code != 202 || got.Status != "cancelled" {
		t.Fatalf("upload: HTTP %d %.300s, want 202 reporting the cancelled job", uw.Code, uw.Body.String())
	}
	st, acc, out, evs := v.state(ctx, "inflight")
	if st != "cancelled" || acc != 100 || out != 100 || evs != 100 {
		t.Fatalf("job status=%s accepted_rows=%d outcomes=%d stored_events=%d, want cancelled 100/100/100", st, acc, out, evs)
	}
	csv := v.get(ctx, "/v1/sites/"+v.site+"/operational-ingestion-jobs/"+jobID+"/errors.csv")
	lines := strings.Split(strings.TrimSpace(csv.Body.String()), "\n")
	if csv.Code != 200 || len(lines) != 1+50 {
		t.Fatalf("errors.csv: HTTP %d with %d data lines, want 50 unprocessed rows", csv.Code, len(lines)-1)
	}
	for _, line := range lines[1:] {
		if !strings.Contains(line, "unprocessed") || !strings.Contains(line, "IMPORT_CANCELLED") {
			t.Fatalf("errors.csv line %q, want unprocessed IMPORT_CANCELLED", line)
		}
	}
}
