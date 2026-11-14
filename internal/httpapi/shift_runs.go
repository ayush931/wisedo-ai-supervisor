package httpapi

import (
	"net/http"
	"strconv"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
)

// registerShiftRuns serves shift runs: start and end take Idempotency-Key and If-Match on the
// plan's state_revision and are for owners and supervisors with a recent authentication; any
// active member reads the run. Tenant, site and actor come only from the session and path.
func registerShiftRuns(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool)) {
	service := workstation.NewShiftRuns(db.DB).WithSessionPolicy(auth.Sessions())
	actor := func(p identity.Principal) workstation.Actor {
		return workstation.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
	}
	respond := func(w http.ResponseWriter, status int, run workstation.ShiftRunV1, err error) {
		if err != nil {
			shiftPlanError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(run.PlanStateRevision, 10)+`"`)
		reply(w, status, run)
	}
	base := "/v1/sites/{site}/shift-plans/{plan}/run"
	mux.HandleFunc("POST "+base, func(w http.ResponseWriter, r *http.Request) {
		key, expected, ok := mutationHeaders(w, r)
		if !ok {
			return
		}
		var body workstation.ShiftRunStartInputV1
		p, sc, ok := stationMutation(w, r, auth, scope, &body, workstation.CodeInvalidShiftRun)
		if !ok {
			return
		}
		run, err := service.Start(r.Context(), actor(p), sc, r.PathValue("plan"), body, key, expected)
		respond(w, 201, run, err)
	})
	mux.HandleFunc("POST "+base+"/end", func(w http.ResponseWriter, r *http.Request) {
		key, expected, ok := mutationHeaders(w, r)
		if !ok {
			return
		}
		var body workstation.ShiftRunEndInputV1
		p, sc, ok := stationMutation(w, r, auth, scope, &body, workstation.CodeInvalidShiftRun)
		if !ok {
			return
		}
		run, err := service.End(r.Context(), actor(p), sc, r.PathValue("plan"), body, key, expected)
		respond(w, 200, run, err)
	})
	mux.HandleFunc("GET "+base, func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r) {
			return
		}
		run, err := service.Get(r.Context(), actor(p), workstation.Scope{TenantID: tenant, SiteID: site}, r.PathValue("plan"))
		respond(w, 200, run, err)
	})
}
