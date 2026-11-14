//go:build integration

package agent

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
)

type budgetDB struct {
	admin *sql.DB
	store *Store
}

// openBudgetDB migrates the disposable database and returns the superuser connection (for
// seeding and read-back only) and a Store on the restricted runtime role.
func openBudgetDB(t *testing.T) budgetDB {
	t.Helper()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL admin/runtime URLs required")
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	runtime, err := sql.Open("pgx", runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if err = migrate.Apply(context.Background(), admin); err != nil {
		t.Fatal(err)
	}
	return budgetDB{admin: admin, store: NewStore(runtime)}
}

func (d budgetDB) site(t *testing.T) RunScope {
	t.Helper()
	sc := RunScope{uuid.NewString(), uuid.NewString()}
	for _, q := range []string{`INSERT INTO supervisor.tenants(id,name) VALUES($1,'agent budget test')`, `INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'agent budget test')`} {
		args := []any{sc.TenantID}
		if strings.Contains(q, "sites") {
			args = append(args, sc.SiteID)
		}
		if _, err := d.admin.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	return sc
}

// conversationRun creates a running conversation run (no case needed) with the given limits.
func (d budgetDB) conversationRun(t *testing.T, sc RunScope, l Limits) string {
	t.Helper()
	id := uuid.NewString()
	r := Run{ID: id, RequestKind: RequestConversation, ConversationID: uuid.NewString(), RequestID: uuid.NewString(), TraceID: NewTraceID(),
		ProviderName: "deterministic-fixture", Model: "fixture-model", ModelRevision: "sha256:fixture", PromptVersion: "p1", ToolVersion: "t1", PolicyRevision: 1}
	if err := d.store.InTx(context.Background(), sc, func(tx *sql.Tx) error { return CreateRunTx(context.Background(), tx, sc, r, l) }); err != nil {
		t.Fatal(err)
	}
	return id
}

type counters struct {
	requests, toolCalls, invocations int
	tokens, cost                     int64
	state                            string
	reasons                          string
	calls                            int
}

func (d budgetDB) read(t *testing.T, sc RunScope, run string) counters {
	t.Helper()
	var c counters
	if err := d.admin.QueryRow(`SELECT requests_left,tool_calls_left,invocations_left,tokens_left,cost_left_microusd,state,array_to_string(outcome_reasons,','),
 (SELECT count(*) FROM supervisor.agent_model_calls m WHERE (m.tenant_id,m.site_id,m.run_id)=(r.tenant_id,r.site_id,r.id))
 FROM supervisor.agent_runs r WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, run).Scan(&c.requests, &c.toolCalls, &c.invocations, &c.tokens, &c.cost, &c.state, &c.reasons, &c.calls); err != nil {
		t.Fatal(err)
	}
	return c
}

func plan(seq int, p Pricing) RequestPlan {
	return RequestPlan{Seq: seq, Attempt: 1, SpanID: NewSpanID(), ProviderActual: false, Model: "fixture-model", ModelRevision: "sha256:fixture",
		RequestSHA256: strings.Repeat("ab", 32), RequestBytes: 3000, Pricing: p}
}

var local = Pricing{Local: true}

func TestWIS702EndpointCountAndFallbackKeepContextLimit(t *testing.T) {
	d := openBudgetDB(t)
	sc := d.site(t)
	for _, tc := range []struct {
		name  string
		bytes int
		count int64
		want  error
	}{
		{"endpoint fits where bytes overflow", 30000, 7000, nil},
		{"endpoint refuses underestimated JSON", 3000, 8001, ErrContextOverflow},
		{"missing tokenizer fails closed", 30000, 0, ErrContextOverflow},
		{"invalid count", 3000, -1, ErrInvalidBudget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := d.conversationRun(t, sc, DefaultLimits())
			p := plan(1, local)
			p.RequestBytes, p.InputTokens = tc.bytes, tc.count
			reservation, err := ReserveRequest(t.Context(), d.store, sc, run, DefaultLimits(), p)
			if !errors.Is(err, tc.want) {
				t.Fatalf("reservation error %v, want %v", err, tc.want)
			}
			got := d.read(t, sc, run)
			if tc.want != nil && (got.calls != 0 || got.requests != 6 || got.tokens != 24000) {
				t.Fatalf("refused input spent budget: %+v", got)
			}
			if tc.want == nil && (reservation.Tokens != 9000 || got.tokens != 15000 || got.calls != 1) {
				t.Fatalf("endpoint count not reserved: %+v %+v", reservation, got)
			}
		})
	}
}

// need is what plan() reserves under DefaultLimits: ceil(3000/3) input + 2000 output.
const need = int64(1000 + 2000)

func TestRunRowRLS(t *testing.T) {
	d := openBudgetDB(t)
	ctx := context.Background()
	a, b := d.site(t), d.site(t)
	run := d.conversationRun(t, a, DefaultLimits())
	var count int
	for _, c := range []struct {
		name  string
		scope RunScope
		want  int
	}{{"own tenant", a, 1}, {"other tenant", b, 0}} {
		err := d.store.InTx(ctx, c.scope, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.agent_runs WHERE id=$1`, run).Scan(&count)
		})
		if err != nil || count != c.want {
			t.Fatalf("%s: runtime role saw %d agent_runs rows (%v), want %d", c.name, count, err, c.want)
		}
	}
	// Naming tenant A's scope while the transaction carries tenant B's context reads nothing.
	err := d.store.InTx(ctx, b, func(tx *sql.Tx) error { _, e := GetRunTx(ctx, tx, a, run); return e })
	if !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("cross-tenant GetRunTx = %v, want ErrRunNotFound", err)
	}
	var bypass bool
	if err = d.store.db.QueryRow(`SELECT rolbypassrls OR rolsuper FROM pg_roles WHERE rolname=current_user`).Scan(&bypass); err != nil || bypass {
		t.Fatalf("runtime role bypasses RLS: %v %v", bypass, err)
	}
	// Defaults are what the run row starts with.
	got := d.read(t, a, run)
	if got.invocations != 3 || got.toolCalls != 12 || got.requests != 6 || got.tokens != 24000 || got.cost != 1_000_000 || got.state != RunRunning {
		t.Fatalf("run started with %+v, want the WIS-275 defaults", got)
	}
}

