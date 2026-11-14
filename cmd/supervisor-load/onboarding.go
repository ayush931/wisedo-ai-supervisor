package main

// Onboarding load gate (proposal section I). Selected by `"kind": "onboarding"` in the
// manifest; the knowledge manifest has no kind and keeps its own path.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	apiconfig "github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/httpapi"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/operational"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// OnboardingThresholds are pointers so an absent threshold is refused, never read as zero.
type OnboardingThresholds struct {
	IntakeP95Ms        *float64 `json:"intake_p95_ms"`
	IntakeP99Ms        *float64 `json:"intake_p99_ms"`
	StationP95Ms       *float64 `json:"station_write_p95_ms"`
	StationP99Ms       *float64 `json:"station_write_p99_ms"`
	ProvisionMaxMs     *float64 `json:"provision_max_ms"`
	ErrorRateMax       *float64 `json:"error_rate_max"`
	MaxRSSMiB          *float64 `json:"max_rss_mib"`
	QueueDepthMax      *int64   `json:"queue_depth_max"`
	CrossTenantRowsMax *int64   `json:"cross_tenant_rows_max"`
}

type OnboardingManifest struct {
	SchemaVersion         int                   `json:"schema_version"`
	Kind                  string                `json:"kind"`
	Issue                 string                `json:"issue"`
	Description           string                `json:"description"`
	Seed                  int64                 `json:"seed"`
	Tenants               int                   `json:"tenants"`
	SitesPerTenant        int                   `json:"sites_per_tenant"`
	WorkstationsPerSite   int                   `json:"workstations_per_site"`
	IntakeBatchesPerSite  int                   `json:"intake_batches_per_site"`
	EventsPerBatch        int                   `json:"events_per_batch"`
	StationRecordsPerSite int                   `json:"station_records_per_site"`
	Concurrency           int                   `json:"concurrency"`
	Thresholds            *OnboardingThresholds `json:"thresholds"`
	ReferenceHost         struct {
		Description         string `json:"description"`
		GOMAXPROCS          int    `json:"gomaxprocs"`
		NumCPU              int    `json:"num_cpu"`
		MemTotalGiB         int    `json:"mem_total_gib"`
		PostgresCPUs        int    `json:"postgres_cpus"`
		PostgresMemoryBytes int64  `json:"postgres_memory_bytes"`
	} `json:"reference_host"`
}

func (m OnboardingManifest) sites() int { return m.Tenants * m.SitesPerTenant }

