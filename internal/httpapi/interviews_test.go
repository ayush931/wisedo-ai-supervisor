package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestInterviewErrorMapsGuardViolation(t *testing.T) {
	w := httptest.NewRecorder()
	interviewError(w, &pgconn.PgError{Code: "23514"})
	if w.Code != 409 {
		t.Fatalf("HTTP status=%d, want 409: %s", w.Code, w.Body.String())
	}
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil || problem.Code != "LIFECYCLE_CONFLICT" {
		t.Fatalf("problem=%s, want LIFECYCLE_CONFLICT (%v)", w.Body.String(), err)
	}
}
