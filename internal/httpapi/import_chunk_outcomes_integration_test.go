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

// importRace is a migrated database with one approved operational source and an operator who can
// upload to it from two sessions (WIS-719 regression tests).
type importRace struct {
	t                  *testing.T
	control            *sql.DB
	tenant, site, src  string
	operatorIss, opSub string
	upload             func(ctx context.Context, key string, file []byte) *httptest.ResponseRecorder
	upload2            func(ctx context.Context, key string, file []byte) *httptest.ResponseRecorder
	get                func(ctx context.Context, path string) *httptest.ResponseRecorder
	batch              func(ctx context.Context, key, body string) *httptest.ResponseRecorder
	// cancel posts the owner/supervisor import cancel as who (owner, supervisor, operator,
	// reviewer, viewer, or foreign: the owner of another tenant's site, foreignSite).
	cancel      func(ctx context.Context, who, site, jobID, key, body string) *httptest.ResponseRecorder
	foreignSite string
}

func setupImportRace(t *testing.T, ctx context.Context) *importRace {
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	database := "wis719_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.ExecContext(ctx, "CREATE DATABASE "+database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)") })
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
	if err = migrate.Apply(ctx, control); err != nil {
		t.Fatal(err)
	}
	db, err := store.New(runtimeURLFor(runtimeURL, database))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); runtime.Close(); control.Close() })
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := control.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	tenant, site := uuid.NewString(), uuid.NewString()
	owner, reviewer, operator := uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, origin := "https://wis719.invalid/"+uuid.NewString(), "http://127.0.0.1:58177"
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic')`, tenant)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'site')`, tenant, site)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner'),($1,$2,$3,$5,'supervisor'),($1,$2,$3,$6,'operator')`, tenant, site, issuer, owner, reviewer, operator)
	type session struct{ token, csrf, hash string }
	seed := func(subject string) session {
		v := session{token: uuid.NewString(), csrf: uuid.NewString()}
		sum := sha256.Sum256([]byte(v.token))
		v.hash = hex.EncodeToString(sum[:])
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, v.hash, subject, issuer, v.csrf)
		return v
	}
	ownerS, reviewerS, operatorS, operatorS2 := seed(owner), seed(reviewer), seed(operator), seed(operator)
	viewer, revRole := uuid.NewString(), uuid.NewString()
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'viewer'),($1,$2,$3,$5,'reviewer')`, tenant, site, issuer, viewer, revRole)
	foreignTenant, foreignSite, foreignOwner := uuid.NewString(), uuid.NewString(), uuid.NewString()
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'foreign')`, foreignTenant)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'foreign site')`, foreignTenant, foreignSite)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, foreignTenant, foreignSite, issuer, foreignOwner)
	as := map[string]session{"owner": ownerS, "supervisor": reviewerS, "operator": operatorS, "viewer": seed(viewer), "reviewer": seed(revRole), "foreign": seed(foreignOwner)}
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "v", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)
	ev := evidence.New(runtime)
	source, err := ev.CreateOrRevise(ctx, evidence.Actor{Issuer: issuer, Subject: owner, SessionHash: ownerS.hash}, evidence.Scope{TenantID: tenant, SiteID: site}, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Asset list", Text: "Press 1"}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	asset, err := knowledge.New(runtime).CreateEntity(ctx, knowledge.Actor{Issuer: issuer, Subject: owner, SessionHash: ownerS.hash}, knowledge.Scope{TenantID: tenant, SiteID: site}, knowledge.EntityInput{EntityID: uuid.NewString(), Type: "asset", AssetKind: "machine", Name: "Press 1", SourceID: source.SourceID, SourceRevision: source.Revision}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	call := func(rctx context.Context, actor session, method, path, key, body, contentType string, ifMatch ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(rctx)
		r.AddCookie(&http.Cookie{Name: "supervisor", Value: actor.token})
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", actor.csrf)
		if contentType != "" {
			r.Header.Set("Content-Type", contentType)
		}
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
	return &importRace{t: t, control: control, tenant: tenant, site: site, src: sourceID, operatorIss: issuer, opSub: operator, foreignSite: foreignSite,
		cancel: func(rctx context.Context, who, siteID, jobID, key, body string) *httptest.ResponseRecorder {
			return call(rctx, as[who], "POST", "/v1/sites/"+siteID+"/operational-ingestion-jobs/"+jobID+"/cancel", key, body, "application/json")
		},
		upload2: func(rctx context.Context, key string, file []byte) *httptest.ResponseRecorder {
			var buf bytes.Buffer
			mw := multipart.NewWriter(&buf)
			_ = mw.WriteField("format", "csv")
			h := make(textproto.MIMEHeader)
			h.Set("Content-Disposition", `form-data; name="file"; filename="import.csv"`)
			h.Set("Content-Type", "text/csv")
			fw, _ := mw.CreatePart(h)
			_, _ = fw.Write(file)
			_ = mw.Close()
			return call(rctx, operatorS2, "POST", sources+"/"+sourceID+"/imports", key, buf.String(), mw.FormDataContentType())
		},
		upload: func(rctx context.Context, key string, file []byte) *httptest.ResponseRecorder {
			var buf bytes.Buffer
			mw := multipart.NewWriter(&buf)
			_ = mw.WriteField("format", "csv")
			h := make(textproto.MIMEHeader)
			h.Set("Content-Disposition", `form-data; name="file"; filename="import.csv"`)
			h.Set("Content-Type", "text/csv")
			fw, _ := mw.CreatePart(h)
			_, _ = fw.Write(file)
			_ = mw.Close()
			return call(rctx, operatorS, "POST", sources+"/"+sourceID+"/imports", key, buf.String(), mw.FormDataContentType())
		},
		get: func(rctx context.Context, path string) *httptest.ResponseRecorder {
			return call(rctx, operatorS, "GET", path, "", "", "")
		},
		batch: func(rctx context.Context, key, body string) *httptest.ResponseRecorder {
			return call(rctx, ownerS, "POST", sources+"/"+sourceID+"/batches", key, body, "application/json")
		},
	}
}

