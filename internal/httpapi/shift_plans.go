package httpapi

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
)

// registerShiftPlans serves WIS-520 shift plans. Tenant, site and actor come only from the
// authenticated session and path; revise and approve require If-Match on state_revision.
func registerShiftPlans(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool), solverPaths ...string) {
	service := workstation.NewShiftPlans(db.DB).WithSessionPolicy(auth.Sessions())
	actor := func(p identity.Principal) workstation.Actor {
		return workstation.Actor{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
	}
	mutation := func(w http.ResponseWriter, r *http.Request, dst any) (identity.Principal, workstation.Scope, bool) {
		return stationMutation(w, r, auth, scope, dst, "INVALID_SHIFT_PLAN")
	}
	respond := func(w http.ResponseWriter, status int, plan workstation.ShiftPlanV1, err error) {
		if err != nil {
			shiftPlanError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(plan.StateRevision, 10)+`"`)
		reply(w, status, plan)
	}
	base := "/v1/sites/{site}/shift-plans"
	solverPath := "/usr/local/bin/supervisor_workstation_cpsat"
	if len(solverPaths) > 0 && solverPaths[0] != "" {
		solverPath = solverPaths[0]
	}
	// Like profile-previews, this is a CSRF-protected computation, not a mutation:
	// no durable effects or idempotency receipt, so no Idempotency-Key is required.
	mux.HandleFunc("POST "+base+"/suggestions", func(w http.ResponseWriter, r *http.Request) {
		var body workstation.ShiftPlanSelectionV1
		p, sc, ok := stationMutation(w, r, auth, scope, &body, "INVALID_PLAN_SELECTION")
		if !ok {
			return
		}
		out, err := service.Suggest(r.Context(), actor(p), sc, body, solverPath)
		if err != nil {
			shiftPlanError(w, err)
			return
		}
		reply(w, 200, out)
	})
	mux.HandleFunc("POST "+base, func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if len(r.Header.Values("Idempotency-Key")) != 1 || len(key) < 1 || len(key) > 128 || strings.TrimSpace(key) != key {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		var body workstation.ShiftPlanBodyV1
		p, sc, ok := mutation(w, r, &body)
		if !ok {
			return
		}
		plan, err := service.Create(r.Context(), actor(p), sc, body, key)
		respond(w, 201, plan, err)
	})
	mux.HandleFunc("POST "+base+"/{plan}/revisions", func(w http.ResponseWriter, r *http.Request) {
		key, expected, ok := mutationHeaders(w, r)
		if !ok {
			return
		}
		var body workstation.ShiftPlanBodyV1
		p, sc, ok := mutation(w, r, &body)
		if !ok {
			return
		}
		plan, err := service.Revise(r.Context(), actor(p), sc, r.PathValue("plan"), body, key, expected)
		respond(w, 201, plan, err)
	})
	mux.HandleFunc("POST "+base+"/{plan}/approvals", func(w http.ResponseWriter, r *http.Request) {
		key, expected, ok := mutationHeaders(w, r)
		if !ok {
			return
		}
		var body workstation.ShiftPlanApproveInputV1
		p, sc, ok := mutation(w, r, &body)
		if !ok {
			return
		}
		plan, err := service.Approve(r.Context(), actor(p), sc, r.PathValue("plan"), body, key, expected)
		respond(w, 200, plan, err)
	})
	mux.HandleFunc("GET "+base+"/{plan}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r) {
			return
		}
		plan, err := service.Get(r.Context(), actor(p), workstation.Scope{TenantID: tenant, SiteID: site}, r.PathValue("plan"))
		respond(w, 200, plan, err)
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
	// The approved plan in force for a line; at defaults to the server clock.
	mux.HandleFunc("GET "+base+"/current", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r, "line_id", "at") {
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
		current, err := service.Current(r.Context(), actor(p), workstation.Scope{TenantID: tenant, SiteID: site}, r.URL.Query().Get("line_id"), at)
		if err != nil {
			shiftPlanError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(current.StateRevision, 10)+`"`)
		reply(w, 200, current)
	})
}

