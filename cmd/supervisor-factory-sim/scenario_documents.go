package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/factorysim"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/chat"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

func (s scenarioRunner) pdf(ctx context.Context, id string) (scenarioObservation, error) {
	b, err := typedPDF("SYNTHETIC edge refusal control. Cycle time: 45 seconds.\n", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		return scenarioObservation{}, err
	}
	var out bytes.Buffer
	if id == "encrypted-pdf" {
		// Encryption uses fresh cryptographic randomness; only the refusal outcome is deterministic.
		err = api.Encrypt(bytes.NewReader(b), &out, model.NewAESConfiguration("fixture-reader", "fixture-owner", 256))
	} else {
		var doc *model.Context
		doc, err = api.ReadContext(bytes.NewReader(b), model.NewDefaultConfiguration())
		if err == nil {
			doc.RootDict["OpenAction"] = types.Dict{"S": types.Name("JavaScript"), "JS": types.StringLiteral("void(0)")}
			err = api.WriteContext(doc, &out)
		}
	}
	if err != nil {
		return scenarioObservation{}, err
	}
	status, err := s.l.requestHeaders(ctx, "owner", "POST", "/v1/sites/"+factorysim.SiteID+"/source-documents", s.prefix+id, "", "application/pdf", out.Bytes(), http.Header{"X-Source-Title": []string{"SYNTHETIC-refusal-control"}}, nil)
	o := scenarioObservation{Status: status}
	var problem *fixtureProblem
	if errors.As(err, &problem) {
		o.Code = problem.Code
		err = nil
	}
	o.Evidence = map[string]any{"payload_sha256": sha(out.Bytes()), "synthetic": true}
	return o, err
}
func (s scenarioRunner) source(ctx context.Context, kind, name, text string) (evidence.Source, evidence.Span, error) {
	var source evidence.Source
	var span evidence.Span
	role := "owner"
	if kind == "interview" {
		role = "operator"
	}
	actualKind := kind
	if kind == "manual" {
		actualKind = "document"
	}
	err := s.l.call(ctx, role, "/sources", name, "0", evidence.Input{SourceID: name, Kind: actualKind, Title: "SYNTHETIC " + s.prefix + " " + kind, Text: text}, &source)
	if err != nil {
		return source, span, err
	}
	err = s.l.call(ctx, role, "/sources/"+name+"/spans", name+"-span", "1", map[string]any{"revision": 1, "start_char": 0, "end_char": len([]rune(text))}, &span)
	if err != nil {
		return source, span, err
	}
	if kind == "interview" {
		return source, span, nil
	}
	var authority evidence.Authority
	err = s.l.call(ctx, "owner", "/sources/"+name+"/authority-decisions", name+"authority", "0", evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Scripted synthetic edge evidence"}, &authority)
	if err != nil {
		return source, span, err
	}
	err = s.l.call(ctx, "owner", "/sources/"+name+"/currentness-decisions", name+"current", "0", evidence.CurrentnessInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, AuthorityRevision: 1, AuthorityDecisionID: authority.DecisionID, Verdict: "confirm_current", Reason: "Scripted fixture"}, nil)
	return source, span, err
}
func (s scenarioRunner) snapshot(ctx context.Context, name string) (string, error) {
	var inventory knowledge.Inventory
	if err := s.l.call(ctx, "owner", "/knowledge-inventory", "", "", nil, &inventory); err != nil {
		return "", err
	}
	id := s.id(name)
	err := s.l.call(ctx, "owner", "/process-snapshots", id, strconv.FormatInt(inventory.Revision, 10), knowledge.SnapshotInput{SnapshotID: id, Reason: "Scripted isolated edge snapshot"}, nil)
	return id, err
}

// snapshotClaims reads a published snapshot and returns the IDs of its claims about entity.
func (s scenarioRunner) snapshotClaims(ctx context.Context, snapshot, entity string) ([]string, error) {
	var snap knowledge.Snapshot
	if err := s.l.call(ctx, "owner", "/process-snapshots/"+snapshot, "", "", nil, &snap); err != nil {
		return nil, err
	}
	ids := []string{}
	for _, c := range snap.Contents.Claims {
		if c.SubjectID == entity {
			ids = append(ids, c.ClaimID)
		}
	}
	return ids, nil
}

