package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

var committedOnboarding = filepath.Join("..", "..", "deploy", "qualification", "onboarding-load.json")

// writeVariant writes the committed onboarding manifest after mutate edits its JSON object.
func writeVariant(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	raw, err := os.ReadFile(committedOnboarding)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err = json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	mutate(m)
	out, _ := json.Marshal(m)
	path := filepath.Join(t.TempDir(), "onboarding.json")
	if err = os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCommittedOnboardingManifestIsFrozen(t *testing.T) {
	m, sum, err := loadOnboardingManifest(committedOnboarding)
	if err != nil || len(sum) != 64 {
		t.Fatal(err)
	}
	th := m.Thresholds
	// Frozen before the first measured run; a change here needs a reviewed manifest change.
	if m.Tenants != 4 || m.SitesPerTenant != 3 || m.IntakeBatchesPerSite != 100 || m.EventsPerBatch != 20 || m.StationRecordsPerSite != 100 || m.Concurrency != 8 ||
		*th.IntakeP95Ms != 1000 || *th.IntakeP99Ms != 2500 || *th.StationP95Ms != 500 || *th.StationP99Ms != 1500 || *th.ProvisionMaxMs != 10000 ||
		*th.ErrorRateMax != 0 || *th.MaxRSSMiB != 512 || *th.QueueDepthMax != 4800 || *th.CrossTenantRowsMax != 0 {
		t.Fatalf("onboarding manifest drifted from its frozen values: %+v %+v", m, *th)
	}
}

func TestOnboardingManifestRefusesIncompleteOrUnknown(t *testing.T) {
	thresholds := func(m map[string]any) map[string]any { return m["thresholds"].(map[string]any) }
	for name, c := range map[string]struct {
		mutate func(map[string]any)
		want   string
	}{
		"no thresholds":           {func(m map[string]any) { delete(m, "thresholds") }, "no thresholds"},
		"null thresholds":         {func(m map[string]any) { m["thresholds"] = nil }, "no thresholds"},
		"one threshold missing":   {func(m map[string]any) { delete(thresholds(m), "station_write_p99_ms") }, "station_write_p99_ms"},
		"error tolerance missing": {func(m map[string]any) { delete(thresholds(m), "error_rate_max") }, "error_rate_max"},
		"unknown top-level field": {func(m map[string]any) { m["warmup"] = 1 }, "unknown field"},
		"unknown threshold":       {func(m map[string]any) { thresholds(m)["p50_ms"] = 1 }, "unknown field"},
		"p99 below p95":           {func(m map[string]any) { thresholds(m)["intake_p99_ms"] = 10 }, "out of range"},
		"zero p95":                {func(m map[string]any) { thresholds(m)["station_write_p95_ms"] = 0 }, "out of range"},
		"one tenant":              {func(m map[string]any) { m["tenants"] = 1 }, "two tenants"},
		"concurrency above sites": {func(m map[string]any) { m["concurrency"] = 13 }, "out of range"},
	} {
		_, _, err := loadOnboardingManifest(writeVariant(t, c.mutate))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err=%v, want %q", name, err, c.want)
		}
	}
}

func passingResult(m OnboardingManifest) OnboardingResult {
	events := int64(m.sites() * m.IntakeBatchesPerSite * m.EventsPerBatch)
	return OnboardingResult{Tenants: m.Tenants, Sites: m.sites(), Provision: Latency{Count: m.Tenants, MaxMs: *m.Thresholds.ProvisionMaxMs},
		Intake:         Latency{Count: m.sites() * m.IntakeBatchesPerSite, P95Ms: *m.Thresholds.IntakeP95Ms, P99Ms: *m.Thresholds.IntakeP99Ms},
		StationWrites:  Latency{Count: m.sites() * m.StationRecordsPerSite, P95Ms: *m.Thresholds.StationP95Ms, P99Ms: *m.Thresholds.StationP99Ms},
		EventsExpected: events, EventsIn202: events, EventsAccepted: events, EventsFolded: events,
		MaxRSSMiB: *m.Thresholds.MaxRSSMiB, QueueDepthPeak: *m.Thresholds.QueueDepthMax, IsolationChecks: 5}
}