// TestSharedBudgetRace: two reservations race on one run that can cover only one of them.
func TestSharedBudgetRace(t *testing.T) {
	d := openBudgetDB(t)
	ctx := context.Background()
	sc := d.site(t)
	for _, c := range []struct {
		name    string
		tokens  int64
		wantOK  int
		wantErr int
	}{
		{"tokens_left below 2x need: exactly one wins", 2*need - 1, 1, 1},
		{"control: tokens_left covers both", 2 * need, 2, 0},
	} {
		for i := 0; i < 5; i++ {
			l := DefaultLimits()
			l.Tokens = c.tokens
			run := d.conversationRun(t, sc, l)
			start := make(chan struct{})
			errs := make([]error, 2)
			var wg sync.WaitGroup
			for g := 0; g < 2; g++ {
				wg.Add(1)
				go func(g int) {
					defer wg.Done()
					<-start
					_, errs[g] = ReserveRequest(ctx, d.store, sc, run, l, plan(g+1, local))
				}(g)
			}
			close(start)
			wg.Wait()
			ok, exhausted := 0, 0
			for _, e := range errs {
				switch {
				case e == nil:
					ok++
				case errors.Is(e, ErrBudgetExhausted):
					exhausted++
				default:
					t.Fatalf("%s: unexpected error %v", c.name, e)
				}
			}
			got := d.read(t, sc, run)
			if ok != c.wantOK || exhausted != c.wantErr || got.tokens != c.tokens-int64(ok)*need || got.requests != 6-ok || got.calls != ok {
				t.Fatalf("%s: ok=%d exhausted=%d run=%+v", c.name, ok, exhausted, got)
			}
		}
	}
}

