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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/operational"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// G3: GET /operational-sources pages every source exactly once in source_id order, stays
// inside its tenant and site, and mapping problems name the failing input row.
func TestOperationalSourceListAndMappingRowPathsPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("real PostgreSQL admin and runtime URLs required")
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	database := "operational_list_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.ExecContext(ctx, "CREATE DATABASE "+database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if _, e := admin.ExecContext(cleanup, "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	})
	connect := func(raw string) *sql.DB {
		t.Helper()
		u, e := url.Parse(raw)
		if e != nil {
			t.Fatal(e)
		}
		u.Path = "/" + database
		db, e := sql.Open("pgx", u.String())
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	control, runtime := connect(adminURL), connect(runtimeURL)
	if err = migrate.Apply(ctx, control); err != nil {
		t.Fatal(err)
	}
	db, err := store.New(runtimeURLFor(runtimeURL, database))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := control.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	issuer, origin := "https://operational-list.invalid/"+uuid.NewString(), "http://127.0.0.1:58178"
	tenant, site, otherSite, foreignTenant, foreignSite := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	owner, viewer, foreignOwner := uuid.NewString(), uuid.NewString(), uuid.NewString()
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic list tenant'),($2,'synthetic foreign tenant')`, tenant, foreignTenant)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'site a'),($1,$3,'site b'),($4,$5,'foreign site')`, tenant, site, otherSite, foreignTenant, foreignSite)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$4,$5,'owner'),($1,$2,$4,$6,'viewer'),($1,$3,$4,$6,'owner')`, tenant, site, otherSite, issuer, owner, viewer)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, foreignTenant, foreignSite, issuer, foreignOwner)
	type session struct{ token, csrf, hash, subject string }
	seed := func(subject string) session {
		t.Helper()
		v := session{token: uuid.NewString(), csrf: uuid.NewString(), subject: subject}
		sum := sha256.Sum256([]byte(v.token))
		v.hash = hex.EncodeToString(sum[:])
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, v.hash, subject, issuer, v.csrf)
		return v
	}
	ownerSession, viewerSession, foreignSession := seed(owner), seed(viewer), seed(foreignOwner)
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "operational-list-test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)
	call := func(actor session, method, path, key, revision, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: "supervisor", Value: actor.token})
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", actor.csrf)
		r.Header.Set("Content-Type", "application/json")
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		if revision != "" {
			r.Header.Set("If-Match", revision)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	problemAt := func(w *httptest.ResponseRecorder, status int, code, path string) {
		t.Helper()
		var p struct {
			Code string  `json:"code"`
			Path *string `json:"path"`
		}
		if w.Code != status || json.Unmarshal(w.Body.Bytes(), &p) != nil || p.Code != code || (path == "" && p.Path != nil) || (path != "" && (p.Path == nil || *p.Path != path)) {
			t.Fatalf("want %d %s path %q, got %d %s", status, code, path, w.Code, w.Body.String())
		}
	}
	sourcesOf := func(siteID string) string { return "/v1/sites/" + siteID + "/operational-sources" }
	create := func(actor session, siteID string) string {
		t.Helper()
		id := uuid.NewString()
		body := `{"source_id":"` + id + `","type":"csv","authority_scope":"production_delta","enabled":true,"access_policy_revision":1,"freshness_seconds":120,"clock_skew_seconds":30}`
		if w := call(actor, "POST", sourcesOf(siteID), uuid.NewString(), `"0"`, body); w.Code != 200 {
			t.Fatalf("create source: %d %s", w.Code, w.Body.String())
		}
		return id
	}
	page := func(actor session, siteID, query string) operational.SourcePage {
		t.Helper()
		w := call(actor, "GET", sourcesOf(siteID)+query, "", "", "")
		var p operational.SourcePage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &p) != nil {
			t.Fatalf("list %s: %d %s", query, w.Code, w.Body.String())
		}
		return p
	}
	all := func(actor session, siteID string, limit string) ([]operational.SourceSummary, []int) {
		t.Helper()
		var out []operational.SourceSummary
		var sizes []int
		cursor := ""
		for i := 0; i < 100; i++ {
			q := "?limit=" + limit
			if limit == "" {
				q = ""
			}
			if cursor != "" {
				q += map[bool]string{true: "?", false: "&"}[q == ""] + "cursor=" + cursor
			}
			p := page(actor, siteID, q)
			out, sizes = append(out, p.Sources...), append(sizes, len(p.Sources))
			if p.NextCursor == "" {
				return out, sizes
			}
			cursor = p.NextCursor
		}
		t.Fatal("cursor never ended")
		return nil, nil
	}

	// Empty site: an empty page, not an error.
	if p := page(ownerSession, site, ""); len(p.Sources) != 0 || p.NextCursor != "" {
		t.Fatalf("empty site page=%+v", p)
	}
	want := []string{}
	for i := 0; i < 51; i++ {
		want = append(want, create(ownerSession, site))
	}
	slices.Sort(want)
	other := create(viewerSession, otherSite) // the site-a viewer owns site b
	foreign := create(foreignSession, foreignSite)

	// Review finding 1: listing writes no audit row per listed source (or per page); a single
	// source read still records source.read + mapping.read, proving the count observes audits.
	audits := func() int {
		t.Helper()
		var n int
		tx, e := control.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); e != nil {
			t.Fatal(e)
		}
		if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.operational_source_audit WHERE tenant_id=$1 AND site_id=$2`, tenant, site).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	before := audits()
	if p := page(ownerSession, site, "?limit=100"); len(p.Sources) != 51 {
		t.Fatalf("one page of 100 returned %d", len(p.Sources))
	}
	page(ownerSession, site, "?limit=20")
	if got := audits(); got != before {
		t.Fatalf("listing wrote %d audit rows, want 0", got-before)
	}
	if w := call(ownerSession, "GET", sourcesOf(site)+"/"+want[0], "", "", ""); w.Code != 200 {
		t.Fatalf("source read: %d", w.Code)
	}
	if got := audits(); got != before+2 {
		t.Fatalf("source read wrote %d audit rows, want 2", got-before)
	}
	// Default limit is 50: 51 sources page as 50 + 1. Explicit limit 20 pages 20+20+11.
	for limit, sizes := range map[string][]int{"": {50, 1}, "20": {20, 20, 11}, "100": {51}} {
		got, gotSizes := all(ownerSession, site, limit)
		ids := make([]string, len(got))
		for i, s := range got {
			ids[i] = s.SourceID
		}
		if !slices.Equal(ids, want) || !slices.Equal(gotSizes, sizes) {
			t.Fatalf("limit %q: pages %v, ids match=%v", limit, gotSizes, slices.Equal(ids, want))
		}
	}
	// A viewer reads the same list; the summary never carries a credential reference.
	viewed, _ := all(viewerSession, site, "100")
	if len(viewed) != 51 || viewed[0].Revision != 1 || viewed[0].MappingRevision != 1 || viewed[0].MappingCount != 0 || viewed[0].Type != "csv" {
		t.Fatalf("viewer list=%d first=%+v", len(viewed), viewed[0])
	}
	if w := call(viewerSession, "GET", sourcesOf(site), "", "", ""); strings.Contains(w.Body.String(), "credential_ref") {
		t.Fatal("list disclosed a credential reference member")
	}
	// Isolation: site b and the foreign tenant list only their own source; other sites are 404.
	if p := page(viewerSession, otherSite, ""); len(p.Sources) != 1 || p.Sources[0].SourceID != other {
		t.Fatalf("site b list=%+v", p)
	}
	if p := page(foreignSession, foreignSite, ""); len(p.Sources) != 1 || p.Sources[0].SourceID != foreign {
		t.Fatalf("foreign list=%+v", p)
	}
	problemAt(call(foreignSession, "GET", sourcesOf(site), "", "", ""), 404, "NOT_FOUND", "")
	problemAt(call(ownerSession, "GET", sourcesOf(otherSite), "", "", ""), 404, "NOT_FOUND", "") // same tenant, not a member
	problemAt(call(viewerSession, "GET", sourcesOf(foreignSite), "", "", ""), 404, "NOT_FOUND", "")
	problemAt(call(viewerSession, "GET", sourcesOf(uuid.NewString()), "", "", ""), 404, "NOT_FOUND", "")
	if w := call(session{}, "GET", sourcesOf(site), "", "", ""); w.Code != 401 {
		t.Fatalf("anonymous list: %d", w.Code)
	}
	for _, q := range []string{"?limit=0", "?limit=101", "?limit=02", "?limit=x", "?cursor=nope", "?cursor=" + strings.ToUpper(want[0]), "?limit=1&limit=2", "?page=2"} {
		problemAt(call(ownerSession, "GET", sourcesOf(site)+q, "", "", ""), 422, "INVALID_REQUEST", "")
	}

	// Mapping row paths. One real entity on site a.
	actor := evidence.Actor{Issuer: issuer, Subject: owner, SessionHash: ownerSession.hash}
	proof, err := evidence.New(runtime).CreateOrRevise(ctx, actor, evidence.Scope{TenantID: tenant, SiteID: site}, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Synthetic asset identity", Text: "Press 1 asset identity"}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	entityID := uuid.NewString()
	entity, err := knowledge.New(runtime).CreateEntity(ctx, knowledge.Actor{Issuer: issuer, Subject: owner, SessionHash: ownerSession.hash}, knowledge.Scope{TenantID: tenant, SiteID: site}, knowledge.EntityInput{EntityID: entityID, Type: "asset", AssetKind: "machine", Name: "Press 1", SourceID: proof.SourceID, SourceRevision: proof.Revision}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	row := func(kind, key, entity string, revision int64) map[string]any {
		return map[string]any{"external_type": kind, "external_key": key, "entity_id": entity, "entity_revision": revision}
	}
	body := func(rows ...map[string]any) string {
		b, e := json.Marshal(map[string]any{"mappings": rows})
		if e != nil {
			t.Fatal(e)
		}
		return string(b)
	}
	target := want[0]
	mappings := sourcesOf(site) + "/" + target + "/mappings"
	good := row("asset", "press-01", entityID, entity.Revision)
	for _, c := range []struct {
		name, body, ifMatch string
		status              int
		code, path          string
	}{
		{"padded key on row 1", body(good, row("order", " WO-1", entityID, entity.Revision)), `"1"`, 422, "INVALID_SOURCE", "mappings[1].external_key"},
		{"unknown type on row 0", body(row("line", "L1", entityID, entity.Revision)), `"1"`, 422, "INVALID_SOURCE", "mappings[0].external_type"},
		{"duplicate key reports the later input row", body(row("asset", "z-key", entityID, entity.Revision), row("asset", "z-key", uuid.NewString(), 1)), `"1"`, 422, "INVALID_SOURCE", "mappings[1].external_key"},
		{"unknown entity keeps 404", body(good, row("order", "WO-1", uuid.NewString(), 1)), `"1"`, 404, "NOT_FOUND", "mappings[1].entity_id"},
		// Input index survives canonical sorting: "z-press" sorts after "press-01".
		{"stale entity revision keeps 412", body(row("asset", "z-press", entityID, entity.Revision+1), good), `"1"`, 412, "REVISION_CONFLICT", "mappings[0].entity_revision"},
		{"stale mapping head has no path", body(good), `"7"`, 412, "REVISION_CONFLICT", ""},
		{"unknown member is a decode error", strings.Replace(body(good), `"entity_revision"`, `"x":120,"entity_revision"`, 1), `"1"`, 422, "INVALID_REQUEST", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			problemAt(call(ownerSession, "POST", mappings, uuid.NewString(), c.ifMatch, c.body), c.status, c.code, c.path)
		})
	}
	problemAt(call(viewerSession, "POST", mappings, uuid.NewString(), `"1"`, body(good)), 403, "PERMISSION_DENIED", "")
	// No rejected attempt appended a revision.
	if p := page(ownerSession, site, "?limit=1"); p.Sources[0].Revision != 1 || p.Sources[0].MappingRevision != 1 {
		t.Fatalf("rejected mapping attempts changed revisions: %+v", p.Sources[0])
	}
	if w := call(ownerSession, "POST", mappings, uuid.NewString(), `"1"`, body(good)); w.Code != 200 || w.Header().Get("ETag") != `"2"` || w.Header().Get("X-Source-ETag") != `"2"` {
		t.Fatalf("valid mapping: %d %s", w.Code, w.Body.String())
	}
	if p := page(ownerSession, site, "?limit=1"); p.Sources[0].SourceID != target || p.Sources[0].Revision != 2 || p.Sources[0].MappingRevision != 2 || p.Sources[0].MappingCount != 1 {
		t.Fatalf("list after mapping=%+v", p.Sources[0])
	}
}
