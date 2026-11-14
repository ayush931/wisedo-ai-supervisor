package oee

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"time"

	"github.com/google/uuid"
)

// Cost of loss (WIS-228). Rates are governed policy inputs administered by an authorized
// customer role (ADR 0001 Sec. 130): they are not ledger truth, never imply realized savings,
// and are never converted between currencies implicitly. The model computes only a modeled
// opportunity; avoided loss and realized savings need a measured outcome and are not computed.

// ErrInvalidCostPolicy means a rate, FX revision or cost policy breaks its contract.
var ErrInvalidCostPolicy = errors.New("invalid cost policy")

// CostReasonCode explains why a contribution could not be priced. The set is closed.
type CostReasonCode string

const (
	CostMissingRate        CostReasonCode = "MISSING_RATE"
	CostExpiredRate        CostReasonCode = "EXPIRED_RATE"
	CostConflictingRate    CostReasonCode = "CONFLICTING_RATE"
	CostCurrencyMismatch   CostReasonCode = "CURRENCY_MISMATCH"
	CostConflictingFX      CostReasonCode = "CONFLICTING_FX"
	CostMissingAttribution CostReasonCode = "MISSING_ATTRIBUTION"
	CostNoMeasure          CostReasonCode = "NO_MEASURE"
)

// CostReasonCodes lists every published cost reason code in stable order.
func CostReasonCodes() []CostReasonCode {
	return []CostReasonCode{CostMissingRate, CostExpiredRate, CostConflictingRate, CostCurrencyMismatch, CostConflictingFX, CostMissingAttribution, CostNoMeasure}
}

// RateRevision is one approved, effective-dated cost assumption for an asset. HourlyRate
// prices lost operating time and is loaded by BurdenRatio (cost = hours x rate x (1 +
// burden)); UnitMargin prices lost units. Amounts are canonical decimal strings in
// Currency. LossCategory narrows the rate to one category; empty applies to all, and a
// category-specific rate takes precedence over a general one.
type RateRevision struct {
	RateID          string           `json:"rate_id"`
	Revision        int64            `json:"revision"`
	Approved        bool             `json:"approved"`
	AssetID         string           `json:"asset_id"`
	LossCategory    LossCategory     `json:"loss_category,omitempty"`
	Currency        CurrencyCode     `json:"currency"`
	HourlyRate      string           `json:"hourly_rate,omitempty"`
	BurdenRatio     string           `json:"burden_ratio,omitempty"`
	UnitMargin      string           `json:"unit_margin,omitempty"`
	EffectiveWindow HalfOpenInterval `json:"effective_window"`
}

// FXRevision is an approved exchange rate: one unit of From equals Rate units of To.
type FXRevision struct {
	FXID            string           `json:"fx_id"`
	Revision        int64            `json:"revision"`
	Approved        bool             `json:"approved"`
	From            CurrencyCode     `json:"from"`
	To              CurrencyCode     `json:"to"`
	Rate            string           `json:"rate"`
	EffectiveWindow HalfOpenInterval `json:"effective_window"`
}

// ContributionAttribution places a loss contribution in the reporting hierarchy.
type ContributionAttribution struct {
	LineID    string `json:"line_id"`
	AssetID   string `json:"asset_id"`
	ProductID string `json:"product_id,omitempty"`
	OrderID   string `json:"order_id,omitempty"`
}

// CostPolicy is the governed cost policy for one calculation.
type CostPolicy struct {
	PolicyRevision int64          `json:"policy_revision"`
	Currency       CurrencyCode   `json:"currency"`    // reporting currency
	MinorUnits     int            `json:"minor_units"` // display scale, 0..6
	Rates          []RateRevision `json:"rates"`
	FX             []FXRevision   `json:"fx"`
}

// CostInput is everything the pure cost model reads.
type CostInput struct {
	Window        HalfOpenInterval
	Contributions []LossContribution
	Attribution   map[string]ContributionAttribution // by contribution id
	Policy        CostPolicy
}

