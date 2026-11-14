// Package advisor assembles the read-only advisor suggestion for one shortfall case (WIS-629).
// A suggestion is built when it is read, never stored: from the case's bound context windows,
// the approved process snapshot that context pins, and the latest stored investigation output
// (WIS-276). There is no new model call and no new model tool: the hypotheses are the
// investigation specialist's validated output, which used only the rule 08 tools. Each
// statement is labelled as an approved fact (a snapshot claim and its approved passages), an
// observation (a server measurement of a context window) or a hypothesis (model output). A
// suggestion approves, dispatches and changes nothing (rule 05).
//
// Build and Validate are pure: no clock, database or network. LoadTx reads their input.
package advisor

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
)

const (
	// SchemaVersion 2 lists each cited passage once per suggestion (SuggestionV1.Passages) and has
	// facts cite them by span ID; version 1 repeated every passage inside every fact citing it.
	SchemaVersion = 2
	// RuleVersion versions the selection and labelling rules below; it is in every suggestion.
	RuleVersion = "wis629-advisor-1"
	// MaxFacts bounds the approved facts one suggestion carries, in claim ID order.
	MaxFacts = 12
)

// Labels: every statement is exactly one of these.
const (
	LabelApprovedFact = "approved_fact"
	LabelObservation  = "observation"
	LabelHypothesis   = "hypothesis"
)

// Readiness of a suggestion (rule 06): NOT_READY carries no statements at all.
const (
	Ready    = "READY"
	Limited  = "LIMITED"
	NotReady = "NOT_READY"
)

// Reason codes. A suggestion's reasons are sorted and unique.
const (
	ReasonNoApprovedPlan            = recovery.ReasonNoApprovedPlan // the case's current window is not under a plan approved now
	ReasonNoProcessSnapshot         = "NO_PROCESS_SNAPSHOT"         // the context pins no approved process snapshot
	ReasonSnapshotStale             = "SNAPSHOT_STALE"              // a newer approved snapshot exists; the pinned one is still used
	ReasonSourceUnavailable         = "SOURCE_UNAVAILABLE"          // a fact was left out: a passage's source is revoked or missing
	ReasonFactsTruncated            = "FACTS_TRUNCATED"             // more than MaxFacts related claims
	ReasonCaseWindowUnknown         = "CASE_WINDOW_UNKNOWN"         // the bound windows do not parse: no claim can be placed in them
	ReasonInvestigationNotRun       = "INVESTIGATION_NOT_RUN"       // no investigation run for this case
	ReasonInvestigationRunning      = "INVESTIGATION_RUNNING"       // the latest run has not finished
	ReasonInvestigationNoOutput     = "INVESTIGATION_NO_OUTPUT"     // the run stored no validated output (its reasons follow)
	ReasonInvestigationEarlier      = "INVESTIGATION_EARLIER_REVISION"
	ReasonHypothesisCitationInvalid = "HYPOTHESIS_CITATION_INVALID" // model output cited something outside its run: all of it is left out
	ReasonSuggestionRejected        = "SUGGESTION_REJECTED"         // the assembled suggestion failed Validate
)

// PassageV1 is one approved source passage the suggestion's facts cite, as the snapshot pinned it.
type PassageV1 struct {
	SpanID   string `json:"span_id" contract:"uuid"`
	SourceID string `json:"source_id" contract:"uuid"`
	Revision int64  `json:"revision"`
	Text     string `json:"text" contract:"max=262144"`
}

// FactV1 is one approved claim of the pinned snapshot about the case's line, asset or order, or
// about something one approved relationship away from them, in effect during the case's bound
// windows. Names are source-derived text; the validity interval is the claim's, unchanged.
type FactV1 struct {
	ClaimID       string  `json:"claim_id" contract:"uuid"`
	SubjectID     string  `json:"subject_id" contract:"uuid"`
	SubjectName   string  `json:"subject_name" contract:"max=200"`
	Predicate     string  `json:"predicate" contract:"max=64"`
	ObjectID      *string `json:"object_entity_id" contract:"uuid"`
	ObjectName    *string `json:"object_name" contract:"max=200"`
	Value         *string `json:"value" contract:"max=4000"`
	Unit          *string `json:"unit" contract:"max=32"`
	EffectiveFrom string  `json:"effective_from" contract:"max=40"`
	EffectiveTo   *string `json:"effective_to" contract:"max=40"`
	// SpanIDs are the claim's passages, each in SuggestionV1.Passages.
	SpanIDs []string `json:"span_ids" contract:"uuid"`
}

