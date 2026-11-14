// Command supervisor-load runs the WIS-270 knowledge-graph load qualification, or the
// onboarding load gate when the manifest has "kind": "onboarding" (see onboarding.go)
// (`make supervisor-load`). It reads frozen thresholds from a committed manifest
// before measuring, seeds a deterministic fixture in a disposable database, times
// scoped 2-hop knowledge.Service.ReadGraph calls as the restricted runtime role and
// writes a JSON report. Exit codes: 0 pass on the reference host, 1 threshold or hard-gate
// failure, 2 BLOCKED (missing database, manifest or other prerequisite, including a run that
// met every threshold on a host that is not the recorded reference host).
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

type Manifest struct {
	SchemaVersion int    `json:"schema_version"`
	Issue         string `json:"issue"`
	Description   string `json:"description"`
	Seed          int64  `json:"seed"`
	Fixture       struct {
		Entities              int `json:"entities"`
		Claims                int `json:"claims"`
		SiblingSiteEntities   int `json:"sibling_site_entities"`
		SiblingSiteClaims     int `json:"sibling_site_claims"`
		ForeignTenantEntities int `json:"foreign_tenant_entities"`
		ForeignTenantClaims   int `json:"foreign_tenant_claims"`
	} `json:"fixture"`
	Queries     int `json:"queries"`
	Concurrency int `json:"concurrency"`
	Hops        int `json:"hops"`
	Thresholds  struct {
		P95Ms           float64 `json:"p95_ms"`
		UnauthorizedMax int     `json:"unauthorized_max"`
		ErrorsMax       int     `json:"errors_max"`
	} `json:"thresholds"`
	ReferenceHost struct {
		Description               string `json:"description"`
		GOMAXPROCS                int    `json:"gomaxprocs"`
		PostgresLimitsAttestation string `json:"postgres_limits_attestation"`
	} `json:"reference_host"`
}

func loadManifest(path string) (Manifest, string, error) {
	var m Manifest
	raw, err := os.ReadFile(path)
	if err != nil {
		return m, "", err
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if err = d.Decode(&m); err != nil {
		return m, "", err
	}
	f := m.Fixture
	if m.SchemaVersion != 1 || m.Queries < 1 || m.Concurrency < 2 || m.Hops != 2 || m.Thresholds.P95Ms <= 0 || m.Thresholds.UnauthorizedMax < 0 || m.Thresholds.ErrorsMax < 0 ||
		f.Entities < 1000 || f.Claims < 1000 || f.SiblingSiteEntities < 100 || f.SiblingSiteClaims < 100 || f.ForeignTenantEntities < 100 || f.ForeignTenantClaims < 100 ||
		m.ReferenceHost.GOMAXPROCS < 1 || m.ReferenceHost.PostgresLimitsAttestation == "" {
		return m, "", errors.New("manifest is incomplete or out of range")
	}
	sum := sha256.Sum256(raw)
	return m, hex.EncodeToString(sum[:]), nil
}

// percentile is nearest-rank over durations: the smallest value with at least p% of
// samples at or below it. It never interpolates.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	return sorted[max(rank, 1)-1]
}

type Latency struct {
	Count int     `json:"count"`
	P50Ms float64 `json:"p50_ms"`
	P95Ms float64 `json:"p95_ms"`
	P99Ms float64 `json:"p99_ms"`
	MaxMs float64 `json:"max_ms"`
}

func summarize(samples []time.Duration) Latency {
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
	out := Latency{Count: len(sorted)}
	if len(sorted) > 0 {
		out.P50Ms, out.P95Ms, out.P99Ms, out.MaxMs = ms(percentile(sorted, 50)), ms(percentile(sorted, 95)), ms(percentile(sorted, 99)), ms(sorted[len(sorted)-1])
	}
	return out
}

// evaluate compares a measurement against the frozen thresholds; every breach is listed.
func evaluate(m Manifest, l Latency, unauthorized, errs int) []string {
	var failures []string
	if l.Count != m.Queries {
		failures = append(failures, fmt.Sprintf("measured %d queries, manifest requires %d", l.Count, m.Queries))
	}
	if l.P95Ms > m.Thresholds.P95Ms {
		failures = append(failures, fmt.Sprintf("p95 %.3fms exceeds %.3fms", l.P95Ms, m.Thresholds.P95Ms))
	}
	if unauthorized > m.Thresholds.UnauthorizedMax {
		failures = append(failures, fmt.Sprintf("%d unauthorized results exceed %d", unauthorized, m.Thresholds.UnauthorizedMax))
	}
	if errs > m.Thresholds.ErrorsMax {
		failures = append(failures, fmt.Sprintf("%d query errors exceed %d", errs, m.Thresholds.ErrorsMax))
	}
	return failures
}