// validate refuses a manifest without every frozen threshold (rule 09) or with an
// out-of-range workload.
func (m OnboardingManifest) validate() error {
	if m.SchemaVersion != 1 || m.Kind != "onboarding" || m.Issue == "" {
		return errors.New("manifest schema_version, kind or issue is invalid")
	}
	if m.Tenants < 2 || m.Tenants > 100 || m.SitesPerTenant < 1 || m.SitesPerTenant > 50 || m.WorkstationsPerSite < 1 || m.WorkstationsPerSite > 200 ||
		m.IntakeBatchesPerSite < 1 || m.EventsPerBatch < 1 || m.EventsPerBatch > ingestion.MaxEvents || m.StationRecordsPerSite < 1 ||
		m.Concurrency < 1 || m.Concurrency > m.sites() {
		return errors.New("manifest workload is out of range (at least two tenants are required for the isolation check)")
	}
	if m.ReferenceHost.GOMAXPROCS < 1 || m.ReferenceHost.NumCPU < 1 || m.ReferenceHost.MemTotalGiB < 1 || m.ReferenceHost.PostgresCPUs < 1 || m.ReferenceHost.PostgresMemoryBytes < 1 {
		return errors.New("manifest reference_host is incomplete")
	}
	t := m.Thresholds
	if t == nil {
		return errors.New("manifest has no thresholds; they must be frozen before any measurement")
	}
	missing := []string{}
	for name, absent := range map[string]bool{"intake_p95_ms": t.IntakeP95Ms == nil, "intake_p99_ms": t.IntakeP99Ms == nil, "station_write_p95_ms": t.StationP95Ms == nil,
		"station_write_p99_ms": t.StationP99Ms == nil, "provision_max_ms": t.ProvisionMaxMs == nil, "error_rate_max": t.ErrorRateMax == nil,
		"max_rss_mib": t.MaxRSSMiB == nil, "queue_depth_max": t.QueueDepthMax == nil, "cross_tenant_rows_max": t.CrossTenantRowsMax == nil} {
		if absent {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("manifest thresholds missing: %s", strings.Join(missing, ", "))
	}
	if *t.IntakeP95Ms <= 0 || *t.IntakeP99Ms < *t.IntakeP95Ms || *t.StationP95Ms <= 0 || *t.StationP99Ms < *t.StationP95Ms || *t.ProvisionMaxMs <= 0 ||
		*t.ErrorRateMax < 0 || *t.ErrorRateMax > 1 || *t.MaxRSSMiB <= 0 || *t.QueueDepthMax < 0 || *t.CrossTenantRowsMax < 0 {
		return errors.New("manifest thresholds are out of range")
	}
	return nil
}

func loadOnboardingManifest(path string) (OnboardingManifest, string, error) {
	var m OnboardingManifest
	raw, err := os.ReadFile(path)
	if err != nil {
		return m, "", err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(&m); err != nil {
		return m, "", err
	}
	if d.More() {
		return m, "", errors.New("trailing data after manifest")
	}
	if err = m.validate(); err != nil {
		return m, "", err
	}
	sum := sha256.Sum256(raw)
	return m, hex.EncodeToString(sum[:]), nil
}

// OnboardingResult is what one run measured.
type OnboardingResult struct {
	Tenants          int            `json:"tenants"`
	Sites            int            `json:"sites"`
	Provision        Latency        `json:"provision"`
	Intake           Latency        `json:"intake_batches"`
	StationWrites    Latency        `json:"station_record_writes"`
	IntakeErrors     int            `json:"intake_errors"`
	StationErrors    int            `json:"station_errors"`
	ErrorRate        float64        `json:"error_rate"`
	ErrorsByStatus   map[string]int `json:"errors_by_status"`
	EventsExpected   int64          `json:"events_expected"`
	EventsIn202      int64          `json:"events_in_accepted_batches"`
	EventsAccepted   int64          `json:"events_accepted"`
	EventsQuarantine int64          `json:"events_quarantined"`
	QueueDepthPeak   int64          `json:"queue_depth_peak_events"`
	QueueResidual    int64          `json:"queue_residual_events"`
	QueueSamples     int            `json:"queue_samples"`
	EventsFolded     int64          `json:"events_folded"`
	MaxRSSMiB        float64        `json:"max_rss_mib"`
	CrossTenantRows  int64          `json:"cross_tenant_rows_visible"`
	IsolationChecks  int            `json:"isolation_checks"`
	MeasuredSeconds  float64        `json:"measured_wall_seconds"`
	SetupSeconds     float64        `json:"setup_seconds"`
	ProvisionSeconds float64        `json:"provision_seconds"`
}

// evaluateOnboarding compares a measurement against the frozen thresholds; every breach is listed.
func evaluateOnboarding(m OnboardingManifest, r OnboardingResult) []string {
	t := m.Thresholds
	var f []string
	if want := m.sites() * m.IntakeBatchesPerSite; r.Intake.Count != want {
		f = append(f, fmt.Sprintf("measured %d intake batches, manifest requires %d", r.Intake.Count, want))
	}
	if want := m.sites() * m.StationRecordsPerSite; r.StationWrites.Count != want {
		f = append(f, fmt.Sprintf("measured %d station record writes, manifest requires %d", r.StationWrites.Count, want))
	}
	events := int64(m.sites() * m.IntakeBatchesPerSite * m.EventsPerBatch)
	for name, got := range map[string]int64{"events counted in 202 responses": r.EventsIn202, "events accepted (sequence heads)": r.EventsAccepted, "events folded": r.EventsFolded} {
		if got != events {
			f = append(f, fmt.Sprintf("%s %d, manifest requires %d", name, got, events))
		}
	}
	if r.EventsQuarantine != 0 {
		f = append(f, fmt.Sprintf("%d events quarantined", r.EventsQuarantine))
	}
	if r.Provision.Count != m.Tenants {
		f = append(f, fmt.Sprintf("provisioned %d tenants, manifest requires %d", r.Provision.Count, m.Tenants))
	}
	check := func(name string, got, limit float64) {
		if got > limit {
			f = append(f, fmt.Sprintf("%s %.3f exceeds %.3f", name, got, limit))
		}
	}
	check("intake p95 ms", r.Intake.P95Ms, *t.IntakeP95Ms)
	check("intake p99 ms", r.Intake.P99Ms, *t.IntakeP99Ms)
	check("station write p95 ms", r.StationWrites.P95Ms, *t.StationP95Ms)
	check("station write p99 ms", r.StationWrites.P99Ms, *t.StationP99Ms)
	check("provision max ms", r.Provision.MaxMs, *t.ProvisionMaxMs)
	check("error rate", r.ErrorRate, *t.ErrorRateMax)
	check("max RSS MiB", r.MaxRSSMiB, *t.MaxRSSMiB)
	check("queue depth peak", float64(r.QueueDepthPeak), float64(*t.QueueDepthMax))
	check("cross-tenant rows visible", float64(r.CrossTenantRows), float64(*t.CrossTenantRowsMax))
	if r.QueueResidual != 0 {
		f = append(f, fmt.Sprintf("outbox not drained: %d events still lagging", r.QueueResidual))
	}
	if r.IsolationChecks == 0 {
		f = append(f, "no tenant isolation check ran")
	}
	return f
}

type OnboardingReport struct {
	SchemaVersion int      `json:"schema_version"`
	Issue         string   `json:"issue"`
	Result        string   `json:"result"`
	Failures      []string `json:"failures"`
	// Blockers are why a run that met every threshold is still not qualification evidence.
	Blockers       []string           `json:"qualification_blockers"`
	GitSHA         string             `json:"git_sha"`
	GitDirty       bool               `json:"git_dirty"`
	Command        string             `json:"command"`
	Argv           []string           `json:"argv"`
	StartedAt      string             `json:"started_at"`
	ManifestPath   string             `json:"manifest_path"`
	ManifestSHA256 string             `json:"manifest_sha256"`
	Manifest       OnboardingManifest `json:"manifest"`
	Host           struct {
		Evidence        string `json:"evidence"`
		NumCPU          int    `json:"num_cpu"`
		GOMAXPROCS      int    `json:"gomaxprocs"`
		MemTotal        string `json:"mem_total"`
		GoVersion       string `json:"go_version"`
		GOOS            string `json:"goos"`
		GOARCH          string `json:"goarch"`
		PostgresVersion string `json:"postgres_version"`
		PostgresCPUs    string `json:"postgres_cgroup_cpus"`
		PostgresMemory  string `json:"postgres_cgroup_memory_bytes"`
		SupervisorBin   string `json:"supervisor_binary_vcs"`
	} `json:"host"`
	Measured string           `json:"measured"`
	SeedRole string           `json:"seed_role"`
	Results  OnboardingResult `json:"results"`
	Probes   []Probe          `json:"probes"`
	Notes    []string         `json:"notes"`
}

const onboardingMeasured = "Provision: wall time of each `supervisor provision` child process (built from this checkout) as supervisor_migrator, one per tenant. Intake: POST /v1/sites/{site}/operational-sources/{source}/batches and station writes: POST /v1/sites/{site}/workstations/{workstation}/records, both through the in-process httpapi handler (cookie session, CSRF, strict JSON, idempotency included; TCP excluded) on the restricted runtime role. Each site is one sequential client stream (both writes take the site lock); sites run in parallel up to the manifest concurrency. An in-process consumer folds the outbox with ingestion.Service.Fold (limit 500), as dispatch.FoldSource does, while intake runs."

const onboardingSeedRole = "Tenant and first site per tenant through `supervisor provision` (with its owner invitation, never redeemed); further sites of a tenant by direct INSERT as supervisor_migrator with the tenant context set (provision creates one site per tenant; these extra sites have no provisioning_audit row). Memberships and synthetic browser sessions by the privileged admin connection, as the integration fixtures do. Every business object (instruction document and its authority, operational source and its approval, station profiles, job orders, shift plan and its approval by a second supervisor, station authorizations) through the HTTP API as the runtime role."

// onboardingOptions exists only for this package's tests. The CLI always passes the zero
// value; testOnly forces NOT_QUALIFIED, so a test manifest can never PASS.
type onboardingOptions struct {
	testOnly bool
	// afterSetup runs as the privileged role in the disposable database before measuring; its
	// restore runs after the probes (negative controls such as a loosened RLS policy).
	afterSetup func(ctx context.Context, admin *sql.DB) (restore func() error, err error)
}

// localDatabase refuses anything but a loopback PostgreSQL: the gate creates and drops
// databases, so it never runs against a shared or production server (as supervisor-seed).
func localDatabase(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "postgresql" && parsed.Scheme != "postgres") || !loopbackHost(parsed.Hostname()) {
		return errors.New("database URL must be a loopback PostgreSQL URL")
	}
	c, err := pgx.ParseConfig(raw)
	if err != nil || !loopbackHost(c.Host) {
		return errors.New("database URL effective host must be loopback")
	}
	for _, fallback := range c.Fallbacks {
		if !loopbackHost(fallback.Host) {
			return errors.New("database URL fallback host must be loopback")
		}
	}
	return nil
}

func loopbackHost(host string) bool {
	ip, err := netip.ParseAddr(host)
	return host == "localhost" || (err == nil && ip.IsLoopback())
}

// hostFacts are what the tool measured about the client host and the PostgreSQL container.
type hostFacts struct {
	GOMAXPROCS, NumCPU int
	MemTotalKiB        int64
	PostgresCPUs       string // from cgroup cpu.max, e.g. "4" or "max"
	PostgresMemory     string // from cgroup memory.max, bytes or "max"
}

// referenceMismatches lists every way the measured host differs from the manifest's reference host.
func referenceMismatches(m OnboardingManifest, h hostFacts) []string {
	ref := m.ReferenceHost
	var out []string
	if h.GOMAXPROCS != ref.GOMAXPROCS {
		out = append(out, fmt.Sprintf("GOMAXPROCS %d, reference %d", h.GOMAXPROCS, ref.GOMAXPROCS))
	}
	if h.NumCPU != ref.NumCPU {
		out = append(out, fmt.Sprintf("client host has %d CPUs, reference %d", h.NumCPU, ref.NumCPU))
	}
	if gib := (h.MemTotalKiB + 512*1024) / (1024 * 1024); gib != int64(ref.MemTotalGiB) {
		out = append(out, fmt.Sprintf("client host MemTotal %d KiB (~%d GiB), reference %d GiB", h.MemTotalKiB, gib, ref.MemTotalGiB))
	}
	if h.PostgresCPUs != strconv.Itoa(ref.PostgresCPUs) {
		out = append(out, fmt.Sprintf("PostgreSQL cgroup CPUs %q, reference %d", h.PostgresCPUs, ref.PostgresCPUs))
	}
	if h.PostgresMemory != strconv.FormatInt(ref.PostgresMemoryBytes, 10) {
		out = append(out, fmt.Sprintf("PostgreSQL cgroup memory %q, reference %d bytes", h.PostgresMemory, ref.PostgresMemoryBytes))
	}
	return out
}

// cgroupCPUs turns a cgroup v2 cpu.max ("quota period" or "max period") into a CPU count.
func cgroupCPUs(raw string) string {
	fields := strings.Fields(raw)
	if len(fields) == 2 && fields[0] == "max" {
		return "max"
	}
	if len(fields) != 2 {
		return strings.TrimSpace(raw)
	}
	quota, e1 := strconv.ParseInt(fields[0], 10, 64)
	period, e2 := strconv.ParseInt(fields[1], 10, 64)
	if e1 != nil || e2 != nil || period <= 0 {
		return "unreadable"
	}
	if quota%period != 0 {
		return strconv.FormatFloat(float64(quota)/float64(period), 'f', 3, 64)
	}
	return strconv.FormatInt(quota/period, 10)
}

func memTotalKiB() int64 {
	n, _ := strconv.ParseInt(strings.TrimSuffix(memTotal(), " kB"), 10, 64)
	return n
}

// provenanceBlockers refuses qualification unless the run is the committed manifest on a clean
// checkout with a known SHA.
func provenanceBlockers(manifestPath string, raw []byte, sha string) []string {
	var out []string
	if sha == "unknown" || sha == "" {
		out = append(out, "git SHA is unknown")
	}
	if status, err := exec.Command("git", "status", "--porcelain").Output(); err != nil || len(bytes.TrimSpace(status)) != 0 {
		out = append(out, "working tree is not clean (tracked changes or untracked files)")
	}
	top := gitOutput("rev-parse", "--show-toplevel")
	abs, err := filepath.Abs(manifestPath)
	rel, relErr := filepath.Rel(top, abs)
	if err != nil || relErr != nil || strings.HasPrefix(rel, "..") {
		return append(out, "manifest is outside the checkout")
	}
	committed, err := exec.Command("git", "show", "HEAD:"+filepath.ToSlash(rel)).Output()
	if err != nil || !bytes.Equal(committed, raw) {
		out = append(out, "manifest bytes differ from the manifest committed at HEAD ("+filepath.ToSlash(rel)+")")
	}
	return out
}

// binaryBlockers requires the provision binary to be built from HEAD of a clean tree.
func binaryBlockers(bin, sha string) (string, []string) {
	info, err := buildinfo.ReadFile(bin)
	if err != nil {
		return "unreadable", []string{"supervisor binary build information is unreadable"}
	}
	return vcsBlockers("supervisor", info.Settings, sha)
}

// harnessBlockers applies the same requirement to this supervisor-load binary, which issues
// the verdict: a modified harness built from a dirty tree must never report PASS.
func harnessBlockers(sha string) []string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return []string{"supervisor-load build information is unavailable"}
	}
	_, b := vcsBlockers("supervisor-load", info.Settings, sha)
	return b
}

