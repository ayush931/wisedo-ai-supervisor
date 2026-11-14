//go:build integration

package agent_test

import (
	"database/sql"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/approvals"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/membership"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plantcontext"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/roster"
)

// TestWIS276MachineStoppageIsMaintenanceEvidence: a recorded stoppage of the case line inside the
// bound windows, from a reviewed machine-state source and with no stated reason, is classified by
// the server as machine-fault evidence; one whose source stated a reason (NO_MATERIAL) is only a
// machine stoppage and cannot carry a maintenance investigation (review of #189, M1). It reaches the model through measurements.get, and a maintenance
// investigation citing it is no longer refused for want of machine evidence. Running, unknown,
// out-of-window, corrected-away, unmapped and revoked-source records never count (rule 06:
// a stoppage is a reason to inspect, and no stoppage is not proof of anything).
func TestWIS276MachineStoppageIsMaintenanceEvidence(t *testing.T) {
	w := newWorld(t)
	line, issuer, operator := w.contexts[1].AssetID, w.reviewer.Issuer, uuid.NewString()
	w.exec(t, `INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'operator')`, w.tenant, w.site, issuer, operator)
	if _, err := roster.New(w.runtimeDB).Decide(w.ctx, roster.Principal{Issuer: issuer, Subject: w.reviewer.Subject, SessionHash: w.reviewer.SessionHash}, roster.Scope{TenantID: w.tenant, SiteID: w.site},
		roster.DecisionInput{LineID: line, AssigneeSubject: operator, Decision: roster.DecisionConfirm, Reason: "Press line maintenance on call"}, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	assignee := membership.ActorID(issuer, operator).String()
	scope := agent.RunScope{TenantID: w.tenant, SiteID: w.site}
	machineEvidence := func() []recovery.Evidence {
		t.Helper()
		var out []recovery.Evidence
		if err := w.store.InTx(w.ctx, scope, func(tx *sql.Tx) error {
			in, e := agent.LoadCaseInput(w.ctx, tx, scope, w.caseID, agent.LoaderOptions{ModeEnabled: true})
			for _, ev := range in.Input.Evidence {
				if ev.Kind != recovery.EvidenceProduction {
					out = append(out, ev)
				}
			}
			return e
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := machineEvidence(); len(got) != 0 {
		t.Fatalf("machine evidence before any machine telemetry: %+v", got)
	}

	// The case binds windows 00:00-00:15 and 00:15-00:30.
	feed := w.newMachineFeed(t, "machine_state,heartbeat")
	state := func(key, state, at string, extra map[string]any) map[string]any {
		payload := map[string]any{"state": state}
		for k, v := range extra {
			payload[k] = v
		}
		return map[string]any{"external_key": key, "kind": "machine_state", "event_at": at, "asset_external_key": "line-01", "payload": payload}
	}
	feed.send(t, state("stop-1", "stopped", "2026-09-27T00:20:00Z", nil)) // sequence 1: the unexplained stoppage, machine-fault evidence
	feed.send(t,
		state("stop-material", "stopped", "2026-09-27T00:24:00Z", map[string]any{"reason_code": "NO_MATERIAL"}), // a stated cause
		state("run-1", "running", "2026-09-27T00:21:00Z", nil),
		state("unknown-1", "unknown", "2026-09-27T00:22:00Z", nil),
		state("stop-after", "stopped", "2026-09-27T00:45:00Z", nil), // after the bound windows
		state("stop-corrected", "stopped", "2026-09-27T00:10:00Z", nil))
	corrected := state("stop-corrected", "running", "2026-09-27T00:10:00Z", nil)
	corrected["revision"] = 2 // the source corrects it: the machine was running
	unmapped := state("stop-unmapped", "stopped", "2026-09-27T00:11:00Z", nil)
	unmapped["asset_external_key"] = "press-99" // no mapping: quarantined
	feed.send(t, corrected, state("stop-moved", "stopped", "2026-09-27T00:12:00Z", nil))
	moved := state("stop-moved", "stopped", "2026-09-27T00:50:00Z", nil)
	moved["revision"] = 2 // the source corrects the time: the stoppage was after the windows
	feed.send(t, moved)
	// A quarantined record stops the source's gap-free progress (as for production deltas), so a
	// stoppage sent after it is not trusted until the gap is resolved.
	feed.send(t, unmapped)
	feed.send(t, state("stop-after-gap", "stopped", "2026-09-27T00:25:00Z", nil))

	stoppage := plantcontext.RecordID(plantcontext.Scope{TenantID: w.tenant, SiteID: w.site}, feed.sourceID, 1)
	var materialSequence int64
	if err := w.control.QueryRowContext(w.ctx, `SELECT source_sequence FROM supervisor.operational_event_records WHERE tenant_id=$1 AND source_id=$2 AND external_key='stop-material'`, w.tenant, feed.sourceID).Scan(&materialSequence); err != nil {
		t.Fatal(err)
	}
	material := plantcontext.RecordID(plantcontext.Scope{TenantID: w.tenant, SiteID: w.site}, feed.sourceID, materialSequence)
	got := machineEvidence()
	if len(got) != 2 || got[0] != (recovery.Evidence{ID: stoppage, Kind: recovery.EvidenceMachineFault}) || got[1] != (recovery.Evidence{ID: material, Kind: recovery.EvidenceMachineStoppage}) {
		t.Fatalf("machine evidence %+v, want the unexplained stoppage %s as machine_fault and the NO_MATERIAL stop %s as machine_stoppage", got, stoppage, material)
	}

	// The model sees the stoppage through measurements.get, classified by the server, and a
	// maintenance investigation citing it now passes every check.
	rev := w.fresh(t)
	prop := w.clarify(rev)
	maintenance(w, &prop, line)
	prop.TaskPayload.AssigneeID = assignee
	prop.Options[len(prop.Options)-1].EvidenceIDs = []string{stoppage}
	prop.Hypotheses = []recovery.Hypothesis{{Text: "Hypothesis: the recorded stoppage may have contributed to the shortfall.", EvidenceIDs: []string{stoppage}, ContradictingEvidenceIDs: []string{}}}
	result := w.result("success")
	result.CitedEvidenceIDs = append(result.CitedEvidenceIDs, stoppage)
	provider := script(toolStep(agent.ToolMeasurementsGet, `{}`), finalStep(t, result, prop))
	res := w.investigate(t, w.runtime(provider, nil), rev)
	row := w.row(t, res.RunID)
	if row.status != recovery.StatusProposed || !row.artifact || res.ArtifactID == "" {
		t.Fatalf("maintenance proposal citing the stoppage: %+v", row)
	}
	var seen bool
	for _, r := range provider.requests {
		for _, m := range r.Messages {
			if m.Role != "tool" || !strings.Contains(m.Content, "machine_stoppages") {
				continue
			}
			var tool struct {
				Stoppages []agent.MachineObservation `json:"machine_stoppages"`
			}
			if err := json.Unmarshal([]byte(m.Content), &tool); err != nil {
				t.Fatalf("measurements.get output: %v: %s", err, m.Content)
			}
			want := []agent.MachineObservation{
				{EvidenceID: stoppage, AssetID: line, EventAt: "2026-09-27T00:20:00Z", State: "stopped", EvidenceClass: recovery.EvidenceMachineFault},
				{EvidenceID: material, AssetID: line, EventAt: "2026-09-27T00:24:00Z", State: "stopped", ReasonCode: "NO_MATERIAL", EvidenceClass: recovery.EvidenceMachineStoppage},
			}
			if len(tool.Stoppages) != 2 || tool.Stoppages[0] != want[0] || tool.Stoppages[1] != want[1] {
				t.Fatalf("measurements.get stoppages %+v, want %+v", tool.Stoppages, want)
			}
			seen = true
		}
	}
	if !seen {
		t.Fatal("measurements.get output never reached the model")
	}

	// Citing only production evidence is still not a maintenance case.
	rev = w.fresh(t)
	plain := w.clarify(rev)
	maintenance(w, &plain, line)
	plain.TaskPayload.AssigneeID = assignee
	res = w.investigate(t, w.runtime(script(finalStep(t, w.result("success"), plain)), nil), rev)
	if row := w.row(t, res.RunID); row.status != recovery.StatusRefused || row.reasons != recovery.RefuseNotMaintenance {
		t.Fatalf("maintenance without citing the stoppage: %+v", row)
	}

	// A stop with a stated non-machine cause is shown but cannot carry maintenance on its own.
	rev = w.fresh(t)
	onlyMaterial := w.clarify(rev)
	maintenance(w, &onlyMaterial, line)
	onlyMaterial.TaskPayload.AssigneeID = assignee
	onlyMaterial.Options[len(onlyMaterial.Options)-1].EvidenceIDs = []string{material}
	onlyMaterial.Hypotheses = []recovery.Hypothesis{{Text: "Hypothesis: the material stop may have contributed to the shortfall.", EvidenceIDs: []string{material}, ContradictingEvidenceIDs: []string{}}}
	materialResult := w.result("success")
	materialResult.CitedEvidenceIDs = append(materialResult.CitedEvidenceIDs, material)
	res = w.investigate(t, w.runtime(script(finalStep(t, materialResult, onlyMaterial)), nil), rev)
	if row := w.row(t, res.RunID); row.status != recovery.StatusRefused || row.reasons != recovery.RefuseNotMaintenance {
		t.Fatalf("maintenance citing only a NO_MATERIAL stop: %+v", row)
	}

	// Review of #189, M2: a proposed run whose machine evidence is withdrawn before submission is
	// not submitted. The run is proposed, the source is revoked, and the submission step then
	// records needs_input with EVIDENCE_REVOKED and creates no approval.
	rev = w.fresh(t)
	again := w.clarify(rev)
	maintenance(w, &again, line)
	again.TaskPayload.AssigneeID = assignee
	again.Options[len(again.Options)-1].EvidenceIDs = []string{stoppage}
	again.Hypotheses = prop.Hypotheses
	res = w.investigate(t, w.runtime(script(finalStep(t, result, again)), nil), rev)
	if row := w.row(t, res.RunID); row.status != recovery.StatusProposed {
		t.Fatalf("setup: %+v", row)
	}
	feed.revoke(t)
	if _, err := w.runtimeWithBridge(agent.NewFixtureProvider(), nil).Investigate(w.ctx, agent.InvestigateRef{TenantID: w.tenant, SiteID: w.site, CaseID: w.caseID}, rev); err != nil {
		t.Fatalf("submission step: %v", err)
	}
	var caseState, reason string
	var proposals int
	if err := w.control.QueryRowContext(w.ctx, `SELECT c.state,COALESCE((SELECT t.reason FROM supervisor.case_transitions t WHERE t.case_id=c.id ORDER BY t.revision DESC LIMIT 1),''),
 (SELECT count(*) FROM supervisor.proposal_revisions p WHERE p.case_id=c.id AND p.case_revision=$2) FROM supervisor.cases c WHERE c.id=$1`, w.caseID, rev).Scan(&caseState, &reason, &proposals); err != nil {
		t.Fatal(err)
	}
	if caseState != "needs_input" || reason != agent.CodeEvidenceRevoked || proposals != 0 {
		t.Fatalf("after revocation: case %s reason %q proposals %d, want needs_input, %s and none", caseState, reason, proposals, agent.CodeEvidenceRevoked)
	}

	// Revoking the machine source withdraws its evidence at the next load.
	if got := machineEvidence(); len(got) != 0 {
		t.Fatalf("revoked source still supplies machine evidence: %+v", got)
	}
}

// TestWIS276MachineEvidenceBound (review of #189, M3): routine running telemetry never counts
// toward the stoppage read bound, and more stoppages than the bound end the investigation as
// needs_input with MACHINE_EVIDENCE_TOO_LARGE before any model call, instead of an error that
// the activity retries forever. No stoppage is silently dropped.
func TestWIS276MachineEvidenceBound(t *testing.T) {
	w := newWorld(t)
	scope := agent.RunScope{TenantID: w.tenant, SiteID: w.site}
	load := func() agent.CaseInput {
		t.Helper()
		var in agent.CaseInput
		if err := w.store.InTx(w.ctx, scope, func(tx *sql.Tx) error {
			var e error
			in, e = agent.LoadCaseInput(w.ctx, tx, scope, w.caseID, agent.LoaderOptions{ModeEnabled: true})
			return e
		}); err != nil {
			t.Fatalf("load: %v", err)
		}
		return in
	}
	feed := w.newMachineFeed(t, "machine_state,heartbeat")
	// Each batch gets its own instants: intake treats two states of one asset at the same instant
	// as a duplicate event.
	batch := func(prefix, state string, n int, offset time.Duration) []map[string]any {
		out := make([]map[string]any, 0, n)
		for i := range n {
			at := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC).Add(offset + time.Duration(i)*4*time.Second) // inside 00:00-00:30
			out = append(out, map[string]any{"external_key": prefix + strconv.Itoa(i), "kind": "machine_state", "event_at": at.Format(time.RFC3339), "asset_external_key": "line-01", "payload": map[string]any{"state": state}})
		}
		return out
	}
	// Intake takes at most 100 events a batch, and the feed adds a heartbeat to each.
	send := func(events []map[string]any) {
		for len(events) > 0 {
			n := min(90, len(events))
			feed.send(t, append([]map[string]any(nil), events[:n]...)...) // a copy: send appends a heartbeat
			events = events[n:]
		}
	}
	send(batch("run-", "running", plantcontext.MaxMachineStates+1, 0))
	send(batch("stop-one-", "stopped", 1, 20*time.Minute+time.Second))
	if in := load(); in.Input.EvidenceTooLarge || len(in.Input.Evidence) == 0 || in.Input.Evidence[len(in.Input.Evidence)-1].Kind != recovery.EvidenceMachineFault {
		t.Fatalf("running telemetry counted toward the bound: too large=%v evidence=%+v", in.Input.EvidenceTooLarge, in.Input.Evidence)
	}
	send(batch("stop-", "stopped", plantcontext.MaxMachineStates, 2*time.Second))
	in := load()
	if !in.Input.EvidenceTooLarge {
		t.Fatal("more stoppages than the bound were not reported")
	}
	for _, ev := range in.Input.Evidence {
		if ev.Kind != recovery.EvidenceProduction {
			t.Fatalf("part of an over-bound stoppage set was used: %+v", ev)
		}
	}
	rev := w.fresh(t)
	res := w.investigate(t, w.runtime(script(finalStep(t, w.result("success"), w.clarify(rev))), nil), rev)
	if row := w.row(t, res.RunID); row.status != recovery.StatusNeedsInput || row.reasons != recovery.ReasonEvidenceTooLarge || row.modelCalls != 0 {
		t.Fatalf("over-bound evidence: %+v, want needs_input %s with no model call", row, recovery.ReasonEvidenceTooLarge)
	}
}

// TestWIS277DispatchRechecksMachineEvidence (review of #188, B1): the executor's bound-evidence
// check covers machine stoppages, not only evidence sources. A payload binding a proposed run's
// stoppage passes while it holds, and is refused once the source corrects that stoppage.
func TestWIS277DispatchRechecksMachineEvidence(t *testing.T) {
	w := newWorld(t)
	feed := w.newMachineFeed(t, "machine_state,heartbeat")
	stop := map[string]any{"external_key": "stop-1", "kind": "machine_state", "event_at": "2026-09-27T00:20:00Z", "asset_external_key": "line-01", "payload": map[string]any{"state": "stopped"}}
	feed.send(t, stop)
	scope := agent.RunScope{TenantID: w.tenant, SiteID: w.site}
	sc, _ := w.openRun(t, agent.DefaultLimits()) // captures the case scope, including the stoppage
	var payload approvals.Payload
	if err := w.store.InTx(w.ctx, scope, func(tx *sql.Tx) error {
		refs, e := agent.RefsTx(w.ctx, tx, scope, sc.RunID)
		for _, r := range refs {
			if r.Kind == agent.RefMachineState || r.Kind == agent.RefMeasurement {
				payload.Evidence = append(payload.Evidence, approvals.Ref{Kind: r.Kind, ID: r.RefID, Revision: r.Revision})
			}
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	payload.GeneratedByRun = sc.RunID
	if !slices.ContainsFunc(payload.Evidence, func(r approvals.Ref) bool { return r.Kind == agent.RefMachineState }) {
		t.Fatalf("setup: the run captured no stoppage: %+v", payload.Evidence)
	}
	code := func() string {
		t.Helper()
		var out string
		if err := w.store.InTx(w.ctx, scope, func(tx *sql.Tx) error {
			var e error
			out, e = agent.DispatchEvidenceCode(w.ctx, tx, w.tenant, w.site, payload)
			return e
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := code(); got != "" {
		t.Fatalf("current evidence refused: %s", got)
	}
	tampered := payload
	tampered.Evidence = append([]approvals.Ref{}, payload.Evidence...)
	tampered.Evidence[0].Revision++
	if got := func() string {
		var out string
		if err := w.store.InTx(w.ctx, scope, func(tx *sql.Tx) error {
			var e error
			out, e = agent.DispatchEvidenceCode(w.ctx, tx, w.tenant, w.site, tampered)
			return e
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}(); got != approvals.DispatchEvidenceRevised {
		t.Fatalf("a bound revision the run never captured: %q", got)
	}
	corrected := map[string]any{"external_key": "stop-1", "kind": "machine_state", "event_at": "2026-09-27T00:20:00Z", "asset_external_key": "line-01", "revision": 2, "payload": map[string]any{"state": "running"}}
	feed.send(t, corrected) // the source corrects it: the machine was running
	if got := code(); got != approvals.DispatchEvidenceRevoked {
		t.Fatalf("a corrected stoppage was not refused at dispatch: %q", got)
	}
}

// TestWIS277DispatchRechecksContextsAndFreshness (review of #188, B1 remainder and M4): at
// dispatch the bound measurements' contexts must still be eligible, exactly as before submission,
// and every source behind the contexts and stoppages must have heartbeated within its freshness.
// Each refusal below would pass without its check: unchanged revisions alone prove neither.
func TestWIS277DispatchRechecksContextsAndFreshness(t *testing.T) {
	w := newWorld(t)
	feed := w.newMachineFeed(t, "machine_state,heartbeat")
	feed.send(t, map[string]any{"external_key": "stop-1", "kind": "machine_state", "event_at": "2026-09-27T00:20:00Z", "asset_external_key": "line-01", "payload": map[string]any{"state": "stopped"}})
	scope := agent.RunScope{TenantID: w.tenant, SiteID: w.site}
	sc, _ := w.openRun(t, agent.DefaultLimits())
	payload := approvals.Payload{GeneratedByRun: sc.RunID}
	if err := w.store.InTx(w.ctx, scope, func(tx *sql.Tx) error {
		refs, e := agent.RefsTx(w.ctx, tx, scope, sc.RunID)
		for _, r := range refs {
			if r.Kind == agent.RefMachineState || r.Kind == agent.RefMeasurement {
				payload.Evidence = append(payload.Evidence, approvals.Ref{Kind: r.Kind, ID: r.RefID, Revision: r.Revision})
			}
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(payload.Evidence, func(r approvals.Ref) bool { return r.Kind == agent.RefMeasurement }) ||
		!slices.ContainsFunc(payload.Evidence, func(r approvals.Ref) bool { return r.Kind == agent.RefMachineState }) {
		t.Fatalf("setup: the run captured no measurement or stoppage: %+v", payload.Evidence)
	}
	code := func() string {
		t.Helper()
		var out string
		if err := w.store.InTx(w.ctx, scope, func(tx *sql.Tx) error {
			var e error
			out, e = agent.DispatchEvidenceCode(w.ctx, tx, w.tenant, w.site, payload)
			return e
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	// heartbeat sets a source's latest heartbeat (fixture write, triggers off).
	heartbeat := func(source, at string) {
		t.Helper()
		tx, err := w.control.BeginTx(w.ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		for _, q := range []string{`SET LOCAL session_replication_role = replica`,
			`UPDATE supervisor.operational_source_progress_revisions r SET last_heartbeat_received_at=` + at + ` FROM supervisor.operational_source_progress_heads h
 WHERE (r.tenant_id,r.site_id,r.source_id,r.revision)=(h.tenant_id,h.site_id,h.source_id,h.current_revision) AND h.source_id='` + source + `'`} {
			if _, err = tx.ExecContext(w.ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if got := code(); got != "" {
		t.Fatalf("current, fresh evidence refused: %s", got)
	}
	// The stoppage source stops heartbeating (freshness 3600 s): its revisions are unchanged.
	heartbeat(feed.sourceID, `clock_timestamp()-interval '2 hours'`)
	if got := code(); got != approvals.DispatchEvidenceStale {
		t.Fatalf("a stale machine source was not refused: %q", got)
	}
	heartbeat(feed.sourceID, `clock_timestamp()`)
	production := w.contexts[1].SourcePolicyRevisionIDs[0].SourceID
	heartbeat(production, `clock_timestamp()-interval '2 hours'`)
	if got := code(); got != approvals.DispatchEvidenceStale {
		t.Fatalf("a stale production source was not refused: %q", got)
	}
	heartbeat(production, `clock_timestamp()`)
	if got := code(); got != "" {
		t.Fatalf("fresh again but refused: %s", got)
	}
	// A late record in the case window: the context is no longer what selection would use
	// (INPUTS_CHANGED), though its stored revision and digest are untouched.
	w.sendLateDelta()
	if got := code(); got != approvals.DispatchEvidenceRevised {
		t.Fatalf("an ineligible context was not refused at dispatch: %q", got)
	}
}
