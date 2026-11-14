//go:build integration

package httpapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/contracts"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/oee"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// conforms checks a JSON value against a generated contract schema: closed objects, required
// properties, types, enums, patterns and lengths. It is the provider half of the WIS-229
// "OpenAPI provider compatibility" acceptance item.
func conforms(schema map[string]any, value any, path string) error {
	if anyOf, ok := schema["anyOf"].([]any); ok {
		for _, alt := range anyOf {
			if conforms(alt.(map[string]any), value, path) == nil {
				return nil
			}
		}
		return fmt.Errorf("%s matches no alternative", path)
	}
	switch schema["type"] {
	case "null":
		if value != nil {
			return fmt.Errorf("%s want null", path)
		}
	case "object":
		obj, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s want object, got %T", path, value)
		}
		props, _ := schema["properties"].(map[string]any)
		for k, v := range obj {
			p, ok := props[k]
			if !ok {
				return fmt.Errorf("%s.%s is not in the published contract", path, k)
			}
			if err := conforms(p.(map[string]any), v, path+"."+k); err != nil {
				return err
			}
		}
		for _, r := range schema["required"].([]any) {
			if _, ok := obj[r.(string)]; !ok {
				return fmt.Errorf("%s.%s is required by the contract but missing", path, r)
			}
		}
	case "array":
		arr, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s want array, got %T", path, value)
		}
		for i, v := range arr {
			if err := conforms(schema["items"].(map[string]any), v, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case "string":
		s, ok := value.(string)
		if !ok {
			return fmt.Errorf("%s want string, got %T", path, value)
		}
		if enum, ok := schema["enum"].([]any); ok {
			found := false
			for _, e := range enum {
				found = found || e == s
			}
			if !found {
				return fmt.Errorf("%s=%q is not a published enum value", path, s)
			}
		}
		if p, ok := schema["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(s) {
			return fmt.Errorf("%s=%q violates %s", path, s, p)
		}
		if m, ok := schema["maxLength"].(float64); ok && len([]rune(s)) > int(m) {
			return fmt.Errorf("%s exceeds maxLength", path)
		}
	case "integer":
		if f, ok := value.(float64); !ok || f != float64(int64(f)) {
			return fmt.Errorf("%s want integer", path)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s want boolean", path)
		}
	case nil: // an open schema (for example json.RawMessage) admits any JSON value
	default:
		return fmt.Errorf("%s: unsupported schema type %v", path, schema["type"])
	}
	return nil
}