func importRaceFile(n int) []byte {
	var b strings.Builder
	b.WriteString("external_key,revision,kind,event_at,asset_external_key,order_external_key,payload_json\n")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "m-%d,1,machine_state,2026-09-27T00:00:00Z,press-01,,{}\n", i)
	}
	return []byte(b.String())
}

// state returns the job status, accepted_rows, its non-rejected outcome rows, and the event records
// stored for the source.
func (v *importRace) state(ctx context.Context, key string) (status string, accepted, outcomes, events int) {
	v.t.Helper()
	if err := v.control.QueryRowContext(ctx, `SELECT j.status,j.accepted_rows,
 (SELECT count(*) FROM supervisor.operational_import_rows r WHERE r.job_id=j.id AND r.status<>'rejected'),
 (SELECT count(*) FROM supervisor.operational_event_records e WHERE e.tenant_id=j.tenant_id AND e.source_id=j.source_id)
 FROM supervisor.operational_import_jobs j WHERE j.tenant_id=$1 AND j.idempotency_key=$2`, v.tenant, key).Scan(&status, &accepted, &outcomes, &events); err != nil {
		v.t.Fatal(err)
	}
	return
}

// liveBatch is a one-event live batch at the source's next sequence and current revisions.
func (v *importRace) liveBatch(ctx context.Context, externalKey string) string {
	v.t.Helper()
	var srcRev, mapRev, last int64
	if err := v.control.QueryRowContext(ctx, `SELECT r.revision,r.mapping_revision,coalesce((SELECT last_sequence FROM supervisor.operational_event_sequence_heads s WHERE (s.tenant_id,s.site_id,s.source_id)=(h.tenant_id,h.site_id,h.source_id)),0)
 FROM supervisor.operational_source_heads h JOIN supervisor.operational_source_revisions r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=(h.tenant_id,h.site_id,h.source_id,h.current_revision)
 WHERE h.tenant_id=$1 AND h.source_id=$2`, v.tenant, v.src).Scan(&srcRev, &mapRev, &last); err != nil {
		v.t.Fatal(err)
	}
	return fmt.Sprintf(`{"schema_version":1,"source_id":%q,"source_revision":%d,"mapping_revision":%d,"events":[{"external_key":%q,"revision":1,"kind":"machine_state","source_sequence":%d,"event_at":"2026-09-27T00:00:00Z","payload":{"state":"running"},"asset_external_key":"press-01"}]}`, v.src, srcRev, mapRev, externalKey, last+1)
}

