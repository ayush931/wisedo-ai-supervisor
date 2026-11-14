package oee

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
)

type lossFixture struct {
	tenant string
	tax    TaxonomyRevision
	window HalfOpenInterval
	in     LossExplanationInput
}

func newLossFixture(t *testing.T) lossFixture {
	t.Helper()
	tenant := "tenant-hardening"
	tax := makeTestTaxonomy(tenant)
	start := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	window, err := NewHalfOpenInterval(start, start.Add(4*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	iv, _ := NewHalfOpenInterval(start, start.Add(10*time.Minute))
	return lossFixture{tenant, tax, window, LossExplanationInput{
		TenantID: tenant, SiteID: "site-1", Window: window, Taxonomy: tax, PolicyRevisionID: "pol-1", RecordedAt: testRecordedAt,
		StoppageEvents:    []RawStoppageFact{{EventID: "evt-stop", ReasonCode: "UNPLANNED_STOP", Interval: iv}},
		SpeedLossEvents:   []RawSpeedLossFact{{EventID: "evt-spd", ReasonCode: "SPEED_LOSS", DurationSeconds: big.NewRat(60, 1), LostQuantity: big.NewRat(5, 1), Unit: "pcs"}},
		QualityLossEvents: []RawQualityLossFact{{EventID: "evt-q", ReasonCode: "SCRAP", LostQuantity: big.NewRat(3, 1), Unit: "pcs"}},
	}}
}

func TestExplainLossesIsPureAndDigestIgnoresRecordTime(t *testing.T) {
	f := newLossFixture(t)
	a, err := ExplainLosses(f.in)
	if err != nil {
		t.Fatal(err)
	}
	later := f.in
	later.RecordedAt = testRecordedAt.Add(72 * time.Hour)
	b, err := ExplainLosses(later)
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest != b.Digest || a.AuditLog[0].AuditID != b.AuditLog[0].AuditID {
		t.Fatalf("identical inputs at different record times must be identical: %s vs %s", a.Digest, b.Digest)
	}
	if a.History[0].AppliedAt.Equal(b.History[0].AppliedAt) {
		t.Fatal("recorded time must be preserved as data, not hashed")
	}
	for name, mutate := range map[string]func(*LossExplanationInput){
		"source event":    func(i *LossExplanationInput) { i.StoppageEvents[0].EventID = "evt-other" },
		"policy revision": func(i *LossExplanationInput) { i.PolicyRevisionID = "pol-2" },
		"reason code":     func(i *LossExplanationInput) { i.StoppageEvents[0].ReasonCode = "MECH_BREAKDOWN" },
		"measurement":     func(i *LossExplanationInput) { i.SpeedLossEvents[0].DurationSeconds = big.NewRat(61, 1) },
	} {
		m := f.in
		m.StoppageEvents = append([]RawStoppageFact(nil), f.in.StoppageEvents...)
		m.SpeedLossEvents = append([]RawSpeedLossFact(nil), f.in.SpeedLossEvents...)
		mutate(&m)
		c, err := ExplainLosses(m)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.Digest == a.Digest {
			t.Errorf("changed %s must change the digest", name)
		}
	}
	// A digest label that does not match the taxonomy content is refused.
	relabelled := f.in
	relabelled.Taxonomy.TaxonomyDigest = "different-taxonomy-digest"
	if _, err := ExplainLosses(relabelled); !errors.Is(err, ErrTaxonomyDigest) {
		t.Errorf("relabelled taxonomy digest: %v", err)
	}
	// Changed taxonomy content changes the result digest, with or without a supplied label.
	taxChanged := f.in
	taxChanged.Taxonomy.Nodes = append([]ReasonNode(nil), f.in.Taxonomy.Nodes...)
	taxChanged.Taxonomy.Nodes[0].Description += " (revised wording)"
	taxChanged.Taxonomy.TaxonomyDigest = ""
	if c, err := ExplainLosses(taxChanged); err != nil || c.Digest == a.Digest || c.TaxonomyDigest == a.TaxonomyDigest {
		t.Errorf("changed taxonomy content must change the result digest: %v", err)
	}
}

func TestExplainLossesRejectsInvalidFactsInsteadOfClamping(t *testing.T) {
	neg := big.NewRat(-1, 1)
	cases := map[string]struct {
		mutate func(*LossExplanationInput)
		want   error
	}{
		"negative stoppage duration":   {func(i *LossExplanationInput) { i.StoppageEvents[0].DurationSeconds = neg }, ErrInvalidLossFact},
		"negative speed duration":      {func(i *LossExplanationInput) { i.SpeedLossEvents[0].DurationSeconds = neg }, ErrInvalidLossFact},
		"negative speed quantity":      {func(i *LossExplanationInput) { i.SpeedLossEvents[0].LostQuantity = neg }, ErrInvalidLossFact},
		"negative quality quantity":    {func(i *LossExplanationInput) { i.QualityLossEvents[0].LostQuantity = neg }, ErrInvalidLossFact},
		"missing speed duration":       {func(i *LossExplanationInput) { i.SpeedLossEvents[0].DurationSeconds = nil }, ErrInvalidLossFact},
		"missing quality quantity":     {func(i *LossExplanationInput) { i.QualityLossEvents[0].LostQuantity = nil }, ErrInvalidLossFact},
		"empty event id":               {func(i *LossExplanationInput) { i.StoppageEvents[0].EventID = " " }, ErrInvalidLossFact},
		"duplicate event across kinds": {func(i *LossExplanationInput) { i.QualityLossEvents[0].EventID = "evt-stop" }, ErrDuplicateEvent},
		"interval outside window": {func(i *LossExplanationInput) {
			iv, _ := NewHalfOpenInterval(i.Window.End.Add(-time.Minute), i.Window.End.Add(time.Minute))
			i.StoppageEvents[0].Interval = iv
		}, ErrFactOutsideWindow},
		"empty interval": {func(i *LossExplanationInput) {
			i.StoppageEvents[0].Interval = HalfOpenInterval{Start: i.Window.Start, End: i.Window.Start}
		}, ErrInvalidInterval},
		"missing recorded time": {func(i *LossExplanationInput) { i.RecordedAt = time.Time{} }, ErrMissingRecordedAt},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newLossFixture(t)
			c.mutate(&f.in)
			res, err := ExplainLosses(f.in)
			if !errors.Is(err, c.want) || res != nil {
				t.Fatalf("got %v (result %v), want %v", err, res != nil, c.want)
			}
		})
	}
}

