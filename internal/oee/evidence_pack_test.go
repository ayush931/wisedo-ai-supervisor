package oee

import (
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const (
	packTenant   = "0a0a0a0a-0a0a-4a0a-8a0a-0a0a0a0a0a0a"
	packSite     = "1a1a1a1a-1a1a-4a1a-8a1a-1a1a1a1a1a1a"
	packEvidence = "0d0d0d0d-0d0d-4d0d-8d0d-0d0d0d0d0d0d"
	packRun      = "0e0e0e0e-0e0e-4e0e-8e0e-0e0e0e0e0e0e"
	packTaxonomy = "0f0f0f0f-0f0f-4f0f-8f0f-0f0f0f0f0f0f"
)

// packTaxonomyRevision is the loss-test taxonomy with a fixed id, so digests are stable.
func packTaxonomyRevision() TaxonomyRevision {
	tax := makeTestTaxonomy(packTenant)
	tax.TaxonomyID = uuid.MustParse(packTaxonomy)
	tax.SiteID = packSite
	tax.TaxonomyDigest, _ = ComputeTaxonomyDigest(tax)
	return tax
}

// packInput runs the real pipeline on the cost golden day: an 8-hour window with a
// 40-minute breakdown and 300 scrapped units out of 19,500.
func packInput(t *testing.T) EvidencePackInput {
	t.Helper()
	window := costWindow()
	down, _ := NewHalfOpenInterval(costT0.Add(4*time.Hour), costT0.Add(4*time.Hour+40*time.Minute))
	tax := packTaxonomyRevision()
	losses, err := ExplainLosses(LossExplanationInput{
		TenantID: packTenant, SiteID: packSite, Window: window, Taxonomy: tax, PolicyRevisionID: "pol-3", RecordedAt: costT0.Add(9 * time.Hour),
		StoppageEvents:    []RawStoppageFact{{EventID: "evt-down", ReasonCode: "MECH_BREAKDOWN", Interval: down}},
		QualityLossEvents: []RawQualityLossFact{{EventID: "evt-scrap", ReasonCode: "SCRAP", LostQuantity: big.NewRat(300, 1), Unit: "pcs"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := DefaultKPIPolicy(uuid.MustParse("12121212-1212-4212-8212-121212121212"))
	run1, _ := NewHalfOpenInterval(costT0, down.Start)
	run2, _ := NewHalfOpenInterval(down.End, window.End)
	timeline := TimelineResolutionResult{
		Window: window,
		Segments: []CanonicalFactSegment{
			{Interval: run1, State: StateRunning, EventID: "evt-run-1"},
			{Interval: down, State: StateUnplannedDowntime, EventID: "evt-down"},
			{Interval: run2, State: StateRunning, EventID: "evt-run-2"},
		},
		Readiness: ReadinessDecision{Status: StatusReady, ReasonCodes: []ReasonCode{}}, CoverageRatio: big.NewRat(1, 1), GapSeconds: new(big.Rat),
	}
	kpi, err := CalculateKPIs(KPICalculationInput{
		Window: window, Timeline: timeline, Policy: policy,
		CountContributions: []ProductCountContribution{{ContributionID: "cnt", ProductID: "P-1", Interval: window, TotalCount: big.NewRat(19500, 1), ScrapCount: big.NewRat(300, 1), Unit: "pcs"}},
		IdealCycles:        []IdealCycleRevision{{ProductID: "P-1", Revision: 1, SecondsPerUnit: big.NewRat(1, 1), Unit: "s/unit"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	costIn := goldenCostInput()
	costIn.Contributions = losses.Contributions
	costIn.Attribution = map[string]ContributionAttribution{}
	for _, c := range losses.Contributions {
		costIn.Attribution[c.ContributionID] = ContributionAttribution{LineID: costLine, AssetID: costAsset, ProductID: "P-1", OrderID: "O-7"}
	}
	cost, err := ModelCostOfLoss(costIn)
	if err != nil {
		t.Fatal(err)
	}
	return EvidencePackInput{
		TenantID: packTenant, SiteID: packSite, EvidenceID: packEvidence, RunID: packRun, CalculatedAt: costT0.Add(9 * time.Hour),
		Policy: policy, KPI: kpi, Losses: losses, Timeline: timeline, Cost: cost, NormalizedInputDigest: strings.Repeat("c", 64),
	}
}

func mustPack(t *testing.T, in EvidencePackInput) OeeEvidencePackV1 {
	t.Helper()
	p, err := BuildOeeEvidencePack(in)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEvidencePackContentsAndDigest(t *testing.T) {
	in := packInput(t)
	p := mustPack(t, in)
	if p.Status != StatusReady || p.Cost.ModeledOpportunity != "2050.00" || p.Durations.UnplannedLoss != "2400" || len(p.DowntimePareto) == 0 {
		t.Fatalf("pack: status %s cost %s unplanned %s pareto %d", p.Status, p.Cost.ModeledOpportunity, p.Durations.UnplannedLoss, len(p.DowntimePareto))
	}
	if p.Versions.TaxonomyDigest == "" || p.Versions.PolicyDigest == "" || p.Versions.CostPolicyRevision != 3 || len(p.Versions.RateRevisions) != 1 ||
		p.Digests.Calculation != in.KPI.CalculationDigest || p.Digests.Classification != in.Losses.Digest || len(p.Sources.SourceEventIDs) != 2 {
		t.Fatalf("versions and provenance: %+v %+v %+v", p.Versions, p.Digests, p.Sources)
	}
	if d, err := OeeEvidencePackDigest(packTenant, p); err != nil || d != p.Digest {
		t.Fatalf("consumer cannot verify the digest: %v", err)
	}
	if d, _ := OeeEvidencePackDigest("2a2a2a2a-2a2a-4a2a-8a2a-2a2a2a2a2a2a", p); d == p.Digest {
		t.Fatal("digest is portable across tenants")
	}
	again := mustPack(t, packInput(t))
	a, _ := json.Marshal(p)
	b, _ := json.Marshal(again)
	if string(a) != string(b) {
		t.Fatal("identical inputs produced different packs")
	}
	// Rule 04: the pack carries the caller's calculation time and its generated IDs, but they
	// are excluded from the digest, so identical inputs sealed later share one digest.
	later := packInput(t)
	later.CalculatedAt = later.CalculatedAt.Add(time.Hour)
	later.EvidenceID, later.RunID = "5b5b5b5b-5b5b-4b5b-8b5b-5b5b5b5b5b5b", "6c6c6c6c-6c6c-4c6c-8c6c-6c6c6c6c6c6c"
	sealedLater := mustPack(t, later)
	if sealedLater.Digest != p.Digest || sealedLater.CalculatedAt == p.CalculatedAt {
		t.Fatal("creation time or generated IDs changed the pack digest")
	}
}

func TestEvidencePackFailsClosed(t *testing.T) {
	for name, change := range map[string]func(*EvidencePackInput){
		"another tenant's losses": func(in *EvidencePackInput) {
			c := *in.Losses
			c.TenantID = "2a2a2a2a-2a2a-4a2a-8a2a-2a2a2a2a2a2a"
			in.Losses = &c
		},
		"another site's losses": func(in *EvidencePackInput) {
			c := *in.Losses
			c.SiteID = "2a2a2a2a-2a2a-4a2a-8a2a-2a2a2a2a2a2a"
			in.Losses = &c
		},
		"no losses":           func(in *EvidencePackInput) { in.Losses = nil },
		"unsealed KPI":        func(in *EvidencePackInput) { in.KPI.CalculationDigest = "" },
		"no calculation time": func(in *EvidencePackInput) { in.CalculatedAt = time.Time{} },
		"bad input digest":    func(in *EvidencePackInput) { in.NormalizedInputDigest = "abc" },
		"bad evidence id":     func(in *EvidencePackInput) { in.EvidenceID = "e-1" },
		// The pack records policy flags; they must be those of the policy that produced the KPI.
		"policy that did not produce the KPI": func(in *EvidencePackInput) { in.Policy.ReworkCountsAsGood = !in.Policy.ReworkCountsAsGood },
		"mismatched windows": func(in *EvidencePackInput) {
			c := *in.Losses
			c.Window.End = c.Window.End.Add(time.Hour)
			in.Losses = &c
		},
	} {
		in := packInput(t)
		change(&in)
		if _, err := BuildOeeEvidencePack(in); !errors.Is(err, ErrInvalidEvidenceInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A cost gap makes the pack LIMITED; it is never a READY pack with missing money.
	in := packInput(t)
	in.Cost.Status = StatusNotReady
	if p := mustPack(t, in); p.Status != StatusLimited {
		t.Fatalf("cost gap: %s", p.Status)
	}
}

// Provenance is identifiers and digests only: no actor identities or justifications.
func TestEvidencePackIsPrivacySafe(t *testing.T) {
	in := packInput(t)
	rev2, err := ReclassifyLoss(in.Losses, ReclassifyRequest{
		TenantID: packTenant, SiteID: packSite, BaseRevision: 1, TargetContributionID: "evt-down", NewReasonCode: "ELEC_BREAKDOWN",
		ActorRef: "jane.doe@plant.example", ActorRoles: []string{"SUPERVISOR"}, Justification: "Confidential: contractor error on site", EvidenceRef: "workorder:91",
		OccurredAt: costT0.Add(10 * time.Hour),
	}, packTaxonomyRevision())
	if err != nil {
		t.Fatal(err)
	}
	in.Losses = rev2
	b, _ := json.Marshal(mustPack(t, in))
	for _, secret := range []string{"jane.doe", "Confidential", "contractor"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("pack leaks %q", secret)
		}
	}
}

// The WIS-221 consumer fixture: Reporting renders the committed pack to CSV without a
// database or any OEE recomputation, verifies its digest, and gets formula-safe cells.
// Regenerate deliberately with UPDATE_EVIDENCE_FIXTURE=1 and review the diff.
func TestReportingConsumerFixture(t *testing.T) {
	dir := filepath.Join("testdata", "evidence-pack")
	if os.Getenv("UPDATE_EVIDENCE_FIXTURE") == "1" {
		p := mustPack(t, packInput(t))
		b, _ := json.MarshalIndent(p, "", "  ")
		csvBytes, err := RenderEvidenceCSV(p)
		if err != nil {
			t.Fatal(err)
		}
		_ = os.MkdirAll(dir, 0o755)
		if err = os.WriteFile(filepath.Join(dir, "pack.json"), append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, "pack.csv"), csvBytes, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "pack.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pack OeeEvidencePackV1
	if err = json.Unmarshal(raw, &pack); err != nil {
		t.Fatal(err)
	}
	if d, err := OeeEvidencePackDigest(packTenant, pack); err != nil || d != pack.Digest {
		t.Fatalf("fixture digest does not verify: %v", err)
	}
	got, err := RenderEvidenceCSV(pack)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join(dir, "pack.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("CSV rendering drifted:\n%s", got)
	}
	if !strings.Contains(string(got), "cost,modeled_opportunity,2050.00") || !strings.Contains(string(got), "cost,realized_savings,not computed") {
		t.Fatalf("monetary rows: %s", got)
	}
	// The committed pack is what the builder produces today.
	if live := mustPack(t, packInput(t)); live.Digest != pack.Digest {
		t.Fatal("pack.json is stale: regenerate with UPDATE_EVIDENCE_FIXTURE=1")
	}
}

func TestRenderEvidenceCSVIsFormulaSafe(t *testing.T) {
	p := mustPack(t, packInput(t))
	p.DowntimePareto[0].Name = `=HYPERLINK("http://x.invalid","click")`
	p.DowntimePareto[0].ReasonCode = "+CMD"
	p.Cost.Lines[0].ReasonCode = "@SUM(A1)"
	out, err := RenderEvidenceCSV(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		for _, cell := range strings.Split(line, ",") {
			cell = strings.Trim(cell, `"`)
			if cell != "" && strings.ContainsRune("=+-@", rune(cell[0])) {
				t.Fatalf("formula cell %q in %q", cell, line)
			}
		}
	}
	for _, label := range []string{"savings,2050", "savings,1"} {
		if strings.Contains(string(out), label) {
			t.Fatalf("a modeled opportunity is labelled as savings: %s", out)
		}
	}
}

func TestReconciliationTemplate(t *testing.T) {
	p := mustPack(t, packInput(t))
	same := ReferenceCalculation{Source: "customer spreadsheet v4", Availability: p.KPIs.Availability.Value, Performance: p.KPIs.Performance.Value,
		Quality: p.KPIs.Quality.Value, OEE: p.KPIs.OEE.Value, PlannedBreaksExcluded: flag(true)}
	r, err := Reconcile(p, same)
	if err != nil || r.Unexplained || len(r.PolicyVariances) != 0 {
		t.Fatalf("identical reference: %+v %v", r, err)
	}
	for _, row := range r.Rows {
		if !row.Matches || row.Difference != "0.000000" {
			t.Fatalf("identical row: %+v", row)
		}
	}
	// A reference that counts rework as good and microstops against availability differs,
	// and the report names both policy differences.
	diff := same
	diff.Quality, diff.ReworkCountsAsGood, diff.MicrostopsAsAvailabilityLoss = "0.999000", flag(true), flag(true)
	r, _ = Reconcile(p, diff)
	if r.Unexplained || len(r.PolicyVariances) != 2 || r.Rows[2].Matches {
		t.Fatalf("explained difference: %+v", r)
	}
	// The same difference with no policy variance is flagged unexplained.
	unexplained := same
	unexplained.Quality = "0.999000"
	if r, _ = Reconcile(p, unexplained); !r.Unexplained {
		t.Fatalf("unexplained difference: %+v", r)
	}
	// A reference that states no policy choices produces no variance: an availability
	// mismatch is unexplained, not excused by a defaulted "planned breaks" flag (rule 06).
	silent := ReferenceCalculation{Source: "spreadsheet", Availability: "0.500000", Performance: p.KPIs.Performance.Value, Quality: p.KPIs.Quality.Value, OEE: p.KPIs.OEE.Value}
	if r, _ = Reconcile(p, silent); !r.Unexplained || len(r.PolicyVariances) != 0 {
		t.Fatalf("unstated policy excused a mismatch: %+v", r)
	}
	// A variance only explains the metrics it can move: a rework difference (quality and
	// OEE) does not explain an availability mismatch.
	unrelated := same
	unrelated.Availability, unrelated.ReworkCountsAsGood = "0.500000", flag(!p.Versions.ReworkCountsAsGood)
	if r, _ = Reconcile(p, unrelated); !r.Unexplained || len(r.PolicyVariances) != 1 {
		t.Fatalf("an unrelated variance excused an availability mismatch: %+v", r)
	}
}

func flag(b bool) *bool { return &b }
