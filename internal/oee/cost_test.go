package oee

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"math/rand"
	"strings"
	"testing"
	"time"
)

var (
	costT0    = time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	costLine  = "11111111-1111-4111-8111-111111111111"
	costAsset = "22222222-2222-4222-8222-222222222222"
	costOther = "33333333-3333-4333-8333-333333333333"
	rateID    = "44444444-4444-4444-8444-444444444444"
	rateID2   = "55555555-5555-4555-8555-555555555555"
	fxID      = "66666666-6666-4666-8666-666666666666"
)

func costWindow() HalfOpenInterval {
	w, _ := NewHalfOpenInterval(costT0, costT0.Add(8*time.Hour))
	return w
}

func approvedRate() RateRevision {
	return RateRevision{
		RateID: rateID, Revision: 1, Approved: true, AssetID: costAsset, Currency: "USD",
		HourlyRate: "1200", BurdenRatio: "0.25", UnitMargin: "3.5",
		EffectiveWindow: HalfOpenInterval{Start: costT0.Add(-24 * time.Hour), End: costT0.Add(30 * 24 * time.Hour)},
	}
}

// goldenCostInput: a 40-minute breakdown (2/3 h x 1200 x 1.25 = 1000) and 300 scrapped
// units (300 x 3.5 = 1050), total 2050.00 USD.
func goldenCostInput() CostInput {
	down, _ := NewHalfOpenInterval(costT0.Add(4*time.Hour), costT0.Add(4*time.Hour+40*time.Minute))
	return CostInput{
		Window: costWindow(),
		Contributions: []LossContribution{
			{ContributionID: "c-down", ReasonCode: "MECH_BREAKDOWN", LossCategory: LossCategoryAvailability, Classification: ClassificationUnplanned, Interval: &down, DurationSeconds: big.NewRat(2400, 1), LostQuantity: new(big.Rat)},
			{ContributionID: "c-scrap", ReasonCode: "SCRAP", LossCategory: LossCategoryQuality, Classification: ClassificationUnplanned, DurationSeconds: new(big.Rat), LostQuantity: big.NewRat(300, 1)},
		},
		Attribution: map[string]ContributionAttribution{
			"c-down":  {LineID: costLine, AssetID: costAsset, ProductID: "P-1", OrderID: "O-7"},
			"c-scrap": {LineID: costLine, AssetID: costAsset, ProductID: "P-1", OrderID: "O-7"},
		},
		Policy: CostPolicy{PolicyRevision: 3, Currency: "USD", MinorUnits: 2, Rates: []RateRevision{approvedRate()}},
	}
}

