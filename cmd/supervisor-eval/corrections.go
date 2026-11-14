package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/chat"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
	"github.com/google/uuid"
)

const SuiteCorrections = "corrections"

type CorrectionSet struct {
	SchemaVersion int      `json:"schema_version"`
	TenantID      string   `json:"tenant_id"`
	SiteID        string   `json:"site_id"`
	SnapshotID    string   `json:"snapshot_id"`
	CaseIDs       []string `json:"case_ids"`
}

type CorrectionObserved struct {
	EntityNames    []string                           `json:"pinned_entity_names"`
	Case           knowledge.CorrectionEvaluationCase `json:"case"`
	SnapshotSHA256 string                             `json:"snapshot_sha256"`
	Run            agent.Run                          `json:"run"`
	Refs           []agent.RunRef                     `json:"refs"`
	ArtifactSHA256 string                             `json:"artifact_sha256"`
	Artifact       agent.ChatAnswerArtifactV2         `json:"artifact"`
	Answer         chat.Message                       `json:"answer"`
	ProviderCalls  int                                `json:"provider_calls"`
	InputTokens    int64                              `json:"input_tokens"`
	OutputTokens   int64                              `json:"output_tokens"`
	LatencyMS      int64                              `json:"latency_ms"`
}

func loadCorrectionSet(path string) (CorrectionSet, string, error) {
	var set CorrectionSet
	raw, err := os.ReadFile(path)
	if err != nil {
		return set, "", errors.New("correction set is unreadable")
	}
	if err = strictjson.Decode(raw, &set, 64<<10); err != nil {
		return set, "", err
	}
	if set.SchemaVersion != 1 || len(set.CaseIDs) == 0 || len(set.CaseIDs) > 100 {
		return set, "", errors.New("select 1–100 correction cases, schema_version 1")
	}
	seen := map[string]bool{}
	for _, id := range append([]string{set.TenantID, set.SiteID, set.SnapshotID}, set.CaseIDs...) {
		if v, e := uuid.Parse(id); e != nil || v == uuid.Nil {
			return set, "", errors.New("correction selection requires UUIDs")
		}
	}
	for _, id := range set.CaseIDs {
		if seen[id] {
			return set, "", errors.New("duplicate correction case")
		}
		seen[id] = true
	}
	sum := sha256.Sum256(raw)
	return set, hex.EncodeToString(sum[:]), nil
}

// A live browser session supplies the human's authority; this runner cannot create reviewers.
func correctionActor(ctx context.Context, db *sql.DB, path string) (knowledge.Actor, error) {
	var a knowledge.Actor
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
		return a, errors.New("correction session file must be a private regular file (0600)")
	}
	token, err := os.ReadFile(path)
	if err != nil {
		return a, err
	}
	token = []byte(strings.TrimSpace(string(token)))
	if len(token) == 0 {
		return a, errors.New("empty correction session")
	}
	sum := sha256.Sum256(token)
	a.SessionHash = hex.EncodeToString(sum[:])
	err = db.QueryRowContext(ctx, `SELECT issuer,subject FROM supervisor.browser_sessions WHERE token_hash=$1`, a.SessionHash).Scan(&a.Issuer, &a.Subject)
	if err != nil {
		return a, errors.New("correction browser session unavailable")
	}
	return a, nil
}

