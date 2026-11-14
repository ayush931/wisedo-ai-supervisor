package agent

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"
)

// Budget reason codes (design §4). Provider codes live with the provider.
const (
	CodeBudgetExhausted = "BUDGET_EXHAUSTED"
	CodeQueueFull       = "QUEUE_FULL"
	CodeSiteBusy        = "SITE_BUSY"
	CodeWorkerBusy      = "WORKER_BUSY"
	CodeOverReservation = "OVER_RESERVATION"
	CodeContextOverflow = "CONTEXT_OVERFLOW"
	CodePriceMissing    = "PRICE_MISSING"
)

var (
	// ErrBudgetExhausted: a counter on the run row cannot cover the reservation; no call is made.
	ErrBudgetExhausted = errors.New(CodeBudgetExhausted)
	// ErrQueueFull: the worker already has MaxConcurrent calls in flight and MaxQueued waiting.
	// It is retryable and spends no budget.
	ErrQueueFull = errors.New(CodeQueueFull)
	// ErrSiteBusy: the site already has MaxActivePerSite investigations running (WIS-273). No run
	// is created; the case workflow waits and tries again.
	ErrSiteBusy = errors.New(CodeSiteBusy)
	// ErrWorkerBusy: this worker already runs MaxActivePerProcess investigations (WIS-273). No
	// run is created; the case workflow waits and tries again.
	ErrWorkerBusy = errors.New(CodeWorkerBusy)
	// ErrOverReservation: the provider reported more usage than was reserved; the run is stopped.
	ErrOverReservation = errors.New(CodeOverReservation)
	// ErrContextOverflow: the request cannot fit the per-request input bound. Required facts are
	// never truncated to make it fit.
	ErrContextOverflow = errors.New(CodeContextOverflow)
	// ErrPriceMissing: a billable (non-local) call has no configured price, so it is blocked.
	ErrPriceMissing  = errors.New(CodePriceMissing)
	ErrInvalidBudget = errors.New("invalid agent budget request")
)

// Limits are the per-cycle engineering guardrails from WIS-275. They are not measured
// customer economics.
type Limits struct {
	Invocations     int   // specialist invocations per cycle
	Depth           int   // delegation depth; children cannot delegate
	ReadBranches    int   // independent read branches
	ToolCalls       int   // total tool calls
	Requests        int   // provider requests, including retries
	Tokens          int64 // cumulative input+output tokens
	CostMicroUSD    int64 // cost ceiling; USD 1.00 = 1,000,000
	MaxOutputTokens int   // per request
	MaxInputTokens  int   // per request (measured when available)
	WallTime        time.Duration
	CallTimeout     time.Duration
	MaxConcurrent   int // in-flight provider calls per worker
	MaxQueued       int // callers waiting for a slot per worker
	// MaxActivePerSite and MaxActivePerProcess bound running case investigations (WIS-273: 2 per
	// site, 10 per process). The site bound is durable; the process bound is per worker.
	MaxActivePerSite    int
	MaxActivePerProcess int
}

// DefaultLimits returns the issue's defaults plus the design's wall-time, concurrency and queue bounds.
func DefaultLimits() Limits {
	return Limits{
		Invocations: 3, Depth: 1, ReadBranches: 2, ToolCalls: 12, Requests: 6,
		Tokens: 24000, CostMicroUSD: 1_000_000, MaxOutputTokens: 2000, MaxInputTokens: 8000,
		WallTime: 4 * time.Minute, CallTimeout: 30 * time.Second, MaxConcurrent: 2, MaxQueued: 8,
		MaxActivePerSite: 2, MaxActivePerProcess: 10,
	}
}

// InterviewLimits keeps onboarding to one discovery hop and a local zero-cost ceiling. A run
// pinned to the hosted route gets a priced ceiling instead, when it is created (CreateRunTx).
func InterviewLimits() Limits {
	l := DefaultLimits()
	l.Invocations, l.CostMicroUSD = 1, 0
	return l
}

