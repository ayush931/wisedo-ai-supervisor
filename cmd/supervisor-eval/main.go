// Command supervisor-eval is the WIS-280 evaluation harness (`make supervisor-eval MODE=...`).
// It loads the frozen manifest (evals/supervisor/manifest.json) before running anything, builds
// each case through the real WIS-272/273 path in a disposable database, runs it through the
// WIS-275 runtime and judges what the database holds.
//
// MODE=deterministic scripts the model with the deterministic-fixture provider, runs once per
// case, labels every line "DETERMINISTIC — not actual-model evidence" and can never qualify.
// MODE=actual requires a pinned actual provider (agent.RequireActual) and runs each eligible case
// three times. Every run also executes the gate controls, which must FAIL.
//
// Exit codes: 0 deterministic pass or actual qualified, 1 FAIL, 2 BLOCKED (missing mode,
// manifest, provider, database or other prerequisite, or an actual run that cannot qualify).
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/retrieval"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
)

type RunResult struct {
	Run      int      `json:"run"`
	Error    string   `json:"error,omitempty"` // a harness error: the run produced no observation
	Label    string   `json:"label,omitempty"`
	Observed Observed `json:"observed"`
	// Knowledge is an onboarding or chat run (WIS-640); Observed stays empty for those.
	Knowledge *KnowledgeObserved `json:"knowledge,omitempty"`
	// OCR is what an OCR run left behind beyond Knowledge (WIS-625).
	Correction *CorrectionObserved `json:"correction,omitempty"`
	OCR        *OCRObserved        `json:"ocr,omitempty"`
	Failures   []string            `json:"failures"`
	Breaches   []string            `json:"breaches"`
}

type CaseResult struct {
	CaseID    string      `json:"case_id"`
	Category  string      `json:"category"`
	Partition string      `json:"partition"`
	Label     string      `json:"label,omitempty"`
	Runs      []RunResult `json:"runs"`
	Failures  []string    `json:"failures"`
	Breaches  []string    `json:"breaches"`
	Errors    []string    `json:"errors"`
}

type ControlResult struct {
	CaseID          string   `json:"case_id"`
	ExpectedFailure string   `json:"expected_failure"`
	Outcome         string   `json:"outcome"`           // FAIL (for ExpectedFailure) is the required outcome
	Problem         string   `json:"problem,omitempty"` // why this control does not prove the gate
	Reasons         []string `json:"reasons"`
	Observed        Observed `json:"observed"`
}

type Report struct {
	SchemaVersion  int                       `json:"schema_version"`
	CorrectionSet  *CorrectionSet            `json:"correction_set,omitempty"`
	Issue          string                    `json:"issue"`
	Label          string                    `json:"label,omitempty"`
	Mode           string                    `json:"mode"`
	Suites         []string                  `json:"suites"`
	Result         string                    `json:"result"`
	Qualified      bool                      `json:"qualified"`
	EvidenceState  string                    `json:"evidence_state"`
	Failures       []string                  `json:"failures"`
	Blockers       []string                  `json:"blockers"`
	GitSHA         string                    `json:"git_sha"`
	GitDirty       bool                      `json:"git_dirty"`
	Command        string                    `json:"command"`
	Argv           []string                  `json:"argv"`
	StartedAt      string                    `json:"started_at"`
	ManifestPath   string                    `json:"manifest_path"`
	ManifestSHA256 string                    `json:"manifest_file_sha256"`
	CorpusSHA256   string                    `json:"corpus_sha256"`
	Corpus         Corpus                    `json:"corpus"`
	Environment    map[string]string         `json:"environment"`
	RunsPerCase    int                       `json:"runs_per_case"`
	Controls       []ControlResult           `json:"gate_controls"`
	Cases          []CaseResult              `json:"cases"`
	Confusion      map[string]map[string]int `json:"confusion_matrix"`
	ErroredRuns    int                       `json:"errored_runs"`
	Tokens         Dist                      `json:"tokens_per_run"`
	LatencyMs      Dist                      `json:"latency_ms_per_run"`
	Measures       []Measure                 `json:"measures"`
	Retrieval      *RetrievalReport          `json:"retrieval,omitempty"`
	Notes          []string                  `json:"notes"`
}

