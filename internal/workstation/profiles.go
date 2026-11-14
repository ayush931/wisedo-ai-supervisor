package workstation

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
	"github.com/google/uuid"
)

// WIS-520 station profiles: per-workstation operator screen configuration, appended as
// immutable StationProfileBodyV1 revisions by an owner or supervisor. A profile selects one
// of the fixed templates and configures typed fields, units, choices, decimal ranges and
// approved instruction references. It is configuration data only: there is no script,
// expression, markup or URL member, and text that looks like one is refused by path.
var (
	ErrProfileUnchanged    = errors.New("station profile revision is identical to the current revision")
	ErrJobOrderNotAssigned = errors.New("job order revision is not assigned to this workstation by an approved shift plan")
)

// JobOrderAssigned reports whether the current approved revision of some shift plan assigns
// exactly this job-order revision to the workstation (any shift window). Preview uses it, and
// station entries reuse it, so a station only ever works a job order it was approved for.
func JobOrderAssigned(ctx context.Context, tx *sql.Tx, sc Scope, workstationID, jobOrderID string, revision int64) (bool, error) {
	var ok bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.shift_plan_heads h JOIN supervisor.shift_plan_assignments x ON (x.tenant_id,x.site_id,x.plan_id,x.revision)=(h.tenant_id,h.site_id,h.id,h.approved_revision)
 WHERE h.tenant_id=$1 AND h.site_id=$2 AND x.workstation_id=$3 AND x.job_order_id=$4 AND x.job_order_revision=$5)`, sc.TenantID, sc.SiteID, workstationID, jobOrderID, revision).Scan(&ok)
	return ok, err
}

const (
	stationProfileSchema        = "StationProfileBodyV1"
	stationProfileDigestVersion = 1
	maxInstructionReferences    = 16
	maxEntryValues              = 128

	CodeInvalidStationProfile = "INVALID_STATION_PROFILE"
	CodeUnsupportedStation    = "UNSUPPORTED_STATION_CONFIGURATION"
)

// ConfigError rejects a station profile and names the offending member. Paths are built
// from the closed member names and array indexes, never from free text.
type ConfigError struct{ Code, Path string }

func (e *ConfigError) Error() string { return e.Code + " at " + e.Path }

func invalidAt(path string) *ConfigError     { return &ConfigError{CodeInvalidStationProfile, path} }
func unsupportedAt(path string) *ConfigError { return &ConfigError{CodeUnsupportedStation, path} }

// InstructionRefV1 pins an approved document instruction by exact revision and hash.
type InstructionRefV1 struct {
	DocumentID string `json:"document_id"`
	Revision   int64  `json:"revision"`
	SHA256     string `json:"sha256" contract:"sha256"`
}

// ProfileFieldV1 is FieldV1 plus an optional inclusive decimal range. Min and Max are
// canonical decimal strings and are only allowed on decimal fields.
type ProfileFieldV1 struct {
	Name     string   `json:"name"`
	Label    string   `json:"label"`
	Kind     string   `json:"kind"`
	Required bool     `json:"required"`
	Unit     string   `json:"unit,omitempty"`
	Choices  []string `json:"choices,omitempty"`
	Min      string   `json:"min,omitempty"`
	Max      string   `json:"max,omitempty"`
}

func (f ProfileFieldV1) base() FieldV1 {
	return FieldV1{Name: f.Name, Label: f.Label, Kind: f.Kind, Required: f.Required, Unit: f.Unit, Choices: f.Choices}
}

// StationProfileBodyV1 is the caller-supplied screen configuration for one workstation. The
// workstation must equal the request path; line and display name may change by revision.
type StationProfileBodyV1 struct {
	SchemaVersion         int                `json:"schema_version"`
	WorkstationID         string             `json:"workstation_id"`
	LineID                string             `json:"line_id"`
	DisplayName           string             `json:"display_name"`
	Template              string             `json:"template"`
	Fields                []ProfileFieldV1   `json:"fields"`
	InstructionReferences []InstructionRefV1 `json:"instruction_references,omitempty"`
}

func supportedTemplate(t string) bool { return t == "production" || t == "quality" || t == "material" }

func supportedKind(k string) bool {
	return k == "text" || k == "decimal" || k == "choice" || k == "boolean" || k == "timestamp"
}

// executable flags operator text that a renderer, template engine or spreadsheet export could
// evaluate: template or interpolation syntax, a backtick, a script or data URL scheme, or a
// leading "=" formula. Labels and choices are rendered as plain text, so "<", ">" and "://"
// are ordinary characters ("Torque > 25 Nm").
// ponytail: closed blocklist; replace with a per-renderer escaping proof if the tablet ever
// renders rich text.
func executable(value string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	for _, marker := range []string{"{{", "}}", "${", "`"} {
		if strings.Contains(trimmed, marker) {
			return true
		}
	}
	for _, prefix := range []string{"=", "javascript:", "vbscript:", "data:"} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

