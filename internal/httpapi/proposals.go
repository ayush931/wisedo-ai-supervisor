package httpapi

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/approvals"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
)

// registerProposals serves WIS-274 approvals. There is deliberately no route that creates a
// proposal or an approved task: proposals are submitted by proposal generation (WIS-276) and
// only a distinct human decides them here.
func registerProposals(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool)) {
	service := approvals.NewService(db.DB).WithSessionPolicy(auth.Sessions())
	principal := func(p identity.Principal) approvals.Principal {
		return approvals.Principal{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
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
		// Unknown fields are refused, so a forged reviewer, tenant or approval field in the
		// body is a 422 rather than silently ignored.
		if strictjson.Decode(data, dst, limits.RequestBodyBytes) != nil {
			problem(w, 422, "INVALID_DECISION")
			return false
		}
		return true
	}
	base := "/v1/sites/{site}/proposals"
	mux.HandleFunc("GET "+base, func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r, "limit", "cursor") {
			return
		}
		limit := approvals.DefaultPageSize
		if raw := r.URL.Query().Get("limit"); raw != "" {
			var err error
			if limit, err = strconv.Atoi(raw); err != nil || limit < 1 || limit > approvals.MaxPageSize {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		page, err := service.List(r.Context(), principal(p), approvals.Scope{TenantID: tenant, SiteID: site}, r.URL.Query().Get("cursor"), limit)
		if err != nil {
			proposalError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		reply(w, 200, page)
	})
	mux.HandleFunc("GET "+base+"/{proposal}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if r.URL.RawQuery != "" {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		result, err := service.Get(r.Context(), principal(p), approvals.Scope{TenantID: tenant, SiteID: site}, r.PathValue("proposal"))
		if err != nil {
			proposalError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		etag(w, result.Revision)
		reply(w, 200, result)
	})
	mutation := func(name string, run func(http.ResponseWriter, *http.Request, approvals.Principal, approvals.Scope, string, int64) (approvals.View, bool, error)) {
		mux.HandleFunc("POST "+base+"/{proposal}/"+name, func(w http.ResponseWriter, r *http.Request) {
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
			result, decoded, err := run(w, r, principal(p), approvals.Scope{TenantID: tenant, SiteID: site}, key, expected)
			if !decoded {
				return
			}
			if err != nil {
				proposalError(w, err)
				return
			}
			etag(w, result.Revision)
			reply(w, 200, result)
		})
	}
	// run reports decoded=false when decode has already written the problem response.
	mutation("decisions", func(w http.ResponseWriter, r *http.Request, a approvals.Principal, sc approvals.Scope, key string, expected int64) (approvals.View, bool, error) {
		var in approvals.DecisionInput
		if !decode(w, r, &in) {
			return approvals.View{}, false, nil
		}
		v, err := service.Decide(r.Context(), a, sc, r.PathValue("proposal"), in, key, expected)
		return v, true, err
	})
	mutation("revoke", func(w http.ResponseWriter, r *http.Request, a approvals.Principal, sc approvals.Scope, key string, expected int64) (approvals.View, bool, error) {
		var in approvals.RevokeInput
		if !decode(w, r, &in) {
			return approvals.View{}, false, nil
		}
		v, err := service.Revoke(r.Context(), a, sc, r.PathValue("proposal"), in, key, expected)
		return v, true, err
	})
}

func proposalError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, approvals.ErrNotFound):
		problem(w, 404, "NOT_FOUND")
	case errors.Is(err, approvals.ErrDenied):
		problem(w, 403, "PERMISSION_DENIED")
	case errors.Is(err, approvals.ErrNotEligible):
		problem(w, 403, "REVIEWER_NOT_ELIGIBLE")
	case errors.Is(err, approvals.ErrReauthentication):
		problem(w, 428, "REAUTHENTICATION_REQUIRED")
	case errors.Is(err, approvals.ErrIdempotency):
		problem(w, 409, "IDEMPOTENCY_CONFLICT")
	case errors.Is(err, approvals.ErrRevisionConflict):
		problem(w, 412, "REVISION_CONFLICT")
	case errors.Is(err, approvals.ErrIllegalTransition):
		problem(w, 409, "ILLEGAL_TRANSITION")
	case errors.Is(err, approvals.ErrExpired):
		problem(w, 409, "APPROVAL_EXPIRED")
	case errors.Is(err, approvals.ErrStaleBinding):
		problem(w, 409, "PROPOSAL_STALE")
	case errors.Is(err, approvals.ErrEvidenceUnavailable):
		problem(w, 409, "EVIDENCE_UNAVAILABLE")
	case errors.Is(err, approvals.ErrActiveProposal):
		problem(w, 409, "PROPOSAL_ACTIVE")
	case errors.Is(err, approvals.ErrInvalidDecision):
		problem(w, 422, "INVALID_DECISION")
	default:
		problem(w, 503, "STORAGE_UNAVAILABLE")
	}
}