// ObservationV1 is one server measurement of a bound context window, copied exactly.
type ObservationV1 struct {
	MeasurementID string `json:"measurement_id" contract:"uuid"`
	ContextID     string `json:"context_id" contract:"uuid"`
	MetricName    string `json:"metric_name" contract:"max=64"`
	Value         string `json:"value_decimal" contract:"max=64"`
	Unit          string `json:"unit" contract:"max=32"`
	WindowStart   string `json:"window_start" contract:"max=40"`
	WindowEnd     string `json:"window_end" contract:"max=40"`
}

// HypothesisV1 is the investigation specialist's labelled uncertainty, with the evidence it cites
// (measurements, machine-state records or plan sources captured by its run).
type HypothesisV1 struct {
	Text                     string   `json:"text" contract:"max=1000"`
	EvidenceIDs              []string `json:"evidence_ids" contract:"uuid"`
	ContradictingEvidenceIDs []string `json:"contradicting_evidence_ids" contract:"uuid"`
}

// UnknownV1 is information the investigation said it lacks.
type UnknownV1 struct {
	Field    string `json:"field" contract:"max=64"`
	Question string `json:"question" contract:"max=1000"`
	Required bool   `json:"required"`
}

// SnapshotRefV1 names the approved process snapshot the facts come from.
type SnapshotRefV1 struct {
	SnapshotID        string `json:"snapshot_id" contract:"uuid"`
	InventoryRevision int64  `json:"inventory_revision"`
	CreatedAt         string `json:"created_at" contract:"max=40"`
	Stale             bool   `json:"stale"`
}

// PlanRefV1 is the plan approved for the case's current window.
type PlanRefV1 struct {
	PlanRevisionID string `json:"plan_revision_id" contract:"uuid"`
	PlanRevision   int64  `json:"plan_revision"`
	ApprovalID     string `json:"approval_id" contract:"uuid"`
}

// InvestigationRefV1 is the investigation run the hypotheses come from.
type InvestigationRefV1 struct {
	RunID        string   `json:"run_id" contract:"uuid"`
	CaseRevision int64    `json:"case_revision"`
	State        string   `json:"state" contract:"max=32"`
	Status       string   `json:"status" contract:"max=32"`
	Reasons      []string `json:"reasons" contract:"max=64"`
	Model        string   `json:"model" contract:"max=200"`
	Actual       bool     `json:"provider_actual"`
}

// SuggestionV1 is the read-only advisor suggestion for one case.
type SuggestionV1 struct {
	SchemaVersion int                 `json:"schema_version"`
	RuleVersion   string              `json:"rule_version" contract:"max=64"`
	CaseID        string              `json:"case_id" contract:"uuid"`
	CaseRevision  int64               `json:"case_revision"`
	Readiness     string              `json:"readiness" contract:"max=16"`
	Reasons       []string            `json:"reasons" contract:"max=64"`
	Snapshot      *SnapshotRefV1      `json:"snapshot"`
	Plan          *PlanRefV1          `json:"plan"`
	Investigation *InvestigationRefV1 `json:"investigation"`
	Facts         []FactV1            `json:"facts"`
	// Passages holds every passage a fact cites, once, in span ID order.
	Passages     []PassageV1     `json:"passages"`
	Observations []ObservationV1 `json:"observations"`
	Hypotheses   []HypothesisV1  `json:"hypotheses"`
	Unknowns     []UnknownV1     `json:"unknowns"`
}

