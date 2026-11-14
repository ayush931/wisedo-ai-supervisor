package contracts

import (
	"reflect"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/onboarding"
)

func interviewCatalog() []named {
	return []named{
		{"InterviewCreateInputV1", reflect.TypeOf(onboarding.CreateInterviewInput{})},
		{"InterviewPatchInputV1", reflect.TypeOf(onboarding.PatchInterviewInput{})},
		{"InterviewSessionV1", reflect.TypeOf(onboarding.InterviewSession{})},
		{"InterviewSessionPageV1", reflect.TypeOf(onboarding.InterviewSessionPage{})},
		{"QuestionV1", reflect.TypeOf(onboarding.Question{})},
		{"QuestionPageV1", reflect.TypeOf(onboarding.QuestionPage{})},
		{"InterviewAnswerInputV1", reflect.TypeOf(onboarding.AnswerInput{})},
		{"InterviewAnswerV1", reflect.TypeOf(onboarding.Answer{})},
		{"ConfigurationDraftRequestInputV1", reflect.TypeOf(onboarding.StartConfigurationDraftsInput{})},
		{"ConfigurationDraftRequestV1", reflect.TypeOf(onboarding.ConfigurationDraftRequest{})},
		{"ConfigurationDraftRunV1", reflect.TypeOf(onboarding.ConfigurationDraftRun{})},
		{"AgentTurnInputV1", reflect.TypeOf(onboarding.StartAgentTurnInput{})},
		{"AgentTurnV1", reflect.TypeOf(onboarding.AgentTurn{})},
		// WIS-625: a photographed page queued for OCR by the local vision model.
		{"SourceScanV1", reflect.TypeOf(evidence.ScanJob{})},
		{"SourceScanBatchV1", reflect.TypeOf(evidence.ScanBatch{})},
	}
}

func interviewEnums() map[reflect.Type][]string {
	return map[reflect.Type][]string{
		reflect.TypeOf(onboarding.InterviewRole("")):   {string(onboarding.RoleOwner), string(onboarding.RoleSupervisor), string(onboarding.RoleOperator)},
		reflect.TypeOf(onboarding.InterviewStatus("")): {string(onboarding.StatusActive), string(onboarding.StatusPaused), string(onboarding.StatusSubmitted), string(onboarding.StatusClosed)},
		reflect.TypeOf(onboarding.QuestionReason("")):  {string(onboarding.QuestionConflict), string(onboarding.QuestionMissing), string(onboarding.QuestionStale)},
		reflect.TypeOf(onboarding.AnswerState("")):     {string(onboarding.AnswerAnswered), string(onboarding.AnswerDismissed), string(onboarding.AnswerUnanswered)},
		reflect.TypeOf(onboarding.PromptOrigin("")):    {string(onboarding.PromptAgent), string(onboarding.PromptTemplate)},
		// Only extractable kinds reach a response; PDF is refused until a parser is qualified (WIS-613).
		reflect.TypeOf(onboarding.Kind("")): {string(onboarding.KindDOCX), string(onboarding.KindPDF), string(onboarding.KindText), string(onboarding.KindXLSX)},
	}
}

func addInterviewEnums(values map[reflect.Type][]string) {
	for enum, valuesForEnum := range interviewEnums() {
		values[enum] = valuesForEnum
	}
}

func restrictInterview(name string, schema map[string]any) {
	if name == "QuestionV1" {
		schema["properties"].(map[string]any)["source_span_ids"].(map[string]any)["maxItems"] = 10
	}
	if name == "AgentTurnInputV1" {
		props := schema["properties"].(map[string]any)
		props["source_span_ids"].(map[string]any)["maxItems"] = agent.MaxRunPassages
		props["source_span_ids"].(map[string]any)["uniqueItems"] = true
	}
	if name == "ConfigurationDraftRequestInputV1" {
		props := schema["properties"].(map[string]any)
		props["source_ids"].(map[string]any)["maxItems"] = 20
		props["source_ids"].(map[string]any)["uniqueItems"] = true
		props["kinds"].(map[string]any)["minItems"] = 1
		props["kinds"].(map[string]any)["maxItems"] = 4
		props["kinds"].(map[string]any)["uniqueItems"] = true
	}
}

