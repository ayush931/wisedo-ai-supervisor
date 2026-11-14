//go:build integration

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/factorysim"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/httpapi"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plantcontext"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFactorySimulatorPublicAPI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("disposable PostgreSQL URLs required")
	}
	admin, e := sql.Open("pgx", adminURL)
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close()
	database := "factory_sim_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, e = admin.ExecContext(ctx, "CREATE DATABASE "+database); e != nil {
		t.Fatal(e)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if _, e := admin.ExecContext(cleanup, "DROP DATABASE "+database+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	}()
	in := func(raw string) string {
		u, e := url.Parse(raw)
		if e != nil {
			t.Fatal(e)
		}
		u.Path = "/" + database
		return u.String()
	}
	control, e := sql.Open("pgx", in(adminURL))
	if e != nil {
		t.Fatal(e)
	}
	defer control.Close()
	if e = migrate.Apply(ctx, control); e != nil {
		t.Fatal(e)
	}
	runtime, e := store.New(in(runtimeURL))
	if e != nil {
		t.Fatal(e)
	}
	defer runtime.Close()
	if e = runtime.VerifyRole(ctx); e != nil {
		t.Fatal(e)
	}
	sqlExec := func(q string, args ...any) {
		t.Helper()
		if _, e := control.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	sqlExec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'Sahyadri Precision Components')`, factorysim.TenantID)
	sqlExec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'Chakan Plant')`, factorysim.TenantID, factorysim.SiteID)
	// Identity/session fixture setup only. Every business write below uses the public HTTP API.
	server := httptest.NewUnstartedServer(nil)
	origin := "http://" + server.Listener.Addr().String()
	issuer := "http://127.0.0.1:59999/realms/supervisor-local"
	env := map[string]string{"SUPERVISOR_ENVIRONMENT": "local"}
	for _, name := range []string{"owner", "planner", "approver", "operator"} {
		role := name
		if name == "planner" || name == "approver" {
			role = "supervisor"
		}
		subject, token, csrf := uuid.NewString(), uuid.NewString(), uuid.NewString()
		sqlExec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, factorysim.TenantID, factorysim.SiteID, issuer, subject, role)
		sqlExec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, sha([]byte(token)), subject, issuer, csrf)
		env["SUPERVISOR_SIM_"+strings.ToUpper(name)+"_COOKIE"] = token
	}
	auth, e := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "factory-test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, runtime.DB, nil)
	if e != nil {
		t.Fatal(e)
	}
	handler, drain := httpapi.NewWithShutdown(config.Config{Environment: "local", WebDir: t.TempDir(), OEERunDeadline: time.Minute, OEELeaseDuration: 15 * time.Second, OEEMaxOpenRuns: 8, OEEMaxConcurrentExecutions: 2, OEEIdempotencyRetention: 90 * 24 * time.Hour}, runtime, auth)
	server.Config.Handler = handler
	server.Start()
	defer server.Close()
	defer func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if e := drain(stopCtx); e != nil {
			t.Error(e)
		}
	}()
	workerCtx, stopWorker := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	workerErrors := make(chan error, 1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(30 * time.Millisecond)
		defer ticker.Stop()
		worker := ingestion.New(runtime.DB)
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-ticker.C:
				refs, e := worker.Lagging(workerCtx, factorysim.TenantID)
				if e != nil {
					if workerCtx.Err() == nil {
						workerErrors <- e
					}
					return
				}
				for _, ref := range refs {
					if _, e = worker.Fold(workerCtx, ref, 1000); e != nil {
						if workerCtx.Err() == nil {
							workerErrors <- e
						}
						return
					}
				}
			}
		}
	}()
	defer func() {
		stopWorker()
		wg.Wait()
		select {
		case e := <-workerErrors:
			t.Error(e)
		default:
		}
	}()
	snapshots := func() map[string]int64 {
		t.Helper()
		rows, e := control.QueryContext(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema='supervisor' AND table_type='BASE TABLE' AND table_name NOT LIKE '%audit%' ORDER BY table_name`)
		if e != nil {
			t.Fatal(e)
		}
		var tables []string
		for rows.Next() {
			var name string
			if e = rows.Scan(&name); e != nil {
				t.Fatal(e)
			}
			tables = append(tables, name)
		}
		rows.Close()
		if e = rows.Err(); e != nil {
			t.Fatal(e)
		}
		counts := map[string]int64{}
		for _, name := range tables {
			var n int64
			if e = control.QueryRowContext(ctx, "SELECT count(*) FROM supervisor."+name).Scan(&n); e != nil {
				t.Fatal(e)
			}
			if n > 0 {
				counts[name] = n
			}
		}
		return counts
	}
	days := 1
	if os.Getenv("SUPERVISOR_FACTORY_SIM_14_DAYS") == "1" {
		days = 14
	}
	dir := filepath.Join(t.TempDir(), "dataset")
	var output bytes.Buffer
	started := time.Now()
	if e = run(ctx, []string{"generate", "--out", dir, "--days", fmt.Sprint(days), "--seed", "278"}, &output); e != nil {
		t.Fatal(e)
	}
	t.Log(output.String())
	d, e := readDataset(dir)
	if e != nil {
		t.Fatal(e)
	}
	client, e := newLoader(server.URL, func(k string) string { return env[k] })
	if e != nil {
		t.Fatal(e)
	}
	defer client.client.CloseIdleConnections()
	// The exact fixture guard must run before the first business write.
	extraTenant, extraSite := uuid.NewString(), uuid.NewString()
	sqlExec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'other fixture')`, extraTenant)
	sqlExec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'customer-like target')`, extraTenant, extraSite)
	var ownerSubject string
	if e = control.QueryRowContext(ctx, `SELECT subject FROM supervisor.browser_sessions WHERE token_hash=$1`, sha([]byte(env["SUPERVISOR_SIM_OWNER_COOKIE"]))).Scan(&ownerSubject); e != nil {
		t.Fatal(e)
	}
	sqlExec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, extraTenant, extraSite, issuer, ownerSubject)
	guardBefore := snapshots()
	if _, e = client.load(ctx, d); e == nil || !strings.Contains(e.Error(), "must belong only") {
		t.Fatalf("customer-like account not refused: %v", e)
	}
	if !reflect.DeepEqual(guardBefore, snapshots()) {
		t.Fatal("guard failure wrote business data")
	}
	sqlExec(`DELETE FROM supervisor.memberships WHERE tenant_id=$1`, extraTenant)
	sqlExec(`DELETE FROM supervisor.sites WHERE tenant_id=$1`, extraTenant)
	sqlExec(`DELETE FROM supervisor.tenants WHERE id=$1`, extraTenant)
	report, e := client.load(ctx, d)
	if e != nil {
		t.Fatal(e)
	}
	if report.ConfigurationRejected != 0 || report.Quarantined != 0 || report.ReadyContexts != 32 || report.ReadyOEE != 6*days || report.Answers != 54 {
		t.Fatalf("bad report: %+v", report)
	}
	// WIS-520: loaded simulator jobs carry routing links, so suggestions can place them
	// instead of refusing every job as ROUTING_UNLINKED (422 NO_FEASIBLE_SCHEDULE).
	var shift workstation.ShiftPlanBodyV1
	if e = json.Unmarshal(d.files["shifts/d00-l0-s0.json"], &shift); e != nil {
		t.Fatal(e)
	}
	selection := workstation.ShiftPlanSelectionV1{SchemaVersion: 1, LineID: client.bindings[shift.LineID], StartsAt: shift.StartsAt, EndsAt: shift.EndsAt, Timezone: shift.Timezone, JobOrderIDs: []string{}}
	for si := range shift.Assignments {
		selection.JobOrderIDs = append(selection.JobOrderIDs, client.bindings[d.id(fmt.Sprintf("job/d00-l0-s0-%d", si))])
	}
	var suggestion workstation.ShiftPlanSuggestionV1
	if e = client.call(ctx, "planner", "/shift-plans/suggestions", "", "", selection, &suggestion); e != nil {
		t.Fatal(e)
	}
	// Same-order steps run in sequence, so later steps of the 8-step line may exceed the
	// shift; the link is proven by step 1 being placed and no job refused as unlinked.
	placed := false
	for _, a := range suggestion.Assignments {
		placed = placed || a.JobOrderID == selection.JobOrderIDs[0]
	}
	for _, u := range suggestion.UnplacedOrders {
		if u.ReasonCode == "ROUTING_UNLINKED" || u.ReasonCode == "ROUTING_NOT_ON_LINE" {
			placed = false
		}
	}
	if !placed {
		t.Fatalf("simulator routing links not used: unplaced=%v assignments=%d", suggestion.UnplacedOrders, len(suggestion.Assignments))
	}
	contexts := map[string]plantcontext.Snapshot{}
	contextIndex := 0
	for _, file := range d.manifest.Files {
		if file.Schema != "PlantContextRequestV1" {
			continue
		}
		var view plantcontext.View
		if e = client.call(ctx, "owner", "/contexts/"+report.ContextIDs[contextIndex], "", "", nil, &view); e != nil {
			t.Fatal(e)
		}
		contexts[file.Path] = view.Snapshot
		contextIndex++
	}
	for _, pair := range []struct {
		line, a, b int
		qualifies  bool
	}{{0, 4, 5, true}, {1, 8, 9, true}, {0, 16, 17, true}, {1, 12, 13, false}} {
		first := contexts[fmt.Sprintf("contexts/d00-l%d-q%02d.json", pair.line, pair.a)]
		second := contexts[fmt.Sprintf("contexts/d00-l%d-q%02d.json", pair.line, pair.b)]
		rule := plantcontext.EvaluateShortfall(first, second)
		if rule.Qualifies != pair.qualifies {
			t.Fatalf("scenario shortfall %+v: %+v", pair, rule)
		}
	}
	t.Logf("first load: %s total=%s", wire(report), time.Since(started))
	before := snapshots()
	report2, e := client.load(ctx, d)
	if e != nil {
		t.Fatal(e)
	}
	after := snapshots()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("replay changed business rows before=%v after=%v", before, after)
	}
	if report2.Answers != 0 || !reflect.DeepEqual(report.ContextIDs, report2.ContextIDs) {
		t.Fatalf("not idempotent: %+v", report2)
	}
	var rejected int
	if e = control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.operational_event_records WHERE quarantine_reason IS NOT NULL`).Scan(&rejected); e != nil || rejected != 0 {
		t.Fatalf("quarantine=%d err=%v", rejected, e)
	}
	if days == 14 {
		// Run the actual command entry point as well as the in-process load/replay assertions.
		binary := filepath.Join(t.TempDir(), "factory-sim")
		build := exec.CommandContext(ctx, "go", "build", "-o", binary, ".")
		if b, e := build.CombinedOutput(); e != nil {
			t.Fatalf("build: %v %s", e, b)
		}
		cliDir := filepath.Join(t.TempDir(), "cli-dataset")
		gen := exec.CommandContext(ctx, binary, "generate", "--out", cliDir, "--days", "14", "--seed", "278")
		if b, e := gen.CombinedOutput(); e != nil {
			t.Fatalf("14-day CLI generate: %v %s", e, b)
		} else {
			t.Log(string(b))
		}
		cmd := exec.CommandContext(ctx, binary, "load", "--out", cliDir, "--url", server.URL)
		cmd.Env = os.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		if b, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("14-day CLI load: %v %s", e, b)
		} else {
			t.Log(string(b))
		}
	}
	t.Logf("final volumes=%v elapsed=%s", after, time.Since(started))
}
