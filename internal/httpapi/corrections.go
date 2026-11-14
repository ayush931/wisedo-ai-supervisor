package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
)

func registerCorrections(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool)) {
	service := knowledge.New(db.DB).WithSessionPolicy(auth.Sessions())
	actor := func(p identity.Principal) knowledge.Actor {
		return knowledge.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
	}
	base := "/v1/sites/{site}/memory-candidates"
	mux.HandleFunc("GET "+base, func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r, "state", "limit", "cursor") {
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
		out, err := service.ListMemoryCandidates(r.Context(), actor(p), knowledge.Scope{TenantID: tenant, SiteID: site}, r.URL.Query().Get("state"), r.URL.Query().Get("cursor"), limit)
		if err != nil {
			correctionError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		reply(w, 200, out)
	})
	mux.HandleFunc("GET "+base+"/{candidate}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r) {
			return
		}
		out, err := service.ReadMemoryCandidate(r.Context(), actor(p), knowledge.Scope{TenantID: tenant, SiteID: site}, r.PathValue("candidate"))
		if err != nil {
			correctionError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("ETag", `"`+strconv.FormatInt(out.Revision, 10)+`"`)
		reply(w, 200, out)
	})
	for _, operation := range []string{"revisions", "review"} {
		mux.HandleFunc("POST "+base+"/{candidate}/"+operation, func(w http.ResponseWriter, r *http.Request) {
			p, tenant, site, ok := scope(w, r)
			if !ok || !onlyQuery(w, r) {
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
			var out knowledge.MemoryCandidate
			var err error
			sc := knowledge.Scope{TenantID: tenant, SiteID: site}
			if operation == "review" {
				var in knowledge.MemoryReviewInput
				if !decodeEvidence(w, r, &in) {
					return
				}
				out, err = service.DecideMemoryReview(r.Context(), actor(p), sc, r.PathValue("candidate"), in, key, expected)
			} else {
				var in knowledge.MemoryRevisionInput
				if !decodeEvidence(w, r, &in) {
					return
				}
				out, err = service.ReviseMemoryCandidate(r.Context(), actor(p), sc, r.PathValue("candidate"), in, key, expected)
			}
			if err != nil {
				correctionError(w, err)
				return
			}
			w.Header().Set("ETag", `"`+strconv.FormatInt(out.Revision, 10)+`"`)
			reply(w, 200, out)
		})
	}
	mux.HandleFunc("GET /v1/sites/{site}/correction-summary", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r) {
			return
		}
		out, err := service.CorrectionSummary(r.Context(), actor(p), knowledge.Scope{TenantID: tenant, SiteID: site})
		if err != nil {
			correctionError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		reply(w, 200, out)
	})
	mux.HandleFunc("GET /v1/sites/{site}/correction-evaluation-cases", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r, "limit", "cursor") {
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
		out, err := service.ListCorrectionEvaluationCases(r.Context(), actor(p), knowledge.Scope{TenantID: tenant, SiteID: site}, r.URL.Query().Get("cursor"), limit)
		if err != nil {
			correctionError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		reply(w, 200, out)
	})

}

func correctionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, knowledge.ErrSelfReview):
		problem(w, 403, "SELF_REVIEW_FORBIDDEN")
	case errors.Is(err, knowledge.ErrCorrectionSuperseded):
		problem(w, 409, "CORRECTION_SUPERSEDED")
	case errors.Is(err, knowledge.ErrCorrectionIncomplete):
		problem(w, 422, "CORRECTION_INCOMPLETE")
	default:
		knowledgeError(w, err)
	}
}
