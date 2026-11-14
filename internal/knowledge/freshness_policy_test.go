package knowledge

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func TestFreshnessPolicyContractBounds(t *testing.T) {
	maxSeconds := int64(math.MaxInt64) / int64(time.Second)
	for _, seconds := range []int64{1, maxSeconds} {
		in := FreshnessPolicyInput{Verdict: "define", MaxAgeSeconds: &seconds, Reason: "Owner-defined document age"}
		if err := in.Validate(); err != nil {
			t.Fatalf("valid max age %d: %v", seconds, err)
		}
		if time.Duration(seconds)*time.Second <= 0 {
			t.Fatal("accepted duration overflow")
		}
	}
	for _, seconds := range []int64{-1, 0, maxSeconds + 1, math.MaxInt64} {
		in := FreshnessPolicyInput{Verdict: "define", MaxAgeSeconds: &seconds, Reason: "Invalid bound"}
		if err := in.Validate(); !errors.Is(err, ErrInvalidContract) {
			t.Fatalf("invalid max age %d: %v", seconds, err)
		}
	}
	one := int64(1)
	invalid := []FreshnessPolicyInput{
		{Verdict: "define", Reason: "Missing age"},
		{Verdict: "withdraw", MaxAgeSeconds: &one, Reason: "Withdrawal cannot set age"},
		{Verdict: "approve", MaxAgeSeconds: &one, Reason: "Wrong decision"},
		{Verdict: "withdraw", Reason: " \t\n"},
		{Verdict: "withdraw", Reason: strings.Repeat("界", 1001)},
		{Verdict: "withdraw", Reason: "reason\x00suffix"},
	}
	for _, in := range invalid {
		if err := in.Validate(); !errors.Is(err, ErrInvalidContract) {
			t.Fatalf("accepted invalid contract %+v: %v", in, err)
		}
	}
	valid := FreshnessPolicyInput{Verdict: "withdraw", Reason: strings.Repeat("界", 1000)}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid bounded Unicode reason: %v", err)
	}
	var absent *FreshnessPolicyInput
	if err := absent.Validate(); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("nil input: %v", err)
	}
}
