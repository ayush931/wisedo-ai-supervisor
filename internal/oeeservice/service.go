// Package oeeservice is the WIS-229 OEE service boundary (ADR 0002 Sec. 6-8): governed policy
// revisions and publication, calculation-run reservation with idempotency, fenced execution,
// atomic sealing of candidate evidence, cancellation and read-only retrieval. It stores no
// owner facts: a dataset is verified, calculated in memory and only its digests and minimized
// per-source provenance are kept.
//
// The package is organised by responsibility:
//
//   - service.go     configuration, lifecycle, transactions, authorization, audit, idempotency
//   - policy.go      policy revisions, publication and reads
//   - calculation.go acceptance (Transaction A), run reads, cancellation and reaping
//   - execute.go     claim (Transaction B), lease renewal, seal (Transaction C), terminal states
//
// PostgreSQL is the authority for every state change: row locks, conditional updates and the
// run state-machine trigger decide outcomes. In-process state (the execution budget) only
// limits local pressure (ADR 0002 Sec. 10).
package oeeservice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/oee"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
	"github.com/google/uuid"
)

var (
	ErrInvalid = errors.New("invalid OEE service contract")
	// ErrLimit is a request outside the dataset bounds (rule 07: 413).
	ErrLimit    = errors.New("OEE request limit exceeded")
	ErrNotFound = errors.New("OEE resource unavailable")
	ErrDenied   = errors.New("OEE permission denied")
	// ErrConflict is an idempotency key reused with a different request.
	ErrConflict = errors.New("OEE idempotency conflict")
	// ErrRevision is a stale If-Match or expected policy revision (rule 07: 412).
	ErrRevision = errors.New("OEE revision precondition failed")
	// ErrLifecycle is a transition the run state machine does not allow (ADR 0002 Sec. 18).
	ErrLifecycle = errors.New("OEE run lifecycle conflict")
	// ErrPolicyNotReady means no single published policy is effective for the window
	// (ADR 0002 Sec. 5 stage 3: missing or ambiguous policy is NOT_READY).
	ErrPolicyNotReady = errors.New("no single published OEE policy is effective for the window")
	// ErrPolicyOverlap rejects publishing a policy whose effective range overlaps another
	// published policy at the site (ADR 0002 Sec. 16).
	ErrPolicyOverlap = errors.New("published OEE policies would overlap")
	// ErrPolicyConflict is a policy code or identical revision that already exists.
	ErrPolicyConflict = errors.New("OEE policy revision conflicts with an existing one")
	// ErrOverloaded rejects work beyond the tenant's open-run budget or this process's
	// execution budget (ADR 0002 Sec. 10, 15.2).
	ErrOverloaded = errors.New("OEE capacity exhausted")
	errIntegrity  = errors.New("OEE stored state failed re-verification")
)

const (
	// cleanupTimeout bounds the transaction that records a terminal outcome after the run's
	// own deadline or context has ended (ADR 0002 Sec. 7.4: cleanup has a separate short
	// deadline and can never change a terminal run to success).
	cleanupTimeout = 10 * time.Second
)

type Actor struct{ Issuer, Subject, SessionHash string }
type Scope struct{ TenantID, SiteID string }

// Config holds the operational limits. They are deployment configuration, not product rules,
// and every one is required.
type Config struct {
	// RunDeadline is the absolute budget from acceptance to seal (ADR 0002 Sec. 10).
	RunDeadline time.Duration
	// LeaseDuration is how long an execution claim stays valid without renewal. A run whose
	// executor disappears is reclaimable after one lease rather than at its deadline.
	LeaseDuration time.Duration
	// MaxOpenRuns bounds live ACCEPTED plus RUNNING runs per tenant (PostgreSQL-fenced).
	MaxOpenRuns int
	// MaxConcurrentExecutions bounds runs executing in this process. Each one holds a
	// dataset in memory, so this caps the process's working set across all tenants.
	MaxConcurrentExecutions int
	// IdempotencyRetention is how long an idempotency key replays before it may be rebound.
	IdempotencyRetention time.Duration
	// ExecutionOwner identifies this process in the execution fence.
	ExecutionOwner string
	// Sessions is the browser-session policy (WIS-290); the zero value means session.Default.
	Sessions session.Policy
}

// minLeaseDuration keeps the renewal interval (a third of the lease) a sane positive period.
const minLeaseDuration = time.Second

func (c Config) valid() bool {
	return c.RunDeadline > 0 && c.LeaseDuration >= minLeaseDuration && c.MaxOpenRuns > 0 && c.MaxConcurrentExecutions > 0 &&
		c.IdempotencyRetention > 0 && validText(c.ExecutionOwner, 128)
}