// vcsBlockers reports a blocker unless the build stamp names HEAD of a clean checkout. The
// stamp is self-reported, so this catches accidents and modified builds, not deliberate forgery.
func vcsBlockers(name string, settings []debug.BuildSetting, sha string) (string, []string) {
	vcs := map[string]string{}
	for _, s := range settings {
		vcs[s.Key] = s.Value
	}
	described := "vcs.revision=" + vcs["vcs.revision"] + " vcs.modified=" + vcs["vcs.modified"]
	if vcs["vcs.revision"] == "" || vcs["vcs.revision"] != sha || vcs["vcs.modified"] != "false" {
		return described, []string{name + " binary was not built from HEAD of a clean checkout (" + described + ")"}
	}
	return described, nil
}

// onboardingVerdict: a breach is FAIL (exit 1); a run that met every threshold but is not
// qualification evidence is NOT_QUALIFIED (exit 2, BLOCKED); only a qualified run is PASS.
func onboardingVerdict(failures int, blockers []string) (string, int) {
	switch {
	case failures > 0:
		return "FAIL", 1
	case len(blockers) > 0:
		return "NOT_QUALIFIED", 2
	default:
		return "PASS", 0
	}
}

func runOnboarding(manifestPath, outDir string, getenv func(string) string, stdout, stderr io.Writer, opts onboardingOptions) int {
	blocked := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "BLOCKED: "+format+"\n", a...)
		return 2
	}
	manifest, manifestSHA, err := loadOnboardingManifest(manifestPath)
	if err != nil {
		return blocked("qualification manifest %s: %v", manifestPath, err)
	}
	raw, _ := os.ReadFile(manifestPath)
	if getenv("SUPERVISOR_ENVIRONMENT") != "local" {
		return blocked("the onboarding load gate creates and drops databases; it requires SUPERVISOR_ENVIRONMENT=local and a disposable loopback PostgreSQL")
	}
	adminURL, runtimeURL, migrationPassword := getenv("SUPERVISOR_TEST_ADMIN_URL"), getenv("SUPERVISOR_TEST_DATABASE_URL"), getenv("SUPERVISOR_MIGRATION_PASSWORD")
	if adminURL == "" || runtimeURL == "" || migrationPassword == "" {
		return blocked("SUPERVISOR_TEST_ADMIN_URL, SUPERVISOR_TEST_DATABASE_URL and SUPERVISOR_MIGRATION_PASSWORD are required (real PostgreSQL)")
	}
	for name, value := range map[string]string{"SUPERVISOR_TEST_ADMIN_URL": adminURL, "SUPERVISOR_TEST_DATABASE_URL": runtimeURL} {
		if err = localDatabase(value); err != nil {
			return blocked("%s refused: %v", name, err)
		}
	}
	r := OnboardingReport{SchemaVersion: 1, Issue: manifest.Issue, Failures: []string{}, Blockers: []string{}, Argv: os.Args, StartedAt: time.Now().UTC().Format(time.RFC3339),
		ManifestPath: manifestPath, ManifestSHA256: manifestSHA, Manifest: manifest, Measured: onboardingMeasured, SeedRole: onboardingSeedRole}
	r.Command = getenv("SUPERVISOR_LOAD_COMMAND")
	if r.Command == "" {
		r.Command = strings.Join(os.Args, " ")
	}
	r.GitSHA = gitOutput("rev-parse", "HEAD")
	r.GitDirty = gitOutput("status", "--porcelain") != ""
	r.Host.NumCPU, r.Host.GOMAXPROCS, r.Host.MemTotal = runtime.NumCPU(), runtime.GOMAXPROCS(0), memTotal()
	r.Host.GoVersion, r.Host.GOOS, r.Host.GOARCH = runtime.Version(), runtime.GOOS, runtime.GOARCH

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	out, err := qualifyOnboarding(ctx, manifest, adminURL, runtimeURL, migrationPassword, getenv("SUPERVISOR_LOAD_SUPERVISOR_BIN"), stderr, opts)
	var block blockedError
	if errors.As(err, &block) {
		return blocked("%v", err)
	}
	if err != nil {
		// A product error mid-run is a failed qualification, not a skipped one.
		r.Failures = append(r.Failures, "run aborted: "+err.Error())
	}
	r.Host.PostgresVersion, r.Host.PostgresCPUs, r.Host.PostgresMemory = out.postgresVersion, out.postgresCPUs, out.postgresMemory
	r.Results, r.Probes, r.Notes = out.result, out.probes, out.notes
	if err == nil {
		r.Failures = append(r.Failures, evaluateOnboarding(manifest, r.Results)...)
	}
	for _, p := range r.Probes {
		if !p.Passed {
			r.Failures = append(r.Failures, fmt.Sprintf("probe %q: %s", p.Name, p.Outcome))
		}
	}
	// Qualification evidence needs the measured reference host and exact provenance (rule 09).
	mismatches := referenceMismatches(manifest, hostFacts{GOMAXPROCS: r.Host.GOMAXPROCS, NumCPU: r.Host.NumCPU, MemTotalKiB: memTotalKiB(), PostgresCPUs: out.postgresCPUs, PostgresMemory: out.postgresMemory})
	for _, mm := range mismatches {
		r.Blockers = append(r.Blockers, "not the reference host: "+mm)
	}
	r.Blockers = append(r.Blockers, provenanceBlockers(manifestPath, raw, r.GitSHA)...)
	r.Host.SupervisorBin = out.supervisorBin
	r.Blockers = append(r.Blockers, out.binaryBlockers...)
	r.Blockers = append(r.Blockers, harnessBlockers(r.GitSHA)...)
	if out.supervisorBin == "" {
		r.Blockers = append(r.Blockers, "supervisor binary provenance was never checked")
	}
	if opts.testOnly {
		r.Blockers = append(r.Blockers, "test-only manifest path: never qualification evidence")
	}
	r.Host.Evidence = "measured reference-host configuration (client CPUs, memory and GOMAXPROCS; PostgreSQL cgroup limits read through the database)"
	if len(mismatches) > 0 {
		r.Host.Evidence = "not reference-host evidence"
	}
	var exit int
	r.Result, exit = onboardingVerdict(len(r.Failures), r.Blockers)
	if err = os.MkdirAll(outDir, 0o755); err != nil {
		return blocked("report directory: %v", err)
	}
	path := filepath.Join(outDir, "onboarding-load-"+r.GitSHA+".json")
	body, _ := json.MarshalIndent(r, "", "  ")
	if err = os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		return blocked("write report: %v", err)
	}
	res, t := r.Results, manifest.Thresholds
	fmt.Fprintf(stdout, "%s onboarding load (%s)\n", r.Result, r.Host.Evidence)
	fmt.Fprintf(stdout, "provision %d tenants (1 site each via supervisor provision, %d more per tenant inserted directly): max %.1f ms (<= %g)\n", res.Tenants, manifest.SitesPerTenant-1, res.Provision.MaxMs, *t.ProvisionMaxMs)
	fmt.Fprintf(stdout, "intake %d batches / %d events: p50 %.3f p95 %.3f p99 %.3f max %.3f ms (p95 <= %g, p99 <= %g)\n", res.Intake.Count, res.EventsIn202, res.Intake.P50Ms, res.Intake.P95Ms, res.Intake.P99Ms, res.Intake.MaxMs, *t.IntakeP95Ms, *t.IntakeP99Ms)
	fmt.Fprintf(stdout, "station writes %d: p50 %.3f p95 %.3f p99 %.3f max %.3f ms (p95 <= %g, p99 <= %g)\n", res.StationWrites.Count, res.StationWrites.P50Ms, res.StationWrites.P95Ms, res.StationWrites.P99Ms, res.StationWrites.MaxMs, *t.StationP95Ms, *t.StationP99Ms)
	fmt.Fprintf(stdout, "errors %d+%d rate %.4f (<= %g); max RSS %.1f MiB (<= %g); events accepted %d folded %d quarantined %d; queue peak %d residual %d (<= %d); cross-tenant rows %d over %d checks\n",
		res.IntakeErrors, res.StationErrors, res.ErrorRate, *t.ErrorRateMax, res.MaxRSSMiB, *t.MaxRSSMiB, res.EventsAccepted, res.EventsFolded, res.EventsQuarantine, res.QueueDepthPeak, res.QueueResidual, *t.QueueDepthMax, res.CrossTenantRows, res.IsolationChecks)
	fmt.Fprintf(stdout, "probes %d; report %s\n", len(r.Probes), path)
	for _, f := range r.Failures {
		fmt.Fprintln(stderr, "FAIL:", f)
	}
	if r.Result == "NOT_QUALIFIED" {
		for _, b := range r.Blockers {
			fmt.Fprintln(stderr, "BLOCKED: not qualification evidence:", b)
		}
	}
	return exit
}

