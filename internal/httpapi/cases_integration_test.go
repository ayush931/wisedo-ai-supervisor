//go:build integration

package httpapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/approvals"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/cases"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/dispatch"
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
	"go.temporal.io/sdk/client"
)

// pipelineSession is a seeded browser session of the shortfall pipeline.
type pipelineSession struct{ token, csrf, hash string }

// shortfallPipeline is the WIS-272 golden fixture driven through the real path (approved plan,
// authenticated intake, fold, POST /contexts), extended to five windows: 1 unit/s with 100
// productive seconds per window expects 100; 80 delivered in each of the first four gives
// deficit 20, so those pairs qualify; 95 in the fifth gives deficit 5, below the threshold.
type shortfallPipeline struct {
	control                                              *sql.DB
	db                                                   *store.Store
	tenant, site, otherTenant, otherSite, sourceID, lead string
	asset, order                                         knowledge.Entity
	// otherOrder runs on the same line in the first window only, with its own approved plan and
	// delivered delta; no context is created for it here.
	otherOrder                                knowledge.Entity
	registerSourceID, processSnapshotID       string
	registerRevision                          int64
	ownerS, leadS, viewerS, outsiderS, staleS pipelineSession
	call                                      func(actor pipelineSession, method, path, key, revision, body string) *httptest.ResponseRecorder
	expect                                    func(w *httptest.ResponseRecorder, status int, code string)
	decodeCase                                func(w *httptest.ResponseRecorder) cases.CaseV1
	exec                                      func(q string, args ...any)
	seed                                      func(subject string, authAge time.Duration) pipelineSession
	issuer                                    string
	contexts                                  []plantcontext.Snapshot
	// correctDelta sends a source correction (a new revision) of window i's delivered delta and
	// folds it, without creating a context.
	correctDelta func(t *testing.T, window int, good string, revision int)
}

