//go:build integration

package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestOperationalImportsPostgres drives the CSV/JSON importer through HTTP against PostgreSQL:
// chunked persistence, replay, conflicting replay, role refusal, malformed files, unmapped rows,
// unapproved sources and paged job reads.
func TestOperationalImportsPostgres(t *testing.T) {
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
	database := "imports_http_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.ExecContext(ctx, "CREATE DATABASE "+database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, _ = admin.ExecContext(cleanup, "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)")
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
	tenant, site := uuid.NewString(), uuid.NewString()
	owner, reviewer, viewer := uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, origin := "https://imports-http.invalid/"+uuid.NewString(), "http://127.0.0.1:58177"
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic import test')`, tenant)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'import site')`, tenant, site)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner'),($1,$2,$3,$5,'supervisor'),($1,$2,$3,$6,'viewer')`, tenant, site, issuer, owner, reviewer, viewer)
	type session struct{ token, csrf, hash string }
	seed := func(subject string) session {
		t.Helper()
		v := session{token: uuid.NewString(), csrf: uuid.NewString()}
		sum := sha256.Sum256([]byte(v.token))
		v.hash = hex.EncodeToString(sum[:])
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, v.hash, subject, issuer, v.csrf)
		return v
	}
	ownerS, reviewerS, viewerS := seed(owner), seed(reviewer), seed(viewer)
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "imports-test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)

	// Entities and provenance: one asset and one order are mapped; "press-99" is not.
	ev := evidence.New(runtime)
	ea := evidence.Actor{Issuer: issuer, Subject: owner, SessionHash: ownerS.hash}
	es := evidence.Scope{TenantID: tenant, SiteID: site}
	source, err := ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Asset list", Text: "Press 1 and order 7"}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	kn := knowledge.New(runtime)
	ka := knowledge.Actor{Issuer: issuer, Subject: owner, SessionHash: ownerS.hash}
	ks := knowledge.Scope{TenantID: tenant, SiteID: site}
	asset, err := kn.CreateEntity(ctx, ka, ks, knowledge.EntityInput{EntityID: uuid.NewString(), Type: "asset", AssetKind: "machine", Name: "Press 1", SourceID: source.SourceID, SourceRevision: source.Revision}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	order, err := kn.CreateEntity(ctx, ka, ks, knowledge.EntityInput{EntityID: uuid.NewString(), Type: "order", Name: "Order 7", SourceID: source.SourceID, SourceRevision: source.Revision}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}

	call := func(actor session, method, path, key, body, contentType string, ifMatch ...string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if actor.token != "" {
			r.AddCookie(&http.Cookie{Name: "supervisor", Value: actor.token})
		}
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
	partTypes := map[string]string{"csv": "text/csv", "json": "application/json"}
	uploadAs := func(actor session, sourceID, key, format, partType, query string, file []byte) *httptest.ResponseRecorder {
		t.Helper()
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("format", format)
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", `form-data; name="file"; filename="import.`+format+`"`)
		if partType != "" {
			h.Set("Content-Type", partType)
		}
		fw, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write(file)
		_ = mw.Close()
		return call(actor, "POST", "/v1/sites/"+site+"/operational-sources/"+sourceID+"/imports"+query, key, buf.String(), mw.FormDataContentType())
	}
	upload := func(actor session, sourceID, key, format string, file []byte) *httptest.ResponseRecorder {
		t.Helper()
		return uploadAs(actor, sourceID, key, format, partTypes[format], "", file)
	}
	expect := func(w *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("HTTP %d want %d: %s", w.Code, status, w.Body.String())
		}
		if code != "" && !strings.Contains(w.Body.String(), `"code":"`+code+`"`) {
			t.Fatalf("code want %s: %s", code, w.Body.String())
		}
	}
	jobCount := func() int {
		t.Helper()
		var n int
		if err := control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.operational_import_jobs WHERE tenant_id=$1`, tenant).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Approved source with the mapping.
	sources := "/v1/sites/" + site + "/operational-sources"
	sourceID := uuid.NewString()
	expect(call(ownerS, "POST", sources, "src", fmt.Sprintf(`{"source_id":%q,"type":"csv","authority_scope":"machine_state,production_delta","enabled":true,"access_policy_revision":1,"freshness_seconds":120,"clock_skew_seconds":30}`, sourceID), "application/json", `"0"`), 200, "")
	mapping, _ := json.Marshal(map[string]any{"mappings": []map[string]any{
		{"external_type": "asset", "external_key": "press-01", "entity_id": asset.EntityID, "entity_revision": asset.Revision},
		{"external_type": "order", "external_key": "order-7", "entity_id": order.EntityID, "entity_revision": order.Revision},
	}})
	expect(call(ownerS, "POST", sources+"/"+sourceID+"/mappings", "map", string(mapping), "application/json", `"1"`), 200, "")
	expect(call(reviewerS, "POST", sources+"/"+sourceID+"/reviews", "rev", `{"verdict":"approved","reason":"Scope verified"}`, "application/json", `"2"`), 200, "")

	at := "2026-09-27T00:00:00Z"
	var csvRows strings.Builder
	csvRows.WriteString("external_key,revision,kind,event_at,asset_external_key,order_external_key,payload_json\n")
	for i := 1; i <= 150; i++ {
		fmt.Fprintf(&csvRows, "machine-%d,1,machine_state,%s,press-01,,\"{\"\"state\"\":\"\"running\"\"}\"\n", i, at)
	}
	file := []byte(csvRows.String())

	// Refused before any job exists: a viewer, a malformed file, a non-reviewed source.
	expect(upload(viewerS, sourceID, "viewer", "csv", file), 403, "PERMISSION_DENIED")
	expect(upload(ownerS, sourceID, "bad", "csv", []byte("external_key,revision\nk,1\n")), 422, "INVALID_IMPORT")
	expect(upload(ownerS, sourceID, "bad-format", "xml", file), 422, "INVALID_IMPORT")
	if n := jobCount(); n != 0 {
		t.Fatalf("refused requests left %d job rows", n)
	}

	// Happy path: 150 rows persist as two chunks, and the job is completed.
	first := upload(ownerS, sourceID, "import-1", "csv", file)
	expect(first, 202, "")
	var job struct {
		ID            string `json:"job_id"`
		Status        string `json:"status"`
		StartSequence int64  `json:"start_sequence"`
		RowCount      int    `json:"row_count"`
		Accepted      int    `json:"accepted_rows"`
		Replayed      bool   `json:"replayed"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if job.Status != "completed" || job.RowCount != 150 || job.Accepted != 150 || job.StartSequence != 1 || job.Replayed {
		t.Fatalf("job=%+v body=%s", job, first.Body.String())
	}

	// Replay: same key and same file returns the same job without writing again.
	replay := upload(ownerS, sourceID, "import-1", "csv", file)
	expect(replay, 200, "")
	if !strings.Contains(replay.Body.String(), `"replayed":true`) || !strings.Contains(replay.Body.String(), job.ID) {
		t.Fatalf("replay body=%s", replay.Body.String())
	}
	// Same key, different file: conflict; no second job.
	expect(upload(ownerS, sourceID, "import-1", "csv", append(file, []byte("extra,1,machine_state,"+at+",,,{}\n")...)), 409, "IDEMPOTENCY_CONFLICT")
	if n := jobCount(); n != 1 {
		t.Fatalf("expected one job, have %d", n)
	}

	// Paged rows: limit 100, then the remaining 50 after row 100.
	page := call(viewerS, "GET", "/v1/sites/"+site+"/operational-ingestion-jobs/"+job.ID+"?limit=100", "", "", "application/json")
	expect(page, 200, "")
	if strings.Count(page.Body.String(), `"status":"accepted"`) != 100 || page.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("first page: %s", page.Body.String()[:200])
	}
	next := call(viewerS, "GET", "/v1/sites/"+site+"/operational-ingestion-jobs/"+job.ID+"?after=100&limit=100", "", "", "application/json")
	expect(next, 200, "")
	if strings.Count(next.Body.String(), `"status":"accepted"`) != 50 {
		t.Fatalf("second page: %s", next.Body.String())
	}
	expect(call(viewerS, "GET", "/v1/sites/"+site+"/operational-ingestion-jobs/"+job.ID+"?limit=101", "", "", "application/json"), 422, "INVALID_REQUEST")
	emptyErrorCSV := call(viewerS, "GET", "/v1/sites/"+site+"/operational-ingestion-jobs/"+job.ID+"/errors.csv", "", "", "")
	expect(emptyErrorCSV, 200, "")
	if emptyErrorCSV.Body.String() != "row_index,status,reason,source_sequence\n" {
		t.Fatalf("empty error CSV = %q", emptyErrorCSV.Body.String())
	}

	// Unmapped asset rows are quarantined, durably recorded, and the job still completes.
	var unmapped strings.Builder
	unmapped.WriteString("external_key,revision,kind,event_at,asset_external_key,order_external_key,payload_json\n")
	unmapped.WriteString("ghost-1,1,machine_state," + at + ",press-99,,{}\n")
	q := upload(ownerS, sourceID, "import-2", "csv", []byte(unmapped.String()))
	expect(q, 202, "")
	if !strings.Contains(q.Body.String(), `"quarantined_rows":1`) || !strings.Contains(q.Body.String(), `"status":"quarantined"`) || !strings.Contains(q.Body.String(), "UNKNOWN_MAPPING") {
		t.Fatalf("quarantine body: %s", q.Body.String())
	}
	var quarantineJob struct {
		ID string `json:"job_id"`
	}
	if err := json.Unmarshal(q.Body.Bytes(), &quarantineJob); err != nil {
		t.Fatal(err)
	}
	errorCSVPath := "/v1/sites/" + site + "/operational-ingestion-jobs/" + quarantineJob.ID + "/errors.csv"
	errorCSV := call(viewerS, "GET", errorCSVPath, "", "", "")
	expect(errorCSV, 200, "")
	if errorCSV.Header().Get("Content-Type") != "text/csv; charset=utf-8" || errorCSV.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(errorCSV.Header().Get("Content-Disposition"), ".csv\"") ||
		!strings.HasPrefix(errorCSV.Body.String(), "row_index,status,reason,source_sequence\n") ||
		!strings.Contains(errorCSV.Body.String(), "quarantined,UNKNOWN_MAPPING") {
		t.Fatalf("error CSV headers/body: %#v %q", errorCSV.Header(), errorCSV.Body.String())
	}
	expect(call(viewerS, "GET", errorCSVPath+"?limit=1", "", "", ""), 422, "INVALID_REQUEST")

	// JSON format imports through the same path.
	jsonFile := []byte(`[{"external_key":"json-1","revision":1,"kind":"machine_state","event_at":"` + at + `","asset_external_key":"press-01","payload":{"state":"stopped"}}]`)
	expect(upload(ownerS, sourceID, "import-3", "json", jsonFile), 202, "")

	// A source that is not reviewed takes no imports and creates no job.
	pending := uuid.NewString()
	expect(call(ownerS, "POST", sources, "pending", fmt.Sprintf(`{"source_id":%q,"type":"csv","authority_scope":"machine_state","enabled":true,"access_policy_revision":1,"freshness_seconds":120,"clock_skew_seconds":30}`, pending), "application/json", `"0"`), 200, "")
	before := jobCount()
	expect(upload(ownerS, pending, "unreviewed", "csv", file), 409, "SOURCE_NOT_APPROVED")
	if jobCount() != before {
		t.Fatal("unreviewed source created a job")
	}
	// The POST response carries one page of rows; GET pages the rest.
	if n := strings.Count(first.Body.String(), `"row_index"`); n != 100 {
		t.Fatalf("POST response carried %d rows, want one page of 100", n)
	}

	// The file part must match the format, and no field comes from the query string.
	expect(uploadAs(ownerS, sourceID, "octet", "csv", "application/octet-stream", "", file), 415, "UNSUPPORTED_MEDIA")
	expect(uploadAs(ownerS, sourceID, "json-as-csv", "json", "text/csv", "", jsonFile), 415, "UNSUPPORTED_MEDIA")
	expect(uploadAs(ownerS, sourceID, "query", "csv", "text/csv", "?format=json", file), 422, "INVALID_IMPORT")

	recordCount := func() int {
		t.Helper()
		var n int
		if err := control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.operational_event_records WHERE tenant_id=$1`, tenant).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	header := "external_key,revision,kind,event_at,asset_external_key,order_external_key,payload_json\n"
	jobsBefore, recordsBefore := jobCount(), recordCount()
	// The whole file is validated first: a malformed plan in the second chunk refuses it all.
	var late strings.Builder
	late.WriteString(header)
	for i := 1; i <= 119; i++ {
		fmt.Fprintf(&late, "late-%d,1,machine_state,%s,press-01,,{}\n", i, at)
	}
	late.WriteString("plan-bad,1,production_plan," + at + ",press-01,order-7,{}\n")
	expect(upload(ownerS, sourceID, "late-bad", "csv", []byte(late.String())), 422, "INVALID_IMPORT")
	// Two contents for one (external_key, revision), in the file or against a stored record.
	expect(upload(ownerS, sourceID, "in-file", "csv", []byte(header+"dup,1,machine_state,"+at+",press-01,,\"{\"\"a\"\":1}\"\ndup,1,machine_state,"+at+",press-01,,\"{\"\"a\"\":2}\"\n")), 409, "EVENT_REVISION_CONFLICT")
	expect(upload(ownerS, sourceID, "stored", "csv", []byte(header+"machine-1,1,machine_state,"+at+",press-01,,\"{\"\"state\"\":\"\"stopped\"\"}\"\n")), 409, "EVENT_REVISION_CONFLICT")
	if jobCount() != jobsBefore || recordCount() != recordsBefore {
		t.Fatal("refused files wrote jobs or records")
	}

	// An interrupted job: put the first job back to running, as a crash after its chunks
	// committed would leave it (the superuser bypasses the finish guard for this).
	tx, err := control.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`SET LOCAL session_replication_role = replica`, `UPDATE supervisor.operational_import_jobs SET status='running',finished_at=NULL,accepted_rows=0 WHERE id='` + job.ID + `'`} {
		if _, err = tx.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// While it runs, it owns the source stream: no other import and no live batch.
	expect(upload(ownerS, sourceID, "while-running", "csv", []byte(unmapped.String())), 409, "IMPORT_IN_PROGRESS")
	expect(call(viewerS, "GET", "/v1/sites/"+site+"/operational-ingestion-jobs/"+job.ID+"/errors.csv", "", "", ""), 409, "IMPORT_IN_PROGRESS")
	live := fmt.Sprintf(`{"schema_version":1,"source_id":%q,"source_revision":1,"mapping_revision":1,"events":[{"external_key":"live-1","revision":1,"kind":"machine_state","source_sequence":9999,"event_at":%q,"payload":{"state":"running"},"asset_external_key":"press-01"}]}`, sourceID, at)
	expect(call(ownerS, "POST", sources+"/"+sourceID+"/batches", "live-1", live, "application/json"), 409, "IMPORT_IN_PROGRESS")
	// The same key resumes it: committed chunks replay, nothing is written twice.
	recordsBefore = recordCount()
	resumed := upload(ownerS, sourceID, "import-1", "csv", file)
	expect(resumed, 202, "")
	if !strings.Contains(resumed.Body.String(), `"status":"completed"`) || !strings.Contains(resumed.Body.String(), `"accepted_rows":150`) || recordCount() != recordsBefore {
		t.Fatalf("resume: records %d -> %d body=%s", recordsBefore, recordCount(), resumed.Body.String()[:300])
	}
	// WIS-699: an import interrupted longer ago than the 15-minute lease no longer blocks the
	// source. The recently interrupted job above still did. Put the job back to running as if it
	// had been created 20 minutes ago: a live batch is accepted and ends the job cancelled, and a
	// retry with the original key replays the cancelled job instead of resuming it.
	tx, err = control.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`SET LOCAL session_replication_role = replica`, `UPDATE supervisor.operational_import_jobs SET status='running',finished_at=NULL,created_at=clock_timestamp()-interval '20 minutes' WHERE id='` + job.ID + `'`} {
		if _, err = tx.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var srcRev, mapRev int64
	if err = control.QueryRowContext(ctx, `SELECT r.revision,r.mapping_revision FROM supervisor.operational_source_heads h JOIN supervisor.operational_source_revisions r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=(h.tenant_id,h.site_id,h.source_id,h.current_revision) WHERE h.tenant_id=$1 AND h.source_id=$2`, tenant, sourceID).Scan(&srcRev, &mapRev); err != nil {
		t.Fatal(err)
	}
	var lastSeq int64
	if err = control.QueryRowContext(ctx, `SELECT last_sequence FROM supervisor.operational_event_sequence_heads WHERE tenant_id=$1 AND source_id=$2`, tenant, sourceID).Scan(&lastSeq); err != nil {
		t.Fatal(err)
	}
	stale := fmt.Sprintf(`{"schema_version":1,"source_id":%q,"source_revision":%d,"mapping_revision":%d,"events":[{"external_key":"live-stale","revision":1,"kind":"machine_state","source_sequence":%d,"event_at":%q,"payload":{"state":"running"},"asset_external_key":"press-01"}]}`, sourceID, srcRev, mapRev, lastSeq+1, at)
	expect(call(ownerS, "POST", sources+"/"+sourceID+"/batches", "live-stale", stale, "application/json"), 202, "")
	var staleStatus string
	if err = control.QueryRowContext(ctx, `SELECT status FROM supervisor.operational_import_jobs WHERE id=$1`, job.ID).Scan(&staleStatus); err != nil || staleStatus != "cancelled" {
		t.Fatalf("stale import status %q (%v), want cancelled", staleStatus, err)
	}
	recordsBefore = recordCount()
	if replayed := upload(ownerS, sourceID, "import-1", "csv", file); replayed.Code != 200 || !strings.Contains(replayed.Body.String(), `"status":"cancelled"`) || recordCount() != recordsBefore {
		t.Fatalf("retry of the abandoned import: HTTP %d records %d -> %d body=%.300s", replayed.Code, recordsBefore, recordCount(), replayed.Body.String())
	}
	// Two imports race on one source: each either runs on its own reserved range or is told an
	// import is in progress. Never a 5xx, never interleaved or duplicated sequences.
	var wg sync.WaitGroup
	results := make([]*httptest.ResponseRecorder, 2)
	for i := range results {
		var body strings.Builder
		body.WriteString(header)
		for j := 1; j <= 150; j++ {
			fmt.Fprintf(&body, "race-%d-%d,1,machine_state,%s,press-01,,{}\n", i, j, at)
		}
		wg.Add(1)
		go func(i int, file []byte) {
			defer wg.Done()
			results[i] = upload(ownerS, sourceID, fmt.Sprintf("race-%d", i), "csv", file)
		}(i, []byte(body.String()))
	}
	wg.Wait()
	for i, w := range results {
		if w.Code != 202 && !(w.Code == 409 && strings.Contains(w.Body.String(), "IMPORT_IN_PROGRESS")) {
			t.Fatalf("race %d: HTTP %d %s", i, w.Code, w.Body.String())
		}
	}
	var gaps, dupes int
	if err := control.QueryRowContext(ctx, `SELECT (SELECT max(source_sequence)-count(*) FROM supervisor.operational_event_records WHERE tenant_id=$1 AND source_id=$2),
 (SELECT count(*)-count(DISTINCT external_key) FROM supervisor.operational_event_records WHERE tenant_id=$1 AND source_id=$2 AND external_key LIKE 'race-%')`, tenant, sourceID).Scan(&gaps, &dupes); err != nil {
		t.Fatal(err)
	}
	if gaps != 0 || dupes != 0 {
		t.Fatalf("sequence gaps=%d duplicate race rows=%d", gaps, dupes)
	}
	// Files above the 1 MiB request buffer but within the 10 MiB import limit are accepted;
	// files above the import limit get 413.
	var big strings.Builder
	big.WriteString(header)
	note := strings.Repeat("x", 7000)
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&big, "big-%d,1,machine_state,%s,press-01,,\"{\"\"note\"\":\"\"%s\"\"}\"\n", i, at, note)
	}
	if big.Len() < 2<<20 {
		t.Fatalf("test file is only %d bytes", big.Len())
	}
	large := upload(ownerS, sourceID, "big", "csv", []byte(big.String()))
	expect(large, 202, "")
	if !strings.Contains(large.Body.String(), `"accepted_rows":300`) {
		t.Fatalf("large import: %s", large.Body.String()[:300])
	}
	expect(upload(ownerS, sourceID, "too-big", "csv", bytes.Repeat([]byte("x"), 10<<20+1)), 413, "BODY_TOO_LARGE")

	// errors.csv review findings (WIS-272): scope, terminal states, ordering, row bound, audit.
	// Jobs are seeded directly (triggers and FKs off) because the read path is under test.
	type seededRow struct {
		index    int
		status   string
		reason   string
		sequence int64
	}
	// seedJobRows seeds a terminal job declaring rowCount file rows and records rows.
	seedJobRows := func(tenantID, siteID, source, status string, rows []seededRow, rowCount int) string {
		t.Helper()
		id := uuid.NewString()
		tx, err := control.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_import_jobs(tenant_id,site_id,id,source_id,format,file_sha256,request_sha256,idempotency_key,actor_issuer,actor_subject,source_revision,mapping_revision,start_sequence,row_count,status,finished_at) VALUES($1,$2,$3,$4,'csv',$5,$5,$9,$6,$7,1,1,900000,$10,$8,clock_timestamp())`, tenantID, siteID, id, source, strings.Repeat("d", 64), issuer, owner, status, "seed-"+id, rowCount); err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_import_rows(tenant_id,site_id,job_id,row_index,status,reason,source_sequence) VALUES($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,0))`, tenantID, siteID, id, r.index, r.status, r.reason, r.sequence); err != nil {
				t.Fatal(err)
			}
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return id
	}
	seedJob := func(tenantID, siteID, source, status string, rows []seededRow) string {
		t.Helper()
		return seedJobRows(tenantID, siteID, source, status, rows, 50000)
	}
	exportAudits := func(jobID string) (n, rows int) {
		t.Helper()
		if err := control.QueryRowContext(ctx, `SELECT count(*),coalesce(max(exported_rows),-1) FROM supervisor.operational_source_audit WHERE tenant_id=$1 AND event_type='import.errors_exported' AND job_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND source_id=$5`, tenant, jobID, issuer, viewer, sourceID).Scan(&n, &rows); err != nil {
			t.Fatal(err)
		}
		return n, rows
	}
	exportPath := func(siteID, jobID string) string {
		return "/v1/sites/" + siteID + "/operational-ingestion-jobs/" + jobID + "/errors.csv"
	}
	// Earlier exports each left exactly one audit row with the exported row count; the refused
	// export of the running job left none.
	if n, rows := exportAudits(job.ID); n != 1 || rows != 0 {
		t.Fatalf("empty export audit n=%d rows=%d", n, rows)
	}
	if n, rows := exportAudits(quarantineJob.ID); n != 1 || rows != 1 {
		t.Fatalf("quarantine export audit n=%d rows=%d", n, rows)
	}

	// Partial and cancelled jobs export their error rows in row order, accepted and duplicate
	// rows excluded, with the exact attachment name.
	mixed := []seededRow{
		{12, "rejected", "CHUNK_REJECTED", 0},
		{3, "quarantined", "UNKNOWN_MAPPING", 900002},
		{5, "accepted", "", 900004},
		{7, "rejected", "=HYPERLINK(1)", 0},
		{9, "duplicate", "DUPLICATE_EVENT", 900008},
		{1, "quarantined", "UNKNOWN_MAPPING", 900000},
	}
	wantMixed := "row_index,status,reason,source_sequence\n" +
		"1,quarantined,UNKNOWN_MAPPING,900000\n" +
		"3,quarantined,UNKNOWN_MAPPING,900002\n" +
		"7,rejected,'=HYPERLINK(1),0\n" +
		"12,rejected,CHUNK_REJECTED,0\n"
	// A cancelled job (source revoked mid-import) also lists every file row it never
	// processed, so a partial error list cannot pass for a complete one (WIS-272 review
	// finding; rule 07 per-row status on imports).
	wantCancelled := "row_index,status,reason,source_sequence\n" +
		"1,quarantined,UNKNOWN_MAPPING,900000\n" +
		"2,unprocessed,IMPORT_CANCELLED,0\n" +
		"3,quarantined,UNKNOWN_MAPPING,900002\n" +
		"4,unprocessed,IMPORT_CANCELLED,0\n" +
		"6,unprocessed,IMPORT_CANCELLED,0\n" +
		"7,rejected,'=HYPERLINK(1),0\n" +
		"8,unprocessed,IMPORT_CANCELLED,0\n" +
		"10,unprocessed,IMPORT_CANCELLED,0\n" +
		"11,unprocessed,IMPORT_CANCELLED,0\n" +
		"12,rejected,CHUNK_REJECTED,0\n" +
		"13,unprocessed,IMPORT_CANCELLED,0\n" +
		"14,unprocessed,IMPORT_CANCELLED,0\n"
	for _, c := range []struct {
		status, want string
		rowCount     int
		audited      int
	}{{"partial", wantMixed, 12, 4}, {"cancelled", wantCancelled, 14, 12}} {
		status := c.status
		id := seedJobRows(tenant, site, sourceID, status, mixed, c.rowCount)
		w := call(viewerS, "GET", exportPath(site, id), "", "", "")
		expect(w, 200, "")
		if w.Body.String() != c.want {
			t.Fatalf("%s export body = %q", status, w.Body.String())
		}
		if got, want := w.Header().Get("Content-Disposition"), `attachment; filename="operational-import-errors-`+id+`.csv"`; got != want {
			t.Fatalf("%s Content-Disposition = %q, want %q", status, got, want)
		}
		if n, rows := exportAudits(id); n != 1 || rows != c.audited {
			t.Fatalf("%s export audit n=%d rows=%d", status, n, rows)
		}
	}
	// A completed job never reports unprocessed rows, even when fewer rows were recorded.
	completed := seedJobRows(tenant, site, sourceID, "partial", mixed, 50000)
	if w := call(viewerS, "GET", exportPath(site, completed), "", "", ""); w.Body.String() != wantMixed {
		t.Fatalf("partial export with unrecorded rows = %q", w.Body.String())
	}

	// Scope: a job on another site of the same tenant, or in another tenant, is the same
	// non-disclosing 404 as a job that does not exist, through either site path.
	site2 := uuid.NewString()
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'second import site')`, tenant, site2)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, tenant, site2, issuer, viewer)
	tenant3, site3 := uuid.NewString(), uuid.NewString()
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'other import tenant')`, tenant3)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'other tenant site')`, tenant3, site3)
	failed := []seededRow{{1, "rejected", "CHUNK_REJECTED", 0}}
	site2Job := seedJob(tenant, site2, uuid.NewString(), "partial", failed)
	tenant3Job := seedJob(tenant3, site3, uuid.NewString(), "partial", failed)
	ownJob := seedJob(tenant, site, sourceID, "partial", failed)
	for name, path := range map[string]string{
		"other site's job via own site":   exportPath(site, site2Job),
		"own site's job via other site":   exportPath(site2, ownJob),
		"other tenant's job via own site": exportPath(site, tenant3Job),
		"other tenant's job via its site": exportPath(site3, tenant3Job),
		"nonexistent job":                 exportPath(site, uuid.NewString()),
	} {
		w := call(viewerS, "GET", path, "", "", "")
		if w.Code != 404 || !strings.Contains(w.Body.String(), `"code":"NOT_FOUND"`) {
			t.Fatalf("%s: HTTP %d %s", name, w.Code, w.Body.String())
		}
	}
	// A malformed job ID is an invalid request, as on the job read.
	expect(call(viewerS, "GET", exportPath(site, "not-a-uuid"), "", "", ""), 422, "INVALID_IMPORT")
	expect(call(viewerS, "GET", "/v1/sites/"+site+"/operational-ingestion-jobs/not-a-uuid", "", "", ""), 422, "INVALID_IMPORT")

	// A non-member and a revoked member get 404 and leave no audit row. The service enforces
	// membership in its own read transaction too, not only through the route's site lookup.
	stranger := uuid.NewString()
	strangerS := seed(stranger)
	revoked := uuid.NewString()
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'operator')`, tenant, site, issuer, revoked)
	revokedS := seed(revoked)
	expect(call(revokedS, "GET", exportPath(site, ownJob), "", "", ""), 200, "")
	exec(`UPDATE supervisor.memberships SET active=false WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4`, tenant, site, issuer, revoked)
	for name, actor := range map[string]session{"non-member": strangerS, "revoked member": revokedS} {
		expect(call(actor, "GET", exportPath(site, ownJob), "", "", ""), 404, "NOT_FOUND")
		_, err := ingestion.New(db.DB).WithSessionPolicy(auth.Sessions()).JobErrorsCSV(ctx, ingestion.Actor{Issuer: issuer, Subject: map[string]string{"non-member": stranger, "revoked member": revoked}[name], SessionHash: actor.hash}, ingestion.Scope{TenantID: tenant, SiteID: site}, ownJob)
		if !errors.Is(err, ingestion.ErrNotFound) {
			t.Fatalf("%s service export error = %v", name, err)
		}
	}
	var revokedAudits int
	if err := control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.operational_source_audit WHERE event_type='import.errors_exported' AND actor_subject IN ($1,$2)`, stranger, revoked).Scan(&revokedAudits); err != nil {
		t.Fatal(err)
	}
	if revokedAudits != 1 {
		t.Fatalf("export audits for non-member/revoked = %d, want only the pre-revocation export", revokedAudits)
	}

	// Row bound, success side: exactly MaxImportRows error rows export in full, audited once.
	full := seedJob(tenant, site, sourceID, "partial", nil)
	exec(`INSERT INTO supervisor.operational_import_rows(tenant_id,site_id,job_id,row_index,status,reason) SELECT $1,$2,$3,i,'rejected','CHUNK_REJECTED' FROM generate_series(1,$4::int) i`, tenant, site, full, ingestion.MaxImportRows)
	fullExport := call(viewerS, "GET", exportPath(site, full), "", "", "")
	expect(fullExport, 200, "")
	body := fullExport.Body.String()
	if lines := strings.Count(body, "\n"); lines != ingestion.MaxImportRows+1 || !strings.HasSuffix(body, fmt.Sprintf("\n%d,rejected,CHUNK_REJECTED,0\n", ingestion.MaxImportRows)) {
		t.Fatalf("full export has %d lines, tail %q", lines, body[len(body)-60:])
	}
	if n, rows := exportAudits(full); n != 1 || rows != ingestion.MaxImportRows {
		t.Fatalf("full export audit n=%d rows=%d", n, rows)
	}

	// Row bound: a job with more error rows than an import can hold is refused explicitly,
	// never truncated to the first MaxImportRows rows, and no export is audited.
	overflow := seedJob(tenant, site, sourceID, "partial", nil)
	exec(`INSERT INTO supervisor.operational_import_rows(tenant_id,site_id,job_id,row_index,status,reason) SELECT $1,$2,$3,i,'rejected','CHUNK_REJECTED' FROM generate_series(1,$4::int) i`, tenant, site, overflow, ingestion.MaxImportRows+1)
	expect(call(viewerS, "GET", exportPath(site, overflow), "", "", ""), 422, "IMPORT_TOO_LARGE")
	if n, _ := exportAudits(overflow); n != 0 {
		t.Fatalf("oversized export audited %d times", n)
	}
}
