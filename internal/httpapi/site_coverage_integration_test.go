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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
)

// WIS-270: GET /v1/sites/{site}/coverage resolves the site's discovery inventory server-side and
// fails closed with an explicit NOT_READY reason instead of an empty or cached result.
func TestKnowledgeHTTPSiteCoverage(t *testing.T) {
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
	name := "site_coverage_http_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	tenant, site, siblingSite, otherTenant, otherSite := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, origin := "https://site-coverage-http.invalid/"+uuid.NewString(), "http://127.0.0.1:58176"
	exec("INSERT INTO supervisor.tenants(id,name) VALUES($1,'site coverage synthetic'),($2,'other tenant')", tenant, otherTenant)
	exec("INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'site coverage synthetic'),($1,$3,'sibling site'),($4,$5,'other tenant site')", tenant, site, siblingSite, otherTenant, otherSite)
	type session struct{ token, csrf, subject string }
	seed := func(tenantID, siteID, role string) session {
		t.Helper()
		a := session{token: uuid.NewString(), csrf: uuid.NewString(), subject: uuid.NewString()}
		sum := sha256.Sum256([]byte(a.token))
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, hex.EncodeToString(sum[:]), a.subject, issuer, a.csrf)
		if role != "" {
			exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, tenantID, siteID, issuer, a.subject, role)
		}
		return a
	}
	owner, viewer := seed(tenant, site, "owner"), seed(tenant, site, "viewer")
	members := map[string]session{"owner": owner, "supervisor": seed(tenant, site, "supervisor"), "operator": seed(tenant, site, "operator"), "viewer": viewer}
	outsider, foreignOwner, siblingOwner := seed("", "", ""), seed(otherTenant, otherSite, "owner"), seed(tenant, siblingSite, "owner")
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "snapshot", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, db.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)
	call := func(a session, method, p, body, key, match string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, p, strings.NewReader(body))
		if a.token != "" {
			r.AddCookie(&http.Cookie{Name: "supervisor", Value: a.token})
		}
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-CSRF-Token", a.csrf)
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		if match != "" {
			r.Header.Set("If-Match", match)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	expect := func(w *httptest.ResponseRecorder, want int, code string) {
		t.Helper()
		var p struct {
			Code string `json:"code"`
		}
		if w.Code != want || (code != "" && (json.Unmarshal(w.Body.Bytes(), &p) != nil || p.Code != code)) {
			t.Fatalf("HTTP%d want %d %s: %s", w.Code, want, code, w.Body.String())
		}
	}
	audits := func() int {
		t.Helper()
		var n int
		if e := admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_audit WHERE event_type='coverage.read' AND tenant_id=$1 AND site_id=$2 AND target_id=$2 AND revision IS NULL`, tenant, site).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	listedAudits := func() int {
		t.Helper()
		var n int
		if e := admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_audit WHERE event_type='discovery.listed' AND tenant_id=$1 AND site_id=$2`, tenant, site).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	sitePath := "/v1/sites/" + site + "/coverage"
	type siteCoverage struct {
		SchemaVersion int            `json:"schema_version"`
		Readiness     string         `json:"readiness"`
		ReasonCode    string         `json:"reason_code"`
		InventoryID   string         `json:"inventory_id"`
		Coverage      map[string]any `json:"coverage"`
	}
	// read performs one site coverage GET and requires exactly one audit row for it.
	read := func(a session, readiness, reason, inventory string) siteCoverage {
		t.Helper()
		before := audits()
		w := call(a, "GET", sitePath, "", "", "")
		expect(w, 200, "")
		var out siteCoverage
		if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil {
			t.Fatal(e)
		}
		if out.SchemaVersion != 1 || out.Readiness != readiness || out.ReasonCode != reason || out.InventoryID != inventory {
			t.Fatalf("site coverage %s", w.Body.String())
		}
		if after := audits(); after != before+1 {
			t.Fatalf("audit rows %d want %d", after, before+1)
		}
		return out
	}
	// sameAsInventory compares the embedded projection with the per-inventory route, field for field.
	sameAsInventory := func(got siteCoverage, inventory string, revision float64) {
		t.Helper()
		w := call(viewer, "GET", "/v1/sites/"+site+"/discovery-inventories/"+inventory+"/coverage", "", "", "")
		expect(w, 200, "")
		var want map[string]any
		if e := json.Unmarshal(w.Body.Bytes(), &want); e != nil {
			t.Fatal(e)
		}
		delete(want, "evaluated_at")
		delete(got.Coverage, "evaluated_at")
		if !reflect.DeepEqual(got.Coverage, want) || want["inventory_revision"] != revision {
			t.Fatalf("site coverage %v differs from inventory coverage %v", got.Coverage, want)
		}
	}

	source := func(a session, siteID string) string {
		id := uuid.NewString()
		expect(call(a, "POST", "/v1/sites/"+siteID+"/sources", `{"source_id":"`+id+`","kind":"manual","title":"Scope evidence","text":"Synthetic area and process inventory"}`, uuid.NewString(), `"0"`), 200, "")
		return id
	}
	entity := func(a session, siteID, sourceID, kind, name, match string, id string) string {
		expect(call(a, "POST", "/v1/sites/"+siteID+"/entities", `{"entity_id":"`+id+`","type":"`+kind+`","display_name":"`+name+`","source_id":"`+sourceID+`","source_revision":1}`, uuid.NewString(), match), 200, "")
		return id
	}
	inventoryBody := func(id, area, process string, processRevision, capability string) string {
		return `{"inventory_id":"` + id + `","area_id":"` + area + `","area_revision":1,"process_id":"` + process + `","process_revision":` + processRevision + `,"capability":"` + capability + `","reason":"Register scoped discovery"}`
	}
	// Inventories in another tenant and in a sibling site of the same tenant must never be
	// resolved for this site: it stays NO_INVENTORY, then a single (not ambiguous) inventory.
	for _, other := range []struct {
		a    session
		site string
	}{{foreignOwner, otherSite}, {siblingOwner, siblingSite}} {
		src := source(other.a, other.site)
		area, process := entity(other.a, other.site, src, "area", "Other area", `"0"`, uuid.NewString()), entity(other.a, other.site, src, "process", "Other process", `"0"`, uuid.NewString())
		expect(call(other.a, "POST", "/v1/sites/"+other.site+"/discovery-inventories", inventoryBody(uuid.NewString(), area, process, "1", "shortfall_investigation"), uuid.NewString(), `"0"`), 200, "")
	}

	// No inventory is an explicit NOT_READY, for every member role, never an empty covered result.
	for role, a := range members {
		if got := read(a, "NOT_READY", "NO_INVENTORY", ""); got.Coverage != nil {
			t.Fatalf("%s: coverage without inventory: %+v", role, got)
		}
	}
	before := audits()
	expect(call(session{}, "GET", sitePath, "", "", ""), 401, "")
	expect(call(outsider, "GET", sitePath, "", "", ""), 404, "NOT_FOUND")
	expect(call(foreignOwner, "GET", sitePath, "", "", ""), 404, "NOT_FOUND")
	expect(call(owner, "GET", "/v1/sites/"+otherSite+"/coverage", "", "", ""), 404, "NOT_FOUND")
	for _, query := range []string{"?inventory_id=" + uuid.NewString(), "?unknown=x", "?bad=%zz"} {
		expect(call(viewer, "GET", sitePath+query, "", "", ""), 422, "INVALID_REQUEST")
	}
	if audits() != before {
		t.Fatal("rejected site coverage reads were audited")
	}

	sourceID, spareSource := source(owner, site), source(owner, site)
	areaID, processID := entity(owner, site, sourceID, "area", "Assembly area", `"0"`, uuid.NewString()), entity(owner, site, sourceID, "process", "Assembly process", `"0"`, uuid.NewString())
	spareArea, spareProcess := entity(owner, site, spareSource, "area", "Packing area", `"0"`, uuid.NewString()), entity(owner, site, spareSource, "process", "Packing process", `"0"`, uuid.NewString())
	id := uuid.NewString()
	expect(call(owner, "POST", "/v1/sites/"+site+"/discovery-inventories", inventoryBody(id, areaID, processID, "1", "shortfall_investigation"), uuid.NewString(), `"0"`), 200, "")

	// One inventory: the projection is the per-inventory coverage of its current revision.
	sameAsInventory(read(viewer, "NOT_READY", "COVERAGE_INCOMPLETE", id), id, 1)
	revised := call(owner, "POST", "/v1/sites/"+site+"/discovery-inventories", inventoryBody(id, areaID, processID, "1", "shortfall_investigation"), uuid.NewString(), `"1"`)
	expect(revised, 200, "")
	sameAsInventory(read(viewer, "NOT_READY", "COVERAGE_INCOMPLETE", id), id, 2)

	// Fully covered: the policy cell is bound and qualified, every other cell is reviewer
	// not_applicable. knowledge_ready is never true in v1 (capability authorization is
	// disabled), so READY is unreachable and the outcome is exactly AUTHORIZATION_DISABLED.
	var inventory struct {
		Requirements []struct {
			RequirementID string `json:"requirement_id"`
			Key           string `json:"key"`
		} `json:"requirements"`
	}
	if e := json.Unmarshal(revised.Body.Bytes(), &inventory); e != nil {
		t.Fatal(e)
	}
	expect(call(owner, "POST", "/v1/sites/"+site+"/document-freshness-policy-decisions", `{"verdict":"define","max_age_seconds":3600,"reason":"Owner approved document review interval"}`, uuid.NewString(), `"0"`), 200, "")
	for _, r := range inventory.Requirements {
		body := `{"inventory_revision":2,"verdict":"not_applicable","reason":"Reviewer confirmed this requirement is outside this scope"}`
		if r.Key == "evidence_freshness_policy" {
			body = `{"inventory_revision":2,"verdict":"bind","freshness_policy_revision":1,"reason":"Explicit owner policy binding"}`
		}
		expect(call(owner, "POST", "/v1/sites/"+site+"/discovery-inventories/"+id+"/requirements/"+r.RequirementID+"/decisions", body, uuid.NewString(), `"0"`), 200, "")
	}
	covered := read(viewer, "NOT_READY", "AUTHORIZATION_DISABLED", id)
	sameAsInventory(covered, id, 2)
	if covered.Coverage["approved_count"] != float64(1) || covered.Coverage["applicable_count"] != float64(1) || covered.Coverage["knowledge_ready"] != false {
		t.Fatalf("not fully covered: %v", covered.Coverage)
	}

	// A revised anchor makes every cell stale with the per-inventory reason scope_revision_changed.
	entity(owner, site, sourceID, "process", "Updated assembly process", `"1"`, processID)
	stale := read(viewer, "NOT_READY", "COVERAGE_STALE", id)
	sameAsInventory(stale, id, 2)
	for _, cell := range stale.Coverage["requirements"].([]any) {
		if c := cell.(map[string]any); c["state"] != "stale" || c["reason_code"] != "scope_revision_changed" {
			t.Fatalf("stale cell %v", c)
		}
	}

	// Revoked anchor evidence: the per-inventory route fails closed with 404; the site read stays
	// an explicit NOT_READY without returning any projection.
	expect(call(owner, "POST", "/v1/sites/"+site+"/sources/"+sourceID+"/revocations", `{"reason":"Source removed from authorized discovery"}`, uuid.NewString(), `"1"`), 200, "")
	expect(call(viewer, "GET", "/v1/sites/"+site+"/discovery-inventories/"+id+"/coverage", "", "", ""), 404, "NOT_FOUND")
	if got := read(viewer, "NOT_READY", "INVENTORY_UNAVAILABLE", ""); got.Coverage != nil {
		t.Fatalf("revoked inventory projected: %+v", got)
	}

	// A second inventory makes the site's inventory ambiguous; none is picked.
	secondInventoryID := uuid.NewString()
	expect(call(owner, "POST", "/v1/sites/"+site+"/discovery-inventories", inventoryBody(secondInventoryID, spareArea, spareProcess, "1", "internal_task_coordination"), uuid.NewString(), `"0"`), 200, "")
	if got := read(viewer, "NOT_READY", "INVENTORY_AMBIGUOUS", ""); got.Coverage != nil {
		t.Fatalf("ambiguous site projected: %+v", got)
	}
	thirdInventoryID := uuid.NewString()
	expect(call(owner, "POST", "/v1/sites/"+site+"/discovery-inventories", inventoryBody(thirdInventoryID, spareArea, spareProcess, "1", "shortfall_investigation"), uuid.NewString(), `"0"`), 200, "")
	expect(call(owner, "POST", "/v1/sites/"+site+"/discovery-inventories", inventoryBody(secondInventoryID, spareArea, spareProcess, "1", "internal_task_coordination"), uuid.NewString(), `"1"`), 200, "")
	// A later process revision must not rename the inventories: the list shows the pinned revision's name.
	entity(owner, site, spareSource, "process", "Packing process renamed", `"1"`, spareProcess)

	listPath := "/v1/sites/" + site + "/discovery-inventories"
	for _, query := range []string{"?limit=0", "?limit=abc", "?limit=101", "?unknown=x", "?limit=1&limit=2", "?cursor=bad", "?process_id=bad"} {
		before := listedAudits()
		expect(call(viewer, "GET", listPath+query, "", "", ""), 422, "INVALID_REQUEST")
		if after := listedAudits(); after != before {
			t.Fatalf("rejected discovery inventory list %s wrote an audit row: %d -> %d", query, before, after)
		}
	}
	expect(call(foreignOwner, "GET", listPath, "", "", ""), 404, "NOT_FOUND")
	expect(call(siblingOwner, "GET", listPath, "", "", ""), 404, "NOT_FOUND")
	expect(call(owner, "GET", "/v1/sites/"+otherSite+"/discovery-inventories", "", "", ""), 404, "NOT_FOUND")
	type inventoryList struct {
		Inventories []struct {
			InventoryID        string `json:"inventory_id"`
			ProcessID          string `json:"process_id"`
			ProcessDisplayName string `json:"process_display_name"`
			CurrentRevision    int64  `json:"current_revision"`
			CreatedAt          string `json:"created_at"`
			UpdatedAt          string `json:"updated_at"`
		} `json:"inventories"`
		NextCursor string `json:"next_cursor"`
	}
	list := func(path string) inventoryList {
		t.Helper()
		before := listedAudits()
		w := call(viewer, "GET", path, "", "", "")
		expect(w, 200, "")
		var page inventoryList
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if after := listedAudits(); after != before+1 {
			t.Fatalf("successful discovery inventory list wrote %d audit rows, want exactly one", after-before)
		}
		return page
	}
	wantRevision := func(id string) int64 {
		if id == secondInventoryID {
			return 2
		}
		return 1
	}
	first := list(listPath + "?limit=1&process_id=" + spareProcess)
	if len(first.Inventories) != 1 || first.NextCursor != first.Inventories[0].InventoryID || first.Inventories[0].ProcessID != spareProcess || first.Inventories[0].ProcessDisplayName != "Packing process" || first.Inventories[0].CurrentRevision != wantRevision(first.Inventories[0].InventoryID) {
		t.Fatalf("first inventory page %+v", first)
	}
	if _, err := time.Parse(time.RFC3339Nano, first.Inventories[0].CreatedAt); err != nil {
		t.Fatalf("invalid inventory created_at %q: %v", first.Inventories[0].CreatedAt, err)
	}
	if _, err := time.Parse(time.RFC3339Nano, first.Inventories[0].UpdatedAt); err != nil {
		t.Fatalf("invalid inventory updated_at %q: %v", first.Inventories[0].UpdatedAt, err)
	}
	repeated := list(listPath + "?limit=1&process_id=" + spareProcess)
	if repeated.Inventories[0].InventoryID != first.Inventories[0].InventoryID || repeated.NextCursor != first.NextCursor {
		t.Fatalf("first page changed between reads: %v then %v", first, repeated)
	}
	second := list(listPath + "?limit=1&cursor=" + first.NextCursor + "&process_id=" + spareProcess)
	if len(second.Inventories) != 1 || second.NextCursor != "" || second.Inventories[0].InventoryID <= first.Inventories[0].InventoryID || second.Inventories[0].ProcessID != spareProcess || second.Inventories[0].ProcessDisplayName != "Packing process" || second.Inventories[0].CurrentRevision != wantRevision(second.Inventories[0].InventoryID) {
		t.Fatalf("second inventory page %v after %v", second, first)
	}
	all := list(listPath)
	if len(all.Inventories) != 2 || all.NextCursor != "" || all.Inventories[0].InventoryID != first.Inventories[0].InventoryID || all.Inventories[1].InventoryID != second.Inventories[0].InventoryID {
		t.Fatalf("site inventory list leaked or omitted rows: %v", all)
	}
	filteredOut := list(listPath + "?process_id=" + processID)
	if len(filteredOut.Inventories) != 0 {
		t.Fatalf("revoked inventory remained visible in filtered list: %v", filteredOut)
	}
	byID := call(outsider, "GET", listPath+"/"+secondInventoryID, "", "", "")
	expect(byID, 404, "NOT_FOUND")
	deniedList := call(outsider, "GET", listPath, "", "", "")
	expect(deniedList, byID.Code, "NOT_FOUND")
}
