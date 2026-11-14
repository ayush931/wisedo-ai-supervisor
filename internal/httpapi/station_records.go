package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
)

// registerStationRecords serves WIS-520 operator station records and the station
// authorizations that permit them. Tenant, site and actor come only from the session and
// path. A record takes Idempotency-Key (records are appended, never revised); a grant or
// revocation also takes If-Match on the grantee's current revision ("0" first). Owners and
// supervisors manage authorizations; only a member with a current authorization records;
// any active member reads.
func registerStationRecords(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool)) {
	service := workstation.NewStationRecords(db.DB).WithSessionPolicy(auth.Sessions())
	actor := func(p identity.Principal) workstation.Actor {
		return workstation.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
	}
	limitOf := func(w http.ResponseWriter, r *http.Request) (int, bool) {
		raw := r.URL.Query().Get("limit")
		if raw == "" {
			return 50, true
		}
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			problem(w, 422, "INVALID_REQUEST")
			return 0, false
		}
		return limit, true
	}
	base := "/v1/sites/{site}/workstations/{workstation}"
	mux.HandleFunc("POST "+base+"/records", func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if len(r.Header.Values("Idempotency-Key")) != 1 || len(key) < 1 || len(key) > 128 || strings.TrimSpace(key) != key {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		var body workstation.StationRecordBodyV1
		p, sc, ok := stationMutation(w, r, auth, scope, &body, workstation.CodeInvalidStationRecord)
		if !ok {
			return
		}
		out, err := service.Submit(r.Context(), actor(p), sc, r.PathValue("workstation"), body, key)
		if err != nil {
			shiftPlanError(w, err)
			return
		}
		reply(w, 201, out)
	})
	mux.HandleFunc("GET "+base+"/records", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r, "job_order_id", "limit", "cursor") {
			return
		}
		limit, ok := limitOf(w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		page, err := service.List(r.Context(), actor(p), workstation.Scope{TenantID: tenant, SiteID: site}, r.PathValue("workstation"), q.Get("job_order_id"), limit, q.Get("cursor"))
		if err != nil {
			shiftPlanError(w, err)
			return
		}
		reply(w, 200, page)
	})
	mux.HandleFunc("GET "+base+"/records/{record}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r) {
			return
		}
		out, err := service.Get(r.Context(), actor(p), workstation.Scope{TenantID: tenant, SiteID: site}, r.PathValue("workstation"), r.PathValue("record"))
		if err != nil {
			shiftPlanError(w, err)
			return
		}
		reply(w, 200, out)
	})
	mux.HandleFunc("POST "+base+"/authorizations", func(w http.ResponseWriter, r *http.Request) {
		key, expected, ok := mutationHeaders(w, r)
		if !ok {
			return
		}
		var body workstation.StationAuthorizationInputV1
		p, sc, ok := stationMutation(w, r, auth, scope, &body, workstation.CodeInvalidAuthorization)
		if !ok {
			return
		}
		out, err := service.Authorize(r.Context(), actor(p), sc, r.PathValue("workstation"), body, key, expected)
		if err != nil {
			shiftPlanError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(out.CurrentRevision, 10)+`"`)
		reply(w, 201, out)
	})
	mux.HandleFunc("GET "+base+"/authorizations", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r, "limit", "cursor") {
			return
		}
		limit, ok := limitOf(w, r)
		if !ok {
			return
		}
		page, err := service.Authorizations(r.Context(), actor(p), workstation.Scope{TenantID: tenant, SiteID: site}, r.PathValue("workstation"), limit, r.URL.Query().Get("cursor"))
		if err != nil {
			shiftPlanError(w, err)
			return
		}
		reply(w, 200, page)
	})
}
