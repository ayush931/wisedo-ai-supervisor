//go:build integration

package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/feedback"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/google/uuid"
)

func correctionActor(f *stationFixture, a stationSession) knowledge.Actor {
	sum := sha256.Sum256([]byte(a.token))
	return knowledge.Actor{Issuer: f.issuer, Subject: a.subject, SessionHash: hex.EncodeToString(sum[:])}
}

func correctionAnswer(f *stationFixture, a stationSession, spans ...string) feedback.FeedbackInputV1 {
	conversation, question, answer := uuid.NewString(), uuid.NewString(), uuid.NewString()
	f.exec(`INSERT INTO supervisor.chat_conversations(tenant_id,site_id,conversation_id,author_issuer,author_subject,title,revision) VALUES($1,$2,$3,$4,$5,'Correct this answer',1)`, f.tenant, f.site, conversation, f.issuer, a.subject)
	f.exec(`INSERT INTO supervisor.chat_messages(tenant_id,site_id,conversation_id,message_id,seq,author_kind,request_id,body,state,revision) VALUES($1,$2,$3,$4,1,'human',$5,'What is the cycle time?','received',1)`, f.tenant, f.site, conversation, question, uuid.NewString())
	f.exec(`INSERT INTO supervisor.chat_messages(tenant_id,site_id,conversation_id,message_id,seq,author_kind,reply_to,state,revision) VALUES($1,$2,$3,$4,2,'assistant',$5,'pending',1)`, f.tenant, f.site, conversation, answer, question)
	citations := []map[string]any{}
	for _, id := range spans {
		citations = append(citations, map[string]any{"kind": "evidence_span", "id": id, "revision": 1})
	}
	f.exec(`UPDATE supervisor.chat_messages SET state='answered',body='The cycle time is 60 seconds.',citations=$4::jsonb,revision=revision+1 WHERE tenant_id=$1 AND site_id=$2 AND message_id=$3`, f.tenant, f.site, answer, f.encode(citations))
	return feedback.FeedbackInputV1{SchemaVersion: 1, Subject: feedback.SubjectInput{Kind: feedback.SubjectAnswer, ConversationID: &conversation, MessageID: &answer}, Kind: feedback.KindCorrection, Verdict: feedback.VerdictUnhelpful, Reason: "The cycle time changed", EvidenceIDs: []string{}}
}

func TestWIS703FeedbackCandidateAtomicPrivateAndIncomplete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	in := correctionAnswer(f, f.lead)
	path := "/v1/sites/" + f.site + "/feedback"
	key := uuid.NewString()
	before := f.count(`SELECT count(*) FROM supervisor.knowledge_claim_revisions WHERE tenant_id=$1 AND site_id=$2`, f.tenant, f.site)
	w := f.call(f.lead, "POST", path, key, "", f.encode(in))
	f.expect(w, 201, "")
	var got feedback.FeedbackV1
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.CandidateID == nil {
		t.Fatalf("candidate not atomically returned: %s %v", w.Body, err)
	}
	candidateID := *got.CandidateID
	w = f.call(f.lead, "POST", path, key, "", f.encode(in))
	f.expect(w, 200, "")
	var replay feedback.FeedbackV1
	if err := json.Unmarshal(w.Body.Bytes(), &replay); err != nil {
		t.Fatal(err)
	}
	if replay.ID != got.ID || replay.CandidateID == nil || *replay.CandidateID != candidateID {
		t.Fatal("lost response changed feedback/candidate identity")
	}
	if n := f.count(`SELECT count(*) FROM supervisor.knowledge_memory_candidates WHERE tenant_id=$1 AND site_id=$2`, f.tenant, f.site); n != 1 {
		t.Fatalf("duplicate candidates: %d", n)
	}
	if n := f.count(`SELECT count(*) FROM supervisor.knowledge_claim_revisions WHERE tenant_id=$1 AND site_id=$2`, f.tenant, f.site); n != before {
		t.Fatal("submission advanced claim inventory")
	}
	in.Reason = "different body"
	f.expect(f.call(f.lead, "POST", path, key, "", f.encode(in)), 409, "IDEMPOTENCY_CONFLICT")
	f.expect(f.call(f.lead, "POST", path, "", "", f.encode(in)), 422, "")
	f.expect(f.call(f.approver, "POST", path, uuid.NewString(), "", f.encode(in)), 404, "NOT_FOUND")
	f.expect(f.call(f.outsider, "POST", path, uuid.NewString(), "", f.encode(in)), 404, "NOT_FOUND")
	f.expect(f.call(f.lead, "POST", path, uuid.NewString(), "", `{"schema_version":1,"actor":"forged"}`), 422, "INVALID_FEEDBACK")
	service := knowledge.New(f.runtime)
	scope := knowledge.Scope{TenantID: f.tenant, SiteID: f.site}
	out, err := service.ReadMemoryCandidate(ctx, correctionActor(f, f.approver), scope, candidateID)
	if err != nil {
		t.Fatal(err)
	}
	if out.ReviewState != "candidate" || len(out.MissingRequirements) != 4 || out.Original.Question == nil || out.Original.Answer == nil {
		t.Fatalf("incomplete candidate: %+v", out)
	}
	// Narrow turn sharing does not change author-only conversation permissions.
	f.expect(f.call(f.approver, "GET", "/v1/sites/"+f.site+"/chat-conversations/"+*in.Subject.ConversationID+"/messages", "", "", ""), 404, "")
	if _, err = service.ReadMemoryCandidate(ctx, correctionActor(f, f.operator), scope, candidateID); !errors.Is(err, knowledge.ErrNotFound) {
		t.Fatalf("operator reads another author's candidate: %v", err)
	}
	amended := out.Content
	amended.Question = "What is the cycle time?"
	out, err = service.ReviseMemoryCandidate(ctx, correctionActor(f, f.lead), scope, candidateID, amended, uuid.NewString(), 1)
	if err != nil || out.Revision != 2 {
		t.Fatalf("amend: %+v %v", out, err)
	}
	if _, err = service.ReviseMemoryCandidate(ctx, correctionActor(f, f.approver), scope, candidateID, amended, uuid.NewString(), 2); !errors.Is(err, knowledge.ErrDenied) {
		t.Fatalf("reviewer changed original author's payload: %v", err)
	}
	if _, err = service.ReviseMemoryCandidate(ctx, correctionActor(f, f.lead), scope, candidateID, amended, uuid.NewString(), 1); !errors.Is(err, knowledge.ErrRevision) {
		t.Fatalf("stale amendment: %v", err)
	}
}

