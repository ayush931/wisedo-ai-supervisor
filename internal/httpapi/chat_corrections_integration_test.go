//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/chat"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/google/uuid"
)

func correctionChat(f *stationFixture, user stationSession, claimID string, revision int64, source, span, value string, exact bool) (chat.Message, string) {
	f.t.Helper()
	actor := correctionActor(f, user)
	a := chat.Actor{Issuer: actor.Issuer, Subject: actor.Subject, SessionHash: actor.SessionHash}
	sc := chat.Scope{TenantID: f.tenant, SiteID: f.site}
	svc := chat.NewService(f.runtime).WithAssistant(true).WithAnswering(chat.AnswerSettings{Enabled: true, Provider: agent.ProviderFixture, Model: "fixture-model", ModelRev: "sha256:fixture"})
	conv, err := svc.Create(f.ctx, a, sc, chat.CreateInput{Title: "Cycle time"}, uuid.NewString())
	if err != nil {
		f.t.Fatal(err)
	}
	result, err := svc.Post(f.ctx, a, sc, conv.ConversationID, chat.PostInput{RequestID: uuid.NewString(), Text: "What is the press cycle time?"}, uuid.NewString(), conv.Revision)
	if err != nil {
		f.t.Fatal(err)
	}
	if result.Reply.State != "pending" {
		f.t.Fatalf("no answer run: %+v", result.Reply)
	}
	var run string
	if err = f.control.QueryRowContext(f.ctx, `SELECT run_id::text FROM supervisor.chat_messages WHERE tenant_id=$1 AND site_id=$2 AND message_id=$3`, f.tenant, f.site, result.Reply.MessageID).Scan(&run); err != nil {
		f.t.Fatal(err)
	}
	stmt := agent.ChatStatement{Kind: "approved_fact", Text: "The press cycle time is " + value + " seconds.", SourceSpanIDs: []string{span}}
	if exact {
		stmt.ClaimRefs = []agent.ChatClaimRef{{ClaimID: claimID, Revision: revision}}
	}
	body, _ := json.Marshal(agent.ChatAnswerOutputV2{SchemaVersion: agent.ChatSchemaVersion, RunID: uuid.NewSHA1(uuid.MustParse(run), []byte(agent.RoleDiscoveryMapping)).String(), Role: agent.RoleDiscoveryMapping, Status: "answered", Statements: []agent.ChatStatement{stmt}, MissingInputs: []string{}})
	p := agent.NewFixtureProvider(agent.FixtureStep{Response: agent.ModelResponse{ToolCalls: []agent.ToolCall{{ID: "read", Name: agent.ToolEvidenceGet, Arguments: json.RawMessage(`{"evidence_id":"` + source + `"}`)}}}}, agent.FixtureStep{Response: agent.ModelResponse{Content: string(body)}})
	rt := &agent.Runtime{Store: agent.NewStore(f.runtime), Provider: p, Tools: &agent.Tools{Store: agent.NewStore(f.runtime)}, Slots: agent.NewSlots(2, 8), Limits: agent.DefaultLimits(), Model: "fixture-model", ModelRevision: "sha256:fixture", Options: agent.LoaderOptions{ModeEnabled: true}}
	outcome, err := rt.ChatAnswer(f.ctx, agent.ChatAnswerRef{TenantID: f.tenant, SiteID: f.site, RunID: run})
	if err != nil || outcome.ReplyState != "answered" {
		f.t.Fatalf("answer %+v %v", outcome, err)
	}
	page, err := svc.Messages(f.ctx, a, sc, conv.ConversationID, 20, "")
	if err != nil {
		f.t.Fatal(err)
	}
	return page.Messages[len(page.Messages)-1], conv.ConversationID
}

