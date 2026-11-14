package ingestion

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFoldFreezesCompletenessAtFirstQuarantine(t *testing.T) {
	at := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	rows := []outboxRow{
		{1, "machine_state", false, at, at},
		{2, "production_plan", true, at, at},
		{3, "machine_state", false, at, at},
		{4, "heartbeat", false, at, at.Add(5 * time.Second)},
		{5, "watermark", false, at, at},
	}
	next, n := fold(Progress{Revision: 1, ClockQuality: "unknown"}, rows, 30*time.Second)
	if n != 5 || next.Revision != 2 || next.ProcessedSequence != 5 {
		t.Fatalf("processed: n=%d revision=%d processed=%d", n, next.Revision, next.ProcessedSequence)
	}
	if next.CompleteSequence != 1 || next.QuarantinedCount != 1 || next.FirstQuarantinedSequence == nil || *next.FirstQuarantinedSequence != 2 {
		t.Fatalf("completeness must freeze at the first quarantined row: %+v", next)
	}
	if next.WatermarkSequence != nil {
		t.Fatalf("a watermark behind a quarantine gap must not count: %+v", next)
	}
	if next.LastHeartbeatEventAt == nil || next.ClockQuality != "ok" {
		t.Fatalf("heartbeat liveness not recorded: %+v", next)
	}
}

func TestFoldStopsAtGapAndFlagsClockSkew(t *testing.T) {
	at := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	rows := []outboxRow{{1, "heartbeat", false, at, at.Add(2 * time.Minute)}, {3, "machine_state", false, at, at}}
	next, n := fold(Progress{Revision: 1, ClockQuality: "unknown"}, rows, 30*time.Second)
	if n != 1 || next.ProcessedSequence != 1 || next.CompleteSequence != 1 {
		t.Fatalf("fold must stop at the first gap: n=%d %+v", n, next)
	}
	if next.ClockQuality != "suspect" {
		t.Fatalf("drift beyond skew must be suspect: %+v", next)
	}
}

func TestEvaluateReadiness(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	policy := LivenessPolicy{FreshnessSeconds: 60, ClockSkewSeconds: 30}
	if r, reasons := evaluate(nil, 3, policy, now); r != "NOT_READY" || len(reasons) != 1 || reasons[0] != "NOT_PROCESSED" {
		t.Fatalf("no progress: %s %v", r, reasons)
	}
	fresh := now.Add(-10 * time.Second)
	seq := int64(4)
	ready := &Progress{ProcessedSequence: 4, CompleteSequence: 4, LastHeartbeatReceivedAt: &fresh, LastHeartbeatEventAt: &fresh, ClockQuality: "ok", WatermarkSequence: &seq, WatermarkEventAt: &fresh}
	if r, reasons := evaluate(ready, 4, policy, now); r != "READY" || len(reasons) != 0 {
		t.Fatalf("ready: %s %v", r, reasons)
	}
	stale := now.Add(-2 * time.Minute)
	q := int64(2)
	bad := &Progress{ProcessedSequence: 3, CompleteSequence: 1, FirstQuarantinedSequence: &q, LastHeartbeatReceivedAt: &stale, LastHeartbeatEventAt: &stale, ClockQuality: "suspect"}
	r, reasons := evaluate(bad, 5, policy, now)
	want := []string{"CONSUMER_LAGGING", "HEARTBEAT_STALE", "CLOCK_SUSPECT", "QUARANTINE_GAP", "NO_WATERMARK"}
	if r != "NOT_READY" || len(reasons) != len(want) {
		t.Fatalf("not ready: %s %v", r, reasons)
	}
	for i := range want {
		if reasons[i] != want[i] {
			t.Fatalf("reason %d = %s, want %s", i, reasons[i], want[i])
		}
	}
}

// WIS-272 simulator defect: liveness judged heartbeats against freshness_seconds (900 s) while
// readiness and dispatch cap them at limits.MaxHeartbeatAge, so a source read READY at an age
// dispatch refused as EVIDENCE_STALE.
func TestLivenessUsesTheEnforcedHeartbeatLimit(t *testing.T) {
	now := time.Date(2026, 11, 5, 19, 0, 0, 0, time.UTC)
	seq := int64(4)
	for _, c := range []struct {
		name      string
		freshness int64
		age       time.Duration
		want      string
	}{
		{"configured longer than the cap", 900, 200 * time.Second, "HEARTBEAT_STALE"},
		{"at the cap", 900, 120 * time.Second, ""},
		{"configured shorter than the cap", 60, 90 * time.Second, "HEARTBEAT_STALE"},
		{"within a shorter configuration", 60, 60 * time.Second, ""},
	} {
		at := now.Add(-c.age)
		p := &Progress{ProcessedSequence: 4, CompleteSequence: 4, LastHeartbeatReceivedAt: &at, LastHeartbeatEventAt: &at, ClockQuality: "ok", WatermarkSequence: &seq, WatermarkEventAt: &at}
		out := Liveness{Policy: LivenessPolicy{FreshnessSeconds: c.freshness}, Ingest: LivenessIngest{AcceptedSequence: 4}}
		buildLiveness(&out, p, now)
		got := strings.Join(out.Reasons, ",")
		if got != c.want || (c.want == "") != (out.Readiness == "READY") {
			t.Errorf("%s: readiness=%s reasons=%q, want reasons %q", c.name, out.Readiness, got, c.want)
		}
		if limit := min(c.freshness, 120); out.Policy.EffectiveFreshnessSeconds != limit {
			t.Errorf("%s: effective_freshness_seconds=%d, want %d", c.name, out.Policy.EffectiveFreshnessSeconds, limit)
		}
	}
}

// WIS-272 simulator defect: liveness emitted the database session's offset (…-05:00) while every
// other read API emits UTC Z (rule 04).
func TestLivenessTimesAreUTC(t *testing.T) {
	local := time.FixedZone("EST", -5*3600)
	now := time.Date(2026, 11, 5, 14, 3, 46, 0, local)
	at := now.Add(-10 * time.Second)
	seq := int64(4)
	p := &Progress{ProcessedSequence: 4, CompleteSequence: 4, LastHeartbeatReceivedAt: &at, LastHeartbeatEventAt: &at, LastEventAt: &at, ClockQuality: "ok", WatermarkSequence: &seq, WatermarkEventAt: &at}
	out := Liveness{Policy: LivenessPolicy{FreshnessSeconds: 60}, Ingest: LivenessIngest{AcceptedSequence: 4}}
	buildLiveness(&out, p, now)
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"evaluated_at":"2026-11-05T19:03:46Z"`,
		`"last_heartbeat_event_at":"2026-11-05T19:03:36Z"`,
		`"last_heartbeat_received_at":"2026-11-05T19:03:36Z"`,
		`"last_event_at":"2026-11-05T19:03:36Z"`,
		`"watermark_event_at":"2026-11-05T19:03:36Z"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("missing %s in %s", want, body)
		}
	}
}
