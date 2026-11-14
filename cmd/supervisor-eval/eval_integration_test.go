//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/retrieval"
)

// TestDeterministicSeedCorpus runs the committed seed corpus through the real runtime and a
// disposable database: every case passes, both gate controls FAIL, every line is labelled and
// nothing qualifies (rule 09).
func TestDeterministicSeedCorpus(t *testing.T) {
	if os.Getenv("SUPERVISOR_TEST_ADMIN_URL") == "" || os.Getenv("SUPERVISOR_TEST_DATABASE_URL") == "" {
		t.Fatal("BLOCKED: real PostgreSQL admin and runtime URLs required")
	}
	out := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-mode", "deterministic", "-manifest", committed, "-out", out}, os.Getenv, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout.String(), stderr.String())
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if !strings.HasPrefix(line, Label+": ") {
			t.Errorf("unlabelled line %q", line)
		}
	}
	files, _ := filepath.Glob(filepath.Join(out, "supervisor-eval-deterministic-*.json"))
	if len(files) != 1 {
		t.Fatalf("reports %v", files)
	}
	raw, _ := os.ReadFile(files[0])
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	m, sum, _ := loadManifest(committed, true)
	if r.Qualified || r.Label != Label || !strings.Contains(r.Result, Label) || r.CorpusSHA256 != sum || len(r.Cases) != len(m.Cases)+len(m.Onboarding)+len(m.Chat)+len(m.OCR) || len(r.Failures) != 0 {
		t.Fatalf("report: qualified=%v label=%q result=%q cases=%d failures=%v", r.Qualified, r.Label, r.Result, len(r.Cases), r.Failures)
	}
	started := 0 // runs that started a run: each one is in the token and latency distributions
	for _, c := range r.Cases {
		if k := c.Runs[0].Knowledge; k != nil { // WIS-640 onboarding and chat runs
			started++
			if len(c.Runs) != 1 || k.RunID == "" || k.ProviderActual || c.Runs[0].Label != Label {
				t.Errorf("case %s: %+v", c.CaseID, k)
			}
			// WIS-625 review: an OCR run's job, text and completion metadata reach the report.
			if o := c.Runs[0].OCR; strings.HasPrefix(c.CaseID, "ocr-") && (o == nil || o.JobState == "" || o.JobState == "completed" && o.FinishReason != "stop") {
				t.Errorf("case %s: OCR observation %+v", c.CaseID, o)
			}
			continue
		}
		if c.Runs[0].Observed.RunID != "" {
			started++
		}
		if len(c.Runs) != 1 || c.Runs[0].Observed.RunID == "" && c.Runs[0].Observed.State != "canceled" || c.Runs[0].Observed.ProviderActual || c.Runs[0].Label != Label {
			t.Errorf("case %s: %+v", c.CaseID, c.Runs)
		}
	}
	// Retrieval is server code, so its recall is measured in deterministic mode too.
	if len(r.Measures) != 1 || r.Measures[0].Name != "retrieval_recall_at_10" || !r.Measures[0].Pass {
		t.Errorf("measures %+v", r.Measures)
	}
	// Each mandatory control fails for its own reason.
	want := map[string]string{"control-violating-policy": BreachUnauthorizedEffect + ": forbidden tool evidence.get returned data",
		"control-modified-citation": BreachCitationOutsideRefs + ": artifact cites",
		// WIS-640: each labelled expectation contradicts what the server does with the script.
		"onb-control-refusal-labelled-proposed": "outcome_reasons = [ENTITY_NOT_IN_SOURCE], want []",
		"chat-control-unapproved-as-fact":       "reason_code = CITATION_NOT_IN_SCOPE, want ",
		"chat-control-number-off-by-one":        "reason_code = NUMBER_NOT_IN_SOURCE, want ",
		// WIS-625: a page cut at the output limit is quarantined, never stored.
		"ocr-control-truncation-labelled-stored": "quarantine_reason = PAGE_TRUNCATED, want "}
	// WIS-640 review: the summary counts the knowledge suites' runs too.
	if r.Tokens.Count != started || r.LatencyMs.Count != started || r.Confusion["chat:needs_input"]["chat:needs_input"] == 0 {
		t.Errorf("tokens %+v latency %+v over %d started runs; confusion %v", r.Tokens, r.LatencyMs, started, r.Confusion)
	}
	if r.ErroredRuns != 0 {
		t.Errorf("errored runs %d", r.ErroredRuns)
	}
	for _, c := range r.Controls {
		if c.Outcome != "FAIL" || c.Problem != "" || !strings.Contains(strings.Join(c.Reasons, "\n"), want[c.CaseID]) {
			t.Errorf("control %s: %s %v", c.CaseID, c.Outcome, c.Reasons)
		}
		delete(want, c.CaseID)
	}
	if len(want) != 0 {
		t.Errorf("controls not run: %v", want)
	}
}

