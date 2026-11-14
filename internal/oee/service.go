package oee

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
)

var (
	ErrCalculationUnavailable = errors.New("OEE calculation is unavailable until an approved policy and golden vectors exist")
	ErrPersistenceUnavailable = errors.New("calculation persistence is not configured")
	ErrNaiveTimestamp         = errors.New("evidence timestamp must be timezone-aware")
)

// Calculate fails closed until approved policy rules and golden vectors exist.
func Calculate(dataset DatasetV1, policy PolicyV1) error {
	_ = dataset
	_ = policy
	return ErrCalculationUnavailable
}

// ClassifyLoss fails closed until approved downtime taxonomy exists.

// ErrReasonNotFound is returned when a reason code is absent from the taxonomy.
var ErrReasonNotFound = errors.New("reason code not found in taxonomy")

// ClassifyLossWithTaxonomy resolves a reason code against a taxonomy revision at
// an instant. Draft, retired and out-of-range reasons are never returned: they
// are indistinguishable from an unknown code, so callers keep the fact visible
// as unclassified instead of inventing a classification.
func ClassifyLossWithTaxonomy(reasonCode string, taxonomy TaxonomyRevision, at time.Time) (ReasonNode, error) {
	if err := ValidateTaxonomy(taxonomy); err != nil {
		return ReasonNode{}, err
	}
	if taxonomy.State != ReasonStateEffective && taxonomy.State != ReasonStatePublished {
		return ReasonNode{}, ErrTaxonomyNotEffective
	}
	if !taxonomy.EffectiveWindow.Contains(at) {
		return ReasonNode{}, ErrTaxonomyNotEffective
	}
	for _, n := range taxonomy.Nodes {
		if n.ReasonCode == reasonCode && usableNode(n, at) {
			return n, nil
		}
	}
	return ReasonNode{}, ErrReasonNotFound
}

// ValidateEvidenceTimestamp normalizes timestamp to UTC.
func ValidateEvidenceTimestamp(t time.Time) (time.Time, error) {
	if t.IsZero() {
		return time.Time{}, ErrNaiveTimestamp
	}
	return t.UTC(), nil
}

// The evidence pack patterns mirror internal/contracts (which imports this package, so it
// cannot be imported here); a contracts test keeps them equal.
const (
	EvidenceSHA256Pattern   = `^[0-9a-f]{64}$`
	EvidenceDecimal6Pattern = `^-?[0-9]+(\.[0-9]{1,6})?$`
)

var (
	evidenceSHA256   = regexp.MustCompile(EvidenceSHA256Pattern)
	evidenceDecimal6 = regexp.MustCompile(EvidenceDecimal6Pattern)
	// ErrInvalidEvidencePack means a pack breaks the published EvidencePackV1 contract.
	ErrInvalidEvidencePack = errors.New("invalid evidence pack")
)

// SealEvidence validates a pack against EvidencePackV1 and returns it with its seal time in
// UTC. It fails closed like Calculate: identifiers and digests must be present and
// well-formed, every ratio must be a decimal6 string, status and reason codes must be
// published values, and a READY pack carries all four ratios and no reason codes.
func SealEvidence(pack EvidencePackV1) (EvidencePackV1, error) {
	bad := func(what string) (EvidencePackV1, error) {
		return EvidencePackV1{}, fmt.Errorf("%w: %s", ErrInvalidEvidencePack, what)
	}
	if pack.EvidenceID == uuid.Nil || pack.RunID == uuid.Nil {
		return bad("evidence_id and run_id are required")
	}
	if !evidenceSHA256.MatchString(pack.DatasetDigest) || !evidenceSHA256.MatchString(pack.PolicyDigest) {
		return bad("dataset_digest and policy_digest must be sha256 hex")
	}
	ratios := []string{pack.Availability, pack.Performance, pack.Quality, pack.OEE}
	for _, r := range ratios {
		if r != "" && !evidenceDecimal6.MatchString(r) {
			return bad("ratios must be decimal6 strings")
		}
	}
	knownStatus := false
	for _, s := range Statuses() {
		knownStatus = knownStatus || s == pack.Status
	}
	if !knownStatus {
		return bad("unknown status")
	}
	published := map[ReasonCode]bool{}
	for _, c := range ReasonCodes() {
		published[c] = true
	}
	for _, c := range pack.ReasonCodes {
		if !published[c] {
			return bad("unknown reason code")
		}
	}
	if pack.Status == StatusReady {
		if len(pack.ReasonCodes) != 0 {
			return bad("a READY pack carries no reason codes")
		}
		for _, r := range ratios {
			if r == "" {
				return bad("a READY pack carries every ratio")
			}
		}
	} else if len(pack.ReasonCodes) == 0 {
		return bad("a pack that is not READY names its reasons")
	}
	sealed, err := ValidateEvidenceTimestamp(pack.SealedAtUTC)
	if err != nil {
		return bad("sealed_at_utc is required")
	}
	pack.SealedAtUTC = sealed
	// A copy, so the caller cannot change a sealed pack; an empty list stays [] (not null).
	codes := make([]ReasonCode, len(pack.ReasonCodes))
	copy(codes, pack.ReasonCodes)
	pack.ReasonCodes = codes
	return pack, nil
}