// signedDecimal6 is a canonical decimal that may be negative (an entered value or a range
// bound, for example -12.5 degrees); "-0" is not canonical. It is a strict subset of the
// published decimal6 pattern.
var signedDecimal6 = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]{0,5}[1-9])?$`)

func decimalRat(value string) (*big.Rat, bool) {
	if !signedDecimal6.MatchString(value) || value == "-0" || len(value) > 32 {
		return nil, false
	}
	return new(big.Rat).SetString(value)
}

// Validate returns the first offending member in document order. Unsupported templates,
// kinds and executable-looking text are UNSUPPORTED_STATION_CONFIGURATION; every other
// failure is INVALID_STATION_PROFILE.
func (p StationProfileBodyV1) Validate() *ConfigError {
	switch {
	case p.SchemaVersion != 1:
		return invalidAt("schema_version")
	case !canonicalID(p.WorkstationID):
		return invalidAt("workstation_id")
	case !canonicalID(p.LineID):
		return invalidAt("line_id")
	case executable(p.DisplayName):
		return unsupportedAt("display_name")
	case !validText(p.DisplayName, 128):
		return invalidAt("display_name")
	case !supportedTemplate(p.Template):
		return unsupportedAt("template")
	case len(p.Fields) == 0 || len(p.Fields) > 64:
		return invalidAt("fields")
	}
	seen := make(map[string]struct{}, len(p.Fields))
	for i, f := range p.Fields {
		at := "fields[" + strconv.Itoa(i) + "]"
		if !supportedKind(f.Kind) {
			return unsupportedAt(at + ".kind")
		}
		if executable(f.Label) {
			return unsupportedAt(at + ".label")
		}
		if executable(f.Unit) {
			return unsupportedAt(at + ".unit")
		}
		for j, choice := range f.Choices {
			if executable(choice) {
				return unsupportedAt(at + ".choices[" + strconv.Itoa(j) + "]")
			}
		}
		if _, dup := seen[f.Name]; dup {
			return invalidAt(at + ".name")
		}
		seen[f.Name] = struct{}{}
		if validateFields([]FieldV1{f.base()}) != nil {
			return invalidAt(at)
		}
		lo, loOK := decimalRat(f.Min)
		hi, hiOK := decimalRat(f.Max)
		switch {
		case f.Min != "" && f.Kind != "decimal":
			return unsupportedAt(at + ".min")
		case f.Max != "" && f.Kind != "decimal":
			return unsupportedAt(at + ".max")
		case f.Min != "" && !loOK:
			return invalidAt(at + ".min")
		case f.Max != "" && !hiOK:
			return invalidAt(at + ".max")
		case loOK && hiOK && lo.Cmp(hi) > 0:
			return invalidAt(at + ".min")
		}
	}
	if len(p.InstructionReferences) > maxInstructionReferences {
		return invalidAt("instruction_references")
	}
	refs := make(map[string]struct{}, len(p.InstructionReferences))
	for i, r := range p.InstructionReferences {
		at := "instruction_references[" + strconv.Itoa(i) + "]"
		if !canonicalID(r.DocumentID) || r.Revision < 1 || !digest64.MatchString(r.SHA256) {
			return invalidAt(at)
		}
		if _, dup := refs[r.DocumentID]; dup {
			return invalidAt(at + ".document_id")
		}
		refs[r.DocumentID] = struct{}{}
	}
	return nil
}

// shape is the closed member tree of a request body; "[]" holds an array element's shape.
type shape map[string]shape

var (
	profileShape = shape{"schema_version": nil, "workstation_id": nil, "line_id": nil, "display_name": nil, "template": nil,
		"fields":                 {"[]": {"name": nil, "label": nil, "kind": nil, "required": nil, "unit": nil, "choices": nil, "min": nil, "max": nil}},
		"instruction_references": {"[]": {"document_id": nil, "revision": nil, "sha256": nil}}}
	previewShape = shape{"profile": profileShape, "job_order_id": nil, "job_order_revision": nil}
)

// checkShape returns the first member outside s, in sorted key order, as unsupported
// configuration at "<parent>.*" (an unknown name is never echoed), or a member whose JSON
// container type is wrong (an object where an array belongs, or the reverse) as invalid.
func checkShape(v any, s shape, path string) *ConfigError {
	if s == nil || v == nil {
		return nil
	}
	join := func(k string) string {
		if path == "" {
			return k
		}
		return path + "." + k
	}
	if element, isArray := s["[]"]; isArray {
		items, ok := v.([]any)
		if !ok {
			return invalidAt(path)
		}
		for i, item := range items {
			if e := checkShape(item, element, path+"["+strconv.Itoa(i)+"]"); e != nil {
				return e
			}
		}
		return nil
	}
	object, ok := v.(map[string]any)
	if !ok {
		return invalidAt(path)
	}
	keys := make([]string, 0, len(object))
	for k := range object {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		child, known := s[k]
		if !known {
			return unsupportedAt(join("*"))
		}
		if e := checkShape(object[k], child, join(k)); e != nil {
			return e
		}
	}
	return nil
}

// decodeConfig decodes data into dst, reporting an unknown member (for example "script" or
// "on_submit") as UNSUPPORTED_STATION_CONFIGURATION at its path before strict decoding.
func decodeConfig(data []byte, s shape, dst any) *ConfigError {
	var generic any
	if len(data) > limits.RequestBodyBytes || json.NewDecoder(bytes.NewReader(data)).Decode(&generic) != nil {
		return invalidAt("")
	}
	if e := checkShape(generic, s, ""); e != nil {
		return e
	}
	if strictjson.Decode(data, dst, limits.RequestBodyBytes) != nil {
		return invalidAt("")
	}
	return nil
}

// DecodeStationProfile strictly decodes and validates a station profile body.
func DecodeStationProfile(data []byte) (StationProfileBodyV1, error) {
	var p StationProfileBodyV1
	if e := decodeConfig(data, profileShape, &p); e != nil {
		return StationProfileBodyV1{}, e
	}
	if e := p.Validate(); e != nil {
		return StationProfileBodyV1{}, e
	}
	return p, nil
}

// StationProfilePreviewInputV1 previews a candidate profile, optionally merged with the
// current revision of a job order. Nothing is persisted.
type StationProfilePreviewInputV1 struct {
	Profile          StationProfileBodyV1 `json:"profile"`
	JobOrderID       string               `json:"job_order_id,omitempty"`
	JobOrderRevision int64                `json:"job_order_revision,omitempty"`
}

// DecodeStationProfilePreview strictly decodes and validates a preview request.
func DecodeStationProfilePreview(data []byte) (StationProfilePreviewInputV1, error) {
	var in StationProfilePreviewInputV1
	if e := decodeConfig(data, previewShape, &in); e != nil {
		return StationProfilePreviewInputV1{}, e
	}
	if e := in.Profile.Validate(); e != nil {
		return StationProfilePreviewInputV1{}, &ConfigError{e.Code, "profile." + e.Path}
	}
	if (in.JobOrderID == "") != (in.JobOrderRevision == 0) || (in.JobOrderID != "" && (!canonicalID(in.JobOrderID) || in.JobOrderRevision < 1)) {
		return StationProfilePreviewInputV1{}, invalidAt("job_order_id")
	}
	return in, nil
}

type FieldSource string

const (
	SourceStationProfile FieldSource = "station_profile"
	SourceJobOrder       FieldSource = "job_order"
	SourceBoth           FieldSource = "both"
)

// FormFieldV1 is one field of the resolved operator form and where it came from.
type FormFieldV1 struct {
	Field  ProfileFieldV1 `json:"field"`
	Source FieldSource    `json:"source"`
}

// ResolveForm merges a station profile with a job order's fields. Precedence:
//   - the profile's fields come first, in profile order, then job-order-only fields in job
//     order order; no field is ever dropped and a job-order requirement is never relaxed;
//   - a field in both must agree on label, kind, unit and choices; Required is the OR of
//     both, and the profile's decimal range applies;
//   - the templates must be equal.
//
// Disagreements are returned as conflict paths ("template", "job_order.fields[i]") rather
// than reconciled; a form with conflicts is not ready for entry. order may be nil.
func ResolveForm(p StationProfileBodyV1, order *JobOrderBodyV1) ([]FormFieldV1, []string) {
	fields := make([]FormFieldV1, 0, len(p.Fields))
	index := make(map[string]int, len(p.Fields))
	for _, f := range p.Fields {
		index[f.Name] = len(fields)
		fields = append(fields, FormFieldV1{Field: f, Source: SourceStationProfile})
	}
	conflicts := []string{}
	if order == nil {
		return fields, conflicts
	}
	if order.Template != p.Template {
		conflicts = append(conflicts, "template")
	}
	for i, of := range order.Fields {
		at, ok := index[of.Name]
		if !ok {
			fields = append(fields, FormFieldV1{Field: ProfileFieldV1{Name: of.Name, Label: of.Label, Kind: of.Kind, Required: of.Required, Unit: of.Unit, Choices: of.Choices}, Source: SourceJobOrder})
			continue
		}
		mine := fields[at].Field.base()
		mine.Required, of.Required = false, false
		if !sameField(mine, of) {
			conflicts = append(conflicts, "job_order.fields["+strconv.Itoa(i)+"]")
			continue
		}
		fields[at].Source = SourceBoth
		fields[at].Field.Required = fields[at].Field.Required || order.Fields[i].Required
	}
	return fields, conflicts
}

// EntryValueV1 is one operator-entered value. Every kind is a string: a canonical decimal
// with its unit, "true"/"false", an RFC 3339 timestamp with offset, a choice or text.
type EntryValueV1 struct {
	Field string `json:"field"`
	Value string `json:"value"`
	Unit  string `json:"unit,omitempty"`
}

// Entry violation codes are stable; slice 4 reports them for operator submissions.
const (
	EntryMissingRequired  = "REQUIRED_VALUE_MISSING"
	EntryUnknownField     = "UNKNOWN_FIELD"
	EntryDuplicate        = "DUPLICATE_VALUE"
	EntryEmpty            = "EMPTY_VALUE"
	EntryWrongUnit        = "UNIT_MISMATCH"
	EntryUnitNotAllowed   = "UNIT_NOT_ALLOWED"
	EntryChoiceNotAllowed = "CHOICE_NOT_ALLOWED"
	EntryInvalidDecimal   = "INVALID_DECIMAL"
	EntryOutOfRange       = "OUT_OF_RANGE"
	EntryInvalidBoolean   = "INVALID_BOOLEAN"
	EntryInvalidTimestamp = "INVALID_TIMESTAMP"
	EntryInvalidText      = "INVALID_TEXT"
	EntryTooMany          = "TOO_MANY_VALUES"
)

// EntryViolationV1 names a rejected value by its position in the submission (values[i]) or,
// for a missing required field, by the form field (fields[i]).
type EntryViolationV1 struct {
	Path string `json:"path"`
	Code string `json:"code"`
}

// ValidateEntry checks operator values against a resolved form: every required field has a
// value, every value names a form field once, decimals are canonical (at most six
// fractional digits) in the field's unit and range, choices are members, booleans are
// "true"/"false" and timestamps carry an offset. It is pure and returns violations in
// submission order followed by missing required fields in form order; nil means valid.
func ValidateEntry(form []FormFieldV1, values []EntryValueV1) []EntryViolationV1 {
	if len(values) > maxEntryValues {
		return []EntryViolationV1{{"values", EntryTooMany}}
	}
	var out []EntryViolationV1
	byName := make(map[string]ProfileFieldV1, len(form))
	for _, f := range form {
		byName[f.Field.Name] = f.Field
	}
	supplied := make(map[string]struct{}, len(values))
	for i, v := range values {
		at := "values[" + strconv.Itoa(i) + "]"
		f, known := byName[v.Field]
		_, dup := supplied[v.Field]
		if code := entryCode(f, known, dup, v); code != "" {
			out = append(out, EntryViolationV1{at, code})
		}
		supplied[v.Field] = struct{}{}
	}
	for i, f := range form {
		if _, ok := supplied[f.Field.Name]; f.Field.Required && !ok {
			out = append(out, EntryViolationV1{"fields[" + strconv.Itoa(i) + "]", EntryMissingRequired})
		}
	}
	return out
}

func entryCode(f ProfileFieldV1, known, dup bool, v EntryValueV1) string {
	switch {
	case !known:
		return EntryUnknownField
	case dup:
		return EntryDuplicate
	case v.Value == "":
		return EntryEmpty
	case f.Kind != "decimal" && v.Unit != "":
		return EntryUnitNotAllowed
	}
	switch f.Kind {
	case "decimal":
		n, ok := decimalRat(v.Value)
		if !ok {
			return EntryInvalidDecimal
		}
		if v.Unit != f.Unit {
			return EntryWrongUnit
		}
		if lo, ok := decimalRat(f.Min); ok && n.Cmp(lo) < 0 {
			return EntryOutOfRange
		}
		if hi, ok := decimalRat(f.Max); ok && n.Cmp(hi) > 0 {
			return EntryOutOfRange
		}
	case "choice":
		for _, c := range f.Choices {
			if c == v.Value {
				return ""
			}
		}
		return EntryChoiceNotAllowed
	case "boolean":
		if v.Value != "true" && v.Value != "false" {
			return EntryInvalidBoolean
		}
	case "timestamp":
		if _, ok := validTime(v.Value); !ok {
			return EntryInvalidTimestamp
		}
	default:
		// Entered text is operator content that reaches summaries, handovers and exports:
		// the same executable filter as comments, labels and choices (acceptance 3).
		if !validText(v.Value, 1024) || executable(v.Value) {
			return EntryInvalidText
		}
	}
	return ""
}

type StationProfileRevisionV1 struct {
	Revision               int64                `json:"revision"`
	State                  JobOrderState        `json:"state"`
	Profile                StationProfileBodyV1 `json:"profile"`
	CanonicalPayloadSHA256 string               `json:"canonical_payload_digest" contract:"sha256"`
	DigestSchemaVersion    int                  `json:"digest_schema_version"`
	SubmittedBy            ActorRefV1           `json:"submitted_by"`
	CreatedAt              time.Time            `json:"created_at"`
}

// StationProfileV1 is a workstation's profile with its ordered revision history; the
// current profile is Revisions[CurrentRevision-1]. CurrentRevision is the If-Match value.
// On an idempotent replay, ReplayedRevision is the revision the original request created;
// the history and current revision still reflect the profile now, so a revision superseded
// since then is never labelled current.
type StationProfileV1 struct {
	WorkstationID    string                     `json:"workstation_id"`
	CurrentRevision  int64                      `json:"current_revision"`
	Revisions        []StationProfileRevisionV1 `json:"revisions"`
	Replayed         bool                       `json:"replayed"`
	ReplayedRevision int64                      `json:"replayed_revision,omitempty"`
}

type StationProfileSummaryV1 struct {
	WorkstationID   string `json:"workstation_id"`
	CurrentRevision int64  `json:"current_revision"`
	DisplayName     string `json:"display_name,omitempty" contract:"description=Current human-readable name for this configured workstation."`
	LineID          string `json:"line_id,omitempty" contract:"uuid,description=UUID of the production line configured for this workstation."`
}

type StationProfilePageV1 struct {
	Workstations []StationProfileSummaryV1 `json:"workstations"`
	NextCursor   string                    `json:"next_cursor,omitempty"`
}

// InstructionStatusV1 re-checks an instruction reference: Available only while it is still
// a confirmed-current approved document revision, otherwise Reason says why (rule 06).
type InstructionStatusV1 struct {
	Instruction InstructionRefV1  `json:"instruction"`
	Source      FieldSource       `json:"source"`
	Available   bool              `json:"available"`
	Reason      InstructionReason `json:"reason,omitempty"`
}

// StationProfilePreviewV1 is the resolved operator form for a candidate profile.
// ProfileDigest equals the canonical digest a revision with this exact profile would store.
// Ready is false while any conflict remains or any instruction is unavailable.
type StationProfilePreviewV1 struct {
	WorkstationID    string                `json:"workstation_id"`
	ProfileDigest    string                `json:"profile_digest" contract:"sha256"`
	Template         string                `json:"template"`
	JobOrderID       string                `json:"job_order_id,omitempty"`
	JobOrderRevision int64                 `json:"job_order_revision,omitempty"`
	Ready            bool                  `json:"ready"`
	Fields           []FormFieldV1         `json:"fields"`
	Conflicts        []string              `json:"conflicts"`
	Instructions     []InstructionStatusV1 `json:"instructions"`
}

type StationProfileService struct{ plans *ShiftPlanService }

func NewStationProfiles(db *sql.DB) *StationProfileService {
	return &StationProfileService{NewShiftPlans(db)}
}

// WithSessionPolicy returns a copy of the service that enforces p (WIS-290).
func (s *StationProfileService) WithSessionPolicy(p session.Policy) *StationProfileService {
	return &StationProfileService{s.plans.WithSessionPolicy(p)}
}

func profileEnvelope(sc Scope, p StationProfileBodyV1) ([]byte, string, error) {
	return canonical.Envelope(stationProfileSchema, stationProfileDigestVersion, sc.TenantID, sc.SiteID, p)
}

func profileAudit(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, workstationID string, revision int64, event string) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO supervisor.station_profile_audit(tenant_id,site_id,actor_issuer,actor_subject,workstation_id,revision,event_type) VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid,NULLIF($6,0),$7)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, workstationID, revision, event)
	return e
}

// Revise appends a revision for the workstation in the path. If-Match "0" creates the
// profile; otherwise it must name the current revision. The new revision supersedes the
// previous one at once; history is never rewritten.
func (s *StationProfileService) Revise(ctx context.Context, a Actor, sc Scope, workstationID string, p StationProfileBodyV1, key string, expected int64) (StationProfileV1, error) {
	if !canonicalID(workstationID) {
		return StationProfileV1{}, invalidAt("workstation_id")
	}
	if e := p.Validate(); e != nil {
		return StationProfileV1{}, e
	}
	if p.WorkstationID != workstationID {
		return StationProfileV1{}, invalidAt("workstation_id")
	}
	if !validText(key, 128) || expected < 0 {
		return StationProfileV1{}, ErrInvalidShiftPlan
	}
	if _, _, err := profileEnvelope(sc, p); err != nil {
		return StationProfileV1{}, invalidAt("")
	}
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftWrite)
	if err != nil {
		return StationProfileV1{}, err
	}
	defer tx.Rollback()
	out, err := reviseStationProfileTx(ctx, tx, a, sc, workstationID, p, key, expected)
	if err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

// reviseStationProfileTx is the body of Revise inside a caller's site-locked owner or
// supervisor transaction (begin shiftWrite). It re-validates its input; the caller commits
// through finish. Configuration draft promotion uses it so a promoted profile takes exactly
// the normal revision path.
func reviseStationProfileTx(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, workstationID string, p StationProfileBodyV1, key string, expected int64) (StationProfileV1, error) {
	if !canonicalID(workstationID) {
		return StationProfileV1{}, invalidAt("workstation_id")
	}
	if e := p.Validate(); e != nil {
		return StationProfileV1{}, e
	}
	if p.WorkstationID != workstationID {
		return StationProfileV1{}, invalidAt("workstation_id")
	}
	if !validText(key, 128) || expected < 0 {
		return StationProfileV1{}, ErrInvalidShiftPlan
	}
	encoded, digest, err := profileEnvelope(sc, p)
	if err != nil {
		return StationProfileV1{}, invalidAt("")
	}
	hash := requestDigest([]any{"station_profile.revise", workstationID, expected, digest})
	var stored string
	var storedRevision int64
	e := tx.QueryRowContext(ctx, `SELECT request_sha256,revision FROM supervisor.station_profile_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&stored, &storedRevision)
	switch {
	case e == nil && stored != hash:
		return StationProfileV1{}, ErrConflict
	case e == nil:
		if err = profileAudit(ctx, tx, a, sc, workstationID, storedRevision, "mutation.replayed"); err != nil {
			return StationProfileV1{}, err
		}
		out, err := loadStationProfile(ctx, tx, sc, workstationID)
		if err != nil {
			return out, err
		}
		out.Replayed, out.ReplayedRevision = true, storedRevision
		return out, nil
	case !errors.Is(e, sql.ErrNoRows):
		return StationProfileV1{}, e
	}
	var current int64
	var currentDigest string
	e = tx.QueryRowContext(ctx, `SELECT h.current_revision,r.canonical_payload_sha256 FROM supervisor.station_profile_heads h JOIN supervisor.station_profile_revisions r ON (r.tenant_id,r.site_id,r.workstation_id,r.revision)=(h.tenant_id,h.site_id,h.workstation_id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.workstation_id=$3 FOR UPDATE OF h`, sc.TenantID, sc.SiteID, workstationID).Scan(&current, &currentDigest)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return StationProfileV1{}, e
	}
	if current != expected {
		return StationProfileV1{}, ErrRevision
	}
	if digest == currentDigest {
		return StationProfileV1{}, ErrProfileUnchanged
	}
	lineAllowed, err := SiteProfileLineAllowed(ctx, tx, sc, p.LineID)
	if err != nil {
		return StationProfileV1{}, err
	}
	if !lineAllowed {
		return StationProfileV1{}, invalidAt("line_id")
	}
	for _, ref := range p.InstructionReferences {
		approved, err := instructionApproved(ctx, tx, sc, ref)
		if err != nil {
			return StationProfileV1{}, err
		}
		if !approved {
			return StationProfileV1{}, ErrInstructionNotApproved
		}
	}
	// Writers already serialize on the site row (begin); the conditional writes keep a
	// concurrent create or revise a 412, never a unique violation, if that ever changes.
	next, event := current+1, "station_profile.revised"
	var written sql.Result
	if current == 0 {
		written, e = tx.ExecContext(ctx, `INSERT INTO supervisor.station_profile_heads(tenant_id,site_id,workstation_id,current_revision) VALUES($1,$2,$3,1) ON CONFLICT DO NOTHING`, sc.TenantID, sc.SiteID, workstationID)
		event = "station_profile.created"
	} else {
		written, e = tx.ExecContext(ctx, `UPDATE supervisor.station_profile_heads SET current_revision=$4 WHERE tenant_id=$1 AND site_id=$2 AND workstation_id=$3 AND current_revision=$5`, sc.TenantID, sc.SiteID, workstationID, next, current)
	}
	if e != nil {
		return StationProfileV1{}, e
	}
	if n, err := written.RowsAffected(); err != nil || n != 1 {
		return StationProfileV1{}, ErrRevision
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return StationProfileV1{}, invalidAt("")
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO supervisor.station_profile_revisions(tenant_id,site_id,workstation_id,revision,payload,canonical_payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5::jsonb,$6,$7,$8,$9,$10)`, sc.TenantID, sc.SiteID, workstationID, next, string(payload), string(encoded), digest, stationProfileDigestVersion, a.Issuer, a.Subject); e != nil {
		return StationProfileV1{}, e
	}
	// A same-key request that committed first makes this one a conflict, never a 500.
	if written, e = tx.ExecContext(ctx, `INSERT INTO supervisor.station_profile_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_sha256,workstation_id,revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, hash, workstationID, next); e != nil {
		return StationProfileV1{}, e
	}
	if n, err := written.RowsAffected(); err != nil || n != 1 {
		return StationProfileV1{}, ErrConflict
	}
	if e = profileAudit(ctx, tx, a, sc, workstationID, next, event); e != nil {
		return StationProfileV1{}, e
	}
	return loadStationProfile(ctx, tx, sc, workstationID)
}