// stationMutation authenticates, checks CSRF, media type and query, and strictly decodes the
// body (unknown fields, duplicate keys and trailing values are rejected as invalid).
func stationMutation(w http.ResponseWriter, r *http.Request, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool), dst any, invalid string) (identity.Principal, workstation.Scope, bool) {
	p, tenant, site, ok := scope(w, r)
	if !ok {
		return p, workstation.Scope{}, false
	}
	if err := auth.CheckCSRF(r, p); err != nil {
		problem(w, 403, "CSRF_REJECTED")
		return p, workstation.Scope{}, false
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		problem(w, 415, "UNSUPPORTED_MEDIA")
		return p, workstation.Scope{}, false
	}
	if r.URL.RawQuery != "" {
		problem(w, 422, "INVALID_REQUEST")
		return p, workstation.Scope{}, false
	}
	data, err := io.ReadAll(r.Body)
	if err != nil || strictjson.Decode(data, dst, limits.RequestBodyBytes) != nil {
		problem(w, 422, invalid)
		return p, workstation.Scope{}, false
	}
	return p, workstation.Scope{TenantID: tenant, SiteID: site}, true
}

// shiftPlanError maps WIS-520 shift plan and job order errors to stable problem codes.
func shiftPlanError(w http.ResponseWriter, err error) {
	var config *workstation.ConfigError
	var unavailable *workstation.InstructionUnavailableError
	switch {
	case errors.Is(err, workstation.ErrInvalidPlanSelection):
		problem(w, 422, "INVALID_PLAN_SELECTION")
	case errors.Is(err, workstation.ErrPlannerLimit):
		problem(w, 422, "PLANNER_LIMIT_EXCEEDED")
	case errors.Is(err, workstation.ErrPlannerData):
		problem(w, 422, "PLANNER_DATA_UNAVAILABLE")
	case errors.Is(err, workstation.ErrNoFeasibleSchedule):
		problem(w, 422, "NO_FEASIBLE_SCHEDULE")
	case errors.Is(err, workstation.ErrPlannerBusy):
		problem(w, 409, "PLANNER_BUSY")
	case errors.As(err, &config):
		problemAt(w, 422, config.Code, config.Path)
	case errors.As(err, &unavailable):
		problemAt(w, 409, "INSTRUCTION_UNAVAILABLE", unavailable.JobOrderID)
	case errors.Is(err, workstation.ErrProfileUnchanged):
		problem(w, 409, "STATION_PROFILE_UNCHANGED")
	case errors.Is(err, workstation.ErrJobOrderNotAssigned):
		problem(w, 409, "JOB_ORDER_NOT_ASSIGNED")
	case errors.Is(err, workstation.ErrStationNotAuthorized):
		problem(w, 403, "STATION_NOT_AUTHORIZED")
	case errors.Is(err, workstation.ErrSelfAuthorization):
		problem(w, 403, "SELF_AUTHORIZATION_FORBIDDEN")
	case errors.Is(err, workstation.ErrGranteeNotEligible):
		problem(w, 422, "GRANTEE_NOT_ELIGIBLE")
	case errors.Is(err, workstation.ErrAuthorizationUnchanged):
		problem(w, 409, "STATION_AUTHORIZATION_UNCHANGED")
	case errors.Is(err, workstation.ErrStationNotConfigured):
		problem(w, 409, "STATION_NOT_CONFIGURED")
	case errors.Is(err, workstation.ErrStationFormConflict):
		problem(w, 409, "STATION_FORM_CONFLICT")
	case errors.Is(err, workstation.ErrNoJobOrderToRecord):
		problem(w, 409, "NO_JOB_ORDER_TO_RECORD")
	case errors.Is(err, workstation.ErrNeedsReplan):
		problem(w, 409, "NEEDS_REPLAN")
	case errors.Is(err, workstation.ErrInstructionRevoked):
		problem(w, 409, "INSTRUCTION_REVOKED")
	case errors.Is(err, workstation.ErrInstructionUnavailable):
		problem(w, 409, "INSTRUCTION_UNAVAILABLE")
	case errors.Is(err, workstation.ErrLifecycle):
		problem(w, 409, "LIFECYCLE_CONFLICT")
	case errors.Is(err, workstation.ErrStationJobCompleted):
		problem(w, 409, "STATION_JOB_COMPLETED")
	case errors.Is(err, workstation.ErrCorrectionNotPermitted):
		problem(w, 403, "CORRECTION_NOT_PERMITTED")
	case errors.Is(err, workstation.ErrShiftNotStarted):
		problem(w, 409, "SHIFT_NOT_STARTED")
	case errors.Is(err, workstation.ErrShiftRunAlreadyStarted):
		problem(w, 409, "SHIFT_RUN_ALREADY_STARTED")
	case errors.Is(err, workstation.ErrShiftRunEnded):
		problem(w, 409, "SHIFT_RUN_ENDED")
	case errors.Is(err, workstation.ErrShiftWindowEnded):
		problem(w, 409, "SHIFT_WINDOW_ENDED")
	case errors.Is(err, workstation.ErrShiftStartTooEarly):
		problem(w, 409, "SHIFT_START_TOO_EARLY")
	case errors.Is(err, workstation.ErrPlanNotApproved):
		problem(w, 409, "SHIFT_PLAN_NOT_APPROVED")
	case errors.Is(err, workstation.ErrSummaryUnchanged):
		problem(w, 409, "SHIFT_SUMMARY_UNCHANGED")
	case errors.Is(err, workstation.ErrHandoverSuperseded):
		problem(w, 409, "HANDOVER_SUPERSEDED")
	case errors.Is(err, workstation.ErrHandoverSourceStale):
		problem(w, 409, "HANDOVER_SOURCE_STALE")
	case errors.Is(err, workstation.ErrHandoverUnchanged):
		problem(w, 409, "SHIFT_HANDOVER_UNCHANGED")
	case errors.Is(err, workstation.ErrHandoverAdopted):
		problem(w, 409, "SHIFT_HANDOVER_ALREADY_ADOPTED")
	case errors.Is(err, workstation.ErrHandoverNoPlan):
		problem(w, 409, "SHIFT_HANDOVER_NO_PROPOSAL")
	case errors.Is(err, workstation.ErrCorrectionConflict):
		problem(w, 409, "CORRECTION_CONFLICT")
	case errors.Is(err, workstation.ErrNotFound):
		problem(w, 404, "NOT_FOUND")
	case errors.Is(err, workstation.ErrNoCurrentPlan):
		problem(w, 404, "NO_APPROVED_SHIFT_PLAN")
	case errors.Is(err, workstation.ErrSelfApproval):
		problem(w, 403, "SELF_REVIEW_FORBIDDEN")
	case errors.Is(err, workstation.ErrDenied):
		problem(w, 403, "PERMISSION_DENIED")
	case errors.Is(err, workstation.ErrReauthentication):
		problem(w, 428, "REAUTHENTICATION_REQUIRED")
	case errors.Is(err, workstation.ErrConflict):
		problem(w, 409, "IDEMPOTENCY_CONFLICT")
	case errors.Is(err, workstation.ErrShiftTaken):
		problem(w, 409, "SHIFT_PLAN_EXISTS")
	case errors.Is(err, workstation.ErrUnchanged):
		problem(w, 409, "SHIFT_PLAN_UNCHANGED")
	case errors.Is(err, workstation.ErrAlreadyApproved):
		problem(w, 409, "SHIFT_PLAN_ALREADY_APPROVED")
	case errors.Is(err, workstation.ErrRevision), errors.Is(err, workstation.ErrDigestMismatch):
		problem(w, 412, "REVISION_CONFLICT")
	case errors.Is(err, workstation.ErrInvalidShiftPlan):
		problem(w, 422, "INVALID_SHIFT_PLAN")
	case errors.Is(err, workstation.ErrInvalidJobOrder):
		problem(w, 422, "INVALID_JOB_ORDER")
	case errors.Is(err, workstation.ErrInstructionNotApproved):
		problem(w, 422, "INSTRUCTION_NOT_APPROVED")
	case errors.Is(err, workstation.ErrJobOrderUnchanged):
		problem(w, 409, "JOB_ORDER_UNCHANGED")
	case errors.Is(err, workstation.ErrJobOrderUnknown):
		problem(w, 422, "JOB_ORDER_REVISION_UNKNOWN")
	case errors.Is(err, workstation.ErrJobOrderNotCurrent):
		problem(w, 409, "JOB_ORDER_NOT_CURRENT")
	case errors.Is(err, workstation.ErrJobOrderMismatch):
		problem(w, 422, "JOB_ORDER_MISMATCH")
	case errors.Is(err, workstation.ErrNoCurrentJobOrder):
		problem(w, 404, "NO_CURRENT_JOB_ORDER")
	case errors.Is(err, workstation.ErrAssignmentConflict):
		problem(w, 409, "WORKSTATION_ASSIGNMENT_CONFLICT")
	default:
		problem(w, 503, "STORAGE_UNAVAILABLE")
	}
}