// Service is safe for concurrent use. Close it on shutdown to drain in-flight executions.
type Service struct {
	db  *sql.DB
	cfg Config

	// slots is the process-wide execution budget: a new run needs a slot before it is
	// inserted, so an accepted run always has an executor and overload is rejected up front.
	slots chan struct{}
	// executions is the parent context of every background execution; Close cancels it when
	// draining runs out of time, so abandoned runs record their outcome instead of running on.
	executions context.Context
	abort      context.CancelFunc
	// abortWait bounds how long Close waits for cancelled executions to record their outcome.
	abortWait time.Duration
	// mu guards closed; running tracks held slots so Close can drain them.
	mu      sync.Mutex
	closed  bool
	running sync.WaitGroup
}

func New(db *sql.DB, cfg Config) (*Service, error) {
	if db == nil || !cfg.valid() {
		return nil, errors.New("oeeservice: database and every limit are required")
	}
	executions, abort := context.WithCancel(context.Background())
	return &Service{db: db, cfg: cfg, slots: make(chan struct{}, cfg.MaxConcurrentExecutions), executions: executions, abort: abort, abortWait: cleanupTimeout}, nil
}

// reserveSlot takes one unit of the execution budget without blocking. It fails when the
// budget is exhausted or the service is closing. A held slot counts toward Close's drain from
// the moment it is reserved, so a request accepted just before shutdown is still waited for.
// The returned release is idempotent.
func (s *Service) reserveSlot() (release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false
	}
	select {
	case s.slots <- struct{}{}:
		s.running.Add(1)
		var once sync.Once
		return func() {
			once.Do(func() {
				<-s.slots
				s.running.Done()
			})
		}, true
	default:
		return nil, false
	}
}

// Close stops accepting new work and waits for in-flight executions to finish. If ctx ends
// first, it cancels the remaining executions and waits up to cleanupTimeout more while each
// records its terminal outcome (it can never become success), then returns ctx.Err(). The
// caller must keep the database open until Close returns.
func (s *Service) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		s.running.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
	}
	s.abort()
	select {
	case <-done:
	case <-time.After(s.abortWait):
	}
	return ctx.Err()
}

func validID(v string) bool {
	id, e := uuid.Parse(v)
	return e == nil && id != uuid.Nil && id.String() == v
}

func validText(v string, max int) bool {
	return len(v) > 0 && len(v) <= max && strings.TrimSpace(v) == v && !strings.ContainsRune(v, 0)
}

func wireTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

type access int

const (
	read access = iota
	author
	publish
	calculate
)

// begin opens a tenant-scoped transaction for a verified browser session and checks the
// caller's active site membership. Separate permissions (ADR 0002 Sec. 15.1): any member reads
// policies, runs and evidence; owner or supervisor authors policies and requests or cancels
// calculations; publication is supervisor-only, as plan approval is.
//
// Mutations take the site row lock (lock_evidence_site with write=true), which serializes
// writers at one site; reads take a key-share lock only.
func (s *Service) begin(ctx context.Context, a Actor, sc Scope, mode access) (*sql.Tx, error) {
	if !validID(sc.TenantID) || !validID(sc.SiteID) || !validText(a.Issuer, 255) || !validText(a.Subject, 255) || !validText(a.SessionHash, 128) {
		return nil, ErrInvalid
	}
	tx, err := s.scoped(ctx, sc)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*sql.Tx, error) {
		_ = tx.Rollback()
		return nil, err
	}
	var issuer, subject string
	var expiry, seen, now time.Time
	err = tx.QueryRowContext(ctx, `SELECT issuer,subject,expires_at,last_seen_at,clock_timestamp() FROM supervisor.browser_sessions WHERE token_hash=$1 FOR SHARE`, a.SessionHash).Scan(&issuer, &subject, &expiry, &seen, &now)
	if err != nil || issuer != a.Issuer || subject != a.Subject || !s.cfg.Sessions.OrDefault().Active(expiry, seen, now) {
		return fail(ErrDenied)
	}
	var found bool
	if err = tx.QueryRowContext(ctx, `SELECT supervisor.lock_member_site($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, mode != read).Scan(&found); err != nil {
		return fail(err)
	}
	if !found {
		return fail(ErrNotFound)
	}
	role, err := memberRole(ctx, tx, sc, a.Issuer, a.Subject)
	if err != nil {
		return fail(err)
	}
	if !allowed(role, mode) {
		return fail(ErrDenied)
	}
	return tx, nil
}

// allowed maps a membership role to the service permissions (ADR 0002 Sec. 15.1). Owner is not
// an implicit bypass for publication.
func allowed(role string, mode access) bool {
	switch mode {
	case author, calculate:
		return role == "owner" || role == "supervisor"
	case publish:
		return role == "supervisor"
	}
	return true
}

func memberRole(ctx context.Context, tx *sql.Tx, sc Scope, issuer, subject string) (string, error) {
	var role string
	err := tx.QueryRowContext(ctx, `SELECT role FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND active FOR SHARE`, sc.TenantID, sc.SiteID, issuer, subject).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return role, err
}

// scoped opens a transaction with transaction-local tenant context (rule 03). The background
// executor uses it directly: it holds no session, so it only ever sees its own tenant's rows.
func (s *Service) scoped(ctx context.Context, sc Scope) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true),set_config('lock_timeout','5s',true)`, sc.TenantID); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

