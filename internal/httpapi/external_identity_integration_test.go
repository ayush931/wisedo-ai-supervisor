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
	"strconv"
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
	_ "github.com/jackc/pgx/v5/stdlib"
)

// WIS-270 ExternalIdentity: only an exact key under a source that intake would accept right now
// resolves; every other case is one indistinguishable 409 UNKNOWN_MAPPING.
func TestWIS270ExternalIdentityResolvePostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
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
	database := "external_identity_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	issuer, origin := "https://external-identity.invalid/"+uuid.NewString(), "http://127.0.0.1:58178"
	type session struct{ subject, token, csrf, hash string }
	seed := func(subject string) session {
		t.Helper()
		v := session{subject: subject, token: uuid.NewString(), csrf: uuid.NewString()}
		sum := sha256.Sum256([]byte(v.token))
		v.hash = hex.EncodeToString(sum[:])
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, v.hash, subject, issuer, v.csrf)
		return v
	}
	type plant struct {
		tenant, site              string
		owner, reviewer, operator session
	}
	newPlant := func(tenant string) plant {
		t.Helper()
		p := plant{tenant: tenant, site: uuid.NewString(), owner: seed(uuid.NewString()), reviewer: seed(uuid.NewString()), operator: seed(uuid.NewString())}
		exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic external identity test') ON CONFLICT DO NOTHING`, tenant)
		exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'test site')`, tenant, p.site)
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner'),($1,$2,$3,$5,'supervisor'),($1,$2,$3,$6,'operator')`, tenant, p.site, issuer, p.owner.subject, p.reviewer.subject, p.operator.subject)
		return p
	}
	tenantA := uuid.NewString()
	a, b := newPlant(tenantA), newPlant(uuid.NewString())
	// A second site of tenant A where A's owner is also a member: same tenant, different site.
	a2 := plant{tenant: tenantA, site: uuid.NewString(), owner: a.owner}
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'second site')`, tenantA, a2.site)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, tenantA, a2.site, issuer, a.owner.subject)

	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "external-identity-test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)
	call := func(actor session, method, path, key, revision, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if actor.token != "" {
			r.AddCookie(&http.Cookie{Name: "supervisor", Value: actor.token})
		}
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
	must := func(w *httptest.ResponseRecorder, expected int) {
		t.Helper()
		if w.Code != expected {
			t.Fatalf("HTTP %d want %d: %s", w.Code, expected, w.Body.String())
		}
	}
	entityKind := func(p plant, name, kind string, expected int64, id string) int64 {
		t.Helper()
		ka, sc := knowledge.Actor{Issuer: issuer, Subject: p.owner.subject, SessionHash: p.owner.hash}, knowledge.Scope{TenantID: p.tenant, SiteID: p.site}
		proof, e := evidence.New(runtime).CreateOrRevise(ctx, evidence.Actor(ka), evidence.Scope(sc), evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Synthetic asset identity", Text: name}, uuid.NewString(), 0)
		if e != nil {
			t.Fatal(e)
		}
		out, e := knowledge.New(runtime).CreateEntity(ctx, ka, sc, knowledge.EntityInput{EntityID: id, Type: "asset", AssetKind: kind, Name: name, SourceID: proof.SourceID, SourceRevision: proof.Revision}, uuid.NewString(), expected)
		if e != nil {
			t.Fatal(e)
		}
		return out.Revision
	}
	entity := func(p plant, name string, expected int64, id string) int64 {
		t.Helper()
		return entityKind(p, name, "machine", expected, id)
	}
	type fixture struct {
		sourceID, entityID, decisionID, mappingSHA string
	}
	// source creates a source mapping "Press-01" and NFC "Café-01" to two entities, then
	// optionally reviews it. Every case gets its own source so cases are order independent.
	source := func(p plant, enabled bool, verdict string) fixture {
		t.Helper()
		f := fixture{sourceID: uuid.NewString(), entityID: uuid.NewString()}
		base := "/v1/sites/" + p.site + "/operational-sources"
		enabledJSON := "false"
		if enabled {
			enabledJSON = "true"
		}
		must(call(p.owner, "POST", base, uuid.NewString(), `"0"`, `{"source_id":"`+f.sourceID+`","type":"event_api","authority_scope":"production_delta","enabled":`+enabledJSON+`,"access_policy_revision":1,"freshness_seconds":120,"clock_skew_seconds":30}`), 200)
		entity(p, "Press 1", 0, f.entityID)
		other := uuid.NewString()
		entity(p, "Cafe press", 0, other)
		body, e := json.Marshal(map[string]any{"mappings": []map[string]any{
			{"external_type": "asset", "external_key": "Press-01", "entity_id": f.entityID, "entity_revision": 1},
			{"external_type": "asset", "external_key": "Café-01", "entity_id": other, "entity_revision": 1},
		}})
		if e != nil {
			t.Fatal(e)
		}
		mapped := call(p.owner, "POST", base+"/"+f.sourceID+"/mappings", uuid.NewString(), `"1"`, string(body))
		must(mapped, 200)
		var src struct {
			MappingSHA256 string `json:"mapping_sha256"`
		}
		if e = json.Unmarshal(mapped.Body.Bytes(), &src); e != nil || src.MappingSHA256 == "" {
			t.Fatalf("mapping response=%s err=%v", mapped.Body.String(), e)
		}
		f.mappingSHA = src.MappingSHA256
		if verdict != "" {
			reviewed := call(p.reviewer, "POST", base+"/"+f.sourceID+"/reviews", uuid.NewString(), `"2"`, `{"verdict":"`+verdict+`","reason":"Scope and mapping verified"}`)
			must(reviewed, 200)
			var review struct {
				DecisionID string `json:"decision_id"`
			}
			if e = json.Unmarshal(reviewed.Body.Bytes(), &review); e != nil || review.DecisionID == "" {
				t.Fatalf("review response=%s err=%v", reviewed.Body.String(), e)
			}
			f.decisionID = review.DecisionID
		}
		return f
	}
	resolvePath := func(site, sourceID, entityType, key string) string {
		return "/v1/sites/" + site + "/external-identities/resolve?" + url.Values{"source_id": {sourceID}, "entity_type": {entityType}, "external_key": {key}}.Encode()
	}
	audits := func(tenant, sourceID string) int {
		t.Helper()
		var n int
		if err := control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.operational_source_audit WHERE ($1='' OR (tenant_id::text=$1 AND source_id::text=$2)) AND event_type='mapping.read'`, tenant, sourceID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	code := func(w *httptest.ResponseRecorder) string {
		t.Helper()
		var p struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatalf("problem body=%s err=%v", w.Body.String(), err)
		}
		return p.Code
	}
	// expect asserts the status and problem code, and that exactly `audit` mapping.read rows were added.
	expect := func(name string, actor session, p plant, f fixture, path string, status int, problemCode string, audit int) *httptest.ResponseRecorder {
		t.Helper()
		before, beforeAll := audits(p.tenant, f.sourceID), audits("", "")
		w := call(actor, "GET", path, "", "", "")
		if w.Code != status {
			t.Fatalf("%s: HTTP %d want %d: %s", name, w.Code, status, w.Body.String())
		}
		if problemCode != "" && code(w) != problemCode {
			t.Fatalf("%s: problem code %q want %q: %s", name, code(w), problemCode, w.Body.String())
		}
		// Exactly `audit` rows for this source, and none anywhere else (e.g. for an unknown ID).
		if after, afterAll := audits(p.tenant, f.sourceID), audits("", ""); after-before != audit || afterAll-beforeAll != audit {
			t.Fatalf("%s: mapping.read audit rows added source=%d all=%d want %d", name, after-before, afterAll-beforeAll, audit)
		}
		return w
	}

	approved := source(a, true, "approved")
	ok := expect("exact approved", a.owner, a, approved, resolvePath(a.site, approved.sourceID, "asset", "Press-01"), 200, "", 1)
	if ok.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("resolve must not be cacheable: %q", ok.Header().Get("Cache-Control"))
	}
	var got map[string]any
	if err = json.Unmarshal(ok.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"entity_id": approved.entityID, "entity_revision": float64(1), "source_id": approved.sourceID, "source_revision": float64(2), "mapping_revision": float64(2), "mapping_sha256": approved.mappingSHA, "review_decision_id": approved.decisionID}
	// A direct mapping has an empty redirect path (an array, never null).
	if path, isList := got["redirect_path"].([]any); len(got) != len(want)+1 || !isList || len(path) != 0 {
		t.Fatalf("resolve body=%s", ok.Body.String())
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("resolve %s=%v want %v (body=%s)", k, got[k], v, ok.Body.String())
		}
	}
	// Any member who can read mappings can resolve, including operators.
	expect("operator reads", a.operator, a, approved, resolvePath(a.site, approved.sourceID, "asset", "Press-01"), 200, "", 1)
	expect("exact NFC key", a.owner, a, approved, resolvePath(a.site, approved.sourceID, "asset", "Café-01"), 200, "", 1)

	// Exact means byte-equal: case, Unicode normalization (NFD), type and unknown keys all miss.
	for name, q := range map[string][2]string{
		"case changed": {"asset", "press-01"},
		"NFD variant":  {"asset", "Café-01"},
		"wrong type":   {"order", "Press-01"},
		"unmapped key": {"asset", "Press-02"},
	} {
		expect(name, a.owner, a, approved, resolvePath(a.site, approved.sourceID, q[0], q[1]), 409, "UNKNOWN_MAPPING", 1)
	}

	// Every non-resolvable source state is the same 409 for the exact mapped key.
	revoked := source(a, true, "approved")
	must(call(a.owner, "POST", "/v1/sites/"+a.site+"/operational-sources/"+revoked.sourceID+"/revoke", uuid.NewString(), `"2"`, ""), 200)
	stale := source(a, true, "approved")
	entity(a, "Press 1 renamed", 1, stale.entityID)
	// The bodies are byte-identical apart from the correlation ID: the refusal is no oracle.
	var refusal string
	for name, c := range map[string]struct {
		f   fixture
		key string
	}{
		"unmapped key":          {approved, "Press-02"},
		"proposed not reviewed": {source(a, true, ""), "Press-01"},
		"rejected":              {source(a, true, "rejected"), "Press-01"},
		"disabled":              {source(a, false, "approved"), "Press-01"},
		"revoked":               {revoked, "Press-01"},
		"stale target":          {stale, "Press-01"},
	} {
		f := c.f
		w := expect(name, a.owner, a, f, resolvePath(a.site, f.sourceID, "asset", c.key), 409, "UNKNOWN_MAPPING", 1)
		correlation := w.Header().Get("X-Correlation-ID")
		body := strings.Replace(w.Body.String(), correlation, "", 1)
		if correlation == "" || body == w.Body.String() {
			t.Fatalf("%s: refusal lacks its correlation ID: %s", name, w.Body.String())
		}
		if refusal == "" {
			refusal = body
		} else if body != refusal {
			t.Fatalf("%s: refusal body differs:\n%s\nvs\n%s", name, body, refusal)
		}
		if strings.Contains(w.Body.String(), f.entityID) {
			t.Fatalf("%s: refusal disclosed the mapped entity: %s", name, w.Body.String())
		}
	}

	// Tenant B has an approved source mapping the same key; it resolves for B only.
	foreign := source(b, true, "approved")
	expect("tenant B own source", b.owner, b, foreign, resolvePath(b.site, foreign.sourceID, "asset", "Press-01"), 200, "", 1)
	expect("cross-tenant source", a.owner, b, foreign, resolvePath(a.site, foreign.sourceID, "asset", "Press-01"), 404, "NOT_FOUND", 0)
	// Same tenant, other site: A's source is not visible through site A2.
	expect("cross-site source", a.owner, a, approved, resolvePath(a2.site, approved.sourceID, "asset", "Press-01"), 404, "NOT_FOUND", 0)
	expect("unknown source", a.owner, a, approved, resolvePath(a.site, uuid.NewString(), "asset", "Press-01"), 404, "NOT_FOUND", 0)
	expect("non-member site", b.owner, a, approved, resolvePath(a.site, approved.sourceID, "asset", "Press-01"), 404, "NOT_FOUND", 0)
	expect("unauthenticated", session{}, a, approved, resolvePath(a.site, approved.sourceID, "asset", "Press-01"), 401, "", 0)

	// Malformed queries are 422 before any lookup or audit.
	good := url.Values{"source_id": {approved.sourceID}, "entity_type": {"asset"}, "external_key": {"Press-01"}}
	for name, mutate := range map[string]func(url.Values){
		"invalid UTF-8 key":  func(v url.Values) { v.Set("external_key", "Press-\xff") },
		"NUL in key":         func(v url.Values) { v.Set("external_key", "Press\x00-01") },
		"padded key":         func(v url.Values) { v.Set("external_key", " Press-01") },
		"trailing space":     func(v url.Values) { v.Set("external_key", "Press-01 ") },
		"empty key":          func(v url.Values) { v.Set("external_key", "") },
		"oversized key":      func(v url.Values) { v.Set("external_key", strings.Repeat("k", 513)) },
		"unknown param":      func(v url.Values) { v.Set("tenant_id", tenantA) },
		"missing param":      func(v url.Values) { v.Del("entity_type") },
		"repeated param":     func(v url.Values) { v.Add("external_key", "Press-01") },
		"unknown type":       func(v url.Values) { v.Set("entity_type", "line") },
		"non-canonical uuid": func(v url.Values) { v.Set("source_id", strings.ToUpper(approved.sourceID)) },
	} {
		v := url.Values{}
		for k, vs := range good {
			v[k] = append([]string(nil), vs...)
		}
		mutate(v)
		expect(name, a.owner, a, approved, "/v1/sites/"+a.site+"/external-identities/resolve?"+v.Encode(), 422, "INVALID_REQUEST", 0)
	}

	// WIS-270 merge: "merging identities creates redirect decision and preserves original
	// IDs/history." Resolution follows the redirect; the mapping and both entities are unchanged.
	merge := func(actor session, p plant, from, into, key, ifMatch string, fromRev, intoRev int, reason string) *httptest.ResponseRecorder {
		t.Helper()
		body, e := json.Marshal(map[string]any{"into_entity_id": into, "from_revision": fromRev, "into_revision": intoRev, "reason": reason})
		if e != nil {
			t.Fatal(e)
		}
		return call(actor, "POST", "/v1/sites/"+p.site+"/entities/"+from+"/merges", key, ifMatch, string(body))
	}
	problemIs := func(name string, w *httptest.ResponseRecorder, status int, problemCode string) {
		t.Helper()
		if w.Code != status || code(w) != problemCode {
			t.Fatalf("%s: HTTP %d %q want %d %q: %s", name, w.Code, code(w), status, problemCode, w.Body.String())
		}
	}
	resolved := func(name string, f fixture, wantEntity string, wantPath ...string) {
		t.Helper()
		w := expect(name, a.owner, a, f, resolvePath(a.site, f.sourceID, "asset", "Press-01"), 200, "", 1)
		var body struct {
			EntityID       string   `json:"entity_id"`
			EntityRevision int64    `json:"entity_revision"`
			RedirectPath   []string `json:"redirect_path"`
		}
		if e := json.Unmarshal(w.Body.Bytes(), &body); e != nil {
			t.Fatal(e)
		}
		if body.EntityID != wantEntity || body.EntityRevision != 1 || strings.Join(body.RedirectPath, ",") != strings.Join(wantPath, ",") {
			t.Fatalf("%s: resolved %s", name, w.Body.String())
		}
	}
	merged := source(a, true, "approved")
	into, third, line := uuid.NewString(), uuid.NewString(), uuid.NewString()
	entity(a, "Press 1 duplicate", 0, into)
	entity(a, "Press 1 triplicate", 0, third)
	entityKind(a, "Line 1", "line", 0, line)
	A := merged.entityID

	problemIs("operator merges", merge(a.operator, a, A, into, uuid.NewString(), `"1"`, 1, 1, "same press"), 403, "PERMISSION_DENIED")
	problemIs("cross-tenant reviewer", merge(b.reviewer, a, A, into, uuid.NewString(), `"1"`, 1, 1, "same press"), 404, "NOT_FOUND")
	problemIs("stale If-Match", merge(a.reviewer, a, A, into, uuid.NewString(), `"2"`, 2, 1, "same press"), 412, "REVISION_CONFLICT")
	problemIs("stale into revision", merge(a.reviewer, a, A, into, uuid.NewString(), `"1"`, 1, 2, "same press"), 412, "REVISION_CONFLICT")
	problemIs("body disagrees with If-Match", merge(a.reviewer, a, A, into, uuid.NewString(), `"1"`, 2, 1, "same press"), 412, "REVISION_CONFLICT")
	problemIs("machine into line", merge(a.reviewer, a, A, line, uuid.NewString(), `"1"`, 1, 1, "same press"), 422, "INVALID_CLAIM")
	problemIs("merge into itself", merge(a.reviewer, a, A, A, uuid.NewString(), `"1"`, 1, 1, "same press"), 422, "INVALID_CLAIM")
	// Recent authentication is required, as for every other review decision.
	staleAuth := session{subject: a.reviewer.subject, token: uuid.NewString(), csrf: uuid.NewString()}
	sum := sha256.Sum256([]byte(staleAuth.token))
	exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now()-interval '1 day')`, hex.EncodeToString(sum[:]), staleAuth.subject, issuer, staleAuth.csrf)
	problemIs("stale authentication", merge(staleAuth, a, A, into, uuid.NewString(), `"1"`, 1, 1, "same press"), 428, "REAUTHENTICATION_REQUIRED")
	resolved("before merge", merged, A)

	key := uuid.NewString()
	first := merge(a.reviewer, a, A, into, key, `"1"`, 1, 1, "same press")
	must(first, 200)
	var decision struct {
		DecisionID   string `json:"decision_id"`
		FromEntityID string `json:"from_entity_id"`
		ToEntityID   string `json:"to_entity_id"`
	}
	if err = json.Unmarshal(first.Body.Bytes(), &decision); err != nil || decision.DecisionID == "" || decision.FromEntityID != A || decision.ToEntityID != into || first.Header().Get("ETag") != `"1"` {
		t.Fatalf("merge response=%s etag=%q err=%v", first.Body.String(), first.Header().Get("ETag"), err)
	}
	replayed := merge(a.reviewer, a, A, into, key, `"1"`, 1, 1, "same press")
	must(replayed, 200)
	if replayed.Body.String() != first.Body.String() {
		t.Fatalf("replay differs:\n%s\nvs\n%s", replayed.Body.String(), first.Body.String())
	}
	problemIs("same key different body", merge(a.reviewer, a, A, into, key, `"1"`, 1, 1, "another reason"), 409, "IDEMPOTENCY_CONFLICT")
	resolved("after merge", merged, into, A)
	// The original entity and its history remain readable under its own ID.
	must(call(a.owner, "GET", "/v1/sites/"+a.site+"/entities/"+A, "", "", ""), 200)
	problemIs("second merge of A", merge(a.reviewer, a, A, third, uuid.NewString(), `"1"`, 1, 1, "same press"), 409, "REVIEW_CONFLICT")
	problemIs("merge into redirected", merge(a.reviewer, a, third, A, uuid.NewString(), `"1"`, 1, 1, "same press"), 409, "KNOWLEDGE_CYCLE")
	must(merge(a.reviewer, a, into, third, uuid.NewString(), `"1"`, 1, 1, "same press"), 200)
	resolved("chain A -> B -> C", merged, third, A, into)

	// Writes to a merged entity are refused with 409; reads stay allowed (checked above).
	ka, ks := knowledge.Actor{Issuer: issuer, Subject: a.owner.subject, SessionHash: a.owner.hash}, knowledge.Scope{TenantID: a.tenant, SiteID: a.site}
	proof, err := evidence.New(runtime).CreateOrRevise(ctx, evidence.Actor(ka), evidence.Scope(ks), evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Merged press", Text: "Press 1 belongs to line 1"}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	span, err := evidence.New(runtime).CreateSpan(ctx, evidence.Actor(ka), evidence.Scope(ks), evidence.SpanInput{SourceID: proof.SourceID, Revision: proof.Revision, Start: 0, End: 7}, uuid.NewString(), 1)
	if err != nil {
		t.Fatal(err)
	}
	revise := `{"entity_id":"` + A + `","type":"asset","asset_kind":"machine","display_name":"Press 1 renamed","source_id":"` + proof.SourceID + `","source_revision":1}`
	problemIs("revise merged entity", call(a.owner, "POST", "/v1/sites/"+a.site+"/entities", uuid.NewString(), `"1"`, revise), 409, "REVIEW_CONFLICT")
	claim := func(subject string) string {
		return `{"claim_id":"` + uuid.NewString() + `","subject_id":"` + subject + `","subject_revision":1,"predicate":"part_of_asset","object_entity_id":"` + line + `","object_revision":1,"effective_from":"2026-09-27T00:00:00Z","evidence_class":"report","source_span_ids":["` + span.ID + `"]}`
	}
	problemIs("claim pins merged entity", call(a.owner, "POST", "/v1/sites/"+a.site+"/claims", uuid.NewString(), `"0"`, claim(A)), 409, "REVIEW_CONFLICT")
	must(call(a.owner, "POST", "/v1/sites/"+a.site+"/claims", uuid.NewString(), `"0"`, claim(third)), 200)

	// Chain bound: a 16-hop chain is allowed and resolves; the merge making it 17 is refused.
	deep := source(a, true, "approved")
	chain := []string{deep.entityID}
	for i := 1; i <= 17; i++ {
		chain = append(chain, uuid.NewString())
		entity(a, "Chain press", 0, chain[i])
	}
	for i := 0; i < 16; i++ {
		must(merge(a.reviewer, a, chain[i], chain[i+1], uuid.NewString(), `"1"`, 1, 1, "same press"), 200)
	}
	problemIs("17th hop", merge(a.reviewer, a, chain[16], chain[17], uuid.NewString(), `"1"`, 1, 1, "same press"), 409, "KNOWLEDGE_CYCLE")
	resolved("16-hop chain", deep, chain[16], chain[:16]...)
	var merges int
	if err = control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_audit WHERE event_type='entity.merged' AND tenant_id=$1`, a.tenant).Scan(&merges); err != nil || merges != 18 { // A->B, B->C and 16 chain hops
		t.Fatalf("entity.merged audit rows=%d err=%v", merges, err)
	}
	// Tenant B sees none of tenant A's redirects.
	var visible int
	if err = func() error {
		tx, e := runtime.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, b.tenant); e != nil {
			return e
		}
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_entity_redirects`).Scan(&visible)
	}(); err != nil || visible != 0 {
		t.Fatalf("tenant B sees %d redirects, err=%v", visible, err)
	}

	// Concurrent opposite merges of a fresh pair: exactly one wins, the other is a 409, never 500.
	left, right := uuid.NewString(), uuid.NewString()
	entity(a, "Left press", 0, left)
	entity(a, "Right press", 0, right)
	results := make(chan *httptest.ResponseRecorder, 2)
	for _, pair := range [][2]string{{left, right}, {right, left}} {
		go func(from, to string) {
			body, _ := json.Marshal(map[string]any{"into_entity_id": to, "from_revision": 1, "into_revision": 1, "reason": "same press"})
			r := httptest.NewRequest("POST", "/v1/sites/"+a.site+"/entities/"+from+"/merges", strings.NewReader(string(body)))
			r.AddCookie(&http.Cookie{Name: "supervisor", Value: a.reviewer.token})
			r.Header.Set("Origin", origin)
			r.Header.Set("X-CSRF-Token", a.reviewer.csrf)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", uuid.NewString())
			r.Header.Set("If-Match", `"1"`)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			results <- w
		}(pair[0], pair[1])
	}
	statuses := map[int]int{}
	for range 2 {
		w := <-results
		statuses[w.Code]++
		if w.Code == 409 && code(w) != "KNOWLEDGE_CYCLE" {
			t.Fatalf("concurrent loser: %s", w.Body.String())
		}
	}
	if statuses[200] != 1 || statuses[409] != 1 {
		t.Fatalf("concurrent opposite merges: %v", statuses)
	}

	// Mapping revision: a redirected entity is no longer a mapping target; the survivor is.
	mapBody := func(target string) string {
		return `{"mappings":[{"external_type":"asset","external_key":"Press-01","entity_id":"` + target + `","entity_revision":1}]}`
	}
	mappings := "/v1/sites/" + a.site + "/operational-sources/" + merged.sourceID + "/mappings"
	problemIs("map to redirected entity", call(a.owner, "POST", mappings, uuid.NewString(), `"2"`, mapBody(A)), 409, "UNKNOWN_MAPPING")
	must(call(a.owner, "POST", mappings, uuid.NewString(), `"2"`, mapBody(third)), 200)

	// PR #119 security review: a redirect resolves only to the revisions the merge decisions
	// pinned, and only while the terminal entity's evidence is current and unrevoked. Each
	// refusal is the same 409 body as every other refusal above.
	refused := func(name string, f fixture) {
		t.Helper()
		w := expect(name, a.owner, a, f, resolvePath(a.site, f.sourceID, "asset", "Press-01"), 409, "UNKNOWN_MAPPING", 1)
		if body := strings.Replace(w.Body.String(), w.Header().Get("X-Correlation-ID"), "", 1); body != refusal {
			t.Fatalf("%s: refusal body differs:\n%s\nvs\n%s", name, body, refusal)
		}
	}
	fresh := func() string {
		t.Helper()
		id := uuid.NewString()
		entity(a, "Fresh press", 0, id)
		return id
	}
	ev := evidence.New(runtime)
	evidenceOf := func(entityID string) string {
		t.Helper()
		var id string
		if e := control.QueryRowContext(ctx, `SELECT source_id::text FROM supervisor.knowledge_entity_revisions WHERE id=$1 AND revision=1`, entityID).Scan(&id); e != nil {
			t.Fatal(e)
		}
		return id
	}
	revokeEvidence := func(entityID string) {
		t.Helper()
		if _, e := ev.Revoke(ctx, evidence.Actor(ka), evidence.Scope(ks), evidenceOf(entityID), "Evidence withdrawn", uuid.NewString(), 1); e != nil {
			t.Fatal(e)
		}
	}
	mergeOK := func(from, into string, fromRev, intoRev int) {
		t.Helper()
		must(merge(a.reviewer, a, from, into, uuid.NewString(), `"`+strconv.Itoa(fromRev)+`"`, fromRev, intoRev, "same press"), 200)
	}

	// Merge A->B, then revise B: B@2 was never reviewed as A.
	revisedTarget := source(a, true, "approved")
	B := fresh()
	mergeOK(revisedTarget.entityID, B, 1, 1)
	resolved("A -> B before revision", revisedTarget, B, revisedTarget.entityID)
	entity(a, "Fresh press renamed", 1, B)
	refused("terminal revised after merge", revisedTarget)

	// Chain A->B->C, then revise C.
	revisedChain := source(a, true, "approved")
	B, C := fresh(), fresh()
	mergeOK(revisedChain.entityID, B, 1, 1)
	mergeOK(B, C, 1, 1)
	resolved("unrevised chain", revisedChain, C, revisedChain.entityID, B)
	entity(a, "Fresh press renamed", 1, C)
	refused("chain terminal revised", revisedChain)

	// Intermediate: A->B@1, B revised to 2, then B@2->C. Nobody reviewed A as B@2.
	revisedMiddle := source(a, true, "approved")
	B, C = fresh(), fresh()
	mergeOK(revisedMiddle.entityID, B, 1, 1)
	entity(a, "Fresh press renamed", 1, B)
	mergeOK(B, C, 2, 1)
	refused("intermediate revised between merges", revisedMiddle)

	// Resolve through a redirect, then revoke the terminal entity's evidence.
	revokedTerminal := source(a, true, "approved")
	B = fresh()
	mergeOK(revokedTerminal.entityID, B, 1, 1)
	resolved("before evidence revocation", revokedTerminal, B, revokedTerminal.entityID)
	revokeEvidence(B)
	refused("terminal evidence revoked", revokedTerminal)

	// PR #119 security review: every entity on the walk, not only the terminal, needs current
	// and unrevoked evidence. Chain A->B->C, then revoke the intermediate B's evidence.
	revokedMiddle := source(a, true, "approved")
	B, C = fresh(), fresh()
	mergeOK(revokedMiddle.entityID, B, 1, 1)
	mergeOK(B, C, 1, 1)
	untouched := source(a, true, "approved")
	B2, C2 := fresh(), fresh()
	mergeOK(untouched.entityID, B2, 1, 1)
	mergeOK(B2, C2, 1, 1)
	resolved("chain before intermediate revocation", revokedMiddle, C, revokedMiddle.entityID, B)
	revokeEvidence(B)
	refused("intermediate evidence revoked", revokedMiddle)
	// Chain A->B->C, then revise the mapped entity A's evidence.
	revisedMapped := source(a, true, "approved")
	B, C = fresh(), fresh()
	mergeOK(revisedMapped.entityID, B, 1, 1)
	mergeOK(B, C, 1, 1)
	resolved("chain before mapped evidence revision", revisedMapped, C, revisedMapped.entityID, B)
	if _, err = ev.CreateOrRevise(ctx, evidence.Actor(ka), evidence.Scope(ks), evidence.Input{SourceID: evidenceOf(revisedMapped.entityID), Kind: "manual", Title: "Synthetic asset identity", Text: "Mapped press, corrected"}, uuid.NewString(), 1); err != nil {
		t.Fatal(err)
	}
	refused("mapped evidence revised", revisedMapped)
	resolved("unaffected chain", untouched, C2, untouched.entityID, B2)

	// WIS-270 review finding: intake must not diverge from resolve. An event keyed to a mapped
	// entity that was merged away, or whose evidence was revoked, is quarantined as
	// UNKNOWN_MAPPING with no entity, never recorded against the identity resolve refuses.
	intake := func(f fixture) (entity sql.NullString, reason sql.NullString) {
		t.Helper()
		var srcRev, mapRev int64
		if e := control.QueryRowContext(ctx, `SELECT r.revision,r.mapping_revision FROM supervisor.operational_source_heads h JOIN supervisor.operational_source_revisions r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=(h.tenant_id,h.site_id,h.source_id,h.current_revision) WHERE h.source_id=$1`, f.sourceID).Scan(&srcRev, &mapRev); e != nil {
			t.Fatal(e)
		}
		batch := `{"schema_version":1,"source_id":"` + f.sourceID + `","source_revision":` + strconv.FormatInt(srcRev, 10) + `,"mapping_revision":` + strconv.FormatInt(mapRev, 10) + `,"events":[{"external_key":"delta-1","revision":1,"kind":"production_delta","source_sequence":1,"event_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","asset_external_key":"Press-01","payload":{"good_units":"1"}}]}`
		must(call(a.owner, "POST", "/v1/sites/"+a.site+"/operational-sources/"+f.sourceID+"/batches", uuid.NewString(), "", batch), 202)
		if e := control.QueryRowContext(ctx, `SELECT asset_entity_id::text,quarantine_reason FROM supervisor.operational_event_records WHERE source_id=$1 AND source_sequence=1`, f.sourceID).Scan(&entity, &reason); e != nil {
			t.Fatal(e)
		}
		return entity, reason
	}
	direct := source(a, true, "approved")
	if entity, reason := intake(direct); entity.String != direct.entityID || reason.Valid {
		t.Fatalf("unmerged mapping: entity=%v reason=%v", entity, reason)
	}
	mergedAway := source(a, true, "approved")
	mergeOK(mergedAway.entityID, fresh(), 1, 1)
	if entity, reason := intake(mergedAway); entity.Valid || reason.String != "UNKNOWN_MAPPING" {
		t.Fatalf("merged mapping: entity=%v reason=%v", entity, reason)
	}
	revokedMapped := source(a, true, "approved")
	revokeEvidence(revokedMapped.entityID)
	if entity, reason := intake(revokedMapped); entity.Valid || reason.String != "UNKNOWN_MAPPING" {
		t.Fatalf("mapping with revoked evidence: entity=%v reason=%v", entity, reason)
	}

	// MergeEntity applies the review-decision evidence gate to both sides: revoked evidence is
	// 404 NOT_FOUND (as for claim reviews), revised evidence 409 SOURCE_CHANGED; nothing is written.
	from, into2 := fresh(), fresh()
	revokeEvidence(from)
	problemIs("merge from revoked evidence", merge(a.reviewer, a, from, into2, uuid.NewString(), `"1"`, 1, 1, "same press"), 404, "NOT_FOUND")
	from, into2 = fresh(), fresh()
	revokeEvidence(into2)
	problemIs("merge into revoked evidence", merge(a.reviewer, a, from, into2, uuid.NewString(), `"1"`, 1, 1, "same press"), 404, "NOT_FOUND")
	from, into2 = fresh(), fresh()
	if _, err = ev.CreateOrRevise(ctx, evidence.Actor(ka), evidence.Scope(ks), evidence.Input{SourceID: evidenceOf(into2), Kind: "manual", Title: "Synthetic asset identity", Text: "Fresh press, corrected"}, uuid.NewString(), 1); err != nil {
		t.Fatal(err)
	}
	problemIs("merge into revised evidence", merge(a.reviewer, a, from, into2, uuid.NewString(), `"1"`, 1, 1, "same press"), 409, "SOURCE_CHANGED")
	var refusedMerges int
	if err = control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_entity_redirects WHERE tenant_id=$1 AND from_entity_id=$2`, a.tenant, from).Scan(&refusedMerges); err != nil || refusedMerges != 0 {
		t.Fatalf("refused merge wrote %d redirects, err=%v", refusedMerges, err)
	}
}
