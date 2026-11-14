package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/chat"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/jackc/pgx/v5/pgconn"
)

func chatActor(p identity.Principal) chat.Actor {
	return chat.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
}

func chatError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, chat.ErrInvalid):
		problem(w, 422, "INVALID_REQUEST")
	case errors.Is(err, chat.ErrDenied):
		problem(w, 403, "PERMISSION_DENIED")
	case errors.Is(err, chat.ErrNotFound):
		problem(w, 404, "NOT_FOUND")
	case errors.Is(err, chat.ErrConflict):
		problem(w, 409, "IDEMPOTENCY_CONFLICT")
	case errors.Is(err, chat.ErrRevision):
		problem(w, 412, "REVISION_CONFLICT")
	default:
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.SchemaName == "supervisor" && pgErr.TableName == "chat_receipts" && pgErr.ConstraintName == "chat_receipts_pkey" {
			problem(w, 409, "IDEMPOTENCY_CONFLICT")
			return
		}
		problem(w, 503, "STORAGE_UNAVAILABLE")
	}
}

// registerChat mounts the WIS-517 operations-chat routes. Chat is optional: these routes never
// gate the workstation workflow, and a reply is recorded as an explicit non-answer until the
// answering loop is qualified.
func registerChat(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool), cfg config.Config) {
	service := func() *chat.Service {
		enabled := cfg.Agent.Provider != "" && cfg.Agent.ModeEnabled
		provider := agent.ProviderLocal
		if cfg.Agent.Provider == config.AgentProviderFixture {
			provider = agent.ProviderFixture
		}
		return chat.NewService(db.DB).WithSessionPolicy(auth.Sessions()).WithAssistant(enabled).WithAnswering(chat.AnswerSettings{
			Enabled: enabled && cfg.Agent.ChatAnswering, Provider: provider, ProviderActual: cfg.Agent.Provider != config.AgentProviderFixture,
			Model: cfg.Agent.Model, ModelRev: cfg.Agent.ModelRevision})
	}
	base := "/v1/sites/{site}/chat-conversations"
	mux.HandleFunc("POST "+base, func(w http.ResponseWriter, r *http.Request) {
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
		var input chat.CreateInput
		if !decodeInterview(w, r, &input) {
			return
		}
		result, err := service().Create(r.Context(), chatActor(p), chat.Scope{TenantID: tenant, SiteID: site}, input, key)
		if err != nil {
			chatError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		reply(w, 201, result)
	})
	mux.HandleFunc("GET "+base, func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		limit, cursor, ok := interviewPageQuery(w, r)
		if !ok {
			return
		}
		result, err := service().List(r.Context(), chatActor(p), chat.Scope{TenantID: tenant, SiteID: site}, limit, cursor)
		if err != nil {
			chatError(w, err)
			return
		}
		reply(w, 200, result)
	})
	mux.HandleFunc("GET "+base+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r) {
			return
		}
		result, err := service().Get(r.Context(), chatActor(p), chat.Scope{TenantID: tenant, SiteID: site}, r.PathValue("id"))
		if err != nil {
			chatError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		reply(w, 200, result)
	})
	mux.HandleFunc("GET "+base+"/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		limit, cursor, ok := interviewPageQuery(w, r)
		if !ok {
			return
		}
		result, err := service().Messages(r.Context(), chatActor(p), chat.Scope{TenantID: tenant, SiteID: site}, r.PathValue("id"), limit, cursor)
		if err != nil {
			chatError(w, err)
			return
		}
		reply(w, 200, result)
	})
	mux.HandleFunc("POST "+base+"/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
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
		var input chat.PostInput
		if !decodeInterview(w, r, &input) {
			return
		}
		result, err := service().Post(r.Context(), chatActor(p), chat.Scope{TenantID: tenant, SiteID: site}, r.PathValue("id"), input, key, expected)
		if err != nil {
			chatError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Conversation.Revision, 10)+`"`)
		status := 201
		if result.Replayed {
			status = 200
		}
		reply(w, status, result)
	})
}
