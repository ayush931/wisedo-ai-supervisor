// Package recovery holds the deterministic core of WIS-276 grounded recovery proposals: the
// RecoveryProposalV1 contract, the server's measurement serialization of a case's contexts, the
// preconditions checked before any provider call, and the validation every specialist output
// must pass before it can become a proposal. The model never decides counts, readiness, source
// authority, assignees or the approval hash. The package is pure: callers pass every fact and
// the time; nothing here reads a clock, a database or the network.
package recovery

import (
	"errors"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/approvals"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plantcontext"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
)

// Outcome statuses. Only Proposed can lead to an action, and only through WIS-274 approval.
const (
	StatusProposed   = "proposed"
	StatusNeedsInput = "needs_input"
	StatusRefused    = "refused"
	StatusEscalated  = "escalated"
)

// Option kinds (WIS-276 output contract).
const (
	OptionDefer       = "defer"
	OptionClarify     = "clarify"
	OptionEscalate    = "escalate"
	OptionMaintenance = "maintenance_investigation"
)

// The only v1 action and template (rule 05).
const (
	TaskKind     = "maintenance_investigation.create"
	TaskTemplate = "inspect_report_only"
)

// Output caps and text bounds from the WIS-276 contract.
const (
	MaxObservations   = 10
	MaxHypotheses     = 5
	MaxUnknowns       = 10
	MaxOptions        = 3
	MaxHypothesisText = 1000
	MaxRationale      = 2000
	MaxDescription    = 4000
	MaxOptionText     = 1000
	MaxQuestion       = 500
	MaxFieldName      = 64
	MaxCitations      = 100
	MaxContractBytes  = 1 << 20
	// MaxTaskDue is how far after server time a proposed investigation may be due.
	MaxTaskDue = 30 * 24 * time.Hour
)

// Metric names the server serializes for every context of a case.
const (
	MetricExpected = "expected_good_units"
	MetricActual   = "actual_good_units"
	MetricDeficit  = "deficit_good_units"
)

// Evidence kinds the server classifies retrievable evidence into. Only MachineFault evidence can
// support a maintenance investigation; materials, staffing and quality restrictions cannot, and
// neither can a MachineStoppage: a stop whose source stated a reason the Supervisor has no reviewed
// mapping for (rule 06: a stop is not a cause).
const (
	EvidenceProduction      = "production"
	EvidenceMachineFault    = "machine_fault"
	EvidenceMachineStoppage = "machine_stoppage"
	EvidenceMaterials       = "materials"
	EvidenceStaffing        = "staffing"
	EvidenceQuality         = "quality"
	EvidenceDocument        = "document"
	EvidenceClarifying      = "clarification"
)

// Observation is one measured fact, copied exactly from the server's serialization.
type Observation struct {
	MeasurementID string   `json:"measurement_id"`
	EvidenceIDs   []string `json:"evidence_ids"`
	MetricName    string   `json:"metric_name"`
	ValueDecimal  string   `json:"value_decimal"`
	Unit          string   `json:"unit"`
	WindowStart   string   `json:"window_start"`
	WindowEnd     string   `json:"window_end"`
}

// Hypothesis is labelled uncertainty, never a finding.
type Hypothesis struct {
	Text                     string   `json:"text"`
	EvidenceIDs              []string `json:"evidence_ids"`
	ContradictingEvidenceIDs []string `json:"contradicting_evidence_ids"`
}

// Unknown is information the proposal lacks.
type Unknown struct {
	Field    string `json:"field"`
	Question string `json:"question"`
	Required bool   `json:"required"`
}

// Option is one bounded course of action.
type Option struct {
	Kind        string   `json:"kind"`
	Text        string   `json:"text"`
	EvidenceIDs []string `json:"evidence_ids"`
}

// TaskPayload is the investigation request a human may approve.
type TaskPayload struct {
	Kind        string    `json:"kind"`
	AssetID     string    `json:"asset_id"`
	LineID      string    `json:"line_id"`
	OrderID     string    `json:"order_id"`
	AssigneeID  string    `json:"assignee_id"`
	Template    string    `json:"template"`
	Description string    `json:"description"`
	DueAt       time.Time `json:"due_at"`
}

// Versions pins everything the proposal was produced under.
type Versions struct {
	Policy int64  `json:"policy_revision"`
	Plan   int64  `json:"plan_revision"`
	Graph  int64  `json:"graph_revision"`
	Model  string `json:"model_version"`
	Prompt string `json:"prompt_version"`
	Tool   string `json:"tool_version"`
}

