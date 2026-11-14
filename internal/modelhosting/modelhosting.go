// Package modelhosting is the per-site switch for hosted model inference (WIS-623, policy in
// rule 08 "Model endpoints", WIS-622). It is off by default: a site without a decision never
// calls a hosted endpoint. Only an owner of the site decides, with a recent sign-in, a reason
// and, to turn it on, a data processing agreement reference. Decisions are append-only events
// under a head with an If-Match revision and an idempotency receipt, like the maintenance roster.
package modelhosting

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
	"github.com/google/uuid"
)

var (
	ErrDenied           = errors.New("model hosting denied")
	ErrNotFound         = errors.New("model hosting not found")
	ErrInvalid          = errors.New("invalid model hosting decision")
	ErrRevision         = errors.New("model hosting revision conflict")
	ErrIdempotency      = errors.New("model hosting idempotency conflict")
	ErrReauthentication = errors.New("model hosting needs a recent sign-in")
	ErrUnchanged        = errors.New("model hosting decision changes nothing")
)

const (
	MaxReason       = 2000
	MaxDPAReference = 200
)

type Principal struct{ Issuer, Subject, SessionHash string }
type Scope struct{ TenantID, SiteID string }

// DecisionInput turns hosted inference on (with the DPA reference) or off for the site.
type DecisionInput struct {
	Enabled      bool   `json:"enabled"`
	DPAReference string `json:"dpa_reference,omitempty" contract:"max=200"`
	Reason       string `json:"reason" contract:"max=2000"`
}

// ActorRef names who decided.
type ActorRef struct {
	Issuer  string `json:"issuer" contract:"max=512"`
	Subject string `json:"subject" contract:"max=255"`
}

// DecisionV1 is one recorded decision.
type DecisionV1 struct {
	Revision     int64     `json:"revision"`
	Enabled      bool      `json:"enabled"`
	DPAReference *string   `json:"dpa_reference"`
	Reason       string    `json:"reason"`
	Actor        ActorRef  `json:"actor"`
	CreatedAt    time.Time `json:"created_at"`
}

// SiteModelHostingV1 is the site's current switch and its decisions, newest first. A site never
// decided is revision 0, off.
type SiteModelHostingV1 struct {
	SchemaVersion int          `json:"schema_version"`
	Enabled       bool         `json:"enabled"`
	DPAReference  *string      `json:"dpa_reference"`
	Revision      int64        `json:"revision"`
	Decisions     []DecisionV1 `json:"decisions"`
	Replayed      bool         `json:"replayed"`
}

type Service struct {
	db       *sql.DB
	sessions session.Policy
}

func New(db *sql.DB) *Service { return &Service{db: db, sessions: session.Default()} }

// WithSessionPolicy returns a copy that enforces p (WIS-290 shared session policy).
func (s *Service) WithSessionPolicy(p session.Policy) *Service {
	c := *s
	c.sessions = p.OrDefault()
	return &c
}

func validID(v string) bool {
	id, err := uuid.Parse(v)
	return err == nil && id != uuid.Nil && id.String() == v
}

func validField(v string, max int) bool {
	return len(v) > 0 && len(v) <= max && strings.TrimSpace(v) == v && utf8.ValidString(v) && !strings.ContainsRune(v, 0)
}

func validText(v string, max int) bool {
	n := utf8.RuneCountInString(v)
	return utf8.ValidString(v) && n >= 1 && n <= max && strings.TrimSpace(v) != "" && !strings.ContainsRune(v, 0)
}

type access int

const (
	read access = iota
	decide
)