func TestWIS703ReviewRouteCannotPromoteIncompleteOrSelfReviewedCorrection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	in := correctionAnswer(f, f.lead)
	w := f.call(f.lead, "POST", "/v1/sites/"+f.site+"/feedback", uuid.NewString(), "", f.encode(in))
	f.expect(w, 201, "")
	var got feedback.FeedbackV1
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.CandidateID == nil {
		t.Fatal("no candidate", err)
	}
	path := "/v1/sites/" + f.site + "/memory-candidates/" + *got.CandidateID + "/review"
	body := `{"verdict":"approve","reason":"Reviewed","expected_revision":1}`
	f.expect(f.call(f.lead, "POST", path, uuid.NewString(), `"1"`, body), 403, "SELF_REVIEW_FORBIDDEN")
	f.expect(f.call(f.operator, "POST", path, uuid.NewString(), `"1"`, body), 403, "PERMISSION_DENIED")
	f.expect(f.call(f.approver, "POST", path, uuid.NewString(), `"1"`, body), 422, "CORRECTION_INCOMPLETE")
	f.expect(f.call(f.stale, "POST", path, uuid.NewString(), `"1"`, body), 428, "REAUTHENTICATION_REQUIRED")
	f.expect(f.call(f.outsider, "POST", path, uuid.NewString(), `"1"`, body), 404, "NOT_FOUND")
	f.expect(f.call(f.approver, "POST", path, "", `"1"`, body), 422, "INVALID_REQUEST")
	f.expect(f.call(f.approver, "POST", path, uuid.NewString(), "", body), 428, "REVISION_REQUIRED")
	f.expect(f.call(f.approver, "POST", path, uuid.NewString(), `"2"`, body), 412, "REVISION_CONFLICT")
	f.expect(f.call(f.approver, "POST", path, uuid.NewString(), `"1"`, `{"verdict":"approve","reason":"Reviewed","expected_revision":1,"passed":true}`), 422, "INVALID_REQUEST")
	badCSRF := f.approver
	badCSRF.csrf = "wrong"
	f.expect(f.call(badCSRF, "POST", path, uuid.NewString(), `"1"`, body), 403, "CSRF_REJECTED")
	if _, err := knowledge.New(f.runtime).DecideMemoryReview(ctx, knowledge.Actor{Issuer: "supervisor:agent", Subject: uuid.NewString()}, knowledge.Scope{TenantID: f.tenant, SiteID: f.site}, *got.CandidateID, knowledge.MemoryReviewInput{Verdict: "approve", Reason: "Agent", ExpectedRevision: 1}, uuid.NewString(), 1); !errors.Is(err, knowledge.ErrDenied) {
		t.Fatalf("service identity review: %v", err)
	}

}

