package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/onboarding"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
)

// registerSourceDocuments serves WIS-613 document upload: the raw document as the body (its kind is
// verified from the bytes, never from a name), the title in X-Source-Title and an Idempotency-Key.
// An upload always creates a new source, so it takes no If-Match. The extracted text becomes a
// document evidence source with automatic citable spans, under the evidence service's own authority.
func registerSourceDocuments(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool), cfg config.Config) {
	service := evidence.New(db.DB).WithSessionPolicy(auth.Sessions()).WithScanTenantCap(cfg.Agent.ScanTenantCap)
	scans := scanSettings(cfg)
	registerSourceScans(mux, service, auth, scope, scans)
	mux.HandleFunc("POST /v1/sites/{site}/source-documents", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if err := auth.CheckCSRF(r, p); err != nil {
			problem(w, 403, "CSRF_REJECTED")
			return
		}
		key, ok := interviewKey(w, r)
		if !ok || !onlyQuery(w, r) {
			return
		}
		title, ok := sourceTitle(r)
		if !ok {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		data, err := onboarding.ReadBounded(http.MaxBytesReader(w, r.Body, limits.DocumentBytes+1), limits.DocumentBytes)
		if err != nil {
			documentError(w, err)
			return
		}
		result, err := onboarding.UploadDocument(r.Context(), service, evidence.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash},
			evidence.Scope{TenantID: tenant, SiteID: site}, onboarding.DocumentUpload{Title: title, DeclaredType: r.Header.Get("Content-Type"), Data: data}, key, scans)
		if err != nil {
			documentError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Source.StateRevision, 10)+`"`)
		reply(w, 200, result)
	})
}

// sourceTitle reads the single X-Source-Title header, percent-decoded as UTF-8 so a title in any
// script travels in an ASCII header. Its length is the evidence service's rule.
func sourceTitle(r *http.Request) (string, bool) {
	values := r.Header.Values("X-Source-Title")
	if len(values) != 1 {
		return "", false
	}
	title, err := url.PathUnescape(values[0])
	return title, err == nil && utf8.ValidString(title) && strings.TrimSpace(title) != "" && !strings.ContainsFunc(title, unicode.IsControl) && !strings.ContainsFunc(title, bidiControl)
}

// bidiControl reports a directional mark, embedding, override or isolate, which reorders how the
// title displays (a title could pose as "SOP.exe"). Zero-width joiners stay allowed: Indic titles
// need them.
func bidiControl(r rune) bool {
	return r == '\u061c' || r == '\u200e' || r == '\u200f' || r >= '\u202a' && r <= '\u202e' || r >= '\u2066' && r <= '\u2069'
}

func documentError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, onboarding.ErrTooLarge):
		problem(w, 413, "LIMIT_EXCEEDED")
	case errors.Is(err, onboarding.ErrUnsupportedMedia):
		problem(w, 415, "UNSUPPORTED_MEDIA")
	case errors.Is(err, onboarding.ErrUnreadableSource):
		problem(w, 422, "UNREADABLE_SOURCE")
	// The file itself is refused (WIS-624): resending it cannot succeed, so each is a 4xx with its
	// own stable code (never err.Error(), which may carry a diagnostic suffix).
	case errors.Is(err, onboarding.ErrPDFEncrypted):
		problem(w, 422, "PDF_ENCRYPTED")
	case errors.Is(err, onboarding.ErrPDFNoTextLayer):
		problem(w, 422, "PDF_NO_TEXT_LAYER")
	case errors.Is(err, onboarding.ErrPDFScanHasText):
		problem(w, 422, "PDF_SCAN_HAS_TEXT")
	case errors.Is(err, onboarding.ErrPDFScanImageUnsupported):
		problem(w, 415, "PDF_SCAN_IMAGE_UNSUPPORTED")
	case errors.Is(err, onboarding.ErrPDFScanHiddenContent):
		problem(w, 422, "PDF_SCAN_HIDDEN_CONTENT")
	case errors.Is(err, onboarding.ErrPDFScanTooManyPages):
		problem(w, 413, "PDF_SCAN_TOO_MANY_PAGES")
	case errors.Is(err, onboarding.ErrPDFTextUnreadable):
		problem(w, 422, "PDF_TEXT_UNREADABLE")
	case errors.Is(err, onboarding.ErrPDFActiveContent):
		problem(w, 415, "PDF_ACTIVE_CONTENT")
	case errors.Is(err, onboarding.ErrPDFExternalReference):
		problem(w, 415, "PDF_EXTERNAL_REFERENCE")
	case errors.Is(err, onboarding.ErrPDFTooManyImages):
		problem(w, 413, "PDF_TOO_MANY_IMAGES")
	case errors.Is(err, onboarding.ErrPDFTooManyPages):
		problem(w, 413, "PDF_TOO_MANY_PAGES")
	case errors.Is(err, onboarding.ErrPDFParseBudget):
		problem(w, 413, "PDF_PARSE_BUDGET_EXCEEDED")
	case errors.Is(err, onboarding.ErrExtractionBusy):
		w.Header().Set("Retry-After", "5") // every parser slot is taken: retrying the same key is safe
		problem(w, 503, "EXTRACTION_BUSY")
	case errors.Is(err, evidence.ErrScanQueueFull):
		w.Header().Set("Retry-After", "60") // the tenant's running pages are at its cap: retrying the same key is safe
		problem(w, 503, "SCAN_QUEUE_FULL")
	case errors.Is(err, onboarding.ErrExtractionUnavailable):
		// The operator needs the cause, e.g. "(sandbox unavailable)": our own text, never the file's.
		slog.Warn("document extraction unavailable", "reason", err.Error())
		problem(w, 503, "EXTRACTION_UNAVAILABLE")
	default:
		evidenceError(w, err)
	}
}

