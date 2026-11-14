// Package knowledge defines candidate-only knowledge. No client field confers approval.
package knowledge

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

var ErrInvalidContract = errors.New("invalid candidate knowledge contract")

type EntityInput struct {
	EntityID       string   `json:"entity_id"`
	Type           string   `json:"type"`
	AssetKind      string   `json:"asset_kind,omitempty"`
	Name           string   `json:"display_name"`
	Aliases        []string `json:"aliases,omitempty"`
	SourceID       string   `json:"source_id"`
	SourceRevision int64    `json:"source_revision"`
}

type ClaimInput struct {
	ClaimID         string     `json:"claim_id"`
	SubjectID       string     `json:"subject_id"`
	SubjectRevision int64      `json:"subject_revision,omitempty"`
	Predicate       string     `json:"predicate"`
	ObjectEntityID  string     `json:"object_entity_id,omitempty"`
	ObjectRevision  int64      `json:"object_revision,omitempty"`
	Literal         *Literal   `json:"literal,omitempty"`
	EffectiveFrom   string     `json:"effective_from"`
	EffectiveTo     *string    `json:"effective_to,omitempty"`
	EvidenceClass   string     `json:"evidence_class"`
	SourceSpanIDs   []string   `json:"source_span_ids"`
	Confidence      *string    `json:"confidence,omitempty"`
	Extractor       *Extractor `json:"extractor,omitempty"`
}

// Value is retained as typed JSON so booleans cannot be coerced from strings,
// and decimal values must be strings rather than lossy JSON numbers.
type Literal struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
	Unit  string          `json:"unit,omitempty"`
}
type Extractor struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Version      string `json:"version"`
	PromptSHA256 string `json:"prompt_sha256"`
}

func contractDecode(data []byte, out any) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' || !utf8.Valid(data) {
		return ErrInvalidContract
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return ErrInvalidContract
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return ErrInvalidContract
	}
	return nil
}
func (e *EntityInput) UnmarshalJSON(data []byte) error {
	type plain EntityInput
	var out plain
	if err := contractDecode(data, &out); err != nil {
		return err
	}
	*e = EntityInput(out)
	return e.Validate()
}
func (c *ClaimInput) UnmarshalJSON(data []byte) error {
	type plain ClaimInput
	var out plain
	if err := contractDecode(data, &out); err != nil {
		return err
	}
	*c = ClaimInput(out)
	return c.Validate()
}
func (l *Literal) UnmarshalJSON(data []byte) error {
	type plain Literal
	var out plain
	if err := contractDecode(data, &out); err != nil {
		return err
	}
	*l = Literal(out)
	return l.Validate()
}
func (e *Extractor) UnmarshalJSON(data []byte) error {
	type plain Extractor
	var out plain
	if err := contractDecode(data, &out); err != nil {
		return err
	}
	*e = Extractor(out)
	return e.Validate()
}
func contractUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id != uuid.Nil && id.String() == s
}

// decisionReasonChars bounds a review, review revocation or conflict resolution reason
// (WIS-270 Decision "reason:1..2000chars"); migration 041 enforces the same character limit.
const decisionReasonChars = 2000

func contractText(s string, max int) bool {
	return utf8.ValidString(s) && len(s) > 0 && !strings.ContainsRune(s, 0) && strings.TrimSpace(s) != "" && utf8.RuneCountInString(s) <= max
}
func knownEntity(t string) bool {
	switch t {
	case "site", "area", "asset", "process", "operation", "order", "material", "tool", "role", "qualification", "document", "constraint",
		// WIS-611 value-stream vocabulary: who supplies and receives material, where it is
		// received and stored, and where quality is checked.
		"supplier", "customer", "location", "inspection_point":
		return true
	}
	return false
}
func (e *EntityInput) Validate() error {
	if e == nil || !contractUUID(e.EntityID) || !contractUUID(e.SourceID) || e.SourceRevision < 1 || !knownEntity(e.Type) || !contractText(e.Name, 200) || len(e.Aliases) > 20 {
		return ErrInvalidContract
	}
	if (e.Type == "asset" && (e.AssetKind != "line" && e.AssetKind != "machine")) || (e.Type != "asset" && e.AssetKind != "") {
		return ErrInvalidContract
	}
	seen := map[string]bool{}
	for _, a := range e.Aliases {
		if !contractText(a, 200) || seen[a] {
			return ErrInvalidContract
		}
		seen[a] = true
	}
	return nil
}

