package httpapi

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/membership"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
)

func registerMembership(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool), agent config.Agent) {
	service := membership.New(db.DB).WithSessionPolicy(auth.Sessions())
	mux.HandleFunc("GET /v1/me", func(w http.ResponseWriter, r *http.Request) {
		p, err := auth.Authenticate(r.Context(), r)
		if err != nil {
			authError(w, err)
			return
		}
		sites, err := db.Sites(r.Context(), p.Issuer, p.Subject)
		if err != nil {
			problem(w, 503, "STORAGE_UNAVAILABLE")
			return
		}
		available, reasonCode := agentAvailability(agent)
		for i := range sites {
			sites[i].AgentAvailable = available
			sites[i].AgentUnavailableReasonCode = reasonCode
		}
		name, err := db.DisplayName(r.Context(), p.Issuer, p.Subject)
		if err != nil {
			problem(w, 503, "STORAGE_UNAVAILABLE")
			return
		}
		reply(w, 200, store.Me{Subject: p.Subject, DisplayName: name, CSRFToken: p.CSRFToken, Sites: sites})
	})
	mux.HandleFunc("GET /v1/sites/{site}/memberships", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		limit := 50
		if raw := r.URL.Query().Get("limit"); raw != "" {
			var err error
			limit, err = strconv.Atoi(raw)
			if err != nil || limit < 1 || limit > 100 {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		page, err := service.List(r.Context(), membership.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}, tenant, site, limit, r.URL.Query().Get("cursor"))
		if err != nil {
			membershipError(w, err)
			return
		}
		reply(w, 200, page)
	})
	// WIS-290 member directory: any active member reads the site's active members and their
	// display names. Like /memberships it is not audited; non-members get the scope 404.
	mux.HandleFunc("GET /v1/sites/{site}/members", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r, "limit", "cursor") {
			return
		}
		limit := 50
		if raw := r.URL.Query().Get("limit"); raw != "" {
			var err error
			limit, err = strconv.Atoi(raw)
			if err != nil || limit < 1 || limit > 100 {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		page, err := service.Directory(r.Context(), membership.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}, tenant, site, limit, r.URL.Query().Get("cursor"))
		if errors.Is(err, membership.ErrDenied) {
			problem(w, 404, "NOT_FOUND") // revoked between scope and read: still non-disclosing
			return
		}
		if err != nil {
			membershipError(w, err)
			return
		}
		reply(w, 200, page)
	})
	mux.HandleFunc("POST /v1/sites/{site}/membership-changes", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if err := auth.CheckCSRF(r, p); err != nil {
			problem(w, 403, "CSRF_REJECTED")
			return
		}
		if err := auth.RequireRecent(p); err != nil {
			problem(w, 428, "REAUTHENTICATION_REQUIRED")
			return
		}
		key, revision, ok := mutationHeaders(w, r)
		if !ok {
			return
		}
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			problem(w, 415, "UNSUPPORTED_MEDIA")
			return
		}
		var body struct {
			Issuer  string `json:"issuer"`
			Subject string `json:"subject"`
			Role    string `json:"role"`
			Active  *bool  `json:"active"`
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil || strictjson.Decode(raw, &body, limits.RequestBodyBytes) != nil || body.Active == nil || body.Issuer != p.Issuer || strings.TrimSpace(body.Subject) == "" {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		result, err := service.Change(r.Context(), membership.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}, membership.Request{TenantID: tenant, SiteID: site, TargetIssuer: body.Issuer, TargetSubject: body.Subject, Role: body.Role, Active: *body.Active, ExpectedRevision: revision, IdempotencyKey: key})
		if err != nil {
			membershipError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
		reply(w, 200, result)
	})
}

// agentAvailability is the one predicate for agent support: GET /v1/me reports it and the
// agent-turn and draft-request routes refuse with it, so the two cannot diverge.
func agentAvailability(agent config.Agent) (bool, store.AgentUnavailableReason) {
	if agent.Provider == "" {
		return false, store.AgentNotConfigured
	}
	if !agent.ModeEnabled {
		return false, store.AgentModeDisabled
	}
	return true, ""
}

func membershipError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, membership.ErrDenied):
		problem(w, 403, "PERMISSION_DENIED")
	case errors.Is(err, membership.ErrReauthentication):
		problem(w, 428, "REAUTHENTICATION_REQUIRED")
	case errors.Is(err, membership.ErrConflict):
		problem(w, 409, "IDEMPOTENCY_CONFLICT")
	case errors.Is(err, membership.ErrRevision):
		problem(w, 412, "REVISION_CONFLICT")
	case errors.Is(err, membership.ErrInvalid):
		problem(w, 422, "INVALID_REQUEST")
	default:
		problem(w, 503, "STORAGE_UNAVAILABLE")
	}
}
