// Package roster keeps the supervisor-confirmed maintenance assignee roster (WIS-276): for each
// line, the site members a supervisor has confirmed as responsible for a maintenance
// investigation. A recovery proposal may assign only someone confirmed here for the case's line;
// an assignee is never inferred from documents or qualification text.
package roster

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/membership"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
)

var (
	// ErrNotFound: no such site for this principal (non-disclosing 404).
	ErrNotFound = errors.New("roster unavailable")
	// ErrDenied: the principal's session or role does not allow this (403).
	ErrDenied = errors.New("roster permission denied")
	// ErrReauthentication: a decision needs a recent sign-in (428).
	ErrReauthentication = errors.New("recent authentication required")
	// ErrInvalid: the request is malformed (422).
	ErrInvalid = errors.New("invalid roster decision")
	// ErrLine: the line is not a current line asset of this site (422).
	ErrLine = errors.New("line unavailable")
	// ErrAssignee: the assignee is not an active operator or supervisor of this site (422).
	ErrAssignee = errors.New("assignee not eligible")
	// ErrIdempotency: the key was used with a different request (409).
	ErrIdempotency = errors.New("idempotency conflict")
	// ErrRevision: If-Match named a stale revision (412).
	ErrRevision = errors.New("roster revision changed")
	// ErrUnchanged: the decision would not change the entry (409).
	ErrUnchanged = errors.New("roster entry unchanged")
)

// Decision is a roster decision: confirm or withdraw an assignee for a line.
type Decision string

const (
	DecisionConfirm  Decision = "confirm"
	DecisionWithdraw Decision = "withdraw"
)

const (
	MaxReason       = 2000
	DefaultPageSize = 50
	MaxPageSize     = 100
)

// EligibleRoles are the membership roles that may be confirmed as a maintenance assignee.
var EligibleRoles = []string{"operator", "supervisor"}

type Principal struct{ Issuer, Subject, SessionHash string }
type Scope struct{ TenantID, SiteID string }

// DecisionInput confirms or withdraws one member as a maintenance assignee for one line. The
// assignee is named by subject; every member of a site shares the site's issuer.
type DecisionInput struct {
	LineID          string   `json:"line_id" contract:"uuid"`
	AssigneeSubject string   `json:"assignee_subject" contract:"max=255"`
	Decision        Decision `json:"decision"`
	Reason          string   `json:"reason" contract:"max=2000"`
}

// Entry is one line/assignee pair and its latest decision. AssigneeEligible is whether the
// assignee is still an active operator or supervisor; a confirmed but ineligible entry is not
// offered to proposals.
type Entry struct {
	LineID           string `json:"line_id" contract:"uuid"`
	AssigneeID       string `json:"assignee_id" contract:"uuid"`
	AssigneeSubject  string `json:"assignee_subject" contract:"max=255"`
	Confirmed        bool   `json:"confirmed"`
	AssigneeEligible bool   `json:"assignee_eligible"`
	Revision         int64  `json:"revision"`
	Reason           string `json:"reason" contract:"max=2000"`
	DecidedBy        string `json:"decided_by_subject" contract:"max=255"`
	DecidedAt        string `json:"decided_at" contract:"datetime"`
}

type Page struct {
	Entries    []Entry `json:"entries"`
	NextCursor string  `json:"next_cursor,omitempty"`
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
	u, err := uuid.Parse(v)
	return err == nil && u.String() == v
}

func validField(v string, max int) bool {
	return len(v) > 0 && len(v) <= max && strings.TrimSpace(v) == v && utf8.ValidString(v) && !strings.ContainsRune(v, 0)
}

func validReason(v string) bool {
	n := utf8.RuneCountInString(v)
	return utf8.ValidString(v) && n >= 1 && n <= MaxReason && strings.TrimSpace(v) != "" && !strings.ContainsRune(v, 0)
}

type access int

const (
	read access = iota
	decide
)

