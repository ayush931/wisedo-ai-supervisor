// Package membership implements explicit, owner-managed site access changes.
package membership

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
)

var (
	ErrDenied           = errors.New("membership authority unavailable")
	ErrReauthentication = errors.New("recent authentication required")
	ErrConflict         = errors.New("idempotency conflict")
	ErrRevision         = errors.New("membership revision changed")
	ErrInvalid          = errors.New("invalid membership change")
)

type Actor struct{ Issuer, Subject, SessionHash string }
type Request struct {
	TenantID, SiteID, TargetIssuer, TargetSubject, Role, IdempotencyKey string
	Active                                                              bool
	ExpectedRevision                                                    int64
}
type Member struct {
	Issuer   string `json:"issuer"`
	Subject  string `json:"subject"`
	Role     string `json:"role"`
	Active   bool   `json:"active"`
	Revision int64  `json:"revision"`
}
type Page struct {
	Members    []Member `json:"members"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

// DirectoryEntry is one active member of a site as GET /v1/sites/{site}/members lists it
// (WIS-290). DisplayName is the IdP name stored at the member's last login; null when unknown.
// The issuer is not published: every member of a site shares the site's single issuer (a
// membership change requires actor and target issuers to match) and records are mapped by
// subject; it is kept only to build the opaque cursor.
type DirectoryEntry struct {
	Issuer      string  `json:"-"`
	Subject     string  `json:"subject" contract:"max=255"`
	DisplayName *string `json:"display_name" contract:"max=128"`
	Role        string  `json:"role" contract:"max=32"`
}
type DirectoryPage struct {
	Members    []DirectoryEntry `json:"members"`
	NextCursor string           `json:"next_cursor,omitempty" contract:"max=4096"`
}
type Service struct {
	db       *sql.DB
	sessions session.Policy
}

func New(db *sql.DB) *Service { return &Service{db: db, sessions: session.Default()} }

// WithSessionPolicy returns a copy of the service that enforces p (WIS-290: one configurable
// session policy for every service). New uses session.Default.
func (s *Service) WithSessionPolicy(p session.Policy) *Service {
	c := *s
	c.sessions = p.OrDefault()
	return &c
}

func validText(value string, max int) bool {
	return utf8.ValidString(value) && strings.IndexByte(value, 0) < 0 && len(value) > 0 && utf8.RuneCountInString(value) <= max
}
func validActor(a Actor) bool {
	decoded, err := hex.DecodeString(a.SessionHash)
	return err == nil && len(decoded) == 32 && strings.ToLower(a.SessionHash) == a.SessionHash && validText(a.Issuer, 512) && validText(a.Subject, 255)
}
func validate(a Actor, r Request) error {
	if !validActor(a) {
		return ErrDenied
	}
	if _, err := uuid.Parse(r.TenantID); err != nil {
		return ErrInvalid
	}
	if _, err := uuid.Parse(r.SiteID); err != nil {
		return ErrInvalid
	}
	if !validText(r.TargetIssuer, 512) || !validText(r.TargetSubject, 255) || !validText(r.IdempotencyKey, 128) || r.ExpectedRevision < 0 || r.ExpectedRevision == math.MaxInt64 || r.TargetIssuer != a.Issuer || r.TargetSubject == a.Subject {
		return ErrInvalid
	}
	switch r.Role {
	case "owner", "supervisor", "operator", "reviewer", "viewer":
	default:
		return ErrInvalid
	}
	if r.ExpectedRevision == 0 && !r.Active {
		return ErrInvalid
	}
	return nil
}

type rowQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Service) session(ctx context.Context, q rowQuery, a Actor, recent, lock bool) error {
	if !validActor(a) {
		return ErrDenied
	}
	query := `SELECT issuer,subject,expires_at,last_seen_at,auth_time,clock_timestamp() FROM supervisor.browser_sessions WHERE token_hash=$1`
	if lock {
		query += " FOR SHARE"
	}
	var issuer, subject string
	var expiry, seen, now time.Time
	var auth sql.NullTime
	err := q.QueryRowContext(ctx, query, a.SessionHash).Scan(&issuer, &subject, &expiry, &seen, &auth, &now)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrDenied
	}
	if err != nil {
		return err
	}
	if issuer != a.Issuer || subject != a.Subject || !expiry.After(now) || !seen.After(now.Add(-s.sessions.Idle)) {
		return ErrDenied
	}
	if recent && (!auth.Valid || auth.Time.After(now) || auth.Time.Before(now.Add(-s.sessions.RecentAuth))) {
		return ErrReauthentication
	}
	return nil
}
func translate(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "P2910":
			return ErrInvalid
		case "P2911":
			return ErrDenied
		case "P2912":
			return ErrReauthentication
		case "P2913":
			return ErrConflict
		case "P2914":
			return ErrRevision
		}
	}
	return err
}
func (s *Service) Change(ctx context.Context, a Actor, r Request) (Member, error) {
	if err := validate(a, r); err != nil {
		return Member{}, err
	}
	if err := s.session(ctx, s.db, a, true, false); err != nil {
		return Member{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Member{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('statement_timeout','10s',true),set_config('lock_timeout','5s',true)`); err != nil {
		return Member{}, err
	}
	var encoded []byte
	err = tx.QueryRowContext(ctx, `SELECT supervisor.change_membership($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, a.SessionHash, r.TenantID, r.SiteID, a.Issuer, a.Subject, r.TargetIssuer, r.TargetSubject, r.Role, r.Active, r.ExpectedRevision, r.IdempotencyKey).Scan(&encoded)
	if err != nil {
		return Member{}, translate(err)
	}
	var member Member
	if err = json.Unmarshal(encoded, &member); err != nil {
		return Member{}, err
	}
	if err = tx.Commit(); err != nil {
		return Member{}, err
	}
	return member, nil
}
func (s *Service) List(ctx context.Context, a Actor, tenant, site string, limit int, cursor string) (Page, error) {
	if _, err := uuid.Parse(tenant); err != nil {
		return Page{}, ErrInvalid
	}
	if _, err := uuid.Parse(site); err != nil {
		return Page{}, ErrInvalid
	}
	if limit < 1 || limit > 100 {
		return Page{}, ErrInvalid
	}
	after, err := decodeCursor(cursor)
	if err != nil {
		return Page{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Page{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true),set_config('lock_timeout','5s',true)`, tenant); err != nil {
		return Page{}, err
	}
	if err = s.session(ctx, tx, a, false, true); err != nil {
		return Page{}, err
	}
	var owner bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND role='owner' AND active FOR SHARE`, tenant, site, a.Issuer, a.Subject).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return Page{}, ErrDenied
	}
	if err != nil {
		return Page{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT issuer,subject,role,active,revision FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND (issuer,subject)>($3,$4) ORDER BY issuer,subject LIMIT $5`, tenant, site, after[0], after[1], limit+1)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	page := Page{Members: []Member{}}
	for rows.Next() {
		var m Member
		if err = rows.Scan(&m.Issuer, &m.Subject, &m.Role, &m.Active, &m.Revision); err != nil {
			return Page{}, err
		}
		page.Members = append(page.Members, m)
	}
	if err = rows.Err(); err != nil {
		return Page{}, err
	}
	if len(page.Members) > limit {
		page.Members = page.Members[:limit]
		page.NextCursor = encodeCursor(page.Members[limit-1].Issuer, page.Members[limit-1].Subject)
	}
	if err = tx.Commit(); err != nil {
		return Page{}, err
	}
	return page, nil
}