// conversationLimits are the limits of an interview turn, chat answer or configuration draft run:
// InterviewLimits, with the per-call deadline the runtime was configured with
// (SUPERVISOR_AGENT_TIMEOUT, set by the worker as for investigations). These runs used a fixed 30 s
// whatever was configured, so a slower local model could never finish a call (WIS-631).
func (r *Runtime) conversationLimits() Limits {
	l := InterviewLimits()
	if r.Limits.CallTimeout > 0 {
		l.CallTimeout = r.Limits.CallTimeout
	}
	return l
}

func (l Limits) Validate() error {
	if l.Invocations < 1 || l.Depth < 0 || l.Depth > 1 || l.ReadBranches < 1 || l.ToolCalls < 0 || l.Requests < 1 ||
		l.Tokens < 1 || l.CostMicroUSD < 0 || l.MaxOutputTokens < 1 || l.MaxInputTokens < 1 ||
		int64(l.MaxOutputTokens) > l.Tokens || l.WallTime <= 0 || l.CallTimeout <= 0 || l.MaxConcurrent < 1 || l.MaxQueued < 0 ||
		l.MaxActivePerSite < 1 || l.MaxActivePerProcess < 1 {
		return ErrInvalidBudget
	}
	return nil
}

// CycleDeadline is the envelope deadline: the earlier of start+WallTime and the activity
// deadline less a 15 s margin for recording the outcome. Times are UTC.
func (l Limits) CycleDeadline(start, activityDeadline time.Time) time.Time {
	d := start.Add(l.WallTime)
	if !activityDeadline.IsZero() {
		if a := activityDeadline.Add(-15 * time.Second); a.Before(d) {
			d = a
		}
	}
	return d.UTC()
}

// EstimateInputTokens is ceil(bytes/3): deliberately pessimistic for a local tokenizer.
func EstimateInputTokens(requestBytes int) int64 {
	if requestBytes <= 0 {
		return 0
	}
	return (int64(requestBytes) + 2) / 3
}

// TokenPrice is a versioned per-million-token price in integer micro-USD.
type TokenPrice struct {
	InputMicroUSDPerMillion  int64
	OutputMicroUSDPerMillion int64
}

// Pricing says how a call is costed. A local call costs 0 but every other limit still applies;
// any other call needs an explicit price.
type Pricing struct {
	Local bool
	Price *TokenPrice
}

// maxPrice bounds price*tokens well inside int64 (tokens are bounded by Limits.Tokens).
const maxPrice, maxTokens = int64(1_000_000_000_000), int64(1_000_000)

// CallCost is the integer micro-USD cost of a call, rounded up. A billable call without a
// price is ErrPriceMissing.
func CallCost(p Pricing, inputTokens, outputTokens int64) (int64, error) {
	if inputTokens < 0 || outputTokens < 0 || inputTokens > maxTokens || outputTokens > maxTokens {
		return 0, ErrInvalidBudget
	}
	if p.Local {
		return 0, nil
	}
	if p.Price == nil {
		return 0, ErrPriceMissing
	}
	in, out := p.Price.InputMicroUSDPerMillion, p.Price.OutputMicroUSDPerMillion
	if in < 0 || out < 0 || in > maxPrice || out > maxPrice {
		return 0, ErrInvalidBudget
	}
	return (inputTokens*in + outputTokens*out + 999_999) / 1_000_000, nil
}

func (p Pricing) basis() string {
	if p.Local {
		return "local"
	}
	return "priced"
}

// RequestPlan describes one provider request before it is dispatched.
type RequestPlan struct {
	Seq, Attempt   int
	SpanID         string
	ParentSpanID   string
	ProviderActual bool
	Model          string
	ModelRevision  string
	RequestSHA256  string
	RequestBytes   int
	// ImageTokens is the estimated input of the request's page images (WIS-625); RequestBytes then
	// excludes their encoded bytes, which the bytes/3 estimate would overstate a hundredfold.
	ImageTokens int64
	// InputTokens is an endpoint count of the complete templated text; zero keeps bytes/3.
	InputTokens        int64
	LocalContextTokens int64 // latest successful local /props probe; zero is unknown
	Pricing            Pricing
}