// scanSettings pins OCR to the configured vision model (WIS-625): local in production, the
// deterministic fixture only outside it. Without a vision model, scans are unavailable.
func scanSettings(cfg config.Config) onboarding.ScanSettings {
	a := cfg.Agent
	if !a.ModeEnabled || a.VisionModel == "" || a.Provider != config.AgentProviderLocal && a.Provider != config.AgentProviderFixture {
		return onboarding.ScanSettings{}
	}
	provider := agent.ProviderLocal
	if a.Provider == config.AgentProviderFixture {
		provider = agent.ProviderFixture
	}
	return onboarding.ScanSettings{Provider: provider, Model: a.VisionModel, ModelRevision: a.VisionModelRevision, ProviderActual: provider == agent.ProviderLocal, Enabled: true}
}

// registerSourceScans serves WIS-625 slice B1: POST /source-scans queues a photographed page for
// OCR (202, the job), GET /source-scans/{id} reads its state. Every member of the site may read a
// scan's state; only a member who may write evidence may queue one.
func registerSourceScans(mux *http.ServeMux, service *evidence.Service, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool), s onboarding.ScanSettings) {
	mux.HandleFunc("POST /v1/sites/{site}/source-scans", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if err := auth.CheckCSRF(r, p); err != nil {
			problem(w, 403, "CSRF_REJECTED")
			return
		}
		key, ok := interviewKey(w, r)
		if !ok || !onlyQuery(w, r) {
			return
		}
		title, ok := sourceTitle(r)
		if !ok {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		data, err := onboarding.ReadBounded(http.MaxBytesReader(w, r.Body, int64(agent.MaxImageBytes)+1), int64(agent.MaxImageBytes))
		if err != nil {
			documentError(w, err)
			return
		}
		job, err := onboarding.QueueScan(r.Context(), service, evidence.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash},
			evidence.Scope{TenantID: tenant, SiteID: site}, onboarding.DocumentUpload{Title: title, DeclaredType: r.Header.Get("Content-Type"), Data: data}, key, s)
		if err != nil {
			documentError(w, err)
			return
		}
		reply(w, 202, job)
	})
	// WIS-625 slice B2: a scanned PDF (every page a picture) queues one job per page, all or none.
	mux.HandleFunc("POST /v1/sites/{site}/source-scan-batches", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if err := auth.CheckCSRF(r, p); err != nil {
			problem(w, 403, "CSRF_REJECTED")
			return
		}
		key, ok := interviewKey(w, r)
		if !ok || !onlyQuery(w, r) {
			return
		}
		title, ok := sourceTitle(r)
		if !ok {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		data, err := onboarding.ReadBounded(http.MaxBytesReader(w, r.Body, limits.DocumentBytes+1), limits.DocumentBytes)
		if err != nil {
			documentError(w, err)
			return
		}
		batch, err := onboarding.QueueScanPages(r.Context(), service, evidence.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash},
			evidence.Scope{TenantID: tenant, SiteID: site}, onboarding.DocumentUpload{Title: title, DeclaredType: r.Header.Get("Content-Type"), Data: data}, key, s)
		if err != nil {
			documentError(w, err)
			return
		}
		reply(w, 202, batch)
	})
	mux.HandleFunc("GET /v1/sites/{site}/source-scans/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r) {
			return
		}
		job, err := service.ReadScan(r.Context(), evidence.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}, evidence.Scope{TenantID: tenant, SiteID: site}, r.PathValue("id"))
		if err != nil {
			evidenceError(w, err)
			return
		}
		reply(w, 200, job)
	})
}
