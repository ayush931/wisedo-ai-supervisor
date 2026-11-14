// Package agent defines the versioned, authority-scoped contracts exchanged with
// specialist model calls. It contains no model client or execution authority.
package agent

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
)

const SchemaVersion = 1

const (
	RequestConversation = "conversation"
	RequestCase         = "case"
	// RequestExtraction is one OCR run over one scan job (WIS-625); the job ID is its
	// conversation_id and request_id.
	RequestExtraction = "extraction"

	RoleDiscoveryMapping = "discovery/mapping"
	RoleInvestigation    = "investigation"
	RoleProposal         = "proposal"
	RoleFeedback         = "feedback"
	RoleOrchestrator     = "orchestrator"
	RoleExecutor         = "executor"

	ToolContextGet      = "context.get"
	ToolEvidenceGet     = "evidence.get"
	ToolClaimsSearch    = "claims.search"
	ToolMeasurementsGet = "measurements.get"
)

var (
	ErrInvalidEnvelope = errors.New("invalid delegation envelope")
	ErrInvalidResult   = errors.New("invalid specialist result")
	ErrInvalidChild    = errors.New("child delegation exceeds parent scope")
)

const (
	maxEvidenceRefs = 500
	maxReasonFields = 100
	maxReasonRunes  = 500
	maxRationale    = 2000
	// MaxContractBytes bounds a decoded envelope or result. The largest valid
	// result (500 refs, two 100x500-rune lists, 2000-rune rationale) is ~0.4 MiB.
	MaxContractBytes = 1 << 20
)

// roleTools is the per-role read-tool ceiling a child may receive.
// PROPOSAL (WIS-275 role matrix lists what each role reads, not tool names; this
// table is derived narrowly from the "Reads" column and fails closed):
//   - discovery/mapping: authorized source snippets, approved ontology -> evidence.get, claims.search
//   - investigation: scoped context, approved claims, measurements, cited evidence -> all four
//   - proposal: validated investigation and task template; cites evidence -> evidence.get
//   - feedback: completed cases, outcomes, reviewed feedback -> evidence.get
//
// Orchestrator and executor are not delegation targets and have no entry.
var roleTools = map[string][]string{
	RoleDiscoveryMapping: {ToolEvidenceGet, ToolClaimsSearch},
	RoleInvestigation:    {ToolContextGet, ToolEvidenceGet, ToolClaimsSearch, ToolMeasurementsGet},
	RoleProposal:         {ToolEvidenceGet},
	RoleFeedback:         {ToolEvidenceGet},
}

// DecodeDelegationEnvelope strictly decodes an envelope. JSON violations return
// strictjson.ErrInvalid; callers still run Validate/ValidateChild.
func DecodeDelegationEnvelope(data []byte) (DelegationEnvelopeV1, error) {
	var e DelegationEnvelopeV1
	if err := strictjson.Decode(data, &e, MaxContractBytes); err != nil {
		return DelegationEnvelopeV1{}, err
	}
	return e, nil
}

// DecodeSpecialistResult strictly decodes untrusted model output: unknown or
// case-variant fields (e.g. "approved", "tenant_id"), duplicate keys, trailing
// data and non-JSON numbers return strictjson.ErrInvalid. Callers still run ValidateFor.
func DecodeSpecialistResult(data []byte) (SpecialistResultV1, error) {
	var r SpecialistResultV1
	if err := strictjson.Decode(data, &r, MaxContractBytes); err != nil {
		return SpecialistResultV1{}, err
	}
	return r, nil
}

// DelegationEnvelopeV1 is built by trusted Go code. PrincipalReference is an
// opaque server-side lookup key, never a bearer credential.
type DelegationEnvelopeV1 struct {
	SchemaVersion          int       `json:"schema_version"`
	RunID                  string    `json:"run_id"`
	ParentRunID            *string   `json:"parent_run_id"`
	Role                   string    `json:"role"`
	RequestKind            string    `json:"request_kind"`
	RequestID              string    `json:"request_id,omitempty"`
	ConversationID         string    `json:"conversation_id,omitempty"`
	CaseID                 string    `json:"case_id,omitempty"`
	TenantID               string    `json:"tenant_id"`
	SiteID                 string    `json:"site_id"`
	ContextSnapshotID      *string   `json:"context_snapshot_id"`
	RequiredEvidenceRefs   []string  `json:"required_evidence_refs"`
	AllowedToolNames       []string  `json:"allowed_tool_names"`
	PrincipalReference     string    `json:"principal_reference"`
	PolicyRevision         int64     `json:"policy_revision"`
	Deadline               time.Time `json:"deadline"`
	RemainingRequests      int64     `json:"remaining_requests"`
	RemainingTokens        int64     `json:"remaining_tokens"`
	RemainingCostMicrousd  int64     `json:"remaining_cost_microusd"`
	CancellationGeneration int64     `json:"cancellation_generation"`
	Hop                    int       `json:"hop"`
}

