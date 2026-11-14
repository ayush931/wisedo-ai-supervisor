// Package workstation defines the manual-first workstation and shift contracts.
package workstation

import (
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
	"github.com/google/uuid"
)

var (
	ErrInvalidShiftPlan = errors.New("invalid workstation shift plan")
	fieldName           = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	decimal6            = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]{0,5}[1-9])?$`)
	digest64            = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// ianaZone admits "UTC" or an Area/Location name; "", "Local" and file paths are refused
	// before time.LoadLocation, which maps "" to UTC and "Local" to the host zone.
	ianaZone = regexp.MustCompile(`^(UTC|[A-Z][A-Za-z]+(/[A-Za-z0-9][A-Za-z0-9_+-]*)+)$`)
)

const maxAssignments = 200

// ShiftPlanBodyV1 is the caller-supplied schedule. Site and revision identifiers
// are server-scoped metadata and deliberately do not appear in this request body.
type ShiftPlanBodyV1 struct {
	SchemaVersion int            `json:"schema_version"`
	LineID        string         `json:"line_id"`
	StartsAt      string         `json:"starts_at"`
	EndsAt        string         `json:"ends_at"`
	Timezone      string         `json:"timezone"`
	Assignments   []AssignmentV1 `json:"assignments"`
}

// AssignmentV1 delivers a job-order revision to one workstation for this shift.
// Instructions are pinned by revision and digest; text is never executable.
type AssignmentV1 struct {
	WorkstationID          string    `json:"workstation_id"`
	JobOrderID             string    `json:"job_order_id"`
	JobOrderRevision       int64     `json:"job_order_revision"`
	OrderID                string    `json:"order_id"`
	OperationID            string    `json:"operation_id"`
	ItemCode               string    `json:"item_code"`
	TargetQuantity         int64     `json:"target_quantity"`
	TargetCycleTimeSeconds string    `json:"target_cycle_time_seconds,omitempty"`
	InstructionDocumentID  string    `json:"instruction_document_id"`
	InstructionRevision    int64     `json:"instruction_revision"`
	InstructionSHA256      string    `json:"instruction_sha256"`
	Template               string    `json:"template"`
	Fields                 []FieldV1 `json:"fields"`
}

// FieldV1 configures one supported operator input; it cannot carry code or a query.
type FieldV1 struct {
	Name     string   `json:"name"`
	Label    string   `json:"label"`
	Kind     string   `json:"kind"`
	Required bool     `json:"required"`
	Unit     string   `json:"unit,omitempty"`
	Choices  []string `json:"choices,omitempty"`
}

func canonicalID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

func validText(value string, max int) bool {
	return len(value) > 0 && len(value) <= max && utf8.ValidString(value) && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) == -1
}

func validTime(value string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, value)
	return t, err == nil
}

// Validate enforces a bounded, non-overlapping shift assignment set and only
// permits the fixed operator templates and field kinds supported by the client.
func (p ShiftPlanBodyV1) Validate() error {
	if p.SchemaVersion != 1 || len(p.Assignments) == 0 || len(p.Assignments) > maxAssignments || !validShift(p.LineID, p.StartsAt, p.EndsAt, p.Timezone) {
		return ErrInvalidShiftPlan
	}
	seenStations := make(map[string]struct{}, len(p.Assignments))
	seenOrders := make(map[string]struct{}, len(p.Assignments))
	for _, a := range p.Assignments {
		if !canonicalID(a.WorkstationID) || !canonicalID(a.JobOrderID) || a.JobOrderRevision < 1 || !canonicalID(a.OrderID) || !canonicalID(a.OperationID) ||
			!validText(a.ItemCode, 128) || a.TargetQuantity < 1 || !canonicalID(a.InstructionDocumentID) || a.InstructionRevision < 1 || !digest64.MatchString(a.InstructionSHA256) ||
			!supportedTemplate(a.Template) {
			return ErrInvalidShiftPlan
		}
		if _, exists := seenStations[a.WorkstationID]; exists {
			return ErrInvalidShiftPlan
		}
		seenStations[a.WorkstationID] = struct{}{}
		if _, exists := seenOrders[a.JobOrderID]; exists {
			return ErrInvalidShiftPlan
		}
		seenOrders[a.JobOrderID] = struct{}{}
		if a.TargetCycleTimeSeconds != "" && (!validDecimal6(a.TargetCycleTimeSeconds) || a.TargetCycleTimeSeconds == "0" || len(a.TargetCycleTimeSeconds) > 32) {
			return ErrInvalidShiftPlan
		}
		if err := validateFields(a.Fields); err != nil {
			return err
		}
	}
	return nil
}

// validShift checks a line and a half-open shift interval whose offsets agree with its IANA
// timezone.
func validShift(lineID, startsAt, endsAt, timezone string) bool {
	start, startOK := validTime(startsAt)
	end, endOK := validTime(endsAt)
	if !canonicalID(lineID) || !startOK || !endOK || !start.Before(end) || len(timezone) > 64 || !ianaZone.MatchString(timezone) {
		return false
	}
	location, err := time.LoadLocation(timezone)
	// shortcut: shift assignments are limited to 24 hours, upgrade when longer windows need scheduling.
	return err == nil && end.Sub(start) <= 24*time.Hour && zoneMatches(start, location) && zoneMatches(end, location)
}

func zoneMatches(value time.Time, location *time.Location) bool {
	_, supplied := value.Zone()
	_, expected := value.In(location).Zone()
	return supplied == expected
}

// DecodeShiftPlan rejects oversized, unknown-field, duplicate-key and malformed
// JSON before running semantic validation.
func DecodeShiftPlan(data []byte) (ShiftPlanBodyV1, error) {
	var plan ShiftPlanBodyV1
	if strictjson.Decode(data, &plan, limits.RequestBodyBytes) != nil || plan.Validate() != nil {
		return ShiftPlanBodyV1{}, ErrInvalidShiftPlan
	}
	return plan, nil
}

func validateFields(fields []FieldV1) error {
	if len(fields) == 0 || len(fields) > 64 {
		return ErrInvalidShiftPlan
	}
	seen := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		if !fieldName.MatchString(field.Name) || !validText(field.Label, 128) || executable(field.Label) || executable(field.Unit) || !supportedKind(field.Kind) {
			return ErrInvalidShiftPlan
		}
		if _, exists := seen[field.Name]; exists {
			return ErrInvalidShiftPlan
		}
		seen[field.Name] = struct{}{}
		if (field.Kind == "decimal" && !validText(field.Unit, 32)) || (field.Kind != "decimal" && field.Unit != "") {
			return ErrInvalidShiftPlan
		}
		if field.Kind == "choice" {
			if len(field.Choices) == 0 || len(field.Choices) > 64 {
				return ErrInvalidShiftPlan
			}
			choices := make(map[string]struct{}, len(field.Choices))
			for _, choice := range field.Choices {
				if !validText(choice, 128) || executable(choice) {
					return ErrInvalidShiftPlan
				}
				if _, exists := choices[choice]; exists {
					return ErrInvalidShiftPlan
				}
				choices[choice] = struct{}{}
			}
		} else if len(field.Choices) != 0 {
			return ErrInvalidShiftPlan
		}
	}
	return nil
}

func validDecimal6(value string) bool {
	return decimal6.MatchString(value)
}
