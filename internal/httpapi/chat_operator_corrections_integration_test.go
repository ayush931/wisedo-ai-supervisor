//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/chat"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/feedback"
	"github.com/google/uuid"
)

func TestWIS703OperatorChatOwnCorrectionsAndEvidenceAuthority(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	base := "/v1/sites/" + f.site + "/chat-conversations"
	f.expect(f.call(f.operator, "POST", base, uuid.NewString(), "", `{"title":"Operator questions"}`), 201, "")
	in, _, old, snapshot, _ := correctionKnowledge(f)
	source := ""
	for _, s := range snapshot.Contents.Spans {
		if s.SpanID == old.SourceSpanIDs[0] {
			source = s.SourceID
		}
	}
	answer, conversation := correctionChat(f, f.operator, old.ClaimID, 1, source, old.SourceSpanIDs[0], "60", true)
	in.Subject = feedback.SubjectInput{Kind: feedback.SubjectAnswer, ConversationID: &conversation, MessageID: &answer.MessageID}
	in.ProposedCorrection = nil
	w := f.call(f.operator, "POST", "/v1/sites/"+f.site+"/feedback", uuid.NewString(), "", f.encode(in))
	f.expect(w, 201, "")
	var got feedback.FeedbackV1
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.CandidateID == nil {
		t.Fatalf("operator correction: %s %v", w.Body, err)
	}
	f.expect(f.call(f.operator, "POST", "/v1/sites/"+f.site+"/memory-candidates/"+*got.CandidateID+"/review", uuid.NewString(), `"1"`, `{"verdict":"approve","reason":"Cannot review","expected_revision":1}`), 403, "PERMISSION_DENIED")
	f.expect(f.call(f.approver, "GET", base+"/"+conversation+"/messages", "", "", ""), 404, "NOT_FOUND")
	// The foreign member is an operator too; its role never grants another site's chat.
	f.exec(`UPDATE supervisor.memberships SET role='operator' WHERE tenant_id=$1 AND site_id=$2`, f.otherTenant, f.otherSite)
	f.expect(f.call(f.outsider, "GET", base+"/"+conversation+"/messages", "", "", ""), 404, "NOT_FOUND")
	before := f.count(`SELECT count(*) FROM supervisor.evidence_audit WHERE tenant_id=$1 AND site_id=$2 AND actor_subject=$3 AND source_id=$4 AND event_type='source.read'`, f.tenant, f.site, f.operator.subject, source)
	f.expect(f.call(f.operator, "GET", base+"/"+conversation+"/messages", "", "", ""), 200, "")
	after := f.count(`SELECT count(*) FROM supervisor.evidence_audit WHERE tenant_id=$1 AND site_id=$2 AND actor_subject=$3 AND source_id=$4 AND event_type='source.read'`, f.tenant, f.site, f.operator.subject, source)
	if after <= before {
		t.Fatal("stored citation was not reread under operator evidence authority")
	}
	f.revokeSource(source)
	w = f.call(f.operator, "GET", base+"/"+conversation+"/messages", "", "", "")
	f.expect(w, 200, "")
	var page chat.MessagePage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	last := page.Messages[len(page.Messages)-1]
	if last.Body != nil || len(last.Citations) != 0 || len(last.ClaimCitations) != 0 || last.ReasonCode == nil || *last.ReasonCode != "EVIDENCE_REVOKED" {
		t.Fatalf("operator saw revoked source: %+v", last)
	}
	f.expect(f.call(f.viewer, "POST", base, uuid.NewString(), "", `{"title":"Viewer cannot chat"}`), 403, "PERMISSION_DENIED")
}

// WIS-703 review finding 7: an operator of a second site in the same tenant gets the
// non-disclosing 404 for the first site's conversation, correction candidate and feedback, even
// when that operator authored them at the first site. Tenant-only scoping would leak all three.
func TestWIS703SameTenantSecondSiteOperatorGets404(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	in, _, old, snapshot, _ := correctionKnowledge(f)
	source := ""
	for _, s := range snapshot.Contents.Spans {
		if s.SpanID == old.SourceSpanIDs[0] {
			source = s.SourceID
		}
	}
	answer, conversation := correctionChat(f, f.operator, old.ClaimID, 1, source, old.SourceSpanIDs[0], "60", true)
	in.Subject = feedback.SubjectInput{Kind: feedback.SubjectAnswer, ConversationID: &conversation, MessageID: &answer.MessageID}
	in.ProposedCorrection = nil
	w := f.call(f.operator, "POST", "/v1/sites/"+f.site+"/feedback", uuid.NewString(), "", f.encode(in))
	f.expect(w, 201, "")
	var got feedback.FeedbackV1
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.CandidateID == nil {
		t.Fatalf("operator correction: %s %v", w.Body, err)
	}
	second := uuid.NewString()
	f.exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'same tenant second site')`, f.tenant, second)
	f.exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'operator')`, f.tenant, second, f.issuer, f.operator.subject)
	other := "/v1/sites/" + second
	// The second-site membership is live, so the 404s below come from site scoping, not non-membership.
	f.expect(f.call(f.operator, "POST", other+"/chat-conversations", uuid.NewString(), "", `{"title":"Second site questions"}`), 201, "")
	f.expect(f.call(f.operator, "GET", other+"/chat-conversations/"+conversation+"/messages", "", "", ""), 404, "NOT_FOUND")
	f.expect(f.call(f.operator, "GET", other+"/memory-candidates/"+*got.CandidateID, "", "", ""), 404, "NOT_FOUND")
	in.Reason = "Feedback through the wrong site"
	f.expect(f.call(f.operator, "POST", other+"/feedback", uuid.NewString(), "", f.encode(in)), 404, "NOT_FOUND")
	// Positive control: the same operator still reads both at the site that owns them.
	f.expect(f.call(f.operator, "GET", "/v1/sites/"+f.site+"/chat-conversations/"+conversation+"/messages", "", "", ""), 200, "")
	f.expect(f.call(f.operator, "GET", "/v1/sites/"+f.site+"/memory-candidates/"+*got.CandidateID, "", "", ""), 200, "")
}
