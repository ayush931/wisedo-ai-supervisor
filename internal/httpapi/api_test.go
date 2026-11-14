package httpapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WIS-278: reloading a client-side route serves the app; API paths and missing assets stay 404.
func TestSPAFallback(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("app-shell"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "index-abc.js"), []byte("hashed"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := spa(dir)
	for _, tc := range []struct {
		method, path string
		status       int
		body         string
	}{
		{"GET", "/sites/00000000-0000-4000-8000-000000000291/shifts/abc", 200, "app-shell"},
		{"GET", "/login", 200, "app-shell"},
		{"HEAD", "/sites/x/workstations/y", 200, ""},
		{"GET", "/app.js", 200, "bundle"},
		{"GET", "/assets/missing.js", 404, ""},
		{"GET", "/v1/sites/x/unknown", 404, ""},
		// WIS-699 (security audit run 1): a method other than GET or HEAD is 405 before the file
		// server, which listed /assets/ for POST, PUT and DELETE.
		{"POST", "/sites/x", 405, ""},
		{"POST", "/assets/", 405, ""},
		{"PUT", "/assets/", 405, ""},
		{"DELETE", "/assets", 405, ""},
		{"GET", "/assets/index-abc.js", 200, "hashed"},
		// Review finding: an existing directory was listed (bundle names enumerable), and
		// "/v1" without a slash, or an unknown API path, was not an API 404 problem.
		{"GET", "/assets", 200, "app-shell"},
		{"GET", "/assets/", 200, "app-shell"},
		{"GET", "/v1", 404, ""},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status || (tc.body != "" && w.Body.String() != tc.body) {
			t.Fatalf("%s %s = %d %q, want %d %q", tc.method, tc.path, w.Code, w.Body.String(), tc.status, tc.body)
		}
		if strings.Contains(w.Body.String(), "index-abc.js") {
			t.Fatalf("%s %s listed a directory: %q", tc.method, tc.path, w.Body.String())
		}
		if (tc.path == "/v1" || strings.HasPrefix(tc.path, "/v1/")) && (w.Header().Get("Content-Type") != "application/problem+json" || !strings.Contains(w.Body.String(), `"code":"NOT_FOUND"`)) {
			t.Fatalf("%s %s is not an API problem: %q %q", tc.method, tc.path, w.Header().Get("Content-Type"), w.Body.String())
		}
	}
}

func TestActualBodyLimitBeforeHandler(t *testing.T) {
	called := false
	h := Boundary(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(204) }), false)
	r := httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("x", (1<<20)+1)))
	r.ContentLength = 1
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 413 || called {
		t.Fatalf("code %d called %v", w.Code, called)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/", strings.NewReader("bounded")))
	if w.Code != 204 || !called {
		t.Fatal("valid body rejected")
	}
}

// WIS-707: the CSP admits the Astryx Layer slide-rules sheet by its hash only. Scripts stay
// 'self'-only and no directive gains 'unsafe-inline' or 'unsafe-hashes'.
func TestBoundaryContentSecurityPolicy(t *testing.T) {
	h := Boundary(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }), false)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	directives := map[string]string{}
	for _, d := range strings.Split(w.Header().Get("Content-Security-Policy"), ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(d), " ")
		directives[name] = value
	}
	want := map[string]string{
		"default-src":     "'self'",
		"script-src":      "'self'",
		"style-src":       "'self' 'sha256-/i2+VVt2qoVh0Ag9OWwyc9593DCZeYGAv6SliqVbheY='",
		"frame-ancestors": "'none'",
		"base-uri":        "'none'",
		"form-action":     "'self'",
	}
	if len(directives) != len(want) {
		t.Fatalf("directives = %v, want %v", directives, want)
	}
	for name, value := range want {
		if directives[name] != value {
			t.Fatalf("%s = %q, want %q", name, directives[name], value)
		}
	}
}
