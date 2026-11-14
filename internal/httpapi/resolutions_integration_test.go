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

func TestKnowledgeHTTPConflictResolutions(t *testing.T) {
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
	issuer, origin := "https://resolutions-http.invalid/"+uuid.NewString(), "http://127.0.0.1:58176"
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
		for _, table := range []string{"knowledge_review_resolution_refs", "knowledge_resolution_decisions", "knowledge_resolution_heads", "knowledge_review_revocations", "knowledge_claim_reviews", "knowledge_conflicts", "knowledge_audit", "knowledge_receipts", "knowledge_claim_spans", "knowledge_claim_entity_refs", "knowledge_claim_revisions", "knowledge_claim_heads", "knowledge_entity_revisions", "knowledge_entity_heads", "knowledge_entities", "evidence_authority_decisions", "evidence_authority_heads", "evidence_audit", "evidence_receipts", "evidence_revocations", "evidence_spans", "evidence_revisions", "evidence_heads", "memberships", "sites", "tenants"} {
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
	status(call(actorSession{}, "POST", base+"/conflicts/"+uuid.NewString()+"/resolutions", uuid.NewString(), `"0"`, "", `{"decision":"neither","reason":"Review"}`), 401)
	source := evidence.Input{SourceID: uuid.NewString(), Kind: "document", Title: "Synthetic conflicting SOP", Text: "Capacity reports 100 or 120"}
	status(call(writer, "POST", base+"/sources", uuid.NewString(), `"0"`, writer.csrf, encode(source)), 200)
	spanResponse := call(writer, "POST", base+"/sources/"+source.SourceID+"/spans", uuid.NewString(), `"1"`, writer.csrf, `{"revision":1,"start_char":0,"end_char":8}`)
	status(spanResponse, 200)
	var span evidence.Span
	if e := json.Unmarshal(spanResponse.Body.Bytes(), &span); e != nil {
		t.Fatal(e)
	}
	entity := knowledge.EntityInput{EntityID: uuid.NewString(), Type: "asset", AssetKind: "machine", Name: "Machine", SourceID: source.SourceID, SourceRevision: 1}
	status(call(writer, "POST", base+"/entities", uuid.NewString(), `"0"`, writer.csrf, encode(entity)), 200)
	first := knowledge.ClaimInput{ClaimID: uuid.NewString(), SubjectID: entity.EntityID, SubjectRevision: 1, Predicate: "capacity_per_hour", Literal: &knowledge.Literal{Type: "decimal", Value: json.RawMessage(`"100"`), Unit: "units/hour"}, EffectiveFrom: "2026-09-27T10:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
	second := first
	second.ClaimID = uuid.NewString()
	second.Literal = &knowledge.Literal{Type: "decimal", Value: json.RawMessage(`"120"`), Unit: "units/hour"}
	for _, claim := range []knowledge.ClaimInput{first, second} {
		status(call(writer, "POST", base+"/claims", uuid.NewString(), `"0"`, writer.csrf, encode(claim)), 200)
	}
	conflictsResponse := call(reader, "GET", base+"/claims/"+first.ClaimID+"/conflicts", "", "", "", "")
	status(conflictsResponse, 200)
	type claimRef struct {
		ClaimID  string `json:"claim_id"`
		Revision int64  `json:"revision"`
	}
	var conflicts struct {
		Conflicts []struct {
			ID    string   `json:"id"`
			Left  claimRef `json:"left"`
			Right claimRef `json:"right"`
		} `json:"conflicts"`
	}
	if e := json.Unmarshal(conflictsResponse.Body.Bytes(), &conflicts); e != nil {
		t.Fatal(e)
	}
	if len(conflicts.Conflicts) != 1 {
		t.Fatal("expected one exact claim conflict")
	}
	conflict := conflicts.Conflicts[0]
	path := base + "/conflicts/" + conflict.ID + "/resolutions"
	body := `{"decision":"prefer_left","reason":"Left account verified"}`
	key := uuid.NewString()
	status(call(reader, "POST", path, key, `"0"`, reader.csrf, body), 403)
	status(call(supervisorSession, "POST", path, key, `"0"`, "", body), 403)
	wantProblem(t, call(supervisorSession, "POST", path, key, "", supervisorSession.csrf, body), 428, "REVISION_REQUIRED")
	status(call(supervisorSession, "POST", path, key, `"0"`, supervisorSession.csrf, `{"decision":"withdraw","reason":"No decision exists"}`), 422)
	for _, extra := range []string{`,"actor_subject":"forged"}`, `,"authority_decision_id":"forged"}`, `,"resolution_eligible":true}`} {
		status(call(supervisorSession, "POST", path, key, `"0"`, supervisorSession.csrf, strings.TrimSuffix(body, "}")+extra), 422)
	}
	// WIS-270 Decision reason is 1..2000 characters; empty and 2001 fail as INVALID_CLAIM.
	for _, reason := range []string{"", strings.Repeat("界", 2001)} {
		w := call(supervisorSession, "POST", path, uuid.NewString(), `"0"`, supervisorSession.csrf, encode(map[string]any{"decision": "neither", "reason": reason}))
		status(w, 422)
		if !strings.Contains(w.Body.String(), `"code":"INVALID_CLAIM"`) {
			t.Fatalf("resolution reason of %d characters: %s", len([]rune(reason)), w.Body.String())
		}
	}
	exec("UPDATE supervisor.browser_sessions SET auth_time=now()-interval '16 minutes' WHERE token_hash=$1", supervisorSession.hash)
	wantProblem(t, call(supervisorSession, "POST", path, key, `"0"`, supervisorSession.csrf, body), 428, "REAUTHENTICATION_REQUIRED")
	exec("UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash=$1", supervisorSession.hash)
	wantProblem(t, call(supervisorSession, "POST", path, key, `"0"`, supervisorSession.csrf, body), 409, "RESOLUTION_BLOCKED")
	status(call(writer, "POST", base+"/sources/"+source.SourceID+"/authority-decisions", uuid.NewString(), `"0"`, writer.csrf, `{"source_revision":1,"expected_source_state_revision":1,"verdict":"designate","reason":"Owner validated document"}`), 200)
	type resolution struct {
		ResolutionID       string   `json:"resolution_id"`
		ConflictID         string   `json:"conflict_id"`
		Revision           int64    `json:"revision"`
		CurrentRevision    int64    `json:"current_revision"`
		Decision           string   `json:"decision"`
		Left               claimRef `json:"left"`
		Right              claimRef `json:"right"`
		ResolutionEligible bool     `json:"resolution_eligible"`
	}
	decode := func(w *httptest.ResponseRecorder) resolution {
		t.Helper()
		status(w, 200)
		var r resolution
		if e := json.Unmarshal(w.Body.Bytes(), &r); e != nil {
			t.Fatal(e)
		}
		return r
	}
	chosen := decode(call(supervisorSession, "POST", path, key, `"0"`, supervisorSession.csrf, body))
	if _, e := uuid.Parse(chosen.ResolutionID); e != nil || chosen.ConflictID != conflict.ID || chosen.Revision != 1 || chosen.CurrentRevision != 1 || chosen.Decision != "prefer_left" || chosen.Left != conflict.Left || chosen.Right != conflict.Right || !chosen.ResolutionEligible {
		t.Fatalf("wrong resolution refs: %+v", chosen)
	}
	if got := decode(call(supervisorSession, "POST", path, key, `"0"`, supervisorSession.csrf, body)); got != chosen {
		t.Fatalf("replay changed %+v", got)
	}
	if got := decode(call(reader, "GET", path, "", "", "", "")); got != chosen {
		t.Fatal("reader resolution differs")
	}
	wantProblem(t, call(supervisorSession, "POST", path, uuid.NewString(), `"0"`, supervisorSession.csrf, body), 412, "REVISION_CONFLICT")
	wantProblem(t, call(supervisorSession, "POST", path, key, `"0"`, supervisorSession.csrf, strings.Replace(body, "Left account verified", "different", 1)), 409, "IDEMPOTENCY_CONFLICT")
	for _, isolated := range []string{otherSite, foreignSite} {
		status(call(writer, "GET", "/v1/sites/"+isolated+"/conflicts/"+conflict.ID+"/resolutions", "", "", "", ""), 404)
	}
	status(call(reader, "GET", path+"?revision=0", "", "", "", ""), 422)
	leftPath := base + "/claims/" + conflict.Left.ClaimID
	rightPath := base + "/claims/" + conflict.Right.ClaimID
	status(call(reader, "GET", leftPath+"/review", "", "", "", ""), 404)
	reviewBody := `{"claim_revision":1,"verdict":"approve","reason":"Winner independently reviewed"}`
	wantProblem(t, call(supervisorSession, "POST", rightPath+"/reviews", uuid.NewString(), `"1"`, supervisorSession.csrf, reviewBody), 409, "REVIEW_BLOCKED")
	approved := call(supervisorSession, "POST", leftPath+"/reviews", uuid.NewString(), `"1"`, supervisorSession.csrf, reviewBody)
	status(approved, 200)
	var review struct {
		ReviewID         string `json:"review_id"`
		ApprovalEligible bool   `json:"approval_eligible"`
		ResolutionRefs   []struct {
			ConflictID   string `json:"conflict_id"`
			ResolutionID string `json:"resolution_id"`
			Revision     int64  `json:"revision"`
		} `json:"resolution_refs"`
	}
	if e := json.Unmarshal(approved.Body.Bytes(), &review); e != nil {
		t.Fatal(e)
	}
	if !review.ApprovalEligible {
		t.Fatal("winner review not eligible")
	}
	if len(review.ResolutionRefs) != 1 || review.ResolutionRefs[0].ConflictID != conflict.ID || review.ResolutionRefs[0].ResolutionID != chosen.ResolutionID || review.ResolutionRefs[0].Revision != 1 {
		t.Fatalf("review lost exact resolution pin: %+v", review)
	}
	// A resolution cannot discard an existing approval without explicit withdrawal.
	wantProblem(t, call(supervisorSession, "POST", path, uuid.NewString(), `"1"`, supervisorSession.csrf, `{"decision":"prefer_right","reason":"Change preference"}`), 409, "RESOLUTION_BLOCKED")
	wantProblem(t, call(supervisorSession, "POST", path, uuid.NewString(), `"1"`, supervisorSession.csrf, `{"decision":"neither","reason":"Discard both"}`), 409, "RESOLUTION_BLOCKED")
	withdrawReason := strings.Repeat("界", 2000)
	withdrawn := decode(call(supervisorSession, "POST", path, uuid.NewString(), `"1"`, supervisorSession.csrf, encode(map[string]any{"decision": "withdraw", "reason": withdrawReason})))
	if withdrawn.Revision != 2 || withdrawn.ResolutionEligible {
		t.Fatalf("withdrawal remained eligible %+v", withdrawn)
	}
	var storedReason string
	if e := admin.QueryRowContext(ctx, `SELECT reason FROM supervisor.knowledge_resolution_decisions WHERE tenant_id=$1 AND resolution_id=$2`, tenant, withdrawn.ResolutionID).Scan(&storedReason); e != nil || storedReason != withdrawReason {
		t.Fatalf("2000-character resolution reason not stored whole: %d characters, %v", len([]rune(storedReason)), e)
	}
	checkIneligible := func() {
		t.Helper()
		w := call(reader, "GET", leftPath+"/review", "", "", "", "")
		status(w, 200)
		var r struct {
			ApprovalEligible bool `json:"approval_eligible"`
		}
		if e := json.Unmarshal(w.Body.Bytes(), &r); e != nil {
			t.Fatal(e)
		}
		if r.ApprovalEligible {
			t.Fatal("old review revived after resolution changed")
		}
	}
	checkIneligible()
	redesignated := decode(call(supervisorSession, "POST", path, uuid.NewString(), `"2"`, supervisorSession.csrf, body))
	if redesignated.Revision != 3 || !redesignated.ResolutionEligible {
		t.Fatal("redesignation failed")
	}
	checkIneligible()
	source.Text = "Superseded account"
	status(call(writer, "POST", base+"/sources", uuid.NewString(), `"1"`, writer.csrf, encode(source)), 200)
	if got := decode(call(reader, "GET", path, "", "", "", "")); got.ResolutionEligible {
		t.Fatal("stale evidence resolution eligible")
	}
	status(call(supervisorSession, "POST", path, uuid.NewString(), `"3"`, supervisorSession.csrf, `{"decision":"withdraw","reason":"Withdraw stale resolution"}`), 200)
	status(call(writer, "POST", base+"/sources/"+source.SourceID+"/revocations", uuid.NewString(), `"2"`, writer.csrf, `{"reason":"Evidence revoked"}`), 200)
	status(call(reader, "GET", path, "", "", "", ""), 404)
	status(call(supervisorSession, "POST", path, key, `"0"`, supervisorSession.csrf, body), 404)
}
