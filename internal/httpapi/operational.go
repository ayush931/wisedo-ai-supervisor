package httpapi

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/operational"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
	"github.com/google/uuid"
)

func registerOperational(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool)) {
	service := operational.New(db.DB).WithSessionPolicy(auth.Sessions())
	actor := func(p identity.Principal) operational.Actor {
		return operational.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
	}
	mutation := func(w http.ResponseWriter, r *http.Request) (identity.Principal, operational.Scope, string, int64, bool) {
		p, tenant, site, ok := scope(w, r)
		s := operational.Scope{TenantID: tenant, SiteID: site}
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
	decode := func(w http.ResponseWriter, r *http.Request, dst any) bool {
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			problem(w, 415, "UNSUPPORTED_MEDIA")
			return false
		}
		// Boundary already bounds bodies; strictjson rejects duplicate keys,
		// unknown fields, invalid UTF-8 and trailing values at this trust edge.
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limits.RequestBodyBytes))
		if err != nil || strictjson.Decode(data, dst, limits.RequestBodyBytes) != nil {
			problem(w, 422, "INVALID_REQUEST")
			return false
		}
		return true
	}
	base := "/v1/sites/{site}/operational-sources"
	mux.HandleFunc("POST "+base, func(w http.ResponseWriter, r *http.Request) {
		p, s, key, expected, ok := mutation(w, r)
		if !ok {
			return
		}
		var input operational.Config
		if !decode(w, r, &input) {
			return
		}
		result, err := service.CreateOrRevise(r.Context(), actor(p), s, input, key, expected)
		if err != nil {
			operationalError(w, err)
			return
		}
		result.CredentialRef = "" // Never disclose secret-store paths through the site API.
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		w.Header().Set("X-Mapping-ETag", `"`+strconv.FormatInt(result.MappingRevision, 10)+`"`)
		reply(w, 200, result)
	})
	// GET /v1/sites/{site}/operational-sources lists the site's sources by source_id with a
	// stable cursor (default 50, max 100). Any active member may read; scope failures are the
	// same non-disclosing 404 as the single-source read.
	mux.HandleFunc("GET "+base, func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r, "limit", "cursor") {
			return
		}
		limit := 50
		if raw := r.URL.Query().Get("limit"); raw != "" {
			var err error
			limit, err = strconv.Atoi(raw)
			if err != nil || limit < 1 || limit > 100 || raw != strconv.Itoa(limit) {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		cursor := r.URL.Query().Get("cursor")
		if parsed, err := uuid.Parse(cursor); cursor != "" && (err != nil || parsed.String() != cursor) {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		page, err := service.List(r.Context(), actor(p), operational.Scope{TenantID: tenant, SiteID: site}, limit, cursor)
		if err != nil {
			operationalError(w, err)
			return
		}
		reply(w, 200, page)
	})
	mux.HandleFunc("GET "+base+"/{source}", func(w http.ResponseWriter, r *http.Request) {
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
		for key, values := range values {
			if key != "revision" || len(values) != 1 {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
			revision, err = strconv.ParseInt(values[0], 10, 64)
			if err != nil || revision < 1 || values[0] != strconv.FormatInt(revision, 10) {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		result, mappings, err := service.Get(r.Context(), actor(p), operational.Scope{TenantID: tenant, SiteID: site}, r.PathValue("source"), revision)
		if err != nil {
			operationalError(w, err)
			return
		}
		result.CredentialRef = ""
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		w.Header().Set("X-Mapping-ETag", `"`+strconv.FormatInt(result.MappingRevision, 10)+`"`)
		reply(w, 200, operational.Detail{Source: result, Mappings: mappings})
	})
	mux.HandleFunc("POST "+base+"/{source}/mappings", func(w http.ResponseWriter, r *http.Request) {
		p, s, key, expected, ok := mutation(w, r)
		if !ok {
			return
		}
		var input operational.MappingsInput
		if !decode(w, r, &input) {
			return
		}
		result, err := service.ReviseMappings(r.Context(), actor(p), s, r.PathValue("source"), input.Mappings, key, expected)
		if err != nil {
			operationalError(w, err)
			return
		}
		result.CredentialRef = ""
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.MappingRevision, 10)+`"`)
		w.Header().Set("X-Source-ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		reply(w, 200, result)
	})
	mux.HandleFunc("POST "+base+"/{source}/reviews", func(w http.ResponseWriter, r *http.Request) {
		p, s, key, expected, ok := mutation(w, r)
		if !ok {
			return
		}
		var input operational.ReviewInput
		if !decode(w, r, &input) {
			return
		}
		result, err := service.Decide(r.Context(), actor(p), s, r.PathValue("source"), input, key, expected)
		if err != nil {
			operationalError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.SourceRevision, 10)+`"`)
		w.Header().Set("X-Review-ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		reply(w, 200, result)
	})
	// WIS-272: POST /v1/sites/{site_id}/operational-sources/{id}/revoke. The request carries
	// no body; If-Match is the current source revision and Idempotency-Key is required.
	mux.HandleFunc("POST "+base+"/{source}/revoke", func(w http.ResponseWriter, r *http.Request) {
		p, s, key, expected, ok := mutation(w, r)
		if !ok {
			return
		}
		if r.URL.RawQuery != "" {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		if extra, err := io.ReadAll(io.LimitReader(r.Body, 1)); err != nil || len(extra) != 0 {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		result, err := service.Revoke(r.Context(), actor(p), s, r.PathValue("source"), key, expected)
		if err != nil {
			operationalError(w, err)
			return
		}
		result.CredentialRef = "" // Never disclose secret-store paths through the site API.
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		w.Header().Set("X-Mapping-ETag", `"`+strconv.FormatInt(result.MappingRevision, 10)+`"`)
		reply(w, 200, result)
	})
	// WIS-270: GET /v1/sites/{site_id}/external-identities/resolve resolves an external key only
	// through an exact, currently approved mapping. Every non-resolvable case is the same 409.
	mux.HandleFunc("GET /v1/sites/{site}/external-identities/resolve", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(values) != 3 {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		param := map[string]string{}
		for _, name := range []string{"source_id", "entity_type", "external_key"} {
			if len(values[name]) != 1 {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
			param[name] = values[name][0]
		}
		if !operational.ValidIdentityQuery(param["source_id"], param["entity_type"], param["external_key"]) {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		result, err := service.Resolve(r.Context(), actor(p), operational.Scope{TenantID: tenant, SiteID: site}, param["source_id"], param["entity_type"], param["external_key"])
		if err != nil {
			operationalError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store") // approval can be revoked at any moment
		reply(w, 200, result)
	})
}

// operationalError maps service errors to problems. A *operational.RowError keeps its
// sentinel's status and code and adds the member path (mappings[i].field, index only).
func operationalError(w http.ResponseWriter, err error) {
	path := ""
	var row *operational.RowError
	if errors.As(err, &row) {
		path = row.Path
	}
	switch {
	case errors.Is(err, operational.ErrNotFound):
		problemAt(w, 404, "NOT_FOUND", path)
	case errors.Is(err, operational.ErrDenied):
		problem(w, 403, "PERMISSION_DENIED")
	case errors.Is(err, operational.ErrReauthentication):
		problem(w, 428, "REAUTHENTICATION_REQUIRED")
	case errors.Is(err, operational.ErrConflict):
		problem(w, 409, "IDEMPOTENCY_CONFLICT")
	case errors.Is(err, operational.ErrUnknownMapping):
		problemAt(w, 409, "UNKNOWN_MAPPING", path)
	case errors.Is(err, operational.ErrRevision):
		problemAt(w, 412, "REVISION_CONFLICT", path)
	case errors.Is(err, operational.ErrAccessPolicyRegression):
		problem(w, 422, "ACCESS_POLICY_REGRESSION")
	case errors.Is(err, operational.ErrInvalid):
		problemAt(w, 422, "INVALID_SOURCE", path)
	default:
		problem(w, 503, "STORAGE_UNAVAILABLE")
	}
}