// CostLine prices one loss contribution, or says why it could not.
type CostLine struct {
	ContributionID     string             `json:"contribution_id"`
	ReasonCode         string             `json:"reason_code"`
	LossCategory       LossCategory       `json:"loss_category"`
	Classification     LossClassification `json:"classification"`
	LineID             string             `json:"line_id"`
	AssetID            string             `json:"asset_id"`
	ProductID          string             `json:"product_id"`
	OrderID            string             `json:"order_id"`
	Basis              string             `json:"basis"` // LOST_TIME_HOURS | LOST_UNITS
	MeasureExact       string             `json:"measure_exact"`
	RateID             string             `json:"rate_id,omitempty"`
	RateRevision       int64              `json:"rate_revision,omitempty"`
	FXID               string             `json:"fx_id,omitempty"`
	FXRevision         int64              `json:"fx_revision,omitempty"`
	ModeledOpportunity string             `json:"modeled_opportunity,omitempty"`
	ExactAmount        string             `json:"exact_amount,omitempty"`
	CostReasonCodes    []CostReasonCode   `json:"cost_reason_codes"`
}

// AllocationRow is one node of the line > asset > product > order > reason > contribution
// hierarchy, flattened in depth-first order. Amount is rounded to the policy's minor units
// so that the rows sharing a ParentPath always sum exactly to that parent's Amount;
// ExactAmount keeps the unrounded value.
type AllocationRow struct {
	Path        string `json:"path"`
	ParentPath  string `json:"parent_path"`
	Level       string `json:"level"`
	Key         string `json:"key"`
	ExactAmount string `json:"exact_amount"`
	Amount      string `json:"amount"`
}

// RateAssumption makes every rate behind a monetary result visible.
type RateAssumption struct {
	RateID         string       `json:"rate_id"`
	Revision       int64        `json:"revision"`
	AssetID        string       `json:"asset_id"`
	LossCategory   LossCategory `json:"loss_category,omitempty"`
	Currency       CurrencyCode `json:"currency"`
	HourlyRate     string       `json:"hourly_rate,omitempty"`
	BurdenRatio    string       `json:"burden_ratio,omitempty"`
	UnitMargin     string       `json:"unit_margin,omitempty"`
	EffectiveStart string       `json:"effective_start"`
	EffectiveEnd   string       `json:"effective_end"`
}

// FXAssumption makes every exchange rate behind a monetary result visible.
type FXAssumption struct {
	FXID     string       `json:"fx_id"`
	Revision int64        `json:"revision"`
	From     CurrencyCode `json:"from"`
	To       CurrencyCode `json:"to"`
	Rate     string       `json:"rate"`
}

// CostResult is the modeled opportunity for one explanation. AvoidedLoss and
// RealizedSavings are always nil: the core cannot infer them, and NotComputed says why.
type CostResult struct {
	Currency           CurrencyCode     `json:"currency"`
	MinorUnits         int              `json:"minor_units"`
	PolicyRevision     int64            `json:"policy_revision"`
	Status             Status           `json:"status"`
	ModeledOpportunity string           `json:"modeled_opportunity"`
	ExactAmount        string           `json:"exact_amount"`
	AvoidedLoss        *string          `json:"avoided_loss"`
	RealizedSavings    *string          `json:"realized_savings"`
	NotComputed        []string         `json:"not_computed"`
	Lines              []CostLine       `json:"lines"`
	Allocation         []AllocationRow  `json:"allocation"`
	RateAssumptions    []RateAssumption `json:"rate_assumptions"`
	FXAssumptions      []FXAssumption   `json:"fx_assumptions"`
	CostReasonCodes    []CostReasonCode `json:"cost_reason_codes"`
}

