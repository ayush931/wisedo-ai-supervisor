// Package bootstrap provides privileged, actor-bound initial owner enrollment.
// Invitations are capabilities bound to an exact site and verified OIDC identity.
// No first-login promotion, email linking, or existing membership elevation occurs.
package bootstrap

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrInvalidInput = errors.New("invalid bootstrap request")
var ErrInvalidInvitation = errors.New("invalid setup invitation")

type Service struct{ DB *sql.DB }
type Actor struct{ Issuer, Subject string }
type IssueRequest struct {
	TenantID, SiteID, Issuer, Subject, Role string
	TTL                                     time.Duration
}
type Invitation struct {
	ID        string    `json:"id"`
	Token     string    `json:"token"`
	TenantID  string    `json:"tenant_id"`
	SiteID    string    `json:"site_id"`
	Role      string    `json:"role"`
	ExpiresAt time.Time `json:"expires_at"`
}

// RedeemRequest carries only the invitation token. Tenant and site are resolved from the
// stored invitation, never from the caller (rule 03).
type RedeemRequest struct{ Token string }
type Membership struct {
	TenantID string `json:"tenant_id"`
	SiteID   string `json:"site_id"`
	Role     string `json:"role"`
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
func validActor(actor Actor) bool {
	u, err := url.Parse(actor.Issuer)
	return err == nil && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Scheme == "https" || (u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "keycloak"))) && len(actor.Issuer) <= 512 && len(actor.Subject) > 0 && len(actor.Subject) <= 255 && strings.TrimSpace(actor.Subject) == actor.Subject
}

// ValidateIssueRequest checks an owner invitation before a caller commits related setup rows.
func ValidateIssueRequest(request IssueRequest) error {
	_, _, err := scope(request.TenantID, request.SiteID)
	if err != nil || !validActor(Actor{request.Issuer, request.Subject}) || request.Role != "owner" || request.TTL <= 0 || request.TTL > 24*time.Hour {
		return ErrInvalidInput
	}
	return nil
}
func scope(tenant, site string) (string, string, error) {
	t, err := uuid.Parse(tenant)
	if err != nil || t == uuid.Nil {
		return "", "", ErrInvalidInput
	}
	s, err := uuid.Parse(site)
	if err != nil || s == uuid.Nil {
		return "", "", ErrInvalidInput
	}
	return t.String(), s.String(), nil
}
func scopedTx(ctx context.Context, db *sql.DB, tenant string) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true),set_config('lock_timeout','5s',true)`, tenant); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

// Issue requires a separately held privileged provisioning connection. Its raw
// token is returned once to the caller for exclusive private-file delivery.
func (s Service) Issue(ctx context.Context, request IssueRequest) (Invitation, error) {
	tenant, _, err := scope(request.TenantID, request.SiteID)
	if err != nil {
		return Invitation{}, err
	}
	if err = ValidateIssueRequest(request); err != nil {
		return Invitation{}, err
	}
	tx, err := scopedTx(ctx, s.DB, tenant)
	if err != nil {
		return Invitation{}, err
	}
	defer tx.Rollback()
	invitation, err := s.IssueTx(ctx, tx, request)
	if err != nil {
		return Invitation{}, err
	}
	if err = tx.Commit(); err != nil {
		return Invitation{}, err
	}
	return invitation, nil
}

// IssueTx writes an invitation and audit event in the caller's transaction.
func (s Service) IssueTx(ctx context.Context, tx *sql.Tx, request IssueRequest) (Invitation, error) {
	tenant, site, err := scope(request.TenantID, request.SiteID)
	if err != nil || tx == nil {
		return Invitation{}, ErrInvalidInput
	}
	if err = ValidateIssueRequest(request); err != nil {
		return Invitation{}, err
	}
	entropy := make([]byte, 32)
	if _, err = rand.Read(entropy); err != nil {
		return Invitation{}, err
	}
	invitation := Invitation{ID: uuid.NewString(), Token: base64.RawURLEncoding.EncodeToString(entropy), TenantID: tenant, SiteID: site, Role: "owner"}
	var existing string
	err = tx.QueryRowContext(ctx, "SELECT id FROM supervisor.sites WHERE tenant_id=$1 AND id=$2 FOR UPDATE", tenant, site).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return Invitation{}, ErrInvalidInvitation
	}
	if err != nil {
		return Invitation{}, err
	}
	var forbidden bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND ((role='owner' AND active) OR (issuer=$3 AND subject=$4)))`, tenant, site, request.Issuer, request.Subject).Scan(&forbidden); err != nil {
		return Invitation{}, err
	}
	if forbidden {
		return Invitation{}, ErrInvalidInvitation
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO supervisor.setup_invitations(id,tenant_id,site_id,token_hash,issuer,subject,role,created_at,expires_at)
 VALUES($1,$2,$3,$4,$5,$6,'owner',statement_timestamp(),statement_timestamp()+make_interval(secs=>$7)) RETURNING expires_at`, invitation.ID, tenant, site, tokenHash(invitation.Token), request.Issuer, request.Subject, request.TTL.Seconds()).Scan(&invitation.ExpiresAt)
	if err != nil {
		return Invitation{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.bootstrap_audit(tenant_id,site_id,invitation_id,issuer,subject,database_actor,event_type) VALUES($1,$2,$3,$4,$5,current_user,'bootstrap.invitation_issued')`, tenant, site, invitation.ID, request.Issuer, request.Subject); err != nil {
		return Invitation{}, err
	}
	return invitation, nil
}