// seedRunningJob inserts a running job for key exactly as insertJob would, created age ago.
func (v *importRace) seedRunningJob(ctx context.Context, key string, file []byte, rows int, age time.Duration) string {
	v.t.Helper()
	fs := sha256.Sum256(file)
	fileSHA := hex.EncodeToString(fs[:])
	b, _ := json.Marshal(struct {
		Format   string `json:"format"`
		FileSHA  string `json:"file_sha256"`
		SourceID string `json:"source_id"`
		Rows     int    `json:"rows"`
	}{"csv", fileSHA, v.src, rows})
	rs := sha256.Sum256(b)
	jobID := uuid.NewString()
	// The job table's tenant-scope trigger needs the tenant context, even for the admin role.
	tx, err := v.control.BeginTx(ctx, nil)
	if err != nil {
		v.t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, v.tenant); err != nil {
		v.t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_import_jobs(tenant_id,site_id,id,source_id,format,file_sha256,request_sha256,idempotency_key,actor_issuer,actor_subject,source_revision,mapping_revision,start_sequence,row_count,status,created_at) VALUES($1,$2,$3,$4,'csv',$5,$6,$7,$8,$9,2,2,1,$10,'running',clock_timestamp()-make_interval(secs => $11))`,
		v.tenant, v.site, jobID, v.src, fileSHA, hex.EncodeToString(rs[:]), key, v.operatorIss, v.opSub, rows, age.Seconds()); err != nil {
		v.t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		v.t.Fatal(err)
	}
	return jobID
}

// lockWaiters counts sessions waiting on a lock while running a statement matching like.
func (v *importRace) lockWaiters(ctx context.Context, like string) int {
	var n int
	_ = v.control.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE $1`, like).Scan(&n)
	return n
}

