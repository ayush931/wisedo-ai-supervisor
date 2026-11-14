package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/google/uuid"
)

func TestProvisionValidatesIANATimezone(t *testing.T) {
	if !validateTimezone("America/Detroit") || !validateTimezone("UTC") {
		t.Fatal("valid IANA timezone rejected")
	}
	for _, zone := range []string{"", "Local", "../etc/passwd", "/etc/localtime", "America/Detroit/.."} {
		if validateTimezone(zone) {
			t.Errorf("invalid timezone accepted: %q", zone)
		}
	}
}

func TestProvisionDigestHasPinnedCanonicalRequest(t *testing.T) {
	request := provisionDigestRequest{
		TenantName: "WiseDo Manufacturing", SiteName: "Detroit Plant", Timezone: "America/Detroit",
		OwnerIssuer: "https://identity.example/realm", OwnerSubject: "owner-subject-123",
		RequestID: "11111111-2222-4333-8444-555555555555",
	}
	_, digest, err := canonical.Digest("SupervisorProvisionRequestV1", 1, request)
	if err != nil {
		t.Fatal(err)
	}
	const want = "a75439506036d8b1c81ba31053ae4fc571ea9a89edc6ad919f442aa16a7b2de4"
	if digest != want {
		t.Fatalf("provision request digest=%s, want pinned %s", digest, want)
	}
}

func TestProvisionRejectsControlCharactersInNames(t *testing.T) {
	valid := provisionRequest{
		TenantName: "Customer", SiteName: "Plant", Timezone: "UTC",
		OwnerIssuer: "https://identity.example/realm", OwnerSubject: "owner-123",
		Operator: "WiseDo operations", OSUser: "operator",
	}
	if !validProvisionRequest(valid) {
		t.Fatal("valid request rejected")
	}
	for field, change := range map[string]func(*provisionRequest){
		"tenant":   func(r *provisionRequest) { r.TenantName = "Customer\nEast" },
		"site":     func(r *provisionRequest) { r.SiteName = "Plant\x7f" },
		"operator": func(r *provisionRequest) { r.Operator = "WiseDo\toperations" },
	} {
		request := valid
		change(&request)
		if validProvisionRequest(request) {
			t.Errorf("%s control character accepted", field)
		}
	}
}

func TestProvisionRefusesExistingOutputBeforeDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invitation.token")
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	// A production-valid URL (WIS-699 checks it first); it is never dialled.
	t.Setenv("SUPERVISOR_ENVIRONMENT", "production") // CI exports local, whose host rule would refuse this URL first
	t.Setenv("SUPERVISOR_PROVISIONING_URL", "postgresql://unused.invalid/supervisor?sslmode=verify-full")
	args := []string{"--tenant-name", "Customer", "--site-name", "Plant", "--timezone", "UTC", "--owner-issuer", "https://identity.example/realm", "--owner-subject", "owner-123", "--operator", "operator", "--request-id", uuid.NewString(), "--output", path}
	err := provision(context.Background(), args)
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing output accepted or DB touched first: %v", err)
	}
	if !strings.Contains(safeProvisionError(err), "already exists") {
		t.Fatalf("existing output error was not actionable: %s", safeProvisionError(err))
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "keep" {
		t.Fatal("existing output changed")
	}
}
