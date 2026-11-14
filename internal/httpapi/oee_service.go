package httpapi

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/oee"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/oeeservice"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
	"github.com/google/uuid"
)

// registerOEEService exposes the WIS-229 OEE service under /v1/sites/{site}/oee (ADR 0002
// Sec. 8 routes, site-scoped per rule 07). Tenant and actor come from the verified session.
// Without valid limits the routes fail closed with 503.
//
// The returned drain function stops new calculations and waits for in-flight executions; call
// it on shutdown after the HTTP server has stopped accepting requests.
func registerOEEService(mux *http.ServeMux, cfg config.Config, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool)) func(context.Context) error {
	host, _ := os.Hostname()
	service, serviceErr := oeeservice.New(db.DB, oeeservice.Config{
		RunDeadline: cfg.OEERunDeadline, LeaseDuration: cfg.OEELeaseDuration, MaxOpenRuns: cfg.OEEMaxOpenRuns,
		MaxConcurrentExecutions: cfg.OEEMaxConcurrentExecutions, IdempotencyRetention: cfg.OEEIdempotencyRetention,
		ExecutionOwner: executionOwner(host),
		Sessions:       cfg.Sessions,
	})
	drain := func(ctx context.Context) error {
		if serviceErr != nil {
			return nil
		}
		return service.Close(ctx)
	}
	actor := func(p identity.Principal) oeeservice.Actor {
		return oeeservice.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
	}
	correlation := func(w http.ResponseWriter) string {
		id := w.Header().Get(correlationHeader)
		if id == "" {
			id = uuid.NewString()
			w.Header().Set(correlationHeader, id)
		}
		return id
	}
	// enter authenticates and scopes the request; mutations also check CSRF and require one
	// Idempotency-Key header.
	enter := func(w http.ResponseWriter, r *http.Request, mutation bool) (identity.Principal, oeeservice.Scope, string, bool) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return p, oeeservice.Scope{}, "", false
		}
		if serviceErr != nil {
			problem(w, 503, "OEE_SERVICE_UNAVAILABLE")
			return p, oeeservice.Scope{}, "", false
		}
		if r.URL.RawQuery != "" {
			problem(w, 422, "INVALID_REQUEST")
			return p, oeeservice.Scope{}, "", false
		}
		key := ""
		if mutation {
			if err := auth.CheckCSRF(r, p); err != nil {
				problem(w, 403, "CSRF_REJECTED")
				return p, oeeservice.Scope{}, "", false
			}
			key = r.Header.Get("Idempotency-Key")
			if len(r.Header.Values("Idempotency-Key")) != 1 || len(key) < 1 || len(key) > 128 || strings.TrimSpace(key) != key {
				problem(w, 422, "INVALID_REQUEST")
				return p, oeeservice.Scope{}, "", false
			}
		}
		return p, oeeservice.Scope{TenantID: tenant, SiteID: site}, key, true
	}
	decode := func(w http.ResponseWriter, r *http.Request, dst any, limit int) bool {
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			problem(w, 415, "UNSUPPORTED_MEDIA")
			return false
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(limit)))
		if err != nil {
			var sizeErr *http.MaxBytesError
			if errors.As(err, &sizeErr) {
				problem(w, 413, "BODY_TOO_LARGE")
				return false
			}
			problem(w, 422, "INVALID_REQUEST")
			return false
		}
		// Unknown fields (a smuggled tenant, policy, URL or formula), duplicate keys, numeric
		// timestamps and trailing values are rejected.
		if strictjson.Decode(data, dst, limit) != nil {
			problem(w, 422, "INVALID_REQUEST")
			return false
		}
		return true
	}
	etag := func(w http.ResponseWriter, v int64) { w.Header().Set("ETag", `"`+strconv.FormatInt(v, 10)+`"`) }
	base := "/v1/sites/{site}/oee"

	mux.HandleFunc("POST "+base+"/calculations", func(w http.ResponseWriter, r *http.Request) {
		p, sc, key, ok := enter(w, r, true)
		if !ok {
			return
		}
		var req oee.OeeCalculationRequestV1
		if !decode(w, r, &req, oee.MaxDatasetRequestBytes) {
			return
		}
		// Submit accepts the run and executes it in the background; the response reports the
		// accepted run, not its outcome (ADR 0002 Sec. 7.1 step 7).
		run, err := service.Submit(r.Context(), actor(p), sc, req, key, correlation(w))
		if err != nil {
			oeeServiceError(w, err)
			return
		}
		etag(w, run.RowVersion)
		reply(w, 202, run)
	})
	mux.HandleFunc("GET "+base+"/calculations/{run}", func(w http.ResponseWriter, r *http.Request) {
		p, sc, _, ok := enter(w, r, false)
		if !ok {
			return
		}
		run, err := service.GetRun(r.Context(), actor(p), sc, r.PathValue("run"), correlation(w))
		if err != nil {
			oeeServiceError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		etag(w, run.RowVersion)
		reply(w, 200, run)
	})
	// ADR 0002 Sec. 8 names the custom method calculations/{runId}:cancel.
	mux.HandleFunc("POST "+base+"/calculations/{action}", func(w http.ResponseWriter, r *http.Request) {
		runID, found := strings.CutSuffix(r.PathValue("action"), ":cancel")
		if !found {
			problem(w, 404, "NOT_FOUND")
			return
		}
		p, sc, key, ok := enter(w, r, true)
		if !ok {
			return
		}
		run, err := service.Cancel(r.Context(), actor(p), sc, runID, key, correlation(w))
		if err != nil {
			oeeServiceError(w, err)
			return
		}
		etag(w, run.RowVersion)
		reply(w, 202, run)
	})
	mux.HandleFunc("GET "+base+"/evidence/{evidence}", func(w http.ResponseWriter, r *http.Request) {
		p, sc, _, ok := enter(w, r, false)
		if !ok {
			return
		}
		evidence, err := service.GetEvidence(r.Context(), actor(p), sc, r.PathValue("evidence"), correlation(w))
		if err != nil {
			oeeServiceError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		reply(w, 200, evidence)
	})
	mux.HandleFunc("GET "+base+"/policies/{policy}", func(w http.ResponseWriter, r *http.Request) {
		p, sc, _, ok := enter(w, r, false)
		if !ok {
			return
		}
		policy, err := service.GetPolicy(r.Context(), actor(p), sc, r.PathValue("policy"))
		if err != nil {
			oeeServiceError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		etag(w, policy.RowVersion)
		reply(w, 200, policy)
	})
	mux.HandleFunc("POST "+base+"/policies/{policy}/revisions", func(w http.ResponseWriter, r *http.Request) {
		p, sc, key, ok := enter(w, r, true)
		if !ok {
			return
		}
		var in oee.OeePolicyRevisionInputV1
		if !decode(w, r, &in, oee.MaxPolicyRevisionRequestBytes) {
			return
		}
		policy, err := service.CreateRevision(r.Context(), actor(p), sc, r.PathValue("policy"), in, key, correlation(w))
		if err != nil {
			oeeServiceError(w, err)
			return
		}
		etag(w, policy.RowVersion)
		reply(w, 201, policy)
	})
	// ADR 0002 Sec. 8: policies/{policyId}/revisions/{revisionId}:publish. Publication moves
	// the policy pointer, so it requires If-Match with the policy row_version (rule 07).
	mux.HandleFunc("POST "+base+"/policies/{policy}/revisions/{action}", func(w http.ResponseWriter, r *http.Request) {
		revisionID, found := strings.CutSuffix(r.PathValue("action"), ":publish")
		if !found {
			problem(w, 404, "NOT_FOUND")
			return
		}
		p, sc, _, ok := enter(w, r, false)
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
		policy, err := service.Publish(r.Context(), actor(p), sc, r.PathValue("policy"), revisionID, expected, key, correlation(w))
		if err != nil {
			oeeServiceError(w, err)
			return
		}
		etag(w, policy.RowVersion)
		reply(w, 200, policy)
	})
	return drain
}

// executionOwner labels this process in the execution fence: host, pid and a random suffix, so
// two processes never share a label. The host part is cut on a rune boundary to keep the whole
// label valid UTF-8 within its 128-byte column. It is an opaque label, never an authority.
func executionOwner(host string) string {
	suffix := ":" + strconv.Itoa(os.Getpid()) + ":" + uuid.NewString()[:8]
	host = strings.ToValidUTF8(strings.TrimSpace(host), "")
	for len(host)+len(suffix) > 128 {
		_, size := utf8.DecodeLastRuneInString(host)
		host = host[:len(host)-size]
	}
	if host == "" {
		host = "host"
	}
	return host + suffix
}

func oeeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, oeeservice.ErrNotFound):
		problem(w, 404, "NOT_FOUND")
	case errors.Is(err, oeeservice.ErrDenied):
		problem(w, 403, "PERMISSION_DENIED")
	case errors.Is(err, oeeservice.ErrConflict):
		problem(w, 409, "IDEMPOTENCY_CONFLICT")
	case errors.Is(err, oeeservice.ErrRevision):
		problem(w, 412, "REVISION_CONFLICT")
	case errors.Is(err, oeeservice.ErrLifecycle):
		problem(w, 409, "RUN_LIFECYCLE_CONFLICT")
	case errors.Is(err, oeeservice.ErrPolicyNotReady):
		problem(w, 409, "POLICY_NOT_READY")
	case errors.Is(err, oeeservice.ErrPolicyOverlap):
		problem(w, 409, "POLICY_OVERLAP")
	case errors.Is(err, oeeservice.ErrPolicyConflict):
		problem(w, 409, "POLICY_REVISION_CONFLICT")
	case errors.Is(err, oeeservice.ErrOverloaded):
		w.Header().Set("Retry-After", "5")
		problem(w, 429, "CAPACITY_EXHAUSTED")
	case errors.Is(err, oeeservice.ErrLimit):
		problem(w, 413, "LIMIT_EXCEEDED")
	case errors.Is(err, oeeservice.ErrInvalid):
		problem(w, 422, "INVALID_REQUEST")
	default:
		problem(w, 503, "STORAGE_UNAVAILABLE")
	}
}
