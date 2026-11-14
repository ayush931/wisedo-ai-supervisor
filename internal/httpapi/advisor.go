package httpapi

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/advisor"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/cases"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
)

// registerAdvisor serves the read-only WIS-629 advisor suggestion of one case. Any active site
// member who may read the case reads it. It is assembled on read from the case, its pinned
// approved snapshot and the latest stored investigation output; it writes nothing and offers no
// action (rule 05). SUPERVISOR_ADVISOR_SUGGESTIONS_DISABLED=true switches it off.
func registerAdvisor(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool), cfg config.Config) {
	service := cases.NewService(db.DB).WithSessionPolicy(auth.Sessions())
	mux.HandleFunc("GET /v1/sites/{site}/cases/{case}/suggestion", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if r.URL.RawQuery != "" {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		if cfg.AdvisorSuggestionsDisabled {
			problem(w, 404, "ADVISOR_DISABLED")
			return
		}
		var out advisor.SuggestionV1
		err := service.ReadTx(r.Context(), cases.Principal{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}, cases.Scope{TenantID: tenant, SiteID: site}, func(tx *sql.Tx) error {
			var e error
			out, e = advisor.LoadTx(r.Context(), tx, agent.RunScope{TenantID: tenant, SiteID: site}, r.PathValue("case"))
			return e
		})
		if errors.Is(err, advisor.ErrNotFound) {
			problem(w, 404, "NOT_FOUND")
			return
		}
		if err != nil {
			caseError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		reply(w, 200, out)
	})
}