func (v *importRace) waitForWaiters(ctx context.Context, like string, want int) {
	v.t.Helper()
	for i := 0; i < 300; i++ { // 3 s, under the application's 5 s lock_timeout
		if v.lockWaiters(ctx, like) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	v.t.Fatalf("lock waiters on %s never reached %d", like, want)
}

const (
	waitChunkInsert = "INSERT INTO supervisor.operational_event_batches%"
	waitCancelLock  = "%operational_import_jobs%FOR UPDATE%"
)

// WIS-719 High 1 (review of #314 at ebfbc4d): a chunk's events committed in one transaction and
// its outcomes in a later one, so a failure or disconnect between them cancelled the job with
// stored events that had no outcome; errors.csv then listed ingested rows as unprocessed. The fault
// here fails every outcome write from row 101 on; chunk 2's events must not survive without them.
func TestImportChunkEventsAndOutcomesCommitTogetherPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	v := setupImportRace(t, ctx)
	if _, err := v.control.ExecContext(ctx, `CREATE FUNCTION supervisor.wis719_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'wis719 injected outcome failure'; END $$;
 CREATE TRIGGER wis719_fail BEFORE INSERT ON supervisor.operational_import_rows FOR EACH ROW WHEN (NEW.row_index > 100) EXECUTE FUNCTION supervisor.wis719_fail()`); err != nil {
		t.Fatal(err)
	}
	file := importRaceFile(150)
	if w := v.upload(ctx, "k1", file); w.Code != 503 {
		t.Fatalf("upload with failing outcome writes: HTTP %d %s, want 503", w.Code, w.Body.String())
	}
	st, acc, out, evs := v.state(ctx, "k1")
	if st != "cancelled" || evs != 100 || out != 100 || acc != 100 {
		t.Fatalf("job status=%s accepted_rows=%d outcomes=%d stored_events=%d, want cancelled 100/100/100: every stored event has an outcome", st, acc, out, evs)
	}
	if _, err := v.control.ExecContext(ctx, `DROP TRIGGER wis719_fail ON supervisor.operational_import_rows`); err != nil {
		t.Fatal(err)
	}
	var jobID string
	if err := v.control.QueryRowContext(ctx, `SELECT id::text FROM supervisor.operational_import_jobs WHERE tenant_id=$1 AND idempotency_key='k1'`, v.tenant).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	// The export lists exactly the 50 rows that were not ingested, each as unprocessed.
	csv := v.get(ctx, "/v1/sites/"+v.site+"/operational-ingestion-jobs/"+jobID+"/errors.csv")
	lines := strings.Split(strings.TrimSpace(csv.Body.String()), "\n")
	if csv.Code != 200 || len(lines) != 1+150-evs {
		t.Fatalf("errors.csv: HTTP %d with %d data lines, want %d (rows not ingested)\n%s", csv.Code, len(lines)-1, 150-evs, csv.Body.String())
	}
	for _, line := range lines[1:] {
		if !strings.Contains(line, "unprocessed") || !strings.Contains(line, "IMPORT_CANCELLED") {
			t.Fatalf("errors.csv line %q, want unprocessed IMPORT_CANCELLED", line)
		}
	}
	// The same-key retry replays the cancelled job unchanged.
	rw := v.upload(ctx, "k1", file)
	var job struct {
		ID       string `json:"job_id"`
		Status   string `json:"status"`
		Replayed bool   `json:"replayed"`
	}
	_ = json.Unmarshal(rw.Body.Bytes(), &job)
	if rw.Code != 200 || job.ID != jobID || job.Status != "cancelled" || !job.Replayed {
		t.Fatalf("retry: HTTP %d %.300s, want 200 replay of job %s", rw.Code, rw.Body.String(), jobID)
	}
	if st2, acc2, out2, evs2 := v.state(ctx, "k1"); st2 != st || acc2 != acc || out2 != out || evs2 != evs {
		t.Fatalf("retry changed the job: %s %d/%d/%d", st2, acc2, out2, evs2)
	}
}

