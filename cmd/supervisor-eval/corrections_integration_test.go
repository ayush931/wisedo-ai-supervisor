//go:build integration

package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/feedback"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/google/uuid"
)

func TestWIS703ApprovedCaseLoadsAndExecutesPinnedSnapshot(t *testing.T) {
	ctx := context.Background()
	admin, db, done, err := disposable(ctx, os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	h := harness{ctx: ctx, control: admin, db: db, webDir: t.TempDir(), mode: ModeDeterministic, model: "fixture-model", revision: "sha256:fixture", agentCfg: fixtureAgent()}
	spec := ChatWorld{Document: []string{"The press cycle time is 60 seconds."}, Facts: []ChatFact{{Symbol: "cycle", SubjectType: "operation", Subject: "press", Predicate: "cycle_time_seconds", Value: "60", Unit: "s", Paragraph: 0, Approved: true}}}
	w, err := h.newChatWorld(spec)
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	actor := knowledge.Actor{Issuer: w.issuer, Subject: w.owner.subject, SessionHash: w.owner.hash}
	svc := knowledge.New(db.DB)
	sc := knowledge.Scope{TenantID: w.tenant, SiteID: w.site}
	initial, err := svc.ReadSnapshot(ctx, actor, sc, w.snapshot)
	must(err)
	conversation, reply, runID, err := w.ask("What is the press cycle time?")
	must(err)
	script := ChatCase{Script: ChatScript{Status: "answered", MissingInputs: []string{}, Statements: []ScriptedStatement{{Kind: "approved_fact", Text: "The press cycle time is 60 seconds.", Cite: []string{"cycle"}}}}}
	answer, err := h.runtime(w.script(spec, script, runID)).ChatAnswer(ctx, agent.ChatAnswerRef{TenantID: w.tenant, SiteID: w.site, RunID: runID})
	must(err)
	if answer.ReplyState != "answered" {
		t.Fatalf("original answer: %+v", answer)
	}
	_, spans, err := w.document([]string{"The press cycle time is 45 seconds."})
	must(err)
	claim := initial.Contents.Claims[0].ClaimInput
	claim.Literal = &knowledge.Literal{Type: "decimal", Value: json.RawMessage(`"45"`), Unit: "s"}
	claim.SourceSpanIDs = spans
	correction := knowledge.MemoryRevisionInput{Reason: "Corrected standard", Question: "What is the press cycle time?", ExpectedAnswer: "The press cycle time is 45 seconds.", Claim: &claim, ExpectedBaseRevision: 1}
	in := feedback.FeedbackInputV1{SchemaVersion: 1, Subject: feedback.SubjectInput{Kind: feedback.SubjectAnswer, ConversationID: &conversation, MessageID: &reply}, Kind: feedback.KindCorrection, Verdict: feedback.VerdictUnhelpful, Reason: correction.Reason, EvidenceIDs: []string{}, ProposedCorrection: &correction}
	var saved feedback.FeedbackV1
	must(w.call(w.asker, "POST", "/feedback", in, -1, 201, &saved))
	var approved knowledge.MemoryCandidate
	must(w.call(w.owner, "POST", "/memory-candidates/"+*saved.CandidateID+"/review", knowledge.MemoryReviewInput{Verdict: "approve", Reason: "Verified", ExpectedRevision: 1}, 1, 200, &approved))
	page, err := svc.ListCorrectionEvaluationCases(ctx, actor, sc, "", 100)
	must(err)
	if len(page.Cases) != 1 {
		t.Fatalf("cases=%d", len(page.Cases))
	}
	set := CorrectionSet{SchemaVersion: 1, TenantID: w.tenant, SiteID: w.site, SnapshotID: w.snapshot, CaseIDs: []string{page.Cases[0].CaseID}}
	cases, err := correctionCases(ctx, svc, actor, set)
	must(err)
	if correctionSnapshot(cases[0], initial) != "AWAITING_SNAPSHOT_OR_UNAVAILABLE_APPROVAL" {
		t.Fatal("unpublished correction passed")
	}
	inventory, err := svc.ReadInventory(ctx, actor, sc)
	must(err)
	published, err := svc.CreateSnapshot(ctx, actor, sc, knowledge.SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Human correction publication"}, uuid.NewString(), inventory.Revision)
	must(err)
	set.SnapshotID = published.SnapshotID
	// A newer publication cannot silently replace the evaluator's selected historical snapshot.
	_, err = svc.CreateSnapshot(ctx, actor, sc, knowledge.SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Later human publication"}, uuid.NewString(), inventory.Revision)
	must(err)
	mustSnapshot := correctionSnapshot(cases[0], published)
	if mustSnapshot != "" {
		t.Fatal(mustSnapshot)
	}
	o, err := executeCorrection(ctx, h, actor, set, cases[0], published)
	must(err)
	if failures := correctionAssertions(cases[0], o); len(failures) != 0 {
		t.Fatalf("%v; observed=%+v", failures, o)
	}
	if o.Answer.SnapshotID == nil || *o.Answer.SnapshotID != set.SnapshotID {
		t.Fatal("snapshot pin replaced")
	}
	if o.ProviderCalls != 2 || o.Run.ProviderActual || o.ArtifactSHA256 == "" {
		t.Fatalf("missing executed provenance: %+v", o)
	}
	// The evaluator cannot read another site's export under this browser session.
	foreign := set
	foreign.SiteID = uuid.NewString()
	if _, err = correctionCases(ctx, svc, actor, foreign); err == nil {
		t.Fatal("foreign correction export accepted")
	}
}
