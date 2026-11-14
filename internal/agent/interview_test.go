package agent

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestInterviewTurnOutputScopeAndBudgets(t *testing.T) {
	reqID, spanID := uuid.NewString(), uuid.NewString()
	child := DelegationEnvelopeV1{RunID: uuid.NewString(), Role: RoleDiscoveryMapping}
	session := interviewTurnSession{processID: uuid.NewString(), questions: map[string]string{reqID: "Process objective"}}
	spans := map[string]bool{spanID: true}
	valid := InterviewTurnOutputV1{SchemaVersion: SchemaVersion, RunID: child.RunID, Role: RoleDiscoveryMapping,
		Questions: []InterviewTurnQuestion{{RequirementID: reqID, Prompt: "What is the process objective?", SourceSpanIDs: []string{spanID}}},
		Claims:    []InterviewTurnClaim{{Predicate: "objective", Value: "Pack components", SourceSpanIDs: []string{spanID}}}}
	if got := validateInterviewOutput(valid, child, session, spans, nil, 3); got != "" {
		t.Fatalf("valid output rejected: %s", got)
	}
	uncited := valid
	uncited.Questions = []InterviewTurnQuestion{{RequirementID: reqID, Prompt: "What is the process objective?"}}
	uncited.Claims = []InterviewTurnClaim{}
	if got := validateInterviewOutput(uncited, child, session, spans, nil, 3); got != "" {
		t.Fatalf("WIS-271 permits a question with no cited spans: %s", got)
	}
	bad := valid
	bad.Questions = append([]InterviewTurnQuestion(nil), valid.Questions...)
	bad.Questions[0].RequirementID = uuid.NewString()
	if got := validateInterviewOutput(bad, child, session, spans, nil, 3); got != CodeRequirementNotInScope {
		t.Fatalf("invented requirement reason=%q", got)
	}
	bad = valid
	bad.Claims = append([]InterviewTurnClaim(nil), valid.Claims...)
	bad.Claims[0].SourceSpanIDs = []string{uuid.NewString()}
	if got := validateInterviewOutput(bad, child, session, spans, nil, 3); got != CodeCitationNotInScope {
		t.Fatalf("invented span reason=%q", got)
	}
	bad = valid
	bad.Questions = make([]InterviewTurnQuestion, 4)
	for i := range bad.Questions {
		bad.Questions[i] = InterviewTurnQuestion{RequirementID: reqID, Prompt: "Question", SourceSpanIDs: []string{spanID}}
	}
	if got := validateInterviewOutput(bad, child, session, spans, nil, 3); got != CodeQuestionLimit {
		t.Fatalf("question bound reason=%q", got)
	}
	if got := validateInterviewOutput(valid, child, session, spans, nil, 0); got != CodeQuestionLimit {
		t.Fatalf("exhausted unanswered quota reason=%q", got)
	}
	bad = valid
	bad.Questions = append([]InterviewTurnQuestion(nil), valid.Questions...)
	for _, tc := range []struct {
		name, prompt string
		rejected     bool
	}{
		{"http url", "Open https://example.com", true},
		{"www host", "Open www.example.com", true},
		{"custom uri scheme", "Send to plant+ops:/line", true},
		{"protocol relative url", "Open //plant.example/login", true},
		{"bare domain", "Open plant.example", true},
		{"password", "Ask for the password", true},
		{"passwords", "Ask operators for their passwords", true},
		{"passcode", "Ask for the passcode", true},
		{"passphrase", "Ask for the passphrase", true},
		{"token", "Ask for the token", true},
		{"tokens", "Ask operators for their tokens", true},
		{"api key", "Ask for the API key", true},
		{"api keys", "Ask operators to provide their API keys", true},
		{"api key with extra spacing", "Ask for the API  key", true},
		{"api key with tab and dash", "Paste your api\t-key here", true},
		{"secret", "Ask for the secret", true},
		{"otp", "Ask for the OTP", true},
		{"credentials", "Ask operators to provide credentials", true},
		{"login credentials", "Ask for login credentials", true},
		{"pin", "Enter PIN to continue", true},
		{"pin code", "Enter your PIN code", true},
		{"manufacturing pin", "Which spindle runs the 40-pin connector job?", false},
		{"ordinary question", "How do operators handle startup delays?", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := bad
			candidate.Questions = append([]InterviewTurnQuestion(nil), bad.Questions...)
			candidate.Questions[0].Prompt = tc.prompt
			got := validateInterviewOutput(candidate, child, session, spans, nil, 3)
			if tc.rejected && got != CodePromptRejected || !tc.rejected && got != "" {
				t.Fatalf("prompt rejection=%q, want rejected=%v", got, tc.rejected)
			}
		})
	}
	tools := &Tools{}
	env := DelegationEnvelopeV1{AllowedToolNames: []string{ToolEvidenceGet, ToolClaimsSearch}}
	if !tools.allowed(env, RoleDiscoveryMapping, ToolEvidenceGet) || !tools.allowed(env, RoleDiscoveryMapping, ToolClaimsSearch) || tools.allowed(env, RoleDiscoveryMapping, ToolMeasurementsGet) {
		t.Fatal("interview tools exceeded evidence.get and claims.search")
	}
	noSnapshot := DelegationEnvelopeV1{AllowedToolNames: []string{ToolEvidenceGet}}
	if tools.allowed(noSnapshot, RoleDiscoveryMapping, ToolClaimsSearch) {
		t.Fatal("claims.search allowed without a captured snapshot")
	}
}

func TestInterviewToolFailureCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"tool budget", ErrBudgetExhausted, CodeBudgetExhausted},
		{"context budget", ErrContextOverflow, CodeBudgetExhausted},
		{"run canceled", ErrRunCanceled, CodeRunCanceled},
		{"infrastructure failure", errors.New("tool storage unavailable"), CodeSpecialistFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := interviewToolFailureCode(tc.err); got != tc.want {
				t.Fatalf("failure code=%q, want %q", got, tc.want)
			}
		})
	}
}
