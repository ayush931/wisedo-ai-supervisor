package httpapi

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/onboarding"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
	"github.com/jackc/pgx/v5/pgconn"
)

func interviewActor(p identity.Principal) onboarding.Actor {
	return onboarding.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
}

func interviewScope(tenant, site string) onboarding.Scope {
	return onboarding.Scope{TenantID: tenant, SiteID: site}
}

func interviewKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.Header.Get("Idempotency-Key")
	if len(r.Header.Values("Idempotency-Key")) != 1 || len(key) < 1 || len(key) > 128 || strings.TrimSpace(key) != key {
		problem(w, 422, "INVALID_REQUEST")
		return "", false
	}
	return key, true
}

func decodeInterview(w http.ResponseWriter, r *http.Request, dst any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		problem(w, 415, "UNSUPPORTED_MEDIA")
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limits.RequestBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			problem(w, 413, "BODY_TOO_LARGE")
		} else {
			problem(w, 422, "INVALID_REQUEST")
		}
		return false
	}
	if strictjson.Decode(body, dst, limits.RequestBodyBytes) != nil {
		problem(w, 422, "INVALID_REQUEST")
		return false
	}
	return true
}

func interviewPageQuery(w http.ResponseWriter, r *http.Request) (int, string, bool) {
	if !onlyQuery(w, r, "cursor", "limit") {
		return 0, "", false
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 || strconv.Itoa(limit) != raw {
			problem(w, 422, "INVALID_REQUEST")
			return 0, "", false
		}
	}
	if values, ok := r.URL.Query()["cursor"]; ok && (len(values) != 1 || values[0] == "") {
		problem(w, 422, "INVALID_REQUEST")
		return 0, "", false
	}
	return limit, r.URL.Query().Get("cursor"), true
}

func interviewError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, onboarding.ErrInterviewInvalid):
		problem(w, 422, "INVALID_REQUEST")
	case errors.Is(err, onboarding.ErrDenied):
		problem(w, 403, "PERMISSION_DENIED")
	case errors.Is(err, onboarding.ErrNotFound):
		problem(w, 404, "NOT_FOUND")
	case errors.Is(err, onboarding.ErrConflict):
		problem(w, 409, "IDEMPOTENCY_CONFLICT")
	case errors.Is(err, onboarding.ErrLifecycle):
		problem(w, 409, "LIFECYCLE_CONFLICT")
	case errors.Is(err, onboarding.ErrInventoryAmbiguous):
		problem(w, 409, "DISCOVERY_INVENTORY_AMBIGUOUS")
	case errors.Is(err, onboarding.ErrInventoryChanged):
		problem(w, 409, "DISCOVERY_INVENTORY_CHANGED")
	case errors.Is(err, onboarding.ErrRevision):
		problem(w, 412, "REVISION_CONFLICT")
	case errors.Is(err, onboarding.ErrStoredCorrupt):
		problem(w, 500, "INTERNAL_ERROR")
	case errors.Is(err, onboarding.ErrSourceChanged):
		problem(w, 409, "SOURCE_CHANGED")
	case errors.Is(err, onboarding.ErrTooLarge):
		problem(w, 413, "LIMIT_EXCEEDED")
	default:
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "23514":
				problem(w, 409, "LIFECYCLE_CONFLICT")
			case "23505":
				problem(w, 409, "IDEMPOTENCY_CONFLICT")
			case "23503":
				problem(w, 422, "INVALID_REQUEST")
			default:
				problem(w, 503, "STORAGE_UNAVAILABLE")
			}
			return
		}
		problem(w, 503, "STORAGE_UNAVAILABLE")
	}
}