// A breach of any frozen threshold is listed and turns into FAIL with exit 1, never PASS.
func TestOnboardingBreachFailsWithNonzeroExit(t *testing.T) {
	m, _, err := loadOnboardingManifest(committedOnboarding)
	if err != nil {
		t.Fatal(err)
	}
	if f := evaluateOnboarding(m, passingResult(m)); len(f) != 0 {
		t.Fatalf("values equal to the thresholds must pass: %v", f)
	}
	for name, mutate := range map[string]func(*OnboardingResult){
		"intake p95":          func(r *OnboardingResult) { r.Intake.P95Ms += 0.001 },
		"intake p99":          func(r *OnboardingResult) { r.Intake.P99Ms += 0.001 },
		"station p95":         func(r *OnboardingResult) { r.StationWrites.P95Ms += 0.001 },
		"station p99":         func(r *OnboardingResult) { r.StationWrites.P99Ms += 0.001 },
		"provision":           func(r *OnboardingResult) { r.Provision.MaxMs += 1 },
		"one error":           func(r *OnboardingResult) { r.ErrorRate = 1.0 / 2400 },
		"rss":                 func(r *OnboardingResult) { r.MaxRSSMiB += 0.1 },
		"queue peak":          func(r *OnboardingResult) { r.QueueDepthPeak++ },
		"queue not drained":   func(r *OnboardingResult) { r.QueueResidual = 1 },
		"cross-tenant row":    func(r *OnboardingResult) { r.CrossTenantRows = 1 },
		"no isolation check":  func(r *OnboardingResult) { r.IsolationChecks = 0 },
		"short intake count":  func(r *OnboardingResult) { r.Intake.Count-- },
		"short station count": func(r *OnboardingResult) { r.StationWrites.Count-- },
		"missing tenant":      func(r *OnboardingResult) { r.Provision.Count-- },
		"short 202 events":    func(r *OnboardingResult) { r.EventsIn202-- },
		"short accepted":      func(r *OnboardingResult) { r.EventsAccepted-- },
		"short folded":        func(r *OnboardingResult) { r.EventsFolded-- },
		"one quarantined":     func(r *OnboardingResult) { r.EventsQuarantine = 1 },
	} {
		r := passingResult(m)
		mutate(&r)
		f := evaluateOnboarding(m, r)
		if len(f) != 1 {
			t.Errorf("%s: failures %v, want exactly one", name, f)
		}
		for _, blockers := range [][]string{nil, {"not the reference host"}} {
			if result, exit := onboardingVerdict(len(f), blockers); result != "FAIL" || exit != 1 {
				t.Errorf("%s (blockers %v): %s exit %d, want FAIL exit 1", name, blockers, result, exit)
			}
		}
	}
}