type onboardingOutcome struct {
	postgresVersion string
	postgresCPUs    string
	postgresMemory  string
	supervisorBin   string
	binaryBlockers  []string
	result          OnboardingResult
	probes          []Probe
	notes           []string
}

type loadActor struct{ subject, token, csrf string }

type loadSite struct {
	tenant, site, source string
	actors               map[string]loadActor // owner, lead, approver, operator (per tenant)
	stations             []loadStation
	nextSequence         int64
	eventsIn202          int64 // events reported by non-replayed 202 responses
}

type loadStation struct {
	workstation, jobOrder string
	lastAt                time.Time
}

type onboardingHarness struct {
	ctx            context.Context
	admin          *sql.DB // privileged; disposable database
	migrator       *sql.DB
	runtime        *store.Store
	handler        http.Handler
	issuer, origin string
	rng            *rand.Rand
	mu             sync.Mutex
	errorsByStatus map[string]int
}

func (h *onboardingHarness) id() string {
	u, err := uuid.NewRandomFromReader(h.rng)
	if err != nil {
		panic(err)
	}
	return u.String()
}

// call drives the in-process API as one synthetic user and returns the HTTP status, the
// handler time, and an error naming the status and problem code only (never the body).
func (h *onboardingHarness) call(a loadActor, site, method, path, key, ifMatch string, body, out any, accepted ...int) (int, time.Duration, error) {
	encoded := []byte(nil)
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			return 0, 0, err
		}
	}
	req := httptest.NewRequestWithContext(h.ctx, method, "/v1/sites/"+site+path, bytes.NewReader(encoded))
	req.AddCookie(&http.Cookie{Name: "supervisor", Value: a.token})
	req.Header.Set("Origin", h.origin)
	req.Header.Set("X-CSRF-Token", a.csrf)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", `"`+ifMatch+`"`)
	}
	w := httptest.NewRecorder()
	started := time.Now()
	h.handler.ServeHTTP(w, req)
	elapsed := time.Since(started)
	for _, status := range accepted {
		if w.Code == status {
			if out == nil {
				return w.Code, elapsed, nil
			}
			return w.Code, elapsed, json.Unmarshal(w.Body.Bytes(), out)
		}
	}
	var problem struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &problem)
	return w.Code, elapsed, fmt.Errorf("%s %s: HTTP %d %s", method, routeOf(path), w.Code, problem.Code)
}

// routeOf replaces identifiers with placeholders so errors aggregate by route.
func routeOf(path string) string {
	parts := strings.Split(strings.SplitN(path, "?", 2)[0], "/")
	for i, p := range parts {
		if _, err := uuid.Parse(p); err == nil {
			parts[i] = "{id}"
		}
	}
	return strings.Join(parts, "/")
}

func (h *onboardingHarness) countError(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.errorsByStatus[err.Error()]++
}

func (h *onboardingHarness) session(subject string) (loadActor, error) {
	secret := make([]byte, 64)
	if _, err := h.rng.Read(secret); err != nil {
		return loadActor{}, err
	}
	a := loadActor{subject: subject, token: hex.EncodeToString(secret[:32]), csrf: hex.EncodeToString(secret[32:])}
	sum := sha256.Sum256([]byte(a.token))
	_, err := h.admin.ExecContext(h.ctx, `INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '4 hours',now())`,
		hex.EncodeToString(sum[:]), subject, h.issuer, a.csrf)
	return a, err
}

