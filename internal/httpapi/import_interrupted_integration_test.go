//go:build integration

package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
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

// WIS-719: an operational import runs inside its HTTP request. When that request is interrupted
// (client disconnect, proxy timeout), the job must end at once; before the fix it stayed 'running'
// and every live batch and import for the source got 409 IMPORT_IN_PROGRESS until the 15-minute
// lease passed (and, before WIS-699's lease, until the uploader retried or the source was revoked).
func TestInterruptedImportDoesNotBlockItsSourcePostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	database := "v02_stuck_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
		t.Helper()
		if _, e := control.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	tenant, site := uuid.NewString(), uuid.NewString()
	owner, reviewer, operator := uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, origin := "https://h2.invalid/"+uuid.NewString(), "http://127.0.0.1:58177"
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic h2')`, tenant)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'h2 site')`, tenant, site)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner'),($1,$2,$3,$5,'supervisor'),($1,$2,$3,$6,'operator')`, tenant, site, issuer, owner, reviewer, operator)
	type session struct{ token, csrf, hash string }
	seed := func(subject string) session {
		v := session{token: uuid.NewString(), csrf: uuid.NewString()}
		sum := sha256.Sum256([]byte(v.token))
		v.hash = hex.EncodeToString(sum[:])
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, v.hash, subject, issuer, v.csrf)
		return v
	}
	ownerS, reviewerS, operatorS := seed(owner), seed(reviewer), seed(operator)
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "h2", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)
	ev := evidence.New(runtime)
	source, err := ev.CreateOrRevise(ctx, evidence.Actor{Issuer: issuer, Subject: owner, SessionHash: ownerS.hash}, evidence.Scope{TenantID: tenant, SiteID: site}, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Asset list", Text: "Press 1"}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	kn := knowledge.New(runtime)
	asset, err := kn.CreateEntity(ctx, knowledge.Actor{Issuer: issuer, Subject: owner, SessionHash: ownerS.hash}, knowledge.Scope{TenantID: tenant, SiteID: site}, knowledge.EntityInput{EntityID: uuid.NewString(), Type: "asset", AssetKind: "machine", Name: "Press 1", SourceID: source.SourceID, SourceRevision: source.Revision}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	call := func(rctx context.Context, actor session, method, path, key, body, contentType string, ifMatch ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(rctx)
		r.AddCookie(&http.Cookie{Name: "supervisor", Value: actor.token})
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", actor.csrf)
		r.Header.Set("Content-Type", contentType)
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		for _, tag := range ifMatch {
			r.Header.Set("If-Match", tag)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	upload := func(rctx context.Context, actor session, sourceID, key string, file []byte) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("format", "csv")
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", `form-data; name="file"; filename="import.csv"`)
		h.Set("Content-Type", "text/csv")
		fw, _ := mw.CreatePart(h)
		_, _ = fw.Write(file)
		_ = mw.Close()
		return call(rctx, actor, "POST", "/v1/sites/"+site+"/operational-sources/"+sourceID+"/imports", key, buf.String(), mw.FormDataContentType())
	}
	sources := "/v1/sites/" + site + "/operational-sources"
	sourceID := uuid.NewString()
	must := func(w *httptest.ResponseRecorder, status int) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("HTTP %d want %d: %s", w.Code, status, w.Body.String())
		}
	}
	must(call(ctx, ownerS, "POST", sources, "src", fmt.Sprintf(`{"source_id":%q,"type":"csv","authority_scope":"machine_state,production_delta","enabled":true,"access_policy_revision":1,"freshness_seconds":120,"clock_skew_seconds":30}`, sourceID), "application/json", `"0"`), 200)
	mapping, _ := json.Marshal(map[string]any{"mappings": []map[string]any{{"external_type": "asset", "external_key": "press-01", "entity_id": asset.EntityID, "entity_revision": asset.Revision}}})
	must(call(ctx, ownerS, "POST", sources+"/"+sourceID+"/mappings", "map", string(mapping), "application/json", `"1"`), 200)
	must(call(ctx, reviewerS, "POST", sources+"/"+sourceID+"/reviews", "rev", `{"verdict":"approved","reason":"Scope verified"}`, "application/json", `"2"`), 200)

	at := "2026-09-27T00:00:00Z"
	var csvRows strings.Builder
	csvRows.WriteString("external_key,revision,kind,event_at,asset_external_key,order_external_key,payload_json\n")
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&csvRows, "m-%d,1,machine_state,%s,press-01,,{}\n", i, at)
	}
	file := []byte(csvRows.String())

	// The operator uploads and the request is cancelled (client disconnect) after the job exists.
	rctx, rcancel := context.WithCancel(ctx)
	go func() {
		for {
			var n int
			_ = control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.operational_import_rows WHERE tenant_id=$1`, tenant).Scan(&n)
			if n >= 100 {
				rcancel()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	w := upload(rctx, operatorS, sourceID, "op-import", file)
	if w.Code < 500 {
		t.Fatalf("interrupted upload: HTTP %d %s", w.Code, w.Body.String())
	}
	var status string
	var rows int
	if err := control.QueryRowContext(ctx, `SELECT status FROM supervisor.operational_import_jobs WHERE tenant_id=$1 AND idempotency_key='op-import'`, tenant).Scan(&status); err != nil || status != "cancelled" {
		t.Fatalf("interrupted job status=%q err=%v, want cancelled", status, err)
	}
	_ = control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.operational_import_rows WHERE tenant_id=$1`, tenant).Scan(&rows)
	if rows < 100 || rows >= 5000 {
		t.Fatalf("recorded rows=%d, want the rows recorded before the interruption", rows)
	}
	// The interrupted job's counts match what it stored: every ingested event has an outcome.
	var jobID string
	var accepted, outcomes, events int
	if err := control.QueryRowContext(ctx, `SELECT j.id::text,j.accepted_rows,(SELECT count(*) FROM supervisor.operational_import_rows r WHERE r.job_id=j.id AND r.status<>'rejected'),
 (SELECT count(*) FROM supervisor.operational_event_records e WHERE e.tenant_id=j.tenant_id AND e.source_id=j.source_id)
 FROM supervisor.operational_import_jobs j WHERE j.tenant_id=$1 AND j.idempotency_key='op-import'`, tenant).Scan(&jobID, &accepted, &outcomes, &events); err != nil {
		t.Fatal(err)
	}
	if accepted != outcomes || outcomes != events {
		t.Fatalf("interrupted job accepted_rows=%d outcomes=%d stored_events=%d, want all equal", accepted, outcomes, events)
	}
	// A live batch at the next sequence and current revisions is accepted, for the owner and the
	// supervisor alike, and its event is stored.
	live := func(actor session, key string) {
		t.Helper()
		var srcRev, mapRev, last int64
		if err := control.QueryRowContext(ctx, `SELECT r.revision,r.mapping_revision,s.last_sequence FROM supervisor.operational_source_heads h
 JOIN supervisor.operational_source_revisions r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=(h.tenant_id,h.site_id,h.source_id,h.current_revision)
 JOIN supervisor.operational_event_sequence_heads s ON (s.tenant_id,s.site_id,s.source_id)=(h.tenant_id,h.site_id,h.source_id)
 WHERE h.tenant_id=$1 AND h.source_id=$2`, tenant, sourceID).Scan(&srcRev, &mapRev, &last); err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`{"schema_version":1,"source_id":%q,"source_revision":%d,"mapping_revision":%d,"events":[{"external_key":%q,"revision":1,"kind":"machine_state","source_sequence":%d,"event_at":%q,"payload":{"state":"running"},"asset_external_key":"press-01"}]}`, sourceID, srcRev, mapRev, key, last+1, at)
		must(call(ctx, actor, "POST", sources+"/"+sourceID+"/batches", key, body, "application/json"), 202)
		var stored int
		if err := control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.operational_event_records WHERE tenant_id=$1 AND source_id=$2 AND external_key=$3 AND source_sequence=$4`, tenant, sourceID, key, last+1).Scan(&stored); err != nil || stored != 1 {
			t.Fatalf("live batch %s: stored=%d err=%v, want its event at sequence %d", key, stored, err, last+1)
		}
	}
	live(ownerS, "live-owner")
	live(reviewerS, "live-supervisor")
	ow := upload(ctx, ownerS, sourceID, "owner-import", []byte("external_key,revision,kind,event_at,asset_external_key,order_external_key,payload_json\nx-1,1,machine_state,"+at+",press-01,,{}\n"))
	if ow.Code != 202 {
		t.Fatalf("owner import after the interrupted import: HTTP %d %s", ow.Code, ow.Body.String())
	}
	// The uploader's retry with the same key replays the cancelled job unchanged; it does not resume it.
	var rowsBefore, eventsBefore, rowsAfter, eventsAfter int
	countAll := func(rows, events *int) {
		t.Helper()
		if err := control.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM supervisor.operational_import_rows WHERE job_id=$2),(SELECT count(*) FROM supervisor.operational_event_records WHERE tenant_id=$1)`, tenant, jobID).Scan(rows, events); err != nil {
			t.Fatal(err)
		}
	}
	countAll(&rowsBefore, &eventsBefore)
	rw := upload(ctx, operatorS, sourceID, "op-import", file)
	var replay struct {
		ID       string `json:"job_id"`
		Status   string `json:"status"`
		Replayed bool   `json:"replayed"`
	}
	_ = json.Unmarshal(rw.Body.Bytes(), &replay)
	if rw.Code != 200 || replay.ID != jobID || replay.Status != "cancelled" || !replay.Replayed {
		t.Fatalf("retry of the interrupted import: HTTP %d %.300s, want 200 replay of cancelled job %s", rw.Code, rw.Body.String(), jobID)
	}
	countAll(&rowsAfter, &eventsAfter)
	if err := control.QueryRowContext(ctx, `SELECT status FROM supervisor.operational_import_jobs WHERE id=$1`, jobID).Scan(&status); err != nil || status != "cancelled" || rowsAfter != rowsBefore || eventsAfter != eventsBefore {
		t.Fatalf("retry changed the job: status=%s rows %d->%d events %d->%d err=%v", status, rowsBefore, rowsAfter, eventsBefore, eventsAfter, err)
	}
}
