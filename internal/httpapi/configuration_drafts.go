package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
)

// registerConfigurationDrafts serves proposal F configuration drafts (human path). Owners and
// supervisors read and write; promotion appends through the normal revision path and is refused
// to the draft's author and editors (rule 05). Mutations take Idempotency-Key; revise, promote
// and discard also take If-Match on current_revision.
func registerConfigurationDrafts(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool)) {
	service := workstation.NewConfigurationDrafts(db.DB).WithSessionPolicy(auth.Sessions())
	actor := func(p identity.Principal) workstation.Actor {
		return workstation.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
	}
	respond := func(w http.ResponseWriter, status int, out workstation.ConfigurationDraftV1, err error) {
		if err != nil {
			configurationDraftError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(out.CurrentRevision, 10)+`"`)
		reply(w, status, out)
	}
	base := "/v1/sites/{site}/configuration-drafts"
	mux.HandleFunc("POST "+base, func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if len(r.Header.Values("Idempotency-Key")) != 1 || len(key) < 1 || len(key) > 128 || strings.TrimSpace(key) != key {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		var in workstation.ConfigurationDraftCreateInputV1
		p, sc, ok := stationMutation(w, r, auth, scope, &in, workstation.CodeInvalidDraft)
		if !ok {
			return
		}
		out, err := service.Create(r.Context(), actor(p), sc, in, key)
		respond(w, http.StatusCreated, out, err)
	})
	mux.HandleFunc("POST "+base+"/{id}/revisions", func(w http.ResponseWriter, r *http.Request) {
		key, expected, ok := mutationHeaders(w, r)
		if !ok {
			return
		}
		var in workstation.ConfigurationDraftReviseInputV1
		p, sc, ok := stationMutation(w, r, auth, scope, &in, workstation.CodeInvalidDraft)
		if !ok {
			return
		}
		out, err := service.Revise(r.Context(), actor(p), sc, r.PathValue("id"), in, key, expected)
		respond(w, http.StatusCreated, out, err)
	})
	transition := func(path string, status int, apply func(*workstation.ConfigurationDraftService, *http.Request, workstation.Actor, workstation.Scope, string, string, int64) (workstation.ConfigurationDraftV1, error)) {
		mux.HandleFunc("POST "+base+"/{id}/"+path, func(w http.ResponseWriter, r *http.Request) {
			key, expected, ok := mutationHeaders(w, r)
			if !ok {
				return
			}
			var in workstation.ConfigurationDraftEmptyInputV1
			p, sc, ok := stationMutation(w, r, auth, scope, &in, workstation.CodeInvalidDraft)
			if !ok {
				return
			}
			out, err := apply(service, r, actor(p), sc, r.PathValue("id"), key, expected)
			respond(w, status, out, err)
		})
	}
	transition("promotions", http.StatusCreated, func(s *workstation.ConfigurationDraftService, r *http.Request, a workstation.Actor, sc workstation.Scope, id, key string, expected int64) (workstation.ConfigurationDraftV1, error) {
		return s.Promote(r.Context(), a, sc, id, key, expected)
	})
	transition("discards", http.StatusOK, func(s *workstation.ConfigurationDraftService, r *http.Request, a workstation.Actor, sc workstation.Scope, id, key string, expected int64) (workstation.ConfigurationDraftV1, error) {
		return s.Discard(r.Context(), a, sc, id, key, expected)
	})
	mux.HandleFunc("GET "+base+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r) {
			return
		}
		out, err := service.Get(r.Context(), actor(p), workstation.Scope{TenantID: tenant, SiteID: site}, r.PathValue("id"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("GET "+base, func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r, "limit", "cursor", "status", "kind") {
			return
		}
		limit := 50
		if raw := r.URL.Query().Get("limit"); raw != "" {
			var err error
			if limit, err = strconv.Atoi(raw); err != nil || limit < 1 || limit > 100 {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		q := r.URL.Query()
		page, err := service.List(r.Context(), actor(p), workstation.Scope{TenantID: tenant, SiteID: site}, workstation.DraftStatus(q.Get("status")), workstation.DraftKind(q.Get("kind")), limit, q.Get("cursor"))
		if err != nil {
			configurationDraftError(w, err)
			return
		}
		reply(w, http.StatusOK, page)
	})
}

// directKey reports whether key may be used on a direct revision route. The configuration-draft
// promotion key namespace is reserved, so a client write can never pre-empt a promotion.
func directKey(w http.ResponseWriter, key string) bool {
	if strings.HasPrefix(key, workstation.DraftObjectKeyPrefix) {
		problem(w, 422, "INVALID_REQUEST")
		return false
	}
	return true
}

func configurationDraftError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, workstation.ErrPromoterIsEditor):
		problem(w, 403, "PROMOTER_IS_EDITOR")
	case errors.Is(err, workstation.ErrDraftNotOpen):
		problem(w, 409, "DRAFT_NOT_OPEN")
	case errors.Is(err, workstation.ErrDraftSourceUnavailable):
		problem(w, 409, "DRAFT_SOURCE_UNAVAILABLE")
	case errors.Is(err, workstation.ErrTargetRevision):
		problem(w, 409, "TARGET_REVISION_CONFLICT")
	case errors.Is(err, workstation.ErrDraftUnchanged):
		problem(w, 409, "DRAFT_UNCHANGED")
	case errors.Is(err, workstation.ErrLineValidationBlocked):
		problem(w, 409, "LINE_VALIDATION_BLOCKED")
	case errors.Is(err, workstation.ErrMasterDataInUse):
		problem(w, 409, "MASTER_DATA_IN_USE")
	case errors.Is(err, workstation.ErrMasterDataUnchanged):
		problem(w, 409, "MASTER_DATA_UNCHANGED")
	case errors.Is(err, workstation.ErrInvalidShiftPlan):
		problem(w, 422, "INVALID_REQUEST")
	default:
		shiftPlanError(w, err)
	}
}
