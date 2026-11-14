package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/contracts"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
)

var (
	handlePattern = regexp.MustCompile(`HandleFunc\("((?:GET|POST) /v1/[^"]*)"`)
	resourceLoop  = regexp.MustCompile(`range \[\]string\{([^}]*)\}`)
	pathParam     = regexp.MustCompile(`\{[a-z_]+\}`)
)

// registeredPatterns reads the method patterns evidence.go and knowledge.go register, expanding
// the GET /v1/sites/{site}/<resource> loop, so a route added there cannot go unpublished.
func registeredPatterns(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, file := range []string{"evidence.go", "knowledge.go", "source_documents.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range handlePattern.FindAllStringSubmatch(string(src), -1) {
			if !strings.HasSuffix(m[1], "/") {
				out = append(out, m[1])
				continue
			}
			loop := resourceLoop.FindStringSubmatch(string(src))
			if loop == nil {
				t.Fatalf("%s: %q is concatenated outside the known resource loop", file, m[1])
			}
			for _, r := range strings.Split(loop[1], ",") {
				out = append(out, m[1]+strings.Trim(strings.TrimSpace(r), `"`))
			}
		}
	}
	sort.Strings(out)
	return out
}

// Every WIS-270 evidence and knowledge route is published, and every published operation is
// served by exactly that route pattern (two-way). The handlers need a database, so routing is
// proven with ServeMux.Handler rather than by calling them.
func TestPublishedKnowledgeRoutesMatchRegisteredRoutes(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]struct {
			Parameters []struct {
				Name string `json:"name"`
				In   string `json:"in"`
			} `json:"parameters"`
			Responses map[string]json.RawMessage `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(contracts.Files()["openapi.json"], &doc); err != nil {
		t.Fatal(err)
	}
	stubScope := func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool) {
		return identity.Principal{}, uuid.NewString(), uuid.NewString(), true
	}
	mux := http.NewServeMux()
	registerEvidence(mux, &store.Store{}, nil, stubScope)
	registerKnowledge(mux, &store.Store{}, nil, stubScope)
	registerSourceDocuments(mux, &store.Store{}, nil, stubScope, config.Config{})
	id := uuid.NewString()
	var published []string
	for path, methods := range doc.Paths {
		if !isKnowledgePath(path) {
			continue
		}
		for method, op := range methods {
			pattern := strings.ToUpper(method) + " " + path
			_, served := mux.Handler(httptest.NewRequest(strings.ToUpper(method), pathParam.ReplaceAllString(path, id), nil))
			if served != pattern {
				t.Errorf("%s is published but routed to %q", pattern, served)
			}
			success := "200"
			if pattern == "POST /v1/sites/{site}/source-scans" || pattern == "POST /v1/sites/{site}/source-scan-batches" {
				success = "202" // queued for the OCR run (WIS-625)
			}
			for _, status := range []string{success, "401", "404", "503"} {
				if _, ok := op.Responses[status]; !ok {
					t.Errorf("%s does not document %s", pattern, status)
				}
			}
			headers := map[string]bool{}
			for _, p := range op.Parameters {
				if p.In == "header" {
					headers[p.Name] = true
				}
			}
			// A document upload always creates a new source (WIS-613), so it alone has no revision precondition.
			// A scan (WIS-625) is the same: it always queues a new job.
			createOnly := pattern == "POST /v1/sites/{site}/source-documents" || pattern == "POST /v1/sites/{site}/source-scans" || pattern == "POST /v1/sites/{site}/source-scan-batches"
			if method == "post" && (!headers["Idempotency-Key"] || headers["If-Match"] == createOnly || !headers["X-CSRF-Token"]) {
				t.Errorf("%s does not document its mutation headers: %v", pattern, headers)
			}
			published = append(published, pattern)
		}
	}
	sort.Strings(published)
	registered := registeredPatterns(t)
	if len(registered) < 30 || strings.Join(published, "\n") != strings.Join(registered, "\n") {
		t.Fatalf("published and registered routes differ\npublished:\n%s\nregistered:\n%s", strings.Join(published, "\n"), strings.Join(registered, "\n"))
	}
}

// isKnowledgePath selects the WIS-270 evidence and knowledge path family by its first segment
// under the site. A route registered under a new segment still fails the two-way check above.
func isKnowledgePath(path string) bool {
	rest, ok := strings.CutPrefix(path, "/v1/sites/{site}/")
	if !ok {
		return false
	}
	segment, _, _ := strings.Cut(rest, "/")
	switch segment {
	case "sources", "source-documents", "source-scans", "source-scan-batches", "source-spans", "process-snapshots", "knowledge-inventory", "document-freshness-policy",
		"document-freshness-policy-decisions", "discovery-inventories", "entities", "claims", "conflicts", "coverage", "knowledge":
		return true
	}
	return false
}