// begin opens a tenant transaction for a live session of an active site member. Reading needs
// the owner or supervisor role; deciding needs the owner role and a recent sign-in.
func (s *Service) begin(ctx context.Context, a Principal, sc Scope, mode access) (*sql.Tx, time.Time, error) {
	if !validID(sc.TenantID) || !validID(sc.SiteID) || !validField(a.Issuer, 512) || !validField(a.Subject, 255) || !validField(a.SessionHash, 128) {
		return nil, time.Time{}, ErrDenied
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: mode == read})
	if err != nil {
		return nil, time.Time{}, err
	}
	fail := func(e error) (*sql.Tx, time.Time, error) {
		_ = tx.Rollback()
		return nil, time.Time{}, e
	}
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true),set_config('lock_timeout','5s',true)`, sc.TenantID); err != nil {
		return fail(err)
	}
	var issuer, subject string
	var expiry, seen, auth, now time.Time
	err = tx.QueryRowContext(ctx, `SELECT issuer,subject,expires_at,last_seen_at,auth_time,clock_timestamp() FROM supervisor.browser_sessions WHERE token_hash=$1`, a.SessionHash).Scan(&issuer, &subject, &expiry, &seen, &auth, &now)
	if err != nil || issuer != a.Issuer || subject != a.Subject || !expiry.After(now) || !seen.After(now.Add(-s.sessions.Idle)) {
		return fail(ErrDenied)
	}
	// A decision holds the membership row until it commits, so a concurrent revocation or role
	// change (change_membership locks the row FOR UPDATE) either waits for it or, committed first,
	// is seen here and refuses it (rule 03). A read-only transaction cannot lock.
	q := `SELECT role FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND active`
	if mode == decide {
		q += ` FOR SHARE`
	}
	var role string
	err = tx.QueryRowContext(ctx, q, sc.TenantID, sc.SiteID, a.Issuer, a.Subject).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return fail(ErrNotFound)
	}
	if err != nil {
		return fail(err)
	}
	switch {
	case mode == decide && role != "owner", mode == read && role != "owner" && role != "supervisor":
		return fail(ErrDenied)
	case mode == decide && (auth.After(now) || auth.Before(now.Add(-s.sessions.RecentAuth))):
		return fail(ErrReauthentication)
	}
	return tx, auth, nil
}

// finish re-checks the session inside the transaction so a sign-out or idle expiry that raced
// the request cannot commit it.
func (s *Service) finish(ctx context.Context, tx *sql.Tx, a Principal) error {
	var ok bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.browser_sessions WHERE token_hash=$1 AND issuer=$2 AND subject=$3 AND expires_at>clock_timestamp() AND last_seen_at>clock_timestamp()-make_interval(secs=>$4))`, a.SessionHash, a.Issuer, a.Subject, s.sessions.IdleSeconds()).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrDenied
	}
	return tx.Commit()
}

// Get returns the site's switch and its decisions.
func (s *Service) Get(ctx context.Context, a Principal, sc Scope) (SiteModelHostingV1, error) {
	tx, _, err := s.begin(ctx, a, sc, read)
	if err != nil {
		return SiteModelHostingV1{}, err
	}
	defer tx.Rollback()
	v, err := load(ctx, tx, sc, 0)
	if err != nil {
		return v, err
	}
	return v, tx.Commit()
}

