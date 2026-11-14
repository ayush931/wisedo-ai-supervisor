package bootstrap

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRejectUnboundedOrUnboundInvitations(t *testing.T) {
	service := Service{}
	valid := IssueRequest{TenantID: "00000000-0000-4000-8000-000000000001", SiteID: "00000000-0000-4000-8000-000000000002", Issuer: "https://identity.example/realm", Subject: "exact-subject", Role: "owner", TTL: 30 * time.Minute}
	for _, change := range []func(*IssueRequest){func(r *IssueRequest) { r.TTL = 0 }, func(r *IssueRequest) { r.TTL = 25 * time.Hour }, func(r *IssueRequest) { r.Subject = "" }, func(r *IssueRequest) { r.TenantID = "bad" }, func(r *IssueRequest) { r.Role = "operator" }} {
		request := valid
		change(&request)
		if _, err := service.Issue(context.Background(), request); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid request reached database: %v", err)
		}
	}
}
