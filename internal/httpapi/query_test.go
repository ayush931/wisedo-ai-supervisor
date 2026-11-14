package httpapi

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
)

func TestOnlyQueryRejectsUnknownAndRepeatedParameters(t *testing.T) {
	for query, want := range map[string]bool{
		"":                              true,
		"limit=10&cursor=abc":           true,
		"limit=10":                      true,
		"state=approved&state=rejected": false,
		"limit=10&limit=20":             false,
		"limit=10&tenant_id=x":          false,
		"bogus=1":                       false,
		"limit=%zz":                     false,
	} {
		w := httptest.NewRecorder()
		got := onlyQuery(w, httptest.NewRequest("GET", "/v1/x?"+query, nil), "limit", "cursor", "state")
		if got != want {
			t.Errorf("%q: got %v want %v", query, got, want)
		}
		if !got && (w.Code != 422 || !strings.Contains(w.Body.String(), "INVALID_REQUEST")) {
			t.Errorf("%q: refusal was %d %s", query, w.Code, w.Body.String())
		}
	}
}

// A deterministic size limit is 413 LIMIT_EXCEEDED: a 503 would invite retries that can
// never succeed.
func TestTraversalLimitIsNotUnavailability(t *testing.T) {
	w := httptest.NewRecorder()
	knowledgeError(w, fmt.Errorf("wrapped: %w", knowledge.ErrTraversalLimit))
	if w.Code != 413 || !strings.Contains(w.Body.String(), `"code":"LIMIT_EXCEEDED"`) {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}
