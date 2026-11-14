package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"os"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/bootstrap"
)

// Provisioning uses an explicitly supplied administrator connection, never the
// API runtime credentials. The invitation is written once to a private file.
func bootstrapInvite(ctx context.Context) error {
	dsn := os.Getenv("SUPERVISOR_PROVISIONING_URL")
	path := os.Getenv("SUPERVISOR_BOOTSTRAP_INVITATION_FILE")
	if dsn == "" || path == "" {
		return errors.New("provisioning connection and private output file required")
	}
	if err := config.ValidateAdminDSN("SUPERVISOR_PROVISIONING_URL", dsn); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		_ = f.Close()
		if !complete {
			_ = os.Remove(path)
		}
	}()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	service := bootstrap.Service{DB: db}
	invitation, err := service.Issue(ctx, bootstrap.IssueRequest{
		TenantID: os.Getenv("SUPERVISOR_BOOTSTRAP_TENANT_ID"),
		SiteID:   os.Getenv("SUPERVISOR_BOOTSTRAP_SITE_ID"),
		Issuer:   os.Getenv("SUPERVISOR_OIDC_ISSUER"),
		Subject:  os.Getenv("SUPERVISOR_BOOTSTRAP_SUBJECT"),
		Role:     "owner", TTL: 15 * time.Minute,
	})
	if err != nil {
		return err
	}
	if err = json.NewEncoder(f).Encode(invitation); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}