func registerInterviews(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool), cfg config.Config) {
	service := func() *onboarding.Service { return onboarding.NewInterviews(db.DB).WithSessionPolicy(auth.Sessions()) }
	mux.HandleFunc("POST /v1/sites/{site}/configuration-draft-requests", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if available, _ := agentAvailability(cfg.Agent); !available {
			problem(w, 503, "AGENT_UNAVAILABLE")
			return
		}
		if err := auth.CheckCSRF(r, p); err != nil {
			problem(w, 403, "CSRF_REJECTED")
			return
		}
		key, ok := interviewKey(w, r)
		if !ok {
			return
		}
		var input onboarding.StartConfigurationDraftsInput
		if !decodeInterview(w, r, &input) {
			return
		}
		provider := agent.ProviderLocal
		if cfg.Agent.Provider == config.AgentProviderFixture {
			provider = agent.ProviderFixture
		}
		result, err := service().StartConfigurationDrafts(r.Context(), interviewActor(p), interviewScope(tenant, site), input, key,
			onboarding.AgentTurnSettings{Provider: provider, ProviderActual: cfg.Agent.Provider != config.AgentProviderFixture, Model: cfg.Agent.Model, ModelRevision: cfg.Agent.ModelRevision, Enabled: cfg.Agent.ModeEnabled})
		if err != nil {
			interviewError(w, err)
			return
		}
		reply(w, http.StatusAccepted, result)
	})
	// Reading a run stays available when the agent is later disabled: the requester must still
	// be able to see why a run was refused (rule 06).
	mux.HandleFunc("GET /v1/sites/{site}/configuration-draft-requests/{runId}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if !onlyQuery(w, r) {
			return
		}
		// The read may reap a stale run (row lock under lock_timeout; a timeout answers 503).
		result, err := service().GetConfigurationDraftRun(r.Context(), interviewActor(p), interviewScope(tenant, site), r.PathValue("runId"))
		if err != nil {
			interviewError(w, err)
			return
		}
		reply(w, 200, result)
	})
	mux.HandleFunc("POST /v1/sites/{site}/interviews/{id}/agent-turns", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if available, _ := agentAvailability(cfg.Agent); !available {
			problem(w, 503, "AGENT_UNAVAILABLE")
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
		var input onboarding.StartAgentTurnInput
		if !decodeInterview(w, r, &input) {
			return
		}
		provider := agent.ProviderLocal
		if cfg.Agent.Provider == config.AgentProviderFixture {
			provider = agent.ProviderFixture
		}
		result, err := service().StartAgentTurn(r.Context(), interviewActor(p), interviewScope(tenant, site), r.PathValue("id"), input, key, expected,
			onboarding.AgentTurnSettings{Provider: provider, ProviderActual: cfg.Agent.Provider != config.AgentProviderFixture, Model: cfg.Agent.Model, ModelRevision: cfg.Agent.ModelRevision, Enabled: cfg.Agent.ModeEnabled})
		if err != nil {
			interviewError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(expected, 10)+`"`)
		reply(w, 202, result)
	})
	mux.HandleFunc("GET /v1/sites/{site}/interviews/{id}/agent-turns/{runId}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if !onlyQuery(w, r) {
			return
		}
		// The read may reap a stale run (row lock under lock_timeout; a timeout answers 503).
		result, err := service().GetAgentTurn(r.Context(), interviewActor(p), interviewScope(tenant, site), r.PathValue("id"), r.PathValue("runId"))
		if err != nil {
			interviewError(w, err)
			return
		}
		reply(w, 200, result)
	})
	mux.HandleFunc("POST /v1/sites/{site}/interviews", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if err := auth.CheckCSRF(r, p); err != nil {
			problem(w, 403, "CSRF_REJECTED")
			return
		}
		key, ok := interviewKey(w, r)
		if !ok {
			return
		}
		var input onboarding.CreateInterviewInput
		if !decodeInterview(w, r, &input) {
			return
		}
		result, err := service().Create(r.Context(), interviewActor(p), interviewScope(tenant, site), input, key)
		if err != nil {
			interviewError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		reply(w, 201, result)
	})
	mux.HandleFunc("GET /v1/sites/{site}/interviews", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		limit, cursor, ok := interviewPageQuery(w, r)
		if !ok {
			return
		}
		result, err := service().List(r.Context(), interviewActor(p), interviewScope(tenant, site), limit, cursor)
		if err != nil {
			interviewError(w, err)
			return
		}
		reply(w, 200, result)
	})
	mux.HandleFunc("GET /v1/sites/{site}/interviews/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if !onlyQuery(w, r) {
			return
		}
		result, err := service().Get(r.Context(), interviewActor(p), interviewScope(tenant, site), r.PathValue("id"))
		if err != nil {
			interviewError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		reply(w, 200, result)
	})
	mux.HandleFunc("PATCH /v1/sites/{site}/interviews/{id}", func(w http.ResponseWriter, r *http.Request) {
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
		var input onboarding.PatchInterviewInput
		if !decodeInterview(w, r, &input) {
			return
		}
		if input.ExpectedRevision != expected {
			problem(w, 422, "INVALID_REVISION")
			return
		}
		result, err := service().Patch(r.Context(), interviewActor(p), interviewScope(tenant, site), r.PathValue("id"), input, key, expected)
		if err != nil {
			interviewError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		reply(w, 200, result)
	})
	mux.HandleFunc("GET /v1/sites/{site}/interviews/{id}/questions", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		limit, cursor, ok := interviewPageQuery(w, r)
		if !ok {
			return
		}
		result, err := service().Questions(r.Context(), interviewActor(p), interviewScope(tenant, site), r.PathValue("id"), limit, cursor)
		if err != nil {
			interviewError(w, err)
			return
		}
		reply(w, 200, result)
	})
	mux.HandleFunc("POST /v1/sites/{site}/interviews/{id}/answers", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if err := auth.CheckCSRF(r, p); err != nil {
			problem(w, 403, "CSRF_REJECTED")
			return
		}
		key, ok := interviewKey(w, r)
		if !ok {
			return
		}
		var input onboarding.AnswerInput
		if !decodeInterview(w, r, &input) {
			return
		}
		result, err := service().Answer(r.Context(), interviewActor(p), interviewScope(tenant, site), r.PathValue("id"), input, key)
		if err != nil {
			interviewError(w, err)
			return
		}
		reply(w, 201, result)
	})
}
