package contracts

import (
	"reflect"
	"slices"
	"strings"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/onboarding"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/operational"
)

// EvidenceSpanInputV1 is the body POST .../sources/{source}/spans decodes; the source comes
// from the path. The handler rejects a body missing any of the three members (422).
type EvidenceSpanInputV1 struct {
	Revision int64 `json:"revision"`
	Start    int   `json:"start_char"`
	End      int   `json:"end_char"`
}

// EvidenceRevocationInputV1 is the body POST .../sources/{source}/revocations decodes.
type EvidenceRevocationInputV1 struct {
	Reason string `json:"reason"`
}

// Catalog lists the published wire contracts: the OEE component and the WIS-270 evidence and
// knowledge routes. RequestContext is deliberately absent: tenant and actor come from
// verified authentication, never a payload.
func Catalog() []named {
	return append(append(append(append(append(append(append(append(append(append(append(append(append(oeeCatalog(), planCatalog()...), contextCatalog()...), knowledgeCatalog()...), memberCatalog()...), proposalCatalog()...), caseCatalog()...), interviewCatalog()...), masterDataCatalog()...), configurationImportCatalog()...), configurationDraftCatalog()...), operationalCatalog()...), chatCatalog()...), feedbackCatalog()...)
}

func knowledgeCatalog() []named {
	return []named{
		{"EvidenceAuthorityInputV1", reflect.TypeOf(evidence.AuthorityInput{})},
		{"EvidenceAuthorityV1", reflect.TypeOf(evidence.Authority{})},
		{"EvidenceCurrentnessInputV1", reflect.TypeOf(evidence.CurrentnessInput{})},
		{"EvidenceCurrentnessV1", reflect.TypeOf(evidence.Currentness{})},
		{"EvidenceRevocationInputV1", reflect.TypeOf(EvidenceRevocationInputV1{})},
		{"EvidenceRevocationV1", reflect.TypeOf(evidence.Revocation{})},
		{"EvidenceSourceInputV1", reflect.TypeOf(evidence.Input{})},
		{"EvidenceSourcePageV1", reflect.TypeOf(evidence.Page{})},
		{"EvidenceSourceV1", reflect.TypeOf(evidence.Source{})},
		{"EvidenceSpanInputV1", reflect.TypeOf(EvidenceSpanInputV1{})},
		{"EvidenceSpanV1", reflect.TypeOf(evidence.Span{})},
		{"SourceDocumentV1", reflect.TypeOf(onboarding.DocumentResult{})},
		{"KnowledgeClaimInputV1", reflect.TypeOf(knowledge.ClaimInput{})},
		{"KnowledgeClaimPageV1", reflect.TypeOf(knowledge.ClaimPage{})},
		{"KnowledgeClaimV1", reflect.TypeOf(knowledge.Claim{})},
		{"KnowledgeConflictPageV1", reflect.TypeOf(knowledge.ConflictPage{})},
		{"KnowledgeCoverageV1", reflect.TypeOf(knowledge.Coverage{})},
		{"KnowledgeSiteCoverageV1", reflect.TypeOf(knowledge.SiteCoverage{})},
		{"ExternalIdentityV1", reflect.TypeOf(operational.ExternalIdentity{})},
		{"KnowledgeDiscoveryInputV1", reflect.TypeOf(knowledge.DiscoveryInput{})},
		{"KnowledgeDiscoveryInventoryV1", reflect.TypeOf(knowledge.DiscoveryInventory{})},
		{"KnowledgeDiscoveryInventoryListItemV1", reflect.TypeOf(knowledge.DiscoveryInventoryListItem{})},
		{"KnowledgeDiscoveryInventoryPageV1", reflect.TypeOf(knowledge.DiscoveryInventoryPage{})},
		{"KnowledgeEntityInputV1", reflect.TypeOf(knowledge.EntityInput{})},
		{"KnowledgeEntityPageV1", reflect.TypeOf(knowledge.EntityPage{})},
		{"KnowledgeEntityV1", reflect.TypeOf(knowledge.Entity{})},
		{"KnowledgeEntityMergeInputV1", reflect.TypeOf(knowledge.MergeInput{})},
		{"KnowledgeEntityRedirectV1", reflect.TypeOf(knowledge.EntityRedirect{})},
		{"KnowledgeFreshnessPolicyInputV1", reflect.TypeOf(knowledge.FreshnessPolicyInput{})},
		{"KnowledgeFreshnessPolicyV1", reflect.TypeOf(knowledge.FreshnessPolicy{})},
		{"KnowledgeGraphV1", reflect.TypeOf(knowledge.GraphResult{})},
		{"KnowledgeInventoryV1", reflect.TypeOf(knowledge.Inventory{})},
		{"KnowledgeReadV1", reflect.TypeOf(knowledge.KnowledgeResult{})},
		{"KnowledgeRequirementDecisionInputV1", reflect.TypeOf(knowledge.RequirementDecisionInput{})},
		{"KnowledgeRequirementDecisionV1", reflect.TypeOf(knowledge.RequirementDecision{})},
		{"KnowledgeResolutionInputV1", reflect.TypeOf(knowledge.ResolutionInput{})},
		{"KnowledgeResolutionV1", reflect.TypeOf(knowledge.Resolution{})},
		{"KnowledgeReviewInputV1", reflect.TypeOf(knowledge.ReviewInput{})},
		{"KnowledgeReviewRevocationInputV1", reflect.TypeOf(knowledge.RevocationInput{})},
		{"KnowledgeReviewV1", reflect.TypeOf(knowledge.Review{})},
		{"KnowledgeRouteSummaryV1", reflect.TypeOf(knowledge.RouteSummary{})},
		{"KnowledgeSnapshotInputV1", reflect.TypeOf(knowledge.SnapshotInput{})},
		{"KnowledgeSnapshotV1", reflect.TypeOf(knowledge.Snapshot{})},
	}
}