func mustCost(t *testing.T, in CostInput) CostResult {
	t.Helper()
	r, err := ModelCostOfLoss(in)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCostGoldenAndAssumptionsVisible(t *testing.T) {
	r := mustCost(t, goldenCostInput())
	if r.Status != StatusReady || r.ModeledOpportunity != "2050.00" || r.ExactAmount != "2050" {
		t.Fatalf("golden: %s %s %s %v", r.Status, r.ModeledOpportunity, r.ExactAmount, r.CostReasonCodes)
	}
	byID := map[string]CostLine{}
	for _, l := range r.Lines {
		byID[l.ContributionID] = l
	}
	if byID["c-down"].ModeledOpportunity != "1000.00" || byID["c-down"].Basis != "LOST_TIME_HOURS" || byID["c-down"].MeasureExact != "2/3" {
		t.Fatalf("downtime line: %+v", byID["c-down"])
	}
	if byID["c-scrap"].ModeledOpportunity != "1050.00" || byID["c-scrap"].Basis != "LOST_UNITS" {
		t.Fatalf("scrap line: %+v", byID["c-scrap"])
	}
	// Every monetary result names the exact rate revision and its assumptions.
	for _, l := range r.Lines {
		if l.RateID != rateID || l.RateRevision != 1 {
			t.Fatalf("line without its rate: %+v", l)
		}
	}
	if len(r.RateAssumptions) != 1 || r.RateAssumptions[0].HourlyRate != "1200" || r.RateAssumptions[0].BurdenRatio != "0.25" || r.RateAssumptions[0].EffectiveStart == "" {
		t.Fatalf("assumptions: %+v", r.RateAssumptions)
	}
}

func TestCostFailsClosedOnMissingExpiredConflictingRates(t *testing.T) {
	cases := map[string]struct {
		change func(*CostInput)
		code   CostReasonCode
	}{
		"rate for another asset": {func(in *CostInput) { in.Policy.Rates[0].AssetID = costOther }, CostMissingRate},
		"unapproved rate":        {func(in *CostInput) { in.Policy.Rates[0].Approved = false }, CostMissingRate},
		"expired rate": {func(in *CostInput) {
			in.Policy.Rates[0].EffectiveWindow = HalfOpenInterval{Start: costT0.Add(-48 * time.Hour), End: costT0.Add(-24 * time.Hour)}
		}, CostExpiredRate},
		"rate ending inside the window": {func(in *CostInput) {
			in.Policy.Rates[0].EffectiveWindow = HalfOpenInterval{Start: costT0.Add(-48 * time.Hour), End: costT0.Add(time.Hour)}
		}, CostExpiredRate},
		"two general rates": {func(in *CostInput) {
			second := approvedRate()
			second.RateID, second.HourlyRate = rateID2, "900"
			in.Policy.Rates = append(in.Policy.Rates, second)
		}, CostConflictingRate},
		"missing attribution":          {func(in *CostInput) { delete(in.Attribution, "c-down") }, CostMissingAttribution},
		"rate without an hourly price": {func(in *CostInput) { in.Policy.Rates[0].HourlyRate = "" }, CostMissingRate},
	}
	for name, c := range cases {
		in := goldenCostInput()
		in.Policy.Rates = append([]RateRevision(nil), in.Policy.Rates...)
		in.Attribution = map[string]ContributionAttribution{"c-down": in.Attribution["c-down"], "c-scrap": in.Attribution["c-scrap"]}
		c.change(&in)
		r := mustCost(t, in)
		found := false
		for _, code := range r.CostReasonCodes {
			found = found || code == c.code
		}
		if r.Status != StatusNotReady || !found {
			t.Errorf("%s: status %s codes %v", name, r.Status, r.CostReasonCodes)
		}
		// The unpriced line stays visible and is never priced at zero.
		for _, l := range r.Lines {
			if len(l.CostReasonCodes) > 0 && (l.ModeledOpportunity != "" || l.ExactAmount != "") {
				t.Errorf("%s: an unpriced line carries an amount: %+v", name, l)
			}
		}
	}
	// A category-specific rate takes precedence over a general one: no conflict.
	in := goldenCostInput()
	specific := approvedRate()
	specific.RateID, specific.LossCategory, specific.UnitMargin = rateID2, LossCategoryQuality, "4"
	in.Policy.Rates = append(in.Policy.Rates, specific)
	r := mustCost(t, in)
	if r.Status != StatusReady || r.ModeledOpportunity != "2200.00" {
		t.Fatalf("specific over general: %s %s %v", r.Status, r.ModeledOpportunity, r.CostReasonCodes)
	}
}

func TestCostNeverConvertsCurrencyImplicitly(t *testing.T) {
	in := goldenCostInput()
	in.Policy.Rates[0].Currency = "EUR"
	r := mustCost(t, in)
	if r.Status != StatusNotReady || r.CostReasonCodes[0] != CostCurrencyMismatch || r.ExactAmount != "0" {
		t.Fatalf("no FX: %s %v %s", r.Status, r.CostReasonCodes, r.ExactAmount)
	}
	fx := FXRevision{FXID: fxID, Revision: 2, Approved: true, From: "EUR", To: "USD", Rate: "1.1", EffectiveWindow: in.Policy.Rates[0].EffectiveWindow}
	in.Policy.FX = []FXRevision{fx}
	r = mustCost(t, in)
	if r.Status != StatusReady || r.ModeledOpportunity != "2255.00" || len(r.FXAssumptions) != 1 || r.Lines[0].FXID != fxID {
		t.Fatalf("approved FX: %s %s %+v", r.Status, r.ModeledOpportunity, r.FXAssumptions)
	}
	in.Policy.FX = []FXRevision{fx, {FXID: rateID2, Revision: 1, Approved: true, From: "EUR", To: "USD", Rate: "1.2", EffectiveWindow: fx.EffectiveWindow}}
	if r = mustCost(t, in); r.CostReasonCodes[0] != CostConflictingFX {
		t.Fatalf("two FX revisions: %v", r.CostReasonCodes)
	}
	in.Policy.FX = []FXRevision{fx}
	in.Policy.FX[0].Approved = false
	if r = mustCost(t, in); r.CostReasonCodes[0] != CostCurrencyMismatch {
		t.Fatalf("unapproved FX: %v", r.CostReasonCodes)
	}
}

func TestCostPolicyValidation(t *testing.T) {
	for name, change := range map[string]func(*CostPolicy){
		"seven decimals": func(p *CostPolicy) { p.Rates[0].HourlyRate = "1.1234567" },
		"negative":       func(p *CostPolicy) { p.Rates[0].UnitMargin = "-1" },
		"trailing zero":  func(p *CostPolicy) { p.Rates[0].BurdenRatio = "0.250" },
		"lowercase ccy":  func(p *CostPolicy) { p.Currency = "usd" },
		"minor units 7":  func(p *CostPolicy) { p.MinorUnits = 7 },
		"no revision":    func(p *CostPolicy) { p.PolicyRevision = 0 },
		"duplicate rate": func(p *CostPolicy) { p.Rates = append(p.Rates, p.Rates[0]) },
		"prices nothing": func(p *CostPolicy) { p.Rates[0].HourlyRate, p.Rates[0].UnitMargin = "", "" },
		"bad rate id":    func(p *CostPolicy) { p.Rates[0].RateID = "rate-1" },
		"fx zero": func(p *CostPolicy) {
			p.FX = []FXRevision{{FXID: fxID, Revision: 1, From: "EUR", To: "USD", Rate: "0", EffectiveWindow: costWindow()}}
		},
		"fx same currency": func(p *CostPolicy) {
			p.FX = []FXRevision{{FXID: fxID, Revision: 1, From: "USD", To: "USD", Rate: "1", EffectiveWindow: costWindow()}}
		},
	} {
		in := goldenCostInput()
		in.Policy.Rates = append([]RateRevision(nil), in.Policy.Rates...)
		change(&in.Policy)
		if _, err := ModelCostOfLoss(in); !errors.Is(err, ErrInvalidCostPolicy) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Rounded amounts reconcile exactly at every level of the hierarchy, and the total is the
// half-even rounding of the exact sum. Order of contributions never changes the result.
func TestCostAllocationReconcilesExactly(t *testing.T) {
	check := func(t *testing.T, rows []AllocationRow) {
		children := map[string]*big.Rat{}
		amount := map[string]string{}
		for _, r := range rows {
			amount[r.Path] = r.Amount
			if r.ParentPath == "" {
				continue
			}
			if children[r.ParentPath] == nil {
				children[r.ParentPath] = new(big.Rat)
			}
			v, _ := new(big.Rat).SetString(r.Amount)
			children[r.ParentPath].Add(children[r.ParentPath], v)
		}
		if len(children) == 0 {
			t.Fatal("no hierarchy")
		}
		for parent, sum := range children {
			p, _ := new(big.Rat).SetString(amount[parent])
			if sum.Cmp(p) != 0 {
				t.Fatalf("%s: children sum %s, parent %s", parent, sum.FloatString(6), amount[parent])
			}
		}
	}
	assets := []string{costAsset, costOther}
	for seed := int64(1); seed <= 150; seed++ {
		rng := rand.New(rand.NewSource(seed))
		in := goldenCostInput()
		in.Contributions, in.Attribution = nil, map[string]ContributionAttribution{}
		r1, r2 := approvedRate(), approvedRate()
		r2.RateID, r2.AssetID = rateID2, costOther
		r1.HourlyRate, r2.HourlyRate = fmt.Sprint(rng.Intn(997)+3), fmt.Sprint(rng.Intn(997)+3)
		in.Policy.Rates = []RateRevision{r1, r2}
		in.Policy.MinorUnits = rng.Intn(3)
		for i := 0; i < rng.Intn(25)+1; i++ {
			id := fmt.Sprintf("c-%03d", i)
			// Durations in thirds of a second make every amount a repeating fraction.
			in.Contributions = append(in.Contributions, LossContribution{ContributionID: id, ReasonCode: fmt.Sprintf("R%d", rng.Intn(4)), LossCategory: LossCategoryAvailability, Classification: ClassificationUnplanned, DurationSeconds: big.NewRat(int64(rng.Intn(5000)+1), 3), LostQuantity: new(big.Rat)})
			in.Attribution[id] = ContributionAttribution{LineID: costLine, AssetID: assets[rng.Intn(2)], ProductID: fmt.Sprintf("P-%d", rng.Intn(3)), OrderID: fmt.Sprintf("O-%d", rng.Intn(3))}
		}
		r := mustCost(t, in)
		check(t, r.Allocation)
		exact, _ := new(big.Rat).SetString(r.ExactAmount)
		unit := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(in.Policy.MinorUnits)), nil))
		if want := formatMinor(roundHalfEvenScaled(exact, unit), in.Policy.MinorUnits); r.ModeledOpportunity != want {
			t.Fatalf("seed %d: total %s, want %s", seed, r.ModeledOpportunity, want)
		}
		lineSum := new(big.Rat)
		for _, l := range r.Lines {
			v, _ := new(big.Rat).SetString(l.ModeledOpportunity)
			lineSum.Add(lineSum, v)
		}
		if total, _ := new(big.Rat).SetString(r.ModeledOpportunity); lineSum.Cmp(total) != 0 {
			t.Fatalf("seed %d: lines sum %s, total %s", seed, lineSum.FloatString(6), r.ModeledOpportunity)
		}
		shuffled := in
		shuffled.Contributions = append([]LossContribution(nil), in.Contributions...)
		rng.Shuffle(len(shuffled.Contributions), func(i, j int) {
			shuffled.Contributions[i], shuffled.Contributions[j] = shuffled.Contributions[j], shuffled.Contributions[i]
		})
		a, _ := json.Marshal(r)
		b, _ := json.Marshal(mustCost(t, shuffled))
		if string(a) != string(b) {
			t.Fatalf("seed %d: contribution order changed the result", seed)
		}
	}
}

// The model computes only a modeled opportunity; avoided loss and realized savings are
// never inferred, and nothing labels the opportunity as savings.
func TestCostNeverClaimsSavings(t *testing.T) {
	r := mustCost(t, goldenCostInput())
	if r.AvoidedLoss != nil || r.RealizedSavings != nil || len(r.NotComputed) != 2 {
		t.Fatalf("savings inferred: %+v", r)
	}
	b, _ := json.Marshal(r)
	if !strings.Contains(string(b), `"avoided_loss":null`) || !strings.Contains(string(b), `"realized_savings":null`) {
		t.Fatalf("savings fields must be explicit nulls: %s", b)
	}
}