func interviewPaths() map[string]any {
	id := map[string]any{"type": "string", "format": "uuid"}
	path := func(name string) map[string]any {
		return map[string]any{"name": name, "in": "path", "required": true, "schema": id}
	}
	header := func(name string, max int) map[string]any {
		return map[string]any{"name": name, "in": "header", "required": true, "schema": map[string]any{"type": "string", "minLength": 1, "maxLength": max}}
	}
	key, csrf := header("Idempotency-Key", 128), header("X-CSRF-Token", 512)
	ifMatch := header("If-Match", 32)
	ifMatch["schema"].(map[string]any)["pattern"] = `^"[1-9][0-9]*"$`
	limit := map[string]any{"name": "limit", "in": "query", "required": false, "schema": map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 50}}
	cursor := map[string]any{"name": "cursor", "in": "query", "required": false, "schema": id}
	session := []any{map[string]any{"session": []any{}}}
	ok := func(status, desc, name string) map[string]any {
		return map[string]any{status: json200(desc, name)}
	}
	problemSet := func(codes ...string) map[string]any {
		out := map[string]any{}
		for _, code := range codes {
			out[code] = map[string]any{"description": "Error", "content": map[string]any{"application/problem+json": map[string]any{"schema": ref("ProblemDetailsV1")}}}
		}
		return out
	}
	operation := func(id string, params []any, body string, response map[string]any, errors ...string) map[string]any {
		o := map[string]any{"operationId": id, "security": session, "parameters": params, "responses": merge(response, problemSet(errors...))}
		if body != "" {
			o["requestBody"] = jsonBody(body)
		}
		return o
	}
	site := path("site")
	return map[string]any{
		"/v1/sites/{site}/interviews": map[string]any{
			"get":  operation("listInterviews", []any{site, cursor, limit}, "", ok("200", "Participant-owned interview sessions", "InterviewSessionPageV1"), "401", "404", "422", "503"),
			"post": operation("createInterview", []any{site, key, csrf}, "InterviewCreateInputV1", ok("201", "Created or idempotently replayed", "InterviewSessionV1"), "401", "403", "404", "409", "413", "415", "422", "503"),
		},
		"/v1/sites/{site}/interviews/{id}": map[string]any{
			"get":   operation("getInterview", []any{site, path("id")}, "", ok("200", "Participant interview session", "InterviewSessionV1"), "401", "404", "422", "503"),
			"patch": operation("patchInterview", []any{site, path("id"), key, ifMatch, csrf}, "InterviewPatchInputV1", ok("200", "Status revision appended or replayed", "InterviewSessionV1"), "401", "403", "404", "409", "412", "413", "415", "422", "428", "503"),
		},
		"/v1/sites/{site}/interviews/{id}/questions": map[string]any{
			"get": operation("listInterviewQuestions", []any{site, path("id"), cursor, limit}, "", ok("200", "Role-specific question page", "QuestionPageV1"), "401", "403", "404", "409", "422", "503"),
		},
		"/v1/sites/{site}/interviews/{id}/answers": map[string]any{
			"post": operation("recordInterviewAnswer", []any{site, path("id"), key, csrf}, "InterviewAnswerInputV1", ok("201", "Answer evidence revision recorded or replayed", "InterviewAnswerV1"), "401", "403", "404", "409", "412", "413", "415", "422", "503"),
		},
		"/v1/sites/{site}/configuration-draft-requests": map[string]any{
			"post": operation("requestConfigurationDrafts", []any{site, key, csrf}, "ConfigurationDraftRequestInputV1", ok("202", "Agent configuration draft run queued", "ConfigurationDraftRequestV1"), "401", "403", "404", "409", "413", "415", "422", "503"),
		},
		"/v1/sites/{site}/configuration-draft-requests/{runId}": map[string]any{
			"get": operation("getConfigurationDraftRun", []any{site, path("runId")}, "", ok("200", "The requester's configuration draft run: state, outcome reason codes and created draft IDs", "ConfigurationDraftRunV1"), "401", "403", "404", "422", "500", "503"),
		},
		"/v1/sites/{site}/interviews/{id}/agent-turns": map[string]any{
			"post": operation("startInterviewAgentTurn", []any{site, path("id"), key, ifMatch, csrf}, "AgentTurnInputV1", ok("202", "Agent follow-up question turn queued or replayed", "AgentTurnV1"), "401", "403", "404", "409", "412", "413", "415", "422", "428", "503"),
		},
		// WIS-625 slice B1: the body is the raw PNG or JPEG; the title is X-Source-Title (as for
		// /source-documents). Nothing is extracted in the request.
		"/v1/sites/{site}/source-scans": map[string]any{
			"post": func() map[string]any {
				title := map[string]any{"name": "X-Source-Title", "in": "header", "required": true, "description": "The source title, UTF-8 percent-encoded (1-200 characters once decoded).", "schema": map[string]any{"type": "string", "minLength": 1}}
				o := operation("queueSourceScan", []any{site, title, key, csrf}, "", ok("202", "Page image verified from its bytes (PNG or JPEG, at most 4 MiB and 40 megapixels) and queued for OCR by the local vision model; replayed for the same key and file. A different file under the same key is 409 IDEMPOTENCY_CONFLICT. The image is kept only while the job runs. 503 EXTRACTION_UNAVAILABLE when no vision model is configured. 503 SCAN_QUEUE_FULL (with Retry-After) when the page would take the tenant past its cap on running scan pages (default 40, across all its sites); nothing is stored, and a replay of a stored key is still answered.", "SourceScanV1"),
					"401", "403", "404", "409", "413", "415", "422", "428", "503")
				o["requestBody"] = map[string]any{"required": true, "content": map[string]any{
					"image/png":  map[string]any{"schema": map[string]any{"type": "string", "format": "binary", "maxLength": 4 << 20}},
					"image/jpeg": map[string]any{"schema": map[string]any{"type": "string", "format": "binary", "maxLength": 4 << 20}}}}
				return o
			}(),
		},
		"/v1/sites/{site}/source-scan-batches": map[string]any{
			"post": func() map[string]any {
				title := map[string]any{"name": "X-Source-Title", "in": "header", "required": true, "description": "The document title, UTF-8 percent-encoded; each page is titled \"<title> (page i of n)\".", "schema": map[string]any{"type": "string", "minLength": 1}}
				o := operation("queueSourceScanBatch", []any{site, title, key, csrf}, "", ok("202", "A scanned PDF (every page one JPEG, lossless or fax-encoded (CCITT Group 4 or one-dimensional Group 3, BlackIs1 false) image, no text layer, at most 20 pages; a fax page is decoded in the child to a PNG) screened like any PDF in a resource-capped child; each page's image is checked like a photo and queued for OCR, all pages in one transaction (page i keyed by a digest of the Idempotency-Key and i, so any valid key replays). A PDF with typed and scanned pages (WIS-645) also stores its typed pages as one typed document (never model-extracted) and links it with the page jobs as one upload (upload_id, document), all in one transaction; the same key replays it and another file under the key is 409. 422 PDF_SCAN_HAS_TEXT when no page is scanned (upload it to /source-documents), 415 PDF_SCAN_IMAGE_UNSUPPORTED for a JBIG2 or JPEG 2000 page, a fax page with other parameters (two-dimensional Group 3, BlackIs1, EndOfLine, columns or rows that are not its size) or a broken stream, a page with no or several images, 422 PDF_SCAN_HIDDEN_CONTENT for a page whose image is not shown whole and alone (a crop box, clip, mask, palette, annotation, drawing over it, placement off the page or on less than half of it, a page under one inch on a side, a page colour space, graphics state or group, or an image that is not a plain 8-bit grey, RGB or CMYK or 1-bit grey fax picture matching its dictionary: OCR reads the embedded image, so it could read what the page hides), 413 PDF_SCAN_TOO_MANY_PAGES, LIMIT_EXCEEDED for a page over 4 MiB or 40 megapixels (its declared size is checked before anything decodes it); other PDF refusals as for /source-documents. 503 EXTRACTION_UNAVAILABLE without a vision model. 503 SCAN_QUEUE_FULL (with Retry-After) when the pages would take the tenant past its cap on running scan pages (default 40, across all its sites): the batch is refused whole, nothing is stored, and a replay of a stored key is still answered.", "SourceScanBatchV1"),
					"401", "403", "404", "409", "413", "415", "422", "428", "503")
				o["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/pdf": map[string]any{"schema": map[string]any{"type": "string", "format": "binary", "maxLength": 20 << 20}}}}
				return o
			}(),
		},
		"/v1/sites/{site}/source-scans/{id}": map[string]any{
			"get": operation("getSourceScan", []any{site, path("id")}, "", ok("200", "Scan state: running, completed with its model-extracted document source, or quarantined with a stable reason (PAGE_ILLEGIBLE, PAGE_TRUNCATED, OUTPUT_INVALID, TOOL_NOT_ALLOWED, EXTRACTION_FAILED, UPLOADER_NOT_AUTHORIZED, TEXT_TOO_LARGE, BUDGET_EXHAUSTED, RUN_CANCELED); no text is stored for a quarantined page", "SourceScanV1"),
				"401", "403", "404", "422", "503"),
		},
		"/v1/sites/{site}/interviews/{id}/agent-turns/{runId}": map[string]any{
			"get": operation("getInterviewAgentTurn", []any{site, path("id"), path("runId")}, "", ok("200", "Agent question turn state, outcome reason codes and created question IDs", "AgentTurnV1"), "401", "403", "404", "422", "500", "503"),
		},
	}
}

func addInterviews(doc map[string]any) {
	merge(doc["paths"].(map[string]any), interviewPaths())
	info := doc["info"].(map[string]any)
	info["description"] = info["description"].(string) + " WIS-271 interview sessions persist participant-owned role interviews, deterministic discovery questions and append-only interview answer evidence."
}
