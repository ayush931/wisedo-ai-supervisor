package agent

import "testing"

func TestDraftToolErrorMessagePreservesSpecificCode(t *testing.T) {
	message := draftToolErrorMessage("evidence-call", CodeEvidenceUnavailable)
	if message.Role != "tool" || message.ToolCallID != "evidence-call" || message.Content != `{"error":"EVIDENCE_UNAVAILABLE"}` {
		t.Fatalf("tool error message = %+v", message)
	}
}