func TestChargeAfterCall(t *testing.T) {
	d := openBudgetDB(t)
	ctx := context.Background()
	sc := d.site(t)
	price := &TokenPrice{InputMicroUSDPerMillion: 3_000_000, OutputMicroUSDPerMillion: 15_000_000} // $3 / $15 per million
	for _, c := range []struct {
		name        string
		pricing     Pricing
		in, out     int64
		wantCharged int64
		wantErr     error
		wantState   string
	}{
		// A local call costs nothing, but tokens and requests are still spent.
		{"local", local, 700, 300, 0, nil, RunRunning},
		// ceil((700*3e6 + 300*15e6)/1e6) = 2100 + 4500 = 6600.
		{"priced", Pricing{Price: price}, 700, 300, 6600, nil, RunRunning},
		// More than reserved: charged in full and the run stops.
		{"over reservation", local, 2500, 1000, 0, ErrOverReservation, RunFailed},
	} {
		run := d.conversationRun(t, sc, DefaultLimits())
		res, err := ReserveRequest(ctx, d.store, sc, run, DefaultLimits(), plan(1, c.pricing))
		if err != nil {
			t.Fatalf("%s: reserve %v", c.name, err)
		}
		mid := d.read(t, sc, run)
		if mid.tokens != 24000-need || mid.cost != 1_000_000-res.MicroUSD || mid.requests != 5 {
			t.Fatalf("%s: reservation not held before dispatch: %+v", c.name, mid)
		}
		charged, err := Charge(ctx, d.store, sc, res, Usage{Status: "completed", InputTokens: c.in, OutputTokens: c.out, WallMillis: 42, ResponseSHA256: strings.Repeat("cd", 32)})
		if !errors.Is(err, c.wantErr) || charged != c.wantCharged {
			t.Fatalf("%s: charge=%d %v, want %d %v", c.name, charged, err, c.wantCharged, c.wantErr)
		}
		got := d.read(t, sc, run)
		wantTokens := 24000 - c.in - c.out // also when over: the excess is charged against the counter
		if got.tokens != wantTokens || got.cost != 1_000_000-c.wantCharged || got.requests != 5 || got.state != c.wantState {
			t.Fatalf("%s: after charge %+v, want tokens=%d cost=%d state=%s", c.name, got, wantTokens, 1_000_000-c.wantCharged, c.wantState)
		}
		var status, basis string
		var in, out, ms, micro int64
		if err = d.admin.QueryRow(`SELECT status,cost_basis,input_tokens,output_tokens,wall_ms,charged_microusd FROM supervisor.agent_model_calls WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3`, sc.TenantID, sc.SiteID, run).Scan(&status, &basis, &in, &out, &ms, &micro); err != nil {
			t.Fatal(err)
		}
		if status != "completed" || in != c.in || out != c.out || ms != 42 || micro != c.wantCharged || (basis == "local") != c.pricing.Local {
			t.Fatalf("%s: call row %s %s %d %d %d %d", c.name, status, basis, in, out, ms, micro)
		}
		if c.wantErr != nil && got.reasons != CodeOverReservation {
			t.Fatalf("%s: run reasons %q", c.name, got.reasons)
		}
		// A settled call cannot be charged twice.
		if _, err = Charge(ctx, d.store, sc, res, Usage{Status: "completed"}); !errors.Is(err, ErrRunNotFound) {
			t.Fatalf("%s: second charge = %v", c.name, err)
		}
	}
}

