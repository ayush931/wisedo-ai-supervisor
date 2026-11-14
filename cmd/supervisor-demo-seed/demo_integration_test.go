//go:build integration

package main

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/cases"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestDemoSeedIdempotentHTTPPostgres runs the demo builder twice against a disposable
// database: the second run must change no business row (only replay audit), leave no
// synthetic session behind, and press-01 must show its job order as current.
func TestDemoSeedIdempotentHTTPPostgres(t *testing.T) {
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
	database := "demo_seed_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.ExecContext(ctx, "CREATE DATABASE "+database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, e := admin.ExecContext(cleanup, "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	})
	in := func(raw string) string {
		u, e := url.Parse(raw)
		if e != nil {
			t.Fatal(e)
		}
		u.Path = "/" + database
		return u.String()
	}
	control, err := sql.Open("pgx", in(adminURL))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if err = migrate.Apply(ctx, control); err != nil {
		t.Fatal(err)
	}
	runtime, err := store.New(in(runtimeURL))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	tenant, site := uuid.NewString(), uuid.NewString()
	if _, err = control.ExecContext(ctx, `INSERT INTO supervisor.tenants(id,name) VALUES($1,'demo seed test')`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err = control.ExecContext(ctx, `INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'demo seed site')`, tenant, site); err != nil {
		t.Fatal(err)
	}
	subjects := map[string]string{ownerUsername: uuid.NewString()}
	for _, u := range demoUsers {
		subjects[u.username] = uuid.NewString()
	}
	scope := demoScope{control: control, runtime: runtime, tenant: tenant, site: site, issuer: "http://127.0.0.1:58077/realms/supervisor-local",
		origin: "http://127.0.0.1:58176", clientSecret: "synthetic-only", subjects: subjects, now: time.Now}

	// Row counts of every non-audit table: replays append audit rows by design.
	snapshot := func() map[string]int {
		t.Helper()
		rows, e := control.QueryContext(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema='supervisor' AND table_type='BASE TABLE' AND table_name NOT LIKE '%audit%'`)
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
		out := map[string]int{}
		for _, table := range tables {
			var n int
			if e = control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.`+table).Scan(&n); e != nil {
				t.Fatal(e)
			}
			if n > 0 {
				out[table] = n
			}
		}
		return out
	}

	first, err := buildDemo(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	after1 := snapshot()
	second, err := buildDemo(ctx, scope)
	if err != nil {
		t.Fatalf("re-run must be safe: %v", err)
	}
	after2 := snapshot()
	if !reflect.DeepEqual(after1, after2) {
		t.Fatalf("re-run changed business rows:\nfirst  %v\nsecond %v", after1, after2)
	}
	if first.Press != second.Press || first.Weld != second.Weld || first.Plan != second.Plan || first.Case == "" || first.Case != second.Case {
		t.Fatalf("re-run returned different ids: %+v vs %+v", first, second)
	}
	for table, want := range map[string]int{"job_order_heads": 2, "station_profile_heads": 2, "shift_plan_heads": 1, "shift_plan_approvals": 1, "station_records": 4, "memberships": 6, "browser_sessions": 0, "cases": 1, "plant_contexts": 2} {
		if after2[table] != want {
			t.Fatalf("%s has %d rows, want %d (%v)", table, after2[table], want, after2)
		}
	}
	// Approver differs from submitter (rule 05), as persisted.
	var submitter, approver string
	if err = control.QueryRowContext(ctx, `SELECT r.actor_subject,a.approver_subject FROM supervisor.shift_plan_revisions r JOIN supervisor.shift_plan_approvals a ON a.plan_id=r.plan_id WHERE r.plan_id=$1`, first.Plan).Scan(&submitter, &approver); err != nil {
		t.Fatal(err)
	}
	if submitter != subjects["shift-lead"] || approver != subjects["shift-approver"] {
		t.Fatalf("plan submitted by %s approved by %s", submitter, approver)
	}
	// The demo case is a real two-window shortfall opened by the case evaluator on the stable line.
	var lineKind, opener string
	if err = control.QueryRowContext(ctx, `SELECT r.payload->>'asset_kind',t.actor_subject FROM supervisor.cases c
 JOIN supervisor.knowledge_entity_heads h ON (h.tenant_id,h.site_id,h.id)=(c.tenant_id,c.site_id,c.line_id)
 JOIN supervisor.knowledge_entity_revisions r ON (r.tenant_id,r.site_id,r.id,r.revision)=(h.tenant_id,h.site_id,h.id,h.current_revision)
 JOIN supervisor.case_transitions t ON (t.tenant_id,t.site_id,t.case_id)=(c.tenant_id,c.site_id,c.id) AND t.revision=1
 WHERE c.id=$1`, first.Case).Scan(&lineKind, &opener); err != nil {
		t.Fatal(err)
	}
	if lineKind != "line" || opener != cases.EvaluatorActor.Subject {
		t.Fatalf("demo case line kind %q opened by %q", lineKind, opener)
	}
	// WIS-635: the demo case carries what a maintenance investigation needs: a supervisor-confirmed
	// assignee for the line and machine-fault evidence. The committed fixture script's investigation
	// then reaches a validated maintenance proposal on it (scripted fixture: a contract check, not
	// actual model behaviour).
	rs := agent.RunScope{TenantID: tenant, SiteID: site}
	st := agent.NewStore(runtime.DB)
	if err = st.InTx(ctx, rs, func(tx *sql.Tx) error {
		in, e := agent.LoadCaseInput(ctx, tx, rs, first.Case, agent.LoaderOptions{ModeEnabled: true})
		if e != nil {
			return e
		}
		faults := 0
		for _, ev := range in.Input.Evidence {
			if ev.Kind == recovery.EvidenceMachineFault {
				faults++
			}
		}
		if len(in.Input.Roster) != 1 || faults != 1 {
			t.Fatalf("demo case roster %+v, machine-fault evidence %d", in.Input.Roster, faults)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	provider, err := agent.LoadFixtureProvider("../../deploy/supervisor/agent-fixture-script.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	status, err := cases.NewService(runtime.DB).WorkflowStatus(ctx, cases.WorkflowRef{TenantID: tenant, SiteID: site, CaseID: first.Case})
	if err != nil || status.State != cases.Detected {
		t.Fatalf("demo case status %+v %v", status, err)
	}
	// The worker's own detected -> investigating move (WIS-636), as on a tick with the agent on.
	svc := cases.NewService(runtime.DB)
	if moved, e := svc.StartDetected(ctx, tenant, true, 100); e != nil || moved != 1 {
		t.Fatalf("StartDetected moved %d: %v", moved, e)
	}
	if status, err = svc.WorkflowStatus(ctx, cases.WorkflowRef{TenantID: tenant, SiteID: site, CaseID: first.Case}); err != nil || status.State != cases.Investigating {
		t.Fatalf("demo case after StartDetected %+v %v", status, err)
	}
	rt := &agent.Runtime{Store: st, Provider: provider, Tools: &agent.Tools{Store: st}, Slots: agent.NewSlots(2, 8), Limits: agent.DefaultLimits(),
		Model: "fixture-model", ModelRevision: "sha256:fixture", Options: agent.LoaderOptions{ModeEnabled: true}}
	res, err := rt.Investigate(ctx, agent.InvestigateRef{TenantID: tenant, SiteID: site, CaseID: first.Case}, status.Revision)
	if err != nil || res.Status != recovery.StatusProposed {
		t.Fatalf("scripted investigation of the demo case: %+v %v", res, err)
	}
	c := second.PressCurrent
	if c.State != workstation.DeliveryCurrent || c.PlanID != first.Plan || c.WorkstationID != first.Press || c.TargetQuantity != 120 || c.Order == nil || c.Order.Order.ItemCode != "Bracket A-100, op 10 Press" {
		t.Fatalf("press-01 current job order: %+v", c)
	}

	// WIS-631: the two browser-pilot tenants. A user without an identity subject is refused before
	// any membership is written; a full run is idempotent, and each pilot user belongs to exactly
	// one pilot site with its role, so the pilot's cross-tenant probes have someone to be refused.
	pilotMembers := func() int {
		t.Helper()
		var n int
		if e := control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.memberships WHERE site_id=ANY($1::uuid[])`, []string{pilotTenants[0].site, pilotTenants[1].site}).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	if err = seedPilotTenants(ctx, control, scope.issuer, subjects); err == nil || !strings.Contains(err.Error(), "chakan-owner") || pilotMembers() != 0 {
		t.Fatalf("pilot seed without pilot subjects: %v, %d memberships", err, pilotMembers())
	}
	for _, p := range pilotTenants {
		for _, u := range p.users {
			subjects[u.username] = uuid.NewString()
		}
	}
	for range 2 {
		if err = seedPilotTenants(ctx, control, scope.issuer, subjects); err != nil {
			t.Fatal(err)
		}
	}
	if n := pilotMembers(); n != 6 {
		t.Fatalf("pilot sites have %d memberships, want 6", n)
	}
	for _, p := range pilotTenants {
		for _, u := range p.users {
			var tenantOf, siteOf, role string
			var count int
			if err = control.QueryRowContext(ctx, `SELECT count(*) OVER (),tenant_id::text,site_id::text,role FROM supervisor.memberships WHERE subject=$1`, subjects[u.username]).Scan(&count, &tenantOf, &siteOf, &role); err != nil {
				t.Fatalf("%s: %v", u.username, err)
			}
			if count != 1 || tenantOf != p.tenant || siteOf != p.site || role != u.role {
				t.Fatalf("%s: %d memberships, first %s/%s as %s", u.username, count, tenantOf, siteOf, role)
			}
		}
	}
}
