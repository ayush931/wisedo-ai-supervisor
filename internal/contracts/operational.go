package contracts

import (
	"reflect"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/operational"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/production"
)

// operationalCatalog publishes the WIS-270/272 operational-source registry and its
// external-key mapping set. The mapping is identity configuration only: an (asset|order,
// external_key) pair resolves to one exact knowledge entity revision.
func operationalCatalog() []named {
	return []named{
		{"OperationalBatchV1", reflect.TypeOf(ingestion.Batch{})},
		{"ProductionDeltaV1", reflect.TypeOf(production.Delta{})},
		{"OperationalSourceConfigV1", reflect.TypeOf(operational.Config{})},
		{"OperationalSourceMappingV1", reflect.TypeOf(operational.Mapping{})},
		{"OperationalSourceMappingsInputV1", reflect.TypeOf(operational.MappingsInput{})},
		{"OperationalSourceReviewV1", reflect.TypeOf(operational.Review{})},
		{"OperationalSourceV1", reflect.TypeOf(operational.Source{})},
		{"OperationalSourceDetailV1", reflect.TypeOf(operational.Detail{})},
		{"OperationalSourceSummaryV1", reflect.TypeOf(operational.SourceSummary{})},
		{"OperationalSourcePageV1", reflect.TypeOf(operational.SourcePage{})},
		{"OperationalImportJobV1", reflect.TypeOf(ingestion.ImportJob{})},
	}
}