// load reads the switch as of revision upTo, or the current switch when upTo is 0. Events are
// append-only, so the state at a past revision is that revision's event.
func load(ctx context.Context, tx *sql.Tx, sc Scope, upTo int64) (SiteModelHostingV1, error) {
	v := SiteModelHostingV1{SchemaVersion: 1, Decisions: []DecisionV1{}}
	var dpa sql.NullString
	q := `SELECT enabled,dpa_reference,revision FROM supervisor.site_model_hosting_heads WHERE tenant_id=$1 AND site_id=$2 AND $3=0`
	if upTo > 0 {
		q = `SELECT enabled,dpa_reference,revision FROM supervisor.site_model_hosting_events WHERE tenant_id=$1 AND site_id=$2 AND revision=$3`
	}
	err := tx.QueryRowContext(ctx, q, sc.TenantID, sc.SiteID, upTo).Scan(&v.Enabled, &dpa, &v.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	if dpa.Valid {
		v.DPAReference = &dpa.String
	}
	rows, err := tx.QueryContext(ctx, `SELECT revision,enabled,dpa_reference,reason,actor_issuer,actor_subject,created_at FROM supervisor.site_model_hosting_events WHERE tenant_id=$1 AND site_id=$2 AND revision<=$3 ORDER BY revision DESC LIMIT 100`, sc.TenantID, sc.SiteID, v.Revision)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	for rows.Next() {
		var d DecisionV1
		var ref sql.NullString
		if err = rows.Scan(&d.Revision, &d.Enabled, &ref, &d.Reason, &d.Actor.Issuer, &d.Actor.Subject, &d.CreatedAt); err != nil {
			return v, err
		}
		if ref.Valid {
			d.DPAReference = &ref.String
		}
		d.CreatedAt = d.CreatedAt.UTC()
		v.Decisions = append(v.Decisions, d)
	}
	return v, rows.Err()
}

// Decide records one decision. expected is the current revision (0 when never decided). The same
// key and request replays the original result after authorization is rechecked; the same key with
// another request is ErrIdempotency.
func (s *Service) Decide(ctx context.Context, a Principal, sc Scope, in DecisionInput, key string, expected int64) (SiteModelHostingV1, error) {
	if !validText(in.Reason, MaxReason) || !validField(key, 128) || expected < 0 ||
		(in.Enabled && !validField(in.DPAReference, MaxDPAReference)) || (!in.Enabled && in.DPAReference != "") {
		return SiteModelHostingV1{}, ErrInvalid
	}
	tx, auth, err := s.begin(ctx, a, sc, decide)
	if err != nil {
		return SiteModelHostingV1{}, err
	}
	defer tx.Rollback()
	b, _ := json.Marshal(struct {
		In       DecisionInput `json:"in"`
		Expected int64         `json:"expected"`
	}{in, expected})
	sum := sha256.Sum256(b)
	digest := hex.EncodeToString(sum[:])
	// One decision per site at a time, taken before the receipt lookup: a concurrent retry of the
	// same key waits here and then finds the receipt, instead of missing it and racing the head
	// (the head row cannot be locked before the first decision creates it).
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('supervisor.model_hosting:'||$1||':'||$2,0))`, sc.TenantID, sc.SiteID); err != nil {
		return SiteModelHostingV1{}, err
	}
	var prior string
	var recorded int64
	err = tx.QueryRowContext(ctx, `SELECT request_sha256,revision FROM supervisor.site_model_hosting_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`,
		sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&prior, &recorded)
	if err == nil {
		if prior != digest {
			return SiteModelHostingV1{}, ErrIdempotency
		}
		// The replay is the result this key recorded, not whatever a later decision made current.
		v, err := load(ctx, tx, sc, recorded)
		if err != nil {
			return v, err
		}
		v.Replayed = true
		return v, s.finish(ctx, tx, a)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return SiteModelHostingV1{}, err
	}
	var dpa *string
	if in.Enabled {
		dpa = &in.DPAReference
	}
	var revision int64
	var enabled bool
	var current sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT revision,enabled,dpa_reference FROM supervisor.site_model_hosting_heads WHERE tenant_id=$1 AND site_id=$2 FOR UPDATE`, sc.TenantID, sc.SiteID).Scan(&revision, &enabled, &current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if expected != 0 {
			return SiteModelHostingV1{}, ErrRevision
		}
		if !in.Enabled {
			return SiteModelHostingV1{}, ErrUnchanged // never decided is already off
		}
		revision = 1
		_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.site_model_hosting_heads(tenant_id,site_id,enabled,dpa_reference,revision) VALUES($1,$2,$3,$4,1)`, sc.TenantID, sc.SiteID, in.Enabled, dpa)
	case err != nil:
		return SiteModelHostingV1{}, err
	case expected != revision:
		return SiteModelHostingV1{}, ErrRevision
	case enabled == in.Enabled && (!in.Enabled || current.String == in.DPAReference):
		return SiteModelHostingV1{}, ErrUnchanged
	default:
		revision++
		_, err = tx.ExecContext(ctx, `UPDATE supervisor.site_model_hosting_heads SET enabled=$3,dpa_reference=$4,revision=$5,updated_at=clock_timestamp() WHERE tenant_id=$1 AND site_id=$2`,
			sc.TenantID, sc.SiteID, in.Enabled, dpa, revision)
	}
	if err != nil {
		return SiteModelHostingV1{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.site_model_hosting_events(tenant_id,site_id,revision,enabled,dpa_reference,reason,actor_issuer,actor_subject,auth_time) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		sc.TenantID, sc.SiteID, revision, in.Enabled, dpa, in.Reason, a.Issuer, a.Subject, auth); err != nil {
		return SiteModelHostingV1{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.site_model_hosting_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_sha256,revision) VALUES($1,$2,$3,$4,$5,$6,$7)`,
		sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, digest, revision); err != nil {
		return SiteModelHostingV1{}, err
	}
	v, err := load(ctx, tx, sc, 0)
	if err != nil {
		return v, err
	}
	return v, s.finish(ctx, tx, a)
}

// EnabledTx reports, inside the caller's tenant transaction, whether the site's switch is on now.
// The agent runtime calls it when a run is created (to pin the provider) and before every hosted
// call (so turning it off takes effect on the next call, including queued work).
func EnabledTx(ctx context.Context, tx *sql.Tx, tenantID, siteID string) (bool, error) {
	var enabled bool
	err := tx.QueryRowContext(ctx, `SELECT enabled FROM supervisor.site_model_hosting_heads WHERE tenant_id=$1 AND site_id=$2`, tenantID, siteID).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return enabled, err
}
