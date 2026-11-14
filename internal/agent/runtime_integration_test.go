//go:build integration

package agent_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/approvals"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/cases"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/dispatch"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
)

// openRun creates a running case run with the captured scope exactly as Investigate does, and the
// server-built investigation child envelope, for driving Tools.Call directly.
func (w *world) openRun(t *testing.T, limits agent.Limits) (agent.ToolServerContext, agent.DelegationEnvelopeV1) {
	t.Helper()
	rev := w.fresh(t)
	runID := uuid.NewString()
	sc := agent.RunScope{TenantID: w.tenant, SiteID: w.site}
	required := []string{}
	var contextID string
	err := w.store.InTx(w.ctx, sc, func(tx *sql.Tx) error {
		run := agent.Run{ID: runID, RequestKind: agent.RequestCase, CaseID: w.caseID, CaseRevision: rev, TraceID: agent.NewTraceID(), ProviderName: agent.ProviderFixture,
			Model: "fixture-model", ModelRevision: "sha256:fixture", PromptVersion: agent.PromptVersion, ToolVersion: agent.ToolVersion, PolicyRevision: 1}
		if e := agent.CreateRunTx(w.ctx, tx, sc, run, limits); e != nil {
			return e
		}
		in, e := agent.LoadCaseInput(w.ctx, tx, sc, w.caseID, agent.LoaderOptions{ModeEnabled: true})
		if e != nil {
			return e
		}
		contextID = in.ContextID
		for _, r := range in.Refs {
			if r.Kind == agent.RefEvidence || r.Kind == agent.RefMeasurement {
				required = append(required, r.RefID)
			}
		}
		return agent.AddRefsTx(w.ctx, tx, sc, runID, in.Refs)
	})
	if err != nil {
		t.Fatal(err)
	}
	env := agent.DelegationEnvelopeV1{SchemaVersion: 1, RunID: uuid.NewSHA1(uuid.MustParse(runID), []byte(agent.RoleInvestigation)).String(), ParentRunID: &runID,
		Role: agent.RoleInvestigation, RequestKind: agent.RequestCase, CaseID: w.caseID, TenantID: w.tenant, SiteID: w.site, ContextSnapshotID: &contextID,
		RequiredEvidenceRefs: required, AllowedToolNames: []string{agent.ToolContextGet, agent.ToolEvidenceGet, agent.ToolClaimsSearch, agent.ToolMeasurementsGet},
		PrincipalReference: runID, PolicyRevision: 1, Deadline: time.Now().UTC().Add(4 * time.Minute), RemainingRequests: 6, RemainingTokens: 24000,
		RemainingCostMicrousd: 1_000_000, CancellationGeneration: rev, Hop: 1}
	return agent.ToolServerContext{TenantID: w.tenant, SiteID: w.site, RunID: runID, Generation: rev, Role: agent.RoleInvestigation}, env
}

func toolCode(err error) string {
	var te *agent.ToolError
	switch {
	case err == nil:
		return "ok"
	case errors.As(err, &te):
		return te.Code
	default:
		return err.Error()
	}
}

