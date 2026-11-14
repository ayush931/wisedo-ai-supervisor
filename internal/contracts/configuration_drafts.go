package contracts

import (
	"reflect"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
)

func configurationDraftCatalog() []named {
	return []named{
		{"ConfigurationDraftFieldSpanInputV1", reflect.TypeOf(workstation.ConfigurationDraftFieldSpanInputV1{})},
		{"ConfigurationDraftCreateInputV1", reflect.TypeOf(workstation.ConfigurationDraftCreateInputV1{})},
		{"ConfigurationDraftReviseInputV1", reflect.TypeOf(workstation.ConfigurationDraftReviseInputV1{})},
		{"ConfigurationDraftEmptyInputV1", reflect.TypeOf(workstation.ConfigurationDraftEmptyInputV1{})},
		{"ConfigurationDraftCitationV1", reflect.TypeOf(workstation.ConfigurationDraftCitationV1{})},
		{"ConfigurationDraftDroppedCitationV1", reflect.TypeOf(workstation.ConfigurationDraftDroppedCitationV1{})},
		{"ConfigurationDraftRevisionV1", reflect.TypeOf(workstation.ConfigurationDraftRevisionV1{})},
		{"ConfigurationDraftPromotionV1", reflect.TypeOf(workstation.ConfigurationDraftPromotionV1{})},
		{"ConfigurationDraftV1", reflect.TypeOf(workstation.ConfigurationDraftV1{})},
		{"ConfigurationDraftSummaryV1", reflect.TypeOf(workstation.ConfigurationDraftSummaryV1{})},
		{"ConfigurationDraftPageV1", reflect.TypeOf(workstation.ConfigurationDraftPageV1{})},
	}
}

func addConfigurationDraftEnums(values map[reflect.Type][]string) {
	kinds := []string{}
	for _, k := range workstation.DraftKinds() {
		kinds = append(kinds, string(k))
	}
	statuses := []string{}
	for _, s := range workstation.DraftStatuses() {
		statuses = append(statuses, string(s))
	}
	values[reflect.TypeOf(workstation.DraftKind(""))] = kinds
	values[reflect.TypeOf(workstation.DraftStatus(""))] = statuses
	authors := []string{}
	for _, k := range workstation.DraftAuthorKinds() {
		authors = append(authors, string(k))
	}
	values[reflect.TypeOf(workstation.DraftAuthorKind(""))] = authors
	reasons := []string{}
	for _, r := range workstation.CitationReasons() {
		reasons = append(reasons, string(r))
	}
	values[reflect.TypeOf(workstation.CitationReason(""))] = reasons
}

func restrictConfigurationDraft(name string, s map[string]any) {
	if name == "ConfigurationDraftCreateInputV1" || name == "ConfigurationDraftReviseInputV1" {
		s["properties"].(map[string]any)["field_spans"].(map[string]any)["maxItems"] = workstation.MaxDraftFieldSpans
	}
}

func addConfigurationDrafts(doc map[string]any) {
	merge(doc["paths"].(map[string]any), configurationDraftPaths())
	info := doc["info"].(map[string]any)
	info["description"] = info["description"].(string) + " Configuration drafts (proposal F) hold candidate line, item, routing and workstation-profile revisions with field-level evidence citations; they are never live configuration. Owners and supervisors read and write drafts. Promotion appends the current draft revision through the normal revision path, requires every cited source to be available, and is refused (PROMOTER_IS_EDITOR) to the draft's author and every human editor. Promotion also requires a recent authentication. Idempotency-Key values starting with cfgdraft: are reserved for promotions and refused on direct revision routes. A request cites at most 500 field spans."
}

