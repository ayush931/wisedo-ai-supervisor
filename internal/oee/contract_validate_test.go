package oee

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Consumer fixtures: every valid_* request decodes, every invalid_* request is refused.
// The same files drive the HTTP route test, so runtime and wire behavior cannot diverge.
func TestCalculationRequestFixtures(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "contract-fixtures", "*_*.json"))
	if err != nil || len(files) < 10 {
		t.Fatalf("fixtures missing: %v %v", files, err)
	}
	checked := 0
	for _, path := range files {
		name := filepath.Base(path)
		if strings.HasPrefix(name, "provider_") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = DecodeCalculationRequest(data)
		switch {
		case strings.HasPrefix(name, "valid_") && err != nil:
			t.Errorf("%s: %v", name, err)
		case strings.HasPrefix(name, "invalid_") && !errors.Is(err, ErrInvalidContract):
			t.Errorf("%s accepted: %v", name, err)
		}
		checked++
	}
	if checked < 10 {
		t.Fatalf("only %d request fixtures checked", checked)
	}
}

// Provider fixture: a READY evidence pack in the published shape seals unchanged.
func TestProviderEvidencePackFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "contract-fixtures", "provider_evidence_pack_ready.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pack EvidencePackV1
	if err = json.Unmarshal(data, &pack); err != nil {
		t.Fatal(err)
	}
	if err = ValidateContract(pack); err != nil {
		t.Fatalf("provider fixture breaks the contract: %v", err)
	}
	sealed, err := SealEvidence(pack)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(sealed)
	if strings.TrimSpace(string(data)) != string(out) {
		t.Fatalf("sealing changed the pack:\n%s\n%s", data, out)
	}
}

func TestValidateContractRules(t *testing.T) {
	good := EvidencePackV1{
		EvidenceID: uuid.New(), RunID: uuid.New(), DatasetDigest: strings.Repeat("a", 64), PolicyDigest: strings.Repeat("b", 64),
		OEE: "0.5", Status: StatusLimited, ReasonCodes: []ReasonCode{ReasonPerformanceOvershoot}, SealedAtUTC: time.Now(),
	}
	if err := ValidateContract(good); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*EvidencePackV1){
		"nil evidence id": func(p *EvidencePackV1) { p.EvidenceID = uuid.Nil },
		"short digest":    func(p *EvidencePackV1) { p.DatasetDigest = "abc" },
		"seven decimals":  func(p *EvidencePackV1) { p.OEE = "0.1234567" },
		"NaN ratio":       func(p *EvidencePackV1) { p.Quality = "NaN" },
		"unknown status":  func(p *EvidencePackV1) { p.Status = "BOGUS" },
		"unknown reason":  func(p *EvidencePackV1) { p.ReasonCodes = []ReasonCode{"MADE_UP"} },
	} {
		p := good
		change(&p)
		if err := ValidateContract(p); !errors.Is(err, ErrInvalidContract) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := ValidateContract(DatasetV1{DatasetID: uuid.New(), Digest: "x"}); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("dataset with a bad digest: %v", err)
	}
	if err := ValidateContract("not a struct"); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("non-struct: %v", err)
	}
}

// Sealed evidence cannot be changed behind its digest by mutating the caller's slice
// (WIS-225 F5).
func TestSealedEvidenceIsNotAliased(t *testing.T) {
	codes := []ReasonCode{ReasonZeroDenominator}
	pack := EvidencePackV1{
		EvidenceID: uuid.New(), RunID: uuid.New(), DatasetDigest: strings.Repeat("a", 64), PolicyDigest: strings.Repeat("b", 64),
		Status: StatusNotReady, ReasonCodes: codes, SealedAtUTC: time.Now(),
	}
	sealed, err := SealEvidence(pack)
	if err != nil {
		t.Fatal(err)
	}
	codes[0] = ReasonMissingData
	if sealed.ReasonCodes[0] != ReasonZeroDenominator {
		t.Fatal("mutating the caller's slice changed the sealed pack")
	}
}