// correctionKnowledge creates actual WIS-270 claims/reviews/snapshot; only the historical chat
// output/run is planted. The mutation under test always goes through runtime-role services/HTTP.
func correctionKnowledge(f *stationFixture) (feedback.FeedbackInputV1, knowledge.Scope, knowledge.ClaimInput, knowledge.Snapshot, string) {
	f.t.Helper()
	sc := knowledge.Scope{TenantID: f.tenant, SiteID: f.site}
	ka := correctionActor(f, f.owner)
	ks := knowledge.New(f.runtime)
	ev := evidence.New(f.runtime)
	ea := evidence.Actor{Issuer: ka.Issuer, Subject: ka.Subject, SessionHash: ka.SessionHash}
	es := evidence.Scope{TenantID: f.tenant, SiteID: f.site}
	source := func(text string) (evidence.Source, string) {
		f.t.Helper()
		src, err := ev.CreateOrRevise(f.ctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Press standard", Text: text}, uuid.NewString(), 0)
		if err != nil {
			f.t.Fatal(err)
		}
		span, err := ev.CreateSpan(f.ctx, ea, es, evidence.SpanInput{SourceID: src.SourceID, Revision: 1, Start: 0, End: len(text)}, uuid.NewString(), 1)
		if err != nil {
			f.t.Fatal(err)
		}
		if _, err = ev.DecideAuthority(f.ctx, ea, es, src.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Reviewed source"}, uuid.NewString(), 0); err != nil {
			f.t.Fatal(err)
		}
		return src, span.ID
	}
	entitySource, _ := source("The press is an operation.")
	oldSource, oldSpan := source("The press cycle time is 60 seconds.")
	newSource, newSpan := source("The press cycle time is 45 seconds.")
	entity, err := ks.CreateEntity(f.ctx, ka, sc, knowledge.EntityInput{EntityID: uuid.NewString(), Type: "operation", Name: "Press", SourceID: entitySource.SourceID, SourceRevision: 1}, uuid.NewString(), 0)
	if err != nil {
		f.t.Fatal(err)
	}
	claim := knowledge.ClaimInput{ClaimID: uuid.NewString(), SubjectID: entity.EntityID, SubjectRevision: 1, Predicate: "cycle_time_seconds", Literal: &knowledge.Literal{Type: "decimal", Value: json.RawMessage(`"60"`), Unit: "s"}, EffectiveFrom: "2026-09-27T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{oldSpan}}
	if _, err = ks.CreateClaim(f.ctx, ka, sc, claim, uuid.NewString(), 0); err != nil {
		f.t.Fatal(err)
	}
	if _, err = ks.DecideReview(f.ctx, ka, sc, claim.ClaimID, knowledge.ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Original standard"}, uuid.NewString(), 1); err != nil {
		f.t.Fatal(err)
	}
	inventory, err := ks.ReadInventory(f.ctx, ka, sc)
	if err != nil {
		f.t.Fatal(err)
	}
	snapshot, err := ks.CreateSnapshot(f.ctx, ka, sc, knowledge.SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Original process"}, uuid.NewString(), inventory.Revision)
	if err != nil {
		f.t.Fatal(err)
	}
	in := correctionAnswer(f, f.lead, oldSpan)
	run := uuid.NewString()
	f.exec(`INSERT INTO supervisor.agent_runs(tenant_id,site_id,id,request_kind,conversation_id,request_id,trace_id,provider_name,provider_actual,model,model_revision,prompt_version,tool_version,policy_revision,invocations_left,tool_calls_left,requests_left,tokens_left,cost_left_microusd,state,ended_at,outcome_status) VALUES($1,$2,$3,'conversation',$4,gen_random_uuid(),repeat('a',32),'fixture',false,'fixture','revision','prompt','tools',1,1,1,1,1,0,'completed',clock_timestamp(),'needs_input')`, f.tenant, f.site, run, *in.Subject.ConversationID)
	f.exec(`INSERT INTO supervisor.agent_run_refs(tenant_id,site_id,run_id,kind,ref_id,revision,sha256) VALUES($1,$2,$3,'snapshot',$4,$5,$6)`, f.tenant, f.site, run, snapshot.SnapshotID, snapshot.InventoryRevision, snapshot.SHA256)
	f.exec(`INSERT INTO supervisor.agent_run_refs(tenant_id,site_id,run_id,kind,ref_id,revision,sha256,access_epoch) VALUES($1,$2,$3,'evidence',$4,1,$5,1)`, f.tenant, f.site, run, oldSource.SourceID, oldSource.SHA256)
	f.exec(`INSERT INTO supervisor.chat_answer_turns(tenant_id,site_id,run_id,conversation_id,question_message_id,reply_message_id) SELECT $1,$2,$3,conversation_id,reply_to,message_id FROM supervisor.chat_messages WHERE tenant_id=$1 AND site_id=$2 AND message_id=$4`, f.tenant, f.site, run, *in.Subject.MessageID)
	corrected := claim
	corrected.Literal = &knowledge.Literal{Type: "decimal", Value: json.RawMessage(`"45"`), Unit: "s"}
	corrected.SourceSpanIDs = []string{newSpan}
	in.ProposedCorrection = &knowledge.MemoryRevisionInput{Reason: "Corrected standard", Claim: &corrected, ExpectedBaseRevision: 1, Question: "What is the press cycle time?", ExpectedAnswer: "The press cycle time is 45 seconds."}
	return in, sc, claim, snapshot, newSource.SourceID
}

func submitCorrection(f *stationFixture, in feedback.FeedbackInputV1) feedback.FeedbackV1 {
	f.t.Helper()
	w := f.call(f.lead, "POST", "/v1/sites/"+f.site+"/feedback", uuid.NewString(), "", f.encode(in))
	f.expect(w, 201, "")
	var out feedback.FeedbackV1
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.CandidateID == nil {
		f.t.Fatalf("candidate %s: %v", w.Body, err)
	}
	return out
}

func TestWIS703ApprovalSupersedesAtomicallyReplayAndExplicitPublication(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	in, sc, old, snapshot, _ := correctionKnowledge(f)
	got := submitCorrection(f, in)
	id := *got.CandidateID
	if got.Provenance == nil || got.Provenance.RunID == nil || got.Provenance.ProcessSnapshotID == nil || *got.Provenance.ProcessSnapshotID != snapshot.SnapshotID || got.Provenance.TerminalSHA256 == nil {
		t.Fatalf("exact original provenance: %+v", got.Provenance)
	}
	ks := knowledge.New(f.runtime)
	ka := correctionActor(f, f.approver)
	review := knowledge.MemoryReviewInput{Verdict: "approve", Reason: "Verified corrected source", ExpectedRevision: 1}
	key := uuid.NewString()
	inv, err := ks.ReadInventory(ctx, ka, sc)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := ks.ReadClaim(ctx, ka, sc, old.ClaimID, 0)
	if err != nil || pending.Revision != 1 {
		t.Fatalf("pending correction displaced approved head: %+v %v", pending, err)
	}
	// The ordinary review route has no pending corrected revision to approve.
	if _, err = ks.DecideReview(ctx, correctionActor(f, f.lead), sc, old.ClaimID, knowledge.ReviewInput{ClaimRevision: 2, Verdict: "approve", Reason: "Bypass"}, uuid.NewString(), 2); !errors.Is(err, knowledge.ErrNotFound) {
		t.Fatalf("direct route bypass: %v", err)
	}
	out, err := ks.DecideMemoryReview(ctx, ka, sc, id, review, key, 1)
	if err != nil {
		t.Fatal(err)
	}
	if out.ReviewState != "approved" || out.PublishedSnapshotID != nil || out.Review == nil || out.Review.ClaimRevision == nil || *out.Review.ClaimRevision != 2 {
		t.Fatalf("approval/publication: %+v", out)
	}
	again, err := knowledge.New(f.runtime).DecideMemoryReview(ctx, ka, sc, id, review, key, 1)
	if err != nil || again.Review.ReviewID != out.Review.ReviewID {
		t.Fatalf("restart/replay: %+v %v", again, err)
	}
	for table, want := range map[string]int{"knowledge_claim_revisions": 2, "knowledge_claim_reviews": 2, "knowledge_memory_candidate_reviews": 1, "correction_evaluation_cases": 1} {
		if n := f.count(`SELECT count(*) FROM supervisor.`+table+` WHERE tenant_id=$1 AND site_id=$2`, f.tenant, f.site); n != want {
			t.Fatalf("%s count %d want %d", table, n, want)
		}
	}
	revised, err := ks.ReadClaim(ctx, ka, sc, old.ClaimID, 2)
	if err != nil || revised.ActorSubject != f.lead.subject || string(revised.Literal.Value) != `"45"` {
		t.Fatalf("corrector attribution/value: %+v %v", revised, err)
	}
	current, err := ks.ReadInventory(ctx, ka, sc)
	if err != nil || current.Revision <= inv.Revision {
		t.Fatalf("approval inventory: %+v %v", current, err)
	}
	next, err := ks.CreateSnapshot(ctx, ka, sc, knowledge.SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Publish reviewed correction"}, uuid.NewString(), current.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Contents.Claims) != 1 || next.Contents.Claims[0].Revision != 2 {
		t.Fatalf("published claims: %+v", next.Contents.Claims)
	}
	out, err = ks.ReadMemoryCandidate(ctx, ka, sc, id)
	if err != nil || out.PublishedSnapshotID == nil || *out.PublishedSnapshotID != next.SnapshotID {
		t.Fatalf("publication readback: %+v %v", out, err)
	}
	summary, err := ks.CorrectionSummary(ctx, ka, sc)
	if err != nil || summary.ApprovedCount != 1 || summary.Timezone != "UTC" {
		t.Fatalf("approval counter %+v %v", summary, err)
	}
}

func TestWIS703RejectedFailedAndCompetingCorrectionsPreserveAuthority(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	in, sc, old, _, sourceID := correctionKnowledge(f)
	ks := knowledge.New(f.runtime)
	ka := correctionActor(f, f.approver)
	inv, err := ks.ReadInventory(ctx, ka, sc)
	if err != nil {
		t.Fatal(err)
	}
	rejected := submitCorrection(f, in)
	if _, err = ks.DecideMemoryReview(ctx, ka, sc, *rejected.CandidateID, knowledge.MemoryReviewInput{Verdict: "reject", Reason: "Not accepted", ExpectedRevision: 1}, uuid.NewString(), 1); err != nil {
		t.Fatal(err)
	}
	after, err := ks.ReadInventory(ctx, ka, sc)
	if err != nil || after.Revision != inv.Revision {
		t.Fatal("rejection changed inventory", err)
	}
	if n := f.count(`SELECT count(*) FROM supervisor.correction_evaluation_cases WHERE tenant_id=$1`, f.tenant); n != 0 {
		t.Fatal("rejection trained a case")
	}
	// Force a late evaluation-ledger failure: neither new head nor canonical review may escape.
	failed := submitCorrection(f, in)
	constraint := "wis703_fail_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	f.exec(`ALTER TABLE supervisor.correction_evaluation_cases ADD CONSTRAINT ` + constraint + ` CHECK(candidate_id<>'` + *failed.CandidateID + `'::uuid) NOT VALID`)
	if _, err = ks.DecideMemoryReview(ctx, ka, sc, *failed.CandidateID, knowledge.MemoryReviewInput{Verdict: "approve", Reason: "Atomic failure", ExpectedRevision: 1}, uuid.NewString(), 1); err == nil {
		t.Fatal("injected evaluation failure committed")
	}
	f.exec(`ALTER TABLE supervisor.correction_evaluation_cases DROP CONSTRAINT ` + constraint)
	claim, err := ks.ReadClaim(ctx, ka, sc, old.ClaimID, 0)
	if err != nil || claim.Revision != 1 {
		t.Fatalf("failed review advanced head %+v %v", claim, err)
	}
	if n := f.count(`SELECT count(*) FROM supervisor.knowledge_memory_candidate_reviews WHERE candidate_id=$1`, *failed.CandidateID); n != 0 {
		t.Fatal("partial decision escaped")
	}
	left, right := submitCorrection(f, in), submitCorrection(f, in)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, id := range []string{*left.CandidateID, *right.CandidateID} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, e := ks.DecideMemoryReview(ctx, ka, sc, id, knowledge.MemoryReviewInput{Verdict: "approve", Reason: "Concurrent review", ExpectedRevision: 1}, uuid.NewString(), 1)
			results <- e
		}(id)
	}
	wg.Wait()
	close(results)
	success, stale := 0, 0
	for e := range results {
		if e == nil {
			success++
		} else if errors.Is(e, knowledge.ErrRevision) {
			stale++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("competing base: %d success %d stale", success, stale)
	}
	// A revoked proposed source cannot be promoted, including an earlier pending candidate.
	f.revokeSource(sourceID)
	if _, err = ks.DecideMemoryReview(ctx, ka, sc, *failed.CandidateID, knowledge.MemoryReviewInput{Verdict: "approve", Reason: "Revoked", ExpectedRevision: 1}, uuid.NewString(), 1); !errors.Is(err, knowledge.ErrNotFound) {
		t.Fatalf("revoked proposal: %v", err)
	}
}

func listMemoryCandidates(f *stationFixture, state string) map[string]string {
	f.t.Helper()
	w := f.call(f.approver, "GET", "/v1/sites/"+f.site+"/memory-candidates?limit=100&state="+state, "", "", "")
	f.expect(w, 200, "")
	var page knowledge.MemoryCandidatePage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		f.t.Fatal(err)
	}
	out := map[string]string{}
	for _, c := range page.Candidates {
		out[c.CandidateID] = c.ReviewState
	}
	return out
}

// WIS-703 review finding 1: a withdrawn (superseded) correction must not be promoted (design §9).
func TestWIS703SupersededCorrectionCannotBeApproved(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	in, sc, old, _, _ := correctionKnowledge(f)
	first := submitCorrection(f, in)
	replacement := in
	replacement.Reason = "The first correction named the wrong source"
	replacement.SupersedesFeedbackID = &first.ID
	second := submitCorrection(f, replacement)
	c1, c2 := *first.CandidateID, *second.CandidateID
	ks := knowledge.New(f.runtime)
	ka := correctionActor(f, f.approver)
	got, err := ks.ReadMemoryCandidate(ctx, ka, sc, c1)
	if err != nil || got.ReviewState != "superseded" {
		t.Fatalf("withdrawn correction state: %q %v", got.ReviewState, err)
	}
	if queue := listMemoryCandidates(f, "in_review"); queue[c1] != "" || queue[c2] != "in_review" {
		t.Fatalf("in_review queue: %v", queue)
	}
	if withdrawn := listMemoryCandidates(f, "superseded"); withdrawn[c1] != "superseded" || len(withdrawn) != 1 {
		t.Fatalf("superseded filter: %v", withdrawn)
	}
	counts := func() [4]int {
		var out [4]int
		for i, table := range []string{"knowledge_claim_revisions", "knowledge_claim_reviews", "knowledge_memory_candidate_reviews", "correction_evaluation_cases"} {
			out[i] = f.count(`SELECT count(*) FROM supervisor.`+table+` WHERE tenant_id=$1 AND site_id=$2`, f.tenant, f.site)
		}
		return out
	}
	before := counts()
	f.expect(f.call(f.approver, "POST", "/v1/sites/"+f.site+"/memory-candidates/"+c1+"/review", uuid.NewString(), `"1"`, `{"verdict":"approve","reason":"Approve the withdrawn value","expected_revision":1}`), 409, "CORRECTION_SUPERSEDED")
	if after := counts(); after != before {
		t.Fatalf("refused approval wrote rows: %v -> %v", before, after)
	}
	if claim, err := ks.ReadClaim(ctx, ka, sc, old.ClaimID, 0); err != nil || claim.Revision != 1 {
		t.Fatalf("refused approval moved the claim head: %+v %v", claim, err)
	}
	// The replacement remains approvable: the refusal is specific to the withdrawn candidate.
	if out, err := ks.DecideMemoryReview(ctx, ka, sc, c2, knowledge.MemoryReviewInput{Verdict: "approve", Reason: "Replacement verified", ExpectedRevision: 1}, uuid.NewString(), 1); err != nil || out.ReviewState != "approved" {
		t.Fatalf("replacement approval: %+v %v", out, err)
	}
}

// WIS-703 supersession follow-up (Codex review of the fix): only approval is refused. Rejection
// of a withdrawn correction succeeds, and a decision recorded before supersession keeps its
// state in reads, filtered lists and idempotent replay.
func TestWIS703SupersessionKeepsRecordedDecisions(t *testing.T) {
	for _, tc := range []struct {
		name, before, after, want string
	}{
		{name: "reject after supersession", after: "reject", want: "rejected"},
		{name: "approval before supersession replays", before: "approve", want: "approved"},
		{name: "rejection before supersession replays", before: "reject", want: "rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			f := newStationFixture(t, ctx)
			in, sc, _, _, _ := correctionKnowledge(f)
			first := submitCorrection(f, in)
			c1 := *first.CandidateID
			path := "/v1/sites/" + f.site + "/memory-candidates/" + c1 + "/review"
			decide := func(verdict, key string) knowledge.MemoryCandidate {
				t.Helper()
				w := f.call(f.approver, "POST", path, key, `"1"`, `{"verdict":"`+verdict+`","reason":"Recorded decision","expected_revision":1}`)
				f.expect(w, 200, "")
				var out knowledge.MemoryCandidate
				if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Review == nil {
					t.Fatalf("decision body %s: %v", w.Body, err)
				}
				return out
			}
			counts := func() [4]int {
				var out [4]int
				for i, table := range []string{"knowledge_claim_revisions", "knowledge_claim_reviews", "knowledge_memory_candidate_reviews", "correction_evaluation_cases"} {
					out[i] = f.count(`SELECT count(*) FROM supervisor.`+table+` WHERE tenant_id=$1 AND site_id=$2`, f.tenant, f.site)
				}
				return out
			}
			key := uuid.NewString()
			var recorded knowledge.MemoryCandidate
			if tc.before != "" {
				recorded = decide(tc.before, key)
			}
			replacement := in
			replacement.Reason = "Replacing my earlier correction"
			replacement.SupersedesFeedbackID = &first.ID
			submitCorrection(f, replacement)
			before := counts()
			if tc.before == "" {
				out := decide(tc.after, key)
				if out.ReviewState != tc.want {
					t.Fatalf("decision state %q want %q", out.ReviewState, tc.want)
				}
				if after := counts(); after != [4]int{before[0], before[1], before[2] + 1, before[3]} {
					t.Fatalf("rejection rows: %v -> %v", before, after)
				}
			} else {
				out := decide(tc.before, key)
				if out.Review.ReviewID != recorded.Review.ReviewID || out.ReviewState != tc.want {
					t.Fatalf("replay changed the recorded decision: %+v vs %+v", out.Review, recorded.Review)
				}
				if after := counts(); after != before {
					t.Fatalf("replay wrote rows: %v -> %v", before, after)
				}
			}
			got, err := knowledge.New(f.runtime).ReadMemoryCandidate(ctx, correctionActor(f, f.approver), sc, c1)
			if err != nil || got.ReviewState != tc.want {
				t.Fatalf("read state %q want %q: %v", got.ReviewState, tc.want, err)
			}
			for _, state := range []string{"approved", "rejected", "superseded", "in_review", "candidate"} {
				listed, want := listMemoryCandidates(f, state)[c1], ""
				if state == tc.want {
					want = tc.want
				}
				if listed != want {
					t.Fatalf("state=%s lists c1 as %q (want only under %s)", state, listed, tc.want)
				}
			}
		})
	}
}

