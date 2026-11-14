package knowledge

import (
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestFreshnessWindow(t *testing.T) {
	at := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		now    time.Time
		reason string
	}{{at, ""}, {at.Add(59 * time.Second), ""}, {at.Add(time.Minute), "document_verification_expired"}, {at.Add(-time.Nanosecond), "confirmation_in_future"}} {
		reason, expiry := freshnessWindow(at, 60, tc.now)
		if reason != tc.reason || expiry == nil || !expiry.Equal(at.Add(time.Minute)) {
			t.Fatalf("reason=%s expiry=%v", reason, expiry)
		}
	}
}
func TestQualifiedRequirementInput(t *testing.T) {
	policy := RequirementDecisionInput{InventoryRevision: 1, Verdict: "bind", Reason: "Explicit policy", FreshnessPolicyRevision: 1}
	if e := policy.Validate(); e != nil {
		t.Fatal(e)
	}
	old := RequirementDecisionInput{InventoryRevision: 1, Verdict: "not_applicable", Reason: "Historical"}
	b, _ := json.Marshal(old)
	if string(b) != `{"inventory_revision":1,"verdict":"not_applicable","reason":"Historical"}` {
		t.Fatalf("legacy fingerprint changed %s", b)
	}
	policy.Verdict = "withdraw"
	if e := policy.Validate(); !errors.Is(e, ErrInvalidContract) {
		t.Fatalf("qualification on withdraw: %v", e)
	}
	structural := RequirementDecisionInput{InventoryRevision: 1, Verdict: "bind", Reason: "Qualified", SnapshotID: uuid.NewString(), AsOf: "2026-09-27T00:00:00Z", ClaimRefs: []ConflictClaimRef{{ClaimID: uuid.NewString(), Revision: 1}}, FreshnessPolicyRevision: 1, VerificationRefs: []VerificationRef{{SourceID: uuid.NewString(), VerificationID: uuid.NewString(), VerificationRevision: 1}}}
	if e := structural.Validate(); e != nil {
		t.Fatal(e)
	}
	structural.VerificationRefs = append(structural.VerificationRefs, structural.VerificationRefs[0])
	if e := structural.Validate(); !errors.Is(e, ErrInvalidContract) {
		t.Fatalf("duplicate verification: %v", e)
	}
}

func TestQualificationClaimTime(t *testing.T) {
	at := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	id := uuid.NewString()
	end := at.Add(time.Minute).Format(time.RFC3339Nano)
	snapshot := Snapshot{Contents: SnapshotContents{Claims: []SnapshotClaim{{ClaimInput: ClaimInput{ClaimID: id, EffectiveFrom: at.Format(time.RFC3339Nano), EffectiveTo: &end}, Revision: 1}}}}
	refs := []ConflictClaimRef{{ClaimID: id, Revision: 1}}
	for _, tc := range []struct {
		now  time.Time
		want string
	}{{at, ""}, {at.Add(time.Minute), "claim_outside_effective_interval"}, {at.Add(-time.Second), "claim_outside_effective_interval"}} {
		got, expiry := qualificationClaimTime(snapshot, refs, tc.now)
		if expiry == nil || !expiry.Equal(at.Add(time.Minute)) {
			t.Fatalf("effective expiry=%v", expiry)
		}
		if got != tc.want {
			t.Fatalf("reason=%s", got)
		}
	}
	refs[0].Revision = 2
	if got, _ := qualificationClaimTime(snapshot, refs, at); got != "claim_outside_effective_interval" {
		t.Fatal("different revision qualified")
	}
}
