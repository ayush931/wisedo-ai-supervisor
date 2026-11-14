//go:build integration

package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
)

type configurationImportRow struct {
	Row              int    `json:"row"`
	Kind             string `json:"kind"`
	Ref              string `json:"ref,omitempty"`
	Status           string `json:"status"`
	Reason           string `json:"reason,omitempty"`
	Path             string `json:"path,omitempty"`
	ObjectID         string `json:"object_id,omitempty"`
	ExpectedRevision *int64 `json:"expected_revision,omitempty"`
}

type configurationImportReport struct {
	SchemaVersion int    `json:"schema_version"`
	FileSHA256    string `json:"file_sha256"`
	ReportSHA256  string `json:"report_sha256"`
	Applicable    bool   `json:"applicable"`
	Replayed      bool   `json:"replayed,omitempty"`
	Counts        struct {
		Accepted  int `json:"accepted"`
		Duplicate int `json:"duplicate"`
		Rejected  int `json:"rejected"`
	} `json:"counts"`
	Rows []configurationImportRow `json:"rows"`
}

func uploadConfigurationImport(f *stationFixture, actor stationSession, path, key, ifMatch, accept string, data []byte) *httptest.ResponseRecorder {
	return uploadConfigurationImportOptions(f, actor, path, key, ifMatch, accept, data, false, true)
}

func uploadConfigurationImportOptions(f *stationFixture, actor stationSession, path, key, ifMatch, accept string, data []byte, extraField, csrf bool) *httptest.ResponseRecorder {
	f.t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="master-data.csv"`)
	header.Set("Content-Type", "text/csv")
	part, err := form.CreatePart(header)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err = part.Write(data); err != nil {
		f.t.Fatal(err)
	}
	if extraField {
		if err = form.WriteField("unexpected", "value"); err != nil {
			f.t.Fatal(err)
		}
	}
	if err = form.Close(); err != nil {
		f.t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, path, &body)
	r.AddCookie(&http.Cookie{Name: "supervisor", Value: actor.token})
	r.Header.Set("Origin", f.origin)
	if csrf {
		r.Header.Set("X-CSRF-Token", actor.csrf)
	}
	r.Header.Set("Content-Type", form.FormDataContentType())
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	if ifMatch != "" {
		r.Header.Set("If-Match", ifMatch)
	}
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func TestConfigurationImportDryRunMatchesApply(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	station := uuid.NewString()
	masterCreateProfile(f, f.owner, f.site, "configuration-import-station", masterProfileBody(station, uuid.NewString(), "Import station"))
	file := []byte("kind,ref,display_name,active,unit,line,item,steps,time_zone,shifts\n" +
		"line,line-a,Assembly,true,,,,,,\n" +
		"item,item-a,Part,true,count,,,,,\n" +
		"shift_calendar,calendar-a,Day,,,,,,America/Detroit,1 07:00-15:00\n" +
		"routing,routing-a,Assembly route,true,,line-a,item-a," + station + ",,\n")
	dry := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports/dry-runs", "", "", "", file)
	f.expect(dry, http.StatusOK, "")
	var dryReport configurationImportReport
	if err := json.Unmarshal(dry.Body.Bytes(), &dryReport); err != nil {
		t.Fatal(err)
	}
	if dryReport.SchemaVersion != 1 || !dryReport.Applicable || dryReport.Counts.Accepted != 4 || len(dryReport.Rows) != 4 {
		t.Fatalf("dry-run report: %+v", dryReport)
	}
	var lineHeads int
	if err := f.control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.line_heads WHERE tenant_id=$1 AND site_id=$2`, f.tenant, f.site).Scan(&lineHeads); err != nil {
		t.Fatal(err)
	}
	if lineHeads != 0 {
		t.Fatalf("dry run wrote %d line heads", lineHeads)
	}
	etag := dry.Header().Get("ETag")
	if etag != `"`+dryReport.ReportSHA256+`"` {
		t.Fatalf("dry-run ETag %q does not bind report %q", etag, dryReport.ReportSHA256)
	}
	applied := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports", "apply-once", etag, "", file)
	f.expect(applied, http.StatusCreated, "")
	var applyReport configurationImportReport
	if err := json.Unmarshal(applied.Body.Bytes(), &applyReport); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(dryReport, applyReport) {
		t.Fatalf("dry-run and apply reports differ:\ndry=%+v\napply=%+v", dryReport, applyReport)
	}
	ids := map[string]string{}
	for _, row := range applyReport.Rows {
		if row.Status != "accepted" || row.ObjectID == "" {
			t.Fatalf("applied row has no accepted object ID: %+v", row)
		}
		ids[row.Ref] = row.ObjectID
	}
	var route workstation.RoutingV1
	read := f.call(f.owner, http.MethodGet, "/v1/sites/"+f.site+"/routings/"+ids["routing-a"], "", "", "")
	f.expect(read, http.StatusOK, "")
	if err := json.Unmarshal(read.Body.Bytes(), &route); err != nil {
		t.Fatal(err)
	}
	if len(route.Revisions) != 1 || route.Revisions[0].Body.LineID != ids["line-a"] || route.Revisions[0].Body.ItemID != ids["item-a"] || route.Revisions[0].Body.Steps[0].WorkstationID != station {
		t.Fatalf("routing refs were not resolved to persisted same-file objects: %+v", route)
	}
}