func configurationDraftPaths() map[string]any {
	param := func(name, in string, required bool, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "in": in, "required": required, "schema": schema}
	}
	id := map[string]any{"type": "string", "format": "uuid"}
	site, draft := param("site", "path", true, id), param("id", "path", true, id)
	key := param("Idempotency-Key", "header", true, map[string]any{"type": "string", "minLength": 1, "maxLength": 128})
	ifMatch := param("If-Match", "header", true, map[string]any{"type": "string", "pattern": `^"[1-9][0-9]*"$`})
	csrf := param("X-CSRF-Token", "header", true, map[string]any{"type": "string", "minLength": 1})
	origin := param("Origin", "header", true, map[string]any{"type": "string", "format": "uri"})
	session := []any{map[string]any{"session": []any{}}}
	etag := map[string]any{"ETag": map[string]any{"description": "Quoted current draft revision; send it as If-Match.", "schema": map[string]any{"type": "string"}}}
	described := func(description string) map[string]any {
		return map[string]any{"description": description, "content": map[string]any{"application/problem+json": map[string]any{"schema": ref("ProblemDetailsV1")}}}
	}
	reads := problemDetails("401", "404", "422", "503")
	reads["403"] = described("PERMISSION_DENIED: drafts are visible to owners and supervisors only")
	writes := problemDetails("401", "404", "412", "413", "415", "503")
	writes["412"] = described("REVISION_CONFLICT: If-Match does not name the draft's current revision")
	writes["428"] = described("REVISION_REQUIRED, or REAUTHENTICATION_REQUIRED when a promoter's authentication is not recent")
	writes["403"] = described("CSRF_REJECTED, PERMISSION_DENIED or PROMOTER_IS_EDITOR")
	writes["409"] = described("TARGET_REVISION_CONFLICT (the target object moved past target_expected_revision; discard the draft and create a new one), IDEMPOTENCY_CONFLICT, DRAFT_NOT_OPEN, DRAFT_UNCHANGED, DRAFT_SOURCE_UNAVAILABLE, LINE_VALIDATION_BLOCKED, MASTER_DATA_IN_USE, MASTER_DATA_UNCHANGED or STATION_PROFILE_UNCHANGED")
	writes["422"] = described("INVALID_CONFIGURATION_DRAFT, CITATION_NOT_IN_SCOPE or LIMIT_EXCEEDED with path, INSTRUCTION_NOT_APPROVED, or INVALID_REQUEST")
	response := func(description string) map[string]any {
		r := json200(description, "ConfigurationDraftV1")
		r["headers"] = etag
		return r
	}
	post := func(op, body, status, description string, params []any) map[string]any {
		return map[string]any{"operationId": op, "security": session, "parameters": append(params, csrf, origin), "requestBody": jsonBody(body), "responses": merge(map[string]any{status: response(description)}, writes)}
	}
	list := map[string]any{"operationId": "listConfigurationDrafts", "security": session,
		"parameters": []any{site, param("status", "query", false, map[string]any{"type": "string", "enum": []string{"discarded", "draft", "promoted"}}),
			param("kind", "query", false, map[string]any{"type": "string", "enum": []string{"item", "line", "routing", "workstation_profile"}}),
			param("limit", "query", false, map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 50}), param("cursor", "query", false, id)},
		"responses": merge(map[string]any{"200": json200("Drafts ordered by ID with current-revision citation counts.", "ConfigurationDraftPageV1")}, reads)}
	get := map[string]any{"operationId": "getConfigurationDraft", "security": session, "parameters": []any{site, draft},
		"responses": merge(map[string]any{"200": response("Draft with every revision; each citation is re-checked against the evidence ledger.")}, reads)}
	base := "/v1/sites/{site}/configuration-drafts"
	return map[string]any{
		base: map[string]any{
			"get":  list,
			"post": post("createConfigurationDraft", "ConfigurationDraftCreateInputV1", "201", "Human draft created at revision 1, or replayed.", []any{site, key}),
		},
		base + "/{id}":            map[string]any{"get": get},
		base + "/{id}/revisions":  map[string]any{"post": post("reviseConfigurationDraft", "ConfigurationDraftReviseInputV1", "201", "Human edit appended as a new immutable revision.", []any{site, draft, key, ifMatch})},
		base + "/{id}/promotions": map[string]any{"post": post("promoteConfigurationDraft", "ConfigurationDraftEmptyInputV1", "201", "Current revision appended to the target configuration object; the draft is promoted.", []any{site, draft, key, ifMatch})},
		base + "/{id}/discards":   map[string]any{"post": post("discardConfigurationDraft", "ConfigurationDraftEmptyInputV1", "200", "Draft discarded; configuration unchanged.", []any{site, draft, key, ifMatch})},
	}
}
