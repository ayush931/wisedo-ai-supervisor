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
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/dispatch"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.temporal.io/sdk/client"
)

func TestOperationalSourceHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
	database := "operational_http_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	tenant, site, owner, reviewer := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, origin := "https://operational-http.invalid/"+uuid.NewString(), "http://127.0.0.1:58177"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := control.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic operational HTTP test')`, tenant)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'test site')`, tenant, site)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner'),($1,$2,$3,$5,'supervisor')`, tenant, site, issuer, owner, reviewer)
	type session struct{ token, csrf, hash string }
	seed := func(subject string) session {
		t.Helper()
		v := session{token: uuid.NewString(), csrf: uuid.NewString()}
		sum := sha256.Sum256([]byte(v.token))
		v.hash = hex.EncodeToString(sum[:])
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, v.hash, subject, issuer, v.csrf)
		return v
	}
	ownerSession, reviewerSession := seed(owner), seed(reviewer)
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "operational-test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)
	call := func(actor session, method, path, key, revision, csrf, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if actor.token != "" {
			r.AddCookie(&http.Cookie{Name: "supervisor", Value: actor.token})
		}
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", csrf)
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
	status := func(w *httptest.ResponseRecorder, expected int) {
		t.Helper()
		if w.Code != expected {
			t.Fatalf("HTTP %d want %d: %s", w.Code, expected, w.Body.String())
		}
	}
	base := "/v1/sites/" + site + "/operational-sources"
	sourceID := uuid.NewString()
	body := `{"source_id":"` + sourceID + `","type":"event_api","authority_scope":"production_delta","credential_ref":"vault://plant/key","enabled":true,"access_policy_revision":1,"freshness_seconds":120,"clock_skew_seconds":30}`
	status(call(session{}, "POST", base, uuid.NewString(), `"0"`, ownerSession.csrf, body), 401)
	status(call(ownerSession, "POST", base, uuid.NewString(), `"0"`, "", body), 403)
	status(call(ownerSession, "POST", base, uuid.NewString(), `"0"`, ownerSession.csrf, strings.Replace(body, `"type":"event_api"`, `"type":"json","type":"event_api"`, 1)), 422)
	created := call(ownerSession, "POST", base, "source-create", `"0"`, ownerSession.csrf, body)
	status(created, 200)
	if created.Header().Get("ETag") != `"1"` || created.Header().Get("X-Mapping-ETag") != `"1"` {
		t.Fatal("source response omitted revision validators")
	}
	if strings.Contains(created.Body.String(), "vault://plant/key") {
		t.Fatal("API disclosed secret-store path")
	}
	var source struct {
		Revision        int64 `json:"revision"`
		MappingRevision int64 `json:"mapping_revision"`
	}
	if err = json.Unmarshal(created.Body.Bytes(), &source); err != nil || source.Revision != 1 || source.MappingRevision != 1 {
		t.Fatalf("source response=%s err=%v", created.Body.String(), err)
	}
	status(call(ownerSession, "POST", base, "source-create", `"0"`, ownerSession.csrf, body), 200)
	wantProblem(t, call(ownerSession, "POST", base, "source-create", `"0"`, ownerSession.csrf, strings.Replace(body, "production_delta", "other", 1)), 409, "IDEMPOTENCY_CONFLICT")
	wantProblem(t, call(ownerSession, "POST", base, "stale-source", `"8"`, ownerSession.csrf, body), 412, "REVISION_CONFLICT")
	read := call(ownerSession, "GET", base+"/"+sourceID, "", "", "", "")
	status(read, 200)
	if read.Header().Get("X-Mapping-ETag") != `"1"` {
		t.Fatal("source read omitted mapping revision validator")
	}
	status(call(ownerSession, "GET", base+"/"+sourceID+"?revision=01", "", "", "", ""), 422)
	proof, err := evidence.New(runtime).CreateOrRevise(ctx, evidence.Actor{Issuer: issuer, Subject: owner, SessionHash: ownerSession.hash}, evidence.Scope{TenantID: tenant, SiteID: site}, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Synthetic asset identity", Text: "Press 1 asset identity"}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	entityID := uuid.NewString()
	entity, err := knowledge.New(runtime).CreateEntity(ctx, knowledge.Actor{Issuer: issuer, Subject: owner, SessionHash: ownerSession.hash}, knowledge.Scope{TenantID: tenant, SiteID: site}, knowledge.EntityInput{EntityID: entityID, Type: "asset", AssetKind: "machine", Name: "Press 1", SourceID: proof.SourceID, SourceRevision: proof.Revision}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	mappingBody, err := json.Marshal(map[string]any{"mappings": []map[string]any{{"external_type": "asset", "external_key": "press-01", "entity_id": entityID, "entity_revision": entity.Revision}}})
	if err != nil {
		t.Fatal(err)
	}
	mapped := call(ownerSession, "POST", base+"/"+sourceID+"/mappings", "mapping-create", `"1"`, ownerSession.csrf, string(mappingBody))
	status(mapped, 200)
	if mapped.Header().Get("ETag") != `"2"` || mapped.Header().Get("X-Source-ETag") != `"2"` {
		t.Fatal("mapping response omitted revision validators")
	}
	configRev := call(ownerSession, "POST", base, "source-revise", `"2"`, ownerSession.csrf, strings.Replace(body, "production_delta", "production_delta_v2", 1))
	status(configRev, 200)
	if configRev.Header().Get("ETag") != `"3"` || configRev.Header().Get("X-Mapping-ETag") != `"2"` {
		t.Fatal("source and mapping revisions were not exposed separately")
	}
	remapped := call(ownerSession, "POST", base+"/"+sourceID+"/mappings", "mapping-revise", `"2"`, ownerSession.csrf, string(mappingBody))
	status(remapped, 200)
	if remapped.Header().Get("ETag") != `"3"` || remapped.Header().Get("X-Source-ETag") != `"4"` {
		t.Fatal("mapping mutation validators do not identify mapping and source revisions")
	}
	firstReview := call(reviewerSession, "POST", base+"/"+sourceID+"/reviews", "review", `"4"`, reviewerSession.csrf, `{"verdict":"approved","reason":"Scope and access policy verified"}`)
	status(firstReview, 200)
	if firstReview.Header().Get("ETag") != `"4"` || firstReview.Header().Get("X-Review-ETag") != `"1"` {
		t.Fatal("review response did not distinguish source and review revisions")
	}
	secondReview := call(reviewerSession, "POST", base+"/"+sourceID+"/reviews", "review-again", `"4"`, reviewerSession.csrf, `{"verdict":"approved","reason":"Reconfirmed scope and access policy"}`)
	status(secondReview, 200)
	if secondReview.Header().Get("ETag") != `"4"` || secondReview.Header().Get("X-Review-ETag") != `"2"` {
		t.Fatal("second review rejected a current source revision")
	}
	approved := call(ownerSession, "GET", base+"/"+sourceID, "", "", "", "")
	status(approved, 200)
	if !strings.Contains(approved.Body.String(), `"review_eligible":true`) {
		t.Fatalf("review not pinned and eligible: %s", approved.Body.String())
	}
	intake := base + "/" + sourceID + "/batches"
	status(call(session{}, "POST", intake, "intake", "", ownerSession.csrf, `{}`), 401)
	status(call(ownerSession, "POST", intake, "intake", "", "", `{}`), 403)
	status(call(ownerSession, "POST", intake, "", "", ownerSession.csrf, `{}`), 422)
	eventAt := time.Now().UTC().Format(time.RFC3339Nano)
	batch := `{"schema_version":1,"source_id":"` + sourceID + `","source_revision":4,"mapping_revision":3,"events":[{"external_key":"machine-state-1","revision":1,"kind":"machine_state","source_sequence":1,"event_at":"` + eventAt + `","asset_external_key":"press-01","payload":{"state":"running"}}]}`
	accepted := call(ownerSession, "POST", intake, "batch-1", "", ownerSession.csrf, batch)
	status(accepted, 202)
	var acceptedBatch struct {
		SourceRevision  int64 `json:"source_revision"`
		MappingRevision int64 `json:"mapping_revision"`
		EventCount      int   `json:"event_count"`
		Replayed        bool  `json:"replayed"`
	}
	if err = json.Unmarshal(accepted.Body.Bytes(), &acceptedBatch); err != nil || acceptedBatch.SourceRevision != 4 || acceptedBatch.MappingRevision != 3 || acceptedBatch.EventCount != 1 || acceptedBatch.Replayed {
		t.Fatalf("batch intake result=%s err=%v", accepted.Body.String(), err)
	}
	replay := call(ownerSession, "POST", intake, "batch-1", "", ownerSession.csrf, batch)
	status(replay, 202)
	if err = json.Unmarshal(replay.Body.Bytes(), &acceptedBatch); err != nil || !acceptedBatch.Replayed {
		t.Fatalf("batch replay result=%s err=%v", replay.Body.String(), err)
	}
	wantProblem(t, call(ownerSession, "POST", intake, "batch-1", "", ownerSession.csrf, strings.Replace(batch, "running", "stopped", 1)), 409, "IDEMPOTENCY_CONFLICT")
	gap := strings.Replace(batch, `"source_sequence":1`, `"source_sequence":3`, 1)
	wantProblem(t, call(ownerSession, "POST", intake, "batch-gap", "", ownerSession.csrf, gap), 409, "SOURCE_SEQUENCE_CONFLICT")
	status(call(ownerSession, "POST", intake, "batch-duplicate-key", "", ownerSession.csrf, strings.Replace(batch, `"kind":"machine_state"`, `"kind":"machine_state","kind":"machine_state"`, 1)), 422)
	// Same external identity/revision with different content is a content conflict, not a replay.
	wantProblem(t, call(ownerSession, "POST", intake, "batch-2", "", ownerSession.csrf, strings.Replace(strings.Replace(batch, `"source_sequence":1`, `"source_sequence":2`, 1), "running", "stopped", 1)), 409, "EVENT_REVISION_CONFLICT")
	// A malformed plan payload is a format error before persistence: nothing is stored.
	malformedPlan := `{"schema_version":1,"source_id":"` + sourceID + `","source_revision":4,"mapping_revision":3,"events":[{"external_key":"plan-1","revision":1,"kind":"production_plan","source_sequence":2,"event_at":"` + eventAt + `","payload":{"approved":true}}]}`
	status(call(ownerSession, "POST", intake, "batch-malformed-plan", "", ownerSession.csrf, malformedPlan), 422)
	// A well-formed plan and an unmapped asset are durably accepted but quarantined; they
	// never advance completeness. This source's reviewed scope covers production_delta
	// only, so the plan is OUT_OF_SCOPE before mappings or approvals are even considered.
	wellFormedPlan := `{"order_id":"` + uuid.NewString() + `","asset_id":"` + entityID + `","window_start":"2026-09-27T00:00:00Z","window_end":"2026-09-27T00:15:00Z","plan_revision":1,"approval_ref":"` + uuid.NewString() + `","unit":"count","rate_segments":[{"start":"2026-09-27T00:00:00Z","end":"2026-09-27T00:15:00Z","target_good_units_per_second":"1"}],"planned_breaks":[]}`
	quarantine := `{"schema_version":1,"source_id":"` + sourceID + `","source_revision":4,"mapping_revision":3,"events":[{"external_key":"plan-1","revision":1,"kind":"production_plan","source_sequence":2,"event_at":"` + eventAt + `","asset_external_key":"press-01","payload":` + wellFormedPlan + `},{"external_key":"machine-state-2","revision":1,"kind":"machine_state","source_sequence":3,"event_at":"` + eventAt + `","asset_external_key":"press-99","payload":{"state":"running"}}]}`
	status(call(ownerSession, "POST", intake, "batch-quarantine", "", ownerSession.csrf, quarantine), 202)
	// Identical retransmit at a new sequence is a durable DUPLICATE_EVENT quarantine, never a conflict.
	status(call(ownerSession, "POST", intake, "batch-retransmit", "", ownerSession.csrf, strings.Replace(batch, `"source_sequence":1`, `"source_sequence":4`, 1)), 202)
	status(call(ownerSession, "POST", intake, "  padded ", "", ownerSession.csrf, batch), 422)
	var head int64
	var reasons, inboxStates, outbox string
	if err = control.QueryRowContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,false)`, tenant).Scan(new(string)); err != nil {
		t.Fatal(err)
	}
	if err = control.QueryRowContext(ctx, `SELECT (SELECT last_sequence FROM supervisor.operational_event_sequence_heads WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3),(SELECT string_agg(coalesce(quarantine_reason,'-'),',' ORDER BY source_sequence) FROM supervisor.operational_event_records WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3),(SELECT string_agg(state,',' ORDER BY source_sequence) FROM supervisor.operational_event_inbox WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3),(SELECT string_agg(event_type,',' ORDER BY source_sequence) FROM supervisor.operational_event_outbox WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3)`, tenant, site, sourceID).Scan(&head, &reasons, &inboxStates, &outbox); err != nil {
		t.Fatal(err)
	}
	if head != 4 || reasons != "-,OUT_OF_SCOPE,UNKNOWN_MAPPING,DUPLICATE_EVENT" || inboxStates != "pending,quarantined,quarantined,quarantined" || outbox != strings.Repeat("operational.event.received,", 3)+"operational.event.received" {
		t.Fatalf("durable intake state head=%d reasons=%s inbox=%s outbox=%s", head, reasons, inboxStates, outbox)
	}
	if _, err = control.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id','',false)`); err != nil {
		t.Fatal(err)
	}
	// Outbox consumption: nothing is projected until the worker folds the outbox.
	liveness := intake[:len(intake)-len("/batches")] + "/liveness"
	status(call(ownerSession, "GET", liveness+"?x=1", "", "", "", ""), 422)
	status(call(ownerSession, "GET", base+"/"+uuid.NewString()+"/liveness", "", "", "", ""), 404)
	var live struct {
		Readiness string   `json:"readiness"`
		Reasons   []string `json:"reasons"`
		Ingest    struct {
			Accepted  int64  `json:"accepted_sequence"`
			Processed *int64 `json:"processed_sequence"`
		} `json:"ingest"`
		Liveness     *json.RawMessage `json:"liveness"`
		Completeness *struct {
			Complete  int64  `json:"complete_sequence"`
			FirstQ    *int64 `json:"first_quarantined_sequence"`
			Watermark *int64 `json:"watermark_sequence"`
		} `json:"completeness"`
	}
	unprocessed := call(ownerSession, "GET", liveness, "", "", "", "")
	status(unprocessed, 200)
	if err = json.Unmarshal(unprocessed.Body.Bytes(), &live); err != nil || live.Readiness != "NOT_READY" || strings.Join(live.Reasons, ",") != "NOT_PROCESSED" || live.Ingest.Accepted != 4 || live.Ingest.Processed != nil || live.Liveness != nil || live.Completeness != nil {
		t.Fatalf("unprocessed liveness=%s err=%v", unprocessed.Body.String(), err)
	}
	if unprocessed.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("liveness must not be cacheable: %q", unprocessed.Header().Get("Cache-Control"))
	}
	folder := ingestion.New(db.DB)
	ref := ingestion.SourceRef{TenantID: tenant, SiteID: site, SourceID: sourceID}
	if n, e := folder.Fold(ctx, ref, 500); e != nil || n != 4 {
		t.Fatalf("fold n=%d err=%v", n, e)
	}
	if n, e := folder.Fold(ctx, ref, 500); e != nil || n != 0 {
		t.Fatalf("second fold must be a no-op: n=%d err=%v", n, e)
	}
	folded := call(ownerSession, "GET", liveness, "", "", "", "")
	status(folded, 200)
	if err = json.Unmarshal(folded.Body.Bytes(), &live); err != nil || live.Readiness != "NOT_READY" || strings.Join(live.Reasons, ",") != "NO_HEARTBEAT,QUARANTINE_GAP,NO_WATERMARK" || live.Ingest.Processed == nil || *live.Ingest.Processed != 4 || live.Completeness == nil || live.Completeness.Complete != 1 || live.Completeness.FirstQ == nil || *live.Completeness.FirstQ != 2 {
		t.Fatalf("folded liveness=%s err=%v", folded.Body.String(), err)
	}
	// A heartbeat and watermark behind the quarantine gap improve liveness but never completeness.
	// A heartbeat reports the source's current clock; stamping it with the test's start time made a
	// slow run (>30s skew) read CLOCK_SUSPECT. The watermark still covers the earlier events.
	signalAt := time.Now().UTC().Format(time.RFC3339Nano)
	signals := `{"schema_version":1,"source_id":"` + sourceID + `","source_revision":4,"mapping_revision":3,"events":[{"external_key":"hb-1","revision":1,"kind":"heartbeat","source_sequence":5,"event_at":"` + signalAt + `","payload":{"ok":true}},{"external_key":"wm-1","revision":1,"kind":"watermark","source_sequence":6,"event_at":"` + signalAt + `","payload":{"through":"` + eventAt + `"}}]}`
	status(call(ownerSession, "POST", intake, "batch-signals", "", ownerSession.csrf, signals), 202)
	if n, e := folder.Fold(ctx, ref, 500); e != nil || n != 2 {
		t.Fatalf("signal fold n=%d err=%v", n, e)
	}
	signalled := call(ownerSession, "GET", liveness, "", "", "", "")
	status(signalled, 200)
	if err = json.Unmarshal(signalled.Body.Bytes(), &live); err != nil || live.Readiness != "NOT_READY" || strings.Join(live.Reasons, ",") != "QUARANTINE_GAP,NO_WATERMARK" || live.Completeness == nil || live.Completeness.Complete != 1 || live.Completeness.Watermark != nil || live.Liveness == nil {
		t.Fatalf("signalled liveness=%s err=%v", signalled.Body.String(), err)
	}
	var revisions int
	if err = control.QueryRowContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,false)`, tenant).Scan(new(string)); err != nil {
		t.Fatal(err)
	}
	if err = control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.operational_source_progress_revisions WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, tenant, site, sourceID).Scan(&revisions); err != nil || revisions != 3 {
		t.Fatalf("progress revisions=%d err=%v (initial + two folds expected)", revisions, err)
	}
	// The guard rejects any advance that is not exactly the next contiguous durable range or that skips a revision.
	for _, bad := range []string{
		`UPDATE supervisor.operational_source_progress_heads SET processed_sequence=processed_sequence+1,current_revision=current_revision+1 WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`,
		`UPDATE supervisor.operational_source_progress_heads SET current_revision=current_revision+2 WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`,
		`UPDATE supervisor.operational_source_progress_heads SET processed_sequence=processed_sequence-1,current_revision=current_revision+1 WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`,
	} {
		if _, e := control.ExecContext(ctx, bad, tenant, site, sourceID); e == nil {
			t.Fatalf("progress guard accepted an invalid advance: %s", bad)
		}
	}
	if _, err = control.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id','',false)`); err != nil {
		t.Fatal(err)
	}
	// Another tenant's runtime session sees no progress rows and cannot create any.
	if _, err = runtime.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,false)`, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	var foreign int
	if err = runtime.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.operational_source_progress_heads h JOIN supervisor.operational_source_progress_revisions r ON (r.tenant_id,r.site_id,r.source_id)=(h.tenant_id,h.site_id,h.source_id)`).Scan(&foreign); err != nil || foreign != 0 {
		t.Fatalf("cross-tenant progress visible: count=%d err=%v", foreign, err)
	}
	if _, e := runtime.ExecContext(ctx, `INSERT INTO supervisor.operational_source_progress_heads(tenant_id,site_id,source_id,processed_sequence,current_revision) VALUES($1,$2,$3,0,1)`, tenant, site, sourceID); e == nil {
		t.Fatal("cross-tenant progress insert must be denied")
	}
	if _, err = runtime.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id','',false)`); err != nil {
		t.Fatal(err)
	}
	if err = control.QueryRowContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,false)`, tenant).Scan(new(string)); err != nil {
		t.Fatal(err)
	}
	if _, err = control.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id','',false)`); err != nil {
		t.Fatal(err)
	}
	// Durable relay through actual Temporal when a service is provided (CASE=all).
	if address := os.Getenv("SUPERVISOR_TEST_TEMPORAL_ADDRESS"); address != "" {
		tc, e := client.Dial(client.Options{HostPort: address, Namespace: "default"})
		if e != nil {
			t.Fatal(e)
		}
		defer tc.Close()
		wcfg := config.Config{TemporalQueue: "operational-progress-test-" + uuid.NewString(), Tenants: []string{tenant}}
		w, e := dispatch.NewWorker(tc, db, wcfg)
		if e == nil {
			e = w.Start()
		}
		if e != nil {
			t.Fatal(e)
		}
		defer w.Stop()
		if n, e := dispatch.RelayOperational(ctx, db, tc, wcfg, wcfg.Tenants); e != nil || n != 0 {
			t.Fatalf("relay with a current cursor must signal nothing: n=%d err=%v", n, e)
		}
		status(call(ownerSession, "POST", intake, "batch-relay", "", ownerSession.csrf, strings.Replace(strings.Replace(batch, `"source_sequence":1`, `"source_sequence":7`, 1), "machine-state-1", "machine-state-7", 1)), 202)
		if n, e := dispatch.RelayOperational(ctx, db, tc, wcfg, wcfg.Tenants); e != nil || n != 1 {
			t.Fatalf("relay must signal the lagging source once: n=%d err=%v", n, e)
		}
		deadline := time.Now().Add(30 * time.Second)
		for {
			relayed := call(ownerSession, "GET", liveness, "", "", "", "")
			status(relayed, 200)
			if err = json.Unmarshal(relayed.Body.Bytes(), &live); err != nil {
				t.Fatal(err)
			}
			if live.Ingest.Processed != nil && *live.Ingest.Processed == 7 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("worker did not fold the relayed outbox: %s", relayed.Body.String())
			}
			time.Sleep(200 * time.Millisecond)
		}
		if n, e := dispatch.RelayOperational(ctx, db, tc, wcfg, wcfg.Tenants); e != nil || n != 0 {
			t.Fatalf("relay after fold must signal nothing: n=%d err=%v", n, e)
		}
	}
	// Concurrent workers folding the same source: exactly one advances, the other is a no-op, neither errors.
	var before, after int
	if err = control.QueryRowContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,false)`, tenant).Scan(new(string)); err != nil {
		t.Fatal(err)
	}
	if err = control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.operational_source_progress_revisions WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, tenant, site, sourceID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	var nextSeq int64
	if err = control.QueryRowContext(ctx, `SELECT last_sequence+1 FROM supervisor.operational_event_sequence_heads WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, tenant, site, sourceID).Scan(&nextSeq); err != nil {
		t.Fatal(err)
	}
	status(call(ownerSession, "POST", intake, "batch-concurrent", "", ownerSession.csrf, strings.Replace(strings.Replace(batch, `"source_sequence":1`, `"source_sequence":`+strconv.FormatInt(nextSeq, 10), 1), "machine-state-1", "machine-state-concurrent", 1)), 202)
	results := make(chan error, 2)
	counts := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			n, e := folder.Fold(ctx, ref, 500)
			counts <- n
			results <- e
		}()
	}
	total := 0
	for i := 0; i < 2; i++ {
		if e := <-results; e != nil {
			t.Fatalf("concurrent fold errored: %v", e)
		}
		total += <-counts
	}
	if err = control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.operational_source_progress_revisions WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, tenant, site, sourceID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if total != 1 || after != before+1 {
		t.Fatalf("concurrent fold: folded=%d revisions before=%d after=%d", total, before, after)
	}
	if _, err = control.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id','',false)`); err != nil {
		t.Fatal(err)
	}
	wantProblem(t, call(ownerSession, "POST", base+"/"+sourceID+"/mappings", "stale-map", `"1"`, ownerSession.csrf, string(mappingBody)), 412, "REVISION_CONFLICT")
	status(call(ownerSession, "GET", "/v1/sites/"+uuid.NewString()+"/operational-sources/"+sourceID, "", "", "", ""), 404)
}

// runtimeURLFor substitutes only the database path while preserving its credentials and TLS options.
func runtimeURLFor(raw, database string) string {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	u.Path = "/" + database
	return u.String()
}
