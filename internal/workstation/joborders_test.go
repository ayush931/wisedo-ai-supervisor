package workstation

import (
	"errors"
	"strings"
	"testing"
)

// Cycle-time targets must already be canonical decimals, so equal values always hash equally:
// "1.50" and "1.5" cannot both be stored for the same target.
func TestJobOrderValidation(t *testing.T) {
	valid := JobOrderBodyV1{SchemaVersion: 1, OrderID: "8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e01", OperationID: "8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e02", ItemCode: "P-100", TargetQuantity: 10,
		TargetCycleTimeSeconds: "1.5", Priority: 1, DueAt: "2026-10-20T18:00:00+02:00", InstructionDocumentID: "8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e03", InstructionRevision: 1,
		InstructionSHA256: strings.Repeat("ab", 32), Template: "quality", Fields: []FieldV1{{Name: "result", Label: "Result", Kind: "choice", Required: true, Choices: []string{"pass", "fail"}}}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid job order rejected: %v", err)
	}
	for name, mutate := range map[string]func(*JobOrderBodyV1){
		"trailing zero cycle":   func(o *JobOrderBodyV1) { o.TargetCycleTimeSeconds = "1.50" },
		"leading zero cycle":    func(o *JobOrderBodyV1) { o.TargetCycleTimeSeconds = "01.5" },
		"bare point cycle":      func(o *JobOrderBodyV1) { o.TargetCycleTimeSeconds = "1." },
		"seven decimals":        func(o *JobOrderBodyV1) { o.TargetCycleTimeSeconds = "1.0000001" },
		"zero cycle":            func(o *JobOrderBodyV1) { o.TargetCycleTimeSeconds = "0" },
		"priority above nine":   func(o *JobOrderBodyV1) { o.Priority = 10 },
		"negative priority":     func(o *JobOrderBodyV1) { o.Priority = -1 },
		"due without offset":    func(o *JobOrderBodyV1) { o.DueAt = "2026-10-20T18:00:00" },
		"non-canonical order":   func(o *JobOrderBodyV1) { o.OrderID = strings.ToUpper(o.OrderID) },
		"short instruction sha": func(o *JobOrderBodyV1) { o.InstructionSHA256 = "ab" },
		"unknown template":      func(o *JobOrderBodyV1) { o.Template = "script" },
		"no fields":             func(o *JobOrderBodyV1) { o.Fields = nil },
	} {
		o := valid
		mutate(&o)
		if err := o.Validate(); !errors.Is(err, ErrInvalidJobOrder) {
			t.Errorf("%s: got %v, want ErrInvalidJobOrder", name, err)
		}
	}
}
