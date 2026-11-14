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
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type stationSession struct{ token, csrf, subject string }

// stationFixture is a WIS-520 database with one tenant site (owner, two supervisors, a
// supervisor whose authentication is stale, two operators and a viewer) and a second tenant
// whose supervisor is the outsider.
type stationFixture struct {
	t                                                    *testing.T
	ctx                                                  context.Context
	control, runtime                                     *sql.DB
	handler                                              http.Handler
	tenant, site, otherTenant, otherSite, issuer, origin string
	owner, lead, approver, stale, operator, viewer       stationSession
	outsider, spare                                      stationSession
}

func newStationFixture(t *testing.T, ctx context.Context) *stationFixture {
	t.Helper()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("real PostgreSQL admin and runtime URLs required")
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	database := "station_http_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	f := &stationFixture{t: t, ctx: ctx, control: connect(adminURL), runtime: connect(runtimeURL),
		tenant: uuid.NewString(), site: uuid.NewString(), otherTenant: uuid.NewString(), otherSite: uuid.NewString(),
		issuer: "https://station-http.invalid/" + uuid.NewString(), origin: "http://127.0.0.1:58178"}
	if err = migrate.Apply(ctx, f.control); err != nil {
		t.Fatal(err)
	}
	db, err := store.New(runtimeURLFor(runtimeURL, database))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	f.exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic station test'),($2,'other tenant')`, f.tenant, f.otherTenant)
	f.exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'station site'),($3,$4,'other site')`, f.tenant, f.site, f.otherTenant, f.otherSite)
	seed := func(tenant, site, role string, authAge time.Duration) stationSession {
		t.Helper()
		v := stationSession{token: uuid.NewString(), csrf: uuid.NewString(), subject: uuid.NewString()}
		sum := sha256.Sum256([]byte(v.token))
		f.exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, tenant, site, f.issuer, v.subject, role)
		f.exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now()-$5::interval)`, hex.EncodeToString(sum[:]), v.subject, f.issuer, v.csrf, strconv.Itoa(int(authAge.Seconds()))+" seconds")
		return v
	}
	f.owner, f.lead, f.approver = seed(f.tenant, f.site, "owner", 0), seed(f.tenant, f.site, "supervisor", 0), seed(f.tenant, f.site, "supervisor", 0)
	f.stale, f.operator, f.viewer = seed(f.tenant, f.site, "supervisor", 20*time.Minute), seed(f.tenant, f.site, "operator", 0), seed(f.tenant, f.site, "viewer", 0)
	f.outsider, f.spare = seed(f.otherTenant, f.otherSite, "supervisor", 0), seed(f.tenant, f.site, "operator", 0)
	auth, err := identity.New(ctx, identity.Config{Issuer: f.issuer, ClientID: "station-test", ClientSecret: "synthetic-only", RedirectURI: f.origin + "/auth/callback"}, f.runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.handler = New(config.Config{Environment: "local", WebDir: t.TempDir(), WorkstationCPSATPath: os.Getenv("SUPERVISOR_WORKSTATION_CPSAT_PATH")}, db, auth)
	return f
}

func (f *stationFixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, e := f.control.ExecContext(f.ctx, q, args...); e != nil {
		f.t.Fatal(e)
	}
}