// restrict narrows a generated request schema to what its handler accepts. The claim
// extractor is server-assigned: POST /claims answers 422 INVALID_CLAIM to any client value.
func restrict(name string, s map[string]any) {
	if name == "KnowledgeClaimInputV1" {
		delete(s["properties"].(map[string]any), "extractor")
	}
	restrictInterview(name, s)
	restrictConfigurationDraft(name, s)
	restrictOperational(name, s)
	restrictChat(name, s)
	restrictShiftSuggestion(name, s)
	restrictFeedback(name, s)
}

// addKnowledge publishes the WIS-270 evidence and knowledge paths into the OpenAPI document.
func addKnowledge(doc map[string]any) {
	merge(doc["paths"].(map[string]any), knowledgePaths())
	info := doc["info"].(map[string]any)
	info["description"] = info["description"].(string) + " The WIS-270 evidence (/sources, /source-spans) and knowledge (entities, claims, reviews, conflicts, discovery inventories, freshness policy, process snapshots) routes are site-scoped with RFC 9457 errors; every mutation, creation included, takes Idempotency-Key, a quoted integer If-Match, X-CSRF-Token and a same-origin Origin, and answers 200."
}

// problemSet documents each error status with the exact codes the handler family can emit.
// The codes come from the request scope, the shared mutation preconditions and the family's
// error mapping (evidenceError, knowledgeError) in internal/httpapi; they are a per-family
// superset, not traced per service method.
func problemSet(statuses map[string][]string) map[string]any {
	out := map[string]any{}
	for status, codes := range statuses {
		out[status] = map[string]any{"description": "Error: " + strings.Join(codes, ", "), "content": map[string]any{"application/problem+json": map[string]any{"schema": ref("ProblemDetailsV1")}}}
	}
	return out
}