// blockedError marks a missing prerequisite (rule 09: BLOCKED, never a green fallback).
type blockedError struct{ error }

func gitOutput(args ...string) string { return gitIn("", args...) }

func gitIn(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// gitDirty reports tracked changes anywhere, and untracked files that can change the binary or
// the corpus (Go sources, module files, evals/). An unreadable status counts as dirty.
func gitDirty(dir string) bool {
	tracked := gitIn(dir, "status", "--porcelain", "--untracked-files=no")
	untracked := gitIn(dir, "status", "--porcelain", "--untracked-files=all", "--", "*.go", "go.mod", "go.sum", "evals/")
	return tracked != "" || untracked != ""
}

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("supervisor-eval", flag.ContinueOnError)
	flags.SetOutput(stderr)
	mode := flags.String("mode", "", "deterministic | actual")
	manifestPath := flags.String("manifest", "evals/supervisor/manifest.json", "frozen eval manifest")
	outDir := flags.String("out", "build/qualification", "report directory")
	printHash := flags.Bool("print-hash", false, "print the computed corpus_sha256 and thresholds digest, then exit")
	correctionSet := flags.String("correction-set", "", "explicit correction tenant/site/snapshot and case IDs (corrections suite only)")
	selector := flags.String("case", getenv("SUPERVISOR_EVAL_CASE"), "suites to run: investigation,onboarding,chat,ocr,chat_synonyms,corrections (empty: all but chat_synonyms)")
	retrievalMode := flags.String("retrieval", RetrievalWord, "chat passage retrieval: word | hybrid | hybrid_rerank (WIS-647; hybrid needs the pinned SUPERVISOR_EMBEDDINGS_* endpoint)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintln(stderr, "BLOCKED: unexpected arguments", flags.Args())
		return 2
	}
	blocked := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "BLOCKED: "+format+"\n", a...)
		return 2
	}
	if *printHash {
		m, sum, err := loadManifest(*manifestPath, false)
		if err != nil {
			return blocked("manifest %s: %v", *manifestPath, err)
		}
		th, _ := thresholdsDigest(m)
		fmt.Fprintf(stdout, "corpus_sha256 %s\nthresholds %s\n", sum, th)
		return 0
	}
	if *mode != ModeDeterministic && *mode != ModeActual {
		return blocked("MODE must be deterministic or actual")
	}
	suites, err := selectSuites(*selector)
	if err != nil {
		return blocked("%v", err)
	}
	if slices.Contains(suites, SuiteCorrections) {
		if len(suites) != 1 {
			return blocked("corrections must run alone against their explicit site case set")
		}
		return runCorrections(*mode, *correctionSet, *outDir, getenv, stdout, stderr)
	}
	if *correctionSet != "" {
		return blocked("-correction-set requires -case corrections")
	}
	rs, err := newRetrievalSetup(context.Background(), *retrievalMode, config.LoadEmbeddings)
	if err != nil {
		return blocked("%v", err)
	}
	// Rule 09: thresholds are frozen and the corpus hash checked before anything runs.
	m, corpusSHA, err := loadManifest(*manifestPath, true)
	if err != nil {
		return blocked("manifest %s: %v", *manifestPath, err)
	}
	raw, _ := os.ReadFile(*manifestPath)
	fileSum := sha256.Sum256(raw)
	env := map[string]string{"go": runtime.Version(), "goos": runtime.GOOS, "goarch": runtime.GOARCH, "num_cpu": fmt.Sprint(runtime.NumCPU()), "provider": agent.ProviderFixture, "model": "fixture-model", "model_revision": "sha256:fixture"}
	var provider, vision agent.Provider
	agentCfg := fixtureAgent()
	if *mode == ModeActual {
		a, err := agentConfig(getenv)
		if err != nil {
			return blocked("actual provider: %v", err)
		}
		// WIS-623: with SUPERVISOR_AGENT_HOSTED_* set, the corpus runs through the hosted route:
		// each case's site has its switch on, so every run is pinned, priced and pseudonymised by
		// the real per-call path. A missing key file, price or allowlisted provider is BLOCKED.
		if a.Hosted.Provider != "" {
			if a.Hosted.APIKeyFile == "" {
				return blocked("hosted provider: SUPERVISOR_AGENT_HOSTED_API_KEY_FILE is required (a file holding the key; never paste it)")
			}
			route, err := agent.NewHostedRoute(a.Hosted, a.Timeout, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now)
			if err != nil {
				return blocked("hosted provider: %v", err)
			}
			agent.SetHostedRoute(route)
			provider = route.Provider
			env["provider"], env["model"], env["model_revision"], env["provider_region"], env["price_version"] = route.Name, route.Model, route.ModelRevision, route.Region, route.PriceVersion
		} else if a.Provider == config.AgentProviderLocal {
			if provider, err = agent.NewLocalModel(a, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now); err != nil {
				return blocked("actual provider: %v", err)
			}
		}
		agentCfg = a
		if err = agent.RequireActual(provider); err != nil {
			return blocked("%v (set SUPERVISOR_AGENT_PROVIDER=local and the pinned SUPERVISOR_AGENT_* settings)", strings.TrimPrefix(err.Error(), "BLOCKED: "))
		}
		if lm, ok := provider.(*agent.LocalModel); ok {
			ctx, cancel := context.WithTimeout(context.Background(), a.Timeout)
			err = lm.Probe(ctx)
			cancel()
			if err != nil {
				return blocked("actual provider probe: %v", err)
			}
			env["provider"], env["model"], env["model_revision"], env["model_base_url"] = lm.Name(), a.Model, a.ModelRevision, a.BaseURL
			if a.ReasoningEffort != "" {
				env["reasoning_effort"] = a.ReasoningEffort // it changes what the model returns: recorded with the run
			}
		}
	}
	if *mode == ModeActual {
		a := agentCfg
		if slices.Contains(suites, SuiteOCR) {
			// OCR reads pages only with a pinned local vision model; without one the suite cannot run.
			vm, err := agent.NewVisionModel(a, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now)
			if err != nil {
				return blocked("ocr suite: %v (set SUPERVISOR_AGENT_PROVIDER=local and SUPERVISOR_AGENT_VISION_MODEL, _VISION_MODEL_REVISION)", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), a.Timeout)
			err = vm.Probe(ctx)
			cancel()
			if err != nil {
				return blocked("ocr suite: vision model probe: %v", err)
			}
			vision = vm
			env["vision_model"], env["vision_model_revision"] = a.VisionModel, a.VisionModelRevision
		}
	}
	adminURL, runtimeURL := getenv("SUPERVISOR_TEST_ADMIN_URL"), getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		return blocked("SUPERVISOR_TEST_ADMIN_URL and SUPERVISOR_TEST_DATABASE_URL are required (real PostgreSQL)")
	}
	r := Report{SchemaVersion: 1, Issue: m.Issue, Mode: *mode, Suites: suites, Measures: []Measure{}, Failures: []string{}, Blockers: []string{}, Argv: os.Args, StartedAt: time.Now().UTC().Format(time.RFC3339),
		ManifestPath: *manifestPath, ManifestSHA256: hex.EncodeToString(fileSum[:]), CorpusSHA256: corpusSHA, Corpus: m.Corpus, Environment: env, RunsPerCase: 1,
		Notes: []string{"simulated_clock anchors the fixture windows; heartbeats, proposal times and the runtime use the wall/database clock (no injectable runtime clock yet)",
			"reviewer labels were authored by engineering from the WIS-275 gate and are not independently reviewed"}}
	if *mode == ModeDeterministic {
		r.Label = Label
	} else {
		r.RunsPerCase = *m.Thresholds.Runs.ActualRunsPerCase
	}
	r.Retrieval = &RetrievalReport{Mode: rs.mode, Labels: map[string]map[string]int{}, AskLatencyMs: map[string]Dist{}}
	env["retrieval"] = rs.mode
	if rs.client != nil {
		// The in-process API reads the process default, as cmd/supervisor sets it.
		retrieval.SetDefault(rs.client)
		defer retrieval.SetDefault(nil)
		e, _ := config.LoadEmbeddings()
		r.Retrieval.Model, r.Retrieval.ModelRevision, r.Retrieval.RerankModel, r.Retrieval.RerankModelRevision = e.Model, e.ModelRevision, e.RerankModel, e.RerankModelRevision
		env["embedding_model"], env["embedding_model_revision"], env["rerank_model"], env["rerank_model_revision"] = e.Model, e.ModelRevision, e.RerankModel, e.RerankModelRevision
	}
	if r.Command = getenv("SUPERVISOR_EVAL_COMMAND"); r.Command == "" {
		r.Command = strings.Join(os.Args, " ")
	}
	r.GitSHA = gitOutput("rev-parse", "HEAD")
	r.GitDirty = gitDirty("")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	err = execute(ctx, &r, m, provider, vision, agentCfg, suites, adminURL, runtimeURL, rs)
	if rs.calls != nil {
		eOK, eFail, rOK, rFail := rs.calls.snapshot()
		r.Retrieval.EmbedCalls, r.Retrieval.RerankCalls = [2]int{eOK, eFail}, [2]int{rOK, rFail}
	}
	var block blockedError
	if errors.As(err, &block) {
		return blocked("%v", err)
	}
	if err != nil {
		r.Failures = append(r.Failures, "run aborted: "+err.Error())
	}
	r.Tokens, r.LatencyMs = runStats(r.Cases)
	for _, c := range r.Controls {
		if c.Problem != "" {
			r.Failures = append(r.Failures, c.Problem)
		}
	}
	for _, c := range r.Cases {
		for _, f := range append(append(slices.Clone(c.Errors), c.Breaches...), c.Failures...) {
			r.Failures = append(r.Failures, c.CaseID+": "+f)
		}
	}
	if *mode == ModeActual {
		if slices.Contains(suites, SuiteInvestigation) {
			r.Blockers = qualificationBlockers(m, r.GitDirty)
		} else if r.GitDirty {
			r.Blockers = append(r.Blockers, "working tree has uncommitted changes")
		}
		r.Blockers = append(r.Blockers, knowledgeBlockers(m, suites)...)
	}
	var exit int
	r.Result, r.Qualified, exit = verdict(*mode, len(r.Failures), r.Blockers)
	r.EvidenceState = evidenceState(*mode, r.Qualified)
	if err = os.MkdirAll(*outDir, 0o755); err != nil {
		return blocked("report directory: %v", err)
	}
	// Every run keeps its own report, failures included.
	path := filepath.Join(*outDir, "supervisor-eval-"+*mode+"-"+r.GitSHA+"-"+time.Now().UTC().Format("20060102T150405Z")+"-"+uuid.NewString()[:8]+".json")
	body, _ := json.MarshalIndent(r, "", "  ")
	if err = os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		return blocked("write report: %v", err)
	}
	for _, line := range append(summaryLines(r), "report "+path) {
		if *mode == ModeDeterministic && !strings.HasPrefix(line, Label) {
			line = Label + ": " + line
		}
		fmt.Fprintln(stdout, line)
	}
	for _, l := range failLines(*mode, r.Failures) {
		fmt.Fprintln(stderr, l)
	}
	return exit
}