func TestExplainLossesFailsClosedOnTaxonomyScopeAndState(t *testing.T) {
	f := newLossFixture(t)
	other := f.in
	other.Taxonomy = makeTestTaxonomy("another-tenant")
	if _, err := ExplainLosses(other); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("foreign taxonomy: %v", err)
	}
	site := f.in
	site.Taxonomy.SiteID = "site-2"
	if _, err := ExplainLosses(site); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("foreign site taxonomy: %v", err)
	}
	draft := f.in
	draft.Taxonomy.State = ReasonStateDraft
	if _, err := ExplainLosses(draft); !errors.Is(err, ErrTaxonomyNotEffective) {
		t.Fatalf("draft taxonomy: %v", err)
	}
	short := f.in
	end := f.window.Start.Add(time.Hour)
	short.Taxonomy.EffectiveWindow = HalfOpenInterval{Start: f.tax.EffectiveWindow.Start, End: end}
	if _, err := ExplainLosses(short); !errors.Is(err, ErrTaxonomyNotEffective) {
		t.Fatalf("taxonomy not covering the window: %v", err)
	}
}

func contributionFor(r *ClassificationResult, event string) LossContribution {
	for _, c := range r.Contributions {
		for _, id := range c.SourceEventIDs {
			if id == event {
				return c
			}
		}
	}
	return LossContribution{}
}

