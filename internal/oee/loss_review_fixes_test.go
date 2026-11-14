package oee

import (
	"errors"
	"math/big"
	"testing"
)

// ADR 0005 Sec. 11 and the kernel's unit rule: lost quantities are normalized to pieces before
// they are summed, and an empty or unknown unit fails instead of being guessed.
func TestLossQuantitiesAreNormalizedToPieces(t *testing.T) {
	f := newLossFixture(t)
	f.in.SpeedLossEvents = nil
	f.in.QualityLossEvents = []RawQualityLossFact{
		{EventID: "evt-q-dozen", ReasonCode: "SCRAP", LostQuantity: big.NewRat(1, 1), Unit: "dozen"},
		{EventID: "evt-q-pcs", ReasonCode: "SCRAP", LostQuantity: big.NewRat(1, 1), Unit: "pcs"},
	}
	res, err := ExplainLosses(f.in)
	if err != nil {
		t.Fatal(err)
	}
	if res.LossTree.TotalLostQuantity.Cmp(big.NewRat(13, 1)) != 0 {
		t.Fatalf("1 dozen + 1 pcs = %s, want 13 pieces", res.LossTree.TotalLostQuantity.RatString())
	}
	for name, unit := range map[string]UnitCode{"unknown unit": "NOT A UNIT", "empty unit": ""} {
		bad := f.in
		bad.QualityLossEvents = []RawQualityLossFact{{EventID: "evt-q", ReasonCode: "SCRAP", LostQuantity: big.NewRat(1, 1), Unit: unit}}
		if _, err := ExplainLosses(bad); !errors.Is(err, ErrUnsupportedUnit) {
			t.Errorf("quality %s: %v", name, err)
		}
		badSpeed := f.in
		badSpeed.SpeedLossEvents = []RawSpeedLossFact{{EventID: "evt-s", ReasonCode: "SPEED_LOSS", DurationSeconds: big.NewRat(60, 1), LostQuantity: big.NewRat(5, 1), Unit: unit}}
		if _, err := ExplainLosses(badSpeed); !errors.Is(err, ErrUnsupportedUnit) {
			t.Errorf("speed %s: %v", name, err)
		}
	}
	// A speed loss with no quantity is a time-only contribution and needs no unit.
	timeOnly := f.in
	timeOnly.SpeedLossEvents = []RawSpeedLossFact{{EventID: "evt-s", ReasonCode: "SPEED_LOSS", DurationSeconds: big.NewRat(60, 1)}}
	if _, err := ExplainLosses(timeOnly); err != nil {
		t.Fatalf("time-only speed loss: %v", err)
	}
}

// Rule 06: unclassified stays visible. A contribution that kept its code but fell back to
// UNCLASSIFIED never merges into the classified row of the same code.
func TestParetoKeepsUnclassifiedSeparate(t *testing.T) {
	contributions := []LossContribution{
		{ContributionID: "a", ReasonCode: "MECH_BREAKDOWN", LossCategory: LossCategoryAvailability, Classification: ClassificationUnplanned, DurationSeconds: big.NewRat(600, 1), LostQuantity: new(big.Rat)},
		{ContributionID: "b", ReasonCode: "MECH_BREAKDOWN", LossCategory: LossCategoryAvailability, Classification: ClassificationUnclassified, DurationSeconds: big.NewRat(3000, 1), LostQuantity: new(big.Rat)},
	}
	p := buildParetoAnalysis("DURATION", contributions, big.NewRat(3600, 1), new(big.Rat))
	if len(p.Items) != 2 {
		t.Fatalf("classified and unclassified losses merged: %+v", p.Items)
	}
	if p.Items[0].Classification != ClassificationUnclassified || p.Items[0].DurationSeconds.Cmp(big.NewRat(3000, 1)) != 0 {
		t.Fatalf("unclassified loss not visible as its own row: %+v", p.Items[0])
	}
}

// Rule 04 and ADR 0005 constraint 3: a share of a zero total is 0/0 and has no published
// value, so the percentage is empty rather than 0%.
func TestParetoZeroTotalPublishesNoPercentage(t *testing.T) {
	contributions := []LossContribution{
		{ContributionID: "q", ReasonCode: "SCRAP", LossCategory: LossCategoryQuality, Classification: ClassificationUnplanned, DurationSeconds: new(big.Rat), LostQuantity: big.NewRat(3, 1)},
	}
	p := buildParetoAnalysis("DURATION", contributions, new(big.Rat), big.NewRat(3, 1))
	if len(p.Items) != 1 || p.Items[0].Percentage != "" || p.Items[0].CumulativePercentage != "" {
		t.Fatalf("0/0 published as a percentage: %+v", p.Items)
	}
	q := buildParetoAnalysis("QUANTITY", contributions, new(big.Rat), big.NewRat(3, 1))
	if q.Items[0].Percentage != "100.000000%" {
		t.Fatalf("quantity share: %q", q.Items[0].Percentage)
	}
}

// Rule 06: an unknown (nil) measurement and a measured zero never share a digest.
func TestDigestDistinguishesUnknownFromZero(t *testing.T) {
	known := LossContribution{ContributionID: "c", ReasonCode: "SCRAP", LossCategory: LossCategoryQuality, DurationSeconds: new(big.Rat), LostQuantity: new(big.Rat)}
	unknown := known
	unknown.LostQuantity = nil
	if ComputeContributionDigest(known) == ComputeContributionDigest(unknown) {
		t.Fatal("nil and zero quantities digest the same")
	}
}
