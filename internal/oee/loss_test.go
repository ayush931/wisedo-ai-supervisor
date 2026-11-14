package oee

import (
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var testRecordedAt = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

func makeTestTaxonomy(tenantID string) TaxonomyRevision {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
	interval, _ := NewHalfOpenInterval(start, end)

	nodes := []ReasonNode{
		// Availability - Planned
		{
			TenantID:       tenantID,
			ReasonCode:     "PLANNED_STOP",
			Name:           "Planned Stoppage",
			LossCategory:   LossCategoryAvailability,
			Classification: ClassificationPlanned,
			State:          ReasonStateEffective,
			EffectiveStart: start,
			Revision:       1,
		},
		{
			TenantID:       tenantID,
			ReasonCode:     "CHANGEOVER",
			ParentCode:     "PLANNED_STOP",
			Name:           "Tool & Product Changeover",
			LossCategory:   LossCategoryAvailability,
			Classification: ClassificationPlanned,
			State:          ReasonStateEffective,
			EffectiveStart: start,
			Revision:       1,
		},
		{
			TenantID:       tenantID,
			ReasonCode:     "PLANNED_MAINT",
			ParentCode:     "PLANNED_STOP",
			Name:           "Preventive Maintenance",
			LossCategory:   LossCategoryAvailability,
			Classification: ClassificationPlanned,
			State:          ReasonStateEffective,
			EffectiveStart: start,
			Revision:       1,
		},

		// Availability - Unplanned
		{
			TenantID:       tenantID,
			ReasonCode:     "UNPLANNED_STOP",
			Name:           "Unplanned Stoppage",
			LossCategory:   LossCategoryAvailability,
			Classification: ClassificationUnplanned,
			State:          ReasonStateEffective,
			EffectiveStart: start,
			Revision:       1,
		},
		{
			TenantID:       tenantID,
			ReasonCode:     "MECH_BREAKDOWN",
			ParentCode:     "UNPLANNED_STOP",
			Name:           "Mechanical Breakdown",
			LossCategory:   LossCategoryAvailability,
			Classification: ClassificationUnplanned,
			State:          ReasonStateEffective,
			EffectiveStart: start,
			Revision:       1,
		},
		{
			TenantID:       tenantID,
			ReasonCode:     "ELEC_BREAKDOWN",
			ParentCode:     "UNPLANNED_STOP",
			Name:           "Electrical Failure",
			LossCategory:   LossCategoryAvailability,
			Classification: ClassificationUnplanned,
			State:          ReasonStateEffective,
			EffectiveStart: start,
			Revision:       1,
		},

		// Performance
		{
			TenantID:       tenantID,
			ReasonCode:     "SPEED_LOSS",
			Name:           "Reduced Speed Operation",
			LossCategory:   LossCategoryPerformance,
			Classification: ClassificationUnplanned,
			State:          ReasonStateEffective,
			EffectiveStart: start,
			Revision:       1,
		},
		{
			TenantID:       tenantID,
			ReasonCode:     "MICROSTOP",
			Name:           "Minor Sensor Jam (< 5m)",
			LossCategory:   LossCategoryPerformance,
			Classification: ClassificationUnplanned,
			State:          ReasonStateEffective,
			EffectiveStart: start,
			Revision:       1,
		},

		// Quality
		{
			TenantID:       tenantID,
			ReasonCode:     "SCRAP",
			Name:           "Defective Scrap Output",
			LossCategory:   LossCategoryQuality,
			Classification: ClassificationUnplanned,
			State:          ReasonStateEffective,
			EffectiveStart: start,
			Revision:       1,
		},
		{
			TenantID:       tenantID,
			ReasonCode:     "REWORK",
			Name:           "Rework Inspection Defect",
			LossCategory:   LossCategoryQuality,
			Classification: ClassificationUnplanned,
			State:          ReasonStateEffective,
			EffectiveStart: start,
			Revision:       1,
		},

		// Retired reason code for testing
		{
			TenantID:       tenantID,
			ReasonCode:     "LEGACY_CODE",
			Name:           "Decommissioned Reason Code",
			LossCategory:   LossCategoryAvailability,
			Classification: ClassificationUnplanned,
			State:          ReasonStateRetired,
			EffectiveStart: start,
			Revision:       1,
		},
	}

	tax := TaxonomyRevision{
		TaxonomyID:      uuid.New(),
		TenantID:        tenantID,
		Revision:        1,
		State:           ReasonStateEffective,
		EffectiveWindow: interval,
		Nodes:           nodes,
	}
	digest, _ := ComputeTaxonomyDigest(tax)
	tax.TaxonomyDigest = digest
	return tax
}

func TestTaxonomyValidation_EmptyRegistryFailsClosed(t *testing.T) {
	emptyTax := TaxonomyRevision{
		TaxonomyID: uuid.New(),
		TenantID:   "tenant-1",
		Nodes:      []ReasonNode{},
	}
	if err := ValidateTaxonomy(emptyTax); err != ErrUnseededTaxonomy {
		t.Fatalf("expected ErrUnseededTaxonomy, got %v", err)
	}

	reg := NewTaxonomyRegistry()
	_, err := reg.GetEffective("tenant-1", time.Now())
	if err != ErrUnseededTaxonomy {
		t.Fatalf("expected ErrUnseededTaxonomy from empty registry, got %v", err)
	}

	// ExplainLosses must also fail closed with empty taxonomy
	wStart := time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	wEnd := time.Date(2026, 6, 1, 16, 0, 0, 0, time.UTC)
	window, _ := NewHalfOpenInterval(wStart, wEnd)
	_, err = ExplainLosses(LossExplanationInput{
		TenantID:   "tenant-1",
		Window:     window,
		Taxonomy:   emptyTax,
		RecordedAt: testRecordedAt,
	})
	if err != ErrUnseededTaxonomy {
		t.Fatalf("expected ErrUnseededTaxonomy from ExplainLosses, got %v", err)
	}
}

func TestTaxonomyValidation_CyclesDuplicatesCrossTenantAndRanges(t *testing.T) {
	tenantID := "tenant-alpha"
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	window, _ := NewHalfOpenInterval(start, end)

	// 1. Direct cycle: A -> B -> A
	cycleTax := TaxonomyRevision{
		TaxonomyID:      uuid.New(),
		TenantID:        tenantID,
		EffectiveWindow: window,
		Nodes: []ReasonNode{
			{TenantID: tenantID, ReasonCode: "A", ParentCode: "B", Name: "A", LossCategory: LossCategoryAvailability, Classification: ClassificationPlanned, State: ReasonStateEffective, EffectiveStart: start},
			{TenantID: tenantID, ReasonCode: "B", ParentCode: "A", Name: "B", LossCategory: LossCategoryAvailability, Classification: ClassificationPlanned, State: ReasonStateEffective, EffectiveStart: start},
		},
	}
	if err := ValidateTaxonomy(cycleTax); err != ErrTaxonomyCycleDetected {
		t.Errorf("expected ErrTaxonomyCycleDetected for direct cycle, got %v", err)
	}

	// 2. Transitive cycle: A -> B -> C -> A
	transitiveCycle := TaxonomyRevision{
		TaxonomyID:      uuid.New(),
		TenantID:        tenantID,
		EffectiveWindow: window,
		Nodes: []ReasonNode{
			{TenantID: tenantID, ReasonCode: "A", ParentCode: "C", Name: "A", LossCategory: LossCategoryAvailability, Classification: ClassificationPlanned, State: ReasonStateEffective, EffectiveStart: start},
			{TenantID: tenantID, ReasonCode: "B", ParentCode: "A", Name: "B", LossCategory: LossCategoryAvailability, Classification: ClassificationPlanned, State: ReasonStateEffective, EffectiveStart: start},
			{TenantID: tenantID, ReasonCode: "C", ParentCode: "B", Name: "C", LossCategory: LossCategoryAvailability, Classification: ClassificationPlanned, State: ReasonStateEffective, EffectiveStart: start},
		},
	}
	if err := ValidateTaxonomy(transitiveCycle); err != ErrTaxonomyCycleDetected {
		t.Errorf("expected ErrTaxonomyCycleDetected for transitive cycle, got %v", err)
	}

	// 3. Duplicate codes
	dupTax := TaxonomyRevision{
		TaxonomyID:      uuid.New(),
		TenantID:        tenantID,
		EffectiveWindow: window,
		Nodes: []ReasonNode{
			{TenantID: tenantID, ReasonCode: "JAM", Name: "Jam 1", LossCategory: LossCategoryAvailability, Classification: ClassificationUnplanned, State: ReasonStateEffective, EffectiveStart: start},
			{TenantID: tenantID, ReasonCode: "JAM", Name: "Jam 2", LossCategory: LossCategoryAvailability, Classification: ClassificationUnplanned, State: ReasonStateEffective, EffectiveStart: start},
		},
	}
	if err := ValidateTaxonomy(dupTax); err != ErrDuplicateReasonCode {
		t.Errorf("expected ErrDuplicateReasonCode, got %v", err)
	}

	// 4. Cross-tenant parent reference
	crossTenantTax := TaxonomyRevision{
		TaxonomyID:      uuid.New(),
		TenantID:        tenantID,
		EffectiveWindow: window,
		Nodes: []ReasonNode{
			{TenantID: "tenant-other", ReasonCode: "PARENT", Name: "Parent", LossCategory: LossCategoryAvailability, Classification: ClassificationPlanned, State: ReasonStateEffective, EffectiveStart: start},
			{TenantID: tenantID, ReasonCode: "CHILD", ParentCode: "PARENT", Name: "Child", LossCategory: LossCategoryAvailability, Classification: ClassificationPlanned, State: ReasonStateEffective, EffectiveStart: start},
		},
	}
	if err := ValidateTaxonomy(crossTenantTax); err != ErrCrossTenantParent {
		t.Errorf("expected ErrCrossTenantParent, got %v", err)
	}

	// 5. Invalid effective range (start >= end)
	badEnd := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	rangeTax := TaxonomyRevision{
		TaxonomyID:      uuid.New(),
		TenantID:        tenantID,
		EffectiveWindow: window,
		Nodes: []ReasonNode{
			{TenantID: tenantID, ReasonCode: "CODE1", Name: "Code 1", LossCategory: LossCategoryAvailability, Classification: ClassificationPlanned, State: ReasonStateEffective, EffectiveStart: start, EffectiveEnd: &badEnd},
		},
	}
	if err := ValidateTaxonomy(rangeTax); err != ErrInvalidEffectiveRange {
		t.Errorf("expected ErrInvalidEffectiveRange, got %v", err)
	}

	// 6. Nonexistent parent code
	missingParentTax := TaxonomyRevision{
		TaxonomyID:      uuid.New(),
		TenantID:        tenantID,
		EffectiveWindow: window,
		Nodes: []ReasonNode{
			{TenantID: tenantID, ReasonCode: "CODE1", ParentCode: "NONEXISTENT", Name: "Code 1", LossCategory: LossCategoryAvailability, Classification: ClassificationPlanned, State: ReasonStateEffective, EffectiveStart: start},
		},
	}
	if err := ValidateTaxonomy(missingParentTax); err != ErrInvalidParentCode {
		t.Errorf("expected ErrInvalidParentCode, got %v", err)
	}
}

func TestGoldenFixture_PlannedUnplannedAndUnclassifiedReconciliation(t *testing.T) {
	tenantID := "tenant-golden"
	tax := makeTestTaxonomy(tenantID)

	wStart := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	wEnd := time.Date(2026, 9, 27, 16, 0, 0, 0, time.UTC)
	window, _ := NewHalfOpenInterval(wStart, wEnd)

	// Golden stoppage scenario:
	// 1. CHANGEOVER (Planned Availability): 1800s
	// 2. PLANNED_MAINT (Planned Availability): 1200s
	// 3. ELEC_BREAKDOWN (Unplanned Availability): 2400s
	// 4. MECH_BREAKDOWN (Unplanned Availability): 900s
	// 5. UNMAPPED_CODE_999 (Unclassified Availability): 600s
	t1 := wStart
	t2 := t1.Add(1800 * time.Second)
	t3 := t2.Add(1200 * time.Second)
	t4 := t3.Add(2400 * time.Second)
	t5 := t4.Add(900 * time.Second)
	t6 := t5.Add(600 * time.Second)

	int1, _ := NewHalfOpenInterval(t1, t2)
	int2, _ := NewHalfOpenInterval(t2, t3)
	int3, _ := NewHalfOpenInterval(t3, t4)
	int4, _ := NewHalfOpenInterval(t4, t5)
	int5, _ := NewHalfOpenInterval(t5, t6)

	stoppages := []RawStoppageFact{
		{EventID: "evt-stop-1", ReasonCode: "CHANGEOVER", Interval: int1, DurationSeconds: big.NewRat(1800, 1)},
		{EventID: "evt-stop-2", ReasonCode: "PLANNED_MAINT", Interval: int2, DurationSeconds: big.NewRat(1200, 1)},
		{EventID: "evt-stop-3", ReasonCode: "ELEC_BREAKDOWN", Interval: int3, DurationSeconds: big.NewRat(2400, 1)},
		{EventID: "evt-stop-4", ReasonCode: "MECH_BREAKDOWN", Interval: int4, DurationSeconds: big.NewRat(900, 1)},
		{EventID: "evt-stop-5", ReasonCode: "UNKNOWN_TRIP_999", Interval: int5, DurationSeconds: big.NewRat(600, 1)},
	}

	// Speed loss: 300s, 50 pcs lost
	pcsUnit, _ := ParseUnitCode("pcs")
	speedLosses := []RawSpeedLossFact{
		{EventID: "evt-spd-1", ReasonCode: "SPEED_LOSS", DurationSeconds: big.NewRat(300, 1), LostQuantity: big.NewRat(50, 1), Unit: pcsUnit},
	}

	// Quality loss: 100 scrap pcs, 600s converted cycle time
	qualityLosses := []RawQualityLossFact{
		{EventID: "evt-qual-1", ReasonCode: "SCRAP", LostQuantity: big.NewRat(100, 1), DurationSeconds: big.NewRat(600, 1), Unit: pcsUnit},
	}

	res, err := ExplainLosses(LossExplanationInput{
		TenantID:          tenantID,
		SiteID:            "site-1",
		Window:            window,
		StoppageEvents:    stoppages,
		SpeedLossEvents:   speedLosses,
		QualityLossEvents: qualityLosses,
		Taxonomy:          tax,
		RecordedAt:        testRecordedAt,
		PolicyRevisionID:  "pol-2026-v1",
	})
	if err != nil {
		t.Fatalf("ExplainLosses failed: %v", err)
	}

	// Verify Total Duration = 1800 + 1200 + 2400 + 900 + 600 + 300 + 600 = 7800s
	expectedTotalDur := big.NewRat(7800, 1)
	if res.LossTree.TotalDurationSeconds.Cmp(expectedTotalDur) != 0 {
		t.Errorf("expected total duration %s, got %s", expectedTotalDur.RatString(), res.LossTree.TotalDurationSeconds.RatString())
	}

	// Verify Total Lost Quantity = 50 + 100 = 150
	expectedTotalQty := big.NewRat(150, 1)
	if res.LossTree.TotalLostQuantity.Cmp(expectedTotalQty) != 0 {
		t.Errorf("expected total quantity %s, got %s", expectedTotalQty.RatString(), res.LossTree.TotalLostQuantity.RatString())
	}

	// Find Category nodes
	var availNode, perfNode, qualNode *LossTreeNode
	for _, child := range res.LossTree.Children {
		switch child.LossCategory {
		case LossCategoryAvailability:
			availNode = child
		case LossCategoryPerformance:
			perfNode = child
		case LossCategoryQuality:
			qualNode = child
		}
	}

	if availNode == nil || perfNode == nil || qualNode == nil {
		t.Fatalf("missing one of the three primary loss category nodes")
	}

	// Availability Duration = 1800 + 1200 + 2400 + 900 + 600 = 6900s
	if availNode.TotalDurationSeconds.Cmp(big.NewRat(6900, 1)) != 0 {
		t.Errorf("expected availability duration 6900s, got %s", availNode.TotalDurationSeconds.RatString())
	}

	// Check Planned vs Unplanned vs Unclassified inside Availability
	var plannedAvail, unplannedAvail, unclassAvail *LossTreeNode
	for _, child := range availNode.Children {
		switch child.Classification {
		case ClassificationPlanned:
			plannedAvail = child
		case ClassificationUnplanned:
			unplannedAvail = child
		case ClassificationUnclassified:
			unclassAvail = child
		}
	}

	if plannedAvail == nil || unplannedAvail == nil || unclassAvail == nil {
		t.Fatalf("missing planned, unplanned, or unclassified branch in availability")
	}

	// Planned = 1800 + 1200 = 3000s
	if plannedAvail.TotalDurationSeconds.Cmp(big.NewRat(3000, 1)) != 0 {
		t.Errorf("expected planned availability 3000s, got %s", plannedAvail.TotalDurationSeconds.RatString())
	}

	// Unplanned = 2400 + 900 = 3300s
	if unplannedAvail.TotalDurationSeconds.Cmp(big.NewRat(3300, 1)) != 0 {
		t.Errorf("expected unplanned availability 3300s, got %s", unplannedAvail.TotalDurationSeconds.RatString())
	}

	// Unclassified remains visible! = 600s
	if unclassAvail.TotalDurationSeconds.Cmp(big.NewRat(600, 1)) != 0 {
		t.Errorf("expected unclassified availability 600s, got %s", unclassAvail.TotalDurationSeconds.RatString())
	}

	// Verify Pareto total reconciliation:
	// Duration Pareto total MUST match LossTree TotalDurationSeconds exactly
	if res.ParetoDuration.TotalLossDurationSeconds.Cmp(res.LossTree.TotalDurationSeconds) != 0 {
		t.Errorf("Pareto duration total %s does not match LossTree total %s",
			res.ParetoDuration.TotalLossDurationSeconds.RatString(), res.LossTree.TotalDurationSeconds.RatString())
	}

	// Quantity Pareto total MUST match LossTree TotalLostQuantity exactly
	if res.ParetoQuantity.TotalLostQuantity.Cmp(res.LossTree.TotalLostQuantity) != 0 {
		t.Errorf("Pareto quantity total %s does not match LossTree total %s",
			res.ParetoQuantity.TotalLostQuantity.RatString(), res.LossTree.TotalLostQuantity.RatString())
	}

	// Verify Pareto item total ordering:
	// Descending by Duration: ELEC_BREAKDOWN (2400) > CHANGEOVER (1800) > PLANNED_MAINT (1200) > MECH_BREAKDOWN (900) > SCRAP (600) == UNKNOWN_TRIP_999 (600) > SPEED_LOSS (300)
	// Between SCRAP and UNKNOWN_TRIP_999 tie-breaker: 'S' < 'U', so SCRAP comes before UNKNOWN_TRIP_999
	pItems := res.ParetoDuration.Items
	if len(pItems) != 7 {
		t.Fatalf("expected 7 pareto items, got %d", len(pItems))
	}
	if pItems[0].ReasonCode != "ELEC_BREAKDOWN" || pItems[0].DurationSeconds.Cmp(big.NewRat(2400, 1)) != 0 {
		t.Errorf("rank 1 mismatch: %+v", pItems[0])
	}
	if pItems[1].ReasonCode != "CHANGEOVER" || pItems[1].DurationSeconds.Cmp(big.NewRat(1800, 1)) != 0 {
		t.Errorf("rank 2 mismatch: %+v", pItems[1])
	}
	if pItems[2].ReasonCode != "PLANNED_MAINT" || pItems[2].DurationSeconds.Cmp(big.NewRat(1200, 1)) != 0 {
		t.Errorf("rank 3 mismatch: %+v", pItems[2])
	}
	if pItems[3].ReasonCode != "MECH_BREAKDOWN" || pItems[3].DurationSeconds.Cmp(big.NewRat(900, 1)) != 0 {
		t.Errorf("rank 4 mismatch: %+v", pItems[3])
	}
	// Tie breaker: SCRAP vs UNKNOWN_TRIP_999 (both 600s)
	if pItems[4].ReasonCode != "SCRAP" || pItems[5].ReasonCode != "UNKNOWN_TRIP_999" {
		t.Errorf("tie-breaker mismatch: rank 5=%s rank 6=%s", pItems[4].ReasonCode, pItems[5].ReasonCode)
	}

	// Check cumulative percentage at end is 100.000000%
	lastItem := pItems[len(pItems)-1]
	if lastItem.CumulativePercentage != "100.000000%" {
		t.Errorf("expected final cumulative percentage 100.000000%%, got %s", lastItem.CumulativePercentage)
	}
}

func TestDrillDownContributingFacts(t *testing.T) {
	tenantID := "tenant-facts"
	tax := makeTestTaxonomy(tenantID)
	wStart := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	wEnd := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	window, _ := NewHalfOpenInterval(wStart, wEnd)

	int1, _ := NewHalfOpenInterval(wStart, wStart.Add(600*time.Second))
	int2, _ := NewHalfOpenInterval(wStart.Add(600*time.Second), wStart.Add(1200*time.Second))

	stoppages := []RawStoppageFact{
		{EventID: "fact-001", ReasonCode: "MECH_BREAKDOWN", Interval: int1, DurationSeconds: big.NewRat(600, 1)},
		{EventID: "fact-002", ReasonCode: "MECH_BREAKDOWN", Interval: int2, DurationSeconds: big.NewRat(600, 1)},
	}

	res, err := ExplainLosses(LossExplanationInput{
		TenantID:       tenantID,
		Window:         window,
		StoppageEvents: stoppages,
		Taxonomy:       tax,
		RecordedAt:     testRecordedAt,
	})
	if err != nil {
		t.Fatalf("ExplainLosses failed: %v", err)
	}

	// Test drill-down from Root
	rootFactIDs := res.LossTree.DrillDownSourceEventIDs()
	if len(rootFactIDs) != 2 || rootFactIDs[0] != "fact-001" || rootFactIDs[1] != "fact-002" {
		t.Errorf("unexpected root drill-down IDs: %v", rootFactIDs)
	}

	// Test drill-down from Pareto item
	if len(res.ParetoDuration.Items) == 0 {
		t.Fatalf("expected at least 1 pareto item")
	}
	pItem := res.ParetoDuration.Items[0]
	if len(pItem.SourceEventIDs) != 2 || pItem.SourceEventIDs[0] != "fact-001" || pItem.SourceEventIDs[1] != "fact-002" {
		t.Errorf("unexpected pareto item drill-down IDs: %v", pItem.SourceEventIDs)
	}
}

func TestGovernedReclassification_AppendRevisionAndPreservePriorEvidence(t *testing.T) {
	tenantID := "tenant-override"
	tax := makeTestTaxonomy(tenantID)
	wStart := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	wEnd := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	window, _ := NewHalfOpenInterval(wStart, wEnd)

	int1, _ := NewHalfOpenInterval(wStart, wStart.Add(900*time.Second))
	stoppages := []RawStoppageFact{
		{EventID: "raw-evt-100", ReasonCode: "", Interval: int1, DurationSeconds: big.NewRat(900, 1)},
	}

	rev1, err := ExplainLosses(LossExplanationInput{
		TenantID:       tenantID,
		SiteID:         "site-1",
		Window:         window,
		StoppageEvents: stoppages,
		Taxonomy:       tax,
		RecordedAt:     testRecordedAt,
	})
	if err != nil {
		t.Fatalf("initial ExplainLosses failed: %v", err)
	}

	// Initial reason code is UNCLASSIFIED
	if rev1.Contributions[0].ReasonCode != "UNCLASSIFIED" {
		t.Fatalf("expected UNCLASSIFIED, got %s", rev1.Contributions[0].ReasonCode)
	}
	rev1Digest := rev1.Digest
	rev1TreeDigest := rev1.LossTree.Digest

	// Operator corrects the unclassified stop to MECH_BREAKDOWN
	overrideTime := time.Date(2026, 9, 27, 12, 30, 0, 0, time.UTC)
	req := ReclassifyRequest{
		TenantID:             tenantID,
		SiteID:               "site-1",
		BaseRevision:         1,
		TargetContributionID: rev1.Contributions[0].ContributionID,
		NewReasonCode:        "MECH_BREAKDOWN",
		ActorRef:             "usr-op-42",
		ActorRoles:           []string{"OPERATOR"},
		Justification:        "Confirmed hydraulic cylinder seal rupture on line 1 conveyor",
		EvidenceRef:          "workorder:WO-99812:photo:IMG-442",
		OccurredAt:           overrideTime,
	}

	rev2, err := ReclassifyLoss(rev1, req, tax)
	if err != nil {
		t.Fatalf("ReclassifyLoss failed: %v", err)
	}

	// Verify revision increment
	if rev2.Revision != 2 {
		t.Errorf("expected revision 2, got %d", rev2.Revision)
	}

	// Verify history chain
	if len(rev2.History) != 2 {
		t.Fatalf("expected 2 history entries, got %d", len(rev2.History))
	}
	if rev2.History[1].ActorRef != "usr-op-42" || rev2.History[1].PreviousDigest != rev1Digest {
		t.Errorf("history chain error: %+v", rev2.History[1])
	}

	// Verify updated contribution
	if rev2.Contributions[0].ReasonCode != "MECH_BREAKDOWN" {
		t.Errorf("expected MECH_BREAKDOWN, got %s", rev2.Contributions[0].ReasonCode)
	}
	if rev2.Contributions[0].Classification != ClassificationUnplanned {
		t.Errorf("expected UNPLANNED, got %s", rev2.Contributions[0].Classification)
	}

	// Verify PRIOR EVIDENCE IS IMMUTABLE (rev1 is unchanged)
	if rev1.Revision != 1 {
		t.Errorf("rev1 was mutated: revision is %d", rev1.Revision)
	}
	if rev1.Contributions[0].ReasonCode != "UNCLASSIFIED" {
		t.Errorf("rev1 contributions were mutated!")
	}
	if rev1.Digest != rev1Digest {
		t.Errorf("rev1 digest mutated!")
	}
	if rev1.LossTree.Digest != rev1TreeDigest {
		t.Errorf("rev1 loss tree mutated!")
	}

	// Audit record in rev2
	if len(rev2.AuditLog) < 2 {
		t.Fatalf("expected audit entries in rev2")
	}
	lastAudit := rev2.AuditLog[len(rev2.AuditLog)-1]
	if lastAudit.Status != "SUCCESS" || lastAudit.Action != "RECLASSIFY_SUCCESS" {
		t.Errorf("unexpected audit status: %+v", lastAudit)
	}
}

func TestGovernedReclassification_UnauthorizedAndAmbiguousFailClosed(t *testing.T) {
	tenantID := "tenant-security"
	tax := makeTestTaxonomy(tenantID)
	wStart := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	wEnd := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	window, _ := NewHalfOpenInterval(wStart, wEnd)

	int1, _ := NewHalfOpenInterval(wStart, wStart.Add(600*time.Second))
	res, err := ExplainLosses(LossExplanationInput{
		TenantID:       tenantID,
		Window:         window,
		StoppageEvents: []RawStoppageFact{{EventID: "evt-1", ReasonCode: "MECH_BREAKDOWN", Interval: int1}},
		Taxonomy:       tax,
		RecordedAt:     testRecordedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	valid := ReclassifyRequest{
		TenantID:             tenantID,
		OccurredAt:           testRecordedAt,
		BaseRevision:         1,
		TargetContributionID: res.Contributions[0].ContributionID,
		NewReasonCode:        "CHANGEOVER",
		ActorRef:             "op-1",
		ActorRoles:           []string{"OPERATOR"},
		Justification:        "Valid reason",
		EvidenceRef:          "ev1",
	}
	for _, tc := range []struct {
		name   string
		change func(*ReclassifyRequest)
		want   error
		audit  string // expected denied audit status; "" when the denial carries none
	}{
		{"empty actor", func(r *ReclassifyRequest) { r.ActorRef = "" }, ErrUnauthorizedOverride, "UNAUTHORIZED"},
		{"read-only role", func(r *ReclassifyRequest) { r.ActorRef, r.ActorRoles = "guest-user", []string{"VIEWER"} }, ErrUnauthorizedOverride, "UNAUTHORIZED"},
		{"blank justification", func(r *ReclassifyRequest) { r.Justification = "   " }, ErrAmbiguousOverride, "AMBIGUOUS"},
		{"missing evidence", func(r *ReclassifyRequest) { r.EvidenceRef = "" }, ErrAmbiguousOverride, "AMBIGUOUS"},
		{"reason not in taxonomy", func(r *ReclassifyRequest) { r.NewReasonCode = "MADE_UP_CODE" }, ErrAmbiguousOverride, "AMBIGUOUS"},
		{"retired reason", func(r *ReclassifyRequest) { r.NewReasonCode = "LEGACY_CODE" }, ErrAmbiguousOverride, "AMBIGUOUS"},
		{"base revision mismatch", func(r *ReclassifyRequest) { r.BaseRevision = 99 }, ErrRevisionConflict, ""},
	} {
		req := valid
		tc.change(&req)
		_, err := ReclassifyLoss(res, req, tax)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
		if tc.audit != "" && deniedAudit(err).Status != tc.audit {
			t.Errorf("%s: audit status %q, want %q", tc.name, deniedAudit(err).Status, tc.audit)
		}
	}
}
func TestClassifyLossWithTaxonomy_FailClosed(t *testing.T) {
	tenantID := "tenant-classify"
	tax := makeTestTaxonomy(tenantID)

	// Valid lookup
	node, err := ClassifyLossWithTaxonomy("CHANGEOVER", tax, testRecordedAt)
	if err != nil {
		t.Fatalf("expected valid node, got %v", err)
	}
	if node.ReasonCode != "CHANGEOVER" || node.Classification != ClassificationPlanned {
		t.Errorf("unexpected node: %+v", node)
	}

	// Unknown lookup
	_, err = ClassifyLossWithTaxonomy("UNKNOWN_RANDOM", tax, testRecordedAt)
	if err != ErrReasonNotFound || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected error for unknown reason, got %v", err)
	}

	// Retired reasons, drafts and instants outside the taxonomy window never classify.
	if _, err = ClassifyLossWithTaxonomy("LEGACY_CODE", tax, testRecordedAt); err != ErrReasonNotFound {
		t.Errorf("retired reason must not classify, got %v", err)
	}
	if _, err = ClassifyLossWithTaxonomy("CHANGEOVER", tax, tax.EffectiveWindow.End.Add(time.Hour)); err != ErrTaxonomyNotEffective {
		t.Errorf("instant outside the taxonomy window must fail closed, got %v", err)
	}
	draft := tax
	draft.State = ReasonStateDraft
	if _, err = ClassifyLossWithTaxonomy("CHANGEOVER", draft, testRecordedAt); err != ErrTaxonomyNotEffective {
		t.Errorf("draft taxonomy must fail closed, got %v", err)
	}

	// Unseeded taxonomy fails closed
	_, err = ClassifyLossWithTaxonomy("CHANGEOVER", TaxonomyRevision{}, testRecordedAt)
	if err != ErrUnseededTaxonomy {
		t.Errorf("expected ErrUnseededTaxonomy, got %v", err)
	}
}

// deniedAudit returns the audit entry a refused override carries.
func deniedAudit(err error) AuditEntry {
	var d *ReclassifyDenied
	if errors.As(err, &d) {
		return d.Audit
	}
	return AuditEntry{}
}
