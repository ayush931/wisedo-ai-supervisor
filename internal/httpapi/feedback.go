package httpapi

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/feedback"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
)

// registerFeedback serves WIS-279 OutcomeFeedbackV1: append-only feedback on a chat answer, a
// claim revision, a mapping revision, a case or a task. There is no update or delete route.
func registerFeedback(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool)) {
	service := feedback.New(db.DB).WithSessionPolicy(auth.Sessions())
	principal := func(p identity.Principal) feedback.Principal {
		return feedback.Principal{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
	}
	base := "/v1/sites/{site}/feedback"
	mux.HandleFunc("POST "+base, func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if err := auth.CheckCSRF(r, p); err != nil {
			problem(w, 403, "CSRF_REJECTED")
			return
		}
		// Feedback receipts key on a UUID; any other key is the same header error as a missing one.
		key := r.Header.Get("Idempotency-Key")
		if id, err := uuid.Parse(key); len(r.Header.Values("Idempotency-Key")) != 1 || err != nil || id.String() != key {
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
		// Unknown fields are refused, so a forged actor, case binding or evidence revision is a
		// 422 rather than silently ignored.
		var in feedback.FeedbackInputV1
		if strictjson.Decode(data, &in, limits.RequestBodyBytes) != nil {
			problem(w, 422, "INVALID_FEEDBACK")
			return
		}
		v, err := service.Submit(r.Context(), principal(p), feedback.Scope{TenantID: tenant, SiteID: site}, in, key)
		if err != nil {
			feedbackError(w, err)
			return
		}
		status := 201
		if v.Replayed {
			status = 200
		}
		reply(w, status, v)
	})
	mux.HandleFunc("GET "+base, func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r, "case_id", "subject_kind", "limit", "cursor") {
			return
		}
		limit := feedback.DefaultPageSize
		q := r.URL.Query()
		if raw := q.Get("limit"); raw != "" {
			var err error
			if limit, err = strconv.Atoi(raw); err != nil || limit < 1 || limit > feedback.MaxPageSize {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		page, err := service.List(r.Context(), principal(p), feedback.Scope{TenantID: tenant, SiteID: site},
			feedback.ListFilter{CaseID: q.Get("case_id"), SubjectKind: feedback.SubjectKind(q.Get("subject_kind"))}, q.Get("cursor"), limit)
		if err != nil {
			if errors.Is(err, feedback.ErrInvalid) {
				problem(w, 422, "INVALID_REQUEST") // a malformed filter or cursor
				return
			}
			feedbackError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		reply(w, 200, page)
	})
	mux.HandleFunc("GET "+base+"/{feedback}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if r.URL.RawQuery != "" {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		v, err := service.Get(r.Context(), principal(p), feedback.Scope{TenantID: tenant, SiteID: site}, r.PathValue("feedback"))
		if err != nil {
			feedbackError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		reply(w, 200, v)
	})
}

func feedbackError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, feedback.ErrNotFound):
		problem(w, 404, "NOT_FOUND")
	case errors.Is(err, feedback.ErrDenied):
		problem(w, 403, "PERMISSION_DENIED")
	case errors.Is(err, feedback.ErrIdempotency):
		problem(w, 409, "IDEMPOTENCY_CONFLICT")
	case errors.Is(err, feedback.ErrSubjectState):
		problem(w, 409, "FEEDBACK_SUBJECT_NOT_READY")
	case errors.Is(err, feedback.ErrSuperseded):
		problem(w, 409, "FEEDBACK_ALREADY_SUPERSEDED")
	case errors.Is(err, feedback.ErrEvidence):
		problem(w, 422, "FEEDBACK_EVIDENCE_UNAVAILABLE")
	case errors.Is(err, feedback.ErrInvalid):
		problem(w, 422, "INVALID_FEEDBACK")
	default:
		problem(w, 503, "STORAGE_UNAVAILABLE")
	}
}
