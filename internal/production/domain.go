// Package production contains pure, exact production-plan arithmetic.
package production

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
	"github.com/google/uuid"
)

const (
	maxIntervals = 1000
	maxDecimal   = 256
	maxJSON      = limits.RequestBodyBytes
)

var (
	ErrInvalidPlan            = errors.New("invalid production plan")
	ErrInvalidDelta           = errors.New("invalid production delta")
	ErrInvalidInterval        = errors.New("invalid production interval")
	ErrPlanDoesNotCoverWindow = errors.New("production plan does not cover context window")
	// decimalRE allows at most six fractional digits (contract: canonical decimal strings).
	decimalRE = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]{0,5}[1-9])?$`)
)

type Interval struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type RateSegment struct {
	Start                    string `json:"start"`
	End                      string `json:"end"`
	TargetGoodUnitsPerSecond string `json:"target_good_units_per_second"`
}

// Plan is the production_plan event payload: the reviewed business plan plus the
// approval_ref naming the human PlanApprovalV1 decision that approved it.
type Plan struct {
	OrderID       string        `json:"order_id"`
	AssetID       string        `json:"asset_id"`
	WindowStart   string        `json:"window_start"`
	WindowEnd     string        `json:"window_end"`
	PlanRevision  int64         `json:"plan_revision"`
	ApprovalRef   string        `json:"approval_ref"`
	Unit          string        `json:"unit"`
	RateSegments  []RateSegment `json:"rate_segments"`
	PlannedBreaks []Interval    `json:"planned_breaks"`
}

// PlanBody is the exact typed business payload a planner submits for review. It
// excludes approval_ref, so an approval can never be part of what it approves.
type PlanBody struct {
	OrderID       string        `json:"order_id"`
	AssetID       string        `json:"asset_id"`
	WindowStart   string        `json:"window_start"`
	WindowEnd     string        `json:"window_end"`
	PlanRevision  int64         `json:"plan_revision"`
	Unit          string        `json:"unit"`
	RateSegments  []RateSegment `json:"rate_segments"`
	PlannedBreaks []Interval    `json:"planned_breaks"`
}

// Body returns the business payload covered by the plan's approval digest.
func (p Plan) Body() PlanBody {
	return PlanBody{OrderID: p.OrderID, AssetID: p.AssetID, WindowStart: p.WindowStart, WindowEnd: p.WindowEnd, PlanRevision: p.PlanRevision, Unit: p.Unit, RateSegments: p.RateSegments, PlannedBreaks: p.PlannedBreaks}
}

// WithApproval returns the event payload that cites approvalRef for this body.
func (b PlanBody) WithApproval(approvalRef string) Plan {
	return Plan{OrderID: b.OrderID, AssetID: b.AssetID, WindowStart: b.WindowStart, WindowEnd: b.WindowEnd, PlanRevision: b.PlanRevision, ApprovalRef: approvalRef, Unit: b.Unit, RateSegments: b.RateSegments, PlannedBreaks: b.PlannedBreaks}
}

type Delta struct {
	OrderID       string `json:"order_id"`
	AssetID       string `json:"asset_id"`
	IntervalStart string `json:"interval_start"`
	IntervalEnd   string `json:"interval_end"`
	GoodUnits     string `json:"good_units"`
	Unit          string `json:"unit"`
}

func canonicalID(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u != uuid.Nil && u.String() == s
}

// canonicalTime parses RFC3339 with an explicit offset and returns the UTC instant.
// Callers store the normalized form (utcString), so every offset spelling of the same
// instant produces the same payload and digest.
func canonicalTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() < 1 || t.Year() > 9999 {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// utcString is the canonical wire form: UTC, RFC3339, with a Z suffix.
func utcString(s string) string {
	t, ok := canonicalTime(s)
	if !ok {
		return s
	}
	return t.Format(time.RFC3339Nano)
}

// Normalized rewrites every timestamp to canonical UTC. Validation and digests run on
// this form.
func (p PlanBody) Normalized() PlanBody {
	out := p
	out.WindowStart, out.WindowEnd = utcString(p.WindowStart), utcString(p.WindowEnd)
	out.RateSegments = make([]RateSegment, len(p.RateSegments))
	for i, r := range p.RateSegments {
		out.RateSegments[i] = RateSegment{Start: utcString(r.Start), End: utcString(r.End), TargetGoodUnitsPerSecond: r.TargetGoodUnitsPerSecond}
	}
	out.PlannedBreaks = make([]Interval, len(p.PlannedBreaks))
	for i, b := range p.PlannedBreaks {
		out.PlannedBreaks[i] = Interval{Start: utcString(b.Start), End: utcString(b.End)}
	}
	return out
}

func decimal(s string) (*big.Rat, bool) {
	if len(s) == 0 || len(s) > maxDecimal || !decimalRE.MatchString(s) {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(s)
	return r, ok && r.Sign() >= 0
}

func validInterval(start, end string) (time.Time, time.Time, bool) {
	s, ok := canonicalTime(start)
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	e, ok := canonicalTime(end)
	return s, e, ok && s.Before(e)
}

func validContextWindow(start, end string) (time.Time, time.Time, bool) {
	s, e, ok := validInterval(start, end)
	return s, e, ok && s.Nanosecond() == 0 && e.Nanosecond() == 0 && s.Unix()%900 == 0 && e.Unix()-s.Unix() == 900
}

func (p Plan) Validate() error {
	if !canonicalID(p.ApprovalRef) {
		return ErrInvalidPlan
	}
	return p.Body().Validate()
}

// Validate checks the business payload: canonical IDs, nonoverlapping ordered
// breaks and rate segments inside the plan window, and rates covering every
// planned productive instant. An absent rate is unknown, so gaps are invalid.
func (p PlanBody) Validate() error {
	if !canonicalID(p.OrderID) || !canonicalID(p.AssetID) || p.PlanRevision < 1 || p.Unit != "count" ||
		len(p.RateSegments) > maxIntervals || len(p.PlannedBreaks) > maxIntervals {
		return ErrInvalidPlan
	}
	start, end, ok := validInterval(p.WindowStart, p.WindowEnd)
	if !ok {
		return ErrInvalidPlan
	}
	breaks := make([]timeInterval, 0, len(p.PlannedBreaks))
	for _, b := range p.PlannedBreaks {
		bs, be, valid := validInterval(b.Start, b.End)
		if !valid || bs.Before(start) || be.After(end) || (len(breaks) > 0 && bs.Before(breaks[len(breaks)-1].end)) {
			return ErrInvalidPlan
		}
		breaks = append(breaks, timeInterval{bs, be})
	}
	rates := make([]timeInterval, 0, len(p.RateSegments))
	for _, segment := range p.RateSegments {
		rs, re, valid := validInterval(segment.Start, segment.End)
		if !valid || rs.Before(start) || re.After(end) || (len(rates) > 0 && rs.Before(rates[len(rates)-1].end)) {
			return ErrInvalidPlan
		}
		if _, valid = decimal(segment.TargetGoodUnitsPerSecond); !valid {
			return ErrInvalidPlan
		}
		rates = append(rates, timeInterval{rs, re})
	}
	for _, productive := range productiveIntervals(start, end, breaks) {
		cursor := productive.start
		for _, rate := range rates {
			if !rate.end.After(cursor) {
				continue
			}
			if rate.start.After(cursor) {
				break
			}
			if rate.end.After(cursor) {
				cursor = rate.end
			}
			if !cursor.Before(productive.end) {
				break
			}
		}
		if cursor.Before(productive.end) {
			return ErrInvalidPlan
		}
	}
	return nil
}

func (p Plan) MarshalJSON() ([]byte, error) {
	type plain Plan
	copy := plain(p)
	if copy.RateSegments == nil {
		copy.RateSegments = []RateSegment{}
	}
	if copy.PlannedBreaks == nil {
		copy.PlannedBreaks = []Interval{}
	}
	return json.Marshal(copy)
}

func (p *Plan) UnmarshalJSON(data []byte) error {
	type plain Plan
	var parsed plain
	if decodeStrict(data, &parsed) != nil {
		return ErrInvalidPlan
	}
	result := Plan(parsed)
	normalized := result.Body().Normalized()
	result.WindowStart, result.WindowEnd = normalized.WindowStart, normalized.WindowEnd
	result.RateSegments, result.PlannedBreaks = normalized.RateSegments, normalized.PlannedBreaks
	if result.Validate() != nil {
		return ErrInvalidPlan
	}
	*p = result
	return nil
}

func DecodePlan(data []byte) (Plan, error) {
	var p Plan
	if json.Unmarshal(data, &p) != nil {
		return Plan{}, ErrInvalidPlan
	}
	return p, nil
}

func (p PlanBody) MarshalJSON() ([]byte, error) {
	type plain PlanBody
	copy := plain(p)
	if copy.RateSegments == nil {
		copy.RateSegments = []RateSegment{}
	}
	if copy.PlannedBreaks == nil {
		copy.PlannedBreaks = []Interval{}
	}
	return json.Marshal(copy)
}

func (p *PlanBody) UnmarshalJSON(data []byte) error {
	type plain PlanBody
	var parsed plain
	if decodeStrict(data, &parsed) != nil {
		return ErrInvalidPlan
	}
	result := PlanBody(parsed).Normalized()
	if result.Validate() != nil {
		return ErrInvalidPlan
	}
	*p = result
	return nil
}

// DecodePlanBody strictly decodes and validates a business payload submitted for review.
func DecodePlanBody(data []byte) (PlanBody, error) {
	var p PlanBody
	if json.Unmarshal(data, &p) != nil {
		return PlanBody{}, ErrInvalidPlan
	}
	return p, nil
}

// PlanDigestSchemaVersion versions the canonical plan payload digest.
const PlanDigestSchemaVersion = 1

// CanonicalPlanPayload is the exact byte sequence a PlanApprovalV1 digest covers:
// schema version, tenant, site and the validated business payload, in fixed field
// order. Times are canonical UTC RFC3339 and decimals canonical strings (both
// enforced by Validate), so equal plans always produce equal bytes.
func CanonicalPlanPayload(tenantID, siteID string, p PlanBody) ([]byte, string, error) {
	p = p.Normalized()
	if !canonicalID(tenantID) || !canonicalID(siteID) || p.Validate() != nil {
		return nil, "", ErrInvalidPlan
	}
	body, err := json.Marshal(struct {
		SchemaVersion int      `json:"schema_version"`
		TenantID      string   `json:"tenant_id"`
		SiteID        string   `json:"site_id"`
		Plan          PlanBody `json:"plan"`
	}{PlanDigestSchemaVersion, tenantID, siteID, p})
	if err != nil {
		return nil, "", ErrInvalidPlan
	}
	sum := sha256.Sum256(body)
	return body, hex.EncodeToString(sum[:]), nil
}

func (d *Delta) UnmarshalJSON(data []byte) error {
	type plain Delta
	var parsed plain
	if decodeStrict(data, &parsed) != nil {
		return ErrInvalidDelta
	}
	result := Delta(parsed)
	result.IntervalStart, result.IntervalEnd = utcString(result.IntervalStart), utcString(result.IntervalEnd)
	if validateDelta(result) != nil {
		return ErrInvalidDelta
	}
	*d = result
	return nil
}

func DecodeDelta(data []byte) (Delta, error) {
	var d Delta
	if json.Unmarshal(data, &d) != nil {
		return Delta{}, ErrInvalidDelta
	}
	return d, nil
}

func validateDelta(d Delta) error {
	if !canonicalID(d.OrderID) || !canonicalID(d.AssetID) || d.Unit != "count" ||
		!validDecimal(d.GoodUnits) {
		return ErrInvalidDelta
	}
	if _, _, ok := validInterval(d.IntervalStart, d.IntervalEnd); !ok {
		return ErrInvalidDelta
	}
	return nil
}

func validDecimal(s string) bool { _, ok := decimal(s); return ok }

type timeInterval struct{ start, end time.Time }

func productiveIntervals(start, end time.Time, breaks []timeInterval) []timeInterval {
	out := make([]timeInterval, 0, len(breaks)+1)
	cursor := start
	for _, b := range breaks {
		if !b.end.After(cursor) || !b.start.Before(end) {
			continue
		}
		if b.start.After(cursor) {
			out = append(out, timeInterval{cursor, b.start})
		}
		if b.end.After(cursor) {
			cursor = b.end
		}
	}
	if cursor.Before(end) {
		out = append(out, timeInterval{cursor, end})
	}
	return out
}

func clipped(a, b, c, d time.Time) (time.Time, time.Time, bool) {
	start, end := a, b
	if c.After(start) {
		start = c
	}
	if d.Before(end) {
		end = d
	}
	return start, end, start.Before(end)
}

func secondsBetween(start, end time.Time) *big.Rat {
	sec := end.Unix() - start.Unix()
	ns := end.Nanosecond() - start.Nanosecond()
	numerator := new(big.Int).Mul(big.NewInt(sec), big.NewInt(1_000_000_000))
	numerator.Add(numerator, big.NewInt(int64(ns)))
	return new(big.Rat).SetFrac(numerator, big.NewInt(1_000_000_000))
}

// ExpectedGoodUnits returns the exact planned count over the requested window's
// intersection with the plan, excluding planned breaks.
func ExpectedGoodUnits(p Plan, start, end string) (*big.Rat, error) {
	if p.Validate() != nil {
		return nil, ErrInvalidPlan
	}
	rs, re, ok := validContextWindow(start, end)
	if !ok {
		return nil, ErrInvalidInterval
	}
	ps, _ := canonicalTime(p.WindowStart)
	pe, _ := canonicalTime(p.WindowEnd)
	if ps.After(rs) || pe.Before(re) {
		return nil, ErrPlanDoesNotCoverWindow
	}
	lo, hi, ok := clipped(rs, re, ps, pe)
	if !ok {
		return nil, ErrPlanDoesNotCoverWindow
	}
	breaks := make([]timeInterval, 0, len(p.PlannedBreaks))
	for _, b := range p.PlannedBreaks {
		bs, _ := canonicalTime(b.Start)
		be, _ := canonicalTime(b.End)
		breaks = append(breaks, timeInterval{bs, be})
	}
	result := new(big.Rat)
	for _, seg := range p.RateSegments {
		rs, _ := canonicalTime(seg.Start)
		re, _ := canonicalTime(seg.End)
		a, b, intersects := clipped(rs, re, lo, hi)
		if !intersects {
			continue
		}
		rate, _ := decimal(seg.TargetGoodUnitsPerSecond)
		for _, productive := range productiveIntervals(a, b, breaks) {
			result.Add(result, new(big.Rat).Mul(rate, secondsBetween(productive.start, productive.end)))
		}
	}
	return result, nil
}

// SumGoodUnitsDeltas sums exact observed counts. Deltas may span planned breaks,
// but must be disjoint, contained in the request, and cover every productive instant.
func SumGoodUnitsDeltas(p Plan, start, end string, deltas []Delta) (*big.Rat, error) {
	if p.Validate() != nil {
		return nil, ErrInvalidPlan
	}
	rs, re, ok := validContextWindow(start, end)
	if !ok {
		return nil, ErrInvalidInterval
	}
	if len(deltas) > maxIntervals {
		return nil, ErrInvalidDelta
	}
	ps, _ := canonicalTime(p.WindowStart)
	pe, _ := canonicalTime(p.WindowEnd)
	if ps.After(rs) || pe.Before(re) {
		return nil, ErrPlanDoesNotCoverWindow
	}
	lo, hi, intersects := clipped(rs, re, ps, pe)
	if !intersects {
		return nil, ErrInvalidInterval
	}
	if len(deltas) == 0 {
		return nil, ErrInvalidDelta
	}
	items := make([]timeInterval, 0, len(deltas))
	values := make([]*big.Rat, 0, len(deltas))
	for _, d := range deltas {
		if validateDelta(d) != nil || d.OrderID != p.OrderID || d.AssetID != p.AssetID || d.Unit != p.Unit {
			return nil, ErrInvalidDelta
		}
		ds, _ := canonicalTime(d.IntervalStart)
		de, _ := canonicalTime(d.IntervalEnd)
		if ds.Before(rs) || de.After(re) || ds.Before(lo) || de.After(hi) {
			return nil, ErrInvalidDelta
		}
		value, _ := decimal(d.GoodUnits)
		items = append(items, timeInterval{ds, de})
		values = append(values, value)
	}
	order := make([]int, len(items))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool { return items[order[i]].start.Before(items[order[j]].start) })
	for i := 1; i < len(order); i++ {
		if items[order[i]].start.Before(items[order[i-1]].end) {
			return nil, ErrInvalidDelta
		}
	}
	breaks := make([]timeInterval, 0, len(p.PlannedBreaks))
	for _, b := range p.PlannedBreaks {
		bs, _ := canonicalTime(b.Start)
		be, _ := canonicalTime(b.End)
		breaks = append(breaks, timeInterval{bs, be})
	}
	for _, productive := range productiveIntervals(lo, hi, breaks) {
		cursor := productive.start
		for _, index := range order {
			item := items[index]
			if !item.end.After(cursor) {
				continue
			}
			if item.start.After(cursor) {
				break
			}
			if item.end.After(cursor) {
				cursor = item.end
			}
			if !cursor.Before(productive.end) {
				break
			}
		}
		if cursor.Before(productive.end) {
			return nil, ErrInvalidDelta
		}
	}
	result := new(big.Rat)
	for _, v := range values {
		result.Add(result, v)
	}
	return result, nil
}

// Decimal6HalfEven renders an exact rational as six decimal places using ties-to-even.
func Decimal6HalfEven(value *big.Rat) (string, error) {
	if value == nil || value.Sign() < 0 {
		return "", fmt.Errorf("invalid nonnegative rational")
	}
	scaled := new(big.Int).Mul(value.Num(), big.NewInt(1_000_000))
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(scaled, value.Denom(), remainder)
	doubled := new(big.Int).Lsh(remainder, 1)
	comparison := doubled.Cmp(value.Denom())
	if comparison > 0 || (comparison == 0 && quotient.Bit(0) == 1) {
		quotient.Add(quotient, big.NewInt(1))
	}
	whole, fraction := new(big.Int), new(big.Int)
	whole.QuoRem(quotient, big.NewInt(1_000_000), fraction)
	return fmt.Sprintf("%s.%06d", whole.String(), fraction.Int64()), nil
}

func decodeStrict(data []byte, dst any) error {
	return strictjson.Decode(data, dst, maxJSON)
}

// ProductiveSeconds returns the exact planned productive duration of the window:
// the plan/window intersection minus planned breaks. Zero means no planned production.
func ProductiveSeconds(p PlanBody, start, end string) (*big.Rat, error) {
	if p.Validate() != nil {
		return nil, ErrInvalidPlan
	}
	rs, re, ok := validContextWindow(start, end)
	if !ok {
		return nil, ErrInvalidInterval
	}
	ps, _ := canonicalTime(p.WindowStart)
	pe, _ := canonicalTime(p.WindowEnd)
	if ps.After(rs) || pe.Before(re) {
		return nil, ErrPlanDoesNotCoverWindow
	}
	breaks := make([]timeInterval, 0, len(p.PlannedBreaks))
	for _, b := range p.PlannedBreaks {
		bs, _ := canonicalTime(b.Start)
		be, _ := canonicalTime(b.End)
		breaks = append(breaks, timeInterval{bs, be})
	}
	total := new(big.Rat)
	for _, productive := range productiveIntervals(rs, re, breaks) {
		total.Add(total, secondsBetween(productive.start, productive.end))
	}
	return total, nil
}

// ExactDecimal renders a nonnegative rational exactly as a canonical decimal string
// (no exponent, no trailing zeros). It fails for values with no finite expansion,
// so stored inputs are never silently rounded.
func ExactDecimal(value *big.Rat) (string, error) {
	if value == nil || value.Sign() < 0 {
		return "", fmt.Errorf("invalid nonnegative rational")
	}
	denominator := new(big.Int).Set(value.Denom())
	places := 0
	for _, factor := range []int64{2, 5} {
		f := big.NewInt(factor)
		for new(big.Int).Mod(denominator, f).Sign() == 0 {
			denominator.Quo(denominator, f)
		}
	}
	if denominator.Cmp(big.NewInt(1)) != 0 {
		return "", fmt.Errorf("rational has no finite decimal expansion")
	}
	scale := big.NewInt(1)
	for new(big.Int).Mod(new(big.Int).Mul(value.Num(), scale), value.Denom()).Sign() != 0 {
		scale.Mul(scale, big.NewInt(10))
		places++
	}
	scaled := new(big.Int).Quo(new(big.Int).Mul(value.Num(), scale), value.Denom()).String()
	if places == 0 {
		return scaled, nil
	}
	for len(scaled) <= places {
		scaled = "0" + scaled
	}
	return scaled[:len(scaled)-places] + "." + scaled[len(scaled)-places:], nil
}

// ShortfallRuleVersion versions the engineering default below. It must be accepted or
// revised by the customer's planner at commissioning.
const ShortfallRuleVersion = 1

// ShortfallMinimumUnits and ShortfallMinimumFraction define a qualifying window:
// deficit >= max(5, 0.10 * expected).
var (
	ShortfallMinimumUnits    = big.NewRat(5, 1)
	ShortfallMinimumFraction = big.NewRat(1, 10)
)

// WindowQualifies reports whether one complete window's exact deficit meets the
// shortfall threshold max(5, 0.10*expected). A case needs two consecutive such windows.
func WindowQualifies(expected, deficit *big.Rat) bool {
	if expected == nil || deficit == nil || expected.Sign() < 0 || deficit.Sign() < 0 {
		return false
	}
	threshold := new(big.Rat).Mul(expected, ShortfallMinimumFraction)
	if threshold.Cmp(ShortfallMinimumUnits) < 0 {
		threshold = ShortfallMinimumUnits
	}
	return deficit.Cmp(threshold) >= 0
}

// NormalizePayloadTimes rewrites the timestamps of a production_plan or production_delta
// payload to canonical UTC in place, so a stored payload has one spelling per instant. SQL
// window selection reads stored payload times with try_utc_timestamp, which accepts only the
// UTC form, and the content digest then treats offset spellings of one instant as the same
// record. Values that do not parse are left as they are for typed validation to reject.
func NormalizePayloadTimes(kind string, payload map[string]any) {
	rewrite := func(m map[string]any, keys ...string) {
		for _, k := range keys {
			if s, ok := m[k].(string); ok {
				m[k] = utcString(s)
			}
		}
	}
	each := func(key string, keys ...string) {
		items, _ := payload[key].([]any)
		for _, item := range items {
			if m, ok := item.(map[string]any); ok {
				rewrite(m, keys...)
			}
		}
	}
	switch kind {
	case "production_delta":
		rewrite(payload, "interval_start", "interval_end")
	case "production_plan":
		rewrite(payload, "window_start", "window_end")
		each("rate_segments", "start", "end")
		each("planned_breaks", "start", "end")
	}
}