// RecoveryProposalV1 is the WIS-276 output contract. It is untrusted until Validate passes.
type RecoveryProposalV1 struct {
	ProposalID      string        `json:"proposal_id"`
	CaseID          string        `json:"case_id"`
	CaseRevision    int64         `json:"case_revision"`
	ContextID       string        `json:"context_id"`
	ContextRevision int64         `json:"context_revision"`
	Observations    []Observation `json:"observations"`
	Hypotheses      []Hypothesis  `json:"hypotheses"`
	Unknowns        []Unknown     `json:"unknowns"`
	Options         []Option      `json:"options"`
	SelectedOption  string        `json:"selected_option"`
	TaskPayload     *TaskPayload  `json:"task_payload"`
	Versions        Versions      `json:"versions"`
	GeneratedAt     time.Time     `json:"generated_at"`
	ExpiresAt       time.Time     `json:"expires_at"`
	Rationale       string        `json:"rationale"`
}

// DecodeProposal strictly decodes untrusted specialist output: unknown fields (for example a
// smuggled "approved" or "tenant_id"), duplicate keys, trailing data and non-JSON numbers are
// rejected before any validation.
func DecodeProposal(data []byte) (RecoveryProposalV1, error) {
	var p RecoveryProposalV1
	if err := strictjson.Decode(data, &p, MaxContractBytes); err != nil {
		return RecoveryProposalV1{}, err
	}
	return p, nil
}

// Measurement is the server's serialization of one metric of one context window.
type Measurement struct {
	ID          string `json:"measurement_id"`
	ContextID   string `json:"context_id"`
	MetricName  string `json:"metric_name"`
	Value       string `json:"value_decimal"`
	Unit        string `json:"unit"`
	WindowStart string `json:"window_start"`
	WindowEnd   string `json:"window_end"`
}

var measurementNamespace = uuid.MustParse("1d5f0c3a-276e-5b7a-9f3e-276000000001")

// MeasurementID is the stable identifier of one metric of one context.
func MeasurementID(contextID, metric string) string {
	return uuid.NewSHA1(measurementNamespace, []byte(contextID+"|"+metric)).String()
}

// Measurements serializes expected, actual and deficit for each READY context, using the
// context's published decimal strings verbatim. A context without a value contributes nothing:
// a missing measurement is never zero.
func Measurements(contexts []plantcontext.Snapshot) []Measurement {
	out := make([]Measurement, 0, 3*len(contexts))
	for _, c := range contexts {
		for _, m := range []struct {
			name  string
			value *string
		}{{MetricExpected, c.ExpectedGoodUnits}, {MetricActual, c.ActualGoodUnits}, {MetricDeficit, c.DeficitGoodUnits}} {
			if m.value == nil {
				continue
			}
			out = append(out, Measurement{ID: MeasurementID(c.ContextID, m.name), ContextID: c.ContextID, MetricName: m.name, Value: *m.value, Unit: c.Unit, WindowStart: c.Window.Start, WindowEnd: c.Window.End})
		}
	}
	return out
}

// Evidence is one retrievable item the server captured in scope, with its classified kind.
type Evidence struct {
	ID   string
	Kind string
}

// Assignee is one entry of the scoped authorized roster: a person a supervisor confirmed as a
// responsible maintenance assignee for the line. Qualification text is never consulted.
type Assignee struct {
	ID          string
	LineID      string
	ConfirmedBy string
}

// CaseRef is the active case the proposal is for.
type CaseRef struct {
	ID       string
	Revision int64
	State    string
	LineID   string
	OrderID  string
}

// ContextView is one context window with its current eligibility, judged by the server now.
type ContextView struct {
	Snapshot plantcontext.Snapshot
	Eligible bool
}

// Input is every server-side fact a proposal is judged against.
type Input struct {
	Case          CaseRef
	Contexts      []ContextView // the case's consecutive windows, oldest first; the last is current
	CurrentPlan   *plantcontext.PlanRef
	Evidence      []Evidence
	Roster        []Assignee
	ModeEnabled   bool // current permission and mode allow investigation
	PolicyVersion int64
	GraphVersion  int64
	Now           time.Time
	// EvidenceTooLarge: the machine evidence for the windows exceeded the read bound and was not
	// read at all, never partly (WIS-276 review M3).
	EvidenceTooLarge bool
}