// begin opens a tenant transaction for a live session of an active site member. Reads need the
// owner or supervisor role; a decision needs the supervisor role and a recent sign-in. It returns
// the session's auth_time for the audit event.
func (s *Service) begin(ctx context.Context, a Principal, sc Scope, mode access) (*sql.Tx, time.Time, error) {
	if !validID(sc.TenantID) || !validID(sc.SiteID) || !validField(a.Issuer, 512) || !validField(a.Subject, 255) || !validField(a.SessionHash, 128) {
		return nil, time.Time{}, ErrDenied
	}
	tx, err := s.db.BeginTx(ctx, nil)
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
	err = tx.QueryRowContext(ctx, `SELECT issuer,subject,expires_at,last_seen_at,auth_time,clock_timestamp() FROM supervisor.browser_sessions WHERE token_hash=$1 FOR SHARE`, a.SessionHash).Scan(&issuer, &subject, &expiry, &seen, &auth, &now)
	if err != nil || issuer != a.Issuer || subject != a.Subject || !expiry.After(now) || !seen.After(now.Add(-s.sessions.Idle)) {
		return fail(ErrDenied)
	}
	var found bool
	if err = tx.QueryRowContext(ctx, `SELECT supervisor.lock_member_site($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, mode == decide).Scan(&found); err != nil {
		return fail(err)
	}
	if !found {
		return fail(ErrNotFound)
	}
	var role string
	err = tx.QueryRowContext(ctx, `SELECT role FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND active FOR SHARE`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return fail(ErrNotFound)
	}
	if err != nil {
		return fail(err)
	}
	switch {
	case mode == decide && role != "supervisor", mode == read && role != "supervisor" && role != "owner":
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

// Decide records one confirm or withdraw decision. expected is the entry's current revision (0
// when the pair has never been decided). The same key and request replays the original result
// after authorization is rechecked; the same key with another request is ErrIdempotency.
func (s *Service) Decide(ctx context.Context, a Principal, sc Scope, in DecisionInput, key string, expected int64) (Entry, error) {
	if !validID(in.LineID) || !validField(in.AssigneeSubject, 255) || (in.Decision != DecisionConfirm && in.Decision != DecisionWithdraw) ||
		!validReason(in.Reason) || !validField(key, 128) || expected < 0 {
		return Entry{}, ErrInvalid
	}
	tx, auth, err := s.begin(ctx, a, sc, decide)
	if err != nil {
		return Entry{}, err
	}
	defer tx.Rollback()
	b, _ := json.Marshal(struct {
		In       DecisionInput `json:"in"`
		Expected int64         `json:"expected"`
	}{in, expected})
	sum := sha256.Sum256(b)
	digest := hex.EncodeToString(sum[:])
	var prior, line, assignee string
	var priorRevision int64
	err = tx.QueryRowContext(ctx, `SELECT request_sha256,line_id::text,assignee_actor_id::text,revision FROM supervisor.maintenance_roster_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`,
		sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&prior, &line, &assignee, &priorRevision)
	if err == nil {
		if prior != digest {
			return Entry{}, ErrIdempotency
		}
		e, err := entryAt(ctx, tx, sc, line, assignee, priorRevision)
		if err != nil {
			return Entry{}, err
		}
		return e, s.finish(ctx, tx, a)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Entry{}, err
	}
	// The line must be a current line asset of this site's knowledge inventory.
	var isLine bool
	err = tx.QueryRowContext(ctx, `SELECT r.payload->>'type'='asset' AND r.payload->>'asset_kind'='line' FROM supervisor.knowledge_entity_heads h
 JOIN supervisor.knowledge_entity_revisions r ON (r.tenant_id,r.site_id,r.id,r.revision)=(h.tenant_id,h.site_id,h.id,h.current_revision)
 WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.id=$3`, sc.TenantID, sc.SiteID, in.LineID).Scan(&isLine)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !isLine) {
		return Entry{}, ErrLine
	}
	if err != nil {
		return Entry{}, err
	}
	// Members of a site share its issuer, so the decider's issuer names the assignee. Confirming
	// needs an active eligible member now; withdrawing is allowed for anyone already on the roster.
	var role string
	var active bool
	err = tx.QueryRowContext(ctx, `SELECT role,active FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 FOR SHARE`,
		sc.TenantID, sc.SiteID, a.Issuer, in.AssigneeSubject).Scan(&role, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, ErrAssignee
	}
	if err != nil {
		return Entry{}, err
	}
	confirm := in.Decision == DecisionConfirm
	if confirm && (!active || !eligibleRole(role)) {
		return Entry{}, ErrAssignee
	}
	assignee = membership.ActorID(a.Issuer, in.AssigneeSubject).String()
	var revision int64
	var current bool
	err = tx.QueryRowContext(ctx, `SELECT revision,confirmed FROM supervisor.maintenance_roster_heads WHERE tenant_id=$1 AND site_id=$2 AND line_id=$3 AND assignee_actor_id=$4 FOR UPDATE`,
		sc.TenantID, sc.SiteID, in.LineID, assignee).Scan(&revision, &current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if expected != 0 {
			return Entry{}, ErrRevision
		}
		if !confirm {
			return Entry{}, ErrUnchanged
		}
		revision = 1
		_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.maintenance_roster_heads(tenant_id,site_id,line_id,assignee_actor_id,assignee_issuer,assignee_subject,confirmed,revision) VALUES($1,$2,$3,$4,$5,$6,true,1)`,
			sc.TenantID, sc.SiteID, in.LineID, assignee, a.Issuer, in.AssigneeSubject)
	case err != nil:
		return Entry{}, err
	case expected != revision:
		return Entry{}, ErrRevision
	case current == confirm:
		return Entry{}, ErrUnchanged
	default:
		revision++
		_, err = tx.ExecContext(ctx, `UPDATE supervisor.maintenance_roster_heads SET confirmed=$5,revision=$6,updated_at=clock_timestamp() WHERE tenant_id=$1 AND site_id=$2 AND line_id=$3 AND assignee_actor_id=$4`,
			sc.TenantID, sc.SiteID, in.LineID, assignee, confirm, revision)
	}
	if err != nil {
		return Entry{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.maintenance_roster_events(tenant_id,site_id,line_id,assignee_actor_id,revision,confirmed,reason,actor_issuer,actor_subject,auth_time) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		sc.TenantID, sc.SiteID, in.LineID, assignee, revision, confirm, in.Reason, a.Issuer, a.Subject, auth); err != nil {
		return Entry{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.maintenance_roster_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_sha256,line_id,assignee_actor_id,revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, digest, in.LineID, assignee, revision); err != nil {
		return Entry{}, err
	}
	e, err := entryAt(ctx, tx, sc, in.LineID, assignee, revision)
	if err != nil {
		return Entry{}, err
	}
	return e, s.finish(ctx, tx, a)
}

func eligibleRole(role string) bool {
	for _, r := range EligibleRoles {
		if r == role {
			return true
		}
	}
	return false
}

const entrySelect = `SELECT h.line_id::text,h.assignee_actor_id::text,h.assignee_subject,e.confirmed,
 COALESCE(m.active AND m.role IN ('operator','supervisor'),false),e.revision,e.reason,e.actor_subject,e.created_at
 FROM supervisor.maintenance_roster_heads h
 JOIN supervisor.maintenance_roster_events e ON (e.tenant_id,e.site_id,e.line_id,e.assignee_actor_id)=(h.tenant_id,h.site_id,h.line_id,h.assignee_actor_id)
 LEFT JOIN supervisor.memberships m ON (m.tenant_id,m.site_id,m.issuer,m.subject)=(h.tenant_id,h.site_id,h.assignee_issuer,h.assignee_subject)`

type scanner interface{ Scan(...any) error }

func scanEntry(r scanner) (Entry, error) {
	var e Entry
	var at time.Time
	if err := r.Scan(&e.LineID, &e.AssigneeID, &e.AssigneeSubject, &e.Confirmed, &e.AssigneeEligible, &e.Revision, &e.Reason, &e.DecidedBy, &at); err != nil {
		return Entry{}, err
	}
	e.DecidedAt = at.UTC().Format(time.RFC3339Nano)
	return e, nil
}

// entryAt is the entry as of one decision revision (a replay returns the original decision).
func entryAt(ctx context.Context, tx *sql.Tx, sc Scope, line, assignee string, revision int64) (Entry, error) {
	return scanEntry(tx.QueryRowContext(ctx, entrySelect+` WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.line_id=$3 AND h.assignee_actor_id=$4 AND e.revision=$5`,
		sc.TenantID, sc.SiteID, line, assignee, revision))
}

// List returns each line/assignee pair at its latest decision, ordered by line then assignee.
// lineID, when set, narrows the list to one line.
func (s *Service) List(ctx context.Context, a Principal, sc Scope, lineID, cursor string, limit int) (Page, error) {
	if (lineID != "" && !validID(lineID)) || limit < 1 || limit > MaxPageSize {
		return Page{}, ErrInvalid
	}
	afterLine, afterAssignee := uuid.Nil.String(), uuid.Nil.String()
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		parts := strings.Split(string(raw), "/")
		if err != nil || len(parts) != 2 || !validID(parts[0]) || !validID(parts[1]) {
			return Page{}, ErrInvalid
		}
		afterLine, afterAssignee = parts[0], parts[1]
	}
	tx, _, err := s.begin(ctx, a, sc, read)
	if err != nil {
		return Page{}, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, entrySelect+` WHERE h.tenant_id=$1 AND h.site_id=$2 AND e.revision=h.revision
 AND ($3='' OR h.line_id=nullif($3,'')::uuid) AND (h.line_id,h.assignee_actor_id)>($4::uuid,$5::uuid)
 ORDER BY h.line_id,h.assignee_actor_id LIMIT $6`, sc.TenantID, sc.SiteID, lineID, afterLine, afterAssignee, limit+1)
	if err != nil {
		return Page{}, err
	}
	page := Page{Entries: []Entry{}}
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			rows.Close()
			return Page{}, err
		}
		page.Entries = append(page.Entries, e)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return Page{}, err
	}
	if len(page.Entries) > limit {
		page.Entries = page.Entries[:limit]
		last := page.Entries[limit-1]
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(last.LineID + "/" + last.AssigneeID))
	}
	return page, s.finish(ctx, tx, a)
}

