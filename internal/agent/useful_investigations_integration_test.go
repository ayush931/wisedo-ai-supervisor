//go:build integration

package agent_test

import (
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/roster"
)

func TestWIS710InvestigationBriefDoesNotAskForConfirmedAssignee(t *testing.T) {
	w := newWorld(t)
	for _, confirmed := range []bool{false, true} {
		if confirmed {
			operator := uuid.NewString()
			w.exec(t, `INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'operator')`, w.tenant, w.site, w.reviewer.Issuer, operator)
			_, err := roster.New(w.runtimeDB).Decide(w.ctx, roster.Principal{Issuer: w.reviewer.Issuer, Subject: w.reviewer.Subject, SessionHash: w.reviewer.SessionHash}, roster.Scope{TenantID: w.tenant, SiteID: w.site}, roster.DecisionInput{LineID: w.contexts[1].AssetID, AssigneeSubject: operator, Decision: roster.DecisionConfirm, Reason: "Confirmed maintenance inspector"}, uuid.NewString(), 0)
			if err != nil {
				t.Fatal(err)
			}
		}
		p := script(finalStep(t, w.result("needs_input"), nil))
		w.investigate(t, w.runtime(p, nil), w.fresh(t))
		var brief struct {
			Missing []string `json:"missing_inspection_inputs"`
		}
		if err := json.Unmarshal([]byte(p.requests[0].Messages[1].Content), &brief); err != nil {
			t.Fatal(err)
		}
		want := []string{"machine_fault", "confirmed_assignee"}
		if confirmed {
			want = want[:1]
		}
		if !slices.Equal(brief.Missing, want) {
			t.Errorf("confirmed=%v missing=%v want=%v", confirmed, brief.Missing, want)
		}
		for i, request := range p.requests {
			var schema struct {
				Properties struct {
					Result struct {
						Properties struct {
							Missing struct{ Const []string } `json:"missing_required_fields"`
							Status  struct{ Enum []string }
						}
					}
					Proposal struct{ Type string }
				}
			}
			if err := json.Unmarshal(request.ResponseSchema, &schema); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(schema.Properties.Result.Properties.Missing.Const, want) ||
				!slices.Equal(schema.Properties.Result.Properties.Status.Enum, []string{"needs_input", "refused", "failed"}) ||
				schema.Properties.Proposal.Type != "null" {
				t.Fatalf("confirmed=%v request=%d sent the wrong schema: %s", confirmed, i, request.ResponseSchema)
			}
		}
	}
}

// The model confused a stoppage record ID with an evidence source ID. Its offered argument
// choices must match the captured kinds; the tool server still independently enforces scope.
func TestWIS710InvestigationToolArgumentsOfferOnlyCapturedSources(t *testing.T) {
	w := newWorld(t)
	p := script(finalStep(t, w.result("needs_input"), nil))
	w.investigate(t, w.runtime(p, nil), w.revision)
	for _, tool := range p.requests[0].Tools {
		if tool.Name != agent.ToolEvidenceGet {
			continue
		}
		var schema struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(tool.Parameters, &schema); err != nil {
			t.Fatal(err)
		}
		choices := schema.Properties["evidence_id"].Enum
		if len(choices) != 1 || choices[0] != w.schedule || slices.Contains(choices, w.measurement(recovery.MetricDeficit).ID) || slices.Contains(choices, w.otherSource) {
			t.Fatalf("document read permits wrong-kind or foreign IDs: %s", tool.Parameters)
		}
		return
	}
	t.Fatal("captured document cannot be read")
}

