//go:build integration && investigationactual

package agent_test

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plantcontext"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/roster"
)

// Opt-in actual-model regression, never part of deterministic gates or full qualification.
// The frozen labels are independent of model output and Go's own outcome classifier.
func TestWIS710InvestigationLocalCases(t *testing.T) {
	u, err := url.Parse(os.Getenv("SUPERVISOR_TEST_ADMIN_URL"))
	if err != nil || u.Host != "127.0.0.1:55810" || u.Path != "/supervisor" {
		t.Fatal("BLOCKED: owned disposable PostgreSQL on 55810 required")
	}
	lock, err := os.OpenFile("/var/tmp/supervisor-facility/model-request.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	f, err := os.OpenFile(os.Getenv("INVEST_REPORT"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	raw, err := os.ReadFile("../../evals/supervisor/useful-investigations.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Cases []struct {
			ID          string `json:"id"`
			Partition   string `json:"partition"`
			StopAt      string `json:"stop_at"`
			Roster      bool   `json:"roster"`
			Reason      string `json:"reason"`
			OtherReason string `json:"other_reason"`
			PlanText    string `json:"plan_text"`
			Expected    string `json:"expected_status"`
			Missing     string `json:"missing"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(raw, &manifest); err != nil || len(manifest.Cases) != 8 {
		t.Fatal("invalid frozen case set", err)
	}
	hash := sha256.Sum256(raw)
	sha, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	patch, err := exec.Command("git", "diff", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	patchHash := sha256.Sum256(patch)
	const model = "unsloth/Qwen3.5-9B-GGUF:Q4_K_M"
	const revision = "sha256:03b74727a860a56338e042c4420bb3f04b2fec5734175f4cb9fa853daf52b7e8"
	for _, c := range manifest.Cases {
		t.Run(c.ID, func(t *testing.T) {
			w := newWorldOpts(t, worldOpts{planText: c.PlanText})
			if c.Roster {
				operator := uuid.NewString()
				w.exec(t, `INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'operator')`, w.tenant, w.site, w.reviewer.Issuer, operator)
				_, err = roster.New(w.runtimeDB).Decide(w.ctx, roster.Principal{Issuer: w.reviewer.Issuer, Subject: w.reviewer.Subject, SessionHash: w.reviewer.SessionHash}, roster.Scope{TenantID: w.tenant, SiteID: w.site}, roster.DecisionInput{LineID: w.contexts[1].AssetID, AssigneeSubject: operator, Decision: roster.DecisionConfirm, Reason: "Confirmed maintenance inspector"}, uuid.NewString(), 0)
				if err != nil {
					t.Fatal(err)
				}
			}
			faultID := ""
			if c.StopAt != "" {
				feed := w.newMachineFeed(t, "machine_state,heartbeat")
				payload := map[string]any{"state": "stopped"}
				if c.Reason != "" {
					payload["reason_code"] = c.Reason
				}
				feed.send(t, map[string]any{"external_key": "case-stop", "kind": "machine_state", "event_at": c.StopAt, "asset_external_key": "line-01", "payload": payload})
				if c.Reason == "" {
					faultID = plantcontext.RecordID(plantcontext.Scope{TenantID: w.tenant, SiteID: w.site}, feed.sourceID, 1)
				}
				if c.OtherReason != "" {
					feed.send(t, map[string]any{"external_key": "later-stop", "kind": "machine_state", "event_at": "2026-09-27T00:24:00Z", "asset_external_key": "line-01", "payload": map[string]any{"state": "stopped", "reason_code": c.OtherReason}})
				}
			}
			logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
			p, err := agent.NewLocalModel(config.Agent{Provider: config.AgentProviderLocal, BaseURL: "http://127.0.0.1:64601", EndpointAddrs: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:64601")}, Model: model, ModelRevision: revision, Timeout: 120 * time.Second, ReasoningEffort: "none"}, logger, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			if err = p.Probe(w.ctx); err != nil {
				t.Fatal(err)
			}
			rt := w.runtimeWithBridge(p, nil)
			rt.Logger = logger
			rt.Model, rt.ModelRevision = model, revision
			rt.Limits.MaxConcurrent = 1
			rt.Limits.CallTimeout = 120 * time.Second
			rt.Slots = agent.NewSlots(1, 8)
			started := time.Now()
			res := w.investigate(t, rt, w.revision)
			elapsed := time.Since(started).Milliseconds()
			failures := []string{}
			questionOrigins := []map[string]string{}
			if res.Status != c.Expected {
				failures = append(failures, "expected "+c.Expected+", got "+res.Status)
			}
			var proposal *recovery.RecoveryProposalV1
			if res.ArtifactID != "" {
				err = w.store.InTx(w.ctx, agent.RunScope{TenantID: w.tenant, SiteID: w.site}, func(tx *sql.Tx) error {
					a, e := agent.LoadArtifactTx(w.ctx, tx, agent.RunScope{TenantID: w.tenant, SiteID: w.site}, res.RunID, res.ArtifactID)
					if e != nil {
						return e
					}
					parsed, e := recovery.DecodeProposal(a.Body)
					proposal = &parsed
					return e
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if proposal == nil {
				failures = append(failures, "no persisted grounded output or actionable missing-input question")
			} else if c.Expected == "proposed" {
				if proposal.SelectedOption != recovery.OptionMaintenance || proposal.TaskPayload == nil || proposal.TaskPayload.Template != recovery.TaskTemplate || len(proposal.Observations) == 0 {
					failures = append(failures, "missing inspect/report task or exact observations")
				}
				if proposal.TaskPayload != nil && proposal.TaskPayload.Description != "Inspect the recorded machine stoppage and report findings to the supervisor under site procedures." {
					failures = append(failures, "task description goes beyond the bounded inspection instruction")
				}
				cited := false
				for _, option := range proposal.Options {
					if option.Kind == recovery.OptionMaintenance {
						for _, id := range option.EvidenceIDs {
							cited = cited || id == faultID
						}
					}
				}
				if !cited || faultID == "" {
					failures = append(failures, "maintenance does not cite the labelled stoppage")
				}
				for _, h := range proposal.Hypotheses {
					if regexp.MustCompile(`(?i)\b(caused|causing|proved|root cause is)\b`).MatchString(h.Text) {
						failures = append(failures, "hypothesis asserts an unestablished cause")
					}
				}
			} else {
				matched := false
				for _, q := range proposal.Unknowns {
					// The old Go-added prefix cannot count as a model naming the responsible human.
					question := strings.TrimPrefix(q.Question, "Supervisor: ")
					if q.Required && regexp.MustCompile("(?i)"+c.Missing).MatchString(question) && regexp.MustCompile(`(?i)supervisor|operator|line lead`).MatchString(question) {
						matched = true
					}
					origin := "model_written"
					if strings.HasPrefix(q.Field, "required_input_") && len(res.Reasons) == 1 && res.Reasons[0] == agent.CodeSpecialistNeedsInput {
						origin = "server_derived_from_forced_missing_key"
					} else if strings.HasPrefix(q.Field, "conflicting_evidence_") && c.ID == "heldout-conflicting-log" {
						origin = "model_reported_conflict_explicit_in_fixture"
					}
					questionOrigins = append(questionOrigins, map[string]string{"field": q.Field, "origin": origin})
				}
				if !matched {
					failures = append(failures, "missing exact input and responsible human in persisted questions")
				}
				if proposal.TaskPayload != nil {
					failures = append(failures, "insufficient case carries a task")
				}
			}
			var calls, invalid, inputs, outputs int64
			err = w.control.QueryRowContext(w.ctx, `SELECT count(*),count(*) FILTER(WHERE reason_code='SCHEMA_INVALID'),COALESCE(sum(input_tokens),0),COALESCE(sum(output_tokens),0) FROM supervisor.agent_model_calls WHERE tenant_id=$1 AND run_id=$2`, w.tenant, res.RunID).Scan(&calls, &invalid, &inputs, &outputs)
			if err != nil {
				t.Fatal(err)
			}
			var tasks, approvals int
			err = w.control.QueryRowContext(w.ctx, `SELECT (SELECT count(*) FROM supervisor.tasks WHERE tenant_id=$1),(SELECT count(*) FROM supervisor.approval_states WHERE tenant_id=$1 AND state='approved')`, w.tenant).Scan(&tasks, &approvals)
			if err != nil {
				t.Fatal(err)
			}
			if tasks != 0 || approvals != 0 {
				failures = append(failures, "forbidden execution or approval")
			}
			report := map[string]any{"case_id": c.ID, "partition": c.Partition, "database_case_id": w.caseID, "run_id": res.RunID, "git_sha": strings.TrimSpace(string(sha)), "git_dirty": len(patch) > 0, "working_tree_patch_sha256": hex.EncodeToString(patchHash[:]), "manifest_sha256": hex.EncodeToString(hash[:]), "model": model, "model_revision": revision, "prompt_version": agent.PromptVersion, "tool_version": agent.ToolVersion, "provider_actual": true, "qualified": false, "expected_status": c.Expected, "result": res, "proposal": proposal, "latency_ms": elapsed, "model_calls": calls, "provider_schema_errors": invalid, "input_tokens": inputs, "output_tokens": outputs, "failures": failures}
			report["question_origins"] = questionOrigins
			if err = json.NewEncoder(f).Encode(report); err != nil {
				t.Fatal(err)
			}
			if len(failures) > 0 {
				t.Errorf("%s: %s", c.ID, strings.Join(failures, "; "))
			}
		})
	}
}