// recordProviderFixtures writes schema-validated responses from this run as a TypeScript
// module when SUPERVISOR_PROVIDER_FIXTURE_OUT names the target file. The committed module
// (web/src/contracts/provider-fixtures.ts) is typechecked by tsc against the generated
// consumer types in contracts/typescript/oee.ts: the consumer half of the WIS-229
// "OpenAPI provider and generated-consumer compatibility" acceptance item.
func recordProviderFixtures(t *testing.T, bodies map[string]any) {
	t.Helper()
	out := os.Getenv("SUPERVISOR_PROVIDER_FIXTURE_OUT")
	if out == "" {
		return
	}
	names := make([]string, 0, len(bodies))
	for name := range bodies {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("// Recorded provider responses from TestOEEServiceHTTPPostgres; each was validated against its\n")
	b.WriteString("// published JSON Schema first. Regenerate with SUPERVISOR_PROVIDER_FIXTURE_OUT set. Do not edit.\n")
	b.WriteString("import type { " + strings.Join(names, ", ") + " } from \"../../../contracts/typescript/oee\";\n")
	for _, name := range names {
		data, err := json.MarshalIndent(bodies[name], "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "\nexport const provider%s = %s as const satisfies %s;\n", strings.TrimSuffix(name, "V1"), data, name)
	}
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOEEServiceHTTPPostgres(t *testing.T) {
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
	database := "oee_http_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	tenant, site, otherTenant, otherSite := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	owner, supervisor, viewer, outsider := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, origin := "https://oee-http.invalid/"+uuid.NewString(), "http://127.0.0.1:58177"
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic oee test'),($2,'other tenant')`, tenant, otherTenant)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'oee site'),($3,$4,'other site')`, tenant, site, otherTenant, otherSite)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner'),($1,$2,$3,$5,'supervisor'),($1,$2,$3,$6,'viewer')`, tenant, site, issuer, owner, supervisor, viewer)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'supervisor')`, otherTenant, otherSite, issuer, outsider)
	type session struct{ token, csrf string }
	seed := func(subject string) session {
		t.Helper()
		v := session{token: uuid.NewString(), csrf: uuid.NewString()}
		sum := sha256.Sum256([]byte(v.token))
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, hex.EncodeToString(sum[:]), subject, issuer, v.csrf)
		return v
	}
	ownerS, supervisorS, viewerS, outsiderS := seed(owner), seed(supervisor), seed(viewer), seed(outsider)
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "oee-test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir(), OEERunDeadline: time.Minute, OEELeaseDuration: 15 * time.Second, OEEMaxOpenRuns: 2, OEEMaxConcurrentExecutions: 4, OEEIdempotencyRetention: time.Hour}, db, auth)
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
	files := contracts.Files()
	expect := func(w *httptest.ResponseRecorder, status int, schemaOrCode string) map[string]any {
		t.Helper()
		if w.Code != status {
			t.Fatalf("HTTP %d want %d: %s", w.Code, status, w.Body.String())
		}
		var body any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		name := schemaOrCode
		if status >= 400 {
			if w.Header().Get("Content-Type") != "application/problem+json" || !strings.Contains(w.Body.String(), `"code":"`+schemaOrCode+`"`) {
				t.Fatalf("problem want %s: %s", schemaOrCode, w.Body.String())
			}
			name = "ProblemDetailsV1"
		}
		var schema map[string]any
		if err := json.Unmarshal(files["schemas/"+name+".schema.json"], &schema); err != nil {
			t.Fatalf("schema %s: %v", name, err)
		}
		if err := conforms(schema, body, name); err != nil {
			t.Fatal(err)
		}
		m, _ := body.(map[string]any)
		return m
	}
	readFixture := func(name string) []byte {
		data, e := os.ReadFile(filepath.Join("..", "oee", "testdata", "service", name))
		if e != nil {
			t.Fatal(e)
		}
		return data
	}
	var policyBody oee.OeePolicyBodyV1
	if err = json.Unmarshal(readFixture("policy.json"), &policyBody); err != nil {
		t.Fatal(err)
	}
	var d oee.OeeDatasetV1
	if err = json.Unmarshal(readFixture("dataset_adr0005.json"), &d); err != nil {
		t.Fatal(err)
	}
	if d.ProvenanceDigest, err = oee.DatasetProvenanceDigest(d); err != nil {
		t.Fatal(err)
	}
	marshal := func(v any) string {
		b, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		return string(b)
	}
	base := "/v1/sites/" + site + "/oee"
	policyID := uuid.NewString()
	revision := marshal(oee.OeePolicyRevisionInputV1{PolicyCode: "line-oee", EffectiveStart: "2026-01-01T00:00:00Z", Policy: policyBody})
	request := marshal(oee.OeeCalculationRequestV1{Dataset: d})

	// Rule 07 and ADR 0002 Sec. 8/12: mutations need an Idempotency-Key; hostile tenant,
	// policy, URL and formula fields are unknown fields and rejected.
	expect(call(ownerS, "POST", base+"/policies/"+policyID+"/revisions", "", "", revision), 422, "INVALID_REQUEST")
	for _, hostile := range []string{`"tenant_id":"` + tenant + `"`, `"policy_id":"` + policyID + `"`, `"url":"http://mes.invalid"`, `"formula":"a*p*q"`} {
		body := strings.Replace(request, "{", "{"+hostile+",", 1)
		expect(call(ownerS, "POST", base+"/calculations", "hostile", "", body), 422, "INVALID_REQUEST")
	}
	// No published policy covers the window yet (ADR 0002 Sec. 5 stage 3).
	expect(call(ownerS, "POST", base+"/calculations", "before-policy", "", request), 409, "POLICY_NOT_READY")

	created := expect(call(ownerS, "POST", base+"/policies/"+policyID+"/revisions", "rev-1", "", revision), 201, "OeePolicyV1")
	revisionID := created["revisions"].([]any)[0].(map[string]any)["policy_revision_id"].(string)
	publish := base + "/policies/" + policyID + "/revisions/" + revisionID + ":publish"
	expect(call(supervisorS, "POST", publish, "pub-1", "", ""), 428, "REVISION_REQUIRED")
	expect(call(ownerS, "POST", publish, "pub-1", `"1"`, ""), 403, "PERMISSION_DENIED")
	expect(call(supervisorS, "POST", publish, "pub-1", `"9"`, ""), 412, "REVISION_CONFLICT")
	expect(call(supervisorS, "POST", publish, "pub-1", `"1"`, ""), 200, "OeePolicyV1")
	expect(call(viewerS, "GET", base+"/policies/"+policyID, "", "", ""), 200, "OeePolicyV1")

	// Accept, execute in the background, seal, then read the run and evidence.
	accepted := expect(call(ownerS, "POST", base+"/calculations", "calc-1", "", request), 202, "OeeCalculationRunV1")
	runID := accepted["run_id"].(string)
	var run map[string]any
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		run = expect(call(viewerS, "GET", base+"/calculations/"+runID, "", "", ""), 200, "OeeCalculationRunV1")
		if run["status"] != oee.RunAccepted && run["status"] != oee.RunRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not finish: %v", run)
		}
	}
	if run["status"] != oee.RunSucceeded || run["result_status"] != "READY" || run["truth_state"] != "CANDIDATE" {
		t.Fatalf("run: %v", run)
	}
	evidence := expect(call(viewerS, "GET", base+"/evidence/"+run["evidence_id"].(string), "", "", ""), 200, "OeeCalculationEvidenceV1")
	if evidence["kpis"].(map[string]any)["oee"].(map[string]any)["value"] != "0.711111" {
		t.Fatalf("evidence: %v", evidence)
	}
	policyView := expect(call(viewerS, "GET", base+"/policies/"+policyID, "", "", ""), 200, "OeePolicyV1")
	problemView := expect(call(outsiderS, "GET", base+"/calculations/"+runID, "", "", ""), 404, "NOT_FOUND")
	recordProviderFixtures(t, map[string]any{"OeeCalculationRunV1": run, "OeeCalculationEvidenceV1": evidence, "OeePolicyV1": policyView, "ProblemDetailsV1": problemView})
	// Replay and key reuse (WIS-229 acceptance).
	replayed := expect(call(ownerS, "POST", base+"/calculations", "calc-1", "", request), 202, "OeeCalculationRunV1")
	if replayed["run_id"] != runID || replayed["replayed"] != true {
		t.Fatalf("replay: %v", replayed)
	}
	expect(call(ownerS, "POST", base+"/calculations", "calc-1", "", marshal(oee.OeeCalculationRequestV1{Dataset: d, ExpectedPolicyRevision: 1})), 409, "IDEMPOTENCY_CONFLICT")
	expect(call(ownerS, "POST", base+"/calculations", "stale-policy", "", marshal(oee.OeeCalculationRequestV1{Dataset: d, ExpectedPolicyRevision: 7})), 412, "REVISION_CONFLICT")
	expect(call(ownerS, "POST", base+"/calculations/"+runID+":cancel", "cancel-done", "", ""), 409, "RUN_LIFECYCLE_CONFLICT")
	expect(call(viewerS, "POST", base+"/calculations", "viewer", "", request), 403, "PERMISSION_DENIED")

	// Cross-tenant reads are non-disclosing 404s (rule 03).
	foreign := "/v1/sites/" + otherSite + "/oee"
	expect(call(outsiderS, "GET", foreign+"/calculations/"+runID, "", "", ""), 404, "NOT_FOUND")
	expect(call(outsiderS, "GET", foreign+"/evidence/"+run["evidence_id"].(string), "", "", ""), 404, "NOT_FOUND")
	expect(call(outsiderS, "GET", base+"/calculations/"+runID, "", "", ""), 404, "NOT_FOUND")

	// Overload is a predictable 429 with Retry-After once the open-run budget is held.
	// Two open runs are seeded directly; nothing executes them.
	exec(`INSERT INTO supervisor.oee_calculation_run(tenant_id,site_id,run_id,dataset_id,asset_id,window_start,window_end,dataset_sha256,policy_id,policy_revision_id,policy_sha256,requested_by_issuer,requested_by_subject,request_sha256,status,deadline_at)
SELECT tenant_id,site_id,gen_random_uuid(),dataset_id,asset_id,window_start,window_end,dataset_sha256,policy_id,policy_revision_id,policy_sha256,requested_by_issuer,requested_by_subject,request_sha256,'ACCEPTED',now()+interval '1 minute' FROM supervisor.oee_calculation_run WHERE run_id=$1
UNION ALL SELECT tenant_id,site_id,gen_random_uuid(),dataset_id,asset_id,window_start,window_end,dataset_sha256,policy_id,policy_revision_id,policy_sha256,requested_by_issuer,requested_by_subject,request_sha256,'ACCEPTED',now()+interval '1 minute' FROM supervisor.oee_calculation_run WHERE run_id=$1`, runID)
	w := call(ownerS, "POST", base+"/calculations", "over-budget", "", request)
	expect(w, 429, "CAPACITY_EXHAUSTED")
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
}