// supervisorBinary returns a prebuilt binary or builds ./cmd/supervisor from this checkout.
func supervisorBinary(ctx context.Context, prebuilt, dir string) (string, error) {
	if prebuilt != "" {
		return prebuilt, nil
	}
	gomod, err := exec.CommandContext(ctx, "go", "env", "GOMOD").Output()
	if err != nil || strings.TrimSpace(string(gomod)) == "" || strings.TrimSpace(string(gomod)) == os.DevNull {
		return "", errors.New("cannot locate the module to build cmd/supervisor (set SUPERVISOR_LOAD_SUPERVISOR_BIN)")
	}
	bin := filepath.Join(dir, "supervisor")
	build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", bin, "./cmd/supervisor")
	build.Dir = filepath.Dir(strings.TrimSpace(string(gomod)))
	if output, err := build.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build cmd/supervisor: %v: %s", err, lastLine(output))
	}
	return bin, nil
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	line := lines[len(lines)-1]
	if len(line) > 200 {
		line = line[:200]
	}
	return line
}

// provisionTenant runs the real `supervisor provision` command and returns its tenant and site.
func provisionTenant(ctx context.Context, bin, provisioningURL, dir string, i int, requestID, ownerSubject string) (string, string, time.Duration, error) {
	cmd := exec.CommandContext(ctx, bin, "provision", "--tenant-name", fmt.Sprintf("Onboarding load tenant %d", i+1), "--site-name", "Onboarding load site 1",
		"--timezone", "UTC", "--owner-issuer", "https://load.invalid/realms/onboarding", "--owner-subject", ownerSubject,
		"--operator", "supervisor-load onboarding gate", "--request-id", requestID, "--output", filepath.Join(dir, fmt.Sprintf("owner-%d.token", i)))
	// The child sees only the provisioning DSN and the local environment the gate itself requires
	// (a loopback DSN without verify-full is refused in production, WIS-699); the DSN never reaches
	// the report or the logs.
	cmd.Env = []string{"SUPERVISOR_PROVISIONING_URL=" + provisioningURL, "SUPERVISOR_ENVIRONMENT=local", "HOME=" + dir}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	started := time.Now()
	err := cmd.Run()
	elapsed := time.Since(started)
	if err != nil {
		return "", "", elapsed, fmt.Errorf("supervisor provision tenant %d: %v: %s", i+1, err, lastLine(stderr.Bytes()))
	}
	var tenant, site string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if v, ok := strings.CutPrefix(line, "tenant_id="); ok {
			tenant = v
		} else if v, ok := strings.CutPrefix(line, "site_id="); ok {
			site = v
		}
	}
	if uuid.Validate(tenant) != nil || uuid.Validate(site) != nil {
		return "", "", elapsed, fmt.Errorf("supervisor provision tenant %d printed no tenant and site", i+1)
	}
	return tenant, site, elapsed, nil
}

// migrationError keeps BLOCKED for a missing or rejected credential or an unreachable
// server; any other migration error is a product failure, reported redacted (code and
// fixed message only, never a DSN).
func migrationError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		if strings.HasPrefix(pg.Code, "28") {
			return blockedError{fmt.Errorf("supervisor_migrator authentication failed (SQLSTATE %s); check SUPERVISOR_MIGRATION_PASSWORD", pg.Code)}
		}
		return fmt.Errorf("migrate disposable database: SQLSTATE %s: %s", pg.Code, pg.Message)
	}
	var connect *pgconn.ConnectError
	if errors.As(err, &connect) {
		return blockedError{errors.New("supervisor_migrator cannot connect to the disposable database")}
	}
	return fmt.Errorf("migrate disposable database: %T", err)
}

func withDatabase(raw, database string, user *url.Userinfo) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("invalid database URL")
	}
	u.Path = "/" + database
	if user != nil {
		u.User = user
	}
	return u.String(), nil
}

