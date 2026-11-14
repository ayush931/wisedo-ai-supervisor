package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"io"
	"net"
	"os"
	"os/user"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/bootstrap"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/text/unicode/norm"
)

var (
	errProvisionInvalidRequest   = errors.New("invalid provision request")
	errProvisionRuntimeRole      = errors.New("provision refuses supervisor_runtime; use the privileged provisioning role")
	errProvisionRole             = errors.New("provision requires supervisor_migrator")
	errProvisionRequestConflict  = errors.New("request ID is already used for a different request")
	errProvisionInvitationIssued = errors.New("invitation already issued for this request ID")
	errTenantAlreadyDeactivated  = errors.New("tenant is already deactivated")
	errTenantUnknown             = errors.New("tenant is unknown")
	errTenantNotProvisioned      = errors.New("tenant has no provisioning audit")
	errTenantDeactivationReplay  = errors.New("tenant deactivation already recorded for this request")
)

type provisionRequest struct {
	TenantName   string `json:"tenant_name"`
	SiteName     string `json:"site_name"`
	Timezone     string `json:"timezone"`
	OwnerIssuer  string `json:"owner_issuer"`
	OwnerSubject string `json:"owner_subject"`
	Operator     string `json:"operator"`
	OSUser       string `json:"os_user"`
}

type provisionDigestRequest struct {
	TenantName   string `json:"tenant_name"`
	SiteName     string `json:"site_name"`
	Timezone     string `json:"timezone"`
	OwnerIssuer  string `json:"owner_issuer"`
	OwnerSubject string `json:"owner_subject"`
	RequestID    string `json:"request_id"`
}

func validateTimezone(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || value == "Local" || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." || part == "." || part == "" {
			return false
		}
	}
	_, err := time.LoadLocation(value)
	return err == nil
}

func validProvisionRequest(r provisionRequest) bool {
	for _, value := range []string{r.TenantName, r.SiteName, r.Operator, r.OSUser} {
		if !validProvisionText(value) {
			return false
		}
	}
	if !validateTimezone(r.Timezone) {
		return false
	}
	return bootstrap.ValidateIssueRequest(bootstrap.IssueRequest{
		TenantID: uuid.NewString(), SiteID: uuid.NewString(), Issuer: r.OwnerIssuer, Subject: r.OwnerSubject, Role: "owner", TTL: 15 * time.Minute,
	}) == nil
}

func validProvisionText(value string) bool {
	return value != "" && utf8.ValidString(value) && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) < 0 && len(value) <= 255
}