// Rule 09: a missing, unreachable or non-local database is BLOCKED with exit 2 and writes no
// report. The required SUPERVISOR_MIGRATION_PASSWORD proves the onboarding gate was selected.
func TestOnboardingBlockedWithoutDatabase(t *testing.T) {
	local := "postgresql://postgres:x@127.0.0.1:1/supervisor?connect_timeout=2"
	runtimeURL := "postgresql://supervisor_runtime:x@127.0.0.1:1/supervisor"
	full := func(extra map[string]string) map[string]string {
		env := map[string]string{"SUPERVISOR_ENVIRONMENT": "local", "SUPERVISOR_TEST_ADMIN_URL": local, "SUPERVISOR_TEST_DATABASE_URL": runtimeURL, "SUPERVISOR_MIGRATION_PASSWORD": "x"}
		for k, v := range extra {
			env[k] = v
		}
		return env
	}
	for name, c := range map[string]struct {
		env  map[string]string
		want string
	}{
		"no environment":         {full(map[string]string{"SUPERVISOR_ENVIRONMENT": ""}), "SUPERVISOR_ENVIRONMENT=local"},
		"production environment": {full(map[string]string{"SUPERVISOR_ENVIRONMENT": "production"}), "SUPERVISOR_ENVIRONMENT=local"},
		"no database":            {map[string]string{"SUPERVISOR_ENVIRONMENT": "local"}, "SUPERVISOR_MIGRATION_PASSWORD"},
		"no migration password":  {full(map[string]string{"SUPERVISOR_MIGRATION_PASSWORD": ""}), "SUPERVISOR_MIGRATION_PASSWORD"},
		"remote admin URL":       {full(map[string]string{"SUPERVISOR_TEST_ADMIN_URL": "postgresql://postgres:x@db.example.com/supervisor"}), "SUPERVISOR_TEST_ADMIN_URL refused"},
		"remote fallback host":   {full(map[string]string{"SUPERVISOR_TEST_DATABASE_URL": "postgresql://supervisor_runtime:x@127.0.0.1:1,db.example.com:5432/supervisor"}), "SUPERVISOR_TEST_DATABASE_URL refused"},
		"unreachable database":   {full(nil), "unreachable"},
	} {
		var stdout, stderr bytes.Buffer
		out := t.TempDir()
		code := run([]string{"-manifest", committedOnboarding, "-out", out}, func(k string) string { return c.env[k] }, &stdout, &stderr)
		if code != 2 || !strings.HasPrefix(stderr.String(), "BLOCKED: ") || !strings.Contains(stderr.String(), c.want) {
			t.Errorf("%s: code=%d stderr=%q, want BLOCKED naming %q", name, code, stderr.String(), c.want)
		}
		if entries, _ := os.ReadDir(out); len(entries) != 0 {
			t.Errorf("%s: blocked run wrote a report: %v", name, entries)
		}
	}
	// A manifest without thresholds is refused before any database is touched.
	var stdout, stderr bytes.Buffer
	path := writeVariant(t, func(m map[string]any) { delete(m, "thresholds") })
	if code := run([]string{"-manifest", path, "-out", t.TempDir()}, func(string) string { return "set" }, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "no thresholds") {
		t.Fatalf("thresholdless manifest code=%d stderr=%q", code, stderr.String())
	}
}

// Review finding: PASS was self-attested by GOMAXPROCS and an operator-supplied string. The
// reference host is now measured, and any mismatch leaves a threshold-clean run NOT_QUALIFIED.
func TestOnboardingReferenceHostIsMeasured(t *testing.T) {
	m, _, err := loadOnboardingManifest(committedOnboarding)
	if err != nil {
		t.Fatal(err)
	}
	ref := hostFacts{GOMAXPROCS: 4, NumCPU: 4, MemTotalKiB: 8_000_000, PostgresCPUs: "4", PostgresMemory: "8589934592"}
	if mm := referenceMismatches(m, ref); len(mm) != 0 {
		t.Fatalf("reference host reported mismatches: %v", mm)
	}
	for name, mutate := range map[string]func(*hostFacts){
		"GOMAXPROCS":           func(h *hostFacts) { h.GOMAXPROCS = 8 },
		"client CPUs":          func(h *hostFacts) { h.NumCPU = 24 },
		"client memory":        func(h *hostFacts) { h.MemTotalKiB = 65_285_336 },
		"unlimited PG CPUs":    func(h *hostFacts) { h.PostgresCPUs = "max" },
		"unlimited PG memory":  func(h *hostFacts) { h.PostgresMemory = "max" },
		"unreadable PG limits": func(h *hostFacts) { h.PostgresCPUs = "unreadable" },
	} {
		h := ref
		mutate(&h)
		if mm := referenceMismatches(m, h); len(mm) != 1 {
			t.Errorf("%s: mismatches %v, want exactly one", name, mm)
		}
	}
	for raw, want := range map[string]string{"400000 100000\n": "4", "max 100000": "max", "250000 100000": "2.500", "junk": "junk"} {
		if got := cgroupCPUs(raw); got != want {
			t.Errorf("cgroupCPUs(%q)=%q want %q", raw, got, want)
		}
	}
	for _, c := range []struct {
		failures int
		blockers []string
		result   string
		exit     int
	}{{0, nil, "PASS", 0}, {0, []string{"not the reference host"}, "NOT_QUALIFIED", 2}, {1, nil, "FAIL", 1}} {
		if result, exit := onboardingVerdict(c.failures, c.blockers); result != c.result || exit != c.exit {
			t.Errorf("onboardingVerdict(%d, %v) = %s %d, want %s %d", c.failures, c.blockers, result, exit, c.result, c.exit)
		}
	}
}

