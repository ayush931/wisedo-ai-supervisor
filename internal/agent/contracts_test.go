package agent

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
)

func validConversationEnvelope() DelegationEnvelopeV1 {
	return DelegationEnvelopeV1{
		SchemaVersion: SchemaVersion, RunID: "00000000-0000-4000-8000-000000000001",
		Role: RoleInvestigation, RequestKind: RequestConversation,
		RequestID: "00000000-0000-4000-8000-000000000002", ConversationID: "00000000-0000-4000-8000-000000000003",
		TenantID: "00000000-0000-4000-8000-000000000004", SiteID: "00000000-0000-4000-8000-000000000005",
		ContextSnapshotID:    stringRef("00000000-0000-4000-8000-000000000006"),
		RequiredEvidenceRefs: []string{"00000000-0000-4000-8000-000000000007"},
		AllowedToolNames:     []string{ToolEvidenceGet, ToolContextGet},
		PrincipalReference:   "00000000-0000-4000-8000-000000000008",
		PolicyRevision:       2, Deadline: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
		RemainingRequests: 3, RemainingTokens: 24000, RemainingCostMicrousd: 1000000,
	}
}

// orchestratorParent is the hop-0 orchestrator. It never calls tools itself; its envelope
// carries the server-captured tool, context snapshot and evidence scope that it delegates.
func orchestratorParent() DelegationEnvelopeV1 {
	parent := validConversationEnvelope()
	parent.RunID = "00000000-0000-4000-8000-000000000030"
	parent.Role = RoleOrchestrator
	parent.AllowedToolNames = []string{ToolContextGet, ToolEvidenceGet, ToolClaimsSearch, ToolMeasurementsGet}
	return parent
}

func validChild(parent DelegationEnvelopeV1) DelegationEnvelopeV1 {
	child := parent
	child.RunID = "00000000-0000-4000-8000-000000000010"
	child.ParentRunID = stringRef(parent.RunID)
	child.Hop = 1
	child.Role = RoleInvestigation
	child.AllowedToolNames = []string{ToolEvidenceGet, ToolContextGet}
	child.RemainingRequests = 2
	child.RemainingTokens = 12000
	child.RemainingCostMicrousd = 500000
	child.Deadline = parent.Deadline.Add(-time.Minute)
	return child
}

func validResult() SpecialistResultV1 {
	return SpecialistResultV1{
		SchemaVersion: SchemaVersion, RunID: "00000000-0000-4000-8000-000000000001",
		Role: RoleInvestigation, Status: "needs_input", ArtifactRef: nil,
		CitedEvidenceIDs:      []string{"00000000-0000-4000-8000-000000000007"},
		MissingRequiredFields: []string{"shift.actual_units"}, Contradictions: []string{},
		ConciseRationale: "The source does not report actual units.", FailureCode: nil,
	}
}

func stringRef(value string) *string { return &value }

