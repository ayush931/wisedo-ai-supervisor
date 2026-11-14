package oee

import (
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

// The same facts in any order produce the same contributions, tree and digest; contribution
// ids derive from the source event, not the input position.
func TestExplainLossesIsInvariantToFactOrder(t *testing.T) {
	f := newLossFixture(t)
	second, _ := NewHalfOpenInterval(f.window.Start.Add(time.Hour), f.window.Start.Add(time.Hour+5*time.Minute))
	f.in.StoppageEvents = append(f.in.StoppageEvents, RawStoppageFact{EventID: "evt-stop-2", ReasonCode: "MECH_BREAKDOWN", Interval: second})
	f.in.QualityLossEvents = append(f.in.QualityLossEvents, RawQualityLossFact{EventID: "evt-q-2", ReasonCode: "SCRAP", LostQuantity: big.NewRat(9, 1), Unit: "pcs"})
	a, err := ExplainLosses(f.in)
	if err != nil {
		t.Fatal(err)
	}
	reversed := f.in
	reversed.StoppageEvents = []RawStoppageFact{f.in.StoppageEvents[1], f.in.StoppageEvents[0]}
	reversed.QualityLossEvents = []RawQualityLossFact{f.in.QualityLossEvents[1], f.in.QualityLossEvents[0]}
	b, err := ExplainLosses(reversed)
	if err != nil {
		t.Fatal(err)
	}
	ja, _ := json.Marshal(a.Contributions)
	jb, _ := json.Marshal(b.Contributions)
	if a.Digest != b.Digest || string(ja) != string(jb) {
		t.Fatalf("fact order changed the result:\n%s\n%s", ja, jb)
	}
	if contributionFor(a, "evt-stop-2").ContributionID != contributionFor(b, "evt-stop-2").ContributionID {
		t.Fatal("a contribution id depends on input position")
	}
}

// A stoppage cannot last longer than its interval, and two stoppages cannot lose the same time.
func TestExplainLossesBoundsStoppages(t *testing.T) {
	f := newLossFixture(t)
	f.in.StoppageEvents[0].DurationSeconds = big.NewRat(1_000_000, 1)
	if _, err := ExplainLosses(f.in); !errors.Is(err, ErrInvalidLossFact) {
		t.Fatalf("duration beyond the interval: %v", err)
	}
	f = newLossFixture(t)
	f.in.StoppageEvents[0].DurationSeconds = big.NewRat(600, 1) // exactly the interval
	if _, err := ExplainLosses(f.in); err != nil {
		t.Fatalf("duration equal to the interval: %v", err)
	}
	f = newLossFixture(t)
	overlap, _ := NewHalfOpenInterval(f.window.Start.Add(5*time.Minute), f.window.Start.Add(15*time.Minute))
	f.in.StoppageEvents = append(f.in.StoppageEvents, RawStoppageFact{EventID: "evt-stop-dup", ReasonCode: "UNPLANNED_STOP", Interval: overlap})
	if _, err := ExplainLosses(f.in); !errors.Is(err, ErrOverlappingStoppage) {
		t.Fatalf("overlapping stoppages: %v", err)
	}
	f = newLossFixture(t)
	touching, _ := NewHalfOpenInterval(f.window.Start.Add(10*time.Minute), f.window.Start.Add(20*time.Minute))
	f.in.StoppageEvents = append(f.in.StoppageEvents, RawStoppageFact{EventID: "evt-stop-next", ReasonCode: "UNPLANNED_STOP", Interval: touching})
	if _, err := ExplainLosses(f.in); err != nil {
		t.Fatalf("half-open intervals that only touch: %v", err)
	}
}

// A target that names more than one contribution fails closed instead of picking the first.
func TestReclassifyRejectsAmbiguousTargets(t *testing.T) {
	f := newLossFixture(t)
	res, err := ExplainLosses(f.in)
	if err != nil {
		t.Fatal(err)
	}
	// Make one contribution's source event id equal another contribution's id.
	clash := contributionFor(res, "evt-q").ContributionID
	for i := range res.Contributions {
		if res.Contributions[i].SourceEventIDs[0] == "evt-stop" {
			res.Contributions[i].SourceEventIDs = []string{clash}
		}
	}
	req := reclassifyRequest(f, res)
	req.TargetContributionID = clash
	_, err = ReclassifyLoss(res, req, f.tax)
	if !errors.Is(err, ErrAmbiguousOverride) || deniedAudit(err).ReasonCode != "AMBIGUOUS_TARGET" {
		t.Fatalf("ambiguous target: %v %+v", err, deniedAudit(err))
	}
}

// The taxonomy is verified by content: a revision with the same number but other nodes, or a
// result whose digest is missing, cannot be used for an override.
func TestReclassifyVerifiesTheTaxonomyDigest(t *testing.T) {
	f := newLossFixture(t)
	res, err := ExplainLosses(f.in)
	if err != nil {
		t.Fatal(err)
	}
	tampered := f.tax
	tampered.Nodes = append([]ReasonNode(nil), f.tax.Nodes...)
	extra := tampered.Nodes[0]
	extra.ReasonCode, extra.Name, extra.ParentCode = "NEW_CODE", "Injected", ""
	tampered.Nodes = append(tampered.Nodes, extra)
	tampered.TaxonomyDigest = ""
	req := reclassifyRequest(f, res)
	req.NewReasonCode = "NEW_CODE"
	if _, err := ReclassifyLoss(res, req, tampered); !errors.Is(err, ErrAmbiguousOverride) {
		t.Fatalf("same revision, other nodes: %v", err)
	}
	unsealed := *res
	unsealed.TaxonomyDigest = ""
	if _, err := ReclassifyLoss(&unsealed, reclassifyRequest(f, res), f.tax); !errors.Is(err, ErrAmbiguousOverride) {
		t.Fatalf("result without a taxonomy digest: %v", err)
	}
}

func TestTaxonomyRejectsPaddedCodes(t *testing.T) {
	f := newLossFixture(t)
	for name, change := range map[string]func(*ReasonNode){
		"padded code":   func(n *ReasonNode) { n.ReasonCode += " " },
		"padded parent": func(n *ReasonNode) { n.ParentCode = " " + n.ReasonCode },
	} {
		tax := f.tax
		tax.Nodes = append([]ReasonNode(nil), f.tax.Nodes...)
		change(&tax.Nodes[len(tax.Nodes)-1])
		// The padding rule itself must refuse it; a padded parent would otherwise only
		// fail later as an unknown parent code.
		if err := ValidateTaxonomy(tax); err == nil || !strings.Contains(err.Error(), "surrounding whitespace") {
			t.Errorf("%s: got %v, want the surrounding-whitespace refusal", name, err)
		}
	}
}