type Report struct {
	SchemaVersion  int      `json:"schema_version"`
	Issue          string   `json:"issue"`
	Result         string   `json:"result"`
	Failures       []string `json:"failures"`
	GitSHA         string   `json:"git_sha"`
	GitDirty       bool     `json:"git_dirty"`
	Command        string   `json:"command"`
	Argv           []string `json:"argv"`
	StartedAt      string   `json:"started_at"`
	ManifestPath   string   `json:"manifest_path"`
	ManifestSHA256 string   `json:"manifest_sha256"`
	Manifest       Manifest `json:"manifest"`
	Host           struct {
		Evidence        string `json:"evidence"`
		NumCPU          int    `json:"num_cpu"`
		GOMAXPROCS      int    `json:"gomaxprocs"`
		MemTotal        string `json:"mem_total"`
		GoVersion       string `json:"go_version"`
		GOOS            string `json:"goos"`
		GOARCH          string `json:"goarch"`
		PostgresVersion string `json:"postgres_version"`
		PostgresLimits  string `json:"postgres_limits_attested"`
	} `json:"host"`
	Measured string   `json:"measured"`
	Fixture  Fixture  `json:"fixture"`
	Runs     []Run    `json:"runs"`
	Probes   []Probe  `json:"probes"`
	Seeding  Timings  `json:"seeding_seconds"`
	Notes    []string `json:"notes"`
}

