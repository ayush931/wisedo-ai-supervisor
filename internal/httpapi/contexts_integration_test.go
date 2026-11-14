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
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plantcontext"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/production"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestPlantContextHTTPPostgres drives the WIS-272 golden fixture through the real path:
// approved plan -> authenticated intake -> outbox fold -> PlantContextSnapshotV1.
// Rate 1 unit/s with 100 productive seconds per window expects 100; contiguous actual
// 80 gives deficit 20 in two consecutive windows, which qualifies; a correction to 95
// gives deficit 5, which does not; incomplete or offline data is NOT_READY.
func TestPlantContextHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
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
	database := "context_http_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, planner, reviewer, viewer, outsider := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, origin := "https://context-http.invalid/"+uuid.NewString(), "http://127.0.0.1:58177"
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic context test'),($2,'other tenant')`, tenant, otherTenant)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'context site'),($3,$4,'other site')`, tenant, site, otherTenant, otherSite)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner'),($1,$2,$3,$5,'supervisor'),($1,$2,$3,$6,'supervisor'),($1,$2,$3,$7,'viewer')`, tenant, site, issuer, owner, planner, reviewer, viewer)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, otherTenant, otherSite, issuer, outsider)
	type session struct{ token, csrf, hash string }
	seed := func(subject string) session {
		t.Helper()
		v := session{token: uuid.NewString(), csrf: uuid.NewString()}
		sum := sha256.Sum256([]byte(v.token))
		v.hash = hex.EncodeToString(sum[:])
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, v.hash, subject, issuer, v.csrf)
		return v
	}
	ownerS, plannerS, reviewerS, viewerS, outsiderS := seed(owner), seed(planner), seed(reviewer), seed(viewer), seed(outsider)
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "context-test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, runtime, nil)
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

	// Provenance, entities and an approved process snapshot.
	ev := evidence.New(runtime)
	ea := evidence.Actor{Issuer: issuer, Subject: owner, SessionHash: ownerS.hash}
	es := evidence.Scope{TenantID: tenant, SiteID: site}
	schedule, err := ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Shift plan", Text: "Order 7 on Press 1 at one unit per second"}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	span, err := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: schedule.SourceID, Revision: schedule.Revision, Start: 0, End: 20}, uuid.NewString(), schedule.StateRevision)
	if err != nil {
		t.Fatal(err)
	}
	kn := knowledge.New(runtime)
	ka := knowledge.Actor{Issuer: issuer, Subject: owner, SessionHash: ownerS.hash}
	ks := knowledge.Scope{TenantID: tenant, SiteID: site}
	entity := func(kind, assetKind, name string) knowledge.Entity {
		t.Helper()
		e, err := kn.CreateEntity(ctx, ka, ks, knowledge.EntityInput{EntityID: uuid.NewString(), Type: kind, AssetKind: assetKind, Name: name, SourceID: schedule.SourceID, SourceRevision: schedule.Revision}, uuid.NewString(), 0)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	asset, order, bigOrder, badOrder := entity("asset", "machine", "Press 1"), entity("order", "", "Order 7"), entity("order", "", "Order 9"), entity("order", "", "Order 10")
	inventory, err := kn.ReadInventory(ctx, ka, ks)
	if err != nil {
		t.Fatal(err)
	}
	process, err := kn.CreateSnapshot(ctx, ka, ks, knowledge.SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Approved process cut for the shift"}, uuid.NewString(), inventory.Revision)
	if err != nil {
		t.Fatal(err)
	}

	// Golden plan: 30 minutes at 1 unit/s with 800-second breaks, so each 15-minute
	// window has exactly 100 productive seconds.
	plan := production.PlanBody{
		OrderID: order.EntityID, AssetID: asset.EntityID, WindowStart: "2026-09-27T00:00:00Z", WindowEnd: "2026-09-27T00:30:00Z", PlanRevision: 1, Unit: "count",
		RateSegments:  []production.RateSegment{{Start: "2026-09-27T00:00:00Z", End: "2026-09-27T00:30:00Z", TargetGoodUnitsPerSecond: "1"}},
		PlannedBreaks: []production.Interval{{Start: "2026-09-27T00:01:40Z", End: "2026-09-27T00:15:00Z"}, {Start: "2026-09-27T00:16:40Z", End: "2026-09-27T00:30:00Z"}},
	}
	planWire, err := json.Marshal(map[string]any{"plan": plan, "source_span_ids": []string{span.ID}})
	if err != nil {
		t.Fatal(err)
	}
	var planRevision struct {
		ID string `json:"id"`
	}
	captured := call(plannerS, "POST", "/v1/sites/"+site+"/plan-revisions", "plan", "", string(planWire))
	expect(captured, 201, "")
	if err = json.Unmarshal(captured.Body.Bytes(), &planRevision); err != nil {
		t.Fatal(err)
	}
	expect(call(reviewerS, "POST", "/v1/sites/"+site+"/plan-revisions/"+planRevision.ID+"/reviews", "approve", `"0"`, `{"decision":"approve","reason":"Matches the shift plan"}`), 200, "")

	// One approved telemetry source authoritative for deltas, mapped to the asset and orders.
	sources := "/v1/sites/" + site + "/operational-sources"
	sourceID := uuid.NewString()
	expect(call(ownerS, "POST", sources, "src", `"0"`, `{"source_id":"`+sourceID+`","type":"event_api","authority_scope":"production_delta,heartbeat,watermark","enabled":true,"access_policy_revision":1,"freshness_seconds":120,"clock_skew_seconds":30}`), 200, "")
	mappings, err := json.Marshal(map[string]any{"mappings": []map[string]any{
		{"external_type": "asset", "external_key": "press-01", "entity_id": asset.EntityID, "entity_revision": asset.Revision},
		{"external_type": "order", "external_key": "order-7", "entity_id": order.EntityID, "entity_revision": order.Revision},
		{"external_type": "order", "external_key": "order-10", "entity_id": badOrder.EntityID, "entity_revision": badOrder.Revision},
	}})
	if err != nil {
		t.Fatal(err)
	}
	expect(call(ownerS, "POST", sources+"/"+sourceID+"/mappings", "map", `"1"`, string(mappings)), 200, "")
	expect(call(reviewerS, "POST", sources+"/"+sourceID+"/reviews", "src-review", `"2"`, `{"verdict":"approved","reason":"Telemetry scope verified"}`), 200, "")
	sequence := int64(0)
	batch := 0
	send := func(events ...map[string]any) {
		t.Helper()
		for _, event := range events {
			sequence++
			event["source_sequence"] = sequence
			if event["revision"] == nil {
				event["revision"] = 1
			}
		}
		body, err := json.Marshal(map[string]any{"schema_version": 1, "source_id": sourceID, "source_revision": 2, "mapping_revision": 2, "events": events})
		if err != nil {
			t.Fatal(err)
		}
		batch++
		expect(call(ownerS, "POST", sources+"/"+sourceID+"/batches", "batch-"+string(rune('a'+batch)), "", string(body)), 202, "")
	}
	now := func() string { return time.Now().UTC().Format(time.RFC3339Nano) }
	deltaEvent := func(key string, revision int, orderID, orderKey, start, end, good string) map[string]any {
		payload, _ := json.Marshal(production.Delta{OrderID: orderID, AssetID: asset.EntityID, IntervalStart: start, IntervalEnd: end, GoodUnits: good, Unit: "count"})
		return map[string]any{"external_key": key, "revision": revision, "kind": "production_delta", "event_at": end, "asset_external_key": "press-01", "order_external_key": orderKey, "payload": json.RawMessage(payload)}
	}
	heartbeat := func() map[string]any {
		return map[string]any{"external_key": "hb-" + uuid.NewString(), "kind": "heartbeat", "event_at": now(), "payload": map[string]any{"observed_at": now()}}
	}
	watermark := func(through string) map[string]any {
		// A watermark at sequence N declares everything through N-1 delivered.
		return map[string]any{"external_key": "wm-" + uuid.NewString(), "kind": "watermark", "event_at": now(), "payload": map[string]any{"complete_through": through, "through_sequence": sequence + 0}}
	}
	folder := ingestion.New(db.DB)
	fold := func() {
		t.Helper()
		if _, e := folder.Fold(ctx, ingestion.SourceRef{TenantID: tenant, SiteID: site, SourceID: sourceID}, 500); e != nil {
			t.Fatal(e)
		}
	}
	type view struct {
		plantcontext.Snapshot
		CurrentStatus plantcontext.CurrentStatus `json:"current_status"`
		Reused        bool                       `json:"reused"`
		Replayed      bool                       `json:"replayed"`
	}
	contexts := "/v1/sites/" + site + "/contexts"
	request := func(orderID, start, end string) string {
		b, _ := json.Marshal(plantcontext.Request{OrderID: orderID, AssetID: asset.EntityID, WindowStart: start, WindowEnd: end, ProcessSnapshotID: process.SnapshotID})
		return string(b)
	}
	window1 := request(order.EntityID, "2026-09-27T00:00:00Z", "2026-09-27T00:15:00Z")
	window2 := request(order.EntityID, "2026-09-27T00:15:00Z", "2026-09-27T00:30:00Z")
	create := func(key, body string) view {
		t.Helper()
		w := call(ownerS, "POST", contexts, key, "", body)
		expect(w, 201, "")
		var v view
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	read := func(id string) view {
		t.Helper()
		w := call(viewerS, "GET", contexts+"/"+id, "", "", "")
		expect(w, 200, "")
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("context reads must not be cached")
		}
		var v view
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	has := func(codes []string, code string) bool {
		for _, c := range codes {
			if c == code {
				return true
			}
		}
		return false
	}
	str := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}

	// Request validation and authority.
	expect(call(session{}, "POST", contexts, "k", "", window1), 401, "AUTHENTICATION_REQUIRED")
	expect(call(viewerS, "POST", contexts, "k", "", window1), 403, "PERMISSION_DENIED")
	expect(call(ownerS, "POST", contexts, "", "", window1), 422, "INVALID_REQUEST")
	expect(call(ownerS, "POST", contexts, "k", "", request(order.EntityID, "2026-09-27T00:01:00Z", "2026-09-27T00:16:00Z")), 422, "INVALID_CONTEXT")
	expect(call(ownerS, "POST", contexts, "k", "", request(order.EntityID, "2026-09-27T00:00:00Z", "2026-09-27T00:30:00Z")), 422, "INVALID_CONTEXT")
	expect(call(ownerS, "POST", contexts, "k", "", strings.Replace(window1, `{"order_id"`, `{"tenant_id":"`+otherTenant+`","order_id"`, 1)), 422, "INVALID_CONTEXT")
	expect(call(ownerS, "POST", contexts, "k", "", request(asset.EntityID, "2026-09-27T00:00:00Z", "2026-09-27T00:15:00Z")), 422, "UNKNOWN_ENTITY")
	expect(call(ownerS, "POST", contexts, "k", "", strings.Replace(window1, process.SnapshotID, uuid.NewString(), 1)), 422, "UNKNOWN_PROCESS_SNAPSHOT")
	expect(call(outsiderS, "POST", contexts, "k", "", window1), 404, "NOT_FOUND")

	// Window 1 data delivered but not yet folded: received is not semantically accepted.
	send(deltaEvent("d-1", 1, order.EntityID, "order-7", "2026-09-27T00:00:00Z", "2026-09-27T00:15:00Z", "80"), heartbeat())
	send(watermark("2026-09-27T00:15:00Z"))
	unprocessed := create("w1-unprocessed", window1)
	if unprocessed.Readiness != plantcontext.NotReady || !has(unprocessed.ReasonCodes, "NOT_PROCESSED") || unprocessed.ActualGoodUnits != nil {
		t.Fatalf("unfolded window: readiness=%s reasons=%v actual=%s", unprocessed.Readiness, unprocessed.ReasonCodes, str(unprocessed.ActualGoodUnits))
	}
	fold()
	w1 := create("w1", window1)
	if w1.Readiness != plantcontext.ReadyState || str(w1.ExpectedGoodUnits) != "100.000000" || str(w1.ActualGoodUnits) != "80.000000" || str(w1.DeficitGoodUnits) != "20.000000" {
		t.Fatalf("golden window 1: readiness=%s reasons=%v expected=%s actual=%s deficit=%s", w1.Readiness, w1.ReasonCodes, str(w1.ExpectedGoodUnits), str(w1.ActualGoodUnits), str(w1.DeficitGoodUnits))
	}
	if w1.SupersedesContextID == nil || *w1.SupersedesContextID != unprocessed.ContextID || w1.ApprovedPlanRevision == nil || w1.ApprovedPlanRevision.PlanRevisionID != planRevision.ID || w1.ProcessSnapshotSHA256 != process.SHA256 || len(w1.SourceRecordRefs) != 2 || !w1.CurrentStatus.Eligible {
		t.Fatalf("window 1 provenance: %+v", w1)
	}
	if stale := read(unprocessed.ContextID); stale.CurrentStatus.Eligible || !has(stale.CurrentStatus.Reasons, "SUPERSEDED") || stale.CurrentStatus.SupersededBy == nil || *stale.CurrentStatus.SupersededBy != w1.ContextID {
		t.Fatalf("superseded context status: %+v", stale.CurrentStatus)
	}
	// Identical revisions yield the same context and digest; idempotency keys replay.
	if again := create("w1-again", window1); again.ContextID != w1.ContextID || again.Digest != w1.Digest || !again.Reused {
		t.Fatalf("repeat request did not reuse: %+v", again)
	}
	if replay := create("w1", window1); replay.ContextID != w1.ContextID || !replay.Replayed {
		t.Fatalf("same-key request did not replay: %+v", replay)
	}
	expect(call(ownerS, "POST", contexts, "w1", "", window2), 409, "IDEMPOTENCY_CONFLICT")

	// Window 2: also 80 against 100. Two consecutive complete windows qualify. This delta
	// is sent with +05:30 offsets: intake stores it in UTC, so SQL window selection finds it.
	send(deltaEvent("d-2", 1, order.EntityID, "order-7", "2026-09-27T05:45:00+05:30", "2026-09-27T06:00:00+05:30", "80"), heartbeat())
	send(watermark("2026-09-27T00:30:00Z"))
	fold()
	w2 := create("w2", window2)
	if w2.Readiness != plantcontext.ReadyState || str(w2.DeficitGoodUnits) != "20.000000" {
		t.Fatalf("golden window 2: readiness=%s reasons=%v deficit=%s", w2.Readiness, w2.ReasonCodes, str(w2.DeficitGoodUnits))
	}
	if r := plantcontext.EvaluateShortfall(w1.Snapshot, w2.Snapshot); !r.Qualifies {
		t.Fatalf("two golden windows must qualify: %+v", r)
	}
	// The offset-spelled delta is stored in UTC, so SQL parses it instead of treating it as an
	// unparseable candidate for every window.
	var storedStart string
	var parsed bool
	if err = control.QueryRowContext(ctx, `SELECT payload->>'interval_start',supervisor.try_utc_timestamp(payload->>'interval_start') IS NOT NULL FROM supervisor.operational_event_records WHERE tenant_id=$1 AND external_key='d-2'`, tenant).Scan(&storedStart, &parsed); err != nil || storedStart != "2026-09-27T00:15:00Z" || !parsed {
		t.Fatalf("stored interval_start=%q parsed=%v err=%v", storedStart, parsed, err)
	}
	// A fact corrected out of window 3 (into window 4) is not an input of window 3, and
	// must not make window 3's context look changed on every read.
	send(deltaEvent("d-move", 1, order.EntityID, "order-7", "2026-09-27T00:30:00Z", "2026-09-27T00:45:00Z", "7"),
		deltaEvent("d-move", 2, order.EntityID, "order-7", "2026-09-27T00:45:00Z", "2026-09-27T01:00:00Z", "7"), heartbeat())
	// A plan that does not cover the window is NOT_READY, never zero.
	w3 := create("w3", request(order.EntityID, "2026-09-27T00:30:00Z", "2026-09-27T00:45:00Z"))
	if w3.Readiness != plantcontext.NotReady || !has(w3.ReasonCodes, "PLAN_NOT_COVERING_WINDOW") || len(w3.DeltaRefs()) != 0 {
		t.Fatalf("uncovered window: %+v", w3)
	}
	if again := read(w3.ContextID); has(again.CurrentStatus.Reasons, "INPUTS_CHANGED") {
		t.Fatalf("a correction that moved out of the window flagged it as changed: %+v", again.CurrentStatus)
	}

	// A newer plan revision for another shift does not displace the plan for this one, and a
	// rejected newer revision contests nothing.
	submitPlan := func(key string, body production.PlanBody) string {
		t.Helper()
		wire, err := json.Marshal(map[string]any{"plan": body, "source_span_ids": []string{span.ID}})
		if err != nil {
			t.Fatal(err)
		}
		w := call(plannerS, "POST", "/v1/sites/"+site+"/plan-revisions", key, "", string(wire))
		expect(w, 201, "")
		var created struct {
			ID string `json:"id"`
		}
		if err = json.Unmarshal(w.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		return created.ID
	}
	laterShift := plan
	laterShift.PlanRevision, laterShift.WindowStart, laterShift.WindowEnd = 2, "2026-09-27T00:30:00Z", "2026-09-27T01:00:00Z"
	laterShift.RateSegments = []production.RateSegment{{Start: "2026-09-27T00:30:00Z", End: "2026-09-27T01:00:00Z", TargetGoodUnitsPerSecond: "1"}}
	laterShift.PlannedBreaks = nil
	laterID := submitPlan("plan-later-shift", laterShift)
	expect(call(reviewerS, "POST", "/v1/sites/"+site+"/plan-revisions/"+laterID+"/reviews", "approve-later", `"0"`, `{"decision":"approve","reason":"Second shift plan"}`), 200, "")
	if still := read(w2.ContextID); !still.CurrentStatus.Eligible || has(still.CurrentStatus.Reasons, "PLAN_NO_LONGER_ELIGIBLE") {
		t.Fatalf("a plan for another shift displaced this window's plan: %+v", still.CurrentStatus)
	}
	rejectedRev := plan
	rejectedRev.PlanRevision = 3
	rejectedID := submitPlan("plan-rejected-rev", rejectedRev)
	expect(call(reviewerS, "POST", "/v1/sites/"+site+"/plan-revisions/"+rejectedID+"/reviews", "reject-rev3", `"0"`, `{"decision":"reject","reason":"Not needed"}`), 200, "")
	if still := read(w2.ContextID); !still.CurrentStatus.Eligible {
		t.Fatalf("a rejected revision contested the approved plan: %+v", still.CurrentStatus)
	}

	// A late correction appends revision 2; the old context reports INPUTS_CHANGED and a
	// new context supersedes it with actual 95 and deficit 5, which no longer qualifies.
	send(deltaEvent("d-1", 2, order.EntityID, "order-7", "2026-09-27T00:00:00Z", "2026-09-27T00:15:00Z", "95"), heartbeat())
	send(watermark("2026-09-27T00:30:00Z"))
	fold()
	if old := read(w1.ContextID); old.CurrentStatus.Eligible || !has(old.CurrentStatus.Reasons, "INPUTS_CHANGED") || str(old.ActualGoodUnits) != "80.000000" {
		t.Fatalf("stale context after correction: status=%+v actual=%s", old.CurrentStatus, str(old.ActualGoodUnits))
	}
	corrected := create("w1-corrected", window1)
	if corrected.Readiness != plantcontext.ReadyState || str(corrected.ActualGoodUnits) != "95.000000" || str(corrected.DeficitGoodUnits) != "5.000000" || corrected.SupersedesContextID == nil || *corrected.SupersedesContextID != w1.ContextID || corrected.Digest == w1.Digest {
		t.Fatalf("corrected context: %+v", corrected)
	}
	if r := plantcontext.EvaluateShortfall(corrected.Snapshot, w2.Snapshot); r.Qualifies || r.Reason != "BELOW_THRESHOLD" {
		t.Fatalf("corrected 95 must not qualify: %+v", r)
	}

	// A later correction that intake quarantined blocks the chain: the old context is
	// ineligible and a new context is NOT_READY, never the superseded value 95 or 80.
	send(deltaEvent("d-1", 3, order.EntityID, "order-99", "2026-09-27T00:00:00Z", "2026-09-27T00:15:00Z", "60"), heartbeat())
	send(watermark("2026-09-27T00:30:00Z"))
	fold()
	if old := read(corrected.ContextID); old.CurrentStatus.Eligible || !has(old.CurrentStatus.Reasons, "INPUTS_CHANGED") {
		t.Fatalf("context stayed eligible after a quarantined correction: %+v", old.CurrentStatus)
	}
	blocked := create("w1-quarantined", window1)
	if blocked.Readiness != plantcontext.NotReady || !has(blocked.ReasonCodes, "CORRECTION_QUARANTINED") || blocked.ActualGoodUnits != nil {
		t.Fatalf("quarantined correction context: %+v", blocked)
	}
	// An unparseable delta for another order never crashes selection; it fails closed.
	bad := deltaEvent("d-bad", 1, badOrder.EntityID, "order-10", "2026-09-27T00:00:00Z", "2026-09-27T00:15:00Z", "1")
	bad["payload"] = json.RawMessage(`{"order_id":"` + badOrder.EntityID + `","asset_id":"` + asset.EntityID + `","interval_start":"yesterday","interval_end":"2026-09-27T00:15:00Z","good_units":"1","unit":"count"}`)
	send(bad, heartbeat())
	send(watermark("2026-09-27T00:30:00Z"))
	fold()
	if badCtx := create("bad", request(badOrder.EntityID, "2026-09-27T00:00:00Z", "2026-09-27T00:15:00Z")); badCtx.Readiness != plantcontext.NotReady || !has(badCtx.ReasonCodes, "INVALID_DELTA") || !has(badCtx.ReasonCodes, "NO_APPROVED_PLAN") {
		t.Fatalf("malformed delta context: %+v", badCtx)
	}

	// Rejecting the plan withdraws eligibility from existing contexts immediately.
	expect(call(ownerS, "POST", "/v1/sites/"+site+"/plan-revisions/"+planRevision.ID+"/reviews", "owner-reject", `"1"`, `{"decision":"reject","reason":"x"}`), 403, "PERMISSION_DENIED")
	other := seed(uuid.NewString())
	_ = other
	expect(call(reviewerS, "POST", "/v1/sites/"+site+"/plan-revisions/"+planRevision.ID+"/reviews", "reject", `"1"`, `{"decision":"reject","reason":"Plan withdrawn by planning"}`), 200, "")
	if after := read(corrected.ContextID); after.CurrentStatus.Eligible || !has(after.CurrentStatus.Reasons, "PLAN_NO_LONGER_ELIGIBLE") {
		t.Fatalf("context stayed eligible after plan rejection: %+v", after.CurrentStatus)
	}
	if rejected := create("w1-rejected", window1); rejected.Readiness != plantcontext.NotReady || !has(rejected.ReasonCodes, "PLAN_NOT_APPROVED") || rejected.ExpectedGoodUnits != nil {
		t.Fatalf("context with rejected plan: %+v", rejected)
	}

	// More than 10k candidate records is WINDOW_TOO_LARGE, never a truncated total.
	if _, err = control.ExecContext(ctx, `SET session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO supervisor.operational_event_records(tenant_id,site_id,source_id,source_revision,mapping_revision,source_sequence,external_key,revision,event_kind,event_at,payload,payload_sha256,event_sha256,batch_id)
SELECT $1,$2,$3,2,2,100000+g,'bulk-'||g,1,'production_delta','2026-09-27T00:15:00Z',jsonb_build_object('order_id',$4::text,'asset_id',$5::text,'interval_start','2026-09-27T00:00:00Z','interval_end','2026-09-27T00:15:00Z','good_units','1','unit','count'),repeat('a',64),repeat('b',64),gen_random_uuid()
FROM generate_series(1,10001) g`, tenant, site, sourceID, bigOrder.EntityID, asset.EntityID)
	if _, err = control.ExecContext(ctx, `SET session_replication_role = origin`); err != nil {
		t.Fatal(err)
	}
	expect(call(ownerS, "POST", contexts, "too-large", "", request(bigOrder.EntityID, "2026-09-27T00:00:00Z", "2026-09-27T00:15:00Z")), 422, "WINDOW_TOO_LARGE")

	// Reads are scoped, non-disclosing and audited.
	expect(call(outsiderS, "GET", "/v1/sites/"+otherSite+"/contexts/"+w1.ContextID, "", "", ""), 404, "NOT_FOUND")
	expect(call(viewerS, "GET", contexts+"/not-a-uuid", "", "", ""), 404, "NOT_FOUND")
	expect(call(viewerS, "GET", contexts+"/"+w1.ContextID+"?x=1", "", "", ""), 422, "INVALID_REQUEST")
	// WIS-272 source revocation: one transaction disables the source with the next
	// access-policy revision and cancels its running import; derived contexts become
	// ineligible at once and intake is refused.
	if before := read(w2.ContextID); has(before.CurrentStatus.Reasons, "SOURCE_NO_LONGER_APPROVED") {
		t.Fatalf("source already reported unapproved before revocation: %+v", before.CurrentStatus)
	}
	jobID := uuid.NewString()
	func() {
		tx, e := control.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); e != nil {
			t.Fatal(e)
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_import_jobs(tenant_id,site_id,id,source_id,format,file_sha256,request_sha256,idempotency_key,actor_issuer,actor_subject,source_revision,mapping_revision,start_sequence,row_count,status) VALUES($1,$2,$3,$4,'csv',repeat('a',64),repeat('b',64),'running-import',$5,$6,2,2,900000,2,'running')`, tenant, site, jobID, sourceID, issuer, owner); e != nil {
			t.Fatal(e)
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_import_rows(tenant_id,site_id,job_id,row_index,status,source_sequence) VALUES($1,$2,$3,1,'accepted',900000)`, tenant, site, jobID); e != nil {
			t.Fatal(e)
		}
		if e = tx.Commit(); e != nil {
			t.Fatal(e)
		}
	}()
	revoke := sources + "/" + sourceID + "/revoke"
	expect(call(viewerS, "POST", revoke, "revoke-viewer", `"2"`, ""), 403, "PERMISSION_DENIED")
	expect(call(ownerS, "POST", revoke, "revoke-stale", `"1"`, ""), 412, "REVISION_CONFLICT")
	expect(call(ownerS, "POST", revoke, "revoke-body", `"2"`, `{"reason":"x"}`), 422, "INVALID_REQUEST")
	expect(call(ownerS, "POST", revoke, "", `"2"`, ""), 422, "INVALID_REQUEST")
	var revoked struct {
		Revision             int64 `json:"revision"`
		Enabled              bool  `json:"enabled"`
		AccessPolicyRevision int64 `json:"access_policy_revision"`
	}
	w := call(ownerS, "POST", revoke, "revoke", `"2"`, "")
	expect(w, 200, "")
	if err = json.Unmarshal(w.Body.Bytes(), &revoked); err != nil || revoked.Revision != 3 || revoked.Enabled || revoked.AccessPolicyRevision != 2 {
		t.Fatalf("revoked source: %s", w.Body.String())
	}
	if replay := call(ownerS, "POST", revoke, "revoke", `"2"`, ""); replay.Code != 200 || !strings.Contains(replay.Body.String(), `"revision":3`) {
		t.Fatalf("revocation replay: HTTP %d %s", replay.Code, replay.Body.String())
	}
	if after := read(w2.ContextID); after.CurrentStatus.Eligible || !has(after.CurrentStatus.Reasons, "SOURCE_NO_LONGER_APPROVED") {
		t.Fatalf("context stayed eligible after source revocation: %+v", after.CurrentStatus)
	}
	job := call(ownerS, "GET", "/v1/sites/"+site+"/operational-ingestion-jobs/"+jobID, "", "", "")
	expect(job, 200, "")
	if !strings.Contains(job.Body.String(), `"status":"cancelled"`) || !strings.Contains(job.Body.String(), `"accepted_rows":1`) {
		t.Fatalf("running import not cancelled with frozen counts: %s", job.Body.String())
	}
	lateEvent := heartbeat()
	lateEvent["source_sequence"], lateEvent["revision"] = 999999, 1
	late, err := json.Marshal(map[string]any{"schema_version": 1, "source_id": sourceID, "source_revision": 2, "mapping_revision": 2, "events": []map[string]any{lateEvent}})
	if err != nil {
		t.Fatal(err)
	}
	expect(call(ownerS, "POST", sources+"/"+sourceID+"/batches", "after-revoke", "", string(late)), 412, "SOURCE_REVISION_CONFLICT")
	// Re-enabling with the pre-revocation access-policy revision would revive old contexts.
	expect(call(ownerS, "POST", sources, "re-enable", `"3"`, `{"source_id":"`+sourceID+`","type":"event_api","authority_scope":"production_delta,heartbeat,watermark","enabled":true,"access_policy_revision":1,"freshness_seconds":120,"clock_skew_seconds":30}`), 422, "ACCESS_POLICY_REGRESSION")
	var revokedAudits int
	if err = control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.operational_source_audit WHERE tenant_id=$1 AND source_id=$2 AND event_type='source.revoked' AND revision=3`, tenant, sourceID).Scan(&revokedAudits); err != nil || revokedAudits != 1 {
		t.Fatalf("revocation audit rows=%d err=%v", revokedAudits, err)
	}
	if _, err = control.ExecContext(ctx, `INSERT INTO supervisor.operational_source_revisions(tenant_id,site_id,source_id,revision,source_type,authority_scope,mapping_revision,mapping_sha256,enabled,access_policy_revision,freshness_seconds,clock_skew_seconds,config_sha256,actor_issuer,actor_subject,auth_time)
SELECT tenant_id,site_id,source_id,4,source_type,authority_scope,mapping_revision,mapping_sha256,true,1,freshness_seconds,clock_skew_seconds,config_sha256,actor_issuer,actor_subject,created_at FROM supervisor.operational_source_revisions WHERE tenant_id=$1 AND source_id=$2 AND revision=3`, tenant, sourceID); err == nil || !strings.Contains(err.Error(), "access-policy revision cannot decrease") {
		t.Fatalf("the database must refuse a lower access-policy revision: %v", err)
	}

	var created, superseded, audits int
	if err = control.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM supervisor.plant_context_outbox WHERE tenant_id=$1 AND event_type='context.created'),(SELECT count(*) FROM supervisor.plant_context_outbox WHERE tenant_id=$1 AND event_type='context.superseded'),(SELECT count(*) FROM supervisor.plant_context_audit WHERE tenant_id=$1)`, tenant).Scan(&created, &superseded, &audits); err != nil {
		t.Fatal(err)
	}
	// Created: w1-unprocessed, w1, w2, w3, w1-corrected, w1-quarantined, bad, w1-rejected.
	// Superseded: four replacements of window 1.
	if created != 8 || superseded != 4 || audits < 12 {
		t.Fatalf("outbox created=%d superseded=%d audits=%d", created, superseded, audits)
	}
	tx, err := runtime.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		t.Fatal(err)
	}
	for name, q := range map[string]string{
		"context update": `UPDATE supervisor.plant_contexts SET readiness='READY' WHERE tenant_id='` + tenant + `'`,
		"context delete": `DELETE FROM supervisor.plant_contexts WHERE tenant_id='` + tenant + `'`,
		"record delete":  `DELETE FROM supervisor.plant_context_records WHERE tenant_id='` + tenant + `'`,
		"head rewind":    `UPDATE supervisor.plant_context_heads SET revision=revision+1 WHERE tenant_id='` + tenant + `'`,
		"head swap":      `UPDATE supervisor.plant_context_heads SET context_id='` + w2.ContextID + `',revision=revision+1 WHERE tenant_id='` + tenant + `' AND context_id<>'` + w2.ContextID + `'`,
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
	var visible int
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, otherTenant); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM supervisor.plant_contexts)+(SELECT count(*) FROM supervisor.plant_context_heads)+(SELECT count(*) FROM supervisor.plant_context_outbox)`).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("cross-tenant contexts visible: %d err=%v", visible, err)
	}
}
