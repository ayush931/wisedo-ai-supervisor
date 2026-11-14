//go:build integration

package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
)

// WIS-715: one tenant may hold at most the configured number of running scan pages, across all its
// sites. Past it, every way a page is queued (photo, scanned PDF, mixed PDF, typed-PDF figure) is
// 503 SCAN_QUEUE_FULL with Retry-After and stores nothing; a stored key still replays; another
// tenant is unaffected; parallel uploads never exceed the cap.
func TestWIS715ScanTenantCapPostgres(t *testing.T) {
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: actual disposable PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	control, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	name := "scancap_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = control.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	defer control.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	in := func(raw string) string {
		u, e := url.Parse(raw)
		if e != nil {
			t.Fatal(e)
		}
		u.Path = "/" + name
		return u.String()
	}
	admin, err := sql.Open("pgx", in(adminURL))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err = migrate.Apply(ctx, admin); err != nil {
		t.Fatal(err)
	}
	db, err := store.New(in(runtimeURL))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.VerifyRole(ctx); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := admin.ExecContext(ctx, q, args...); e != nil {
			t.Fatalf("%s: %v", q, e)
		}
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if e := admin.QueryRowContext(ctx, q, args...).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	issuer, origin := "https://scan-cap.invalid/"+uuid.NewString(), "http://127.0.0.1:58176"
	type member struct{ site, subject, token, csrf string }
	seed := func(tenantID, siteID string) member {
		m := member{site: siteID, subject: uuid.NewString(), token: uuid.NewString(), csrf: uuid.NewString()}
		sum := sha256.Sum256([]byte(m.token))
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, hex.EncodeToString(sum[:]), m.subject, issuer, m.csrf)
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'operator')`, tenantID, siteID, issuer, m.subject)
		return m
	}
	// tenant returns a new tenant with two sites and an operator on each.
	tenant := func() (string, member, member) {
		id, s1, s2 := uuid.NewString(), uuid.NewString(), uuid.NewString()
		exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'scan cap tenant')`, id)
		exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'site one'),($1,$3,'site two')`, id, s1, s2)
		return id, seed(id, s1), seed(id, s2)
	}
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "scan-cap-test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, db.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	const limit = 3
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir(), Agent: config.Agent{Provider: config.AgentProviderFixture, Model: "fixture-model", ModelRevision: "sha256:fixture",
		ModeEnabled: true, VisionModel: "fixture-vision", VisionModelRevision: "sha256:vision", ScanTenantCap: limit}}, db, auth)

	post := func(m member, route, media string, body []byte, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/sites/"+m.site+"/"+route, bytes.NewReader(body))
		r.AddCookie(&http.Cookie{Name: "supervisor", Value: m.token})
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", m.csrf)
		r.Header.Set("Content-Type", media)
		r.Header.Set("X-Source-Title", url.PathEscape("Receiving log"))
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	code := func(w *httptest.ResponseRecorder) string {
		var p struct{ Code string }
		_ = json.Unmarshal(w.Body.Bytes(), &p)
		return p.Code
	}
	photo := func(m member, key string) *httptest.ResponseRecorder {
		return post(m, "source-scans", "image/png", pagePNG(t, 300, 200, 40), key)
	}
	accept := func(w *httptest.ResponseRecorder) evidence.ScanJob {
		t.Helper()
		var job evidence.ScanJob
		if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &job) != nil || job.State != "running" {
			t.Fatalf("queue: %d %s", w.Code, w.Body.String())
		}
		return job
	}
	// stored is everything an upload can write for a tenant: refused uploads must leave it unchanged.
	stored := func(id string) [5]int {
		return [5]int{
			count(`SELECT count(*) FROM supervisor.document_scan_jobs WHERE tenant_id=$1`, id),
			count(`SELECT count(*) FROM supervisor.document_scan_outbox WHERE tenant_id=$1`, id),
			count(`SELECT count(*) FROM supervisor.agent_runs WHERE tenant_id=$1`, id),
			count(`SELECT count(*) FROM supervisor.document_uploads WHERE tenant_id=$1`, id),
			count(`SELECT count(*) FROM supervisor.evidence_revisions WHERE tenant_id=$1`, id),
		}
	}
	running := func(id string) int {
		return count(`SELECT count(*) FROM supervisor.document_scan_jobs WHERE tenant_id=$1 AND state='running'`, id)
	}
	full := func(t *testing.T, what string, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != 503 || code(w) != "SCAN_QUEUE_FULL" || w.Header().Get("Retry-After") == "" || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/problem+json") {
			t.Fatalf("%s: %d %q Retry-After=%q, want 503 SCAN_QUEUE_FULL with Retry-After", what, w.Code, w.Body.String(), w.Header().Get("Retry-After"))
		}
	}

	// Fixtures for the PDF paths: a two-page scan, a one-page fax scan, a mixed PDF and a typed PDF
	// with one figure.
	var twoPages bytes.Buffer
	model.ConfigPath = "disable"
	if err = pdfapi.ImportImages(nil, &twoPages, []io.Reader{bytes.NewReader(pagePNG(t, 320, 200, 120)), bytes.NewReader(pagePNG(t, 300, 180, 121))}, pdfcpu.DefaultImportConfig(), model.NewDefaultConfiguration()); err != nil {
		t.Fatal(err)
	}
	g4, err := os.ReadFile(filepath.Join("..", "onboarding", "testdata", "bw-gopher.ccitt_group4"))
	if err != nil {
		t.Fatal(err)
	}
	grey := image.NewGray(image.Rect(0, 0, 400, 300))
	for i := range grey.Pix {
		grey.Pix[i] = uint8(i % 241)
	}
	var jpg bytes.Buffer
	if err = jpeg.Encode(&jpg, grey, nil); err != nil {
		t.Fatal(err)
	}

	t.Run("at the cap every path is refused and stores nothing; replays and other tenants pass", func(t *testing.T) {
		id, one, two := tenant()
		accept(photo(one, uuid.NewString()))
		firstKey := uuid.NewString()
		first := accept(photo(one, firstKey))
		// Two running pages across two sites: a two-page batch would make four, so it is refused whole.
		before := stored(id)
		full(t, "batch crossing the cap", post(two, "source-scan-batches", "application/pdf", twoPages.Bytes(), uuid.NewString()))
		if after := stored(id); after != before {
			t.Fatalf("a batch crossing the cap stored rows: %v -> %v", before, after)
		}
		accept(photo(two, uuid.NewString())) // the third page, on the other site, fills the cap
		if n := running(id); n != limit {
			t.Fatalf("running pages %d, want %d", n, limit)
		}
		before = stored(id)
		for _, c := range []struct {
			name, route, media string
			body               []byte
		}{
			{"photo", "source-scans", "image/png", pagePNG(t, 300, 200, 41)},
			{"scanned PDF", "source-scan-batches", "application/pdf", faxPDF(g4, "/K -1 /Columns 153 /Rows 55")},
			{"mixed PDF", "source-scan-batches", "application/pdf", mixedPDF(g4)},
			{"typed-PDF figure", "source-documents", "application/pdf", figurePDF(jpg.Bytes())},
		} {
			for _, m := range []member{one, two} {
				full(t, c.name, post(m, c.route, c.media, c.body, uuid.NewString()))
			}
		}
		if after := stored(id); after != before {
			t.Fatalf("refused uploads stored rows: %v -> %v", before, after)
		}
		// rule 07: a stored key replays before the cap is consulted.
		if again := accept(photo(one, firstKey)); again.JobID != first.JobID {
			t.Fatalf("replay at the cap returned job %s, want %s", again.JobID, first.JobID)
		}
		// Another tenant is admitted while this one is full.
		_, other, _ := tenant()
		accept(photo(other, uuid.NewString()))
		// Only running pages count: a finished page frees its place.
		exec(`UPDATE supervisor.document_scan_jobs SET state='quarantined',quarantine_reason='RUN_CANCELED',image=NULL,finished_at=now() WHERE tenant_id=$1 AND job_id=$2`, id, first.JobID)
		accept(photo(two, uuid.NewString()))
		full(t, "photo after refilling", photo(one, uuid.NewString()))
	})

	// WIS-715 review: a page left running (worker down, workflow terminated, history lost) must
	// not hold the tenant's place for ever. Before the fix the count had no age limit, so the
	// tenant stayed at SCAN_QUEUE_FULL until someone edited the database.
	t.Run("a page left running past the admission window stops holding a place", func(t *testing.T) {
		id, one, two := tenant()
		for i := 0; i < limit; i++ {
			accept(photo(one, uuid.NewString()))
		}
		full(t, "photo at the cap", photo(two, uuid.NewString()))
		restore := evidence.SetScanAdmissionWindowForTest(2 * time.Second)
		defer restore()
		time.Sleep(2500 * time.Millisecond)
		accept(photo(two, uuid.NewString()))
		if n := running(id); n != limit+1 {
			t.Fatalf("running pages %d, want %d: the stale pages stay visible as running", n, limit+1)
		}
		// The new page is within the window, so the cap applies to fresh pages again.
		for i := 1; i < limit; i++ {
			accept(photo(two, uuid.NewString()))
		}
		full(t, "photo with the cap full of fresh pages", photo(one, uuid.NewString()))
	})

	t.Run("parallel uploads across two sites never exceed the cap", func(t *testing.T) {
		id, one, two := tenant()
		const uploads = 12
		results := make([]*httptest.ResponseRecorder, uploads)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range uploads {
			m := one
			if i%2 == 1 {
				m = two
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results[i] = photo(m, uuid.NewString())
			}()
		}
		close(start)
		wg.Wait()
		accepted := 0
		for _, w := range results {
			switch {
			case w.Code == 202:
				accepted++
			case w.Code == 503 && code(w) == "SCAN_QUEUE_FULL":
			default:
				t.Fatalf("parallel upload: %d %s", w.Code, w.Body.String())
			}
		}
		if accepted != limit || running(id) != limit || stored(id)[0] != limit {
			t.Fatalf("accepted %d, running %d, stored %d; want %d each", accepted, running(id), stored(id)[0], limit)
		}
	})
}
