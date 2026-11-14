//go:build integration

package pilot_test

import (
	"bytes"
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
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/approvals"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/cases"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/httpapi"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/outcome"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/tasks"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestWIS631OnboardingPilotTwoTenantsHTTPPostgres is the WIS-631 synthetic pilot: onboarding
// (document, interview, review, snapshot, cited chat) and then operations on what was approved
// (shift plan from the approved cycle time, shortfall, advisor, human approval, executed task,
// outcome observation). Two tenants run the same script at the same time through one
// API server over one database; each tenant's factory states different numbers, so any crossing
// shows up as the other tenant's value. Every step asserts its persisted rows, and every resource
// a step creates is probed from the other tenant afterwards: it must be the non-disclosing 404,
// identical (apart from the correlation ID) to the reply for an ID that never existed.
//
// Model output is scripted (agent.NewFixtureProvider) and executed through the worker's own entry
// points (Runtime.InterviewTurn, Runtime.ChatAnswer, Runtime.Investigate with the committed
// fixture script, the executor and the outcome observer): a contract check of the boundary, not
// evidence that a model onboards a factory or investigates a shortfall (rule 09).
//
// Right after completion the outcome is pending: it is decided on two complete 15-minute windows
// after the completion, by the database clock. The opt-in long run (pilot_outcome_integration_test.go,
// CASE=onboarding-pilot-outcome) waits for them.
func TestWIS631OnboardingPilotTwoTenantsHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	runPilot(t, ctx)
}

// pilotFactories: the first factory recovers after its task (100 of 100 parts per window), the
// second does not (80 of 100); only the long run uses recoveredUnits.
var pilotFactories = []factory{
	{name: "Chakan brackets", supplier: "Bharat Steel", leadDays: "14", cycleSeconds: "42", recoveredUnits: "100"},
	{name: "Pune hinges", supplier: "Jindal Steel", leadDays: "21", cycleSeconds: "38", recoveredUnits: "80"},
}

// runPilot runs both tenants' scripts concurrently, then checks that nothing crossed and probes
// every created resource from the other tenant. It returns the runs for further steps.
func runPilot(t *testing.T, ctx context.Context) (*pilotEnv, []*pilotRun) {
	t.Helper()
	// The advisory CP-SAT solver is optional; the greedy planner keeps the suggestion deterministic.
	t.Setenv("SUPERVISOR_WORKSTATION_CPSAT_PATH", "/missing/pilot-solver")
	env := newPilotEnv(t, ctx)
	factories := pilotFactories
	runs := make([]*pilotRun, len(factories))
	for i, f := range factories {
		runs[i] = env.tenant(t, f)
	}

	// Both tenants run the script concurrently; a failure stops only its own run.
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, r := range runs {
		wg.Add(1)
		go func(r *pilotRun) {
			defer wg.Done()
			<-start
			r.execute(ctx)
		}(r)
	}
	close(start)
	wg.Wait()
	for _, r := range runs {
		if r.err != nil {
			t.Fatalf("%s: %v", r.f.name, r.err)
		}
	}

	// Nothing crossed: each tenant's approved snapshot holds only its own factory's numbers.
	for i, r := range runs {
		other := runs[1-i]
		var contents string
		if err := env.admin.QueryRowContext(ctx, `SELECT canonical_contents FROM supervisor.knowledge_process_snapshots WHERE tenant_id=$1 AND snapshot_id=$2`, r.tenantID, r.snapshotID).Scan(&contents); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(contents, other.f.supplier) || strings.Contains(contents, other.tenantID) {
			t.Fatalf("%s snapshot contains %s's data", r.f.name, other.f.name)
		}
	}

	// Cross-tenant probes for every resource every step created.
	for i, r := range runs {
		other := runs[1-i]
		for _, p := range r.probes {
			env.probe(t, other, r, p)
		}
		if len(r.probes) < 25 {
			t.Fatalf("%s recorded only %d probes", r.f.name, len(r.probes))
		}
	}
	return env, runs
}

type factory struct{ name, supplier, leadDays, cycleSeconds, recoveredUnits string }

type actor struct{ token, csrf, hash, subject string }

// probe is a GET path under the site with the created IDs it contains.
type probe struct {
	step, path string
	ids        []string
}

type pilotEnv struct {
	admin   *sql.DB
	db      *store.Store
	handler http.Handler
	issuer  string
	origin  string
}

type pilotRun struct {
	env              *pilotEnv
	f                factory
	tenantID, siteID string
	owner, lead      actor
	approver         actor
	operator         actor
	probes           []probe
	snapshotID       string
	outcome          outcomeRun
	err              error
}

// outcomeRun is what the opt-in long run needs to keep measuring after step 10.
type outcomeRun struct {
	taskID, orderID, lineID string
	counts, machine         *feed
}

// failure unwinds a run's goroutine; t.Fatal may only be called from the test goroutine.
type failure struct{ msg string }

func (r *pilotRun) must(ok bool, format string, args ...any) {
	if !ok {
		panic(failure{fmt.Sprintf(format, args...)})
	}
}

func (r *pilotRun) record(step, path string, ids ...string) {
	r.probes = append(r.probes, probe{step: step, path: path, ids: ids})
}

func newPilotEnv(t *testing.T, ctx context.Context) *pilotEnv {
	t.Helper()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL admin and runtime URLs required")
	}
	control, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	name := "pilot_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = control.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, e := control.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	})
	in := func(raw string) string {
		u, e := url.Parse(raw)
		if e != nil {
			t.Fatal(e)
		}
		u.Path = "/" + name
		return u.String()
	}
	admin, err := sql.Open("pgx", in(adminURL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err = migrate.Apply(ctx, admin); err != nil {
		t.Fatal(err)
	}
	db, err := store.New(in(runtimeURL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = db.VerifyRole(ctx); err != nil {
		t.Fatal(err)
	}
	e := &pilotEnv{admin: admin, db: db, issuer: "https://pilot.invalid/" + uuid.NewString(), origin: "http://127.0.0.1:58176"}
	auth, err := identity.New(ctx, identity.Config{Issuer: e.issuer, ClientID: "pilot-test", ClientSecret: "synthetic-only", RedirectURI: e.origin + "/auth/callback"}, db.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	// One server for both tenants, with the scripted agent and chat answering on.
	e.handler = httpapi.New(config.Config{Environment: "local", WebDir: t.TempDir(),
		Agent: config.Agent{Provider: config.AgentProviderFixture, Model: "fixture-model", ModelRevision: "sha256:fixture", ModeEnabled: true, ChatAnswering: true}}, db, auth)
	return e
}

// tenant provisions one tenant, its site and three members with recent sign-ins (the privileged
// connection does only this, as the integration fixtures do; every business write is the API).
func (e *pilotEnv) tenant(t *testing.T, f factory) *pilotRun {
	t.Helper()
	r := &pilotRun{env: e, f: f, tenantID: uuid.NewString(), siteID: uuid.NewString()}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := e.admin.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,$2)`, r.tenantID, f.name)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,$3)`, r.tenantID, r.siteID, f.name+" plant")
	seed := func(role string) actor {
		a := actor{token: uuid.NewString(), csrf: uuid.NewString(), subject: uuid.NewString()}
		sum := sha256.Sum256([]byte(a.token))
		a.hash = hex.EncodeToString(sum[:])
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, a.hash, a.subject, e.issuer, a.csrf)
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, r.tenantID, r.siteID, e.issuer, a.subject, role)
		return a
	}
	r.owner, r.lead, r.approver, r.operator = seed("owner"), seed("supervisor"), seed("supervisor"), seed("operator")
	return r
}