// Confirmed is one assignee a recovery proposal may name for a line, with the actor ID of the
// supervisor whose decision confirmed them.
type Confirmed struct{ AssigneeID, ConfirmedBy string }

// ConfirmedTx returns the line's confirmed assignees inside the caller's tenant transaction, in
// assignee order. It fails closed on revocation: an assignee who is no longer an active operator
// or supervisor, or whose confirming supervisor no longer holds that role, is left out.
func ConfirmedTx(ctx context.Context, tx *sql.Tx, tenantID, siteID, lineID string) ([]Confirmed, error) {
	if !validID(tenantID) || !validID(siteID) || !validID(lineID) {
		return []Confirmed{}, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT h.assignee_actor_id::text,e.actor_issuer,e.actor_subject FROM supervisor.maintenance_roster_heads h
 JOIN supervisor.maintenance_roster_events e ON (e.tenant_id,e.site_id,e.line_id,e.assignee_actor_id,e.revision)=(h.tenant_id,h.site_id,h.line_id,h.assignee_actor_id,h.revision)
 JOIN supervisor.memberships m ON (m.tenant_id,m.site_id,m.issuer,m.subject)=(h.tenant_id,h.site_id,h.assignee_issuer,h.assignee_subject)
 JOIN supervisor.memberships d ON (d.tenant_id,d.site_id,d.issuer,d.subject)=(e.tenant_id,e.site_id,e.actor_issuer,e.actor_subject)
 WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.line_id=$3 AND h.confirmed AND m.active AND m.role IN ('operator','supervisor') AND d.active AND d.role='supervisor'
 ORDER BY 1`, tenantID, siteID, lineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Confirmed{}
	for rows.Next() {
		var c Confirmed
		var issuer, subject string
		if err := rows.Scan(&c.AssigneeID, &issuer, &subject); err != nil {
			return nil, err
		}
		c.ConfirmedBy = membership.ActorID(issuer, subject).String()
		out = append(out, c)
	}
	return out, rows.Err()
}