// Redeem's actor MUST come from the verified server session. Never construct it
// from the request body. HTTP callers must enforce CSRF before invoking this.
func (s Service) Redeem(ctx context.Context, actor Actor, request RedeemRequest) (Membership, error) {
	if !validActor(actor) {
		return Membership{}, ErrInvalidInvitation
	}
	entropy, err := base64.RawURLEncoding.DecodeString(request.Token)
	if err != nil || len(entropy) != 32 || base64.RawURLEncoding.EncodeToString(entropy) != request.Token {
		return Membership{}, ErrInvalidInvitation
	}
	hash := tokenHash(request.Token)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Membership{}, err
	}
	defer tx.Rollback()
	// Resolve the invitation from its token. The lookup policy exposes only this hash's row.
	if _, err = tx.ExecContext(ctx, "SELECT set_config('supervisor.redeem_token_hash',$1,true)", hash); err != nil {
		return Membership{}, err
	}
	var tenant, site string
	err = tx.QueryRowContext(ctx, `SELECT tenant_id::text,site_id::text FROM supervisor.setup_invitations WHERE token_hash=$1 AND consumed_at IS NULL AND expires_at>clock_timestamp()`, hash).Scan(&tenant, &site)
	if errors.Is(err, sql.ErrNoRows) {
		return Membership{}, ErrInvalidInvitation
	}
	if err != nil {
		return Membership{}, err
	}
	var active bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.active_tenant_ids() AS active(tenant_id) WHERE active.tenant_id=$1::uuid)`, tenant).Scan(&active); err != nil {
		return Membership{}, err
	}
	if !active {
		return Membership{}, ErrInvalidInvitation
	}
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true),set_config('lock_timeout','5s',true)`, tenant); err != nil {
		return Membership{}, err
	}
	if _, err = tx.ExecContext(ctx, "SELECT set_config('supervisor.issuer',$1,true),set_config('supervisor.subject',$2,true)", actor.Issuer, actor.Subject); err != nil {
		return Membership{}, err
	}
	membership := Membership{TenantID: tenant, SiteID: site}
	err = tx.QueryRowContext(ctx, "SELECT supervisor.redeem_setup_invitation($1,$2::uuid,$3::uuid)", hash, tenant, site).Scan(&membership.Role)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "P2901" || pgErr.Code == "23505") {
			return Membership{}, ErrInvalidInvitation
		}
		return Membership{}, err
	}
	if err = tx.Commit(); err != nil {
		return Membership{}, err
	}
	return membership, nil
}
