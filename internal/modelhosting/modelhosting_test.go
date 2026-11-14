package modelhosting

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Decide refuses a malformed decision before it opens a transaction (no database is needed to
// reach these outcomes, and none is touched): turning it on needs a DPA reference, turning it off
// must not carry one, and a reason, key and non-negative expected revision are required.
func TestDecideRefusesInvalidInputBeforeStorage(t *testing.T) {
	s := New(nil)
	a := Principal{Issuer: "https://i.invalid", Subject: "owner", SessionHash: "h"}
	sc := Scope{TenantID: "00000000-0000-4000-8000-000000000001", SiteID: "00000000-0000-4000-8000-000000000002"}
	for name, c := range map[string]struct {
		in       DecisionInput
		key      string
		expected int64
	}{
		"on without a DPA reference":   {DecisionInput{Enabled: true, Reason: "r"}, "k", 0},
		"on with a padded reference":   {DecisionInput{Enabled: true, DPAReference: " DPA ", Reason: "r"}, "k", 0},
		"on with an oversized ref":     {DecisionInput{Enabled: true, DPAReference: strings.Repeat("x", MaxDPAReference+1), Reason: "r"}, "k", 0},
		"off carrying a DPA reference": {DecisionInput{Enabled: false, DPAReference: "DPA", Reason: "r"}, "k", 0},
		"blank reason":                 {DecisionInput{Enabled: false, Reason: "  "}, "k", 0},
		"oversized reason":             {DecisionInput{Enabled: false, Reason: strings.Repeat("é", MaxReason+1)}, "k", 0},
		"missing idempotency key":      {DecisionInput{Enabled: false, Reason: "r"}, "", 0},
		"negative expected revision":   {DecisionInput{Enabled: false, Reason: "r"}, "k", -1},
	} {
		if _, err := s.Decide(context.Background(), a, sc, c.in, c.key, c.expected); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	// A principal without a session or a scope that is not a UUID is denied, also before storage.
	if _, err := s.Get(context.Background(), Principal{}, sc); !errors.Is(err, ErrDenied) {
		t.Errorf("no session: %v", err)
	}
	if _, err := s.Get(context.Background(), a, Scope{TenantID: "t", SiteID: sc.SiteID}); !errors.Is(err, ErrDenied) {
		t.Errorf("bad scope: %v", err)
	}
}