func newShortfallPipeline(t *testing.T, ctx context.Context) *shortfallPipeline {
	t.Helper()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: real PostgreSQL admin and runtime URLs required")
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	database := "case_http_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	t.Cleanup(func() { db.Close() })
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := control.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	tenant, site, otherTenant, otherSite := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	owner, planner, reviewer, lead, staleLead, viewer, outsider := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, origin := "https://case-http.invalid/"+uuid.NewString(), "http://127.0.0.1:58177"
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic case test'),($2,'other tenant')`, tenant, otherTenant)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'case site'),($3,$4,'other site')`, tenant, site, otherTenant, otherSite)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner'),($1,$2,$3,$5,'supervisor'),($1,$2,$3,$6,'supervisor'),($1,$2,$3,$7,'supervisor'),($1,$2,$3,$8,'supervisor'),($1,$2,$3,$9,'viewer')`, tenant, site, issuer, owner, planner, reviewer, lead, staleLead, viewer)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'supervisor')`, otherTenant, otherSite, issuer, outsider)
	seed := func(subject string, authAge time.Duration) pipelineSession {
		t.Helper()
		v := pipelineSession{token: uuid.NewString(), csrf: uuid.NewString()}
		sum := sha256.Sum256([]byte(v.token))
		v.hash = hex.EncodeToString(sum[:])
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now()-$5::interval)`, v.hash, subject, issuer, v.csrf, strconv.Itoa(int(authAge.Seconds()))+" seconds")
		return v
	}
	ownerS, plannerS, reviewerS, leadS, viewerS, outsiderS := seed(owner, 0), seed(planner, 0), seed(reviewer, 0), seed(lead, 0), seed(viewer, 0), seed(outsider, 0)
	staleS := seed(staleLead, 20*time.Minute)
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "case-test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)
	call := func(actor pipelineSession, method, path, key, revision, body string) *httptest.ResponseRecorder {
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
	expect := func(w *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("HTTP %d want %d: %s", w.Code, status, w.Body.String())
		}
		if code != "" && !strings.Contains(w.Body.String(), `"code":"`+code+`"`) {
			t.Fatalf("problem code want %s: %s", code, w.Body.String())
		}
	}
	decodeCase := func(w *httptest.ResponseRecorder) cases.CaseV1 {
		t.Helper()
		var v cases.CaseV1
		if e := json.Unmarshal(w.Body.Bytes(), &v); e != nil {
			t.Fatalf("%v: %s", e, w.Body.String())
		}
		return v
	}

	// The WIS-272 golden fixture, extended to five windows: 1 unit/s with 100 productive
	// seconds per window expects 100; 80 delivered in each of the first four gives deficit 20,
	// so those pairs qualify; 95 in the fifth gives deficit 5, below the rule's threshold.
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
	register, err := ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Asset and order register", Text: "Press 1, Order 7"}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	kn := knowledge.New(runtime)
	ka := knowledge.Actor{Issuer: issuer, Subject: owner, SessionHash: ownerS.hash}
	ks := knowledge.Scope{TenantID: tenant, SiteID: site}
	entity := func(kind, assetKind, name string) knowledge.Entity {
		t.Helper()
		e, err := kn.CreateEntity(ctx, ka, ks, knowledge.EntityInput{EntityID: uuid.NewString(), Type: kind, AssetKind: assetKind, Name: name, SourceID: register.SourceID, SourceRevision: register.Revision}, uuid.NewString(), 0)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	asset, order, otherOrder := entity("asset", "line", "Press line 1"), entity("order", "", "Order 7"), entity("order", "", "Order 8")
	inventory, err := kn.ReadInventory(ctx, ka, ks)
	if err != nil {
		t.Fatal(err)
	}
	process, err := kn.CreateSnapshot(ctx, ka, ks, knowledge.SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Approved process cut for the shift"}, uuid.NewString(), inventory.Revision)
	if err != nil {
		t.Fatal(err)
	}
	windows := []string{"2026-09-27T00:00:00Z", "2026-09-27T00:15:00Z", "2026-09-27T00:30:00Z", "2026-09-27T00:45:00Z", "2026-09-27T01:00:00Z", "2026-09-27T01:15:00Z"}
	breaks := make([]production.Interval, 0, 5)
	for i := 0; i < 5; i++ {
		start, _ := time.Parse(time.RFC3339, windows[i])
		breaks = append(breaks, production.Interval{Start: start.Add(100 * time.Second).Format(time.RFC3339), End: windows[i+1]})
	}
	plan := production.PlanBody{
		OrderID: order.EntityID, AssetID: asset.EntityID, WindowStart: windows[0], WindowEnd: windows[5], PlanRevision: 1, Unit: "count",
		RateSegments:  []production.RateSegment{{Start: windows[0], End: windows[5], TargetGoodUnitsPerSecond: "1"}},
		PlannedBreaks: breaks,
	}
	planWire, err := json.Marshal(map[string]any{"plan": plan, "source_span_ids": []string{span.ID}})
	if err != nil {
		t.Fatal(err)
	}
	captured := call(plannerS, "POST", "/v1/sites/"+site+"/plan-revisions", "plan", "", string(planWire))
	expect(captured, 201, "")
	var planRevision struct {
		ID string `json:"id"`
	}
	if err = json.Unmarshal(captured.Body.Bytes(), &planRevision); err != nil {
		t.Fatal(err)
	}
	expect(call(reviewerS, "POST", "/v1/sites/"+site+"/plan-revisions/"+planRevision.ID+"/reviews", "approve", `"0"`, `{"decision":"approve","reason":"Matches the shift plan"}`), 200, "")
	otherPlan := plan
	otherPlan.OrderID, otherPlan.WindowEnd = otherOrder.EntityID, windows[1]
	otherPlan.RateSegments = []production.RateSegment{{Start: windows[0], End: windows[1], TargetGoodUnitsPerSecond: "1"}}
	otherPlan.PlannedBreaks = breaks[:1]
	otherWire, err := json.Marshal(map[string]any{"plan": otherPlan, "source_span_ids": []string{span.ID}})
	if err != nil {
		t.Fatal(err)
	}
	captured = call(plannerS, "POST", "/v1/sites/"+site+"/plan-revisions", "plan-8", "", string(otherWire))
	expect(captured, 201, "")
	if err = json.Unmarshal(captured.Body.Bytes(), &planRevision); err != nil {
		t.Fatal(err)
	}
	expect(call(reviewerS, "POST", "/v1/sites/"+site+"/plan-revisions/"+planRevision.ID+"/reviews", "approve-8", `"0"`, `{"decision":"approve","reason":"Matches the shift plan"}`), 200, "")

	sources := "/v1/sites/" + site + "/operational-sources"
	sourceID := uuid.NewString()
	expect(call(ownerS, "POST", sources, "src", `"0"`, `{"source_id":"`+sourceID+`","type":"event_api","authority_scope":"production_delta,heartbeat,watermark","enabled":true,"access_policy_revision":1,"freshness_seconds":120,"clock_skew_seconds":30}`), 200, "")
	mappings, err := json.Marshal(map[string]any{"mappings": []map[string]any{
		{"external_type": "asset", "external_key": "line-01", "entity_id": asset.EntityID, "entity_revision": asset.Revision},
		{"external_type": "order", "external_key": "order-7", "entity_id": order.EntityID, "entity_revision": order.Revision},
		{"external_type": "order", "external_key": "order-8", "entity_id": otherOrder.EntityID, "entity_revision": otherOrder.Revision},
	}})
	if err != nil {
		t.Fatal(err)
	}
	expect(call(ownerS, "POST", sources+"/"+sourceID+"/mappings", "map", `"1"`, string(mappings)), 200, "")
	expect(call(reviewerS, "POST", sources+"/"+sourceID+"/reviews", "src-review", `"2"`, `{"verdict":"approved","reason":"Telemetry scope verified"}`), 200, "")
	sequence := int64(0)
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
		expect(call(ownerS, "POST", sources+"/"+sourceID+"/batches", "batch-"+strconv.FormatInt(sequence, 10), "", string(body)), 202, "")
	}
	now := func() string { return time.Now().UTC().Format(time.RFC3339Nano) }
	folder := ingestion.New(db.DB)
	contexts := make([]plantcontext.Snapshot, 5)
	for i := 0; i < 5; i++ {
		good := "80"
		if i == 4 {
			good = "95"
		}
		payload, _ := json.Marshal(production.Delta{OrderID: order.EntityID, AssetID: asset.EntityID, IntervalStart: windows[i], IntervalEnd: windows[i+1], GoodUnits: good, Unit: "count"})
		send(map[string]any{"external_key": "d-" + strconv.Itoa(i), "kind": "production_delta", "event_at": windows[i+1], "asset_external_key": "line-01", "order_external_key": "order-7", "payload": json.RawMessage(payload)},
			map[string]any{"external_key": "hb-" + uuid.NewString(), "kind": "heartbeat", "event_at": now(), "payload": map[string]any{"observed_at": now()}})
		if i == 0 {
			other, _ := json.Marshal(production.Delta{OrderID: otherOrder.EntityID, AssetID: asset.EntityID, IntervalStart: windows[0], IntervalEnd: windows[1], GoodUnits: "80", Unit: "count"})
			send(map[string]any{"external_key": "d-order-8", "kind": "production_delta", "event_at": windows[1], "asset_external_key": "line-01", "order_external_key": "order-8", "payload": json.RawMessage(other)})
		}
		send(map[string]any{"external_key": "wm-" + uuid.NewString(), "kind": "watermark", "event_at": now(), "payload": map[string]any{"complete_through": windows[i+1], "through_sequence": sequence}})
		if _, e := folder.Fold(ctx, ingestion.SourceRef{TenantID: tenant, SiteID: site, SourceID: sourceID}, 500); e != nil {
			t.Fatal(e)
		}
		body, _ := json.Marshal(plantcontext.Request{OrderID: order.EntityID, AssetID: asset.EntityID, WindowStart: windows[i], WindowEnd: windows[i+1], ProcessSnapshotID: process.SnapshotID})
		w := call(ownerS, "POST", "/v1/sites/"+site+"/contexts", "ctx-"+strconv.Itoa(i), "", string(body))
		expect(w, 201, "")
		if err = json.Unmarshal(w.Body.Bytes(), &contexts[i]); err != nil {
			t.Fatal(err)
		}
		if contexts[i].Readiness != plantcontext.ReadyState || len(contexts[i].DeltaRefs()) != 1 {
			t.Fatalf("window %d: readiness=%s reasons=%v", i, contexts[i].Readiness, contexts[i].ReasonCodes)
		}
	}
	correctDelta := func(t *testing.T, i int, good string, revision int) {
		t.Helper()
		payload, _ := json.Marshal(production.Delta{OrderID: order.EntityID, AssetID: asset.EntityID, IntervalStart: windows[i], IntervalEnd: windows[i+1], GoodUnits: good, Unit: "count"})
		send(map[string]any{"external_key": "d-" + strconv.Itoa(i), "revision": revision, "kind": "production_delta", "event_at": windows[i+1], "asset_external_key": "line-01", "order_external_key": "order-7", "payload": json.RawMessage(payload)},
			map[string]any{"external_key": "hb-" + uuid.NewString(), "kind": "heartbeat", "event_at": now(), "payload": map[string]any{"observed_at": now()}})
		send(map[string]any{"external_key": "wm-" + uuid.NewString(), "kind": "watermark", "event_at": now(), "payload": map[string]any{"complete_through": windows[5], "through_sequence": sequence}})
		if _, e := folder.Fold(ctx, ingestion.SourceRef{TenantID: tenant, SiteID: site, SourceID: sourceID}, 500); e != nil {
			t.Fatal(e)
		}
	}
	return &shortfallPipeline{correctDelta: correctDelta, control: control, db: db, tenant: tenant, site: site, otherTenant: otherTenant, otherSite: otherSite, sourceID: sourceID, lead: lead,
		asset: asset, order: order, otherOrder: otherOrder, registerSourceID: register.SourceID, registerRevision: register.Revision, processSnapshotID: process.SnapshotID,
		ownerS: ownerS, leadS: leadS, viewerS: viewerS, outsiderS: outsiderS, staleS: staleS,
		call: call, expect: expect, decodeCase: decodeCase, exec: exec, contexts: contexts, seed: seed, issuer: issuer}
}

// TestCaseTriggersAndLifecycleHTTPPostgres covers the WIS-273 trigger and lifecycle increment
// end to end on the real WIS-272 path (approved plan -> intake -> fold -> contexts): only a
// qualifying two-window shortfall opens a case, 100 duplicate triggers make one case, a later
// window joins the active episode, and cancel, reopen and resume follow the transition table
// with revision, idempotency, authority and tenant checks.
func TestCaseTriggersAndLifecycleHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	p := newShortfallPipeline(t, ctx)
	control, db, tenant, site, otherSite, sourceID, lead := p.control, p.db, p.tenant, p.site, p.otherSite, p.sourceID, p.lead
	asset, order, contexts := p.asset, p.order, p.contexts
	register := struct {
		SourceID string
		Revision int64
	}{p.registerSourceID, p.registerRevision}
	process := struct{ SnapshotID string }{p.processSnapshotID}
	ownerS, leadS, viewerS, outsiderS, staleS := p.ownerS, p.leadS, p.viewerS, p.outsiderS, p.staleS
	call, expect, decodeCase, exec := p.call, p.expect, p.decodeCase, p.exec
	_ = ownerS
	var err error
	event := func(i int) string { return contexts[i].DeltaRefs()[0].RecordID }
	service := cases.NewService(db.DB)
	scope := cases.Scope{TenantID: tenant, SiteID: site}
	actor := cases.ServiceActor{Issuer: "https://supervisor.internal", Subject: "shortfall-evaluator"}
	trigger := func(window, contextWindow int) (cases.TriggerResult, error) {
		return service.Trigger(ctx, scope, actor, cases.TriggerInput{SourceID: sourceID, SourceEventID: event(window), ContextSnapshotID: contexts[contextWindow].ContextID, ExceptionKind: cases.ExceptionKind})
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if e := control.QueryRowContext(ctx, q, args...).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}

	// Refusals write nothing: no previous window, an event the context did not select, an
	// unknown context, an unknown kind.
	if _, err = trigger(0, 0); !errors.Is(err, cases.ErrNotQualified) {
		t.Fatalf("first window alone: %v", err)
	}
	if _, err = trigger(4, 4); !errors.Is(err, cases.ErrNotQualified) {
		t.Fatalf("window below the shortfall threshold: %v", err)
	}
	if _, err = trigger(0, 1); !errors.Is(err, cases.ErrInvalid) {
		t.Fatalf("event outside its context: %v", err)
	}
	if _, err = service.Trigger(ctx, scope, actor, cases.TriggerInput{SourceID: sourceID, SourceEventID: event(1), ContextSnapshotID: uuid.NewString(), ExceptionKind: cases.ExceptionKind}); !errors.Is(err, cases.ErrInvalid) {
		t.Fatalf("unknown context: %v", err)
	}
	if _, err = service.Trigger(ctx, scope, actor, cases.TriggerInput{SourceID: sourceID, SourceEventID: event(1), ContextSnapshotID: contexts[1].ContextID, ExceptionKind: "machine_down"}); !errors.Is(err, cases.ErrInvalid) {
		t.Fatalf("unknown exception kind: %v", err)
	}
	// A context for a machine (asset_kind=machine), not a line, never opens a case. It is planted
	// (FK triggers off) because only its asset's kind is under test.
	machine, machineContext := uuid.NewString(), uuid.NewString()
	ptx, err := control.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	machineSnapshot := `{"readiness":"NOT_READY","digest":"` + strings.Repeat("e", 64) + `","context_id":"` + machineContext + `","order_id":"` + order.EntityID + `","asset_id":"` + machine + `","site_id":"` + site + `"}`
	for _, q := range []string{
		`SET LOCAL session_replication_role = replica`,
		`SELECT set_config('supervisor.tenant_id','` + tenant + `',true)`,
		`INSERT INTO supervisor.knowledge_entity_heads(tenant_id,site_id,id,current_revision) VALUES('` + tenant + `','` + site + `','` + machine + `',1)`,
		`INSERT INTO supervisor.knowledge_entity_revisions(tenant_id,site_id,id,revision,entity_type,payload,source_id,source_revision,actor_issuer,actor_subject) VALUES('` + tenant + `','` + site + `','` + machine + `',1,'asset','{"entity_id":"` + machine + `","type":"asset","asset_kind":"machine","source_id":"` + register.SourceID + `","source_revision":` + strconv.FormatInt(register.Revision, 10) + `}','` + register.SourceID + `',` + strconv.FormatInt(register.Revision, 10) + `,'test','owner')`,
		`INSERT INTO supervisor.plant_contexts(tenant_id,site_id,id,order_id,asset_id,window_start,window_end,process_snapshot_id,process_snapshot_sha256,readiness,snapshot,digest,actor_issuer,actor_subject,evaluated_at) VALUES('` + tenant + `','` + site + `','` + machineContext + `','` + order.EntityID + `','` + machine + `','2026-09-27T00:15:00Z','2026-09-27T00:30:00Z','` + process.SnapshotID + `','` + strings.Repeat("d", 64) + `','NOT_READY','` + machineSnapshot + `','` + strings.Repeat("e", 64) + `','test','owner','2026-09-27T00:30:00Z')`,
	} {
		if _, err = ptx.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err = ptx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Trigger(ctx, scope, actor, cases.TriggerInput{SourceID: sourceID, SourceEventID: event(1), ContextSnapshotID: machineContext, ExceptionKind: cases.ExceptionKind}); !errors.Is(err, cases.ErrInvalid) {
		t.Fatalf("machine-level context: %v", err)
	}
	if n := count(`SELECT count(*) FROM supervisor.cases`); n != 0 {
		t.Fatalf("refused triggers wrote %d cases", n)
	}

	// 100 duplicate triggers for the qualifying window 2 make exactly one case, one trigger
	// record and one outbox start; every caller is told the same case.
	results := make([]cases.TriggerResult, 100)
	failures := make([]error, 100)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], failures[i] = trigger(1, 1)
		}(i)
	}
	wg.Wait()
	opened := 0
	for i, r := range results {
		if failures[i] != nil || r.CaseID != results[0].CaseID || r.Revision != 1 || !r.Opened {
			t.Fatalf("trigger %d: %+v %v", i, r, failures[i])
		}
		if !r.Replayed {
			opened++
		}
	}
	first := results[0].CaseID
	if opened != 1 || count(`SELECT count(*) FROM supervisor.cases`) != 1 || count(`SELECT count(*) FROM supervisor.case_triggers`) != 1 ||
		count(`SELECT count(*) FROM supervisor.case_outbox WHERE case_id=$1 AND kind='start' AND case_revision=1`, first) != 1 {
		t.Fatalf("duplicate triggers: opened=%d", opened)
	}
	// The same source event naming a different context is a different request.
	if _, err = service.Trigger(ctx, scope, actor, cases.TriggerInput{SourceID: sourceID, SourceEventID: event(1), ContextSnapshotID: contexts[2].ContextID, ExceptionKind: cases.ExceptionKind}); !errors.Is(err, cases.ErrIdempotency) {
		t.Fatalf("source event reused for another context: %v", err)
	}
	// Window 3 qualifies with window 2 and joins the active episode.
	joined, err := trigger(2, 2)
	if err != nil || joined.CaseID != first || joined.Opened || joined.Replayed {
		t.Fatalf("join: %+v %v", joined, err)
	}
	if count(`SELECT count(*) FROM supervisor.case_outbox WHERE case_id=$1 AND kind='signal'`, first) != 1 {
		t.Fatal("joining trigger did not signal the workflow")
	}

	// Reads: any member, scope-filtered; another tenant gets the non-disclosing 404.
	casePath := func(id string) string { return "/v1/sites/" + site + "/cases/" + id }
	w := call(viewerS, "GET", casePath(first), "", "", "")
	expect(w, 200, "")
	got := decodeCase(w)
	if w.Header().Get("ETag") != `"1"` || got.State != cases.Detected || got.Episode != 1 || got.LineID != asset.EntityID || got.OrderID != order.EntityID ||
		got.ContextSnapshotID != contexts[1].ContextID || got.WorkflowID != "case:"+tenant+":"+first || len(got.TriggerEvidenceIDs) != 2 ||
		len(got.Transitions) != 1 || got.Transitions[0].Event != "trigger" || got.Transitions[0].Actor.Subject != actor.Subject {
		t.Fatalf("case read: %s", w.Body.String())
	}
	for _, id := range []string{event(1), event(2)} {
		if !strings.Contains(w.Body.String(), id) {
			t.Fatalf("trigger evidence %s missing: %s", id, w.Body.String())
		}
	}
	expect(call(outsiderS, "GET", casePath(first), "", "", ""), 404, "NOT_FOUND")
	expect(call(outsiderS, "GET", "/v1/sites/"+otherSite+"/cases/"+first, "", "", ""), 404, "NOT_FOUND")
	if w = call(outsiderS, "GET", "/v1/sites/"+otherSite+"/cases", "", "", ""); !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatalf("foreign list leaked: %s", w.Body.String())
	}
	if w = call(viewerS, "GET", "/v1/sites/"+site+"/cases", "", "", ""); !strings.Contains(w.Body.String(), first) {
		t.Fatalf("site list: %s", w.Body.String())
	}

	// Cancel: authority, contract and revision refusals change nothing.
	cancelBody := func(revision int64, reason string) string {
		return `{"expected_revision":` + strconv.FormatInt(revision, 10) + `,"reason":"` + reason + `"}`
	}
	key := uuid.NewString()
	expect(call(viewerS, "POST", casePath(first)+"/cancel", uuid.NewString(), `"1"`, cancelBody(1, "viewer")), 403, "PERMISSION_DENIED")
	expect(call(ownerS, "POST", casePath(first)+"/cancel", uuid.NewString(), `"1"`, cancelBody(1, "owner bypass")), 403, "PERMISSION_DENIED")
	expect(call(staleS, "POST", casePath(first)+"/cancel", uuid.NewString(), `"1"`, cancelBody(1, "old login")), 428, "REAUTHENTICATION_REQUIRED")
	expect(call(outsiderS, "POST", casePath(first)+"/cancel", uuid.NewString(), `"1"`, cancelBody(1, "foreign")), 404, "NOT_FOUND")
	expect(call(leadS, "POST", casePath(first)+"/cancel", key, `"1"`, cancelBody(2, "header and body disagree")), 422, "INVALID_CASE_REQUEST")
	expect(call(leadS, "POST", casePath(first)+"/cancel", key, `"1"`, `{"expected_revision":1,"reason":"x","state":"resolved"}`), 422, "INVALID_CASE_REQUEST")
	expect(call(leadS, "POST", casePath(first)+"/cancel", "not-a-uuid", `"1"`, cancelBody(1, "bad key")), 422, "INVALID_CASE_REQUEST")
	expect(call(leadS, "POST", casePath(first)+"/cancel", key, `"2"`, cancelBody(2, "stale")), 412, "REVISION_CONFLICT")
	if n := count(`SELECT count(*) FROM supervisor.case_transitions WHERE case_id=$1`, first); n != 1 {
		t.Fatalf("refused cancels recorded %d transitions", n)
	}
	w = call(leadS, "POST", casePath(first)+"/cancel", key, `"1"`, cancelBody(1, "Order moved to another line"))
	expect(w, 200, "")
	if c := decodeCase(w); c.State != cases.Canceled || c.Revision != 2 || c.Transitions[1].Event != "cancel" || *c.Transitions[1].Reason != "Order moved to another line" {
		t.Fatalf("cancel: %s", w.Body.String())
	}
	if c := decodeCase(call(leadS, "POST", casePath(first)+"/cancel", key, `"1"`, cancelBody(1, "Order moved to another line"))); !c.Replayed || c.Revision != 2 {
		t.Fatalf("cancel replay: %+v", c)
	}
	expect(call(leadS, "POST", casePath(first)+"/cancel", key, `"1"`, cancelBody(1, "a different reason")), 409, "IDEMPOTENCY_CONFLICT")
	expect(call(leadS, "POST", casePath(first)+"/cancel", uuid.NewString(), `"2"`, cancelBody(2, "again")), 409, "ILLEGAL_TRANSITION")
	if count(`SELECT count(*) FROM supervisor.case_outbox WHERE case_id=$1 AND kind='cancel' AND case_revision=2`, first) != 1 {
		t.Fatal("cancel did not signal the workflow")
	}

	// Reopen: a new episode referencing the canceled one, with its trigger evidence; the old
	// episode is untouched, and no second reopen while the new one is active.
	reopenBody := func(revision int64, reason string) string {
		return `{"expected_revision":` + strconv.FormatInt(revision, 10) + `,"reason":"` + reason + `"}`
	}
	w = call(leadS, "POST", casePath(first)+"/reopen", uuid.NewString(), `"2"`, reopenBody(2, "Line moved back"))
	expect(w, 201, "")
	second := decodeCase(w)
	if second.ID == first || second.Episode != 2 || second.PreviousCaseID == nil || *second.PreviousCaseID != first || second.State != cases.Detected ||
		second.Revision != 1 || len(second.TriggerEvidenceIDs) != 2 || second.Transitions[0].Event != "reopen" {
		t.Fatalf("reopen: %s", w.Body.String())
	}
	if old := decodeCase(call(viewerS, "GET", casePath(first), "", "", "")); old.State != cases.Canceled || old.Revision != 2 {
		t.Fatalf("reopen changed the old episode: %+v", old)
	}
	expect(call(leadS, "POST", casePath(first)+"/reopen", uuid.NewString(), `"2"`, reopenBody(2, "Twice")), 409, "CASE_EPISODE_ACTIVE")
	expect(call(leadS, "POST", casePath(second.ID)+"/reopen", uuid.NewString(), `"1"`, reopenBody(1, "Not terminal")), 409, "ILLEGAL_TRANSITION")

	// A trigger after the active episode is canceled opens the next episode by itself.
	expect(call(leadS, "POST", casePath(second.ID)+"/cancel", uuid.NewString(), `"1"`, cancelBody(1, "Duplicate of the earlier alert")), 200, "")
	third, err := trigger(3, 3)
	if err != nil || !third.Opened || third.CaseID == first || third.CaseID == second.ID {
		t.Fatalf("trigger after terminal episodes: %+v %v", third, err)
	}
	if c := decodeCase(call(viewerS, "GET", casePath(third.CaseID), "", "", "")); c.Episode != 3 || c.PreviousCaseID == nil || *c.PreviousCaseID != second.ID || c.ContextSnapshotID != contexts[3].ContextID {
		t.Fatalf("third episode: %+v", c)
	}

	// Resume: only an escalated case, only with a new eligible READY context for the same
	// order and line. The escalation is recorded the way a workflow deadline would record it.
	resumeBody := func(revision int64, contextID string) string {
		return `{"expected_revision":` + strconv.FormatInt(revision, 10) + `,"reason":"New window evidence reviewed","new_context_id":"` + contextID + `"}`
	}
	expect(call(leadS, "POST", casePath(third.CaseID)+"/resume", uuid.NewString(), `"1"`, resumeBody(1, contexts[2].ContextID)), 409, "ILLEGAL_TRANSITION")
	// The episode gets an approved proposal (WIS-274): resume must never regenerate a live
	// approved payload, but an approval whose window has closed does not count. The case moves
	// the way the workflow would record it (WIS-276 owns these transitions): investigating,
	// proposed (where the proposal is submitted at its revision), awaiting_approval (where it is
	// approved; rule 05 allows approval only there or in proposed), then escalated at the 24 h
	// deadline.
	record := func(revision int, from, to, event string) {
		t.Helper()
		tx, e := control.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		for _, q := range []string{
			`SELECT set_config('supervisor.tenant_id','` + tenant + `',true)`,
			`INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,reason,actor_issuer,actor_subject) VALUES('` + tenant + `','` + site + `','` + third.CaseID + `',` + strconv.Itoa(revision) + `,'` + from + `','` + to + `','` + event + `','Recorded by the case workflow','https://supervisor.internal','case-workflow')`,
			`UPDATE supervisor.cases SET state='` + to + `',revision=` + strconv.Itoa(revision) + ` WHERE id='` + third.CaseID + `'`,
		} {
			if _, e = tx.ExecContext(ctx, q); e != nil {
				t.Fatal(e)
			}
		}
		if e = tx.Commit(); e != nil {
			t.Fatal(e)
		}
	}
	record(2, "detected", "investigating", "context_ready")
	record(3, "investigating", "proposed", "proposal_validated")
	approvalsService := approvals.NewService(db.DB)
	submitted, err := approvalsService.Submit(ctx, approvals.Scope{TenantID: tenant, SiteID: site}, approvals.Submission{Payload: approvals.Payload{
		ActionKind: "maintenance_investigation.create", Template: "inspect_report_only",
		AssetID: asset.EntityID, LineID: asset.EntityID, OrderID: order.EntityID, AssigneeID: uuid.NewString(),
		Instructions: "Inspect the press and report findings.", DueAt: time.Now().UTC().Add(4 * time.Hour).Truncate(time.Second),
		CaseID: third.CaseID, CaseRevision: 3, ContextSnapshotID: contexts[3].ContextID, ContextRevision: 1, ContextDigest: contexts[3].Digest,
		ConfigRevision: 1, GraphRevision: 1, PolicyRevision: 1,
		Evidence:       []approvals.Ref{{Kind: "plant_context", ID: contexts[3].ContextID, Revision: 1}},
		GeneratedByRun: uuid.NewString(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	record(4, "proposed", "awaiting_approval", "approval_requested")
	expect(call(leadS, "POST", "/v1/sites/"+site+"/proposals/"+submitted.ID+"/decisions", uuid.NewString(), `"1"`, `{"decision":"approve","expected_revision":1,"reason":"Evidence reviewed"}`), 200, "")
	record(5, "awaiting_approval", "escalated", "wait_deadline")
	expect(call(leadS, "POST", casePath(third.CaseID)+"/resume", uuid.NewString(), `"5"`, resumeBody(5, contexts[3].ContextID)), 409, "TRANSITION_GUARD_FAILED")
	expect(call(leadS, "POST", casePath(third.CaseID)+"/resume", uuid.NewString(), `"5"`, resumeBody(5, uuid.NewString())), 422, "INVALID_CASE_REQUEST")
	expect(call(leadS, "POST", casePath(third.CaseID)+"/resume", uuid.NewString(), `"5"`, resumeBody(5, contexts[2].ContextID)), 409, "TRANSITION_GUARD_FAILED")
	// Review finding: an approval that expired (but whose expiry nobody has recorded yet by
	// reading or deciding it) blocked resume. Only a live or consumed approval counts.
	etx, err := control.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`SET LOCAL session_replication_role = replica`,
		`SELECT set_config('supervisor.tenant_id','` + tenant + `',true)`,
		`UPDATE supervisor.proposal_revisions SET issued_at=issued_at-interval '1 hour',expires_at=expires_at-interval '1 hour' WHERE proposal_id='` + submitted.ID + `'`,
	} {
		if _, err = etx.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err = etx.Commit(); err != nil {
		t.Fatal(err)
	}
	w = call(leadS, "POST", casePath(third.CaseID)+"/resume", uuid.NewString(), `"5"`, resumeBody(5, contexts[2].ContextID))
	expect(w, 200, "")
	if c := decodeCase(w); c.State != cases.Investigating || c.Revision != 6 || c.ContextSnapshotID != contexts[2].ContextID || c.ContextDigest != contexts[2].Digest {
		t.Fatalf("resume: %s", w.Body.String())
	}
	if count(`SELECT count(*) FROM supervisor.case_outbox WHERE case_id=$1 AND kind='signal' AND case_revision=6`, third.CaseID) != 1 {
		t.Fatal("resume did not signal the workflow")
	}
	// Canceling the case marks its still-approved proposal stale; review finding: the stale audit
	// row recorded no actor, though a person canceled the case. It records the canceller.
	expect(call(leadS, "POST", casePath(third.CaseID)+"/cancel", uuid.NewString(), `"6"`, cancelBody(6, "Order withdrawn")), 200, "")
	if n := count(`SELECT count(*) FROM supervisor.approval_audit WHERE proposal_id=$1 AND event_type='proposal.stale' AND actor_issuer=$2 AND actor_subject=$3`, submitted.ID, p.issuer, lead); n != 1 {
		t.Fatalf("stale audit rows attributed to the canceller: %d, want 1", n)
	}
	// Scope revocation takes effect on the next request.
	exec(`UPDATE supervisor.memberships SET active=false WHERE tenant_id=$1 AND site_id=$2 AND subject=$3`, tenant, site, lead)
	expect(call(leadS, "GET", casePath(third.CaseID), "", "", ""), 404, "NOT_FOUND")
}

// WIS-273 automatic triggering: every created context is evaluated once against the shortfall
// rule through the trigger path. On the five-window pipeline the first window has no previous
// window, the second opens the case, the third and fourth join it, and the fifth is below the
// threshold. A second pass evaluates nothing, and a storage failure leaves the failing contexts
// for a later pass while bounded passes resume past them to newer ones.
func TestCaseContextEvaluationHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	p := newShortfallPipeline(t, ctx)
	service := cases.NewService(p.db.DB)
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if e := p.control.QueryRowContext(ctx, q, args...).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	// Review finding (WIS-273): contexts that failed every time were skipped but never recorded,
	// so a page of them at the queue head blocked every newer context. During a storage failure
	// the three contexts that need a trigger record (windows 2-4) fail. WIS-718: a pass budget
	// counts attempts, failures included, so each pass bounded to one context attempts exactly one,
	// and the next pass resumes after it: window 1 (no previous window) is recorded, windows 2-4
	// fail one per pass, and the fifth pass reaches and records window 5 (below threshold).
	p.exec(`REVOKE INSERT ON supervisor.case_triggers FROM supervisor_runtime`)
	var err error
	var cursor *cases.EvaluationCursor
	for i, want := range []int{1, 0, 0, 0, 1} {
		var got []cases.Evaluation
		got, cursor, err = service.EvaluatePass(ctx, p.tenant, cases.EvaluatorActor, cases.EvaluationBudget{Contexts: 1, Records: plantcontext.MaxRecords}, cursor)
		if len(got) != want || (want == 0) != (err != nil) || (want == 1 && got[0].ContextID != p.contexts[i].ContextID) {
			t.Fatalf("bounded pass %d during a storage failure evaluated %v: %v", i, got, err)
		}
	}
	p.exec(`GRANT INSERT ON supervisor.case_triggers TO supervisor_runtime`)
	if n := count(`SELECT count(*) FROM supervisor.case_context_evaluations WHERE context_id IN ($1,$2) AND outcome='not_qualified' AND case_id IS NULL`, p.contexts[0].ContextID, p.contexts[4].ContextID); n != 2 {
		t.Fatalf("storage failure recorded %d of the two contexts that need no trigger", n)
	}
	if n := count(`SELECT count(*) FROM supervisor.case_context_evaluations`); n != 2 {
		t.Fatalf("storage failure recorded %d evaluations, want 2", n)
	}
	// The failed contexts are retried on the next pass, and an unusable tenant entry listed first
	// does not stop the worker's evaluation of this tenant.
	retried, err := dispatch.NewEvaluator(p.db).Evaluate(ctx, []string{"not-a-tenant", p.tenant})
	if err == nil || retried != 3 {
		t.Fatalf("retry pass evaluated %d: %v", retried, err)
	}
	outcomes := map[string]cases.Evaluation{}
	rows, err := p.control.QueryContext(ctx, `SELECT context_id,outcome,coalesce(case_id::text,'') FROM supervisor.case_context_evaluations`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var e cases.Evaluation
		if err = rows.Scan(&e.ContextID, &e.Outcome, &e.CaseID); err != nil {
			t.Fatal(err)
		}
		outcomes[e.ContextID] = e
	}
	if err = rows.Close(); err != nil {
		t.Fatal(err)
	}
	want := []string{cases.OutcomeNotQualified, cases.OutcomeOpened, cases.OutcomeJoined, cases.OutcomeJoined, cases.OutcomeNotQualified}
	caseID := outcomes[p.contexts[1].ContextID].CaseID
	for i, w := range want {
		got := outcomes[p.contexts[i].ContextID]
		if got.Outcome != w {
			t.Fatalf("window %d outcome %q, want %q", i, got.Outcome, w)
		}
		if (w == cases.OutcomeOpened || w == cases.OutcomeJoined) != (got.CaseID != "") || (got.CaseID != "" && got.CaseID != caseID) {
			t.Fatalf("window %d case %q, want %q", i, got.CaseID, caseID)
		}
	}
	if count(`SELECT count(*) FROM supervisor.cases`) != 1 || count(`SELECT count(*) FROM supervisor.case_trigger_evidence WHERE case_id=$1`, caseID) != 3 ||
		count(`SELECT count(*) FROM supervisor.case_outbox WHERE case_id=$1 AND kind='start'`, caseID) != 1 || count(`SELECT count(*) FROM supervisor.case_outbox WHERE case_id=$1 AND kind='signal'`, caseID) != 2 {
		t.Fatal("automatic triggers did not make one case with three records, one start and two signals")
	}
	if n := count(`SELECT count(*) FROM supervisor.case_context_evaluations WHERE actor_subject=$1`, cases.EvaluatorActor.Subject); n != 5 {
		t.Fatalf("recorded %d evaluations, want 5", n)
	}
	again, err := service.EvaluateCreatedContexts(ctx, p.tenant, cases.EvaluatorActor, 100)
	if err != nil || len(again) != 0 {
		t.Fatalf("second pass evaluated %d contexts: %v", len(again), err)
	}
	// The case is readable like any other, with the evaluator as the opening actor.
	w := p.call(p.viewerS, "GET", "/v1/sites/"+p.site+"/cases/"+caseID, "", "", "")
	p.expect(w, 200, "")
	if c := p.decodeCase(w); c.Transitions[0].Actor.Subject != cases.EvaluatorActor.Subject || len(c.TriggerEvidenceIDs) != 3 {
		t.Fatalf("evaluated case: %s", w.Body.String())
	}
	// Another tenant's pass sees none of this tenant's contexts.
	if other, err := service.EvaluateCreatedContexts(ctx, p.otherTenant, cases.EvaluatorActor, 100); err != nil || len(other) != 0 {
		t.Fatalf("other tenant pass: %d %v", len(other), err)
	}
}

// WIS-718: the evaluator's per-tenant budget counts attempts, failures included, and every tenant
// gets its own budget each pass. Tenant A has three contexts that fail every time; tenant B has
// the pipeline's five windows. With a budget of one context, each pass attempts exactly one of A's
// contexts (one failure returned, not three) and still evaluates one of B's; A's next pass
// resumes after the context it attempted and wraps to the oldest after its last. A record budget
// of one stops a tenant's pass after its first context.
func TestCaseEvaluationBudgetHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	p := newShortfallPipeline(t, ctx)
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if e := p.control.QueryRowContext(ctx, q, args...).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	// Outbox rows naming contexts that do not exist: loading each fails on every attempt.
	failing := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	tx, err := p.control.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `SET LOCAL session_replication_role=replica`); err != nil {
		t.Fatal(err)
	}
	for i, id := range failing {
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.plant_context_outbox(tenant_id,site_id,context_id,event_type,created_at) VALUES($1,$2,$3,'context.created',now()+$4*interval '1 second')`, p.otherTenant, p.otherSite, id, i); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// leaves counts the individual failures inside the joined error a pass returns.
	var leaves func(error) int
	leaves = func(err error) int {
		if j, ok := err.(interface{ Unwrap() []error }); ok {
			n := 0
			for _, e := range j.Unwrap() {
				n += leaves(e)
			}
			return n
		}
		if err == nil {
			return 0
		}
		return 1
	}
	recorded := func(tenant string) int {
		return count(`SELECT count(*) FROM supervisor.case_context_evaluations WHERE tenant_id=$1`, tenant)
	}
	ev := dispatch.NewEvaluator(p.db)
	ev.Budget = cases.EvaluationBudget{Contexts: 1, Records: plantcontext.MaxRecords}
	tenants := []string{p.otherTenant, p.tenant}
	for pass := 1; pass <= len(failing); pass++ {
		n, err := ev.Evaluate(ctx, tenants)
		if n != 1 || leaves(err) != 1 {
			t.Fatalf("pass %d evaluated %d and failed %d contexts (%v), want 1 and 1", pass, n, leaves(err), err)
		}
		if got := recorded(p.tenant); got != pass {
			t.Fatalf("after pass %d tenant B recorded %d evaluations", pass, got)
		}
	}
	// A record budget of one stops B's pass after one of its two remaining contexts. A's failing
	// contexts select no records, so A's pass wraps from after its last and attempts all three once.
	ev.Budget = cases.EvaluationBudget{Contexts: 100, Records: 1}
	if n, err := ev.Evaluate(ctx, tenants); n != 1 || leaves(err) != len(failing) || recorded(p.tenant) != 4 {
		t.Fatalf("record-bounded pass evaluated %d and failed %d (%v); tenant B recorded %d, want 1, 3 and 4", n, leaves(err), err, recorded(p.tenant))
	}
	if got := recorded(p.otherTenant); got != 0 {
		t.Fatalf("tenant A's failing contexts recorded %d evaluations", got)
	}
}