// Precondition reason codes: the case needs input, and no provider is called.
const (
	ReasonCaseNotInvestigating = "CASE_NOT_INVESTIGATING"
	ReasonModeDisabled         = "MODE_DISABLED"
	ReasonTwoWindowsRequired   = "TWO_WINDOWS_REQUIRED"
	ReasonContextNotReady      = "CONTEXT_NOT_READY"
	ReasonContextNotEligible   = "CONTEXT_NOT_ELIGIBLE"
	ReasonIdentityMismatch     = "IDENTITY_MISMATCH"
	ReasonNoApprovedPlan       = "NO_APPROVED_PLAN"
	ReasonPlanChanged          = "PLAN_CHANGED"
	ReasonUnitMismatch         = "UNIT_MISMATCH"
	ReasonNotConsecutive       = "NOT_CONSECUTIVE"
	ReasonNotQualified         = "SHORTFALL_NOT_QUALIFIED"
	// ReasonEvidenceTooLarge: the windows hold more machine stoppages than the read bound, so the
	// evidence cannot be read completely; the case waits for a person instead of an infinite retry.
	ReasonEvidenceTooLarge = "MACHINE_EVIDENCE_TOO_LARGE"
)

// Preconditions checks, before any provider call, that the case is investigating under an
// enabled mode and that its windows are consecutive, READY, eligible now, of the same order and
// line, in one countable unit, and under the plan that is approved now and still qualify. It
// returns the reason codes, sorted; none means the investigation may run. Context reason codes
// (for example CORRECTION_GAP for a counter reset, NO_HEARTBEAT for a missing source) are
// carried through so the clarification names the actual gap.
func Preconditions(in Input) []string {
	why := map[string]bool{}
	if in.Case.State != "investigating" {
		why[ReasonCaseNotInvestigating] = true
	}
	if !in.ModeEnabled {
		why[ReasonModeDisabled] = true
	}
	if in.EvidenceTooLarge {
		why[ReasonEvidenceTooLarge] = true
	}
	if len(in.Contexts) < 2 {
		why[ReasonTwoWindowsRequired] = true
	}
	var unit string
	for i, v := range in.Contexts {
		c := v.Snapshot
		if c.Readiness != plantcontext.ReadyState {
			why[ReasonContextNotReady] = true
			for _, r := range c.ReasonCodes {
				why[r] = true
			}
		}
		if !v.Eligible {
			why[ReasonContextNotEligible] = true
		}
		if c.OrderID != in.Case.OrderID || c.AssetID != in.Case.LineID {
			why[ReasonIdentityMismatch] = true
		}
		switch {
		case c.ApprovedPlanRevision == nil || in.CurrentPlan == nil:
			why[ReasonNoApprovedPlan] = true
		case *c.ApprovedPlanRevision != *in.CurrentPlan:
			why[ReasonPlanChanged] = true
		}
		if i == 0 {
			unit = c.Unit
		}
		if c.Unit == "" || c.Unit != unit || c.Unit != "count" {
			why[ReasonUnitMismatch] = true
		}
		if i > 0 && in.Contexts[i-1].Snapshot.Window.End != c.Window.Start {
			why[ReasonNotConsecutive] = true
		}
	}
	if len(why) == 0 {
		last := len(in.Contexts) - 1
		if !plantcontext.EvaluateShortfall(in.Contexts[last-1].Snapshot, in.Contexts[last].Snapshot).Qualifies {
			why[ReasonNotQualified] = true
		}
	}
	out := make([]string, 0, len(why))
	for r := range why {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// Refusal reason codes: the specialist output is rejected whole; nothing is proposed.
const (
	RefuseScope              = "PROPOSAL_SCOPE_MISMATCH"
	RefuseBounds             = "PROPOSAL_OUT_OF_BOUNDS"
	RefuseText               = "INVALID_TEXT"
	RefuseUnknownMeasurement = "UNKNOWN_MEASUREMENT"
	RefuseNumericMismatch    = "NUMERIC_MISMATCH"
	RefuseUnsupportedNumber  = "UNSUPPORTED_NUMERIC_STATEMENT"
	RefuseForeignCitation    = "FOREIGN_CITATION"
	RefuseUncited            = "UNCITED_CLAIM"
	RefuseOptionKind         = "UNSUPPORTED_OPTION"
	RefuseSelection          = "SELECTION_NOT_OFFERED"
	RefuseAction             = "UNSUPPORTED_ACTION"
	RefuseTaskScope          = "TASK_SCOPE_MISMATCH"
	RefuseAssignee           = "ASSIGNEE_NOT_AUTHORIZED"
	RefuseNeedsClarify       = "NO_ASSIGNEE_REQUIRES_CLARIFY"
	RefuseTaskMismatch       = "TASK_PAYLOAD_MISMATCH"
	RefuseNotMaintenance     = "MAINTENANCE_WITHOUT_MACHINE_EVIDENCE"
	RefuseTimes              = "INVALID_TIMES"
	RefuseVersions           = "VERSION_MISMATCH"
)

// ErrRefused wraps every validation refusal; Reasons lists the codes.
var ErrRefused = errors.New("specialist output refused")

// Refusal is a validation failure with its sorted reason codes.
type Refusal struct{ Reasons []string }

func (r *Refusal) Error() string { return ErrRefused.Error() + ": " + strings.Join(r.Reasons, ",") }
func (r *Refusal) Unwrap() error { return ErrRefused }

var (
	// statedNumber finds every number in free text, with any sign, digit grouping or script,
	// and whether a percentage follows it.
	statedNumber = regexp.MustCompile(`(?i)([-+\p{Pd}\x{2212}]?\p{Nd}+(?:[.,]\p{Nd}+)*)\s*(%|(?:percent(?:age)?|pct|per\s*cent)\b)?`)
	fieldName    = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)
)

// Validate applies every deterministic WIS-276 check to untrusted specialist output. Any
// failure refuses the whole output: invented or cross-tenant citations, numbers that do not
// exactly match the server's measurements, an action outside v1, a task for anyone but a
// supervisor-confirmed assignee, or a maintenance task not supported by machine-fault evidence.
func Validate(p RecoveryProposalV1, in Input) error {
	if len(in.Contexts) == 0 {
		return &Refusal{Reasons: []string{RefuseScope}}
	}
	why := map[string]bool{}
	current := in.Contexts[len(in.Contexts)-1].Snapshot
	if !validUUID(p.ProposalID) || p.CaseID != in.Case.ID || p.CaseRevision != in.Case.Revision || p.ContextID != current.ContextID || p.ContextRevision != current.Revision {
		why[RefuseScope] = true
	}
	if len(p.Observations) > MaxObservations || len(p.Hypotheses) > MaxHypotheses || len(p.Unknowns) > MaxUnknowns ||
		len(p.Options) == 0 || len(p.Options) > MaxOptions {
		why[RefuseBounds] = true
	}
	if in.CurrentPlan == nil || p.Versions.Plan != in.CurrentPlan.PlanRevision || p.Versions.Policy != in.PolicyVersion || p.Versions.Graph != in.GraphVersion ||
		!validText(p.Versions.Model, 128) || !validText(p.Versions.Prompt, 128) || !validText(p.Versions.Tool, 128) {
		why[RefuseVersions] = true
	}
	// Expiry is server policy (rule 05): still open now, and no later than the approval TTL
	// from generation, whatever the model claims.
	if p.GeneratedAt.IsZero() || p.GeneratedAt.After(in.Now) || !p.ExpiresAt.After(in.Now) || p.ExpiresAt.After(p.GeneratedAt.Add(approvals.ApprovalTTL)) {
		why[RefuseTimes] = true
	}
	if !validText(p.Rationale, MaxRationale) {
		why[RefuseText] = true
	}

	evidence := map[string]string{}
	for _, e := range in.Evidence {
		evidence[e.ID] = e.Kind
	}
	cites := func(ids []string, required bool) {
		if len(ids) > MaxCitations || (required && len(ids) == 0) {
			if len(ids) == 0 {
				why[RefuseUncited] = true
			} else {
				why[RefuseBounds] = true
			}
		}
		for _, id := range ids {
			if _, ok := evidence[id]; !ok {
				why[RefuseForeignCitation] = true
			}
		}
	}

	measured := map[string]Measurement{}
	var contexts []plantcontext.Snapshot
	for _, v := range in.Contexts {
		contexts = append(contexts, v.Snapshot)
	}
	values := []*big.Rat{}
	for _, m := range Measurements(contexts) {
		measured[m.ID] = m
		if r, ok := new(big.Rat).SetString(m.Value); ok {
			values = append(values, r)
		}
	}
	for _, o := range p.Observations {
		m, ok := measured[o.MeasurementID]
		switch {
		case !ok:
			why[RefuseUnknownMeasurement] = true
		case o.MetricName != m.MetricName || o.ValueDecimal != m.Value || o.Unit != m.Unit || o.WindowStart != m.WindowStart || o.WindowEnd != m.WindowEnd:
			why[RefuseNumericMismatch] = true
		}
		cites(o.EvidenceIDs, true)
	}
	for _, h := range p.Hypotheses {
		if !validText(h.Text, MaxHypothesisText) {
			why[RefuseText] = true
		}
		cites(h.EvidenceIDs, true)
		cites(h.ContradictingEvidenceIDs, false)
	}
	for _, u := range p.Unknowns {
		if !fieldName.MatchString(u.Field) || !validText(u.Question, MaxQuestion) {
			why[RefuseText] = true
		}
	}
	kinds := map[string]Option{}
	for _, o := range p.Options {
		switch o.Kind {
		case OptionDefer, OptionClarify, OptionEscalate, OptionMaintenance:
		default:
			why[RefuseOptionKind] = true
		}
		if _, dup := kinds[o.Kind]; dup {
			why[RefuseBounds] = true
		}
		kinds[o.Kind] = o
		if !validText(o.Text, MaxOptionText) {
			why[RefuseText] = true
		}
		cites(o.EvidenceIDs, o.Kind == OptionMaintenance)
	}
	selected, offered := kinds[p.SelectedOption]
	if !offered {
		why[RefuseSelection] = true
	}

	// Every number in model-visible free text must be exactly one of the server's measurements,
	// written as a plain unsigned ASCII decimal and not as a percentage. Anything else (grouped
	// digits, signs, other scripts, numbers the server never measured) fails closed.
	texts := []string{p.Rationale}
	for _, u := range p.Unknowns {
		texts = append(texts, u.Question)
	}
	for _, h := range p.Hypotheses {
		texts = append(texts, h.Text)
	}
	for _, o := range p.Options {
		texts = append(texts, o.Text)
	}
	if p.TaskPayload != nil {
		texts = append(texts, p.TaskPayload.Description)
	}
	for _, t := range texts {
		for _, m := range statedNumber.FindAllStringSubmatch(t, -1) {
			stated, ok := new(big.Rat).SetString(m[1])
			if !ok || strings.Contains(m[1], ",") || !unicode.IsDigit(rune(m[1][0])) || m[2] != "" || !containsRat(values, stated) {
				why[RefuseUnsupportedNumber] = true
			}
		}
	}

	// A maintenance investigation needs an authorized assignee, the exact v1 task, the case's
	// own scope and machine-fault evidence; with no confirmed assignee only clarify is allowed.
	roster := map[string]bool{}
	for _, a := range in.Roster {
		if a.LineID == in.Case.LineID && validUUID(a.ID) && a.ConfirmedBy != "" {
			roster[a.ID] = true
		}
	}
	switch {
	case p.SelectedOption == OptionMaintenance:
		if len(roster) == 0 {
			why[RefuseNeedsClarify] = true
		}
		if p.TaskPayload == nil {
			why[RefuseTaskMismatch] = true
			break
		}
		t := p.TaskPayload
		if t.Kind != TaskKind || t.Template != TaskTemplate {
			why[RefuseAction] = true
		}
		if t.LineID != in.Case.LineID || t.OrderID != in.Case.OrderID || t.AssetID != current.AssetID {
			why[RefuseTaskScope] = true
		}
		if len(roster) > 0 && !roster[t.AssigneeID] {
			why[RefuseAssignee] = true
		}
		if !validText(t.Description, MaxDescription) || workstation.Executable(t.Description) {
			why[RefuseText] = true
		}
		// The due time must also pass approvals.Canonical, which runs later at submit (WIS-721):
		// microsecond precision, and after the submit-time issued_at. A due time at least one approval
		// TTL away is after any issued_at the proposal can still be approved under. Before, a model's
		// nanosecond or near-term due_at passed here, every submit was refused, and the case stayed
		// in investigating with no reason.
		if t.DueAt.Nanosecond()%int(time.Microsecond) != 0 || t.DueAt.Before(in.Now.Add(approvals.ApprovalTTL)) ||
			!t.DueAt.After(p.GeneratedAt) || t.DueAt.After(in.Now.Add(MaxTaskDue)) {
			why[RefuseTimes] = true
		}
		machine := false
		if offered {
			for _, id := range selected.EvidenceIDs {
				machine = machine || evidence[id] == EvidenceMachineFault
			}
		}
		if !machine {
			why[RefuseNotMaintenance] = true
		}
	case p.TaskPayload != nil:
		// Only a maintenance investigation carries a task; anything else must not.
		why[RefuseTaskMismatch] = true
	}
	if len(why) == 0 {
		return nil
	}
	reasons := make([]string, 0, len(why))
	for r := range why {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	return &Refusal{Reasons: reasons}
}

func containsRat(values []*big.Rat, r *big.Rat) bool {
	for _, v := range values {
		if v.Cmp(r) == 0 {
			return true
		}
	}
	return false
}

func validUUID(v string) bool {
	id, err := uuid.Parse(v)
	return err == nil && id != uuid.Nil && id.String() == v
}

// validText requires valid UTF-8, 1..max runes, some non-space content and no control
// characters other than tab and newline.
func validText(s string, max int) bool {
	n := utf8.RuneCountInString(s)
	if !utf8.ValidString(s) || n < 1 || n > max || strings.TrimSpace(s) == "" {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return false
		}
	}
	return true
}

// Outcome reason codes outside validation.
const (
	ReasonProviderFailed = "PROVIDER_FAILED"
	ReasonSchemaInvalid  = "SCHEMA_INVALID"
	// ReasonRequiredUnknown: the specialist selected a maintenance investigation while listing
	// information it marks as required, so the case needs that input first.
	ReasonRequiredUnknown = "REQUIRED_INPUT_MISSING"
)

// Outcome is the deterministic result of one investigation cycle. Only StatusProposed carries a
// proposal with a task, and even then it only becomes an action after WIS-274 human approval.
type Outcome struct {
	Status   string              `json:"status"`
	Reasons  []string            `json:"reasons"`
	Proposal *RecoveryProposalV1 `json:"proposal"`
}

// Action reports whether the outcome carries a task for approval. Every other outcome has zero
// action.
func (o Outcome) Action() bool {
	return o.Status == StatusProposed && o.Proposal != nil && o.Proposal.TaskPayload != nil
}

// Precheck returns the needs_input outcome when the preconditions fail, so no provider is
// called; ok is true when the investigation may run.
func Precheck(in Input) (Outcome, bool) {
	if reasons := Preconditions(in); len(reasons) > 0 {
		return Outcome{Status: StatusNeedsInput, Reasons: reasons}, false
	}
	return Outcome{}, true
}

// Judge turns one specialist result into an outcome. A provider failure escalates to a human
// (after WIS-275's single narrowed retry, which the runtime owns); output that fails strict
// decoding or validation is refused whole; a valid proposal follows its selected option:
// a maintenance investigation is proposed for human approval, clarify and defer wait for input
// with the specialist's questions, and escalate escalates. Nothing here performs an action.
//
// The preconditions are re-evaluated first against the Input passed here, which the caller
// reads after the provider call: a case, mode or context that changed meanwhile stops the
// cycle as needs_input. The model's proposal_id is never carried: approvals.Submit assigns the
// ID of a new proposal and treats a non-empty one as an edit of an existing proposal.
func Judge(in Input, raw []byte, providerErr error) Outcome {
	if out, ok := Precheck(in); !ok {
		return out
	}
	if providerErr != nil {
		return Outcome{Status: StatusEscalated, Reasons: []string{ReasonProviderFailed}}
	}
	p, err := DecodeProposal(raw)
	if err != nil {
		return Outcome{Status: StatusRefused, Reasons: []string{ReasonSchemaInvalid}}
	}
	if err = Validate(p, in); err != nil {
		var r *Refusal
		if errors.As(err, &r) {
			return Outcome{Status: StatusRefused, Reasons: r.Reasons}
		}
		return Outcome{Status: StatusRefused, Reasons: []string{ReasonSchemaInvalid}}
	}
	p.ProposalID = ""
	switch p.SelectedOption {
	case OptionMaintenance:
		for _, u := range p.Unknowns {
			if u.Required {
				return Outcome{Status: StatusNeedsInput, Reasons: []string{ReasonRequiredUnknown}, Proposal: &p}
			}
		}
		return Outcome{Status: StatusProposed, Reasons: []string{}, Proposal: &p}
	case OptionEscalate:
		return Outcome{Status: StatusEscalated, Reasons: []string{"SPECIALIST_ESCALATED"}, Proposal: &p}
	default:
		return Outcome{Status: StatusNeedsInput, Reasons: []string{"CLARIFICATION_REQUESTED"}, Proposal: &p}
	}
}
