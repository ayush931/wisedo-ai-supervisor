// Package oee provides migrated pure domain primitives, contracts, and calculation boundaries.
package oee

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"regexp"
	"sort"
	"time"

	"github.com/google/uuid"
)

var (
	unitRE     = regexp.MustCompile(`^[a-z][a-z0-9._-]*$`)
	sha256RE   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	currencyRE = regexp.MustCompile(`^[A-Z]{3}$`)

	ErrInvalidInterval = errors.New("start must be strictly before end")
	ErrInvalidDigest   = errors.New("invalid SHA-256 digest: expected 64 lowercase hex characters")
	ErrInvalidUnit     = errors.New("invalid unit code: must match ^[a-z][a-z0-9._-]*$ (1-32 chars)")
	ErrInvalidCurrency = errors.New("invalid currency code: must match ISO 4217 ^[A-Z]{3}$")
	ErrNegativeAmount  = errors.New("quantity amount cannot be negative")
	ErrInvalidRatio    = errors.New("invalid ratio: numerator must be >= 0 and denominator > 0")
)

// HalfOpenInterval is an immutable half-open time interval [Start, End) in UTC.
type HalfOpenInterval struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// NewHalfOpenInterval constructs and normalizes a HalfOpenInterval in UTC.
func NewHalfOpenInterval(start, end time.Time) (HalfOpenInterval, error) {
	s := start.UTC()
	e := end.UTC()
	if !s.Before(e) {
		return HalfOpenInterval{}, ErrInvalidInterval
	}
	return HalfOpenInterval{Start: s, End: e}, nil
}

func (h HalfOpenInterval) Duration() time.Duration {
	return h.End.Sub(h.Start)
}

func (h HalfOpenInterval) DurationSeconds() *big.Rat {
	sec := h.End.Unix() - h.Start.Unix()
	ns := int64(h.End.Nanosecond() - h.Start.Nanosecond())
	numerator := new(big.Int).Mul(big.NewInt(sec), big.NewInt(1_000_000_000))
	numerator.Add(numerator, big.NewInt(ns))
	return new(big.Rat).SetFrac(numerator, big.NewInt(1_000_000_000))
}

func (h HalfOpenInterval) Contains(t time.Time) bool {
	utc := t.UTC()
	return !utc.Before(h.Start) && utc.Before(h.End)
}

func (h HalfOpenInterval) Overlaps(other HalfOpenInterval) bool {
	maxStart := h.Start
	if other.Start.After(maxStart) {
		maxStart = other.Start
	}
	minEnd := h.End
	if other.End.Before(minEnd) {
		minEnd = other.End
	}
	return maxStart.Before(minEnd)
}

func (h HalfOpenInterval) IsAdjacent(other HalfOpenInterval) bool {
	return h.End.Equal(other.Start) || other.End.Equal(h.Start)
}

func (h HalfOpenInterval) Intersection(other HalfOpenInterval) (HalfOpenInterval, bool) {
	maxStart := h.Start
	if other.Start.After(maxStart) {
		maxStart = other.Start
	}
	minEnd := h.End
	if other.End.Before(minEnd) {
		minEnd = other.End
	}
	if maxStart.Before(minEnd) {
		return HalfOpenInterval{Start: maxStart, End: minEnd}, true
	}
	return HalfOpenInterval{}, false
}

// SHA256Digest represents a validated 64-character lowercase hex SHA-256 digest.
type SHA256Digest string

func ParseSHA256Digest(s string) (SHA256Digest, error) {
	if !sha256RE.MatchString(s) {
		return "", ErrInvalidDigest
	}
	return SHA256Digest(s), nil
}

func DigestFromBytes(data []byte) SHA256Digest {
	h := sha256.Sum256(data)
	return SHA256Digest(hex.EncodeToString(h[:]))
}

func DigestFromString(text string) SHA256Digest {
	return DigestFromBytes([]byte(text))
}

func (d SHA256Digest) String() string {
	return string(d)
}

// UnitCode represents a canonical unit identifier (1-32 chars).
type UnitCode string

func ParseUnitCode(s string) (UnitCode, error) {
	if len(s) < 1 || len(s) > 32 || !unitRE.MatchString(s) {
		return "", ErrInvalidUnit
	}
	return UnitCode(s), nil
}

func (u UnitCode) String() string {
	return string(u)
}

// CurrencyCode represents an ISO 4217 uppercase 3-letter currency code.
type CurrencyCode string

func ParseCurrencyCode(s string) (CurrencyCode, error) {
	if !currencyRE.MatchString(s) {
		return "", ErrInvalidCurrency
	}
	return CurrencyCode(s), nil
}

func (c CurrencyCode) String() string {
	return string(c)
}

// Quantity represents an immutable physical quantity with a non-negative amount.
type Quantity struct {
	Amount *big.Rat
	Unit   UnitCode
}

func NewQuantity(amount *big.Rat, unit UnitCode) (Quantity, error) {
	if amount == nil || amount.Sign() < 0 {
		return Quantity{}, ErrNegativeAmount
	}
	return Quantity{
		Amount: new(big.Rat).Set(amount),
		Unit:   unit,
	}, nil
}

// Ratio represents an OEE component ratio with exact rational arithmetic.
type Ratio struct {
	Numerator        *big.Rat
	Denominator      *big.Rat
	Unit             UnitCode
	PolicyRevisionID uuid.UUID
}

func NewRatio(num, den *big.Rat, unit UnitCode, policyID uuid.UUID) (Ratio, error) {
	if num == nil || den == nil || num.Sign() < 0 || den.Sign() <= 0 {
		return Ratio{}, ErrInvalidRatio
	}
	return Ratio{
		Numerator:        new(big.Rat).Set(num),
		Denominator:      new(big.Rat).Set(den),
		Unit:             unit,
		PolicyRevisionID: policyID,
	}, nil
}

func (r Ratio) Value() *big.Rat {
	return new(big.Rat).Quo(r.Numerator, r.Denominator)
}

// CanonicalJSONBytes produces deterministic, sorted-key, compact JSON bytes.
func CanonicalJSONBytes(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var canonical any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&canonical); err != nil {
		return nil, err
	}
	return marshalCanonical(canonical)
}

func marshalCanonical(v any) ([]byte, error) {
	switch val := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var buf bytes.Buffer
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			keyBytes, _ := json.Marshal(k)
			buf.Write(keyBytes)
			buf.WriteByte(':')
			childBytes, err := marshalCanonical(val[k])
			if err != nil {
				return nil, err
			}
			buf.Write(childBytes)
		}
		buf.WriteByte('}')
		return buf.Bytes(), nil
	case []any:
		var buf bytes.Buffer
		buf.WriteByte('[')
		for i, item := range val {
			if i > 0 {
				buf.WriteByte(',')
			}
			childBytes, err := marshalCanonical(item)
			if err != nil {
				return nil, err
			}
			buf.Write(childBytes)
		}
		buf.WriteByte(']')
		return buf.Bytes(), nil
	default:
		return json.Marshal(val)
	}
}

// CanonicalSHA256Hex returns the deterministic hex digest of canonical JSON.
func CanonicalSHA256Hex(v any) (string, error) {
	b, err := CanonicalJSONBytes(v)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
