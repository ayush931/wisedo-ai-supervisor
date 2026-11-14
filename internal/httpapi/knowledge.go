package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
)

func mutationHeaders(w http.ResponseWriter, r *http.Request) (string, int64, bool) {
	tag := r.Header.Get("If-Match")
	if tag == "" {
		problem(w, 428, "REVISION_REQUIRED")
		return "", 0, false
	}
	if len(r.Header.Values("If-Match")) != 1 || len(tag) < 3 || tag[0] != '"' || tag[len(tag)-1] != '"' {
		problem(w, 422, "INVALID_REVISION")
		return "", 0, false
	}
	revision, err := strconv.ParseInt(tag[1:len(tag)-1], 10, 64)
	if err != nil || revision < 0 || tag != `"`+strconv.FormatInt(revision, 10)+`"` {
		problem(w, 422, "INVALID_REVISION")
		return "", 0, false
	}
	key := r.Header.Get("Idempotency-Key")
	if len(r.Header.Values("Idempotency-Key")) != 1 || len(key) < 1 || len(key) > 128 || strings.TrimSpace(key) != key {
		problem(w, 422, "INVALID_REQUEST")
		return "", 0, false
	}
	return key, revision, true
}

func registerKnowledge(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool)) {
	service := knowledge.New(db.DB).WithSessionPolicy(auth.Sessions())
	actor := func(p identity.Principal) knowledge.Actor {
		return knowledge.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
	}
	mux.HandleFunc("GET /v1/sites/{site}/process-snapshots/{snapshot}/graph", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		for key, v := range values {
			switch key {
			case "root", "predicate":
			case "as_of", "hops", "max_entities", "max_claims", "direction":
				if len(v) != 1 {
					problem(w, 422, "INVALID_REQUEST")
					return
				}
			default:
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		query := knowledge.GraphQuery{Roots: values["root"], Predicates: values["predicate"], AsOf: values.Get("as_of"), Hops: 1, MaxEntities: 200, MaxClaims: 500, Direction: "outgoing"}
		if v, exists := values["direction"]; exists {
			query.Direction = v[0]
		}
		for key, target := range map[string]*int{"hops": &query.Hops, "max_entities": &query.MaxEntities, "max_claims": &query.MaxClaims} {
			if v, exists := values[key]; exists {
				*target, err = strconv.Atoi(v[0])
				if err != nil {
					problem(w, 422, "INVALID_REQUEST")
					return
				}
			}
		}
		if err = query.Validate(); err != nil {
			knowledgeError(w, err)
			return
		}
		body, err := service.ReadGraph(r.Context(), actor(p), knowledge.Scope{TenantID: tenant, SiteID: site}, r.PathValue("snapshot"), query)
		if err != nil {
			knowledgeError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})
	// GET .../route-summary (WIS-642): the ordered route of one process in one approved snapshot,
	// with exact totals computed in Go. process_id and as_of are each required exactly once.
	mux.HandleFunc("GET /v1/sites/{site}/process-snapshots/{snapshot}/route-summary", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(values) != 2 || len(values["process_id"]) != 1 || len(values["as_of"]) != 1 {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		result, err := service.ReadRoute(r.Context(), actor(p), knowledge.Scope{TenantID: tenant, SiteID: site}, r.PathValue("snapshot"), knowledge.RouteQuery{ProcessID: values.Get("process_id"), AsOf: values.Get("as_of")})
		if err != nil {
			knowledgeError(w, err)
			return
		}
		reply(w, 200, result)
	})
	// GET /knowledge resolves the snapshot in force at as_of; see
	// knowledge.Service.ReadKnowledge for selection and readiness semantics.
	// Each parameter appears at most once; entity_id is required; depth
	// defaults to 1 (the snapshot graph's hop default) and must be 0, 1 or 2.
	mux.HandleFunc("GET /v1/sites/{site}/knowledge", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		for key, v := range values {
			if (key != "entity_id" && key != "as_of" && key != "depth") || len(v) != 1 {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		depth := 1
		if v, exists := values["depth"]; exists {
			switch v[0] {
			case "0", "1", "2":
				depth = int(v[0][0] - '0')
			default:
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		if _, exists := values["entity_id"]; !exists {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		// A present but empty as_of is malformed, not "now".
		if v, exists := values["as_of"]; exists && v[0] == "" {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		result, err := service.ReadKnowledge(r.Context(), actor(p), knowledge.Scope{TenantID: tenant, SiteID: site}, values.Get("entity_id"), values.Get("as_of"), depth)
		if err != nil {
			knowledgeError(w, err)
			return
		}
		reply(w, 200, result)
	})
	mux.HandleFunc("GET /v1/sites/{site}/knowledge-inventory", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		result, err := service.ReadInventory(r.Context(), actor(p), knowledge.Scope{TenantID: tenant, SiteID: site})
		if err != nil {
			knowledgeError(w, err)
			return
		}
		reply(w, 200, result)
	})
	mutation := func(w http.ResponseWriter, r *http.Request) (identity.Principal, knowledge.Scope, string, int64, bool) {
		p, tenant, site, ok := scope(w, r)
		s := knowledge.Scope{TenantID: tenant, SiteID: site}
		if !ok {
			return p, s, "", 0, false
		}
		if err := auth.CheckCSRF(r, p); err != nil {
			problem(w, 403, "CSRF_REJECTED")
			return p, s, "", 0, false
		}
		key, revision, ok := mutationHeaders(w, r)
		return p, s, key, revision, ok
	}
	mux.HandleFunc("POST /v1/sites/{site}/document-freshness-policy-decisions", func(w http.ResponseWriter, r *http.Request) {
		p, s, key, revision, ok := mutation(w, r)
		if !ok {
			return
		}
		var input knowledge.FreshnessPolicyInput
		if !decodeEvidence(w, r, &input) {
			return
		}
		result, err := service.DecideFreshnessPolicy(r.Context(), actor(p), s, input, key, revision)
		if err != nil {
			knowledgeError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		reply(w, 200, result)
	})
	mux.HandleFunc("GET /v1/sites/{site}/document-freshness-policy", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		var revision int64
		for key, vals := range values {
			if key != "revision" || len(vals) != 1 {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
			revision, err = strconv.ParseInt(vals[0], 10, 64)
			if err != nil || revision < 1 || vals[0] != strconv.FormatInt(revision, 10) {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		result, err := service.ReadFreshnessPolicy(r.Context(), actor(p), knowledge.Scope{TenantID: tenant, SiteID: site}, revision)
		if err != nil {
			knowledgeError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		reply(w, 200, result)
	})
	mux.HandleFunc("GET /v1/sites/{site}/discovery-inventories/{inventory}/coverage", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if r.URL.RawQuery != "" {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		result, err := service.ReadCoverage(r.Context(), actor(p), knowledge.Scope{TenantID: tenant, SiteID: site}, r.PathValue("inventory"))
		if err != nil {
			knowledgeError(w, err)
			return
		}
		reply(w, 200, result)
	})
	mux.HandleFunc("GET /v1/sites/{site}/coverage", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if r.URL.RawQuery != "" {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		result, err := service.ReadSiteCoverage(r.Context(), actor(p), knowledge.Scope{TenantID: tenant, SiteID: site})
		if err != nil {
			knowledgeError(w, err)
			return
		}
		reply(w, 200, result)
	})
	mux.HandleFunc("POST /v1/sites/{site}/discovery-inventories/{inventory}/requirements/{requirement}/decisions", func(w http.ResponseWriter, r *http.Request) {
		p, s, key, revision, ok := mutation(w, r)
		if !ok {
			return
		}
		var input knowledge.RequirementDecisionInput
		if !decodeEvidence(w, r, &input) {
			return
		}
		result, err := service.DecideRequirement(r.Context(), actor(p), s, r.PathValue("inventory"), r.PathValue("requirement"), input, key, revision)
		if err != nil {
			knowledgeError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		reply(w, 200, result)
	})
	mux.HandleFunc("GET /v1/sites/{site}/discovery-inventories/{inventory}/requirements/{requirement}/decisions", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || !values.Has("inventory_revision") {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		var inventoryRevision, revision int64
		for key, vals := range values {
			if (key != "inventory_revision" && key != "revision") || len(vals) != 1 {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
			n, err := strconv.ParseInt(vals[0], 10, 64)
			if err != nil || n < 1 || vals[0] != strconv.FormatInt(n, 10) {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
			if key == "inventory_revision" {
				inventoryRevision = n
			} else {
				revision = n
			}
		}
		result, err := service.ReadRequirementDecision(r.Context(), actor(p), knowledge.Scope{TenantID: tenant, SiteID: site}, r.PathValue("inventory"), r.PathValue("requirement"), inventoryRevision, revision)
		if err != nil {
			knowledgeError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		reply(w, 200, result)
	})
	mux.HandleFunc("POST /v1/sites/{site}/discovery-inventories", func(w http.ResponseWriter, r *http.Request) {
		p, s, key, revision, ok := mutation(w, r)
		if !ok {
			return
		}
		var input knowledge.DiscoveryInput
		if !decodeEvidence(w, r, &input) {
			return
		}
		result, err := service.CreateDiscoveryInventory(r.Context(), actor(p), s, input, key, revision)
		if err != nil {
			knowledgeError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		reply(w, 200, result)
	})
	mux.HandleFunc("GET /v1/sites/{site}/discovery-inventories", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if !onlyQuery(w, r, "limit", "cursor", "process_id") {
			return
		}
		q := r.URL.Query()
		limit := 50
		if raw, exists := q["limit"]; exists {
			var err error
			limit, err = strconv.Atoi(raw[0])
			if err != nil || limit < 1 || limit > 100 {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		cursor, processID := q.Get("cursor"), q.Get("process_id")
		if cursor != "" {
			parsed, err := uuid.Parse(cursor)
			if err != nil || parsed.String() != cursor {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		if processID != "" {
			parsed, err := uuid.Parse(processID)
			if err != nil || parsed.String() != processID {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		result, err := service.ListDiscoveryInventories(r.Context(), actor(p), knowledge.Scope{TenantID: tenant, SiteID: site}, limit, cursor, processID)
		if err != nil {
			knowledgeError(w, err)
			return
		}
		reply(w, 200, result)
	})
	mux.HandleFunc("GET /v1/sites/{site}/discovery-inventories/{inventory}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		var revision int64
		for key, vals := range values {
			if key != "revision" || len(vals) != 1 {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
			revision, err = strconv.ParseInt(vals[0], 10, 64)
			if err != nil || revision < 1 || vals[0] != strconv.FormatInt(revision, 10) {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		result, err := service.ReadDiscoveryInventory(r.Context(), actor(p), knowledge.Scope{TenantID: tenant, SiteID: site}, r.PathValue("inventory"), revision)
		if err != nil {
			knowledgeError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		reply(w, 200, result)
	})
	mux.HandleFunc("POST /v1/sites/{site}/process-snapshots", func(w http.ResponseWriter, r *http.Request) {
		p, s, key, revision, ok := mutation(w, r)
		if !ok {
			return
		}
		var input knowledge.SnapshotInput
		if !decodeEvidence(w, r, &input) {
			return
		}
		result, err := service.CreateSnapshot(r.Context(), actor(p), s, input, key, revision)
		if err != nil {
			knowledgeError(w, err)
			return
		}
		reply(w, 200, result)
	})
	mux.HandleFunc("GET /v1/sites/{site}/process-snapshots/{snapshot}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		result, err := service.ReadSnapshot(r.Context(), actor(p), knowledge.Scope{TenantID: tenant, SiteID: site}, r.PathValue("snapshot"))
		if err != nil {
			knowledgeError(w, err)
			return
		}
		reply(w, 200, result)
	})
	mux.HandleFunc("POST /v1/sites/{site}/entities", func(w http.ResponseWriter, r *http.Request) {
		p, s, key, revision, ok := mutation(w, r)
		if !ok {
			return
		}
		var input knowledge.EntityInput
		if !decodeEvidence(w, r, &input) {
			return
		}
		result, err := service.CreateEntity(r.Context(), actor(p), s, input, key, revision)
		if err != nil {
			knowledgeError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		reply(w, 200, result)
	})
	// WIS-270: a reviewer merges {entity} into into_entity_id by recording an immutable redirect.
	mux.HandleFunc("POST /v1/sites/{site}/entities/{entity}/merges", func(w http.ResponseWriter, r *http.Request) {
		p, s, key, revision, ok := mutation(w, r)
		if !ok {
			return
		}
		var input knowledge.MergeInput
		if !decodeEvidence(w, r, &input) {
			return
		}
		result, err := service.MergeEntity(r.Context(), actor(p), s, r.PathValue("entity"), input, key, revision)
		if err != nil {
			knowledgeError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.FromEntityRevision, 10)+`"`)
		reply(w, 200, result)
	})
	mux.HandleFunc("GET /v1/sites/{site}/entities/{entity}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if !onlyQuery(w, r, "revision") {
			return
		}
		var revision int64
		if raw := r.URL.Query().Get("revision"); raw != "" {
			var err error
			revision, err = strconv.ParseInt(raw, 10, 64)
			if err != nil || revision < 1 {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		result, err := service.ReadEntity(r.Context(), actor(p), knowledge.Scope{TenantID: tenant, SiteID: site}, r.PathValue("entity"), revision)
		if err != nil {
			knowledgeError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		reply(w, 200, result)
	})
	mux.HandleFunc("POST /v1/sites/{site}/claims", func(w http.ResponseWriter, r *http.Request) {
		p, s, key, revision, ok := mutation(w, r)
		if !ok {
			return
		}
		var input knowledge.ClaimInput
		if !decodeEvidence(w, r, &input) {
			return
		}
		if err := input.ValidateEntityRevisions(); err != nil {
			knowledgeError(w, err)
			return
		}
		// Extractor identity must come from a trusted server activity, never a client assertion.
		if input.Extractor != nil {
			problem(w, 422, "INVALID_CLAIM")
			return
		}
		result, err := service.CreateClaim(r.Context(), actor(p), s, input, key, revision)
		if err != nil {
			knowledgeError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		reply(w, 200, result)
	})
	mux.HandleFunc("POST /v1/sites/{site}/conflicts/{conflict}/resolutions", func(w http.ResponseWriter, r *http.Request) {
		p, s, key, revision, ok := mutation(w, r)
		if !ok {
			return
		}
		var input knowledge.ResolutionInput
		if !decodeEvidence(w, r, &input) {
			return
		}
		result, err := service.DecideResolution(r.Context(), actor(p), s, r.PathValue("conflict"), input, key, revision)
		if err != nil {
			knowledgeError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		reply(w, 200, result)
	})
	mux.HandleFunc("GET /v1/sites/{site}/conflicts/{conflict}/resolutions", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if !onlyQuery(w, r, "revision") {
			return
		}
		var revision int64
		if raw := r.URL.Query().Get("revision"); raw != "" {
			var err error
			revision, err = strconv.ParseInt(raw, 10, 64)
			if err != nil || revision < 1 {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		result, err := service.ReadResolution(r.Context(), actor(p), knowledge.Scope{TenantID: tenant, SiteID: site}, r.PathValue("conflict"), revision)
		if err != nil {
			knowledgeError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		reply(w, 200, result)
	})
	mux.HandleFunc("POST /v1/sites/{site}/claims/{claim}/reviews", func(w http.ResponseWriter, r *http.Request) {
		p, s, key, revision, ok := mutation(w, r)
		if !ok {
			return
		}
		var input knowledge.ReviewInput
		if !decodeEvidence(w, r, &input) {
			return
		}
		result, err := service.DecideReview(r.Context(), actor(p), s, r.PathValue("claim"), input, key, revision)
		if err != nil {
			knowledgeError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.ClaimRevision, 10)+`"`)
		reply(w, 200, result)
	})
	mux.HandleFunc("POST /v1/sites/{site}/claims/{claim}/review-revocations", func(w http.ResponseWriter, r *http.Request) {
		p, s, key, revision, ok := mutation(w, r)
		if !ok {
			return
		}
		var input knowledge.RevocationInput
		if !decodeEvidence(w, r, &input) {
			return
		}
		result, err := service.RevokeReview(r.Context(), actor(p), s, r.PathValue("claim"), input, key, revision)
		if err != nil {
			knowledgeError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.ClaimRevision, 10)+`"`)
		reply(w, 200, result)
	})
	mux.HandleFunc("GET /v1/sites/{site}/claims/{claim}/review", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if !onlyQuery(w, r, "revision") {
			return
		}
		var revision int64
		if raw := r.URL.Query().Get("revision"); raw != "" {
			var err error
			revision, err = strconv.ParseInt(raw, 10, 64)
			if err != nil || revision < 1 {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		result, err := service.ReadReview(r.Context(), actor(p), knowledge.Scope{TenantID: tenant, SiteID: site}, r.PathValue("claim"), revision)
		if err != nil {
			knowledgeError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.ClaimRevision, 10)+`"`)
		reply(w, 200, result)
	})
	for _, resource := range []string{"entities", "claims"} {
		mux.HandleFunc("GET /v1/sites/{site}/"+resource, func(w http.ResponseWriter, r *http.Request) {
			p, tenant, site, ok := scope(w, r)
			if !ok {
				return
			}
			allowed := []string{"limit", "cursor"}
			if resource == "claims" {
				allowed = append(allowed, "subject_id", "state")
			}
			if !onlyQuery(w, r, allowed...) {
				return
			}
			q := r.URL.Query()
			limit := 50
			if raw := q.Get("limit"); raw != "" {
				var err error
				limit, err = strconv.Atoi(raw)
				if err != nil || limit < 1 || limit > 100 {
					problem(w, 422, "INVALID_REQUEST")
					return
				}
			}
			s := knowledge.Scope{TenantID: tenant, SiteID: site}
			if resource == "entities" {
				result, err := service.ListEntities(r.Context(), actor(p), s, limit, q.Get("cursor"))
				if err != nil {
					knowledgeError(w, err)
					return
				}
				reply(w, 200, result)
				return
			}
			result, err := service.ListClaimsByState(r.Context(), actor(p), s, limit, q.Get("cursor"), q.Get("subject_id"), q.Get("state"))
			if err != nil {
				knowledgeError(w, err)
				return
			}
			reply(w, 200, result)
		})
	}
	mux.HandleFunc("GET /v1/sites/{site}/claims/{claim}/conflicts", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if !onlyQuery(w, r, "revision", "limit", "cursor") {
			return
		}
		q := r.URL.Query()
		var revision int64
		if raw := q.Get("revision"); raw != "" {
			var err error
			revision, err = strconv.ParseInt(raw, 10, 64)
			if err != nil || revision < 1 {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		limit := 50
		if raw := q.Get("limit"); raw != "" {
			var err error
			limit, err = strconv.Atoi(raw)
			if err != nil || limit < 1 || limit > 100 {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		result, err := service.ListConflicts(r.Context(), actor(p), knowledge.Scope{TenantID: tenant, SiteID: site}, r.PathValue("claim"), revision, limit, q.Get("cursor"))
		if err != nil {
			knowledgeError(w, err)
			return
		}
		reply(w, 200, result)
	})
	mux.HandleFunc("GET /v1/sites/{site}/claims/{claim}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if !onlyQuery(w, r, "revision") {
			return
		}
		var revision int64
		if raw := r.URL.Query().Get("revision"); raw != "" {
			var err error
			revision, err = strconv.ParseInt(raw, 10, 64)
			if err != nil || revision < 1 {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		result, err := service.ReadClaim(r.Context(), actor(p), knowledge.Scope{TenantID: tenant, SiteID: site}, r.PathValue("claim"), revision)
		if err != nil {
			knowledgeError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		reply(w, 200, result)
	})
}

func knowledgeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, knowledge.ErrFreshnessBlocked):
		problem(w, 409, "FRESHNESS_BLOCKED")
	case errors.Is(err, knowledge.ErrBindingUnsupported):
		problem(w, 422, "BINDING_UNSUPPORTED")
	case errors.Is(err, knowledge.ErrBindingBlocked):
		problem(w, 409, "BINDING_BLOCKED")
	case errors.Is(err, knowledge.ErrDiscoveryConflict):
		problem(w, 409, "DISCOVERY_CONFLICT")
	case errors.Is(err, knowledge.ErrSnapshotStale):
		problem(w, 409, "SNAPSHOT_STALE")
	case errors.Is(err, knowledge.ErrSnapshotConflict):
		problem(w, 409, "SNAPSHOT_CONFLICT")
	case errors.Is(err, knowledge.ErrResolutionBlocked):
		problem(w, 409, "RESOLUTION_BLOCKED")
	case errors.Is(err, knowledge.ErrReviewConflict):
		problem(w, 409, "REVIEW_CONFLICT")
	case errors.Is(err, knowledge.ErrReviewBlocked):
		problem(w, 409, "REVIEW_BLOCKED")
	case errors.Is(err, knowledge.ErrReauthenticationRequired):
		problem(w, 428, "REAUTHENTICATION_REQUIRED")
	case errors.Is(err, knowledge.ErrNotFound):
		problem(w, 404, "NOT_FOUND")
	case errors.Is(err, knowledge.ErrDenied):
		problem(w, 403, "PERMISSION_DENIED")
	case errors.Is(err, knowledge.ErrConflict):
		problem(w, 409, "IDEMPOTENCY_CONFLICT")
	case errors.Is(err, knowledge.ErrRevision):
		problem(w, 412, "REVISION_CONFLICT")
	case errors.Is(err, knowledge.ErrSourceChanged):
		problem(w, 409, "SOURCE_CHANGED")
	case errors.Is(err, knowledge.ErrCycle):
		problem(w, 409, "KNOWLEDGE_CYCLE")
	// A deterministic size limit never succeeds on retry, so it is not reported as
	// unavailability (503).
	case errors.Is(err, knowledge.ErrTraversalLimit):
		problem(w, 413, "LIMIT_EXCEEDED")
	case errors.Is(err, knowledge.ErrInvalidContract):
		problem(w, 422, "INVALID_CLAIM")
	default:
		problem(w, 503, "STORAGE_UNAVAILABLE")
	}
}
