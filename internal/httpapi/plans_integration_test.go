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
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/production"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestPlanApprovalHTTPPostgres proves the WIS-272 authoritative plan bootstrap end to end:
// capture with provenance, supervisor-only distinct-human review over an exact digest, and
// intake admitting a production_plan only through a current, matching, unrevoked approval.
func TestPlanApprovalHTTPPostgres(t *testing.T) {
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
	database := "plan_http_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, planner, reviewer, secondReviewer, staleReviewer, viewer, operator, outsider := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, origin := "https://plan-http.invalid/"+uuid.NewString(), "http://127.0.0.1:58177"
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic plan test'),($2,'other tenant')`, tenant, otherTenant)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'plan site'),($3,$4,'other site')`, tenant, site, otherTenant, otherSite)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner'),($1,$2,$3,$5,'supervisor'),($1,$2,$3,$6,'supervisor'),($1,$2,$3,$7,'supervisor'),($1,$2,$3,$8,'supervisor'),($1,$2,$3,$9,'viewer'),($1,$2,$3,$10,'operator')`, tenant, site, issuer, owner, planner, reviewer, secondReviewer, staleReviewer, viewer, operator)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'supervisor')`, otherTenant, otherSite, issuer, outsider)
	type session struct{ token, csrf, hash, subject string }
	seed := func(subject string, authAge time.Duration) session {
		t.Helper()
		v := session{token: uuid.NewString(), csrf: uuid.NewString(), subject: subject}
		sum := sha256.Sum256([]byte(v.token))
		v.hash = hex.EncodeToString(sum[:])
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now()-$5::interval)`, v.hash, subject, issuer, v.csrf, strconv.Itoa(int(authAge.Seconds()))+" seconds")
		return v
	}
	ownerS, plannerS, reviewerS, secondS, viewerS, operatorS, outsiderS := seed(owner, 0), seed(planner, 0), seed(reviewer, 0), seed(secondReviewer, 0), seed(viewer, 0), seed(operator, 0), seed(outsider, 0)
	staleS := seed(staleReviewer, 20*time.Minute)
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "plan-test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, runtime, nil)
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
	expect := func(w *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("HTTP %d want %d: %s", w.Code, status, w.Body.String())
		}
		if code != "" && !strings.Contains(w.Body.String(), `"code":"`+code+`"`) {
			t.Fatalf("problem code want %s: %s", code, w.Body.String())
		}
	}

	// Provenance and entities: a planner's schedule document with one span, an asset
	// and an order, plus a second asset that a different external key maps to.
	ev := evidence.New(runtime)
	ea := evidence.Actor{Issuer: issuer, Subject: owner, SessionHash: ownerS.hash}
	es := evidence.Scope{TenantID: tenant, SiteID: site}
	schedule, err := ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Shift plan", Text: "Order 7 on Press 1: 1 unit per second, break after 100 seconds"}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	span, err := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: schedule.SourceID, Revision: schedule.Revision, Start: 0, End: 30}, uuid.NewString(), schedule.StateRevision)
	if err != nil {
		t.Fatal(err)
	}
	kn := knowledge.New(runtime)
	ka := knowledge.Actor{Issuer: issuer, Subject: owner, SessionHash: ownerS.hash}
	ks := knowledge.Scope{TenantID: tenant, SiteID: site}
	// Entities cite their own registry document, not the schedule: the schedule's revision and
	// revocation below test plan provenance, and intake also fails a mapping closed when the
	// mapped entity's own evidence is revoked (WIS-270).
	registry, err := ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Asset and order register", Text: "Press 1, Press 2, Order 7, Order 8"}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	entity := func(kind, assetKind, name string) knowledge.Entity {
		t.Helper()
		e, err := kn.CreateEntity(ctx, ka, ks, knowledge.EntityInput{EntityID: uuid.NewString(), Type: kind, AssetKind: assetKind, Name: name, SourceID: registry.SourceID, SourceRevision: registry.Revision}, uuid.NewString(), 0)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	asset, otherAsset, order := entity("asset", "machine", "Press 1"), entity("asset", "machine", "Press 2"), entity("order", "", "Order 7")
	order8 := entity("order", "", "Order 8")
	// A second, independent schedule document for the supersession scenario.
	schedule2, err := ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Order 8 plan", Text: "Order 8 on Press 1: revised rates for the second shift"}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	span2, err := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: schedule2.SourceID, Revision: schedule2.Revision, Start: 0, End: 20}, uuid.NewString(), schedule2.StateRevision)
	if err != nil {
		t.Fatal(err)
	}

	planBody := production.PlanBody{
		OrderID: order.EntityID, AssetID: asset.EntityID, WindowStart: "2026-09-27T00:00:00Z", WindowEnd: "2026-09-27T00:30:00Z",
		PlanRevision: 1, Unit: "count",
		RateSegments:  []production.RateSegment{{Start: "2026-09-27T00:00:00Z", End: "2026-09-27T00:30:00Z", TargetGoodUnitsPerSecond: "1"}},
		PlannedBreaks: []production.Interval{{Start: "2026-09-27T00:01:40Z", End: "2026-09-27T00:15:00Z"}, {Start: "2026-09-27T00:16:40Z", End: "2026-09-27T00:30:00Z"}},
	}
	captureBody := func(p production.PlanBody, spans ...string) string {
		t.Helper()
		b, err := json.Marshal(map[string]any{"plan": p, "source_span_ids": spans})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	base := "/v1/sites/" + site + "/plan-revisions"

	// Capture: authentication, CSRF, headers, media type and strict contract.
	expect(call(session{}, "POST", base, "c", "", captureBody(planBody, span.ID)), 401, "AUTHENTICATION_REQUIRED")
	expect(call(session{token: ownerS.token, csrf: "wrong"}, "POST", base, "c", "", captureBody(planBody, span.ID)), 403, "CSRF_REJECTED")
	expect(call(ownerS, "POST", base, "", "", captureBody(planBody, span.ID)), 422, "INVALID_REQUEST")
	expect(call(ownerS, "POST", base+"?x=1", "c", "", captureBody(planBody, span.ID)), 422, "INVALID_REQUEST")
	smuggled := strings.Replace(captureBody(planBody, span.ID), `"unit":"count"`, `"unit":"count","approval_ref":"`+uuid.NewString()+`"`, 1)
	expect(call(ownerS, "POST", base, "c", "", smuggled), 422, "INVALID_PLAN")
	scoped := strings.Replace(captureBody(planBody, span.ID), `{"plan":`, `{"tenant_id":"`+otherTenant+`","plan":`, 1)
	expect(call(ownerS, "POST", base, "c", "", scoped), 422, "INVALID_PLAN")
	expect(call(ownerS, "POST", base, "c", "", captureBody(planBody)), 422, "INVALID_PLAN")
	expect(call(ownerS, "POST", base, "c", "", captureBody(planBody, uuid.NewString())), 422, "UNKNOWN_SOURCE_SPAN")
	swapped := planBody
	swapped.AssetID, swapped.OrderID = order.EntityID, asset.EntityID
	expect(call(ownerS, "POST", base, "c", "", captureBody(swapped, span.ID)), 422, "UNKNOWN_ENTITY")
	// One entity cannot be both the order and the asset.
	sameEntity := planBody
	sameEntity.OrderID = asset.EntityID
	expect(call(ownerS, "POST", base, "c", "", captureBody(sameEntity, span.ID)), 422, "UNKNOWN_ENTITY")
	expect(call(viewerS, "POST", base, "c", "", captureBody(planBody, span.ID)), 403, "PERMISSION_DENIED")
	expect(call(operatorS, "POST", base, "c", "", captureBody(planBody, span.ID)), 403, "PERMISSION_DENIED")
	expect(call(outsiderS, "POST", base, "c", "", captureBody(planBody, span.ID)), 404, "NOT_FOUND")
	gap := planBody
	gap.PlanRevision = 2
	expect(call(ownerS, "POST", base, "gap", "", captureBody(gap, span.ID)), 409, "PLAN_REVISION_GAP")

	type approvalView struct {
		ID             string                           `json:"id"`
		PlanRevisionID string                           `json:"plan_revision_id"`
		ReviewRevision int64                            `json:"review_revision"`
		Digest         string                           `json:"canonical_payload_digest"`
		Decision       string                           `json:"decision"`
		Reviewer       struct{ Issuer, Subject string } `json:"reviewer_actor_id"`
		PolicyRevision int64                            `json:"policy_revision"`
		Spans          []string                         `json:"source_span_ids"`
		Replayed       bool                             `json:"replayed"`
	}
	type revisionView struct {
		ID              string         `json:"id"`
		Digest          string         `json:"canonical_payload_digest"`
		Spans           []string       `json:"source_span_ids"`
		ReviewRevision  int64          `json:"review_revision"`
		CurrentDecision *approvalView  `json:"current_decision"`
		Decisions       []approvalView `json:"decisions"`
		Superseded      bool           `json:"superseded"`
		Approved        bool           `json:"approved"`
		Replayed        bool           `json:"replayed"`
	}
	decodeRevision := func(w *httptest.ResponseRecorder) revisionView {
		t.Helper()
		var v revisionView
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatalf("%v: %s", err, w.Body.String())
		}
		return v
	}
	decodeApproval := func(w *httptest.ResponseRecorder) approvalView {
		t.Helper()
		var v approvalView
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatalf("%v: %s", err, w.Body.String())
		}
		return v
	}

	captured := call(plannerS, "POST", base, "capture-1", "", captureBody(planBody, span.ID))
	expect(captured, 201, "")
	plan := decodeRevision(captured)
	_, wantDigest, err := production.CanonicalPlanPayload(tenant, site, planBody)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Digest != wantDigest || plan.ReviewRevision != 0 || plan.Approved || plan.CurrentDecision != nil || len(plan.Decisions) != 0 || len(plan.Spans) != 1 || plan.Spans[0] != span.ID || captured.Header().Get("ETag") != `"0"` {
		t.Fatalf("captured plan=%s etag=%s", captured.Body.String(), captured.Header().Get("ETag"))
	}
	replayed := decodeRevision(call(plannerS, "POST", base, "capture-1", "", captureBody(planBody, span.ID)))
	if replayed.ID != plan.ID || !replayed.Replayed {
		t.Fatalf("same-key capture did not replay: %+v", replayed)
	}
	changedRate := planBody
	changedRate.RateSegments = []production.RateSegment{{Start: "2026-09-27T00:00:00Z", End: "2026-09-27T00:30:00Z", TargetGoodUnitsPerSecond: "2"}}
	expect(call(plannerS, "POST", base, "capture-1", "", captureBody(changedRate, span.ID)), 409, "IDEMPOTENCY_CONFLICT")
	if again := decodeRevision(call(plannerS, "POST", base, "capture-1b", "", captureBody(planBody, span.ID))); again.ID != plan.ID || !again.Replayed {
		t.Fatalf("identical recapture under a new key must return the same revision: %+v", again)
	}
	expect(call(plannerS, "POST", base, "capture-conflict", "", captureBody(changedRate, span.ID)), 409, "PLAN_REVISION_CONFLICT")
	// Another person re-capturing identical content is not a replay: it would make them an
	// unrecorded co-submitter able to approve the plan.
	expect(call(secondS, "POST", base, "capture-other-actor", "", captureBody(planBody, span.ID)), 409, "PLAN_REVISION_CONFLICT")

	// Review: supervisor only, distinct human, recent authentication, If-Match preconditions.
	reviews := base + "/" + plan.ID + "/reviews"
	approve := `{"decision":"approve","reason":"Rates and breaks match the shift plan"}`
	expect(call(ownerS, "POST", reviews, "r-owner", `"0"`, approve), 403, "PERMISSION_DENIED")
	expect(call(operatorS, "POST", reviews, "r-op", `"0"`, approve), 403, "PERMISSION_DENIED")
	expect(call(plannerS, "POST", reviews, "r-self", `"0"`, approve), 403, "SELF_REVIEW_FORBIDDEN")
	expect(call(staleS, "POST", reviews, "r-stale", `"0"`, approve), 428, "REAUTHENTICATION_REQUIRED")
	expect(call(reviewerS, "POST", reviews, "r-nomatch", "", approve), 428, "REVISION_REQUIRED")
	expect(call(reviewerS, "POST", reviews, "r-future", `"1"`, approve), 412, "REVISION_CONFLICT")
	expect(call(reviewerS, "POST", reviews, "r-bad", `"0"`, `{"decision":"approved","reason":"x"}`), 422, "INVALID_PLAN")
	expect(call(reviewerS, "POST", base+"/"+uuid.NewString()+"/reviews", "r-missing", `"0"`, approve), 404, "NOT_FOUND")
	approvedW := call(reviewerS, "POST", reviews, "r-1", `"0"`, approve)
	expect(approvedW, 200, "")
	first := decodeApproval(approvedW)
	if first.PlanRevisionID != plan.ID || first.ReviewRevision != 1 || first.Digest != wantDigest || first.Decision != "approve" || first.Reviewer.Subject != reviewer || first.PolicyRevision != 1 || len(first.Spans) != 1 || first.Spans[0] != span.ID || approvedW.Header().Get("ETag") != `"1"` {
		t.Fatalf("approval=%s", approvedW.Body.String())
	}
	if again := decodeApproval(call(reviewerS, "POST", reviews, "r-1", `"0"`, approve)); again.ID != first.ID || !again.Replayed {
		t.Fatalf("same-key review did not replay: %+v", again)
	}
	expect(call(reviewerS, "POST", reviews, "r-1", `"0"`, `{"decision":"reject","reason":"changed mind"}`), 409, "IDEMPOTENCY_CONFLICT")
	expect(call(secondS, "POST", reviews, "r-stale-match", `"0"`, approve), 412, "REVISION_CONFLICT")
	read := call(viewerS, "GET", base+"/"+plan.ID, "", "", "")
	expect(read, 200, "")
	if v := decodeRevision(read); !v.Approved || v.ReviewRevision != 1 || v.CurrentDecision == nil || v.CurrentDecision.ID != first.ID || len(v.Decisions) != 1 || read.Header().Get("ETag") != `"1"` || read.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("plan read=%s", read.Body.String())
	}
	expect(call(viewerS, "GET", base+"/"+plan.ID+"?revision=1", "", "", ""), 422, "INVALID_REQUEST")
	expect(call(outsiderS, "GET", "/v1/sites/"+otherSite+"/plan-revisions/"+plan.ID, "", "", ""), 404, "NOT_FOUND")
	expect(call(viewerS, "GET", base+"/"+uuid.NewString(), "", "", ""), 404, "NOT_FOUND")

	// Intake: an approved operational source mapped to the asset, a second asset key and the order.
	sourceID := uuid.NewString()
	sources := "/v1/sites/" + site + "/operational-sources"
	expect(call(ownerS, "POST", sources, "src", `"0"`, `{"source_id":"`+sourceID+`","type":"event_api","authority_scope":"production_plan","enabled":true,"access_policy_revision":1,"freshness_seconds":120,"clock_skew_seconds":30}`), 200, "")
	mappings, err := json.Marshal(map[string]any{"mappings": []map[string]any{
		{"external_type": "asset", "external_key": "press-01", "entity_id": asset.EntityID, "entity_revision": asset.Revision},
		{"external_type": "asset", "external_key": "press-02", "entity_id": otherAsset.EntityID, "entity_revision": otherAsset.Revision},
		{"external_type": "order", "external_key": "order-7", "entity_id": order.EntityID, "entity_revision": order.Revision},
		{"external_type": "order", "external_key": "order-8", "entity_id": order8.EntityID, "entity_revision": order8.Revision},
	}})
	if err != nil {
		t.Fatal(err)
	}
	expect(call(ownerS, "POST", sources+"/"+sourceID+"/mappings", "map", `"1"`, string(mappings)), 200, "")
	expect(call(reviewerS, "POST", sources+"/"+sourceID+"/reviews", "src-review", `"2"`, `{"verdict":"approved","reason":"Scope verified"}`), 200, "")
	// A telemetry-only source with identical mappings: its reviewed scope excludes plans.
	telemetryID := uuid.NewString()
	expect(call(ownerS, "POST", sources, "telemetry", `"0"`, `{"source_id":"`+telemetryID+`","type":"event_api","authority_scope":"machine_state,production_delta","enabled":true,"access_policy_revision":1,"freshness_seconds":120,"clock_skew_seconds":30}`), 200, "")
	expect(call(ownerS, "POST", sources+"/"+telemetryID+"/mappings", "telemetry-map", `"1"`, string(mappings)), 200, "")
	expect(call(reviewerS, "POST", sources+"/"+telemetryID+"/reviews", "telemetry-review", `"2"`, `{"verdict":"approved","reason":"Telemetry scope verified"}`), 200, "")
	sequences := map[string]int64{}
	eventAt := time.Now().UTC().Format(time.RFC3339Nano)
	submitVia := func(src, key string, payload production.Plan, assetKey, orderKey string) {
		t.Helper()
		sequences[src]++
		sequence := sequences[src]
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		event := map[string]any{"external_key": key, "revision": 1, "kind": "production_plan", "source_sequence": sequence, "event_at": eventAt, "payload": json.RawMessage(raw)}
		if assetKey != "" {
			event["asset_external_key"] = assetKey
		}
		if orderKey != "" {
			event["order_external_key"] = orderKey
		}
		body, err := json.Marshal(map[string]any{"schema_version": 1, "source_id": src, "source_revision": 2, "mapping_revision": 2, "events": []any{event}})
		if err != nil {
			t.Fatal(err)
		}
		expect(call(operatorS, "POST", sources+"/"+src+"/batches", "batch-"+key, "", string(body)), 202, "")
	}
	submit := func(key string, payload production.Plan, assetKey, orderKey string) {
		t.Helper()
		submitVia(sourceID, key, payload, assetKey, orderKey)
	}
	intake := sources + "/" + sourceID + "/batches"
	outcomeIn := func(src, key string) (string, string) {
		t.Helper()
		var reason, approval sql.NullString
		tx, err := control.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
			t.Fatal(err)
		}
		if err = tx.QueryRowContext(ctx, `SELECT quarantine_reason,plan_approval_id FROM supervisor.operational_event_records WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND external_key=$4`, tenant, site, src, key).Scan(&reason, &approval); err != nil {
			t.Fatal(err)
		}
		return reason.String, approval.String
	}
	outcome := func(key string) (string, string) { t.Helper(); return outcomeIn(sourceID, key) }
	// eligibility re-evaluates an already stored plan record now, through the runtime role,
	// exactly as later consumers must; the stored admission is only a durable receipt.
	eligibility := func(key string) string {
		t.Helper()
		tx, err := runtime.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
			t.Fatal(err)
		}
		var sequence int64
		if err = tx.QueryRowContext(ctx, `SELECT source_sequence FROM supervisor.operational_event_records WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND external_key=$4`, tenant, site, sourceID, key).Scan(&sequence); err != nil {
			t.Fatal(err)
		}
		reason, err := ingestion.PlanRecordEligibility(ctx, tx, ingestion.Scope{TenantID: tenant, SiteID: site}, sourceID, sequence)
		if err != nil {
			t.Fatal(err)
		}
		return reason
	}
	admitted := planBody.WithApproval(first.ID)
	submit("plan-ok", admitted, "press-01", "order-7")
	if reason, approval := outcome("plan-ok"); reason != "" || approval != first.ID {
		t.Fatalf("approved plan not admitted: reason=%q approval=%q", reason, approval)
	}
	// A valid approval ID paired with altered rates never admits.
	submit("plan-tampered", changedRate.WithApproval(first.ID), "press-01", "order-7")
	if reason, _ := outcome("plan-tampered"); reason != "UNAPPROVED_PLAN" {
		t.Fatalf("tampered plan reason=%q", reason)
	}
	// Source A posting asset B: the payload's asset is not what the event's key maps to.
	// Source A posting asset B: the payload names Press 2 but the event's key maps to Press 1.
	assetB := planBody
	assetB.AssetID = otherAsset.EntityID
	submit("plan-asset-b", assetB.WithApproval(first.ID), "press-01", "order-7")
	if reason, _ := outcome("plan-asset-b"); reason != "MAPPING_MISMATCH" {
		t.Fatalf("source posting another asset reason=%q", reason)
	}
	submit("plan-other-asset", admitted, "press-02", "order-7")
	if reason, _ := outcome("plan-other-asset"); reason != "MAPPING_MISMATCH" {
		t.Fatalf("cross-asset plan reason=%q", reason)
	}
	submit("plan-no-order", admitted, "press-01", "")
	if reason, _ := outcome("plan-no-order"); reason != "UNKNOWN_MAPPING" {
		t.Fatalf("plan without order key reason=%q", reason)
	}
	submit("plan-unmapped", admitted, "press-99", "order-7")
	if reason, _ := outcome("plan-unmapped"); reason != "UNKNOWN_MAPPING" {
		t.Fatalf("unmapped plan reason=%q", reason)
	}
	submit("plan-fabricated", planBody.WithApproval(uuid.NewString()), "press-01", "order-7")
	if reason, _ := outcome("plan-fabricated"); reason != "UNAPPROVED_PLAN" {
		t.Fatalf("fabricated approval reason=%q", reason)
	}
	// A malformed plan is rejected before persistence and consumes no sequence.
	malformed := `{"schema_version":1,"source_id":"` + sourceID + `","source_revision":2,"mapping_revision":2,"events":[{"external_key":"plan-bad","revision":1,"kind":"production_plan","source_sequence":` + strconv.FormatInt(sequences[sourceID]+1, 10) + `,"event_at":"` + eventAt + `","asset_external_key":"press-01","order_external_key":"order-7","payload":{"approved":true}}]}`
	expect(call(operatorS, "POST", intake, "batch-malformed", "", malformed), 422, "INVALID_EVENT_BATCH")

	// Rejection supersedes the approval: the old approval_ref stops admitting immediately.
	rejected := decodeApproval(call(secondS, "POST", reviews, "r-2", `"1"`, `{"decision":"reject","reason":"Break schedule withdrawn"}`))
	if rejected.ReviewRevision != 2 || rejected.Decision != "reject" {
		t.Fatalf("rejection=%+v", rejected)
	}
	// Records admitted before the rejection are no longer eligible on re-evaluation.
	if reason := eligibility("plan-ok"); reason != "UNAPPROVED_PLAN" {
		t.Fatalf("previously admitted plan stayed eligible after rejection: %q", reason)
	}
	submit("plan-after-reject", admitted, "press-01", "order-7")
	if reason, _ := outcome("plan-after-reject"); reason != "UNAPPROVED_PLAN" {
		t.Fatalf("plan citing a superseded approval reason=%q", reason)
	}
	reapproved := decodeApproval(call(reviewerS, "POST", reviews, "r-3", `"2"`, approve))
	submit("plan-old-ref", admitted, "press-01", "order-7")
	if reason, _ := outcome("plan-old-ref"); reason != "UNAPPROVED_PLAN" {
		t.Fatalf("old approval_ref after re-approval reason=%q", reason)
	}
	submit("plan-new-ref", planBody.WithApproval(reapproved.ID), "press-01", "order-7")
	if reason, approval := outcome("plan-new-ref"); reason != "" || approval != reapproved.ID {
		t.Fatalf("re-approved plan reason=%q approval=%q", reason, approval)
	}
	if eligibility("plan-new-ref") != "" || eligibility("plan-ok") != "UNAPPROVED_PLAN" {
		t.Fatal("re-approval must make only records citing the new approval eligible")
	}
	history := decodeRevision(call(viewerS, "GET", base+"/"+plan.ID, "", "", ""))
	if len(history.Decisions) != 3 || history.Decisions[0].Decision != "approve" || history.Decisions[1].Decision != "reject" || history.Decisions[2].ID != reapproved.ID || !history.Approved {
		t.Fatalf("decision history=%+v", history)
	}

	// Revising the cited evidence does not withdraw an existing approval (only revocation
	// does, identically in reads and intake), but new approvals must cite current provenance.
	if _, err = ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: schedule.SourceID, Kind: "manual", Title: "Shift plan", Text: "Order 7 on Press 1: revised commentary"}, uuid.NewString(), schedule.Revision); err != nil {
		t.Fatal(err)
	}
	if v := decodeRevision(call(viewerS, "GET", base+"/"+plan.ID, "", "", "")); !v.Approved {
		t.Fatal("a revised (not revoked) source withdrew an existing approval on read")
	}
	submit("plan-after-revision", planBody.WithApproval(reapproved.ID), "press-01", "order-7")
	if reason, approval := outcome("plan-after-revision"); reason != "" || approval != reapproved.ID {
		t.Fatalf("read and intake disagree after a source revision: reason=%q approval=%q", reason, approval)
	}
	expect(call(secondS, "POST", reviews, "r-stale-span", `"3"`, approve), 409, "SOURCE_SPAN_UNAVAILABLE")

	// Revoking the cited evidence removes eligibility on every authoritative check.
	current, err := ev.Read(ctx, ea, es, schedule.SourceID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ev.Revoke(ctx, ea, es, schedule.SourceID, "Shift plan withdrawn", uuid.NewString(), current.StateRevision); err != nil {
		t.Fatal(err)
	}
	if v := decodeRevision(call(viewerS, "GET", base+"/"+plan.ID, "", "", "")); v.Approved {
		t.Fatal("plan stayed approved after its provenance was revoked")
	}
	submit("plan-revoked", planBody.WithApproval(reapproved.ID), "press-01", "order-7")
	if reason, _ := outcome("plan-revoked"); reason != "UNAPPROVED_PLAN" {
		t.Fatalf("plan with revoked provenance reason=%q", reason)
	}
	expect(call(secondS, "POST", reviews, "r-4", `"3"`, approve), 409, "SOURCE_SPAN_UNAVAILABLE")
	expect(call(secondS, "POST", reviews, "r-5", `"3"`, `{"decision":"reject","reason":"Provenance revoked"}`), 200, "")
	expect(call(ownerS, "POST", base, "capture-revoked", "", captureBody(gap, span.ID)), 409, "SOURCE_SPAN_UNAVAILABLE")
	if reason := eligibility("plan-new-ref"); reason != "UNAPPROVED_PLAN" {
		t.Fatalf("admitted plan stayed eligible after its provenance was revoked: %q", reason)
	}

	// Supersession: once revision 2 exists for an order/asset, revision 1 is contested
	// and stops being admissible or eligible, whether or not revision 2 is approved yet.
	order8Plan := planBody
	order8Plan.OrderID = order8.EntityID
	o8 := decodeRevision(call(plannerS, "POST", base, "o8-r1", "", captureBody(order8Plan, span2.ID)))
	o8Approval := decodeApproval(call(reviewerS, "POST", base+"/"+o8.ID+"/reviews", "o8-approve", `"0"`, approve))
	submit("o8-r1-ok", order8Plan.WithApproval(o8Approval.ID), "press-01", "order-8")
	if reason, _ := outcome("o8-r1-ok"); reason != "" {
		t.Fatalf("order 8 revision 1 not admitted: %q", reason)
	}
	// The same approved plan through a source whose reviewed scope excludes plans.
	submitVia(telemetryID, "o8-telemetry", order8Plan.WithApproval(o8Approval.ID), "press-01", "order-8")
	if reason, _ := outcomeIn(telemetryID, "o8-telemetry"); reason != "OUT_OF_SCOPE" {
		t.Fatalf("telemetry source self-expanded into plan authority: reason=%q", reason)
	}
	order8Rev2 := order8Plan
	order8Rev2.PlanRevision = 2
	order8Rev2.RateSegments = []production.RateSegment{{Start: "2026-09-27T00:00:00Z", End: "2026-09-27T00:30:00Z", TargetGoodUnitsPerSecond: "2"}}
	o8r2 := decodeRevision(call(plannerS, "POST", base, "o8-r2", "", captureBody(order8Rev2, span2.ID)))
	if v := decodeRevision(call(viewerS, "GET", base+"/"+o8.ID, "", "", "")); !v.Superseded || v.Approved {
		t.Fatalf("revision 1 not superseded on read: %+v", v)
	}
	submit("o8-r1-stale", order8Plan.WithApproval(o8Approval.ID), "press-01", "order-8")
	if reason, _ := outcome("o8-r1-stale"); reason != "SUPERSEDED_PLAN" {
		t.Fatalf("superseded revision admitted: reason=%q", reason)
	}
	if reason := eligibility("o8-r1-ok"); reason != "SUPERSEDED_PLAN" {
		t.Fatalf("previously admitted superseded revision stayed eligible: %q", reason)
	}
	expect(call(secondS, "POST", base+"/"+o8.ID+"/reviews", "o8-reapprove", `"1"`, approve), 409, "PLAN_SUPERSEDED")
	o8r2Approval := decodeApproval(call(reviewerS, "POST", base+"/"+o8r2.ID+"/reviews", "o8-r2-approve", `"0"`, approve))
	submit("o8-r2-ok", order8Rev2.WithApproval(o8r2Approval.ID), "press-01", "order-8")
	if reason, approval := outcome("o8-r2-ok"); reason != "" || approval != o8r2Approval.ID || eligibility("o8-r2-ok") != "" {
		t.Fatalf("latest approved revision not admitted: reason=%q approval=%q", reason, approval)
	}

	// Database invariants hold even for direct SQL under the runtime role.
	tx, err := runtime.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		t.Fatal(err)
	}
	for name, q := range map[string]string{
		"approval update": `UPDATE supervisor.plan_approvals SET decision='approve' WHERE tenant_id='` + tenant + `'`,
		"approval delete": `DELETE FROM supervisor.plan_approvals WHERE tenant_id='` + tenant + `'`,
		"revision update": `UPDATE supervisor.plan_revisions SET plan_revision=9 WHERE tenant_id='` + tenant + `'`,
		"head skip":       `UPDATE supervisor.plan_review_heads SET current_revision=current_revision+2 WHERE tenant_id='` + tenant + `'`,
	} {
		if _, e := tx.ExecContext(ctx, "SAVEPOINT s"); e != nil {
			t.Fatal(e)
		}
		if _, e := tx.ExecContext(ctx, q); e == nil {
			t.Fatalf("%s was permitted", name)
		}
		if _, e := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT s"); e != nil {
			t.Fatal(e)
		}
	}
	if _, err = tx.ExecContext(ctx, "SAVEPOINT s"); err != nil {
		t.Fatal(err)
	}
	if _, e := tx.ExecContext(ctx, `UPDATE supervisor.plan_review_heads SET current_revision=current_revision+1 WHERE tenant_id=$1 AND plan_revision_id=$2`, tenant, plan.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := tx.ExecContext(ctx, `INSERT INTO supervisor.plan_approvals(tenant_id,site_id,id,plan_revision_id,review_revision,canonical_payload_sha256,decision,reason,reviewer_issuer,reviewer_subject,policy_revision,auth_time) VALUES($1,$2,$3,$4,5,$5,'approve','self',$6,$7,1,now())`, tenant, site, uuid.NewString(), plan.ID, wantDigest, issuer, planner); e == nil || !strings.Contains(e.Error(), "differ from submitter") {
		t.Fatalf("database allowed a submitter to approve their own plan: %v", e)
	}
	if _, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT s"); err != nil {
		t.Fatal(err)
	}
	var visible int
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, otherTenant); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM supervisor.plan_revisions)+(SELECT count(*) FROM supervisor.plan_approvals)+(SELECT count(*) FROM supervisor.plan_audit)`).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("cross-tenant plan rows visible: %d err=%v", visible, err)
	}
	var audits int
	if err = control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.plan_audit WHERE tenant_id=$1 AND event_type IN ('plan.captured','review.decided','mutation.replayed','plan.read')`, tenant).Scan(&audits); err != nil || audits < 10 {
		t.Fatalf("plan audit trail too thin: %d err=%v", audits, err)
	}
}