func TestConfigurationImportCSVRejectsFormulaPrefixAndUnknownColumn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	for _, tc := range []struct {
		name, file, code string
		status           int
	}{
		{"formula prefix", "kind,display_name,active\nline,+SUM(A1),true\n", "INVALID_VALUE", http.StatusOK},
		{"D executable display name", "kind,display_name,active\nline,=1+1,true\n", "INVALID_MASTER_DATA", http.StatusOK},
		{"unknown header", "kind,display_name,active,secret\nline,Line,true,x\n", "UNKNOWN_COLUMN", http.StatusUnprocessableEntity},
		{"duplicate header", "kind,display_name,display_name,active\nline,Line,Again,true\n", "DUPLICATE_COLUMN", http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports/dry-runs", "", "", "", []byte(tc.file))
			f.expect(w, tc.status, "")
			if tc.status == http.StatusOK {
				var report configurationImportReport
				if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
					t.Fatal(err)
				}
				if len(report.Rows) != 1 || report.Rows[0].Status != "rejected" || report.Rows[0].Reason != tc.code {
					t.Fatalf("formula row was not rejected without echoing input: %+v", report.Rows)
				}
			} else {
				var problemBody map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &problemBody); err != nil || problemBody["code"] != tc.code {
					t.Fatalf("problem=%s err=%v, want %s", w.Body.String(), err, tc.code)
				}
			}
			if strings.Contains(w.Body.String(), "SUM") || strings.Contains(w.Body.String(), "secret") {
				t.Fatalf("parser echoed untrusted CSV content: %s", w.Body.String())
			}
		})
	}
	csvResponse := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports/dry-runs", "", "", "text/csv", []byte("kind,display_name,active\nline,@SUM(A1),true\n"))
	f.expect(csvResponse, http.StatusOK, "")
	if !strings.HasPrefix(csvResponse.Header().Get("Content-Type"), "text/csv") || strings.Contains(csvResponse.Body.String(), "SUM") {
		t.Fatalf("unsafe or unexpected errors.csv: content-type=%q body=%s", csvResponse.Header().Get("Content-Type"), csvResponse.Body.String())
	}
	if _, err := csv.NewReader(strings.NewReader(csvResponse.Body.String())).ReadAll(); err != nil {
		t.Fatalf("errors.csv was not valid CSV: %v", err)
	}
	direct := f.call(f.owner, http.MethodPost, "/v1/sites/"+f.site+"/lines", "formula-direct", `"0"`, f.encode(masterLineBody("=1+1", true)))
	f.expect(direct, http.StatusUnprocessableEntity, "INVALID_MASTER_DATA")
}