func decodeCursor(cursor string) ([2]string, error) {
	var after [2]string
	if cursor == "" {
		return after, nil
	}
	if len(cursor) > 4096 {
		return after, ErrInvalid
	}
	body, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return after, ErrInvalid
	}
	if err = json.Unmarshal(body, &after); err != nil || !validText(after[0], 512) || !validText(after[1], 255) {
		return after, ErrInvalid
	}
	return after, nil
}

func encodeCursor(issuer, subject string) string {
	encoded, _ := json.Marshal([2]string{issuer, subject})
	return base64.RawURLEncoding.EncodeToString(encoded)
}

// Directory lists a site's active members with their display names for any active member of
// that site (WIS-290). Revoked members are absent; emails are never stored or returned.
// ErrDenied means the actor is not (or no longer) an active member.
func (s *Service) Directory(ctx context.Context, a Actor, tenant, site string, limit int, cursor string) (DirectoryPage, error) {
	if _, err := uuid.Parse(tenant); err != nil {
		return DirectoryPage{}, ErrInvalid
	}
	if _, err := uuid.Parse(site); err != nil {
		return DirectoryPage{}, ErrInvalid
	}
	if limit < 1 || limit > 100 {
		return DirectoryPage{}, ErrInvalid
	}
	after, err := decodeCursor(cursor)
	if err != nil {
		return DirectoryPage{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DirectoryPage{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true)`, tenant); err != nil {
		return DirectoryPage{}, err
	}
	if err = s.session(ctx, tx, a, false, false); err != nil {
		return DirectoryPage{}, err
	}
	var member bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND active`, tenant, site, a.Issuer, a.Subject).Scan(&member)
	if errors.Is(err, sql.ErrNoRows) {
		return DirectoryPage{}, ErrDenied
	}
	if err != nil {
		return DirectoryPage{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.issuer,m.subject,p.display_name,m.role FROM supervisor.memberships m
 LEFT JOIN supervisor.member_profiles p ON p.tenant_id=m.tenant_id AND p.issuer=m.issuer AND p.subject=m.subject
 WHERE m.tenant_id=$1 AND m.site_id=$2 AND m.active AND (m.issuer,m.subject)>($3,$4) ORDER BY m.issuer,m.subject LIMIT $5`, tenant, site, after[0], after[1], limit+1)
	if err != nil {
		return DirectoryPage{}, err
	}
	defer rows.Close()
	page := DirectoryPage{Members: []DirectoryEntry{}}
	for rows.Next() {
		var m DirectoryEntry
		var name sql.NullString
		if err = rows.Scan(&m.Issuer, &m.Subject, &name, &m.Role); err != nil {
			return DirectoryPage{}, err
		}
		if name.Valid {
			m.DisplayName = &name.String
		}
		page.Members = append(page.Members, m)
	}
	if err = rows.Err(); err != nil {
		return DirectoryPage{}, err
	}
	if len(page.Members) > limit {
		page.Members = page.Members[:limit]
		page.NextCursor = encodeCursor(page.Members[limit-1].Issuer, page.Members[limit-1].Subject)
	}
	return page, tx.Commit()
}

// ActorID is the stable UUIDv5 the Supervisor uses for one verified identity (issuer, subject).
// Onboarding and the maintenance roster both name people by it, so it is derived in one place.
func ActorID(issuer, subject string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("wisedo.actor.v1\x00"+issuer+"\x00"+subject))
}