func TestUnusableReasonsStayVisibleAsUnclassified(t *testing.T) {
	f := newLossFixture(t)
	f.in.StoppageEvents[0].ReasonCode = "LEGACY_CODE" // retired in the fixture
	// A node whose effective range ended before the loss also cannot classify it.
	expired := f.in
	expired.Taxonomy.Nodes = append([]ReasonNode(nil), f.tax.Nodes...)
	for i := range expired.Taxonomy.Nodes {
		if expired.Taxonomy.Nodes[i].ReasonCode == "SPEED_LOSS" {
			end := f.window.Start.Add(-time.Hour)
			expired.Taxonomy.Nodes[i].EffectiveEnd = &end
		}
	}
	expired.Taxonomy.TaxonomyDigest, _ = ComputeTaxonomyDigest(expired.Taxonomy)
	for name, in := range map[string]LossExplanationInput{"retired": f.in, "expired": expired} {
		res, err := ExplainLosses(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		event := "evt-stop"
		if name == "expired" {
			event = "evt-spd"
		}
		if c := contributionFor(res, event); c.Classification != ClassificationUnclassified || c.ReasonCode == "" {
			t.Fatalf("%s reason must stay visible as unclassified: %+v", name, c)
		}
	}
}

// A reason whose parent lives in another classification used to be dropped from
// the tree, losing its contribution from the drill-down.
func TestChildOfOtherClassificationIsNotOrphaned(t *testing.T) {
	f := newLossFixture(t)
	mk := func(code, parent string, class LossClassification) ReasonNode {
		return ReasonNode{TenantID: f.tenant, ReasonCode: code, ParentCode: parent, Name: code, LossCategory: LossCategoryAvailability,
			Classification: class, State: ReasonStateEffective, EffectiveStart: f.tax.EffectiveWindow.Start, Revision: 1}
	}
	tax := f.tax
	tax.Nodes = []ReasonNode{mk("PARENT_UNPLANNED", "", ClassificationUnplanned), mk("CHILD_PLANNED", "PARENT_UNPLANNED", ClassificationPlanned)}
	tax.TaxonomyDigest, _ = ComputeTaxonomyDigest(tax)
	in := f.in
	in.Taxonomy = tax
	in.SpeedLossEvents, in.QualityLossEvents = nil, nil
	in.StoppageEvents[0].ReasonCode = "CHILD_PLANNED"
	res, err := ExplainLosses(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.LossTree.DrillDownContributingFacts(); len(got) != 1 || got[0].ReasonCode != "CHILD_PLANNED" {
		t.Fatalf("contribution dropped from drill-down: %+v", got)
	}
}

func TestParetoDoesNotMergeUnclassifiedAcrossCategories(t *testing.T) {
	f := newLossFixture(t)
	f.in.StoppageEvents = nil
	f.in.SpeedLossEvents[0].ReasonCode, f.in.QualityLossEvents[0].ReasonCode = "", ""
	res, err := ExplainLosses(f.in)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(res.ParetoQuantity.Items); n != 2 {
		t.Fatalf("unclassified performance and quality must stay separate rows, got %d", n)
	}
	for _, it := range res.ParetoQuantity.Items {
		if it.Classification != ClassificationUnclassified {
			t.Fatalf("unclassified must stay visible: %+v", it)
		}
	}
}

func reclassifyRequest(f lossFixture, res *ClassificationResult) ReclassifyRequest {
	return ReclassifyRequest{
		TenantID: f.tenant, SiteID: "site-1", BaseRevision: res.Revision, TargetContributionID: contributionFor(res, "evt-stop").ContributionID,
		NewReasonCode: "MECH_BREAKDOWN", ActorRef: "usr-1", ActorRoles: []string{"SUPERVISOR"},
		Justification: "Confirmed on the shop floor", EvidenceRef: "workorder:1", OccurredAt: testRecordedAt.Add(time.Hour),
	}
}

func TestReclassifyIsScopedDeterministicAndChained(t *testing.T) {
	f := newLossFixture(t)
	rev1, err := ExplainLosses(f.in)
	if err != nil {
		t.Fatal(err)
	}
	req := reclassifyRequest(f, rev1)
	a, err := ReclassifyLoss(rev1, req, f.tax)
	if err != nil {
		t.Fatal(err)
	}
	later := req
	later.OccurredAt = req.OccurredAt.Add(48 * time.Hour)
	rev1b, _ := ExplainLosses(f.in)
	b, err := ReclassifyLoss(rev1b, later, f.tax)
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest != b.Digest || a.Digest == rev1.Digest {
		t.Fatal("reclassification digest must ignore occurrence time and differ from its predecessor")
	}
	if last := a.History[len(a.History)-1]; last.PreviousDigest != rev1.Digest || last.Revision != 2 {
		t.Fatalf("revision chain broken: %+v", last)
	}
	if rev1.Revision != 1 || contributionFor(rev1, "evt-stop").ReasonCode != "UNPLANNED_STOP" {
		t.Fatal("prior revision must be preserved")
	}
}

func TestReclassifyDeniesCrossScopeAndUnusableRequests(t *testing.T) {
	f := newLossFixture(t)
	fresh := func() (*ClassificationResult, ReclassifyRequest) {
		r, err := ExplainLosses(f.in)
		if err != nil {
			t.Fatal(err)
		}
		return r, reclassifyRequest(f, r)
	}
	t.Run("another tenant", func(t *testing.T) {
		r, req := fresh()
		req.TenantID = "another-tenant"
		before := len(r.AuditLog)
		_, err := ReclassifyLoss(r, req, f.tax)
		if !errors.Is(err, ErrContributionNotFound) {
			t.Fatalf("got %v, want non-disclosing not found", err)
		}
		if audit := deniedAudit(err); audit.ReasonCode != "SCOPE_MISMATCH" || audit.ActorRef != req.ActorRef {
			t.Fatalf("scope denial must be audited: %+v", audit)
		}
		// The other tenant's published result is not touched by the denied request.
		if len(r.AuditLog) != before {
			t.Fatal("a denied request wrote into the published result")
		}
	})
	t.Run("another site", func(t *testing.T) {
		r, req := fresh()
		req.SiteID = "site-2"
		if _, err := ReclassifyLoss(r, req, f.tax); !errors.Is(err, ErrContributionNotFound) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("foreign taxonomy", func(t *testing.T) {
		r, req := fresh()
		if _, err := ReclassifyLoss(r, req, makeTestTaxonomy("another-tenant")); !errors.Is(err, ErrContributionNotFound) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("different taxonomy revision", func(t *testing.T) {
		r, req := fresh()
		tax := f.tax
		tax.Revision = 2
		if _, err := ReclassifyLoss(r, req, tax); !errors.Is(err, ErrAmbiguousOverride) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("retired reason", func(t *testing.T) {
		r, req := fresh()
		req.NewReasonCode = "LEGACY_CODE"
		if _, err := ReclassifyLoss(r, req, f.tax); !errors.Is(err, ErrAmbiguousOverride) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("missing occurrence time", func(t *testing.T) {
		r, req := fresh()
		req.OccurredAt = time.Time{}
		if _, err := ReclassifyLoss(r, req, f.tax); !errors.Is(err, ErrAmbiguousOverride) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("invalid taxonomy", func(t *testing.T) {
		r, req := fresh()
		bad := f.tax
		bad.Nodes = nil
		if _, err := ReclassifyLoss(r, req, bad); !errors.Is(err, ErrUnseededTaxonomy) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("denied attempts never change the result", func(t *testing.T) {
		r, req := fresh()
		before := r.Digest
		req.ActorRoles = []string{"VIEWER"}
		if _, err := ReclassifyLoss(r, req, f.tax); !errors.Is(err, ErrUnauthorizedOverride) {
			t.Fatalf("got %v", err)
		}
		if r.Digest != before || r.Revision != 1 || contributionFor(r, "evt-stop").ReasonCode != "UNPLANNED_STOP" {
			t.Fatal("a denied override must not alter the result")
		}
	})
}

func TestAuditEntriesAreSafe(t *testing.T) {
	f := newLossFixture(t)
	r, err := ExplainLosses(f.in)
	if err != nil {
		t.Fatal(err)
	}
	req := reclassifyRequest(f, r)
	req.ActorRoles = nil
	_, err = ReclassifyLoss(r, req, f.tax)
	if !errors.Is(err, ErrUnauthorizedOverride) {
		t.Fatalf("no roles: got %v, want ErrUnauthorizedOverride", err)
	}
	entry := deniedAudit(err)
	if _, err := uuid.Parse(entry.AuditID); err != nil || entry.Digest == "" || entry.Status != "UNAUTHORIZED" {
		t.Fatalf("unsafe or malformed audit entry: %+v", entry)
	}
}
