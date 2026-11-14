package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestBootstrapInvitationFileIsNeverOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invitation.json")
	if err := os.WriteFile(path, []byte("existing private invitation"), 0600); err != nil {
		t.Fatal(err)
	}
	// A production-valid URL (WIS-699 checks it first); it is never dialled.
	t.Setenv("SUPERVISOR_ENVIRONMENT", "production") // CI exports local, whose host rule would refuse this URL first
	t.Setenv("SUPERVISOR_PROVISIONING_URL", "postgresql://unused.invalid/unreachable?sslmode=verify-full")
	t.Setenv("SUPERVISOR_BOOTSTRAP_INVITATION_FILE", path)
	if err := bootstrapInvite(context.Background()); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("existing invitation file must be refused before any database work: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "existing private invitation" {
		t.Fatal("existing private file changed")
	}
	// Missing provisioning authority must not leave a seemingly usable artifact.
	t.Setenv("SUPERVISOR_PROVISIONING_URL", "")
	missing := filepath.Join(t.TempDir(), "missing.json")
	t.Setenv("SUPERVISOR_BOOTSTRAP_INVITATION_FILE", missing)
	if err := bootstrapInvite(context.Background()); err == nil {
		t.Fatal("missing provisioning authority accepted")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("artifact written without provisioning authority")
	}
}
