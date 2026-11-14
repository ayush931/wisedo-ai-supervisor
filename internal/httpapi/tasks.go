package httpapi

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/tasks"
)

// registerTasks serves the WIS-277 internal tasks. There is no route that creates a task: only
// the deterministic executor does, inside the transaction that consumes an approval. An operator
// reads only the tasks assigned to them (another's is the non-disclosing 404, WIS-634); other
// active members read the site's tasks, as with cases.
func registerTasks(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool)) {
	service := tasks.NewService(db.DB).WithSessionPolicy(auth.Sessions())
	principal := func(p identity.Principal) tasks.Principal {
		return tasks.Principal{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
	}
	base := "/v1/sites/{site}/tasks"
	mux.HandleFunc("GET "+base, func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r, "case_id", "limit", "cursor") {
			return
		}
		limit := tasks.DefaultPageSize
		if raw := r.URL.Query().Get("limit"); raw != "" {
			var err error
			if limit, err = strconv.Atoi(raw); err != nil || limit < 1 || limit > tasks.MaxPageSize {
				problem(w, 422, "INVALID_REQUEST")
				return
			}
		}
		q := r.URL.Query()
		page, err := service.List(r.Context(), principal(p), tasks.Scope{TenantID: tenant, SiteID: site}, q.Get("case_id"), q.Get("cursor"), limit)
		if err != nil {
			taskError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		reply(w, 200, page)
	})
	mux.HandleFunc("GET "+base+"/{task}", func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok {
			return
		}
		if r.URL.RawQuery != "" {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		task, err := service.Get(r.Context(), principal(p), tasks.Scope{TenantID: tenant, SiteID: site}, r.PathValue("task"))
		if err != nil {
			taskError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("ETag", `"`+strconv.FormatInt(task.Revision, 10)+`"`)
		reply(w, 200, task)
	})

	// Lifecycle: the assignee acknowledges, starts and completes; a recently authenticated site
	// supervisor blocks, cancels and reopens. Each needs CSRF, a UUID Idempotency-Key and If-Match
	// equal to expected_revision.
	decode := func(w http.ResponseWriter, r *http.Request, dst any) bool {
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			problem(w, 415, "UNSUPPORTED_MEDIA")
			return false
		}
		if r.URL.RawQuery != "" {
			problem(w, 422, "INVALID_REQUEST")
			return false
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limits.RequestBodyBytes))
		if err != nil {
			var sizeErr *http.MaxBytesError
			if errors.As(err, &sizeErr) {
				problem(w, 413, "BODY_TOO_LARGE")
				return false
			}
			problem(w, 422, "INVALID_REQUEST")
			return false
		}
		// Unknown fields (a forged actor, assignee or state) are refused, never ignored.
		if strictjson.Decode(data, dst, limits.RequestBodyBytes) != nil {
			problem(w, 422, "INVALID_TASK_REQUEST")
			return false
		}
		return true
	}
	mutation := func(name string, run func(*http.Request, tasks.Principal, tasks.Scope, string, string, int64, func(any) bool) (tasks.TaskV1, bool, error)) {
		mux.HandleFunc("POST "+base+"/{task}/"+name, func(w http.ResponseWriter, r *http.Request) {
			p, tenant, site, ok := scope(w, r)
			if !ok {
				return
			}
			if err := auth.CheckCSRF(r, p); err != nil {
				problem(w, 403, "CSRF_REJECTED")
				return
			}
			key, expected, ok := mutationHeaders(w, r)
			if !ok {
				return
			}
			task, decoded, err := run(r, principal(p), tasks.Scope{TenantID: tenant, SiteID: site}, r.PathValue("task"), key, expected, func(dst any) bool { return decode(w, r, dst) })
			if !decoded {
				return
			}
			if err != nil {
				taskError(w, err)
				return
			}
			w.Header().Set("ETag", `"`+strconv.FormatInt(task.Revision, 10)+`"`)
			reply(w, 200, task)
		})
	}
	type transition func(context.Context, tasks.Principal, tasks.Scope, string, tasks.TransitionInput, string, int64) (tasks.TaskV1, error)
	for name, apply := range map[string]transition{"acknowledge": service.Acknowledge, "start": service.Start, "block": service.Block, "cancel": service.Cancel, "reopen": service.Reopen} {
		mutation(name, func(r *http.Request, a tasks.Principal, sc tasks.Scope, id, key string, expected int64, decode func(any) bool) (tasks.TaskV1, bool, error) {
			var in tasks.TransitionInput
			if !decode(&in) {
				return tasks.TaskV1{}, false, nil
			}
			v, err := apply(r.Context(), a, sc, id, in, key, expected)
			return v, true, err
		})
	}
	mutation("complete", func(r *http.Request, a tasks.Principal, sc tasks.Scope, id, key string, expected int64, decode func(any) bool) (tasks.TaskV1, bool, error) {
		var in tasks.CompleteInput
		if !decode(&in) {
			return tasks.TaskV1{}, false, nil
		}
		v, err := service.Complete(r.Context(), a, sc, id, in, key, expected)
		return v, true, err
	})
}

func taskError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, tasks.ErrNotFound):
		problem(w, 404, "NOT_FOUND")
	case errors.Is(err, tasks.ErrDenied):
		problem(w, 403, "PERMISSION_DENIED")
	case errors.Is(err, tasks.ErrInvalid):
		problem(w, 422, "INVALID_TASK_REQUEST")
	case errors.Is(err, tasks.ErrReasonRequired):
		problem(w, 422, "REASON_REQUIRED")
	case errors.Is(err, tasks.ErrEvidence):
		problem(w, 422, "COMPLETION_EVIDENCE_UNUSABLE")
	case errors.Is(err, tasks.ErrNotAuthorized):
		problem(w, 403, "TASK_ROLE_REQUIRED")
	case errors.Is(err, tasks.ErrReauthentication):
		problem(w, 428, "REAUTHENTICATION_REQUIRED")
	case errors.Is(err, tasks.ErrIllegalTransition):
		problem(w, 409, "ILLEGAL_TRANSITION")
	case errors.Is(err, tasks.ErrIdempotency):
		problem(w, 409, "IDEMPOTENCY_CONFLICT")
	case errors.Is(err, tasks.ErrRevision):
		problem(w, 412, "REVISION_CONFLICT")
	default:
		problem(w, 503, "STORAGE_UNAVAILABLE")
	}
}