func qualifyOnboarding(ctx context.Context, m OnboardingManifest, adminURL, runtimeURL, migrationPassword, prebuilt string, logw io.Writer, opts onboardingOptions) (onboardingOutcome, error) {
	out := onboardingOutcome{notes: []string{
		"Intake and station writes both take the site row lock, so one site's writes serialize; concurrency is across sites.",
		"Max RSS is this process (load client, in-process API handler and outbox consumer), not PostgreSQL or the provision children.",
	}}
	logf := func(format string, a ...any) { fmt.Fprintf(logw, "supervisor-load: "+format+"\n", a...) }
	control, err := sql.Open("pgx", adminURL)
	if err != nil {
		return out, blockedError{errors.New("admin database URL is invalid")}
	}
	defer control.Close()
	if err = control.PingContext(ctx); err != nil {
		return out, blockedError{errors.New("admin database unreachable")}
	}
	tmp, err := os.MkdirTemp("", "supervisor-onboarding-load-")
	if err != nil {
		return out, blockedError{err}
	}
	defer os.RemoveAll(tmp) // holds the owner invitation tokens: credentials, removed on exit
	bin, err := supervisorBinary(ctx, prebuilt, tmp)
	if err != nil {
		return out, blockedError{err}
	}
	// Read the binary's provenance now: a binary built here is removed with tmp on return.
	out.supervisorBin, out.binaryBlockers = binaryBlockers(bin, gitOutput("rev-parse", "HEAD"))
	// Disposable database owned by the migration role, migrated by it, as in production.
	database := "onboarding_load_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = control.ExecContext(ctx, `CREATE DATABASE `+database+` OWNER supervisor_migrator`); err != nil {
		return out, blockedError{fmt.Errorf("create disposable database: %v", err)}
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if _, e := control.ExecContext(cleanup, `DROP DATABASE IF EXISTS `+database+` WITH (FORCE)`); e != nil {
			logf("drop %s: %v", database, e)
		}
	}()
	adminDSN, err := withDatabase(adminURL, database, nil)
	if err != nil {
		return out, blockedError{err}
	}
	migratorDSN, err := withDatabase(adminURL, database, url.UserPassword("supervisor_migrator", migrationPassword))
	if err != nil {
		return out, blockedError{err}
	}
	runtimeDSN, err := withDatabase(runtimeURL, database, nil)
	if err != nil {
		return out, blockedError{err}
	}
	admin, _ := sql.Open("pgx", adminDSN)
	defer admin.Close()
	migrator, _ := sql.Open("pgx", migratorDSN)
	defer migrator.Close()
	started := time.Now()
	if err = migrate.Apply(ctx, migrator); err != nil {
		return out, migrationError(err)
	}
	runtimeStore, err := store.New(runtimeDSN)
	if err != nil {
		return out, blockedError{err}
	}
	defer runtimeStore.Close()
	runtimeStore.DB.SetMaxOpenConns(m.Concurrency + 4)
	if err = runtimeStore.DB.PingContext(ctx); err != nil {
		return out, blockedError{errors.New("runtime role cannot connect to the disposable database")}
	}
	// Rule 03: the measured role must not own tables or bypass RLS, or the isolation check is void.
	if err = runtimeStore.VerifyRole(ctx); err != nil {
		return out, blockedError{err}
	}
	if err = admin.QueryRowContext(ctx, `SELECT current_setting('server_version')`).Scan(&out.postgresVersion); err != nil {
		return out, err
	}
	// The PostgreSQL container's real limits, read by the server itself (cgroup v2); a
	// missing or unreadable file is recorded and can never match the reference host.
	var cpuMax, memoryMax sql.NullString
	_ = admin.QueryRowContext(ctx, `SELECT pg_read_file('/sys/fs/cgroup/cpu.max', true), pg_read_file('/sys/fs/cgroup/memory.max', true)`).Scan(&cpuMax, &memoryMax)
	out.postgresCPUs, out.postgresMemory = "unreadable", "unreadable"
	if cpuMax.Valid {
		out.postgresCPUs = cgroupCPUs(cpuMax.String)
	}
	if memoryMax.Valid {
		out.postgresMemory = strings.TrimSpace(memoryMax.String)
	}

	h := &onboardingHarness{ctx: ctx, admin: admin, migrator: migrator, runtime: runtimeStore, issuer: "http://127.0.0.1:9/realms/onboarding-load", origin: "http://127.0.0.1:9",
		rng: rand.New(rand.NewSource(m.Seed)), errorsByStatus: map[string]int{}}
	auth, err := identity.New(ctx, identity.Config{Issuer: h.issuer, ClientID: "supervisor", ClientSecret: "synthetic-only", RedirectURI: h.origin + "/auth/callback"}, runtimeStore.DB, nil)
	if err != nil {
		return out, blockedError{fmt.Errorf("identity service: %v", err)}
	}
	handler, drain := httpapi.NewWithShutdown(apiconfig.Config{Environment: "local", WebDir: os.DevNull}, runtimeStore, auth)
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = drain(stop)
	}()
	h.handler = handler

	// 1. Provision every tenant through the real command.
	res := &out.result
	provisionStarted := time.Now()
	var provisionTimes []time.Duration
	var sites []*loadSite
	tenants := make([]string, 0, m.Tenants)
	for i := 0; i < m.Tenants; i++ {
		tenant, first, elapsed, err := provisionTenant(ctx, bin, migratorDSN, tmp, i, h.id(), h.id())
		if err != nil {
			return out, err
		}
		provisionTimes = append(provisionTimes, elapsed)
		tenants = append(tenants, tenant)
		siteIDs := []string{first}
		for j := 1; j < m.SitesPerTenant; j++ {
			siteIDs = append(siteIDs, h.id())
		}
		if err = h.addSites(tenant, siteIDs[1:]); err != nil {
			return out, err
		}
		actors := map[string]loadActor{}
		for _, role := range []struct{ name, role string }{{"owner", "owner"}, {"lead", "supervisor"}, {"approver", "supervisor"}, {"operator", "operator"}} {
			a, err := h.session(h.id())
			if err != nil {
				return out, fmt.Errorf("synthetic session: %w", err)
			}
			for _, s := range siteIDs {
				if _, err = admin.ExecContext(ctx, `INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, tenant, s, h.issuer, a.subject, role.role); err != nil {
					return out, fmt.Errorf("synthetic membership: %w", err)
				}
			}
			actors[role.name] = a
		}
		for _, s := range siteIDs {
			sites = append(sites, &loadSite{tenant: tenant, site: s, actors: actors, nextSequence: 1})
		}
	}
	res.Tenants, res.Sites, res.Provision = m.Tenants, len(sites), summarize(provisionTimes)
	res.ProvisionSeconds = time.Since(provisionStarted).Seconds()

	// 2. Configure every site through the API (not measured).
	logf("configuring %d sites through the API", len(sites))
	for _, s := range sites {
		if err = h.configureSite(s, m.WorkstationsPerSite); err != nil {
			return out, fmt.Errorf("configure site: %w", err)
		}
	}
	res.SetupSeconds = time.Since(started).Seconds()
	if opts.afterSetup != nil {
		restore, err := opts.afterSetup(ctx, admin)
		if err != nil {
			return out, fmt.Errorf("test setup hook: %w", err)
		}
		defer func() {
			if e := restore(); e != nil {
				logf("test restore hook: %v", e)
			}
		}()
	}

	// 3. Measured phase: per-site streams, outbox consumer and isolation checks in parallel.
	logf("measuring %d intake batches and %d station writes over %d sites, concurrency %d", m.sites()*m.IntakeBatchesPerSite, m.sites()*m.StationRecordsPerSite, len(sites), m.Concurrency)
	var intake, station []time.Duration
	var mu sync.Mutex
	queue := make(chan *loadSite, len(sites))
	for _, s := range sites {
		queue <- s
	}
	close(queue)
	producers := sync.WaitGroup{}
	measureStarted := time.Now()
	for w := 0; w < m.Concurrency; w++ {
		producers.Add(1)
		go func() {
			defer producers.Done()
			for s := range queue {
				i, st := h.drive(s, m)
				mu.Lock()
				intake, station = append(intake, i...), append(station, st...)
				mu.Unlock()
			}
		}()
	}
	done := make(chan struct{})
	consumer := make(chan error, 1)
	go func() { consumer <- h.consume(sites, tenants, done, res) }()
	producers.Wait()
	res.MeasuredSeconds = time.Since(measureStarted).Seconds()
	close(done)
	if err = <-consumer; err != nil {
		return out, err
	}

	res.Intake, res.StationWrites = summarize(intake), summarize(station)
	res.EventsExpected = int64(m.sites() * m.IntakeBatchesPerSite * m.EventsPerBatch)
	for _, s := range sites {
		res.EventsIn202 += s.eventsIn202
	}
	if err = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.operational_event_records WHERE quarantine_reason IS NOT NULL`).Scan(&res.EventsQuarantine); err != nil {
		return out, err
	}
	for key, n := range h.errorsByStatus {
		if strings.Contains(key, "/batches") {
			res.IntakeErrors += n
		} else {
			res.StationErrors += n
		}
	}
	res.ErrorsByStatus = h.errorsByStatus
	if attempts := m.sites() * (m.IntakeBatchesPerSite + m.StationRecordsPerSite); attempts > 0 {
		res.ErrorRate = float64(res.IntakeErrors+res.StationErrors) / float64(attempts)
	}
	var usage syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) == nil {
		res.MaxRSSMiB = maxRSSMiB(int64(usage.Maxrss), runtime.GOOS)
	}
	out.probes, err = h.probes(sites, m)
	return out, err
}

// addSites creates a tenant's further sites as the provisioning role (provision creates one).
func (h *onboardingHarness) addSites(tenant string, siteIDs []string) error {
	if len(siteIDs) == 0 {
		return nil
	}
	tx, err := h.migrator.BeginTx(h.ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(h.ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		return err
	}
	for i, s := range siteIDs {
		if _, err = tx.ExecContext(h.ctx, `INSERT INTO supervisor.sites(tenant_id,id,name,timezone) VALUES($1,$2,$3,'UTC')`, tenant, s, fmt.Sprintf("Onboarding load site %d", i+2)); err != nil {
			return fmt.Errorf("add site: %w", err)
		}
	}
	return tx.Commit()
}

// configureSite creates, through the API, everything intake and station writes need.
func (h *onboardingHarness) configureSite(s *loadSite, workstations int) error {
	owner, lead, approver, operator := s.actors["owner"], s.actors["lead"], s.actors["approver"], s.actors["operator"]
	call := func(a loadActor, method, path, key, ifMatch string, body, out any, accepted ...int) error {
		_, _, err := h.call(a, s.site, method, path, key, ifMatch, body, out, accepted...)
		return err
	}
	// Operational source, approved by a second member (rule 05).
	s.source = h.id()
	cfg := operational.Config{SourceID: s.source, Type: "event_api", AuthorityScope: "onboarding-load", Enabled: true, AccessPolicyRevision: 1, FreshnessSeconds: 300, ClockSkewSeconds: 30}
	if err := call(lead, "POST", "/operational-sources", h.id(), "0", cfg, nil, 200, 201); err != nil {
		return err
	}
	if err := call(approver, "POST", "/operational-sources/"+s.source+"/reviews", h.id(), "1", operational.ReviewInput{Verdict: "approved", Reason: "Onboarding load source verified"}, nil, 200, 201); err != nil {
		return err
	}
	// Instruction document designated authoritative and current by the owner.
	text := "Synthetic onboarding load instruction: load blank, cycle press, record good and reject counts."
	document := h.id()
	if err := call(owner, "POST", "/sources", h.id(), "0", map[string]string{"source_id": document, "kind": "document", "title": "Onboarding load SOP", "text": text}, nil, 200, 201); err != nil {
		return err
	}
	var authority struct {
		DecisionID string `json:"decision_id"`
	}
	if err := call(owner, "POST", "/sources/"+document+"/authority-decisions", h.id(), "0",
		map[string]any{"source_revision": 1, "expected_source_state_revision": 1, "verdict": "designate", "reason": "Owner designated the load SOP"}, &authority, 200, 201); err != nil {
		return err
	}
	if err := call(owner, "POST", "/sources/"+document+"/currentness-decisions", h.id(), "0",
		map[string]any{"source_revision": 1, "expected_source_state_revision": 1, "authority_decision_id": authority.DecisionID, "authority_revision": 1, "verdict": "confirm_current", "reason": "Owner confirms the SOP is current"}, nil, 200, 201); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(text))
	sha := hex.EncodeToString(sum[:])
	line := h.id()
	profileFields := []workstation.ProfileFieldV1{{Name: "good_units", Label: "Good quantity", Kind: "decimal", Required: true, Unit: "count"},
		{Name: "reject_units", Label: "Reject quantity", Kind: "decimal", Required: true, Unit: "count"}}
	fields := []workstation.FieldV1{}
	for _, f := range profileFields {
		fields = append(fields, workstation.FieldV1{Name: f.Name, Label: f.Label, Kind: f.Kind, Required: f.Required, Unit: f.Unit})
	}
	assignments := []workstation.AssignmentV1{}
	for i := 0; i < workstations; i++ {
		ws := h.id()
		profile := workstation.StationProfileBodyV1{SchemaVersion: 1, WorkstationID: ws, LineID: line, DisplayName: fmt.Sprintf("station-%02d", i+1), Template: "production", Fields: profileFields}
		if err := call(lead, "POST", "/workstations/"+ws+"/profile-revisions", h.id(), "0", profile, nil, 200, 201); err != nil {
			return err
		}
		order := workstation.JobOrderBodyV1{SchemaVersion: 1, OrderID: h.id(), OperationID: h.id(), ItemCode: fmt.Sprintf("LOAD-%02d", i+1), TargetQuantity: 1000,
			TargetCycleTimeSeconds: "30", Priority: 2, InstructionDocumentID: document, InstructionRevision: 1, InstructionSHA256: sha, Template: "production", Fields: fields}
		var job workstation.JobOrderV1
		if err := call(lead, "POST", "/job-orders", h.id(), "", order, &job, 200, 201); err != nil {
			return err
		}
		assignments = append(assignments, workstation.AssignmentV1{WorkstationID: ws, JobOrderID: job.ID, JobOrderRevision: 1, OrderID: order.OrderID, OperationID: order.OperationID,
			ItemCode: order.ItemCode, TargetQuantity: order.TargetQuantity, TargetCycleTimeSeconds: "30", InstructionDocumentID: document, InstructionRevision: 1, InstructionSHA256: sha,
			Template: "production", Fields: fields})
		s.stations = append(s.stations, loadStation{workstation: ws, jobOrder: job.ID})
	}
	starts := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	var plan workstation.ShiftPlanV1
	if err := call(lead, "POST", "/shift-plans", h.id(), "", workstation.ShiftPlanBodyV1{SchemaVersion: 1, LineID: line, StartsAt: starts.Format(time.RFC3339),
		EndsAt: starts.Add(12 * time.Hour).Format(time.RFC3339), Timezone: "UTC", Assignments: assignments}, &plan, 200, 201); err != nil {
		return err
	}
	if err := call(approver, "POST", "/shift-plans/"+plan.ID+"/approvals", h.id(), strconv.FormatInt(plan.StateRevision, 10),
		workstation.ShiftPlanApproveInputV1{CanonicalPayloadSHA256: plan.Revisions[plan.CurrentRevision-1].CanonicalPayloadSHA256}, &plan, 200); err != nil {
		return err
	}
	// Records need a started shift run.
	if err := call(lead, "POST", "/shift-plans/"+plan.ID+"/run", h.id(), strconv.FormatInt(plan.StateRevision, 10),
		workstation.ShiftRunStartInputV1{SchemaVersion: 1, PlanRevision: *plan.ApprovedRevision}, nil, 201); err != nil {
		return err
	}
	// Event times follow the approval; the job is acknowledged and started before measuring.
	at := time.Now().UTC().Truncate(time.Millisecond).Add(time.Second)
	for i := range s.stations {
		st := &s.stations[i]
		if err := call(lead, "POST", "/workstations/"+st.workstation+"/authorizations", h.id(), "0",
			workstation.StationAuthorizationInputV1{GranteeIssuer: h.issuer, GranteeSubject: operator.subject, Action: workstation.AuthorizationGrant}, nil, 200, 201); err != nil {
			return err
		}
		for j, kind := range []workstation.RecordKind{workstation.KindAcknowledged, workstation.KindStarted} {
			st.lastAt = at.Add(time.Duration(j+1) * time.Millisecond)
			body := workstation.StationRecordBodyV1{SchemaVersion: 1, Kind: kind, WorkstationID: st.workstation, JobOrderID: st.jobOrder, JobOrderRevision: 1, EventAt: st.lastAt.Format(time.RFC3339Nano)}
			if err := call(operator, "POST", "/workstations/"+st.workstation+"/records", h.id(), "", body, nil, 201); err != nil {
				return err
			}
		}
	}
	return nil
}

// drive runs one site's measured stream: intake batches and quantity records interleaved.
func (h *onboardingHarness) drive(s *loadSite, m OnboardingManifest) (intake, station []time.Duration) {
	operator := s.actors["operator"]
	h.mu.Lock()
	keys := make([]string, 0, m.IntakeBatchesPerSite+m.StationRecordsPerSite)
	quantities := make([][2]int64, m.StationRecordsPerSite)
	for i := 0; i < cap(keys); i++ {
		keys = append(keys, h.id())
	}
	for i := range quantities {
		quantities[i] = [2]int64{int64(h.rng.Intn(50)), int64(h.rng.Intn(4))}
	}
	h.mu.Unlock()
	payload := json.RawMessage(`{"state":"running"}`)
	for i := 0; i < max(m.IntakeBatchesPerSite, m.StationRecordsPerSite); i++ {
		if i < m.IntakeBatchesPerSite {
			batch := ingestion.Batch{SchemaVersion: 1, SourceID: s.source, SourceRevision: 1, MappingRevision: 1}
			now := time.Now().UTC().Truncate(time.Microsecond)
			for e := 0; e < m.EventsPerBatch; e++ {
				seq := s.nextSequence + int64(e)
				batch.Events = append(batch.Events, ingestion.Event{ExternalKey: "load-" + strconv.FormatInt(seq, 10), Revision: 1, Kind: "machine_state", SourceSequence: seq, EventAt: now, Payload: payload})
			}
			var accepted ingestion.Result
			_, elapsed, err := h.call(operator, s.site, "POST", "/operational-sources/"+s.source+"/batches", keys[i], "", batch, &accepted, 202)
			intake = append(intake, elapsed)
			if err == nil && (accepted.Replayed || accepted.EventCount != m.EventsPerBatch) {
				err = fmt.Errorf("POST /operational-sources/{id}/batches: 202 with %d events (replayed %v)", accepted.EventCount, accepted.Replayed)
			}
			if err != nil {
				h.countError(err)
			} else {
				s.eventsIn202 += int64(accepted.EventCount)
				s.nextSequence += int64(m.EventsPerBatch) // only accepted batches advance the stream
			}
		}
		if i < m.StationRecordsPerSite {
			st := &s.stations[i%len(s.stations)]
			at := time.Now().UTC().Truncate(time.Microsecond)
			if !at.After(st.lastAt) {
				at = st.lastAt.Add(time.Millisecond)
			}
			good, reject := quantities[i][0], quantities[i][1]
			body := workstation.StationRecordBodyV1{SchemaVersion: 1, Kind: workstation.KindQuantity, WorkstationID: st.workstation, JobOrderID: st.jobOrder, JobOrderRevision: 1,
				EventAt: at.Format(time.RFC3339Nano), GoodQuantity: &good, RejectQuantity: &reject}
			_, elapsed, err := h.call(operator, s.site, "POST", "/workstations/"+st.workstation+"/records", keys[m.IntakeBatchesPerSite+i], "", body, nil, 201)
			station = append(station, elapsed)
			if err != nil {
				h.countError(err)
			} else {
				st.lastAt = at
			}
		}
	}
	return intake, station
}

// consume folds the outbox while producers run, samples the lag, checks tenant isolation
// as the runtime role, and after done drains until the lag is zero or the deadline passes.
func (h *onboardingHarness) consume(sites []*loadSite, tenants []string, done <-chan struct{}, res *OnboardingResult) error {
	fold := ingestion.New(h.runtime.DB)
	lag := func() (int64, error) {
		var n int64
		err := h.admin.QueryRowContext(h.ctx, `SELECT coalesce(sum(q.last_sequence-coalesce(p.processed_sequence,0)),0)::bigint FROM supervisor.operational_event_sequence_heads q
 LEFT JOIN supervisor.operational_source_progress_heads p ON (p.tenant_id,p.site_id,p.source_id)=(q.tenant_id,q.site_id,q.source_id)`).Scan(&n)
		return n, err
	}
	var deadline <-chan time.Time
	finished := false
	for pass := 0; ; pass++ {
		if !finished {
			select {
			case <-done:
				finished, deadline = true, time.After(2*time.Minute)
			default:
			}
		}
		depth, err := lag()
		if err != nil {
			return fmt.Errorf("sample queue depth: %w", err)
		}
		res.QueueSamples++
		res.QueueDepthPeak = max(res.QueueDepthPeak, depth)
		if finished && depth == 0 {
			res.QueueResidual = 0
			break
		}
		if pass%5 == 0 {
			rows, err := h.foreignRows(tenants[pass/5%len(tenants)])
			if err != nil {
				return err
			}
			res.CrossTenantRows = max(res.CrossTenantRows, rows)
			res.IsolationChecks++
		}
		folded := 0
		for _, s := range sites {
			for {
				n, err := fold.Fold(h.ctx, ingestion.SourceRef{TenantID: s.tenant, SiteID: s.site, SourceID: s.source}, 500)
				if errors.Is(err, ingestion.ErrNotFound) {
					break // no batch accepted yet for this source
				}
				if err != nil {
					return fmt.Errorf("fold outbox: %w", err)
				}
				folded += n
				res.EventsFolded += int64(n)
				if n == 0 {
					break
				}
			}
		}
		if finished {
			select {
			case <-deadline:
				res.QueueResidual = depth
				return nil
			default:
			}
		}
		if folded == 0 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	// Final isolation pass over every tenant once all writes are durable.
	for _, t := range tenants {
		rows, err := h.foreignRows(t)
		if err != nil {
			return err
		}
		res.CrossTenantRows = max(res.CrossTenantRows, rows)
		res.IsolationChecks++
	}
	return h.admin.QueryRowContext(h.ctx, `SELECT coalesce(sum(last_sequence),0)::bigint FROM supervisor.operational_event_sequence_heads`).Scan(&res.EventsAccepted)
}

// isolationTables are the tenant-owned tables this run writes, with their tenant column.
var isolationTables = [][2]string{{"tenants", "id"}, {"sites", "tenant_id"}, {"memberships", "tenant_id"}, {"operational_event_batches", "tenant_id"},
	{"operational_event_records", "tenant_id"}, {"operational_event_outbox", "tenant_id"}, {"station_records", "tenant_id"}, {"station_record_receipts", "tenant_id"}}

// foreignRows counts, as the runtime role inside tenant's context, rows of any other tenant.
func (h *onboardingHarness) foreignRows(tenant string) (int64, error) {
	tx, err := h.runtime.DB.BeginTx(h.ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(h.ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		return 0, err
	}
	var total int64
	for _, t := range isolationTables {
		var n int64
		if err = tx.QueryRowContext(h.ctx, `SELECT count(*) FROM supervisor.`+t[0]+` WHERE `+t[1]+`<>$1`, tenant).Scan(&n); err != nil {
			return 0, fmt.Errorf("isolation check on %s: %w", t[0], err)
		}
		total += n
	}
	return total, nil
}

// probes prove the isolation check is not vacuous and that the API refuses another tenant's site.
func (h *onboardingHarness) probes(sites []*loadSite, m OnboardingManifest) ([]Probe, error) {
	var probes []Probe
	first, foreign := sites[0], sites[len(sites)-1]
	// Every tenant's own rows are visible to it as the runtime role, and other tenants' rows exist.
	var all int64
	if err := h.admin.QueryRowContext(h.ctx, `SELECT count(*) FROM supervisor.station_records`).Scan(&all); err != nil {
		return nil, err
	}
	tx, err := h.runtime.DB.BeginTx(h.ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	var own int64
	_, err = tx.ExecContext(h.ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, first.tenant)
	if err == nil {
		err = tx.QueryRowContext(h.ctx, `SELECT count(*) FROM supervisor.station_records`).Scan(&own)
	}
	_ = tx.Rollback()
	if err != nil {
		return nil, err
	}
	perTenant := int64(m.SitesPerTenant * (m.StationRecordsPerSite + 2*m.WorkstationsPerSite))
	probes = append(probes, Probe{Name: "runtime role sees exactly its tenant's station records", Passed: own == perTenant && all == perTenant*int64(m.Tenants),
		Outcome: fmt.Sprintf("tenant sees %d, expected %d; all tenants hold %d", own, perTenant, all), Unauthorized: own > perTenant})
	// Without tenant context the runtime role sees nothing.
	var bare int64
	if err = h.runtime.DB.QueryRowContext(h.ctx, `SELECT count(*) FROM supervisor.operational_event_records`).Scan(&bare); err != nil {
		return nil, err
	}
	probes = append(probes, Probe{Name: "runtime role without tenant context sees no event rows", Passed: bare == 0, Outcome: fmt.Sprintf("%d rows", bare), Unauthorized: bare > 0})
	// API: a member of one tenant reading another tenant's source gets the non-disclosing 404,
	// while the same read in its own site succeeds (positive control).
	operator := first.actors["operator"]
	ownStatus, _, _ := h.call(operator, first.site, "GET", "/operational-sources/"+first.source, "", "", nil, nil, 200)
	foreignStatus, _, _ := h.call(operator, foreign.site, "GET", "/operational-sources/"+foreign.source, "", "", nil, nil, 404)
	probes = append(probes, Probe{Name: "cross-tenant source read is a non-disclosing 404", Passed: ownStatus == 200 && foreignStatus == 404,
		Outcome: fmt.Sprintf("own site %d, other tenant %d", ownStatus, foreignStatus), Unauthorized: foreignStatus == 200})
	return probes, nil
}

// maxRSSMiB converts getrusage ru_maxrss to MiB. Its unit is platform-defined: bytes on darwin,
// KiB on Linux (WIS-638: dividing a darwin value by 1024 overstated peak memory 1024 times).
func maxRSSMiB(maxrss int64, goos string) float64 {
	if goos == "darwin" {
		return float64(maxrss) / (1024 * 1024)
	}
	return float64(maxrss) / 1024
}
