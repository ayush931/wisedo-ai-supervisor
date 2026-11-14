//go:build integration

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/contracts"
)

type publishedOperation struct {
	Responses map[string]struct {
		Description string `json:"description"`
		Content     map[string]struct {
			Schema struct {
				Ref string `json:"$ref"`
			} `json:"schema"`
		} `json:"content"`
	} `json:"responses"`
}

var contractFiles = sync.OnceValue(contracts.Files)

var published = sync.OnceValues(func() (*http.ServeMux, map[string]publishedOperation) {
	var doc struct {
		Paths map[string]map[string]publishedOperation `json:"paths"`
	}
	if err := json.Unmarshal(contractFiles()["openapi.json"], &doc); err != nil {
		panic(err)
	}
	mux, ops := http.NewServeMux(), map[string]publishedOperation{}
	for path, methods := range doc.Paths {
		if !isKnowledgePath(path) {
			continue // other families check their own responses; ":cancel" is not a mux pattern
		}
		for method, op := range methods {
			pattern := strings.ToUpper(method) + " " + path
			mux.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {})
			ops[pattern] = op
		}
	}
	return mux, ops
})

// conformsToPublished checks a real handler response against the published contract of the
// operation that served it: the status is documented, a success body conforms to its schema,
// and an error is problem+json whose code is one the status documents. A request that no
// published operation matches (a deliberately wrong path or method) is not checked.
func conformsToPublished(t *testing.T, r *http.Request, w *httptest.ResponseRecorder) {
	t.Helper()
	mux, ops := published()
	_, pattern := mux.Handler(r)
	op, ok := ops[pattern]
	if !ok {
		return
	}
	status := w.Result().Status[:3]
	response, ok := op.Responses[status]
	if !ok {
		t.Errorf("%s answered undocumented %s: %s", pattern, status, w.Body.String())
		return
	}
	var body any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Errorf("%s %s: body is not JSON: %v", pattern, status, err)
		return
	}
	media := "application/json"
	if w.Code >= 400 {
		media = "application/problem+json"
		code, _ := body.(map[string]any)["code"].(string)
		if codes, listed := strings.CutPrefix(response.Description, "Error: "); listed && !slices.Contains(strings.Split(codes, ", "), code) {
			t.Errorf("%s answered %s %s, not among %q", pattern, status, code, response.Description)
		}
	}
	if got := w.Header().Get("Content-Type"); got != media {
		t.Errorf("%s %s: Content-Type %q want %q", pattern, status, got, media)
	}
	name := strings.TrimPrefix(response.Content[media].Schema.Ref, "#/components/schemas/")
	var schema map[string]any
	if err := json.Unmarshal(contractFiles()["schemas/"+name+".schema.json"], &schema); err != nil {
		t.Errorf("%s %s: schema %q: %v", pattern, status, name, err)
		return
	}
	if err := conforms(schema, body, name); err != nil {
		t.Errorf("%s %s: %v", pattern, status, err)
	}
}