func provision(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("provision", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	tenantName := flags.String("tenant-name", "", "customer tenant name")
	siteName := flags.String("site-name", "", "plant site name")
	timezone := flags.String("timezone", "", "site IANA timezone")
	ownerIssuer := flags.String("owner-issuer", "", "exact owner OIDC issuer")
	ownerSubject := flags.String("owner-subject", "", "exact owner OIDC subject")
	operator := flags.String("operator", "", "WiseDo operator identity")
	requestID := flags.String("request-id", "", "idempotency UUID")
	output := flags.String("output", "", "new private file for the invitation token")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errProvisionInvalidRequest
	}
	parsedRequestID, err := uuid.Parse(*requestID)
	if err != nil || parsedRequestID == uuid.Nil || *output == "" {
		return errProvisionInvalidRequest
	}
	currentUser, err := user.Current()
	if err != nil || currentUser.Username == "" {
		return errProvisionInvalidRequest
	}
	request := provisionRequest{
		TenantName: norm.NFC.String(*tenantName), SiteName: norm.NFC.String(*siteName), Timezone: *timezone,
		OwnerIssuer: *ownerIssuer, OwnerSubject: *ownerSubject, Operator: norm.NFC.String(*operator), OSUser: currentUser.Username,
	}
	if !validProvisionRequest(request) {
		return errProvisionInvalidRequest
	}
	dsn := os.Getenv("SUPERVISOR_PROVISIONING_URL")
	if err := config.ValidateAdminDSN("SUPERVISOR_PROVISIONING_URL", dsn); err != nil {
		return err
	}
	requestIDText := parsedRequestID.String()
	_, requestDigest, err := canonical.Digest("SupervisorProvisionRequestV1", 1, provisionDigestRequest{
		TenantName: request.TenantName, SiteName: request.SiteName, Timezone: request.Timezone,
		OwnerIssuer: request.OwnerIssuer, OwnerSubject: request.OwnerSubject, RequestID: requestIDText,
	})
	if err != nil {
		return errProvisionInvalidRequest
	}

	file, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	keepOutput := false
	defer func() {
		_ = file.Close()
		if !keepOutput {
			_ = os.Remove(*output)
		}
	}()
	if err = file.Chmod(0600); err != nil {
		return err
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var role string
	if err = db.QueryRowContext(ctx, "SELECT current_user").Scan(&role); err != nil {
		return err
	}
	if role != "supervisor_migrator" {
		if role == "supervisor_runtime" {
			return errProvisionRuntimeRole
		}
		return errProvisionRole
	}

	if err = refuseExistingProvision(ctx, db, requestIDText, requestDigest); err != nil {
		return err
	}
	tenantID, siteID := uuid.NewString(), uuid.NewString()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true),set_config('lock_timeout','5s',true)`, tenantID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.tenants(id,name) VALUES($1,$2)`, tenantID, request.TenantName); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.sites(tenant_id,id,name,timezone) VALUES($1,$2,$3,$4)`, tenantID, siteID, request.SiteName, request.Timezone); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.provisioning_audit(request_id,tenant_id,site_id,operator,os_user,request_sha256) VALUES($1,$2,$3,$4,$5,$6)`, requestIDText, tenantID, siteID, request.Operator, request.OSUser, requestDigest); err != nil {
		_ = tx.Rollback()
		if sqlState(err) == "23505" {
			return refuseExistingProvision(ctx, db, requestIDText, requestDigest)
		}
		return err
	}
	invitation, err := (bootstrap.Service{DB: db}).IssueTx(ctx, tx, bootstrap.IssueRequest{
		TenantID: tenantID, SiteID: siteID, Issuer: request.OwnerIssuer, Subject: request.OwnerSubject, Role: "owner", TTL: 15 * time.Minute,
	})
	if err != nil {
		return err
	}
	if _, err = io.WriteString(file, invitation.Token+"\n"); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	// Keep the file if Commit returns an error because the database result may be unknown.
	keepOutput = true
	if err = tx.Commit(); err != nil {
		return err
	}
	fmt.Printf("tenant_id=%s\nsite_id=%s\n", tenantID, siteID)
	return nil
}

type provisionDeactivateDigestRequest struct {
	TenantID  string `json:"tenant_id"`
	RequestID string `json:"request_id"`
}