// Review finding: a modified manifest, a dirty tree or a foreign binary could PASS.
func TestOnboardingProvenanceBlocksModifiedInputs(t *testing.T) {
	raw, err := os.ReadFile(committedOnboarding)
	if err != nil {
		t.Fatal(err)
	}
	edited := append(bytes.Clone(raw), ' ')
	if b := provenanceBlockers(committedOnboarding, edited, "abc"); !containsText(b, "differ from the manifest committed at HEAD") {
		t.Errorf("edited manifest bytes not blocked: %v", b)
	}
	if b := provenanceBlockers(filepath.Join(t.TempDir(), "m.json"), raw, "unknown"); !containsText(b, "outside the checkout") || !containsText(b, "git SHA is unknown") {
		t.Errorf("foreign manifest or unknown SHA not blocked: %v", b)
	}
	// The test binary carries no VCS stamp, and a missing file has no build information.
	for _, bin := range []string{os.Args[0], filepath.Join(t.TempDir(), "absent")} {
		if _, b := binaryBlockers(bin, "abc"); len(b) != 1 {
			t.Errorf("binary %s not blocked: %v", bin, b)
		}
	}
}

func containsText(list []string, text string) bool {
	for _, s := range list {
		if strings.Contains(s, text) {
			return true
		}
	}
	return false
}

// Review finding: every migration error was BLOCKED "check password". Only credentials and
// connectivity block; a migration defect is a FAIL with a redacted SQLSTATE.
func TestOnboardingMigrationErrorClassification(t *testing.T) {
	var block blockedError
	if err := migrationError(&pgconn.PgError{Code: "28P01", Message: "password authentication failed"}); !errors.As(err, &block) {
		t.Errorf("bad password not BLOCKED: %v", err)
	}
	err := migrationError(fmt.Errorf("apply 055: %w", &pgconn.PgError{Code: "42P07", Message: "relation already exists", Detail: "postgresql://secret@host"}))
	if errors.As(err, &block) || err.Error() != "migrate disposable database: SQLSTATE 42P07: relation already exists" {
		t.Errorf("migration defect: %v", err)
	}
}

// Review finding: provenance covered the supervisor binary but not the supervisor-load harness
// that issues the verdict. Both now share vcsBlockers.
func TestVCSBlockersRequireCleanHEADStamp(t *testing.T) {
	stamp := func(rev, modified string) []debug.BuildSetting {
		return []debug.BuildSetting{{Key: "vcs.revision", Value: rev}, {Key: "vcs.modified", Value: modified}}
	}
	for _, c := range []struct {
		name     string
		settings []debug.BuildSetting
		blocked  bool
	}{
		{"clean HEAD", stamp("abc", "false"), false},
		{"dirty tree", stamp("abc", "true"), true},
		{"other revision", stamp("def", "false"), true},
		{"no stamp", nil, true},
	} {
		if _, b := vcsBlockers("supervisor-load", c.settings, "abc"); (len(b) > 0) != c.blocked {
			t.Fatalf("%s: blockers %v, want blocked=%v", c.name, b, c.blocked)
		}
	}
	// A test binary carries no VCS stamp, so the running harness can never qualify from here.
	if b := harnessBlockers("abc"); len(b) == 0 {
		t.Fatal("unstamped harness was not blocked")
	}
}

// WIS-638: ru_maxrss is bytes on darwin and KiB on Linux; the same 111 MiB peak must read 111 MiB on both.
func TestMaxRSSMiBUsesThePlatformUnit(t *testing.T) {
	for _, c := range []struct {
		goos   string
		maxrss int64
		want   float64
	}{
		{"darwin", 111 * 1024 * 1024, 111},
		{"linux", 111 * 1024, 111},
		{"linux", 0, 0},
	} {
		if got := maxRSSMiB(c.maxrss, c.goos); got != c.want {
			t.Errorf("maxRSSMiB(%d, %q) = %v, want %v", c.maxrss, c.goos, got, c.want)
		}
	}
}