// WIS-703 review finding 3: the in_review filter applies the same 1000-character expected
// answer limit as approval, so the queue never lists a candidate that approval refuses.
func TestWIS703InReviewFilterAppliesExpectedAnswerLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	in, _, _, _, _ := correctionKnowledge(f)
	long := *in.ProposedCorrection
	long.ExpectedAnswer = strings.Repeat("é", 1001) // 1001 characters, 2002 bytes
	in.ProposedCorrection = &long
	id := *submitCorrection(f, in).CandidateID
	if queue := listMemoryCandidates(f, "in_review"); queue[id] != "" {
		t.Fatalf("over-long expectation listed for review: %v", queue)
	}
	if pending := listMemoryCandidates(f, "candidate"); pending[id] != "candidate" {
		t.Fatalf("over-long expectation missing from candidates: %v", pending)
	}
	f.expect(f.call(f.approver, "POST", "/v1/sites/"+f.site+"/memory-candidates/"+id+"/review", uuid.NewString(), `"1"`, `{"verdict":"approve","reason":"Too long","expected_revision":1}`), 422, "CORRECTION_INCOMPLETE")
	long.ExpectedAnswer = strings.Repeat("é", 1000)
	in.ProposedCorrection = &long
	exact := *submitCorrection(f, in).CandidateID
	if queue := listMemoryCandidates(f, "in_review"); queue[exact] != "in_review" {
		t.Fatalf("1000-character expectation not in review: %v", queue)
	}
}