func provisionDeactivate(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("provision deactivate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	tenant := flags.String("tenant", "", "tenant UUID")
	operator := flags.String("operator", "", "WiseDo operator identity")
	requestID := flags.String("request-id", "", "idempotency UUID")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errProvisionInvalidRequest
	}
	tenantID, err := uuid.Parse(*tenant)
	if err != nil || tenantID == uuid.Nil {
		return errProvisionInvalidRequest
	}
	idempotencyID, err := uuid.Parse(*requestID)
	if err != nil || idempotencyID == uuid.Nil {
		return errProvisionInvalidRequest
	}
	operatorName := norm.NFC.String(*operator)
	currentUser, err := user.Current()
	if err != nil || !validProvisionText(currentUser.Username) || !validProvisionText(operatorName) {
		return errProvisionInvalidRequest
	}
	dsn := os.Getenv("SUPERVISOR_PROVISIONING_URL")
	if err := config.ValidateAdminDSN("SUPERVISOR_PROVISIONING_URL", dsn); err != nil {
		return err
	}
	requestIDText := idempotencyID.String()
	_, requestDigest, err := canonical.Digest("SupervisorTenantDeactivateRequestV1", 1, provisionDeactivateDigestRequest{TenantID: tenantID.String(), RequestID: requestIDText})
	if err != nil {
		return errProvisionInvalidRequest
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var role string
	if err = db.QueryRowContext(ctx, "SELECT current_user").Scan(&role); err != nil {
		return err
	}
	if role != "supervisor_migrator" {
		if role == "supervisor_runtime" {
			return errProvisionRuntimeRole
		}
		return errProvisionRole
	}
	if err = refuseExistingDeactivation(ctx, db, requestIDText, requestDigest); errors.Is(err, errTenantDeactivationReplay) {
		fmt.Printf("tenant_id=%s deactivated\n", tenantID.String())
		return nil
	} else if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true),set_config('lock_timeout','5s',true)`, tenantID.String()); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE supervisor.tenants SET deactivated_at=clock_timestamp() WHERE id=$1 AND deactivated_at IS NULL`, tenantID.String())
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 0 {
		var deactivatedAt sql.NullTime
		err = tx.QueryRowContext(ctx, `SELECT deactivated_at FROM supervisor.tenants WHERE id=$1`, tenantID.String()).Scan(&deactivatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return errTenantUnknown
		}
		if err != nil {
			return err
		}
		if deactivatedAt.Valid {
			_ = tx.Rollback()
			if err = refuseExistingDeactivation(ctx, db, requestIDText, requestDigest); errors.Is(err, errTenantDeactivationReplay) {
				fmt.Printf("tenant_id=%s deactivated\n", tenantID.String())
				return nil
			} else if err != nil {
				return err
			}
		}
		return errTenantAlreadyDeactivated
	}
	var siteID string
	err = tx.QueryRowContext(ctx, `SELECT site_id::text FROM supervisor.provisioning_audit WHERE tenant_id=$1 AND action='tenant.provisioned' ORDER BY created_at LIMIT 1`, tenantID.String()).Scan(&siteID)
	if errors.Is(err, sql.ErrNoRows) {
		return errTenantNotProvisioned
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.provisioning_audit(request_id,tenant_id,site_id,operator,os_user,request_sha256,action) VALUES($1,$2,$3,$4,$5,$6,'tenant.deactivated')`, requestIDText, tenantID.String(), siteID, operatorName, currentUser.Username, requestDigest); err != nil {
		_ = tx.Rollback()
		if sqlState(err) == "23505" {
			if replayErr := refuseExistingDeactivation(ctx, db, requestIDText, requestDigest); errors.Is(replayErr, errTenantDeactivationReplay) {
				fmt.Printf("tenant_id=%s deactivated\n", tenantID.String())
				return nil
			} else {
				return replayErr
			}
		}
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	fmt.Printf("tenant_id=%s deactivated\n", tenantID.String())
	return nil
}

func refuseExistingDeactivation(ctx context.Context, db *sql.DB, requestID, requestDigest string) error {
	var priorDigest, action string
	err := db.QueryRowContext(ctx, `SELECT request_sha256,action FROM supervisor.provisioning_audit WHERE request_id=$1`, requestID).Scan(&priorDigest, &action)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if priorDigest != requestDigest || action != "tenant.deactivated" {
		return errProvisionRequestConflict
	}
	return errTenantDeactivationReplay
}

func refuseExistingProvision(ctx context.Context, db *sql.DB, requestID, requestDigest string) error {
	var priorDigest string
	err := db.QueryRowContext(ctx, `SELECT request_sha256 FROM supervisor.provisioning_audit WHERE request_id=$1`, requestID).Scan(&priorDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if priorDigest != requestDigest {
		return errProvisionRequestConflict
	}
	return errProvisionInvitationIssued
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func safeProvisionError(err error) string {
	switch {
	case errors.Is(err, errProvisionInvalidRequest):
		return "invalid provision arguments; names, IANA timezone, owner identity, operator, request ID UUID and output are required"
	case errors.Is(err, errProvisionRuntimeRole), errors.Is(err, errProvisionRole), errors.Is(err, errProvisionRequestConflict), errors.Is(err, errProvisionInvitationIssued), errors.Is(err, errTenantAlreadyDeactivated), errors.Is(err, errTenantUnknown), errors.Is(err, errTenantNotProvisioned):
		return err.Error()
	case errors.Is(err, os.ErrExist):
		return "invitation output already exists; refusing to overwrite"
	}
	var dsnErr *config.AdminDSNError
	if errors.As(err, &dsnErr) {
		return dsnErr.Error()
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return "provision output file unavailable: " + pathErr.Err.Error()
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return "provision database failure (SQLSTATE " + pgErr.Code + ")"
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return "provision database network failure: " + netErr.Op + " " + netErr.Net
	}
	return fmt.Sprintf("provision failed (%T)", err)
}