// Reservation is what ReserveRequest held on the run row.
type Reservation struct {
	RunID          string
	Seq, Attempt   int
	Tokens         int64
	MicroUSD       int64
	Pricing        Pricing
	RequestsLeft   int
	TokensLeft     int64
	CostLeftMicros int64
}

// reserveSQL decrements the run's counters and records the reserved call in one statement.
// The row lock and the WHERE re-check under READ COMMITTED make parallel reservations on one
// run serialize; one that cannot be covered updates nothing and inserts nothing.
const reserveSQL = `WITH run AS (
 UPDATE supervisor.agent_runs SET requests_left=requests_left-1,tokens_left=tokens_left-$4,cost_left_microusd=cost_left_microusd-$5
 WHERE tenant_id=$1 AND site_id=$2 AND id=$3 AND state='running' AND requests_left>=1 AND tokens_left>=$4 AND cost_left_microusd>=$5
 RETURNING tenant_id,site_id,id,requests_left,tokens_left,cost_left_microusd),
call AS (
 INSERT INTO supervisor.agent_model_calls(tenant_id,site_id,run_id,seq,attempt,span_id,parent_span_id,provider_actual,model,model_revision,request_sha256,reserved_tokens,reserved_microusd,cost_basis)
 SELECT tenant_id,site_id,id,$6,$7,$8,$9,$10,$11,$12,$13,$4,$5,$14 FROM run RETURNING 1)
SELECT requests_left,tokens_left,cost_left_microusd FROM run,call`

// ReserveRequest reserves one provider request, its estimated input plus MaxOutputTokens, and
// its maximum cost, in its own committed transaction. The caller dispatches only after it
// returns nil; a crash after this point leaves the reservation held, which fails closed.
func ReserveRequest(ctx context.Context, s *Store, sc RunScope, runID string, l Limits, p RequestPlan) (Reservation, error) {
	return reserveRequest(ctx, s, sc, runID, l, p, nil)
}

func reserveRequest(ctx context.Context, s *Store, sc RunScope, runID string, l Limits, p RequestPlan, before func(*sql.Tx) error) (Reservation, error) {
	var r Reservation
	if !validRunScope(sc) || !isUUID(runID) || p.Seq <= 0 || p.Attempt < 1 || p.Attempt > 2 || !validSpanID(p.SpanID) ||
		(p.ParentSpanID != "" && !validSpanID(p.ParentSpanID)) || !hex64.MatchString(p.RequestSHA256) || p.Model == "" || p.ModelRevision == "" || l.Validate() != nil {
		return r, ErrInvalidBudget
	}
	if p.ImageTokens < 0 || p.InputTokens < 0 || (p.InputTokens > 0 && p.ImageTokens > 0) {
		return r, ErrInvalidBudget
	}
	input := EstimateInputTokens(p.RequestBytes) + p.ImageTokens
	if p.InputTokens > 0 {
		input = p.InputTokens
	}
	tokens := input + int64(l.MaxOutputTokens)
	// The price check precedes any SQL: a blocked call leaves no row and no counter change.
	cost, err := CallCost(p.Pricing, input, int64(l.MaxOutputTokens))
	if err != nil {
		return r, err
	}
	r = Reservation{RunID: runID, Seq: p.Seq, Attempt: p.Attempt, Tokens: tokens, MicroUSD: cost, Pricing: p.Pricing}
	err = s.InTx(ctx, sc, func(tx *sql.Tx) error {
		run, e := GetRunTx(ctx, tx, sc, runID)
		if e != nil {
			return e
		}
		effective, e := limitsForRun(run, l, p)
		if e != nil {
			return e
		}
		if input > int64(effective.MaxInputTokens) {
			return ErrContextOverflow
		}
		if before != nil {
			if e := before(tx); e != nil {
				return e
			}
		}
		e = tx.QueryRowContext(ctx, reserveSQL, sc.TenantID, sc.SiteID, runID, tokens, cost, p.Seq, p.Attempt, p.SpanID, nullString(p.ParentSpanID),
			p.ProviderActual, p.Model, p.ModelRevision, p.RequestSHA256, p.Pricing.basis()).Scan(&r.RequestsLeft, &r.TokensLeft, &r.CostLeftMicros)
		if errors.Is(e, sql.ErrNoRows) {
			return runUnavailableOr(ctx, tx, sc, runID, ErrBudgetExhausted)
		}
		return e
	})
	if err != nil {
		return Reservation{}, err
	}
	return r, nil
}