func TestWIS703InvestigationPinsHistoricalRunArtifactAndTerminalOutcome(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	_, _, _, snapshot, _ := correctionKnowledge(f)
	// Only seed historical case/context rows; feedback and its FK/provenance checks use the runtime.
	plant := func(q string, args ...any) {
		t.Helper()
		tx, e := f.control.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		if _, e = tx.ExecContext(ctx, `SET LOCAL session_replication_role=replica`); e != nil {
			t.Fatal(e)
		}
		if _, e = tx.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
		if e = tx.Commit(); e != nil {
			t.Fatal(e)
		}
	}
	caseID, line, order := uuid.NewString(), uuid.NewString(), uuid.NewString()
	contextIDs := []string{"ffffffff-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002", uuid.NewString()}
	for i, id := range contextIDs {
		starts := time.Date(2026, 10, 20, 6, i*15, 0, 0, time.UTC)
		digest := strings.Repeat(string(rune('a'+i)), 64)
		body := f.encode(map[string]any{"readiness": "NOT_READY", "digest": digest, "context_id": id, "order_id": order, "asset_id": line, "site_id": f.site})
		plant(`INSERT INTO supervisor.plant_contexts(tenant_id,site_id,id,order_id,asset_id,window_start,window_end,process_snapshot_id,process_snapshot_sha256,readiness,snapshot,digest,actor_issuer,actor_subject,evaluated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'NOT_READY',$10,$11,'fixture','fixture',$7)`, f.tenant, f.site, id, order, line, starts, starts.Add(15*time.Minute), snapshot.SnapshotID, snapshot.SHA256, body, digest)
	}
	plant(`INSERT INTO supervisor.cases(tenant_id,site_id,id,line_id,order_id,exception_kind,episode,revision,context_snapshot_id,context_revision,context_digest,state,detected_at,latest_window_end,workflow_id) VALUES($1,$2,$3,$4,$5,'production_shortfall',1,5,$6,1,$7,'needs_input',now(),now(),$8)`, f.tenant, f.site, caseID, line, order, contextIDs[2], strings.Repeat("c", 64), "case:"+f.tenant+":"+caseID)
	states := []string{"none", "detected", "investigating", "needs_input", "investigating", "needs_input"}
	for i := 1; i < len(states); i++ {
		plant(`INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,$6,'fixture','fixture','fixture')`, f.tenant, f.site, caseID, i, states[i-1], states[i])
	}
	for i, withArtifact := range []bool{true, false} {
		run := uuid.NewString()
		revision := int64(2 + i*2)
		f.exec(`INSERT INTO supervisor.agent_runs(tenant_id,site_id,id,request_kind,case_id,case_revision,trace_id,provider_name,provider_actual,model,model_revision,prompt_version,tool_version,policy_revision,invocations_left,tool_calls_left,requests_left,tokens_left,cost_left_microusd) VALUES($1,$2,$3,'case',$4,$5,repeat('a',32),'fixture',false,'fixture','revision','prompt','tools',1,1,1,1,1,0)`, f.tenant, f.site, run, caseID, revision)
		for j, id := range contextIDs[:2] {
			f.exec(`INSERT INTO supervisor.agent_run_refs(tenant_id,site_id,run_id,kind,ref_id,revision,sha256) VALUES($1,$2,$3,'context',$4,1,$5)`, f.tenant, f.site, run, id, strings.Repeat(string(rune('a'+j)), 64))
		}
		f.exec(`INSERT INTO supervisor.agent_run_refs(tenant_id,site_id,run_id,kind,ref_id,revision,sha256) VALUES($1,$2,$3,'snapshot',$4,$5,$6)`, f.tenant, f.site, run, snapshot.SnapshotID, snapshot.InventoryRevision, snapshot.SHA256)
		var artifact any
		sha := ""
		if withArtifact {
			artifact = uuid.NewString()
			sha = strings.Repeat("d", 64)
			f.exec(`INSERT INTO supervisor.agent_artifacts(tenant_id,site_id,id,run_id,schema,body,sha256) VALUES($1,$2,$3,$4,'recovery_proposal_v1','{"rationale":"The earlier investigation rationale."}',$5)`, f.tenant, f.site, artifact, run, sha)
		}
		f.exec(`UPDATE supervisor.agent_runs SET state='completed',ended_at=clock_timestamp(),outcome_status='needs_input',outcome_reasons=ARRAY['REQUIRED_INPUT_MISSING'],artifact_id=$4 WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, f.tenant, f.site, run, artifact)
		in := feedback.FeedbackInputV1{SchemaVersion: 1, Subject: feedback.SubjectInput{Kind: feedback.SubjectInvestigation, RunID: &run}, Kind: feedback.KindCorrection, Verdict: feedback.VerdictUnhelpful, Reason: "Wrong investigation", EvidenceIDs: []string{}}
		got := submitCorrection(f, in)
		if got.Subject.CaseRevision == nil || *got.Subject.CaseRevision != revision || got.ContextSnapshotID == nil || *got.ContextSnapshotID != contextIDs[1] || got.Provenance == nil || got.Provenance.RunID == nil || *got.Provenance.RunID != run {
			t.Fatalf("bound current case instead of captured history: %+v", got)
		}
		if withArtifact && (got.Provenance.ArtifactSHA256 == nil || *got.Provenance.ArtifactSHA256 != sha || got.Provenance.TerminalSHA256 != nil) {
			t.Fatalf("artifact pin %+v", got.Provenance)
		}
		if !withArtifact && (got.Provenance.ArtifactID != nil || got.Provenance.TerminalSHA256 == nil) {
			t.Fatalf("terminal pin %+v", got.Provenance)
		}
		candidate, err := knowledge.New(f.runtime).ReadMemoryCandidate(ctx, correctionActor(f, f.approver), knowledge.Scope{TenantID: f.tenant, SiteID: f.site}, *got.CandidateID)
		if err != nil {
			t.Fatal(err)
		}
		if candidate.Original.OutcomeStatus != "needs_input" || len(candidate.Original.OutcomeReasons) != 1 || (withArtifact && candidate.Original.InvestigationRationale == nil) {
			t.Fatalf("original investigation projection %+v", candidate.Original)
		}
		f.expect(f.call(f.outsider, "POST", "/v1/sites/"+f.site+"/feedback", uuid.NewString(), "", f.encode(in)), 404, "NOT_FOUND")
	}
}

func TestWIS703ProposedReferencesFailClosedButHistoricalEvidenceMayBeCorrected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	in, sc, old, _, _ := correctionKnowledge(f)
	_, foreignSpan := f.draftSpan(f.outsider, f.otherSite, "Foreign process")
	wrong := *in.ProposedCorrection
	claim := *wrong.Claim
	claim.SourceSpanIDs = []string{foreignSpan}
	wrong.Claim = &claim
	in.ProposedCorrection = &wrong
	f.expect(f.call(f.lead, "POST", "/v1/sites/"+f.site+"/feedback", uuid.NewString(), "", f.encode(in)), 404, "NOT_FOUND")
	if n := f.count(`SELECT count(*) FROM supervisor.feedback WHERE tenant_id=$1`, f.tenant); n != 0 {
		t.Fatal("failed scoped candidate left feedback")
	}
	claim.SourceSpanIDs = old.SourceSpanIDs
	claim.ClaimID = uuid.NewString()
	wrong.Claim = &claim
	f.expect(f.call(f.lead, "POST", "/v1/sites/"+f.site+"/feedback", uuid.NewString(), "", f.encode(in)), 404, "NOT_FOUND")
	// A valid corrected source remains usable when the original source becomes unavailable.
	var newSpan string
	if err := f.control.QueryRowContext(ctx, `SELECT s.id::text FROM supervisor.evidence_spans s JOIN supervisor.evidence_revisions r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=(s.tenant_id,s.site_id,s.source_id,s.revision) WHERE s.tenant_id=$1 AND s.site_id=$2 AND r.body='The press cycle time is 45 seconds.'`, f.tenant, f.site).Scan(&newSpan); err != nil {
		t.Fatal(err)
	}
	claim = old
	claim.Literal = &knowledge.Literal{Type: "decimal", Value: json.RawMessage(`"45"`), Unit: "s"}
	claim.SourceSpanIDs = []string{newSpan}
	wrong.Claim = &claim
	in.ProposedCorrection = &wrong
	got := submitCorrection(f, in)
	var oldSource string
	if err := f.control.QueryRowContext(ctx, `SELECT source_id::text FROM supervisor.evidence_spans WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, f.tenant, f.site, old.SourceSpanIDs[0]).Scan(&oldSource); err != nil {
		t.Fatal(err)
	}
	f.revokeSource(oldSource)
	ks := knowledge.New(f.runtime)
	ka := correctionActor(f, f.approver)
	out, err := ks.ReadMemoryCandidate(ctx, ka, sc, *got.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	if out.Original.Available || out.Original.Answer != nil || out.Original.Question != nil {
		t.Fatal("revoked original content disclosed")
	}
	if _, err = ks.DecideMemoryReview(ctx, ka, sc, *got.CandidateID, knowledge.MemoryReviewInput{Verdict: "approve", Reason: "New authoritative source corrects historical evidence", ExpectedRevision: 1}, uuid.NewString(), 1); err != nil {
		t.Fatalf("historical evidence incorrectly required current: %v", err)
	}
}

func TestWIS703StaleEntityAndWithdrawnAuthorityRollBackApproval(t *testing.T) {
	for _, change := range []string{"entity", "authority", "candidate", "expectation"} {
		t.Run(change, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			f := newStationFixture(t, ctx)
			in, sc, old, _, source := correctionKnowledge(f)
			saved := submitCorrection(f, in)
			ks := knowledge.New(f.runtime)
			owner := correctionActor(f, f.owner)
			switch change {
			case "entity":
				entity, err := ks.ReadEntity(ctx, owner, sc, old.SubjectID, 1)
				if err != nil {
					t.Fatal(err)
				}
				entity.Name = "Press revised"
				if _, err = ks.CreateEntity(ctx, owner, sc, entity.EntityInput, uuid.NewString(), 1); err != nil {
					t.Fatal(err)
				}
			case "authority":
				_, err := evidence.New(f.runtime).DecideAuthority(ctx, evidence.Actor{Issuer: owner.Issuer, Subject: owner.Subject, SessionHash: owner.SessionHash}, evidence.Scope{TenantID: f.tenant, SiteID: f.site}, source, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "withdraw", Reason: "Authority needs review"}, uuid.NewString(), 1)
				if err != nil {
					t.Fatal(err)
				}
			case "candidate":
				revised := *in.ProposedCorrection
				revised.Reason = "Author amended pending correction"
				if _, err := ks.ReviseMemoryCandidate(ctx, correctionActor(f, f.lead), sc, *saved.CandidateID, revised, uuid.NewString(), 1); err != nil {
					t.Fatal(err)
				}
			case "expectation":
				revised := *in.ProposedCorrection
				revised.ExpectedAnswer = strings.Repeat("x", 1001)
				if _, err := ks.ReviseMemoryCandidate(ctx, correctionActor(f, f.lead), sc, *saved.CandidateID, revised, uuid.NewString(), 1); err != nil {
					t.Fatal(err)
				}
			}
			inventory, err := ks.ReadInventory(ctx, owner, sc)
			if err != nil {
				t.Fatal(err)
			}
			revision := int64(1)
			if change == "expectation" {
				revision = 2
			}
			_, err = ks.DecideMemoryReview(ctx, correctionActor(f, f.approver), sc, *saved.CandidateID, knowledge.MemoryReviewInput{Verdict: "approve", Reason: "No stale approval", ExpectedRevision: revision}, uuid.NewString(), revision)
			if err == nil {
				t.Fatal("invalid pending correction approved")
			}
			current, err := ks.ReadClaim(ctx, owner, sc, old.ClaimID, 0)
			if err != nil || current.Revision != 1 {
				t.Fatal("failed approval advanced claim", err)
			}
			after, err := ks.ReadInventory(ctx, owner, sc)
			if err != nil || after.Revision != inventory.Revision {
				t.Fatal("failed approval changed inventory", err)
			}
			if n := f.count(`SELECT count(*) FROM supervisor.correction_evaluation_cases WHERE tenant_id=$1`, f.tenant); n != 0 {
				t.Fatal("failed approval created evaluation")
			}
		})
	}
}
