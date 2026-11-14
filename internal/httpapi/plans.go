package httpapi

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plans"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
)

func registerPlans(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool)) {
	service := plans.New(db.DB).WithSessionPolicy(auth.Sessions())
	actor := func(p identity.Principal) plans.Actor {
		return plans.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
	}
	decode := func(w http.ResponseWriter, r *http.Request, dst any) bool {
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			problem(w, 415, "UNSUPPORTED_MEDIA")
			return false
		}
		if r.URL.RawQuery != "" {
			problem(w, 422, "INVALID_REQUEST")
			return false
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limits.RequestBodyBytes))
		if err != nil {
			var sizeErr *http.MaxBytesError
			if errors.As(err, &sizeErr) {
				problem(w, 413, "BODY_TOO_LARGE")
				return false
			}
			problem(w, 422, "INVALID_REQUEST")
			return false
		}
		// strictjson rejects duplicate keys, unknown fields (including a smuggled
		// approval_ref), numeric timestamps, invalid UTF-8 and trailing values.
		if strictjson.Decode(data, dst, limits.RequestBodyBytes) != nil {
			problem(w, 422, "INVALID_PLAN")
			return false
		}
		return true
	}
	etag := func(w http.ResponseWriter, revision int64) {
		w.Header().Set("ETag", `"`+strconv.FormatInt(revision, 10)+`"`)
	}
	base := "/v1/sites/{site}/plan-revisions"
	mux.HandleFunc("POST "+base, func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if err := auth.CheckCSRF(r, p); err != nil {
			problem(w, 403, "CSRF_REJECTED")
			return
		}
		key := r.Header.Get("Idempotency-Key")
		if len(r.Header.Values("Idempotency-Key")) != 1 || len(key) < 1 || len(key) > 128 || strings.TrimSpace(key) != key {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		var input plans.CaptureInput
		if !decode(w, r, &input) {
			return
		}
		result, err := service.Capture(r.Context(), actor(p), plans.Scope{TenantID: tenant, SiteID: site}, input, key)
		if err != nil {
			planError(w, err)
			return
		}
		etag(w, result.ReviewRevision)
		reply(w, 201, result)
	})
	mux.HandleFunc("GET "+base+"/{plan}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if r.URL.RawQuery != "" {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		result, err := service.Get(r.Context(), actor(p), plans.Scope{TenantID: tenant, SiteID: site}, r.PathValue("plan"))
		if err != nil {
			planError(w, err)
			return
		}
		// Approval eligibility depends on live revocation state.
		w.Header().Set("Cache-Control", "no-store")
		etag(w, result.ReviewRevision)
		reply(w, 200, result)
	})
	mux.HandleFunc("POST "+base+"/{plan}/reviews", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if err := auth.CheckCSRF(r, p); err != nil {
			problem(w, 403, "CSRF_REJECTED")
			return
		}
		key, expected, ok := mutationHeaders(w, r)
		if !ok {
			return
		}
		var input plans.ReviewInput
		if !decode(w, r, &input) {
			return
		}
		result, err := service.Decide(r.Context(), actor(p), plans.Scope{TenantID: tenant, SiteID: site}, r.PathValue("plan"), input, key, expected)
		if err != nil {
			planError(w, err)
			return
		}
		etag(w, result.ReviewRevision)
		reply(w, 200, result)
	})
}

func planError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, plans.ErrNotFound):
		problem(w, 404, "NOT_FOUND")
	case errors.Is(err, plans.ErrSelfReview):
		problem(w, 403, "SELF_REVIEW_FORBIDDEN")
	case errors.Is(err, plans.ErrDenied):
		problem(w, 403, "PERMISSION_DENIED")
	case errors.Is(err, plans.ErrReauthentication):
		problem(w, 428, "REAUTHENTICATION_REQUIRED")
	case errors.Is(err, plans.ErrConflict):
		problem(w, 409, "IDEMPOTENCY_CONFLICT")
	case errors.Is(err, plans.ErrPlanConflict):
		problem(w, 409, "PLAN_REVISION_CONFLICT")
	case errors.Is(err, plans.ErrPlanGap):
		problem(w, 409, "PLAN_REVISION_GAP")
	case errors.Is(err, plans.ErrSuperseded):
		problem(w, 409, "PLAN_SUPERSEDED")
	case errors.Is(err, plans.ErrSpanUnavailable):
		problem(w, 409, "SOURCE_SPAN_UNAVAILABLE")
	case errors.Is(err, plans.ErrRevision):
		problem(w, 412, "REVISION_CONFLICT")
	case errors.Is(err, plans.ErrUnknownEntity):
		problem(w, 422, "UNKNOWN_ENTITY")
	case errors.Is(err, plans.ErrUnknownSpan):
		problem(w, 422, "UNKNOWN_SOURCE_SPAN")
	case errors.Is(err, plans.ErrInvalid):
		problem(w, 422, "INVALID_PLAN")
	default:
		problem(w, 503, "STORAGE_UNAVAILABLE")
	}
}