// WIS-273 clarifications: a person's answer to a case waiting on input is appended as
// attributed case evidence and returns the case to investigating in one transaction. Only an
// active site supervisor or operator may clarify, only a case in needs_input accepts it, and the
// usual revision, idempotency and tenant rules apply.
func TestCaseClarificationsHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	p := newShortfallPipeline(t, ctx)
	operator := uuid.NewString()
	p.exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'operator')`, p.tenant, p.site, p.issuer, operator)
	operatorS := p.seed(operator, 0)
	service := cases.NewService(p.db.DB)
	opened, err := service.Trigger(ctx, cases.Scope{TenantID: p.tenant, SiteID: p.site}, cases.EvaluatorActor, cases.TriggerInput{SourceID: p.sourceID, SourceEventID: p.contexts[1].DeltaRefs()[0].RecordID, ContextSnapshotID: p.contexts[1].ContextID, ExceptionKind: cases.ExceptionKind})
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/sites/" + p.site + "/cases/" + opened.CaseID + "/clarifications"
	body := func(revision int64, text string) string {
		b, _ := json.Marshal(map[string]any{"expected_revision": revision, "body": text})
		return string(b)
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if e := p.control.QueryRowContext(ctx, q, args...).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	// Review finding: the workflow's ledger reconcile copied undelivered outbox rows too, marking
	// an event processed before any run received it (migration 046). Only delivered rows count,
	// and only in the caller's tenant.
	ref := cases.WorkflowRef{TenantID: p.tenant, SiteID: p.site, CaseID: opened.CaseID}
	ledger := func() int {
		return count(`SELECT count(*) FROM supervisor.case_workflow_events WHERE case_id=$1`, opened.CaseID)
	}
	if added, e := service.ReconcileWorkflowEvents(ctx, ref); e != nil || added != 0 || ledger() != 0 {
		t.Fatalf("reconcile of an undelivered start added %d (ledger %d): %v", added, ledger(), e)
	}
	pending, err := service.PendingOutbox(ctx, p.tenant, 100)
	if err != nil || len(pending) != 1 || pending[0].Ref.CaseID != opened.CaseID || pending[0].Event.Kind != "start" {
		t.Fatalf("pending outbox: %+v %v", pending, err)
	}
	if err = service.MarkDelivered(ctx, pending[0]); err != nil {
		t.Fatal(err)
	}
	foreign := cases.WorkflowRef{TenantID: p.otherTenant, SiteID: p.site, CaseID: opened.CaseID}
	if added, e := service.ReconcileWorkflowEvents(ctx, foreign); !errors.Is(e, cases.ErrNotFound) || added != 0 || ledger() != 0 {
		t.Fatalf("reconcile under another tenant added %d (ledger %d): %v", added, ledger(), e)
	}
	if added, e := service.ReconcileWorkflowEvents(ctx, ref); e != nil || added != 1 ||
		count(`SELECT count(*) FROM supervisor.case_workflow_events WHERE case_id=$1 AND event_id=$2 AND kind='start' AND case_revision=1`, opened.CaseID, pending[0].Event.EventID) != 1 {
		t.Fatalf("reconcile of the delivered start added %d: %v", added, e)
	}
	if added, e := service.ReconcileWorkflowEvents(ctx, ref); e != nil || added != 0 || ledger() != 1 {
		t.Fatalf("second reconcile added %d (ledger %d): %v", added, ledger(), e)
	}

	// waitOnInput records the transition the investigation would make when evidence is missing.
	waitOnInput := func(from string, revision int) {
		t.Helper()
		tx, e := p.control.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		for _, q := range []string{
			`SELECT set_config('supervisor.tenant_id','` + p.tenant + `',true)`,
			`INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,reason,actor_issuer,actor_subject) VALUES('` + p.tenant + `','` + p.site + `','` + opened.CaseID + `',` + strconv.Itoa(revision) + `,'` + from + `','needs_input','evidence_missing','MISSING_ACTUALS','supervisor:temporal','case-workflow')`,
			`UPDATE supervisor.cases SET state='needs_input',revision=` + strconv.Itoa(revision) + ` WHERE id='` + opened.CaseID + `'`,
		} {
			if _, e = tx.ExecContext(ctx, q); e != nil {
				t.Fatal(e)
			}
		}
		if e = tx.Commit(); e != nil {
			t.Fatal(e)
		}
	}

	// A case that is not waiting on input refuses a clarification and records nothing.
	p.expect(p.call(operatorS, "POST", path, uuid.NewString(), `"1"`, body(1, "Material change at 06:10")), 409, "ILLEGAL_TRANSITION")
	waitOnInput("detected", 2)
	text := "Line 1 had a planned material change at 06:10; the 80 units are correct."
	key := uuid.NewString()
	p.expect(p.call(p.viewerS, "POST", path, uuid.NewString(), `"2"`, body(2, text)), 403, "PERMISSION_DENIED")
	p.expect(p.call(p.ownerS, "POST", path, uuid.NewString(), `"2"`, body(2, text)), 403, "PERMISSION_DENIED")
	p.expect(p.call(p.outsiderS, "POST", path, uuid.NewString(), `"2"`, body(2, text)), 404, "NOT_FOUND")
	p.expect(p.call(operatorS, "POST", path, key, `"2"`, body(2, "   ")), 422, "INVALID_CASE_REQUEST")
	p.expect(p.call(operatorS, "POST", path, key, `"2"`, body(2, strings.Repeat("x", cases.MaxClarificationRunes+1))), 422, "INVALID_CASE_REQUEST")
	p.expect(p.call(operatorS, "POST", path, key, `"2"`, `{"expected_revision":2,"body":"x","observation":"edited"}`), 422, "INVALID_CASE_REQUEST")
	p.expect(p.call(operatorS, "POST", path, key, `"1"`, body(2, text)), 422, "INVALID_CASE_REQUEST")
	p.expect(p.call(operatorS, "POST", path, key, `"1"`, body(1, text)), 412, "REVISION_CONFLICT")
	if n := count(`SELECT count(*) FROM supervisor.case_clarifications`); n != 0 {
		t.Fatalf("refused clarifications recorded %d rows", n)
	}
	w := p.call(operatorS, "POST", path, key, `"2"`, body(2, text))
	p.expect(w, 200, "")
	c := p.decodeCase(w)
	sum := sha256.Sum256([]byte(text))
	if c.State != cases.Investigating || c.Revision != 3 || len(c.Clarifications) != 1 || c.Clarifications[0].Body != text || c.Clarifications[0].CaseRevision != 3 ||
		c.Clarifications[0].BodySHA256 != hex.EncodeToString(sum[:]) || c.Clarifications[0].Actor.Subject != operator || c.Clarifications[0].ContextSnapshotID != nil ||
		c.ContextSnapshotID != p.contexts[1].ContextID || c.Transitions[2].Event != "clarified" {
		t.Fatalf("clarification: %s", w.Body.String())
	}
	if replay := p.decodeCase(p.call(operatorS, "POST", path, key, `"2"`, body(2, text))); !replay.Replayed || len(replay.Clarifications) != 1 {
		t.Fatalf("replay: %+v", replay)
	}
	p.expect(p.call(operatorS, "POST", path, key, `"2"`, body(2, "a different answer")), 409, "IDEMPOTENCY_CONFLICT")
	if count(`SELECT count(*) FROM supervisor.case_outbox WHERE case_id=$1 AND kind='signal' AND case_revision=3`, opened.CaseID) != 1 {
		t.Fatal("clarification did not signal the workflow")
	}

	// A supervisor may clarify without a recent sign-in (the stale session), binding a new
	// eligible context for the same order and line; an unknown context is refused.
	waitOnInput("investigating", 4)
	withContext := func(contextID string) string {
		b, _ := json.Marshal(map[string]any{"expected_revision": 4, "body": "Use the next window", "new_context_id": contextID})
		return string(b)
	}
	p.expect(p.call(p.staleS, "POST", path, uuid.NewString(), `"4"`, withContext(uuid.NewString())), 422, "INVALID_CASE_REQUEST")
	w = p.call(p.staleS, "POST", path, uuid.NewString(), `"4"`, withContext(p.contexts[2].ContextID))
	p.expect(w, 200, "")
	if c = p.decodeCase(w); c.State != cases.Investigating || c.Revision != 5 || c.ContextSnapshotID != p.contexts[2].ContextID || c.ContextDigest != p.contexts[2].Digest ||
		len(c.Clarifications) != 2 || c.Clarifications[1].ContextSnapshotID == nil || *c.Clarifications[1].ContextSnapshotID != p.contexts[2].ContextID {
		t.Fatalf("clarification with context: %s", w.Body.String())
	}
	// Review finding: binding an eligible READY context of another order on the line was
	// untested. It is refused as 422 and records nothing; a context that is not eligible is 409.
	waitOnInput("investigating", 6)
	newContext := func(orderID, start, end string) plantcontext.Snapshot {
		t.Helper()
		b, _ := json.Marshal(plantcontext.Request{OrderID: orderID, AssetID: p.asset.EntityID, WindowStart: start, WindowEnd: end, ProcessSnapshotID: p.processSnapshotID})
		w := p.call(p.ownerS, "POST", "/v1/sites/"+p.site+"/contexts", uuid.NewString(), "", string(b))
		p.expect(w, 201, "")
		var v plantcontext.Snapshot
		if e := json.Unmarshal(w.Body.Bytes(), &v); e != nil {
			t.Fatal(e)
		}
		return v
	}
	foreignOrder := newContext(p.otherOrder.EntityID, "2026-09-27T00:00:00Z", "2026-09-27T00:15:00Z")
	if foreignOrder.Readiness != plantcontext.ReadyState || len(foreignOrder.DeltaRefs()) != 1 {
		t.Fatalf("other order context: readiness=%s reasons=%v", foreignOrder.Readiness, foreignOrder.ReasonCodes)
	}
	withContext6 := func(contextID string) string {
		b, _ := json.Marshal(map[string]any{"expected_revision": 6, "body": "Use this window", "new_context_id": contextID})
		return string(b)
	}
	p.expect(p.call(operatorS, "POST", path, uuid.NewString(), `"6"`, withContext6(foreignOrder.ContextID)), 422, "INVALID_CASE_REQUEST")
	unplanned := newContext(p.order.EntityID, "2026-09-27T01:15:00Z", "2026-09-27T01:30:00Z")
	if unplanned.Readiness == plantcontext.ReadyState {
		t.Fatalf("unplanned window is READY: %v", unplanned.ReasonCodes)
	}
	p.expect(p.call(operatorS, "POST", path, uuid.NewString(), `"6"`, withContext6(unplanned.ContextID)), 409, "CONTEXT_NOT_ELIGIBLE")
	if c = p.decodeCase(p.call(p.viewerS, "GET", "/v1/sites/"+p.site+"/cases/"+opened.CaseID, "", "", "")); c.State != cases.NeedsInput || c.Revision != 6 ||
		len(c.Clarifications) != 2 || c.ContextSnapshotID != p.contexts[2].ContextID {
		t.Fatalf("refused context bindings changed the case: %+v", c)
	}
	// History is append-only: the runtime role cannot rewrite a clarification.
	if _, err = p.db.DB.ExecContext(ctx, `UPDATE supervisor.case_clarifications SET body='rewritten'`); err == nil {
		t.Fatal("runtime role rewrote a clarification")
	}
}

// WIS-636: no production code applied context_ready, so a case the evaluator opened stayed
// detected and was never investigated. The worker's sweep now moves it, through the real path and
// no SQL, exactly when the agent is enabled for the deployment; with the agent off it stays
// detected and moves on the first sweep after the agent is turned on.
func TestDetectedCaseStartsInvestigatingHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	p := newShortfallPipeline(t, ctx)
	if _, err := dispatch.NewEvaluator(p.db).Evaluate(ctx, []string{p.tenant}); err != nil {
		t.Fatal(err)
	}
	var caseID string
	if err := p.control.QueryRowContext(ctx, `SELECT id::text FROM supervisor.cases WHERE tenant_id=$1`, p.tenant).Scan(&caseID); err != nil {
		t.Fatalf("the evaluator opened no case: %v", err)
	}
	read := func() cases.CaseV1 {
		t.Helper()
		w := p.call(p.viewerS, "GET", "/v1/sites/"+p.site+"/cases/"+caseID, "", "", "")
		p.expect(w, 200, "")
		return p.decodeCase(w)
	}
	opened := read()
	if opened.State != cases.Detected {
		t.Fatalf("opened case is %s, want detected", opened.State)
	}
	// The sweep's switch comes from the worker's built agent runtime, as the case workflow's does.
	lazy, err := client.NewLazyClient(client.Options{HostPort: "127.0.0.1:1"}) // never dialed
	if err != nil {
		t.Fatal(err)
	}
	defer lazy.Close()
	agentEnabled := func(a config.Agent) bool {
		t.Helper()
		w, err := dispatch.NewWorker(lazy, p.db, config.Config{TemporalQueue: "wis-636-" + uuid.NewString(), Agent: a})
		if err != nil {
			t.Fatal(err)
		}
		return w.AgentEnabled
	}
	signals := func(revision int64) int {
		t.Helper()
		var n int
		if err := p.control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.case_outbox WHERE case_id=$1 AND case_revision=$2 AND kind='signal'`, caseID, revision).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	off := agentEnabled(config.Agent{Provider: config.AgentProviderFixture, ModeEnabled: false})
	// PR #232 review M1: agent mode on with a local provider whose runtime cannot be built (no
	// model) leaves the workflow unable to investigate, so the sweep must not move the case.
	broken := agentEnabled(config.Agent{Provider: config.AgentProviderLocal, ModeEnabled: true})
	for name, enabled := range map[string]bool{"agent off": off, "runtime unavailable": broken} {
		if n, err := dispatch.StartDetectedCases(ctx, p.db, enabled, []string{p.tenant}); err != nil || n != 0 {
			t.Fatalf("%s moved %d cases: %v", name, n, err)
		}
		if c := read(); c.State != cases.Detected || c.Revision != opened.Revision || signals(opened.Revision+1) != 0 {
			t.Fatalf("%s: case %s r%d", name, c.State, c.Revision)
		}
	}
	on := agentEnabled(config.Agent{Provider: config.AgentProviderFixture, ModeEnabled: true})
	if !on {
		t.Fatal("fixture runtime with agent mode on is not enabled")
	}
	// Another tenant's sweep never reaches this tenant's case.
	if n, err := dispatch.StartDetectedCases(ctx, p.db, on, []string{p.otherTenant}); err != nil || n != 0 {
		t.Fatalf("other tenant moved %d: %v", n, err)
	}
	// PR #232 review m2: a detected case that fails on every sweep, ordered first, must not pin the
	// window. It has no transitions, so loading it fails its integrity check.
	stuck := "00000000-0000-0000-0000-000000000001"
	seedStuck := func(q string) {
		t.Helper()
		tx, err := p.control.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `SET LOCAL session_replication_role=replica`); err == nil {
			_, err = tx.ExecContext(ctx, q, p.tenant, p.site, stuck)
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	seedStuck(`INSERT INTO supervisor.cases(tenant_id,site_id,id,line_id,order_id,exception_kind,episode,revision,context_snapshot_id,context_revision,context_digest,state,detected_at,latest_window_end,workflow_id)
		VALUES($1,$2,$3,gen_random_uuid(),gen_random_uuid(),'production_shortfall',1,1,gen_random_uuid(),1,repeat('a',64),'detected',now(),now(),'case:'||$1::uuid::text||':'||$3::uuid::text)`)
	if n, err := cases.NewService(p.db.DB).StartDetected(ctx, p.tenant, on, 1); err == nil || n != 1 {
		t.Fatalf("a failing case pinned the sweep window: moved %d: %v", n, err)
	}
	seedStuck(`DELETE FROM supervisor.cases WHERE tenant_id=$1 AND site_id=$2 AND id=$3`)
	c := read()
	last := c.Transitions[len(c.Transitions)-1]
	if c.State != cases.Investigating || c.Revision != opened.Revision+1 || last.Event != string(cases.ContextReady) || last.FromState != string(cases.Detected) ||
		last.Reason == nil || *last.Reason != cases.ReasonContextReady || last.Actor != cases.SweepActor {
		t.Fatalf("after the sweep: %s r%d last %+v", c.State, c.Revision, last)
	}
	// The move wakes the case workflow: exactly one signal, for the new revision.
	if n := signals(c.Revision); n != 1 {
		t.Fatalf("the sweep queued %d workflow signals for r%d, want 1", n, c.Revision)
	}
	// A second sweep has nothing to do.
	if n, err := dispatch.StartDetectedCases(ctx, p.db, on, []string{p.tenant}); err != nil || n != 0 {
		t.Fatalf("second sweep moved %d: %v", n, err)
	}
}