// Validate checks the closed V1 shape without consulting a clock or external state.
// Passing validation does not establish current identity, membership, revocation,
// or policy authority; the runtime must recheck those before each effect.
func (e DelegationEnvelopeV1) Validate() error {
	if e.SchemaVersion != SchemaVersion || !isUUID(e.RunID) || !validRole(e.Role) ||
		!isUUID(e.TenantID) || !isUUID(e.SiteID) || !isUUID(e.PrincipalReference) ||
		e.PolicyRevision < 1 || !validDeadline(e.Deadline) || e.RemainingRequests < 0 ||
		e.RemainingTokens < 0 || e.RemainingCostMicrousd < 0 ||
		e.CancellationGeneration < 0 || e.Hop < 0 || e.Hop > 1 {
		return ErrInvalidEnvelope
	}
	if e.ParentRunID == nil {
		if e.Hop != 0 {
			return ErrInvalidEnvelope
		}
	} else if !isUUID(*e.ParentRunID) || *e.ParentRunID == e.RunID || e.Hop != 1 {
		return ErrInvalidEnvelope
	}
	if e.RequiredEvidenceRefs == nil || len(e.RequiredEvidenceRefs) > maxEvidenceRefs || !uniqueUUIDs(e.RequiredEvidenceRefs) {
		return ErrInvalidEnvelope
	}
	if e.AllowedToolNames == nil || !validTools(e.AllowedToolNames) {
		return ErrInvalidEnvelope
	}
	// The executor is deterministic and reads no model tools. The orchestrator never calls a
	// tool either; its AllowedToolNames is the run's server-captured tool scope, the parent
	// term of every child's intersection (ValidateChild).
	if e.Role == RoleExecutor && len(e.AllowedToolNames) != 0 {
		return ErrInvalidEnvelope
	}
	if e.ContextSnapshotID != nil && !isUUID(*e.ContextSnapshotID) {
		return ErrInvalidEnvelope
	}
	switch e.RequestKind {
	case RequestConversation:
		if !isUUID(e.RequestID) || !isUUID(e.ConversationID) || e.CaseID != "" {
			return ErrInvalidEnvelope
		}
	case RequestCase:
		if !isUUID(e.CaseID) || e.RequestID != "" || e.ConversationID != "" {
			return ErrInvalidEnvelope
		}
	default:
		return ErrInvalidEnvelope
	}
	if (e.RequestKind == RequestCase && e.Role == RoleInvestigation ||
		containsTool(e.AllowedToolNames, ToolContextGet) || containsTool(e.AllowedToolNames, ToolMeasurementsGet)) && e.ContextSnapshotID == nil {
		return ErrInvalidEnvelope
	}
	return nil
}

// ValidateChild verifies that a single child call is a strict authority subset.
// The parent must be the hop-0 orchestrator; the child must be a specialist at hop 1
// (children cannot delegate). Child tools are within intersection(parent scope, role
// table): the orchestrator envelope carries the run's captured tool, context snapshot and
// evidence scope even though it uses none of them itself, and the child keeps the same
// request/conversation/case and snapshot ids. Current policy is the runtime's third term;
// it must recheck the tools against policy before each dispatch.
func (parent DelegationEnvelopeV1) ValidateChild(child DelegationEnvelopeV1) error {
	if parent.Validate() != nil || child.Validate() != nil ||
		parent.Role != RoleOrchestrator || parent.Hop != 0 || parent.ParentRunID != nil ||
		!validSpecialistRole(child.Role) || child.Hop != parent.Hop+1 ||
		child.ParentRunID == nil || *child.ParentRunID != parent.RunID ||
		child.TenantID != parent.TenantID || child.SiteID != parent.SiteID ||
		child.PrincipalReference != parent.PrincipalReference || child.PolicyRevision != parent.PolicyRevision ||
		child.RequestKind != parent.RequestKind || child.RequestID != parent.RequestID ||
		child.ConversationID != parent.ConversationID || child.CaseID != parent.CaseID ||
		child.CancellationGeneration != parent.CancellationGeneration || child.Deadline.After(parent.Deadline) ||
		child.RemainingRequests > parent.RemainingRequests || child.RemainingTokens > parent.RemainingTokens ||
		child.RemainingCostMicrousd > parent.RemainingCostMicrousd ||
		!sameOptional(child.ContextSnapshotID, parent.ContextSnapshotID) ||
		!subset(child.AllowedToolNames, parent.AllowedToolNames) ||
		!subset(child.AllowedToolNames, roleTools[child.Role]) ||
		!subset(child.RequiredEvidenceRefs, parent.RequiredEvidenceRefs) {
		return ErrInvalidChild
	}
	return nil
}