func correctionCases(ctx context.Context, svc *knowledge.Service, a knowledge.Actor, set CorrectionSet) ([]knowledge.CorrectionEvaluationCase, error) {
	sc := knowledge.Scope{TenantID: set.TenantID, SiteID: set.SiteID}
	found := map[string]knowledge.CorrectionEvaluationCase{}
	for cursor := ""; ; {
		page, err := svc.ListCorrectionEvaluationCases(ctx, a, sc, cursor, 100)
		if err != nil {
			return nil, err
		}
		for _, c := range page.Cases {
			if slices.Contains(set.CaseIDs, c.CaseID) {
				found[c.CaseID] = c
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	out := make([]knowledge.CorrectionEvaluationCase, 0, len(set.CaseIDs))
	for _, id := range set.CaseIDs {
		c, ok := found[id]
		if !ok {
			return nil, errors.New("selected correction case unavailable")
		}
		pin := c
		pin.CaseSHA256 = ""
		pin.CreatedAt = time.Time{}
		_, sum, err := canonical.Envelope("CorrectionEvaluationCaseV1", 1, set.TenantID, set.SiteID, pin)
		if err != nil || sum != c.CaseSHA256 {
			return nil, errors.New("correction case digest mismatch")
		}
		out = append(out, c)
	}
	return out, nil
}

func correctionSnapshot(c knowledge.CorrectionEvaluationCase, s knowledge.Snapshot) string {
	for _, claim := range s.Contents.Claims {
		if claim.ClaimID != c.ClaimID || claim.Revision != c.ClaimRevision || claim.ReviewID != c.CanonicalReviewID {
			continue
		}
		if !reflect.DeepEqual(claim.ClaimInput, c.Claim) {
			return "CLAIM_PIN_MISMATCH"
		}
		for _, pin := range c.SourcePins {
			if !slices.Contains(s.Contents.Sources, pin) {
				return "SOURCE_PIN_MISMATCH"
			}
		}
		return ""
	}
	return "AWAITING_SNAPSHOT_OR_UNAVAILABLE_APPROVAL"
}

func correctionScript(c knowledge.CorrectionEvaluationCase, runID string) agent.Provider {
	calls := []agent.ToolCall{}
	for i, p := range c.SourcePins {
		calls = append(calls, toolCall(fmt.Sprintf("read-%d", i), agent.ToolEvidenceGet, p.SourceID))
	}
	stmt := agent.ChatStatement{Kind: "approved_fact", Text: c.ExpectedAnswer, SourceSpanIDs: c.Claim.SourceSpanIDs, ClaimRefs: []agent.ChatClaimRef{{ClaimID: c.ClaimID, Revision: c.ClaimRevision}}}
	raw, _ := json.Marshal(agent.ChatAnswerOutputV2{SchemaVersion: agent.ChatSchemaVersion, RunID: outputRunID(runID), Role: agent.RoleDiscoveryMapping, Status: "answered", Statements: []agent.ChatStatement{stmt}, MissingInputs: []string{}})
	return agent.NewFixtureProvider(agent.FixtureStep{Response: agent.ModelResponse{ToolCalls: calls}}, agent.FixtureStep{Response: agent.ModelResponse{Content: string(raw)}})
}

// shortcut: one complete reviewed statement, whitespace-normalized; broader semantic matching
// needs independently reviewed assertions. Extra, negated and contradictory statements fail.
func correctionAssertions(c knowledge.CorrectionEvaluationCase, o CorrectionObserved) []string {
	if o.Answer.State != "answered" || o.Answer.Body == nil || o.Artifact.Status != "answered" {
		return []string{"ANSWER_ABSTAINED_OR_UNAVAILABLE"}
	}
	settled, ok := settledStatements(*o.Answer.Body, o.Artifact.Statements)
	if !ok {
		return []string{"DELIVERED_ARTIFACT_MISMATCH"}
	}
	if len(o.Artifact.Statements) != 1 {
		return []string{"EXPECTED_COMPLETE_STATEMENT"}
	}
	stmt := o.Artifact.Statements[0]
	normalize := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	if settled[0].Kind != "approved_fact" || normalize(stmt.Text) != normalize(c.ExpectedAnswer) {
		return []string{"EXPECTED_ANSWER_MISMATCH"}
	}
	expected := agent.ChatClaimRef{ClaimID: c.ClaimID, Revision: c.ClaimRevision}
	if len(stmt.ClaimRefs) != 1 || stmt.ClaimRefs[0] != expected {
		return []string{"EXACT_CLAIM_REQUIRED"}
	}
	spans := slices.Clone(c.Claim.SourceSpanIDs)
	slices.Sort(spans)
	cited := slices.Clone(stmt.SourceSpanIDs)
	slices.Sort(cited)
	if !slices.Equal(spans, cited) {
		return []string{"EXACT_SPANS_REQUIRED"}
	}
	if len(o.Answer.ClaimCitations) != 1 || len(o.Artifact.ClaimCitations) != 1 || !reflect.DeepEqual(o.Answer.ClaimCitations, o.Artifact.ClaimCitations) {
		return []string{"DELIVERED_CLAIM_MISMATCH"}
	}
	cite := o.Answer.ClaimCitations[0]
	slices.Sort(cite.SourceSpanIDs)
	if cite.ChatClaimRef != expected || cite.Statement != 0 || !slices.Equal(cite.SourceSpanIDs, spans) {
		return []string{"EXACT_CLAIM_REQUIRED"}
	}
	for _, p := range c.SourcePins {
		if !slices.Contains(o.Refs, agent.RunRef{Kind: agent.RefEvidence, RefID: p.SourceID, Revision: p.Revision, SHA256: p.SHA256, AccessEpoch: p.AccessEpoch}) {
			return []string{"SOURCE_PIN_MISMATCH"}
		}
	}
	// Decimal text and units must state the exact reviewed quantity; no float conversion.
	if c.Claim.Literal != nil && c.Claim.Literal.Type == "decimal" {
		var value string
		if json.Unmarshal(c.Claim.Literal.Value, &value) != nil {
			return []string{"INVALID_DECIMAL_ASSERTION"}
		}
		states, conflict := statementQuantity(maskNames(stmt.Text, o.EntityNames), value, quantity{unit: c.Claim.Literal.Unit})
		if !states || conflict {
			return []string{"EXACT_QUANTITY_REQUIRED"}
		}
	}
	return nil
}

func executeCorrection(ctx context.Context, h harness, a knowledge.Actor, set CorrectionSet, c knowledge.CorrectionEvaluationCase, snap knowledge.Snapshot) (CorrectionObserved, error) {
	o := CorrectionObserved{Case: c, SnapshotSHA256: snap.SHA256}
	for _, e := range snap.Contents.Entities {
		if e.EntityID == c.Claim.SubjectID || e.EntityID == c.Claim.ObjectEntityID {
			o.EntityNames = append(o.EntityNames, e.Name)
		}
	}
	sc := chat.Scope{TenantID: set.TenantID, SiteID: set.SiteID}
	rs := agent.RunScope{TenantID: set.TenantID, SiteID: set.SiteID}
	actor := chat.Actor{Issuer: a.Issuer, Subject: a.Subject, SessionHash: a.SessionHash}
	svc, err := chat.NewService(h.db.DB).WithAssistant(true).WithAnswering(chat.AnswerSettings{Enabled: true, Provider: providerName(h.mode), Model: h.model, ModelRev: h.revision, ProviderActual: h.mode == ModeActual}).WithEvaluationSnapshot(set.SnapshotID)
	if err != nil {
		return o, err
	}
	conv, err := svc.Create(ctx, actor, sc, chat.CreateInput{Title: "Correction regression"}, uuid.NewString())
	if err != nil {
		return o, err
	}
	posted, err := svc.Post(ctx, actor, sc, conv.ConversationID, chat.PostInput{RequestID: uuid.NewString(), Text: c.Question}, uuid.NewString(), conv.Revision)
	if err != nil {
		return o, err
	}
	st := agent.NewStore(h.db.DB)
	var runID string
	err = st.InTx(ctx, rs, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT run_id::text FROM supervisor.chat_messages WHERE tenant_id=$1 AND site_id=$2 AND message_id=$3`, set.TenantID, set.SiteID, posted.Reply.MessageID).Scan(&runID)
	})
	if err != nil {
		return o, errors.New("question did not start an authorized answer run")
	}
	p := h.actual
	if h.mode == ModeDeterministic {
		p = correctionScript(c, runID)
	}
	start := time.Now()
	_, err = h.runtime(p).ChatAnswer(ctx, agent.ChatAnswerRef{TenantID: set.TenantID, SiteID: set.SiteID, RunID: runID})
	o.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		return o, err
	}
	err = st.InTx(ctx, rs, func(tx *sql.Tx) error {
		var e error
		o.Run, e = agent.GetRunTx(ctx, tx, rs, runID)
		if e != nil {
			return e
		}
		o.Refs, e = agent.RefsTx(ctx, tx, rs, runID)
		if e != nil {
			return e
		}
		var artifactID string
		if e = tx.QueryRowContext(ctx, `SELECT id::text FROM supervisor.agent_artifacts WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3 AND schema=$4`, set.TenantID, set.SiteID, runID, agent.ChatArtifactSchema).Scan(&artifactID); e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if artifactID != "" {
			artifact, e := agent.LoadArtifactTx(ctx, tx, rs, runID, artifactID)
			if e != nil {
				return e
			}
			if artifact.Schema != agent.ChatArtifactSchema {
				return errors.New("correction answer artifact is not v2")
			}
			o.ArtifactSHA256 = artifact.SHA256
			if e = json.Unmarshal(artifact.Body, &o.Artifact); e != nil {
				return e
			}
		}
		return tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(input_tokens),0),coalesce(sum(output_tokens),0) FROM supervisor.agent_model_calls WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3 AND status='completed'`, set.TenantID, set.SiteID, runID).Scan(&o.ProviderCalls, &o.InputTokens, &o.OutputTokens)
	})
	if err != nil {
		return o, err
	}
	page, err := svc.Messages(ctx, actor, sc, conv.ConversationID, 100, "")
	if err != nil {
		return o, err
	}
	for _, m := range page.Messages {
		if m.MessageID == posted.Reply.MessageID {
			o.Answer = m
			return o, nil
		}
	}
	return o, errors.New("answer message unavailable")
}

func runCorrections(mode, path, outDir string, getenv func(string) string, stdout, stderr io.Writer) int {
	blocked := func(err error) int { fmt.Fprintln(stderr, "BLOCKED:", err); return 2 }
	set, fileSHA, err := loadCorrectionSet(path)
	if err != nil {
		return blocked(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	db, err := store.New(getenv("SUPERVISOR_TEST_DATABASE_URL"))
	if err != nil {
		return blocked(errors.New("correction database unavailable"))
	}
	defer db.Close()
	if err = db.VerifyRole(ctx); err != nil {
		return blocked(errors.New("correction database requires the non-owner runtime role"))
	}
	a, err := correctionActor(ctx, db.DB, getenv("SUPERVISOR_EVAL_CORRECTION_SESSION_FILE"))
	if err != nil {
		return blocked(err)
	}
	svc := knowledge.New(db.DB)
	cases, err := correctionCases(ctx, svc, a, set)
	if err != nil {
		return blocked(err)
	}
	snap, err := svc.ReadSnapshot(ctx, a, knowledge.Scope{TenantID: set.TenantID, SiteID: set.SiteID}, set.SnapshotID)
	if err != nil {
		return blocked(err)
	}
	cfg := fixtureAgent()
	var provider agent.Provider
	if mode == ModeActual {
		cfg, err = agentConfig(getenv)
		if err != nil {
			return blocked(err)
		}
		if cfg.Provider != "local" || cfg.Hosted.Provider != "" {
			return blocked(errors.New("correction actual evaluation requires the pinned local model"))
		}
		model, e := agent.NewLocalModel(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now)
		if e != nil {
			return blocked(e)
		}
		if err = model.Probe(ctx); err != nil {
			return blocked(err)
		}
		provider = model
		if err = agent.RequireActual(provider); err != nil {
			return blocked(err)
		}
	}
	_, datasetSHA, err := canonical.Envelope("CorrectionEvaluationSetV1", 1, set.TenantID, set.SiteID, struct {
		Selection      CorrectionSet
		SnapshotSHA256 string
		Cases          []knowledge.CorrectionEvaluationCase
	}{set, snap.SHA256, cases})
	if err != nil {
		return blocked(err)
	}
	r := Report{SchemaVersion: 1, Issue: "WIS-703", Mode: mode, Suites: []string{SuiteCorrections}, CorrectionSet: &set, Result: "PASS", Qualified: false, EvidenceState: "site correction regression only; not full model qualification", GitSHA: gitOutput("rev-parse", "HEAD"), GitDirty: gitDirty(""), Command: "supervisor-eval -case corrections", StartedAt: time.Now().UTC().Format(time.RFC3339), ManifestPath: path, ManifestSHA256: fileSHA, CorpusSHA256: datasetSHA, RunsPerCase: 1, Environment: map[string]string{"provider": cfg.Provider, "model": cfg.Model, "model_revision": cfg.ModelRevision, "prompt_version": agent.ChatPromptVersion, "tool_version": agent.ToolVersion}, Notes: []string{"Explicit immutable approved cases and published snapshot; no review or publication writes. The run does write to the evaluated site: one chat conversation per case, its messages, agent runs and model calls, as the session's member.", "manifest_path and manifest_sha256 name the correction set file, not the frozen qualification manifest.", "Conservative complete-statement and exact claim/source/decimal assertions; equivalent paraphrases may fail."}}
	if mode == ModeDeterministic {
		r.Label = Label
	} else {
		r.RunsPerCase = 3
	}
	h := harness{ctx: ctx, db: db, mode: mode, agentCfg: cfg, model: cfg.Model, revision: cfg.ModelRevision, actual: provider}
	for _, c := range cases {
		result := CaseResult{CaseID: c.CaseID, Category: SuiteCorrections, Partition: "site-regression", Label: r.Label}
		if reason := correctionSnapshot(c, snap); reason != "" {
			result.Failures = []string{reason}
			result.Runs = []RunResult{{Run: 0, Correction: &CorrectionObserved{Case: c, SnapshotSHA256: snap.SHA256}, Failures: []string{reason}}}
		} else {
			for n := 1; n <= r.RunsPerCase; n++ {
				o, e := executeCorrection(ctx, h, a, set, c, snap)
				run := RunResult{Run: n, Label: r.Label, Correction: &o}
				if e != nil {
					run.Error = e.Error()
					result.Errors = append(result.Errors, e.Error())
				} else {
					run.Failures = correctionAssertions(c, o)
					if o.Run.ProviderActual != (mode == ModeActual) || o.ProviderCalls == 0 || (mode == ModeActual && o.InputTokens+o.OutputTokens <= 0) {
						run.Failures = append(run.Failures, "PROVIDER_USAGE_PROVENANCE_MISMATCH")
					}
					result.Failures = append(result.Failures, run.Failures...)
				}
				result.Runs = append(result.Runs, run)
			}
		}
		r.Cases = append(r.Cases, result)
		for _, f := range append(result.Failures, result.Errors...) {
			r.Failures = append(r.Failures, c.CaseID+": "+f)
		}
	}
	r.Controls = correctionControls()
	for _, c := range r.Controls {
		if c.Problem != "" {
			r.Failures = append(r.Failures, c.Problem)
		}
	}
	if mode == ModeActual && r.GitDirty {
		r.Failures = append(r.Failures, "working tree has uncommitted changes")
	}
	exit := 0
	if len(r.Failures) > 0 {
		r.Result = "FAIL"
		exit = 1
	}
	if err = os.MkdirAll(outDir, 0700); err != nil {
		return blocked(err)
	}
	reportPath := filepath.Join(outDir, "supervisor-corrections-"+mode+"-"+time.Now().UTC().Format("20060102T150405Z")+"-"+uuid.NewString()[:8]+".json")
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return blocked(err)
	}
	if err = os.WriteFile(reportPath, append(body, '\n'), 0600); err != nil {
		return blocked(err)
	}
	fmt.Fprintf(stdout, "%s corrections %s; qualified=false; report %s\n", mode, r.Result, reportPath)
	for _, f := range r.Failures {
		fmt.Fprintln(stderr, f)
	}
	return exit
}

// Negative controls exercise the judge with fully cited but deliberately false observations.
func correctionControlSample() (knowledge.CorrectionEvaluationCase, CorrectionObserved) {
	c := knowledge.CorrectionEvaluationCase{ClaimID: "00000000-0000-4000-8000-000000000703", ClaimRevision: 2, ExpectedAnswer: "The cycle time is 45 seconds.", Claim: knowledge.ClaimInput{SourceSpanIDs: []string{"00000000-0000-4000-8000-000000000704"}, Literal: &knowledge.Literal{Type: "decimal", Value: json.RawMessage(`"45"`), Unit: "s"}}}
	ref := agent.ChatClaimRef{ClaimID: c.ClaimID, Revision: 2}
	cite := agent.ChatClaimCitation{ChatClaimRef: ref, Statement: 0, SourceSpanIDs: slices.Clone(c.Claim.SourceSpanIDs)}
	body := "- [Approved process] " + c.ExpectedAnswer + " [1]"
	o := CorrectionObserved{Answer: chat.Message{State: "answered", Body: &body, ClaimCitations: []agent.ChatClaimCitation{cite}}, Artifact: agent.ChatAnswerArtifactV2{Status: "answered", Statements: []agent.ChatStatement{{Kind: "approved_fact", Text: c.ExpectedAnswer, SourceSpanIDs: slices.Clone(c.Claim.SourceSpanIDs), ClaimRefs: []agent.ChatClaimRef{ref}}}, ClaimCitations: []agent.ChatClaimCitation{cite}}}
	return c, o
}

func correctionControls() []ControlResult {
	out := []ControlResult{}
	for _, name := range []string{"old-answer", "wrong-citation", "negated-answer", "contradictory-answer", "abstention"} {
		c, o := correctionControlSample()
		control := ControlResult{CaseID: "correction-control-" + name, ExpectedFailure: "strict correction assertion", Outcome: "FAIL"}
		if f := correctionAssertions(c, o); len(f) > 0 {
			control.Problem = "positive correction control failed: " + strings.Join(f, ",")
		}
		switch name {
		case "old-answer":
			o.Artifact.Statements[0].Text = "The cycle time is 60 seconds."
		case "wrong-citation":
			o.Artifact.Statements[0].ClaimRefs[0].Revision = 1
		case "negated-answer":
			o.Artifact.Statements[0].Text = "The cycle time is not 45 seconds."
		case "contradictory-answer":
			o.Artifact.Statements[0].Text = "The cycle time is 45 seconds, but actually 60 seconds."
		case "abstention":
			o.Answer.State = "needs_input"
			o.Artifact.Status = "needs_input"
		}
		body := "- [Approved process] " + o.Artifact.Statements[0].Text + " [1]"
		o.Answer.Body = &body
		control.Reasons = correctionAssertions(c, o)
		if len(control.Reasons) == 0 {
			control.Outcome = "PASS"
			control.Problem = "negative correction control falsely passed: " + name
		}
		out = append(out, control)
	}
	return out
}

func providerName(mode string) string {
	if mode == ModeActual {
		return agent.ProviderLocal
	}
	return agent.ProviderFixture
}