// agentConfig reads the pinned model settings. ponytail: duplicates the subset of the
// unexported config.loadAgent this harness needs; an exported loader would replace it.
func agentConfig(getenv func(string) string) (config.Agent, error) {
	a := config.Agent{Provider: getenv("SUPERVISOR_AGENT_PROVIDER"), FixtureScript: getenv("SUPERVISOR_AGENT_FIXTURE_SCRIPT"), BaseURL: getenv("SUPERVISOR_AGENT_BASE_URL"), Model: getenv("SUPERVISOR_AGENT_MODEL"),
		ModelRevision: getenv("SUPERVISOR_AGENT_MODEL_REVISION"), APIKeyFile: getenv("SUPERVISOR_AGENT_API_KEY_FILE"), Timeout: 30 * time.Second,
		VisionModel: getenv("SUPERVISOR_AGENT_VISION_MODEL"), VisionModelRevision: getenv("SUPERVISOR_AGENT_VISION_MODEL_REVISION"), ReasoningEffort: getenv("SUPERVISOR_AGENT_REASONING_EFFORT"),
		Hosted: config.HostedAgent{Provider: getenv("SUPERVISOR_AGENT_HOSTED_PROVIDER"), Model: getenv("SUPERVISOR_AGENT_HOSTED_MODEL"), ModelRevision: getenv("SUPERVISOR_AGENT_HOSTED_MODEL_REVISION"), APIKeyFile: getenv("SUPERVISOR_AGENT_HOSTED_API_KEY_FILE")}}
	if a.FixtureScript != "" && a.Provider != config.AgentProviderFixture {
		return a, errors.New("SUPERVISOR_AGENT_FIXTURE_SCRIPT requires SUPERVISOR_AGENT_PROVIDER=fixture")
	}
	if a.ReasoningEffort != "" && (a.Provider != config.AgentProviderLocal || !slices.Contains([]string{"none", "low", "medium", "high"}, a.ReasoningEffort)) {
		return a, errors.New("SUPERVISOR_AGENT_REASONING_EFFORT must be none, low, medium or high, and requires SUPERVISOR_AGENT_PROVIDER=local")
	}
	if s := getenv("SUPERVISOR_AGENT_ENDPOINT_ALLOWLIST"); s != "" {
		a.EndpointAllowlist = strings.Split(s, ",")
	}
	for _, raw := range strings.Split(getenv("SUPERVISOR_AGENT_ENDPOINT_ADDRS"), ",") {
		if raw = strings.TrimSpace(raw); raw == "" {
			continue
		}
		ap, err := netip.ParseAddrPort(raw)
		if err != nil {
			ip, e := netip.ParseAddr(raw)
			if e != nil {
				return a, errors.New("invalid SUPERVISOR_AGENT_ENDPOINT_ADDRS")
			}
			ap = netip.AddrPortFrom(ip, 0)
		}
		a.EndpointAddrs = append(a.EndpointAddrs, ap)
	}
	if s := getenv("SUPERVISOR_AGENT_TIMEOUT"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d < time.Second || d > 5*time.Minute {
			return a, errors.New("invalid SUPERVISOR_AGENT_TIMEOUT")
		}
		a.Timeout = d
	}
	return a, nil
}