// Get returns a workstation's profile with every revision.
func (s *StationProfileService) Get(ctx context.Context, a Actor, sc Scope, workstationID string) (StationProfileV1, error) {
	if !canonicalID(workstationID) {
		return StationProfileV1{}, ErrNotFound
	}
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftRead)
	if err != nil {
		return StationProfileV1{}, err
	}
	defer tx.Rollback()
	out, err := loadStationProfile(ctx, tx, sc, workstationID)
	if err != nil {
		return out, err
	}
	if err = profileAudit(ctx, tx, a, sc, workstationID, out.CurrentRevision, "station_profile.read"); err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

// List pages configured workstations by ID.
func (s *StationProfileService) List(ctx context.Context, a Actor, sc Scope, limit int, cursor string) (StationProfilePageV1, error) {
	out := StationProfilePageV1{Workstations: []StationProfileSummaryV1{}}
	if limit < 1 || limit > 100 || (cursor != "" && !canonicalID(cursor)) {
		return out, invalidAt("")
	}
	if cursor == "" {
		cursor = uuid.Nil.String()
	}
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftRead)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT h.workstation_id,h.current_revision,COALESCE(r.payload->>'display_name',''),COALESCE(r.payload->>'line_id','')
		FROM supervisor.station_profile_heads h
		JOIN supervisor.station_profile_revisions r ON r.tenant_id=h.tenant_id AND r.site_id=h.site_id AND r.workstation_id=h.workstation_id AND r.revision=h.current_revision
		WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.workstation_id>$3 ORDER BY h.workstation_id LIMIT $4`, sc.TenantID, sc.SiteID, cursor, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var w StationProfileSummaryV1
		if err = rows.Scan(&w.WorkstationID, &w.CurrentRevision, &w.DisplayName, &w.LineID); err != nil {
			return out, err
		}
		out.Workstations = append(out.Workstations, w)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(out.Workstations) > limit {
		out.Workstations = out.Workstations[:limit]
		out.NextCursor = out.Workstations[limit-1].WorkstationID
	}
	if err = profileAudit(ctx, tx, a, sc, "", 0, "station_profile.listed"); err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

// Preview resolves the operator form for a candidate profile without persisting it. A pinned
// job order must be its current revision; instructions are re-checked against the document
// ledger. It never records a delivery.
func (s *StationProfileService) Preview(ctx context.Context, a Actor, sc Scope, workstationID string, in StationProfilePreviewInputV1) (StationProfilePreviewV1, error) {
	out := StationProfilePreviewV1{WorkstationID: workstationID, Template: in.Profile.Template, JobOrderID: in.JobOrderID, JobOrderRevision: in.JobOrderRevision, Instructions: []InstructionStatusV1{}}
	if !canonicalID(workstationID) {
		return out, invalidAt("workstation_id")
	}
	if e := in.Profile.Validate(); e != nil {
		return out, &ConfigError{e.Code, "profile." + e.Path}
	}
	if in.Profile.WorkstationID != workstationID {
		return out, invalidAt("profile.workstation_id")
	}
	_, digest, err := profileEnvelope(sc, in.Profile)
	if err != nil {
		return out, invalidAt("profile")
	}
	out.ProfileDigest = digest
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftRead)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var order *JobOrderBodyV1
	if in.JobOrderID != "" {
		loaded, err := loadJobOrder(ctx, tx, sc, in.JobOrderID)
		switch {
		case errors.Is(err, ErrNotFound) || (err == nil && in.JobOrderRevision > int64(len(loaded.Revisions))):
			return out, ErrJobOrderUnknown
		case err != nil:
			return out, err
		case in.JobOrderRevision != loaded.CurrentRevision:
			return out, ErrJobOrderNotCurrent
		}
		assigned, err := JobOrderAssigned(ctx, tx, sc, workstationID, in.JobOrderID, in.JobOrderRevision)
		if err != nil {
			return out, err
		}
		if !assigned {
			return out, ErrJobOrderNotAssigned
		}
		order = &loaded.Revisions[in.JobOrderRevision-1].Order
	}
	out.Fields, out.Conflicts = ResolveForm(in.Profile, order)
	refs := make([]InstructionStatusV1, 0, len(in.Profile.InstructionReferences)+1)
	for _, r := range in.Profile.InstructionReferences {
		refs = append(refs, InstructionStatusV1{Instruction: r, Source: SourceStationProfile})
	}
	if order != nil {
		refs = append(refs, InstructionStatusV1{Instruction: orderInstruction(*order), Source: SourceJobOrder})
	}
	out.Ready = len(out.Conflicts) == 0
	for i := range refs {
		reason, err := instructionReason(ctx, tx, sc, refs[i].Instruction)
		if err != nil {
			return out, err
		}
		refs[i].Available, refs[i].Reason = reason == "", reason
		out.Ready = out.Ready && reason == ""
	}
	out.Instructions = refs
	if err = profileAudit(ctx, tx, a, sc, workstationID, 0, "station_profile.previewed"); err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

func loadStationProfile(ctx context.Context, tx *sql.Tx, sc Scope, workstationID string) (StationProfileV1, error) {
	out := StationProfileV1{Revisions: []StationProfileRevisionV1{}}
	e := tx.QueryRowContext(ctx, `SELECT workstation_id,current_revision FROM supervisor.station_profile_heads WHERE tenant_id=$1 AND site_id=$2 AND workstation_id=$3`, sc.TenantID, sc.SiteID, workstationID).Scan(&out.WorkstationID, &out.CurrentRevision)
	if errors.Is(e, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if e != nil {
		return out, e
	}
	rows, e := tx.QueryContext(ctx, `SELECT revision,payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject,created_at FROM supervisor.station_profile_revisions WHERE tenant_id=$1 AND site_id=$2 AND workstation_id=$3 ORDER BY revision`, sc.TenantID, sc.SiteID, workstationID)
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		var r StationProfileRevisionV1
		var payload []byte
		if e = rows.Scan(&r.Revision, &payload, &r.CanonicalPayloadSHA256, &r.DigestSchemaVersion, &r.SubmittedBy.Issuer, &r.SubmittedBy.Subject, &r.CreatedAt); e != nil {
			return out, e
		}
		// Stored bodies are re-validated and re-verified against their digest on every read.
		if r.Profile, e = DecodeStationProfile(payload); e != nil {
			return out, errIntegrity
		}
		if _, recomputed, err := profileEnvelope(sc, r.Profile); err != nil || recomputed != r.CanonicalPayloadSHA256 || r.Revision != int64(len(out.Revisions)+1) {
			return out, errIntegrity
		}
		r.CreatedAt, r.State = r.CreatedAt.UTC(), JobOrderSuperseded
		if r.Revision == out.CurrentRevision {
			r.State = JobOrderCurrent
		}
		out.Revisions = append(out.Revisions, r)
	}
	if e = rows.Err(); e != nil {
		return out, e
	}
	if int64(len(out.Revisions)) != out.CurrentRevision {
		return out, errIntegrity
	}
	return out, nil
}

// Executable exposes the executable-text rule to other packages that render untrusted text
// (WIS-290 member display names from the IdP).
func Executable(value string) bool { return executable(value) }
