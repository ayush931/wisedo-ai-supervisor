package httpapi

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
)

func registerConfigurationImports(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool)) {
	service := workstation.NewMasterData(db.DB).WithSessionPolicy(auth.Sessions())

	mux.HandleFunc("POST /v1/sites/{site}/configuration-imports/dry-runs", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if err := auth.CheckCSRF(r, p); err != nil {
			problem(w, 403, "CSRF_REJECTED")
			return
		}
		data, ok := configurationImportFile(w, r)
		if !ok {
			return
		}
		report, err := service.DryRunConfigurationImport(r.Context(), workstationImportActor(p), workstation.Scope{TenantID: tenant, SiteID: site}, data)
		if err != nil {
			configurationImportError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+report.ReportSHA256+`"`)
		w.Header().Set("Cache-Control", "no-store")
		if strings.TrimSpace(r.Header.Get("Accept")) == "text/csv" {
			writeConfigurationImportErrorsCSV(w, report)
			return
		}
		reply(w, http.StatusOK, report)
	})

	mux.HandleFunc("POST /v1/sites/{site}/configuration-imports", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if err := auth.CheckCSRF(r, p); err != nil {
			problem(w, 403, "CSRF_REJECTED")
			return
		}
		key, ifMatch, ok := configurationImportMutationHeaders(w, r)
		if !ok {
			return
		}
		data, ok := configurationImportFile(w, r)
		if !ok {
			return
		}
		report, id, replayed, err := service.ApplyConfigurationImport(r.Context(), workstationImportActor(p), workstation.Scope{TenantID: tenant, SiteID: site}, data, key, ifMatch)
		if err != nil {
			configurationImportError(w, err)
			return
		}
		if !replayed {
			slog.InfoContext(r.Context(), "configuration import applied", "import_id", id, "accepted", report.Counts.Accepted, "duplicate", report.Counts.Duplicate, "correlation_id", w.Header().Get(correlationHeader))
		}
		w.Header().Set("ETag", `"`+report.ReportSHA256+`"`)
		w.Header().Set("Location", "/v1/sites/"+site+"/configuration-imports/"+id)
		w.Header().Set("Cache-Control", "no-store")
		status := http.StatusCreated
		if replayed {
			status = http.StatusOK
		}
		reply(w, status, report)
	})

	mux.HandleFunc("GET /v1/sites/{site}/configuration-imports/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r) {
			return
		}
		report, err := service.GetConfigurationImport(r.Context(), workstationImportActor(p), workstation.Scope{TenantID: tenant, SiteID: site}, r.PathValue("id"))
		if err != nil {
			configurationImportError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+report.ReportSHA256+`"`)
		w.Header().Set("Cache-Control", "no-store")
		reply(w, http.StatusOK, report)
	})
}

func workstationImportActor(p identity.Principal) workstation.Actor {
	return workstation.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
}

func configurationImportMutationHeaders(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	key := r.Header.Get("Idempotency-Key")
	if len(r.Header.Values("Idempotency-Key")) != 1 || len(key) < 1 || len(key) > 128 || strings.TrimSpace(key) != key {
		problem(w, 422, "INVALID_REQUEST")
		return "", "", false
	}
	tag := r.Header.Get("If-Match")
	if tag == "" {
		problem(w, 428, "REVISION_REQUIRED")
		return "", "", false
	}
	if len(r.Header.Values("If-Match")) != 1 || !workstation.ValidConfigurationImportETag(tag) {
		problem(w, 422, "INVALID_REVISION")
		return "", "", false
	}
	return key, tag, true
}

func configurationImportFile(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.URL.RawQuery != "" {
		problem(w, 422, "INVALID_REQUEST")
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, limits.ImportBytes+(64<<10))
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	if err := r.ParseMultipartForm(64 << 10); err != nil {
		var sizeErr *http.MaxBytesError
		if errors.As(err, &sizeErr) {
			problem(w, 413, "IMPORT_TOO_LARGE")
		} else {
			problem(w, 422, "INVALID_IMPORT")
		}
		return nil, false
	}
	form := r.MultipartForm
	if form == nil || len(form.Value) != 0 || len(form.File) != 1 || len(form.File["file"]) != 1 {
		problem(w, 422, "INVALID_REQUEST")
		return nil, false
	}
	header := form.File["file"][0]
	if !importMediaType("csv", header.Header.Get("Content-Type")) {
		problem(w, 415, "UNSUPPORTED_MEDIA")
		return nil, false
	}
	file, err := header.Open()
	if err != nil {
		problem(w, 422, "INVALID_IMPORT")
		return nil, false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limits.ImportBytes+1))
	if err != nil {
		problem(w, 422, "INVALID_IMPORT")
		return nil, false
	}
	if len(data) > limits.ImportBytes {
		problem(w, 413, "IMPORT_TOO_LARGE")
		return nil, false
	}
	return data, true
}

func configurationImportError(w http.ResponseWriter, err error) {
	var fileErr *workstation.ConfigurationImportFileError
	if errors.As(err, &fileErr) {
		switch fileErr.Code {
		case "IMPORT_TOO_LARGE", "IMPORT_ROWS_EXCEEDED":
			problem(w, 413, fileErr.Code)
		case "UNKNOWN_COLUMN", "DUPLICATE_COLUMN":
			problemAt(w, 422, fileErr.Code, strconv.Itoa(fileErr.Column))
		default:
			problem(w, 422, fileErr.Code)
		}
		return
	}
	switch {
	case errors.Is(err, workstation.ErrConfigurationImportRejected):
		problem(w, 422, "IMPORT_HAS_REJECTED_ROWS")
	case errors.Is(err, workstation.ErrInvalidReportETag):
		problem(w, 422, "INVALID_REVISION")
	case errors.Is(err, workstation.ErrInvalidShiftPlan):
		problem(w, 422, "INVALID_REQUEST")
	case errors.Is(err, workstation.ErrConflict):
		problem(w, 409, "IDEMPOTENCY_CONFLICT")
	case errors.Is(err, workstation.ErrRevision):
		problem(w, 412, "REVISION_CONFLICT")
	case errors.Is(err, workstation.ErrNotFound):
		problem(w, 404, "NOT_FOUND")
	case errors.Is(err, workstation.ErrDenied):
		problem(w, 403, "PERMISSION_DENIED")
	default:
		problem(w, 503, "STORAGE_UNAVAILABLE")
	}
}

func writeConfigurationImportErrorsCSV(w http.ResponseWriter, report workstation.ConfigurationImportReportV1) {
	var output bytes.Buffer
	writer := csv.NewWriter(&output)
	_ = writer.Write([]string{"row", "kind", "ref", "display_name", "status", "reason", "path"})
	for _, row := range report.Rows {
		if row.Status != "rejected" {
			continue
		}
		_ = writer.Write([]string{
			strconv.Itoa(row.Row),
			ingestion.SpreadsheetSafeCell(row.Kind),
			ingestion.SpreadsheetSafeCell(row.Ref),
			ingestion.SpreadsheetSafeCell(row.DisplayName),
			ingestion.SpreadsheetSafeCell(row.Status),
			ingestion.SpreadsheetSafeCell(row.Reason),
			ingestion.SpreadsheetSafeCell(row.Path),
		})
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		configurationImportError(w, fmt.Errorf("csv serialization failed: %w", err))
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="configuration-import-errors.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(output.Bytes())
}