var decimalPattern = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]{0,5}[1-9])?$`)
var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var unitPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_./%-]{0,31}$`)

// Reject excessive timestamp precision rather than silently rounding it in PostgreSQL.
var timePattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,6})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)

func decimal(s string) (*big.Rat, error) {
	if len(s) > 32 || s == "-0" || !decimalPattern.MatchString(s) {
		return nil, ErrInvalidContract
	}
	v, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, ErrInvalidContract
	}
	return v, nil
}
func normalizedTime(s string) (time.Time, string, error) {
	if !timePattern.MatchString(s) {
		return time.Time{}, "", ErrInvalidContract
	}
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || v.Year() < 1 || v.Year() > 9999 {
		return time.Time{}, "", ErrInvalidContract
	}
	v = v.UTC()
	if v.Year() < 1 || v.Year() > 9999 {
		return time.Time{}, "", ErrInvalidContract
	}
	return v, v.Format(time.RFC3339Nano), nil
}
func (l *Literal) Validate() error {
	if l == nil || len(l.Value) == 0 || !utf8.Valid(l.Value) {
		return ErrInvalidContract
	}
	if l.Unit != "" && !unitPattern.MatchString(l.Unit) {
		return ErrInvalidContract
	}
	switch l.Type {
	case "string", "decimal", "timestamp":
		var value string
		if len(bytes.TrimSpace(l.Value)) == 0 || bytes.TrimSpace(l.Value)[0] != '"' || json.Unmarshal(l.Value, &value) != nil {
			return ErrInvalidContract
		}
		switch l.Type {
		case "string":
			if !contractText(value, 2000) || l.Unit != "" {
				return ErrInvalidContract
			}
		case "decimal":
			if _, err := decimal(value); err != nil {
				return err
			}
		case "timestamp":
			_, normalized, err := normalizedTime(value)
			if err != nil || l.Unit != "" {
				return ErrInvalidContract
			}
			value = normalized
		}
		l.Value, _ = json.Marshal(value)
	case "boolean":
		value := bytes.TrimSpace(l.Value)
		if (!bytes.Equal(value, []byte("true")) && !bytes.Equal(value, []byte("false"))) || l.Unit != "" {
			return ErrInvalidContract
		}
		l.Value = append(json.RawMessage(nil), value...)
	default:
		return ErrInvalidContract
	}
	return nil
}
func (e *Extractor) Validate() error {
	if e == nil || !contractText(e.Provider, 80) || !contractText(e.Model, 200) || !contractText(e.Version, 128) || !hashPattern.MatchString(e.PromptSHA256) {
		return ErrInvalidContract
	}
	return nil
}

// relationshipPredicates and propertyPredicates are the claim vocabulary. Every other list of
// predicates (the graph read's neighbourhood, below) is built from these two, so a predicate added
// here cannot be left out elsewhere (WIS-611 audit: the neighbourhood was a hand-copied list).
var (
	relationshipPredicates = []string{"located_in", "part_of_asset", "performs", "part_of", "precedes", "consumes", "produces", "requires_tool", "requires_qualification", "responsible_role", "follows_document", "has_constraint",
		"supplied_by", "received_at", "stored_at", "inspected_at", "ships_to"}
	propertyPredicates = []string{"capacity_per_hour", "escalation_after_seconds", "objective", "actual_step",
		"lead_time_days", "cycle_time_seconds", "setup_time_seconds", "batch_size", "yield_ratio", "sample_size", "inspection_method"}
)

// Predicates is every claim predicate: relationships, then properties.
func Predicates() []string {
	return append(append([]string{}, relationshipPredicates...), propertyPredicates...)
}

func property(p string) bool { return slices.Contains(propertyPredicates, p) }