// firstSortingID keeps the hashed UUID's tail but sorts before every generated source ID. Chat
// sorts captured source IDs (internal/agent/chat.go) and the pilot fixture provider reads the
// first one, so the fixture provider reads and cites this source (WIS-703 made it also pin the
// first approved fact, so the claim ID sorts first too); a real model must still choose
// to cite it. The gate below requires that citation from every provider.
func firstSortingID(id string) string {
	return "00000000-0000-0000-" + id[19:]
}
func (s scenarioRunner) claim(ctx context.Context, id, entity, value, span string, approve bool) error {
	in := knowledge.ClaimInput{ClaimID: id, SubjectID: entity, SubjectRevision: 1, Predicate: "cycle_time_seconds", Literal: &knowledge.Literal{Type: "decimal", Value: json.RawMessage(wire(value)), Unit: "s"}, EffectiveFrom: "2026-09-01T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span}}
	if err := s.l.call(ctx, "planner", "/claims", id, "0", in, nil); err != nil {
		return err
	}
	if approve {
		return s.l.call(ctx, "approver", "/claims/"+id+"/reviews", id+"review", "1", knowledge.ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Scripted exact fictional source review"}, nil)
	}
	return nil
}
func (s scenarioRunner) knowledge(ctx context.Context, id string) (scenarioObservation, error) {
	o := scenarioObservation{}
	machine, manualText := s.manualPassage()
	sourceID := s.id("manual")
	if id == "revoked-document" {
		sourceID = firstSortingID(sourceID)
	}
	source, span, err := s.source(ctx, "manual", sourceID, manualText)
	if err != nil {
		return o, err
	}
	entity := knowledge.EntityInput{EntityID: s.id("entity"), Type: "asset", AssetKind: "machine", Name: machine, SourceID: source.SourceID, SourceRevision: 1}
	if err = s.l.call(ctx, "owner", "/entities", entity.EntityID, "0", entity, nil); err != nil {
		return o, err
	}
	manualClaim := s.id("manual-claim")
	if id == "revoked-document" {
		// The pilot fixture also pins the first approved fact (approved_facts[0], ordered by claim ID).
		manualClaim = firstSortingID(manualClaim)
	}
	if err = s.claim(ctx, manualClaim, entity.EntityID, "45", span.ID, true); err != nil {
		return o, err
	}
	snap, err := s.snapshot(ctx, "before")
	if err != nil {
		return o, err
	}
	before, err := s.snapshotClaims(ctx, snap, entity.EntityID)
	if err != nil {
		return o, err
	}
	if len(before) != 1 || before[0] != manualClaim {
		return o, fmt.Errorf("before snapshot does not hold exactly the approved manual claim: %v", before)
	}
	if id == "stale-snapshot" {
		entity.EntityID = s.id("another-entity")
		entity.Name = "Another fixture spindle"
		if err = s.l.call(ctx, "owner", "/entities", entity.EntityID, "0", entity, nil); err != nil {
			return o, err
		}
		return s.request(ctx, "owner", "GET", "/process-snapshots/"+snap+"/graph?root="+s.id("entity")+"&predicate=cycle_time_seconds&as_of="+url.QueryEscape(time.Now().UTC().Format(time.RFC3339)), "", "", "", nil, nil)
	}
	question := "What is the rated cycle time of " + machine + "?"
	if id == "contradictory-interview" {
		_, other, err := s.source(ctx, "interview", s.id("interview"), "SYNTHETIC operator interview transcript: "+machine+" rated cycle time: 60 seconds.")
		if err != nil {
			return o, err
		}
		if err = s.claim(ctx, s.id("interview-claim"), entity.EntityID, "60", other.ID, false); err != nil {
			return o, err
		}
		manual, interview := manualClaim, s.id("interview-claim")
		conflict := func() (knowledge.ConflictPage, error) {
			var page knowledge.ConflictPage
			if err := s.l.call(ctx, "owner", "/claims/"+manual+"/conflicts", "", "", nil, &page); err != nil {
				return page, err
			}
			if len(page.Conflicts) != 1 || page.Conflicts[0].Kind != "value_conflict" {
				return page, errors.New("competing interview/manual values not retained as one value_conflict")
			}
			pair := []string{page.Conflicts[0].Left.ClaimID, page.Conflicts[0].Right.ClaimID}
			if !slices.Contains(pair, manual) || !slices.Contains(pair, interview) {
				return page, fmt.Errorf("value_conflict names other claims: %v", pair)
			}
			return page, nil
		}
		if _, err = conflict(); err != nil {
			return o, err
		}
		afterSnap, err := s.snapshot(ctx, "after-conflict")
		if err != nil {
			return o, err
		}
		after, err := s.snapshotClaims(ctx, afterSnap, entity.EntityID)
		if err != nil {
			return o, err
		}
		if len(after) != 0 {
			return o, fmt.Errorf("after-conflict snapshot picked a winner: %v", after)
		}
		// Publishing a snapshot must not resolve the conflict.
		conflicts, err := conflict()
		if err != nil {
			return o, err
		}
		o.Status = 200
		o.Code = conflicts.Conflicts[0].Kind
		// The chat reply depends on the model; it is recorded and never gated, even on error.
		observation := map[string]any{"gate": false, "label": "model observation; provider-dependent; not gate evidence"}
		reply, askErr := askManual(ctx, s.l, s.id("conflict-question"), question+" The operator interview disagrees with the manual; which value can be used?")
		if askErr != nil {
			observation["error"] = askErr.Error()
		} else {
			observation["reply"] = reply
		}
		o.Evidence = map[string]any{"conflicts": conflicts, "before_snapshot_claims": before, "after_conflict_snapshot": afterSnap, "after_conflict_snapshot_claims": after, "interview_span": other, "manual_span": span, "chat_observation": observation}
		return o, nil
	}
	reply, conversation, err := askManualConversation(ctx, s.l, s.id("before-revoke"), question)
	if err != nil {
		return o, err
	}
	cited := false
	for _, c := range reply.Citations {
		cited = cited || c.ID == span.ID
	}
	// Revoke before checking the precondition, so a failed run never leaves a first-sorting
	// source behind to sort ahead of the next run's target.
	if err = s.l.call(ctx, "owner", "/sources/"+source.SourceID+"/revocations", s.prefix+"revoke", "1", map[string]string{"reason": "Scripted edge revocation"}, nil); err != nil {
		return o, err
	}
	if reply.State != "answered" || !cited {
		o.Evidence = map[string]any{"before": reply, "revoked_span": span.ID}
		return o, fmt.Errorf("revocation control did not first answer citing the target span: state=%s cited=%t", reply.State, cited)
	}
	var messages chat.MessagePage
	if err = s.l.call(ctx, "owner", "/chat-conversations/"+conversation+"/messages?limit=100", "", "", nil, &messages); err != nil {
		return o, err
	}
	var redacted chat.Message
	for _, m := range messages.Messages {
		if m.MessageID == reply.MessageID {
			redacted = m
		}
	}
	o.Status = 200
	o.Readiness = redacted.State
	if redacted.ReasonCode != nil {
		o.Code = string(*redacted.ReasonCode)
	}
	if redacted.Body != nil || len(redacted.Citations) != 0 {
		return o, errors.New("revoked prior reply still exposes evidence")
	}
	after, err := askManual(ctx, s.l, s.id("after-revoke"), question)
	if err != nil {
		return o, err
	}
	for _, c := range after.Citations {
		if c.ID == span.ID {
			return o, errors.New("new reply cited revoked span")
		}
	}
	// The latest snapshot still holds the claim on the revoked span, so capture refuses it.
	if after.State != "unavailable" || after.ReasonCode == nil || string(*after.ReasonCode) != "SNAPSHOT_EVIDENCE_UNAVAILABLE" || after.Body != nil || len(after.Citations) != 0 {
		return o, fmt.Errorf("post-revocation answer is not unavailable/SNAPSHOT_EVIDENCE_UNAVAILABLE: state=%s", after.State)
	}
	o.Evidence = map[string]any{"before": reply, "before_cited_target": cited, "revoked_span": span.ID, "redacted": redacted, "after": after}
	return o, nil
}

// machineTag names the scenario's machine with letters only. The entity ID is hex, and chat's
// number guard rightly refuses a passage with an exponent-like token such as "13e1", so a name
// built from the raw ID made revoked-document fail on some runs (WIS-709, run 20261112T073838Z).
func (s scenarioRunner) machineTag() string {
	const letters = "abcdefghijklmnop"
	tag := make([]byte, 0, 12)
	for _, r := range s.id("entity") {
		if r == '-' {
			continue
		}
		v := strings.IndexRune("0123456789abcdef", r)
		if v < 0 {
			continue
		}
		tag = append(tag, letters[v])
		if len(tag) == cap(tag) {
			break
		}
	}
	return string(tag)
}

// manualPassage is the scenario machine's name and the manual passage that chat later quotes.
func (s scenarioRunner) manualPassage() (machine, text string) {
	machine = "Facility spindle " + s.machineTag()
	return machine, machine + " rated cycle time: 45 seconds."
}