type reply struct {
	code int
	body []byte
}

func (e *pilotEnv) do(a actor, method, path, contentType string, body []byte, headers map[string]string) reply {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "supervisor", Value: a.token})
	req.Header.Set("Origin", e.origin)
	req.Header.Set("X-CSRF-Token", a.csrf)
	req.Header.Set("Content-Type", contentType)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, req)
	return reply{code: w.Code, body: w.Body.Bytes()}
}

// call sends JSON as one member of the run's site. ifMatch < 0 sends no If-Match; a mutation
// gets a fresh Idempotency-Key.
func (r *pilotRun) call(a actor, method, path string, body any, ifMatch int64, want int, out any) {
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		r.must(err == nil, "encode %s %s: %v", method, path, err)
	}
	headers := map[string]string{}
	if method != "GET" {
		headers["Idempotency-Key"] = uuid.NewString()
	}
	if ifMatch >= 0 {
		headers["If-Match"] = `"` + strconv.FormatInt(ifMatch, 10) + `"`
	}
	got := r.env.do(a, method, "/v1/sites/"+r.siteID+path, "application/json", raw, headers)
	r.must(got.code == want, "%s %s: HTTP %d want %d: %s", method, path, got.code, want, got.body)
	if out != nil {
		r.must(json.Unmarshal(got.body, out) == nil, "decode %s %s: %s", method, path, got.body)
	}
}

func (r *pilotRun) query(ctx context.Context, q string, args []any, dest ...any) {
	err := r.env.admin.QueryRowContext(ctx, q, args...).Scan(dest...)
	r.must(err == nil, "%s: %v", q, err)
}

func (r *pilotRun) runtime(p agent.Provider) *agent.Runtime {
	st := agent.NewStore(r.env.db.DB)
	return &agent.Runtime{Store: st, Provider: p, Tools: &agent.Tools{Store: st}, Slots: agent.NewSlots(2, 8), Limits: agent.DefaultLimits(),
		Model: "fixture-model", ModelRevision: "sha256:fixture", Options: agent.LoaderOptions{ModeEnabled: true}}
}

