package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ms(values ...int) []time.Duration {
	out := make([]time.Duration, len(values))
	for i, v := range values {
		out[i] = time.Duration(v) * time.Millisecond
	}
	return out
}

func TestPercentileNearestRank(t *testing.T) {
	sorted := ms(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20)
	for _, c := range []struct {
		p    float64
		want time.Duration
	}{{50, 10 * time.Millisecond}, {95, 19 * time.Millisecond}, {99, 20 * time.Millisecond}, {100, 20 * time.Millisecond}, {1, time.Millisecond}} {
		if got := percentile(sorted, c.p); got != c.want {
			t.Errorf("p%v=%v want %v", c.p, got, c.want)
		}
	}
	if got := percentile(nil, 95); got != 0 {
		t.Errorf("empty p95=%v", got)
	}
	// Unsorted input: summarize must sort before ranking, and report the true maximum.
	l := summarize(ms(900, 1, 2, 3, 4, 5, 6, 7, 8, 9))
	if l.Count != 10 || l.P50Ms != 5 || l.P95Ms != 900 || l.MaxMs != 900 {
		t.Errorf("summary %+v", l)
	}
}

func TestEvaluateThresholds(t *testing.T) {
	var m Manifest
	m.Queries = 1000
	m.Thresholds.P95Ms, m.Thresholds.UnauthorizedMax, m.Thresholds.ErrorsMax = 500, 0, 0
	at := Latency{Count: 1000, P95Ms: 500}
	if f := evaluate(m, at, 0, 0); len(f) != 0 {
		t.Fatalf("p95 equal to the threshold must pass: %v", f)
	}
	for name, c := range map[string]struct {
		l                    Latency
		unauthorized, errors int
	}{
		"p95 breach":        {Latency{Count: 1000, P95Ms: 500.001}, 0, 0},
		"one unauthorized":  {at, 1, 0},
		"one error":         {at, 0, 1},
		"short query count": {Latency{Count: 999, P95Ms: 1}, 0, 0},
	} {
		if f := evaluate(m, c.l, c.unauthorized, c.errors); len(f) != 1 {
			t.Errorf("%s: failures %v, want exactly one", name, f)
		}
	}
}

func TestRunBlockedWithoutDatabase(t *testing.T) {
	manifest := filepath.Join("..", "..", "deploy", "qualification", "knowledge-load.json")
	var stdout, stderr bytes.Buffer
	out := t.TempDir()
	code := run([]string{"-manifest", manifest, "-out", out}, func(string) string { return "" }, &stdout, &stderr)
	if code != 2 || !strings.HasPrefix(stderr.String(), "BLOCKED: ") || !strings.Contains(stderr.String(), "SUPERVISOR_TEST_ADMIN_URL") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Fatalf("blocked run wrote a report: %v", entries)
	}
	// A missing or altered manifest blocks before any measurement.
	stderr.Reset()
	if code = run([]string{"-manifest", filepath.Join(out, "absent.json")}, func(string) string { return "set" }, &stdout, &stderr); code != 2 || !strings.HasPrefix(stderr.String(), "BLOCKED: ") {
		t.Fatalf("missing manifest code=%d stderr=%q", code, stderr.String())
	}
}

func TestCommittedManifestFreezesSpecThresholds(t *testing.T) {
	m, sum, err := loadManifest(filepath.Join("..", "..", "deploy", "qualification", "knowledge-load.json"))
	if err != nil || len(sum) != 64 {
		t.Fatal(err)
	}
	// WIS-270: 10k entities/100k claims, scoped 2-hop p95<=500ms over 1000 queries, zero unauthorized.
	if m.Fixture.Entities != 10000 || m.Fixture.Claims != 100000 || m.Queries != 1000 || m.Concurrency != 8 || m.Hops != 2 || m.Thresholds.P95Ms != 500 || m.Thresholds.UnauthorizedMax != 0 {
		t.Fatalf("manifest drifted from the WIS-270 specification: %+v", m)
	}
}

// Review finding (rule 09): a run that met every threshold on a non-reference host printed
// PASS and exited 0, which reads as proof of the reference-host criterion. It is BLOCKED.
func TestVerdictPassesOnlyOnTheReferenceHost(t *testing.T) {
	for _, c := range []struct {
		failures  int
		reference bool
		result    string
		exit      int
	}{
		{0, true, "PASS", 0},
		{0, false, "BLOCKED", 2},
		{1, true, "FAIL", 1},
		{2, false, "FAIL", 1},
	} {
		if result, exit := verdict(c.failures, c.reference); result != c.result || exit != c.exit {
			t.Errorf("verdict(%d, %v) = %s %d, want %s %d", c.failures, c.reference, result, exit, c.result, c.exit)
		}
	}
}