// restrictOperational adds the closed value sets the Go validators enforce on plain string
// fields, and removes credential_ref from read models: the site API always clears it. The
// generator inlines nested structs, so the rules apply to every nested object by shape.
func restrictOperational(name string, s map[string]any) {
	if name == "OperationalBatchV1" {
		// Payload shape is selected by event kind; other event kinds keep their own formats.
		delta := Schema(reflect.TypeOf(ingestion.Event{}), nil)
		props := delta["properties"].(map[string]any)
		props["kind"] = map[string]any{"type": "string", "enum": []any{"production_delta"}}
		props["payload"] = Schema(reflect.TypeOf(production.Delta{}), nil)
		other := Schema(reflect.TypeOf(ingestion.Event{}), nil)
		other["properties"].(map[string]any)["kind"] = map[string]any{"type": "string", "enum": []any{"production_plan", "machine_state", "operator_report", "heartbeat", "watermark"}}
		s["properties"].(map[string]any)["events"].(map[string]any)["items"] = map[string]any{"anyOf": []any{delta, other}}
	}
	if name == "OperationalImportJobV1" {
		props := s["properties"].(map[string]any)
		props["status"].(map[string]any)["enum"] = []any{"running", "completed", "partial", "cancelled"}
		row := props["rows"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
		row["status"].(map[string]any)["enum"] = []any{"accepted", "duplicate", "quarantined", "rejected"}
	}
	if len(name) < len("Operational") || name[:len("Operational")] != "Operational" {
		return
	}
	var walk func(v any)
	walk = func(v any) {
		node, ok := v.(map[string]any)
		if !ok {
			return
		}
		if props, ok := node["properties"].(map[string]any); ok {
			set := func(field, key string, value any) {
				if p, ok := props[field].(map[string]any); ok {
					p[key] = value
				}
			}
			if _, mapping := props["external_type"]; mapping {
				set("external_type", "enum", []any{"asset", "order"})
				set("external_key", "minLength", 1)
				set("external_key", "maxLength", 512)
				set("entity_id", "format", "uuid")
				set("entity_revision", "minimum", 1)
			}
			if _, source := props["authority_scope"]; source {
				set("type", "enum", []any{"csv", "event_api", "json", "manual"})
				set("source_id", "format", "uuid")
				if name != "OperationalSourceConfigV1" {
					delete(props, "credential_ref")
				}
			}
			set("verdict", "enum", []any{"approved", "rejected"})
			set("review_verdict", "enum", []any{"approved", "rejected"})
			if _, mappings := props["mappings"]; mappings && name == "OperationalSourceMappingsInputV1" {
				set("mappings", "maxItems", 10000)
			}
			for _, child := range props {
				walk(child)
			}
		}
		walk(node["items"])
		if any, ok := node["anyOf"].([]any); ok {
			for _, child := range any {
				walk(child)
			}
		}
	}
	walk(s)
}

func addOperational(doc map[string]any) {
	merge(doc["paths"].(map[string]any), operationalPaths())
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	batch := schemas["OperationalBatchV1"].(map[string]any)
	events := batch["properties"].(map[string]any)["events"].(map[string]any)
	delta := events["items"].(map[string]any)["anyOf"].([]any)[0].(map[string]any)
	delta["properties"].(map[string]any)["payload"] = ref("ProductionDeltaV1")
	info := doc["info"].(map[string]any)
	info["description"] = info["description"].(string) + " Operational sources (WIS-270/272) are site-scoped and listed by source_id with a stable cursor. Every active member may read; registry and mapping writes require an owner or supervisor role with recent authentication. Batch intake uses its own event admission checks. A mapping revision takes If-Match on the mapping revision (X-Mapping-ETag) and appends a new source revision, which invalidates any earlier review approval. A row-level mapping problem carries path mappings[i].member with the caller's input index: 422 INVALID_SOURCE, 404 NOT_FOUND (unknown entity), 412 REVISION_CONFLICT (stale entity_revision) or 409 UNKNOWN_MAPPING (merged entity); a 412 without a path is a stale mapping revision."
}

func operationalPaths() map[string]any {
	param := func(name, in string, required bool, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "in": in, "required": required, "schema": schema}
	}
	id := map[string]any{"type": "string", "format": "uuid"}
	site := param("site", "path", true, id)
	source := param("source", "path", true, id)
	key := param("Idempotency-Key", "header", true, map[string]any{"type": "string", "minLength": 1, "maxLength": 128})
	ifMatch := param("If-Match", "header", true, map[string]any{"type": "string", "pattern": `^"(0|[1-9][0-9]*)"$`})
	csrf := param("X-CSRF-Token", "header", true, map[string]any{"type": "string", "minLength": 1})
	origin := param("Origin", "header", true, map[string]any{"type": "string", "format": "uri"})
	session := []any{map[string]any{"session": []any{}}}
	header := func(description string) map[string]any {
		return map[string]any{"description": description, "schema": map[string]any{"type": "string"}}
	}
	reads := problemSet(map[string][]string{"401": {"AUTHENTICATION_REQUIRED"}, "404": {"NOT_FOUND"}, "422": {"INVALID_REQUEST", "INVALID_SOURCE"}, "503": {"STORAGE_UNAVAILABLE"}})
	writes := problemSet(map[string][]string{
		"401": {"AUTHENTICATION_REQUIRED"}, "403": {"CSRF_REJECTED", "PERMISSION_DENIED"}, "404": {"NOT_FOUND"},
		"409": {"IDEMPOTENCY_CONFLICT", "UNKNOWN_MAPPING"}, "412": {"REVISION_CONFLICT"}, "415": {"UNSUPPORTED_MEDIA"},
		"422": {"INVALID_REQUEST", "INVALID_SOURCE", "ACCESS_POLICY_REGRESSION"}, "428": {"REAUTHENTICATION_REQUIRED"}, "503": {"STORAGE_UNAVAILABLE"},
	})
	list := json200("One page of the site's operational sources ordered by source_id.", "OperationalSourcePageV1")
	detail := json200("Current (or requested) source revision with its mapping rows.", "OperationalSourceDetailV1")
	detail["headers"] = map[string]any{"ETag": header("Quoted source revision."), "X-Mapping-ETag": header("Quoted mapping revision; send it as If-Match on /mappings.")}
	sourceWrite := json200("Created or revised source.", "OperationalSourceV1")
	sourceWrite["headers"] = map[string]any{"ETag": header("Quoted source revision."), "X-Mapping-ETag": header("Quoted mapping revision.")}
	mappingWrite := json200("Source after the mapping revision.", "OperationalSourceV1")
	mappingWrite["headers"] = map[string]any{"ETag": header("Quoted new mapping revision."), "X-Source-ETag": header("Quoted new source revision.")}
	base := "/v1/sites/{site}/operational-sources"
	cancel := json200("The cancelled job with its first page of row outcomes; replayed is true for a same-key retry.", "OperationalImportJobV1")
	empty := map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "object", "additionalProperties": false}}}}
	return map[string]any{
		"/v1/sites/{site}/operational-ingestion-jobs/{job}/cancel": map[string]any{
			"post": map[string]any{"operationId": "cancelOperationalImport", "description": "An owner or supervisor ends a running import (WIS-719). The job keeps the outcomes of every chunk that committed; the cancellation waits for a chunk in flight. Operators may import but not cancel. The job has no revision, so no If-Match: a job that is no longer running is 409 IMPORT_NOT_RUNNING. A same-key retry by the same actor replays 200 with replayed=true.",
				"security": session, "parameters": []any{site, param("job", "path", true, id), key, csrf, origin}, "requestBody": empty, "responses": merge(map[string]any{"200": cancel}, problemSet(map[string][]string{
					"401": {"AUTHENTICATION_REQUIRED"}, "403": {"CSRF_REJECTED", "PERMISSION_DENIED"}, "404": {"NOT_FOUND"},
					"409": {"IDEMPOTENCY_CONFLICT", "IMPORT_NOT_RUNNING"}, "415": {"UNSUPPORTED_MEDIA"}, "422": {"INVALID_REQUEST"}, "503": {"INGESTION_UNAVAILABLE"},
				}))},
		},
		base: map[string]any{
			"get": map[string]any{"operationId": "listOperationalSources", "security": session, "parameters": []any{site,
				param("limit", "query", false, map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 50}),
				param("cursor", "query", false, id)}, "responses": merge(map[string]any{"200": list}, reads)},
			"post": map[string]any{"operationId": "createOrReviseOperationalSource", "security": session, "parameters": []any{site, key, ifMatch, csrf, origin}, "requestBody": jsonBody("OperationalSourceConfigV1"), "responses": merge(map[string]any{"200": sourceWrite}, writes)},
		},
		base + "/{source}": map[string]any{
			"get": map[string]any{"operationId": "getOperationalSource", "security": session, "parameters": []any{site, source,
				param("revision", "query", false, map[string]any{"type": "integer", "minimum": 1})}, "responses": merge(map[string]any{"200": detail}, reads)},
		},
		base + "/{source}/mappings": map[string]any{
			"post": map[string]any{"operationId": "reviseOperationalSourceMappings", "security": session, "parameters": []any{site, source, key, ifMatch, csrf, origin}, "requestBody": jsonBody("OperationalSourceMappingsInputV1"), "responses": merge(map[string]any{"200": mappingWrite}, writes)},
		},
		base + "/{source}/batches": map[string]any{
			"post": map[string]any{"operationId": "acceptOperationalBatch", "security": session, "parameters": []any{site, source, key, csrf, origin}, "requestBody": jsonBody("OperationalBatchV1"), "responses": merge(map[string]any{"202": map[string]any{"description": "Durable batch receipt, including replay status. Semantic acceptance and quarantine are reported after folding."}}, problemSet(map[string][]string{
				"401": {"AUTHENTICATION_REQUIRED"}, "403": {"CSRF_REJECTED", "PERMISSION_DENIED"}, "404": {"NOT_FOUND"},
				"409": {"IDEMPOTENCY_CONFLICT", "SOURCE_SEQUENCE_CONFLICT", "IMPORT_IN_PROGRESS", "EVENT_REVISION_CONFLICT"}, "412": {"SOURCE_REVISION_CONFLICT"}, "413": {"BODY_TOO_LARGE"}, "415": {"UNSUPPORTED_MEDIA"}, "422": {"INVALID_REQUEST", "INVALID_EVENT_BATCH"}, "503": {"INGESTION_UNAVAILABLE"},
			}))},
		},
	}
}
