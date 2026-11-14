package contracts

import (
	"reflect"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/feedback"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
)

func feedbackCatalog() []named {
	return []named{
		{"MemoryCandidateV1", reflect.TypeOf(knowledge.MemoryCandidate{})},
		{"MemoryCandidatePageV1", reflect.TypeOf(knowledge.MemoryCandidatePage{})},
		{"MemoryCandidateRevisionInputV1", reflect.TypeOf(knowledge.MemoryRevisionInput{})},
		{"MemoryCandidateReviewInputV1", reflect.TypeOf(knowledge.MemoryReviewInput{})},
		{"CorrectionSummaryV1", reflect.TypeOf(knowledge.CorrectionSummary{})},
		{"CorrectionEvaluationCaseV1", reflect.TypeOf(knowledge.CorrectionEvaluationCase{})},
		{"CorrectionEvaluationPageV1", reflect.TypeOf(knowledge.CorrectionEvaluationPage{})},
		{"OutcomeFeedbackInputV1", reflect.TypeOf(feedback.FeedbackInputV1{})},
		{"OutcomeFeedbackV1", reflect.TypeOf(feedback.FeedbackV1{})},
		{"OutcomeFeedbackPageV1", reflect.TypeOf(feedback.FeedbackPageV1{})},
	}
}

func addFeedbackEnums(values map[reflect.Type][]string) {
	values[reflect.TypeOf(feedback.SubjectKind(""))] = []string{string(feedback.SubjectAnswer), string(feedback.SubjectInvestigation), string(feedback.SubjectCase), string(feedback.SubjectClaim), string(feedback.SubjectMapping), string(feedback.SubjectTask)}
	values[reflect.TypeOf(feedback.Kind(""))] = []string{string(feedback.KindCorrection), string(feedback.KindObservation), string(feedback.KindUsefulness)}
	values[reflect.TypeOf(feedback.Verdict(""))] = []string{string(feedback.VerdictHelpful), string(feedback.VerdictNotApplicable), string(feedback.VerdictUncertain), string(feedback.VerdictUnhelpful)}
}

func restrictFeedback(name string, s map[string]any) {
	if name == "OutcomeFeedbackInputV1" {
		s["properties"].(map[string]any)["evidence_ids"].(map[string]any)["maxItems"] = feedback.MaxEvidence
	}
}

// feedbackPaths documents the WIS-279 feedback routes. Feedback is append-only: there is no
// update or delete route, so a submission takes Idempotency-Key (a UUID) but no If-Match.
func feedbackPaths() map[string]any {
	param := func(name, in string, required bool, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "in": in, "required": required, "schema": schema}
	}
	id := map[string]any{"type": "string", "format": "uuid"}
	site := param("site", "path", true, id)
	key := param("Idempotency-Key", "header", true, id)
	csrf := param("X-CSRF-Token", "header", true, map[string]any{"type": "string", "minLength": 1})
	session := []any{map[string]any{"session": []any{}}}
	reads := problemSet(map[string][]string{
		"401": {"UNAUTHENTICATED"}, "404": {"NOT_FOUND"}, "422": {"INVALID_REQUEST"}, "503": {"STORAGE_UNAVAILABLE"},
	})
	writes := problemSet(map[string][]string{
		"401": {"UNAUTHENTICATED"},
		"403": {"CSRF_REJECTED", "PERMISSION_DENIED"},
		"404": {"NOT_FOUND"},
		"409": {"IDEMPOTENCY_CONFLICT", "FEEDBACK_SUBJECT_NOT_READY", "FEEDBACK_ALREADY_SUPERSEDED"},
		"413": {"BODY_TOO_LARGE"},
		"415": {"UNSUPPORTED_MEDIA"},
		"422": {"INVALID_REQUEST", "INVALID_FEEDBACK", "FEEDBACK_EVIDENCE_UNAVAILABLE"},
		"503": {"STORAGE_UNAVAILABLE"},
	})
	op := func(opID string, params []any, body string, responses map[string]any) map[string]any {
		o := map[string]any{"operationId": opID, "security": session, "parameters": params, "responses": responses}
		if body != "" {
			o["requestBody"] = jsonBody(body)
		}
		return o
	}
	return map[string]any{
		"/v1/sites/{site}/feedback": map[string]any{
			"post": op("submitFeedback", []any{site, key, csrf}, "OutcomeFeedbackInputV1",
				merge(map[string]any{
					"201": json200("Feedback recorded. Site owners and supervisors give feedback on every subject; an operator only on a task assigned to them or its case (claims and mappings are 403 PERMISSION_DENIED); an answer is rated only by its own conversation author once finished. An answer or investigation correction atomically creates a pending process-claim candidate; its corrected value and source are optional until review. Investigation feedback binds an exact terminal run. Other corrections challenge a claim/mapping or source revision. supersedes_feedback_id replaces the author's own earlier feedback on the same subject (422 otherwise); a record is superseded at most once (409 FEEDBACK_ALREADY_SUPERSEDED)", "OutcomeFeedbackV1"),
					"200": json200("The same key and body replayed: the original record with replayed=true", "OutcomeFeedbackV1"),
				}, writes)),
			"get": op("listFeedback", []any{site,
				param("case_id", "query", false, id),
				param("subject_kind", "query", false, map[string]any{"type": "string", "enum": []any{"answer", "investigation", "case", "claim", "mapping", "task"}}),
				param("limit", "query", false, map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 50}),
				param("cursor", "query", false, map[string]any{"type": "string", "maxLength": 128})}, "",
				merge(map[string]any{"200": json200("Visible feedback, newest first: the reader's own, everything but answer feedback for an owner or supervisor, and feedback on a task assigned to the reader or on a case where the reader holds a task", "OutcomeFeedbackPageV1")}, reads)),
		},
		"/v1/sites/{site}/feedback/{feedback}": map[string]any{
			"get": op("getFeedback", []any{site, param("feedback", "path", true, id)}, "",
				merge(map[string]any{"200": json200("One visible feedback record; cited evidence that has since been revoked is listed with available=false", "OutcomeFeedbackV1")}, reads)),
		},
	}
}

func addFeedback(doc map[string]any) {
	merge(doc["paths"].(map[string]any), feedbackPaths())
	merge(doc["paths"].(map[string]any), correctionPaths())
	info := doc["info"].(map[string]any)
	info["description"] = info["description"].(string) + " WIS-279 outcome feedback is append-only and site-scoped; the server binds the subject, case, context and evidence revisions, the reason is stored as data and never an instruction, and a correction overwrites nothing."
}