func (r *pilotRun) execute(ctx context.Context) {
	defer func() {
		if p := recover(); p != nil {
			f, ok := p.(failure)
			if !ok {
				r.err = fmt.Errorf("panic: %v", p)
				return
			}
			r.err = fmt.Errorf("%s", f.msg)
		}
	}()
	f := r.f
	args := func(a ...any) []any { return append([]any{r.tenantID, r.siteID}, a...) }

	// Step 1a: the owner uploads the factory's SOP. The server detects the type from the bytes,
	// stores it as a document source and splits it into one citable span per paragraph.
	// Each section is long enough (over half of onboarding.MaxDocumentSpanRunes) to be its own
	// passage, so every fact cites the section that states it.
	section := func(s string) string {
		return s + strings.Repeat(" Follow the posted safety rules and record what you observe in the shift log.", 15)
	}
	paragraphs := []string{
		section("Steel coil comes from " + f.supplier + " with a lead time of " + f.leadDays + " days."),
		section("The press takes " + f.cycleSeconds + " seconds a part."),
		section("Pressed parts are checked at Gate 2 incoming QC."),
	}
	upload := r.env.do(r.owner, "POST", "/v1/sites/"+r.siteID+"/source-documents", "text/plain", []byte(strings.Join(paragraphs, "\n")),
		map[string]string{"X-Source-Title": url.PathEscape(f.name + " SOP"), "Idempotency-Key": uuid.NewString()})
	r.must(upload.code == 200, "upload: HTTP %d %s", upload.code, upload.body)
	var document struct {
		Source struct {
			SourceID      string `json:"source_id"`
			StateRevision int64  `json:"state_revision"`
		} `json:"source"`
		Spans []struct {
			ID string `json:"id"`
		} `json:"spans"`
	}
	r.must(json.Unmarshal(upload.body, &document) == nil && len(document.Spans) == len(paragraphs), "upload result %s", upload.body)
	doc := document.Source.SourceID
	spans := []string{document.Spans[0].ID, document.Spans[1].ID, document.Spans[2].ID}
	var kind string
	var spanCount int
	r.query(ctx, `SELECT r.kind,(SELECT count(*) FROM supervisor.evidence_spans s WHERE (s.tenant_id,s.site_id,s.source_id)=(h.tenant_id,h.site_id,h.source_id))
 FROM supervisor.evidence_heads h JOIN supervisor.evidence_revisions r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=(h.tenant_id,h.site_id,h.source_id,h.current_revision)
 WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.source_id=$3`, args(doc), &kind, &spanCount)
	r.must(kind == "document" && spanCount == 3, "persisted document kind=%s spans=%d", kind, spanCount)
	r.record("upload", "/sources/"+doc, doc)
	// Approval needs the cited source to be authoritative; the owner, not the agent, designates it.
	var source struct {
		StateRevision int64 `json:"state_revision"`
	}
	r.call(r.owner, "GET", "/sources/"+doc, nil, -1, 200, &source)
	var authority struct {
		DecisionID string `json:"decision_id"`
	}
	r.call(r.owner, "POST", "/sources/"+doc+"/authority-decisions", map[string]any{"source_revision": 1, "expected_source_state_revision": source.StateRevision, "verdict": "designate", "reason": "Owner verified the SOP"}, 0, 200, &authority)
	// The SOP is also the job orders' work instruction (step 5), which must be confirmed current.
	r.call(r.owner, "GET", "/sources/"+doc, nil, -1, 200, &source)
	r.call(r.owner, "POST", "/sources/"+doc+"/currentness-decisions", map[string]any{"source_revision": 1, "expected_source_state_revision": source.StateRevision,
		"authority_decision_id": authority.DecisionID, "authority_revision": 1, "verdict": "confirm_current", "reason": "Owner confirms the SOP is current"}, 0, 200, nil)
	var docSHA string
	r.query(ctx, `SELECT r.sha256 FROM supervisor.evidence_revisions r WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.source_id=$3 AND r.revision=1`, args(doc), &docSHA)

	// Step 1b: commissioning names the area, process, line and order (an agent may not propose
	// those), and registers the value-stream checklist. These are knowledge writes, so they come
	// before the snapshot the later steps rely on.
	register := uuid.NewString()
	r.call(r.owner, "POST", "/sources", map[string]string{"source_id": register, "kind": "manual", "title": "Commissioning register", "text": "Press area, bracket process, bracket line and its order"}, 0, 200, nil)
	areaID, processID, inventoryID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	op := operations{lineID: uuid.NewString(), orderID: uuid.NewString(), doc: doc, docSHA: docSHA}
	for _, e := range [][3]string{{areaID, "area", "Press area"}, {processID, "process", "Bracket process"}, {op.orderID, "order", "Order " + f.name}} {
		r.call(r.owner, "POST", "/entities", map[string]any{"entity_id": e[0], "type": e[1], "display_name": e[2], "source_id": register, "source_revision": 1}, 0, 200, nil)
	}
	r.call(r.owner, "POST", "/entities", map[string]any{"entity_id": op.lineID, "type": "asset", "asset_kind": "line", "display_name": "Bracket line", "source_id": register, "source_revision": 1}, 0, 200, nil)
	op.stationID = uuid.NewString()
	r.call(r.owner, "POST", "/entities", map[string]any{"entity_id": op.stationID, "type": "asset", "asset_kind": "machine", "display_name": "press-01", "source_id": register, "source_revision": 1}, 0, 200, nil)
	r.record("commissioned line", "/entities/"+op.lineID, op.lineID)
	scheduleText := "Order " + f.name + " on the bracket line at one part per second"
	op.scheduleSpan = r.sourceSpan(scheduleText)
	r.call(r.owner, "POST", "/discovery-inventories", map[string]any{"inventory_id": inventoryID, "area_id": areaID, "area_revision": 1, "process_id": processID, "process_revision": 1,
		"capability": "process_understanding", "reason": "Pilot value-stream interview"}, 0, 200, nil)
	r.record("inventory", "/discovery-inventories/"+inventoryID, inventoryID)

	// Step 1c: the shift lead is interviewed and answers a question in their own words.
	var session struct {
		ID       string `json:"session_id"`
		Revision int64  `json:"revision"`
	}
	r.call(r.lead, "POST", "/interviews", map[string]string{"role": "supervisor", "process_id": processID, "inventory_id": inventoryID}, -1, 201, &session)
	r.record("interview", "/interviews/"+session.ID, session.ID)
	var questions struct {
		Questions []struct {
			ID       string `json:"question_id"`
			Revision int64  `json:"revision"`
		} `json:"questions"`
	}
	r.call(r.lead, "GET", "/interviews/"+session.ID+"/questions", nil, -1, 200, &questions)
	r.must(len(questions.Questions) > 0, "the value-stream checklist asked no question")
	var answer struct {
		SourceID string `json:"source_id"`
	}
	r.call(r.lead, "POST", "/interviews/"+session.ID+"/answers", map[string]any{"question_id": questions.Questions[0].ID, "question_revision": questions.Questions[0].Revision,
		"text": "Coil arrives from " + f.supplier + "; the press is our pacing step.", "client_answer_id": uuid.NewString()}, -1, 201, &answer)
	var answered int
	r.query(ctx, `SELECT count(*) FROM supervisor.evidence_heads WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, args(answer.SourceID), &answered)
	r.must(answered == 1, "the answer was not stored as evidence")
	r.record("answer", "/sources/"+answer.SourceID, answer.SourceID)

	// Step 1d: an agent turn reads the SOP passages and proposes typed, grounded facts. The worker
	// runs it through Runtime.InterviewTurn; the scripted model knows only what the run gave it.
	r.call(r.lead, "GET", "/interviews/"+session.ID, nil, -1, 200, &session)
	var turn struct {
		RunID string `json:"run_id"`
	}
	r.call(r.lead, "POST", "/interviews/"+session.ID+"/agent-turns", map[string]any{"request_id": uuid.NewString(), "source_span_ids": spans}, session.Revision, 202, &turn)
	output, _ := json.Marshal(agent.InterviewTurnOutputV1{SchemaVersion: agent.SchemaVersion,
		RunID: uuid.NewSHA1(uuid.MustParse(turn.RunID), []byte(agent.RoleDiscoveryMapping)).String(), Role: agent.RoleDiscoveryMapping,
		Questions: []agent.InterviewTurnQuestion{},
		Entities: []agent.InterviewTurnEntity{
			{Ref: "coil", Type: "material", Name: "Steel coil", SourceSpanIDs: spans[:1]},
			{Ref: "mill", Type: "supplier", Name: f.supplier, SourceSpanIDs: spans[:1]},
			{Ref: "press", Type: "operation", Name: "press", SourceSpanIDs: spans[1:2]},
			{Ref: "qc", Type: "inspection_point", Name: "Gate 2 incoming QC", SourceSpanIDs: spans[2:3]},
		},
		Claims: []agent.InterviewTurnClaim{
			{Predicate: "supplied_by", Subject: "coil", Object: "mill", SourceSpanIDs: spans[:1]},
			{Predicate: "lead_time_days", Subject: "coil", Value: f.leadDays, Unit: "day", SourceSpanIDs: spans[:1]},
			{Predicate: "cycle_time_seconds", Subject: "press", Value: f.cycleSeconds, Unit: "s", SourceSpanIDs: spans[1:2]},
		}})
	provider := agent.NewFixtureProvider(
		agent.FixtureStep{Response: agent.ModelResponse{ToolCalls: []agent.ToolCall{{ID: "read-sop", Name: agent.ToolEvidenceGet, Arguments: []byte(`{"evidence_id":"` + doc + `"}`)}}}},
		agent.FixtureStep{Response: agent.ModelResponse{Content: string(output)}},
	)
	result, err := r.runtime(provider).InterviewTurn(ctx, agent.InterviewTurnRef{TenantID: r.tenantID, SiteID: r.siteID, SessionID: session.ID, RunID: turn.RunID})
	r.must(err == nil && result.OutcomeStatus == "proposed" && len(result.EntityIDs) == 4 && len(result.ClaimIDs) == 3, "interview turn: %+v (%v)", result, err)
	r.record("agent turn", "/interviews/"+session.ID+"/agent-turns/"+turn.RunID, session.ID, turn.RunID)
	var runState, runOutcome string
	r.query(ctx, `SELECT state,COALESCE(outcome_status,'') FROM supervisor.agent_runs WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, args(turn.RunID), &runState, &runOutcome)
	r.must(runState == agent.RunCompleted && runOutcome == "proposed", "persisted run state=%s outcome=%s", runState, runOutcome)
	claims := map[string]string{} // predicate -> claim ID
	for _, id := range result.ClaimIDs {
		var predicate, issuer string
		r.query(ctx, `SELECT predicate,actor_issuer FROM supervisor.knowledge_claim_revisions WHERE tenant_id=$1 AND site_id=$2 AND claim_id=$3 AND revision=1`, args(id), &predicate, &issuer)
		r.must(issuer == agent.ServiceActor.Issuer, "claim %s authored by %s, not the agent", predicate, issuer)
		claims[predicate] = id
		r.record("proposed claim", "/claims/"+id, id)
	}
	for _, id := range result.EntityIDs {
		r.record("proposed entity", "/entities/"+id, id)
		var entityType string
		r.query(ctx, `SELECT entity_type FROM supervisor.knowledge_entities WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, args(id), &entityType)
		if entityType == "operation" {
			op.pressOperationID = id // the onboarded operation the job order runs (step 5)
		}
	}
	r.must(op.pressOperationID != "", "the turn proposed no operation")
	var reviews int
	r.query(ctx, `SELECT count(*) FROM supervisor.knowledge_claim_reviews WHERE tenant_id=$1 AND site_id=$2 AND claim_id=ANY($3::uuid[])`, args(result.ClaimIDs), &reviews)
	r.must(reviews == 0 && len(claims) == 3, "an agent proposal arrived reviewed (%d) or a predicate is missing (%v)", reviews, claims)
	// The agent may not name assets, so a person links the commissioned station to the onboarded
	// operation, citing the SOP section; it is reviewed like the agent's facts.
	performs := uuid.NewString()
	r.call(r.owner, "POST", "/claims", map[string]any{"claim_id": performs, "subject_id": op.stationID, "subject_revision": 1, "predicate": "performs", "object_entity_id": op.pressOperationID,
		"object_revision": 1, "effective_from": "2026-01-01T00:00:00Z", "evidence_class": "report", "source_span_ids": spans[1:2]}, 0, 200, nil)
	claims["performs"] = performs

	// Step 2: a supervisor approves the supplier, cycle-time and station facts; the lead time stays a
	// candidate, so it must not reach the snapshot.
	for _, predicate := range []string{"supplied_by", "cycle_time_seconds", "performs"} {
		r.call(r.approver, "POST", "/claims/"+claims[predicate]+"/reviews", map[string]any{"claim_revision": 1, "verdict": "approve", "reason": "Matches the SOP"}, 1, 200, nil)
		var verdict, reviewer string
		r.query(ctx, `SELECT verdict,actor_subject FROM supervisor.knowledge_claim_reviews WHERE tenant_id=$1 AND site_id=$2 AND claim_id=$3 AND claim_revision=1`, args(claims[predicate]), &verdict, &reviewer)
		r.must(verdict == "approve" && reviewer == r.approver.subject, "%s review persisted as %s by %s", predicate, verdict, reviewer)
	}

	// Step 3: the owner publishes the approved process snapshot.
	var inventory struct {
		Revision int64 `json:"revision"`
	}
	r.call(r.owner, "GET", "/knowledge-inventory", nil, -1, 200, &inventory)
	r.snapshotID = uuid.NewString()
	r.call(r.owner, "POST", "/process-snapshots", map[string]string{"snapshot_id": r.snapshotID, "reason": "Pilot onboarding reviewed"}, inventory.Revision, 200, nil)
	r.record("snapshot", "/process-snapshots/"+r.snapshotID, r.snapshotID)
	var inSnapshot []string
	rows, err := r.env.admin.QueryContext(ctx, `SELECT claim_id::text FROM supervisor.knowledge_snapshot_claims WHERE tenant_id=$1 AND site_id=$2 AND snapshot_id=$3 ORDER BY claim_id`, r.tenantID, r.siteID, r.snapshotID)
	r.must(err == nil, "snapshot claims: %v", err)
	for rows.Next() {
		var id string
		r.must(rows.Scan(&id) == nil, "scan snapshot claim")
		inSnapshot = append(inSnapshot, id)
	}
	r.must(rows.Close() == nil, "close snapshot rows")
	want := []string{claims["supplied_by"], claims["cycle_time_seconds"], claims["performs"]}
	slices.Sort(want)
	r.must(reflect.DeepEqual(inSnapshot, want), "snapshot claims %v, want the three approved %v (not the lead time %s)", inSnapshot, want, claims["lead_time_days"])

	// Step 4: the lead asks chat; the answer cites the approved cycle-time passage.
	var conversation struct {
		ID       string `json:"conversation_id"`
		Revision int64  `json:"revision"`
	}
	r.call(r.lead, "POST", "/chat-conversations", map[string]string{"title": "Press questions"}, -1, 201, &conversation)
	r.record("chat conversation", "/chat-conversations/"+conversation.ID, conversation.ID)
	var posted struct {
		Reply struct {
			MessageID string `json:"message_id"`
			State     string `json:"state"`
		} `json:"reply"`
	}
	r.call(r.lead, "POST", "/chat-conversations/"+conversation.ID+"/messages", map[string]string{"request_id": uuid.NewString(), "text": "How many seconds does the press take a part?"}, conversation.Revision, 201, &posted)
	r.must(posted.Reply.State == "pending", "chat reply started %s", posted.Reply.State)
	var chatRun sql.NullString
	r.query(ctx, `SELECT run_id::text FROM supervisor.chat_messages WHERE tenant_id=$1 AND site_id=$2 AND message_id=$3`, args(posted.Reply.MessageID), &chatRun)
	r.must(chatRun.Valid, "the question started no chat run")
	statement := "The press takes " + f.cycleSeconds + " seconds a part."
	answerBody, _ := json.Marshal(agent.ChatAnswerOutputV2{SchemaVersion: agent.ChatSchemaVersion, RunID: uuid.NewSHA1(uuid.MustParse(chatRun.String), []byte(agent.RoleDiscoveryMapping)).String(),
		Role: agent.RoleDiscoveryMapping, Status: "answered", Statements: []agent.ChatStatement{{Kind: "approved_fact", Text: statement, SourceSpanIDs: spans[1:2]}}, MissingInputs: []string{}})
	chatProvider := agent.NewFixtureProvider(
		agent.FixtureStep{Response: agent.ModelResponse{ToolCalls: []agent.ToolCall{{ID: "read-sop", Name: agent.ToolEvidenceGet, Arguments: []byte(`{"evidence_id":"` + doc + `"}`)}}}},
		agent.FixtureStep{Response: agent.ModelResponse{Content: string(answerBody)}},
	)
	answered2, err := r.runtime(chatProvider).ChatAnswer(ctx, agent.ChatAnswerRef{TenantID: r.tenantID, SiteID: r.siteID, RunID: chatRun.String})
	r.must(err == nil && answered2.ReplyState == "answered", "chat answer: %+v (%v)", answered2, err)
	var page struct {
		Messages []struct {
			MessageID  string  `json:"message_id"`
			State      string  `json:"state"`
			Body       *string `json:"body"`
			SnapshotID *string `json:"snapshot_id"`
			Citations  []struct {
				Kind string `json:"kind"`
				ID   string `json:"id"`
			} `json:"citations"`
		} `json:"messages"`
	}
	r.call(r.lead, "GET", "/chat-conversations/"+conversation.ID+"/messages?limit=100", nil, -1, 200, &page)
	found := false
	for _, m := range page.Messages {
		if m.MessageID != posted.Reply.MessageID {
			continue
		}
		found = true
		cited := len(m.Citations) == 1 && m.Citations[0].ID == spans[1]
		r.must(m.State == "answered" && m.Body != nil && strings.Contains(*m.Body, statement) && cited && m.SnapshotID != nil && *m.SnapshotID == r.snapshotID,
			"chat reply %+v", m)
	}
	r.must(found, "the reply is not listed")
	var persisted string
	r.query(ctx, `SELECT state FROM supervisor.chat_messages WHERE tenant_id=$1 AND site_id=$2 AND message_id=$3`, args(posted.Reply.MessageID), &persisted)
	r.must(persisted == "answered", "persisted reply state %s", persisted)
	r.record("chat messages", "/chat-conversations/"+conversation.ID+"/messages", conversation.ID)

	op.cycleClaimID, op.cycleSpan = claims["cycle_time_seconds"], spans[1]
	r.operate(ctx, op)
}

// operations carries what onboarding produced into steps 5-10: the commissioned line and order,
// the onboarded press operation, its approved cycle time and the confirmed-current SOP.
type operations struct {
	lineID, orderID, pressOperationID string
	stationID                         string
	cycleClaimID, cycleSpan           string
	doc, docSHA, scheduleSpan         string
}

// sourceSpan creates a manual evidence source with one span over its text, as the owner.
func (r *pilotRun) sourceSpan(text string) string {
	id := uuid.NewString()
	var created struct {
		Revision      int64 `json:"revision"`
		StateRevision int64 `json:"state_revision"`
	}
	r.call(r.owner, "POST", "/sources", map[string]string{"source_id": id, "kind": "manual", "title": "Shift plan", "text": text}, 0, 200, &created)
	var span struct {
		ID string `json:"id"`
	}
	r.call(r.owner, "POST", "/sources/"+id+"/spans", map[string]any{"revision": created.Revision, "start_char": 0, "end_char": len(text)}, created.StateRevision, 200, &span)
	return span.ID
}

// shiftWindow is the 06:00/14:00/22:00 Asia/Kolkata shift containing now.
func shiftWindow(now time.Time) (time.Time, time.Time) {
	ist := time.FixedZone("IST", 5*3600+1800)
	local := now.In(ist)
	start := time.Date(local.Year(), local.Month(), local.Day(), 6, 0, 0, 0, ist)
	for start.After(local) {
		start = start.Add(-8 * time.Hour)
	}
	for !local.Before(start.Add(8 * time.Hour)) {
		start = start.Add(8 * time.Hour)
	}
	return start, start.Add(8 * time.Hour)
}

// feed is an operational source this run owns; it numbers its events and folds them the way the
// worker's FoldOperationalSource activity does.
type feed struct {
	r        *pilotRun
	id       string
	sequence int64
}

func (r *pilotRun) newFeed(scope string, mappings []map[string]any) *feed {
	f := &feed{r: r, id: uuid.NewString()}
	r.call(r.owner, "POST", "/operational-sources", map[string]any{"source_id": f.id, "type": "event_api", "authority_scope": scope, "enabled": true, "access_policy_revision": 1, "freshness_seconds": 900, "clock_skew_seconds": 30}, 0, 200, nil)
	r.call(r.owner, "POST", "/operational-sources/"+f.id+"/mappings", map[string]any{"mappings": mappings}, 1, 200, nil)
	r.call(r.approver, "POST", "/operational-sources/"+f.id+"/reviews", map[string]string{"verdict": "approved", "reason": "Pilot telemetry scope verified"}, 2, 200, nil)
	r.record("operational source", "/operational-sources/"+f.id, f.id)
	return f
}

func (f *feed) send(ctx context.Context, events ...map[string]any) {
	for _, e := range events {
		f.sequence++
		e["source_sequence"], e["revision"] = f.sequence, 1
	}
	f.r.call(f.r.owner, "POST", "/operational-sources/"+f.id+"/batches", map[string]any{"schema_version": 1, "source_id": f.id, "source_revision": 2, "mapping_revision": 2, "events": events}, -1, 202, nil)
	_, err := ingestion.New(f.r.env.db.DB).Fold(ctx, ingestion.SourceRef{TenantID: f.r.tenantID, SiteID: f.r.siteID, SourceID: f.id}, 500)
	f.r.must(err == nil, "fold %s: %v", f.id, err)
}

func (f *feed) heartbeat(ctx context.Context) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	f.send(ctx, map[string]any{"external_key": "heartbeat-" + strconv.FormatInt(f.sequence+1, 10), "kind": "heartbeat", "event_at": now, "payload": map[string]any{"observed_at": now}})
}

// operate runs steps 5-10 on what onboarding approved.
func (r *pilotRun) operate(ctx context.Context, op operations) {
	args := func(a ...any) []any { return append([]any{r.tenantID, r.siteID}, a...) }
	stamp := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }

	// Step 5: a shift plan built from the approved times. The press job order runs the onboarded
	// operation; the suggestion compares its cycle time with the approved one and cites it.
	// Planning master data has its own server-issued line ID: the planning line and the
	// commissioned line asset are not linked; the station (press-01) is what joins the two.
	station := op.stationID
	var planningLine workstation.LineV1
	r.call(r.owner, "POST", "/lines", workstation.LineBodyV1{SchemaVersion: 1, DisplayName: "Bracket line", Active: true}, 0, 201, &planningLine)
	r.record("planning line", "/lines/"+planningLine.ID, planningLine.ID)
	profile := workstation.StationProfileBodyV1{SchemaVersion: 1, WorkstationID: station, LineID: planningLine.ID, DisplayName: "press-01", Template: "production",
		Fields: []workstation.ProfileFieldV1{{Name: "good_units", Label: "Good quantity", Kind: "decimal", Required: true, Unit: "count"}}}
	r.call(r.lead, "POST", "/workstations/"+station+"/profile-revisions", profile, 0, 201, nil)
	var item workstation.ItemV1
	r.call(r.owner, "POST", "/items", workstation.ItemBodyV1{SchemaVersion: 1, DisplayName: "Bracket", Unit: workstation.MasterDataUnitCount, Active: true}, 0, 201, &item)
	var route workstation.RoutingV1
	r.call(r.owner, "POST", "/routings", workstation.RoutingBodyV1{SchemaVersion: 1, DisplayName: "Bracket route", Active: true, LineID: planningLine.ID, ItemID: item.ID,
		Steps: []workstation.RoutingStepV1{{StepNumber: 1, WorkstationID: station}}}, 0, 201, &route)
	r.record("routing", "/routings/"+route.ID, route.ID)
	step := 1
	order := workstation.JobOrderBodyV1{SchemaVersion: 1, OrderID: op.orderID, OperationID: op.pressOperationID, ItemCode: "Bracket, op 10 Press", ItemID: item.ID, RoutingID: route.ID, StepNumber: &step,
		TargetQuantity: 120, TargetCycleTimeSeconds: r.f.cycleSeconds, Priority: 2, InstructionDocumentID: op.doc, InstructionRevision: 1, InstructionSHA256: op.docSHA,
		Template: "production", Fields: []workstation.FieldV1{{Name: "good_units", Label: "Good quantity", Kind: "decimal", Required: true, Unit: "count"}}}
	var job workstation.JobOrderV1
	r.call(r.lead, "POST", "/job-orders", order, -1, 201, &job)
	r.record("job order", "/job-orders/"+job.ID, job.ID)
	starts, ends := shiftWindow(time.Now())
	var suggestion workstation.ShiftPlanSuggestionV1
	r.call(r.lead, "POST", "/shift-plans/suggestions", map[string]any{"schema_version": 1, "line_id": planningLine.ID, "starts_at": starts.Format(time.RFC3339), "ends_at": ends.Format(time.RFC3339),
		"timezone": "Asia/Kolkata", "job_order_ids": []string{job.ID}}, -1, 200, &suggestion)
	r.must(len(suggestion.ApprovedCycleTimes) == 1, "approved cycle times %+v", suggestion.ApprovedCycleTimes)
	cmp := suggestion.ApprovedCycleTimes[0]
	r.must(cmp.Status == workstation.CycleTimeMatches && cmp.ApprovedSeconds == r.f.cycleSeconds && reflect.DeepEqual(cmp.ClaimIDs, []string{op.cycleClaimID}) &&
		reflect.DeepEqual(cmp.SourceSpanIDs, []string{op.cycleSpan}) && suggestion.ProcessSnapshotID == r.snapshotID && len(suggestion.Assignments) == 1,
		"suggestion did not compare with the approved cycle time: %+v snapshot=%s", cmp, suggestion.ProcessSnapshotID)
	var plan workstation.ShiftPlanV1
	r.call(r.lead, "POST", "/shift-plans", workstation.ShiftPlanBodyV1{SchemaVersion: 1, LineID: planningLine.ID, StartsAt: starts.Format(time.RFC3339), EndsAt: ends.Format(time.RFC3339),
		Timezone: "Asia/Kolkata", Assignments: suggestion.Assignments}, -1, 201, &plan)
	r.call(r.approver, "POST", "/shift-plans/"+plan.ID+"/approvals", workstation.ShiftPlanApproveInputV1{CanonicalPayloadSHA256: plan.Revisions[plan.CurrentRevision-1].CanonicalPayloadSHA256}, plan.StateRevision, 200, nil)
	var planApprover string
	r.query(ctx, `SELECT approver_subject FROM supervisor.shift_plan_approvals WHERE tenant_id=$1 AND site_id=$2 AND plan_id=$3`, args(plan.ID), &planApprover)
	r.must(planApprover == r.approver.subject, "shift plan approved by %s", planApprover)
	r.record("shift plan", "/shift-plans/"+plan.ID, plan.ID)

	// Step 6: a shortfall. An approved production plan expects 100 parts per 15-minute window;
	// telemetry reports 80 in two consecutive windows, with a machine stoppage in the second. The
	// worker's fold and case evaluator open the case.
	end := time.Now().UTC().Truncate(15 * time.Minute).Add(-15 * time.Minute)
	windows := []time.Time{end.Add(-30 * time.Minute), end.Add(-15 * time.Minute), end}
	// The plan runs on for three hours, so the windows after the task are planned too (the outcome
	// compares them with the same approved plan); each window has 100 productive seconds.
	planEnd := windows[2].Add(3 * time.Hour)
	breaks := []map[string]string{}
	for w := windows[0]; w.Before(planEnd); w = w.Add(15 * time.Minute) {
		breaks = append(breaks, map[string]string{"start": stamp(w.Add(100 * time.Second)), "end": stamp(w.Add(15 * time.Minute))})
	}
	production := map[string]any{"order_id": op.orderID, "asset_id": op.lineID, "window_start": stamp(windows[0]), "window_end": stamp(planEnd), "plan_revision": 1, "unit": "count",
		"rate_segments":  []map[string]string{{"start": stamp(windows[0]), "end": stamp(planEnd), "target_good_units_per_second": "1"}},
		"planned_breaks": breaks}
	var captured struct {
		ID string `json:"id"`
	}
	r.call(r.lead, "POST", "/plan-revisions", map[string]any{"plan": production, "source_span_ids": []string{op.scheduleSpan}}, -1, 201, &captured)
	r.call(r.approver, "POST", "/plan-revisions/"+captured.ID+"/reviews", map[string]string{"decision": "approve", "reason": "Matches the shift plan"}, 0, 200, nil)
	r.record("production plan", "/plan-revisions/"+captured.ID, captured.ID)
	var line, order2 struct {
		Revision int64 `json:"revision"`
	}
	r.call(r.owner, "GET", "/entities/"+op.lineID, nil, -1, 200, &line)
	r.call(r.owner, "GET", "/entities/"+op.orderID, nil, -1, 200, &order2)
	lineMap := map[string]any{"external_type": "asset", "external_key": "bracket-line", "entity_id": op.lineID, "entity_revision": line.Revision}
	counts := r.newFeed("production_delta,heartbeat,watermark", []map[string]any{lineMap, {"external_type": "order", "external_key": "order-1", "entity_id": op.orderID, "entity_revision": order2.Revision}})
	machine := r.newFeed("machine_state,heartbeat", []map[string]any{lineMap})
	r.call(r.approver, "POST", "/maintenance-roster/decisions", map[string]any{"line_id": op.lineID, "assignee_subject": r.operator.subject, "decision": "confirm", "reason": "Bracket line maintenance on call"}, 0, 200, nil)
	machine.send(ctx, map[string]any{"external_key": "stop-1", "kind": "machine_state", "event_at": stamp(windows[1].Add(5 * time.Minute)), "asset_external_key": "bracket-line", "payload": map[string]any{"state": "stopped"}})
	machine.heartbeat(ctx)
	contexts := []string{}
	for i := 0; i < 2; i++ {
		delta := map[string]any{"order_id": op.orderID, "asset_id": op.lineID, "interval_start": stamp(windows[i]), "interval_end": stamp(windows[i+1]), "good_units": "80", "unit": "count"}
		counts.send(ctx, map[string]any{"external_key": "delta-" + strconv.Itoa(i), "kind": "production_delta", "event_at": stamp(windows[i+1]), "asset_external_key": "bracket-line", "order_external_key": "order-1", "payload": delta})
		counts.heartbeat(ctx)
		counts.send(ctx, map[string]any{"external_key": "watermark-" + strconv.Itoa(i), "kind": "watermark", "event_at": time.Now().UTC().Format(time.RFC3339Nano), "payload": map[string]any{"complete_through": stamp(windows[i+1]), "through_sequence": counts.sequence}})
		var context struct {
			ID        string `json:"id"`
			Readiness string `json:"readiness"`
		}
		r.call(r.owner, "POST", "/contexts", map[string]string{"order_id": op.orderID, "asset_id": op.lineID, "window_start": stamp(windows[i]), "window_end": stamp(windows[i+1]), "process_snapshot_id": r.snapshotID}, -1, 201, &context)
		r.must(context.Readiness == "READY", "context window %d is %s", i, context.Readiness)
		contexts = append(contexts, context.ID)
		r.record("context", "/contexts/"+context.ID, context.ID)
	}
	svc := cases.NewService(r.env.db.DB)
	_, err := svc.EvaluateCreatedContexts(ctx, r.tenantID, cases.EvaluatorActor, 100)
	r.must(err == nil, "case evaluator: %v", err)
	var caseID, caseState, opener string
	r.query(ctx, `SELECT c.id::text,c.state,t.actor_subject FROM supervisor.cases c JOIN supervisor.case_transitions t ON (t.tenant_id,t.site_id,t.case_id,t.revision)=(c.tenant_id,c.site_id,c.id,1)
 WHERE c.tenant_id=$1 AND c.site_id=$2 AND c.line_id=$3`, args(op.lineID), &caseID, &caseState, &opener)
	r.must(caseState == "detected" && opener == cases.EvaluatorActor.Subject, "case %s opened by %s", caseState, opener)
	r.record("case", "/cases/"+caseID, caseID)

	// Step 7: the case starts investigating (WIS-636) and the advisor's read-only suggestion is
	// limited until an investigation has run, then ready.
	moved, err := svc.StartDetected(ctx, r.tenantID, true, 10)
	r.must(err == nil && moved == 1, "StartDetected moved %d (%v)", moved, err)
	var advice struct {
		Readiness    string            `json:"readiness"`
		Reasons      []string          `json:"reasons"`
		Observations []json.RawMessage `json:"observations"`
		Hypotheses   []json.RawMessage `json:"hypotheses"`
	}
	r.call(r.lead, "GET", "/cases/"+caseID+"/suggestion", nil, -1, 200, &advice)
	r.must(advice.Readiness == "LIMITED" && strings.Contains(strings.Join(advice.Reasons, ","), "INVESTIGATION_NOT_RUN"), "advice before investigation: %+v", advice)
	r.record("advisor suggestion", "/cases/"+caseID+"/suggestion", caseID)
	var caseRevision int64
	r.query(ctx, `SELECT revision FROM supervisor.cases WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, args(caseID), &caseRevision)
	script, err := agent.LoadFixtureProvider("../../deploy/supervisor/agent-fixture-script.v1.json")
	r.must(err == nil, "fixture script: %v", err)
	rt := r.runtime(script)
	rt.Approvals, rt.Cases = approvals.NewService(r.env.db.DB), svc
	investigation, err := rt.Investigate(ctx, agent.InvestigateRef{TenantID: r.tenantID, SiteID: r.siteID, CaseID: caseID}, caseRevision)
	r.must(err == nil && investigation.Status == recovery.StatusProposed, "investigation: %+v (%v)", investigation, err)
	r.call(r.lead, "GET", "/cases/"+caseID+"/suggestion", nil, -1, 200, &advice)
	r.must(advice.Readiness == "READY" && len(advice.Observations) > 0 && len(advice.Hypotheses) > 0, "advice after investigation: %+v", advice)

	// Step 8: a supervisor who did not raise it approves the proposal, with fresh source heartbeats
	// (the executor re-checks them at dispatch).
	var proposalID string
	r.query(ctx, `SELECT proposal_id::text FROM supervisor.proposal_revisions WHERE tenant_id=$1 AND site_id=$2 AND case_id=$3 AND generated_by_run_id=$4`, args(caseID, investigation.RunID), &proposalID)
	r.record("proposal", "/proposals/"+proposalID, proposalID)
	var proposal struct {
		Revision  int64  `json:"revision"`
		State     string `json:"state"`
		Decisions []struct {
			Decision string `json:"decision"`
		} `json:"decisions"`
	}
	r.call(r.approver, "GET", "/proposals/"+proposalID, nil, -1, 200, &proposal)
	r.must(proposal.State == "pending", "proposal is %s", proposal.State)
	counts.heartbeat(ctx)
	machine.heartbeat(ctx)
	r.call(r.approver, "POST", "/proposals/"+proposalID+"/decisions", map[string]any{"decision": "approve", "expected_revision": proposal.Revision, "reason": "Inspect the stopped line"}, proposal.Revision, 200, &proposal)
	r.must(proposal.State == "approved" && len(proposal.Decisions) == 1, "approval: %+v", proposal)

	// Step 9: the executor creates the one task; the confirmed assignee acknowledges, starts and
	// completes it with a written report.
	_, err = svc.WithBoundEvidence(agent.DispatchEvidenceCode).ExecuteApproved(ctx, r.tenantID, true, 50)
	r.must(err == nil, "executor: %v", err)
	var taskID, creator string
	var taskCount int
	r.query(ctx, `SELECT count(*) OVER (),t.id::text,e.actor_issuer FROM supervisor.tasks t JOIN supervisor.task_events e ON (e.tenant_id,e.site_id,e.task_id,e.revision)=(t.tenant_id,t.site_id,t.id,1)
 WHERE t.tenant_id=$1 AND t.site_id=$2 AND t.case_id=$3`, args(caseID), &taskCount, &taskID, &creator)
	r.must(taskCount == 1 && creator == cases.ExecutorActor.Issuer, "tasks=%d created by %s", taskCount, creator)
	r.record("task", "/tasks/"+taskID, taskID)
	var task struct {
		State           string `json:"state"`
		Revision        int64  `json:"revision"`
		AssigneeSubject string `json:"assignee_subject"`
		Outcome         *struct {
			Status string `json:"status"`
		} `json:"outcome"`
	}
	r.call(r.operator, "GET", "/tasks/"+taskID, nil, -1, 200, &task)
	r.must(task.State == "assigned" && task.AssigneeSubject == r.operator.subject, "task %+v", task)
	for _, action := range []string{"acknowledge", "start"} {
		r.call(r.operator, "POST", "/tasks/"+taskID+"/"+action, map[string]int64{"expected_revision": task.Revision}, task.Revision, 200, &task)
	}
	report := uuid.NewString()
	r.call(r.operator, "POST", "/sources", map[string]string{"source_id": report, "kind": "manual", "title": "Inspection report", "text": "Infeed guard found open on the bracket line; reported, nothing adjusted."}, 0, 200, nil)
	r.call(r.operator, "POST", "/tasks/"+taskID+"/complete", map[string]any{"expected_revision": task.Revision, "completion_note": "Inspection done", "evidence_ids": []string{report}}, task.Revision, 200, &task)
	var completions int
	r.query(ctx, `SELECT count(*) FROM supervisor.task_completions WHERE tenant_id=$1 AND site_id=$2 AND task_id=$3`, args(taskID), &completions)
	r.must(task.State == "completed" && completions == 1, "task %s with %d completions", task.State, completions)

	// Step 10: the outcome observer runs. Recovery is judged only on two complete 15-minute windows
	// after the completion (database clock), so right after completing it is pending and nothing is
	// stored; the opt-in long run (CASE=onboarding-pilot-outcome) waits for the windows.
	observed, err := tasks.NewService(r.env.db.DB).ObserveOutcomes(ctx, r.tenantID, 10)
	r.must(err == nil && len(observed) == 1 && observed[0].Status == outcome.Pending, "observer: %+v (%v)", observed, err)
	r.call(r.operator, "GET", "/tasks/"+taskID, nil, -1, 200, &task)
	var stored int
	r.query(ctx, `SELECT count(*) FROM supervisor.outcome_observations WHERE tenant_id=$1 AND site_id=$2 AND task_id=$3`, args(taskID), &stored)
	r.must(task.Outcome != nil && task.Outcome.Status == string(outcome.Pending) && stored == 0, "outcome %+v stored=%d", task.Outcome, stored)
	r.outcome = outcomeRun{taskID: taskID, counts: counts, machine: machine, orderID: op.orderID, lineID: op.lineID}
}

// probe asks for the owner's resource as the other tenant's owner, under the owner's site and
// under the prober's own site. Each must be a 404 whose body, apart from the correlation ID,
// equals the reply for the same path with never-existing IDs: the response says nothing about
// whether the resource exists (rule 03).
func (e *pilotEnv) probe(t *testing.T, prober, owner *pilotRun, p probe) {
	t.Helper()
	unknown := p.path
	for _, id := range p.ids {
		unknown = strings.ReplaceAll(unknown, id, uuid.NewString())
	}
	for _, site := range []string{owner.siteID, prober.siteID} {
		got := e.do(prober.owner, "GET", "/v1/sites/"+site+p.path, "application/json", nil, nil)
		baseline := e.do(prober.owner, "GET", "/v1/sites/"+site+unknown, "application/json", nil, nil)
		if got.code != 404 || baseline.code != 404 {
			t.Errorf("%s probe %s under %s site: HTTP %d (unknown ID %d), want 404", p.step, p.path, siteName(site, owner), got.code, baseline.code)
			continue
		}
		if a, b := withoutCorrelation(t, got.body), withoutCorrelation(t, baseline.body); !reflect.DeepEqual(a, b) {
			t.Errorf("%s probe %s under %s site discloses existence: %v vs unknown %v", p.step, p.path, siteName(site, owner), a, b)
		}
	}
}

func siteName(site string, owner *pilotRun) string {
	if site == owner.siteID {
		return "the owning"
	}
	return "the prober's"
}

func withoutCorrelation(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("problem body %s: %v", body, err)
	}
	delete(m, "correlation_id")
	return m
}
