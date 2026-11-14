package oee

import (
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestHalfOpenInterval(t *testing.T) {
	t0 := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 9, 30, 0, 15, 0, 0, time.UTC)
	t2 := time.Date(2026, 9, 30, 0, 30, 0, 0, time.UTC)

	iv, err := NewHalfOpenInterval(t0, t1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if iv.Duration() != 15*time.Minute {
		t.Fatalf("expected 15m duration, got %v", iv.Duration())
	}
	if iv.DurationSeconds().Cmp(big.NewRat(900, 1)) != 0 {
		t.Fatalf("expected 900 seconds, got %v", iv.DurationSeconds())
	}

	// Contains
	if !iv.Contains(t0) {
		t.Errorf("expected interval to contain start")
	}
	if iv.Contains(t1) {
		t.Errorf("expected interval NOT to contain end (half-open)")
	}
	if !iv.Contains(time.Date(2026, 9, 30, 0, 7, 30, 0, time.UTC)) {
		t.Errorf("expected interval to contain midpoint")
	}

	// Overlaps and Adjacent
	iv2, _ := NewHalfOpenInterval(t1, t2)
	if iv.Overlaps(iv2) {
		t.Errorf("adjacent intervals must not overlap")
	}
	if !iv.IsAdjacent(iv2) {
		t.Errorf("intervals sharing boundary must be adjacent")
	}

	iv3, _ := NewHalfOpenInterval(time.Date(2026, 9, 30, 0, 10, 0, 0, time.UTC), t2)
	if !iv.Overlaps(iv3) {
		t.Errorf("overlapping intervals must overlap")
	}
	intersect, ok := iv.Intersection(iv3)
	if !ok || !intersect.Start.Equal(time.Date(2026, 9, 30, 0, 10, 0, 0, time.UTC)) || !intersect.End.Equal(t1) {
		t.Errorf("wrong intersection: %v", intersect)
	}

	// Reversed interval fails
	if _, err := NewHalfOpenInterval(t1, t0); err != ErrInvalidInterval {
		t.Errorf("expected ErrInvalidInterval for reversed interval, got %v", err)
	}
}

func TestSHA256Digest(t *testing.T) {
	d := DigestFromString("hello world")
	if len(d) != 64 {
		t.Fatalf("expected 64 chars digest, got %d", len(d))
	}
	parsed, err := ParseSHA256Digest(string(d))
	if err != nil || parsed != d {
		t.Fatalf("expected successful parse, got %v", err)
	}
	if _, err := ParseSHA256Digest("invalid-digest"); err != ErrInvalidDigest {
		t.Fatalf("expected ErrInvalidDigest, got %v", err)
	}
}

func TestUnitAndCurrencyCode(t *testing.T) {
	u, err := ParseUnitCode("parts_per_min")
	if err != nil || u.String() != "parts_per_min" {
		t.Fatalf("valid unit failed: %v", err)
	}
	if _, err := ParseUnitCode("1invalid"); err != ErrInvalidUnit {
		t.Fatalf("expected ErrInvalidUnit for invalid unit, got %v", err)
	}

	c, err := ParseCurrencyCode("USD")
	if err != nil || c.String() != "USD" {
		t.Fatalf("valid currency failed: %v", err)
	}
	if _, err := ParseCurrencyCode("usd"); err != ErrInvalidCurrency {
		t.Fatalf("lowercase currency must fail: %v", err)
	}
}

func TestQuantityAndRatio(t *testing.T) {
	u, _ := ParseUnitCode("count")
	q, err := NewQuantity(big.NewRat(10, 1), u)
	if err != nil || q.Amount.Cmp(big.NewRat(10, 1)) != 0 {
		t.Fatalf("valid quantity failed: %v", err)
	}
	if _, err := NewQuantity(big.NewRat(-1, 1), u); err != ErrNegativeAmount {
		t.Fatalf("negative quantity should fail")
	}

	polID := uuid.New()
	ratio, err := NewRatio(big.NewRat(95, 1), big.NewRat(100, 1), u, polID)
	if err != nil {
		t.Fatalf("valid ratio failed: %v", err)
	}
	if ratio.Value().Cmp(big.NewRat(19, 20)) != 0 {
		t.Fatalf("ratio value calculation failed: %v", ratio.Value())
	}
	if _, err := NewRatio(big.NewRat(1, 1), big.NewRat(0, 1), u, polID); err != ErrInvalidRatio {
		t.Fatalf("zero denominator must fail")
	}
}

func TestCanonicalJSONAndSHA256(t *testing.T) {
	m1 := map[string]any{"b": 2, "a": 1}
	m2 := map[string]any{"a": 1, "b": 2}

	b1, err1 := CanonicalJSONBytes(m1)
	b2, err2 := CanonicalJSONBytes(m2)
	if err1 != nil || err2 != nil || string(b1) != string(b2) {
		t.Fatalf("canonical JSON bytes differed: %s vs %s", b1, b2)
	}

	h1, _ := CanonicalSHA256Hex(m1)
	h2, _ := CanonicalSHA256Hex(m2)
	if h1 != h2 {
		t.Fatalf("canonical hashes differed")
	}
}
