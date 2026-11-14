package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
)

// registerStationProfiles serves WIS-520 station profiles: the operator screen configured per
// workstation. Tenant, site and actor come only from the session and path. Revisions take
// Idempotency-Key and If-Match on current_revision ("0" creates); owners and supervisors
// write, any active member reads and previews. A rejected configuration is 422 with a
// stable code and the offending member path.
func registerStationProfiles(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool)) {
	service := workstation.NewStationProfiles(db.DB).WithSessionPolicy(auth.Sessions())
	actor := func(p identity.Principal) workstation.Actor {
		return workstation.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
	}
	// body runs the shared mutation checks and keeps the raw document, so the workstation
	// decoder can name an unsupported member instead of a generic strict-JSON failure.
	body := func(w http.ResponseWriter, r *http.Request) (identity.Principal, workstation.Scope, []byte, bool) {
		var raw json.RawMessage
		p, sc, ok := stationMutation(w, r, auth, scope, &raw, workstation.CodeInvalidStationProfile)
		return p, sc, raw, ok
	}
	respond := func(w http.ResponseWriter, status int, profile workstation.StationProfileV1, err error) {
		if err != nil {
			shiftPlanError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(profile.CurrentRevision, 10)+`"`)
		reply(w, status, profile)
	}
	base := "/v1/sites/{site}/workstations"
	mux.HandleFunc("POST "+base+"/{workstation}/profile-revisions", func(w http.ResponseWriter, r *http.Request) {
		key, expected, ok := mutationHeaders(w, r)
		if !ok || !directKey(w, key) {
			return
		}
		p, sc, raw, ok := body(w, r)
		if !ok {
			return
		}
		profile, err := workstation.DecodeStationProfile(raw)
		if err != nil {
			shiftPlanError(w, err)
			return
		}
		out, err := service.Revise(r.Context(), actor(p), sc, r.PathValue("workstation"), profile, key, expected)
		respond(w, 201, out, err)
	})
	mux.HandleFunc("GET "+base+"/{workstation}/profile", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r) {
			return
		}
		out, err := service.Get(r.Context(), actor(p), workstation.Scope{TenantID: tenant, SiteID: site}, r.PathValue("workstation"))
		respond(w, 200, out, err)
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
		page, err := service.List(r.Context(), actor(p), workstation.Scope{TenantID: tenant, SiteID: site}, limit, r.URL.Query().Get("cursor"))
		if err != nil {
			shiftPlanError(w, err)
			return
		}
		reply(w, 200, page)
	})
	// A preview persists nothing, so it takes no Idempotency-Key; it is still CSRF-checked.
	mux.HandleFunc("POST "+base+"/{workstation}/profile-previews", func(w http.ResponseWriter, r *http.Request) {
		p, sc, raw, ok := body(w, r)
		if !ok {
			return
		}
		in, err := workstation.DecodeStationProfilePreview(raw)
		if err != nil {
			shiftPlanError(w, err)
			return
		}
		out, err := service.Preview(r.Context(), actor(p), sc, r.PathValue("workstation"), in)
		if err != nil {
			shiftPlanError(w, err)
			return
		}
		reply(w, 200, out)
	})
}