// RunRunning is the state of a run that has not finished (agent.RunRunning).
const RunRunning = "running"

// Run is the latest investigation run of the case, with what it captured.
type Run struct {
	ID           string
	CaseRevision int64
	State        string
	Status       string
	Reasons      []string
	Model        string
	Actual       bool
	// Reinvestigated reports that the case went back to investigating after this run's revision
	// (a clarification, a fresh window or a reopen), so newer information exists. The case moving
	// on to proposed, awaiting approval, executing or observing is this run's own consequence and
	// does not make it earlier (WIS-631 found the advisor calling every proposing run "earlier").
	Reinvestigated bool
	// Output is the stored validated investigation output; nil when the run stored none.
	Output *recovery.RecoveryProposalV1
	// Captured holds every ID the run captured (measurements, machine-state records, evidence).
	Captured map[string]bool
	// Revoked holds the captured evidence sources revoked since.
	Revoked map[string]bool
}

// Input is everything Build reads. LoadTx fills it inside one tenant transaction.
type Input struct {
	CaseID       string
	CaseRevision int64
	LineID       string
	OrderID      string
	AssetID      string
	// WindowStart and WindowEnd span the case's bound context windows, [first start, last end):
	// only claims in effect during it are facts. Zero times select no claim (fail closed).
	WindowStart, WindowEnd time.Time
	Plan                   *PlanRefV1 // nil: no plan approved now for the current window
	// ContextNotEligible: a bound window is not eligible now (recovery.Preconditions' rule).
	ContextNotEligible bool
	Measurements       []recovery.Measurement
	Snapshot           *knowledge.Snapshot // the pinned snapshot, or nil
	SnapshotStale      bool
	// SnapshotUnavailable: the context pins a snapshot that can no longer be read because one of
	// its sources was revoked (Snapshot is nil).
	SnapshotUnavailable bool
	// PassageText holds the text of every span of the snapshot whose source is still available
	// (not revoked, revision present). A span missing here is unavailable.
	PassageText map[string]string
	Run         *Run
}

// ErrRejected is returned by Validate when a statement cites anything outside its input.
var ErrRejected = errors.New("suggestion cites outside its scope")