func TestBudgetGuards(t *testing.T) {
	d := openBudgetDB(t)
	ctx := context.Background()
	sc := d.site(t)

	t.Run("missing price blocks a billable call", func(t *testing.T) {
		run := d.conversationRun(t, sc, DefaultLimits())
		before := d.read(t, sc, run)
		if _, err := ReserveRequest(ctx, d.store, sc, run, DefaultLimits(), plan(1, Pricing{})); !errors.Is(err, ErrPriceMissing) {
			t.Fatalf("unpriced billable call = %v, want ErrPriceMissing", err)
		}
		if after := d.read(t, sc, run); after != before || after.calls != 0 {
			t.Fatalf("blocked call changed the run: %+v -> %+v", before, after)
		}
		// Control: the same call priced (or local) is reserved.
		if _, err := ReserveRequest(ctx, d.store, sc, run, DefaultLimits(), plan(1, Pricing{Price: &TokenPrice{1, 1}})); err != nil {
			t.Fatalf("priced call: %v", err)
		}
	})

	t.Run("request ceiling includes retries", func(t *testing.T) {
		l := DefaultLimits()
		l.Requests = 2
		run := d.conversationRun(t, sc, l)
		for _, p := range []RequestPlan{plan(1, local), func() RequestPlan { p := plan(1, local); p.Attempt = 2; return p }()} {
			if _, err := ReserveRequest(ctx, d.store, sc, run, l, p); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := ReserveRequest(ctx, d.store, sc, run, l, plan(2, local)); !errors.Is(err, ErrBudgetExhausted) {
			t.Fatalf("third request = %v, want ErrBudgetExhausted", err)
		}
	})

	t.Run("cost ceiling", func(t *testing.T) {
		l := DefaultLimits()
		l.CostMicroUSD = 10
		run := d.conversationRun(t, sc, l)
		// 1000 input at $3/M + 2000 output at $15/M = 3000+30000 micro-USD > 10.
		if _, err := ReserveRequest(ctx, d.store, sc, run, l, plan(1, Pricing{Price: &TokenPrice{3_000_000, 15_000_000}})); !errors.Is(err, ErrBudgetExhausted) {
			t.Fatalf("over-cost request = %v", err)
		}
		if _, err := ReserveRequest(ctx, d.store, sc, run, l, plan(1, local)); err != nil {
			t.Fatalf("local call under a cost ceiling: %v", err)
		}
	})

	t.Run("context overflow is never truncated", func(t *testing.T) {
		run := d.conversationRun(t, sc, DefaultLimits())
		p := plan(1, local)
		p.RequestBytes = 3*8000 + 1
		if _, err := ReserveRequest(ctx, d.store, sc, run, DefaultLimits(), p); !errors.Is(err, ErrContextOverflow) {
			t.Fatalf("oversized request = %v", err)
		}
		if got := d.read(t, sc, run); got.calls != 0 || got.requests != 6 {
			t.Fatalf("overflow spent budget: %+v", got)
		}
	})

	t.Run("tool calls and invocations", func(t *testing.T) {
		l := DefaultLimits()
		l.ToolCalls, l.Invocations = 1, 1
		run := d.conversationRun(t, sc, l)
		for _, reserve := range []func(context.Context, *sql.Tx, RunScope, string) error{ReserveToolCallTx, ReserveInvocationTx} {
			if err := d.store.InTx(ctx, sc, func(tx *sql.Tx) error { return reserve(ctx, tx, sc, run) }); err != nil {
				t.Fatal(err)
			}
			if err := d.store.InTx(ctx, sc, func(tx *sql.Tx) error { return reserve(ctx, tx, sc, run) }); !errors.Is(err, ErrBudgetExhausted) {
				t.Fatalf("second reservation = %v", err)
			}
		}
	})

	t.Run("terminal run spends nothing", func(t *testing.T) {
		run := d.conversationRun(t, sc, DefaultLimits())
		finish := func() error {
			return d.store.InTx(ctx, sc, func(tx *sql.Tx) error {
				return FinishRunTx(ctx, tx, sc, run, Finish{State: RunDegraded, OutcomeStatus: "escalated", OutcomeReasons: []string{"PROVIDER_FAILED"}})
			})
		}
		if err := finish(); err != nil {
			t.Fatal(err)
		}
		if err := finish(); !errors.Is(err, ErrRunNotFound) {
			t.Fatalf("second finish = %v", err)
		}
		if _, err := ReserveRequest(ctx, d.store, sc, run, DefaultLimits(), plan(1, local)); !errors.Is(err, ErrRunNotFound) {
			t.Fatalf("reserve on terminal run = %v", err)
		}
		var ended time.Time
		if err := d.admin.QueryRow(`SELECT ended_at FROM supervisor.agent_runs WHERE tenant_id=$1 AND id=$2`, sc.TenantID, run).Scan(&ended); err != nil || ended.IsZero() {
			t.Fatalf("ended_at %v %v", ended, err)
		}
	})
}

// TestStoreRecordsTrace writes one of each trace row and reads the captured scope back.
func TestStoreRecordsTrace(t *testing.T) {
	d := openBudgetDB(t)
	ctx := context.Background()
	sc := d.site(t)
	run := d.conversationRun(t, sc, DefaultLimits())
	now := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	root, child := NewSpanID(), NewSpanID()
	evidenceID := uuid.NewString()
	artifactBody := []byte(`{"proposal_id":"` + uuid.NewString() + `","case_id":"` + uuid.NewString() + `","case_revision":2,"selected_option":"clarify","rationale":"Needs a reason."}`)
	var digest string
	err := d.store.InTx(ctx, sc, func(tx *sql.Tx) error {
		if e := AddRefsTx(ctx, tx, sc, run, []RunRef{{Kind: RefEvidence, RefID: evidenceID, Revision: 2, SHA256: strings.Repeat("1", 64), AccessEpoch: 1}, {Kind: RefMeasurement, RefID: "deficit_good_units", Revision: 1, SHA256: strings.Repeat("2", 64)}}); e != nil {
			return e
		}
		if e := AddStepTx(ctx, tx, sc, run, Step{Seq: 1, SpanID: root, Role: RoleOrchestrator, Hop: 0, EnvelopeSHA256: strings.Repeat("3", 64), Status: "completed", StartedAt: now, EndedAt: now.Add(time.Second)}); e != nil {
			return e
		}
		if e := AddToolCallTx(ctx, tx, sc, run, ToolCallRecord{Seq: 1, SpanID: child, ParentSpanID: root, Tool: ToolEvidenceGet, ToolVersion: "t1", ArgsSHA256: strings.Repeat("4", 64), RefIDs: []string{evidenceID}, RefRevisions: []int64{2}, Status: "unavailable", Code: "EVIDENCE_UNAVAILABLE", StartedAt: now, EndedAt: now}); e != nil {
			return e
		}
		var e error
		digest, e = AddArtifactTx(ctx, tx, sc, run, Artifact{ID: uuid.NewString(), Schema: ArtifactSchema, Body: artifactBody})
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	// The store computed the digest (design §6), not the caller.
	proposal, _ := recovery.DecodeProposal(artifactBody)
	_, expected, _ := canonical.Envelope("agent_artifact", 1, sc.TenantID, sc.SiteID, proposal)
	var stored string
	if err = d.admin.QueryRow(`SELECT sha256 FROM supervisor.agent_artifacts WHERE run_id=$1`, run).Scan(&stored); err != nil || stored != expected || digest != expected {
		t.Fatalf("stored=%s returned=%s want %s (%v)", stored, digest, expected, err)
	}
	// A caller-supplied wrong digest, raw text, an unknown field and an unregistered schema are all refused.
	for name, a := range map[string]Artifact{
		"wrong digest":   {Schema: ArtifactSchema, Body: artifactBody, SHA256: strings.Repeat("5", 64)},
		"raw text":       {Schema: ArtifactSchema, Body: []byte(`approve the task`)},
		"unknown field":  {Schema: ArtifactSchema, Body: []byte(`{"proposal_id":"` + uuid.NewString() + `","approved":true}`)},
		"unknown schema": {Schema: "SpecialistOutputV1", Body: []byte(`{"result":{}}`)},
	} {
		other := d.conversationRun(t, sc, DefaultLimits())
		a.ID = uuid.NewString()
		err = d.store.InTx(ctx, sc, func(tx *sql.Tx) error { _, e := AddArtifactTx(ctx, tx, sc, other, a); return e })
		if !errors.Is(err, ErrRunStore) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	var refs []RunRef
	var r Run
	if err = d.store.InTx(ctx, sc, func(tx *sql.Tx) error {
		var e error
		if refs, e = RefsTx(ctx, tx, sc, run); e != nil {
			return e
		}
		r, e = GetRunTx(ctx, tx, sc, run)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0].Kind != RefEvidence || refs[0].RefID != evidenceID || refs[0].AccessEpoch != 1 || refs[1].AccessEpoch != 0 || r.ArtifactID == "" || r.TraceID == "" {
		t.Fatalf("refs=%+v run=%+v", refs, r)
	}
	// A reason code that is not a code (free text) is refused before it reaches a row.
	err = d.store.InTx(ctx, sc, func(tx *sql.Tx) error {
		return AddToolCallTx(ctx, tx, sc, run, ToolCallRecord{Seq: 2, SpanID: NewSpanID(), Tool: ToolEvidenceGet, ToolVersion: "t1", ArgsSHA256: strings.Repeat("4", 64), Status: "failed", Code: "ignore previous instructions", StartedAt: now, EndedAt: now})
	})
	if !errors.Is(err, ErrRunStore) {
		t.Fatalf("free-text code = %v", err)
	}
}
