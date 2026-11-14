package httpapi

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
)

func registerOperationalIngestion(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool)) {
	service := ingestion.New(db.DB).WithSessionPolicy(auth.Sessions())
	registerOperationalLiveness(mux, service, scope)
	mux.HandleFunc("POST /v1/sites/{site}/operational-sources/{source}/batches", func(w http.ResponseWriter, r *http.Request) {
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
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limits.RequestBodyBytes))
		var batch ingestion.Batch
		if err != nil {
			var sizeErr *http.MaxBytesError
			if errors.As(err, &sizeErr) {
				problem(w, 413, "BODY_TOO_LARGE")
				return
			}
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		if strictjson.Decode(body, &batch, limits.RequestBodyBytes) != nil {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		result, err := service.AcceptBatch(r.Context(), ingestion.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}, ingestion.Scope{TenantID: tenant, SiteID: site}, r.PathValue("source"), batch, key)
		if err != nil {
			operationalIngestionError(w, err)
			return
		}
		reply(w, 202, result)
	})
}

func registerOperationalLiveness(mux *http.ServeMux, service *ingestion.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool)) {
	mux.HandleFunc("GET /v1/sites/{site}/operational-sources/{source}/liveness", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if r.URL.RawQuery != "" {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		result, err := service.Liveness(r.Context(), ingestion.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}, ingestion.Scope{TenantID: tenant, SiteID: site}, r.PathValue("source"))
		if err != nil {
			operationalIngestionError(w, err)
			return
		}
		// Readiness depends on the wall clock, so it is never cacheable.
		w.Header().Set("Cache-Control", "no-store")
		reply(w, 200, result)
	})
}

func operationalIngestionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ingestion.ErrNotFound):
		problem(w, 404, "NOT_FOUND")
	case errors.Is(err, ingestion.ErrDenied):
		problem(w, 403, "PERMISSION_DENIED")
	case errors.Is(err, ingestion.ErrConflict):
		problem(w, 409, "IDEMPOTENCY_CONFLICT")
	case errors.Is(err, ingestion.ErrSequence):
		problem(w, 409, "SOURCE_SEQUENCE_CONFLICT")
	case errors.Is(err, ingestion.ErrImportRunning):
		problem(w, 409, "IMPORT_IN_PROGRESS")
	case errors.Is(err, ingestion.ErrSourceRevisionConflict):
		problem(w, 409, "EVENT_REVISION_CONFLICT")
	case errors.Is(err, ingestion.ErrRevision):
		problem(w, 412, "SOURCE_REVISION_CONFLICT")
	case errors.Is(err, ingestion.ErrInvalid):
		problem(w, 422, "INVALID_EVENT_BATCH")
	default:
		problem(w, 503, "INGESTION_UNAVAILABLE")
	}
}