func knowledgePaths() map[string]any {
	param := func(name, in string, required bool, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "in": in, "required": required, "schema": schema}
	}
	id := map[string]any{"type": "string", "format": "uuid"}
	revisionSchema := map[string]any{"type": "integer", "minimum": 1}
	key := param("Idempotency-Key", "header", true, map[string]any{"type": "string", "minLength": 1, "maxLength": 128})
	ifMatch := param("If-Match", "header", true, map[string]any{"type": "string", "pattern": `^"(0|[1-9][0-9]*)"$`})
	ifMatch["description"] = `Quoted current revision of the target ("0" when creating); a stale value is 412 REVISION_CONFLICT.`
	csrf := param("X-CSRF-Token", "header", true, map[string]any{"type": "string", "minLength": 1})
	csrf["description"] = "The csrf_token from GET /v1/me; the request must also carry the application's own Origin."
	revision := param("revision", "query", false, revisionSchema)
	revision["description"] = "Read this immutable revision instead of the current one."
	limit := param("limit", "query", false, map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 50})
	cursor := param("cursor", "query", false, map[string]any{"type": "string", "minLength": 1})
	session := []any{map[string]any{"session": []any{}}}
	etag := map[string]any{"ETag": map[string]any{"description": "Quoted revision of the returned state; send it back as If-Match.", "schema": map[string]any{"type": "string"}}}

	evidenceReads := map[string][]string{
		"401": {"AUTHENTICATION_REQUIRED"},
		"403": {"PERMISSION_DENIED"},
		"404": {"NOT_FOUND"},
		"409": {"SOURCE_CHANGED"},
		"422": {"INVALID_REQUEST", "INVALID_SOURCE"},
		"428": {"REAUTHENTICATION_REQUIRED"},
		"503": {"IDENTITY_UNAVAILABLE", "STORAGE_UNAVAILABLE", "EVIDENCE_UNAVAILABLE"},
	}
	knowledgeReads := map[string][]string{
		"401": {"AUTHENTICATION_REQUIRED"},
		"403": {"PERMISSION_DENIED"},
		"404": {"NOT_FOUND"},
		"409": {"BINDING_BLOCKED", "DISCOVERY_CONFLICT", "FRESHNESS_BLOCKED", "KNOWLEDGE_CYCLE", "RESOLUTION_BLOCKED", "REVIEW_BLOCKED", "REVIEW_CONFLICT", "SNAPSHOT_CONFLICT", "SNAPSHOT_STALE", "SOURCE_CHANGED"},
		"413": {"LIMIT_EXCEEDED"},
		"422": {"BINDING_UNSUPPORTED", "INVALID_CLAIM", "INVALID_REQUEST"},
		"428": {"REAUTHENTICATION_REQUIRED"},
		"503": {"IDENTITY_UNAVAILABLE", "STORAGE_UNAVAILABLE"},
	}
	// Writes add the shared mutation preconditions (CSRF, If-Match, Idempotency-Key, media
	// type, the 1 MiB body limit and strict decoding) to the family's read codes.
	writes := func(reads map[string][]string) map[string][]string {
		out := map[string][]string{}
		for k, v := range reads {
			out[k] = append([]string(nil), v...)
		}
		add := map[string][]string{
			"403": {"CSRF_REJECTED"},
			"409": {"IDEMPOTENCY_CONFLICT"},
			"412": {"REVISION_CONFLICT"},
			"413": {"BODY_TOO_LARGE"},
			"415": {"UNSUPPORTED_MEDIA"},
			"422": {"INVALID_REVISION", "INVALID_SOURCE"},
			"428": {"REVISION_REQUIRED"},
		}
		for k, v := range add {
			for _, c := range v {
				if !slices.Contains(out[k], c) {
					out[k] = append(out[k], c)
				}
			}
		}
		return out
	}
	evidenceWrites, knowledgeWrites := writes(evidenceReads), writes(knowledgeReads)
	// A document upload (WIS-613) has no revision precondition and a raw body: it adds the
	// upload refusals to the evidence reads instead of the JSON mutation preconditions.
	documentWrites := map[string][]string{}
	for k, v := range evidenceReads {
		documentWrites[k] = append([]string(nil), v...)
	}
	for k, v := range map[string][]string{"403": {"CSRF_REJECTED"}, "409": {"IDEMPOTENCY_CONFLICT"}, "413": {"LIMIT_EXCEEDED", "PDF_TOO_MANY_PAGES", "PDF_PARSE_BUDGET_EXCEEDED", "PDF_TOO_MANY_IMAGES"},
		"415": {"UNSUPPORTED_MEDIA", "PDF_ACTIVE_CONTENT", "PDF_EXTERNAL_REFERENCE", "PDF_SCAN_IMAGE_UNSUPPORTED"}, "422": {"UNREADABLE_SOURCE", "PDF_ENCRYPTED", "PDF_NO_TEXT_LAYER", "PDF_TEXT_UNREADABLE", "PDF_SCAN_HIDDEN_CONTENT"}, "503": {"EXTRACTION_UNAVAILABLE", "EXTRACTION_BUSY", "SCAN_QUEUE_FULL"}} {
		documentWrites[k] = append(documentWrites[k], v...)
	}

	ok := func(description, schema string, withETag bool) map[string]any {
		r := json200(description, schema)
		if withETag {
			r["headers"] = etag
		}
		return map[string]any{"200": r}
	}
	get := func(opID string, params []any, response map[string]any, errs map[string][]string) map[string]any {
		return map[string]any{"operationId": opID, "security": session, "parameters": params, "responses": merge(response, problemSet(errs))}
	}
	post := func(opID string, params []any, body string, response map[string]any, errs map[string][]string) map[string]any {
		o := get(opID, append(params, key, ifMatch, csrf), response, errs)
		o["requestBody"] = jsonBody(body)
		return o
	}
	site := param("site", "path", true, id)
	path := func(name string) map[string]any { return param(name, "path", true, id) }
	source, claim, conflict, inventory := path("source"), path("claim"), path("conflict"), path("inventory")

	graph := []any{site, path("snapshot"),
		param("root", "query", true, map[string]any{"type": "array", "items": id, "minItems": 1, "maxItems": 20}),
		param("predicate", "query", true, map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "maxItems": 32}),
		param("as_of", "query", true, map[string]any{"type": "string", "format": "date-time"}),
		param("hops", "query", false, map[string]any{"type": "integer", "minimum": 0, "maximum": 2, "default": 1}),
		param("max_entities", "query", false, map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "default": 200}),
		param("max_claims", "query", false, map[string]any{"type": "integer", "minimum": 1, "maximum": 500, "default": 500}),
		param("direction", "query", false, map[string]any{"type": "string", "enum": []any{"both", "incoming", "outgoing"}, "default": "outgoing"}),
	}
	routeSummary := []any{site, path("snapshot"),
		param("process_id", "query", true, id),
		param("as_of", "query", true, map[string]any{"type": "string", "format": "date-time"}),
	}
	claimFilters := []any{site, limit, cursor,
		param("subject_id", "query", false, id),
		param("state", "query", false, map[string]any{"type": "string", "enum": []any{"approved", "candidate", "contested", "rejected"}}),
	}
	createClaim := withDescription(post("createClaim", []any{site}, "KnowledgeClaimInputV1", ok("Claim revision recorded, or replayed", "KnowledgeClaimV1", true), knowledgeWrites),
		"extractor is server-assigned; a request carrying it is 422 INVALID_CLAIM.")
	requirementDecisions := "/v1/sites/{site}/discovery-inventories/{inventory}/requirements/{requirement}/decisions"
	requirement := path("requirement")

	return map[string]any{
		"/v1/sites/{site}/sources": map[string]any{
			"post": post("createOrReviseSource", []any{site}, "EvidenceSourceInputV1", ok("Source revision recorded, or replayed", "EvidenceSourceV1", true), evidenceWrites),
			"get":  get("listSources", []any{site, limit, cursor}, ok("Source summaries", "EvidenceSourcePageV1", false), evidenceReads),
		},
		// WIS-613: an uploaded document becomes a document source with automatic spans. The body is
		// the raw document; its kind is verified from the bytes, never the declared type or a name.
		"/v1/sites/{site}/source-documents": map[string]any{
			"post": func() map[string]any {
				title := param("X-Source-Title", "header", true, map[string]any{"type": "string", "minLength": 1})
				title["description"] = "The source title, UTF-8 percent-encoded (1-200 characters once decoded)."
				o := get("uploadSourceDocument", []any{site, title, key, csrf},
					ok("Document verified and extracted (Word paragraphs and table rows, Excel sheets and rows, a born-digital PDF's pages one line per text row, CSV or text, values as stored). Its text and its automatic citable spans (consecutive lines, at most 2000 characters each) are created in one transaction as a document source; source.byte_length is the extracted size. The original bytes are not kept: original_sha256 identifies them and is bound by the Idempotency-Key, so a different file under the same key is 409 IDEMPOTENCY_CONFLICT. A PDF is parsed in a short-lived, resource-capped child process. Refusals decided by the file are 4xx and resending it cannot succeed: 413 LIMIT_EXCEEDED over 20 MiB or when the text exceeds a source, PDF_TOO_MANY_PAGES over 1000 pages, PDF_PARSE_BUDGET_EXCEEDED when parsing needs more than its memory or time; 415 UNSUPPORTED_MEDIA for executables, archives and a type that does not match the bytes, PDF_ACTIVE_CONTENT for JavaScript, attachments, launch, form submission or rich media, PDF_EXTERNAL_REFERENCE for hyperlinks and remote references (as DOCX external links); 422 UNREADABLE_SOURCE for macro-enabled, externally linked Office files, corrupt or textless files, PDF_ENCRYPTED, PDF_NO_TEXT_LAYER for a PDF without typed text (a scan, including one with an invisible OCR layer: read it by /source-scan-batches) or a page showing what neither its text nor the vision model can read (an annotation, an inline image, a form or pattern that shows anything, vector drawing on a page without typed text), PDF_TEXT_UNREADABLE for text that is not valid UTF-8, PDF_SCAN_HIDDEN_CONTENT for typed text the page hides from a reader (painted over or clipped, for example) or a figure the page does not show whole and plainly (off the page or its crop box, clipped, painted over, rotated, transparent, masked). A typed PDF's raster figures (at least 100 px on the shorter side and 2% of the page; smaller images are skipped and counted in images_skipped) are queued for the local vision model in the same transaction, one job per figure titled \"<title> (page P, image K)\" (image_reads); each becomes its own model-extracted source, designated only with confirm_model_extracted. At most 20 figures (413 PDF_TOO_MANY_IMAGES); 415 PDF_SCAN_IMAGE_UNSUPPORTED for a figure that is not JPEG or lossless; LIMIT_EXCEEDED for one over 4 MiB; 503 EXTRACTION_UNAVAILABLE for a PDF with figures when no vision model is configured. 503 EXTRACTION_BUSY (with Retry-After) when every parser slot is taken, and EXTRACTION_UNAVAILABLE when the parser cannot run: retrying the same key is safe. 503 SCAN_QUEUE_FULL (with Retry-After) when the figures would take the tenant past its cap on running scan pages (default 40); nothing is stored, and the same key retries", "SourceDocumentV1", true),
					documentWrites)
				content := map[string]any{}
				for _, media := range []string{"text/plain", "text/markdown", "text/csv", "application/pdf",
					"application/vnd.openxmlformats-officedocument.wordprocessingml.document", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"} {
					content[media] = map[string]any{"schema": map[string]any{"type": "string", "format": "binary", "maxLength": 20 << 20}}
				}
				o["requestBody"] = map[string]any{"required": true, "content": content}
				return o
			}(),
		},
		"/v1/sites/{site}/sources/{source}": map[string]any{
			"get": get("getSource", []any{site, source, revision}, ok("Source revision", "EvidenceSourceV1", true), evidenceReads),
		},
		"/v1/sites/{site}/sources/{source}/spans": map[string]any{
			"post": post("createSourceSpan", []any{site, source}, "EvidenceSpanInputV1", ok("Span recorded, or replayed", "EvidenceSpanV1", false), evidenceWrites),
		},
		"/v1/sites/{site}/source-spans/{span}": map[string]any{
			"get": get("getSourceSpan", []any{site, path("span")}, ok("Span with its cited text", "EvidenceSpanV1", false), evidenceReads),
		},
		"/v1/sites/{site}/sources/{source}/revocations": map[string]any{
			"post": post("revokeSource", []any{site, source}, "EvidenceRevocationInputV1", ok("Source revoked, or replayed; requires recent authentication", "EvidenceRevocationV1", false), evidenceWrites),
		},
		"/v1/sites/{site}/sources/{source}/currentness-decisions": map[string]any{
			"post": post("decideSourceCurrentness", []any{site, source}, "EvidenceCurrentnessInputV1", ok("Currentness decision recorded, or replayed; requires recent authentication", "EvidenceCurrentnessV1", true), evidenceWrites),
		},
		"/v1/sites/{site}/sources/{source}/currentness": map[string]any{
			"get": get("getSourceCurrentness", []any{site, source, revision}, ok("Currentness decision", "EvidenceCurrentnessV1", true), evidenceReads),
		},
		"/v1/sites/{site}/sources/{source}/authority-decisions": map[string]any{
			"post": post("decideSourceAuthority", []any{site, source}, "EvidenceAuthorityInputV1", ok("Authority decision recorded, or replayed; requires recent authentication", "EvidenceAuthorityV1", true), evidenceWrites),
		},
		"/v1/sites/{site}/sources/{source}/authority": map[string]any{
			"get": get("getSourceAuthority", []any{site, source, revision}, ok("Authority decision", "EvidenceAuthorityV1", true), evidenceReads),
		},
		"/v1/sites/{site}/process-snapshots/{snapshot}/graph": map[string]any{
			"get": get("getSnapshotGraph", graph, ok("Bounded traversal of an approved process snapshot", "KnowledgeGraphV1", false), knowledgeReads),
		},
		"/v1/sites/{site}/process-snapshots/{snapshot}/route-summary": map[string]any{
			"get": get("getSnapshotRouteSummary", routeSummary, ok("Ordered route of one process with exact totals (WIS-642)", "KnowledgeRouteSummaryV1", false), knowledgeReads),
		},
		"/v1/sites/{site}/knowledge-inventory": map[string]any{
			"get": get("getKnowledgeInventory", []any{site}, ok("Site knowledge inventory", "KnowledgeInventoryV1", false), knowledgeReads),
		},
		"/v1/sites/{site}/document-freshness-policy-decisions": map[string]any{
			"post": post("decideFreshnessPolicy", []any{site}, "KnowledgeFreshnessPolicyInputV1", ok("Policy revision recorded, or replayed", "KnowledgeFreshnessPolicyV1", true), knowledgeWrites),
		},
		"/v1/sites/{site}/document-freshness-policy": map[string]any{
			"get": get("getFreshnessPolicy", []any{site, revision}, ok("Document freshness policy", "KnowledgeFreshnessPolicyV1", true), knowledgeReads),
		},
		"/v1/sites/{site}/discovery-inventories": map[string]any{
			"post": post("createDiscoveryInventory", []any{site}, "KnowledgeDiscoveryInputV1", ok("Inventory revision recorded, or replayed", "KnowledgeDiscoveryInventoryV1", true), knowledgeWrites),
			"get": withDescription(get("listDiscoveryInventories", []any{site, limit, param("cursor", "query", false, id), param("process_id", "query", false, id)}, ok("Site-scoped discovery inventories ordered by inventory ID", "KnowledgeDiscoveryInventoryPageV1", false), knowledgeReads),
				"Unknown or repeated query parameters are 422 INVALID_REQUEST. Pagination defaults to 50 and is capped at 100."),
		},
		"/v1/sites/{site}/discovery-inventories/{inventory}": map[string]any{
			"get": get("getDiscoveryInventory", []any{site, inventory, revision}, ok("Discovery inventory", "KnowledgeDiscoveryInventoryV1", true), knowledgeReads),
		},
		"/v1/sites/{site}/discovery-inventories/{inventory}/coverage": map[string]any{
			"get": withDescription(get("getDiscoveryCoverage", []any{site, inventory}, ok("Requirement coverage of the current inventory revision", "KnowledgeCoverageV1", false), knowledgeReads),
				"Any query string is 422 INVALID_REQUEST."),
		},
		"/v1/sites/{site}/knowledge": map[string]any{
			"get": withDescription(get("getKnowledge", []any{site,
				param("entity_id", "query", true, id),
				param("as_of", "query", false, map[string]any{"type": "string", "format": "date-time"}),
				param("depth", "query", false, map[string]any{"type": "integer", "minimum": 0, "maximum": 2, "default": 1}),
			}, ok("The entity's neighbourhood in the latest snapshot captured at or before as_of, or NOT_READY with NO_SNAPSHOT, SNAPSHOT_STALE, SNAPSHOT_UNAVAILABLE or ENTITY_NOT_IN_SNAPSHOT and no graph content", "KnowledgeReadV1", false), knowledgeReads),
				"as_of is RFC3339 with an explicit offset and must not be in the future; omitted means now. Unknown query parameters are 422 INVALID_REQUEST."),
		},
		"/v1/sites/{site}/coverage": map[string]any{
			"get": withDescription(get("getSiteCoverage", []any{site}, ok("Coverage of the site's single discovery inventory, or NOT_READY with NO_INVENTORY, INVENTORY_AMBIGUOUS or INVENTORY_UNAVAILABLE", "KnowledgeSiteCoverageV1", false), knowledgeReads),
				"Any query string is 422 INVALID_REQUEST."),
		},
		requirementDecisions: map[string]any{
			"post": post("decideRequirement", []any{site, inventory, requirement}, "KnowledgeRequirementDecisionInputV1", ok("Requirement decision recorded, or replayed", "KnowledgeRequirementDecisionV1", true), knowledgeWrites),
			"get": get("getRequirementDecision", []any{site, inventory, requirement, param("inventory_revision", "query", true, revisionSchema), revision},
				ok("Requirement decision for the inventory revision", "KnowledgeRequirementDecisionV1", true), knowledgeReads),
		},
		"/v1/sites/{site}/process-snapshots": map[string]any{
			"post": post("createProcessSnapshot", []any{site}, "KnowledgeSnapshotInputV1", ok("Snapshot sealed, or replayed", "KnowledgeSnapshotV1", false), knowledgeWrites),
		},
		"/v1/sites/{site}/process-snapshots/{snapshot}": map[string]any{
			"get": get("getProcessSnapshot", []any{site, path("snapshot")}, ok("Sealed process snapshot", "KnowledgeSnapshotV1", false), knowledgeReads),
		},
		"/v1/sites/{site}/entities": map[string]any{
			"post": post("createEntity", []any{site}, "KnowledgeEntityInputV1", ok("Entity revision recorded, or replayed", "KnowledgeEntityV1", true), knowledgeWrites),
			"get":  get("listEntities", []any{site, limit, cursor}, ok("Entity heads", "KnowledgeEntityPageV1", false), knowledgeReads),
		},
		"/v1/sites/{site}/entities/{entity}": map[string]any{
			"get": get("getEntity", []any{site, path("entity"), revision}, ok("Entity revision", "KnowledgeEntityV1", true), knowledgeReads),
		},
		"/v1/sites/{site}/entities/{entity}/merges": map[string]any{
			"post": withDescription(post("mergeEntity", []any{site, path("entity")}, "KnowledgeEntityMergeInputV1", ok("Redirect decision recorded, or replayed", "KnowledgeEntityRedirectV1", true), knowledgeWrites),
				"Owner or supervisor with fresh authentication. If-Match carries from_revision. An entity is redirected at most once; merging into a redirected entity, or a merge that would make any chain longer than 16 hops, is 409 KNOWLEDGE_CYCLE."),
		},
		"/v1/sites/{site}/claims": map[string]any{
			"post": createClaim,
			"get":  get("listClaims", claimFilters, ok("Claim heads filtered by the review state of their current revision", "KnowledgeClaimPageV1", false), knowledgeReads),
		},
		"/v1/sites/{site}/claims/{claim}": map[string]any{
			"get": get("getClaim", []any{site, claim, revision}, ok("Claim revision", "KnowledgeClaimV1", true), knowledgeReads),
		},
		"/v1/sites/{site}/claims/{claim}/conflicts": map[string]any{
			"get": get("listClaimConflicts", []any{site, claim, revision, limit, cursor}, ok("Conflicts recorded against the claim revision", "KnowledgeConflictPageV1", false), knowledgeReads),
		},
		"/v1/sites/{site}/claims/{claim}/reviews": map[string]any{
			"post": post("reviewClaim", []any{site, claim}, "KnowledgeReviewInputV1", ok("Review recorded, or replayed", "KnowledgeReviewV1", true), knowledgeWrites),
		},
		"/v1/sites/{site}/claims/{claim}/review-revocations": map[string]any{
			"post": post("revokeClaimReview", []any{site, claim}, "KnowledgeReviewRevocationInputV1", ok("Review withdrawn, or replayed", "KnowledgeReviewV1", true), knowledgeWrites),
		},
		"/v1/sites/{site}/claims/{claim}/review": map[string]any{
			"get": get("getClaimReview", []any{site, claim, revision}, ok("Review of the claim revision", "KnowledgeReviewV1", true), knowledgeReads),
		},
		// WIS-270 external identity resolution (registered in internal/httpapi/operational.go).
		"/v1/sites/{site}/external-identities/resolve": map[string]any{
			"get": withDescription(get("resolveExternalIdentity", []any{site,
				param("source_id", "query", true, id),
				param("entity_type", "query", true, map[string]any{"type": "string", "enum": []any{"asset", "order"}}),
				param("external_key", "query", true, map[string]any{"type": "string", "minLength": 1, "maxLength": 512}),
			}, ok("The entity an exact, currently approved mapping names, following merge redirects", "ExternalIdentityV1", false), map[string][]string{
				"401": {"AUTHENTICATION_REQUIRED"},
				"403": {"PERMISSION_DENIED"},
				"404": {"NOT_FOUND"},
				"409": {"UNKNOWN_MAPPING"},
				"422": {"INVALID_REQUEST"},
				"428": {"REAUTHENTICATION_REQUIRED"},
				"503": {"IDENTITY_UNAVAILABLE", "STORAGE_UNAVAILABLE"},
			}),
				"Exactly the three query parameters, each once; anything else is 422 INVALID_REQUEST. An unknown or out-of-scope source is 404. Every other refusal (unmapped, unreviewed, rejected, disabled or revoked source, stale or merged-away target, revised or revoked evidence) is the same 409 UNKNOWN_MAPPING body. Not cached: approval can be revoked at any moment."),
		},
		"/v1/sites/{site}/conflicts/{conflict}/resolutions": map[string]any{
			"post": post("resolveConflict", []any{site, conflict}, "KnowledgeResolutionInputV1", ok("Resolution recorded, or replayed", "KnowledgeResolutionV1", true), knowledgeWrites),
			"get":  get("getConflictResolution", []any{site, conflict, revision}, ok("Conflict resolution", "KnowledgeResolutionV1", true), knowledgeReads),
		},
	}
}

func withDescription(op map[string]any, description string) map[string]any {
	op["description"] = description
	return op
}
