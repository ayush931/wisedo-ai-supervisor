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
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
)

func TestKnowledgeHTTPDiscoveryInventories(t *testing.T) {
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
	name := "discovery_http_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	issuer, origin := "https://discovery-http.invalid/"+uuid.NewString(), "http://127.0.0.1:58176"
	exec("INSERT INTO supervisor.tenants(id,name) VALUES($1,'discovery synthetic')", tenant)
	exec("INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'discovery synthetic'),($1,$3,'inaccessible')", tenant, site, foreign)
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
	path := "/v1/sites/" + site + "/discovery-inventories"
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
	status(call(session{}, "GET", path+"/"+uuid.NewString(), "", "", "", ""), 401)
	sourceID, areaID, processID, id := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	source := `{"source_id":"` + sourceID + `","kind":"manual","title":"Scope evidence","text":"Synthetic area and process inventory"}`
	status(call(owner, "POST", "/v1/sites/"+site+"/sources", source, uuid.NewString(), `"0"`, owner.csrf), 200)
	entity := func(id, kind, name string) string {
		return `{"entity_id":"` + id + `","type":"` + kind + `","display_name":"` + name + `","source_id":"` + sourceID + `","source_revision":1}`
	}
	status(call(owner, "POST", "/v1/sites/"+site+"/entities", entity(areaID, "area", "Assembly area"), uuid.NewString(), `"0"`, owner.csrf), 200)
	status(call(owner, "POST", "/v1/sites/"+site+"/entities", entity(processID, "process", "Assembly process"), uuid.NewString(), `"0"`, owner.csrf), 200)
	body := `{"inventory_id":"` + id + `","area_id":"` + areaID + `","area_revision":1,"process_id":"` + processID + `","process_revision":1,"capability":"shortfall_investigation","reason":"Register scoped discovery"}`
	key := uuid.NewString()
	status(call(owner, "POST", path, body, key, `"0"`, "wrong"), 403)
	wantProblem(t, call(owner, "POST", path, body, key, "", owner.csrf), 428, "REVISION_REQUIRED")
	status(call(owner, "POST", path, body, key, `"00"`, owner.csrf), 422)
	status(call(owner, "POST", path, strings.Replace(body, `"reason":`, `"ready":true,"reason":`, 1), key, `"0"`, owner.csrf), 422)
	status(call(owner, "POST", path, strings.Replace(body, `"reason":`, `"requirements":[],"reason":`, 1), key, `"0"`, owner.csrf), 422)
	status(call(viewer, "POST", path, body, key, `"0"`, viewer.csrf), 403)
	wantProblem(t, call(owner, "POST", path, body, key, `"1"`, owner.csrf), 412, "REVISION_CONFLICT")
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now()-interval '20 minutes' WHERE token_hash=$1`, owner.hash)
	wantProblem(t, call(owner, "POST", path, body, key, `"0"`, owner.csrf), 428, "REAUTHENTICATION_REQUIRED")
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash=$1`, owner.hash)
	created := call(owner, "POST", path, body, key, `"0"`, owner.csrf)
	status(created, 200)
	var inventory struct {
		InventoryID     string `json:"inventory_id"`
		Revision        int64  `json:"revision"`
		CurrentRevision int64  `json:"current_revision"`
		ScopeCurrent    bool   `json:"scope_current"`
		Requirements    []struct {
			RequirementID string `json:"requirement_id"`
		} `json:"requirements"`
	}
	decode := func(w *httptest.ResponseRecorder) {
		t.Helper()
		if e := json.Unmarshal(w.Body.Bytes(), &inventory); e != nil {
			t.Fatal(e)
		}
	}
	decode(created)
	if inventory.InventoryID != id || inventory.Revision != 1 || !inventory.ScopeCurrent || len(inventory.Requirements) != 9 || created.Header().Get("ETag") != `"1"` {
		t.Fatalf("inventory metadata %s", created.Body.String())
	}
	firstRequirement := inventory.Requirements[0].RequirementID
	policyPath := "/v1/sites/" + site + "/document-freshness-policy"
	policyBody := `{"verdict":"define","max_age_seconds":3600,"reason":"Owner approved document review interval"}`
	policyKey := uuid.NewString()
	status(call(owner, "POST", policyPath+"-decisions", policyBody, policyKey, `"0"`, "wrong"), 403)
	status(call(viewer, "POST", policyPath+"-decisions", policyBody, policyKey, `"0"`, viewer.csrf), 403)
	wantProblem(t, call(owner, "POST", policyPath+"-decisions", policyBody, policyKey, "", owner.csrf), 428, "REVISION_REQUIRED")
	status(call(owner, "POST", policyPath+"-decisions", strings.Replace(policyBody, `"reason":`, `"basis":"upload_time","reason":`, 1), policyKey, `"0"`, owner.csrf), 422)
	policyResult := call(owner, "POST", policyPath+"-decisions", policyBody, policyKey, `"0"`, owner.csrf)
	status(policyResult, 200)
	if policyResult.Header().Get("ETag") != `"1"` || !strings.Contains(policyResult.Body.String(), `"policy_current":true`) {
		t.Fatal(policyResult.Body.String())
	}
	status(call(owner, "POST", policyPath+"-decisions", policyBody, policyKey, `"0"`, owner.csrf), 200)
	wantProblem(t, call(owner, "POST", policyPath+"-decisions", policyBody, uuid.NewString(), `"0"`, owner.csrf), 412, "REVISION_CONFLICT")
	status(call(viewer, "GET", policyPath, "", "", "", ""), 200)
	for _, query := range []string{"?revision=0", "?revision=01", "?revision=1&revision=2", "?unknown=1", "?revision=%zz"} {
		status(call(viewer, "GET", policyPath+query, "", "", "", ""), 422)
	}
	status(call(owner, "POST", policyPath+"-decisions", `{"verdict":"withdraw","reason":"Policy needs revision"}`, uuid.NewString(), `"1"`, owner.csrf), 200)
	policyHistory := call(viewer, "GET", policyPath+"?revision=1", "", "", "", "")
	status(policyHistory, 200)
	if !strings.Contains(policyHistory.Body.String(), `"policy_current":false`) {
		t.Fatal("withdrawn policy remains current")
	}
	coveragePath := path + "/" + id + "/coverage"
	coverage := call(viewer, "GET", coveragePath, "", "", "", "")
	status(coverage, 200)
	var report struct {
		ApplicableCount    int    `json:"applicable_count"`
		ApprovedCount      int    `json:"approved_count"`
		BoundCount         int    `json:"bound_count"`
		KnowledgeReady     bool   `json:"knowledge_ready"`
		AuthorizationState string `json:"authorization_state"`
		EvaluationState    string `json:"evaluation_state"`
	}
	checkCoverage := func(w *httptest.ResponseRecorder, applicable int) {
		t.Helper()
		status(w, 200)
		if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.ApplicableCount != applicable || report.ApprovedCount != 0 || report.BoundCount != 0 || report.KnowledgeReady || report.AuthorizationState != "disabled" || report.EvaluationState != "partial_support" {
			t.Fatalf("misleading coverage: %s", w.Body.String())
		}
	}
	checkCoverage(coverage, 9)
	status(call(session{}, "GET", coveragePath, "", "", "", ""), 401)
	for _, query := range []string{"?revision=1", "?unknown=x", "?bad=%zz"} {
		status(call(viewer, "GET", coveragePath+query, "", "", "", ""), 422)
	}
	decisionPath := path + "/" + id + "/requirements/" + firstRequirement + "/decisions"
	decisionBody := `{"inventory_revision":1,"verdict":"not_applicable","reason":"Reviewer confirmed this requirement is outside this scope"}`
	decisionKey := uuid.NewString()
	status(call(owner, "POST", decisionPath, decisionBody, decisionKey, `"0"`, "wrong"), 403)
	wantProblem(t, call(owner, "POST", decisionPath, decisionBody, decisionKey, "", owner.csrf), 428, "REVISION_REQUIRED")
	status(call(viewer, "POST", decisionPath, decisionBody, decisionKey, `"0"`, viewer.csrf), 403)
	status(call(owner, "POST", decisionPath, strings.Replace(decisionBody, `"reason":`, `"decision_eligible":true,"reason":`, 1), decisionKey, `"0"`, owner.csrf), 422)
	decision := call(owner, "POST", decisionPath, decisionBody, decisionKey, `"0"`, owner.csrf)
	status(decision, 200)
	if decision.Header().Get("ETag") != `"1"` || !strings.Contains(decision.Body.String(), `"decision_eligible":true`) {
		t.Fatalf("invalid decision metadata: %s", decision.Body.String())
	}
	status(call(owner, "POST", decisionPath, decisionBody, decisionKey, `"0"`, owner.csrf), 200)
	checkCoverage(call(viewer, "GET", coveragePath, "", "", "", ""), 8)
	wantProblem(t, call(owner, "POST", decisionPath, decisionBody, uuid.NewString(), `"0"`, owner.csrf), 412, "REVISION_CONFLICT")
	status(call(viewer, "GET", decisionPath+"?inventory_revision=1", "", "", "", ""), 200)
	for _, query := range []string{"", "?inventory_revision=0", "?inventory_revision=01", "?inventory_revision=1&revision=0", "?inventory_revision=1&revision=1&revision=2", "?inventory_revision=1&extra=x", "?inventory_revision=%zz"} {
		status(call(viewer, "GET", decisionPath+query, "", "", "", ""), 422)
	}
	withdraw := strings.Replace(decisionBody, "not_applicable", "withdraw", 1)
	status(call(owner, "POST", decisionPath, withdraw, uuid.NewString(), `"1"`, owner.csrf), 200)
	checkCoverage(call(viewer, "GET", coveragePath, "", "", "", ""), 9)
	historicalDecision := call(viewer, "GET", decisionPath+"?inventory_revision=1&revision=1", "", "", "", "")
	status(historicalDecision, 200)
	if !strings.Contains(historicalDecision.Body.String(), `"decision_eligible":false`) {
		t.Fatal("withdrawn decision remains eligible")
	}
	readPath := path + "/" + id
	status(call(viewer, "GET", readPath, "", "", "", ""), 200)
	status(call(owner, "POST", path, body, key, `"0"`, owner.csrf), 200)
	wantProblem(t, call(owner, "POST", path, strings.Replace(body, "Register", "Change", 1), key, `"0"`, owner.csrf), 409, "IDEMPOTENCY_CONFLICT")
	wantProblem(t, call(owner, "POST", path, strings.Replace(body, "shortfall_investigation", "internal_task_coordination", 1), uuid.NewString(), `"1"`, owner.csrf), 409, "DISCOVERY_CONFLICT")
	next := call(owner, "POST", path, body, uuid.NewString(), `"1"`, owner.csrf)
	status(next, 200)
	decode(next)
	if inventory.Revision != 2 || inventory.Requirements[0].RequirementID != firstRequirement {
		t.Fatalf("unstable requirement IDs: %s", next.Body.String())
	}
	history := call(viewer, "GET", readPath+"?revision=1", "", "", "", "")
	status(history, 200)
	decode(history)
	if inventory.Revision != 1 || inventory.CurrentRevision != 2 {
		t.Fatalf("history overwritten %s", history.Body.String())
	}
	for _, query := range []string{"?revision=0", "?revision=-1", "?revision=1&revision=2", "?revision=1&unknown=x", "?revision=%zz"} {
		status(call(viewer, "GET", readPath+query, "", "", "", ""), 422)
	}
	status(call(owner, "GET", "/v1/sites/"+foreign+"/discovery-inventories/"+id, "", "", "", ""), 404)
	// Anchor updates make history visibly stale without rewriting its original pins.
	status(call(owner, "POST", "/v1/sites/"+site+"/entities", entity(processID, "process", "Updated assembly process"), uuid.NewString(), `"1"`, owner.csrf), 200)
	stale := call(viewer, "GET", readPath, "", "", "", "")
	status(stale, 200)
	decode(stale)
	if inventory.ScopeCurrent {
		t.Fatal("stale process anchor reported current")
	}
	wantProblem(t, call(owner, "POST", path, body, uuid.NewString(), `"2"`, owner.csrf), 409, "SOURCE_CHANGED")
	refreshed := strings.Replace(body, `"process_revision":1`, `"process_revision":2`, 1)
	status(call(owner, "POST", path, refreshed, uuid.NewString(), `"2"`, owner.csrf), 200)
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now()-interval '20 minutes' WHERE token_hash=$1`, owner.hash)
	wantProblem(t, call(owner, "POST", path, body, key, `"0"`, owner.csrf), 428, "REAUTHENTICATION_REQUIRED")
	status(call(owner, "GET", readPath, "", "", "", ""), 200)
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash=$1`, owner.hash)
	status(call(owner, "POST", "/v1/sites/"+site+"/sources/"+sourceID+"/revocations", `{"reason":"Source removed from authorized discovery"}`, uuid.NewString(), `"1"`, owner.csrf), 200)
	status(call(viewer, "GET", readPath, "", "", "", ""), 404)
	status(call(viewer, "GET", coveragePath, "", "", "", ""), 404)
	status(call(owner, "POST", path, body, key, `"0"`, owner.csrf), 404)
}
