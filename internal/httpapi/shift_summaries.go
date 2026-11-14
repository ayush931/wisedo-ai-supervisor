package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
)

// registerShiftSummaries serves WIS-520 shift summary snapshots. Tenant, site and actor come
// only from the session and path. Generating takes Idempotency-Key and is for owners and
// supervisors; any active member reads.
func registerShiftSummaries(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool)) {
	service := workstation.NewShiftSummaries(db.DB).WithSessionPolicy(auth.Sessions())
	actor := func(p identity.Principal) workstation.Actor {
		return workstation.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
	}
	base := "/v1/sites/{site}/shift-plans/{plan}/summaries"
	mux.HandleFunc("POST "+base, func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if len(r.Header.Values("Idempotency-Key")) != 1 || len(key) < 1 || len(key) > 128 || strings.TrimSpace(key) != key {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		var body workstation.ShiftSummaryRequestV1
		p, sc, ok := stationMutation(w, r, auth, scope, &body, workstation.CodeInvalidShiftSummary)
		if !ok {
			return
		}
		out, err := service.Generate(r.Context(), actor(p), sc, r.PathValue("plan"), body, key)
		if err != nil {
			shiftPlanError(w, err)
			return
		}
		reply(w, 201, out)
	})
	mux.HandleFunc("GET "+base, func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r, "limit", "cursor") {
			return
		}
		limit := 50
		if raw := r.URL.Query().Get("limit"); raw != "" {
			var err error
			if limit, err = strconv.Atoi(raw); err != nil || limit < 1 || limit > 100 {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		page, err := service.List(r.Context(), actor(p), workstation.Scope{TenantID: tenant, SiteID: site}, r.PathValue("plan"), limit, r.URL.Query().Get("cursor"))
		if err != nil {
			shiftPlanError(w, err)
			return
		}
		reply(w, 200, page)
	})
	mux.HandleFunc("GET "+base+"/{revision}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r) {
			return
		}
		// A malformed revision names no snapshot: the same non-disclosing 404.
		revision, _ := strconv.ParseInt(r.PathValue("revision"), 10, 64)
		out, err := service.Get(r.Context(), actor(p), workstation.Scope{TenantID: tenant, SiteID: site}, r.PathValue("plan"), revision)
		if err != nil {
			shiftPlanError(w, err)
			return
		}
		reply(w, 200, out)
	})
}