// valueStreamUnits gives the one unit each WIS-611 numeric property is held in; its name already
// says which. A claim may omit the unit, in which case it is set to this one, but any other unit
// is refused: a value is never converted, so "2 weeks" cannot pass as lead_time_days = 2.
var valueStreamUnits = map[string]string{
	"lead_time_days": "day", "cycle_time_seconds": "s", "setup_time_seconds": "s",
	"batch_size": "count", "sample_size": "count", "yield_ratio": "",
}

// countProperty reports the properties that count whole items: a positive integer.
func countProperty(p string) bool {
	return p == "batch_size" || p == "sample_size"
}

// PropertyLiteralType is the literal type a property predicate takes ("string" or "decimal"), or
// "" when the predicate is not a property. Agents shape their proposals with it; the claim
// validators remain the authority.
func PropertyLiteralType(p string) string {
	switch {
	case !property(p):
		return ""
	case textProperty(p):
		return "string"
	}
	return "decimal"
}

// IsRelationship reports whether p relates two entities.
func IsRelationship(p string) bool { return relationship(p) }

// textProperty reports the properties whose literal is text; every other property is a
// non-negative decimal.
func textProperty(p string) bool {
	return p == "objective" || p == "actual_step" || p == "inspection_method"
}

func relationship(p string) bool { return slices.Contains(relationshipPredicates, p) }

// singleValued reports the predicates a subject holds one value of at a time: two current claims
// that disagree over an overlapping period are a conflict for review. Relationships such as
// supplied_by or performs are many-valued and never conflict by value.
func singleValued(p string) bool {
	switch p {
	case "part_of_asset", "located_in", "part_of", "capacity_per_hour", "escalation_after_seconds", "objective", "actual_step",
		"lead_time_days", "cycle_time_seconds", "setup_time_seconds", "batch_size", "yield_ratio", "sample_size", "inspection_method":
		return true
	}
	return false
}
func (c *ClaimInput) Validate() error {
	if c == nil || !contractUUID(c.ClaimID) || !contractUUID(c.SubjectID) || len(c.SourceSpanIDs) < 1 || len(c.SourceSpanIDs) > 20 {
		return ErrInvalidContract
	}
	// Zero pins remain decodable only for immutable pre-pin payloads from migration 006.
	// Creation/public write paths must also call ValidateEntityRevisions.
	if c.SubjectRevision < 0 || c.ObjectRevision < 0 || (c.ObjectEntityID == "" && c.ObjectRevision != 0) {
		return ErrInvalidContract
	}
	if (c.ObjectEntityID != "") == (c.Literal != nil) {
		return ErrInvalidContract
	}
	if c.ObjectEntityID != "" {
		if !contractUUID(c.ObjectEntityID) || c.ObjectEntityID == c.SubjectID || !relationship(c.Predicate) {
			return ErrInvalidContract
		}
	} else {
		if !property(c.Predicate) || c.Literal.Validate() != nil {
			return ErrInvalidContract
		}
		if textProperty(c.Predicate) {
			if c.Literal.Type != "string" {
				return ErrInvalidContract
			}
		} else {
			if c.Literal.Type != "decimal" {
				return ErrInvalidContract
			}
			var value string
			_ = json.Unmarshal(c.Literal.Value, &value)
			n, err := decimal(value)
			if err != nil || n.Sign() < 0 {
				return ErrInvalidContract
			}
			if unit, fixed := valueStreamUnits[c.Predicate]; fixed {
				if c.Literal.Unit != "" && c.Literal.Unit != unit {
					return ErrInvalidContract
				}
				c.Literal.Unit = unit
			}
			if countProperty(c.Predicate) && (!n.IsInt() || n.Sign() <= 0) {
				return ErrInvalidContract
			}
			// A yield is a fraction of input that comes out good: at most one.
			if c.Predicate == "yield_ratio" && n.Cmp(big.NewRat(1, 1)) > 0 {
				return ErrInvalidContract
			}
		}
	}
	switch c.EvidenceClass {
	case "report", "observation", "hypothesis", "constraint":
	default:
		return ErrInvalidContract
	}
	seen := map[string]bool{}
	for _, id := range c.SourceSpanIDs {
		if !contractUUID(id) || seen[id] {
			return ErrInvalidContract
		}
		seen[id] = true
	}
	start, from, err := normalizedTime(c.EffectiveFrom)
	if err != nil {
		return err
	}
	c.EffectiveFrom = from
	if c.EffectiveTo != nil {
		end, to, err := normalizedTime(*c.EffectiveTo)
		if err != nil || !end.After(start) {
			return ErrInvalidContract
		}
		c.EffectiveTo = &to
	}
	if c.Confidence != nil {
		n, err := decimal(*c.Confidence)
		if err != nil || n.Sign() < 0 || n.Cmp(big.NewRat(1, 1)) > 0 {
			return ErrInvalidContract
		}
	}
	if c.Extractor != nil {
		return c.Extractor.Validate()
	}
	return nil
}

