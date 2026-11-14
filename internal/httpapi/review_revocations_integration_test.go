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

func TestKnowledgeHTTPReviewRevocation(t *testing.T) {
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
	issuer, origin := "https://review-revocations-http.invalid/"+uuid.NewString(), "http://127.0.0.1:58176"
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
		for _, table := range []string{"knowledge_review_revocations", "knowledge_claim_reviews", "knowledge_conflicts", "knowledge_audit", "knowledge_receipts", "knowledge_claim_spans", "knowledge_claim_entity_refs", "knowledge_claim_revisions", "knowledge_claim_heads", "knowledge_entity_revisions", "knowledge_entity_heads", "knowledge_entities", "evidence_authority_decisions", "evidence_authority_heads", "evidence_audit", "evidence_receipts", "evidence_revocations", "evidence_spans", "evidence_revisions", "evidence_heads", "memberships", "sites", "tenants"} {
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
	status(call(actorSession{}, "POST", base+"/claims/"+uuid.NewString()+"/review-revocations", uuid.NewString(), `"1"`, "", `{}`), 401)
	source := evidence.Input{SourceID: uuid.NewString(), Kind: "document", Title: "Synthetic approved procedure", Text: "Machine capacity 100 units/hour"}
	status(call(writer, "POST", base+"/sources", uuid.NewString(), `"0"`, writer.csrf, encode(source)), 200)
	spanResponse := call(writer, "POST", base+"/sources/"+source.SourceID+"/spans", uuid.NewString(), `"1"`, writer.csrf, `{"revision":1,"start_char":0,"end_char":8}`)
	status(spanResponse, 200)
	var span evidence.Span
	if e := json.Unmarshal(spanResponse.Body.Bytes(), &span); e != nil {
		t.Fatal(e)
	}
	entity := knowledge.EntityInput{EntityID: uuid.NewString(), Type: "asset", AssetKind: "machine", Name: "Machine", SourceID: source.SourceID, SourceRevision: 1}
	status(call(writer, "POST", base+"/entities", uuid.NewString(), `"0"`, writer.csrf, encode(entity)), 200)
	claim := knowledge.ClaimInput{ClaimID: uuid.NewString(), SubjectID: entity.EntityID, SubjectRevision: 1, Predicate: "capacity_per_hour", Literal: &knowledge.Literal{Type: "decimal", Value: json.RawMessage(`"100"`), Unit: "units/hour"}, EffectiveFrom: "2026-09-27T10:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
	status(call(writer, "POST", base+"/claims", uuid.NewString(), `"0"`, writer.csrf, encode(claim)), 200)
	status(call(writer, "POST", base+"/sources/"+source.SourceID+"/authority-decisions", uuid.NewString(), `"0"`, writer.csrf, `{"source_revision":1,"expected_source_state_revision":1,"verdict":"designate","reason":"Owner verified SOP"}`), 200)
	claimPath := base + "/claims/" + claim.ClaimID
	approvalKey := uuid.NewString()
	approvalBody := `{"claim_revision":1,"verdict":"approve","reason":"Validated SOP"}`
	var approved map[string]json.RawMessage
	approval := call(supervisorSession, "POST", claimPath+"/reviews", approvalKey, `"1"`, supervisorSession.csrf, approvalBody)
	status(approval, 200)
	if e := json.Unmarshal(approval.Body.Bytes(), &approved); e != nil {
		t.Fatal(e)
	}
	var reviewID string
	if e := json.Unmarshal(approved["review_id"], &reviewID); e != nil {
		t.Fatal(e)
	}
	rejectedClaim := claim
	rejectedClaim.ClaimID = uuid.NewString()
	status(call(writer, "POST", base+"/claims", uuid.NewString(), `"0"`, writer.csrf, encode(rejectedClaim)), 200)
	rejectedPath := base + "/claims/" + rejectedClaim.ClaimID
	rejectedResponse := call(supervisorSession, "POST", rejectedPath+"/reviews", uuid.NewString(), `"1"`, supervisorSession.csrf, `{"claim_revision":1,"verdict":"reject","reason":"Not accepted"}`)
	status(rejectedResponse, 200)
	var rejected struct {
		ReviewID string `json:"review_id"`
	}
	if e := json.Unmarshal(rejectedResponse.Body.Bytes(), &rejected); e != nil {
		t.Fatal(e)
	}
	status(call(supervisorSession, "POST", rejectedPath+"/review-revocations", uuid.NewString(), `"1"`, supervisorSession.csrf, encode(map[string]any{"claim_revision": 1, "review_id": rejected.ReviewID, "reason": "withdraw"})), 422)
	// The accepted withdrawal carries the maximum 2000-character (6000-byte) reason.
	withdrawReason := strings.Repeat("界", 2000)
	body := encode(map[string]any{"claim_revision": 1, "review_id": reviewID, "reason": withdrawReason})
	key := uuid.NewString()
	path := claimPath + "/review-revocations"
	status(call(supervisorSession, "POST", path, key, `"1"`, "", body), 403)
	status(call(reader, "POST", path, key, `"1"`, reader.csrf, body), 403)
	wantProblem(t, call(supervisorSession, "POST", path, key, "", supervisorSession.csrf, body), 428, "REVISION_REQUIRED")
	for _, tag := range []string{`"0"`, `W/"1"`, `1`} {
		status(call(supervisorSession, "POST", path, key, tag, supervisorSession.csrf, body), 422)
	}
	for _, extra := range []string{`,"actor_subject":"forged"}`, `,"revocation_id":"forged"}`, `,"approval_eligible":true}`} {
		status(call(supervisorSession, "POST", path, key, `"1"`, supervisorSession.csrf, strings.TrimSuffix(body, "}")+extra), 422)
	}
	for _, bad := range []string{encode(map[string]any{"claim_revision": 1, "review_id": reviewID, "reason": " "}), encode(map[string]any{"claim_revision": 0, "review_id": reviewID, "reason": "withdraw"}), encode(map[string]any{"claim_revision": 1, "review_id": "invalid", "reason": "withdraw"})} {
		status(call(supervisorSession, "POST", path, key, `"1"`, supervisorSession.csrf, bad), 422)
	}
	// WIS-270 Decision reason is 1..2000 characters; empty and 2001 fail as INVALID_CLAIM.
	for _, reason := range []string{"", strings.Repeat("界", 2001)} {
		w := call(supervisorSession, "POST", path, uuid.NewString(), `"1"`, supervisorSession.csrf, encode(map[string]any{"claim_revision": 1, "review_id": reviewID, "reason": reason}))
		status(w, 422)
		if !strings.Contains(w.Body.String(), `"code":"INVALID_CLAIM"`) {
			t.Fatalf("revocation reason of %d characters: %s", len([]rune(reason)), w.Body.String())
		}
	}
	wantProblem(t, call(supervisorSession, "POST", path, key, `"1"`, supervisorSession.csrf, strings.Replace(body, reviewID, uuid.NewString(), 1)), 409, "REVIEW_CONFLICT")
	exec("UPDATE supervisor.browser_sessions SET auth_time=now()-interval '16 minutes' WHERE token_hash=$1", supervisorSession.hash)
	wantProblem(t, call(supervisorSession, "POST", path, key, `"1"`, supervisorSession.csrf, body), 428, "REAUTHENTICATION_REQUIRED")
	exec("UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash=$1", supervisorSession.hash)
	// Historic approvals remain withdrawable after superseding claim and evidence.
	status(call(writer, "POST", base+"/claims", uuid.NewString(), `"1"`, writer.csrf, encode(claim)), 200)
	entity.Name = "Renamed machine"
	status(call(writer, "POST", base+"/entities", uuid.NewString(), `"1"`, writer.csrf, encode(entity)), 200)
	source.Text = "Updated procedure"
	status(call(writer, "POST", base+"/sources", uuid.NewString(), `"1"`, writer.csrf, encode(source)), 200)
	withdrawn := call(supervisorSession, "POST", path, key, `"1"`, supervisorSession.csrf, body)
	status(withdrawn, 200)
	var revoked map[string]json.RawMessage
	if e := json.Unmarshal(withdrawn.Body.Bytes(), &revoked); e != nil {
		t.Fatal(e)
	}
	for _, field := range []string{"review_id", "claim_id", "claim_revision", "verdict", "reason", "actor_issuer", "actor_subject", "authenticated_at", "created_at"} {
		if string(revoked[field]) != string(approved[field]) {
			t.Fatalf("withdrawal rewrote original %s", field)
		}
	}
	if string(revoked["approval_eligible"]) != "false" || len(revoked["revocation"]) == 0 || string(revoked["revocation"]) == "null" {
		t.Fatalf("missing revocation metadata: %s", withdrawn.Body.String())
	}
	var metadata struct {
		SchemaVersion   int       `json:"schema_version"`
		RevocationID    string    `json:"revocation_id"`
		ReviewID        string    `json:"review_id"`
		ClaimID         string    `json:"claim_id"`
		ClaimRevision   int64     `json:"claim_revision"`
		Reason          string    `json:"reason"`
		ActorIssuer     string    `json:"actor_issuer"`
		ActorSubject    string    `json:"actor_subject"`
		AuthenticatedAt time.Time `json:"authenticated_at"`
		CreatedAt       time.Time `json:"created_at"`
	}
	if e := json.Unmarshal(revoked["revocation"], &metadata); e != nil {
		t.Fatal(e)
	}
	if _, e := uuid.Parse(metadata.RevocationID); e != nil || metadata.SchemaVersion != 1 || metadata.ReviewID != reviewID || metadata.ClaimID != claim.ClaimID || metadata.ClaimRevision != 1 || metadata.Reason != withdrawReason || metadata.ActorIssuer != issuer || metadata.ActorSubject != supervisor || metadata.AuthenticatedAt.IsZero() || metadata.CreatedAt.IsZero() {
		t.Fatalf("wrong server revocation provenance: %+v", metadata)
	}
	for _, response := range []*httptest.ResponseRecorder{
		call(supervisorSession, "POST", path, key, `"1"`, supervisorSession.csrf, body),
		call(reader, "GET", claimPath+"/review?revision=1", "", "", "", ""),
		call(supervisorSession, "POST", claimPath+"/reviews", approvalKey, `"1"`, supervisorSession.csrf, approvalBody),
	} {
		status(response, 200)
		var got map[string]json.RawMessage
		if e := json.Unmarshal(response.Body.Bytes(), &got); e != nil {
			t.Fatal(e)
		}
		if string(got["approval_eligible"]) != "false" || string(got["revocation"]) != string(revoked["revocation"]) {
			t.Fatalf("read/replay resurrected approval: %s", response.Body.String())
		}
	}
	wantProblem(t, call(supervisorSession, "POST", path, uuid.NewString(), `"1"`, supervisorSession.csrf, body), 409, "REVIEW_CONFLICT")
	changedReview := call(supervisorSession, "POST", path, key, `"1"`, supervisorSession.csrf, strings.Replace(body, reviewID, uuid.NewString(), 1))
	status(changedReview, 409)
	if !strings.Contains(changedReview.Body.String(), "IDEMPOTENCY_CONFLICT") {
		t.Fatalf("changed replay review ID category: %s", changedReview.Body.String())
	}
	wantProblem(t, call(supervisorSession, "POST", path, key, `"1"`, supervisorSession.csrf, strings.Replace(body, withdrawReason, "different", 1)), 409, "IDEMPOTENCY_CONFLICT")
	exec("UPDATE supervisor.browser_sessions SET auth_time=now()-interval '16 minutes' WHERE token_hash=$1", supervisorSession.hash)
	wantProblem(t, call(supervisorSession, "POST", path, key, `"1"`, supervisorSession.csrf, body), 428, "REAUTHENTICATION_REQUIRED")
	exec("UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash=$1", supervisorSession.hash)
	for _, isolated := range []string{otherSite, foreignSite} {
		status(call(writer, "POST", "/v1/sites/"+isolated+"/claims/"+claim.ClaimID+"/review-revocations", uuid.NewString(), `"1"`, writer.csrf, body), 404)
	}
	status(call(writer, "POST", base+"/sources/"+source.SourceID+"/revocations", uuid.NewString(), `"2"`, writer.csrf, `{"reason":"SOP access revoked"}`), 200)
	status(call(supervisorSession, "POST", path, key, `"1"`, supervisorSession.csrf, body), 404)
	status(call(reader, "GET", claimPath+"/review?revision=1", "", "", "", ""), 404)
}