// Build assembles the suggestion. It never fails: a suggestion that does not pass Validate is
// returned as NOT_READY with SUGGESTION_REJECTED and no statements.
func Build(in Input) SuggestionV1 {
	s := SuggestionV1{SchemaVersion: SchemaVersion, RuleVersion: RuleVersion, CaseID: in.CaseID, CaseRevision: in.CaseRevision,
		Plan: in.Plan, Facts: []FactV1{}, Passages: []PassageV1{}, Observations: []ObservationV1{}, Hypotheses: []HypothesisV1{}, Unknowns: []UnknownV1{}, Reasons: []string{}}
	reasons := map[string]bool{}
	if in.Snapshot != nil {
		s.Snapshot = &SnapshotRefV1{SnapshotID: in.Snapshot.SnapshotID, InventoryRevision: in.Snapshot.InventoryRevision, CreatedAt: in.Snapshot.CreatedAt.UTC().Format(time.RFC3339Nano), Stale: in.SnapshotStale}
	}
	if in.Run != nil {
		s.Investigation = &InvestigationRefV1{RunID: in.Run.ID, CaseRevision: in.Run.CaseRevision, State: in.Run.State, Status: in.Run.Status, Reasons: sortedCopy(in.Run.Reasons), Model: in.Run.Model, Actual: in.Run.Actual}
	}
	// No plan approved now, or a bound window no longer eligible: the shortfall cannot be stated,
	// so nothing is suggested (rule 06).
	if in.Plan == nil || in.ContextNotEligible {
		if in.Plan == nil {
			reasons[ReasonNoApprovedPlan] = true
		}
		if in.ContextNotEligible {
			reasons[recovery.ReasonContextNotEligible] = true
		}
		s.Readiness, s.Reasons = NotReady, keys(reasons)
		return s
	}
	for _, m := range in.Measurements {
		s.Observations = append(s.Observations, ObservationV1{MeasurementID: m.ID, ContextID: m.ContextID, MetricName: m.MetricName, Value: m.Value, Unit: m.Unit, WindowStart: m.WindowStart, WindowEnd: m.WindowEnd})
	}
	switch {
	case in.Snapshot == nil && in.SnapshotUnavailable:
		reasons[ReasonSourceUnavailable] = true
	case in.Snapshot == nil:
		reasons[ReasonNoProcessSnapshot] = true
	case !in.WindowEnd.After(in.WindowStart):
		// Unknown is not empty (rule 06): no fact is selected, and the suggestion says why.
		reasons[ReasonCaseWindowUnknown] = true
	default:
		if in.SnapshotStale {
			reasons[ReasonSnapshotStale] = true
		}
		facts, passages, unavailable, truncated := selectFacts(in)
		s.Facts, s.Passages = facts, passages
		if unavailable {
			reasons[ReasonSourceUnavailable] = true
		}
		if truncated {
			reasons[ReasonFactsTruncated] = true
		}
	}
	switch {
	case in.Run == nil:
		reasons[ReasonInvestigationNotRun] = true
	case in.Run.Output == nil && in.Run.State == RunRunning:
		reasons[ReasonInvestigationRunning] = true
	case in.Run.Output == nil:
		reasons[ReasonInvestigationNoOutput] = true
		for _, r := range in.Run.Reasons {
			reasons[r] = true
		}
	default:
		if in.Run.Reinvestigated {
			reasons[ReasonInvestigationEarlier] = true
		}
		// Model output is accepted whole or not at all (rule 08): one citation outside the run's
		// captured scope, or of a source revoked since, leaves out every hypothesis and unknown.
		if hypothesesInScope(in.Run.Output.Hypotheses, in.Run) {
			for _, h := range in.Run.Output.Hypotheses {
				s.Hypotheses = append(s.Hypotheses, HypothesisV1{Text: h.Text, EvidenceIDs: sortedCopy(h.EvidenceIDs), ContradictingEvidenceIDs: sortedCopy(h.ContradictingEvidenceIDs)})
			}
			for _, u := range in.Run.Output.Unknowns {
				s.Unknowns = append(s.Unknowns, UnknownV1{Field: u.Field, Question: u.Question, Required: u.Required})
			}
		} else {
			reasons[ReasonHypothesisCitationInvalid] = true
		}
	}
	s.Reasons = keys(reasons)
	s.Readiness = Ready
	if len(s.Reasons) > 0 {
		s.Readiness = Limited
	}
	if Validate(s, in) != nil {
		return SuggestionV1{SchemaVersion: SchemaVersion, RuleVersion: RuleVersion, CaseID: in.CaseID, CaseRevision: in.CaseRevision, Readiness: NotReady,
			Reasons: []string{ReasonSuggestionRejected}, Snapshot: s.Snapshot, Plan: s.Plan, Investigation: s.Investigation,
			Facts: []FactV1{}, Passages: []PassageV1{}, Observations: []ObservationV1{}, Hypotheses: []HypothesisV1{}, Unknowns: []UnknownV1{}}
	}
	return s
}