// Direct needs_input used to discard every question before the advisor could read it.
func TestWIS710InvestigationMissingInputSurvivesToAdvisor(t *testing.T) {
	for _, tc := range []struct{ name, missing, contradiction, reason, want string }{
		{"missing assignee", "Which maintenance assignee can the supervisor confirm for this line?", "", agent.CodeSpecialistNeedsInput, "Which maintenance assignee can the supervisor confirm for this line?"},
		{"field name", "confirmed_assignee", "", agent.CodeSpecialistNeedsInput, "Which maintenance assignee can the supervisor confirm for this line?"},
		{"conflicting evidence", "", "The supervisor must reconcile the machine stop and the operator log claiming continuous running.", agent.CodeContradictoryEvidence, "The supervisor must reconcile the machine stop and the operator log claiming continuous running."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			result := w.result("needs_input")
			result.ConciseRationale = "The measured deficit is approximately five units."
			if tc.missing != "" {
				result.MissingRequiredFields = []string{tc.missing}
			}
			if tc.contradiction != "" {
				result.Contradictions = []string{tc.contradiction}
			}
			rev := w.fresh(t)
			res := w.investigate(t, w.runtimeWithBridge(script(finalStep(t, result, nil)), nil), rev)
			if res.Status != recovery.StatusNeedsInput || len(res.Reasons) != 1 || res.Reasons[0] != tc.reason || res.ArtifactID == "" {
				t.Fatalf("lost actionable needs_input: %+v", res)
			}
			var p recovery.RecoveryProposalV1
			err := w.store.InTx(w.ctx, agent.RunScope{TenantID: w.tenant, SiteID: w.site}, func(tx *sql.Tx) error {
				a, e := agent.LoadArtifactTx(w.ctx, tx, agent.RunScope{TenantID: w.tenant, SiteID: w.site}, res.RunID, res.ArtifactID)
				if e != nil {
					return e
				}
				p, e = recovery.DecodeProposal(a.Body)
				return e
			})
			if err != nil {
				t.Fatal(err)
			}
			if p.SelectedOption != recovery.OptionClarify || p.TaskPayload != nil || len(p.Unknowns) != 1 || !p.Unknowns[0].Required || p.Unknowns[0].Question != tc.want {
				t.Fatalf("unsafe or missing clarification: %+v", p)
			}
			if p.Rationale == result.ConciseRationale || strings.Contains(p.Rationale, "five") {
				t.Fatalf("unverified model quantity persisted in rationale: %s", p.Rationale)
			}
			code, body := w.get(t, "reviewer", "/v1/sites/"+w.site+"/cases/"+w.caseID+"/suggestion")
			if code != 200 || !strings.Contains(string(body), tc.want) {
				t.Fatalf("advisor lost question: HTTP %d %s", code, body)
			}
			var tasks, proposals int
			if err = w.control.QueryRowContext(w.ctx, `SELECT (SELECT count(*) FROM supervisor.tasks WHERE tenant_id=$1),(SELECT count(*) FROM supervisor.proposal_revisions WHERE tenant_id=$1)`, w.tenant).Scan(&tasks, &proposals); err != nil {
				t.Fatal(err)
			}
			if tasks != 0 || proposals != 0 {
				t.Fatalf("clarification authorized an action: tasks=%d proposals=%d", tasks, proposals)
			}
		})
	}
}

func TestWIS710InvestigationMissingInputStillValidatesModelText(t *testing.T) {
	w := newWorld(t)
	result := w.result("needs_input")
	result.MissingRequiredFields = []string{"Can the supervisor confirm 999 units were lost?"}
	res := w.investigate(t, w.runtime(script(finalStep(t, result, nil)), nil), w.revision)
	if res.Status != recovery.StatusNeedsInput || len(res.Reasons) != 1 || res.Reasons[0] != agent.CodeSpecialistNeedsInput || res.ArtifactID != "" {
		t.Fatalf("unsupported fact in question reached user: %+v", res)
	}
}

func TestWIS710InvestigationContradictionStaysNeedsInput(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		artifact   bool
	}{
		{"unsupported digit", "The supervisor must reconcile log entry 17 with the stopped telemetry.", false},
		{"full length", strings.Repeat("Reconcile the records. ", 23)[:495], true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			result := w.result("needs_input")
			result.Contradictions = []string{tc.text}
			res := w.investigate(t, w.runtime(script(finalStep(t, result, nil)), nil), w.fresh(t))
			if res.Status != recovery.StatusNeedsInput || len(res.Reasons) != 1 || res.Reasons[0] != agent.CodeContradictoryEvidence || (res.ArtifactID != "") != tc.artifact {
				t.Fatalf("contradiction lost or unvalidated artifact stored: %+v", res)
			}
			var tasks, proposals int
			if err := w.control.QueryRowContext(w.ctx, `SELECT (SELECT count(*) FROM supervisor.tasks WHERE tenant_id=$1),(SELECT count(*) FROM supervisor.proposal_revisions WHERE tenant_id=$1)`, w.tenant).Scan(&tasks, &proposals); err != nil {
				t.Fatal(err)
			}
			if tasks != 0 || proposals != 0 {
				t.Fatalf("contradiction authorized an action: tasks=%d proposals=%d", tasks, proposals)
			}
		})
	}
}