func TestDelegationEnvelopeValidate(t *testing.T) {
	base := validConversationEnvelope()
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	caseEnvelope := base
	caseEnvelope.RequestKind = RequestCase
	caseEnvelope.RequestID = ""
	caseEnvelope.ConversationID = ""
	caseEnvelope.CaseID = "00000000-0000-4000-8000-000000000009"
	if err := caseEnvelope.Validate(); err != nil {
		t.Fatalf("valid case envelope: %v", err)
	}

	tests := []struct {
		name string
		edit func(*DelegationEnvelopeV1)
	}{
		{"unsupported schema", func(v *DelegationEnvelopeV1) { v.SchemaVersion++ }},
		{"missing principal", func(v *DelegationEnvelopeV1) { v.PrincipalReference = "" }},
		{"missing policy", func(v *DelegationEnvelopeV1) { v.PolicyRevision = 0 }},
		{"missing explicit evidence scope", func(v *DelegationEnvelopeV1) { v.RequiredEvidenceRefs = nil }},
		{"duplicate evidence ref", func(v *DelegationEnvelopeV1) {
			v.RequiredEvidenceRefs = append(v.RequiredEvidenceRefs, v.RequiredEvidenceRefs[0])
		}},
		{"unknown tool", func(v *DelegationEnvelopeV1) { v.AllowedToolNames = []string{"shell"} }},
		{"context tool without context", func(v *DelegationEnvelopeV1) { v.ContextSnapshotID = nil }},
		{"conversation missing request", func(v *DelegationEnvelopeV1) { v.RequestID = "" }},
		{"conversation missing conversation", func(v *DelegationEnvelopeV1) { v.ConversationID = "" }},
		{"conversation with case id", func(v *DelegationEnvelopeV1) { v.CaseID = "00000000-0000-4000-8000-000000000009" }},
		{"case investigation without context", func(v *DelegationEnvelopeV1) {
			v.RequestKind, v.RequestID, v.ConversationID = RequestCase, "", ""
			v.CaseID, v.ContextSnapshotID = "00000000-0000-4000-8000-000000000009", nil
		}},
		{"case without id", func(v *DelegationEnvelopeV1) { v.RequestKind, v.RequestID, v.ConversationID = RequestCase, "", "" }},
		{"model tool for executor", func(v *DelegationEnvelopeV1) { v.Role = RoleExecutor }},
		{"non-UTC deadline", func(v *DelegationEnvelopeV1) {
			v.Deadline = time.Date(2030, 1, 2, 4, 4, 5, 0, time.FixedZone("+01", 3600))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := base
			test.edit(&value)
			if err := value.Validate(); !errors.Is(err, ErrInvalidEnvelope) {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestValidateChild(t *testing.T) {
	parent := orchestratorParent()
	child := validChild(parent)
	if err := parent.ValidateChild(child); err != nil {
		t.Fatalf("valid narrowed child: %v", err)
	}
	caseParent := parent
	caseParent.RequestKind, caseParent.RequestID, caseParent.ConversationID = RequestCase, "", ""
	caseParent.CaseID = "00000000-0000-4000-8000-000000000009"
	caseChild := validChild(caseParent)
	if err := caseParent.ValidateChild(caseChild); err != nil {
		t.Fatalf("valid case child: %v", err)
	}

	tests := []struct {
		name   string
		parent DelegationEnvelopeV1
		edit   func(p, c *DelegationEnvelopeV1)
	}{
		{"different tenant", parent, func(_, v *DelegationEnvelopeV1) { v.TenantID = "00000000-0000-4000-8000-000000000011" }},
		{"different principal", parent, func(_, v *DelegationEnvelopeV1) { v.PrincipalReference = "00000000-0000-4000-8000-000000000012" }},
		{"tool outside role table", parent, func(_, v *DelegationEnvelopeV1) {
			v.Role, v.AllowedToolNames = RoleProposal, []string{ToolEvidenceGet, ToolClaimsSearch}
		}},
		// WIS-275 review: child authority is intersection(parent, role, policy). A run the server
		// scoped to evidence.get only must not hand claims.search to a discovery child, even
		// though the discovery role table allows it.
		{"tool outside parent scope", parent, func(p, v *DelegationEnvelopeV1) {
			p.AllowedToolNames = []string{ToolEvidenceGet}
			v.Role, v.AllowedToolNames, v.ContextSnapshotID = RoleDiscoveryMapping, []string{ToolEvidenceGet, ToolClaimsSearch}, p.ContextSnapshotID
		}},
		{"feedback with measurements", parent, func(_, v *DelegationEnvelopeV1) {
			v.Role, v.AllowedToolNames = RoleFeedback, []string{ToolMeasurementsGet}
		}},
		{"new evidence", parent, func(_, v *DelegationEnvelopeV1) {
			v.RequiredEvidenceRefs = append(v.RequiredEvidenceRefs, "00000000-0000-4000-8000-000000000013")
		}},
		{"larger budget", parent, func(_, v *DelegationEnvelopeV1) { v.RemainingTokens = parent.RemainingTokens + 1 }},
		{"later deadline", parent, func(_, v *DelegationEnvelopeV1) { v.Deadline = parent.Deadline.Add(time.Second) }},
		{"new context", parent, func(_, v *DelegationEnvelopeV1) {
			v.ContextSnapshotID = stringRef("00000000-0000-4000-8000-000000000014")
		}},
		{"snapshot dropped", parent, func(_, v *DelegationEnvelopeV1) {
			v.Role, v.AllowedToolNames, v.ContextSnapshotID = RoleProposal, []string{ToolEvidenceGet}, nil
		}},
		{"snapshot added to parent without one", parent, func(p, _ *DelegationEnvelopeV1) { p.ContextSnapshotID = nil }},
		{"wrong parent", parent, func(_, v *DelegationEnvelopeV1) { v.ParentRunID = stringRef("00000000-0000-4000-8000-000000000015") }},
		{"stale cancellation generation", parent, func(_, v *DelegationEnvelopeV1) { v.CancellationGeneration++ }},
		{"different request id", parent, func(_, v *DelegationEnvelopeV1) { v.RequestID = "00000000-0000-4000-8000-000000000016" }},
		{"different conversation id", parent, func(_, v *DelegationEnvelopeV1) { v.ConversationID = "00000000-0000-4000-8000-000000000017" }},
		{"different case id", caseParent, func(_, v *DelegationEnvelopeV1) { v.CaseID = "00000000-0000-4000-8000-000000000018" }},
		{"hop 0 child", parent, func(_, v *DelegationEnvelopeV1) { v.Hop, v.ParentRunID = 0, nil }},
		{"specialist parent", parent, func(p, _ *DelegationEnvelopeV1) {
			p.Role, p.AllowedToolNames = RoleInvestigation, []string{ToolEvidenceGet, ToolContextGet}
		}},
		{"orchestrator child", parent, func(_, v *DelegationEnvelopeV1) { v.Role, v.AllowedToolNames = RoleOrchestrator, []string{} }},
		{"executor child", parent, func(_, v *DelegationEnvelopeV1) { v.Role, v.AllowedToolNames = RoleExecutor, []string{} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := test.parent
			c := validChild(test.parent)
			test.edit(&p, &c)
			if err := p.ValidateChild(c); !errors.Is(err, ErrInvalidChild) {
				t.Fatalf("ValidateChild() error = %v", err)
			}
		})
	}

	t.Run("child that is itself a parent", func(t *testing.T) {
		grandchild := validChild(child)
		grandchild.RunID = "00000000-0000-4000-8000-000000000019"
		if err := child.ValidateChild(grandchild); !errors.Is(err, ErrInvalidChild) {
			t.Fatalf("ValidateChild() error = %v", err)
		}
	})
}

func TestSpecialistResultValidate(t *testing.T) {
	base := validResult()
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	valid := []struct {
		name string
		edit func(*SpecialistResultV1)
	}{
		{"success with citation", func(v *SpecialistResultV1) { v.Status, v.MissingRequiredFields = "success", []string{} }},
		{"failed with code", func(v *SpecialistResultV1) { v.Status, v.FailureCode = "failed", stringRef("context_overflow") }},
		{"refused with code", func(v *SpecialistResultV1) { v.Status, v.FailureCode = "refused", stringRef("SCOPE_DENIED") }},
		{"needs_input with contradiction", func(v *SpecialistResultV1) { v.Contradictions = []string{"counts disagree"} }},
	}
	for _, test := range valid {
		t.Run("valid "+test.name, func(t *testing.T) {
			value := base
			test.edit(&value)
			if err := value.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
	tests := []struct {
		name string
		edit func(*SpecialistResultV1)
	}{
		{"unknown status", func(v *SpecialistResultV1) { v.Status = "approved" }},
		{"unknown role", func(v *SpecialistResultV1) { v.Role = "administrator" }},
		{"non-model executor role", func(v *SpecialistResultV1) { v.Role = RoleExecutor }},
		{"missing citation scope", func(v *SpecialistResultV1) { v.CitedEvidenceIDs = nil }},
		{"duplicate citation", func(v *SpecialistResultV1) { v.CitedEvidenceIDs = append(v.CitedEvidenceIDs, v.CitedEvidenceIDs[0]) }},
		{"oversized rationale", func(v *SpecialistResultV1) { v.ConciseRationale = strings.Repeat("x", maxRationale+1) }},
		{"bad artifact ref", func(v *SpecialistResultV1) { v.ArtifactRef = stringRef("not-an-id") }},
		{"success without citation", func(v *SpecialistResultV1) { v.Status, v.CitedEvidenceIDs = "success", []string{} }},
		{"success with failure code", func(v *SpecialistResultV1) { v.Status, v.FailureCode = "success", stringRef("X") }},
		{"success with contradictions", func(v *SpecialistResultV1) { v.Status, v.Contradictions = "success", []string{"counts disagree"} }},
		{"partial with contradictions", func(v *SpecialistResultV1) { v.Status, v.Contradictions = "partial", []string{"counts disagree"} }},
		{"failed with contradictions", func(v *SpecialistResultV1) {
			v.Status, v.FailureCode, v.Contradictions = "failed", stringRef("X"), []string{"counts disagree"}
		}},
		{"failed without code", func(v *SpecialistResultV1) { v.Status = "failed" }},
		{"refused without code", func(v *SpecialistResultV1) { v.Status = "refused" }},
		{"needs_input with failure code", func(v *SpecialistResultV1) { v.FailureCode = stringRef("MISSING_INPUT") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := base
			test.edit(&value)
			if err := value.Validate(); !errors.Is(err, ErrInvalidResult) {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestSpecialistResultValidateFor(t *testing.T) {
	envelope := validConversationEnvelope()
	if err := validResult().ValidateFor(envelope); err != nil {
		t.Fatalf("valid scoped result: %v", err)
	}
	orchestrator := orchestratorParent()
	orchestrator.RunID = envelope.RunID
	tests := []struct {
		name     string
		envelope DelegationEnvelopeV1
		edit     func(*SpecialistResultV1)
	}{
		{"different run", envelope, func(v *SpecialistResultV1) { v.RunID = "00000000-0000-4000-8000-000000000020" }},
		{"different role", envelope, func(v *SpecialistResultV1) { v.Role = RoleProposal }},
		{"citation outside bound evidence", envelope, func(v *SpecialistResultV1) {
			v.CitedEvidenceIDs = []string{"00000000-0000-4000-8000-000000000021"}
		}},
		{"non-specialist envelope", orchestrator, func(*SpecialistResultV1) {}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := validResult()
			test.edit(&value)
			if err := value.ValidateFor(test.envelope); !errors.Is(err, ErrInvalidResult) {
				t.Fatalf("ValidateFor() error = %v", err)
			}
		})
	}
}

func TestDecodeRoundTrip(t *testing.T) {
	envelope := validChild(orchestratorParent())
	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeDelegationEnvelope(data)
	if err != nil {
		t.Fatalf("DecodeDelegationEnvelope() error = %v", err)
	}
	if !decoded.Deadline.Equal(envelope.Deadline) {
		t.Fatalf("deadline = %v, want %v", decoded.Deadline, envelope.Deadline)
	}
	decoded.Deadline = envelope.Deadline
	if !reflect.DeepEqual(decoded, envelope) {
		t.Fatalf("envelope round trip = %+v, want %+v", decoded, envelope)
	}

	result := validResult()
	data, err = json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	decodedResult, err := DecodeSpecialistResult(data)
	if err != nil {
		t.Fatalf("DecodeSpecialistResult() error = %v", err)
	}
	if !reflect.DeepEqual(decodedResult, result) {
		t.Fatalf("result round trip = %+v, want %+v", decodedResult, result)
	}
}

func TestDecodeRejectsNonStrictJSON(t *testing.T) {
	resultJSON, err := json.Marshal(validResult())
	if err != nil {
		t.Fatal(err)
	}
	envelopeJSON, err := json.Marshal(validConversationEnvelope())
	if err != nil {
		t.Fatal(err)
	}
	result, envelope := string(resultJSON), string(envelopeJSON)
	withField := func(doc, field string) string { return "{" + field + "," + doc[1:] }
	tests := []struct {
		name   string
		decode func([]byte) error
		data   string
	}{
		{"result approved field", decodeResult, withField(result, `"approved":true`)},
		{"result tenant_id field", decodeResult, withField(result, `"tenant_id":"00000000-0000-4000-8000-000000000004"`)},
		{"result case-variant key", decodeResult, strings.Replace(result, `"run_id"`, `"RUN_ID"`, 1)},
		{"result duplicate key", decodeResult, withField(result, `"status":"success"`)},
		{"result trailing data", decodeResult, result + `{}`},
		{"result NaN", decodeResult, strings.Replace(result, `"schema_version":1`, `"schema_version":NaN`, 1)},
		{"result empty", decodeResult, ``},
		{"envelope unknown field", decodeEnvelope, withField(envelope, `"token":"x"`)},
		{"envelope case-variant key", decodeEnvelope, strings.Replace(envelope, `"tenant_id"`, `"Tenant_ID"`, 1)},
		{"envelope trailing data", decodeEnvelope, envelope + ` 1`},
		{"envelope numeric deadline", decodeEnvelope, strings.Replace(envelope, `"2030-01-02T03:04:05Z"`, `1893553445`, 1)},
		{"envelope NaN budget", decodeEnvelope, strings.Replace(envelope, `"remaining_tokens":24000`, `"remaining_tokens":NaN`, 1)},
		{"envelope oversized", decodeEnvelope, withField(envelope, `"run_id":"`+strings.Repeat("x", MaxContractBytes)+`"`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.data == result || test.data == envelope {
				t.Fatal("mutation did not apply; fixture text changed")
			}
			if err := test.decode([]byte(test.data)); !errors.Is(err, strictjson.ErrInvalid) {
				t.Fatalf("decode error = %v", err)
			}
		})
	}
}

func decodeResult(data []byte) error {
	_, err := DecodeSpecialistResult(data)
	return err
}

func decodeEnvelope(data []byte) error {
	_, err := DecodeDelegationEnvelope(data)
	return err
}
