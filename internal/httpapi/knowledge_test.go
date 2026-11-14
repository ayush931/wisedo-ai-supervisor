package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMutationHeadersRequireOneCanonicalPrecondition(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tags, keys []string
		status     int
		code       string
	}{
		{"valid", []string{`"0"`}, []string{"capture-1"}, 200, ""},
		{"missing", nil, []string{"capture-1"}, 428, "REVISION_REQUIRED"},
		{"weak", []string{`W/"1"`}, []string{"capture-1"}, 422, "INVALID_REVISION"},
		{"noncanonical", []string{`"01"`}, []string{"capture-1"}, 422, "INVALID_REVISION"},
		{"negative", []string{`"-1"`}, []string{"capture-1"}, 422, "INVALID_REVISION"},
		{"overflow", []string{`"9223372036854775808"`}, []string{"capture-1"}, 422, "INVALID_REVISION"},
		{"duplicate revision", []string{`"0"`, `"1"`}, []string{"capture-1"}, 422, "INVALID_REVISION"},
		{"duplicate key", []string{`"0"`}, []string{"capture-1", "capture-2"}, 422, "INVALID_REQUEST"},
		{"missing key", []string{`"0"`}, nil, 422, "INVALID_REQUEST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/", nil)
			for _, tag := range tc.tags {
				r.Header.Add("If-Match", tag)
			}
			for _, key := range tc.keys {
				r.Header.Add("Idempotency-Key", key)
			}
			w := httptest.NewRecorder()
			key, revision, ok := mutationHeaders(w, r)
			if w.Code != tc.status || ok != (tc.status == 200) {
				t.Fatalf("status=%d ok=%v", w.Code, ok)
			}
			if tc.code != "" && !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("problem %s want %s", w.Body.String(), tc.code)
			}
			if ok && (key != "capture-1" || revision != 0) {
				t.Fatalf("key=%q revision=%d", key, revision)
			}
		})
	}
}
