package httpapi

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plantcontext"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
)

func registerContexts(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool)) {
	service := plantcontext.New(db.DB).WithSessionPolicy(auth.Sessions())
	actor := func(p identity.Principal) plantcontext.Actor {
		return plantcontext.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
	}
	base := "/v1/sites/{site}/contexts"
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
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			problem(w, 415, "UNSUPPORTED_MEDIA")
			return
		}
		if r.URL.RawQuery != "" {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			var sizeErr *http.MaxBytesError
			if errors.As(err, &sizeErr) {
				problem(w, 413, "BODY_TOO_LARGE")
				return
			}
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		var input plantcontext.Request
		if strictjson.Decode(data, &input, 64<<10) != nil {
			problem(w, 422, "INVALID_CONTEXT")
			return
		}
		result, err := service.Create(r.Context(), actor(p), plantcontext.Scope{TenantID: tenant, SiteID: site}, input, key)
		if err != nil {
			contextError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		reply(w, 201, result)
	})
	mux.HandleFunc("GET "+base+"/{context}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if r.URL.RawQuery != "" {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		result, err := service.Get(r.Context(), actor(p), plantcontext.Scope{TenantID: tenant, SiteID: site}, r.PathValue("context"))
		if err != nil {
			contextError(w, err)
			return
		}
		// current_status depends on live authority and later records.
		w.Header().Set("Cache-Control", "no-store")
		reply(w, 200, result)
	})
}

func contextError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, plantcontext.ErrNotFound):
		problem(w, 404, "NOT_FOUND")
	case errors.Is(err, plantcontext.ErrDenied):
		problem(w, 403, "PERMISSION_DENIED")
	case errors.Is(err, plantcontext.ErrConflict):
		problem(w, 409, "IDEMPOTENCY_CONFLICT")
	case errors.Is(err, plantcontext.ErrWindowTooLarge):
		problem(w, 422, "WINDOW_TOO_LARGE")
	case errors.Is(err, plantcontext.ErrUnknownEntity):
		problem(w, 422, "UNKNOWN_ENTITY")
	case errors.Is(err, plantcontext.ErrUnknownSnapshot):
		problem(w, 422, "UNKNOWN_PROCESS_SNAPSHOT")
	case errors.Is(err, plantcontext.ErrInvalid):
		problem(w, 422, "INVALID_CONTEXT")
	default:
		problem(w, 503, "STORAGE_UNAVAILABLE")
	}
}