// WIS-719 High 2 (review of #314 at ebfbc4d): a chunk read its job's status without a lock, so an
// interrupted request's cleanup could cancel the job after a same-key retry's chunk passed that check
// and before its events committed. Those events then had no outcomes. R1 is interrupted mid-chunk;
// its cleanup waits on G1. R2, the immediate retry, is paused at its chunk's first write by G2.
// Releasing G1 must not let the cancellation commit while R2's chunk is in flight.
func TestImportCancelWaitsForAnInFlightChunkPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	v := setupImportRace(t, ctx)
	file := importRaceFile(3)
	jobID := v.seedRunningJob(ctx, "k2", file, 3, 0)
	// G1 delays R1's cleanup: it holds the job row FOR SHARE, which blocks a cancellation only.
	g1, err := v.control.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g1.Rollback()
	if _, err = g1.ExecContext(ctx, `SELECT 1 FROM supervisor.operational_import_jobs WHERE id=$1 FOR SHARE`, jobID); err != nil {
		t.Fatal(err)
	}
	// G2 pauses a chunk at its first write.
	g2, err := v.control.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g2.Rollback()
	if _, err = g2.ExecContext(ctx, `LOCK TABLE supervisor.operational_event_batches IN SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	r1ctx, r1cancel := context.WithCancel(ctx)
	r1done := make(chan *httptest.ResponseRecorder, 1)
	go func() { r1done <- v.upload(r1ctx, "k2", file) }()
	v.waitForWaiters(ctx, waitChunkInsert, 1)
	r1cancel() // the client disconnects while its chunk is in flight
	v.waitForWaiters(ctx, waitCancelLock, 1)
	v.waitForWaiters(ctx, waitChunkInsert, 0)
	r2done := make(chan *httptest.ResponseRecorder, 1)
	go func() { r2done <- v.upload2(ctx, "k2", file) }()
	v.waitForWaiters(ctx, waitChunkInsert, 1)
	if err = g1.Commit(); err != nil {
		t.Fatal(err)
	}
	// R2's chunk has passed its status check and has not committed: the job must stay running.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var status string
		if err = v.control.QueryRowContext(ctx, `SELECT status FROM supervisor.operational_import_jobs WHERE id=$1`, jobID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "running" {
			_ = g2.Commit()
			<-r2done
			st, acc, out, evs := v.state(ctx, "k2")
			t.Fatalf("job became %s while a chunk was in flight; final status=%s accepted_rows=%d outcomes=%d stored_events=%d", status, st, acc, out, evs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err = g2.Commit(); err != nil {
		t.Fatal(err)
	}
	r2, r1 := <-r2done, <-r1done
	if r1.Code < 500 {
		t.Fatalf("interrupted request: HTTP %d %s", r1.Code, r1.Body.String())
	}
	if r2.Code != 202 {
		t.Fatalf("retry: HTTP %d %s, want 202", r2.Code, r2.Body.String())
	}
	st, acc, out, evs := v.state(ctx, "k2")
	if evs != 3 || out != 3 || acc != 3 || (st != "cancelled" && st != "completed") {
		t.Fatalf("job status=%s accepted_rows=%d outcomes=%d stored_events=%d, want 3/3/3 and a terminal status", st, acc, out, evs)
	}
}

// WIS-719 Medium 3 (review of #314 at ebfbc4d): cancellation locked the job and counted its rows in
// one statement, so after waiting for a writer that held the job FOR SHARE it froze counts from the
// snapshot taken before that writer committed. Here a live batch ends an import past its lease while
// a control transaction is recording one outcome for it.
func TestImportCancelCountsRowsCommittedWhileItWaitedPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	v := setupImportRace(t, ctx)
	jobID := v.seedRunningJob(ctx, "k3", importRaceFile(2), 2, 16*time.Minute)
	writer, err := v.control.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err = writer.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, v.tenant); err != nil {
		t.Fatal(err)
	}
	if _, err = writer.ExecContext(ctx, `SELECT 1 FROM supervisor.operational_import_jobs WHERE id=$1 FOR SHARE`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err = writer.ExecContext(ctx, `INSERT INTO supervisor.operational_import_rows(tenant_id,site_id,job_id,row_index,status) VALUES($1,$2,$3,1,'accepted')`, v.tenant, v.site, jobID); err != nil {
		t.Fatal(err)
	}
	body := v.liveBatch(ctx, "live-1")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- v.batch(ctx, "live-1", body) }()
	v.waitForWaiters(ctx, waitCancelLock, 1)
	if err = writer.Commit(); err != nil {
		t.Fatal(err)
	}
	if w := <-done; w.Code != 202 {
		t.Fatalf("live batch: HTTP %d %s, want 202", w.Code, w.Body.String())
	}
	var status string
	var accepted int
	if err = v.control.QueryRowContext(ctx, `SELECT status,accepted_rows FROM supervisor.operational_import_jobs WHERE id=$1`, jobID).Scan(&status, &accepted); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" || accepted != 1 {
		t.Fatalf("job status=%s accepted_rows=%d, want cancelled with the 1 row committed while the cancellation waited", status, accepted)
	}
}