// Simulator finding (2026-11-05): a second qualifying window for the same line and order joined
// an escalated case with no trace on the case read beyond a trigger-record count. The join
// changes no state (escalated has no system event), so the read must show it as a trigger.
func TestJoinedTriggerVisibleOnEscalatedCaseHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	p := newShortfallPipeline(t, ctx)
	service := cases.NewService(p.db.DB)
	scope := cases.Scope{TenantID: p.tenant, SiteID: p.site}
	trigger := func(window int) (cases.TriggerResult, error) {
		return service.Trigger(ctx, scope, cases.EvaluatorActor, cases.TriggerInput{SourceID: p.sourceID, SourceEventID: p.contexts[window].DeltaRefs()[0].RecordID, ContextSnapshotID: p.contexts[window].ContextID, ExceptionKind: cases.ExceptionKind})
	}
	opened, err := trigger(1)
	if err != nil || !opened.Opened {
		t.Fatalf("open: %+v %v", opened, err)
	}
	// Escalated the way the workflow's wait deadline records it.
	tx, err := p.control.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`SELECT set_config('supervisor.tenant_id','` + p.tenant + `',true)`,
		`INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,reason,actor_issuer,actor_subject) VALUES('` + p.tenant + `','` + p.site + `','` + opened.CaseID + `',2,'detected','escalated','wait_deadline','HUMAN_WAIT_DEADLINE','supervisor:temporal','case-workflow')`,
		`UPDATE supervisor.cases SET state='escalated',revision=2 WHERE id='` + opened.CaseID + `'`,
	} {
		if _, err = tx.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	joined, err := trigger(2)
	if err != nil || joined.CaseID != opened.CaseID || joined.Opened || joined.Revision != 2 {
		t.Fatalf("join: %+v %v", joined, err)
	}
	w := p.call(p.viewerS, "GET", "/v1/sites/"+p.site+"/cases/"+opened.CaseID, "", "", "")
	p.expect(w, 200, "")
	body := w.Body.String()
	// The case stays escalated at revision 2 bound to the opening context; the joining window's
	// context appears only through the trigger ledger.
	var got struct {
		State             string `json:"state"`
		Revision          int64  `json:"revision"`
		ContextSnapshotID string `json:"context_snapshot_id"`
		Triggers          []struct {
			SourceEventID     string                   `json:"source_event_id"`
			ContextSnapshotID string                   `json:"context_snapshot_id"`
			Opened            bool                     `json:"opened"`
			Actor             struct{ Subject string } `json:"actor"`
			CreatedAt         string                   `json:"created_at"`
		} `json:"triggers"`
	}
	if err = json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != "escalated" || got.Revision != 2 || got.ContextSnapshotID != p.contexts[1].ContextID || len(got.Triggers) != 2 {
		t.Fatalf("joined trigger not shown on the escalated case: %s", body)
	}
	first, second := got.Triggers[0], got.Triggers[1]
	if !first.Opened || first.ContextSnapshotID != p.contexts[1].ContextID || first.SourceEventID != p.contexts[1].DeltaRefs()[0].RecordID ||
		second.Opened || second.ContextSnapshotID != p.contexts[2].ContextID || second.SourceEventID != p.contexts[2].DeltaRefs()[0].RecordID ||
		second.Actor.Subject != cases.EvaluatorActor.Subject || second.CreatedAt == "" {
		t.Fatalf("trigger ledger on the case read: %s", body)
	}
	// Another tenant still gets the non-disclosing 404.
	p.expect(p.call(p.outsiderS, "GET", "/v1/sites/"+p.site+"/cases/"+opened.CaseID, "", "", ""), 404, "NOT_FOUND")
}