// finish re-checks the session before commit, so a session revoked mid-request commits nothing.
func (s *Service) finish(ctx context.Context, tx *sql.Tx, a Actor) error {
	var ok bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.browser_sessions WHERE token_hash=$1 AND issuer=$2 AND subject=$3 AND expires_at>clock_timestamp() AND last_seen_at>clock_timestamp()-make_interval(secs=>$4))`, a.SessionHash, a.Issuer, a.Subject, s.cfg.Sessions.OrDefault().IdleSeconds()).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrDenied
	}
	return tx.Commit()
}

// auditEntry is one append-only oee_audit_entry row (ADR 0002 Sec. 11). Empty optional fields
// are stored as NULL.
type auditEntry struct {
	issuer, subject, action, resourceType, resourceID, before, after, reason, correlation string
}

func audit(ctx context.Context, tx *sql.Tx, sc Scope, e auditEntry) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO supervisor.oee_audit_entry(tenant_id,site_id,actor_issuer,actor_subject,action,resource_type,resource_id,before_sha256,after_sha256,reason_code,correlation_id) VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,'')::uuid,NULLIF($8,''),NULLIF($9,''),NULLIF($10,''),$11)`,
		sc.TenantID, sc.SiteID, e.issuer, e.subject, e.action, e.resourceType, e.resourceID, e.before, e.after, e.reason, e.correlation)
	return err
}

// keyResponse is the stored response of an idempotency key: a reference to the resource the
// original request created or changed. A replay reloads that resource, so it always reflects
// the resource's current state and authorization is rechecked by the replaying request.
type keyResponse struct {
	PolicyID string `json:"policy_id,omitempty"`
	RunID    string `json:"run_id,omitempty"`
}

// claimKey reserves an idempotency key (ADR 0002 Sec. 7.1 step 2). A new key, or one whose
// retention has expired, is (re)bound to this request in one INSERT ... ON CONFLICT statement;
// a concurrent duplicate blocks on the key until the first transaction ends. A live key that
// holds the same request returns its stored response; one that holds a different request is
// ErrConflict.
func (s *Service) claimKey(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, op, key, hash string) (keyResponse, bool, error) {
	if !validText(key, 128) {
		return keyResponse{}, false, ErrInvalid
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO supervisor.oee_idempotency_key AS k(tenant_id,site_id,actor_issuer,actor_subject,operation,idempotency_key,request_sha256,expires_at)
VALUES($1,$2,$3,$4,$5,$6,$7,clock_timestamp()+make_interval(secs=>$8))
ON CONFLICT (tenant_id,site_id,actor_issuer,actor_subject,operation,idempotency_key) DO UPDATE
 SET request_sha256=EXCLUDED.request_sha256,run_id=NULL,response_status=NULL,response_json=NULL,created_at=clock_timestamp(),expires_at=EXCLUDED.expires_at
 WHERE k.expires_at<=clock_timestamp()`,
		sc.TenantID, sc.SiteID, a.Issuer, a.Subject, op, key, hash, s.cfg.IdempotencyRetention.Seconds())
	if err != nil {
		return keyResponse{}, false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return keyResponse{}, false, nil
	}
	var stored string
	var body []byte
	err = tx.QueryRowContext(ctx, `SELECT request_sha256,response_json FROM supervisor.oee_idempotency_key WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND operation=$5 AND idempotency_key=$6`,
		sc.TenantID, sc.SiteID, a.Issuer, a.Subject, op, key).Scan(&stored, &body)
	if err != nil {
		return keyResponse{}, false, err
	}
	if stored != hash || body == nil {
		return keyResponse{}, false, ErrConflict
	}
	var ref keyResponse
	if err = json.Unmarshal(body, &ref); err != nil {
		return keyResponse{}, false, errIntegrity
	}
	return ref, true, nil
}

// completeKey records the response of a key claimed in this transaction.
func completeKey(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, op, key string, status int, ref keyResponse) error {
	body, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE supervisor.oee_idempotency_key SET run_id=NULLIF($7,'')::uuid,response_status=$8,response_json=$9 WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND operation=$5 AND idempotency_key=$6`,
		sc.TenantID, sc.SiteID, a.Issuer, a.Subject, op, key, ref.RunID, status, body)
	return err
}

// requestDigest binds an idempotent request to its schema, version, tenant and site (rule 07).
func requestDigest(sc Scope, schema string, body any) (string, error) {
	_, digest, err := canonical.Envelope(schema, oee.ServiceSchemaVersion, sc.TenantID, sc.SiteID, body)
	return digest, err
}
