package knowledge

import (
	"encoding/json"
	"math/big"
	"sort"
	"strings"
	"time"
)

// Route readiness reason codes (WIS-642). A route that is not one unbroken chain of the
// process's operations is reported by shape, never ordered by guess or by name, and then
// carries no totals.
const (
	RouteEmpty         = "ROUTE_EMPTY"          // no approved operation is part_of the process
	RouteBranched      = "ROUTE_BRANCHED"       // an operation has two successors or two predecessors
	RouteCycle         = "ROUTE_CYCLE"          // the precedes facts loop
	RouteIncomplete    = "ROUTE_INCOMPLETE"     // a gap: more than one chain
	RouteLeavesProcess = "ROUTE_LEAVES_PROCESS" // a precedes fact links an operation of this process to one outside it
)

// Value statuses of a step's time: known (one approved value, possibly cited by several
// claims), missing (no approved value; never zero, rule 06) or conflict (approved claims
// disagree; none is picked).
const (
	RouteValueKnown    = "known"
	RouteValueMissing  = "missing"
	RouteValueConflict = "conflict"
)

type RouteQuery struct {
	ProcessID string `json:"process_id"`
	AsOf      string `json:"as_of"`
}

func (q *RouteQuery) Validate() error {
	if q == nil || !contractUUID(q.ProcessID) {
		return ErrInvalidContract
	}
	_, normalized, err := normalizedTime(q.AsOf)
	if err != nil {
		return err
	}
	q.AsOf = normalized
	return nil
}

type RouteValue struct {
	Status        string   `json:"status"`
	Value         *string  `json:"value"`
	Unit          string   `json:"unit"`
	ClaimIDs      []string `json:"claim_ids"`
	SourceSpanIDs []string `json:"source_span_ids"`
}

type RouteStep struct {
	EntityID         string     `json:"entity_id"`
	DisplayName      string     `json:"display_name"`
	Position         *int       `json:"position"`
	CycleTimeSeconds RouteValue `json:"cycle_time_seconds"`
	SetupTimeSeconds RouteValue `json:"setup_time_seconds"`
}

// RouteTotal is the exact sum of one time over the ordered route. Value is null unless the route
// is READY and every step's value is known: a partial sum is never shown as the total.
type RouteTotal struct {
	Value        *string  `json:"value"`
	Unit         string   `json:"unit"`
	StepsMissing int      `json:"steps_missing"`
	ClaimIDs     []string `json:"claim_ids"`
}

// RouteLeadTime is the longest approved lead_time_days among the materials the route's operations
// consume and those materials' suppliers. Materials with no approved lead time (on themselves or
// any supplier) are counted, so a longest value over some materials is never read as all of them.
type RouteLeadTime struct {
	Value                    *string  `json:"value"`
	Unit                     string   `json:"unit"`
	EntityID                 *string  `json:"entity_id"`
	EntityName               *string  `json:"entity_name"`
	Materials                int      `json:"materials"`
	MaterialsWithoutLeadTime int      `json:"materials_without_lead_time"`
	ClaimIDs                 []string `json:"claim_ids"`
	SourceSpanIDs            []string `json:"source_span_ids"`
}

// RouteSummary (KnowledgeRouteSummaryV1) is the route of one process in one approved snapshot.
type RouteSummary struct {
	SchemaVersion       int           `json:"schema_version"`
	SnapshotID          string        `json:"snapshot_id"`
	SHA256              string        `json:"sha256"`
	InventoryRevision   int64         `json:"inventory_revision"`
	ProcessID           string        `json:"process_id"`
	ProcessName         string        `json:"process_name"`
	AsOf                string        `json:"as_of"`
	Readiness           string        `json:"readiness"`
	ReasonCodes         []string      `json:"reason_codes"`
	StepCount           int           `json:"step_count"`
	Steps               []RouteStep   `json:"steps"`
	OrderClaimIDs       []string      `json:"order_claim_ids"`
	CycleTimeSeconds    RouteTotal    `json:"cycle_time_seconds"`
	SetupTimeSeconds    RouteTotal    `json:"setup_time_seconds"`
	LongestLeadTimeDays RouteLeadTime `json:"longest_lead_time_days"`
}

