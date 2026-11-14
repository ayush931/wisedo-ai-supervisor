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

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
)

func TestSourceAuthorityHTTPProvenanceAndAuthorization(t *testing.T) {
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
	owner, viewer, supervisor := uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, origin := "https://authority-http.invalid/"+uuid.NewString(), "http://127.0.0.1:58176"
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
		for _, table := range []string{"evidence_authority_decisions", "evidence_authority_heads", "evidence_audit", "evidence_receipts", "evidence_revocations", "evidence_spans", "evidence_revisions", "evidence_heads", "memberships", "sites", "tenants"} {
			var exists bool
			if e = tx.QueryRow("SELECT to_regclass($1) IS NOT NULL", "supervisor."+table).Scan(&exists); e != nil {
				t.Error(e)
				return
			}
			if !exists {
				continue
			}
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
	exec("UPDATE supervisor.memberships SET role='viewer' WHERE issuer=$1 AND subject=$2", issuer, viewer)
	exec("INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'supervisor')", tenant, site, issuer, supervisor)
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
	supervisorSession := seedSession(supervisor)
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
	sourceID := uuid.NewString()
	base := "/v1/sites/" + site
	sourcePath := base + "/sources/" + sourceID
	writePath, readPath := sourcePath+"/authority-decisions", sourcePath+"/authority"
	input := evidence.Input{SourceID: sourceID, Kind: "document", Title: "Synthetic SOP", Text: "Current documented process"}
	status(call(ownerSession, "POST", base+"/sources", uuid.NewString(), `"0"`, ownerSession.csrf, encode(input)), 200)
	body := `{"source_revision":1,"expected_source_state_revision":1,"verdict":"designate","reason":"Owner verified current SOP"}`
	key := uuid.NewString()
	status(call(session{}, "POST", writePath, key, `"0"`, "", body), 401)
	status(call(ownerSession, "POST", writePath, key, `"0"`, "", body), 403)
	wantProblem(t, call(ownerSession, "POST", writePath, key, `"0"`, ownerSession.csrf, body), 428, "REAUTHENTICATION_REQUIRED")
	exec("UPDATE supervisor.browser_sessions SET auth_time=now() WHERE issuer=$1", issuer)
	status(call(supervisorSession, "POST", writePath, key, `"0"`, supervisorSession.csrf, body), 403)
	status(call(viewerSession, "POST", writePath, key, `"0"`, viewerSession.csrf, body), 403)
	wantProblem(t, call(ownerSession, "POST", writePath, key, "", ownerSession.csrf, body), 428, "REVISION_REQUIRED")
	for _, field := range []string{`,"issuer":"forged"}`, `,"subject":"forged"}`, `,"tenant_id":"forged"}`, `,"approved":true}`, `,"source_sha256":"forged"}`} {
		status(call(ownerSession, "POST", writePath, key, `"0"`, ownerSession.csrf, strings.TrimSuffix(body, "}")+field), 422)
	}
	for _, invalid := range []string{
		`{"source_revision":1,"expected_source_state_revision":1,"verdict":"approve","reason":"invalid"}`,
		`{"source_revision":1,"expected_source_state_revision":1,"verdict":"designate","reason":""}`,
		`{"source_revision":0,"expected_source_state_revision":1,"verdict":"designate","reason":"invalid"}`,
	} {
		status(call(ownerSession, "POST", writePath, key, `"0"`, ownerSession.csrf, invalid), 422)
	}
	wantProblem(t, call(ownerSession, "POST", writePath, key, `"0"`, ownerSession.csrf, strings.Replace(body, `"source_revision":1`, `"source_revision":2`, 1)), 409, "SOURCE_CHANGED")
	wantProblem(t, call(ownerSession, "POST", writePath, key, `"0"`, ownerSession.csrf, strings.Replace(body, `"expected_source_state_revision":1`, `"expected_source_state_revision":2`, 1)), 409, "SOURCE_CHANGED")
	created := call(ownerSession, "POST", writePath, key, `"0"`, ownerSession.csrf, body)
	status(created, 200)
	type decision struct {
		DecisionID             string `json:"decision_id"`
		SourceID               string `json:"source_id"`
		ActorIssuer            string `json:"actor_issuer"`
		ActorSubject           string `json:"actor_subject"`
		Revision               int64  `json:"revision"`
		CurrentRevision        int64  `json:"current_revision"`
		SourceRevision         int64  `json:"source_revision"`
		SourceSHA256           string `json:"source_sha256"`
		SourceAccessEpoch      int64  `json:"source_access_epoch"`
		Verdict                string `json:"verdict"`
		Reason                 string `json:"reason"`
		ReviewEvidenceEligible bool   `json:"review_evidence_eligible"`
	}
	decode := func(w *httptest.ResponseRecorder) decision {
		t.Helper()
		status(w, 200)
		var d decision
		if e := json.Unmarshal(w.Body.Bytes(), &d); e != nil {
			t.Fatal(e)
		}
		return d
	}
	first := decode(created)
	if _, e := uuid.Parse(first.DecisionID); e != nil || first.SourceID != sourceID || first.ActorIssuer != issuer || first.ActorSubject != owner {
		t.Fatalf("authority actor or identity not server pinned: %+v", first)
	}
	digest := sha256.Sum256([]byte(input.Text))
	if created.Header().Get("ETag") != `"1"` || first.Revision != 1 || first.CurrentRevision != 1 || first.SourceRevision != 1 || first.SourceSHA256 != hex.EncodeToString(digest[:]) || first.SourceAccessEpoch != 1 || first.Verdict != "designate" || !first.ReviewEvidenceEligible {
		t.Fatalf("wrong server-pinned authority decision: %+v", first)
	}
	if replay := decode(call(ownerSession, "POST", writePath, key, `"0"`, ownerSession.csrf, body)); replay != first {
		t.Fatalf("replay differs: %+v", replay)
	}
	wantProblem(t, call(ownerSession, "POST", writePath, key, `"0"`, ownerSession.csrf, strings.Replace(body, "Owner verified current SOP", "different", 1)), 409, "IDEMPOTENCY_CONFLICT")
	wantProblem(t, call(ownerSession, "POST", writePath, uuid.NewString(), `"0"`, ownerSession.csrf, body), 412, "REVISION_CONFLICT")
	if got := decode(call(viewerSession, "GET", readPath, "", "", "", "")); got != first {
		t.Fatalf("viewer read differs: %+v", got)
	}
	var reported evidence.Source
	w := call(viewerSession, "GET", sourcePath, "", "", "", "")
	status(w, 200)
	if e := json.Unmarshal(w.Body.Bytes(), &reported); e != nil || reported.Authority != "reported" {
		t.Fatal("source designation must not grant claim approval or execution authority")
	}
	for _, isolatedSite := range []string{otherSite, foreignSite} {
		status(call(ownerSession, "GET", "/v1/sites/"+isolatedSite+"/sources/"+sourceID+"/authority", "", "", "", ""), 404)
	}
	status(call(viewerSession, "GET", readPath+"?revision=0", "", "", "", ""), 422)
	status(call(viewerSession, "GET", readPath+"?revision=99", "", "", "", ""), 404)
	input.Text = "Updated process requires another owner decision"
	status(call(ownerSession, "POST", base+"/sources", uuid.NewString(), `"1"`, ownerSession.csrf, encode(input)), 200)
	stale := decode(call(viewerSession, "GET", readPath, "", "", "", ""))
	if stale.ReviewEvidenceEligible || stale.SourceSHA256 != first.SourceSHA256 || stale.SourceRevision != 1 {
		t.Fatalf("source change silently promoted old designation: %+v", stale)
	}
	if replay := decode(call(ownerSession, "POST", writePath, key, `"0"`, ownerSession.csrf, body)); replay.ReviewEvidenceEligible {
		t.Fatal("stale replay retained eligibility")
	}
	wantProblem(t, call(ownerSession, "POST", writePath, uuid.NewString(), `"1"`, ownerSession.csrf, body), 409, "SOURCE_CHANGED")
	body2 := `{"source_revision":2,"expected_source_state_revision":2,"verdict":"designate","reason":"Owner reviewed revised SOP"}`
	second := decode(call(ownerSession, "POST", writePath, uuid.NewString(), `"1"`, ownerSession.csrf, body2))
	if second.Revision != 2 || second.SourceRevision != 2 || !second.ReviewEvidenceEligible || second.SourceSHA256 == first.SourceSHA256 {
		t.Fatalf("redesignation failed: %+v", second)
	}
	withdrawBody := `{"source_revision":2,"expected_source_state_revision":2,"verdict":"withdraw","reason":"Needs further validation"}`
	withdrawn := decode(call(ownerSession, "POST", writePath, uuid.NewString(), `"2"`, ownerSession.csrf, withdrawBody))
	if withdrawn.Revision != 3 || withdrawn.Verdict != "withdraw" || withdrawn.ReviewEvidenceEligible {
		t.Fatalf("withdrawal eligible: %+v", withdrawn)
	}
	for _, revision := range []string{"1", "2"} {
		w := call(viewerSession, "GET", readPath+"?revision="+revision, "", "", "", "")
		old := decode(w)
		if old.CurrentRevision != 3 || old.ReviewEvidenceEligible || w.Header().Get("ETag") != `"`+revision+`"` {
			t.Fatalf("historical decision became current: %+v", old)
		}
	}
	status(call(ownerSession, "POST", sourcePath+"/revocations", uuid.NewString(), `"2"`, ownerSession.csrf, `{"reason":"Source revoked"}`), 200)
	for _, path := range []string{readPath, readPath + "?revision=1", readPath + "?revision=2"} {
		status(call(viewerSession, "GET", path, "", "", "", ""), 404)
	}
	status(call(ownerSession, "POST", writePath, key, `"0"`, ownerSession.csrf, body), 404)
	status(call(ownerSession, "POST", writePath, uuid.NewString(), `"3"`, ownerSession.csrf, body2), 404)
}
