package httpapi

import (
	"errors"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
)

func registerEvidence(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool)) {
	service := evidence.New(db.DB).WithSessionPolicy(auth.Sessions())
	actor := func(p identity.Principal) evidence.Actor {
		return evidence.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
	}
	mutation := func(w http.ResponseWriter, r *http.Request) (identity.Principal, evidence.Scope, string, int64, bool) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return p, evidence.Scope{}, "", 0, false
		}
		s := evidence.Scope{TenantID: tenant, SiteID: site}
		if err := auth.CheckCSRF(r, p); err != nil {
			problem(w, 403, "CSRF_REJECTED")
			return p, s, "", 0, false
		}
		key, revision, ok := mutationHeaders(w, r)
		return p, s, key, revision, ok
	}
	mux.HandleFunc("POST /v1/sites/{site}/sources", func(w http.ResponseWriter, r *http.Request) {
		p, s, key, revision, ok := mutation(w, r)
		if !ok {
			return
		}
		var body struct {
			SourceID string `json:"source_id"`
			Kind     string `json:"kind"`
			Title    string `json:"title"`
			Text     string `json:"text"`
			// Optional WIS-270 fields; tenant/site never come from the body.
			AuthorityClass      string `json:"authority_class"`
			IndependenceGroupID string `json:"independence_group_id"`
		}
		if !decodeEvidence(w, r, &body) {
			return
		}
		result, err := service.CreateOrRevise(r.Context(), actor(p), s, evidence.Input{SourceID: body.SourceID, Kind: body.Kind, Title: body.Title, Text: body.Text, AuthorityClass: body.AuthorityClass, IndependenceGroupID: body.IndependenceGroupID}, key, revision)
		if err != nil {
			evidenceError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.StateRevision, 10)+`"`)
		reply(w, 200, result)
	})
	mux.HandleFunc("GET /v1/sites/{site}/sources", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if !onlyQuery(w, r, "limit", "cursor") {
			return
		}
		limit := 50
		if raw := r.URL.Query().Get("limit"); raw != "" {
			var err error
			limit, err = strconv.Atoi(raw)
			if err != nil || limit < 1 || limit > 100 {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		result, err := service.List(r.Context(), actor(p), evidence.Scope{TenantID: tenant, SiteID: site}, limit, r.URL.Query().Get("cursor"))
		if err != nil {
			evidenceError(w, err)
			return
		}
		reply(w, 200, result)
	})
	mux.HandleFunc("GET /v1/sites/{site}/sources/{source}", func(w http.ResponseWriter, r *http.Request) {
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
		result, err := service.Read(r.Context(), actor(p), evidence.Scope{TenantID: tenant, SiteID: site}, r.PathValue("source"), revision)
		if err != nil {
			evidenceError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.StateRevision, 10)+`"`)
		reply(w, 200, result)
	})
	mux.HandleFunc("POST /v1/sites/{site}/sources/{source}/spans", func(w http.ResponseWriter, r *http.Request) {
		p, s, key, expected, ok := mutation(w, r)
		if !ok {
			return
		}
		var body struct {
			Revision *int64 `json:"revision"`
			Start    *int   `json:"start_char"`
			End      *int   `json:"end_char"`
		}
		if !decodeEvidence(w, r, &body) {
			return
		}
		if body.Revision == nil || body.Start == nil || body.End == nil {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		result, err := service.CreateSpan(r.Context(), actor(p), s, evidence.SpanInput{SourceID: r.PathValue("source"), Revision: *body.Revision, Start: *body.Start, End: *body.End}, key, expected)
		if err != nil {
			evidenceError(w, err)
			return
		}
		reply(w, 200, result)
	})
	mux.HandleFunc("GET /v1/sites/{site}/source-spans/{span}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		result, err := service.ReadSpan(r.Context(), actor(p), evidence.Scope{TenantID: tenant, SiteID: site}, r.PathValue("span"))
		if err != nil {
			evidenceError(w, err)
			return
		}
		reply(w, 200, result)
	})
	mux.HandleFunc("POST /v1/sites/{site}/sources/{source}/revocations", func(w http.ResponseWriter, r *http.Request) {
		p, s, key, expected, ok := mutation(w, r)
		if !ok {
			return
		}
		if err := auth.RequireRecent(p); err != nil {
			problem(w, 428, "REAUTHENTICATION_REQUIRED")
			return
		}
		var body struct {
			Reason string `json:"reason"`
		}
		if !decodeEvidence(w, r, &body) {
			return
		}
		result, err := service.Revoke(r.Context(), actor(p), s, r.PathValue("source"), body.Reason, key, expected)
		if err != nil {
			evidenceError(w, err)
			return
		}
		reply(w, 200, result)
	})
	mux.HandleFunc("POST /v1/sites/{site}/sources/{source}/currentness-decisions", func(w http.ResponseWriter, r *http.Request) {
		p, s, key, expected, ok := mutation(w, r)
		if !ok {
			return
		}
		if err := auth.RequireRecent(p); err != nil {
			problem(w, 428, "REAUTHENTICATION_REQUIRED")
			return
		}
		var input evidence.CurrentnessInput
		if !decodeEvidence(w, r, &input) {
			return
		}
		result, err := service.DecideCurrentness(r.Context(), actor(p), s, r.PathValue("source"), input, key, expected)
		if err != nil {
			evidenceError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		reply(w, 200, result)
	})
	mux.HandleFunc("GET /v1/sites/{site}/sources/{source}/currentness", func(w http.ResponseWriter, r *http.Request) {
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
		result, err := service.ReadCurrentness(r.Context(), actor(p), evidence.Scope{TenantID: tenant, SiteID: site}, r.PathValue("source"), revision)
		if err != nil {
			evidenceError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		reply(w, 200, result)
	})
	mux.HandleFunc("POST /v1/sites/{site}/sources/{source}/authority-decisions", func(w http.ResponseWriter, r *http.Request) {
		p, s, key, expected, ok := mutation(w, r)
		if !ok {
			return
		}
		if err := auth.RequireRecent(p); err != nil {
			problem(w, 428, "REAUTHENTICATION_REQUIRED")
			return
		}
		var input evidence.AuthorityInput
		if !decodeEvidence(w, r, &input) {
			return
		}
		result, err := service.DecideAuthority(r.Context(), actor(p), s, r.PathValue("source"), input, key, expected)
		if err != nil {
			evidenceError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		reply(w, 200, result)
	})
	mux.HandleFunc("GET /v1/sites/{site}/sources/{source}/authority", func(w http.ResponseWriter, r *http.Request) {
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
		result, err := service.ReadAuthority(r.Context(), actor(p), evidence.Scope{TenantID: tenant, SiteID: site}, r.PathValue("source"), revision)
		if err != nil {
			evidenceError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		reply(w, 200, result)
	})
}

func decodeEvidence(w http.ResponseWriter, r *http.Request, value any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		problem(w, 415, "UNSUPPORTED_MEDIA")
		return false
	}
	body, err := io.ReadAll(r.Body)
	if err != nil || !validJSONUnicode(body) {
		problem(w, 422, "INVALID_SOURCE")
		return false
	}
	if strictjson.Decode(body, value, limits.RequestBodyBytes) != nil {
		problem(w, 422, "INVALID_REQUEST")
		return false
	}
	return true
}

// encoding/json replaces malformed Unicode with U+FFFD. Evidence intake must
// reject those bytes instead of silently changing the source before hashing.
func validJSONUnicode(body []byte) bool {
	if !utf8.Valid(body) {
		return false
	}
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			continue
		}
		i++
		if i >= len(body) {
			return false
		}
		if body[i] != 'u' {
			continue
		}
		if i+4 >= len(body) {
			return false
		}
		value, err := strconv.ParseUint(string(body[i+1:i+5]), 16, 16)
		if err != nil || value >= 0xdc00 && value <= 0xdfff {
			return false
		}
		if value >= 0xd800 && value <= 0xdbff {
			if i+10 >= len(body) || body[i+5] != '\\' || body[i+6] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(body[i+7:i+11]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 10
		} else {
			i += 4
		}
	}
	return true
}
func evidenceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, evidence.ErrSourceChanged):
		problem(w, 409, "SOURCE_CHANGED")
	case errors.Is(err, evidence.ErrModelExtractedUnconfirmed):
		problem(w, 409, "MODEL_EXTRACTED_UNCONFIRMED")
	case errors.Is(err, evidence.ErrNotFound):
		problem(w, 404, "NOT_FOUND")
	case errors.Is(err, evidence.ErrDenied):
		problem(w, 403, "PERMISSION_DENIED")
	case errors.Is(err, evidence.ErrReauthentication):
		problem(w, 428, "REAUTHENTICATION_REQUIRED")
	case errors.Is(err, evidence.ErrConflict):
		problem(w, 409, "IDEMPOTENCY_CONFLICT")
	case errors.Is(err, evidence.ErrRevision):
		problem(w, 412, "REVISION_CONFLICT")
	case errors.Is(err, evidence.ErrInvalid):
		problem(w, 422, "INVALID_SOURCE")
	default:
		problem(w, 503, "EVIDENCE_UNAVAILABLE")
	}
}
