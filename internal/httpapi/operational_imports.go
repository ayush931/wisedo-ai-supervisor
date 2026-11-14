package httpapi

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/operational"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
)

// registerOperationalImports wires the CSV/JSON importer: a multipart upload that creates a
// durable job, a paginated read of that job's per-row outcomes, and the owner/supervisor cancel.
func registerOperationalImports(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool)) {
	registry := operational.New(db.DB).WithSessionPolicy(auth.Sessions())
	service := ingestion.New(db.DB).WithSessionPolicy(auth.Sessions())
	mux.HandleFunc("POST /v1/sites/{site}/operational-sources/{source}/imports", func(w http.ResponseWriter, r *http.Request) {
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
		// The body limit allows the file plus multipart framing.
		r.Body = http.MaxBytesReader(w, r.Body, ingestion.MaxImportBytes+(64<<10))
		defer func() {
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
		}()
		if err := r.ParseMultipartForm(64 << 10); err != nil {
			var sizeErr *http.MaxBytesError
			if errors.As(err, &sizeErr) {
				problem(w, 413, "BODY_TOO_LARGE")
				return
			}
			problem(w, 422, "INVALID_IMPORT")
			return
		}
		// Fields come only from the multipart body: one format, one file, nothing else.
		form := r.MultipartForm
		if len(r.URL.RawQuery) != 0 || len(form.Value) != 1 || len(form.Value["format"]) != 1 || len(form.File) != 1 || len(form.File["file"]) != 1 {
			problem(w, 422, "INVALID_IMPORT")
			return
		}
		format := form.Value["format"][0]
		if format != "csv" && format != "json" {
			problem(w, 422, "INVALID_IMPORT")
			return
		}
		header := form.File["file"][0]
		if !importMediaType(format, header.Header.Get("Content-Type")) {
			problem(w, 415, "UNSUPPORTED_MEDIA")
			return
		}
		file, err := header.Open()
		if err != nil {
			problem(w, 422, "INVALID_IMPORT")
			return
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, ingestion.MaxImportBytes+1))
		if err != nil {
			problem(w, 422, "INVALID_IMPORT")
			return
		}
		if len(data) > ingestion.MaxImportBytes {
			problem(w, 413, "BODY_TOO_LARGE")
			return
		}
		// Only reviewed sources accept imports. Checking before the job exists means a refused
		// request leaves nothing behind.
		src, _, err := registry.Get(r.Context(), operational.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}, operational.Scope{TenantID: tenant, SiteID: site}, r.PathValue("source"), 0)
		if err != nil {
			operationalError(w, err)
			return
		}
		if !src.ReviewEligible {
			problem(w, 409, "SOURCE_NOT_APPROVED")
			return
		}
		job, err := service.Import(r.Context(), ingestion.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}, ingestion.Scope{TenantID: tenant, SiteID: site}, ingestion.ImportRequest{
			SourceID:        r.PathValue("source"),
			SourceRevision:  src.Revision,
			MappingRevision: src.MappingRevision,
			Format:          format,
			Data:            data,
			Key:             key,
		})
		if err != nil {
			importError(w, err)
			return
		}
		status := 202
		if job.Replayed {
			status = 200
		}
		reply(w, status, job)
	})
	mux.HandleFunc("GET /v1/sites/{site}/operational-ingestion-jobs/{job}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		after, limit := 0, 50
		for name, values := range r.URL.Query() {
			if len(values) != 1 {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
			value, err := strconv.Atoi(values[0])
			if err != nil || strconv.Itoa(value) != values[0] {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
			switch name {
			case "after":
				after = value
			case "limit":
				limit = value
			default:
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		if limit < 1 || limit > 100 || after < 0 {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		job, err := service.Job(r.Context(), ingestion.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}, ingestion.Scope{TenantID: tenant, SiteID: site}, r.PathValue("job"), after, limit)
		if err != nil {
			importError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		reply(w, 200, job)
	})
	// An owner or supervisor ends a running import (WIS-719), for example one a crashed process left
	// holding its source. The body is an empty JSON object. The job has no revision, so there is no
	// If-Match: a job that is no longer running is a lifecycle conflict (409 IMPORT_NOT_RUNNING).
	mux.HandleFunc("POST /v1/sites/{site}/operational-ingestion-jobs/{job}/cancel", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if err := auth.CheckCSRF(r, p); err != nil {
			problem(w, 403, "CSRF_REJECTED")
			return
		}
		key := r.Header.Get("Idempotency-Key")
		if len(r.Header.Values("Idempotency-Key")) != 1 || len(key) < 1 || len(key) > 128 || strings.TrimSpace(key) != key || r.URL.RawQuery != "" {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/json" {
			problem(w, 415, "UNSUPPORTED_MEDIA")
			return
		}
		// A pointer, so the body null (which decodes into struct{} without error) stays nil and is refused.
		var body *struct{}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limits.RequestBodyBytes))
		if err != nil || strictjson.Decode(raw, &body, limits.RequestBodyBytes) != nil || body == nil {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		job, err := service.CancelJob(r.Context(), ingestion.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}, ingestion.Scope{TenantID: tenant, SiteID: site}, r.PathValue("job"), key)
		if err != nil {
			importError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		reply(w, 200, job)
	})
	mux.HandleFunc("GET /v1/sites/{site}/operational-ingestion-jobs/{job}/errors.csv", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if r.URL.RawQuery != "" {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		actor := ingestion.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
		jobID := r.PathValue("job")
		body, err := service.JobErrorsCSV(r.Context(), actor, ingestion.Scope{TenantID: tenant, SiteID: site}, jobID)
		if err != nil {
			importError(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="operational-import-errors-`+jobID+`.csv"`)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
}

func importError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ingestion.ErrImportTooLarge):
		problem(w, 422, "IMPORT_TOO_LARGE")
	case errors.Is(err, ingestion.ErrInvalid):
		problem(w, 422, "INVALID_IMPORT")
	case errors.Is(err, ingestion.ErrConflict):
		problem(w, 409, "IDEMPOTENCY_CONFLICT")
	case errors.Is(err, ingestion.ErrImportRunning):
		problem(w, 409, "IMPORT_IN_PROGRESS")
	case errors.Is(err, ingestion.ErrImportNotRunning):
		problem(w, 409, "IMPORT_NOT_RUNNING")
	case errors.Is(err, ingestion.ErrSourceRevisionConflict):
		problem(w, 409, "EVENT_REVISION_CONFLICT")
	case errors.Is(err, ingestion.ErrRevision):
		problem(w, 412, "SOURCE_REVISION_CONFLICT")
	case errors.Is(err, ingestion.ErrDenied):
		problem(w, 403, "PERMISSION_DENIED")
	case errors.Is(err, ingestion.ErrNotFound):
		problem(w, 404, "NOT_FOUND")
	default:
		problem(w, 503, "INGESTION_UNAVAILABLE")
	}
}

// importMediaType accepts the declared part types for each format. Browsers send text/plain
// or an empty type for some CSV files, so those are accepted for CSV only.
func importMediaType(format, declared string) bool {
	if declared == "" {
		return format == "csv"
	}
	media, _, err := mime.ParseMediaType(declared)
	if err != nil {
		return false
	}
	switch format {
	case "csv":
		return media == "text/csv" || media == "application/csv" || media == "text/plain"
	case "json":
		return media == "application/json"
	}
	return false
}
