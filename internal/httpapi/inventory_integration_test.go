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

func TestKnowledgeHTTPInventoryScopeAndRevision(t *testing.T) {
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
	issuer, origin := "https://inventory-http.invalid/"+uuid.NewString(), "http://127.0.0.1:58176"
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
		if _, e = tx.Exec("SELECT set_config('supervisor.tenant_id',$1,true)", tenant); e != nil {
			t.Error(e)
			return
		}
		for _, table := range []string{"knowledge_inventory_revisions", "knowledge_review_resolution_refs", "knowledge_resolution_decisions", "knowledge_resolution_heads", "knowledge_review_revocations", "knowledge_claim_reviews", "knowledge_conflicts", "knowledge_audit", "knowledge_receipts", "knowledge_claim_spans", "knowledge_claim_entity_refs", "knowledge_claim_revisions", "knowledge_claim_heads", "knowledge_entity_revisions", "knowledge_entity_heads", "knowledge_entities", "evidence_authority_decisions", "evidence_authority_heads", "evidence_audit", "evidence_receipts", "evidence_revocations", "evidence_spans", "evidence_revisions", "evidence_heads", "memberships", "sites", "tenants"} {
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
			if _, e = tx.Exec("DELETE FROM supervisor."+table+" WHERE "+column+" IN($1,$2)", tenant, foreignTenant); e != nil {
				t.Error(e)
				return
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
	exec("INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner'),($1,$2,$3,$5,'viewer'),($1,$6,$3,$4,'owner')", tenant, site, issuer, owner, viewer, otherSite)
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
	path := "/v1/sites/" + site + "/knowledge-inventory"
	status(call(actorSession{}, "GET", path, "", "", "", ""), 401)
	type inventory struct {
		SchemaVersion int   `json:"schema_version"`
		Revision      int64 `json:"revision"`
	}
	read := func(actor actorSession, path string) inventory {
		t.Helper()
		w := call(actor, "GET", path, "", "", "", "")
		status(w, 200)
		var got inventory
		if e := json.Unmarshal(w.Body.Bytes(), &got); e != nil {
			t.Fatal(e)
		}
		if got.SchemaVersion != 1 || got.Revision < 0 {
			t.Fatalf("invalid inventory DTO: %s", w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("inventory response must not be cached")
		}
		return got
	}
	empty := read(reader, path)
	if empty.Revision != 0 {
		t.Fatal("empty site has graph revision")
	}
	if got := read(reader, path); got != empty {
		t.Fatal("read advanced inventory revision")
	}
	status(call(writer, "GET", "/v1/sites/"+foreignSite+"/knowledge-inventory", "", "", "", ""), 404)
	status(call(reader, "GET", "/v1/sites/"+otherSite+"/knowledge-inventory", "", "", "", ""), 404)
	status(call(reader, "GET", "/v1/sites/invalid/knowledge-inventory", "", "", "", ""), 404)
	exec("UPDATE supervisor.browser_sessions SET auth_time=NULL WHERE token_hash=$1", reader.hash)
	if got := read(reader, path); got != empty {
		t.Fatal("read requires no fresh identity proof")
	}
	input := evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Inventory observation", Text: "Source information"}
	body := encode(input)
	key := uuid.NewString()
	status(call(writer, "POST", "/v1/sites/"+site+"/sources", key, `"0"`, writer.csrf, body), 200)
	changed := read(reader, path)
	if changed.Revision <= empty.Revision {
		t.Fatal("source mutation did not advance inventory")
	}
	if got := read(writer, path); got != changed {
		t.Fatal("read changed revision")
	}
	status(call(writer, "POST", "/v1/sites/"+site+"/sources", key, `"0"`, writer.csrf, body), 200)
	if got := read(reader, path); got != changed {
		t.Fatal("idempotent replay advanced inventory")
	}
	if got := read(writer, "/v1/sites/"+otherSite+"/knowledge-inventory"); got.Revision != 0 {
		t.Fatal("other site revision contaminated")
	}
	exec("UPDATE supervisor.memberships SET active=false WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4", tenant, site, issuer, viewer)
	status(call(reader, "GET", path, "", "", "", ""), 404)
	exec("UPDATE supervisor.browser_sessions SET expires_at=now()-interval '1 second' WHERE token_hash=$1", writer.hash)
	status(call(writer, "GET", path, "", "", "", ""), 401)
}