func (f *stationFixture) call(actor stationSession, method, path, key, revision, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if actor.token != "" {
		r.AddCookie(&http.Cookie{Name: "supervisor", Value: actor.token})
	}
	r.Header.Set("Origin", f.origin)
	r.Header.Set("X-CSRF-Token", actor.csrf)
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	if revision != "" {
		r.Header.Set("If-Match", revision)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func (f *stationFixture) expect(w *httptest.ResponseRecorder, status int, code string) {
	f.t.Helper()
	if w.Code != status {
		f.t.Fatalf("HTTP %d want %d: %s", w.Code, status, w.Body.String())
	}
	if code != "" && !strings.Contains(w.Body.String(), `"code":"`+code+`"`) {
		f.t.Fatalf("problem code want %s: %s", code, w.Body.String())
	}
}

func (f *stationFixture) encode(v any) string {
	f.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

// instruction stores a document as the owner and, unless confirm is false, designates it
// and confirms revision 1 current. It returns the source ID, content hash and designation.
func (f *stationFixture) instruction(text string, confirm bool) (string, string, string) {
	return f.instructionAt(f.owner, f.site, text, confirm)
}

func (f *stationFixture) instructionAt(owner stationSession, site, text string, confirm bool) (string, string, string) {
	f.t.Helper()
	source := uuid.NewString()
	base := "/v1/sites/" + site + "/sources/" + source
	f.expect(f.call(owner, "POST", "/v1/sites/"+site+"/sources", uuid.NewString(), `"0"`, `{"source_id":"`+source+`","kind":"document","title":"Synthetic SOP","text":"`+text+`"}`), 200, "")
	sum := sha256.Sum256([]byte(text))
	if !confirm {
		return source, hex.EncodeToString(sum[:]), ""
	}
	designation := f.call(owner, "POST", base+"/authority-decisions", uuid.NewString(), `"0"`, `{"source_revision":1,"expected_source_state_revision":1,"verdict":"designate","reason":"Owner designated SOP"}`)
	f.expect(designation, 200, "")
	var authority struct {
		DecisionID string `json:"decision_id"`
	}
	if err := json.Unmarshal(designation.Body.Bytes(), &authority); err != nil || authority.DecisionID == "" {
		f.t.Fatalf("authority: %s", designation.Body.String())
	}
	f.expect(f.call(owner, "POST", base+"/currentness-decisions", uuid.NewString(), `"0"`, `{"source_revision":1,"expected_source_state_revision":1,"authority_decision_id":"`+authority.DecisionID+`","authority_revision":1,"verdict":"confirm_current","reason":"Owner confirms document is current"}`), 200, "")
	return source, hex.EncodeToString(sum[:]), authority.DecisionID
}

func orderBody(source, sha string, quantity int64) workstation.JobOrderBodyV1 {
	return workstation.JobOrderBodyV1{
		SchemaVersion: 1, OrderID: uuid.NewString(), OperationID: uuid.NewString(), ItemCode: "P-100", TargetQuantity: quantity, TargetCycleTimeSeconds: "2.5",
		Priority: 2, DueAt: "2026-10-20T18:00:00+02:00", InstructionDocumentID: source, InstructionRevision: 1, InstructionSHA256: sha, Template: "production",
		Fields: []workstation.FieldV1{{Name: "good_units", Label: "Good units", Kind: "decimal", Required: true, Unit: "count"}, {Name: "cycle_time", Label: "Cycle time", Kind: "decimal", Required: true, Unit: "s"}},
	}
}

func (f *stationFixture) decodeOrder(w *httptest.ResponseRecorder) workstation.JobOrderV1 {
	f.t.Helper()
	var o workstation.JobOrderV1
	if err := json.Unmarshal(w.Body.Bytes(), &o); err != nil {
		f.t.Fatal(err)
	}
	if w.Header().Get("ETag") != `"`+strconv.FormatInt(o.CurrentRevision, 10)+`"` {
		f.t.Fatalf("ETag %q does not carry current revision %d", w.Header().Get("ETag"), o.CurrentRevision)
	}
	return o
}

// jobOrder creates a job order as actor and returns it.
func (f *stationFixture) jobOrder(actor stationSession, body workstation.JobOrderBodyV1) workstation.JobOrderV1 {
	return f.jobOrderAt(actor, f.site, body)
}

func (f *stationFixture) jobOrderAt(actor stationSession, site string, body workstation.JobOrderBodyV1) workstation.JobOrderV1 {
	f.t.Helper()
	w := f.call(actor, "POST", "/v1/sites/"+site+"/job-orders", uuid.NewString(), "", f.encode(body))
	f.expect(w, 201, "")
	return f.decodeOrder(w)
}

// pin assigns the job order's current revision to a workstation for a shift plan.
func pin(station string, order workstation.JobOrderV1, quantity int64) workstation.AssignmentV1 {
	r := order.Revisions[order.CurrentRevision-1]
	return workstation.AssignmentV1{
		WorkstationID: station, JobOrderID: order.ID, JobOrderRevision: r.Revision, OrderID: r.Order.OrderID, OperationID: r.Order.OperationID,
		ItemCode: r.Order.ItemCode, TargetQuantity: quantity, TargetCycleTimeSeconds: "2.5",
		InstructionDocumentID: r.Order.InstructionDocumentID, InstructionRevision: r.Order.InstructionRevision, InstructionSHA256: r.Order.InstructionSHA256,
		Template: r.Order.Template, Fields: append([]workstation.FieldV1(nil), r.Order.Fields...),
	}
}

// refused runs statement as the runtime role in tenant scope and requires the exact SQLSTATE.
func (f *stationFixture) refused(tenant, name, code, message, statement string, args ...any) {
	f.t.Helper()
	rtx, err := f.runtime.BeginTx(f.ctx, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rtx.Rollback()
	if _, err = rtx.ExecContext(f.ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		f.t.Fatal(err)
	}
	if _, err = rtx.ExecContext(f.ctx, statement, args...); err == nil {
		err = rtx.Commit()
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code || !strings.Contains(pgErr.Message, message) {
		f.t.Fatalf("runtime %s: want %s %q, got %v", name, code, message, err)
	}
}

// TestJobOrderDeliveryHTTPPostgres proves WIS-520 acceptance item 2 against PostgreSQL: a
// digital job order with an approved instruction is delivered to the workstation its approved
// shift plan assigns, and a superseded order is never shown as current.
func TestJobOrderDeliveryHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	source, sha, sourceDesignation := f.instruction("Synthetic SOP: torque M8 to 25 Nm", true)
	orders := "/v1/sites/" + f.site + "/job-orders"
	plans := "/v1/sites/" + f.site + "/shift-plans"
	tag := func(revision int64) string { return `"` + strconv.FormatInt(revision, 10) + `"` }

	// Authority, strict contract and instruction approval on create.
	body := orderBody(source, sha, 200)
	f.expect(f.call(stationSession{}, "POST", orders, "c", "", f.encode(body)), 401, "AUTHENTICATION_REQUIRED")
	f.expect(f.call(f.viewer, "POST", orders, "c", "", f.encode(body)), 403, "PERMISSION_DENIED")
	f.expect(f.call(f.operator, "POST", orders, "c", "", f.encode(body)), 403, "PERMISSION_DENIED")
	f.expect(f.call(f.lead, "POST", orders, "", "", f.encode(body)), 422, "INVALID_REQUEST")
	for _, extra := range []string{`"tenant_id":"` + f.otherTenant + `"`, `"approved":true`, `"workstation_id":"` + uuid.NewString() + `"`} {
		f.expect(f.call(f.lead, "POST", orders, "c", "", strings.Replace(f.encode(body), `"schema_version":1`, `"schema_version":1,`+extra, 1)), 422, "INVALID_JOB_ORDER")
	}
	for name, mutate := range map[string]func(*workstation.JobOrderBodyV1){
		"priority above 9":   func(o *workstation.JobOrderBodyV1) { o.Priority = 10 },
		"due without offset": func(o *workstation.JobOrderBodyV1) { o.DueAt = "2026-10-20T18:00:00" },
		"zero quantity":      func(o *workstation.JobOrderBodyV1) { o.TargetQuantity = 0 },
		"script template":    func(o *workstation.JobOrderBodyV1) { o.Template = "script" },
	} {
		bad := orderBody(source, sha, 200)
		mutate(&bad)
		if w := f.call(f.lead, "POST", orders, "bad", "", f.encode(bad)); w.Code != 422 || !strings.Contains(w.Body.String(), `"code":"INVALID_JOB_ORDER"`) {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	unconfirmed, unconfirmedSHA, _ := f.instruction("Unconfirmed draft SOP", false)
	withdrawn, withdrawnSHA, designation := f.instruction("Withdrawn SOP", true)
	f.expect(f.call(f.owner, "POST", "/v1/sites/"+f.site+"/sources/"+withdrawn+"/currentness-decisions", uuid.NewString(), `"1"`, `{"source_revision":1,"expected_source_state_revision":1,"authority_decision_id":"`+designation+`","authority_revision":1,"verdict":"withdraw","reason":"Withdraw confirmation"}`), 200, "")
	for name, pinTo := range map[string][3]string{
		"unknown document":   {uuid.NewString(), sha},
		"wrong content hash": {source, strings.Repeat("ab", 32)},
		"unconfirmed":        {unconfirmed, unconfirmedSHA},
		"withdrawn":          {withdrawn, withdrawnSHA},
	} {
		if w := f.call(f.lead, "POST", orders, "instr-"+name, "", f.encode(orderBody(pinTo[0], pinTo[1], 200))); w.Code != 422 || !strings.Contains(w.Body.String(), `"code":"INSTRUCTION_NOT_APPROVED"`) {
			t.Fatalf("instruction %s: %d %s", name, w.Code, w.Body.String())
		}
	}

	// Create: revision 1, attributed, priority and due time round-trip (due normalized to UTC).
	created := f.call(f.lead, "POST", orders, "order-a", "", f.encode(body))
	f.expect(created, 201, "")
	orderA := f.decodeOrder(created)
	a1 := orderA.Revisions[0]
	if orderA.CurrentRevision != 1 || len(orderA.Revisions) != 1 || a1.State != workstation.JobOrderCurrent || a1.SubmittedBy.Subject != f.lead.subject ||
		a1.Order.Priority != 2 || a1.Order.DueAt != "2026-10-20T16:00:00Z" || a1.Order.TargetQuantity != 200 || a1.Order.InstructionSHA256 != sha || len(a1.CanonicalPayloadSHA256) != 64 {
		t.Fatalf("created order: %+v", orderA)
	}
	replay := f.call(f.lead, "POST", orders, "order-a", "", f.encode(body))
	f.expect(replay, 201, "")
	if o := f.decodeOrder(replay); !o.Replayed || o.ID != orderA.ID || len(o.Revisions) != 1 {
		t.Fatalf("replay: %+v", o)
	}
	f.expect(f.call(f.lead, "POST", orders, "order-a", "", f.encode(orderBody(source, sha, 7))), 409, "IDEMPOTENCY_CONFLICT")
	orderURL := orders + "/" + orderA.ID
	// Revise preconditions: If-Match required and current, body changed, order visible.
	changed := body
	changed.TargetQuantity = 180
	f.expect(f.call(f.owner, "POST", orderURL+"/revisions", "r", "", f.encode(changed)), 428, "REVISION_REQUIRED")
	f.expect(f.call(f.owner, "POST", orderURL+"/revisions", "r", `"5"`, f.encode(changed)), 412, "REVISION_CONFLICT")
	f.expect(f.call(f.owner, "POST", orderURL+"/revisions", "r", `"1"`, f.encode(body)), 409, "JOB_ORDER_UNCHANGED")
	f.expect(f.call(f.operator, "POST", orderURL+"/revisions", "r", `"1"`, f.encode(changed)), 403, "PERMISSION_DENIED")
	f.expect(f.call(f.owner, "POST", orders+"/"+uuid.NewString()+"/revisions", "r", `"1"`, f.encode(changed)), 404, "NOT_FOUND")
	f.expect(f.call(f.outsider, "GET", "/v1/sites/"+f.otherSite+"/job-orders/"+orderA.ID, "", "", ""), 404, "NOT_FOUND")
	f.expect(f.call(f.outsider, "GET", orderURL, "", "", ""), 404, "NOT_FOUND")

	// Plans bind each assignment to an existing, current, matching job-order revision.
	line, stationA, stationB := uuid.NewString(), uuid.NewString(), uuid.NewString()
	orderB := f.jobOrder(f.owner, orderBody(source, sha, 50))
	shift := func(lineID string, assignments ...workstation.AssignmentV1) workstation.ShiftPlanBodyV1 {
		return workstation.ShiftPlanBodyV1{SchemaVersion: 1, LineID: lineID, StartsAt: "2026-10-20T06:00:00+02:00", EndsAt: "2026-10-20T14:00:00+02:00", Timezone: "Europe/Berlin", Assignments: assignments}
	}
	unknown := pin(stationA, orderA, 120)
	unknown.JobOrderRevision = 2
	f.expect(f.call(f.lead, "POST", plans, "p-unknown", "", f.encode(shift(line, unknown))), 422, "JOB_ORDER_REVISION_UNKNOWN")
	foreign := pin(stationA, orderA, 120)
	foreign.JobOrderID = uuid.NewString()
	f.expect(f.call(f.lead, "POST", plans, "p-foreign", "", f.encode(shift(line, foreign))), 422, "JOB_ORDER_REVISION_UNKNOWN")
	for name, mutate := range map[string]func(*workstation.AssignmentV1){
		"item":                   func(a *workstation.AssignmentV1) { a.ItemCode = "P-999" },
		"order":                  func(a *workstation.AssignmentV1) { a.OrderID = uuid.NewString() },
		"instruction":            func(a *workstation.AssignmentV1) { a.InstructionRevision = 2 },
		"over target":            func(a *workstation.AssignmentV1) { a.TargetQuantity = 201 },
		"template":               func(a *workstation.AssignmentV1) { a.Template = "quality" },
		"missing required field": func(a *workstation.AssignmentV1) { a.Fields = a.Fields[:1] },
		"changed field unit":     func(a *workstation.AssignmentV1) { a.Fields[1].Unit = "ms" },
	} {
		bad := pin(stationA, orderA, 120)
		mutate(&bad)
		if w := f.call(f.lead, "POST", plans, "p-"+name, "", f.encode(shift(line, bad))); w.Code != 422 || !strings.Contains(w.Body.String(), `"code":"JOB_ORDER_MISMATCH"`) {
			t.Fatalf("mismatch %s: %d %s", name, w.Code, w.Body.String())
		}
	}
	// A partial quantity of the order is a valid shift target (carried forward later).
	planned := f.call(f.lead, "POST", plans, "plan", "", f.encode(shift(line, pin(stationA, orderA, 120), pin(stationB, orderB, 50))))
	f.expect(planned, 201, "")
	var plan workstation.ShiftPlanV1
	if err := json.Unmarshal(planned.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	// The derived assignment rows equal the stored payload, row for row.
	var derived, fromPayload string
	if err := f.control.QueryRowContext(ctx, `SELECT (SELECT string_agg(workstation_id||'/'||job_order_id||'/'||job_order_revision,',' ORDER BY workstation_id) FROM supervisor.shift_plan_assignments WHERE plan_id=$1 AND revision=1),(SELECT string_agg((a->>'workstation_id')||'/'||(a->>'job_order_id')||'/'||(a->>'job_order_revision'),',' ORDER BY (a->>'workstation_id')::uuid) FROM supervisor.shift_plan_revisions r,jsonb_array_elements(r.payload->'assignments') a WHERE r.plan_id=$1 AND r.revision=1)`, plan.ID).Scan(&derived, &fromPayload); err != nil {
		t.Fatal(err)
	}
	if derived == "" || derived != fromPayload || !strings.Contains(derived, stationA+"/"+orderA.ID+"/1") {
		t.Fatalf("derived assignments %q payload %q", derived, fromPayload)
	}
	current := func(station, at string) *httptest.ResponseRecorder {
		return f.call(f.operator, "GET", "/v1/sites/"+f.site+"/workstations/"+station+"/current-job-order?at="+url.QueryEscape(at), "", "", "")
	}
	decodeCurrent := func(w *httptest.ResponseRecorder) workstation.CurrentJobOrderV1 {
		t.Helper()
		f.expect(w, 200, "")
		var c workstation.CurrentJobOrderV1
		if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	morning := "2026-10-20T08:00:00+02:00"
	// A draft plan delivers nothing.
	f.expect(current(stationA, morning), 404, "NO_CURRENT_JOB_ORDER")
	approve := func(p workstation.ShiftPlanV1) *httptest.ResponseRecorder {
		return f.call(f.approver, "POST", plans+"/"+p.ID+"/approvals", uuid.NewString(), tag(p.StateRevision), f.encode(workstation.ShiftPlanApproveInputV1{CanonicalPayloadSHA256: p.Revisions[p.CurrentRevision-1].CanonicalPayloadSHA256}))
	}
	decodePlan := func(w *httptest.ResponseRecorder, status int) workstation.ShiftPlanV1 {
		t.Helper()
		f.expect(w, status, "")
		var p workstation.ShiftPlanV1
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	plan = decodePlan(approve(plan), 200)

	// Readback: each station gets its own order; operator and viewer can read.
	got := decodeCurrent(current(stationA, morning))
	if got.State != workstation.DeliveryCurrent || got.Order == nil || got.Order.Revision != 1 || got.Order.State != workstation.JobOrderCurrent || got.PlanID != plan.ID ||
		got.PlanRevision != 1 || got.JobOrderID != orderA.ID || got.PinnedRevision != 1 || got.CurrentRevision != 1 || got.TargetQuantity != 120 || got.Timezone != "Europe/Berlin" ||
		got.Order.Order.Priority != 2 || got.Order.Order.DueAt != "2026-10-20T16:00:00Z" || got.Order.Order.TargetQuantity != 200 || got.Order.Order.InstructionSHA256 != sha ||
		got.Order.CanonicalPayloadSHA256 != a1.CanonicalPayloadSHA256 || !got.StartsAt.Equal(time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC)) {
		t.Fatalf("station A delivery: %+v", got)
	}
	if b := decodeCurrent(current(stationB, morning)); b.State != workstation.DeliveryCurrent || b.JobOrderID != orderB.ID || b.Order == nil || b.Order.Order.OrderID != orderB.Revisions[0].Order.OrderID {
		t.Fatalf("station B delivery: %+v", b)
	}
	f.expect(f.call(f.viewer, "GET", "/v1/sites/"+f.site+"/workstations/"+stationA+"/current-job-order?at="+url.QueryEscape(morning), "", "", ""), 200, "")
	// The shift start instant is inside the half-open window.
	if c := decodeCurrent(current(stationA, "2026-10-20T06:00:00+02:00")); c.State != workstation.DeliveryCurrent {
		t.Fatalf("delivery at shift start: %+v", c)
	}
	// Rule 06: the instruction is re-checked on every read. Withdrawn confirmation means the
	// order is not delivered; re-confirming the same revision delivers it again.
	currentness := "/v1/sites/" + f.site + "/sources/" + source + "/currentness-decisions"
	decision := func(verdict, reason string) string {
		return `{"source_revision":1,"expected_source_state_revision":1,"authority_decision_id":"` + sourceDesignation + `","authority_revision":1,"verdict":"` + verdict + `","reason":"` + reason + `"}`
	}
	f.expect(f.call(f.owner, "POST", currentness, uuid.NewString(), `"1"`, decision("withdraw", "Withdraw for review")), 200, "")
	unavailable := current(stationA, morning)
	if c := decodeCurrent(unavailable); c.State != workstation.DeliveryInstructionUnavailable || c.InstructionReason != workstation.InstructionNotConfirmed || c.Order != nil ||
		!strings.Contains(unavailable.Body.String(), `"order":null`) || strings.Contains(unavailable.Body.String(), sha) {
		t.Fatalf("withdrawn instruction delivered: %s", unavailable.Body.String())
	}
	f.expect(f.call(f.owner, "POST", currentness, uuid.NewString(), `"2"`, decision("confirm_current", "Reconfirmed after review")), 200, "")
	if c := decodeCurrent(current(stationA, morning)); c.State != workstation.DeliveryCurrent || c.Order == nil || c.InstructionReason != "" {
		t.Fatalf("reconfirmed instruction: %+v", c)
	}
	// Half-open shift: the end instant, before the start and an unassigned station get nothing.
	f.expect(current(stationA, "2026-10-20T14:00:00+02:00"), 404, "NO_CURRENT_JOB_ORDER")
	f.expect(current(stationA, "2026-10-20T05:59:59+02:00"), 404, "NO_CURRENT_JOB_ORDER")
	f.expect(current(uuid.NewString(), morning), 404, "NO_CURRENT_JOB_ORDER")
	f.expect(f.call(f.operator, "GET", "/v1/sites/"+f.site+"/workstations/"+stationA+"/current-job-order?at=yesterday", "", "", ""), 422, "INVALID_REQUEST")
	f.expect(f.call(f.operator, "GET", "/v1/sites/"+f.site+"/workstations/"+stationA+"/current-job-order?station=x", "", "", ""), 422, "INVALID_REQUEST")
	f.expect(f.call(f.operator, "GET", "/v1/sites/"+f.site+"/workstations/not-a-uuid/current-job-order", "", "", ""), 422, "INVALID_JOB_ORDER")
	// Tenant isolation: another tenant sees neither the site nor any delivery for the station.
	f.expect(f.call(f.outsider, "GET", "/v1/sites/"+f.site+"/workstations/"+stationA+"/current-job-order?at="+url.QueryEscape(morning), "", "", ""), 404, "NOT_FOUND")
	f.expect(f.call(f.outsider, "GET", "/v1/sites/"+f.otherSite+"/workstations/"+stationA+"/current-job-order?at="+url.QueryEscape(morning), "", "", ""), 404, "NO_CURRENT_JOB_ORDER")

	// Snapshot order history; revising must append only.
	history := func() string {
		t.Helper()
		var s string
		if e := f.control.QueryRowContext(ctx, `SELECT string_agg(concat_ws('|',revision,payload::text,canonical_payload_sha256,actor_subject,created_at),',' ORDER BY revision) FROM supervisor.job_order_revisions WHERE job_order_id=$1`, orderA.ID).Scan(&s); e != nil {
			t.Fatal(e)
		}
		return s
	}
	before := history()
	// Revising the order supersedes the pinned revision: the station shows needs_replan with a
	// null order, never the superseded body; station B is unaffected.
	revised := f.call(f.owner, "POST", orderURL+"/revisions", "rev-a2", `"1"`, f.encode(changed))
	f.expect(revised, 201, "")
	orderA = f.decodeOrder(revised)
	if orderA.CurrentRevision != 2 || orderA.Revisions[0].State != workstation.JobOrderSuperseded || orderA.Revisions[1].State != workstation.JobOrderCurrent || orderA.Revisions[1].SubmittedBy.Subject != f.owner.subject {
		t.Fatalf("revised order: %+v", orderA)
	}
	if after := history(); !strings.HasPrefix(after, before+",") {
		t.Fatalf("order history changed:\nbefore %s\nafter  %s", before, after)
	}
	stale := current(stationA, morning)
	if c := decodeCurrent(stale); c.State != workstation.DeliveryNeedsReplan || c.Order != nil || c.PinnedRevision != 1 || c.CurrentRevision != 2 || c.JobOrderID != orderA.ID ||
		!strings.Contains(stale.Body.String(), `"order":null`) || strings.Contains(stale.Body.String(), "P-100") || strings.Contains(stale.Body.String(), sha) {
		t.Fatalf("stale pin delivered: %s", stale.Body.String())
	}
	if b := decodeCurrent(current(stationB, morning)); b.State != workstation.DeliveryCurrent {
		t.Fatalf("station B after A revised: %+v", b)
	}
	// A plan cannot newly pin, or be approved with, a superseded revision.
	oldPin := pin(stationA, orderA, 110)
	oldPin.JobOrderRevision = 1
	f.expect(f.call(f.owner, "POST", plans+"/"+plan.ID+"/revisions", "pin-old", tag(plan.StateRevision), f.encode(shift(line, oldPin, pin(stationB, orderB, 50)))), 409, "JOB_ORDER_NOT_CURRENT")
	repinned := decodePlan(f.call(f.owner, "POST", plans+"/"+plan.ID+"/revisions", "pin-2", tag(plan.StateRevision), f.encode(shift(line, pin(stationA, orderA, 120), pin(stationB, orderB, 50)))), 201)
	third := orderBody(source, sha, 200)
	third.OrderID, third.OperationID, third.TargetQuantity = changed.OrderID, changed.OperationID, 160
	orderA = f.decodeOrder(f.call(f.owner, "POST", orderURL+"/revisions", "rev-a3", `"2"`, f.encode(third)))
	f.expect(approve(repinned), 409, "JOB_ORDER_NOT_CURRENT")
	// Still the approved revision 1 in force, still needs_replan.
	if c := decodeCurrent(current(stationA, morning)); c.State != workstation.DeliveryNeedsReplan || c.PlanRevision != 1 || c.Order != nil {
		t.Fatalf("unapproved re-pin took effect: %+v", c)
	}
	// Re-pin to the current revision and re-approve: the station shows revision 3.
	repinned = decodePlan(f.call(f.owner, "POST", plans+"/"+plan.ID+"/revisions", "pin-3", tag(repinned.StateRevision), f.encode(shift(line, pin(stationA, orderA, 120), pin(stationB, orderB, 50)))), 201)
	plan = decodePlan(approve(repinned), 200)
	if c := decodeCurrent(current(stationA, morning)); c.State != workstation.DeliveryCurrent || c.Order == nil || c.Order.Revision != 3 || c.PlanRevision != 3 || c.Order.Order.TargetQuantity != 160 {
		t.Fatalf("re-approved delivery: %+v", c)
	}

	// A workstation cannot be booked by a second line's overlapping approved plan.
	orderC := f.jobOrder(f.lead, orderBody(source, sha, 30))
	other := decodePlan(f.call(f.lead, "POST", plans, "other-line", "", f.encode(shift(uuid.NewString(), pin(stationA, orderC, 30)))), 201)
	f.expect(approve(other), 409, "WORKSTATION_ASSIGNMENT_CONFLICT")
	if c := decodeCurrent(current(stationA, morning)); c.JobOrderID != orderA.ID || c.PlanID != plan.ID {
		t.Fatalf("double booking changed delivery: %+v", c)
	}

	// The database refuses the same bypasses for the runtime role.
	otherDigest := other.Revisions[0].CanonicalPayloadSHA256
	insertApproval := `INSERT INTO supervisor.shift_plan_approvals(tenant_id,site_id,plan_id,revision,canonical_payload_sha256,approver_issuer,approver_subject,policy_revision,auth_time) VALUES($1,$2,$3,1,$4,$5,$6,1,now())`
	f.refused(f.tenant, "double-booked approval", "23P01", "Workstation is assigned in an overlapping approved shift plan", insertApproval, f.tenant, f.site, other.ID, otherDigest, f.issuer, f.approver.subject)
	f.refused(f.tenant, "wrote an assignment", "42501", "permission denied", `INSERT INTO supervisor.shift_plan_assignments(tenant_id,site_id,plan_id,revision,workstation_id,job_order_id,job_order_revision) VALUES($1,$2,$3,1,$4,$5,1)`, f.tenant, f.site, other.ID, uuid.NewString(), orderC.ID)
	f.refused(f.tenant, "rewrote an assignment", "42501", "permission denied", `UPDATE supervisor.shift_plan_assignments SET job_order_revision=1 WHERE plan_id=$1`, plan.ID)
	f.refused(f.tenant, "rewrote order history", "42501", "permission denied", `UPDATE supervisor.job_order_revisions SET actor_subject='x' WHERE job_order_id=$1`, orderA.ID)
	f.refused(f.tenant, "deleted an order", "42501", "permission denied", `DELETE FROM supervisor.job_order_heads WHERE id=$1`, orderA.ID)
	f.refused(f.tenant, "rewound an order head", "23514", "Invalid job order head transition", `UPDATE supervisor.job_order_heads SET current_revision=1 WHERE id=$1`, orderA.ID)
	f.refused(f.otherTenant, "wrote into another tenant", "42501", "Job order tenant scope denied", `INSERT INTO supervisor.job_order_heads(tenant_id,site_id,id,current_revision) VALUES($1,$2,$3,1)`, f.tenant, f.site, uuid.NewString())
	orderC = f.decodeOrder(f.call(f.lead, "POST", orders+"/"+orderC.ID+"/revisions", "rev-c2", `"1"`, f.encode(orderBody(source, sha, 31))))
	f.refused(f.tenant, "stale pin approval", "23514", "pins a superseded job order revision", insertApproval, f.tenant, f.site, other.ID, otherDigest, f.issuer, f.approver.subject)
	// RLS: another tenant's context reads no job orders, assignments or deliveries.
	var visible int
	rtx, err := f.runtime.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = rtx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, f.otherTenant); err != nil {
		t.Fatal(err)
	}
	if err = rtx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM supervisor.job_order_heads)+(SELECT count(*) FROM supervisor.job_order_revisions)+(SELECT count(*) FROM supervisor.shift_plan_assignments)+(SELECT count(*) FROM supervisor.job_order_audit)`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	_ = rtx.Rollback()
	if visible != 0 {
		t.Fatalf("other tenant sees %d job order rows", visible)
	}

	// List and audit.
	var page workstation.JobOrderPageV1
	list := f.call(f.operator, "GET", orders+"?limit=2", "", "", "")
	f.expect(list, 200, "")
	if err = json.Unmarshal(list.Body.Bytes(), &page); err != nil || len(page.JobOrders) != 2 || page.NextCursor == "" {
		t.Fatalf("job order page: %s", list.Body.String())
	}
	f.expect(f.call(f.operator, "GET", orders+"?limit=0", "", "", ""), 422, "INVALID_REQUEST")
	// Every delivery read is audited with its outcome and the pinned revision.
	for event, revision := range map[string]int64{"job_order.delivered": 3, "delivery.needs_replan": 1, "delivery.instruction_unavailable": 1} {
		var n int
		if err = f.control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.job_order_audit WHERE event_type=$1 AND job_order_id=$2 AND revision=$3 AND actor_subject=$4`, event, orderA.ID, revision, f.operator.subject).Scan(&n); err != nil || n == 0 {
			t.Fatalf("%s audit rows=%d err=%v", event, n, err)
		}
	}

	// Defensive read: if a guard bypass ever left two approved plans assigning one station,
	// delivery refuses to choose (forced here with the guard disabled in this test database).
	f.exec(`ALTER TABLE supervisor.shift_plan_approvals DISABLE TRIGGER shift_plan_approval_guard`)
	forced, err := f.control.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = forced.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, f.tenant); err == nil {
		if _, err = forced.ExecContext(ctx, insertApproval, f.tenant, f.site, other.ID, otherDigest, f.issuer, f.approver.subject); err == nil {
			if _, err = forced.ExecContext(ctx, `UPDATE supervisor.shift_plan_heads SET approved_revision=1,state_revision=state_revision+1 WHERE id=$1`, other.ID); err == nil {
				err = forced.Commit()
			}
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`ALTER TABLE supervisor.shift_plan_approvals ENABLE TRIGGER shift_plan_approval_guard`)
	f.expect(current(stationA, morning), 409, "WORKSTATION_ASSIGNMENT_CONFLICT")

	// A newer source revision supersedes the pinned instruction; revocation withdraws it.
	sourceURL := "/v1/sites/" + f.site + "/sources"
	f.expect(f.call(f.owner, "POST", sourceURL, uuid.NewString(), `"1"`, `{"source_id":"`+source+`","kind":"document","title":"Synthetic SOP","text":"Synthetic SOP: torque M8 to 27 Nm"}`), 200, "")
	if c := decodeCurrent(current(stationB, morning)); c.State != workstation.DeliveryInstructionUnavailable || c.InstructionReason != workstation.InstructionSuperseded || c.Order != nil {
		t.Fatalf("superseded instruction: %+v", c)
	}
	f.expect(f.call(f.owner, "POST", sourceURL+"/"+source+"/revocations", uuid.NewString(), `"2"`, `{"reason":"Withdraw source access"}`), 200, "")
	if c := decodeCurrent(current(stationB, morning)); c.State != workstation.DeliveryInstructionUnavailable || c.InstructionReason != workstation.InstructionRevoked || c.Order != nil {
		t.Fatalf("revoked instruction: %+v", c)
	}
}

// TestJobOrderListCurrentHTTPPostgres proves the opt-in list body matches the detail route's
// current revision, reflects later revisions, preserves pagination, and remains tenant/site scoped.
func TestJobOrderListCurrentHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	source, sha, _ := f.instruction("Synthetic SOP: torque M8 to 25 Nm", true)
	orders := "/v1/sites/" + f.site + "/job-orders"
	first := f.jobOrder(f.lead, orderBody(source, sha, 200))
	second := f.jobOrder(f.owner, orderBody(source, sha, 80))

	list := func(actor stationSession, site, query string) workstation.JobOrderPageV1 {
		t.Helper()
		w := f.call(actor, "GET", "/v1/sites/"+site+"/job-orders"+query, "", "", "")
		f.expect(w, 200, "")
		var page workstation.JobOrderPageV1
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		return page
	}

	// The default response keeps the original two-field summary.
	plain := f.call(f.operator, "GET", orders+"?limit=100", "", "", "")
	f.expect(plain, 200, "")
	var plainPage struct {
		JobOrders []json.RawMessage `json:"job_orders"`
	}
	if err := json.Unmarshal(plain.Body.Bytes(), &plainPage); err != nil || len(plainPage.JobOrders) != 2 {
		t.Fatalf("default job-order list: %s err=%v", plain.Body.String(), err)
	}
	for _, raw := range plainPage.JobOrders {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 2 || fields["id"] == nil || fields["current_revision"] == nil || fields["current_order"] != nil {
			t.Fatalf("default summary changed: %s err=%v", raw, err)
		}
	}
	f.expect(f.call(f.operator, "GET", orders+"?include=history", "", "", ""), 422, "INVALID_REQUEST")
	for _, query := range []string{"?include=current&include=current", "?include=Current", "?include="} {
		f.expect(f.call(f.operator, "GET", orders+query, "", "", ""), 422, "INVALID_REQUEST")
	}

	// Opt-in responses keep ID ordering and the existing cursor boundary.
	ids := []string{first.ID, second.ID}
	sort.Strings(ids)
	var listedBefore, readsBefore int
	if err := f.control.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE event_type='job_order.listed' AND job_order_id IS NULL),count(*) FILTER (WHERE event_type='job_order.read' AND job_order_id IS NOT NULL) FROM supervisor.job_order_audit WHERE actor_subject=$1`, f.operator.subject).Scan(&listedBefore, &readsBefore); err != nil {
		t.Fatal(err)
	}
	page1 := list(f.operator, f.site, "?limit=1&include=current")
	if len(page1.JobOrders) != 1 || page1.JobOrders[0].ID != ids[0] || page1.NextCursor != ids[0] || page1.JobOrders[0].CurrentOrder == nil {
		t.Fatalf("first current-body page: %+v", page1)
	}
	var listedAfter, readsAfter, readCount int
	if err := f.control.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE event_type='job_order.listed' AND job_order_id IS NULL),count(*) FILTER (WHERE event_type='job_order.read' AND job_order_id IS NOT NULL),count(*) FILTER (WHERE event_type='job_order.read' AND job_order_id=$2) FROM supervisor.job_order_audit WHERE actor_subject=$1`, f.operator.subject, ids[0]).Scan(&listedAfter, &readsAfter, &readCount); err != nil {
		t.Fatal(err)
	}
	if listedAfter-listedBefore != 1 || readsAfter-readsBefore != 1 || readCount != 1 {
		t.Fatalf("current-body list audit: listed %d->%d, reads %d->%d, first-order reads=%d", listedBefore, listedAfter, readsBefore, readsAfter, readCount)
	}
	page2 := list(f.operator, f.site, "?limit=1&include=current&cursor="+page1.NextCursor)
	if len(page2.JobOrders) != 1 || page2.JobOrders[0].ID != ids[1] || page2.NextCursor != "" || page2.JobOrders[0].CurrentOrder == nil {
		t.Fatalf("second current-body page: %+v", page2)
	}
	if err := f.control.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE event_type='job_order.listed' AND job_order_id IS NULL),count(*) FILTER (WHERE event_type='job_order.read' AND job_order_id IS NOT NULL),count(*) FILTER (WHERE event_type='job_order.read' AND job_order_id=$2) FROM supervisor.job_order_audit WHERE actor_subject=$1`, f.operator.subject, ids[1]).Scan(&listedAfter, &readsAfter, &readCount); err != nil {
		t.Fatal(err)
	}
	if listedAfter-listedBefore != 2 || readsAfter-readsBefore != 2 || readCount != 1 {
		t.Fatalf("paged current-body audit: listed %d->%d, reads %d->%d, second-order reads=%d", listedBefore, listedAfter, readsBefore, readsAfter, readCount)
	}

	detail := f.decodeOrder(f.call(f.operator, "GET", orders+"/"+first.ID, "", "", ""))
	page := list(f.operator, f.site, "?limit=100&include=current")
	var firstSummary *workstation.JobOrderSummaryV1
	for i := range page.JobOrders {
		if page.JobOrders[i].ID == first.ID {
			firstSummary = &page.JobOrders[i]
		}
	}
	if firstSummary == nil || firstSummary.CurrentRevision != detail.CurrentRevision || firstSummary.CurrentOrder == nil ||
		!reflect.DeepEqual(*firstSummary.CurrentOrder, detail.Revisions[detail.CurrentRevision-1].Order) {
		t.Fatalf("list current body differs from detail: list=%+v detail=%+v", firstSummary, detail)
	}

	// A later head revision must replace the embedded current body.
	revised := detail.Revisions[detail.CurrentRevision-1].Order
	revised.TargetQuantity = 180
	f.expect(f.call(f.owner, "POST", orders+"/"+first.ID+"/revisions", "list-revision", `"1"`, f.encode(revised)), 201, "")
	detail = f.decodeOrder(f.call(f.operator, "GET", orders+"/"+first.ID, "", "", ""))
	page = list(f.operator, f.site, "?limit=100&include=current")
	firstSummary = nil
	for i := range page.JobOrders {
		if page.JobOrders[i].ID == first.ID {
			firstSummary = &page.JobOrders[i]
		}
	}
	if firstSummary == nil || firstSummary.CurrentRevision != 2 || firstSummary.CurrentOrder == nil || firstSummary.CurrentOrder.TargetQuantity != 180 ||
		!reflect.DeepEqual(*firstSummary.CurrentOrder, detail.Revisions[detail.CurrentRevision-1].Order) {
		t.Fatalf("revised list body differs from current detail: list=%+v detail=%+v", firstSummary, detail)
	}

	// Give another tenant the same site UUID, and this tenant a second site, so each scope
	// boundary is exercised independently through the public route.
	sameTenantOtherSite := uuid.NewString()
	f.exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'same-tenant other site')`, f.tenant, sameTenantOtherSite)
	f.exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'same-site other tenant')`, f.otherTenant, f.site)
	seedOwner := func(tenant, site string) stationSession {
		t.Helper()
		session := stationSession{token: uuid.NewString(), csrf: uuid.NewString(), subject: uuid.NewString()}
		tokenHash := sha256.Sum256([]byte(session.token))
		f.exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, tenant, site, f.issuer, session.subject)
		f.exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, hex.EncodeToString(tokenHash[:]), session.subject, f.issuer, session.csrf)
		return session
	}
	sameSiteOwner := seedOwner(f.tenant, sameTenantOtherSite)
	otherTenantOwner := seedOwner(f.otherTenant, f.site)
	sameSiteSource, sameSiteSHA, _ := f.instructionAt(sameSiteOwner, sameTenantOtherSite, "Same tenant other-site SOP", true)
	otherTenantSource, otherTenantSHA, _ := f.instructionAt(otherTenantOwner, f.site, "Other tenant same-site SOP", true)
	sameSiteOrder := f.jobOrderAt(sameSiteOwner, sameTenantOtherSite, orderBody(sameSiteSource, sameSiteSHA, 40))
	otherTenantOrder := f.jobOrderAt(otherTenantOwner, f.site, orderBody(otherTenantSource, otherTenantSHA, 30))
	page = list(f.operator, f.site, "?limit=100&include=current")

	// Both foreign rows exist and are visible only in their own tenant/site lists.
	if len(page.JobOrders) != 2 || page.JobOrders[0].ID == sameSiteOrder.ID || page.JobOrders[0].ID == otherTenantOrder.ID || page.JobOrders[1].ID == sameSiteOrder.ID || page.JobOrders[1].ID == otherTenantOrder.ID {
		t.Fatalf("foreign job order leaked into current tenant/site: %+v", page.JobOrders)
	}
	otherTenantPage := list(otherTenantOwner, f.site, "?include=current")
	if len(otherTenantPage.JobOrders) != 1 || otherTenantPage.JobOrders[0].ID != otherTenantOrder.ID || otherTenantPage.JobOrders[0].ID == first.ID {
		t.Fatalf("cross-tenant rows visible at same site UUID: %+v", otherTenantPage.JobOrders)
	}
	sameTenantOtherSitePage := list(sameSiteOwner, sameTenantOtherSite, "?include=current")
	if len(sameTenantOtherSitePage.JobOrders) != 1 || sameTenantOtherSitePage.JobOrders[0].ID != sameSiteOrder.ID || sameTenantOtherSitePage.JobOrders[0].ID == first.ID {
		t.Fatalf("cross-site rows visible in same tenant: %+v", sameTenantOtherSitePage.JobOrders)
	}
}

func TestJobOrderListCurrentRejectsTamperedRevisionHTTPPostgres(t *testing.T) {
	for _, tamper := range []string{"payload", "digest"} {
		t.Run(tamper, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
			defer cancel()
			f := newStationFixture(t, ctx)
			source, sha, _ := f.instruction("Synthetic SOP: torque M8 to 25 Nm", true)
			first := f.jobOrder(f.lead, orderBody(source, sha, 200))
			second := f.jobOrder(f.owner, orderBody(source, sha, 80))
			ids := []string{first.ID, second.ID}
			sort.Strings(ids)
			targetID := ids[1]

			f.exec(`ALTER TABLE supervisor.job_order_revisions DISABLE TRIGGER job_order_revisions_immutable`)
			switch tamper {
			case "payload":
				f.exec(`WITH changed AS (
				 SELECT tenant_id,site_id,job_order_id,revision,
				        jsonb_set(payload,'{target_quantity}','0'::jsonb) AS payload,
				        jsonb_set(canonical_payload::jsonb,'{body,target_quantity}','0'::jsonb)::text AS canonical_payload
				 FROM supervisor.job_order_revisions WHERE job_order_id=$1 AND revision=1 AND tenant_id=$2 AND site_id=$3
				)
				UPDATE supervisor.job_order_revisions r SET payload=c.payload,canonical_payload=c.canonical_payload,
				 canonical_payload_sha256=encode(sha256(convert_to(c.canonical_payload,'UTF8')),'hex')
				FROM changed c WHERE (r.tenant_id,r.site_id,r.job_order_id,r.revision)=(c.tenant_id,c.site_id,c.job_order_id,c.revision)`, targetID, f.tenant, f.site)
			case "digest":
				f.exec(`ALTER TABLE supervisor.job_order_revisions DROP CONSTRAINT job_order_revisions_check`)
				f.exec(`UPDATE supervisor.job_order_revisions SET canonical_payload_sha256=CASE WHEN left(canonical_payload_sha256,1)='0' THEN '1' ELSE '0' END||substr(canonical_payload_sha256,2) WHERE job_order_id=$1 AND revision=1 AND tenant_id=$2 AND site_id=$3`, targetID, f.tenant, f.site)
			}
			f.exec(`ALTER TABLE supervisor.job_order_revisions ENABLE TRIGGER job_order_revisions_immutable`)

			orders := "/v1/sites/" + f.site + "/job-orders"
			listed := f.call(f.operator, "GET", orders+"?limit=100&include=current", "", "", "")
			detail := f.call(f.operator, "GET", orders+"/"+targetID, "", "", "")
			var listProblem, detailProblem struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(listed.Body.Bytes(), &listProblem); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(detail.Body.Bytes(), &detailProblem); err != nil {
				t.Fatal(err)
			}
			if listed.Code != 503 || listed.Code != detail.Code || listProblem.Code != "STORAGE_UNAVAILABLE" || listProblem.Code != detailProblem.Code {
				t.Fatalf("list/detail integrity problems differ: list=%d %+v detail=%d %+v", listed.Code, listProblem, detail.Code, detailProblem)
			}
			var partial struct {
				JobOrders []json.RawMessage `json:"job_orders"`
			}
			if err := json.Unmarshal(listed.Body.Bytes(), &partial); err != nil || partial.JobOrders != nil {
				t.Fatalf("integrity failure returned a partial page: %s err=%v", listed.Body.String(), err)
			}
		})
	}
}