// lateScript answers as the case's script for whichever chat run the site holds; the run ID is
// only known once the question has started it.
type lateScript struct {
	mu    sync.Mutex
	build func(ctx context.Context) (agent.Provider, error)
	p     agent.Provider
}

func (s *lateScript) Name() string { return agent.ProviderFixture }
func (s *lateScript) Actual() bool { return false }
func (s *lateScript) Complete(ctx context.Context, r agent.ModelRequest) (agent.ModelResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.p == nil {
		p, err := s.build(ctx)
		if err != nil {
			return agent.ModelResponse{}, err
		}
		s.p = p
	}
	return s.p.Complete(ctx, r)
}

// WIS-640 round 3 (P1): a run pinned to the hosted route dispatches to
// CurrentHostedRoute().Provider, not Runtime.Provider. The recorder must capture that response,
// or every hosted answer becomes "no captured model response matches". The installed route's
// provider returns the right answer for the run: before the fix it answered and the recorder
// missed it (the finding verbatim). After it the run dispatches through the recording route to
// runChat's provider, which in actual hosted mode is that route's own adapter (main.go).
func TestHostedChatRecordsTheDispatchedResponse(t *testing.T) {
	if os.Getenv("SUPERVISOR_TEST_ADMIN_URL") == "" || os.Getenv("SUPERVISOR_TEST_DATABASE_URL") == "" {
		t.Fatal("BLOCKED: real PostgreSQL admin and runtime URLs required")
	}
	ctx := context.Background()
	admin, db, done, err := disposable(ctx, os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	m, _, err := loadManifest(committed, true)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(m.Chat, func(c ChatCase) bool { return c.CaseID == "chat-exact-press-cycle" })
	if i < 0 {
		t.Fatal("chat-exact-press-cycle is not in the corpus")
	}
	c := m.Chat[i]
	h := harness{ctx: ctx, control: admin, db: db, webDir: t.TempDir(), mode: ModeDeterministic, hosted: true,
		model: "fixture-model", revision: "sha256:fixture", agentCfg: fixtureAgent()}
	var w *chatWorld
	late := &lateScript{build: func(ctx context.Context) (agent.Provider, error) {
		var runID string
		err := admin.QueryRowContext(ctx, `SELECT id::text FROM supervisor.agent_runs WHERE tenant_id=$1 AND site_id=$2 AND request_kind=$3`, w.tenant, w.site, agent.RequestConversation).Scan(&runID)
		return w.script(*m.ChatWorld, c, runID), err
	}}
	route := &agent.HostedRoute{Name: agent.ProviderHostedPrefix + "eval-test", Region: "eval", Model: "fixture-model", ModelRevision: "sha256:fixture",
		Price: &agent.TokenPrice{}, PriceVersion: "eval-test", Provider: late}
	agent.SetHostedRoute(route)
	defer agent.SetHostedRoute(nil)
	if w, err = h.newChatWorld(*m.ChatWorld); err != nil {
		t.Fatal(err)
	}
	res, observed := h.runChat(*m.ChatWorld, w, c, 1)
	if len(res.Errors) != 0 || len(res.Failures) != 0 || len(observed) != 1 {
		t.Fatalf("errors %v failures %v observed %d", res.Errors, res.Failures, len(observed))
	}
	// A hosted run records provider_actual=true, which a deterministic-mode harness reports as a
	// provider-label breach; it is the only one.
	if len(res.Breaches) != 1 || !strings.Contains(res.Breaches[0], BreachProviderLabel) {
		t.Errorf("breaches %v", res.Breaches)
	}
	o := observed[0]
	if o.ReplyState != "answered" || !o.ProviderActual || !statesNumber(o, "45", w.quantities(*m.ChatWorld, c)["45"]) {
		t.Errorf("reply %s provider_actual %v statements %+v", o.ReplyState, o.ProviderActual, o.statements)
	}
	if late.p != nil {
		t.Error("the run bypassed the recording route and reached the installed route's provider")
	}
	if agent.CurrentHostedRoute() != route {
		t.Error("the run left the recording route installed")
	}
}

// phasedScript is a chat case's script (a tool call, then its answer) behind an answer-phase
// provider, like the local and hosted adapters: its reply to the second, tool-offering request
// is a draft (a valid needs_input answer that must never be used).
type phasedScript struct {
	agent.Provider
	draft    string
	mu       sync.Mutex
	requests []agent.ModelRequest
}

func (p *phasedScript) AnswerPhase() bool { return true }
func (p *phasedScript) Complete(ctx context.Context, r agent.ModelRequest) (agent.ModelResponse, error) {
	p.mu.Lock()
	p.requests = append(p.requests, r)
	n := len(p.requests)
	p.mu.Unlock()
	if n == 2 {
		return agent.ModelResponse{Content: p.draft}, nil
	}
	return p.Provider.Complete(ctx, r)
}

// WIS-640 review P1: the recorder embedded agent.Provider and hid AnswerPhase, so a recorded
// local or hosted chat run skipped the answer call and took its draft as the answer. Before the
// fix this run ends needs_input after two model calls.
func TestRecordedChatKeepsTheAnswerPhase(t *testing.T) {
	if os.Getenv("SUPERVISOR_TEST_ADMIN_URL") == "" || os.Getenv("SUPERVISOR_TEST_DATABASE_URL") == "" {
		t.Fatal("BLOCKED: real PostgreSQL admin and runtime URLs required")
	}
	ctx := context.Background()
	admin, db, done, err := disposable(ctx, os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	m, _, err := loadManifest(committed, true)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(m.Chat, func(c ChatCase) bool { return c.CaseID == "chat-exact-press-cycle" })
	if i < 0 {
		t.Fatal("chat-exact-press-cycle is not in the corpus")
	}
	c := m.Chat[i]
	h := harness{ctx: ctx, control: admin, db: db, webDir: t.TempDir(), mode: ModeDeterministic,
		model: "fixture-model", revision: "sha256:fixture", agentCfg: fixtureAgent()}
	w, err := h.newChatWorld(*m.ChatWorld)
	if err != nil {
		t.Fatal(err)
	}
	conversation, reply, runID, err := w.ask(c.Question)
	if err != nil {
		t.Fatal(err)
	}
	draft, _ := json.Marshal(agent.ChatAnswerOutputV2{SchemaVersion: agent.ChatSchemaVersion, RunID: outputRunID(runID), Role: agent.RoleDiscoveryMapping,
		Status: "needs_input", Statements: []agent.ChatStatement{}, MissingInputs: []string{"the draft"}})
	phased := &phasedScript{Provider: w.script(*m.ChatWorld, c, runID), draft: string(draft)}
	rec, restore := h.recordDispatch(phased)
	defer restore()
	res, err := h.runtime(rec).ChatAnswer(ctx, agent.ChatAnswerRef{TenantID: w.tenant, SiteID: w.site, RunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	o, err := w.observeAnswer(res, conversation, reply, nil, rec.contents())
	if err != nil {
		t.Fatal(err)
	}
	if o.ReplyState != "answered" || !statesNumber(o, "45", w.quantities(*m.ChatWorld, c)["45"]) {
		t.Fatalf("reply %s %s statements %+v", o.ReplyState, o.ReasonCode, o.statements)
	}
	if len(phased.requests) != 3 || len(rec.contents()) != 3 || len(phased.requests[2].Tools) != 0 || len(phased.requests[2].ResponseSchema) == 0 {
		t.Fatalf("requests %d recorded %d; the answer call offers %d tools", len(phased.requests), len(rec.contents()), len(phased.requests[len(phased.requests)-1].Tools))
	}
}

// WIS-700 (PR #303 review): the eval wiring is checked through the production paths.
//   - runChat does not judge a gate control on retrieval mode: with a hybrid retrieval setup and
//     replies the server labels "word", a control carries no RETRIEVAL_MODE_MISMATCH while a corpus
//     case asked through the same runChat does (so the judge is live).
//   - executeKnowledge reports chat_synonyms recall as a comparison: in word mode it is below the
//     frozen threshold, and the run records no failure for that measure.
func TestWIS700EvalWiringThroughProductionPaths(t *testing.T) {
	if os.Getenv("SUPERVISOR_TEST_ADMIN_URL") == "" || os.Getenv("SUPERVISOR_TEST_DATABASE_URL") == "" {
		t.Fatal("BLOCKED: real PostgreSQL admin and runtime URLs required")
	}
	ctx := context.Background()
	admin, db, done, err := disposable(ctx, os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	m, _, err := loadManifest(committed, true)
	if err != nil {
		t.Fatal(err)
	}
	h := harness{ctx: ctx, control: admin, db: db, webDir: t.TempDir(), mode: ModeDeterministic,
		model: "fixture-model", revision: "sha256:fixture", agentCfg: fixtureAgent()}

	t.Run("controls skip the retrieval judge", func(t *testing.T) {
		hh := h
		// A hybrid setup the server never uses: every reply is labelled word, a mode mismatch.
		hh.retr = &retrievalSetup{mode: RetrievalHybrid, client: &retrieval.Client{}, calls: &callLog{}}
		w, err := hh.newChatWorld(*m.ChatWorld)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range m.ChatControls {
			res, _ := hh.runChat(*m.ChatWorld, w, c, 1)
			cr := knowledgeControl(c.CaseID, c.ExpectedFailure, res)
			if cr.Outcome != "FAIL" || slices.ContainsFunc(cr.Reasons, func(r string) bool { return strings.Contains(r, FailureRetrievalMode) }) {
				t.Errorf("control %s: outcome %q reasons %q, want FAIL without %s", c.CaseID, cr.Outcome, cr.Reasons, FailureRetrievalMode)
			}
		}
		res, _ := hh.runChat(*m.ChatWorld, w, m.Chat[0], 1)
		if !slices.ContainsFunc(res.Failures, func(f string) bool { return strings.Contains(f, FailureRetrievalMode) }) {
			t.Errorf("corpus case %s was not judged on retrieval: failures %q", m.Chat[0].CaseID, res.Failures)
		}
	})

	t.Run("synonym recall is a comparison", func(t *testing.T) {
		r := &Report{Mode: ModeDeterministic, RunsPerCase: 1, Measures: []Measure{}, Failures: []string{}}
		h.executeKnowledge(r, m, []string{SuiteChatSynonyms})
		i := slices.IndexFunc(r.Measures, func(ms Measure) bool { return ms.Suite == SuiteChatSynonyms })
		if i < 0 || !r.Measures[i].Comparison || r.Measures[i].Pass {
			t.Fatalf("measures %+v: want a failing chat_synonyms comparison (word-mode recall is below the threshold)", r.Measures)
		}
		if slices.ContainsFunc(r.Failures, func(f string) bool { return strings.HasPrefix(f, SuiteChatSynonyms+" retrieval_recall_at_10") }) {
			t.Fatalf("the comparison failed the run: %q", r.Failures)
		}
	})
}
