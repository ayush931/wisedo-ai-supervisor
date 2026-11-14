package membership

import (
	"strings"
	"testing"
)

func TestValidationRejectsSelfMutationsAndMalformedAuthority(t *testing.T) {
	a := Actor{Issuer: "https://identity.example/realm", Subject: "owner", SessionHash: strings.Repeat("a", 64)}
	r := Request{TenantID: "00000000-0000-4000-8000-000000000001", SiteID: "00000000-0000-4000-8000-000000000002", TargetIssuer: a.Issuer, TargetSubject: "operator", Role: "viewer", Active: true, IdempotencyKey: "key"}
	if err := validate(a, r); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Request){
		func(r *Request) { r.TargetSubject = a.Subject }, func(r *Request) { r.TargetIssuer = "https://other.example" },
		func(r *Request) { r.Role = "admin" }, func(r *Request) { r.ExpectedRevision = -1 },
		func(r *Request) { r.Active = false }, func(r *Request) { r.IdempotencyKey = strings.Repeat("x", 129) },
		func(r *Request) { r.TargetSubject = "null\x00byte" },
	} {
		copy := r
		mutate(&copy)
		if err := validate(a, copy); err != ErrInvalid {
			t.Fatalf("invalid mutation accepted: %v", err)
		}
	}
	a.SessionHash = "forged"
	if err := validate(a, r); err != ErrDenied {
		t.Fatalf("invalid actor accepted: %v", err)
	}
}
