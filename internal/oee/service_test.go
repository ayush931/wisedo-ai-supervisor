package oee

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestFailClosedBoundaries(t *testing.T) {
	if err := Calculate(DatasetV1{}, PolicyV1{}); err != ErrCalculationUnavailable {
		t.Errorf("expected ErrCalculationUnavailable, got %v", err)
	}

	ts, err := ValidateEvidenceTimestamp(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	if err != nil || ts.IsZero() {
		t.Errorf("valid timestamp failed: %v", err)
	}

	if _, err := ValidateEvidenceTimestamp(time.Time{}); err != ErrNaiveTimestamp {
		t.Errorf("zero timestamp should fail")
	}

	pack := validPack()
	sealed, err := SealEvidence(pack)
	if err != nil || sealed.EvidenceID != pack.EvidenceID || sealed.SealedAtUTC.Location() != time.UTC {
		t.Fatalf("valid pack: %v", err)
	}
}

func validPack() EvidencePackV1 {
	return EvidencePackV1{
		EvidenceID: uuid.New(), RunID: uuid.New(),
		DatasetDigest: strings.Repeat("a", 64), PolicyDigest: strings.Repeat("b", 64),
		Availability: "0.900000", Performance: "0.950000", Quality: "0.990000", OEE: "0.846450",
		Status: StatusReady, ReasonCodes: []ReasonCode{},
		SealedAtUTC: time.Date(2026, 10, 16, 14, 0, 0, 0, time.FixedZone("IST", 19800)),
	}
}

// SealEvidence fails closed on anything outside the EvidencePackV1 contract.
func TestSealEvidenceRejectsInvalidPacks(t *testing.T) {
	for name, change := range map[string]func(*EvidencePackV1){
		"bogus status":            func(p *EvidencePackV1) { p.Status = "BOGUS" },
		"NaN ratio":               func(p *EvidencePackV1) { p.Availability = "NaN" },
		"seven decimals":          func(p *EvidencePackV1) { p.OEE = "0.8464501" },
		"short digest":            func(p *EvidencePackV1) { p.DatasetDigest = "abc" },
		"uppercase digest":        func(p *EvidencePackV1) { p.PolicyDigest = strings.Repeat("B", 64) },
		"nil evidence id":         func(p *EvidencePackV1) { p.EvidenceID = uuid.Nil },
		"nil run id":              func(p *EvidencePackV1) { p.RunID = uuid.Nil },
		"unknown reason":          func(p *EvidencePackV1) { p.Status, p.ReasonCodes = StatusNotReady, []ReasonCode{"MADE_UP"} },
		"ready with reasons":      func(p *EvidencePackV1) { p.ReasonCodes = []ReasonCode{ReasonMissingData} },
		"ready missing a ratio":   func(p *EvidencePackV1) { p.Quality = "" },
		"not ready without cause": func(p *EvidencePackV1) { p.Status, p.OEE = StatusNotReady, "" },
		"no seal time":            func(p *EvidencePackV1) { p.SealedAtUTC = time.Time{} },
	} {
		p := validPack()
		change(&p)
		if _, err := SealEvidence(p); !errors.Is(err, ErrInvalidEvidencePack) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	notReady := validPack()
	notReady.Status, notReady.OEE, notReady.ReasonCodes = StatusNotReady, "", []ReasonCode{ReasonZeroDenominator}
	if _, err := SealEvidence(notReady); err != nil {
		t.Fatalf("a NOT_READY pack with its reason: %v", err)
	}
}