var costDecimalRE = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]{0,5}[1-9])?$`)

func costDecimal(s string) (*big.Rat, bool) {
	if !costDecimalRE.MatchString(s) {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(s)
	return r, ok
}

func canonicalCostID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id != uuid.Nil && id.String() == s
}

// validateCostPolicy rejects malformed policy inputs; a missing rate for a contribution is
// not a policy error, it is a per-line reason code.
func validateCostPolicy(p CostPolicy) error {
	bad := func(f string, a ...any) error {
		return fmt.Errorf("%w: "+f, append([]any{ErrInvalidCostPolicy}, a...)...)
	}
	if _, err := ParseCurrencyCode(string(p.Currency)); err != nil {
		return bad("currency %q", p.Currency)
	}
	if p.MinorUnits < 0 || p.MinorUnits > 6 || p.PolicyRevision < 1 {
		return bad("minor_units must be 0..6 and policy_revision >= 1")
	}
	seen := map[string]bool{}
	for _, r := range p.Rates {
		key := fmt.Sprintf("%s@%d", r.RateID, r.Revision)
		if !canonicalCostID(r.RateID) || r.Revision < 1 || seen[key] || !canonicalCostID(r.AssetID) {
			return bad("rate %s must have a canonical id, asset and unique revision", key)
		}
		seen[key] = true
		if _, err := ParseCurrencyCode(string(r.Currency)); err != nil {
			return bad("rate %s currency", key)
		}
		if r.LossCategory != "" && r.LossCategory != LossCategoryAvailability && r.LossCategory != LossCategoryPerformance && r.LossCategory != LossCategoryQuality {
			return bad("rate %s loss category", key)
		}
		for name, v := range map[string]string{"hourly_rate": r.HourlyRate, "burden_ratio": r.BurdenRatio, "unit_margin": r.UnitMargin} {
			if v != "" {
				if _, ok := costDecimal(v); !ok {
					return bad("rate %s %s must be a canonical decimal", key, name)
				}
			}
		}
		if r.HourlyRate == "" && r.UnitMargin == "" {
			return bad("rate %s prices nothing", key)
		}
		if !r.EffectiveWindow.Start.Before(r.EffectiveWindow.End) {
			return bad("rate %s effective window", key)
		}
	}
	for _, f := range p.FX {
		key := fmt.Sprintf("%s@%d", f.FXID, f.Revision)
		rate, ok := costDecimal(f.Rate)
		if !canonicalCostID(f.FXID) || f.Revision < 1 || !ok || rate.Sign() == 0 || f.From == f.To || !f.EffectiveWindow.Start.Before(f.EffectiveWindow.End) {
			return bad("fx %s", key)
		}
		if _, err := ParseCurrencyCode(string(f.From)); err != nil {
			return bad("fx %s from", key)
		}
		if _, err := ParseCurrencyCode(string(f.To)); err != nil {
			return bad("fx %s to", key)
		}
	}
	return nil
}

func covers(window, iv HalfOpenInterval) bool {
	return !iv.Start.Before(window.Start) && !iv.End.After(window.End)
}

// selectRate finds the one approved rate for an asset and category that is effective for
// the whole interval. A category-specific rate beats a general one; two at the same
// specificity conflict.
func selectRate(rates []RateRevision, assetID string, cat LossCategory, iv HalfOpenInterval) (*RateRevision, CostReasonCode) {
	var specific, general []*RateRevision
	scoped := false
	for i := range rates {
		r := &rates[i]
		if !r.Approved || r.AssetID != assetID || (r.LossCategory != "" && r.LossCategory != cat) {
			continue
		}
		scoped = true
		if !covers(r.EffectiveWindow, iv) {
			continue
		}
		if r.LossCategory == cat {
			specific = append(specific, r)
		} else {
			general = append(general, r)
		}
	}
	for _, set := range [][]*RateRevision{specific, general} {
		switch len(set) {
		case 0:
			continue
		case 1:
			return set[0], ""
		default:
			return nil, CostConflictingRate
		}
	}
	if scoped {
		return nil, CostExpiredRate
	}
	return nil, CostMissingRate
}

func selectFX(fx []FXRevision, from, to CurrencyCode, iv HalfOpenInterval) (*FXRevision, CostReasonCode) {
	var match []*FXRevision
	for i := range fx {
		f := &fx[i]
		if f.Approved && f.From == from && f.To == to && covers(f.EffectiveWindow, iv) {
			match = append(match, f)
		}
	}
	switch len(match) {
	case 0:
		return nil, CostCurrencyMismatch
	case 1:
		return match[0], ""
	}
	return nil, CostConflictingFX
}

// ModelCostOfLoss prices every loss contribution under the cost policy and allocates the
// result across the reporting hierarchy. It is pure and deterministic. Unpriceable
// contributions stay visible with their reason codes and make the result NOT_READY; they
// are never priced at zero.
func ModelCostOfLoss(in CostInput) (CostResult, error) {
	if err := validateCostPolicy(in.Policy); err != nil {
		return CostResult{}, err
	}
	if !in.Window.Start.Before(in.Window.End) {
		return CostResult{}, fmt.Errorf("%w: window", ErrInvalidCostPolicy)
	}
	contributions := append([]LossContribution(nil), in.Contributions...)
	sort.Slice(contributions, func(i, j int) bool { return contributions[i].ContributionID < contributions[j].ContributionID })

	res := CostResult{
		Currency: in.Policy.Currency, MinorUnits: in.Policy.MinorUnits, PolicyRevision: in.Policy.PolicyRevision,
		NotComputed: []string{
			"avoided_loss: requires a measured post-action outcome; the core never infers it",
			"realized_savings: requires finance-ledger confirmation; the core never infers it",
		},
		Lines: []CostLine{}, RateAssumptions: []RateAssumption{}, FXAssumptions: []FXAssumption{}, CostReasonCodes: []CostReasonCode{},
	}
	usedRates := map[string]RateRevision{}
	usedFX := map[string]FXRevision{}
	reasons := map[CostReasonCode]bool{}
	exact := map[int]*big.Rat{}
	for _, c := range contributions {
		line := CostLine{ContributionID: c.ContributionID, ReasonCode: c.ReasonCode, LossCategory: c.LossCategory, Classification: c.Classification, CostReasonCodes: []CostReasonCode{}}
		fail := func(code CostReasonCode) {
			line.CostReasonCodes = append(line.CostReasonCodes, code)
			reasons[code] = true
		}
		attr, ok := in.Attribution[c.ContributionID]
		if !ok || !canonicalCostID(attr.AssetID) || !canonicalCostID(attr.LineID) {
			fail(CostMissingAttribution)
			res.Lines = append(res.Lines, line)
			continue
		}
		line.LineID, line.AssetID, line.ProductID, line.OrderID = attr.LineID, attr.AssetID, attr.ProductID, attr.OrderID
		// Time losses are priced per lost hour; quality losses per lost unit.
		var measure *big.Rat
		switch {
		case c.LossCategory == LossCategoryQuality && c.LostQuantity != nil && c.LostQuantity.Sign() > 0:
			line.Basis, measure = "LOST_UNITS", new(big.Rat).Set(c.LostQuantity)
		case c.LossCategory != LossCategoryQuality && c.DurationSeconds != nil && c.DurationSeconds.Sign() > 0:
			line.Basis, measure = "LOST_TIME_HOURS", new(big.Rat).Quo(c.DurationSeconds, big.NewRat(3600, 1))
		default:
			fail(CostNoMeasure)
			res.Lines = append(res.Lines, line)
			continue
		}
		line.MeasureExact = measure.RatString()
		iv := in.Window
		if c.Interval != nil {
			iv = *c.Interval
		}
		rate, code := selectRate(in.Policy.Rates, attr.AssetID, c.LossCategory, iv)
		if rate == nil {
			fail(code)
			res.Lines = append(res.Lines, line)
			continue
		}
		var amount *big.Rat
		if line.Basis == "LOST_UNITS" {
			margin, ok := costDecimal(rate.UnitMargin)
			if !ok {
				fail(CostMissingRate)
				res.Lines = append(res.Lines, line)
				continue
			}
			amount = new(big.Rat).Mul(measure, margin)
		} else {
			hourly, ok := costDecimal(rate.HourlyRate)
			if !ok {
				fail(CostMissingRate)
				res.Lines = append(res.Lines, line)
				continue
			}
			burden := new(big.Rat)
			if rate.BurdenRatio != "" {
				burden, _ = costDecimal(rate.BurdenRatio)
			}
			amount = new(big.Rat).Mul(measure, new(big.Rat).Mul(hourly, new(big.Rat).Add(big.NewRat(1, 1), burden)))
		}
		line.RateID, line.RateRevision = rate.RateID, rate.Revision
		if rate.Currency != in.Policy.Currency {
			fx, code := selectFX(in.Policy.FX, rate.Currency, in.Policy.Currency, iv)
			if fx == nil {
				fail(code)
				res.Lines = append(res.Lines, line)
				continue
			}
			fxRate, _ := costDecimal(fx.Rate)
			amount.Mul(amount, fxRate)
			line.FXID, line.FXRevision = fx.FXID, fx.Revision
			usedFX[fmt.Sprintf("%s@%d", fx.FXID, fx.Revision)] = *fx
		}
		usedRates[fmt.Sprintf("%s@%d", rate.RateID, rate.Revision)] = *rate
		line.ExactAmount = amount.RatString()
		exact[len(res.Lines)] = amount
		res.Lines = append(res.Lines, line)
	}

	res.Allocation = allocate(res.Lines, exact, in.Policy.MinorUnits)
	res.ModeledOpportunity, res.ExactAmount = res.Allocation[0].Amount, res.Allocation[0].ExactAmount
	for _, key := range sortedKeys(usedRates) {
		r := usedRates[key]
		res.RateAssumptions = append(res.RateAssumptions, RateAssumption{
			RateID: r.RateID, Revision: r.Revision, AssetID: r.AssetID, LossCategory: r.LossCategory, Currency: r.Currency,
			HourlyRate: r.HourlyRate, BurdenRatio: r.BurdenRatio, UnitMargin: r.UnitMargin,
			EffectiveStart: r.EffectiveWindow.Start.UTC().Format(time.RFC3339Nano), EffectiveEnd: r.EffectiveWindow.End.UTC().Format(time.RFC3339Nano),
		})
	}
	for _, key := range sortedKeys(usedFX) {
		f := usedFX[key]
		res.FXAssumptions = append(res.FXAssumptions, FXAssumption{FXID: f.FXID, Revision: f.Revision, From: f.From, To: f.To, Rate: f.Rate})
	}
	for _, code := range CostReasonCodes() {
		if reasons[code] {
			res.CostReasonCodes = append(res.CostReasonCodes, code)
		}
	}
	res.Status = StatusReady
	if len(res.CostReasonCodes) > 0 {
		res.Status = StatusNotReady
	}
	return res, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// allocate builds the line > asset > product > order > reason > contribution hierarchy
// from priced lines. Exact amounts are summed exactly; each node's rounded amount is
// distributed to its children by the largest-remainder method, so rounded children always
// sum exactly to their rounded parent. Lines receive their rounded share in place.
func allocate(lines []CostLine, exact map[int]*big.Rat, scale int) []AllocationRow {
	root := &allocNode{level: "total", key: "total", children: map[string]*allocNode{}, exact: new(big.Rat), line: -1}
	levels := []string{"line", "asset", "product", "order", "reason", "contribution"}
	for i, l := range lines {
		amount, ok := exact[i]
		if !ok {
			continue
		}
		path := []string{l.LineID, l.AssetID, l.ProductID, l.OrderID, l.ReasonCode, l.ContributionID}
		n := root
		n.exact.Add(n.exact, amount)
		for d, key := range path {
			child, ok := n.children[key]
			if !ok {
				child = &allocNode{level: levels[d], key: key, children: map[string]*allocNode{}, exact: new(big.Rat), line: -1}
				n.children[key] = child
			}
			child.exact.Add(child.exact, amount)
			n = child
		}
		n.line = i
	}
	unit := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil))
	root.rounded = roundHalfEvenScaled(root.exact, unit)
	root.distribute(unit)
	var rows []AllocationRow
	root.export(scale, "", &rows)
	root.assign(lines, scale)
	return rows
}

type allocNode struct {
	level, key string
	exact      *big.Rat
	rounded    *big.Int // in minor units
	children   map[string]*allocNode
	line       int
}

func roundHalfEvenScaled(v *big.Rat, unit *big.Rat) *big.Int {
	scaled := new(big.Rat).Mul(v, unit)
	q, r := new(big.Int).QuoRem(scaled.Num(), scaled.Denom(), new(big.Int))
	twice := new(big.Int).Mul(r, big.NewInt(2))
	if c := twice.Cmp(scaled.Denom()); c > 0 || (c == 0 && q.Bit(0) == 1) {
		q.Add(q, big.NewInt(1))
	}
	return q
}

func (n *allocNode) sortedChildren() []*allocNode {
	out := make([]*allocNode, 0, len(n.children))
	for _, c := range n.children {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

// distribute gives each child floor(exact) minor units, then hands the remaining units of
// the parent's rounded amount to the children with the largest fractional parts (ties by
// key). Amounts are non-negative, so the remainder is between 0 and the child count.
func (n *allocNode) distribute(unit *big.Rat) {
	children := n.sortedChildren()
	if len(children) == 0 {
		return
	}
	type share struct {
		c    *allocNode
		frac *big.Rat
	}
	shares := make([]share, len(children))
	sum := new(big.Int)
	for i, c := range children {
		scaled := new(big.Rat).Mul(c.exact, unit)
		floor := new(big.Int).Quo(scaled.Num(), scaled.Denom())
		c.rounded = floor
		sum.Add(sum, floor)
		shares[i] = share{c, new(big.Rat).Sub(scaled, new(big.Rat).SetInt(floor))}
	}
	remaining := new(big.Int).Sub(n.rounded, sum).Int64()
	sort.SliceStable(shares, func(i, j int) bool {
		if c := shares[i].frac.Cmp(shares[j].frac); c != 0 {
			return c > 0
		}
		return shares[i].c.key < shares[j].c.key
	})
	for i := 0; i < int(remaining) && i < len(shares); i++ {
		shares[i].c.rounded.Add(shares[i].c.rounded, big.NewInt(1))
	}
	for _, c := range children {
		c.distribute(unit)
	}
}

func formatMinor(v *big.Int, scale int) string {
	s := new(big.Int).Abs(v).String()
	if scale == 0 {
		return s
	}
	for len(s) <= scale {
		s = "0" + s
	}
	out := s[:len(s)-scale] + "." + s[len(s)-scale:]
	if v.Sign() < 0 {
		out = "-" + out
	}
	return out
}

func (n *allocNode) export(scale int, parent string, rows *[]AllocationRow) {
	path := n.level + "=" + n.key
	if parent != "" {
		path = parent + "/" + path
	}
	*rows = append(*rows, AllocationRow{Path: path, ParentPath: parent, Level: n.level, Key: n.key, ExactAmount: n.exact.RatString(), Amount: formatMinor(n.rounded, scale)})
	for _, c := range n.sortedChildren() {
		c.export(scale, path, rows)
	}
}

func (n *allocNode) assign(lines []CostLine, scale int) {
	if n.line >= 0 {
		lines[n.line].ModeledOpportunity = formatMinor(n.rounded, scale)
	}
	for _, c := range n.children {
		c.assign(lines, scale)
	}
}
