package httpapi

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/oee"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/google/uuid"
)

func registerOEE(mux *http.ServeMux, auth *identity.Service) {
	var authenticate, checkCSRF func(*http.Request) error
	if auth != nil {
		authenticate = func(r *http.Request) error {
			_, err := auth.Authenticate(r.Context(), r)
			return err
		}
		checkCSRF = func(r *http.Request) error {
			p, err := auth.Authenticate(r.Context(), r)
			if err != nil {
				return err
			}
			return auth.CheckCSRF(r, p)
		}
	}
	registerOEEWith(mux, authenticate, checkCSRF)
}

// registerOEEWith takes the authenticator and the CSRF check as functions so the fail-closed
// seam can be tested without a live identity service.
func registerOEEWith(mux *http.ServeMux, authenticate, checkCSRF func(*http.Request) error) {
	requireAuth := func(w http.ResponseWriter, r *http.Request) bool {
		// Fail closed: an unconfigured authenticator never admits a caller.
		if authenticate == nil {
			problem(w, http.StatusServiceUnavailable, "AUTHENTICATION_UNAVAILABLE")
			return false
		}
		if err := authenticate(r); err != nil {
			authError(w, err)
			return false
		}
		return true
	}
	// A POST is cookie-authenticated, so it checks CSRF like every other mutating route, even
	// while it only answers CALCULATION_UNAVAILABLE (security audit run 1, WIS-699).
	requireMutation := func(w http.ResponseWriter, r *http.Request) bool {
		if !requireAuth(w, r) {
			return false
		}
		if checkCSRF == nil {
			problem(w, http.StatusServiceUnavailable, "AUTHENTICATION_UNAVAILABLE")
			return false
		}
		if err := checkCSRF(r); err != nil {
			if errors.Is(err, identity.ErrUnauthenticated) {
				authError(w, err)
			} else {
				problem(w, http.StatusForbidden, "CSRF_REJECTED")
			}
			return false
		}
		return true
	}

	mux.HandleFunc("POST /v1/oee/calculations", func(w http.ResponseWriter, r *http.Request) {
		if !requireMutation(w, r) {
			return
		}
		// The body is decoded and validated against the published CalculationRequestV1
		// contract before the unavailable seam, so an invalid request is 422 and a valid one
		// reaches the seam (WIS-225 F3/F4).
		if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/json" {
			problem(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA")
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			problem(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE")
			return
		}
		if _, err = oee.DecodeCalculationRequest(body); err != nil {
			problem(w, http.StatusUnprocessableEntity, "INVALID_REQUEST")
			return
		}
		problem(w, http.StatusServiceUnavailable, "CALCULATION_UNAVAILABLE")
	})

	mux.HandleFunc("GET /v1/oee/calculations/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		if _, err := uuid.Parse(r.PathValue("run_id")); err != nil {
			problem(w, http.StatusUnprocessableEntity, "INVALID_REQUEST")
			return
		}
		problem(w, http.StatusServiceUnavailable, "CALCULATION_UNAVAILABLE")
	})

	mux.HandleFunc("POST /v1/oee/calculations/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		if !requireMutation(w, r) {
			return
		}
		runIDStr := r.PathValue("run_id")
		runIDStr = strings.TrimSuffix(runIDStr, ":cancel")
		if _, err := uuid.Parse(runIDStr); err != nil {
			problem(w, http.StatusUnprocessableEntity, "INVALID_REQUEST")
			return
		}
		problem(w, http.StatusServiceUnavailable, "CALCULATION_UNAVAILABLE")
	})

	mux.HandleFunc("GET /v1/oee/evidence/{evidence_id}", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		if _, err := uuid.Parse(r.PathValue("evidence_id")); err != nil {
			problem(w, http.StatusUnprocessableEntity, "INVALID_REQUEST")
			return
		}
		problem(w, http.StatusServiceUnavailable, "NOT_READY")
	})
}
