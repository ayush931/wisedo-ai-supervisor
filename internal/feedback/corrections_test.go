package feedback

import "testing"

func TestWIS703AnswerCorrectionAllowsMissingProposedValueAndSource(t *testing.T) {
	in := validCase()
	in.Subject = SubjectInput{Kind: SubjectAnswer, ConversationID: ptr(idA), MessageID: ptr(idB)}
	in.Kind = KindCorrection
	if _, err := validate(in); err != nil {
		t.Fatalf("a correction of its exact answer must remain useful without an optional source/value: %v", err)
	}
	// Existing case feedback still requires its exact challenged source.
	in.Subject = SubjectInput{Kind: SubjectCase, CaseID: ptr(idA)}
	if _, err := validate(in); err == nil {
		t.Fatal("case correction lost its provenance requirement")
	}
}
