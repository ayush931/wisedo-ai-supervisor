//go:build integration

package httpapi

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/onboarding"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
)

const (
	wordMedia  = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	excelMedia = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
)

// officePackage builds a minimal Office Open XML package from its parts.
func officePackage(t *testing.T, parts map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range parts {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func wordDocument(t *testing.T, body string) []byte {
	t.Helper()
	return officePackage(t, map[string]string{
		"[Content_Types].xml": "<Types/>",
		"word/document.xml":   `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` + body + `</w:body></w:document>`,
	})
}

// TestSourceDocumentUploadPostgres (WIS-613) drives POST /source-documents as the runtime role: an
// uploaded Word document becomes a document source with spans citing its passages, a replay
// returns the same result, refusals write nothing, and authority is rechecked on every request.
func TestSourceDocumentUploadPostgres(t *testing.T) {
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: actual disposable PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	db, err := store.New(runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.VerifyRole(ctx); err != nil {
		t.Fatal(err)
	}
	tenant, site, otherSite := uuid.NewString(), uuid.NewString(), uuid.NewString()
	foreignTenant, foreignSite := uuid.NewString(), uuid.NewString()
	operator, reviewer, neighbour, outsider := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, origin := "https://source-documents.invalid/"+uuid.NewString(), "http://127.0.0.1:58176"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := admin.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	defer func() {
		tx, e := admin.BeginTx(context.Background(), nil)
		if e != nil {
			t.Error(e)
			return
		}
		defer tx.Rollback()
		if _, e = tx.Exec("SET CONSTRAINTS ALL DEFERRED"); e != nil {
			t.Error(e)
			return
		}
		for _, table := range []string{"evidence_audit", "evidence_receipts", "evidence_spans", "evidence_revisions", "evidence_heads", "memberships", "sites", "tenants"} {
			column := "tenant_id"
			if table == "tenants" {
				column = "id"
			}
			for _, scoped := range []string{tenant, foreignTenant} {
				if _, e = tx.Exec("SELECT set_config('supervisor.tenant_id',$1,true)", scoped); e != nil {
					t.Error(e)
					return
				}
				if _, e = tx.Exec("DELETE FROM supervisor."+table+" WHERE "+column+"=$1", scoped); e != nil {
					t.Error(e)
					return
				}
			}
		}
		if _, e = tx.Exec("DELETE FROM supervisor.browser_sessions WHERE issuer=$1", issuer); e != nil {
			t.Error(e)
			return
		}
		if e = tx.Commit(); e != nil {
			t.Error(e)
		}
	}()
	exec("INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic document upload'),($2,'synthetic foreign')", tenant, foreignTenant)
	exec("INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'upload site'),($1,$3,'neighbour site'),($4,$5,'foreign site')", tenant, site, otherSite, foreignTenant, foreignSite)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES
		($1,$2,$3,$4,'operator'),($1,$2,$3,$5,'reviewer'),($1,$6,$3,$7,'owner'),($8,$9,$3,$10,'owner')`,
		tenant, site, issuer, operator, reviewer, otherSite, neighbour, foreignTenant, foreignSite, outsider)
	type session struct{ token, csrf string }
	seedSession := func(subject string) session {
		t.Helper()
		v := session{token: uuid.NewString(), csrf: uuid.NewString()}
		sum := sha256.Sum256([]byte(v.token))
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',NULL)`, hex.EncodeToString(sum[:]), subject, issuer, v.csrf)
		return v
	}
	operatorS, reviewerS, neighbourS, outsiderS := seedSession(operator), seedSession(reviewer), seedSession(neighbour), seedSession(outsider)
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "source-documents-test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, db.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)

	type upload struct {
		actor        session
		csrf, key    string
		title        []string // header values, already percent-encoded
		media, query string
		body         []byte
	}
	serve := func(u upload) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/sites/"+site+"/source-documents"+u.query, bytes.NewReader(u.body))
		if u.actor.token != "" {
			r.AddCookie(&http.Cookie{Name: "supervisor", Value: u.actor.token})
		}
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", u.csrf)
		r.Header.Set("Content-Type", u.media)
		if u.key != "" {
			r.Header.Set("Idempotency-Key", u.key)
		}
		for _, v := range u.title {
			r.Header.Add("X-Source-Title", v)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		conformsToPublished(t, r, w)
		return w
	}
	count := func(table string) int {
		t.Helper()
		tx, e := admin.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		if _, e = tx.Exec("SELECT set_config('supervisor.tenant_id',$1,true)", tenant); e != nil {
			t.Fatal(e)
		}
		var n int
		if e = tx.QueryRow("SELECT count(*) FROM supervisor."+table+" WHERE tenant_id=$1 AND site_id=$2", tenant, site).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	decode := func(w *httptest.ResponseRecorder) onboarding.DocumentResult {
		t.Helper()
		if w.Code != 200 {
			t.Fatalf("HTTP %d want 200: %s", w.Code, w.Body.String())
		}
		var out onboarding.DocumentResult
		if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil {
			t.Fatal(e)
		}
		return out
	}

	sop := wordDocument(t, `<w:p><w:r><w:t>Receiving SOP</w:t></w:r></w:p>`+
		`<w:p><w:r><w:t>Steel coil from Bharat Steel is checked at Gate 2.</w:t></w:r></w:p>`+
		`<w:tbl><w:tr><w:tc><w:p><w:r><w:t>Step</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>Cycle (s)</w:t></w:r></w:p></w:tc></w:tr>`+
		`<w:tr><w:tc><w:p><w:r><w:t>Press</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>42</w:t></w:r></w:p></w:tc></w:tr></w:tbl>`)
	title := "प्राप्ति SOP / Receiving"
	encodedTitle := url.PathEscape(title)
	good := upload{actor: operatorS, csrf: operatorS.csrf, key: uuid.NewString(), title: []string{encodedTitle}, media: wordMedia, body: sop}
	with := func(change func(*upload)) upload { u := good; change(&u); return u }

	// Transport refusals come first and write nothing.
	wantProblem(t, serve(with(func(u *upload) { u.actor = session{} })), 401, "AUTHENTICATION_REQUIRED")
	wantProblem(t, serve(with(func(u *upload) { u.csrf = "" })), 403, "CSRF_REJECTED")
	wantProblem(t, serve(with(func(u *upload) { u.key = "" })), 422, "INVALID_REQUEST")
	for name, values := range map[string][]string{"missing": nil, "repeated": {encodedTitle, encodedTitle}, "bad escape": {"SOP%zz"}, "invalid utf8": {"%ff"}, "blank": {"%20"}, "control character": {"SOP%0Ainjected"},
		// Audit gap (WIS-613): a bidi override reorders how the title displays (rule 08 spoofing).
		"bidi override": {"SOP%E2%80%AEtxt.exe"}} {
		if w := serve(with(func(u *upload) { u.title = values })); w.Code != 422 {
			t.Fatalf("title %s: HTTP %d want 422", name, w.Code)
		}
	}
	wantProblem(t, serve(with(func(u *upload) { u.title = []string{url.PathEscape(strings.Repeat("é", 201))} })), 422, "INVALID_SOURCE")
	wantProblem(t, serve(with(func(u *upload) { u.query = "?title=SOP" })), 422, "INVALID_REQUEST")
	wantProblem(t, serve(with(func(u *upload) { u.body = bytes.Repeat([]byte("a"), 20<<20+1); u.media = "text/plain" })), 413, "LIMIT_EXCEEDED")
	// WIS-624: a PDF is parsed in the resource-capped child; the file's own refusals are 4xx
	// with their own codes (a client retrying a 5xx would resend a file that can never pass).
	pdfMedia := "application/pdf"
	if !onboarding.PDFSandboxSupported {
		t.Fatalf("BLOCKED: the PDF parser sandbox needs linux/amd64, this is %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	wantProblem(t, serve(with(func(u *upload) { u.body, u.media = []byte("%PDF-1.7\n1 0 obj\n%%EOF"), pdfMedia })), 422, "UNREADABLE_SOURCE")
	wantProblem(t, serve(with(func(u *upload) { u.body, u.media = testPDF("Hello", "/Encrypt 9 0 R", ""), pdfMedia })), 422, "PDF_ENCRYPTED")
	wantProblem(t, serve(with(func(u *upload) {
		u.body, u.media = testPDF("Hello", "/OpenAction << /S /JavaScript /JS (app.alert(1)) >>", ""), pdfMedia
	})), 415, "PDF_ACTIVE_CONTENT")
	wantProblem(t, serve(with(func(u *upload) {
		u.body, u.media = testPDF("Hello", "", "/Annots [<< /Type /Annot /Subtype /Link /Rect [0 0 1 1] /A << /S /URI /URI (http://x.invalid) >> >>]"), pdfMedia
	})), 415, "PDF_EXTERNAL_REFERENCE")
	wantProblem(t, serve(with(func(u *upload) { u.body, u.media = testPDF("", "", ""), pdfMedia })), 422, "PDF_NO_TEXT_LAYER")
	wantProblem(t, serve(with(func(u *upload) { u.media = "text/csv" })), 415, "UNSUPPORTED_MEDIA")
	wantProblem(t, serve(with(func(u *upload) { u.body = append([]byte("MZ"), make([]byte, 64)...) })), 415, "UNSUPPORTED_MEDIA")
	macro := officePackage(t, map[string]string{"[Content_Types].xml": "<Types/>", "xl/workbook.xml": "<workbook/>", "xl/vbaProject.bin": "x"})
	wantProblem(t, serve(with(func(u *upload) { u.body, u.media = macro, excelMedia })), 422, "UNREADABLE_SOURCE")
	wantProblem(t, serve(with(func(u *upload) { u.actor, u.csrf = reviewerS, reviewerS.csrf })), 403, "PERMISSION_DENIED")
	// Authority is decided before the file is inspected: a member who cannot write, or a caller
	// outside the site, is refused for who they are, never for what they sent.
	wantProblem(t, serve(with(func(u *upload) { u.actor, u.csrf, u.body, u.media = reviewerS, reviewerS.csrf, macro, excelMedia })), 403, "PERMISSION_DENIED")
	wantProblem(t, serve(with(func(u *upload) { u.actor, u.csrf, u.body, u.media = neighbourS, neighbourS.csrf, macro, excelMedia })), 404, "NOT_FOUND")
	wantProblem(t, serve(with(func(u *upload) { u.actor, u.csrf = neighbourS, neighbourS.csrf })), 404, "NOT_FOUND")
	wantProblem(t, serve(with(func(u *upload) { u.actor, u.csrf = outsiderS, outsiderS.csrf })), 404, "NOT_FOUND")
	if n := count("evidence_heads") + count("evidence_spans") + count("evidence_receipts"); n != 0 {
		t.Fatalf("refused uploads wrote %d evidence rows", n)
	}

	// A member's upload stores the extracted text as a document source with citable spans. Two
	// copies of the first request racing on one key create it once; the other replays it.
	var wg sync.WaitGroup
	first := make([]*httptest.ResponseRecorder, 2)
	for i := range first {
		wg.Add(1)
		go func() { defer wg.Done(); first[i] = serve(good) }()
	}
	wg.Wait()
	doc := decode(first[0])
	if raced := decode(first[1]); raced.Source.SourceID != doc.Source.SourceID || len(raced.Spans) != 1 || len(doc.Spans) != 1 || raced.Spans[0].ID != doc.Spans[0].ID {
		t.Fatalf("racing copies of one upload differ: %+v / %+v", raced, doc)
	}
	for _, w := range first {
		if w.Header().Get("ETag") != `"1"` {
			t.Fatalf("ETag %q", w.Header().Get("ETag"))
		}
	}
	sum := sha256.Sum256(sop)
	extracted := "Receiving SOP\nSteel coil from Bharat Steel is checked at Gate 2.\nStep\tCycle (s)\nPress\t42\n\n"
	if doc.Kind != onboarding.KindDOCX || doc.OriginalSHA256 != hex.EncodeToString(sum[:]) || doc.Source.Kind != "document" ||
		doc.Source.Title != title || doc.Source.Text != extracted || doc.Source.Revision != 1 || doc.Source.Authority != "reported" {
		t.Fatalf("document source differs: kind=%q sha=%q source=%+v", doc.Kind, doc.OriginalSHA256, doc.Source)
	}
	if len(doc.Spans) != 1 || doc.Spans[0].Text != strings.TrimSpace(extracted) || doc.Spans[0].SourceID != doc.Source.SourceID ||
		doc.Spans[0].Revision != 1 || doc.Spans[0].SourceSHA256 != doc.Source.SHA256 {
		t.Fatalf("spans differ: %+v", doc.Spans)
	}
	if count("evidence_heads") != 1 || count("evidence_spans") != 1 {
		t.Fatal("one upload must store exactly one source and its spans")
	}

	// A later retry with the same key and file replays the original result.
	if again := decode(serve(good)); again.Source.SourceID != doc.Source.SourceID || len(again.Spans) != 1 || again.Spans[0].ID != doc.Spans[0].ID || again.OriginalSHA256 != doc.OriginalSHA256 {
		t.Fatalf("replay differs: %+v", again)
	}
	// A different file under the same key is a conflict, even when its text is identical.
	resaved := wordDocument(t, `<w:p><w:r><w:t>Receiving SOP</w:t></w:r></w:p>`+
		`<w:p><w:r><w:t>Steel coil from Bharat Steel is checked at Gate 2.</w:t></w:r></w:p>`+
		`<w:tbl><w:tr><w:tc><w:p><w:r><w:t xml:space="preserve">Step</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>Cycle (s)</w:t></w:r></w:p></w:tc></w:tr>`+
		`<w:tr><w:tc><w:p><w:r><w:t>Press</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>42</w:t></w:r></w:p></w:tc></w:tr></w:tbl>`)
	wantProblem(t, serve(with(func(u *upload) { u.body = resaved })), 409, "IDEMPOTENCY_CONFLICT")
	if count("evidence_heads") != 1 || count("evidence_spans") != 1 {
		t.Fatal("a refused replay wrote evidence")
	}

	// A CSV upload under a new key is a second source; its rows are kept as stored.
	csv := decode(serve(with(func(u *upload) {
		u.key, u.media, u.title = uuid.NewString(), "text/csv", []string{"Suppliers"}
		u.body = []byte("supplier,material,lead_time_days\r\nBharat Steel,Steel coil,14\r\n")
	})))
	if csv.Kind != onboarding.KindText || csv.Source.Text != "supplier,material,lead_time_days\nBharat Steel,Steel coil,14\n" || len(csv.Spans) != 1 {
		t.Fatalf("csv source differs: %+v", csv)
	}

	// A file over the 1 MiB JSON body limit is read under the 20 MiB document limit: an illustrated
	// SOP whose picture makes it 2 MiB is accepted, and only its text is kept.
	picture := make([]byte, 2<<20)
	if _, err = rand.Read(picture); err != nil {
		t.Fatal(err)
	}
	illustrated := officePackage(t, map[string]string{
		"[Content_Types].xml":   "<Types/>",
		"word/document.xml":     `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>Inspect weld seam</w:t></w:r></w:p></w:body></w:document>`,
		"word/media/image1.png": string(picture),
	})
	if len(illustrated) <= 1<<20 {
		t.Fatalf("fixture is %d bytes, not over the JSON body limit", len(illustrated))
	}
	if big := decode(serve(with(func(u *upload) { u.key, u.body = uuid.NewString(), illustrated }))); big.Source.Text != "Inspect weld seam\n" || len(big.Spans) != 1 {
		t.Fatalf("illustrated document differs: %+v", big.Source)
	}

	// WIS-624: a born-digital PDF becomes a document source like any other upload.
	pdf := testPDF("The press takes 42 seconds a part.", "", "")
	if got := decode(serve(with(func(u *upload) { u.key, u.body, u.media = uuid.NewString(), pdf, pdfMedia }))); got.Kind != onboarding.KindPDF ||
		got.Source.Kind != "document" || got.Source.Text != "The press takes 42 seconds a part.\n" || len(got.Spans) != 1 || got.Spans[0].Text != "The press takes 42 seconds a part." {
		t.Fatalf("pdf document differs: %+v", got)
	}

	// Authority is rechecked before a replay: a deactivated member gets the non-disclosing 404.
	exec("UPDATE supervisor.memberships SET active=false WHERE tenant_id=$1 AND site_id=$2 AND subject=$3", tenant, site, operator)
	wantProblem(t, serve(good), 404, "NOT_FOUND")
}

// testPDF is a one-page PDF with text (none when empty), extra catalog entries and extra page
// entries, with computed cross-reference offsets.
func testPDF(text, catalogExtra, pageExtra string) []byte {
	content := ""
	if text != "" {
		content = "BT /F1 12 Tf 72 720 Td (" + text + ") Tj ET"
	}
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R " + catalogExtra + " >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R " + pageExtra + " >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		"<< /Length " + strconv.Itoa(len(content)) + " >>\nstream\n" + content + "\nendstream",
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.7\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		b.WriteString(strconv.Itoa(i+1) + " 0 obj\n" + o + "\nendobj\n")
	}
	xref := b.Len()
	b.WriteString("xref\n0 " + strconv.Itoa(len(objs)+1) + "\n0000000000 65535 f \n")
	for _, off := range offsets {
		b.WriteString(fmt.Sprintf("%010d 00000 n \n", off))
	}
	b.WriteString("trailer\n<< /Size " + strconv.Itoa(len(objs)+1) + " /Root 1 0 R >>\nstartxref\n" + strconv.Itoa(xref) + "\n%%EOF\n")
	return b.Bytes()
}
