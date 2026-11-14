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
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
)

func TestKnowledgeHTTPReviewAuthorityAndScope(t *testing.T) {
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
	tenant, site, otherSite, foreignTenant, foreignSite := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	owner, viewer, supervisor := uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, origin := "https://reviews-http.invalid/"+uuid.NewString(), "http://127.0.0.1:58176"
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
		for _, table := range []string{"knowledge_claim_reviews", "knowledge_conflicts", "knowledge_audit", "knowledge_receipts", "knowledge_claim_spans", "knowledge_claim_entity_refs", "knowledge_claim_revisions", "knowledge_claim_heads", "knowledge_entity_revisions", "knowledge_entity_heads", "knowledge_entities", "evidence_authority_decisions", "evidence_authority_heads", "evidence_audit", "evidence_receipts", "evidence_revocations", "evidence_spans", "evidence_revisions", "evidence_heads", "memberships", "sites", "tenants"} {
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
	exec("INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic candidate HTTP'),($2,'foreign candidate')", tenant, foreignTenant)
	exec("INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'candidate site'),($1,$3,'other candidate site'),($4,$5,'foreign')", tenant, site, otherSite, foreignTenant, foreignSite)
	exec("INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner'),($1,$2,$3,$5,'reviewer'),($1,$6,$3,$4,'owner')", tenant, site, issuer, owner, viewer, otherSite)
	type actorSession struct{ token, csrf, hash string }
	seed := func(subject string) actorSession {
		t.Helper()
		a := actorSession{token: uuid.NewString(), csrf: uuid.NewString()}
		sum := sha256.Sum256([]byte(a.token))
		a.hash = hex.EncodeToString(sum[:])
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, a.hash, subject, issuer, a.csrf)
		return a
	}
	writer, reader := seed(owner), seed(viewer)
	exec("INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'supervisor')", tenant, site, issuer, supervisor)
	supervisorSession := seed(supervisor)
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "knowledge-test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, db.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)
	call := func(actor actorSession, method, path, key, revision, csrf, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if actor.token != "" {
			r.AddCookie(&http.Cookie{Name: "supervisor", Value: actor.token})
		}
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-CSRF-Token", csrf)
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
	base := "/v1/sites/" + site
	// First RED is missing route; later RED is the missing review persistence.
	status(call(actorSession{}, "POST", base+"/claims/"+uuid.NewString()+"/reviews", uuid.NewString(), `"1"`, "", `{"claim_revision":1,"verdict":"approve","reason":"Reviewed"}`), 401)
	source := evidence.Input{SourceID: uuid.NewString(), Kind: "document", Title: "Synthetic validated SOP", Text: "Machine capacity 100 units/hour"}
	status(call(writer, "POST", base+"/sources", uuid.NewString(), `"0"`, writer.csrf, encode(source)), 200)
	spanResponse := call(writer, "POST", base+"/sources/"+source.SourceID+"/spans", uuid.NewString(), `"1"`, writer.csrf, `{"revision":1,"start_char":0,"end_char":8}`)
	status(spanResponse, 200)
	var span evidence.Span
	if e := json.Unmarshal(spanResponse.Body.Bytes(), &span); e != nil {
		t.Fatal(e)
	}
	entity := knowledge.EntityInput{EntityID: uuid.NewString(), Type: "asset", AssetKind: "machine", Name: "Synthetic machine", SourceID: source.SourceID, SourceRevision: 1}
	status(call(writer, "POST", base+"/entities", uuid.NewString(), `"0"`, writer.csrf, encode(entity)), 200)
	claim := knowledge.ClaimInput{ClaimID: uuid.NewString(), SubjectID: entity.EntityID, SubjectRevision: 1, Predicate: "capacity_per_hour", Literal: &knowledge.Literal{Type: "decimal", Value: json.RawMessage(`"100"`), Unit: "units/hour"}, EffectiveFrom: "2026-09-27T10:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
	writeClaim := func(in knowledge.ClaimInput, revision string) {
		t.Helper()
		status(call(writer, "POST", base+"/claims", uuid.NewString(), revision, writer.csrf, encode(in)), 200)
	}
	writeClaim(claim, `"0"`)
	writePath := base + "/claims/" + claim.ClaimID + "/reviews"
	readPath := base + "/claims/" + claim.ClaimID + "/review"
	body := `{"claim_revision":1,"verdict":"approve","reason":"Reviewed exact current SOP"}`
	key := uuid.NewString()
	status(call(writer, "POST", writePath, key, `"1"`, "", body), 403)
	status(call(reader, "POST", writePath, key, `"1"`, reader.csrf, body), 403)
	wantProblem(t, call(writer, "POST", writePath, key, "", writer.csrf, body), 428, "REVISION_REQUIRED")
	for _, tag := range []string{`"0"`, `W/"1"`, `1`, `"01"`} {
		status(call(writer, "POST", writePath, key, tag, writer.csrf, body), 422)
	}
	for _, extra := range []string{`,"actor_subject":"forged"}`, `,"reviewer":"forged"}`, `,"state":"approved"}`, `,"authority":"verified"}`} {
		status(call(writer, "POST", writePath, key, `"1"`, writer.csrf, strings.TrimSuffix(body, "}")+extra), 422)
	}
	for _, bad := range []string{`{"claim_revision":0,"verdict":"approve","reason":"Reviewed"}`, `{"claim_revision":1,"verdict":"execute","reason":"Reviewed"}`, `{"claim_revision":1,"verdict":"approve","reason":" "}`} {
		status(call(writer, "POST", writePath, key, `"1"`, writer.csrf, bad), 422)
	}
	exec("UPDATE supervisor.browser_sessions SET auth_time=now()-interval '16 minutes' WHERE token_hash=$1", supervisorSession.hash)
	wantProblem(t, call(supervisorSession, "POST", writePath, key, `"1"`, supervisorSession.csrf, body), 428, "REAUTHENTICATION_REQUIRED")
	exec("UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash=$1", supervisorSession.hash)
	blocked := call(supervisorSession, "POST", writePath, key, `"1"`, supervisorSession.csrf, body)
	status(blocked, 409)
	if !strings.Contains(blocked.Body.String(), "REVIEW_BLOCKED") {
		t.Fatalf("missing authority not explicit: %s", blocked.Body.String())
	}
	// Rejection does not fabricate authority and remains possible for unsupported candidates.
	rejectedClaim := claim
	rejectedClaim.ClaimID = uuid.NewString()
	writeClaim(rejectedClaim, `"0"`)
	status(call(supervisorSession, "POST", base+"/claims/"+rejectedClaim.ClaimID+"/reviews", uuid.NewString(), `"1"`, supervisorSession.csrf, `{"claim_revision":1,"verdict":"reject","reason":"Not enough evidence"}`), 200)
	// WIS-270 Decision reason is 1..2000 characters (not bytes): empty and 2001 are rejected,
	// 2000 three-byte characters are stored whole.
	boundaryClaim := claim
	boundaryClaim.ClaimID = uuid.NewString()
	writeClaim(boundaryClaim, `"0"`)
	boundaryPath := base + "/claims/" + boundaryClaim.ClaimID + "/reviews"
	for _, reason := range []string{"", strings.Repeat("界", 2001)} {
		w := call(supervisorSession, "POST", boundaryPath, uuid.NewString(), `"1"`, supervisorSession.csrf, encode(map[string]any{"claim_revision": 1, "verdict": "reject", "reason": reason}))
		status(w, 422)
		if !strings.Contains(w.Body.String(), `"code":"INVALID_CLAIM"`) {
			t.Fatalf("reason of %d characters: %s", len([]rune(reason)), w.Body.String())
		}
	}
	longReason := strings.Repeat("界", 2000)
	status(call(supervisorSession, "POST", boundaryPath, uuid.NewString(), `"1"`, supervisorSession.csrf, encode(map[string]any{"claim_revision": 1, "verdict": "reject", "reason": longReason})), 200)
	var storedReason string
	if e := admin.QueryRowContext(ctx, `SELECT reason FROM supervisor.knowledge_claim_reviews WHERE tenant_id=$1 AND claim_id=$2`, tenant, boundaryClaim.ClaimID).Scan(&storedReason); e != nil || storedReason != longReason {
		t.Fatalf("2000-character reason not stored whole: %d characters, %v", len([]rune(storedReason)), e)
	}
	authorityBody := `{"source_revision":1,"expected_source_state_revision":1,"verdict":"designate","reason":"Owner verified current document"}`
	authorityResponse := call(writer, "POST", base+"/sources/"+source.SourceID+"/authority-decisions", uuid.NewString(), `"0"`, writer.csrf, authorityBody)
	status(authorityResponse, 200)
	var authority struct {
		DecisionID string `json:"decision_id"`
	}
	if e := json.Unmarshal(authorityResponse.Body.Bytes(), &authority); e != nil {
		t.Fatal(e)
	}
	type review struct {
		ReviewID                string `json:"review_id"`
		ClaimID                 string `json:"claim_id"`
		ClaimRevision           int64  `json:"claim_revision"`
		SchemaVersion           int    `json:"schema_version"`
		Verdict                 string `json:"verdict"`
		Reason                  string `json:"reason"`
		ActorIssuer             string `json:"actor_issuer"`
		ActorSubject            string `json:"actor_subject"`
		ApprovalEligible        bool   `json:"approval_eligible"`
		AuthorityDecisionID     string `json:"authority_decision_id"`
		AuthoritySourceID       string `json:"authority_source_id"`
		AuthoritySourceRevision int64  `json:"authority_source_revision"`
		AuthorityRevision       int64  `json:"authority_revision"`
	}
	decode := func(w *httptest.ResponseRecorder) review {
		t.Helper()
		status(w, 200)
		var r review
		if e := json.Unmarshal(w.Body.Bytes(), &r); e != nil {
			t.Fatal(e)
		}
		return r
	}
	approved := decode(call(supervisorSession, "POST", writePath, key, `"1"`, supervisorSession.csrf, body))
	if approved.ClaimID != claim.ClaimID || approved.ClaimRevision != 1 || approved.SchemaVersion != 1 || approved.Verdict != "approve" || !approved.ApprovalEligible || approved.ActorIssuer != issuer || approved.ActorSubject != supervisor {
		t.Fatalf("wrong exact review authority: %+v", approved)
	}
	if _, e := uuid.Parse(approved.ReviewID); e != nil {
		t.Fatal("missing server review identifier")
	}
	if approved.AuthorityDecisionID != authority.DecisionID || approved.AuthoritySourceID != source.SourceID || approved.AuthoritySourceRevision != 1 || approved.AuthorityRevision != 1 {
		t.Fatalf("exact authority decision pin lost: %+v", approved)
	}
	if replay := decode(call(supervisorSession, "POST", writePath, key, `"1"`, supervisorSession.csrf, body)); replay != approved {
		t.Fatalf("replay changed: %+v", replay)
	}
	if got := decode(call(reader, "GET", readPath, "", "", "", "")); got != approved {
		t.Fatalf("read differs: %+v", got)
	}
	wantProblem(t, call(supervisorSession, "POST", writePath, key, `"1"`, supervisorSession.csrf, strings.Replace(body, "Reviewed exact current SOP", "changed", 1)), 409, "IDEMPOTENCY_CONFLICT")
	decided := call(writer, "POST", writePath, uuid.NewString(), `"1"`, writer.csrf, body)
	status(decided, 409)
	if !strings.Contains(decided.Body.String(), "REVIEW_CONFLICT") {
		t.Fatalf("duplicate review category: %s", decided.Body.String())
	}
	for _, isolated := range []string{otherSite, foreignSite} {
		status(call(writer, "GET", "/v1/sites/"+isolated+"/claims/"+claim.ClaimID+"/review", "", "", "", ""), 404)
	}
	status(call(reader, "GET", readPath+"?revision=0", "", "", "", ""), 422)
	// A conflicting newer observation immediately invalidates old approval eligibility.
	conflicting := claim
	conflicting.ClaimID = uuid.NewString()
	conflicting.Literal = &knowledge.Literal{Type: "decimal", Value: json.RawMessage(`"120"`), Unit: "units/hour"}
	writeClaim(conflicting, `"0"`)
	wantProblem(t, call(supervisorSession, "POST", base+"/claims/"+conflicting.ClaimID+"/reviews", uuid.NewString(), `"1"`, supervisorSession.csrf, body), 409, "REVIEW_BLOCKED")
	if got := decode(call(reader, "GET", readPath, "", "", "", "")); got.ApprovalEligible {
		t.Fatal("conflicted review stayed eligible")
	}
	// Claim revision/history and source revision drift cannot carry approval forward.
	writeClaim(claim, `"1"`)
	old := decode(call(reader, "GET", readPath+"?revision=1", "", "", "", ""))
	if old.ApprovalEligible || old.ClaimRevision != 1 {
		t.Fatal("historical review became latest")
	}
	status(call(reader, "GET", readPath, "", "", "", ""), 404)
	source.Text = "Revised document requires renewed review"
	status(call(writer, "POST", base+"/sources", uuid.NewString(), `"1"`, writer.csrf, encode(source)), 200)
	if got := decode(call(supervisorSession, "POST", writePath, key, `"1"`, supervisorSession.csrf, body)); got.ApprovalEligible {
		t.Fatal("stale replay revived approval")
	}
	status(call(writer, "POST", base+"/sources/"+source.SourceID+"/revocations", uuid.NewString(), `"2"`, writer.csrf, `{"reason":"Document revoked"}`), 200)
	status(call(reader, "GET", readPath+"?revision=1", "", "", "", ""), 404)
	status(call(supervisorSession, "POST", writePath, key, `"1"`, supervisorSession.csrf, body), 404)
}
