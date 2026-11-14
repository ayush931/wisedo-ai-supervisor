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

func TestKnowledgeHTTPConflictDetectionAndVisibility(t *testing.T) {
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
	owner, viewer := uuid.NewString(), uuid.NewString()
	issuer, origin := "https://conflicts-http.invalid/"+uuid.NewString(), "http://127.0.0.1:58176"
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
		for _, table := range []string{"knowledge_conflicts", "knowledge_audit", "knowledge_receipts", "knowledge_claim_spans", "knowledge_claim_entity_refs", "knowledge_claim_revisions", "knowledge_claim_heads", "knowledge_entity_revisions", "knowledge_entity_heads", "knowledge_entities", "evidence_audit", "evidence_receipts", "evidence_revocations", "evidence_spans", "evidence_revisions", "evidence_heads", "memberships", "sites", "tenants"} {
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
	exec("UPDATE supervisor.memberships SET role='viewer' WHERE issuer=$1 AND subject=$2", issuer, viewer)
	makeSource := func(text string) (evidence.Input, evidence.Span) {
		t.Helper()
		source := evidence.Input{SourceID: uuid.NewString(), Kind: "interview", Title: "Synthetic capacity report", Text: text}
		status(call(writer, "POST", base+"/sources", uuid.NewString(), `"0"`, writer.csrf, encode(source)), 200)
		w := call(writer, "POST", base+"/sources/"+source.SourceID+"/spans", uuid.NewString(), `"1"`, writer.csrf, `{"revision":1,"start_char":0,"end_char":8}`)
		status(w, 200)
		var span evidence.Span
		if e := json.Unmarshal(w.Body.Bytes(), &span); e != nil {
			t.Fatal(e)
		}
		return source, span
	}
	originSource, originSpan := makeSource("Machine inventory report")
	otherSource, otherSpan := makeSource("Different capacity report")
	entity := knowledge.EntityInput{EntityID: uuid.NewString(), Type: "asset", AssetKind: "machine", Name: "Machine A", SourceID: originSource.SourceID, SourceRevision: 1}
	status(call(writer, "POST", base+"/entities", uuid.NewString(), `"0"`, writer.csrf, encode(entity)), 200)
	until := "2026-09-28T10:00:00Z"
	first := knowledge.ClaimInput{ClaimID: uuid.NewString(), SubjectID: entity.EntityID, SubjectRevision: 1, Predicate: "capacity_per_hour", Literal: &knowledge.Literal{Type: "decimal", Value: json.RawMessage(`"100"`), Unit: "units/hour"}, EffectiveFrom: "2026-09-27T10:00:00Z", EffectiveTo: &until, EvidenceClass: "report", SourceSpanIDs: []string{originSpan.ID}}
	write := func(claim knowledge.ClaimInput, revision string) {
		t.Helper()
		status(call(writer, "POST", base+"/claims", uuid.NewString(), revision, writer.csrf, encode(claim)), 200)
	}
	write(first, `"0"`)
	path := base + "/claims/" + first.ClaimID + "/conflicts"
	status(call(actorSession{}, "GET", path, "", "", "", ""), 401)
	read := func(path string) []json.RawMessage {
		t.Helper()
		w := call(reader, "GET", path, "", "", "", "")
		status(w, 200)
		var page struct {
			Conflicts []json.RawMessage `json:"conflicts"`
		}
		if e := json.Unmarshal(w.Body.Bytes(), &page); e != nil {
			t.Fatal(e)
		}
		return page.Conflicts
	}
	if got := read(path); len(got) != 0 {
		t.Fatal("single claim conflicts with itself")
	}
	for _, query := range []string{"?limit=0", "?limit=101", "?limit=abc", "?revision=0", "?cursor=bad"} {
		status(call(reader, "GET", path+query, "", "", "", ""), 422)
	}
	for _, isolated := range []string{otherSite, foreignSite} {
		status(call(writer, "GET", "/v1/sites/"+isolated+"/claims/"+first.ClaimID+"/conflicts", "", "", "", ""), 404)
	}
	second := first
	second.ClaimID = uuid.NewString()
	second.SourceSpanIDs = []string{otherSpan.ID}
	second.Literal = &knowledge.Literal{Type: "decimal", Value: json.RawMessage(`"120"`), Unit: "units/hour"}
	write(second, `"0"`)
	conflicts := read(path)
	if len(conflicts) != 1 {
		t.Fatalf("overlapping unequal values: conflicts=%s", encode(conflicts))
	}
	// WIS-270 HTTP enum roundtrip: both sides of the open conflict list as contested, and
	// the state filter accepts contested and rejects an unknown state.
	listByState := func(state string) map[string]string {
		t.Helper()
		w := call(reader, "GET", base+"/claims?subject_id="+entity.EntityID+"&state="+state, "", "", "", "")
		status(w, 200)
		var page struct {
			Claims []struct {
				ClaimID     string `json:"claim_id"`
				ReviewState string `json:"review_state"`
			} `json:"claims"`
		}
		if e := json.Unmarshal(w.Body.Bytes(), &page); e != nil {
			t.Fatal(e)
		}
		out := map[string]string{}
		for _, c := range page.Claims {
			out[c.ClaimID] = c.ReviewState
		}
		return out
	}
	if got := listByState(knowledge.StateContested); got[first.ClaimID] != knowledge.StateContested || got[second.ClaimID] != knowledge.StateContested {
		t.Fatalf("contested claims over HTTP: %v", got)
	}
	if got := listByState(knowledge.StateCandidate); len(got) != 0 {
		t.Fatalf("contested claims listed as candidates: %v", got)
	}
	status(call(reader, "GET", base+"/claims?state=disputed", "", "", "", ""), 422)
	var conflict map[string]any
	if e := json.Unmarshal(conflicts[0], &conflict); e != nil {
		t.Fatal(e)
	}
	if conflict["kind"] != "value_conflict" {
		t.Fatalf("wrong conflict classification: %s", conflicts[0])
	}
	var pair struct {
		ID            string `json:"id"`
		SchemaVersion int    `json:"schema_version"`
		Left          struct {
			ClaimID  string `json:"claim_id"`
			Revision int64  `json:"revision"`
		} `json:"left"`
		Right struct {
			ClaimID  string `json:"claim_id"`
			Revision int64  `json:"revision"`
		} `json:"right"`
		EffectiveFrom time.Time `json:"effective_from"`
		EffectiveTo   time.Time `json:"effective_to"`
		CreatedAt     time.Time `json:"created_at"`
	}
	if e := json.Unmarshal(conflicts[0], &pair); e != nil {
		t.Fatal(e)
	}
	wantFrom, err := time.Parse(time.RFC3339, first.EffectiveFrom)
	if err != nil {
		t.Fatal(err)
	}
	wantTo, err := time.Parse(time.RFC3339, until)
	if err != nil {
		t.Fatal(err)
	}
	if _, e := uuid.Parse(pair.ID); e != nil || pair.SchemaVersion != 1 || pair.Left.Revision != 1 || pair.Right.Revision != 1 || pair.CreatedAt.IsZero() || !pair.EffectiveFrom.Equal(wantFrom) || !pair.EffectiveTo.Equal(wantTo) {
		t.Fatalf("conflict lost exact revision/interval metadata: %s", conflicts[0])
	}
	if !((pair.Left.ClaimID == first.ClaimID && pair.Right.ClaimID == second.ClaimID) || (pair.Right.ClaimID == first.ClaimID && pair.Left.ClaimID == second.ClaimID)) {
		t.Fatal("conflict points to different claims")
	}
	// Equal canonical values and an interval beginning at the first interval's
	// exclusive end must not introduce additional conflicts for the first claim.
	equal := first
	equal.ClaimID = uuid.NewString()
	write(equal, `"0"`)
	disjoint := second
	disjoint.ClaimID = uuid.NewString()
	disjoint.EffectiveFrom = until
	disjoint.EffectiveTo = nil
	write(disjoint, `"0"`)
	if got := read(path); len(got) != 1 {
		t.Fatalf("equal or disjoint claim added false conflict: %s", encode(got))
	}
	// Different units require human unit review, never an implicit conversion.
	units := first
	units.ClaimID = uuid.NewString()
	units.Literal = &knowledge.Literal{Type: "decimal", Value: json.RawMessage(`"100"`), Unit: "pieces/hour"}
	write(units, `"0"`)
	conflicts = read(path)
	if len(conflicts) != 2 {
		t.Fatalf("unit mismatch missing: %s", encode(conflicts))
	}
	unitFound := false
	for _, raw := range conflicts {
		var c map[string]any
		if e := json.Unmarshal(raw, &c); e != nil {
			t.Fatal(e)
		}
		unitFound = unitFound || c["kind"] == "unit_review"
	}
	if !unitFound {
		t.Fatal("unit mismatch asserted numeric contradiction")
	}
	// One machine can perform multiple operations at the same time; these
	// different object IDs are not a single-valued assignment conflict.
	for i := 0; i < 2; i++ {
		operation := knowledge.EntityInput{EntityID: uuid.NewString(), Type: "operation", Name: "Synthetic operation", SourceID: originSource.SourceID, SourceRevision: 1}
		status(call(writer, "POST", base+"/entities", uuid.NewString(), `"0"`, writer.csrf, encode(operation)), 200)
		relation := first
		relation.ClaimID = uuid.NewString()
		relation.Predicate = "performs"
		relation.Literal = nil
		relation.ObjectEntityID = operation.EntityID
		relation.ObjectRevision = 1
		write(relation, `"0"`)
		if got := read(base + "/claims/" + relation.ClaimID + "/conflicts"); len(got) != 0 {
			t.Fatalf("multivalued relation conflicts: %s", encode(got))
		}
	}
	// Revising the other claim hides its old exact-revision conflict.
	second.Literal = first.Literal
	write(second, `"1"`)
	if got := read(path); len(got) != 1 {
		t.Fatalf("old claim revision stayed active: %s", encode(got))
	}
	second.Literal = &knowledge.Literal{Type: "decimal", Value: json.RawMessage(`"140"`), Unit: "units/hour"}
	write(second, `"2"`)
	if got := read(path); len(got) != 2 {
		t.Fatalf("new peer revision conflict missing: %s", encode(got))
	}
	// Revoking only the other claim's source must not expose its IDs in results.
	status(call(writer, "POST", base+"/sources/"+otherSource.SourceID+"/revocations", uuid.NewString(), `"1"`, writer.csrf, `{"reason":"Report withdrawn"}`), 200)
	w := call(reader, "GET", path, "", "", "", "")
	status(w, 200)
	if got := read(path); len(got) != 1 {
		t.Fatalf("revoked peer conflict still active: %s", encode(got))
	}
	if strings.Contains(w.Body.String(), second.ClaimID) || strings.Contains(w.Body.String(), otherSource.SourceID) {
		t.Fatal("revoked peer identity leaked")
	}
	// A new source revision makes every old entity/source pin stale, so no
	// recorded pair can remain active merely because its immutable row exists.
	originSource.Text = "Revised machine inventory report"
	status(call(writer, "POST", base+"/sources", uuid.NewString(), `"1"`, writer.csrf, encode(originSource)), 200)
	if got := read(path); len(got) != 0 {
		t.Fatalf("stale provenance conflict remained active: %s", encode(got))
	}
	status(call(writer, "POST", base+"/sources/"+originSource.SourceID+"/revocations", uuid.NewString(), `"2"`, writer.csrf, `{"reason":"Source withdrawn"}`), 200)
	status(call(reader, "GET", path, "", "", "", ""), 404)
}