// Validate checks every citation of s against its input: each fact is a claim of the pinned
// snapshot citing only that claim's spans, with the passage text the source holds now; each
// observation is a server measurement, byte for byte; each hypothesis cites only what its run
// captured and nothing revoked since. A NOT_READY suggestion has no statements.
func Validate(s SuggestionV1, in Input) error {
	if s.Readiness == NotReady {
		if len(s.Facts)+len(s.Passages)+len(s.Observations)+len(s.Hypotheses)+len(s.Unknowns) != 0 {
			return ErrRejected
		}
		return nil
	}
	if in.ContextNotEligible || in.Plan == nil || s.Plan == nil || *s.Plan != *in.Plan {
		return ErrRejected
	}
	if len(s.Facts) > 0 && in.Snapshot == nil {
		return ErrRejected
	}
	// Only the claims selection may consider: related to the case and in effect during its windows.
	claims, spans := map[string]knowledge.SnapshotClaim{}, map[string]knowledge.SnapshotSpan{}
	if in.Snapshot != nil {
		for _, c := range candidates(in) {
			claims[c.ClaimID] = c
		}
		for _, sp := range in.Snapshot.Contents.Spans {
			spans[sp.SpanID] = sp
		}
	}
	// Each passage is a span of the snapshot with the text its source holds now, listed once.
	passages := map[string]bool{}
	for _, p := range s.Passages {
		sp, ok := spans[p.SpanID]
		text, available := in.PassageText[p.SpanID]
		if !ok || !available || passages[p.SpanID] || sp.SourceID != p.SourceID || sp.Revision != p.Revision || text != p.Text {
			return ErrRejected
		}
		passages[p.SpanID] = true
	}
	// Each fact cites only its claim's spans, each once and listed; every listed passage is cited.
	cited := map[string]bool{}
	for _, f := range s.Facts {
		c, ok := claims[f.ClaimID]
		if !ok || c.SubjectID != f.SubjectID || c.Predicate != f.Predicate || c.EffectiveFrom != f.EffectiveFrom || !equalPtr(c.EffectiveTo, f.EffectiveTo) || len(f.SpanIDs) == 0 {
			return ErrRejected
		}
		seen := map[string]bool{}
		for _, id := range f.SpanIDs {
			if !passages[id] || seen[id] || !slices.Contains(c.SourceSpanIDs, id) {
				return ErrRejected
			}
			seen[id], cited[id] = true, true
		}
	}
	if len(cited) != len(passages) {
		return ErrRejected
	}
	measured := map[string]recovery.Measurement{}
	for _, m := range in.Measurements {
		measured[m.ID] = m
	}
	for _, o := range s.Observations {
		m, ok := measured[o.MeasurementID]
		if !ok || m.Value != o.Value || m.Unit != o.Unit || m.MetricName != o.MetricName || m.ContextID != o.ContextID || m.WindowStart != o.WindowStart || m.WindowEnd != o.WindowEnd {
			return ErrRejected
		}
	}
	if len(s.Hypotheses)+len(s.Unknowns) > 0 && (in.Run == nil || in.Run.Output == nil) {
		return ErrRejected
	}
	for _, h := range s.Hypotheses {
		if !citesInScope(h.EvidenceIDs, in.Run) || !citesInScope(h.ContradictingEvidenceIDs, in.Run) {
			return ErrRejected
		}
	}
	return nil
}

func hypothesesInScope(hs []recovery.Hypothesis, run *Run) bool {
	for _, h := range hs {
		if !citesInScope(h.EvidenceIDs, run) || !citesInScope(h.ContradictingEvidenceIDs, run) {
			return false
		}
	}
	return true
}

func citesInScope(ids []string, run *Run) bool {
	for _, id := range ids {
		if run == nil || !run.Captured[id] || run.Revoked[id] {
			return false
		}
	}
	return true
}

