// Package httpapi is the standalone foundation boundary, with no plant actions.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/bootstrap"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
	"github.com/google/uuid"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"
)

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// problem writes an RFC 9457 problem document. It returns the stable code and the request's
// correlation ID only; no request payload, token or upstream body is ever echoed.
func problem(w http.ResponseWriter, status int, code string) { problemAt(w, status, code, "") }

// problemAt adds the request member that caused the problem. The path is built from closed
// member names and array indexes; an unknown member's own name appears only when it matches
// ^[a-z][a-z0-9_]{0,63}$ and is otherwise "*". No submitted value is ever echoed.
func problemAt(w http.ResponseWriter, status int, code, path string) {
	// Boundary assigns one correlation ID per request and returns it as a header, so the
	// body, the header and the server log all carry the same value.
	correlation := w.Header().Get(correlationHeader)
	if correlation == "" {
		correlation = uuid.NewString()
		w.Header().Set(correlationHeader, correlation)
	}
	if status >= 500 {
		if ctx, ok := requestContexts.Load(w); ok && errors.Is(ctx.(context.Context).Err(), context.Canceled) {
			// WIS-290: the client hung up; the error is its cancellation, not a server failure. INFO
			// keeps an incident where every client gives up visible without paging on it.
			slog.Info("request cancelled by client", "status", status, "code", code, "correlation_id", correlation)
		} else {
			slog.Warn("request failed", "status", status, "code", code, "correlation_id", correlation)
		}
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	body := map[string]any{
		"type":           "about:blank",
		"title":          code,
		"status":         status,
		"code":           code,
		"correlation_id": correlation,
	}
	if path != "" {
		body["path"] = path
	}
	_ = json.NewEncoder(w).Encode(body)
}

// requestContexts maps each in-flight response writer to its request context, so problemAt can
// tell a client that hung up from a server failure. Boundary owns the entries. A wrapping
// ResponseWriter would hide http.MaxBytesReader's early close on oversized bodies.
var requestContexts sync.Map

// correlationHeader carries the server-generated request correlation ID. A client value is
// never trusted or echoed.
const correlationHeader = "X-Correlation-ID"

// New returns the API handler. Callers that run a server should use NewWithShutdown so
// background work started by requests can be drained.
func New(cfg config.Config, db *store.Store, auth *identity.Service) http.Handler {
	handler, _ := NewWithShutdown(cfg, db, auth)
	return handler
}

// NewWithShutdown returns the API handler and a drain function. Call drain after
// http.Server.Shutdown: it stops new OEE calculations and waits, up to ctx, for executions that
// accepted requests started.
func NewWithShutdown(cfg config.Config, db *store.Store, auth *identity.Service) (http.Handler, func(context.Context) error) {
	mux := http.NewServeMux()
	auth.Register(mux)
	mux.HandleFunc("POST /v1/bootstrap/redemptions", func(w http.ResponseWriter, r *http.Request) {
		p, err := auth.Authenticate(r.Context(), r)
		if err != nil {
			authError(w, err)
			return
		}
		if err = auth.CheckCSRF(r, p); err != nil {
			problem(w, 403, "CSRF_REJECTED")
			return
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			problem(w, 415, "UNSUPPORTED_MEDIA")
			return
		}
		// Only the token is accepted. Tenant and site fields are unknown and rejected (rule 03).
		var body struct {
			Token string `json:"token"`
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil || strictjson.Decode(raw, &body, limits.RequestBodyBytes) != nil {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		service := bootstrap.Service{DB: db.DB}
		membership, err := service.Redeem(r.Context(), bootstrap.Actor{Issuer: p.Issuer, Subject: p.Subject}, bootstrap.RedeemRequest{Token: body.Token})
		switch {
		case errors.Is(err, bootstrap.ErrInvalidInvitation):
			problem(w, 403, "INVALID_INVITATION")
		case errors.Is(err, bootstrap.ErrInvalidInput):
			problem(w, 422, "INVALID_REQUEST")
		case err != nil:
			problem(w, 503, "STORAGE_UNAVAILABLE")
		default:
			reply(w, 201, membership)
		}
	})
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) { reply(w, 200, map[string]string{"status": "alive"}) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := db.VerifyRole(r.Context()); err != nil {
			problem(w, 503, "STORAGE_UNAVAILABLE")
			return
		}
		if err := migrate.Check(r.Context(), db.DB); err != nil {
			problem(w, 503, "SCHEMA_UNAVAILABLE")
			return
		}
		reply(w, 200, map[string]string{"status": "ready", "scope": "durable-intake"})
	})
	scope := func(w http.ResponseWriter, r *http.Request) (identity.Principal, string, string, bool) {
		p, err := auth.Authenticate(r.Context(), r)
		if err != nil {
			authError(w, err)
			return p, "", "", false
		}
		sid, err := uuid.Parse(r.PathValue("site"))
		if err != nil {
			problem(w, 404, "NOT_FOUND")
			return p, "", "", false
		}
		sites, err := db.Sites(r.Context(), p.Issuer, p.Subject)
		if err != nil {
			problem(w, 503, "STORAGE_UNAVAILABLE")
			return p, "", "", false
		}
		for _, s := range sites {
			if s.ID == sid.String() {
				return p, s.TenantID, s.ID, true
			}
		}
		problem(w, 404, "NOT_FOUND")
		return p, "", "", false
	}
	registerMembership(mux, db, auth, scope, cfg.Agent)
	registerEvidence(mux, db, auth, scope)
	registerSourceDocuments(mux, db, auth, scope, cfg)
	registerKnowledge(mux, db, auth, scope)
	registerInterviews(mux, db, auth, scope, cfg)
	registerChat(mux, db, auth, scope, cfg)
	registerOperational(mux, db, auth, scope)
	registerOperationalIngestion(mux, db, auth, scope)
	registerOperationalImports(mux, db, auth, scope)
	registerPlans(mux, db, auth, scope)
	registerProposals(mux, db, auth, scope)
	registerMaintenanceRoster(mux, db, auth, scope)
	registerModelHosting(mux, db, auth, scope)
	registerFeedback(mux, db, auth, scope)
	registerCorrections(mux, db, auth, scope)
	registerCases(mux, db, auth, scope)
	registerTasks(mux, db, auth, scope)
	registerAdvisor(mux, db, auth, scope, cfg)
	registerContexts(mux, db, auth, scope)
	registerShiftPlans(mux, db, auth, scope, cfg.WorkstationCPSATPath)
	registerJobOrders(mux, db, auth, scope)
	registerStationProfiles(mux, db, auth, scope)
	registerMasterData(mux, db, auth, scope)
	registerConfigurationImports(mux, db, auth, scope)
	registerConfigurationDrafts(mux, db, auth, scope)
	registerStationRecords(mux, db, auth, scope)
	registerShiftSummaries(mux, db, auth, scope)
	registerShiftHandovers(mux, db, auth, scope)
	registerShiftRuns(mux, db, auth, scope)
	registerOEE(mux, auth)
	drainOEE := registerOEEService(mux, cfg, db, auth, scope)
	mux.HandleFunc("POST /v1/sites/{site}/foundation/probes", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if err := auth.CheckCSRF(r, p); err != nil {
			problem(w, 403, "CSRF_REJECTED")
			return
		}
		key := r.Header.Get("Idempotency-Key")
		if len(key) < 1 || len(key) > 128 {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			problem(w, 415, "UNSUPPORTED_MEDIA")
			return
		}
		var body struct {
			Message string `json:"message"`
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil || strictjson.Decode(raw, &body, limits.RequestBodyBytes) != nil || !utf8.ValidString(body.Message) || utf8.RuneCountInString(body.Message) < 1 || utf8.RuneCountInString(body.Message) > 500 {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		result, err := db.SubmitProbe(r.Context(), tenant, site, p.Issuer, p.Subject, key, body.Message)
		if err != nil {
			storeError(w, err)
			return
		}
		reply(w, 202, result)
	})
	mux.HandleFunc("GET /v1/sites/{site}/foundation/probes/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			problem(w, 404, "NOT_FOUND")
			return
		}
		result, err := db.GetProbe(r.Context(), tenant, site, p.Issuer, p.Subject, id.String())
		if err != nil {
			storeError(w, err)
			return
		}
		reply(w, 200, result)
	})
	mux.Handle("/", spa(filepath.Clean(cfg.WebDir)))
	return Boundary(mux, cfg.Environment == "production"), drainOEE
}