// SpecialistResultV1 is untrusted model output. It deliberately carries no
// identity, policy, approval, dispatch, or other authority-bearing fields.
type SpecialistResultV1 struct {
	SchemaVersion         int      `json:"schema_version"`
	RunID                 string   `json:"run_id"`
	Role                  string   `json:"role"`
	Status                string   `json:"status"`
	ArtifactRef           *string  `json:"artifact_ref"`
	CitedEvidenceIDs      []string `json:"cited_evidence_ids"`
	MissingRequiredFields []string `json:"missing_required_fields"`
	Contradictions        []string `json:"contradictions"`
	ConciseRationale      string   `json:"concise_rationale"`
	FailureCode           *string  `json:"failure_code"`
}

func (r SpecialistResultV1) Validate() error {
	if r.SchemaVersion != SchemaVersion || !isUUID(r.RunID) || !validSpecialistRole(r.Role) ||
		!validStatus(r.Status) || r.CitedEvidenceIDs == nil || len(r.CitedEvidenceIDs) > maxEvidenceRefs || !uniqueUUIDs(r.CitedEvidenceIDs) ||
		!validTextList(r.MissingRequiredFields, maxReasonFields, maxReasonRunes) ||
		!validTextList(r.Contradictions, maxReasonFields, maxReasonRunes) ||
		!validText(r.ConciseRationale, maxRationale) {
		return ErrInvalidResult
	}
	if r.ArtifactRef != nil && !isUUID(*r.ArtifactRef) {
		return ErrInvalidResult
	}
	if r.FailureCode != nil && !validText(*r.FailureCode, maxReasonRunes) {
		return ErrInvalidResult
	}
	// Contradictory evidence becomes needs_input, never a majority vote (WIS-275).
	if len(r.Contradictions) != 0 && r.Status != "needs_input" {
		return ErrInvalidResult
	}
	switch r.Status {
	case "success":
		if len(r.CitedEvidenceIDs) == 0 || r.FailureCode != nil {
			return ErrInvalidResult
		}
	case "failed", "refused":
		if r.FailureCode == nil {
			return ErrInvalidResult
		}
	case "needs_input":
		if r.FailureCode != nil {
			return ErrInvalidResult
		}
	}
	return nil
}

// ValidateFor binds untrusted result identity and citations to the server-built envelope.
// Citations must be within RequiredEvidenceRefs: WIS-275 scopes evidence.get to the
// captured scope and grants no evidence ids beyond it.
// It does not replace runtime checks of current role, policy, or evidence access.
func (r SpecialistResultV1) ValidateFor(envelope DelegationEnvelopeV1) error {
	if envelope.Validate() != nil || !validSpecialistRole(envelope.Role) || r.Validate() != nil || r.RunID != envelope.RunID || r.Role != envelope.Role ||
		!subset(r.CitedEvidenceIDs, envelope.RequiredEvidenceRefs) {
		return ErrInvalidResult
	}
	return nil
}

func isUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id != uuid.Nil && id.String() == s
}

func validRole(role string) bool {
	switch role {
	case RoleDiscoveryMapping, RoleInvestigation, RoleProposal, RoleFeedback, RoleOrchestrator, RoleExecutor:
		return true
	default:
		return false
	}
}

func validSpecialistRole(role string) bool {
	return role == RoleDiscoveryMapping || role == RoleInvestigation || role == RoleProposal || role == RoleFeedback
}

func validStatus(status string) bool {
	switch status {
	case "success", "partial", "needs_input", "refused", "failed":
		return true
	default:
		return false
	}
}

func validDeadline(deadline time.Time) bool {
	if deadline.IsZero() {
		return false
	}
	_, offset := deadline.Zone()
	return offset == 0
}

func validTools(tools []string) bool {
	if len(tools) > 4 {
		return false
	}
	seen := make(map[string]bool, len(tools))
	for _, tool := range tools {
		if tool != ToolContextGet && tool != ToolEvidenceGet && tool != ToolClaimsSearch && tool != ToolMeasurementsGet || seen[tool] {
			return false
		}
		seen[tool] = true
	}
	return true
}

func uniqueUUIDs(values []string) bool {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !isUUID(value) || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func validTextList(values []string, maxCount, maxRunes int) bool {
	if values == nil || len(values) > maxCount {
		return false
	}
	for _, value := range values {
		if !validText(value, maxRunes) {
			return false
		}
	}
	return true
}

func validText(value string, maxRunes int) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" && !strings.ContainsRune(value, 0) && utf8.RuneCountInString(value) <= maxRunes
}

func containsTool(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func subset[T comparable](child, parent []T) bool {
	allowed := make(map[T]bool, len(parent))
	for _, value := range parent {
		allowed[value] = true
	}
	for _, value := range child {
		if !allowed[value] {
			return false
		}
	}
	return true
}

func sameOptional(child, parent *string) bool {
	return child == nil && parent == nil || child != nil && parent != nil && *child == *parent
}
