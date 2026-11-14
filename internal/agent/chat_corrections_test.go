package agent

import (
	"encoding/json"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"testing"
)

func TestWIS703ChatRejectsOldGenerationContract(t *testing.T) {
	child := DelegationEnvelopeV1{RunID: "00000000-0000-4000-8000-000000000001", Role: RoleDiscoveryMapping}
	out := ChatAnswerOutputV2{SchemaVersion: 1, RunID: child.RunID, Role: child.Role, Status: "needs_input", Statements: []ChatStatement{}, MissingInputs: []string{"Approved process"}}
	if code := validateChatOutput(out, child, nil, nil, nil); code != CodeAnswerInvalid {
		t.Fatalf("old generation contract accepted: %q", code)
	}
}

func TestWIS703ExactClaimCitationsDoNotInferFromSharedSpans(t *testing.T) {
	const span = "00000000-0000-4000-8000-000000000002"
	const claim = "00000000-0000-4000-8000-000000000003"
	c := decimalClaim(claim, "cycle_time_seconds", "45", "s", span)
	c.Revision = 2
	other := c
	other.ClaimID = "00000000-0000-4000-8000-000000000004"
	for _, tc := range []struct {
		name  string
		refs  []ChatClaimRef
		spans []string
		kind  string
		want  string
		count int
	}{
		{"exact", []ChatClaimRef{{claim, 2}}, []string{span}, "approved_fact", "", 1},
		{"span only", nil, []string{span}, "reported_observation", "", 0},
		{"old revision", []ChatClaimRef{{claim, 1}}, []string{span}, "approved_fact", CodeFactNotApproved, 0},
		{"forged", []ChatClaimRef{{"00000000-0000-4000-8000-000000000005", 2}}, []string{span}, "approved_fact", CodeFactNotApproved, 0},
		{"missing span", []ChatClaimRef{{claim, 2}}, nil, "approved_fact", CodeFactNotApproved, 0},
		{"duplicate", []ChatClaimRef{{claim, 2}, {claim, 2}}, []string{span}, "approved_fact", CodeFactNotApproved, 0},
		{"hypothesis", []ChatClaimRef{{claim, 2}}, []string{span}, "hypothesis", CodeFactNotApproved, 0},
		{"different fact sharing span", []ChatClaimRef{{other.ClaimID, 2}}, []string{span}, "approved_fact", "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, code := resolveChatClaimCitations(ChatAnswerOutputV2{Status: "answered", Statements: []ChatStatement{{Kind: tc.kind, Text: "45 seconds.", SourceSpanIDs: tc.spans, ClaimRefs: tc.refs}}}, []knowledge.SnapshotClaim{c, other})
			if code != tc.want || len(got) != tc.count {
				t.Fatalf("citations %+v, code %q", got, code)
			}
			if len(got) > 0 && (got[0].ChatClaimRef != tc.refs[0] || got[0].SourceSpanIDs[0] != span || got[0].Statement != 0) {
				t.Fatalf("wrong fact substituted: %+v", got)
			}
		})
	}
}

func TestWIS703LegacyArtifactsStayReadableWithoutInventedClaims(t *testing.T) {
	old := []byte(`{"schema_version":1,"reply_message_id":"00000000-0000-4000-8000-000000000001","status":"answered"}`)
	if _, err := artifactSchemas["chat_answer_v1"](old); err != nil {
		t.Fatal(err)
	}
	if _, err := artifactSchemas[ChatArtifactSchema](old); err == nil {
		t.Fatal("old artifact accepted as new evidence")
	}
	ref := ChatClaimRef{"00000000-0000-4000-8000-000000000002", 2}
	span := "00000000-0000-4000-8000-000000000003"
	v := ChatAnswerArtifactV2{SchemaVersion: ChatSchemaVersion, ReplyID: "00000000-0000-4000-8000-000000000001", Status: "answered", Statements: []ChatStatement{{Kind: "approved_fact", Text: "45 seconds.", SourceSpanIDs: []string{span}, ClaimRefs: []ChatClaimRef{ref}}}, ClaimCitations: []ChatClaimCitation{{ref, 0, []string{span}}}}
	b, _ := json.Marshal(v)
	if _, err := artifactSchemas[ChatArtifactSchema](b); err != nil {
		t.Fatal(err)
	}
	v.ClaimCitations[0].Revision = 1
	b, _ = json.Marshal(v)
	if _, err := artifactSchemas[ChatArtifactSchema](b); err == nil {
		t.Fatal("artifact mismatched exact citation accepted")
	}
}
