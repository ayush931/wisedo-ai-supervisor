package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/contracts"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
)

func TestOEEEndpointsFailClosed(t *testing.T) {
	mux := http.NewServeMux()
	registerOEEWith(mux, func(*http.Request) error { return nil }, func(*http.Request) error { return nil })

	// POST calculation -> 503 CALCULATION_UNAVAILABLE
	req := httptest.NewRequest(http.MethodPost, "/v1/oee/calculations", strings.NewReader(validCalculationRequest(t)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	var prob map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &prob)
	if prob["code"] != "CALCULATION_UNAVAILABLE" {
		t.Fatalf("expected CALCULATION_UNAVAILABLE, got %v", prob)
	}

	// GET calculation with valid UUID -> 503 CALCULATION_UNAVAILABLE
	validID := uuid.New().String()
	req = httptest.NewRequest(http.MethodGet, "/v1/oee/calculations/"+validID, nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}

	// GET calculation with invalid UUID -> 422 INVALID_REQUEST
	req = httptest.NewRequest(http.MethodGet, "/v1/oee/calculations/invalid-uuid", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", rec.Code)
	}

	// GET evidence with valid UUID -> 503 NOT_READY
	req = httptest.NewRequest(http.MethodGet, "/v1/oee/evidence/"+validID, nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &prob)
	if prob["code"] != "NOT_READY" {
		t.Fatalf("expected NOT_READY, got %v", prob)
	}
}

func TestOEEEndpointsRejectWithoutAuthenticator(t *testing.T) {
	mux := http.NewServeMux()
	registerOEE(mux, nil)
	id := uuid.New().String()
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/v1/oee/calculations"},
		{http.MethodGet, "/v1/oee/calculations/" + id},
		{http.MethodPost, "/v1/oee/calculations/" + id},
		{http.MethodGet, "/v1/oee/evidence/" + id},
		{http.MethodGet, "/v1/oee/evidence/not-a-uuid"},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: expected 503, got %d", c.method, c.path, rec.Code)
		}
		var prob map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &prob)
		if prob["code"] != "AUTHENTICATION_UNAVAILABLE" {
			t.Fatalf("%s %s: expected AUTHENTICATION_UNAVAILABLE, got %v", c.method, c.path, prob)
		}
	}
}

