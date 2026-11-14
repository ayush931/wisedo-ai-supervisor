package ingestion

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testBatch() Batch {
	return Batch{SchemaVersion: 1, SourceID: uuid.NewString(), SourceRevision: 1, MappingRevision: 1, Events: []Event{{ExternalKey: "press-1", Revision: 1, Kind: "machine_state", SourceSequence: 1, EventAt: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC), Payload: json.RawMessage(`{"state":"running"}`), AssetExternalKey: "press-1"}}}
}
func TestBatchContractAndCanonicalPayload(t *testing.T) {
	b := testBatch()
	_, one, err := canonicalBatch(b)
	if err != nil {
		t.Fatal(err)
	}
	b.Events[0].Payload = json.RawMessage("{ \"state\" : \"running\" }")
	_, two, err := canonicalBatch(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(one) != string(two) {
		t.Fatalf("payload whitespace changed canonical envelope: %s != %s", one, two)
	}
	b = testBatch()
	b.Events[0].Payload = json.RawMessage(`{"n":123456789012345678901234567890.123456789}`)
	_, large, err := canonicalBatch(b)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(large) || !strings.Contains(string(large), "123456789012345678901234567890.123456789") {
		t.Fatalf("canonical payload lost exact large-number precision: %s", large)
	}
}
func TestBatchRejectsNonObjectAndMalformedPayload(t *testing.T) {
	for _, payload := range []string{"null", "[]", "1", "{\"x\":1,\"x\":2}", "{\"x\":\"\\ud800\"}"} {
		b := testBatch()
		b.Events[0].Payload = json.RawMessage(payload)
		if err := b.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("payload %q accepted: %v", payload, err)
		}
	}
}
func TestBatchRequiresContiguousSequences(t *testing.T) {
	b := testBatch()
	b.Events = append(b.Events, b.Events[0])
	b.Events[1].SourceSequence = 3
	if err := b.Validate(); !errors.Is(err, ErrSequence) {
		t.Fatalf("gap validation = %v", err)
	}
}

func TestScopeAllowsKindMatchesWholeKindTokens(t *testing.T) {
	for scope, want := range map[string]bool{
		"production_plan":                      true,
		"production_plan:order-7":              true,
		"machine_state, production_plan:press": true,
		"machine_state production_plan":        true,
		"production_delta":                     false,
		"production_plan_v2":                   false,
		"machine_state:production_plan":        false,
		"":                                     false,
	} {
		if got := scopeAllowsKind(scope, "production_plan"); got != want {
			t.Errorf("scopeAllowsKind(%q)=%v want %v", scope, got, want)
		}
	}
}

// Offset spellings of one instant are stored as one canonical payload with one digest.
func TestCanonicalBatchNormalizesPayloadTimes(t *testing.T) {
	batch := func(start string) Batch {
		return Batch{SchemaVersion: 1, SourceID: "0a0a0a0a-0a0a-4a0a-8a0a-0a0a0a0a0a0a", SourceRevision: 1, MappingRevision: 1, Events: []Event{{
			ExternalKey: "d-1", Revision: 1, Kind: "production_delta", SourceSequence: 1, EventAt: time.Date(2026, 9, 27, 0, 15, 0, 0, time.UTC),
			Payload: json.RawMessage(`{"interval_start":"` + start + `","interval_end":"2026-09-27T00:15:00Z","good_units":"5"}`),
		}}}
	}
	utc, _, err := canonicalBatch(batch("2026-09-27T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	ist, _, err := canonicalBatch(batch("2026-09-27T05:30:00+05:30"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ist.Events[0].Payload), `"interval_start":"2026-09-27T00:00:00Z"`) {
		t.Fatalf("payload not normalized: %s", ist.Events[0].Payload)
	}
	if eventDigest(utc.Events[0]) != eventDigest(ist.Events[0]) {
		t.Fatal("two spellings of one instant have different content digests")
	}
}
