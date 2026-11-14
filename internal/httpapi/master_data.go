package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
)

type masterCreate[T any, E any] func(*workstation.MasterDataService, context.Context, workstation.Actor, workstation.Scope, T, string, int64) (E, error)
type masterRevise[T any, E any] func(*workstation.MasterDataService, context.Context, workstation.Actor, workstation.Scope, string, T, string, int64) (E, error)
type masterGet[E any] func(*workstation.MasterDataService, context.Context, workstation.Actor, workstation.Scope, string) (E, error)
type masterList[P any] func(*workstation.MasterDataService, context.Context, workstation.Actor, workstation.Scope, int, string) (P, error)

type masterResource[T any, E any, P any] struct {
	name     string
	base     string
	create   masterCreate[T, E]
	revise   masterRevise[T, E]
	get      masterGet[E]
	list     masterList[P]
	decode   func([]byte) (T, error)
	revision func(E) int64
}

func registerMasterResource[T any, E any, P any](mux *http.ServeMux, service *workstation.MasterDataService, auth *identity.Service,
	scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool), resource masterResource[T, E, P]) {
	actor := func(p identity.Principal) workstation.Actor {
		return workstation.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
	}
	body := func(w http.ResponseWriter, r *http.Request) (identity.Principal, workstation.Scope, T, string, int64, bool) {
		var zero T
		key, expected, ok := mutationHeaders(w, r)
		if !ok || !directKey(w, key) {
			return identity.Principal{}, workstation.Scope{}, zero, "", 0, false
		}
		var raw json.RawMessage
		p, sc, ok := stationMutation(w, r, auth, scope, &raw, "INVALID_MASTER_DATA")
		if !ok {
			return p, sc, zero, "", 0, false
		}
		value, err := resource.decode(raw)
		if err != nil {
			masterDataError(w, err)
			return p, sc, zero, "", 0, false
		}
		return p, sc, value, key, expected, true
	}
	respond := func(w http.ResponseWriter, status int, value E, err error) {
		if err != nil {
			masterDataError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(resource.revision(value), 10)+`"`)
		reply(w, status, value)
	}
	mux.HandleFunc("POST "+resource.base, func(w http.ResponseWriter, r *http.Request) {
		p, sc, value, key, expected, ok := body(w, r)
		if !ok {
			return
		}
		out, err := resource.create(service, r.Context(), actor(p), sc, value, key, expected)
		respond(w, http.StatusCreated, out, err)
	})
	mux.HandleFunc("POST "+resource.base+"/{id}/revisions", func(w http.ResponseWriter, r *http.Request) {
		p, sc, value, key, expected, ok := body(w, r)
		if !ok {
			return
		}
		out, err := resource.revise(service, r.Context(), actor(p), sc, r.PathValue("id"), value, key, expected)
		respond(w, http.StatusCreated, out, err)
	})
	mux.HandleFunc("GET "+resource.base+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r) {
			return
		}
		out, err := resource.get(service, r.Context(), actor(p), workstation.Scope{TenantID: tenant, SiteID: site}, r.PathValue("id"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("GET "+resource.base+"/{id}/revisions", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r) {
			return
		}
		out, err := resource.get(service, r.Context(), actor(p), workstation.Scope{TenantID: tenant, SiteID: site}, r.PathValue("id"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("GET "+resource.base, func(w http.ResponseWriter, r *http.Request) {
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
		page, err := resource.list(service, r.Context(), actor(p), workstation.Scope{TenantID: tenant, SiteID: site}, limit, r.URL.Query().Get("cursor"))
		if err != nil {
			masterDataError(w, err)
			return
		}
		reply(w, http.StatusOK, page)
	})
}

func masterDataError(w http.ResponseWriter, err error) {
	var invalid *workstation.MasterDataValidationError
	switch {
	case errors.As(err, &invalid):
		problemAt(w, 422, "INVALID_MASTER_DATA", invalid.Path)
	case errors.Is(err, workstation.ErrLineValidationBlocked):
		problem(w, 409, "LINE_VALIDATION_BLOCKED")
	case errors.Is(err, workstation.ErrMasterDataInUse):
		problem(w, 409, "MASTER_DATA_IN_USE")
	case errors.Is(err, workstation.ErrNotFound):
		problem(w, 404, "NOT_FOUND")
	case errors.Is(err, workstation.ErrDenied):
		problem(w, 403, "PERMISSION_DENIED")
	case errors.Is(err, workstation.ErrConflict):
		problem(w, 409, "IDEMPOTENCY_CONFLICT")
	case errors.Is(err, workstation.ErrRevision):
		problem(w, 412, "REVISION_CONFLICT")
	case errors.Is(err, workstation.ErrMasterDataUnchanged):
		problem(w, 409, "MASTER_DATA_UNCHANGED")
	case errors.Is(err, workstation.ErrInvalidShiftPlan):
		problem(w, 422, "INVALID_REQUEST")
	default:
		problem(w, 503, "STORAGE_UNAVAILABLE")
	}
}

func decodeMasterBody[T any](kind string, raw []byte) (T, error) {
	var zero T
	value, err := workstation.DecodeMasterDataBody(kind, raw)
	if err != nil {
		return zero, err
	}
	body, ok := value.(*T)
	if !ok {
		return zero, &workstation.MasterDataValidationError{}
	}
	return *body, nil
}

func registerMasterData(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool)) {
	service := workstation.NewMasterData(db.DB).WithSessionPolicy(auth.Sessions())
	registerMasterResource(mux, service, auth, scope, masterResource[workstation.LineBodyV1, workstation.LineV1, workstation.LinePageV1]{
		name: "lines", base: "/v1/sites/{site}/lines", decode: func(b []byte) (workstation.LineBodyV1, error) {
			return decodeMasterBody[workstation.LineBodyV1]("line", b)
		},
		create: (*workstation.MasterDataService).CreateLine, revise: (*workstation.MasterDataService).ReviseLine, get: (*workstation.MasterDataService).GetLine, list: (*workstation.MasterDataService).ListLines, revision: func(v workstation.LineV1) int64 { return v.CurrentRevision },
	})
	registerMasterResource(mux, service, auth, scope, masterResource[workstation.ItemBodyV1, workstation.ItemV1, workstation.ItemPageV1]{
		name: "items", base: "/v1/sites/{site}/items", decode: func(b []byte) (workstation.ItemBodyV1, error) {
			return decodeMasterBody[workstation.ItemBodyV1]("item", b)
		},
		create: (*workstation.MasterDataService).CreateItem, revise: (*workstation.MasterDataService).ReviseItem, get: (*workstation.MasterDataService).GetItem, list: (*workstation.MasterDataService).ListItems, revision: func(v workstation.ItemV1) int64 { return v.CurrentRevision },
	})
	registerMasterResource(mux, service, auth, scope, masterResource[workstation.RoutingBodyV1, workstation.RoutingV1, workstation.RoutingPageV1]{
		name: "routings", base: "/v1/sites/{site}/routings", decode: func(b []byte) (workstation.RoutingBodyV1, error) {
			return decodeMasterBody[workstation.RoutingBodyV1]("routing", b)
		},
		create: (*workstation.MasterDataService).CreateRouting, revise: (*workstation.MasterDataService).ReviseRouting, get: (*workstation.MasterDataService).GetRouting, list: (*workstation.MasterDataService).ListRoutings, revision: func(v workstation.RoutingV1) int64 { return v.CurrentRevision },
	})
	registerMasterResource(mux, service, auth, scope, masterResource[workstation.ShiftCalendarBodyV1, workstation.ShiftCalendarV1, workstation.ShiftCalendarPageV1]{
		name: "shift calendars", base: "/v1/sites/{site}/shift-calendars", decode: func(b []byte) (workstation.ShiftCalendarBodyV1, error) {
			return decodeMasterBody[workstation.ShiftCalendarBodyV1]("shift_calendar", b)
		},
		create: (*workstation.MasterDataService).CreateShiftCalendar, revise: (*workstation.MasterDataService).ReviseShiftCalendar, get: (*workstation.MasterDataService).GetShiftCalendar, list: (*workstation.MasterDataService).ListShiftCalendars, revision: func(v workstation.ShiftCalendarV1) int64 { return v.CurrentRevision },
	})
	mux.HandleFunc("POST /v1/sites/{site}/master-data-settings/revisions", func(w http.ResponseWriter, r *http.Request) {
		key, expected, ok := mutationHeaders(w, r)
		if !ok || !directKey(w, key) {
			return
		}
		var raw json.RawMessage
		p, sc, ok := stationMutation(w, r, auth, scope, &raw, "INVALID_MASTER_DATA")
		if !ok {
			return
		}
		body, err := decodeMasterBody[workstation.SiteMasterDataSettingsBodyV1]("site_settings", raw)
		if err != nil {
			masterDataError(w, err)
			return
		}
		out, err := service.ReviseSettings(r.Context(), workstation.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}, sc, body, key, expected)
		if err != nil {
			masterDataError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(out.CurrentRevision, 10)+`"`)
		reply(w, http.StatusCreated, out)
	})
	mux.HandleFunc("GET /v1/sites/{site}/master-data-settings", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r) {
			return
		}
		out, err := service.GetSettings(r.Context(), workstation.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}, workstation.Scope{TenantID: tenant, SiteID: site})
		if err != nil {
			masterDataError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(out.CurrentRevision, 10)+`"`)
		reply(w, http.StatusOK, out)
	})
}
