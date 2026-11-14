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
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
)

func TestKnowledgeHTTPApprovedGraph(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("actual disposable PostgreSQL required")
	}
	control, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	name := "graph_http_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = control.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, e := control.ExecContext(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	}()
	connectURL := func(raw string) string {
		u, e := url.Parse(raw)
		if e != nil {
			t.Fatal(e)
		}
		u.Path = "/" + name
		return u.String()
	}
	admin, err := sql.Open("pgx", connectURL(adminURL))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err = migrate.Apply(ctx, admin); err != nil {
		t.Fatal(err)
	}
	db, err := store.New(connectURL(runtimeURL))
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
			t.Fatal(e)
		}
	}
	tenant, site, foreign := uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, origin := "https://graph-http.invalid/"+uuid.NewString(), "http://127.0.0.1:58176"
	exec("INSERT INTO supervisor.tenants(id,name) VALUES($1,'snapshot synthetic')", tenant)
	exec("INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'snapshot synthetic'),($1,$3,'inaccessible')", tenant, site, foreign)
	type session struct{ token, csrf, hash, subject string }
	seed := func(role string) session {
		t.Helper()
		a := session{token: uuid.NewString(), csrf: uuid.NewString(), subject: uuid.NewString()}
		sum := sha256.Sum256([]byte(a.token))
		a.hash = hex.EncodeToString(sum[:])
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, a.hash, a.subject, issuer, a.csrf)
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, tenant, site, issuer, a.subject, role)
		return a
	}
	owner, viewer := seed("owner"), seed("viewer")
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "snapshot", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, db.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)
	path := "/v1/sites/" + site + "/process-snapshots"
	call := func(a session, method, p, body, key, match, csrf string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, p, strings.NewReader(body))
		if a.token != "" {
			r.AddCookie(&http.Cookie{Name: "supervisor", Value: a.token})
		}
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-CSRF-Token", csrf)
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		if match != "" {
			r.Header.Set("If-Match", match)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		conformsToPublished(t, r, w)
		return w
	}
	status := func(w *httptest.ResponseRecorder, want int) {
		t.Helper()
		if w.Code != want {
			t.Fatalf("HTTP%d want%d: %s", w.Code, want, w.Body.String())
		}
	}
	status(call(session{}, "GET", path+"/"+uuid.NewString()+"/graph", "", "", "", ""), 401)
	sc := knowledge.Scope{TenantID: tenant, SiteID: site}
	actor := knowledge.Actor{Issuer: issuer, Subject: owner.subject, SessionHash: owner.hash}
	service, ev := knowledge.New(db.DB), evidence.New(db.DB)
	es := evidence.Scope{TenantID: tenant, SiteID: site}
	ea := evidence.Actor{Issuer: issuer, Subject: owner.subject, SessionHash: owner.hash}
	source, e := ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Approved route", Text: "Operation A then B then C"}, uuid.NewString(), 0)
	if e != nil {
		t.Fatal(e)
	}
	span, e := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: source.SourceID, Revision: 1, Start: 0, End: 9}, uuid.NewString(), 1)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = ev.DecideAuthority(ctx, ea, es, source.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Owner verified routing"}, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	ids := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	for _, id := range ids {
		if _, e = service.CreateEntity(ctx, actor, sc, knowledge.EntityInput{EntityID: id, Type: "operation", Name: "Routing operation", SourceID: source.SourceID, SourceRevision: 1}, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
	}
	for i := 0; i < 2; i++ {
		c := knowledge.ClaimInput{ClaimID: uuid.NewString(), SubjectID: ids[i], SubjectRevision: 1, Predicate: "precedes", ObjectEntityID: ids[i+1], ObjectRevision: 1, EffectiveFrom: "2026-09-27T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
		if _, e = service.CreateClaim(ctx, actor, sc, c, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		if _, e = service.DecideReview(ctx, actor, sc, c.ClaimID, knowledge.ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Verified route edge"}, uuid.NewString(), 1); e != nil {
			t.Fatal(e)
		}
	}
	inventory, e := service.ReadInventory(ctx, actor, sc)
	if e != nil {
		t.Fatal(e)
	}
	snapshot, e := service.CreateSnapshot(ctx, actor, sc, knowledge.SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Reviewed routing graph"}, uuid.NewString(), inventory.Revision)
	if e != nil {
		t.Fatal(e)
	}
	graphPath := path + "/" + snapshot.SnapshotID + "/graph"
	query := "?root=" + ids[0] + "&predicate=precedes&as_of=2026-09-27T12%3A00%3A00Z&hops=2"
	result := call(viewer, "GET", graphPath+query, "", "", "", "")
	status(result, 200)
	if len(result.Body.Bytes()) > 65536 || strings.HasSuffix(result.Body.String(), "\n") || !json.Valid(result.Body.Bytes()) {
		t.Fatal("graph response violated exact bounded JSON contract")
	}
	if result.Header().Get("Cache-Control") != "no-store" || result.Header().Get("Content-Type") != "application/json" {
		t.Fatal("graph headers differ")
	}
	var graph struct {
		Entities          []json.RawMessage `json:"entities"`
		Claims            []json.RawMessage `json:"claims"`
		Truncated         bool              `json:"truncated"`
		SnapshotID        string            `json:"snapshot_id"`
		SHA256            string            `json:"sha256"`
		InventoryRevision int64             `json:"inventory_revision"`
	}
	if e = json.Unmarshal(result.Body.Bytes(), &graph); e != nil {
		t.Fatal(e)
	}
	if len(graph.Entities) != 3 || len(graph.Claims) != 2 || graph.Truncated || graph.SnapshotID != snapshot.SnapshotID || graph.InventoryRevision != inventory.Revision || graph.SHA256 != snapshot.SHA256 {
		t.Fatalf("traceable graph mismatch: %s", result.Body.String())
	}
	for _, suffix := range []string{
		"", "?root=" + ids[0], query + "&unknown=1", query + "&hops=1", query + "&as_of=2026-09-27T12%3A00%3A00Z", query + "&max_entities=1&max_entities=2", query + "&max_claims=1&max_claims=2", query + "&predicate=not-a-predicate", query + "&root=invalid", query + "&bad=%zz", query + "&bad=one;two", strings.Replace(query, "hops=2", "hops=3", 1), strings.Replace(query, "hops=2", "hops=-1", 1), query + "&max_entities=0", query + "&max_claims=501", strings.Replace(query, "2026-09-27T12%3A00%3A00Z", "not-time", 1),
	} {
		status(call(viewer, "GET", graphPath+suffix, "", "", "", ""), 422)
	}
	for _, suffix := range []string{"&direction=invalid", "&direction=incoming&direction=outgoing", strings.Repeat("&root="+ids[0], 20), "&hops="} {
		status(call(viewer, "GET", graphPath+query+suffix, "", "", "", ""), 422)
	}
	incoming := call(viewer, "GET", graphPath+strings.Replace(query, ids[0], ids[2], 1)+"&direction=incoming", "", "", "", "")
	status(incoming, 200)
	var reverse struct {
		Direction string            `json:"direction"`
		Entities  []json.RawMessage `json:"entities"`
		Claims    []struct {
			SubjectID string `json:"subject_id"`
			ObjectID  string `json:"object_entity_id"`
		} `json:"claims"`
	}
	if e = json.Unmarshal(incoming.Body.Bytes(), &reverse); e != nil {
		t.Fatal(e)
	}
	if reverse.Direction != "incoming" || len(reverse.Entities) != 3 || len(reverse.Claims) != 2 {
		t.Fatalf("reverse navigation incomplete: %s", incoming.Body.String())
	}
	for _, c := range reverse.Claims {
		if !(c.SubjectID == ids[0] && c.ObjectID == ids[1]) && !(c.SubjectID == ids[1] && c.ObjectID == ids[2]) {
			t.Fatal("reverse traversal invented inverse claim")
		}
	}
	status(call(viewer, "GET", "/v1/sites/"+foreign+"/process-snapshots/"+snapshot.SnapshotID+"/graph"+query, "", "", "", ""), 404)
	status(call(viewer, "GET", strings.Replace(graphPath, snapshot.SnapshotID, uuid.NewString(), 1)+query, "", "", "", ""), 404)
	unknownRoot := call(viewer, "GET", graphPath+strings.Replace(query, ids[0], uuid.NewString(), 1), "", "", "", "")
	status(unknownRoot, 404)
	capped := call(viewer, "GET", graphPath+query+"&max_entities=2&max_claims=1", "", "", "", "")
	status(capped, 200)
	var limited struct {
		Entities  []json.RawMessage `json:"entities"`
		Claims    []json.RawMessage `json:"claims"`
		Truncated bool              `json:"truncated"`
	}
	if e = json.Unmarshal(capped.Body.Bytes(), &limited); e != nil {
		t.Fatal(e)
	}
	if !limited.Truncated || len(limited.Entities) > 2 || len(limited.Claims) > 1 {
		t.Fatal("cap silently omitted nodes without truncation")
	}
	if repeat := call(viewer, "GET", graphPath+query+"&max_entities=2&max_claims=1", "", "", "", ""); repeat.Body.String() != capped.Body.String() {
		t.Fatal("bounded traversal is unstable")
	}
	exec(`UPDATE supervisor.browser_sessions SET auth_time=NULL WHERE token_hash=$1`, viewer.hash)
	status(call(viewer, "GET", graphPath+query, "", "", "", ""), 200)
	exec(`UPDATE supervisor.memberships SET active=false WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4`, tenant, site, issuer, viewer.subject)
	status(call(viewer, "GET", graphPath+query, "", "", "", ""), 404)
	exec(`UPDATE supervisor.memberships SET active=true WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4`, tenant, site, issuer, viewer.subject)
	if _, e = ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "New observation", Text: "Inventory changed"}, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	stale := call(viewer, "GET", graphPath+query, "", "", "", "")
	status(stale, 409)
	if !strings.Contains(stale.Body.String(), "SNAPSHOT_STALE") {
		t.Fatal("stale snapshot lacks explicit error")
	}
	if _, e = ev.Revoke(ctx, ea, es, source.SourceID, "Remove captured source", uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	status(call(viewer, "GET", graphPath+query, "", "", "", ""), 404)
}
