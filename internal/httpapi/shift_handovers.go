package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
)

// registerShiftHandovers serves WIS-520 next-shift handover drafts. Tenant, site and actor
// come only from the session and path. Generating and adopting take Idempotency-Key and are
// for owners and supervisors; any active member reads. Adopting takes an optional If-Match:
// absent, it creates a new draft plan; present, it revises the plan for exactly the proposed
// line and shift at that state revision. It never approves.
func registerShiftHandovers(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool)) {
	service := workstation.NewShiftHandovers(db.DB).WithSessionPolicy(auth.Sessions())
	actor := func(p identity.Principal) workstation.Actor {
		return workstation.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
	}
	idempotencyKey := func(w http.ResponseWriter, r *http.Request) (string, bool) {
		key := r.Header.Get("Idempotency-Key")
		if len(r.Header.Values("Idempotency-Key")) != 1 || len(key) < 1 || len(key) > 128 || strings.TrimSpace(key) != key {
			problem(w, 422, "INVALID_REQUEST")
			return "", false
		}
		return key, true
	}
	base := "/v1/sites/{site}/shift-plans/{plan}/handovers"
	mux.HandleFunc("POST "+base, func(w http.ResponseWriter, r *http.Request) {
		key, ok := idempotencyKey(w, r)
		if !ok {
			return
		}
		var body workstation.ShiftHandoverRequestV1
		p, sc, ok := stationMutation(w, r, auth, scope, &body, workstation.CodeInvalidShiftHandover)
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
	mux.HandleFunc("POST "+base+"/{revision}/adopt", func(w http.ResponseWriter, r *http.Request) {
		key, ok := idempotencyKey(w, r)
		if !ok {
			return
		}
		var expected int64
		if values := r.Header.Values("If-Match"); len(values) > 0 {
			tag := values[0]
			var err error
			if len(values) != 1 || len(tag) < 3 || tag[0] != '"' || tag[len(tag)-1] != '"' {
				problem(w, 422, "INVALID_REVISION")
				return
			}
			if expected, err = strconv.ParseInt(tag[1:len(tag)-1], 10, 64); err != nil || expected < 1 || tag != `"`+strconv.FormatInt(expected, 10)+`"` {
				problem(w, 422, "INVALID_REVISION")
				return
			}
		}
		var body workstation.ShiftHandoverAdoptInputV1
		p, sc, ok := stationMutation(w, r, auth, scope, &body, workstation.CodeInvalidShiftHandover)
		if !ok {
			return
		}
		// A malformed revision names no draft: the same non-disclosing 404.
		revision, _ := strconv.ParseInt(r.PathValue("revision"), 10, 64)
		plan, err := service.Adopt(r.Context(), actor(p), sc, r.PathValue("plan"), revision, body, key, expected)
		if err != nil {
			shiftPlanError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(plan.StateRevision, 10)+`"`)
		reply(w, 201, plan)
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
		revision, _ := strconv.ParseInt(r.PathValue("revision"), 10, 64)
		out, err := service.Get(r.Context(), actor(p), workstation.Scope{TenantID: tenant, SiteID: site}, r.PathValue("plan"), revision)
		if err != nil {
			shiftPlanError(w, err)
			return
		}
		reply(w, 200, out)
	})
}
