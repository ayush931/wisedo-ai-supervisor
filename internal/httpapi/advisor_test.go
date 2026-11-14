package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
)

// TestAdvisorSuggestionSwitch: the WIS-629 rollback switch refuses before any read, and a query
// string is refused; both are decided without a database.
func TestAdvisorSuggestionSwitch(t *testing.T) {
	scope := func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool) {
		return identity.Principal{}, uuid.NewString(), uuid.NewString(), true
	}
	path := "/v1/sites/" + uuid.NewString() + "/cases/" + uuid.NewString() + "/suggestion"
	for _, tc := range []struct {
		name   string
		cfg    config.Config
		path   string
		status int
		code   string
	}{
		{"switched off", config.Config{AdvisorSuggestionsDisabled: true}, path, 404, "ADVISOR_DISABLED"},
		{"query string", config.Config{}, path + "?limit=1", 422, "INVALID_REQUEST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			registerAdvisor(mux, &store.Store{}, nil, scope, tc.cfg)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest("GET", tc.path, nil))
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("got %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}
