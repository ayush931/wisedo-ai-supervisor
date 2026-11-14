package contracts

import (
	"reflect"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
)

func configurationImportCatalog() []named {
	return []named{
		{"ConfigurationImportCountsV1", reflect.TypeOf(workstation.ConfigurationImportCountsV1{})},
		{"ConfigurationImportRowV1", reflect.TypeOf(workstation.ConfigurationImportRowV1{})},
		{"ConfigurationImportReportV1", reflect.TypeOf(workstation.ConfigurationImportReportV1{})},
	}
}

func addConfigurationImports(doc map[string]any) {
	merge(doc["paths"].(map[string]any), configurationImportPaths())
	info := doc["info"].(map[string]any)
	info["description"] = info["description"].(string) + " Site configuration imports accept strict CSV for lines, items, shift calendars and routings, capped at 2,000 rows per file to keep the site lock within its 5-second writer lock timeout; raising this cap requires a faster import and a re-measured bound. Dry-run report digests bind the apply precondition to current site master data. OBJECT_ALREADY_CREATED identifies a deterministic import object ID that already exists with changed content and reports path ref."
}

func configurationImportPaths() map[string]any {
	session := []any{map[string]any{"session": []any{}}}
	param := func(name, in string, required bool, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "in": in, "required": required, "schema": schema}
	}
	site := param("site", "path", true, map[string]any{"type": "string", "format": "uuid"})
	id := param("id", "path", true, map[string]any{"type": "string", "format": "uuid"})
	csrf := param("X-CSRF-Token", "header", true, map[string]any{"type": "string", "minLength": 1})
	origin := param("Origin", "header", true, map[string]any{"type": "string", "format": "uri"})
	key := param("Idempotency-Key", "header", true, map[string]any{"type": "string", "minLength": 1, "maxLength": 128})
	ifMatch := param("If-Match", "header", true, map[string]any{"type": "string", "pattern": `^"[0-9a-f]{64}"$`})
	fileBody := map[string]any{"required": true, "content": map[string]any{"multipart/form-data": map[string]any{"schema": map[string]any{"type": "object", "required": []string{"file"}, "properties": map[string]any{"file": map[string]any{"type": "string", "format": "binary"}}}}}}
	responseHeaders := map[string]any{
		"ETag":     map[string]any{"description": "Quoted report digest for the apply precondition.", "schema": map[string]any{"type": "string"}},
		"Location": map[string]any{"description": "Site-scoped URI of the immutable applied report.", "schema": map[string]any{"type": "string", "format": "uri-reference"}},
	}
	reads := problemDetails("401", "403", "404", "422", "503")
	writes := problemDetails("401", "403", "404", "409", "412", "413", "415", "422", "428", "503")
	dryResponse := json200("Evaluated configuration rows; no changes are persisted.", "ConfigurationImportReportV1")
	dryResponse["content"].(map[string]any)["text/csv"] = map[string]any{"schema": map[string]any{"type": "string"}}
	dryResponse["headers"] = map[string]any{
		"ETag":                responseHeaders["ETag"],
		"Content-Disposition": map[string]any{"description": "Static attachment name for the errors CSV.", "schema": map[string]any{"type": "string"}},
	}
	dryErrors := merge(problemDetails("413", "415"), problemDetails("422"))
	dryErrors["422"] = map[string]any{"description": "INVALID_CSV, UNKNOWN_COLUMN, DUPLICATE_COLUMN, MISSING_COLUMN, EMPTY_IMPORT or INVALID_REQUEST", "content": map[string]any{"application/problem+json": map[string]any{"schema": ref("ProblemDetailsV1")}}}
	dry := map[string]any{"operationId": "dryRunConfigurationImport", "security": session, "parameters": []any{site, csrf, origin, param("Accept", "header", false, map[string]any{"type": "string", "enum": []string{"application/json", "text/csv"}})}, "requestBody": fileBody, "responses": merge(map[string]any{"200": dryResponse}, merge(reads, dryErrors))}
	applyResponse := json200("Atomically applied configuration rows or replayed the same idempotent import.", "ConfigurationImportReportV1")
	applyResponse["headers"] = responseHeaders
	apply := map[string]any{"operationId": "applyConfigurationImport", "security": session, "parameters": []any{site, key, ifMatch, csrf, origin}, "requestBody": fileBody, "responses": merge(map[string]any{"200": applyResponse, "201": applyResponse}, writes)}
	get := json200("Applied configuration import report.", "ConfigurationImportReportV1")
	get["headers"] = map[string]any{"ETag": responseHeaders["ETag"]}
	return map[string]any{
		"/v1/sites/{site}/configuration-imports/dry-runs": map[string]any{"post": dry},
		"/v1/sites/{site}/configuration-imports":          map[string]any{"post": apply},
		"/v1/sites/{site}/configuration-imports/{id}":     map[string]any{"get": map[string]any{"operationId": "getConfigurationImport", "security": session, "parameters": []any{site, id}, "responses": merge(map[string]any{"200": get}, reads)}},
	}
}