// ValidateEntityPair uses persisted entity types, never types supplied alongside a claim.
// Scope/provenance lookup and cross-record routing-cycle checks belong to storage.
func ValidateEntityPair(predicate, subjectType, objectType string) error {
	if !knownEntity(subjectType) || !knownEntity(objectType) {
		return ErrInvalidContract
	}
	valid := false
	switch predicate {
	case "located_in":
		valid = (subjectType == "asset" || subjectType == "area") && (objectType == "area" || objectType == "site")
	case "part_of_asset":
		valid = subjectType == "asset" && objectType == "asset"
	case "performs":
		valid = subjectType == "asset" && objectType == "operation"
	case "part_of":
		valid = subjectType == "operation" && objectType == "process"
	case "precedes":
		valid = subjectType == "operation" && objectType == "operation"
	case "consumes", "produces":
		valid = subjectType == "operation" && objectType == "material"
	case "requires_tool":
		valid = subjectType == "operation" && objectType == "tool"
	case "requires_qualification":
		valid = subjectType == "operation" && objectType == "qualification"
	case "responsible_role":
		valid = (subjectType == "process" || subjectType == "operation") && objectType == "role"
	case "follows_document":
		valid = (subjectType == "process" || subjectType == "operation") && objectType == "document"
	case "has_constraint":
		valid = (subjectType == "process" || subjectType == "operation" || subjectType == "asset") && objectType == "constraint"
	case "supplied_by":
		valid = subjectType == "material" && objectType == "supplier"
	case "received_at", "stored_at":
		valid = subjectType == "material" && objectType == "location"
	case "inspected_at":
		valid = (subjectType == "material" || subjectType == "operation") && objectType == "inspection_point"
	case "ships_to":
		valid = subjectType == "material" && objectType == "customer"
	}
	if !valid {
		return ErrInvalidContract
	}
	return nil
}
func ValidateLiteralSubject(predicate, subjectType string) error {
	valid := false
	switch predicate {
	case "capacity_per_hour", "escalation_after_seconds":
		valid = subjectType == "asset" || subjectType == "operation" || subjectType == "process"
	case "objective":
		valid = subjectType == "site" || subjectType == "area" || subjectType == "process"
	case "actual_step":
		valid = subjectType == "operation"
	case "lead_time_days":
		valid = subjectType == "material" || subjectType == "supplier"
	case "cycle_time_seconds", "setup_time_seconds":
		valid = subjectType == "operation" || subjectType == "asset"
	case "batch_size", "yield_ratio":
		valid = subjectType == "operation"
	case "sample_size", "inspection_method":
		valid = subjectType == "inspection_point"
	}
	if !valid {
		return ErrInvalidContract
	}
	return nil
}

// ValidateEntityRevisions is the write-boundary gate. Validate deliberately accepts
// absent legacy pins so historical payload bytes never need rewriting. Storage
// resolves explicit pins to same-site immutable entity revisions and checks currency.
func (c *ClaimInput) ValidateEntityRevisions() error {
	if c == nil || c.SubjectRevision <= 0 {
		return ErrInvalidContract
	}
	if c.ObjectEntityID != "" {
		if c.ObjectRevision <= 0 {
			return ErrInvalidContract
		}
	} else if c.ObjectRevision != 0 {
		return ErrInvalidContract
	}
	return nil
}