// selectFacts picks the snapshot claims about the case's line, asset or order (subject or
// object), plus the property claims of anything one of those relationships links them to, in
// claim ID order, at most MaxFacts, with the passages they cite, each once in span ID order. A
// claim whose passages are not all available is left out.
func selectFacts(in Input) (facts []FactV1, passages []PassageV1, unavailable, truncated bool) {
	c := in.Snapshot.Contents
	names := map[string]string{}
	for _, e := range c.Entities {
		names[e.EntityID] = e.Name
	}
	spans := map[string]knowledge.SnapshotSpan{}
	for _, sp := range c.Spans {
		spans[sp.SpanID] = sp
	}
	picked := candidates(in)
	facts, cited := []FactV1{}, map[string]PassageV1{}
	for _, cl := range picked {
		f := FactV1{ClaimID: cl.ClaimID, SubjectID: cl.SubjectID, SubjectName: names[cl.SubjectID], Predicate: cl.Predicate, EffectiveFrom: cl.EffectiveFrom, EffectiveTo: cl.EffectiveTo, SpanIDs: []string{}}
		if cl.ObjectEntityID != "" {
			id, name := cl.ObjectEntityID, names[cl.ObjectEntityID]
			f.ObjectID, f.ObjectName = &id, &name
		}
		if cl.Literal != nil {
			v := literalText(cl.Literal.Value)
			f.Value = &v
			if cl.Literal.Unit != "" {
				u := cl.Literal.Unit
				f.Unit = &u
			}
		}
		ok := len(cl.SourceSpanIDs) > 0
		for _, id := range sortedCopy(cl.SourceSpanIDs) {
			if _, known := spans[id]; !known {
				ok = false
			}
			if _, available := in.PassageText[id]; !available {
				ok = false
			}
		}
		if !ok {
			unavailable = true
			continue
		}
		if len(facts) == MaxFacts {
			truncated = true
			break
		}
		for _, id := range sortedCopy(cl.SourceSpanIDs) {
			sp := spans[id]
			f.SpanIDs = append(f.SpanIDs, id)
			cited[id] = PassageV1{SpanID: id, SourceID: sp.SourceID, Revision: sp.Revision, Text: in.PassageText[id]}
		}
		facts = append(facts, f)
	}
	passages = []PassageV1{}
	for _, id := range slices.Sorted(maps.Keys(cited)) {
		passages = append(passages, cited[id])
	}
	return facts, passages, unavailable, truncated
}

// candidates are the claims selectFacts considers, in claim ID order: every claim whose subject or
// object is the case's line, asset or order, and every property claim of an entity one approved
// relationship away from them. A claim counts only while in effect during the case's windows, with
// the investigation's own overlap rule (agent.EffectiveDuring); a property reached through a
// relationship must be in effect during the part of the windows that relationship is.
func candidates(in Input) []knowledge.SnapshotClaim {
	c := in.Snapshot.Contents
	anchor := map[string]bool{}
	for _, id := range []string{in.LineID, in.AssetID, in.OrderID} {
		if id != "" {
			anchor[id] = true
		}
	}
	type span struct{ start, end time.Time }
	related := map[string][]span{}
	var picked []knowledge.SnapshotClaim
	for _, cl := range c.Claims {
		if !agent.EffectiveDuring(cl.EffectiveFrom, cl.EffectiveTo, in.WindowStart, in.WindowEnd) {
			continue
		}
		if anchor[cl.SubjectID] || anchor[cl.ObjectEntityID] {
			picked = append(picked, cl)
		}
		if cl.ObjectEntityID == "" || anchor[cl.SubjectID] == anchor[cl.ObjectEntityID] {
			continue
		}
		// The part of the windows this relationship is in effect (bounds parsed by EffectiveDuring).
		s := span{in.WindowStart, in.WindowEnd}
		if from, _ := time.Parse(time.RFC3339, cl.EffectiveFrom); from.After(s.start) {
			s.start = from
		}
		if cl.EffectiveTo != nil {
			if to, _ := time.Parse(time.RFC3339, *cl.EffectiveTo); to.Before(s.end) {
				s.end = to
			}
		}
		other := cl.ObjectEntityID
		if anchor[cl.ObjectEntityID] {
			other = cl.SubjectID
		}
		related[other] = append(related[other], s)
	}
	for _, cl := range c.Claims {
		if cl.Literal == nil || anchor[cl.SubjectID] {
			continue
		}
		for _, s := range related[cl.SubjectID] {
			if agent.EffectiveDuring(cl.EffectiveFrom, cl.EffectiveTo, s.start, s.end) {
				picked = append(picked, cl)
				break
			}
		}
	}
	sort.Slice(picked, func(i, j int) bool { return picked[i].ClaimID < picked[j].ClaimID })
	return picked
}

// literalText shows a JSON string literal unquoted and any other literal as its exact JSON text.
func literalText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func equalPtr(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedCopy(s []string) []string {
	out := append([]string{}, s...)
	sort.Strings(out)
	return out
}