// spa serves the built web app; a client-side route such as /sites/{site}/shifts/{id} has no
// file, so a reload falls back to index.html. Missing files that look like assets (they have
// an extension) stay 404. Every /v1 path that reaches here matched no API route, so it is an
// API 404 problem, never HTML. A directory is never listed: it falls back to the app shell,
// so built bundle names cannot be enumerated.
func spa(dir string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1" || strings.HasPrefix(r.URL.Path, "/v1/") {
			problem(w, 404, "NOT_FOUND")
			return
		}
		// The SPA is only read: any other method is refused before the file server, which would
		// otherwise list a directory such as /assets/ (security audit run 1, WIS-699).
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			problem(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
			return
		}
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Path != "/" && filepath.Ext(r.URL.Path) == "" {
			f, err := http.Dir(dir).Open(r.URL.Path)
			if err == nil {
				info, statErr := f.Stat()
				f.Close()
				if statErr == nil && !info.IsDir() {
					files.ServeHTTP(w, r)
					return
				}
			}
			http.ServeFile(w, r, filepath.Join(dir, "index.html"))
			return
		}
		files.ServeHTTP(w, r)
	})
}
func authError(w http.ResponseWriter, err error) {
	if errors.Is(err, identity.ErrUnauthenticated) {
		problem(w, 401, "AUTHENTICATION_REQUIRED")
	} else {
		problem(w, 503, "IDENTITY_UNAVAILABLE")
	}
}
func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrConflict):
		problem(w, 409, "IDEMPOTENCY_CONFLICT")
	case errors.Is(err, store.ErrDenied):
		problem(w, 403, "PERMISSION_DENIED")
	case errors.Is(err, store.ErrNotFound):
		problem(w, 404, "NOT_FOUND")
	default:
		problem(w, 503, "STORAGE_UNAVAILABLE")
	}
}

