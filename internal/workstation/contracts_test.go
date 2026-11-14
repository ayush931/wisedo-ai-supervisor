package workstation

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func validShiftPlan() ShiftPlanBodyV1 {
	return ShiftPlanBodyV1{
		SchemaVersion: 1, LineID: "1c6b4260-a7fc-4fef-8e80-19b611f83f7b",
		StartsAt: "2026-10-20T06:00:00-04:00", EndsAt: "2026-10-20T14:00:00-04:00", Timezone: "America/Detroit",
		Assignments: []AssignmentV1{{
			WorkstationID: "7ee1e85e-7683-46b2-815e-38ce4d4681d1", JobOrderID: "ed6ceeb6-23a2-41e9-8f25-d88aedf78b58",
			JobOrderRevision: 1, OrderID: "0f711607-3600-4321-bddb-5aa105f2bd9c", OperationID: "5d4a1a15-ae68-42a7-81e0-1c607169e369",
			ItemCode: "P-100", TargetQuantity: 120, TargetCycleTimeSeconds: "2.5",
			InstructionDocumentID: "605ff7bd-07a9-4017-a8c5-bb01d918bb7f", InstructionRevision: 3,
			InstructionSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			Template:          "production", Fields: []FieldV1{{Name: "good_units", Label: "Good units", Kind: "decimal", Required: true, Unit: "count"}},
		}},
	}
}

func TestShiftPlanV1Validation(t *testing.T) {
	if err := validShiftPlan().Validate(); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
	tests := map[string]func(*ShiftPlanBodyV1){
		"schema version":                func(p *ShiftPlanBodyV1) { p.SchemaVersion = 2 },
		"offset required":               func(p *ShiftPlanBodyV1) { p.StartsAt = "2026-10-20T06:00:00" },
		"timezone required":             func(p *ShiftPlanBodyV1) { p.Timezone = "factory-local" },
		"empty timezone":                func(p *ShiftPlanBodyV1) { p.Timezone = "" },
		"host timezone":                 func(p *ShiftPlanBodyV1) { p.Timezone = "Local" },
		"bounded timezone":              func(p *ShiftPlanBodyV1) { p.Timezone = "America/" + strings.Repeat("A", 57) },
		"unknown timezone":              func(p *ShiftPlanBodyV1) { p.Timezone = "Mars/Olympus_Mons" },
		"timezone path":                 func(p *ShiftPlanBodyV1) { p.Timezone = "America/../Detroit" },
		"offset matches plant timezone": func(p *ShiftPlanBodyV1) { p.StartsAt = "2026-10-20T06:00:00Z" },
		"shift length bounded":          func(p *ShiftPlanBodyV1) { p.EndsAt = "2026-10-21T07:00:00-04:00" },
		"positive shift interval":       func(p *ShiftPlanBodyV1) { p.EndsAt = p.StartsAt },
		"canonical quantity":            func(p *ShiftPlanBodyV1) { p.Assignments[0].TargetQuantity = 0 },
		"bounded decimal precision":     func(p *ShiftPlanBodyV1) { p.Assignments[0].TargetCycleTimeSeconds = "1.0000001" },
		"template allowlist":            func(p *ShiftPlanBodyV1) { p.Assignments[0].Template = "<script>" },
		"field name allowlist":          func(p *ShiftPlanBodyV1) { p.Assignments[0].Fields[0].Name = "Good units" },
		"choice fields require bounded options": func(p *ShiftPlanBodyV1) {
			p.Assignments[0].Fields[0] = FieldV1{Name: "disposition", Label: "Disposition", Kind: "choice", Required: true}
		},
		"duplicate station": func(p *ShiftPlanBodyV1) { p.Assignments = append(p.Assignments, p.Assignments[0]) },
		"duplicate job order": func(p *ShiftPlanBodyV1) {
			second := p.Assignments[0]
			second.WorkstationID = "e0ff2d73-ae44-4fe1-918f-efc2a419da92"
			p.Assignments = append(p.Assignments, second)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			plan := validShiftPlan()
			mutate(&plan)
			if err := plan.Validate(); !errors.Is(err, ErrInvalidShiftPlan) {
				t.Fatalf("Validate() = %v, want %v", err, ErrInvalidShiftPlan)
			}
		})
	}
}

func TestDecodeShiftPlanRejectsUnknownAndDuplicateFields(t *testing.T) {
	plan, err := json.Marshal(validShiftPlan())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = DecodeShiftPlan(plan); err != nil {
		t.Fatalf("valid JSON rejected: %v", err)
	}
	duplicate := bytes.Replace(plan, []byte(`"schema_version":1`), []byte(`"schema_version":1,"schema_version":1`), 1)
	inputs := [][]byte{
		append(append([]byte(nil), plan[:len(plan)-1]...), []byte(`,"site_id":"28c5589a-90a0-4c6c-8708-8363ae4ca4f6"}`)...),
		duplicate,
		append(append([]byte(nil), plan...), bytes.Repeat([]byte(" "), (1<<20)+1-len(plan))...),
	}
	for _, input := range inputs {
		if _, err := DecodeShiftPlan(input); !errors.Is(err, ErrInvalidShiftPlan) {
			t.Errorf("DecodeShiftPlan accepted invalid input: %v", err)
		}
	}
}

// The shift plan digest ignores assignment order, uses UTC shift times and is scope-bound.
func TestShiftPlanDigestIsCanonical(t *testing.T) {
	const tenant, site = "0a0a0a0a-0a0a-4a0a-8a0a-0a0a0a0a0a0a", "1a1a1a1a-1a1a-4a1a-8a1a-1a1a1a1a1a1a"
	plan := validShiftPlan()
	second := plan.Assignments[0]
	second.WorkstationID, second.JobOrderID = "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"
	plan.Assignments = append(plan.Assignments, second)
	stored, encoded, digest, err := normalized(tenant, site, plan)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Assignments[0].WorkstationID != second.WorkstationID || stored.StartsAt != plan.StartsAt || !bytes.Contains(encoded, []byte(`"starts_at":"2026-10-20T10:00:00Z"`)) {
		t.Fatalf("normalization: %+v %s", stored, encoded)
	}
	plan.Assignments[0], plan.Assignments[1] = plan.Assignments[1], plan.Assignments[0]
	if _, _, again, _ := normalized(tenant, site, plan); again != digest {
		t.Fatal("assignment order changed the digest")
	}
	if _, _, other, _ := normalized(site, tenant, plan); other == digest {
		t.Fatal("digest is not scope-bound")
	}
	plan.Assignments[0].TargetQuantity++
	if _, _, changed, _ := normalized(tenant, site, plan); changed == digest {
		t.Fatal("changed content kept the digest")
	}
}