// TestEvaluateRecordsIneligibleReadyContextNotQualified is the PR #317 (WIS-718) review regression:
// a stored READY context that is no longer eligible when evaluated (its delta was corrected,
// INPUTS_CHANGED; or a newer context superseded it) must be recorded not_qualified with no trigger,
// not returned as an error that every later pass retries.
func TestEvaluateRecordsIneligibleReadyContextNotQualified(t *testing.T) {
	for name, supersede := range map[string]bool{"inputs changed": false, "superseded": true} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
			defer cancel()
			p := newShortfallPipeline(t, ctx)
			service := cases.NewService(p.db.DB)
			target := p.contexts[1]
			p.correctDelta(t, 1, "70", 2)
			if supersede {
				body, _ := json.Marshal(plantcontext.Request{OrderID: p.order.EntityID, AssetID: p.asset.EntityID, WindowStart: target.Window.Start, WindowEnd: target.Window.End, ProcessSnapshotID: p.processSnapshotID})
				w := p.call(p.ownerS, "POST", "/v1/sites/"+p.site+"/contexts", "ctx-1-superseding", "", string(body))
				p.expect(w, 201, "")
				var head plantcontext.Snapshot
				if err := json.Unmarshal(w.Body.Bytes(), &head); err != nil || head.SupersedesContextID == nil || *head.SupersedesContextID != target.ContextID {
					t.Fatalf("new context does not supersede the target: %v", err)
				}
			}
			first, err := service.EvaluateCreatedContexts(ctx, p.tenant, cases.EvaluatorActor, 100)
			if err != nil {
				t.Fatalf("first pass evaluated %d: %v", len(first), err)
			}
			var outcome string
			var hasCase bool
			if err = p.control.QueryRowContext(ctx, `SELECT outcome, case_id IS NOT NULL FROM supervisor.case_context_evaluations WHERE context_id=$1`, target.ContextID).Scan(&outcome, &hasCase); err != nil {
				t.Fatalf("no evaluation recorded for the ineligible context: %v", err)
			}
			var triggers int
			if err = p.control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.case_triggers WHERE context_snapshot_id=$1`, target.ContextID).Scan(&triggers); err != nil {
				t.Fatal(err)
			}
			if outcome != cases.OutcomeNotQualified || hasCase || triggers != 0 {
				t.Fatalf("ineligible context recorded outcome=%q case=%v triggers=%d, want not_qualified with no case or trigger", outcome, hasCase, triggers)
			}
			second, err := service.EvaluateCreatedContexts(ctx, p.tenant, cases.EvaluatorActor, 100)
			if err != nil || len(second) != 0 {
				t.Fatalf("second pass retried: evaluated=%d err=%v", len(second), err)
			}
		})
	}
}
