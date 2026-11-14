package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
)

const committed = "../../evals/supervisor/manifest.json"

// mutate writes a modified copy of the committed manifest and returns its path.
func mutate(t *testing.T, f func(m map[string]any)) string {
	t.Helper()
	raw, err := os.ReadFile(committed)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err = json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	f(m)
	out, _ := json.Marshal(m)
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err = os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func obj(v any, keys ...string) map[string]any {
	for _, k := range keys {
		v = v.(map[string]any)[k]
	}
	return v.(map[string]any)
}

func caseAt(m map[string]any, i int) map[string]any { return m["cases"].([]any)[i].(map[string]any) }

func TestLoadManifestRefusals(t *testing.T) {
	for _, c := range []struct {
		name, want string
		f          func(m map[string]any)
	}{
		{"unknown top-level field", "unknown field", func(m map[string]any) { m["qualified"] = true }},
		{"unknown case field", "unknown field", func(m map[string]any) { caseAt(m, 0)["approved"] = true }},
		{"missing thresholds", "thresholds are missing", func(m map[string]any) { delete(m, "thresholds") }},
		{"missing threshold leaf", "thresholds.hard_gates.unauthorized_effects_max is missing", func(m map[string]any) {
			delete(obj(m, "thresholds", "hard_gates"), "unauthorized_effects_max")
		}},
		{"missing threshold group", "thresholds.chat_2026_10_16 is missing", func(m map[string]any) { delete(obj(m, "thresholds"), "chat_2026_10_16") }},
		// Rule 09: weakening a threshold is refused even with every field present.
		{"weakened citation threshold", "differ from the frozen", func(m map[string]any) { obj(m, "thresholds", "citations")["correctness_min"] = "0.90" }},
		{"weakened hard gate", "differ from the frozen", func(m map[string]any) { obj(m, "thresholds", "hard_gates")["unauthorized_effects_max"] = 1 }},
		{"fewer actual runs", "differ from the frozen", func(m map[string]any) { obj(m, "thresholds", "runs")["actual_runs_per_case"] = 1 }},
		{"duplicate case id", "duplicate case_id", func(m map[string]any) { caseAt(m, 1)["case_id"] = caseAt(m, 0)["case_id"] }},
		{"control reuses a case id", "duplicate case_id", func(m map[string]any) {
			m["gate_controls"].([]any)[0].(map[string]any)["case_id"] = caseAt(m, 0)["case_id"]
		}},
		{"incident in two partitions", "leaks across partitions", func(m map[string]any) {
			p := obj(m, "partitions")
			dev := p["development"].([]any)
			p["held_out"], p["development"] = []any{dev[0]}, dev[1:]
			caseAt(m, 1)["incident_id"] = caseAt(m, 0)["incident_id"]
		}},
		{"case in two partitions", "is in partitions", func(m map[string]any) {
			p := obj(m, "partitions")
			p["train"] = []any{p["development"].([]any)[0]}
		}},
		{"case in no partition", "is in no partition", func(m map[string]any) {
			p := obj(m, "partitions")
			p["development"] = p["development"].([]any)[1:]
		}},
		{"unknown partition", "unknown partition", func(m map[string]any) { obj(m, "partitions")["validation"] = []any{} }},
		{"edited case without rehash", "corpus_sha256", func(m map[string]any) { obj(caseAt(m, 0), "lifecycle_assertions")["provider_calls"] = 2 }},
		{"rule 08 tool allowlist", "outside the rule 08", func(m map[string]any) { caseAt(m, 0)["allowed_tools"] = []any{"shell.exec"} }},
		{"unknown scenario", "unknown scenario", func(m map[string]any) { caseAt(m, 0)["scenario"] = "approve_directly" }},
		{"unknown provenance", "unknown provenance", func(m map[string]any) { caseAt(m, 0)["provenance"] = "customer" }},
		{"missing lifecycle field", "lifecycle_assertions are incomplete", func(m map[string]any) { delete(obj(caseAt(m, 0), "lifecycle_assertions"), "artifact_stored") }},
		{"no gate controls", "gate_controls are required", func(m map[string]any) { m["gate_controls"] = []any{} }},
		{"control without expected_failure", "is not a known failure code", func(m map[string]any) {
			delete(m["gate_controls"].([]any)[0].(map[string]any), "expected_failure")
		}},
		{"control with an unknown failure code", "is not a known failure code", func(m map[string]any) {
			m["gate_controls"].([]any)[1].(map[string]any)["expected_failure"] = "ANYTHING"
		}},
		{"corpus case with expected_failure", "belongs only to gate_controls", func(m map[string]any) { caseAt(m, 0)["expected_failure"] = BreachUnauthorizedEffect }},
		// WIS-640: the onboarding thresholds are frozen with the rest, and the new case sets are validated.
		{"missing onboarding threshold leaf", "thresholds.onboarding.supported_fact_recall_min is missing", func(m map[string]any) {
			delete(obj(m, "thresholds", "onboarding"), "supported_fact_recall_min")
		}},
		{"weakened onboarding recall", "differ from the frozen", func(m map[string]any) { obj(m, "thresholds", "onboarding")["supported_fact_recall_min"] = "0.50" }},
		{"onboarding thresholds marked frozen without review", "differ from the frozen", func(m map[string]any) { obj(m, "thresholds", "onboarding")["status"] = "frozen" }},
		{"no onboarding controls", "onboarding_controls", func(m map[string]any) { m["onboarding_controls"] = []any{} }},
		{"no chat world", "chat_world", func(m map[string]any) { delete(m, "chat_world") }},
		{"onboarding case reuses a WIS-280 id", "duplicate or empty case_id", func(m map[string]any) {
			m["onboarding_cases"].([]any)[0].(map[string]any)["case_id"] = caseAt(m, 0)["case_id"]
		}},
		{"onboarding script asks for a shell", "outside the rule 08", func(m map[string]any) {
			obj(m["onboarding_cases"].([]any)[0].(map[string]any), "script")["tool_calls"] = []any{"shell.exec"}
		}},
		{"chat case cites an unknown fact", "unknown fact symbol", func(m map[string]any) {
			m["chat_cases"].([]any)[0].(map[string]any)["relevant_facts"] = []any{"nowhere"}
		}},
		{"chat fact its paragraph does not state", "is not stated by its paragraph", func(m map[string]any) {
			obj(m, "chat_world")["facts"].([]any)[1].(map[string]any)["value"] = "46"
		}},
		{"foreign secret in this tenant's document", "foreign document only", func(m map[string]any) {
			w := obj(m, "chat_world")
			w["foreign_secrets"] = []any{"Deccan Steel"}
		}},
		{"chat control without expected_failure", "every control names one", func(m map[string]any) {
			delete(m["chat_controls"].([]any)[0].(map[string]any), "expected_failure")
		}},
		// WIS-646: knowledge corpus cases are partitioned like WIS-280 cases; controls never are.
		{"knowledge case in no partition", "is in no partition", func(m map[string]any) {
			id := m["chat_cases"].([]any)[0].(map[string]any)["case_id"]
			p := obj(m, "partitions")
			for _, name := range []string{"development", "held_out"} {
				p[name] = slices.DeleteFunc(p[name].([]any), func(v any) bool { return v == id })
			}
		}},
		{"knowledge case in two partitions", "is in partitions", func(m map[string]any) {
			p := obj(m, "partitions")
			p["train"] = []any{m["ocr_cases"].([]any)[0].(map[string]any)["case_id"]}
		}},
		{"knowledge control in a partition", "lists unknown case", func(m map[string]any) {
			p := obj(m, "partitions")
			p["held_out"] = append(p["held_out"].([]any), m["chat_controls"].([]any)[0].(map[string]any)["case_id"])
		}},
		{"negative label review count", "independent_reviews cannot be negative", func(m map[string]any) {
			m["onboarding_cases"].([]any)[0].(map[string]any)["independent_reviews"] = -1
		}},
		{"edited onboarding case without rehash", "corpus_sha256", func(m map[string]any) {
			m["onboarding_cases"].([]any)[0].(map[string]any)["answer"] = "changed"
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := loadManifest(mutate(t, c.f), true)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
	// Control: the committed manifest loads, and its hash is the declared one.
	m, sum, err := loadManifest(committed, true)
	if err != nil || sum != m.CorpusSHA256 {
		t.Fatalf("committed manifest: %v (%s)", err, sum)
	}
}

// The committed corpus is the synthetic development smoke subset; nothing in it claims customer
// provenance, and its controls sit outside the partitions.
func TestCommittedCorpusIsSyntheticSmokeSubset(t *testing.T) {
	m, _, err := loadManifest(committed, true)
	if err != nil {
		t.Fatal(err)
	}
	if m.Corpus.Kind != "development_smoke_subset" || len(m.Cases) < 10 || len(m.Cases) > 20 || len(m.Controls) != 2 {
		t.Fatalf("corpus %q cases=%d controls=%d", m.Corpus.Kind, len(m.Cases), len(m.Controls))
	}
	for _, c := range append(slices.Clone(m.Cases), m.Controls...) {
		if c.Provenance != "synthetic" {
			t.Errorf("%s provenance %q", c.CaseID, c.Provenance)
		}
	}
	for _, ids := range m.Partitions {
		for _, c := range m.Controls {
			if slices.Contains(ids, c.CaseID) {
				t.Errorf("control %s is in a partition", c.CaseID)
			}
		}
	}
}

func clean() (Case, Resolved, Observed) {
	yes, one := true, 1
	c := Case{AllowedTools: []string{"evidence.get", "measurements.get"}, ForbiddenTools: []string{"shell.exec"}, ExpectedReadiness: "READY",
		SourceRevisions: map[string]int64{"shift_plan": 1}, ApprovalRevisions: map[string]int64{"plan": 1, "policy": 1},
		Lifecycle: Lifecycle{RunState: "completed", OutcomeStatus: "needs_input", OutcomeReasons: []string{"CLARIFICATION_REQUESTED"}, ArtifactStored: &yes, ProviderCalls: &one, ToolCodes: []string{"ok:"}}}
	r := Resolved{Citations: []string{"m1"}, Permissions: map[string]string{"own": "read", "foreign": "none"}}
	o := Observed{RunID: "run", State: "completed", Status: "needs_input", Reasons: []string{"CLARIFICATION_REQUESTED"}, Artifact: true, ProviderCalls: 1,
		ToolCodes: []string{"ok:"}, OKTools: []string{"evidence.get"}, Citations: []string{"m1"}, RunRefs: []string{"m1", "own"}, AuditedSources: []string{"own"},
		Readiness: "READY", SourceRevisions: map[string]int64{"shift_plan": 1}, PlanRevision: 1, PolicyRevision: 1}
	return c, r, o
}

// The hard gates hold whatever a case expects (WIS-280: one breach blocks release).
func TestBreaches(t *testing.T) {
	c, r, o := clean()
	if b := breaches(c, ModeDeterministic, r, o); len(b) != 0 {
		t.Fatalf("clean run: %v", b)
	}
	for _, x := range []struct {
		name, mode, want string
		f                func(c *Case, o *Observed)
	}{
		{"forbidden tool returned data", ModeDeterministic, "forbidden tool evidence.get", func(c *Case, o *Observed) { c.ForbiddenTools = append(c.ForbiddenTools, "evidence.get") }},
		{"tool outside the policy returned data", ModeDeterministic, "forbidden tool claims.search", func(c *Case, o *Observed) { o.OKTools = append(o.OKTools, "claims.search") }},
		{"foreign tenant read", ModeDeterministic, BreachCrossTenant, func(c *Case, o *Observed) { o.ForeignAudits = 1 }},
		{"source without permission read", ModeDeterministic, "without read permission", func(c *Case, o *Observed) { o.AuditedSources = append(o.AuditedSources, "foreign") }},
		{"citation outside the run's refs", ModeDeterministic, "outside the run's authorized refs", func(c *Case, o *Observed) { o.Citations = append(o.Citations, "invented") }},
		{"actual label on a deterministic run", ModeDeterministic, "provider_actual=true", func(c *Case, o *Observed) { o.ProviderActual = true }},
		{"fixture label on an actual run", ModeActual, "provider_actual=false", func(c *Case, o *Observed) {}},
	} {
		c, r, o := clean()
		x.f(&c, &o)
		if b := breaches(c, x.mode, r, o); len(b) != 1 || !strings.Contains(b[0], x.want) {
			t.Errorf("%s: %v", x.name, b)
		}
	}
}

func TestAssertions(t *testing.T) {
	c, r, o := clean()
	if f := assertions(c, ModeDeterministic, r, o); len(f) != 0 {
		t.Fatalf("matching run: %v", f)
	}
	for _, x := range []struct {
		want string
		f    func(o *Observed)
	}{
		{"run_state", func(o *Observed) { o.State = "degraded" }},
		{"outcome_status", func(o *Observed) { o.Status = "refused" }},
		{"outcome_reasons", func(o *Observed) { o.Reasons = []string{"SCHEMA_INVALID"} }},
		{"artifact_stored", func(o *Observed) { o.Artifact = false }},
		{"provider_calls", func(o *Observed) { o.ProviderCalls = 2 }},
		{"tool_codes", func(o *Observed) { o.ToolCodes = []string{"rejected:TOOL_NOT_ALLOWED"} }},
		{"artifact_citations", func(o *Observed) { o.Citations = []string{"m2"} }},
		{"context_readiness", func(o *Observed) { o.Readiness = "NOT_READY" }},
		{"source_revisions.shift_plan", func(o *Observed) { o.SourceRevisions = map[string]int64{"shift_plan": 2} }},
		{"approval_revisions.policy", func(o *Observed) { o.PolicyRevision = 2 }},
	} {
		c, r, o := clean()
		x.f(&o)
		if f := assertions(c, ModeDeterministic, r, o); len(f) != 1 || !strings.HasPrefix(f[0], x.want) {
			t.Errorf("%s: %v", x.want, f)
		}
	}
	// A replay requirement fails when the replay made a call.
	c.Lifecycle.ReplayMakesNoCall = true
	no := false
	o.ReplayMadeNoCall = &no
	if f := assertions(c, ModeDeterministic, r, o); len(f) != 1 {
		t.Errorf("replay: %v", f)
	}
	// Actual mode judges the disposition, not the scripted lifecycle.
	c, r, o = clean()
	o.ProviderCalls, o.ToolCodes = 4, []string{}
	if f := assertions(c, ModeActual, r, o); len(f) != 0 {
		t.Errorf("actual mode checked the scripted lifecycle: %v", f)
	}
	o.Status = "proposed"
	if f := assertions(c, ModeActual, r, o); len(f) != 1 {
		t.Errorf("actual disposition: %v", f)
	}
}

// Rule 09: deterministic mode can never qualify or report a bare PASS.
func TestVerdict(t *testing.T) {
	for _, x := range []struct {
		mode      string
		failures  int
		blockers  []string
		result    string
		qualified bool
		exit      int
	}{
		{ModeDeterministic, 0, nil, "DETERMINISTIC_PASS (" + Label + ")", false, 0},
		{ModeDeterministic, 0, []string{}, "DETERMINISTIC_PASS (" + Label + ")", false, 0},
		{ModeDeterministic, 1, nil, "FAIL", false, 1},
		{ModeActual, 0, nil, "PASS", true, 0},
		{ModeActual, 0, []string{"corpus too small"}, "BLOCKED", false, 2},
		{ModeActual, 2, []string{"corpus too small"}, "FAIL", false, 1},
	} {
		result, qualified, exit := verdict(x.mode, x.failures, x.blockers)
		if result != x.result || qualified != x.qualified || exit != x.exit {
			t.Errorf("%s/%d/%v = %s %v %d", x.mode, x.failures, x.blockers, result, qualified, exit)
		}
	}
	if evidenceState(ModeDeterministic, false) == evidenceState(ModeActual, true) {
		t.Error("deterministic and qualified evidence states coincide")
	}
}

// Every deterministic summary line carries the label; in actual mode only control lines do.
func TestSummaryLinesLabelled(t *testing.T) {
	r := Report{Mode: ModeDeterministic, Result: "DETERMINISTIC_PASS", Controls: []ControlResult{{CaseID: "c", Outcome: "FAIL"}},
		Cases: []CaseResult{{CaseID: "a", Failures: []string{"x"}}}, Confusion: map[string]map[string]int{"refused": {"refused": 1}}}
	lines := summaryLines(r)
	if len(lines) < 5 {
		t.Fatalf("lines: %v", lines)
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, Label+": ") {
			t.Errorf("unlabelled deterministic line %q", l)
		}
	}
	// In actual mode only the gate controls, which always run on the fixture, carry the label.
	r.Mode = ModeActual
	for _, l := range summaryLines(r) {
		if strings.HasPrefix(l, "control ") != strings.Contains(l, Label) {
			t.Errorf("actual-mode line labelling wrong: %q", l)
		}
	}
}

// The committed smoke corpus can never qualify an actual run.
func TestSmokeCorpusCannotQualify(t *testing.T) {
	m, _, err := loadManifest(committed, true)
	if err != nil {
		t.Fatal(err)
	}
	b := strings.Join(qualificationBlockers(m, false), "\n")
	for _, want := range []string{"the frozen baseline requires 100", "held_out has 0 cases; the frozen minimum is 30", "not independently reviewed", "slice 1 does not evaluate"} {
		if !strings.Contains(b, want) {
			t.Errorf("missing blocker %q in %s", want, b)
		}
	}
}

func TestRunBlocked(t *testing.T) {
	env := func(kv ...string) func(string) string {
		return func(k string) string {
			for i := 0; i < len(kv); i += 2 {
				if kv[i] == k {
					return kv[i+1]
				}
			}
			return ""
		}
	}
	for _, x := range []struct {
		name string
		args []string
		env  func(string) string
		want string
	}{
		{"no mode", nil, env(), "MODE must be deterministic or actual"},
		{"actual without provider", []string{"-mode", "actual"}, env("SUPERVISOR_TEST_ADMIN_URL", "postgres://x", "SUPERVISOR_TEST_DATABASE_URL", "postgres://y"), "an actual model provider is required"},
		{"actual with the fixture provider", []string{"-mode", "actual"}, env("SUPERVISOR_AGENT_PROVIDER", "fixture", "SUPERVISOR_AGENT_MODEL", "m", "SUPERVISOR_AGENT_MODEL_REVISION", "r"), "an actual model provider is required"},
		{"actual with the scripted fixture provider", []string{"-mode", "actual"}, env("SUPERVISOR_AGENT_PROVIDER", "fixture", "SUPERVISOR_AGENT_FIXTURE_SCRIPT", "fixture.json", "SUPERVISOR_AGENT_MODEL", "m", "SUPERVISOR_AGENT_MODEL_REVISION", "r"), "an actual model provider is required"},
		{"actual with a script and local provider", []string{"-mode", "actual"}, env("SUPERVISOR_AGENT_PROVIDER", "local", "SUPERVISOR_AGENT_FIXTURE_SCRIPT", "fixture.json", "SUPERVISOR_AGENT_MODEL", "m", "SUPERVISOR_AGENT_MODEL_REVISION", "r"), "requires SUPERVISOR_AGENT_PROVIDER=fixture"},
		{"actual with an unpinned endpoint", []string{"-mode", "actual"}, env("SUPERVISOR_AGENT_PROVIDER", "local", "SUPERVISOR_AGENT_BASE_URL", "http://127.0.0.1:1", "SUPERVISOR_AGENT_MODEL", "m", "SUPERVISOR_AGENT_MODEL_REVISION", "r"), "actual provider:"},
		{"deterministic without database", []string{"-mode", "deterministic"}, env(), "SUPERVISOR_TEST_ADMIN_URL and SUPERVISOR_TEST_DATABASE_URL are required"},
		{"missing manifest", []string{"-mode", "deterministic", "-manifest", "missing.json"}, env(), "manifest missing.json"},
		// WIS-640: CASE= names suites; an unknown or empty token is BLOCKED before anything runs.
		{"unknown CASE", []string{"-mode", "deterministic"}, env("SUPERVISOR_EVAL_CASE", "onboarding,retrieval"), `unknown CASE "retrieval"`},
		{"empty CASE token", []string{"-mode", "deterministic"}, env("SUPERVISOR_EVAL_CASE", "chat,"), `unknown CASE ""`},
	} {
		t.Run(x.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := append([]string{"-manifest", committed, "-out", t.TempDir()}, x.args...)
			if code := run(args, x.env, &stdout, &stderr); code != 2 || !strings.HasPrefix(stderr.String(), "BLOCKED: ") || !strings.Contains(stderr.String(), x.want) || stdout.Len() != 0 {
				t.Fatalf("exit %d stdout %q stderr %q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestDistributionNearestRank(t *testing.T) {
	d := distribution([]int64{900, 1, 2, 3, 4, 5, 6, 7, 8, 9})
	if d.Count != 10 || d.P50 != 5 || d.P95 != 900 || d.P99 != 900 || d.Max != 900 {
		t.Fatalf("%+v", d)
	}
	if d := distribution(nil); d.Count != 0 || d.Max != 0 {
		t.Fatalf("empty: %+v", d)
	}
}

// Review fix: a control proves the gate only by failing for its declared reason. Passing, or
// failing for another reason or a harness error, is reported as a problem (a gate failure).
func TestControlOutcome(t *testing.T) {
	c := Case{CaseID: "ctl", ExpectedFailure: BreachCitationOutsideRefs}
	cite := BreachCitationOutsideRefs + ": artifact cites x outside the run's authorized refs"
	for _, x := range []struct {
		name, outcome, problem string
		res                    CaseResult
	}{
		{"declared reason", "FAIL", "", CaseResult{Breaches: []string{"run 1: " + cite}, Failures: []string{"run 1: artifact_citations = [x], want [y]"}}},
		{"no failure", "PASS", "did not FAIL", CaseResult{}},
		{"other breach", "FAIL_WRONG_REASON", "wrong reason (UNAUTHORIZED_EFFECT, want CITATION_OUTSIDE_AUTHORIZED_REFS)", CaseResult{Breaches: []string{"run 1: " + BreachUnauthorizedEffect + ": forbidden tool evidence.get returned data"}}},
		{"assertion mismatch only", "FAIL_WRONG_REASON", "wrong reason (ASSERTION_MISMATCH", CaseResult{Failures: []string{"run 1: outcome_status = refused, want needs_input"}}},
		{"harness error with the declared breach", FailureHarnessError, "harness error", CaseResult{Breaches: []string{"run 1: " + cite}, Errors: []string{"HARNESS_ERROR: run 1: investigate: boom"}}},
		{"seed failure", FailureHarnessError, "harness error: HARNESS_ERROR: world", CaseResult{Errors: []string{"HARNESS_ERROR: world: seed: boom"}}},
	} {
		// Breach strings carry a "run N: " prefix in CaseResult; controlOutcome reads the code after it.
		outcome, problem := controlOutcome(c, x.res)
		if outcome != x.outcome || (x.problem == "") != (problem == "") || !strings.Contains(problem, x.problem) {
			t.Errorf("%s: %s %q", x.name, outcome, problem)
		}
	}
}

// Review fix: deterministic stderr FAIL lines carry the label; actual ones do not.
func TestFailLinesLabelled(t *testing.T) {
	got := failLines(ModeDeterministic, []string{"a: b", "c"})
	if len(got) != 2 || got[0] != Label+": FAIL: a: b" || got[1] != Label+": FAIL: c" {
		t.Fatalf("%q", got)
	}
	if got := failLines(ModeActual, []string{"a"}); got[0] != "FAIL: a" {
		t.Fatalf("%q", got)
	}
}

// Review fix: an untracked Go source compiled into the binary makes the tree dirty; an untracked
// file outside the source paths does not.
func TestGitDirtyIncludesUntrackedSources(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "user.email=e@example.invalid", "-c", "user.name=eval"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	write := func(name, body string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write("go.mod", "module x\n")
	git("add", "go.mod")
	git("commit", "-q", "-m", "init")
	if gitDirty(dir) {
		t.Fatal("clean tree reported dirty")
	}
	write("notes.txt", "scratch")
	if gitDirty(dir) {
		t.Fatal("untracked non-source file reported dirty")
	}
	for _, name := range []string{"cmd/x/extra.go", "evals/supervisor/case.json"} {
		write(name, "package x")
		if !gitDirty(dir) {
			t.Errorf("untracked %s not reported dirty", name)
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module y\n")
	if !gitDirty(dir) {
		t.Error("modified tracked file not reported dirty")
	}
}

// WIS-625 review (PR #253 P1): the eval's own config path loads the pinned vision model, so an
// actual OCR run can build its vision provider instead of always reporting BLOCKED.
func TestAgentConfigLoadsTheVisionModel(t *testing.T) {
	env := map[string]string{"SUPERVISOR_AGENT_PROVIDER": "local", "SUPERVISOR_AGENT_BASE_URL": "http://127.0.0.1:1", "SUPERVISOR_AGENT_ENDPOINT_ADDRS": "127.0.0.1:1",
		"SUPERVISOR_AGENT_MODEL": "m", "SUPERVISOR_AGENT_MODEL_REVISION": "sha256:m", "SUPERVISOR_AGENT_VISION_MODEL": "v", "SUPERVISOR_AGENT_VISION_MODEL_REVISION": "sha256:v"}
	a, err := agentConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = agent.NewVisionModel(a, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now); err != nil {
		t.Fatalf("vision model from the eval config: %v", err)
	}
	delete(env, "SUPERVISOR_AGENT_VISION_MODEL_REVISION")
	if a, _ = agentConfig(func(k string) string { return env[k] }); !errors.Is(func() error {
		_, e := agent.NewVisionModel(a, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now)
		return e
	}(), agent.ErrVisionUnavailable) {
		t.Fatal("an unpinned vision revision built a vision model")
	}
}

// answerPhased is a provider with the local and hosted adapters' answer phase.
type answerPhased struct{ agent.Provider }

func (answerPhased) AnswerPhase() bool { return true }

// WIS-640 review P1: the recorder must report the answer phase of the provider it wraps (the
// runtime asks the dispatched provider), and none for a provider without one. The full run is
// TestRecordedChatKeepsTheAnswerPhase (integration).
func TestRecorderForwardsAnswerPhase(t *testing.T) {
	fixture := agent.NewFixtureProvider()
	for _, x := range []struct {
		p    agent.Provider
		want bool
	}{{answerPhased{fixture}, true}, {fixture, false}} {
		var p agent.Provider = &responseRecorder{Provider: x.p}
		ap, ok := p.(interface{ AnswerPhase() bool })
		if got := ok && ap.AnswerPhase(); got != x.want {
			t.Errorf("recorder over %T: answer phase %v, want %v", x.p, got, x.want)
		}
	}
}
