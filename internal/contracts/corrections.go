package contracts

func correctionPaths() map[string]any {
	param := func(name, in string, required bool, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "in": in, "required": required, "schema": schema}
	}
	id := map[string]any{"type": "string", "format": "uuid"}
	site := param("site", "path", true, id)
	candidate := param("candidate", "path", true, id)
	key := param("Idempotency-Key", "header", true, id)
	match := param("If-Match", "header", true, map[string]any{"type": "string", "pattern": `^"[1-9][0-9]*"$`})
	csrf := param("X-CSRF-Token", "header", true, map[string]any{"type": "string", "minLength": 1})
	limit := param("limit", "query", false, map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 50})
	cursor := param("cursor", "query", false, map[string]any{"type": "string", "maxLength": 128})
	codes := map[string][]string{"401": {"UNAUTHENTICATED"}, "403": {"PERMISSION_DENIED"}, "404": {"NOT_FOUND"}, "422": {"INVALID_REQUEST", "INVALID_CLAIM"}, "503": {"STORAGE_UNAVAILABLE", "IDENTITY_UNAVAILABLE"}}
	op := func(name, body, result string, params []any, write bool) map[string]any {
		errors := map[string][]string{}
		for k, v := range codes {
			errors[k] = append([]string{}, v...)
		}
		if write {
			errors["403"] = append(errors["403"], "CSRF_REJECTED", "SELF_REVIEW_FORBIDDEN")
			errors["409"] = []string{"IDEMPOTENCY_CONFLICT", "CORRECTION_SUPERSEDED", "REVIEW_CONFLICT", "REVIEW_BLOCKED", "SOURCE_CHANGED", "KNOWLEDGE_CYCLE"}
			errors["412"] = []string{"REVISION_CONFLICT"}
			errors["413"] = []string{"BODY_TOO_LARGE", "LIMIT_EXCEEDED"}
			errors["415"] = []string{"UNSUPPORTED_MEDIA"}
			errors["422"] = append(errors["422"], "INVALID_REVISION", "INVALID_SOURCE", "CORRECTION_INCOMPLETE")
			errors["428"] = []string{"REVISION_REQUIRED", "REAUTHENTICATION_REQUIRED"}
		}
		out := map[string]any{"operationId": name, "security": []any{map[string]any{"session": []any{}}}, "parameters": params, "responses": merge(map[string]any{"200": json200("Scoped reviewed correction ledger. Approval awaits separate human snapshot publication.", result)}, problemSet(errors))}
		if body != "" {
			out["requestBody"] = jsonBody(body)
		}
		return out
	}
	return map[string]any{
		"/v1/sites/{site}/memory-candidates":                       map[string]any{"get": op("listMemoryCandidates", "", "MemoryCandidatePageV1", []any{site, limit, cursor, param("state", "query", false, map[string]any{"type": "string", "enum": []any{"candidate", "in_review", "approved", "rejected", "superseded"}})}, false)},
		"/v1/sites/{site}/memory-candidates/{candidate}":           map[string]any{"get": op("getMemoryCandidate", "", "MemoryCandidateV1", []any{site, candidate}, false)},
		"/v1/sites/{site}/memory-candidates/{candidate}/revisions": map[string]any{"post": op("reviseMemoryCandidate", "MemoryCandidateRevisionInputV1", "MemoryCandidateV1", []any{site, candidate, key, match, csrf}, true)},
		"/v1/sites/{site}/memory-candidates/{candidate}/review":    map[string]any{"post": op("reviewMemoryCandidate", "MemoryCandidateReviewInputV1", "MemoryCandidateV1", []any{site, candidate, key, match, csrf}, true)},
		"/v1/sites/{site}/correction-summary":                      map[string]any{"get": op("getCorrectionSummary", "", "CorrectionSummaryV1", []any{site}, false)},
		"/v1/sites/{site}/correction-evaluation-cases":             map[string]any{"get": op("listCorrectionEvaluationCases", "", "CorrectionEvaluationPageV1", []any{site, limit, cursor}, false)},
	}
}
