//go:build integration

package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// wantProblem asserts the status and the stable problem code (rule 07). 409, 412 and 428 each
// carry several contracts (idempotency vs source drift, revision required vs reauthentication),
// so the status alone cannot tell which check fired.
func wantProblem(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("HTTP %d want %d: %s", w.Code, status, w.Body.String())
	}
	var p struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || p.Code != code {
		t.Errorf("problem code want %s: %s", code, w.Body.String())
	}
}