func (w *world) agentAudits(t *testing.T, tenant, source string) int {
	t.Helper()
	var n int
	if err := w.control.QueryRowContext(w.ctx, `SELECT count(*) FROM supervisor.evidence_audit WHERE tenant_id=$1 AND source_id=$2 AND actor_issuer=$3`, tenant, source, agent.ServiceActor.Issuer).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func want(t *testing.T, got runRow, state, status, reasons string) {
	t.Helper()
	if got.state != state || got.status != status || got.reasons != reasons {
		t.Fatalf("run = %s/%s/%s, want %s/%s/%s (%+v)", got.state, got.status, got.reasons, state, status, reasons, got)
	}
}

// TestWIS275Gate is the issue's required table-driven gate (make supervisor-integration
// CASE=WIS-275). Every scenario builds its own investigating case in its own database through the
// real WIS-272/273 path, uses only the deterministic fixture provider, and pairs the attack with a
// control that differs only in the attack.
func TestWIS275Gate(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, w *world)
	}{
		{"cross_tenant", func(t *testing.T, w *world) {
			// A tenant-B source ID, then an unknown ID: both read exactly alike, and the second miss
			// stops the run. Tenant B gets no audit row.
			rev := w.revision
			p := script(toolStep(agent.ToolEvidenceGet, `{"evidence_id":"`+w.otherSource+`"}`), toolStep(agent.ToolEvidenceGet, `{"evidence_id":"`+uuid.NewString()+`"}`))
			res := w.investigate(t, w.runtime(p, nil), rev)
			got := w.row(t, res.RunID)
			want(t, got, agent.RunCompleted, recovery.StatusRefused, agent.CodeToolRefNotInScope)
			if got.toolCodes != "unavailable:TOOL_REF_NOT_IN_SCOPE,unavailable:TOOL_REF_NOT_IN_SCOPE" || w.agentAudits(t, w.otherTenant, w.otherSource) != 0 {
				t.Fatalf("tool rows %q, tenant-B audits %d", got.toolCodes, w.agentAudits(t, w.otherTenant, w.otherSource))
			}
			// Citing the tenant-B ID rejects the whole output.
			cite := w.result("success")
			cite.CitedEvidenceIDs = append(cite.CitedEvidenceIDs, w.otherSource)
			res = w.investigate(t, w.runtime(script(finalStep(t, cite, w.clarify(w.fresh(t)))), nil), w.revision)
			if got := w.row(t, res.RunID); got.status != recovery.StatusRefused || got.reasons != recovery.ReasonSchemaInvalid || got.artifact {
				t.Fatalf("foreign citation: %+v", got)
			}
			// Control: the captured tenant-A source returns its text, audited as the agent.
			sc, env := w.openRun(t, agent.DefaultLimits())
			out, err := w.runtime(nil, nil).Tools.Call(w.ctx, sc, env, agent.ToolEvidenceGet, []byte(`{"evidence_id":"`+w.schedule+`"}`))
			if err != nil || !strings.Contains(string(out.Content), "untrusted_source") || w.agentAudits(t, w.tenant, w.schedule) != 1 {
				t.Fatalf("control: %v %s", err, out.Content)
			}
		}},
		{"forged_delegation", func(t *testing.T, w *world) {
			tools := &agent.Tools{Store: w.store}
			args := []byte(`{"evidence_id":"` + w.schedule + `"}`)
			for name, forge := range map[string]func(e *agent.DelegationEnvelopeV1){
				"other tenant":    func(e *agent.DelegationEnvelopeV1) { e.TenantID, e.SiteID = w.otherTenant, w.otherSite },
				"other principal": func(e *agent.DelegationEnvelopeV1) { e.PrincipalReference = uuid.NewString() },
				"orchestrator":    func(e *agent.DelegationEnvelopeV1) { e.Role, e.Hop, e.ParentRunID = agent.RoleOrchestrator, 0, nil },
				"other parent":    func(e *agent.DelegationEnvelopeV1) { p := uuid.NewString(); e.ParentRunID = &p },
				"old generation":  func(e *agent.DelegationEnvelopeV1) { e.CancellationGeneration-- },
			} {
				sc, env := w.openRun(t, agent.DefaultLimits())
				forge(&env)
				if _, err := tools.Call(w.ctx, sc, env, agent.ToolEvidenceGet, args); toolCode(err) != agent.CodeScopeDenied {
					t.Fatalf("%s: %v", name, err)
				}
				w.endRun(t, sc) // scaffolding only: it must not hold one of the site's investigation places
			}
			// A model result naming another run or role is refused whole.
			for _, forge := range []func(r *agent.SpecialistResultV1){
				func(r *agent.SpecialistResultV1) { r.RunID = uuid.NewString() },
				func(r *agent.SpecialistResultV1) { r.Role = agent.RoleProposal },
			} {
				r := w.result("success")
				forge(&r)
				rev := w.fresh(t)
				res := w.investigate(t, w.runtime(script(finalStep(t, r, w.clarify(rev))), nil), rev)
				if got := w.row(t, res.RunID); got.status != recovery.StatusRefused || got.reasons != recovery.ReasonSchemaInvalid || got.artifact {
					t.Fatalf("forged result: %+v", got)
				}
			}
			// Control: the server-built child reads.
			sc, env := w.openRun(t, agent.DefaultLimits())
			if _, err := tools.Call(w.ctx, sc, env, agent.ToolEvidenceGet, args); err != nil {
				t.Fatalf("control: %v", err)
			}
		}},
		{"instruction_in_source", func(t *testing.T, w *world) {
			// The script reads the poisoned source and obeys it: the smuggled tenant_id is rejected,
			// the run stops, nothing is stored.
			rev := w.revision
			p := script(toolStep(agent.ToolEvidenceGet, `{"evidence_id":"`+w.schedule+`"}`), toolStep(agent.ToolMeasurementsGet, `{"tenant_id":"`+w.otherTenant+`"}`),
				finalStep(t, w.result("success"), w.clarify(rev)))
			res := w.investigate(t, w.runtime(p, nil), rev)
			got := w.row(t, res.RunID)
			want(t, got, agent.RunCompleted, recovery.StatusRefused, agent.CodeToolArgumentRejected)
			if got.artifact || p.Calls() != 2 {
				t.Fatalf("calls=%d %+v", p.Calls(), got)
			}
			// The source text reached the model only as data inside untrusted_source.
			var read struct {
				Passages []struct {
					Untrusted string `json:"untrusted_source"`
				} `json:"passages"`
			}
			last := p.requests[1].Messages[len(p.requests[1].Messages)-1]
			if last.Role != "tool" || json.Unmarshal([]byte(last.Content), &read) != nil || (len(read.Passages) != 1 || !strings.Contains(read.Passages[0].Untrusted, injection)) {
				t.Fatalf("tool message: %+v", last)
			}
			// Control: same source, the script ignores the instruction.
			rev = w.fresh(t)
			p = script(toolStep(agent.ToolEvidenceGet, `{"evidence_id":"`+w.schedule+`"}`), finalStep(t, w.result("success"), w.clarify(rev)))
			res = w.investigate(t, w.runtime(p, nil), rev)
			if got := w.row(t, res.RunID); got.status != recovery.StatusNeedsInput || got.reasons != "CLARIFICATION_REQUESTED" || !got.artifact {
				t.Fatalf("control: %+v", got)
			}
		}},
		{"schema_valid_malicious_payload", func(t *testing.T, w *world) {
			for name, c := range map[string]struct {
				forge  func(p *recovery.RecoveryProposalV1)
				reason string
			}{
				"unrostered assignee": {func(p *recovery.RecoveryProposalV1) { maintenance(w, p, w.contexts[1].AssetID) }, recovery.RefuseNeedsClarify},
				"deficit mismatch":    {func(p *recovery.RecoveryProposalV1) { p.Observations[0].ValueDecimal = "2" }, recovery.RefuseNumericMismatch},
				"task on other line":  {func(p *recovery.RecoveryProposalV1) { maintenance(w, p, uuid.NewString()) }, recovery.RefuseTaskScope},
			} {
				rev := w.fresh(t)
				prop := w.clarify(rev)
				c.forge(&prop)
				res := w.investigate(t, w.runtime(script(finalStep(t, w.result("success"), prop)), nil), rev)
				got := w.row(t, res.RunID)
				if got.status != recovery.StatusRefused || !strings.Contains(got.reasons, c.reason) || got.artifact || res.ArtifactID != "" {
					t.Fatalf("%s: %+v", name, got)
				}
			}
			// Control: a valid clarify proposal is stored and waits for input.
			rev := w.fresh(t)
			res := w.investigate(t, w.runtime(script(finalStep(t, w.result("success"), w.clarify(rev))), nil), rev)
			if got := w.row(t, res.RunID); got.status != recovery.StatusNeedsInput || !got.artifact {
				t.Fatalf("control: %+v", got)
			}
		}},
		{"stale_scope", func(t *testing.T, w *world) {
			// The tenant is deactivated while the first call is in flight: its result is discarded
			// and the next turn makes no provider call.
			rev := w.revision
			p := script(toolStep(agent.ToolMeasurementsGet, `{}`), finalStep(t, w.result("success"), w.clarify(rev)))
			p.before = func(n int) {
				if n == 1 {
					w.exec(t, `UPDATE supervisor.tenants SET deactivated_at=now() WHERE id=$1`, w.tenant)
				}
			}
			res := w.investigate(t, w.runtime(p, nil), rev)
			w.exec(t, `UPDATE supervisor.tenants SET deactivated_at=NULL WHERE id=$1`, w.tenant)
			got := w.row(t, res.RunID)
			want(t, got, agent.RunCompleted, recovery.StatusNeedsInput, agent.CodeScopeDenied)
			if p.Calls() != 1 || got.toolCalls != 0 {
				t.Fatalf("calls=%d %+v", p.Calls(), got)
			}
			// Late data changes a captured context's inputs mid-run: STALE_SCOPE, no further call.
			rev = w.fresh(t)
			p = script(toolStep(agent.ToolMeasurementsGet, `{}`), finalStep(t, w.result("success"), w.clarify(rev)))
			p.before = func(n int) {
				if n == 1 {
					w.sendLateDelta()
				}
			}
			res = w.investigate(t, w.runtime(p, nil), rev)
			if got := w.row(t, res.RunID); got.reasons != agent.CodeStaleScope || p.Calls() != 1 {
				t.Fatalf("superseded: calls=%d %+v", p.Calls(), got)
			}
		}},
		{"stale_scope_control", func(t *testing.T, w *world) {
			rev := w.revision
			p := script(toolStep(agent.ToolMeasurementsGet, `{}`), finalStep(t, w.result("success"), w.clarify(rev)))
			res := w.investigate(t, w.runtime(p, nil), rev)
			if got := w.row(t, res.RunID); p.Calls() != 2 || got.toolCodes != "ok:" || got.status != recovery.StatusNeedsInput {
				t.Fatalf("calls=%d %+v", p.Calls(), got)
			}
		}},
		{"cancellation", func(t *testing.T, w *world) {
			// The case moves (a clarification bumps its revision) between a tool's read and its
			// post-read check: the read is discarded and the run ends canceled with no outcome.
			rev := w.revision
			rt := w.runtime(script(toolStep(agent.ToolMeasurementsGet, `{}`), finalStep(t, w.result("success"), w.clarify(rev))), nil)
			once := sync.Once{}
			agent.SetAfterRead(rt.Tools, func() { once.Do(func() { w.move(t, "investigating", "needs_input") }) })
			res := w.investigate(t, rt, rev)
			got := w.row(t, res.RunID)
			want(t, got, agent.RunCanceled, "", agent.CodeRunCanceled)
			if got.toolCodes != "canceled:RUN_CANCELED" || got.artifact {
				t.Fatalf("%+v", got)
			}
			w.move(t, "needs_input", "investigating")
			// The case moves while the final answer is in flight: nothing is committed.
			rev = w.revision
			p := script(finalStep(t, w.result("success"), w.clarify(rev)))
			p.before = func(int) { w.move(t, "investigating", "needs_input") }
			res = w.investigate(t, w.runtime(p, nil), rev)
			if got := w.row(t, res.RunID); got.state != agent.RunCanceled || got.status != "" || got.artifact {
				t.Fatalf("before commit: %+v", got)
			}
			w.move(t, "needs_input", "investigating")
			// Control: no move, the artifact is stored.
			rev = w.revision
			res = w.investigate(t, w.runtime(script(toolStep(agent.ToolMeasurementsGet, `{}`), finalStep(t, w.result("success"), w.clarify(rev))), nil), rev)
			if got := w.row(t, res.RunID); got.state != agent.RunCompleted || !got.artifact || got.toolCodes != "ok:" {
				t.Fatalf("control: %+v", got)
			}
		}},
		{"unknown_tool", func(t *testing.T, w *world) {
			// The model asks for a tool it was never offered: non-retryable, no further call.
			rev := w.revision
			p := script(toolStep("shell.exec", `{"cmd":"id"}`), finalStep(t, w.result("success"), w.clarify(rev)))
			res := w.investigate(t, w.runtime(p, nil), rev)
			got := w.row(t, res.RunID)
			want(t, got, agent.RunCompleted, recovery.StatusRefused, agent.CodeToolNotAllowed)
			if p.Calls() != 1 || got.toolCalls != 0 {
				t.Fatalf("calls=%d %+v", p.Calls(), got)
			}
			// The executor itself refuses names outside envelope ∩ role ∩ policy.
			args := []byte(`{"evidence_id":"` + w.schedule + `"}`)
			sc, env := w.openRun(t, agent.DefaultLimits())
			if _, err := (&agent.Tools{Store: w.store}).Call(w.ctx, sc, env, "sql.query", []byte(`{}`)); toolCode(err) != agent.CodeToolNotAllowed {
				t.Fatalf("sql.query: %v", err)
			}
			if _, err := (&agent.Tools{Store: w.store, Policy: []string{agent.ToolMeasurementsGet}}).Call(w.ctx, sc, env, agent.ToolEvidenceGet, args); toolCode(err) != agent.CodeToolNotAllowed {
				t.Fatalf("dropped by policy: %v", err)
			}
			if got := w.row(t, sc.RunID); got.toolCodes != "rejected:TOOL_NOT_ALLOWED,rejected:TOOL_NOT_ALLOWED" {
				t.Fatalf("recorded: %q", got.toolCodes)
			}
			// Control: an allowed tool reads.
			if _, err := (&agent.Tools{Store: w.store}).Call(w.ctx, sc, env, agent.ToolEvidenceGet, args); err != nil {
				t.Fatalf("control: %v", err)
			}
			proposalSC, proposalEnv := w.openRun(t, agent.DefaultLimits())
			proposalSC.Role, proposalEnv.Role = agent.RoleProposal, agent.RoleProposal
			if _, err := (&agent.Tools{Store: w.store}).Call(w.ctx, proposalSC, proposalEnv, agent.ToolClaimsSearch, []byte(`{"entity_id":"`+w.contexts[1].AssetID+`"}`)); toolCode(err) != agent.CodeToolNotAllowed {
				t.Fatalf("proposal role claims.search: %v", err)
			}
		}},
		{"shared_budget_race", func(t *testing.T, w *world) {
			// Two parallel tool calls on one run with one tool call left: exactly one runs.
			for _, left := range []int{1, 2} {
				l := agent.DefaultLimits()
				l.ToolCalls = left
				sc, env := w.openRun(t, l)
				tools := &agent.Tools{Store: w.store}
				codes := make([]string, 2)
				var wg sync.WaitGroup
				for i := range codes {
					wg.Add(1)
					go func() {
						defer wg.Done()
						_, err := tools.Call(w.ctx, sc, env, agent.ToolMeasurementsGet, []byte(`{}`))
						codes[i] = toolCode(err)
					}()
				}
				wg.Wait()
				ok := strings.Count(strings.Join(codes, ","), "ok")
				var remaining int
				if err := w.control.QueryRow(`SELECT tool_calls_left FROM supervisor.agent_runs WHERE id=$1`, sc.RunID).Scan(&remaining); err != nil {
					t.Fatal(err)
				}
				if ok != left || remaining != 0 || (left == 1 && !slicesContain(codes, agent.CodeBudgetExhausted)) {
					t.Fatalf("left=%d codes=%v remaining=%d", left, codes, remaining)
				}
			}
			// Two parallel provider reservations that only one fits: exactly one succeeds.
			l := agent.DefaultLimits()
			l.Tokens = 2*(1000+2000) - 1
			sc, _ := w.openRun(t, l)
			rs := agent.RunScope{TenantID: w.tenant, SiteID: w.site}
			errs := make([]error, 2)
			var wg sync.WaitGroup
			for i := range errs {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, errs[i] = agent.ReserveRequest(w.ctx, w.store, rs, sc.RunID, l, agent.RequestPlan{Seq: i + 1, Attempt: 1, SpanID: agent.NewSpanID(), Model: "fixture-model",
						ModelRevision: "sha256:fixture", RequestSHA256: strings.Repeat("ab", 32), RequestBytes: 3000, Pricing: agent.Pricing{Local: true}})
				}()
			}
			wg.Wait()
			if (errs[0] == nil) == (errs[1] == nil) || !(errors.Is(errs[0], agent.ErrBudgetExhausted) || errors.Is(errs[1], agent.ErrBudgetExhausted)) {
				t.Fatalf("reservations: %v", errs)
			}
		}},
		{"retry_ceiling", func(t *testing.T, w *world) {
			unavailable := agent.FixtureStep{Err: agent.RetryableProviderError(agent.CodeProviderFailed, 503)}
			malformed := agent.FixtureStep{Response: agent.ModelResponse{Content: `{"result":`, InputTokens: 900, OutputTokens: 3}}
			for name, c := range map[string]struct {
				steps          []agent.FixtureStep
				status, reason string
			}{
				"503 twice":       {[]agent.FixtureStep{unavailable, unavailable, finalStep(t, w.result("success"), nil)}, recovery.StatusEscalated, recovery.ReasonProviderFailed},
				"malformed twice": {[]agent.FixtureStep{malformed, malformed, finalStep(t, w.result("success"), nil)}, recovery.StatusRefused, recovery.ReasonSchemaInvalid},
			} {
				rev := w.fresh(t)
				p := script(c.steps...)
				rt := w.runtime(p, nil)
				res := w.investigate(t, rt, rev)
				got := w.row(t, res.RunID)
				want(t, got, agent.RunDegraded, c.status, c.reason)
				if p.Calls() != 2 || got.modelCalls != 2 || got.artifact {
					t.Fatalf("%s: calls=%d %+v", name, p.Calls(), got)
				}
				// A re-invoked activity replays the terminal run: no new provider call.
				for i := 0; i < 3; i++ {
					if again := w.investigate(t, rt, rev); !again.Replayed || again.RunID != res.RunID || p.Calls() != 2 {
						t.Fatalf("%s replay %d: %+v calls=%d", name, i, again, p.Calls())
					}
				}
			}
			// Control: 503 then success uses the single narrowed retry and charges both calls.
			rev := w.fresh(t)
			p := script(unavailable, finalStep(t, w.result("success"), w.clarify(rev)))
			res := w.investigate(t, w.runtime(p, nil), rev)
			if got := w.row(t, res.RunID); got.state != agent.RunCompleted || got.modelCalls != 2 || !got.artifact {
				t.Fatalf("control: %+v", got)
			}
		}},
		{"contradiction", func(t *testing.T, w *world) {
			// Contradictions claimed as success are invalid; as needs_input they stay needs_input
			// and their accompanying model proposal is ignored. Null-proposal clarifications are judged separately.
			claimed := w.result("success")
			claimed.Contradictions = []string{"The counter and the line log disagree."}
			rev := w.fresh(t)
			res := w.investigate(t, w.runtime(script(finalStep(t, claimed, w.clarify(rev))), nil), rev)
			want(t, w.row(t, res.RunID), agent.RunCompleted, recovery.StatusRefused, recovery.ReasonSchemaInvalid)
			flagged := w.result("needs_input")
			flagged.Contradictions = claimed.Contradictions
			rev = w.fresh(t)
			res = w.investigate(t, w.runtime(script(finalStep(t, flagged, w.clarify(rev))), nil), rev)
			got := w.row(t, res.RunID)
			want(t, got, agent.RunCompleted, recovery.StatusNeedsInput, agent.CodeContradictoryEvidence)
			if got.artifact {
				t.Fatalf("%+v", got)
			}
			// Control: no contradictions, the Judge path decides.
			rev = w.fresh(t)
			res = w.investigate(t, w.runtime(script(finalStep(t, w.result("success"), w.clarify(rev))), nil), rev)
			want(t, w.row(t, res.RunID), agent.RunCompleted, recovery.StatusNeedsInput, "CLARIFICATION_REQUESTED")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.run(t, newWorldOpts(t, worldOpts{poisonedPlanSpan: tc.name == "instruction_in_source"}))
		})
	}
}

