package httpapi

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/roster"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
)

// registerMaintenanceRoster serves the WIS-276 supervisor-confirmed maintenance assignee roster.
// A recently authenticated site supervisor confirms or withdraws an active operator or
// supervisor for a line; owners and supervisors read it.
func registerMaintenanceRoster(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool)) {
	service := roster.New(db.DB).WithSessionPolicy(auth.Sessions())
	principal := func(p identity.Principal) roster.Principal {
		return roster.Principal{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
	}
	base := "/v1/sites/{site}/maintenance-roster"
	mux.HandleFunc("GET "+base, func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r, "line_id", "limit", "cursor") {
			return
		}
		limit := roster.DefaultPageSize
		if raw := r.URL.Query().Get("limit"); raw != "" {
			var err error
			if limit, err = strconv.Atoi(raw); err != nil || limit < 1 || limit > roster.MaxPageSize {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		q := r.URL.Query()
		page, err := service.List(r.Context(), principal(p), roster.Scope{TenantID: tenant, SiteID: site}, q.Get("line_id"), q.Get("cursor"), limit)
		if err != nil {
			rosterError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		reply(w, 200, page)
	})
	mux.HandleFunc("POST "+base+"/decisions", func(w http.ResponseWriter, r *http.Request) {
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
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			problem(w, 415, "UNSUPPORTED_MEDIA")
			return
		}
		if r.URL.RawQuery != "" {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limits.RequestBodyBytes))
		if err != nil {
			var sizeErr *http.MaxBytesError
			if errors.As(err, &sizeErr) {
				problem(w, 413, "BODY_TOO_LARGE")
				return
			}
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		// Unknown fields are refused, so a forged decider, tenant or confirmed field is a 422.
		var in roster.DecisionInput
		if strictjson.Decode(data, &in, limits.RequestBodyBytes) != nil {
			problem(w, 422, "INVALID_ROSTER_DECISION")
			return
		}
		entry, err := service.Decide(r.Context(), principal(p), roster.Scope{TenantID: tenant, SiteID: site}, in, key, expected)
		if err != nil {
			rosterError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(entry.Revision, 10)+`"`)
		reply(w, 200, entry)
	})
}

func rosterError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, roster.ErrNotFound):
		problem(w, 404, "NOT_FOUND")
	case errors.Is(err, roster.ErrDenied):
		problem(w, 403, "PERMISSION_DENIED")
	case errors.Is(err, roster.ErrReauthentication):
		problem(w, 428, "REAUTHENTICATION_REQUIRED")
	case errors.Is(err, roster.ErrIdempotency):
		problem(w, 409, "IDEMPOTENCY_CONFLICT")
	case errors.Is(err, roster.ErrUnchanged):
		problem(w, 409, "ROSTER_UNCHANGED")
	case errors.Is(err, roster.ErrRevision):
		problem(w, 412, "REVISION_CONFLICT")
	case errors.Is(err, roster.ErrLine):
		problem(w, 422, "LINE_UNAVAILABLE")
	case errors.Is(err, roster.ErrAssignee):
		problem(w, 422, "ASSIGNEE_NOT_ELIGIBLE")
	case errors.Is(err, roster.ErrInvalid):
		problem(w, 422, "INVALID_ROSTER_DECISION")
	default:
		problem(w, 503, "STORAGE_UNAVAILABLE")
	}
}