// contentSecurityPolicy allows styles from 'self' plus exactly one inline sheet: the static
// <style id="astryx-layer-slide-rules"> that @astryxdesign/core 0.7.13 Layer installs at runtime
// (slideRulesCss(), Layer/layerSlideRules.ts). Without it every tooltip and menu loses its
// @position-try fallbacks (WIS-707). No 'unsafe-inline': any other inline style stays blocked.
// An Astryx upgrade that changes those rules changes the hash; web/tests/station.spec.ts
// then fails on the violation.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'sha256-/i2+VVt2qoVh0Ag9OWwyc9593DCZeYGAv6SliqVbheY='; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

func Boundary(next http.Handler, secure bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(correlationHeader, uuid.NewString())
		requestContexts.Store(w, r.Context())
		defer requestContexts.Delete(w)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		if secure {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		// File imports stream through their handler, which applies the import byte limit;
		// every other body is buffered under the 1 MiB request limit.
		if isImportUpload(r) {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limits.RequestBodyBytes))
		if err != nil {
			problem(w, 413, "BODY_TOO_LARGE")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}

// isImportUpload matches the bounded file upload endpoints exactly: the multipart imports and the
// raw document upload (WIS-613), each of which applies its own byte limit.
func isImportUpload(r *http.Request) bool {
	parts := strings.Split(r.URL.Path, "/")
	if r.Method != http.MethodPost || len(parts) < 5 || parts[0] != "" || parts[1] != "v1" || parts[2] != "sites" || parts[3] == "" {
		return false
	}
	// A scan (WIS-625) streams too: its handler caps the page at agent.MaxImageBytes.
	if len(parts) == 5 && (parts[4] == "source-documents" || parts[4] == "source-scans" || parts[4] == "source-scan-batches") {
		return true
	}
	if len(parts) == 7 && parts[4] == "operational-sources" && parts[5] != "" && parts[6] == "imports" {
		return true
	}
	return len(parts) == 5 && parts[4] == "configuration-imports" || len(parts) == 6 && parts[4] == "configuration-imports" && parts[5] == "dry-runs"
}

// onlyQuery accepts a query string that names only the allowed parameters, each at most
// once, and answers 422 otherwise. url.Values.Get silently ignores unknown parameters and
// takes the first of repeated ones (state=approved&state=rejected).
func onlyQuery(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		problem(w, 422, "INVALID_REQUEST")
		return false
	}
	for name, v := range values {
		known := false
		for _, a := range allowed {
			known = known || a == name
		}
		if !known || len(v) != 1 {
			problem(w, 422, "INVALID_REQUEST")
			return false
		}
	}
	return true
}