// disposable creates and migrates a throwaway database and opens it as the admin and the runtime
// role; done closes both and drops it.
func disposable(ctx context.Context, adminURL, runtimeURL string) (admin *sql.DB, db *store.Store, done func(), err error) {
	control, err := sql.Open("pgx", adminURL)
	if err != nil {
		return nil, nil, nil, blockedError{fmt.Errorf("admin database: %w", err)}
	}
	if err = control.PingContext(ctx); err != nil {
		control.Close()
		return nil, nil, nil, blockedError{errors.New("admin database unreachable")}
	}
	database := "supervisor_eval_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = control.ExecContext(ctx, `CREATE DATABASE `+database); err != nil {
		control.Close()
		return nil, nil, nil, blockedError{fmt.Errorf("create disposable database: %w", err)}
	}
	done = func() {
		if db != nil {
			db.Close()
		}
		if admin != nil {
			admin.Close()
		}
		c, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_, _ = control.ExecContext(c, `DROP DATABASE IF EXISTS `+database+` WITH (FORCE)`)
		control.Close()
	}
	fail := func(e error) (*sql.DB, *store.Store, func(), error) {
		done()
		return nil, nil, nil, blockedError{e}
	}
	at := func(raw string) (string, error) {
		u, e := url.Parse(raw)
		if e != nil {
			return "", e
		}
		u.Path = "/" + database
		return u.String(), nil
	}
	adminDB, err := at(adminURL)
	if err != nil {
		return fail(err)
	}
	if admin, err = sql.Open("pgx", adminDB); err != nil {
		return fail(err)
	}
	if err = migrate.Apply(ctx, admin); err != nil {
		return fail(fmt.Errorf("migrate disposable database: %w", err))
	}
	runtimeDB, err := at(runtimeURL)
	if err != nil {
		return fail(err)
	}
	if db, err = store.New(runtimeDB); err != nil {
		return fail(fmt.Errorf("runtime role: %w", err))
	}
	return admin, db, done, nil
}