func gitOutput(args ...string) string {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func memTotal() string {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return "unknown"
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return "unknown"
}

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("supervisor-load", flag.ContinueOnError)
	flags.SetOutput(stderr)
	manifestPath := flags.String("manifest", "deploy/qualification/knowledge-load.json", "frozen qualification manifest")
	outDir := flags.String("out", "build/qualification", "report directory")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	blocked := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "BLOCKED: "+format+"\n", a...)
		return 2
	}
	// The onboarding gate is selected by its manifest kind; the knowledge manifest has none.
	if raw, err := os.ReadFile(*manifestPath); err == nil {
		var probe struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(raw, &probe) == nil && probe.Kind == "onboarding" {
			return runOnboarding(*manifestPath, *outDir, getenv, stdout, stderr, onboardingOptions{})
		}
		if json.Unmarshal(raw, &probe) == nil && probe.Kind == "chat-passages" {
			return runPassages(*manifestPath, *outDir, getenv, stdout, stderr)
		}
	}
	// Rule 09: freeze the thresholds before any measurement exists.
	manifest, manifestSHA, err := loadManifest(*manifestPath)
	if err != nil {
		return blocked("qualification manifest %s: %v", *manifestPath, err)
	}
	adminURL, runtimeURL := getenv("SUPERVISOR_TEST_ADMIN_URL"), getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		return blocked("SUPERVISOR_TEST_ADMIN_URL and SUPERVISOR_TEST_DATABASE_URL are required (real PostgreSQL)")
	}
	r := Report{SchemaVersion: 1, Issue: manifest.Issue, Failures: []string{}, Argv: os.Args, StartedAt: time.Now().UTC().Format(time.RFC3339), ManifestPath: *manifestPath, ManifestSHA256: manifestSHA, Manifest: manifest}
	r.Command = getenv("SUPERVISOR_LOAD_COMMAND")
	if r.Command == "" {
		r.Command = strings.Join(os.Args, " ")
	}
	r.GitSHA = gitOutput("rev-parse", "HEAD")
	r.GitDirty = gitOutput("status", "--porcelain", "--untracked-files=no") != ""
	r.Host.NumCPU, r.Host.GOMAXPROCS, r.Host.MemTotal = runtime.NumCPU(), runtime.GOMAXPROCS(0), memTotal()
	r.Host.GoVersion, r.Host.GOOS, r.Host.GOARCH = runtime.Version(), runtime.GOOS, runtime.GOARCH
	r.Host.PostgresLimits = getenv("SUPERVISOR_LOAD_PG_LIMITS")
	r.Host.Evidence = "not reference-host evidence: client GOMAXPROCS and PostgreSQL limits do not match the recorded 4 vCPU / 8 GiB host"
	reference := r.Host.GOMAXPROCS == manifest.ReferenceHost.GOMAXPROCS && r.Host.PostgresLimits == manifest.ReferenceHost.PostgresLimitsAttestation
	if reference {
		r.Host.Evidence = "operator-attested reference-host configuration (GOMAXPROCS matched; PostgreSQL limits attested by SUPERVISOR_LOAD_PG_LIMITS, not measured by this tool)"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	result, err := qualify(ctx, manifest, adminURL, runtimeURL, stderr)
	var block blockedError
	if errors.As(err, &block) {
		return blocked("%v", err)
	}
	if err != nil {
		// A fixture or product error mid-run is a failed qualification, not a skipped one.
		r.Failures = append(r.Failures, "run aborted: "+err.Error())
	}
	r.Host.PostgresVersion = result.PostgresVersion
	r.Measured, r.Fixture, r.Probes, r.Seeding = result.Measured, result.Fixture, result.Probes, result.Timings
	r.Runs, r.Notes = result.Runs, result.Notes
	if err == nil && len(r.Runs) != 2 {
		r.Failures = append(r.Failures, "sequential and concurrent runs are both required")
	}
	// The same frozen p95 and zero-unauthorized gates apply to every run.
	for _, run := range r.Runs {
		for _, f := range evaluate(manifest, run.Latency, run.Unauthorized, run.Errors) {
			r.Failures = append(r.Failures, run.Name+": "+f)
		}
	}
	for _, p := range r.Probes {
		if !p.Passed {
			r.Failures = append(r.Failures, fmt.Sprintf("probe %q: %s", p.Name, p.Outcome))
		}
	}
	var exit int
	r.Result, exit = verdict(len(r.Failures), reference)
	if err = os.MkdirAll(*outDir, 0o755); err != nil {
		return blocked("report directory: %v", err)
	}
	path := filepath.Join(*outDir, "knowledge-load-"+r.GitSHA+".json")
	body, _ := json.MarshalIndent(r, "", "  ")
	if err = os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		return blocked("write report: %v", err)
	}
	fmt.Fprintf(stdout, "%s knowledge load (%s)\n", r.Result, r.Host.Evidence)
	fx := r.Fixture
	fmt.Fprintf(stdout, "fixture: site %d entities / %d claims; snapshot %d entities (%.1f%%) / %d claims (%.1f%%) / %d bytes (%.1f%% of the byte cap)\n", fx.Entities, fx.Claims, fx.SnapshotEntities, fx.SnapshotEntitiesPct, fx.SnapshotClaims, fx.SnapshotClaimsPct, fx.SnapshotBytes, fx.SnapshotBytesPct)
	for _, run := range r.Runs {
		l := run.Latency
		fmt.Fprintf(stdout, "%s x%d: %d queries, p50 %.3f p95 %.3f p99 %.3f max %.3f ms (p95 <= %.0f), unauthorized %d, errors %d, truncated %d\n", run.Name, run.Concurrency, l.Count, l.P50Ms, l.P95Ms, l.P99Ms, l.MaxMs, manifest.Thresholds.P95Ms, run.Unauthorized, run.Errors, run.TruncatedResponses)
	}
	fmt.Fprintf(stdout, "probes %d; report %s\n", len(r.Probes), path)
	for _, f := range r.Failures {
		fmt.Fprintln(stderr, "FAIL:", f)
	}
	if r.Result == "BLOCKED" {
		fmt.Fprintln(stderr, "BLOCKED: every threshold was met, but not on the recorded reference host; this run is not qualification evidence")
	}
	return exit
}

// verdict maps a run to its result and exit code. Only a run on the reference host can pass:
// meeting the thresholds elsewhere is a missing prerequisite (rule 09: BLOCKED, never green).
func verdict(failures int, reference bool) (string, int) {
	switch {
	case failures > 0:
		return "FAIL", 1
	case !reference:
		return "BLOCKED", 2
	default:
		return "PASS", 0
	}
}