func TestWIS703PublishedCorrectionAnswersAndExactAttribution(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	in, sc, old, snapshot, source := correctionKnowledge(f)
	oldSource := ""
	for _, s := range snapshot.Contents.Spans {
		if s.SpanID == old.SourceSpanIDs[0] {
			oldSource = s.SourceID
		}
	}
	historical, _ := correctionChat(f, f.lead, old.ClaimID, 1, oldSource, old.SourceSpanIDs[0], "60", true)
	if len(historical.Corrections) != 0 {
		t.Fatal("uncorrected revision labeled")
	}
	got := submitCorrection(f, in)
	ks := knowledge.New(f.runtime)
	ka := correctionActor(f, f.approver)
	approved, err := ks.DecideMemoryReview(ctx, ka, sc, *got.CandidateID, knowledge.MemoryReviewInput{Verdict: "approve", Reason: "Verified new standard", ExpectedRevision: 1}, uuid.NewString(), 1)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := correctionChat(f, f.lead, old.ClaimID, 1, oldSource, old.SourceSpanIDs[0], "60", true)
	if len(before.Corrections) != 0 || before.SnapshotID == nil || *before.SnapshotID != snapshot.SnapshotID {
		t.Fatal("unpublished correction leaked into answer")
	}
	// A different fact sharing exactly the corrected span must not inherit its attribution.
	shared := *in.ProposedCorrection.Claim
	shared.ClaimID = uuid.NewString()
	shared.Predicate = "setup_time_seconds"
	if _, err = ks.CreateClaim(ctx, ka, sc, shared, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err = ks.DecideReview(ctx, ka, sc, shared.ClaimID, knowledge.ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Separate approved fact"}, uuid.NewString(), 1); err != nil {
		t.Fatal(err)
	}
	inv, err := ks.ReadInventory(ctx, ka, sc)
	if err != nil {
		t.Fatal(err)
	}
	next, err := ks.CreateSnapshot(ctx, ka, sc, knowledge.SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Publish correction"}, uuid.NewString(), inv.Revision)
	if err != nil {
		t.Fatal(err)
	}
	var corrected chat.Message
	var conversation string
	for _, user := range []stationSession{f.lead, f.operator} {
		corrected, conversation = correctionChat(f, user, old.ClaimID, 2, source, shared.SourceSpanIDs[0], "45", true)
		if len(corrected.ClaimCitations) != 1 || corrected.ClaimCitations[0].ClaimID != old.ClaimID || corrected.ClaimCitations[0].Revision != 2 || len(corrected.Corrections) != 1 || corrected.Corrections[0].Reviewer != "Site reviewer" || corrected.Corrections[0].ReviewedAt.IsZero() || corrected.SnapshotID == nil || *corrected.SnapshotID != next.SnapshotID {
			t.Fatalf("corrected answer %+v", corrected)
		}
	}
	f.exec(`INSERT INTO supervisor.member_profiles(tenant_id,issuer,subject,display_name) VALUES($1,$2,$3,'Morgan')`, f.tenant, f.issuer, f.approver.subject)
	corrected, conversation = correctionChat(f, f.owner, old.ClaimID, 2, source, shared.SourceSpanIDs[0], "45", true)
	if len(corrected.Corrections) != 1 || corrected.Corrections[0].Reviewer != "Morgan" {
		t.Fatalf("reviewer display name: %+v", corrected.Corrections)
	}
	unrelated, _ := correctionChat(f, f.lead, shared.ClaimID, 1, source, shared.SourceSpanIDs[0], "45", true)
	legacy, _ := correctionChat(f, f.lead, "", 0, source, shared.SourceSpanIDs[0], "45", false)
	if len(unrelated.Corrections) != 0 || len(legacy.Corrections) != 0 || len(legacy.ClaimCitations) != 0 {
		t.Fatal("shared span or span-only answer labeled as corrected")
	}
	// Withdrawing canonical approval suppresses stored content, including its correction label.
	if _, err = ks.RevokeReview(ctx, ka, sc, old.ClaimID, knowledge.RevocationInput{ClaimRevision: 2, ReviewID: *approved.Review.CanonicalReviewID, Reason: "Withdraw corrected approval"}, uuid.NewString(), 2); err != nil {
		t.Fatal(err)
	}
	actor := correctionActor(f, f.owner)
	page, err := chat.NewService(f.runtime).Messages(ctx, chat.Actor{Issuer: actor.Issuer, Subject: actor.Subject, SessionHash: actor.SessionHash}, chat.Scope{TenantID: f.tenant, SiteID: f.site}, conversation, 20, "")
	if err != nil {
		t.Fatal(err)
	}
	last := page.Messages[len(page.Messages)-1]
	if last.Body != nil || len(last.ClaimCitations) != 0 || len(last.Corrections) != 0 || last.ReasonCode == nil || *last.ReasonCode != agent.CodeFactNotApproved {
		t.Fatalf("withdrawn answer leaked: %+v", last)
	}
}
