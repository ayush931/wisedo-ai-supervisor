// Package store owns tenant-scoped PostgreSQL receipts for inert foundation probes.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

var (
	ErrDenied   = errors.New("permission denied")
	ErrConflict = errors.New("idempotency key identifies a different body")
	ErrNotFound = errors.New("not found")
)

type Store struct{ DB *sql.DB }
type Site struct {
	ID                         string                 `json:"id"`
	TenantID                   string                 `json:"tenant_id"`
	Name                       string                 `json:"name"`
	Role                       string                 `json:"role"`
	AgentAvailable             bool                   `json:"agent_available"`
	AgentUnavailableReasonCode AgentUnavailableReason `json:"agent_unavailable_reason_code,omitempty" contract:"description=Why agent support is unavailable; omitted when available. Availability is deployment-wide and is reported per site until a site agent policy exists."`
}

// AgentUnavailableReason is the closed set of reasons GET /v1/me reports for unavailable agent support.
type AgentUnavailableReason string

const (
	AgentNotConfigured AgentUnavailableReason = "AGENT_NOT_CONFIGURED"
	AgentModeDisabled  AgentUnavailableReason = "AGENT_MODE_DISABLED"
)

// Me is GET /v1/me (MeV1): the verified subject, its IdP display name (null when the IdP
// offered no acceptable name or the principal has no membership yet), the CSRF secret and the
// sites it is an active member of. Site agent availability contains no provider or model settings.
type Me struct {
	Subject     string  `json:"subject" contract:"max=255"`
	DisplayName *string `json:"display_name" contract:"max=128"`
	CSRFToken   string  `json:"csrf_token" contract:"max=128"`
	Sites       []Site  `json:"sites"`
}
type Receipt struct {
	ID        string    `json:"id"`
	SiteID    string    `json:"site_id"`
	Status    string    `json:"status"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}
type Event struct{ ID, CommandID, TenantID, SiteID string }

// ActiveTenants discovers worker scopes without exposing tenant names or rows.
func (s *Store) ActiveTenants(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT tenant_id::text FROM supervisor.active_tenant_ids() AS active(tenant_id) ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tenants []string
	for rows.Next() {
		var tenant string
		if err = rows.Scan(&tenant); err != nil {
			return nil, err
		}
		tenants = append(tenants, tenant)
	}
	return tenants, rows.Err()
}

// TenantActive reports current tenant lifecycle state through the discovery function.
func (s *Store) TenantActive(ctx context.Context, tenant string) (bool, error) {
	id, err := uuid.Parse(tenant)
	if err != nil {
		return false, nil
	}
	var active bool
	err = s.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM supervisor.active_tenant_ids() AS active(id) WHERE active.id=$1)`, id).Scan(&active)
	return active, err
}

