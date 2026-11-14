package httpapi

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/cases"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
)

// registerCases serves the WIS-273 case reads and human lifecycle mutations. Triggers are not
// routed: they are service-only and service principals do not exist yet (WIS-290), so only
// trusted Go code opens cases (cases.Service.Trigger).
func registerCases(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool)) {
	service := cases.NewService(db.DB).WithSessionPolicy(auth.Sessions())
	principal := func(p identity.Principal) cases.Principal {
		return cases.Principal{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
	}
	etag := func(w http.ResponseWriter, revision int64) {
		w.Header().Set("ETag", `"`+strconv.FormatInt(revision, 10)+`"`)
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
		// Unknown fields (a forged actor, tenant or state) are refused, never ignored.
		if strictjson.Decode(data, dst, limits.RequestBodyBytes) != nil {
			problem(w, 422, "INVALID_CASE_REQUEST")
			return false
		}
		return true
	}
	base := "/v1/sites/{site}/cases"
	mux.HandleFunc("GET "+base, func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r, "limit", "cursor") {
			return
		}
		limit := cases.DefaultPageSize
		if raw := r.URL.Query().Get("limit"); raw != "" {
			var err error
			if limit, err = strconv.Atoi(raw); err != nil || limit < 1 || limit > cases.MaxPageSize {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		page, err := service.List(r.Context(), principal(p), cases.Scope{TenantID: tenant, SiteID: site}, r.URL.Query().Get("cursor"), limit)
		if err != nil {
			caseError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		reply(w, 200, page)
	})
	mux.HandleFunc("GET "+base+"/{case}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if r.URL.RawQuery != "" {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		result, err := service.Get(r.Context(), principal(p), cases.Scope{TenantID: tenant, SiteID: site}, r.PathValue("case"))
		if err != nil {
			caseError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		etag(w, result.Revision)
		reply(w, 200, result)
	})
	mutation := func(name string, status int, run func(http.ResponseWriter, *http.Request, cases.Principal, cases.Scope, string, int64) (cases.CaseV1, bool, error)) {
		mux.HandleFunc("POST "+base+"/{case}/"+name, func(w http.ResponseWriter, r *http.Request) {
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
			result, decoded, err := run(w, r, principal(p), cases.Scope{TenantID: tenant, SiteID: site}, key, expected)
			if !decoded {
				return
			}
			if err != nil {
				caseError(w, err)
				return
			}
			etag(w, result.Revision)
			code := status
			if result.Replayed {
				code = 200
			}
			reply(w, code, result)
		})
	}
	// run reports decoded=false when decode has already written the problem response.
	mutation("cancel", 200, func(w http.ResponseWriter, r *http.Request, a cases.Principal, sc cases.Scope, key string, expected int64) (cases.CaseV1, bool, error) {
		var in cases.CancelInput
		if !decode(w, r, &in) {
			return cases.CaseV1{}, false, nil
		}
		v, err := service.Cancel(r.Context(), a, sc, r.PathValue("case"), in, key, expected)
		return v, true, err
	})
	mutation("resume", 200, func(w http.ResponseWriter, r *http.Request, a cases.Principal, sc cases.Scope, key string, expected int64) (cases.CaseV1, bool, error) {
		var in cases.ResumeInput
		if !decode(w, r, &in) {
			return cases.CaseV1{}, false, nil
		}
		v, err := service.Resume(r.Context(), a, sc, r.PathValue("case"), in, key, expected)
		return v, true, err
	})
	mutation("close", 200, func(w http.ResponseWriter, r *http.Request, a cases.Principal, sc cases.Scope, key string, expected int64) (cases.CaseV1, bool, error) {
		var in cases.CloseInput
		if !decode(w, r, &in) {
			return cases.CaseV1{}, false, nil
		}
		v, err := service.Close(r.Context(), a, sc, r.PathValue("case"), in, key, expected)
		return v, true, err
	})
	mutation("clarifications", 200, func(w http.ResponseWriter, r *http.Request, a cases.Principal, sc cases.Scope, key string, expected int64) (cases.CaseV1, bool, error) {
		var in cases.ClarifyInput
		if !decode(w, r, &in) {
			return cases.CaseV1{}, false, nil
		}
		v, err := service.Clarify(r.Context(), a, sc, r.PathValue("case"), in, key, expected)
		return v, true, err
	})
	// Reopen answers with the new episode (201); a replay answers 200.
	mutation("reopen", 201, func(w http.ResponseWriter, r *http.Request, a cases.Principal, sc cases.Scope, key string, expected int64) (cases.CaseV1, bool, error) {
		var in cases.ReopenInput
		if !decode(w, r, &in) {
			return cases.CaseV1{}, false, nil
		}
		v, err := service.Reopen(r.Context(), a, sc, r.PathValue("case"), in, key, expected)
		return v, true, err
	})
}

func caseError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, cases.ErrNotFound):
		problem(w, 404, "NOT_FOUND")
	case errors.Is(err, cases.ErrDenied):
		problem(w, 403, "PERMISSION_DENIED")
	case errors.Is(err, cases.ErrReauthentication):
		problem(w, 428, "REAUTHENTICATION_REQUIRED")
	case errors.Is(err, cases.ErrIdempotency):
		problem(w, 409, "IDEMPOTENCY_CONFLICT")
	case errors.Is(err, cases.ErrRevision):
		problem(w, 412, "REVISION_CONFLICT")
	case errors.Is(err, cases.ErrIllegalTransition):
		problem(w, 409, "ILLEGAL_TRANSITION")
	case errors.Is(err, cases.ErrGuardFailed):
		problem(w, 409, "TRANSITION_GUARD_FAILED")
	case errors.Is(err, cases.ErrActiveEpisode):
		problem(w, 409, "CASE_EPISODE_ACTIVE")
	case errors.Is(err, cases.ErrNotQualified):
		problem(w, 409, "CONTEXT_NOT_ELIGIBLE")
	case errors.Is(err, cases.ErrInvalid):
		problem(w, 422, "INVALID_CASE_REQUEST")
	default:
		problem(w, 503, "STORAGE_UNAVAILABLE")
	}
}
