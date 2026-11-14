package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
)

// registerJobOrders serves WIS-520 job orders and their workstation delivery. Tenant, site
// and actor come only from the authenticated session and path; revise requires If-Match on
// current_revision. Writers are owners and supervisors; any active member may read.
func registerJobOrders(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool)) {
	service := workstation.NewJobOrders(db.DB).WithSessionPolicy(auth.Sessions())
	actor := func(p identity.Principal) workstation.Actor {
		return workstation.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
	}
	respond := func(w http.ResponseWriter, status int, order workstation.JobOrderV1, err error) {
		if err != nil {
			shiftPlanError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(order.CurrentRevision, 10)+`"`)
		reply(w, status, order)
	}
	base := "/v1/sites/{site}/job-orders"
	mux.HandleFunc("POST "+base, func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if len(r.Header.Values("Idempotency-Key")) != 1 || len(key) < 1 || len(key) > 128 || strings.TrimSpace(key) != key {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		var body workstation.JobOrderBodyV1
		p, sc, ok := stationMutation(w, r, auth, scope, &body, "INVALID_JOB_ORDER")
		if !ok {
			return
		}
		order, err := service.Create(r.Context(), actor(p), sc, body, key)
		respond(w, 201, order, err)
	})
	mux.HandleFunc("POST "+base+"/{job_order}/revisions", func(w http.ResponseWriter, r *http.Request) {
		key, expected, ok := mutationHeaders(w, r)
		if !ok {
			return
		}
		var body workstation.JobOrderBodyV1
		p, sc, ok := stationMutation(w, r, auth, scope, &body, "INVALID_JOB_ORDER")
		if !ok {
			return
		}
		order, err := service.Revise(r.Context(), actor(p), sc, r.PathValue("job_order"), body, key, expected)
		respond(w, 201, order, err)
	})
	mux.HandleFunc("GET "+base+"/{job_order}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r) {
			return
		}
		order, err := service.Get(r.Context(), actor(p), workstation.Scope{TenantID: tenant, SiteID: site}, r.PathValue("job_order"))
		respond(w, 200, order, err)
	})
	mux.HandleFunc("GET "+base, func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r, "limit", "cursor", "include") {
			return
		}
		query := r.URL.Query()
		includeCurrent := false
		if values, exists := query["include"]; exists {
			if len(values) != 1 || values[0] != "current" {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
			includeCurrent = true
		}
		limit := 50
		if raw := query.Get("limit"); raw != "" {
			var err error
			if limit, err = strconv.Atoi(raw); err != nil || limit < 1 || limit > 100 {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		page, err := service.List(r.Context(), actor(p), workstation.Scope{TenantID: tenant, SiteID: site}, limit, query.Get("cursor"), includeCurrent)
		if err != nil {
			shiftPlanError(w, err)
			return
		}
		reply(w, 200, page)
	})
	// What a workstation shows now (or at the given instant); a stale pin yields needs_replan
	// with a null order instead of the superseded revision.
	mux.HandleFunc("GET /v1/sites/{site}/workstations/{workstation}/current-job-order", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r, "at") {
			return
		}
		var at *time.Time
		if raw := r.URL.Query().Get("at"); raw != "" {
			t, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
			at = &t
		}
		current, err := service.Current(r.Context(), actor(p), workstation.Scope{TenantID: tenant, SiteID: site}, r.PathValue("workstation"), at)
		if err != nil {
			shiftPlanError(w, err)
			return
		}
		reply(w, 200, current)
	})
}