// TestWIS275InventedCitationRejectsWholeOutput: one invented ID anywhere refuses everything.
func TestWIS275InventedCitationRejectsWholeOutput(t *testing.T) {
	w := newWorld(t)
	invented := uuid.NewString()
	inResult := w.result("success")
	inResult.CitedEvidenceIDs = append(inResult.CitedEvidenceIDs, invented)
	rev := w.revision
	res := w.investigate(t, w.runtime(script(finalStep(t, inResult, w.clarify(rev))), nil), rev)
	if got := w.row(t, res.RunID); got.status != recovery.StatusRefused || got.reasons != recovery.ReasonSchemaInvalid || got.artifact {
		t.Fatalf("result citation: %+v", got)
	}
	rev = w.fresh(t)
	prop := w.clarify(rev)
	prop.Options[0].EvidenceIDs = append(prop.Options[0].EvidenceIDs, invented)
	res = w.investigate(t, w.runtime(script(finalStep(t, w.result("success"), prop)), nil), rev)
	if got := w.row(t, res.RunID); got.status != recovery.StatusRefused || got.reasons != recovery.RefuseForeignCitation || got.artifact {
		t.Fatalf("proposal citation: %+v", got)
	}
	// The model's proposal_id is never kept: the stored artifact has none.
	rev = w.fresh(t)
	res = w.investigate(t, w.runtime(script(finalStep(t, w.result("success"), w.clarify(rev))), nil), rev)
	var body []byte
	if err := w.control.QueryRow(`SELECT body::text FROM supervisor.agent_artifacts WHERE id=$1`, res.ArtifactID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var stored recovery.RecoveryProposalV1
	if err := json.Unmarshal(body, &stored); err != nil || stored.ProposalID != "" || stored.CaseRevision != rev {
		t.Fatalf("artifact: %v %s", err, body)
	}
}

// TestWIS275SmuggledToolArguments: tenant, site, actor and principal fields in any case are rejected.
func TestWIS275SmuggledToolArguments(t *testing.T) {
	w := newWorld(t)
	tools := &agent.Tools{Store: w.store}
	for _, args := range []string{
		`{"evidence_id":"` + w.schedule + `","tenant_id":"` + w.otherTenant + `"}`,
		`{"evidence_id":"` + w.schedule + `","Tenant_ID":"` + w.otherTenant + `"}`,
		`{"evidence_id":"` + w.schedule + `","site_id":"` + w.otherSite + `"}`,
		`{"evidence_id":"` + w.schedule + `","actor":"supervisor"}`,
		`{"evidence_id":"` + w.schedule + `","principal":"` + uuid.NewString() + `"}`,
		`{"evidence_id":"` + w.schedule + `","evidence_id":"` + w.otherSource + `"}`,
	} {
		sc, env := w.openRun(t, agent.DefaultLimits())
		if _, err := tools.Call(w.ctx, sc, env, agent.ToolEvidenceGet, []byte(args)); toolCode(err) != agent.CodeToolArgumentRejected {
			t.Fatalf("%s: %v", args, err)
		}
		if w.agentAudits(t, w.tenant, w.schedule) != 0 {
			t.Fatalf("%s was read", args)
		}
	}
	sc, env := w.openRun(t, agent.DefaultLimits())
	if _, err := tools.Call(w.ctx, sc, env, agent.ToolEvidenceGet, []byte(`{"evidence_id":"`+w.schedule+`"}`)); err != nil {
		t.Fatalf("control: %v", err)
	}
}

// TestWIS275RevokedEvidenceMidRun: a revocation that wins during a call discards the result and
// records revoked_after_dispatch; a tool read after revocation returns no bytes.
func TestWIS275RevokedEvidenceMidRun(t *testing.T) {
	revoke := func(w *world) {
		es := evidence.Scope{TenantID: w.tenant, SiteID: w.site}
		head, err := w.ev.Read(w.ctx, w.ea, es, w.schedule, 0)
		if err == nil {
			_, err = w.ev.Revoke(w.ctx, w.ea, es, w.schedule, "Withdrawn by the owner", uuid.NewString(), head.StateRevision)
		}
		if err != nil {
			t.Error(err)
		}
	}
	w := newWorld(t)
	rev := w.revision
	p := script(toolStep(agent.ToolEvidenceGet, `{"evidence_id":"`+w.schedule+`"}`), finalStep(t, w.result("success"), w.clarify(rev)))
	p.before = func(n int) {
		if n == 2 {
			revoke(w)
		}
	}
	res := w.investigate(t, w.runtime(p, nil), rev)
	got := w.row(t, res.RunID)
	want(t, got, agent.RunCompleted, recovery.StatusNeedsInput, agent.CodeEvidenceRevoked)
	if !got.revoked || got.artifact || p.Calls() != 2 || got.toolCodes != "ok:" {
		t.Fatalf("calls=%d %+v", p.Calls(), got)
	}

	// Answer-phase provider (Codex review P2 on 8ff2ab9): revoked while the draft is in flight, the
	// answer call is refused before dispatch, and the run still records an after-dispatch revocation.
	w = newWorld(t)
	rev = w.revision
	ph := phased{script(toolStep(agent.ToolEvidenceGet, `{"evidence_id":"`+w.schedule+`"}`), finalStep(t, w.result("success"), w.clarify(rev)), finalStep(t, w.result("success"), w.clarify(rev)))}
	ph.before = func(n int) {
		if n == 2 {
			revoke(w)
		}
	}
	res = w.investigate(t, w.runtime(ph, nil), rev)
	got = w.row(t, res.RunID)
	want(t, got, agent.RunCompleted, recovery.StatusNeedsInput, agent.CodeEvidenceRevoked)
	var reason string
	if err := w.control.QueryRowContext(w.ctx, `SELECT COALESCE(revocation_reason,'') FROM supervisor.agent_runs WHERE id=$1`, res.RunID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if !got.revoked || reason != agent.CodeEvidenceRevoked || got.artifact || ph.Calls() != 2 || got.modelCalls != 2 {
		t.Fatalf("phased: calls=%d reason=%q %+v", ph.Calls(), reason, got)
	}

	w = newWorld(t)
	sc, env := w.openRun(t, agent.DefaultLimits())
	revoke(w)
	out, err := (&agent.Tools{Store: w.store}).Call(w.ctx, sc, env, agent.ToolEvidenceGet, []byte(`{"evidence_id":"`+w.schedule+`"}`))
	if toolCode(err) != agent.CodeEvidenceUnavailable || len(out.Content) != 0 || w.row(t, sc.RunID).toolCodes != "unavailable:EVIDENCE_UNAVAILABLE" {
		t.Fatalf("after revocation: %v %s", err, out.Content)
	}
}

// TestWIS275RechecksAfterQueueWait: revocation while waiting for a provider slot prevents the
// dispatch and leaves the provider request budget untouched.
func TestWIS275RechecksAfterQueueWait(t *testing.T) {
	w := newWorld(t)
	rev := w.revision
	p := script(finalStep(t, w.result("success"), w.clarify(rev)))
	rt := w.runtime(p, nil)
	rt.Slots = agent.NewSlots(1, 1)
	release, err := rt.Slots.Acquire(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	held := true
	t.Cleanup(func() {
		if held {
			release()
		}
	})
	type investigation struct {
		result agent.Result
		err    error
	}
	done := make(chan investigation, 1)
	go func() {
		result, e := rt.Investigate(w.ctx, agent.InvestigateRef{TenantID: w.tenant, SiteID: w.site, CaseID: w.caseID}, rev)
		done <- investigation{result: result, err: e}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for rt.Slots.Waiting() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if rt.Slots.Waiting() != 1 {
		t.Fatal("investigation did not enter the provider queue")
	}
	es := evidence.Scope{TenantID: w.tenant, SiteID: w.site}
	head, err := w.ev.Read(w.ctx, w.ea, es, w.schedule, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.ev.Revoke(w.ctx, w.ea, es, w.schedule, "Withdrawn while queued", uuid.NewString(), head.StateRevision); err != nil {
		t.Fatal(err)
	}
	release()
	held = false
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.result.Status != recovery.StatusNeedsInput || len(got.result.Reasons) != 1 || got.result.Reasons[0] != agent.CodeEvidenceRevoked {
			t.Fatalf("outcome %+v, want needs_input EVIDENCE_REVOKED", got.result)
		}
		row := w.row(t, got.result.RunID)
		var requests int
		if err := w.control.QueryRowContext(w.ctx, `SELECT requests_left FROM supervisor.agent_runs WHERE id=$1`, got.result.RunID).Scan(&requests); err != nil {
			t.Fatal(err)
		}
		if p.Calls() != 0 || row.modelCalls != 0 || requests != agent.DefaultLimits().Requests {
			t.Fatalf("revoked queued run dispatched: calls=%d model_calls=%d requests_left=%d", p.Calls(), row.modelCalls, requests)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("investigation did not finish after releasing the slot")
	}
}

// TestWIS275ResumeScopeFailureFinishesRun: a resumed run whose case is no longer visible is
// terminally failed instead of being left running behind a non-retryable scope error.
func TestWIS275ResumeScopeFailureFinishesRun(t *testing.T) {
	w := newWorld(t)
	runID, missingCase := uuid.NewString(), uuid.NewString()
	sc := agent.RunScope{TenantID: w.tenant, SiteID: w.site}
	if err := w.store.InTx(w.ctx, sc, func(tx *sql.Tx) error {
		return agent.CreateRunTx(w.ctx, tx, sc, agent.Run{ID: runID, RequestKind: agent.RequestCase, CaseID: w.caseID, CaseRevision: w.revision,
			TraceID: agent.NewTraceID(), ProviderName: agent.ProviderFixture, Model: "fixture-model", ModelRevision: "sha256:fixture",
			PromptVersion: agent.PromptVersion, ToolVersion: agent.ToolVersion, PolicyRevision: 1}, agent.DefaultLimits())
	}); err != nil {
		t.Fatal(err)
	}
	conn, err := w.control.Conn(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	tx, err := conn.BeginTx(w.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(w.ctx, `SET LOCAL session_replication_role='replica'`); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(w.ctx, `UPDATE supervisor.agent_runs SET case_id=$2 WHERE tenant_id=$1 AND site_id=$3 AND id=$4`, w.tenant, missingCase, w.site, runID); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	p := script()
	got, err := w.runtime(p, nil).Investigate(w.ctx, agent.InvestigateRef{TenantID: w.tenant, SiteID: w.site, CaseID: missingCase}, w.revision)
	if err != nil {
		t.Fatal(err)
	}
	if got.RunID != runID || got.State != agent.RunFailed || got.Status != recovery.StatusEscalated || len(got.Reasons) != 1 || got.Reasons[0] != agent.CodeScopeDenied || got.Replayed || p.Calls() != 0 {
		t.Fatalf("resumed missing-scope result %+v calls=%d", got, p.Calls())
	}
	row := w.row(t, runID)
	if row.state != agent.RunFailed || row.status != recovery.StatusEscalated || row.reasons != agent.CodeScopeDenied {
		t.Fatalf("persisted run outcome %+v", row)
	}
}

func TestWIS275OverReservationReturnsTerminalOutcome(t *testing.T) {
	w := newWorld(t)
	rev := w.revision
	step := finalStep(t, w.result("success"), w.clarify(rev))
	step.Response.InputTokens, step.Response.OutputTokens = 1_000_000, 1_000_000
	p := script(step)
	rt := w.runtime(p, nil)
	got := w.investigate(t, rt, rev)
	row := w.row(t, got.RunID)
	if row.state != agent.RunFailed || row.status != recovery.StatusEscalated || row.reasons != agent.CodeOverReservation || p.Calls() != 1 {
		t.Fatalf("over-reservation outcome %+v calls=%d", row, p.Calls())
	}
	if replay := w.investigate(t, rt, rev); !replay.Replayed || replay.State != row.state || replay.Status != row.status || strings.Join(replay.Reasons, ",") != row.reasons || p.Calls() != 1 {
		t.Fatalf("terminal over-reservation replay %+v calls=%d", replay, p.Calls())
	}
}

func seedProposedRun(t *testing.T, w *world) (string, int64, recovery.RecoveryProposalV1) {
	t.Helper()
	return seedRun(t, w, agent.Finish{State: agent.RunCompleted, OutcomeStatus: recovery.StatusProposed})
}

// seedRun stores a terminal run finished as fin; a proposed outcome gets its artifact.
func seedRun(t *testing.T, w *world, fin agent.Finish) (string, int64, recovery.RecoveryProposalV1) {
	t.Helper()
	rev := w.revision
	proposal := w.clarify(rev)
	maintenance(w, &proposal, w.contexts[1].AssetID)
	proposal.ProposalID = "" // Judge removes the model's proposal identity before storing the artifact.
	proposal.TaskPayload.DueAt = proposal.TaskPayload.DueAt.UTC().Truncate(time.Microsecond)
	body, err := json.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	runID, sc := uuid.NewString(), agent.RunScope{TenantID: w.tenant, SiteID: w.site}
	err = w.store.InTx(w.ctx, sc, func(tx *sql.Tx) error {
		in, e := agent.LoadCaseInput(w.ctx, tx, sc, w.caseID, agent.LoaderOptions{ModeEnabled: true})
		if e != nil {
			return e
		}
		if e = agent.CreateRunTx(w.ctx, tx, sc, agent.Run{ID: runID, RequestKind: agent.RequestCase, CaseID: w.caseID, CaseRevision: rev,
			TraceID: agent.NewTraceID(), ProviderName: agent.ProviderFixture, Model: "fixture-model", ModelRevision: "sha256:fixture",
			PromptVersion: agent.PromptVersion, ToolVersion: agent.ToolVersion, PolicyRevision: approvals.PolicyRevision}, agent.DefaultLimits()); e != nil {
			return e
		}
		if e = agent.AddRefsTx(w.ctx, tx, sc, runID, in.Refs); e != nil {
			return e
		}
		if fin.OutcomeStatus == recovery.StatusProposed {
			if _, e = agent.AddArtifactTx(w.ctx, tx, sc, runID, agent.Artifact{ID: uuid.NewString(), Schema: agent.ArtifactSchema, Body: body}); e != nil {
				return e
			}
		}
		return agent.FinishRunTx(w.ctx, tx, sc, runID, fin)
	})
	if err != nil {
		t.Fatal(err)
	}
	return runID, rev, proposal
}

func activityFor(w *world, rt *agent.Runtime, allowed ...string) *dispatch.Activities {
	allow := make(map[string]bool, len(allowed))
	for _, tenant := range allowed {
		allow[tenant] = true
	}
	return &dispatch.Activities{Store: &store.Store{DB: w.runtimeDB}, Agent: rt, Allow: allow}
}

func TestWIS275DuplicateActivitiesSubmitOneServerBuiltProposal(t *testing.T) {
	w := newWorld(t)
	runID, rev, modelProposal := seedProposedRun(t, w)
	rt := w.runtimeWithBridge(agent.NewFixtureProvider(), nil)
	activities := activityFor(w, rt, w.tenant)
	ref := dispatch.InvestigateRef{Tenant: w.tenant, Site: w.site, Case: w.caseID, Revision: rev}
	type call struct {
		result agent.Result
		err    error
	}
	results := make(chan call, 2)
	for range 2 {
		go func() {
			result, err := activities.InvestigateCase(w.ctx, ref)
			results <- call{result: result, err: err}
		}()
	}
	for range 2 {
		got := <-results
		if got.err != nil || got.result.RunID != runID || got.result.Status != recovery.StatusProposed {
			t.Fatalf("duplicate activity result %+v err=%v", got.result, got.err)
		}
	}
	var proposalID string
	var proposals int
	if err := w.control.QueryRowContext(w.ctx, `SELECT count(*),COALESCE(min(proposal_id::text),'') FROM supervisor.proposal_revisions WHERE tenant_id=$1 AND site_id=$2 AND case_id=$3 AND generated_by_run_id=$4`, w.tenant, w.site, w.caseID, runID).Scan(&proposals, &proposalID); err != nil {
		t.Fatal(err)
	}
	if proposals != 1 || proposalID == "" {
		t.Fatalf("generated proposals=%d id=%q, want exactly one", proposals, proposalID)
	}
	var linked string
	if err := w.control.QueryRowContext(w.ctx, `SELECT proposal_id::text FROM supervisor.agent_runs WHERE id=$1`, runID).Scan(&linked); err != nil || linked != proposalID {
		t.Fatalf("agent run proposal id=%q want %q err=%v", linked, proposalID, err)
	}
	if err := w.store.InTx(w.ctx, agent.RunScope{TenantID: w.tenant, SiteID: w.site}, func(tx *sql.Tx) error {
		return agent.LinkProposalTx(w.ctx, tx, agent.RunScope{TenantID: w.tenant, SiteID: w.site}, runID, uuid.NewString())
	}); !errors.Is(err, agent.ErrRunNotFound) {
		t.Fatalf("run accepted a proposal ID not generated from it: %v", err)
	}
	scope := approvals.Scope{TenantID: w.tenant, SiteID: w.site}
	service := approvals.NewService(w.runtimeDB)
	view, err := service.Get(w.ctx, w.reviewer, scope, proposalID)
	if err != nil {
		t.Fatal(err)
	}
	if view.SubmittedBy != nil {
		t.Fatalf("run-generated proposal submitter=%+v, want nil", view.SubmittedBy)
	}
	var envelope struct {
		Body struct {
			ActionKind        string `json:"action_kind"`
			ProposalID        string `json:"proposal_id"`
			AssetID           string `json:"asset_id"`
			LineID            string `json:"line_id"`
			OrderID           string `json:"order_id"`
			AssigneeID        string `json:"assignee_id"`
			Instructions      string `json:"instructions"`
			CaseID            string `json:"case_id"`
			CaseRevision      int64  `json:"case_revision"`
			ContextSnapshotID string `json:"context_snapshot_id"`
			ContextRevision   int64  `json:"context_revision"`
			ContextDigest     string `json:"context_digest"`
			ConfigRevision    int64  `json:"config_revision"`
			GraphRevision     int64  `json:"graph_revision"`
			PolicyRevision    int64  `json:"policy_revision"`
			GeneratedByRun    string `json:"generated_by_run_id"`
			Evidence          []struct {
				Kind     string `json:"kind"`
				ID       string `json:"id"`
				Revision int64  `json:"revision"`
			} `json:"evidence_revisions"`
		} `json:"body"`
	}
	if err = json.Unmarshal(view.CanonicalPayload, &envelope); err != nil {
		t.Fatal(err)
	}
	payload := envelope.Body
	current := w.contexts[1]
	if payload.ActionKind != recovery.TaskKind || payload.ProposalID != proposalID || payload.AssetID != modelProposal.TaskPayload.AssetID || payload.LineID != modelProposal.TaskPayload.LineID ||
		payload.OrderID != modelProposal.TaskPayload.OrderID || payload.AssigneeID != modelProposal.TaskPayload.AssigneeID || payload.Instructions != modelProposal.TaskPayload.Description ||
		payload.CaseID != w.caseID || payload.CaseRevision != rev || payload.ContextSnapshotID != current.ContextID || payload.ContextRevision != current.Revision || payload.ContextDigest != current.Digest ||
		payload.ConfigRevision != current.ReadinessPolicyRevision || payload.GraphRevision != w.graph || payload.PolicyRevision != approvals.PolicyRevision || payload.GeneratedByRun != runID || len(payload.Evidence) == 0 {
		t.Fatalf("proposal payload was not built from the validated server facts: %+v", payload)
	}
	decided, err := service.Decide(w.ctx, w.reviewer, scope, proposalID, approvals.DecisionInput{Decision: "approve", ExpectedRevision: 1, Reason: "Reviewed"}, uuid.NewString(), 1)
	if err != nil || decided.State != approvals.Approved {
		t.Fatalf("first human approver could not approve run-generated proposal: state=%s err=%v", decided.State, err)
	}
	status, err := cases.NewService(w.runtimeDB).WorkflowStatus(w.ctx, cases.WorkflowRef{TenantID: w.tenant, SiteID: w.site, CaseID: w.caseID})
	if err != nil || status.State != cases.AwaitingApproval || status.Revision != rev+2 {
		t.Fatalf("proposal case state %+v err=%v", status, err)
	}
	rows, err := w.control.QueryContext(w.ctx, `SELECT from_state,to_state,event,actor_issuer,actor_subject FROM supervisor.case_transitions WHERE tenant_id=$1 AND site_id=$2 AND case_id=$3 AND revision>$4 ORDER BY revision`, w.tenant, w.site, w.caseID, rev)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := [][5]string{{"investigating", "proposed", "proposal_validated", cases.WorkflowActor.Issuer, cases.WorkflowActor.Subject}, {"proposed", "awaiting_approval", "approval_requested", cases.WorkflowActor.Issuer, cases.WorkflowActor.Subject}}
	for i := range want {
		var got [5]string
		if !rows.Next() {
			t.Fatalf("missing case transition %d", i)
		}
		if err = rows.Scan(&got[0], &got[1], &got[2], &got[3], &got[4]); err != nil || got != want[i] {
			t.Fatalf("transition %d got=%v want=%v err=%v", i, got, want[i], err)
		}
	}
	if rows.Next() {
		t.Fatal("proposal bridge recorded more than two case transitions")
	}
}

func TestWIS275InvestigationOutcomesMapToCaseTransitions(t *testing.T) {
	for _, tc := range []struct {
		name, status, event, from, to, reason string
		proposal                              func(*world, int64) recovery.RecoveryProposalV1
	}{
		{"needs_input", recovery.StatusNeedsInput, "evidence_missing", "investigating", "needs_input", "CLARIFICATION_REQUESTED", func(w *world, rev int64) recovery.RecoveryProposalV1 { return w.clarify(rev) }},
		{"escalated", recovery.StatusEscalated, "fatal", "investigating", "escalated", "SPECIALIST_ESCALATED", func(w *world, rev int64) recovery.RecoveryProposalV1 {
			p := w.clarify(rev)
			p.Options[0].Kind, p.Options[0].Text, p.SelectedOption = recovery.OptionEscalate, "Escalate for supervisor review.", recovery.OptionEscalate
			return p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			rev := w.revision
			provider := script(finalStep(t, w.result("success"), tc.proposal(w, rev)))
			result := w.investigate(t, w.runtimeWithBridge(provider, nil), rev)
			if result.Status != tc.status || provider.Calls() != 1 {
				t.Fatalf("agent result %+v provider calls=%d", result, provider.Calls())
			}
			status, err := cases.NewService(w.runtimeDB).WorkflowStatus(w.ctx, cases.WorkflowRef{TenantID: w.tenant, SiteID: w.site, CaseID: w.caseID})
			if err != nil || string(status.State) != tc.to || status.Revision != rev+1 {
				t.Fatalf("case status %+v err=%v", status, err)
			}
			var from, to, event, reason, issuer, subject string
			if err = w.control.QueryRowContext(w.ctx, `SELECT from_state,to_state,event,reason,actor_issuer,actor_subject FROM supervisor.case_transitions WHERE tenant_id=$1 AND site_id=$2 AND case_id=$3 AND revision=$4`, w.tenant, w.site, w.caseID, rev+1).Scan(&from, &to, &event, &reason, &issuer, &subject); err != nil {
				t.Fatal(err)
			}
			if from != tc.from || to != tc.to || event != tc.event || reason != tc.reason || issuer != cases.WorkflowActor.Issuer || subject != cases.WorkflowActor.Subject {
				t.Fatalf("transition %s -> %s %s reason=%s actor=%s/%s", from, to, event, reason, issuer, subject)
			}
		})
	}
}

// WIS-275 review: a replayed terminal run that is canceled or degraded at the current case
// revision moves the case to needs_input (EvidenceMissing) instead of leaving it investigating
// behind a dead run, and a degraded run's artifact is never submitted for approval.
func TestWIS275DeadRunAtCurrentRevisionNeedsInput(t *testing.T) {
	for _, tc := range []struct {
		name, state, reason string
		seed                func(*testing.T, *world) (string, int64)
	}{
		{"canceled", agent.RunCanceled, agent.CodeRunCanceled, func(t *testing.T, w *world) (string, int64) {
			runID, rev, _ := seedRun(t, w, agent.Finish{State: agent.RunCanceled, OutcomeReasons: []string{agent.CodeRunCanceled}})
			return runID, rev
		}},
		{"degraded", agent.RunDegraded, agent.CodeSpecialistFailed, func(t *testing.T, w *world) (string, int64) {
			runID, rev, _ := seedRun(t, w, agent.Finish{State: agent.RunDegraded, OutcomeStatus: recovery.StatusProposed})
			return runID, rev
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			runID, rev := tc.seed(t, w)
			// The Temporal retry replays the terminal run with the bridge configured.
			p := script()
			res := w.investigate(t, w.runtimeWithBridge(p, nil), rev)
			if res.RunID != runID || res.State != tc.state || !res.Replayed || p.Calls() != 0 {
				t.Fatalf("replay %+v provider calls=%d", res, p.Calls())
			}
			status, err := cases.NewService(w.runtimeDB).WorkflowStatus(w.ctx, cases.WorkflowRef{TenantID: w.tenant, SiteID: w.site, CaseID: w.caseID})
			if err != nil || status.State != cases.NeedsInput || status.Revision != rev+1 || !status.WaitingOnHuman {
				t.Fatalf("case status %+v err=%v, want needs_input at %d", status, err, rev+1)
			}
			var event, reason string
			if err = w.control.QueryRowContext(w.ctx, `SELECT event,reason FROM supervisor.case_transitions WHERE tenant_id=$1 AND site_id=$2 AND case_id=$3 AND revision=$4`, w.tenant, w.site, w.caseID, rev+1).Scan(&event, &reason); err != nil {
				t.Fatal(err)
			}
			var proposals int
			if err = w.control.QueryRowContext(w.ctx, `SELECT count(*) FROM supervisor.proposal_revisions WHERE tenant_id=$1 AND generated_by_run_id=$2`, w.tenant, runID).Scan(&proposals); err != nil {
				t.Fatal(err)
			}
			if event != "evidence_missing" || reason != tc.reason || proposals != 0 {
				t.Fatalf("transition %s reason=%s proposals=%d", event, reason, proposals)
			}
			// A second replay changes nothing: the case already left this revision.
			if again := w.investigate(t, w.runtimeWithBridge(p, nil), rev); again.RunID != runID {
				t.Fatalf("second replay %+v", again)
			}
			if status, err = cases.NewService(w.runtimeDB).WorkflowStatus(w.ctx, cases.WorkflowRef{TenantID: w.tenant, SiteID: w.site, CaseID: w.caseID}); err != nil || status.Revision != rev+1 {
				t.Fatalf("second replay moved the case: %+v err=%v", status, err)
			}
		})
	}
}

// WIS-275 review: a worker shutdown cancels the activity context mid-call. That is not an outcome:
// the run stays running with the in-flight call refunded, the case stays investigating, and the
// retry resumes the same run. A moved generation (a person clarifies) still ends the run canceled.
func TestWIS275WorkerShutdownResumesRun(t *testing.T) {
	w := newWorld(t)
	rev := w.revision
	p := script(finalStep(t, w.result("success"), w.clarify(rev)))
	ctx, shutdown := context.WithCancel(w.ctx)
	defer shutdown()
	p.before = func(call int) {
		if call == 1 {
			shutdown()
		}
	}
	rt := w.runtimeWithBridge(p, nil)
	ref := agent.InvestigateRef{TenantID: w.tenant, SiteID: w.site, CaseID: w.caseID}
	if res, err := rt.Investigate(ctx, ref, rev); !errors.Is(err, context.Canceled) || res.RunID != "" {
		t.Fatalf("shutdown attempt result %+v err=%v, want context.Canceled", res, err)
	}
	var runID, state string
	var tokensLeft int64
	if err := w.control.QueryRowContext(w.ctx, `SELECT id::text,state,tokens_left FROM supervisor.agent_runs WHERE tenant_id=$1 AND case_id=$2 AND case_revision=$3`, w.tenant, w.caseID, rev).Scan(&runID, &state, &tokensLeft); err != nil {
		t.Fatal(err)
	}
	var settled string
	if err := w.control.QueryRowContext(w.ctx, `SELECT status||':'||COALESCE(reason_code,'')||':'||input_tokens||':'||output_tokens FROM supervisor.agent_model_calls WHERE run_id=$1`, runID).Scan(&settled); err != nil {
		t.Fatal(err)
	}
	if state != agent.RunRunning || tokensLeft != agent.DefaultLimits().Tokens || settled != "failed:"+agent.CodeProviderCanceled+":0:0" {
		t.Fatalf("after shutdown run state=%s tokens_left=%d call=%s, want running, fully refunded", state, tokensLeft, settled)
	}
	status, err := cases.NewService(w.runtimeDB).WorkflowStatus(w.ctx, cases.WorkflowRef{TenantID: w.tenant, SiteID: w.site, CaseID: w.caseID})
	if err != nil || status.State != cases.Investigating || status.Revision != rev {
		t.Fatalf("shutdown moved the case: %+v err=%v", status, err)
	}
	// The Temporal retry resumes the same run; its outcome is the model's, not RUN_CANCELED.
	res := w.investigate(t, rt, rev)
	if res.RunID != runID || res.Replayed || res.State != agent.RunCompleted || res.Status != recovery.StatusNeedsInput || p.Calls() != 1 {
		t.Fatalf("resumed %+v provider calls=%d", res, p.Calls())
	}
	var reason string
	if err = w.control.QueryRowContext(w.ctx, `SELECT reason FROM supervisor.case_transitions WHERE tenant_id=$1 AND case_id=$2 AND revision=$3`, w.tenant, w.caseID, rev+1).Scan(&reason); err != nil || reason != "CLARIFICATION_REQUESTED" {
		t.Fatalf("transition reason=%q err=%v, want the model outcome", reason, err)
	}
	// Control: a person clarifies during the call; the generation moved, so the run ends canceled.
	w.revision = rev + 1 // the bridge recorded needs_input
	w.move(t, "needs_input", "investigating")
	rev = w.revision
	p = script(finalStep(t, w.result("success"), w.clarify(rev)))
	p.before = func(int) { w.move(t, "investigating", "needs_input") }
	res = w.investigate(t, w.runtimeWithBridge(p, nil), rev)
	if got := w.row(t, res.RunID); got.state != agent.RunCanceled || got.reasons != agent.CodeRunCanceled {
		t.Fatalf("generation change: %+v", got)
	}
}

// WIS-275 review: a completed run whose linked proposal expired before the case update landed
// moves the case to needs_input with PROPOSAL_EXPIRED instead of retrying forever, and no second
// proposal is submitted.
func TestWIS275ExpiredLinkedProposalNeedsInput(t *testing.T) {
	w := newWorld(t)
	runID, rev, _ := seedProposedRun(t, w)
	expired := w.generatedProposal(t, runID, rev, "-1 hour")
	sc := agent.RunScope{TenantID: w.tenant, SiteID: w.site}
	if err := w.store.InTx(w.ctx, sc, func(tx *sql.Tx) error { return agent.LinkProposalTx(w.ctx, tx, sc, runID, expired) }); err != nil {
		t.Fatal(err)
	}
	res := w.investigate(t, w.runtimeWithBridge(script(), nil), rev)
	if res.RunID != runID || !res.Replayed {
		t.Fatalf("replay %+v", res)
	}
	status, err := cases.NewService(w.runtimeDB).WorkflowStatus(w.ctx, cases.WorkflowRef{TenantID: w.tenant, SiteID: w.site, CaseID: w.caseID})
	if err != nil || status.State != cases.NeedsInput || status.Revision != rev+1 {
		t.Fatalf("case status %+v err=%v, want needs_input at %d", status, err, rev+1)
	}
	var reason string
	var proposals int
	if err = w.control.QueryRowContext(w.ctx, `SELECT reason,(SELECT count(*) FROM supervisor.proposal_revisions WHERE generated_by_run_id=$4) FROM supervisor.case_transitions WHERE tenant_id=$1 AND case_id=$2 AND revision=$3`, w.tenant, w.caseID, rev+1, runID).Scan(&reason, &proposals); err != nil {
		t.Fatal(err)
	}
	if reason != agent.CodeProposalExpired || proposals != 1 {
		t.Fatalf("reason=%s proposals=%d, want PROPOSAL_EXPIRED and only the expired one", reason, proposals)
	}
}

// WIS-275 review: lockGeneratedProposal(requireLive) treats an expired pending proposal as not
// live, so the case is not moved to awaiting approval on it. Control: an unexpired one is.
func TestWIS275ExpiredGeneratedProposalIsNotLive(t *testing.T) {
	w := newWorld(t)
	runID, rev, _ := seedProposedRun(t, w)
	svc := cases.NewService(w.runtimeDB)
	ref := cases.WorkflowRef{TenantID: w.tenant, SiteID: w.site, CaseID: w.caseID}
	expired := w.generatedProposal(t, runID, rev, "-1 hour")
	if _, err := svc.RecordProposal(w.ctx, ref, rev, runID, recovery.StatusProposed, nil, expired); !errors.Is(err, cases.ErrRevision) {
		t.Fatalf("expired pending proposal err=%v, want ErrRevision", err)
	}
	if status, err := svc.WorkflowStatus(w.ctx, ref); err != nil || status.State != cases.Investigating || status.Revision != rev {
		t.Fatalf("case moved on an expired proposal: %+v err=%v", status, err)
	}
	live := w.generatedProposal(t, runID, rev, "0 seconds")
	if _, err := svc.RecordProposal(w.ctx, ref, rev, runID, recovery.StatusProposed, nil, live); err != nil {
		t.Fatalf("live proposal: %v", err)
	}
	if status, err := svc.WorkflowStatus(w.ctx, ref); err != nil || status.State != cases.AwaitingApproval || status.Revision != rev+2 {
		t.Fatalf("live proposal case status %+v err=%v", status, err)
	}
}

// generatedProposal stores a pending run-generated proposal issued at now()+shift with the 30 min
// TTL. The approvals guards are bypassed (superuser, replica role) only to place it in the past.
func (w *world) generatedProposal(t *testing.T, runID string, caseRevision int64, shift string) string {
	t.Helper()
	id := uuid.NewString()
	payload, err := json.Marshal(map[string]any{"schema": "ApprovedActionV1", "schema_version": 1, "tenant_id": w.tenant, "site_id": w.site,
		"body": map[string]any{"proposal_id": id, "proposal_revision": 1, "case_id": w.caseID, "case_revision": caseRevision, "context_snapshot_id": w.contexts[1].ContextID, "generated_by_run_id": runID}})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := w.control.BeginTx(w.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, q := range []string{`SET LOCAL session_replication_role=replica`,
		`INSERT INTO supervisor.proposal_revisions(tenant_id,site_id,proposal_id,revision,case_id,case_revision,context_snapshot_id,generated_by_run_id,issued_at,expires_at,input_sha256,canonical_payload,canonical_payload_sha256)
 SELECT $1,$2,$3,1,$4,$5,$6,$7,i,i+interval '30 minutes',repeat('ab',32),$8,encode(sha256(convert_to($8,'UTF8')),'hex') FROM (SELECT clock_timestamp()+$9::interval i) t`,
		`INSERT INTO supervisor.approval_states(tenant_id,site_id,proposal_id,current_revision,state,state_revision) VALUES($1,$2,$3,1,'pending',1)`} {
		var args []any
		switch {
		case strings.HasPrefix(q, "INSERT INTO supervisor.proposal_revisions"):
			args = []any{w.tenant, w.site, id, w.caseID, caseRevision, w.contexts[1].ContextID, runID, string(payload), shift}
		case strings.HasPrefix(q, "INSERT INTO supervisor.approval_states"):
			args = []any{w.tenant, w.site, id}
		}
		if _, err = tx.ExecContext(w.ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestWIS275CrossTenantActivityCannotInvestigateAnotherTenantsCase(t *testing.T) {
	w := newWorld(t)
	provider := script(finalStep(t, w.result("success"), w.clarify(w.revision)))
	activities := activityFor(w, w.runtimeWithBridge(provider, nil), w.otherTenant)
	_, err := activities.InvestigateCase(w.ctx, dispatch.InvestigateRef{Tenant: w.otherTenant, Site: w.otherSite, Case: w.caseID, Revision: w.revision})
	if err == nil || provider.Calls() != 0 {
		t.Fatalf("cross-tenant investigation err=%v provider calls=%d", err, provider.Calls())
	}
	var runs int
	if err = w.control.QueryRowContext(w.ctx, `SELECT count(*) FROM supervisor.agent_runs WHERE tenant_id=$1 AND case_id=$2`, w.otherTenant, w.caseID).Scan(&runs); err != nil || runs != 0 {
		t.Fatalf("cross-tenant runs=%d err=%v", runs, err)
	}
}

func TestWIS275RevokedEvidenceBetweenJudgeAndSubmitPreventsProposal(t *testing.T) {
	w := newWorld(t)
	runID, rev, _ := seedProposedRun(t, w)
	scope := evidence.Scope{TenantID: w.tenant, SiteID: w.site}
	head, err := w.ev.Read(w.ctx, w.ea, scope, w.schedule, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.ev.Revoke(w.ctx, w.ea, scope, w.schedule, "Withdrawn after Judge", uuid.NewString(), head.StateRevision); err != nil {
		t.Fatal(err)
	}
	activities := activityFor(w, w.runtimeWithBridge(agent.NewFixtureProvider(), nil), w.tenant)
	result, err := activities.InvestigateCase(w.ctx, dispatch.InvestigateRef{Tenant: w.tenant, Site: w.site, Case: w.caseID, Revision: rev})
	if err != nil || result.RunID != runID {
		t.Fatalf("investigation result %+v err=%v", result, err)
	}
	var proposals int
	if err = w.control.QueryRowContext(w.ctx, `SELECT count(*) FROM supervisor.proposal_revisions WHERE tenant_id=$1 AND site_id=$2 AND generated_by_run_id=$3`, w.tenant, w.site, runID).Scan(&proposals); err != nil || proposals != 0 {
		t.Fatalf("revoked run submitted %d proposals err=%v", proposals, err)
	}
	var linked sql.NullString
	if err = w.control.QueryRowContext(w.ctx, `SELECT proposal_id FROM supervisor.agent_runs WHERE id=$1`, runID).Scan(&linked); err != nil || linked.Valid {
		t.Fatalf("revoked run linked proposal %v err=%v", linked, err)
	}
	status, err := cases.NewService(w.runtimeDB).WorkflowStatus(w.ctx, cases.WorkflowRef{TenantID: w.tenant, SiteID: w.site, CaseID: w.caseID})
	if err != nil || status.State != cases.NeedsInput || status.Revision != rev+1 {
		t.Fatalf("revoked case status %+v err=%v", status, err)
	}
}

func TestWIS275ModelProposalExtraFieldIsRejected(t *testing.T) {
	w := newWorld(t)
	rev := w.revision
	proposal := w.clarify(rev)
	var body map[string]any
	encoded, err := json.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	body["approved"] = true
	output, err := json.Marshal(map[string]any{"result": w.result("success"), "proposal": body})
	if err != nil {
		t.Fatal(err)
	}
	result := w.investigate(t, w.runtime(script(agent.FixtureStep{Response: agent.ModelResponse{Content: string(output), InputTokens: 1200, OutputTokens: 400}}), nil), rev)
	row := w.row(t, result.RunID)
	if row.status != recovery.StatusRefused || row.reasons != recovery.ReasonSchemaInvalid || row.artifact {
		t.Fatalf("extra model field was accepted: %+v", row)
	}
}

// TestWIS275MissingHeadFailsClosed: a captured evidence ref whose head HeadsTx does not return is
// treated as revoked at the pre-commit recheck (which re-reads the stored scope): nothing is
// committed and the run stops, never treating the ref as current.
func TestWIS275MissingHeadFailsClosed(t *testing.T) {
	w := newWorld(t)
	rev := w.revision
	p := script(toolStep(agent.ToolMeasurementsGet, `{}`), finalStep(t, w.result("success"), w.clarify(rev)))
	p.before = func(n int) {
		if n == 1 {
			w.exec(t, `INSERT INTO supervisor.agent_run_refs(tenant_id,site_id,run_id,kind,ref_id,revision,sha256,access_epoch)
 SELECT tenant_id,site_id,id,'evidence',$3,1,repeat('0',64),1 FROM supervisor.agent_runs WHERE tenant_id=$1 AND case_id=$2 AND case_revision=$4`, w.tenant, w.caseID, uuid.NewString(), rev)
		}
	}
	res := w.investigate(t, w.runtime(p, nil), rev)
	got := w.row(t, res.RunID)
	want(t, got, agent.RunCompleted, recovery.StatusNeedsInput, agent.CodeEvidenceRevoked)
	if got.artifact || !got.revoked {
		t.Fatalf("calls=%d %+v", p.Calls(), got)
	}
}

// TestWIS275FixtureRecordedNotActual: fixture runs are labelled and can never satisfy an
// actual-model gate.
func TestWIS275FixtureRecordedNotActual(t *testing.T) {
	w := newWorld(t)
	p := script(finalStep(t, w.result("success"), w.clarify(w.revision)))
	res := w.investigate(t, w.runtime(p, nil), w.revision)
	var name string
	var actual, callActual bool
	if err := w.control.QueryRow(`SELECT provider_name,provider_actual,(SELECT bool_or(provider_actual) FROM supervisor.agent_model_calls WHERE run_id=$1) FROM supervisor.agent_runs WHERE id=$1`, res.RunID).Scan(&name, &actual, &callActual); err != nil {
		t.Fatal(err)
	}
	if name != agent.ProviderFixture || actual || callActual || agent.RequireActual(p) == nil {
		t.Fatalf("name=%s actual=%v call=%v", name, actual, callActual)
	}
}

func maintenance(w *world, p *recovery.RecoveryProposalV1, line string) {
	d := w.measurement(recovery.MetricDeficit)
	p.Options = append(p.Options, recovery.Option{Kind: recovery.OptionMaintenance, Text: "Inspect the press and report.", EvidenceIDs: []string{d.ID}})
	p.SelectedOption = recovery.OptionMaintenance
	p.Unknowns = []recovery.Unknown{}
	p.TaskPayload = &recovery.TaskPayload{Kind: recovery.TaskKind, AssetID: w.contexts[1].AssetID, LineID: line, OrderID: w.contexts[1].OrderID, AssigneeID: uuid.NewString(),
		Template: recovery.TaskTemplate, Description: "Inspect the press and report what stopped it.", DueAt: time.Now().UTC().Add(4 * time.Hour).Truncate(time.Second)}
}

func slicesContain(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func (w *world) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := w.control.ExecContext(w.ctx, q, args...); err != nil {
		t.Fatal(err)
	}
}

// TestWIS275ConversationRunsFailClosed: slice 1 has no conversation entry point, so a tool call on
// a conversation run is refused with its own code, never treated as a case run.
func TestWIS275ConversationRunsFailClosed(t *testing.T) {
	w := newWorld(t)
	rs := agent.RunScope{TenantID: w.tenant, SiteID: w.site}
	runID := uuid.NewString()
	if err := w.store.InTx(w.ctx, rs, func(tx *sql.Tx) error {
		return agent.CreateRunTx(w.ctx, tx, rs, agent.Run{ID: runID, RequestKind: agent.RequestConversation, ConversationID: uuid.NewString(), RequestID: uuid.NewString(),
			TraceID: agent.NewTraceID(), ProviderName: agent.ProviderFixture, Model: "fixture-model", ModelRevision: "sha256:fixture", PromptVersion: agent.PromptVersion,
			ToolVersion: agent.ToolVersion, PolicyRevision: 1}, agent.DefaultLimits())
	}); err != nil {
		t.Fatal(err)
	}
	_, env := w.openRun(t, agent.DefaultLimits())
	// Even with the live case's generation, the conversation run does not borrow the case path.
	sc := agent.ToolServerContext{TenantID: w.tenant, SiteID: w.site, RunID: runID, Generation: w.revision, Role: agent.RoleInvestigation}
	if _, err := (&agent.Tools{Store: w.store}).Call(w.ctx, sc, env, agent.ToolMeasurementsGet, []byte(`{}`)); !errors.Is(err, agent.ErrConversationDisabled) {
		t.Fatalf("conversation run: %v", err)
	}
	if got := w.row(t, runID); got.toolCalls != 0 {
		t.Fatalf("recorded %d tool calls", got.toolCalls)
	}
}

// TestWIS275EvidenceSpanIDs: evidence.get returns only the spans of the captured source revision.
func TestWIS275EvidenceSpanIDs(t *testing.T) {
	w := newWorld(t)
	// Another source of the same site with its own span: never listed for the captured source.
	es := evidence.Scope{TenantID: w.tenant, SiteID: w.site}
	other, err := w.ev.CreateOrRevise(w.ctx, w.ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Other note", Text: "Unrelated maintenance note"}, uuid.NewString(), 0)
	if err == nil {
		_, err = w.ev.CreateSpan(w.ctx, w.ea, es, evidence.SpanInput{SourceID: other.SourceID, Revision: other.Revision, Start: 0, End: 9}, uuid.NewString(), other.StateRevision)
	}
	if err != nil {
		t.Fatal(err)
	}
	sc, env := w.openRun(t, agent.DefaultLimits())
	out, err := (&agent.Tools{Store: w.store}).Call(w.ctx, sc, env, agent.ToolEvidenceGet, []byte(`{"evidence_id":"`+w.schedule+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	var read struct {
		SpanIDs []string `json:"span_ids"`
	}
	if err = json.Unmarshal(out.Content, &read); err != nil || len(read.SpanIDs) != 1 {
		t.Fatalf("%v %s", err, out.Content)
	}
	var source string
	if err = w.control.QueryRow(`SELECT source_id::text FROM supervisor.evidence_spans WHERE tenant_id=$1 AND id=$2`, w.tenant, read.SpanIDs[0]).Scan(&source); err != nil || source != w.schedule {
		t.Fatalf("span %s belongs to %s (%v)", read.SpanIDs[0], source, err)
	}
}