// Route computes the route summary of a process from an approved snapshot. It is pure: the
// service has already authorized the read and refused a stale snapshot. Operations are those
// with an effective part_of claim to the process (as WIS-615 scopes them); their order comes
// only from precedes claims between them. Arithmetic is exact (math/big, rule 04).
func Route(snapshot Snapshot, q RouteQuery) (RouteSummary, error) {
	if err := q.Validate(); err != nil {
		return RouteSummary{}, err
	}
	at, _, _ := normalizedTime(q.AsOf)
	entities := map[string]SnapshotEntity{}
	for _, e := range snapshot.Contents.Entities {
		entities[e.EntityID] = e
	}
	process, found := entities[q.ProcessID]
	if !found || process.Type != "process" {
		return RouteSummary{}, ErrNotFound
	}
	var claims []SnapshotClaim
	for _, c := range snapshot.Contents.Claims {
		in, err := effectiveAt(c, at)
		if err != nil {
			return RouteSummary{}, err
		}
		if in {
			claims = append(claims, c)
		}
	}
	sort.Slice(claims, func(i, j int) bool { return claims[i].ClaimID < claims[j].ClaimID })

	out := RouteSummary{
		SchemaVersion: 1, SnapshotID: snapshot.SnapshotID, SHA256: snapshot.SHA256, InventoryRevision: snapshot.InventoryRevision,
		ProcessID: process.EntityID, ProcessName: process.Name, AsOf: q.AsOf, ReasonCodes: []string{}, Steps: []RouteStep{}, OrderClaimIDs: []string{},
		CycleTimeSeconds:    RouteTotal{Unit: "s", ClaimIDs: []string{}},
		SetupTimeSeconds:    RouteTotal{Unit: "s", ClaimIDs: []string{}},
		LongestLeadTimeDays: RouteLeadTime{Unit: "day", ClaimIDs: []string{}, SourceSpanIDs: []string{}},
	}

	ops := map[string]bool{}
	var order []string
	for _, c := range claims {
		if c.Predicate == "part_of" && c.ObjectEntityID == process.EntityID && entities[c.SubjectID].Type == "operation" {
			ops[c.SubjectID] = true
			order = append(order, c.ClaimID)
		}
	}
	next, prev := map[string]map[string]bool{}, map[string]map[string]bool{}
	reasons := map[string]bool{}
	for _, c := range claims {
		if c.Predicate != "precedes" {
			continue
		}
		from, to := ops[c.SubjectID], ops[c.ObjectEntityID]
		switch {
		case from && to:
			if next[c.SubjectID] == nil {
				next[c.SubjectID] = map[string]bool{}
			}
			if prev[c.ObjectEntityID] == nil {
				prev[c.ObjectEntityID] = map[string]bool{}
			}
			next[c.SubjectID][c.ObjectEntityID] = true
			prev[c.ObjectEntityID][c.SubjectID] = true
			order = append(order, c.ClaimID)
		case from || to:
			reasons[RouteLeavesProcess] = true
			order = append(order, c.ClaimID)
		}
	}
	out.OrderClaimIDs = compactSorted(order)
	out.StepCount = len(ops)

	ids := make([]string, 0, len(ops))
	for id := range ops {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	ordered := routeOrder(ids, next, prev, reasons)

	steps := map[string]*RouteStep{}
	for _, id := range ids {
		steps[id] = &RouteStep{
			EntityID: id, DisplayName: entities[id].Name,
			CycleTimeSeconds: propertyValue(claims, id, "cycle_time_seconds", "s"),
			SetupTimeSeconds: propertyValue(claims, id, "setup_time_seconds", "s"),
		}
	}
	if len(reasons) == 0 {
		for i, id := range ordered {
			pos := i + 1
			steps[id].Position = &pos
			out.Steps = append(out.Steps, *steps[id])
		}
	} else {
		for _, id := range ids {
			out.Steps = append(out.Steps, *steps[id])
		}
	}
	for r := range reasons {
		out.ReasonCodes = append(out.ReasonCodes, r)
	}
	sort.Strings(out.ReasonCodes)
	out.Readiness = "READY"
	if len(out.ReasonCodes) > 0 {
		out.Readiness = "NOT_READY"
	}

	out.CycleTimeSeconds = routeTotal(out.Steps, func(s RouteStep) RouteValue { return s.CycleTimeSeconds }, "s", out.Readiness == "READY")
	out.SetupTimeSeconds = routeTotal(out.Steps, func(s RouteStep) RouteValue { return s.SetupTimeSeconds }, "s", out.Readiness == "READY")
	out.LongestLeadTimeDays = longestLeadTime(claims, ops, entities, out.Readiness == "READY")
	return out, nil
}

// routeOrder returns the single chain through ids, or adds the shape reasons that prevent one.
func routeOrder(ids []string, next, prev map[string]map[string]bool, reasons map[string]bool) []string {
	if len(ids) == 0 {
		reasons[RouteEmpty] = true
		return nil
	}
	for _, id := range ids {
		if len(next[id]) > 1 || len(prev[id]) > 1 {
			reasons[RouteBranched] = true
		}
	}
	if reasons[RouteBranched] {
		return nil
	}
	var starts []string
	for _, id := range ids {
		if len(prev[id]) == 0 {
			starts = append(starts, id)
		}
	}
	// With at most one successor and predecessor each, the operations split into simple paths and
	// simple loops; a loop has no start, so anything a walk from the starts misses is a cycle.
	seen := map[string]bool{}
	var chain []string
	for _, s := range starts {
		for cur := s; cur != "" && !seen[cur]; {
			seen[cur] = true
			chain = append(chain, cur)
			cur = only(next[cur])
		}
	}
	if len(seen) < len(ids) {
		reasons[RouteCycle] = true
	}
	if len(starts) > 1 {
		reasons[RouteIncomplete] = true
	}
	if len(reasons) > 0 {
		return nil
	}
	return chain
}

func only(set map[string]bool) string {
	for k := range set {
		return k
	}
	return ""
}

// propertyValue reads one single-valued decimal property of subject. Several approved claims with
// the same exact value are one known value citing all of them; differing values are a conflict,
// and no value is picked (as WIS-628's approved_conflict).
func propertyValue(claims []SnapshotClaim, subject, predicate, unit string) RouteValue {
	v := RouteValue{Status: RouteValueMissing, Unit: unit, ClaimIDs: []string{}, SourceSpanIDs: []string{}}
	var value *big.Rat
	var ids, spans []string
	for _, c := range claims {
		if c.SubjectID != subject || c.Predicate != predicate || c.Literal == nil || c.Literal.Type != "decimal" {
			continue
		}
		r, ok := literalDecimal(c.Literal.Value)
		if !ok {
			continue
		}
		ids = append(ids, c.ClaimID)
		spans = append(spans, c.SourceSpanIDs...)
		if value == nil {
			value = r
		} else if value.Cmp(r) != 0 {
			v.Status = RouteValueConflict
		}
	}
	v.ClaimIDs, v.SourceSpanIDs = compactSorted(ids), compactSorted(spans)
	if value != nil && v.Status != RouteValueConflict {
		v.Status = RouteValueKnown
		s := canonicalDecimal(value)
		v.Value = &s
	}
	return v
}

func routeTotal(steps []RouteStep, get func(RouteStep) RouteValue, unit string, ready bool) RouteTotal {
	t := RouteTotal{Unit: unit, ClaimIDs: []string{}}
	sum := new(big.Rat)
	var ids []string
	for _, s := range steps {
		v := get(s)
		if v.Status != RouteValueKnown {
			t.StepsMissing++
			continue
		}
		r, _ := new(big.Rat).SetString(*v.Value)
		sum.Add(sum, r)
		ids = append(ids, v.ClaimIDs...)
	}
	if ready && t.StepsMissing == 0 && len(steps) > 0 {
		s := canonicalDecimal(sum)
		t.Value = &s
		t.ClaimIDs = compactSorted(ids)
	}
	return t
}

func longestLeadTime(claims []SnapshotClaim, ops map[string]bool, entities map[string]SnapshotEntity, ready bool) RouteLeadTime {
	out := RouteLeadTime{Unit: "day", ClaimIDs: []string{}, SourceSpanIDs: []string{}}
	materials := map[string]bool{}
	suppliers := map[string][]string{}
	for _, c := range claims {
		if c.Predicate == "consumes" && ops[c.SubjectID] && entities[c.ObjectEntityID].Type == "material" {
			materials[c.ObjectEntityID] = true
		}
	}
	for _, c := range claims {
		if c.Predicate == "supplied_by" && materials[c.SubjectID] {
			suppliers[c.SubjectID] = append(suppliers[c.SubjectID], c.ObjectEntityID)
		}
	}
	out.Materials = len(materials)
	ids := make([]string, 0, len(materials))
	for m := range materials {
		ids = append(ids, m)
	}
	sort.Strings(ids)
	var best *big.Rat
	var bestValue RouteValue
	var bestEntity string
	for _, m := range ids {
		subjects := append([]string{m}, suppliers[m]...)
		sort.Strings(subjects[1:])
		known := false
		for _, s := range subjects {
			v := propertyValue(claims, s, "lead_time_days", "day")
			if v.Status != RouteValueKnown {
				continue
			}
			known = true
			r, _ := new(big.Rat).SetString(*v.Value)
			if best == nil || r.Cmp(best) > 0 {
				best, bestValue, bestEntity = r, v, s
			}
		}
		if !known {
			out.MaterialsWithoutLeadTime++
		}
	}
	if ready && best != nil {
		name := entities[bestEntity].Name
		out.Value, out.EntityID, out.EntityName = bestValue.Value, &bestEntity, &name
		out.ClaimIDs, out.SourceSpanIDs = bestValue.ClaimIDs, bestValue.SourceSpanIDs
	}
	return out
}

// effectiveAt reports whether claim c is in effect at the half-open instant at (rule 04).
func effectiveAt(c SnapshotClaim, at time.Time) (bool, error) {
	lo, _, err := normalizedTime(c.EffectiveFrom)
	if err != nil {
		return false, err
	}
	if at.Before(lo) {
		return false, nil
	}
	if c.EffectiveTo != nil {
		hi, _, err := normalizedTime(*c.EffectiveTo)
		if err != nil || !hi.After(lo) {
			return false, ErrInvalidContract
		}
		if !at.Before(hi) {
			return false, nil
		}
	}
	return true, nil
}

// literalDecimal reads a decimal literal, a JSON string such as "60.5", exactly.
func literalDecimal(raw json.RawMessage) (*big.Rat, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return nil, false
	}
	r, err := decimal(s)
	return r, err == nil
}

// canonicalDecimal writes an exact finite decimal with no trailing zeros ("270", "4.5"). The sum
// of decimal strings always has a power-of-ten denominator, so the expansion is exact.
func canonicalDecimal(r *big.Rat) string {
	// A denominator 2^a·5^b needs max(a, b) fractional digits; each division by gcd(d, 10)
	// removes one factor of 2 and/or 5. Anything else cannot come from decimal inputs.
	digits, one, ten := 0, big.NewInt(1), big.NewInt(10)
	for d := new(big.Int).Set(r.Denom()); d.Cmp(one) > 0; digits++ {
		g := new(big.Int).GCD(nil, nil, d, ten)
		if g.Cmp(one) == 0 {
			panic("knowledge: canonicalDecimal of a non-decimal rational")
		}
		d.Quo(d, g)
	}
	s := r.FloatString(digits)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

func compactSorted(values []string) []string {
	out := append([]string{}, values...)
	sort.Strings(out)
	k := 0
	for i, v := range out {
		if i == 0 || v != out[k-1] {
			out[k] = v
			k++
		}
	}
	return out[:k]
}