// runUnavailableOr distinguishes a missing or terminal run from an exhausted budget.
func runUnavailableOr(ctx context.Context, tx *sql.Tx, sc RunScope, runID string, exhausted error) error {
	var running bool
	err := tx.QueryRowContext(ctx, `SELECT state='running' FROM supervisor.agent_runs WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, runID).Scan(&running)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !running) {
		return ErrRunNotFound
	}
	if err != nil {
		return err
	}
	return exhausted
}

// ReserveToolCallTx takes one tool call from the run inside the caller's transaction.
func ReserveToolCallTx(ctx context.Context, tx *sql.Tx, sc RunScope, runID string) error {
	return decrementTx(ctx, tx, sc, runID, `UPDATE supervisor.agent_runs SET tool_calls_left=tool_calls_left-1 WHERE tenant_id=$1 AND site_id=$2 AND id=$3 AND state='running' AND tool_calls_left>=1`)
}

// ReserveInvocationTx takes one specialist invocation from the run inside the caller's transaction.
func ReserveInvocationTx(ctx context.Context, tx *sql.Tx, sc RunScope, runID string) error {
	return decrementTx(ctx, tx, sc, runID, `UPDATE supervisor.agent_runs SET invocations_left=invocations_left-1 WHERE tenant_id=$1 AND site_id=$2 AND id=$3 AND state='running' AND invocations_left>=1`)
}

func decrementTx(ctx context.Context, tx *sql.Tx, sc RunScope, runID, q string) error {
	if !validRunScope(sc) || !isUUID(runID) {
		return ErrInvalidBudget
	}
	res, err := tx.ExecContext(ctx, q, sc.TenantID, sc.SiteID, runID)
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil || n == 1 {
		return e
	}
	return runUnavailableOr(ctx, tx, sc, runID, ErrBudgetExhausted)
}

// Usage is what the provider reported, or what is known about a failed call.
type Usage struct {
	Status         string // completed | failed | discarded
	InputTokens    int64
	OutputTokens   int64
	WallMillis     int64
	ResponseSHA256 string
	Class          string // lowercase provider error class, optional
	HTTPStatus     int    // optional
	ReasonCode     string // optional
}

// Charge settles a reservation in its own transaction: it records actual usage and refunds the
// unused tokens and cost to the run. Usage above the reservation is charged in full (the run
// counter floors at zero, the call row keeps the actual numbers), the run ends failed with
// OVER_RESERVATION, and ErrOverReservation is returned. A run that has already ended keeps its
// terminal state; the call is still settled.
func Charge(ctx context.Context, s *Store, sc RunScope, r Reservation, u Usage) (charged int64, err error) {
	switch u.Status {
	case "completed", "failed", "discarded":
	default:
		return 0, ErrInvalidBudget
	}
	if !validRunScope(sc) || !isUUID(r.RunID) || u.WallMillis < 0 || (u.ResponseSHA256 != "" && !hex64.MatchString(u.ResponseSHA256)) ||
		(u.ReasonCode != "" && !reasonPat.MatchString(u.ReasonCode)) || (u.HTTPStatus != 0 && (u.HTTPStatus < 100 || u.HTTPStatus > 599)) {
		return 0, ErrInvalidBudget
	}
	charged, err = CallCost(r.Pricing, u.InputTokens, u.OutputTokens)
	if err != nil {
		return 0, err
	}
	used := u.InputTokens + u.OutputTokens
	over := used > r.Tokens || charged > r.MicroUSD
	reason := u.ReasonCode
	if over {
		reason = CodeOverReservation
	}
	var httpStatus any
	if u.HTTPStatus != 0 {
		httpStatus = u.HTTPStatus
	}
	err = s.InTx(ctx, sc, func(tx *sql.Tx) error {
		res, e := tx.ExecContext(ctx, `UPDATE supervisor.agent_model_calls SET status=$6,input_tokens=$7,output_tokens=$8,wall_ms=$9,charged_microusd=$10,response_sha256=$11,class=$12,http_status=$13,reason_code=$14,ended_at=clock_timestamp()
 WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3 AND seq=$4 AND attempt=$5 AND status='reserved' AND reserved_tokens=$15 AND reserved_microusd=$16`,
			sc.TenantID, sc.SiteID, r.RunID, r.Seq, r.Attempt, u.Status, u.InputTokens, u.OutputTokens, u.WallMillis, charged,
			nullString(u.ResponseSHA256), nullString(u.Class), httpStatus, nullString(reason), r.Tokens, r.MicroUSD)
		if e = oneRow(res, e); e != nil {
			return e
		}
		// Refund (or, when over, charge the excess) against the reservation already taken.
		_, e = tx.ExecContext(ctx, `UPDATE supervisor.agent_runs SET tokens_left=GREATEST(tokens_left+$4,0),cost_left_microusd=GREATEST(cost_left_microusd+$5,0)
 WHERE tenant_id=$1 AND site_id=$2 AND id=$3 AND state='running'`, sc.TenantID, sc.SiteID, r.RunID, r.Tokens-used, r.MicroUSD-charged)
		if e != nil || !over {
			return e
		}
		_, e = tx.ExecContext(ctx, `UPDATE supervisor.agent_runs SET state='failed',outcome_status='escalated',outcome_reasons=ARRAY[$4::text],ended_at=clock_timestamp()
 WHERE tenant_id=$1 AND site_id=$2 AND id=$3 AND state='running'`, sc.TenantID, sc.SiteID, r.RunID, CodeOverReservation)
		return e
	})
	if err != nil {
		return 0, err
	}
	if over {
		return charged, ErrOverReservation
	}
	return charged, nil
}

// Slots bounds in-flight provider calls per worker and the number of callers waiting for one.
// ponytail: per-process bound (design §10.7); make it global only if workers scale out.
type Slots struct {
	sem       chan struct{}
	mu        sync.Mutex
	waiting   int
	maxQueued int
}

func NewSlots(maxConcurrent, maxQueued int) *Slots {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	if maxQueued < 0 {
		maxQueued = 0
	}
	return &Slots{sem: make(chan struct{}, maxConcurrent), maxQueued: maxQueued}
}

// Acquire takes a slot, waiting in the bounded queue if all are busy. A full queue returns
// ErrQueueFull at once; a canceled context returns its error. release must be called once.
func (s *Slots) Acquire(ctx context.Context) (release func(), err error) {
	release = func() { <-s.sem }
	select {
	case s.sem <- struct{}{}:
		return release, nil
	default:
	}
	s.mu.Lock()
	if s.waiting >= s.maxQueued {
		s.mu.Unlock()
		return nil, ErrQueueFull
	}
	s.waiting++
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.waiting--; s.mu.Unlock() }()
	select {
	case s.sem <- struct{}{}:
		return release, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Waiting reports the number of queued callers.
func (s *Slots) Waiting() int { s.mu.Lock(); defer s.mu.Unlock(); return s.waiting }
