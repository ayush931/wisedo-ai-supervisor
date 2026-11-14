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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/cases"
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

// WIS-718: case triggering revalidated the whole context once per delta record, re-assembling the
// current and previous windows each time, so one context with n records cost O(n^2) inside the
// serial worker Tick that serves every tenant (about 71 s at n=1000). The context is now validated
// once per evaluation. The test compares one evaluation pass at 64 and 512 records per window:
// linear work grows about 8x, the old quadratic work about 20x. It also checks the rows are what
// per-record triggering wrote: one case, n triggers, n outbox signals, capped evidence.
func TestCaseTriggerCostIsLinearInContextRecordsPostgres(t *testing.T) {
	small, large := triggerPass(t, 64), triggerPass(t, 512)
	if ratio := float64(large) / float64(small); ratio > 12 {
		t.Fatalf("one evaluation pass took %v at 64 records and %v at 512 (x%.1f): growth is not linear", small, large, ratio)
	}
}

// triggerPass builds two consecutive READY shortfall windows of n delta records each through the
// real pipeline (approved plan, authenticated intake, fold, POST /contexts) and returns how long
// one EvaluateCreatedContexts pass takes.
func triggerPass(t *testing.T, n int) time.Duration {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL admin/runtime URLs required")
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	database := "triggercost_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.ExecContext(ctx, "CREATE DATABASE "+database); err != nil {
		t.Fatal(err)
	}
	defer admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)")
	connect := func(raw string) *sql.DB {
		u, _ := url.Parse(raw)
		u.Path = "/" + database
		db, e := sql.Open("pgx", u.String())
		if e != nil {
			t.Fatal(e)
		}
		return db
	}
	control, runtime := connect(adminURL), connect(runtimeURL)
	defer control.Close()
	defer runtime.Close()
	if err = migrate.Apply(ctx, control); err != nil {
		t.Fatal(err)
	}
	db, err := store.New(runtimeURLFor(runtimeURL, database))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		if _, e := control.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	tenant, site := uuid.NewString(), uuid.NewString()
	owner, planner, reviewer := uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, origin := "https://trigger-cost.invalid/"+uuid.NewString(), "http://127.0.0.1:58177"
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'dummy tenant')`, tenant)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'dummy site')`, tenant, site)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner'),($1,$2,$3,$5,'supervisor'),($1,$2,$3,$6,'supervisor')`, tenant, site, issuer, owner, planner, reviewer)
	seed := func(subject string) pipelineSession {
		v := pipelineSession{token: uuid.NewString(), csrf: uuid.NewString()}
		sum := sha256.Sum256([]byte(v.token))
		v.hash = hex.EncodeToString(sum[:])
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, v.hash, subject, issuer, v.csrf)
		return v
	}
	ownerS, plannerS, reviewerS := seed(owner), seed(planner), seed(reviewer)
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "trigger-cost", ClientSecret: "dummy", RedirectURI: origin + "/auth/callback"}, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)
	call := func(actor pipelineSession, method, path, key, revision, body string) *httptest.ResponseRecorder {
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
	expect := func(w *httptest.ResponseRecorder, status int) {
		if w.Code != status {
			t.Fatalf("HTTP %d want %d: %s", w.Code, status, w.Body.String())
		}
	}
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
	register, err := ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Register", Text: "Press 1, Order 7"}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	kn := knowledge.New(runtime)
	ka := knowledge.Actor{Issuer: issuer, Subject: owner, SessionHash: ownerS.hash}
	ks := knowledge.Scope{TenantID: tenant, SiteID: site}
	entity := func(kind, assetKind, name string) knowledge.Entity {
		e, err := kn.CreateEntity(ctx, ka, ks, knowledge.EntityInput{EntityID: uuid.NewString(), Type: kind, AssetKind: assetKind, Name: name, SourceID: register.SourceID, SourceRevision: register.Revision}, uuid.NewString(), 0)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	asset, order := entity("asset", "line", "Press line 1"), entity("order", "", "Order 7")
	inventory, err := kn.ReadInventory(ctx, ka, ks)
	if err != nil {
		t.Fatal(err)
	}
	process, err := kn.CreateSnapshot(ctx, ka, ks, knowledge.SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Approved process cut"}, uuid.NewString(), inventory.Revision)
	if err != nil {
		t.Fatal(err)
	}
	windows := []string{"2026-09-27T00:00:00Z", "2026-09-27T00:15:00Z", "2026-09-27T00:30:00Z"}
	breaks := []production.Interval{}
	for i := 0; i < 2; i++ {
		start, _ := time.Parse(time.RFC3339, windows[i])
		breaks = append(breaks, production.Interval{Start: start.Add(100 * time.Second).Format(time.RFC3339), End: windows[i+1]})
	}
	plan := production.PlanBody{OrderID: order.EntityID, AssetID: asset.EntityID, WindowStart: windows[0], WindowEnd: windows[2], PlanRevision: 1, Unit: "count",
		RateSegments: []production.RateSegment{{Start: windows[0], End: windows[2], TargetGoodUnitsPerSecond: "1"}}, PlannedBreaks: breaks}
	planWire, _ := json.Marshal(map[string]any{"plan": plan, "source_span_ids": []string{span.ID}})
	captured := call(plannerS, "POST", "/v1/sites/"+site+"/plan-revisions", "plan", "", string(planWire))
	expect(captured, 201)
	var pr struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(captured.Body.Bytes(), &pr)
	expect(call(reviewerS, "POST", "/v1/sites/"+site+"/plan-revisions/"+pr.ID+"/reviews", "approve", `"0"`, `{"decision":"approve","reason":"Matches"}`), 200)
	sources := "/v1/sites/" + site + "/operational-sources"
	sourceID := uuid.NewString()
	expect(call(ownerS, "POST", sources, "src", `"0"`, `{"source_id":"`+sourceID+`","type":"event_api","authority_scope":"production_delta,heartbeat,watermark","enabled":true,"access_policy_revision":1,"freshness_seconds":120,"clock_skew_seconds":30}`), 200)
	mappings, _ := json.Marshal(map[string]any{"mappings": []map[string]any{
		{"external_type": "asset", "external_key": "line-01", "entity_id": asset.EntityID, "entity_revision": asset.Revision},
		{"external_type": "order", "external_key": "order-7", "entity_id": order.EntityID, "entity_revision": order.Revision},
	}})
	expect(call(ownerS, "POST", sources+"/"+sourceID+"/mappings", "map", `"1"`, string(mappings)), 200)
	expect(call(reviewerS, "POST", sources+"/"+sourceID+"/reviews", "src-review", `"2"`, `{"verdict":"approved","reason":"ok"}`), 200)
	sequence := int64(0)
	send := func(events ...map[string]any) {
		for _, event := range events {
			sequence++
			event["source_sequence"] = sequence
			event["revision"] = 1
		}
		body, _ := json.Marshal(map[string]any{"schema_version": 1, "source_id": sourceID, "source_revision": 2, "mapping_revision": 2, "events": events})
		expect(call(ownerS, "POST", sources+"/"+sourceID+"/batches", "batch-"+strconv.FormatInt(sequence, 10), "", string(body)), 202)
	}
	now := func() string { return time.Now().UTC().Format(time.RFC3339Nano) }
	folder := ingestion.New(db.DB)
	step := 900 * time.Second / time.Duration(n)
	for i := 0; i < 2; i++ {
		ws, _ := time.Parse(time.RFC3339, windows[i])
		var batch []map[string]any
		for k := 0; k < n; k++ {
			good := "0"
			if k == 0 {
				good = "80" // expected 100: deficit 20 qualifies
			}
			s, e := ws.Add(time.Duration(k)*step), ws.Add(time.Duration(k+1)*step)
			if k == n-1 {
				e = ws.Add(900 * time.Second)
			}
			payload, _ := json.Marshal(production.Delta{OrderID: order.EntityID, AssetID: asset.EntityID, IntervalStart: s.Format(time.RFC3339Nano), IntervalEnd: e.Format(time.RFC3339Nano), GoodUnits: good, Unit: "count"})
			batch = append(batch, map[string]any{"external_key": fmt.Sprintf("d-%d-%d", i, k), "kind": "production_delta", "event_at": windows[i+1], "asset_external_key": "line-01", "order_external_key": "order-7", "payload": json.RawMessage(payload)})
			if len(batch) == 99 {
				send(batch...)
				batch = nil
			}
		}
		batch = append(batch, map[string]any{"external_key": "hb-" + uuid.NewString(), "kind": "heartbeat", "event_at": now(), "payload": map[string]any{"observed_at": now()}})
		send(batch...)
		send(map[string]any{"external_key": "wm-" + uuid.NewString(), "kind": "watermark", "event_at": now(), "payload": map[string]any{"complete_through": windows[i+1], "through_sequence": sequence}})
		for {
			m, e := folder.Fold(ctx, ingestion.SourceRef{TenantID: tenant, SiteID: site, SourceID: sourceID}, 500)
			if e != nil {
				t.Fatal(e)
			}
			if m == 0 {
				break
			}
		}
		body, _ := json.Marshal(plantcontext.Request{OrderID: order.EntityID, AssetID: asset.EntityID, WindowStart: windows[i], WindowEnd: windows[i+1], ProcessSnapshotID: process.SnapshotID})
		w := call(ownerS, "POST", "/v1/sites/"+site+"/contexts", "ctx-"+strconv.Itoa(i), "", string(body))
		expect(w, 201)
		var snap plantcontext.Snapshot
		_ = json.Unmarshal(w.Body.Bytes(), &snap)
		if snap.Readiness != plantcontext.ReadyState || len(snap.DeltaRefs()) != n {
			t.Fatalf("window %d: readiness=%s refs=%d reasons=%v", i, snap.Readiness, len(snap.DeltaRefs()), snap.ReasonCodes)
		}
	}
	service := cases.NewService(db.DB)
	started := time.Now()
	got, err := service.EvaluateCreatedContexts(ctx, tenant, cases.EvaluatorActor, 100)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	var triggers, outbox, evidenceRows, opened int
	for q, dst := range map[string]*int{
		`SELECT count(*) FROM supervisor.case_triggers WHERE tenant_id=$1`:         &triggers,
		`SELECT count(*) FROM supervisor.case_outbox WHERE tenant_id=$1`:           &outbox,
		`SELECT count(*) FROM supervisor.case_trigger_evidence WHERE tenant_id=$1`: &evidenceRows,
		`SELECT count(*) FROM supervisor.cases WHERE tenant_id=$1`:                 &opened,
	} {
		if e := control.QueryRowContext(ctx, q, tenant).Scan(dst); e != nil {
			t.Fatal(e)
		}
	}
	if len(got) != 2 || got[1].Outcome != cases.OutcomeOpened || opened != 1 || triggers != n || outbox != n || evidenceRows != min(n, cases.MaxTriggerEvidence) {
		t.Fatalf("n=%d: outcomes=%+v cases=%d triggers=%d outbox=%d evidence=%d", n, got, opened, triggers, outbox, evidenceRows)
	}
	return elapsed
}
