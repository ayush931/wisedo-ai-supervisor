package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/modelhosting"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
)

// registerModelHosting serves the WIS-623 per-site switch for hosted model inference (rule 08):
// owners and supervisors read it; only a recently authenticated owner of the site decides, with a
// reason and, to turn it on, a data processing agreement reference.
func registerModelHosting(mux *http.ServeMux, db *store.Store, auth *identity.Service, scope func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool)) {
	service := modelhosting.New(db.DB).WithSessionPolicy(auth.Sessions())
	principal := func(p identity.Principal) modelhosting.Principal {
		return modelhosting.Principal{Issuer: p.Issuer, Subject: p.Subject, SessionHash: p.SessionHash}
	}
	base := "/v1/sites/{site}/model-hosting"
	mux.HandleFunc("GET "+base, func(w http.ResponseWriter, r *http.Request) {
		p, tenant, site, ok := scope(w, r)
		if !ok || !onlyQuery(w, r) {
			return
		}
		v, err := service.Get(r.Context(), principal(p), modelhosting.Scope{TenantID: tenant, SiteID: site})
		if err != nil {
			modelHostingError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("ETag", `"`+strconv.FormatInt(v.Revision, 10)+`"`)
		reply(w, 200, v)
	})
	mux.HandleFunc("POST "+base+"/decisions", func(w http.ResponseWriter, r *http.Request) {
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
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			problem(w, 415, "UNSUPPORTED_MEDIA")
			return
		}
		if r.URL.RawQuery != "" {
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limits.RequestBodyBytes))
		if err != nil {
			var sizeErr *http.MaxBytesError
			if errors.As(err, &sizeErr) {
				problem(w, 413, "BODY_TOO_LARGE")
				return
			}
			problem(w, 422, "INVALID_REQUEST")
			return
		}
		// Unknown fields are refused, so a forged decider, tenant or site field is a 422.
		// "enabled" must be an explicit boolean: absent or null would decode as false and turn
		// hosting off, so either is refused before anything changes.
		var in modelhosting.DecisionInput
		var present struct {
			Enabled *bool `json:"enabled"`
		}
		if strictjson.Decode(data, &in, limits.RequestBodyBytes) != nil || json.Unmarshal(data, &present) != nil || present.Enabled == nil {
			problem(w, 422, "INVALID_MODEL_HOSTING_DECISION")
			return
		}
		v, err := service.Decide(r.Context(), principal(p), modelhosting.Scope{TenantID: tenant, SiteID: site}, in, key, expected)
		if err != nil {
			modelHostingError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(v.Revision, 10)+`"`)
		reply(w, 200, v)
	})
}

func modelHostingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, modelhosting.ErrNotFound):
		problem(w, 404, "NOT_FOUND")
	case errors.Is(err, modelhosting.ErrDenied):
		problem(w, 403, "PERMISSION_DENIED")
	case errors.Is(err, modelhosting.ErrReauthentication):
		problem(w, 428, "REAUTHENTICATION_REQUIRED")
	case errors.Is(err, modelhosting.ErrIdempotency):
		problem(w, 409, "IDEMPOTENCY_CONFLICT")
	case errors.Is(err, modelhosting.ErrUnchanged):
		problem(w, 409, "MODEL_HOSTING_UNCHANGED")
	case errors.Is(err, modelhosting.ErrRevision):
		problem(w, 412, "REVISION_CONFLICT")
	case errors.Is(err, modelhosting.ErrInvalid):
		problem(w, 422, "INVALID_MODEL_HOSTING_DECISION")
	default:
		problem(w, 503, "STORAGE_UNAVAILABLE")
	}
}