func TestConfigurationImportRejectedRowRollsBackEveryWrite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	foreignStation := uuid.NewString()
	masterCreateProfile(f, f.outsider, f.otherSite, "configuration-import-foreign", masterProfileBody(foreignStation, uuid.NewString(), "Foreign station"))
	file := []byte("kind,ref,display_name,active,unit,line,item,steps,time_zone,shifts\n" +
		"line,line-a,Assembly,true,,,,,,\n" +
		"item,item-a,Part,true,count,,,,,\n" +
		"routing,routing-a,Assembly route,true,,line-a,item-a," + foreignStation + ",,\n")
	dry := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports/dry-runs", "", "", "", file)
	f.expect(dry, http.StatusOK, "")
	var report configurationImportReport
	if err := json.Unmarshal(dry.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Applicable || report.Counts.Accepted != 2 || report.Counts.Rejected != 1 || report.Rows[2].Reason != "INVALID_MASTER_DATA" || report.Rows[2].Path != "steps[0].workstation_id" {
		t.Fatalf("foreign station outcome: %+v", report)
	}
	apply := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports", "reject-file", `"`+report.ReportSHA256+`"`, "", file)
	f.expect(apply, http.StatusUnprocessableEntity, "IMPORT_HAS_REJECTED_ROWS")
	for _, table := range []string{"line_heads", "item_heads", "routing_heads"} {
		var count int
		if err := f.control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.`+table+` WHERE tenant_id=$1 AND site_id=$2`, f.tenant, f.site).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("rejected import left %d rows in %s", count, table)
		}
	}
	var imports, rowReceipts int
	if err := f.control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.configuration_imports WHERE tenant_id=$1 AND site_id=$2`, f.tenant, f.site).Scan(&imports); err != nil {
		t.Fatal(err)
	}
	if err := f.control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.master_data_receipts WHERE tenant_id=$1 AND site_id=$2 AND idempotency_key LIKE 'cfgimp:%'`, f.tenant, f.site).Scan(&rowReceipts); err != nil {
		t.Fatal(err)
	}
	if imports != 0 || rowReceipts != 0 {
		t.Fatalf("rejected import persisted import receipts=%d row receipts=%d", imports, rowReceipts)
	}
}

func TestConfigurationImportIdempotencyAndReapply(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	file := []byte("kind,ref,display_name,active\nline,line-a,Idempotent line,true\n")
	dry := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports/dry-runs", "", "", "", file)
	f.expect(dry, http.StatusOK, "")
	var firstDry configurationImportReport
	if err := json.Unmarshal(dry.Body.Bytes(), &firstDry); err != nil {
		t.Fatal(err)
	}
	etag := `"` + firstDry.ReportSHA256 + `"`
	first := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports", "one-key", etag, "", file)
	f.expect(first, http.StatusCreated, "")
	var firstReport configurationImportReport
	if err := json.Unmarshal(first.Body.Bytes(), &firstReport); err != nil {
		t.Fatal(err)
	}
	replay := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports", "one-key", etag, "", file)
	f.expect(replay, http.StatusOK, "")
	var replayReport configurationImportReport
	if err := json.Unmarshal(replay.Body.Bytes(), &replayReport); err != nil {
		t.Fatal(err)
	}
	if !replayReport.Replayed {
		t.Fatalf("idempotency replay was not marked: %+v", replayReport)
	}
	replayReport.Replayed = false
	if !reflect.DeepEqual(firstReport, replayReport) || replay.Header().Get("Location") != first.Header().Get("Location") {
		t.Fatalf("idempotency replay differs: first=%+v replay=%+v", firstReport, replayReport)
	}
	changed := []byte("kind,ref,display_name,active\nline,line-b,Other line,true\n")
	conflict := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports", "one-key", etag, "", changed)
	f.expect(conflict, http.StatusConflict, "IDEMPOTENCY_CONFLICT")
	secondDry := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports/dry-runs", "", "", "", file)
	f.expect(secondDry, http.StatusOK, "")
	var duplicateDry configurationImportReport
	if err := json.Unmarshal(secondDry.Body.Bytes(), &duplicateDry); err != nil {
		t.Fatal(err)
	}
	second := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports", "second-key", `"`+duplicateDry.ReportSHA256+`"`, "", file)
	f.expect(second, http.StatusCreated, "")
	var duplicateReport configurationImportReport
	if err := json.Unmarshal(second.Body.Bytes(), &duplicateReport); err != nil {
		t.Fatal(err)
	}
	if duplicateReport.Counts.Accepted != 0 || duplicateReport.Counts.Duplicate != 1 || duplicateReport.Counts.Rejected != 0 || duplicateReport.Rows[0].Status != "duplicate" {
		t.Fatalf("same file on a new key was not a full duplicate: %+v", duplicateReport)
	}
	read := f.call(f.owner, http.MethodGet, first.Header().Get("Location"), "", "", "")
	f.expect(read, http.StatusOK, "")
	var stored configurationImportReport
	if err := json.Unmarshal(read.Body.Bytes(), &stored); err != nil || !reflect.DeepEqual(stored, firstReport) {
		t.Fatalf("GET import report differs: report=%+v error=%v", stored, err)
	}
}

func TestConfigurationImportDeterministicRejectedRow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	file := []byte("kind,ref,display_name,active,unit,line\nline,line-a,Assembly,true,count,line-a\n")
	var digest, path string
	for i := 0; i < 20; i++ {
		w := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports/dry-runs", "", "", "", file)
		f.expect(w, http.StatusOK, "")
		var report configurationImportReport
		if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if len(report.Rows) != 1 || report.Rows[0].Reason != "COLUMN_NOT_APPLICABLE" || report.Rows[0].Path != "unit" {
			t.Fatalf("iteration %d rejected row: %+v", i, report.Rows)
		}
		if i == 0 {
			digest, path = report.ReportSHA256, report.Rows[0].Path
		} else if report.ReportSHA256 != digest || report.Rows[0].Path != path {
			t.Fatalf("iteration %d changed digest/path: digest=%s path=%s; want digest=%s path=%s", i, report.ReportSHA256, report.Rows[0].Path, digest, path)
		}
	}
}

func TestConfigurationImportRevisionNameCollisions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	line := masterCreateLine(f, f.owner, f.site, "configuration-import-name-a", masterLineBody("Line A", true))
	masterCreateLine(f, f.owner, f.site, "configuration-import-name-b", masterLineBody("Taken", true))

	dryRun := func(file []byte) configurationImportReport {
		t.Helper()
		w := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports/dry-runs", "", "", "", file)
		f.expect(w, http.StatusOK, "")
		var report configurationImportReport
		if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		return report
	}

	current := []byte("kind,ref,id,revision,display_name,active\nline,line-a," + line.ID + ",1,Taken,false\n")
	report := dryRun(current)
	if report.Rows[0].Reason != "DISPLAY_NAME_EXISTS" || report.Rows[0].Path != "display_name" {
		t.Fatalf("revision onto another current name: %+v", report.Rows[0])
	}

	createName := []byte("kind,ref,id,revision,display_name,active\n" +
		"line,line-a," + line.ID + ",1,New name,false\n" +
		"line,line-new,,,New name,true\n")
	report = dryRun(createName)
	if report.Rows[0].Reason != "DISPLAY_NAME_EXISTS" || report.Rows[0].Path != "display_name" || report.Rows[1].Status != "accepted" {
		t.Fatalf("revision onto a create name: %+v", report.Rows)
	}

	self := []byte("kind,ref,id,revision,display_name,active\nline,line-a," + line.ID + ",1,Line A,false\n")
	report = dryRun(self)
	if report.Rows[0].Status != "accepted" {
		t.Fatalf("revision matching its own current name was treated as a collision: %+v", report.Rows[0])
	}
}

func TestConfigurationImportCreatedObjectChangedByMasterDataAPI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	file := []byte("kind,ref,display_name,active\nline,line-a,Imported line,true\n")
	dry := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports/dry-runs", "", "", "", file)
	f.expect(dry, http.StatusOK, "")
	var before configurationImportReport
	if err := json.Unmarshal(dry.Body.Bytes(), &before); err != nil {
		t.Fatal(err)
	}
	apply := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports", "create-line", `"`+before.ReportSHA256+`"`, "", file)
	f.expect(apply, http.StatusCreated, "")
	var applied configurationImportReport
	if err := json.Unmarshal(apply.Body.Bytes(), &applied); err != nil {
		t.Fatal(err)
	}
	objectID := applied.Rows[0].ObjectID
	if objectID == "" {
		t.Fatalf("created row omitted its deterministic ID: %+v", applied.Rows[0])
	}
	change := f.call(f.owner, http.MethodPost, "/v1/sites/"+f.site+"/lines/"+objectID+"/revisions", "changed-imported-line", `"1"`, f.encode(masterLineBody("Changed through D API", true)))
	f.expect(change, http.StatusCreated, "")

	dry = uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports/dry-runs", "", "", "", file)
	f.expect(dry, http.StatusOK, "")
	var after configurationImportReport
	if err := json.Unmarshal(dry.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if after.Rows[0].Reason != "OBJECT_ALREADY_CREATED" || after.Rows[0].Path != "ref" || after.Rows[0].Ref != "line-a" {
		t.Fatalf("changed deterministic create ID did not report its row ref: %+v", after.Rows[0])
	}
}

func TestConfigurationImportBounds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	var file strings.Builder
	file.WriteString("kind,ref,display_name,active\n")
	for i := 0; i < limits.ConfigImportRows; i++ {
		n := strconv.Itoa(i)
		file.WriteString("line,line-")
		file.WriteString(n)
		file.WriteString(",Timed line ")
		file.WriteString(n)
		file.WriteString(",true\n")
	}
	data := []byte(file.String())
	hash := sha256.Sum256([]byte(f.owner.token))
	actor := workstation.Actor{Issuer: f.issuer, Subject: f.owner.subject, SessionHash: hex.EncodeToString(hash[:])}
	scope := workstation.Scope{TenantID: f.tenant, SiteID: f.site}
	service := workstation.NewMasterData(f.runtime)

	measureLockHold := func(run func() error) (time.Duration, error) {
		t.Helper()
		blocker, err := f.control.BeginTx(ctx, nil)
		if err != nil {
			return 0, err
		}
		defer blocker.Rollback()
		var lockedSite string
		var blockerPID int
		if err := blocker.QueryRowContext(ctx, `SELECT pg_backend_pid(),id::text FROM supervisor.sites WHERE tenant_id=$1 AND id=$2 FOR NO KEY UPDATE`, scope.TenantID, scope.SiteID).Scan(&blockerPID, &lockedSite); err != nil {
			return 0, err
		}
		serviceDone := make(chan error, 1)
		go func() { serviceDone <- run() }()
		waitForLock := func(pid, blockerPID, queuedBehindPID int) error {
			deadline := time.Now().Add(30 * time.Second)
			for time.Now().Before(deadline) {
				var waiting bool
				if err := f.control.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND datname=current_database() AND state='active' AND wait_event_type='Lock' AND ($2=ANY(pg_blocking_pids(pid)) OR $3=ANY(pg_blocking_pids(pid))))`, pid, blockerPID, queuedBehindPID).Scan(&waiting); err != nil {
					return err
				}
				if waiting {
					return nil
				}
				time.Sleep(5 * time.Millisecond)
			}
			return errors.New("timed out waiting for site-row lock contention")
		}
		waitForImport := func() (int, error) {
			deadline := time.Now().Add(30 * time.Second)
			for time.Now().Before(deadline) {
				var pid int
				err := f.control.QueryRowContext(ctx, `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND state='active' AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid)) ORDER BY pid LIMIT 1`, blockerPID).Scan(&pid)
				if err == nil {
					return pid, nil
				}
				if !errors.Is(err, sql.ErrNoRows) {
					return 0, err
				}
				time.Sleep(5 * time.Millisecond)
			}
			return 0, errors.New("timed out waiting for import backend to contend on the site row")
		}
		importPID, err := waitForImport()
		if err != nil {
			return 0, err
		}
		if err := waitForLock(importPID, blockerPID, 0); err != nil {
			return 0, err
		}
		type probeStart struct {
			pid int
			err error
		}
		probeStarted := make(chan probeStart, 1)
		probeDone := make(chan error, 1)
		go func() {
			probe, err := f.control.BeginTx(ctx, nil)
			if err == nil {
				defer probe.Rollback()
				var pid int
				err = probe.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid)
				probeStarted <- probeStart{pid: pid, err: err}
				if err == nil {
					var id string
					err = probe.QueryRowContext(ctx, `SELECT id::text FROM supervisor.sites WHERE tenant_id=$1 AND id=$2 FOR NO KEY UPDATE`, scope.TenantID, scope.SiteID).Scan(&id)
					if err == nil {
						err = probe.Commit()
					}
				}
			} else {
				probeStarted <- probeStart{err: err}
			}
			probeDone <- err
		}()
		var probe probeStart
		select {
		case probe = <-probeStarted:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		if probe.err != nil {
			return 0, probe.err
		}
		if err := waitForLock(probe.pid, blockerPID, importPID); err != nil {
			return 0, err
		}
		if err := blocker.Rollback(); err != nil {
			return 0, err
		}
		started := time.Now()
		for _, completion := range []struct {
			name string
			done <-chan error
		}{{"import", serviceDone}, {"probe", probeDone}} {
			select {
			case err := <-completion.done:
				if err != nil {
					return 0, fmt.Errorf("%s backend after site lock release: %w", completion.name, err)
				}
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		return time.Since(started), nil
	}

	var dry workstation.ConfigurationImportReportV1
	dryRunDuration, err := measureLockHold(func() error {
		var err error
		dry, err = service.DryRunConfigurationImport(ctx, actor, scope, data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if dry.Counts.Accepted != limits.ConfigImportRows || !dry.Applicable {
		t.Fatalf("%d-row dry run: accepted=%d rejected=%d applicable=%v", limits.ConfigImportRows, dry.Counts.Accepted, dry.Counts.Rejected, dry.Applicable)
	}
	if dryRunDuration >= 5*time.Second {
		t.Fatalf("%d-row dry run site-lock hold reached the 5s writer lock timeout: %s", limits.ConfigImportRows, dryRunDuration)
	}

	var applied workstation.ConfigurationImportReportV1
	applyDuration, err := measureLockHold(func() error {
		var err error
		applied, _, _, err = service.ApplyConfigurationImport(ctx, actor, scope, data, "timed-apply", `"`+dry.ReportSHA256+`"`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied.Counts.Accepted != limits.ConfigImportRows || !applied.Applicable || applied.ReportSHA256 != dry.ReportSHA256 {
		t.Fatalf("%d-row apply differs: accepted=%d rejected=%d applicable=%v digestMatches=%v", limits.ConfigImportRows, applied.Counts.Accepted, applied.Counts.Rejected, applied.Applicable, applied.ReportSHA256 == dry.ReportSHA256)
	}
	if applyDuration >= 5*time.Second {
		t.Fatalf("%d-row apply site-lock hold reached the 5s writer lock timeout: %s", limits.ConfigImportRows, applyDuration)
	}
	t.Logf("%d-row site-lock hold: dry-run=%s apply=%s", limits.ConfigImportRows, dryRunDuration, applyDuration)
}

func TestConfigurationImportStaleReportAndRequiredIfMatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	file := []byte("kind,ref,display_name,active\nline,line-a,Will collide,true\n")
	dry := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports/dry-runs", "", "", "", file)
	f.expect(dry, http.StatusOK, "")
	var report configurationImportReport
	if err := json.Unmarshal(dry.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	masterCreateLine(f, f.owner, f.site, "concurrent-line", masterLineBody("Unrelated concurrent line", true))
	stale := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports", "stale-key", `"`+report.ReportSHA256+`"`, "", file)
	f.expect(stale, http.StatusPreconditionFailed, "REVISION_CONFLICT")
	missing := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports", "missing-match", "", "", file)
	f.expect(missing, http.StatusPreconditionRequired, "REVISION_REQUIRED")
	malformed := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports", "bad-match", `"not-a-digest"`, "", file)
	f.expect(malformed, http.StatusUnprocessableEntity, "INVALID_REVISION")
	var imports, lines int
	if err := f.control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.configuration_imports WHERE tenant_id=$1 AND site_id=$2`, f.tenant, f.site).Scan(&imports); err != nil {
		t.Fatal(err)
	}
	if err := f.control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.line_heads WHERE tenant_id=$1 AND site_id=$2`, f.tenant, f.site).Scan(&lines); err != nil {
		t.Fatal(err)
	}
	if imports != 0 || lines != 1 {
		t.Fatalf("stale apply persisted import or line: imports=%d lines=%d", imports, lines)
	}
}

func TestConfigurationImportViewerAndCrossSiteAreDenied(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	file := []byte("kind,display_name,active\nline,Scope line,true\n")
	dry := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports/dry-runs", "", "", "", file)
	f.expect(dry, http.StatusOK, "")
	var report configurationImportReport
	if err := json.Unmarshal(dry.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	etag := `"` + report.ReportSHA256 + `"`
	f.expect(uploadConfigurationImport(f, f.viewer, "/v1/sites/"+f.site+"/configuration-imports/dry-runs", "", "", "", file), http.StatusForbidden, "PERMISSION_DENIED")
	f.expect(uploadConfigurationImport(f, f.viewer, "/v1/sites/"+f.site+"/configuration-imports", "viewer-apply", etag, "", file), http.StatusForbidden, "PERMISSION_DENIED")
	f.expect(uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.otherSite+"/configuration-imports/dry-runs", "", "", "", file), http.StatusNotFound, "NOT_FOUND")
	apply := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports", "scope-apply", etag, "", file)
	f.expect(apply, http.StatusCreated, "")
	out := f.call(f.outsider, http.MethodGet, apply.Header().Get("Location"), "", "", "")
	f.expect(out, http.StatusNotFound, "NOT_FOUND")
}

func TestConfigurationImportRowsAndRequestBytesAreBounded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	var rows strings.Builder
	rows.WriteString("kind,display_name,active\n")
	for i := 0; i < limits.ConfigImportRows+1; i++ {
		rows.WriteString("line,Line ")
		rows.WriteString(strings.Repeat("x", 1))
		rows.WriteByte('0' + byte(i%10))
		rows.WriteString(",true\n")
	}
	w := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports/dry-runs", "", "", "", []byte(rows.String()))
	f.expect(w, http.StatusRequestEntityTooLarge, "IMPORT_ROWS_EXCEEDED")
	large := append([]byte("kind,display_name,active\nline,Big,true\n"), bytes.Repeat([]byte("x"), (10<<20)+1)...)
	w = uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports/dry-runs", "", "", "", large)
	f.expect(w, http.StatusRequestEntityTooLarge, "IMPORT_TOO_LARGE")
	var imports, lines int
	if err := f.control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.configuration_imports WHERE tenant_id=$1 AND site_id=$2`, f.tenant, f.site).Scan(&imports); err != nil {
		t.Fatal(err)
	}
	if err := f.control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.line_heads WHERE tenant_id=$1 AND site_id=$2`, f.tenant, f.site).Scan(&lines); err != nil {
		t.Fatal(err)
	}
	if imports != 0 || lines != 0 {
		t.Fatalf("bounded request persisted data: imports=%d lines=%d", imports, lines)
	}
}

func TestConfigurationImportRefsReportRejectedAndUnknownReferences(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	station := uuid.NewString()
	duplicateRefs := []byte("kind,ref,display_name,active,unit,line,item,steps\n" +
		"line,line-a,Assembly,true,,,,\n" +
		"line,line-a,Assembly two,true,,,,\n" +
		"item,item-a,Part,true,count,,,\n" +
		"routing,routing-a,Route,true,,line-a,item-a," + station + "\n")
	w := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports/dry-runs", "", "", "", duplicateRefs)
	f.expect(w, http.StatusOK, "")
	var report configurationImportReport
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Rows[0].Reason != "DUPLICATE_REF" || report.Rows[1].Reason != "DUPLICATE_REF" || report.Rows[3].Reason != "REFERENCE_REJECTED" {
		t.Fatalf("duplicate ref and dependent outcome: %+v", report.Rows)
	}
	unknown := []byte("kind,ref,display_name,active,unit,line,item,steps\n" +
		"routing,routing-a,Route,true,,missing-line,missing-item," + station + "\n")
	w = uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports/dry-runs", "", "", "", unknown)
	f.expect(w, http.StatusOK, "")
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Rows[0].Reason != "UNKNOWN_REF" || report.Rows[0].Path != "line" {
		t.Fatalf("unknown reference outcome: %+v", report.Rows[0])
	}
}

func TestConfigurationImportRevisionRowsAndForeignIDs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	line := masterCreateLine(f, f.owner, f.site, "configuration-import-before", masterLineBody("Before", true))
	foreign := masterCreateLine(f, f.outsider, f.otherSite, "configuration-import-foreign-line", masterLineBody("Foreign", true))
	change := []byte("kind,id,revision,display_name,active\nline," + line.ID + ",1,After,true\n")
	dry := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports/dry-runs", "", "", "", change)
	f.expect(dry, http.StatusOK, "")
	var report configurationImportReport
	if err := json.Unmarshal(dry.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Counts.Accepted != 1 || report.Rows[0].ExpectedRevision == nil || *report.Rows[0].ExpectedRevision != 1 {
		t.Fatalf("revision dry run: %+v", report)
	}
	apply := uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports", "revise-line", `"`+report.ReportSHA256+`"`, "", change)
	f.expect(apply, http.StatusCreated, "")
	unchanged := []byte("kind,id,revision,display_name,active\nline," + line.ID + ",2,After,true\n")
	dry = uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports/dry-runs", "", "", "", unchanged)
	f.expect(dry, http.StatusOK, "")
	if err := json.Unmarshal(dry.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Counts.Duplicate != 1 || report.Rows[0].Status != "duplicate" {
		t.Fatalf("unchanged revision was not duplicate: %+v", report.Rows[0])
	}
	stale := []byte("kind,id,revision,display_name,active\nline," + line.ID + ",1,Stale,true\n")
	dry = uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports/dry-runs", "", "", "", stale)
	f.expect(dry, http.StatusOK, "")
	if err := json.Unmarshal(dry.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Rows[0].Reason != "REVISION_CONFLICT" {
		t.Fatalf("stale revision row: %+v", report.Rows[0])
	}
	foreignFile := []byte("kind,id,revision,display_name,active\nline," + foreign.ID + ",1,Foreign,true\n")
	dry = uploadConfigurationImport(f, f.owner, "/v1/sites/"+f.site+"/configuration-imports/dry-runs", "", "", "", foreignFile)
	f.expect(dry, http.StatusOK, "")
	report = configurationImportReport{}
	if err := json.Unmarshal(dry.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Rows[0].Reason != "NOT_FOUND" || report.Rows[0].ObjectID != "" {
		t.Fatalf("foreign ID outcome leaked scope: %+v body=%s", report.Rows[0], dry.Body.String())
	}
}

func TestConfigurationImportMultipartCSRFAndQueryGuards(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	file := []byte("kind,display_name,active\nline,Guarded,true\n")
	path := "/v1/sites/" + f.site + "/configuration-imports/dry-runs"
	f.expect(uploadConfigurationImportOptions(f, f.owner, path, "", "", "", file, false, false), http.StatusForbidden, "CSRF_REJECTED")
	f.expect(uploadConfigurationImportOptions(f, f.owner, path, "", "", "", file, true, true), http.StatusUnprocessableEntity, "INVALID_REQUEST")
	f.expect(uploadConfigurationImport(f, f.owner, path+"?unexpected=1", "", "", "", file), http.StatusUnprocessableEntity, "INVALID_REQUEST")
}
