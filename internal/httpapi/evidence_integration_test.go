//go:build integration

package httpapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
)

func TestEvidenceHTTPProvenanceAuthorityAndRevocation(t *testing.T) {
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: actual disposable PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
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
	owner, viewer := uuid.NewString(), uuid.NewString()
	issuer, origin := "https://evidence-http.invalid/"+uuid.NewString(), "http://127.0.0.1:58176"
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
		for _, table := range []string{"evidence_audit", "evidence_receipts", "evidence_revocations", "evidence_spans", "evidence_revisions", "evidence_heads", "memberships", "sites", "tenants"} {
			column := "tenant_id"
			if table == "tenants" {
				column = "id"
			}
			for _, scopedTenant := range []string{tenant, foreignTenant} {
				if _, e = tx.Exec("SELECT set_config('supervisor.tenant_id',$1,true)", scopedTenant); e != nil {
					t.Error(e)
					return
				}
				if _, e = tx.Exec("DELETE FROM supervisor."+table+" WHERE "+column+"=$1", scopedTenant); e != nil {
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
	exec("INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic evidence HTTP'),($2,'synthetic foreign')", tenant, foreignTenant)
	exec("INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'evidence site'),($1,$3,'second site'),($4,$5,'foreign site')", tenant, site, otherSite, foreignTenant, foreignSite)
	exec("INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner'),($1,$2,$3,$5,'reviewer'),($1,$6,$3,$4,'owner')", tenant, site, issuer, owner, viewer, otherSite)
	type session struct{ token, csrf, hash string }
	seedSession := func(subject string) session {
		t.Helper()
		v := session{token: uuid.NewString(), csrf: uuid.NewString()}
		sum := sha256.Sum256([]byte(v.token))
		v.hash = hex.EncodeToString(sum[:])
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',NULL)`, v.hash, subject, issuer, v.csrf)
		return v
	}
	ownerSession, viewerSession := seedSession(owner), seedSession(viewer)
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "evidence-test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, db.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)
	call := func(actor session, method, path, key, revision, csrf, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if actor.token != "" {
			r.AddCookie(&http.Cookie{Name: "supervisor", Value: actor.token})
		}
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", csrf)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set("If-Match", revision)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		conformsToPublished(t, r, w)
		return w
	}
	status := func(w *httptest.ResponseRecorder, want int) {
		t.Helper()
		if w.Code != want {
			t.Fatalf("HTTP %d want %d: %s", w.Code, want, w.Body.String())
		}
	}
	encode := func(value any) string {
		t.Helper()
		b, e := json.Marshal(value)
		if e != nil {
			t.Fatal(e)
		}
		return string(b)
	}
	sourceID, createKey := uuid.NewString(), uuid.NewString()
	originalText := "A中🙂e\u0301Z"
	input := evidence.Input{SourceID: sourceID, Kind: "interview", Title: "Operator account", Text: originalText}
	body := encode(input)
	base := "/v1/sites/" + site
	sourcePath := base + "/sources/" + sourceID
	status(call(session{}, "POST", base+"/sources", createKey, `"0"`, ownerSession.csrf, body), 401)
	status(call(ownerSession, "POST", base+"/sources", createKey, `"0"`, "", body), 403)
	wantProblem(t, call(ownerSession, "POST", base+"/sources", createKey, "", ownerSession.csrf, body), 428, "REVISION_REQUIRED")
	status(call(ownerSession, "POST", base+"/sources", createKey, `W/"0"`, ownerSession.csrf, body), 422)
	status(call(ownerSession, "POST", base+"/sources", "", `"0"`, ownerSession.csrf, body), 422)
	for _, field := range []string{`,"authority":"approved"}`, `,"tenant_id":"forged"}`, `,"sha256":"forged"}`} {
		status(call(ownerSession, "POST", base+"/sources", createKey, `"0"`, ownerSession.csrf, strings.TrimSuffix(body, "}")+field), 422)
	}
	for _, badText := range []string{`"\ud800"`, `"\udc00"`, `"\ud800x"`, "\"" + string([]byte{0xff}) + "\""} {
		bad := `{"source_id":"` + sourceID + `","kind":"interview","title":"bad","text":` + badText + `}`
		status(call(ownerSession, "POST", base+"/sources", createKey, `"0"`, ownerSession.csrf, bad), 422)
	}
	status(call(viewerSession, "POST", base+"/sources", createKey, `"0"`, viewerSession.csrf, body), 403)
	created := call(ownerSession, "POST", base+"/sources", createKey, `"0"`, ownerSession.csrf, body)
	status(created, 200)
	if created.Header().Get("ETag") != `"1"` {
		t.Fatal("missing source revision ETag")
	}
	var source evidence.Source
	if err = json.Unmarshal(created.Body.Bytes(), &source); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(originalText))
	if source.SHA256 != hex.EncodeToString(digest[:]) || source.Text != originalText || source.ByteLength != len(originalText) || source.RuneLength != utf8.RuneCountInString(originalText) || source.Authority != "reported" || source.Revision != 1 {
		t.Fatalf("provenance differs: %+v", source)
	}
	status(call(ownerSession, "POST", base+"/sources", createKey, `"0"`, ownerSession.csrf, body), 200)
	if source.AuthorityClass != "reported" || source.GroupID != sourceID {
		t.Fatalf("WIS-270 defaults differ: class=%q group=%q", source.AuthorityClass, source.GroupID)
	}
	// WIS-270: a group from another site or tenant is non-disclosing 404; a closed enum is 422.
	otherSource := evidence.Input{SourceID: uuid.NewString(), Kind: "document", Title: "other site SOP", Text: "Other site SOP " + uuid.NewString()}
	status(call(ownerSession, "POST", "/v1/sites/"+otherSite+"/sources", uuid.NewString(), `"0"`, ownerSession.csrf, encode(otherSource)), 200)
	for _, group := range []string{otherSource.SourceID, uuid.NewString()} {
		grouped := evidence.Input{SourceID: uuid.NewString(), Kind: "document", Title: "grouped", Text: "Grouped " + uuid.NewString(), IndependenceGroupID: group}
		w := call(ownerSession, "POST", base+"/sources", uuid.NewString(), `"0"`, ownerSession.csrf, encode(grouped))
		status(w, 404)
		if !strings.Contains(w.Body.String(), `"NOT_FOUND"`) {
			t.Fatalf("cross-site group disclosed: %s", w.Body.String())
		}
	}
	for _, field := range []string{`"authority_class":"designated"`, `"independence_group_id":"not-a-uuid"`, `"independence_group_id":1`} {
		bad := `{"source_id":"` + uuid.NewString() + `","kind":"document","title":"bad","text":"bad ` + uuid.NewString() + `",` + field + `}`
		w := call(ownerSession, "POST", base+"/sources", uuid.NewString(), `"0"`, ownerSession.csrf, bad)
		status(w, 422)
	}
	changed := input
	changed.Text = "A different observation"
	wantProblem(t, call(ownerSession, "POST", base+"/sources", createKey, `"0"`, ownerSession.csrf, encode(changed)), 409, "IDEMPOTENCY_CONFLICT")
	wantProblem(t, call(ownerSession, "POST", base+"/sources", uuid.NewString(), `"0"`, ownerSession.csrf, encode(changed)), 412, "REVISION_CONFLICT")
	status(call(viewerSession, "GET", sourcePath, "", "", "", ""), 200)
	status(call(ownerSession, "GET", "/v1/sites/"+otherSite+"/sources/"+sourceID, "", "", "", ""), 404)
	status(call(ownerSession, "GET", "/v1/sites/"+foreignSite+"/sources/"+sourceID, "", "", "", ""), 404)
	status(call(ownerSession, "GET", base+"/sources?limit=101", "", "", "", ""), 422)
	status(call(ownerSession, "GET", base+"/sources?limit=1", "", "", "", ""), 200)
	spanBody := `{"revision":1,"start_char":1,"end_char":3}`
	spanKey := uuid.NewString()
	status(call(viewerSession, "POST", sourcePath+"/spans", spanKey, `"1"`, viewerSession.csrf, spanBody), 403)
	spanResponse := call(ownerSession, "POST", sourcePath+"/spans", spanKey, `"1"`, ownerSession.csrf, spanBody)
	status(spanResponse, 200)
	var span evidence.Span
	if err = json.Unmarshal(spanResponse.Body.Bytes(), &span); err != nil {
		t.Fatal(err)
	}
	if span.Text != "中🙂" || span.Start != 1 || span.End != 3 || span.SourceSHA256 != source.SHA256 {
		t.Fatalf("Unicode citation mismatch %+v", span)
	}
	spanPath := base + "/source-spans/" + span.ID
	status(call(viewerSession, "GET", spanPath, "", "", "", ""), 200)
	status(call(ownerSession, "GET", "/v1/sites/"+otherSite+"/source-spans/"+span.ID, "", "", "", ""), 404)
	status(call(ownerSession, "POST", sourcePath+"/spans", uuid.NewString(), `"1"`, ownerSession.csrf, `{"revision":1,"start_char":1,"end_char":1000}`), 422)
	revised := call(ownerSession, "POST", base+"/sources", uuid.NewString(), `"1"`, ownerSession.csrf, encode(changed))
	status(revised, 200)
	if revised.Header().Get("ETag") != `"2"` {
		t.Fatal("revision did not advance")
	}
	old := call(viewerSession, "GET", sourcePath+"?revision=1", "", "", "", "")
	status(old, 200)
	var oldSource evidence.Source
	if err = json.Unmarshal(old.Body.Bytes(), &oldSource); err != nil {
		t.Fatal(err)
	}
	if oldSource.Text != originalText || oldSource.SHA256 != source.SHA256 || oldSource.Revision != 1 || oldSource.CurrentRevision != 2 {
		t.Fatal("old revision was rewritten")
	}
	oldSpan := call(viewerSession, "GET", spanPath, "", "", "", "")
	status(oldSpan, 200)
	var persistedSpan evidence.Span
	if err = json.Unmarshal(oldSpan.Body.Bytes(), &persistedSpan); err != nil {
		t.Fatal(err)
	}
	if persistedSpan.Text != span.Text || persistedSpan.SourceSHA256 != span.SourceSHA256 {
		t.Fatal("old citation changed after revision")
	}
	var persistedText, persistedSHA string
	if err = admin.QueryRowContext(ctx, "SELECT body,sha256 FROM supervisor.evidence_revisions WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND revision=1", tenant, site, sourceID).Scan(&persistedText, &persistedSHA); err != nil {
		t.Fatal(err)
	}
	if persistedText != originalText || persistedSHA != source.SHA256 {
		t.Fatal("database revision differs from receipt")
	}
	revokeKey := uuid.NewString()
	revokeBody := `{"reason":"source withdrawn by owner"}`
	wantProblem(t, call(ownerSession, "POST", sourcePath+"/revocations", revokeKey, `"2"`, ownerSession.csrf, revokeBody), 428, "REAUTHENTICATION_REQUIRED")
	exec("UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash IN($1,$2)", ownerSession.hash, viewerSession.hash)
	status(call(viewerSession, "POST", sourcePath+"/revocations", revokeKey, `"2"`, viewerSession.csrf, revokeBody), 403)
	wantProblem(t, call(ownerSession, "POST", sourcePath+"/revocations", revokeKey, `"1"`, ownerSession.csrf, revokeBody), 412, "REVISION_CONFLICT")
	revoked := call(ownerSession, "POST", sourcePath+"/revocations", revokeKey, `"2"`, ownerSession.csrf, revokeBody)
	status(revoked, 200)
	var revocation evidence.Revocation
	if err = json.Unmarshal(revoked.Body.Bytes(), &revocation); err != nil {
		t.Fatal(err)
	}
	if !revocation.Revoked || revocation.StateRevision != 3 || revocation.AccessEpoch != 2 {
		t.Fatalf("revocation receipt %+v", revocation)
	}
	for _, path := range []string{sourcePath, sourcePath + "?revision=1", spanPath} {
		w := call(viewerSession, "GET", path, "", "", "", "")
		status(w, 404)
		if strings.Contains(w.Body.String(), originalText) || strings.Contains(w.Body.String(), span.Text) {
			t.Fatal("revoked evidence leaked")
		}
	}
	status(call(ownerSession, "POST", base+"/sources", createKey, `"0"`, ownerSession.csrf, body), 404)
	status(call(ownerSession, "POST", sourcePath+"/spans", spanKey, `"1"`, ownerSession.csrf, spanBody), 404)
	pageResponse := call(ownerSession, "GET", base+"/sources", "", "", "", "")
	status(pageResponse, 200)
	var page evidence.Page
	if err = json.Unmarshal(pageResponse.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Sources) != 0 {
		t.Fatal("revoked source still listed")
	}
	// Reads and mutations have real append-only audit rows, without original text.
	rows, err := admin.QueryContext(ctx, "SELECT event_type,coalesce(reason,'') FROM supervisor.evidence_audit WHERE tenant_id=$1 AND site_id=$2", tenant, site)
	if err != nil {
		t.Fatal(err)
	}
	events := map[string]int{}
	for rows.Next() {
		var event, reason string
		if err = rows.Scan(&event, &reason); err != nil {
			t.Fatal(err)
		}
		events[event]++
		if strings.Contains(reason, originalText) {
			t.Fatal("raw source leaked into audit")
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	for _, event := range []string{"source.created", "source.revised", "source.read", "source.listed", "span.created", "span.read", "source.revoked", "mutation.replayed"} {
		if events[event] == 0 {
			t.Fatalf("missing persisted audit %s", event)
		}
	}
	if events["source.created"] != 1 || events["source.revised"] != 1 || events["source.revoked"] != 1 {
		t.Fatalf("duplicate effects %+v", events)
	}
	exec("UPDATE supervisor.memberships SET active=false WHERE tenant_id=$1 AND site_id=$2 AND subject=$3", tenant, site, owner)
	status(call(ownerSession, "GET", base+"/sources", "", "", "", ""), 404)
}