// Every operation in the published OpenAPI document must be routable and must
// end in its documented fail-closed outcome, so the document cannot name a
// route the server does not have.
func TestPublishedOpenAPIRoutesAreServed(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]struct {
			Responses   map[string]json.RawMessage `json:"responses"`
			RequestBody json.RawMessage            `json:"requestBody"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(contracts.Files()["openapi.json"], &doc); err != nil {
		t.Fatal(err)
	}
	// Legacy /v1/oee routes fail closed at the unavailable seam. The site-scoped service routes
	// (WIS-229) fail closed with 503 when the service has no database or limits configured.
	mux := http.NewServeMux()
	registerOEEWith(mux, func(*http.Request) error { return nil }, func(*http.Request) error { return nil })
	stubScope := func(http.ResponseWriter, *http.Request) (identity.Principal, string, string, bool) {
		return identity.Principal{}, uuid.NewString(), uuid.NewString(), true
	}
	registerOEEService(mux, config.Config{}, &store.Store{}, nil, stubScope)
	// WIS-520 shift plan, job order, station profile, station record, shift summary and
	// handover routes need a database; they are checked for registration only.
	shiftMux := http.NewServeMux()
	registerShiftPlans(shiftMux, &store.Store{}, nil, stubScope)
	registerJobOrders(shiftMux, &store.Store{}, nil, stubScope)
	registerStationProfiles(shiftMux, &store.Store{}, nil, stubScope)
	registerMasterData(shiftMux, &store.Store{}, nil, stubScope)
	registerConfigurationImports(shiftMux, &store.Store{}, nil, stubScope)
	registerConfigurationDrafts(shiftMux, &store.Store{}, nil, stubScope)
	registerStationRecords(shiftMux, &store.Store{}, nil, stubScope)
	registerShiftSummaries(shiftMux, &store.Store{}, nil, stubScope)
	registerShiftHandovers(shiftMux, &store.Store{}, nil, stubScope)
	registerShiftRuns(shiftMux, &store.Store{}, nil, stubScope)
	// WIS-290 session and member directory routes need a database; registration only.
	memberMux := http.NewServeMux()
	registerMembership(memberMux, &store.Store{}, nil, stubScope, config.Agent{})
	// WIS-270 external identity resolution needs a database; registration only.
	identityMux := http.NewServeMux()
	registerOperational(identityMux, &store.Store{}, nil, stubScope)
	registerOperationalIngestion(identityMux, &store.Store{}, nil, stubScope)
	registerOperationalImports(identityMux, &store.Store{}, nil, stubScope) // WIS-719 import cancel
	registerPlans(identityMux, &store.Store{}, nil, stubScope)
	registerContexts(identityMux, &store.Store{}, nil, stubScope)
	// WIS-274 proposal routes need a database; registration only.
	proposalMux := http.NewServeMux()
	registerProposals(proposalMux, &store.Store{}, nil, stubScope)
	registerMaintenanceRoster(proposalMux, &store.Store{}, nil, stubScope)
	registerModelHosting(proposalMux, &store.Store{}, nil, stubScope) // WIS-623 hosted-inference switch
	// WIS-273 case routes need a database; registration only.
	caseMux := http.NewServeMux()
	registerCases(caseMux, &store.Store{}, nil, stubScope)
	registerTasks(caseMux, &store.Store{}, nil, stubScope)                    // WIS-277 task reads
	registerAdvisor(caseMux, &store.Store{}, nil, stubScope, config.Config{}) // WIS-629 advisor suggestion read
	// WIS-271 interview routes need a database; registration only.
	interviewMux := http.NewServeMux()
	registerInterviews(interviewMux, &store.Store{}, nil, stubScope, config.Config{})
	// WIS-517 chat routes need a database; registration only.
	chatMux := http.NewServeMux()
	registerChat(chatMux, &store.Store{}, nil, stubScope, config.Config{})
	// WIS-279 feedback routes need a database; registration only.
	feedbackMux := http.NewServeMux()
	registerFeedback(feedbackMux, &store.Store{}, nil, stubScope)
	registerCorrections(feedbackMux, &store.Store{}, nil, stubScope)
	id := uuid.New().String()
	legacy, site, shift, members, proposals, identities, caseOps, interviews, operationalOps, chatOps, feedbackOps := 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0
	for path, methods := range doc.Paths {
		if isKnowledgePath(path) {
			continue // TestPublishedKnowledgeRoutesMatchRegisteredRoutes covers the WIS-270 routes
		}
		concrete := strings.NewReplacer("{run_id}", id, "{evidence_id}", id, "{site}", id, "{run}", id, "{evidence}", id, "{policy}", id, "{revision}", id, "{plan}", id, "{job_order}", id, "{workstation}", id, "{record}", id, "{proposal}", id, "{case}", id, "{task}", id, "{id}", id, "{source}", id, "{feedback}", id, "{job}", id).Replace(path)
		for method, op := range methods {
			if path == "/v1/me" || strings.HasSuffix(path, "/members") {
				if _, pattern := memberMux.Handler(httptest.NewRequest(strings.ToUpper(method), concrete, nil)); pattern == "" {
					t.Errorf("%s %s is published but not routed", method, path)
				}
				if _, documented := op.Responses["503"]; !documented {
					t.Errorf("%s %s does not document 503", method, path)
				}
				members++
				continue
			}
			// G3: the operational-source list, read, create/revise and mapping routes need a
			// database; each published operation must hit its exact registered pattern.
			if strings.HasPrefix(path, "/v1/sites/{site}/operational-sources") || strings.HasPrefix(path, "/v1/sites/{site}/operational-ingestion-jobs") || path == "/v1/sites/{site}/plan-revisions" || path == "/v1/sites/{site}/contexts" {
				if _, pattern := identityMux.Handler(httptest.NewRequest(strings.ToUpper(method), concrete, nil)); pattern != strings.ToUpper(method)+" "+path {
					t.Errorf("%s %s is published but routed to %q", method, path, pattern)
				}
				if _, documented := op.Responses["503"]; !documented {
					t.Errorf("%s %s does not document 503", method, path)
				}
				operationalOps++
				continue
			}
			if strings.HasPrefix(path, "/v1/sites/{site}/external-identities/") {
				if _, pattern := identityMux.Handler(httptest.NewRequest(strings.ToUpper(method), concrete, nil)); pattern != strings.ToUpper(method)+" "+path {
					t.Errorf("%s %s is published but routed to %q", method, path, pattern)
				}
				if _, documented := op.Responses["503"]; !documented {
					t.Errorf("%s %s does not document 503", method, path)
				}
				identities++
				continue
			}
			if strings.HasPrefix(path, "/v1/sites/{site}/feedback") || strings.HasPrefix(path, "/v1/sites/{site}/memory-candidates") || strings.HasPrefix(path, "/v1/sites/{site}/correction-") {
				if _, pattern := feedbackMux.Handler(httptest.NewRequest(strings.ToUpper(method), concrete, nil)); pattern != strings.ToUpper(method)+" "+path {
					t.Errorf("%s %s is published but routed to %q", method, path, pattern)
				}
				if _, documented := op.Responses["503"]; !documented {
					t.Errorf("%s %s does not document 503", method, path)
				}
				feedbackOps++
				continue
			}
			if strings.HasPrefix(path, "/v1/sites/{site}/chat-conversations") {
				if _, pattern := chatMux.Handler(httptest.NewRequest(strings.ToUpper(method), concrete, nil)); pattern != strings.ToUpper(method)+" "+path {
					t.Errorf("%s %s is published but routed to %q", method, path, pattern)
				}
				if _, documented := op.Responses["503"]; !documented {
					t.Errorf("%s %s does not document 503", method, path)
				}
				chatOps++
				continue
			}
			if strings.HasPrefix(path, "/v1/sites/{site}/cases") || strings.HasPrefix(path, "/v1/sites/{site}/tasks") {
				if _, pattern := caseMux.Handler(httptest.NewRequest(strings.ToUpper(method), concrete, nil)); pattern != strings.ToUpper(method)+" "+path {
					t.Errorf("%s %s is published but routed to %q", method, path, pattern)
				}
				if _, documented := op.Responses["503"]; !documented {
					t.Errorf("%s %s does not document 503", method, path)
				}
				caseOps++
				continue
			}
			if strings.Contains(path, "/interviews") || strings.Contains(path, "/configuration-draft-requests") {
				if _, pattern := interviewMux.Handler(httptest.NewRequest(strings.ToUpper(method), concrete, nil)); pattern != strings.ToUpper(method)+" "+path {
					t.Errorf("%s %s is published but routed to %q", method, path, pattern)
				}
				if _, documented := op.Responses["503"]; !documented {
					t.Errorf("%s %s does not document 503", method, path)
				}
				interviews++
				continue
			}
			if strings.Contains(path, "/proposals") || strings.Contains(path, "/maintenance-roster") || strings.Contains(path, "/model-hosting") {
				if _, pattern := proposalMux.Handler(httptest.NewRequest(strings.ToUpper(method), concrete, nil)); pattern == "" {
					t.Errorf("%s %s is published but not routed", method, path)
				}
				if _, documented := op.Responses["503"]; !documented {
					t.Errorf("%s %s does not document 503", method, path)
				}
				proposals++
				continue
			}
			if strings.Contains(path, "/shift-plans") || strings.Contains(path, "/job-orders") || strings.Contains(path, "/workstations") || strings.Contains(path, "/lines") || strings.Contains(path, "/items") || strings.Contains(path, "/routings") || strings.Contains(path, "/shift-calendars") || strings.Contains(path, "/master-data-settings") || strings.Contains(path, "/configuration-imports") || strings.Contains(path, "/configuration-drafts") {
				if _, pattern := shiftMux.Handler(httptest.NewRequest(strings.ToUpper(method), concrete, nil)); pattern == "" {
					t.Errorf("%s %s is published but not routed", method, path)
				}
				if _, documented := op.Responses["503"]; !documented {
					t.Errorf("%s %s does not document 503", method, path)
				}
				shift++
				continue
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(strings.ToUpper(method), concrete, nil)
			if op.RequestBody != nil {
				req = httptest.NewRequest(strings.ToUpper(method), concrete, strings.NewReader(validCalculationRequest(t)))
				req.Header.Set("Content-Type", "application/json")
			}
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("%s %s: got %d, want documented 503", method, path, rec.Code)
			}
			if _, documented := op.Responses["503"]; !documented {
				t.Errorf("%s %s does not document 503", method, path)
			}
			if strings.HasPrefix(path, "/v1/oee/") {
				legacy++
			} else {
				var prob map[string]any
				_ = json.Unmarshal(rec.Body.Bytes(), &prob)
				if prob["code"] != "OEE_SERVICE_UNAVAILABLE" {
					t.Errorf("%s %s: %v", method, path, prob)
				}
				site++
			}
		}
	}
	// The registered resolve route is published (it once was served but missing from OpenAPI).
	if feedbackOps != 9 { // three append-only feedback operations and six WIS-703 correction operations
		t.Fatalf("expected 9 published feedback and correction operations, found %d", feedbackOps)
	}
	if chatOps != 5 {
		t.Fatalf("expected 5 published chat operations, found %d", chatOps)
	}
	if caseOps != 16 { // six case operations, case close, the WIS-629 suggestion read, the two WIS-277 task reads and six task lifecycle operations
		t.Fatalf("expected 16 published case and task operations, found %d", caseOps)
	}
	if operationalOps != 8 { // plus the WIS-719 import cancel
		t.Fatalf("expected 8 published operational-source, import-cancel, plan and context operations, found %d", operationalOps)
	}
	if identities != 1 {
		t.Fatalf("expected the external identity resolve operation to be published, found %d", identities)
	}
	if legacy != 4 || site != 7 || shift != 62 || members != 2 || proposals != 8 || interviews != 10 {
		t.Fatalf("expected 4 legacy, 7 site, 62 workstation/master-data (59 plus the three shift run operations), 2 member, 8 proposal, maintenance-roster (WIS-276) and model-hosting (WIS-623) and 10 interview operations; found %d, %d, %d, %d, %d and %d", legacy, site, shift, members, proposals, interviews)
	}
}

func validCalculationRequest(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "oee", "testdata", "contract-fixtures", "valid_calculation_request.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The consumer fixtures drive the real route: a valid populated request reaches the
// application seam (503), an invalid one is 422, and media type and size are enforced
// (WIS-225 F2, F3, F4).
func TestCalculationRouteDecodesThePublishedContract(t *testing.T) {
	mux := http.NewServeMux()
	registerOEEWith(mux, func(*http.Request) error { return nil }, func(*http.Request) error { return nil })
	h := Boundary(mux, false)
	files, _ := filepath.Glob(filepath.Join("..", "oee", "testdata", "contract-fixtures", "*_*.json"))
	checked := 0
	for _, path := range files {
		name := filepath.Base(path)
		if strings.HasPrefix(name, "provider_") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/oee/calculations", bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		want := http.StatusUnprocessableEntity
		if strings.HasPrefix(name, "valid_") {
			want = http.StatusServiceUnavailable
		}
		if rec.Code != want {
			t.Errorf("%s: got %d want %d: %s", name, rec.Code, want, rec.Body.String())
		}
		checked++
	}
	if checked < 10 {
		t.Fatalf("only %d fixtures", checked)
	}
	// Wrong media type.
	req := httptest.NewRequest(http.MethodPost, "/v1/oee/calculations", strings.NewReader(validCalculationRequest(t)))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain: %d", rec.Code)
	}
	// A body over the limit with no Content-Length is cut off by actual bytes read.
	big := io.MultiReader(strings.NewReader(`{"scope":"`), strings.NewReader(strings.Repeat("x", 2<<20)), strings.NewReader(`"}`))
	req = httptest.NewRequest(http.MethodPost, "/v1/oee/calculations", big)
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("2 MiB body without Content-Length: %d", rec.Code)
	}
}

// The execution owner is unique per process and always valid UTF-8 within its 128-byte column,
// however long or multibyte the hostname is.
func TestExecutionOwnerIsBoundedUniqueAndValidUTF8(t *testing.T) {
	long := strings.Repeat("é", 100) // 200 bytes of two-byte runes
	a, b := executionOwner(long), executionOwner(long)
	if a == b {
		t.Fatal("two processes on one host share an owner label")
	}
	for _, owner := range []string{a, executionOwner(""), executionOwner("plant-host-1")} {
		if len(owner) > 128 || !utf8.ValidString(owner) || strings.TrimSpace(owner) != owner || owner == "" {
			t.Fatalf("owner %q is not a bounded valid label", owner)
		}
	}
}

// WIS-699 (security audit run 1): the legacy OEE POSTs are cookie-authenticated, so they check
// CSRF like every other mutating route; a GET does not need it.
func TestWIS699LegacyOEEPostsRequireCSRF(t *testing.T) {
	mux := http.NewServeMux()
	registerOEEWith(mux, func(*http.Request) error { return nil }, func(*http.Request) error { return errors.New("no csrf token") })
	for _, path := range []string{"/v1/oee/calculations", "/v1/oee/calculations/" + "00000000-0000-4000-8000-000000000699:cancel"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		r.Header.Set("Content-Type", "application/json")
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"code":"CSRF_REJECTED"`) {
			t.Fatalf("POST %s without CSRF: %d %s", path, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/oee/calculations/00000000-0000-4000-8000-000000000699", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET is not CSRF-checked: %d %s", w.Code, w.Body.String())
	}
}