// execute creates one disposable database, then a fresh tenant/site world per case and run.
func execute(ctx context.Context, r *Report, m Manifest, actual, vision agent.Provider, agentCfg config.Agent, suites []string, adminURL, runtimeURL string, rs *retrievalSetup) error {
	admin, db, done, err := disposable(ctx, adminURL, runtimeURL)
	if err != nil {
		return err
	}
	defer done()
	var pgVersion string
	if err = admin.QueryRowContext(ctx, `SELECT current_setting('server_version')`).Scan(&pgVersion); err != nil {
		return blockedError{err}
	}
	r.Environment["postgres"] = pgVersion
	webDir, err := os.MkdirTemp("", "supervisor-eval-web-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(webDir)
	h := harness{ctx: ctx, control: admin, db: db, webDir: webDir, mode: r.Mode, hosted: r.Mode == ModeActual && agent.CurrentHostedRoute() != nil,
		actual: actual, model: r.Environment["model"], revision: r.Environment["model_revision"], agentCfg: agentCfg, vision: vision, retr: rs}
	defer h.executeKnowledge(r, m, suites)
	if !slices.Contains(suites, SuiteInvestigation) {
		return nil
	}

	// Controls are scripted violations: they always run on the deterministic fixture, even in
	// actual mode, where a well-behaved model would otherwise never commit the violation.
	hc := h
	hc.mode, hc.model, hc.revision = ModeDeterministic, "fixture-model", "sha256:fixture"
	for _, c := range m.Controls {
		res := CaseResult{CaseID: c.CaseID}
		hc.runCase(c, 1, &res)
		cr := ControlResult{CaseID: c.CaseID, ExpectedFailure: c.ExpectedFailure, Reasons: append(append(slices.Clone(res.Errors), res.Breaches...), res.Failures...)}
		cr.Outcome, cr.Problem = controlOutcome(c, res)
		if len(res.Runs) > 0 {
			cr.Observed = res.Runs[0].Observed
		}
		r.Controls = append(r.Controls, cr)
	}
	partition := m.partitionOf()
	var pairs [][2]string
	for _, c := range m.Cases {
		if r.Mode == ModeActual && !c.ActualEligible {
			continue
		}
		res := CaseResult{CaseID: c.CaseID, Category: c.Category, Partition: partition[c.CaseID], Label: r.Label, Failures: []string{}, Breaches: []string{}, Errors: []string{}}
		h.runCase(c, r.RunsPerCase, &res)
		// A case whose world could not be built ran no run at all: it counts as one error.
		if len(res.Runs) == 0 {
			res.Runs = append(res.Runs, RunResult{Run: 1, Error: strings.Join(res.Errors, "; ")})
		}
		for _, run := range res.Runs {
			if run.Error != "" {
				r.ErroredRuns++
				pairs = append(pairs, [2]string{outcomeKey(c.Lifecycle.RunState, c.Lifecycle.OutcomeStatus), "error"})
				continue
			}
			o := run.Observed
			pairs = append(pairs, [2]string{outcomeKey(c.Lifecycle.RunState, c.Lifecycle.OutcomeStatus), outcomeKey(o.State, o.Status)})
		}
		r.Cases = append(r.Cases, res)
	}
	r.Confusion = confusion(pairs)
	return nil
}

type harness struct {
	ctx      context.Context
	control  *sql.DB
	db       *store.Store
	webDir   string
	mode     string
	hosted   bool // WIS-623: runs are pinned to the hosted route
	actual   agent.Provider
	model    string
	revision string
	agentCfg config.Agent    // the settings the in-process API records on onboarding and chat runs
	vision   agent.Provider  // the pinned vision model (WIS-625 ocr suite, actual mode)
	retr     *retrievalSetup // chat passage retrieval (WIS-647); word mode when it has no client
}

// runCase builds the case's world and runs it n times; every failure lands on res.
func (h harness) runCase(c Case, n int, res *CaseResult) {
	fail := func(f string) { res.Failures = append(res.Failures, f) }
	harnessErr := func(f string) { res.Errors = append(res.Errors, FailureHarnessError+": "+f) }
	errRun := func(i int, f string) {
		harnessErr(fmt.Sprintf("run %d: %s", i, f))
		res.Runs = append(res.Runs, RunResult{Run: i, Error: f})
	}
	clock, _ := time.Parse(time.RFC3339, c.SimulatedClock)
	w, err := newWorld(h.ctx, h.control, h.db, h.webDir, clock)
	if err != nil {
		harnessErr("world: " + err.Error())
		return
	}
	if h.hosted {
		// The eval site's owner has switched hosted inference on (seeded, as the world seeds members).
		for _, q := range []string{
			`INSERT INTO supervisor.site_model_hosting_heads(tenant_id,site_id,enabled,dpa_reference,revision) VALUES($1,$2,true,'eval DPA',1)`,
			`INSERT INTO supervisor.site_model_hosting_events(tenant_id,site_id,revision,enabled,dpa_reference,reason,actor_issuer,actor_subject,auth_time) VALUES($1,$2,1,true,'eval DPA','supervisor-eval hosted run','supervisor:eval','eval',now())`,
		} {
			if _, err = h.control.ExecContext(h.ctx, q, w.tenant, w.site); err != nil {
				harnessErr("hosted switch: " + err.Error())
				return
			}
		}
	}
	resolved := Resolved{Permissions: map[string]string{}}
	for sym, perm := range c.SourcePermissions {
		resolved.Permissions[w.sources[sym]] = perm
	}
	for _, ref := range c.ExpectedCitations {
		resolved.Citations = append(resolved.Citations, w.measurement(strings.TrimPrefix(ref, "measurement:")).ID)
	}
	for i := 1; i <= n; i++ {
		rev := w.revision
		if i > 1 {
			// A repeated run needs a new case generation, or it would replay the first run.
			if rev, err = w.fresh(); err != nil {
				errRun(i, "fresh generation: "+err.Error())
				return
			}
		}
		var p agent.Provider = h.actual
		var script *scripted
		if h.mode == ModeDeterministic {
			s, before := scenarios[c.Scenario](w)
			script = &scripted{FixtureProvider: agent.NewFixtureProvider(s...), before: before}
			p = script
		}
		rt := &agent.Runtime{Store: w.store, Provider: p, Tools: &agent.Tools{Store: w.store, Policy: c.AllowedTools}, Slots: agent.NewSlots(2, 8),
			Limits: agent.DefaultLimits(), Model: h.model, ModelRevision: h.revision, Options: agent.LoaderOptions{ModeEnabled: true}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		ref := agent.InvestigateRef{TenantID: w.tenant, SiteID: w.site, CaseID: w.caseID}
		started := time.Now()
		result, err := rt.Investigate(h.ctx, ref, rev)
		elapsed := time.Since(started).Milliseconds()
		if err != nil {
			errRun(i, "investigate: "+err.Error())
			continue
		}
		if script != nil {
			if e := script.Err(); e != nil {
				errRun(i, "scenario hook: "+e.Error())
				continue
			}
		}
		if result.Replayed {
			errRun(i, "replayed an earlier run: not a new sample")
			continue
		}
		if after := afterRun[c.Scenario]; after != nil && h.mode == ModeDeterministic && result.RunID != "" {
			if e := after(w, result.RunID); e != nil {
				errRun(i, "scenario after-run hook: "+e.Error())
				continue
			}
		}
		o, err := w.observe(result)
		if err != nil {
			errRun(i, "read back: "+err.Error())
			continue
		}
		o.LatencyMs = elapsed
		if c.Lifecycle.ReplayMakesNoCall && result.RunID != "" {
			again, e := rt.Investigate(h.ctx, ref, rev)
			calls := o.ProviderCalls
			if after, e2 := w.observe(again); e == nil && e2 == nil {
				calls = after.ProviderCalls
			}
			ok := e == nil && again.Replayed && again.RunID == result.RunID && calls == o.ProviderCalls
			o.ReplayMadeNoCall = &ok
		}
		run := RunResult{Run: i, Observed: o, Breaches: breaches(c, h.mode, resolved, o), Failures: assertions(c, h.mode, resolved, o)}
		if h.mode == ModeDeterministic {
			run.Label = Label
		}
		for _, b := range run.Breaches {
			res.Breaches = append(res.Breaches, fmt.Sprintf("run %d: %s", i, b))
		}
		for _, f := range run.Failures {
			fail(fmt.Sprintf("run %d: %s", i, f))
		}
		res.Runs = append(res.Runs, run)
	}
}

// observe reads one run back as the privileged role.
func (w *world) observe(res agent.Result) (Observed, error) {
	o := Observed{Tenant: w.tenant, Site: w.site, RunID: res.RunID, State: res.State, Status: res.Status, Reasons: []string{}, ToolCodes: []string{}, OKTools: []string{},
		Citations: []string{}, RunRefs: []string{}, AuditedSources: []string{}, Readiness: w.readiness(), SourceRevisions: w.revisions, PlanRevision: w.plan}
	q := func(query string, args ...any) (*sql.Rows, error) {
		return w.control.QueryContext(w.ctx, query, args...)
	}
	collect := func(query string, into *[]string, args ...any) error {
		rows, err := q(query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err = rows.Scan(&s); err != nil {
				return err
			}
			*into = append(*into, s)
		}
		return rows.Err()
	}
	if err := w.control.QueryRowContext(w.ctx, `SELECT count(*) FROM supervisor.evidence_audit WHERE tenant_id=$1 AND actor_issuer=$2`, w.otherTenant, agent.ServiceActor.Issuer).Scan(&o.ForeignAudits); err != nil {
		return o, err
	}
	if err := collect(`SELECT DISTINCT source_id::text FROM supervisor.evidence_audit WHERE tenant_id=$1 AND actor_issuer=$2 ORDER BY 1`, &o.AuditedSources, w.tenant, agent.ServiceActor.Issuer); err != nil {
		return o, err
	}
	if res.RunID == "" {
		return o, nil
	}
	var reasons string
	var body []byte
	if err := w.control.QueryRowContext(w.ctx, `SELECT r.state,COALESCE(r.outcome_status,''),array_to_string(r.outcome_reasons,','),r.artifact_id IS NOT NULL,r.provider_actual,r.trace_id,r.policy_revision,
 (SELECT count(*) FROM supervisor.agent_model_calls m WHERE m.run_id=r.id),
 (SELECT COALESCE(sum(input_tokens),0) FROM supervisor.agent_model_calls m WHERE m.run_id=r.id),(SELECT COALESCE(sum(output_tokens),0) FROM supervisor.agent_model_calls m WHERE m.run_id=r.id),
 (SELECT a.body::text FROM supervisor.agent_artifacts a WHERE a.run_id=r.id AND a.id=r.artifact_id)
 FROM supervisor.agent_runs r WHERE r.id=$1`, res.RunID).Scan(&o.State, &o.Status, &reasons, &o.Artifact, &o.ProviderActual, &o.TraceID, &o.PolicyRevision, &o.ProviderCalls, &o.InputTokens, &o.OutputTokens, &body); err != nil {
		return o, err
	}
	if reasons != "" {
		o.Reasons = strings.Split(reasons, ",")
	}
	if err := collect(`SELECT status||':'||COALESCE(code,'') FROM supervisor.agent_tool_calls WHERE run_id=$1 ORDER BY seq`, &o.ToolCodes, res.RunID); err != nil {
		return o, err
	}
	if err := collect(`SELECT tool FROM supervisor.agent_tool_calls WHERE run_id=$1 AND status='ok' ORDER BY seq`, &o.OKTools, res.RunID); err != nil {
		return o, err
	}
	if err := collect(`SELECT ref_id FROM supervisor.agent_run_refs WHERE run_id=$1 ORDER BY ref_id`, &o.RunRefs, res.RunID); err != nil {
		return o, err
	}
	if body != nil {
		var doc any
		if err := json.Unmarshal(body, &doc); err != nil {
			return o, err
		}
		o.Citations = citedIDs(doc, map[string]bool{})
	}
	return o, nil
}

// citedIDs collects every measurement_id and evidence_ids value in a stored artifact.
func citedIDs(v any, seen map[string]bool) []string {
	var out []string
	add := func(s any) {
		if id, ok := s.(string); ok && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			switch k {
			case "measurement_id":
				add(child)
			case "evidence_ids":
				if list, ok := child.([]any); ok {
					for _, id := range list {
						add(id)
					}
				}
			default:
				out = append(out, citedIDs(child, seen)...)
			}
		}
	case []any:
		for _, child := range t {
			out = append(out, citedIDs(child, seen)...)
		}
	}
	return out
}