func New(url string) (*Store, error) {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	return &Store{DB: db}, nil
}
func (s *Store) Close() error { return s.DB.Close() }
func (s *Store) VerifyRole(ctx context.Context) error {
	var unsafe bool
	err := s.DB.QueryRowContext(ctx, `SELECT r.rolsuper OR r.rolbypassrls
 OR EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='supervisor' AND pg_has_role(current_user,c.relowner,'MEMBER'))
 OR EXISTS(SELECT 1 FROM pg_roles p WHERE (p.rolsuper OR p.rolbypassrls)
 AND pg_has_role(current_user,p.oid,'MEMBER'))
 FROM pg_roles r WHERE r.rolname=current_user`).Scan(&unsafe)
	if err != nil {
		return err
	}
	if unsafe {
		return errors.New("runtime database role must not own tables or bypass RLS")
	}
	return nil
}
func (s *Store) transaction(ctx context.Context, tenant string) (*sql.Tx, error) {
	id, err := uuid.Parse(tenant)
	if err != nil {
		return nil, ErrDenied
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	for _, pair := range [][2]string{{"supervisor.tenant_id", id.String()}, {"statement_timeout", "10s"}, {"lock_timeout", "5s"}} {
		if _, err = tx.ExecContext(ctx, "SELECT set_config($1,$2,true)", pair[0], pair[1]); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
	}
	return tx, nil
}
func authorize(ctx context.Context, tx *sql.Tx, tenant, site, issuer, subject string, write bool) error {
	var role string
	err := tx.QueryRowContext(ctx, `SELECT role FROM supervisor.memberships
 WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND active FOR SHARE`, tenant, site, issuer, subject).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrDenied
	}
	if err != nil {
		return err
	}
	if write && role != "owner" && role != "supervisor" {
		return ErrDenied
	}
	return nil
}
func (s *Store) Sites(ctx context.Context, issuer, subject string) ([]Site, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.issuer',$1,true),set_config('supervisor.subject',$2,true)`, issuer, subject); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT s.id,s.tenant_id,s.name,m.role FROM supervisor.sites s
 JOIN supervisor.memberships m ON m.tenant_id=s.tenant_id AND m.site_id=s.id
 JOIN supervisor.active_tenant_ids() AS active(tenant_id) ON active.tenant_id=s.tenant_id
 WHERE m.issuer=$1 AND m.subject=$2 AND m.active ORDER BY s.tenant_id,s.id`, issuer, subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sites := []Site{}
	for rows.Next() {
		var site Site
		if err = rows.Scan(&site.ID, &site.TenantID, &site.Name, &site.Role); err != nil {
			return nil, err
		}
		sites = append(sites, site)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return sites, tx.Commit()
}

// DisplayName returns the principal's most recently stored display name across its tenants
// (WIS-290, written at login), or nil when none is known. Only the self_profile policy admits it.
func (s *Store) DisplayName(ctx context.Context, issuer, subject string) (*string, error) {
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.issuer',$1,true),set_config('supervisor.subject',$2,true)`, issuer, subject); err != nil {
		return nil, err
	}
	var name sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT display_name FROM supervisor.member_profiles WHERE issuer=$1 AND subject=$2 ORDER BY updated_at DESC LIMIT 1`, issuer, subject).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !name.Valid) {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, err
	}
	return &name.String, tx.Commit()
}
func scanReceipt(row *sql.Row) (Receipt, error) {
	var r Receipt
	err := row.Scan(&r.ID, &r.SiteID, &r.Status, &r.Message, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, ErrNotFound
	}
	return r, err
}

const receiptColumns = `id,site_id,status,payload->>'message',created_at`

func (s *Store) SubmitProbe(ctx context.Context, tenant, site, issuer, subject, key, message string) (Receipt, error) {
	if len(key) < 1 || len(key) > 128 {
		return Receipt{}, errors.New("idempotency key must contain 1..128 bytes")
	}
	body, err := json.Marshal(struct {
		Message string `json:"message"`
	}{message})
	if err != nil {
		return Receipt{}, err
	}
	if len(body) > 4096 {
		return Receipt{}, errors.New("probe payload exceeds 4096 bytes")
	}
	digest := sha256.Sum256(body)
	hash := hex.EncodeToString(digest[:])
	tx, err := s.transaction(ctx, tenant)
	if err != nil {
		return Receipt{}, err
	}
	defer tx.Rollback()
	if err = authorize(ctx, tx, tenant, site, issuer, subject, true); err != nil {
		return Receipt{}, err
	}
	id := uuid.NewString()
	result, err := tx.ExecContext(ctx, `INSERT INTO supervisor.commands
 (id,tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,payload_hash,payload,kind)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8::jsonb,'foundation_probe')
 ON CONFLICT(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key) DO NOTHING`, id, tenant, site, issuer, subject, key, hash, string(body))
	if err != nil {
		return Receipt{}, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return Receipt{}, err
	}
	var storedHash string
	if err = tx.QueryRowContext(ctx, `SELECT id,payload_hash FROM supervisor.commands
 WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, tenant, site, issuer, subject, key).Scan(&id, &storedHash); err != nil {
		return Receipt{}, err
	}
	if storedHash != hash {
		return Receipt{}, ErrConflict
	}
	if inserted == 1 {
		if err = audit(ctx, tx, tenant, site, id, issuer, subject, "foundation_probe.accepted"); err != nil {
			return Receipt{}, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.outbox(id,tenant_id,site_id,command_id) VALUES($1,$2,$3,$4)`, uuid.NewString(), tenant, site, id); err != nil {
			return Receipt{}, err
		}
	}
	receipt, err := scanReceipt(tx.QueryRowContext(ctx, `SELECT `+receiptColumns+` FROM supervisor.commands WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, tenant, site, id))
	if err != nil {
		return Receipt{}, err
	}
	return receipt, tx.Commit()
}
func audit(ctx context.Context, tx *sql.Tx, tenant, site, command, issuer, subject, event string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO supervisor.audit_events(id,tenant_id,site_id,command_id,actor_issuer,actor_subject,event_type)
 VALUES($1,$2,$3,$4,$5,$6,$7)`, uuid.NewString(), tenant, site, command, issuer, subject, event)
	return err
}
func (s *Store) GetProbe(ctx context.Context, tenant, site, issuer, subject, command string) (Receipt, error) {
	tx, err := s.transaction(ctx, tenant)
	if err != nil {
		return Receipt{}, err
	}
	defer tx.Rollback()
	if err = authorize(ctx, tx, tenant, site, issuer, subject, false); err != nil {
		return Receipt{}, err
	}
	receipt, err := scanReceipt(tx.QueryRowContext(ctx, `SELECT `+receiptColumns+` FROM supervisor.commands WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, tenant, site, command))
	if err != nil {
		return Receipt{}, err
	}
	return receipt, tx.Commit()
}
func (s *Store) Pending(ctx context.Context, tenant string) ([]Event, error) {
	tx, err := s.transaction(ctx, tenant)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,command_id,tenant_id,site_id FROM supervisor.outbox
 WHERE tenant_id=$1 AND delivered_at IS NULL ORDER BY created_at,id LIMIT 100`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		var event Event
		if err = rows.Scan(&event.ID, &event.CommandID, &event.TenantID, &event.SiteID); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return events, tx.Commit()
}
func (s *Store) MarkDelivered(ctx context.Context, tenant, id string) error {
	tx, err := s.transaction(ctx, tenant)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE supervisor.outbox SET delivered_at=now() WHERE tenant_id=$1 AND id=$2 AND delivered_at IS NULL`, tenant, id); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) ProcessProbe(ctx context.Context, tenant, id string) (string, error) {
	tx, err := s.transaction(ctx, tenant)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var site, issuer, subject string
	if err = tx.QueryRowContext(ctx, `SELECT site_id,actor_issuer,actor_subject FROM supervisor.commands WHERE tenant_id=$1 AND id=$2`, tenant, id).Scan(&site, &issuer, &subject); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	// Membership lock always precedes command lock, including acceptance and replay.
	next := "completed"
	if err = authorize(ctx, tx, tenant, site, issuer, subject, true); err != nil {
		if errors.Is(err, ErrDenied) {
			next = "rejected"
		} else {
			return "", err
		}
	}
	var current string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM supervisor.commands WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenant, id).Scan(&current); err != nil {
		return "", err
	}
	if current != "accepted" {
		return current, tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, `UPDATE supervisor.commands SET status=$1,completed_at=now() WHERE tenant_id=$2 AND id=$3`, next, tenant, id); err != nil {
		return "", err
	}
	if err = audit(ctx, tx, tenant, site, id, issuer, subject, "foundation_probe."+next); err != nil {
		return "", err
	}
	return next, tx.Commit()
}
