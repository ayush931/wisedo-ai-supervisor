package oee

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	ErrUnseededTaxonomy      = errors.New("taxonomy registry is unseeded or empty for tenant")
	ErrTaxonomyCycleDetected = errors.New("cycle detected in reason hierarchy")
	ErrDuplicateReasonCode   = errors.New("duplicate reason code in taxonomy revision")
	ErrCrossTenantParent     = errors.New("parent node belongs to a different tenant")
	ErrInvalidParentCode     = errors.New("parent reason code does not exist in taxonomy")
	ErrInvalidEffectiveRange = errors.New("effective start must be strictly before effective end")
	ErrTaxonomyNotEffective  = errors.New("taxonomy revision is not effective or published")
	ErrUnauthorizedOverride  = errors.New("operator is unauthorized to perform reclassification")
	ErrAmbiguousOverride     = errors.New("reclassification override is ambiguous or incomplete")
	ErrRevisionConflict      = errors.New("base revision precondition mismatch")
	ErrContributionNotFound  = errors.New("target contribution not found in current revision")
	ErrInvalidLossFact       = errors.New("loss fact is invalid: negative or missing measurement, or empty event id")
	ErrDuplicateEvent        = errors.New("duplicate source event id across loss facts")
	ErrFactOutsideWindow     = errors.New("loss fact interval is not contained in the explanation window")
	ErrScopeMismatch         = errors.New("taxonomy tenant or site does not match the explanation scope")
	ErrMissingRecordedAt     = errors.New("recorded_at is required: the kernel reads no clock")
	ErrReconciliation        = errors.New("loss tree, Pareto and contributions do not reconcile")
	ErrOverlappingStoppage   = errors.New("stoppage facts overlap: one stopped interval cannot be lost twice")
	ErrTaxonomyDigest        = errors.New("taxonomy digest does not match its content")
)

// lossIDNamespace derives audit IDs deterministically: the kernel reads no
// randomness, so identical inputs always yield identical results.
var lossIDNamespace = uuid.MustParse("5b6c1a0e-7f43-4d5c-9a2e-0c6f2b1d8e11")

func lossID(parts ...string) string {
	b, err := CanonicalJSONBytes(parts)
	if err != nil {
		panic(err) // []string always marshals
	}
	return uuid.NewSHA1(lossIDNamespace, b).String()
}

// usableNode reports whether a reason may classify a fact at the given instant:
// published or effective, and inside its own effective range. Draft and retired
// reasons never classify; the fact stays visible as unclassified.
func usableNode(n ReasonNode, at time.Time) bool {
	if n.State != ReasonStatePublished && n.State != ReasonStateEffective {
		return false
	}
	at = at.UTC()
	if at.Before(n.EffectiveStart.UTC()) {
		return false
	}
	return n.EffectiveEnd == nil || at.Before(n.EffectiveEnd.UTC())
}

// ReasonLifecycleState defines the governance lifecycle of a taxonomy or reason code.
type ReasonLifecycleState string

const (
	ReasonStateDraft     ReasonLifecycleState = "DRAFT"
	ReasonStatePublished ReasonLifecycleState = "PUBLISHED"
	ReasonStateEffective ReasonLifecycleState = "EFFECTIVE"
	ReasonStateRetired   ReasonLifecycleState = "RETIRED"
)

// LossCategory represents the stable OEE loss categories.
type LossCategory string

const (
	LossCategoryAvailability LossCategory = "AVAILABILITY"
	LossCategoryPerformance  LossCategory = "PERFORMANCE"
	LossCategoryQuality      LossCategory = "QUALITY"
)

// LossClassification defines planned vs unplanned nature of an operational loss.
type LossClassification string

const (
	ClassificationPlanned      LossClassification = "PLANNED"
	ClassificationUnplanned    LossClassification = "UNPLANNED"
	ClassificationUnclassified LossClassification = "UNCLASSIFIED"
)

// ReasonNode represents a single node in the versioned reason hierarchy.
type ReasonNode struct {
	TenantID       string               `json:"tenant_id"`
	SiteID         string               `json:"site_id,omitempty"`
	ReasonCode     string               `json:"reason_code"`
	ParentCode     string               `json:"parent_code,omitempty"`
	Name           string               `json:"name"`
	Description    string               `json:"description,omitempty"`
	LossCategory   LossCategory         `json:"loss_category"`
	Classification LossClassification   `json:"classification"`
	State          ReasonLifecycleState `json:"state"`
	EffectiveStart time.Time            `json:"effective_start"`
	EffectiveEnd   *time.Time           `json:"effective_end,omitempty"`
	Revision       int64                `json:"revision"`
}

// TaxonomyRevision is an immutable, tenant-scoped, versioned collection of reason nodes.
type TaxonomyRevision struct {
	TaxonomyID      uuid.UUID            `json:"taxonomy_id"`
	TenantID        string               `json:"tenant_id"`
	SiteID          string               `json:"site_id,omitempty"`
	Revision        int64                `json:"revision"`
	State           ReasonLifecycleState `json:"state"`
	EffectiveWindow HalfOpenInterval     `json:"effective_window"`
	Nodes           []ReasonNode         `json:"nodes"`
	TaxonomyDigest  string               `json:"taxonomy_digest"`
}

// TaxonomyRegistry manages tenant-scoped registered taxonomy revisions.
type TaxonomyRegistry struct {
	mu         sync.RWMutex
	taxonomies map[string][]TaxonomyRevision // tenantID -> revisions sorted desc by Revision
}

// NewTaxonomyRegistry constructs an empty TaxonomyRegistry.
func NewTaxonomyRegistry() *TaxonomyRegistry {
	return &TaxonomyRegistry{
		taxonomies: make(map[string][]TaxonomyRevision),
	}
}

// Register validates and stores a taxonomy revision for its tenant.
func (r *TaxonomyRegistry) Register(tax TaxonomyRevision) error {
	if err := ValidateTaxonomy(tax); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	revs := r.taxonomies[tax.TenantID]
	for _, existing := range revs {
		if existing.Revision == tax.Revision {
			return errors.New("taxonomy revision already registered for tenant")
		}
	}

	revs = append(revs, tax)
	sort.Slice(revs, func(i, j int) bool {
		return revs[i].Revision > revs[j].Revision
	})
	r.taxonomies[tax.TenantID] = revs
	return nil
}

// GetEffective retrieves the latest effective taxonomy revision for the tenant at the given instant.
// If the tenant registry is unseeded or empty, it fails closed.
func (r *TaxonomyRegistry) GetEffective(tenantID string, at time.Time) (TaxonomyRevision, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	revs, ok := r.taxonomies[tenantID]
	if !ok || len(revs) == 0 {
		return TaxonomyRevision{}, ErrUnseededTaxonomy
	}

	utc := at.UTC()
	for _, rev := range revs {
		if (rev.State == ReasonStateEffective || rev.State == ReasonStatePublished) &&
			rev.EffectiveWindow.Contains(utc) {
			return rev, nil
		}
	}

	return TaxonomyRevision{}, ErrTaxonomyNotEffective
}

// ValidateTaxonomy validates reason hierarchy invariants:
// - Never bypasses an empty/unseeded registry (fails closed).
// - Cycles, duplicate codes, cross-tenant parents, and invalid effective ranges are rejected.
func ValidateTaxonomy(tax TaxonomyRevision) error {
	if len(tax.Nodes) == 0 {
		return ErrUnseededTaxonomy
	}
	if strings.TrimSpace(tax.TenantID) == "" {
		return errors.New("taxonomy tenant_id cannot be empty")
	}
	if !tax.EffectiveWindow.Start.Before(tax.EffectiveWindow.End) {
		return ErrInvalidEffectiveRange
	}

	nodeMap := make(map[string]ReasonNode, len(tax.Nodes))
	for _, node := range tax.Nodes {
		code := node.ReasonCode
		if strings.TrimSpace(code) == "" {
			return errors.New("reason code cannot be empty")
		}
		// Codes are identifiers: padding would make the duplicate, parent and cycle checks
		// disagree, so it is rejected rather than trimmed.
		if strings.TrimSpace(code) != code || strings.TrimSpace(node.ParentCode) != node.ParentCode {
			return fmt.Errorf("reason code %q or its parent has surrounding whitespace", code)
		}
		if _, exists := nodeMap[code]; exists {
			return ErrDuplicateReasonCode
		}
		if node.TenantID != tax.TenantID {
			return ErrCrossTenantParent
		}
		if node.EffectiveEnd != nil && !node.EffectiveStart.Before(*node.EffectiveEnd) {
			return ErrInvalidEffectiveRange
		}
		switch node.LossCategory {
		case LossCategoryAvailability, LossCategoryPerformance, LossCategoryQuality:
		default:
			return fmt.Errorf("invalid loss category %q", node.LossCategory)
		}
		switch node.Classification {
		case ClassificationPlanned, ClassificationUnplanned, ClassificationUnclassified:
		default:
			return fmt.Errorf("invalid loss classification %q", node.Classification)
		}
		nodeMap[code] = node
	}

	// Validate parent references
	for _, node := range tax.Nodes {
		parentCode := node.ParentCode
		if parentCode == "" {
			continue
		}
		parent, exists := nodeMap[parentCode]
		if !exists {
			return ErrInvalidParentCode
		}
		if parent.TenantID != node.TenantID {
			return ErrCrossTenantParent
		}
	}

	// Detect cycles
	for _, node := range tax.Nodes {
		visited := make(map[string]bool)
		curr := node.ReasonCode
		for curr != "" {
			if visited[curr] {
				return ErrTaxonomyCycleDetected
			}
			visited[curr] = true
			parent, ok := nodeMap[curr]
			if !ok {
				break
			}
			curr = parent.ParentCode
		}
	}

	return nil
}

// ComputeTaxonomyDigest produces a canonical SHA-256 digest of a taxonomy revision.
func ComputeTaxonomyDigest(tax TaxonomyRevision) (string, error) {
	type canonicalNode struct {
		Classification string `json:"classification"`
		Description    string `json:"description"`
		EffectiveEnd   string `json:"effective_end"`
		EffectiveStart string `json:"effective_start"`
		LossCategory   string `json:"loss_category"`
		Name           string `json:"name"`
		ParentCode     string `json:"parent_code"`
		ReasonCode     string `json:"reason_code"`
		Revision       int64  `json:"revision"`
		SiteID         string `json:"site_id"`
		State          string `json:"state"`
		TenantID       string `json:"tenant_id"`
	}

	nodes := make([]canonicalNode, len(tax.Nodes))
	for i, n := range tax.Nodes {
		endStr := ""
		if n.EffectiveEnd != nil {
			endStr = n.EffectiveEnd.UTC().Format(time.RFC3339Nano)
		}
		nodes[i] = canonicalNode{
			Classification: string(n.Classification),
			Description:    n.Description,
			EffectiveEnd:   endStr,
			EffectiveStart: n.EffectiveStart.UTC().Format(time.RFC3339Nano),
			LossCategory:   string(n.LossCategory),
			Name:           n.Name,
			ParentCode:     n.ParentCode,
			ReasonCode:     n.ReasonCode,
			Revision:       n.Revision,
			SiteID:         n.SiteID,
			State:          string(n.State),
			TenantID:       n.TenantID,
		}
	}
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].ReasonCode < nodes[j].ReasonCode
	})

	canonical := map[string]any{
		"effective_end":   tax.EffectiveWindow.End.UTC().Format(time.RFC3339Nano),
		"effective_start": tax.EffectiveWindow.Start.UTC().Format(time.RFC3339Nano),
		"nodes":           nodes,
		"revision":        tax.Revision,
		"site_id":         tax.SiteID,
		"state":           string(tax.State),
		"taxonomy_id":     tax.TaxonomyID.String(),
		"tenant_id":       tax.TenantID,
	}

	return CanonicalSHA256Hex(canonical)
}

// RawStoppageFact represents an observed stoppage fact from equipment state or operator report.
type RawStoppageFact struct {
	EventID         string           `json:"event_id"`
	ReasonCode      string           `json:"reason_code"`
	Interval        HalfOpenInterval `json:"interval"`
	DurationSeconds *big.Rat         `json:"duration_seconds,omitempty"`
}

// RawSpeedLossFact represents lost time or output due to reduced machine speed or microstops.
type RawSpeedLossFact struct {
	EventID         string            `json:"event_id"`
	ReasonCode      string            `json:"reason_code"`
	Interval        *HalfOpenInterval `json:"interval,omitempty"`
	DurationSeconds *big.Rat          `json:"duration_seconds"`
	LostQuantity    *big.Rat          `json:"lost_quantity,omitempty"`
	Unit            UnitCode          `json:"unit"`
}

// RawQualityLossFact represents lost output (scrap/rework) from inspection or reject counts.
type RawQualityLossFact struct {
	EventID         string   `json:"event_id"`
	ReasonCode      string   `json:"reason_code"`
	LostQuantity    *big.Rat `json:"lost_quantity"`
	DurationSeconds *big.Rat `json:"duration_seconds,omitempty"`
	Unit            UnitCode `json:"unit"`
}

// LossContribution represents a single granular loss contribution backed by source facts.
type LossContribution struct {
	ContributionID     string             `json:"contribution_id"`
	SourceEventIDs     []string           `json:"source_event_ids"`
	ReasonCode         string             `json:"reason_code"`
	ReasonName         string             `json:"reason_name"`
	LossCategory       LossCategory       `json:"loss_category"`
	Classification     LossClassification `json:"classification"`
	Interval           *HalfOpenInterval  `json:"interval,omitempty"`
	DurationSeconds    *big.Rat           `json:"duration_seconds"`
	LostQuantity       *big.Rat           `json:"lost_quantity"`
	Unit               UnitCode           `json:"unit"`
	TaxonomyRevisionID string             `json:"taxonomy_revision_id"`
	PolicyRevisionID   string             `json:"policy_revision_id"`
	Digest             string             `json:"digest"`
}

// ComputeContributionDigest calculates a canonical digest for a loss contribution:
// schema-versioned, exact rationals as strings, no clock, no generated fields.
func ComputeContributionDigest(c LossContribution) string {
	events := append([]string(nil), c.SourceEventIDs...)
	sort.Strings(events)
	var startStr, endStr string
	if c.Interval != nil {
		startStr = c.Interval.Start.UTC().Format(time.RFC3339Nano)
		endStr = c.Interval.End.UTC().Format(time.RFC3339Nano)
	}
	return mustDigest(map[string]any{
		"schema_version":       1,
		"contribution_id":      c.ContributionID,
		"source_event_ids":     events,
		"reason_code":          c.ReasonCode,
		"loss_category":        string(c.LossCategory),
		"classification":       string(c.Classification),
		"interval_start":       startStr,
		"interval_end":         endStr,
		"duration_seconds":     ratString(c.DurationSeconds),
		"lost_quantity":        ratString(c.LostQuantity),
		"unit":                 string(c.Unit),
		"taxonomy_revision_id": c.TaxonomyRevisionID,
		"policy_revision_id":   c.PolicyRevisionID,
	})
}

func mustDigest(body map[string]any) string {
	d, err := CanonicalSHA256Hex(body)
	if err != nil {
		panic(err) // maps of strings, ints and string slices always marshal
	}
	return d
}

func contributionDigests(cs []LossContribution) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Digest
	}
	sort.Strings(out)
	return out
}

// ratString is the digest form of an exact value. An unknown (nil) value is "null", never "0",
// so a missing measurement and a measured zero always digest differently (rule 06).
func ratString(r *big.Rat) string {
	if r == nil {
		return "null"
	}
	return r.RatString()
}

// LossTreeNode is an aggregate node in the loss hierarchy.
type LossTreeNode struct {
	NodeID               string             `json:"node_id"`
	ReasonCode           string             `json:"reason_code"`
	Name                 string             `json:"name"`
	LossCategory         LossCategory       `json:"loss_category"`
	Classification       LossClassification `json:"classification"`
	TotalDurationSeconds *big.Rat           `json:"total_duration_seconds"`
	TotalLostQuantity    *big.Rat           `json:"total_lost_quantity"`
	ContributionCount    int                `json:"contribution_count"`
	Children             []*LossTreeNode    `json:"children,omitempty"`
	Contributions        []LossContribution `json:"contributions,omitempty"`
	Digest               string             `json:"digest"`
}

// DrillDownContributingFacts returns all underlying source contributions from this node and its children.
func (n *LossTreeNode) DrillDownContributingFacts() []LossContribution {
	if n == nil {
		return nil
	}
	var res []LossContribution
	res = append(res, n.Contributions...)
	for _, child := range n.Children {
		res = append(res, child.DrillDownContributingFacts()...)
	}
	return res
}

// DrillDownSourceEventIDs returns all distinct source event IDs contributing to this node.
func (n *LossTreeNode) DrillDownSourceEventIDs() []string {
	facts := n.DrillDownContributingFacts()
	seen := make(map[string]bool)
	var ids []string
	for _, f := range facts {
		for _, id := range f.SourceEventIDs {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	sort.Strings(ids)
	return ids
}

// ParetoItem represents an aggregated item in the Pareto distribution.
type ParetoItem struct {
	Rank                 int                `json:"rank"`
	ReasonCode           string             `json:"reason_code"`
	Name                 string             `json:"name"`
	LossCategory         LossCategory       `json:"loss_category"`
	Classification       LossClassification `json:"classification"`
	DurationSeconds      *big.Rat           `json:"duration_seconds"`
	LostQuantity         *big.Rat           `json:"lost_quantity"`
	Percentage           string             `json:"percentage"`
	CumulativePercentage string             `json:"cumulative_percentage"`
	SourceEventIDs       []string           `json:"source_event_ids"`
	Contributions        []LossContribution `json:"contributions"`
	Digest               string             `json:"digest"`
}

// ParetoAnalysis captures the ordered Pareto ranking and total reconciliation.
type ParetoAnalysis struct {
	Metric                   string       `json:"metric"` // "DURATION" or "QUANTITY"
	TotalLossDurationSeconds *big.Rat     `json:"total_loss_duration_seconds"`
	TotalLostQuantity        *big.Rat     `json:"total_lost_quantity"`
	Items                    []ParetoItem `json:"items"`
	Digest                   string       `json:"digest"`
}

// AuditEntry records governed audit facts for authorization and operator corrections.
type AuditEntry struct {
	AuditID       string    `json:"audit_id"`
	TenantID      string    `json:"tenant_id"`
	SiteID        string    `json:"site_id,omitempty"`
	OccurredAt    time.Time `json:"occurred_at"`
	ActorRef      string    `json:"actor_ref"`
	Action        string    `json:"action"` // "RECLASSIFY_ATTEMPT", "RECLASSIFY_SUCCESS", "RECLASSIFY_DENIED"
	Status        string    `json:"status"` // "SUCCESS", "UNAUTHORIZED", "AMBIGUOUS", "CONFLICT"
	TargetID      string    `json:"target_id"`
	OriginalCode  string    `json:"original_code,omitempty"`
	RequestedCode string    `json:"requested_code,omitempty"`
	Justification string    `json:"justification,omitempty"`
	EvidenceRef   string    `json:"evidence_ref,omitempty"`
	ReasonCode    string    `json:"reason_code,omitempty"`
	Digest        string    `json:"digest"`
}

// RevisionHistoryEntry documents an immutable snapshot in the revision chain.
type RevisionHistoryEntry struct {
	Revision       int64     `json:"revision"`
	AppliedAt      time.Time `json:"applied_at"`
	ActorRef       string    `json:"actor_ref"`
	Justification  string    `json:"justification"`
	EvidenceRef    string    `json:"evidence_ref"`
	PreviousDigest string    `json:"previous_digest"`
	Digest         string    `json:"digest"`
}

// ClassificationResult contains the full evidence-linked loss explanation.
type ClassificationResult struct {
	Revision       int64                  `json:"revision"`
	TenantID       string                 `json:"tenant_id"`
	SiteID         string                 `json:"site_id,omitempty"`
	Window         HalfOpenInterval       `json:"window"`
	TaxonomyDigest string                 `json:"taxonomy_digest"`
	PolicyRevision string                 `json:"policy_revision_id"`
	LossTree       *LossTreeNode          `json:"loss_tree"`
	ParetoDuration ParetoAnalysis         `json:"pareto_duration"`
	ParetoQuantity ParetoAnalysis         `json:"pareto_quantity"`
	Contributions  []LossContribution     `json:"contributions"`
	History        []RevisionHistoryEntry `json:"history"`
	AuditLog       []AuditEntry           `json:"audit_log"`
	Digest         string                 `json:"digest"`
}

// LossExplanationInput provides raw observations and policy references for explanation.
type LossExplanationInput struct {
	TenantID          string
	SiteID            string
	Window            HalfOpenInterval
	StoppageEvents    []RawStoppageFact
	SpeedLossEvents   []RawSpeedLossFact
	QualityLossEvents []RawQualityLossFact
	Taxonomy          TaxonomyRevision
	PolicyRevisionID  string
	// RecordedAt is the caller-supplied recording time (ingestion time, kept
	// separate from event time). It is stored but never hashed.
	RecordedAt time.Time
}

// ReclassifyRequest specifies an operator correction targeting an existing classification result.
type ReclassifyRequest struct {
	TenantID             string
	SiteID               string
	BaseRevision         int64
	TargetContributionID string
	NewReasonCode        string
	ActorRef             string
	ActorRoles           []string
	Justification        string
	EvidenceRef          string
	OccurredAt           time.Time
}

// ExplainLosses produces an evidence-linked, tenant-scoped explanation of lost time and lost output.
// It fails closed on an empty or unseeded taxonomy, a taxonomy of another tenant or
// site, a draft or non-covering taxonomy, and any invalid fact. It never clamps.
func ExplainLosses(input LossExplanationInput) (*ClassificationResult, error) {
	if err := ValidateTaxonomy(input.Taxonomy); err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.TenantID) == "" || input.Taxonomy.TenantID != input.TenantID ||
		(input.Taxonomy.SiteID != "" && input.Taxonomy.SiteID != input.SiteID) {
		return nil, ErrScopeMismatch
	}
	if input.RecordedAt.IsZero() {
		return nil, ErrMissingRecordedAt
	}
	if !input.Window.Start.Before(input.Window.End) {
		return nil, ErrInvalidInterval
	}
	tax := input.Taxonomy
	if (tax.State != ReasonStateEffective && tax.State != ReasonStatePublished) ||
		input.Window.Start.Before(tax.EffectiveWindow.Start) || input.Window.End.After(tax.EffectiveWindow.End) {
		return nil, ErrTaxonomyNotEffective
	}
	// The taxonomy digest is computed from its content; a supplied digest must match it.
	taxDigest, err := ComputeTaxonomyDigest(tax)
	if err != nil {
		return nil, err
	}
	if tax.TaxonomyDigest != "" && tax.TaxonomyDigest != taxDigest {
		return nil, ErrTaxonomyDigest
	}
	// Facts are processed in event-id order and contribution ids derive from the event, so
	// the same facts in any order give the same contributions, tree and digest.
	stoppages := append([]RawStoppageFact(nil), input.StoppageEvents...)
	sort.Slice(stoppages, func(i, j int) bool { return stoppages[i].EventID < stoppages[j].EventID })
	speeds := append([]RawSpeedLossFact(nil), input.SpeedLossEvents...)
	sort.Slice(speeds, func(i, j int) bool { return speeds[i].EventID < speeds[j].EventID })
	qualities := append([]RawQualityLossFact(nil), input.QualityLossEvents...)
	sort.Slice(qualities, func(i, j int) bool { return qualities[i].EventID < qualities[j].EventID })
	contributionID := func(kind, eventID string) string {
		return kind + "-" + lossID(input.TenantID, input.SiteID, kind, eventID)
	}

	nodeMap := make(map[string]ReasonNode, len(tax.Nodes))
	for _, n := range tax.Nodes {
		nodeMap[n.ReasonCode] = n
	}
	taxRev := fmt.Sprintf("%d", tax.Revision)
	secUnit, _ := ParseUnitCode("s")
	pcsUnit, _ := ParseUnitCode("pcs")
	zero := func() *big.Rat { return big.NewRat(0, 1) }

	seen := map[string]bool{}
	claim := func(id string) error {
		if strings.TrimSpace(id) == "" {
			return ErrInvalidLossFact
		}
		if seen[id] {
			return ErrDuplicateEvent
		}
		seen[id] = true
		return nil
	}
	within := func(iv HalfOpenInterval) error {
		if !iv.Start.Before(iv.End) {
			return ErrInvalidInterval
		}
		if iv.Start.Before(input.Window.Start) || iv.End.After(input.Window.End) {
			return ErrFactOutsideWindow
		}
		return nil
	}
	nonNegative := func(r *big.Rat) error {
		if r != nil && r.Sign() < 0 {
			return ErrInvalidLossFact
		}
		return nil
	}
	classify := func(code string, at time.Time, cat LossCategory, fallback string) (string, string, LossCategory, LossClassification) {
		code = strings.TrimSpace(code)
		if n, ok := nodeMap[code]; ok && usableNode(n, at) {
			return code, n.Name, n.LossCategory, n.Classification
		}
		if code == "" {
			code = "UNCLASSIFIED"
		}
		return code, fallback, cat, ClassificationUnclassified
	}

	var contributions []LossContribution
	add := func(c LossContribution) {
		c.TaxonomyRevisionID = taxRev
		c.PolicyRevisionID = input.PolicyRevisionID
		c.Digest = ComputeContributionDigest(c)
		contributions = append(contributions, c)
	}

	// 1. Stoppage facts (availability). A stopped state is a loss to explain, never a cause.
	for _, stop := range stoppages {
		if err := claim(stop.EventID); err != nil {
			return nil, err
		}
		if err := within(stop.Interval); err != nil {
			return nil, err
		}
		if err := nonNegative(stop.DurationSeconds); err != nil {
			return nil, err
		}
		dur := stop.DurationSeconds
		if dur == nil {
			dur = stop.Interval.DurationSeconds()
		}
		// A stoppage cannot last longer than the interval it was observed in.
		if dur.Cmp(stop.Interval.DurationSeconds()) > 0 {
			return nil, ErrInvalidLossFact
		}
		iv := stop.Interval
		code, name, cat, class := classify(stop.ReasonCode, iv.Start, LossCategoryAvailability, "Unclassified Availability Loss")
		add(LossContribution{
			ContributionID: contributionID("avail", stop.EventID), SourceEventIDs: []string{stop.EventID},
			ReasonCode: code, ReasonName: name, LossCategory: cat, Classification: class,
			Interval: &iv, DurationSeconds: new(big.Rat).Set(dur), LostQuantity: zero(), Unit: secUnit,
		})
	}

	// Two stoppages cannot claim the same stopped time.
	byStart := append([]RawStoppageFact(nil), stoppages...)
	sort.Slice(byStart, func(i, j int) bool { return byStart[i].Interval.Start.Before(byStart[j].Interval.Start) })
	for i := 1; i < len(byStart); i++ {
		if byStart[i].Interval.Start.Before(byStart[i-1].Interval.End) {
			return nil, ErrOverlappingStoppage
		}
	}

	// 2. Speed-loss facts (performance). Duration is required: a missing value is unknown, not zero.
	for _, spd := range speeds {
		if err := claim(spd.EventID); err != nil {
			return nil, err
		}
		if spd.DurationSeconds == nil {
			return nil, ErrInvalidLossFact
		}
		if err := nonNegative(spd.DurationSeconds); err != nil {
			return nil, err
		}
		if err := nonNegative(spd.LostQuantity); err != nil {
			return nil, err
		}
		at := input.Window.Start
		if spd.Interval != nil {
			if err := within(*spd.Interval); err != nil {
				return nil, err
			}
			at = spd.Interval.Start
		}
		// A speed loss may report lost output; when it does, the quantity is normalized to
		// pieces with an explicit unit (ADR 0005 Sec. 11: units are never guessed). A loss
		// without a quantity is a time-only contribution.
		qty, unit := zero(), secUnit
		if spd.LostQuantity != nil {
			pieces, err := ConvertCountToPieces(spd.LostQuantity, spd.Unit)
			if err != nil {
				return nil, fmt.Errorf("speed loss %q: %w", spd.EventID, err)
			}
			qty, unit = pieces, pcsUnit
		}
		code, name, cat, class := classify(spd.ReasonCode, at, LossCategoryPerformance, "Unclassified Performance Loss")
		add(LossContribution{
			ContributionID: contributionID("perf", spd.EventID), SourceEventIDs: []string{spd.EventID},
			ReasonCode: code, ReasonName: name, LossCategory: cat, Classification: class,
			Interval: spd.Interval, DurationSeconds: new(big.Rat).Set(spd.DurationSeconds), LostQuantity: qty, Unit: unit,
		})
	}

	// 3. Quality-loss facts. The lost quantity is required; an absent duration means no
	// time component and is recorded as zero seconds, not as a measured zero loss of time.
	for _, q := range qualities {
		if err := claim(q.EventID); err != nil {
			return nil, err
		}
		if q.LostQuantity == nil {
			return nil, ErrInvalidLossFact
		}
		if err := nonNegative(q.LostQuantity); err != nil {
			return nil, err
		}
		if err := nonNegative(q.DurationSeconds); err != nil {
			return nil, err
		}
		dur := zero()
		if q.DurationSeconds != nil {
			dur.Set(q.DurationSeconds)
		}
		// Quantities from different units are summed in the tree and the Pareto, so each is
		// normalized to pieces first; an empty or unknown unit fails rather than being assumed.
		pieces, err := ConvertCountToPieces(q.LostQuantity, q.Unit)
		if err != nil {
			return nil, fmt.Errorf("quality loss %q: %w", q.EventID, err)
		}
		code, name, cat, class := classify(q.ReasonCode, input.Window.Start, LossCategoryQuality, "Unclassified Quality Loss")
		add(LossContribution{
			ContributionID: contributionID("qual", q.EventID), SourceEventIDs: []string{q.EventID},
			ReasonCode: code, ReasonName: name, LossCategory: cat, Classification: class,
			DurationSeconds: dur, LostQuantity: pieces, Unit: pcsUnit,
		})
	}

	// Build Loss Tree
	lossTree := buildLossTree(input.Taxonomy, contributions)

	// Build Pareto Analyses
	paretoDuration := buildParetoAnalysis("DURATION", contributions, lossTree.TotalDurationSeconds, lossTree.TotalLostQuantity)
	paretoQuantity := buildParetoAnalysis("QUANTITY", contributions, lossTree.TotalDurationSeconds, lossTree.TotalLostQuantity)

	res := &ClassificationResult{
		Revision:       1,
		TenantID:       input.TenantID,
		SiteID:         input.SiteID,
		Window:         input.Window,
		TaxonomyDigest: taxDigest,
		PolicyRevision: input.PolicyRevisionID,
		LossTree:       lossTree,
		ParetoDuration: paretoDuration,
		ParetoQuantity: paretoQuantity,
		Contributions:  contributions,
		History: []RevisionHistoryEntry{
			{
				Revision:       1,
				AppliedAt:      input.RecordedAt.UTC(),
				ActorRef:       "system",
				Justification:  "Initial deterministic loss explanation",
				EvidenceRef:    fmt.Sprintf("tax:%d:policy:%s", input.Taxonomy.Revision, input.PolicyRevisionID),
				PreviousDigest: "",
				Digest:         "",
			},
		},
		AuditLog: []AuditEntry{
			{
				AuditID:    lossID(input.TenantID, input.SiteID, "INITIAL_EXPLANATION", fmt.Sprintf("%d", tax.Revision), input.PolicyRevisionID),
				TenantID:   input.TenantID,
				SiteID:     input.SiteID,
				OccurredAt: input.RecordedAt.UTC(),
				ActorRef:   "system",
				Action:     "INITIAL_EXPLANATION",
				Status:     "SUCCESS",
				TargetID:   "all",
				ReasonCode: "OK",
				Digest:     "",
			},
		},
	}

	if err := verifyReconciliation(res); err != nil {
		return nil, err
	}
	digest := computeClassificationResultDigest(res)
	res.Digest = digest
	res.History[0].Digest = digest

	return res, nil
}

// buildLossTree constructs a structured hierarchical tree from taxonomy and contributions.
func buildLossTree(taxonomy TaxonomyRevision, contributions []LossContribution) *LossTreeNode {
	totalDur := big.NewRat(0, 1)
	totalQty := big.NewRat(0, 1)

	for _, c := range contributions {
		if c.DurationSeconds != nil {
			totalDur.Add(totalDur, c.DurationSeconds)
		}
		if c.LostQuantity != nil {
			totalQty.Add(totalQty, c.LostQuantity)
		}
	}

	// Group contributions by Category -> Classification -> ReasonCode
	availChildren := buildCategorySubtree(LossCategoryAvailability, taxonomy, contributions)
	perfChildren := buildCategorySubtree(LossCategoryPerformance, taxonomy, contributions)
	qualChildren := buildCategorySubtree(LossCategoryQuality, taxonomy, contributions)

	root := &LossTreeNode{
		NodeID:               "ROOT_LOSS_TREE",
		ReasonCode:           "TOTAL_LOSS",
		Name:                 "Total Operational Loss",
		LossCategory:         "ALL",
		Classification:       "ALL",
		TotalDurationSeconds: totalDur,
		TotalLostQuantity:    totalQty,
		ContributionCount:    len(contributions),
		Children:             []*LossTreeNode{availChildren, perfChildren, qualChildren},
	}
	root.Digest = computeNodeDigest(root)
	return root
}

func buildCategorySubtree(cat LossCategory, taxonomy TaxonomyRevision, contributions []LossContribution) *LossTreeNode {
	catDur := big.NewRat(0, 1)
	catQty := big.NewRat(0, 1)
	var catContribs []LossContribution

	for _, c := range contributions {
		if c.LossCategory == cat {
			catContribs = append(catContribs, c)
			if c.DurationSeconds != nil {
				catDur.Add(catDur, c.DurationSeconds)
			}
			if c.LostQuantity != nil {
				catQty.Add(catQty, c.LostQuantity)
			}
		}
	}

	plannedNode := buildClassificationSubtree(cat, ClassificationPlanned, taxonomy, catContribs)
	unplannedNode := buildClassificationSubtree(cat, ClassificationUnplanned, taxonomy, catContribs)
	unclassifiedNode := buildClassificationSubtree(cat, ClassificationUnclassified, taxonomy, catContribs)

	catNode := &LossTreeNode{
		NodeID:               fmt.Sprintf("CAT_%s", cat),
		ReasonCode:           string(cat),
		Name:                 fmt.Sprintf("%s Loss", cat),
		LossCategory:         cat,
		Classification:       "ALL",
		TotalDurationSeconds: catDur,
		TotalLostQuantity:    catQty,
		ContributionCount:    len(catContribs),
		Children:             []*LossTreeNode{plannedNode, unplannedNode, unclassifiedNode},
	}
	catNode.Digest = computeNodeDigest(catNode)
	return catNode
}

func buildClassificationSubtree(cat LossCategory, class LossClassification, taxonomy TaxonomyRevision, catContribs []LossContribution) *LossTreeNode {
	subDur := big.NewRat(0, 1)
	subQty := big.NewRat(0, 1)
	var subContribs []LossContribution

	for _, c := range catContribs {
		if c.Classification == class {
			subContribs = append(subContribs, c)
			if c.DurationSeconds != nil {
				subDur.Add(subDur, c.DurationSeconds)
			}
			if c.LostQuantity != nil {
				subQty.Add(subQty, c.LostQuantity)
			}
		}
	}

	// Map contributions by ReasonCode
	contribsByCode := make(map[string][]LossContribution)
	for _, c := range subContribs {
		contribsByCode[c.ReasonCode] = append(contribsByCode[c.ReasonCode], c)
	}

	var reasonNodes []*LossTreeNode

	if class == ClassificationUnclassified {
		// Group all unclassified reasons
		var unclassCodes []string
		for code := range contribsByCode {
			unclassCodes = append(unclassCodes, code)
		}
		sort.Strings(unclassCodes)

		for _, code := range unclassCodes {
			cList := contribsByCode[code]
			rDur := big.NewRat(0, 1)
			rQty := big.NewRat(0, 1)
			for _, c := range cList {
				rDur.Add(rDur, c.DurationSeconds)
				rQty.Add(rQty, c.LostQuantity)
			}
			leaf := &LossTreeNode{
				NodeID:               fmt.Sprintf("UNCLASS_%s_%s", cat, code),
				ReasonCode:           code,
				Name:                 fmt.Sprintf("Unclassified (%s)", code),
				LossCategory:         cat,
				Classification:       ClassificationUnclassified,
				TotalDurationSeconds: rDur,
				TotalLostQuantity:    rQty,
				ContributionCount:    len(cList),
				Contributions:        cList,
			}
			leaf.Digest = computeNodeDigest(leaf)
			reasonNodes = append(reasonNodes, leaf)
		}
	} else {
		// Build hierarchy according to taxonomy parent-child links
		nodesInScope := make(map[string]ReasonNode)
		childrenMap := make(map[string][]string) // in-scope parentCode -> childCodes
		var rootCodes []string
		for _, n := range taxonomy.Nodes {
			if n.LossCategory == cat && n.Classification == class {
				nodesInScope[n.ReasonCode] = n
			}
		}
		for _, n := range taxonomy.Nodes {
			if _, ok := nodesInScope[n.ReasonCode]; !ok {
				continue
			}
			// A reason whose parent sits in another category or classification
			// is a root here; dropping it would orphan its contributions.
			p := strings.TrimSpace(n.ParentCode)
			if _, parentInScope := nodesInScope[p]; p != "" && parentInScope {
				childrenMap[p] = append(childrenMap[p], n.ReasonCode)
			} else {
				rootCodes = append(rootCodes, n.ReasonCode)
			}
		}
		sort.Strings(rootCodes)
		for p := range childrenMap {
			sort.Strings(childrenMap[p])
		}

		var buildNode func(code string) *LossTreeNode
		buildNode = func(code string) *LossTreeNode {
			meta := nodesInScope[code]
			cList := contribsByCode[code]
			rDur := big.NewRat(0, 1)
			rQty := big.NewRat(0, 1)
			for _, c := range cList {
				rDur.Add(rDur, c.DurationSeconds)
				rQty.Add(rQty, c.LostQuantity)
			}

			var children []*LossTreeNode
			childCodes := childrenMap[code]
			for _, cc := range childCodes {
				childTreeNode := buildNode(cc)
				children = append(children, childTreeNode)
				rDur.Add(rDur, childTreeNode.TotalDurationSeconds)
				rQty.Add(rQty, childTreeNode.TotalLostQuantity)
			}

			totalCount := len(cList)
			for _, ch := range children {
				totalCount += ch.ContributionCount
			}

			n := &LossTreeNode{
				NodeID:               fmt.Sprintf("REASON_%s", code),
				ReasonCode:           code,
				Name:                 meta.Name,
				LossCategory:         cat,
				Classification:       class,
				TotalDurationSeconds: rDur,
				TotalLostQuantity:    rQty,
				ContributionCount:    totalCount,
				Children:             children,
				Contributions:        cList,
			}
			n.Digest = computeNodeDigest(n)
			return n
		}

		for _, rc := range rootCodes {
			reasonNodes = append(reasonNodes, buildNode(rc))
		}
	}

	classNode := &LossTreeNode{
		NodeID:               fmt.Sprintf("CLASS_%s_%s", cat, class),
		ReasonCode:           string(class),
		Name:                 fmt.Sprintf("%s %s Loss", class, cat),
		LossCategory:         cat,
		Classification:       class,
		TotalDurationSeconds: subDur,
		TotalLostQuantity:    subQty,
		ContributionCount:    len(subContribs),
		Children:             reasonNodes,
	}
	classNode.Digest = computeNodeDigest(classNode)
	return classNode
}

func computeNodeDigest(n *LossTreeNode) string {
	children := make([]string, len(n.Children))
	for i, c := range n.Children {
		children[i] = c.Digest
	}
	return mustDigest(map[string]any{
		"schema_version":     1,
		"node_id":            n.NodeID,
		"reason_code":        n.ReasonCode,
		"loss_category":      string(n.LossCategory),
		"classification":     string(n.Classification),
		"duration_seconds":   ratString(n.TotalDurationSeconds),
		"lost_quantity":      ratString(n.TotalLostQuantity),
		"contribution_count": n.ContributionCount,
		"children":           children,
		"contributions":      contributionDigests(n.Contributions),
	})
}

// buildParetoAnalysis builds Pareto distribution with deterministic total ordering and exact reconciliation.
func buildParetoAnalysis(metric string, contributions []LossContribution, totalDur, totalQty *big.Rat) ParetoAnalysis {
	// Aggregate by ReasonCode
	type aggItem struct {
		ReasonCode     string
		Name           string
		Category       LossCategory
		Classification LossClassification
		Duration       *big.Rat
		Quantity       *big.Rat
		Contribs       []LossContribution
	}

	// Group by category, code and classification: UNCLASSIFIED in availability, performance
	// and quality are different losses, and a contribution that kept its reason code but fell
	// back to UNCLASSIFIED (an expired or unusable reason) must stay visible as unclassified
	// rather than merge into the classified row of the same code (rule 06).
	groupMap := make(map[string]*aggItem)
	for _, c := range contributions {
		key := string(c.LossCategory) + "|" + c.ReasonCode + "|" + string(c.Classification)
		item, ok := groupMap[key]
		if !ok {
			item = &aggItem{
				ReasonCode:     c.ReasonCode,
				Name:           c.ReasonName,
				Category:       c.LossCategory,
				Classification: c.Classification,
				Duration:       big.NewRat(0, 1),
				Quantity:       big.NewRat(0, 1),
			}
			groupMap[key] = item
		}
		item.Contribs = append(item.Contribs, c)
		if c.DurationSeconds != nil {
			item.Duration.Add(item.Duration, c.DurationSeconds)
		}
		if c.LostQuantity != nil {
			item.Quantity.Add(item.Quantity, c.LostQuantity)
		}
	}

	var list []*aggItem
	for _, it := range groupMap {
		list = append(list, it)
	}

	// Deterministic total ordering:
	// Primary: Value descending
	// Tie-breaker: ReasonCode ascending
	sort.Slice(list, func(i, j int) bool {
		var cmp int
		if metric == "DURATION" {
			cmp = list[i].Duration.Cmp(list[j].Duration)
		} else {
			cmp = list[i].Quantity.Cmp(list[j].Quantity)
		}
		if cmp != 0 {
			return cmp > 0 // Descending
		}
		if list[i].ReasonCode != list[j].ReasonCode {
			return list[i].ReasonCode < list[j].ReasonCode // Ascending tie-breaker
		}
		if list[i].Category != list[j].Category {
			return list[i].Category < list[j].Category
		}
		return list[i].Classification < list[j].Classification
	})

	var items []ParetoItem
	cumDur := big.NewRat(0, 1)
	cumQty := big.NewRat(0, 1)

	reconciledDur := big.NewRat(0, 1)
	reconciledQty := big.NewRat(0, 1)

	for idx, it := range list {
		reconciledDur.Add(reconciledDur, it.Duration)
		reconciledQty.Add(reconciledQty, it.Quantity)

		cumDur.Add(cumDur, it.Duration)
		cumQty.Add(cumQty, it.Quantity)

		// A share of a zero total is 0/0: it has no published value (rule 04, ADR 0005
		// constraint 3), so the percentage is left empty rather than shown as 0%.
		value, cum, total := it.Quantity, cumQty, totalQty
		if metric == "DURATION" {
			value, cum, total = it.Duration, cumDur, totalDur
		}
		pct, cumPct := "", ""
		if total != nil && total.Sign() > 0 {
			hundred := big.NewRat(100, 1)
			p, _ := FormatDecimal(new(big.Rat).Mul(new(big.Rat).Quo(value, total), hundred), 6, RoundHalfEven)
			c, _ := FormatDecimal(new(big.Rat).Mul(new(big.Rat).Quo(cum, total), hundred), 6, RoundHalfEven)
			pct, cumPct = p+"%", c+"%"
		}

		var eventIDs []string
		seen := make(map[string]bool)
		for _, c := range it.Contribs {
			for _, id := range c.SourceEventIDs {
				if !seen[id] {
					seen[id] = true
					eventIDs = append(eventIDs, id)
				}
			}
		}
		sort.Strings(eventIDs)

		pItem := ParetoItem{
			Rank:                 idx + 1,
			ReasonCode:           it.ReasonCode,
			Name:                 it.Name,
			LossCategory:         it.Category,
			Classification:       it.Classification,
			DurationSeconds:      it.Duration,
			LostQuantity:         it.Quantity,
			Percentage:           pct,
			CumulativePercentage: cumPct,
			SourceEventIDs:       eventIDs,
			Contributions:        it.Contribs,
		}
		pItem.Digest = computeParetoItemDigest(pItem)
		items = append(items, pItem)
	}

	analysis := ParetoAnalysis{
		Metric:                   metric,
		TotalLossDurationSeconds: reconciledDur,
		TotalLostQuantity:        reconciledQty,
		Items:                    items,
	}
	analysis.Digest = computeParetoAnalysisDigest(analysis)
	return analysis
}

func computeParetoItemDigest(p ParetoItem) string {
	return mustDigest(map[string]any{
		"schema_version":        1,
		"rank":                  p.Rank,
		"reason_code":           p.ReasonCode,
		"loss_category":         string(p.LossCategory),
		"classification":        string(p.Classification),
		"duration_seconds":      ratString(p.DurationSeconds),
		"lost_quantity":         ratString(p.LostQuantity),
		"percentage":            p.Percentage,
		"cumulative_percentage": p.CumulativePercentage,
		"contributions":         contributionDigests(p.Contributions),
	})
}

func computeParetoAnalysisDigest(a ParetoAnalysis) string {
	items := make([]string, len(a.Items))
	for i, it := range a.Items {
		items[i] = it.Digest
	}
	return mustDigest(map[string]any{
		"schema_version":             1,
		"metric":                     a.Metric,
		"total_loss_duration_second": ratString(a.TotalLossDurationSeconds),
		"total_lost_quantity":        ratString(a.TotalLostQuantity),
		"items":                      items,
	})
}

// computeClassificationResultDigest binds scope, window, taxonomy and policy
// versions, every contribution, the tree, both Pareto analyses and the predecessor
// digest. Generated IDs, recorded times and the digest itself are excluded, so
// identical inputs digest identically and any changed fact or revision changes it.
func computeClassificationResultDigest(r *ClassificationResult) string {
	previous := ""
	if n := len(r.History); n > 0 {
		previous = r.History[n-1].PreviousDigest
	}
	return mustDigest(map[string]any{
		"schema_version":     1,
		"revision":           r.Revision,
		"tenant_id":          r.TenantID,
		"site_id":            r.SiteID,
		"window_start":       r.Window.Start.UTC().Format(time.RFC3339Nano),
		"window_end":         r.Window.End.UTC().Format(time.RFC3339Nano),
		"taxonomy_digest":    r.TaxonomyDigest,
		"policy_revision_id": r.PolicyRevision,
		"loss_tree":          r.LossTree.Digest,
		"pareto_duration":    r.ParetoDuration.Digest,
		"pareto_quantity":    r.ParetoQuantity.Digest,
		"contributions":      contributionDigests(r.Contributions),
		"previous_digest":    previous,
	})
}

// verifyReconciliation proves the tree, both Pareto analyses and the contribution
// list agree exactly, and that every contribution appears once in the drill-down.
func verifyReconciliation(r *ClassificationResult) error {
	sumDur, sumQty := big.NewRat(0, 1), big.NewRat(0, 1)
	for _, c := range r.Contributions {
		sumDur.Add(sumDur, c.DurationSeconds)
		sumQty.Add(sumQty, c.LostQuantity)
	}
	root := r.LossTree
	if root == nil || root.TotalDurationSeconds.Cmp(sumDur) != 0 || root.TotalLostQuantity.Cmp(sumQty) != 0 || root.ContributionCount != len(r.Contributions) {
		return ErrReconciliation
	}
	for _, p := range []ParetoAnalysis{r.ParetoDuration, r.ParetoQuantity} {
		if p.TotalLossDurationSeconds.Cmp(sumDur) != 0 || p.TotalLostQuantity.Cmp(sumQty) != 0 {
			return ErrReconciliation
		}
	}
	var check func(n *LossTreeNode) error
	check = func(n *LossTreeNode) error {
		dur, qty, count := big.NewRat(0, 1), big.NewRat(0, 1), len(n.Contributions)
		for _, c := range n.Contributions {
			dur.Add(dur, c.DurationSeconds)
			qty.Add(qty, c.LostQuantity)
		}
		for _, child := range n.Children {
			if err := check(child); err != nil {
				return err
			}
			dur.Add(dur, child.TotalDurationSeconds)
			qty.Add(qty, child.TotalLostQuantity)
			count += child.ContributionCount
		}
		if n.TotalDurationSeconds.Cmp(dur) != 0 || n.TotalLostQuantity.Cmp(qty) != 0 || n.ContributionCount != count {
			return ErrReconciliation
		}
		return nil
	}
	if err := check(root); err != nil {
		return err
	}
	seen := map[string]int{}
	for _, c := range root.DrillDownContributingFacts() {
		seen[c.ContributionID]++
	}
	if len(seen) != len(r.Contributions) {
		return ErrReconciliation
	}
	for _, c := range r.Contributions {
		if seen[c.ContributionID] != 1 {
			return ErrReconciliation
		}
	}
	return nil
}

// ReclassifyLoss processes an operator reclassification override.
// Appends a new governed classification revision with actor, reason, and evidence.
// History is never overwritten. Unauthorized or ambiguous overrides fail closed and produce safe audit records.
// ReclassifyDenied is returned when an override is refused. It carries the audit entry for
// the refusal; the published result is never modified, so a denied (possibly cross-tenant)
// request cannot write into another record. errors.Is still matches the cause.
type ReclassifyDenied struct {
	Err   error
	Audit AuditEntry
}

func (d *ReclassifyDenied) Error() string { return d.Err.Error() }
func (d *ReclassifyDenied) Unwrap() error { return d.Err }

func ReclassifyLoss(current *ClassificationResult, req ReclassifyRequest, taxonomy TaxonomyRevision) (*ClassificationResult, error) {
	if current == nil {
		return nil, errors.New("current classification result cannot be nil")
	}

	var denial AuditEntry
	deny := func(err error) (*ClassificationResult, error) {
		return nil, &ReclassifyDenied{Err: err, Audit: denial}
	}
	recordAudit := func(action, status, reasonCode string) AuditEntry {
		entry := AuditEntry{
			AuditID:       lossID(current.TenantID, current.SiteID, action, status, reasonCode, fmt.Sprintf("%d", current.Revision), fmt.Sprintf("%d", len(current.AuditLog)), req.ActorRef, req.TargetContributionID, req.OccurredAt.UTC().Format(time.RFC3339Nano)),
			TenantID:      current.TenantID,
			SiteID:        current.SiteID,
			OccurredAt:    req.OccurredAt.UTC(),
			ActorRef:      req.ActorRef,
			Action:        action,
			Status:        status,
			TargetID:      req.TargetContributionID,
			RequestedCode: req.NewReasonCode,
			Justification: req.Justification,
			EvidenceRef:   req.EvidenceRef,
			ReasonCode:    reasonCode,
		}
		raw := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s",
			entry.AuditID, entry.OccurredAt.Format(time.RFC3339Nano), entry.ActorRef, entry.Action, entry.Status, entry.TargetID, entry.ReasonCode)
		h := sha256.Sum256([]byte(raw))
		entry.Digest = hex.EncodeToString(h[:])
		denial = entry
		return entry
	}

	// 0. Scope: a request for another tenant or site, or a foreign taxonomy, is
	// indistinguishable from a missing target (non-disclosing) and is audited on
	// the result it tried to touch.
	if req.TenantID != current.TenantID || req.SiteID != current.SiteID ||
		taxonomy.TenantID != current.TenantID || (taxonomy.SiteID != "" && taxonomy.SiteID != current.SiteID) {
		recordAudit("RECLASSIFY_DENIED", "NOT_FOUND", "SCOPE_MISMATCH")
		return deny(ErrContributionNotFound)
	}

	// 1. Authorization checks
	actor := strings.TrimSpace(req.ActorRef)
	if actor == "" {
		recordAudit("RECLASSIFY_DENIED", "UNAUTHORIZED", "MISSING_ACTOR")
		return deny(ErrUnauthorizedOverride)
	}

	isAuthorized := false
	for _, r := range req.ActorRoles {
		switch strings.ToUpper(strings.TrimSpace(r)) {
		case "OPERATOR", "SUPERVISOR", "OWNER":
			isAuthorized = true
		}
	}
	if !isAuthorized {
		recordAudit("RECLASSIFY_DENIED", "UNAUTHORIZED", "UNAUTHORIZED_ROLE")
		return deny(ErrUnauthorizedOverride)
	}

	// 2. Ambiguity checks
	justification := strings.TrimSpace(req.Justification)
	evidenceRef := strings.TrimSpace(req.EvidenceRef)
	newCode := strings.TrimSpace(req.NewReasonCode)

	if justification == "" {
		recordAudit("RECLASSIFY_DENIED", "AMBIGUOUS", "MISSING_JUSTIFICATION")
		return deny(ErrAmbiguousOverride)
	}
	if evidenceRef == "" {
		recordAudit("RECLASSIFY_DENIED", "AMBIGUOUS", "MISSING_EVIDENCE")
		return deny(ErrAmbiguousOverride)
	}
	if newCode == "" {
		recordAudit("RECLASSIFY_DENIED", "AMBIGUOUS", "MISSING_REASON_CODE")
		return deny(ErrAmbiguousOverride)
	}
	if req.OccurredAt.IsZero() {
		recordAudit("RECLASSIFY_DENIED", "AMBIGUOUS", "MISSING_OCCURRED_AT")
		return deny(ErrAmbiguousOverride)
	}
	if err := ValidateTaxonomy(taxonomy); err != nil {
		recordAudit("RECLASSIFY_DENIED", "AMBIGUOUS", "INVALID_TAXONOMY")
		return deny(err)
	}

	// Verify target reason code exists and is effective in taxonomy
	nodeMap := make(map[string]ReasonNode, len(taxonomy.Nodes))
	for _, n := range taxonomy.Nodes {
		nodeMap[n.ReasonCode] = n
	}
	targetNode, exists := nodeMap[newCode]
	if !exists {
		recordAudit("RECLASSIFY_DENIED", "AMBIGUOUS", "UNKNOWN_REASON_CODE")
		return deny(ErrAmbiguousOverride)
	}

	// 3. Optimistic Concurrency check
	if req.BaseRevision != current.Revision {
		recordAudit("RECLASSIFY_DENIED", "CONFLICT", "BASE_REVISION_MISMATCH")
		return deny(ErrRevisionConflict)
	}

	// 4. Target lookup. A target is a contribution id or a source event id, and it must name
	// exactly one contribution; anything else fails closed instead of picking the first.
	matches := map[int]bool{}
	for i, c := range current.Contributions {
		if c.ContributionID == req.TargetContributionID {
			matches[i] = true
		}
		for _, eid := range c.SourceEventIDs {
			if eid == req.TargetContributionID {
				matches[i] = true
			}
		}
	}
	if len(matches) > 1 {
		recordAudit("RECLASSIFY_DENIED", "AMBIGUOUS", "AMBIGUOUS_TARGET")
		return deny(ErrAmbiguousOverride)
	}
	targetIdx := -1
	for i := range matches {
		targetIdx = i
	}
	if targetIdx == -1 {
		recordAudit("RECLASSIFY_DENIED", "NOT_FOUND", "CONTRIBUTION_NOT_FOUND")
		return deny(ErrContributionNotFound)
	}
	originalCode := current.Contributions[targetIdx].ReasonCode

	// The override must use the exact taxonomy the contributions were explained
	// under, and its reason must be usable when the loss occurred.
	orig0 := current.Contributions[targetIdx]
	taxDigest, digestErr := ComputeTaxonomyDigest(taxonomy)
	if orig0.TaxonomyRevisionID != fmt.Sprintf("%d", taxonomy.Revision) || digestErr != nil || current.TaxonomyDigest != taxDigest ||
		(taxonomy.TaxonomyDigest != "" && taxonomy.TaxonomyDigest != taxDigest) {
		recordAudit("RECLASSIFY_DENIED", "AMBIGUOUS", "TAXONOMY_REVISION_MISMATCH")
		return deny(ErrAmbiguousOverride)
	}
	at := current.Window.Start
	if orig0.Interval != nil {
		at = orig0.Interval.Start
	}
	if !usableNode(targetNode, at) {
		recordAudit("RECLASSIFY_DENIED", "AMBIGUOUS", "REASON_CODE_NOT_EFFECTIVE")
		return deny(ErrAmbiguousOverride)
	}

	// 5. Append New Governed Revision (Preserve history and prior evidence!)
	newContributions := make([]LossContribution, len(current.Contributions))
	copy(newContributions, current.Contributions)

	orig := newContributions[targetIdx]
	updated := LossContribution{
		ContributionID:     orig.ContributionID,
		SourceEventIDs:     orig.SourceEventIDs,
		ReasonCode:         newCode,
		ReasonName:         targetNode.Name,
		LossCategory:       targetNode.LossCategory,
		Classification:     targetNode.Classification,
		Interval:           orig.Interval,
		DurationSeconds:    new(big.Rat).Set(orig.DurationSeconds),
		LostQuantity:       new(big.Rat).Set(orig.LostQuantity),
		Unit:               orig.Unit,
		TaxonomyRevisionID: orig.TaxonomyRevisionID,
		PolicyRevisionID:   orig.PolicyRevisionID,
	}
	updated.Digest = ComputeContributionDigest(updated)
	newContributions[targetIdx] = updated

	// Recompute tree and Pareto
	newTree := buildLossTree(taxonomy, newContributions)
	newParetoDur := buildParetoAnalysis("DURATION", newContributions, newTree.TotalDurationSeconds, newTree.TotalLostQuantity)
	newParetoQty := buildParetoAnalysis("QUANTITY", newContributions, newTree.TotalDurationSeconds, newTree.TotalLostQuantity)

	newRevNo := current.Revision + 1
	newHistory := make([]RevisionHistoryEntry, len(current.History))
	copy(newHistory, current.History)

	histEntry := RevisionHistoryEntry{
		Revision:       newRevNo,
		AppliedAt:      req.OccurredAt.UTC(),
		ActorRef:       req.ActorRef,
		Justification:  req.Justification,
		EvidenceRef:    req.EvidenceRef,
		PreviousDigest: current.Digest,
	}

	newAuditLog := make([]AuditEntry, len(current.AuditLog))
	copy(newAuditLog, current.AuditLog)

	auditEntry := AuditEntry{
		AuditID:       lossID(current.TenantID, current.SiteID, "RECLASSIFY_SUCCESS", fmt.Sprintf("%d", newRevNo), req.TargetContributionID, newCode, req.ActorRef, req.OccurredAt.UTC().Format(time.RFC3339Nano)),
		TenantID:      current.TenantID,
		SiteID:        current.SiteID,
		OccurredAt:    req.OccurredAt.UTC(),
		ActorRef:      req.ActorRef,
		Action:        "RECLASSIFY_SUCCESS",
		Status:        "SUCCESS",
		TargetID:      req.TargetContributionID,
		OriginalCode:  originalCode,
		RequestedCode: newCode,
		Justification: req.Justification,
		EvidenceRef:   req.EvidenceRef,
		ReasonCode:    "OVERRIDE_APPLIED",
	}
	rawAudit := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s",
		auditEntry.AuditID, auditEntry.OccurredAt.Format(time.RFC3339Nano), auditEntry.ActorRef, auditEntry.Action, auditEntry.Status, auditEntry.TargetID, auditEntry.ReasonCode)
	hAudit := sha256.Sum256([]byte(rawAudit))
	auditEntry.Digest = hex.EncodeToString(hAudit[:])
	newAuditLog = append(newAuditLog, auditEntry)

	newResult := &ClassificationResult{
		Revision:       newRevNo,
		TenantID:       current.TenantID,
		SiteID:         current.SiteID,
		Window:         current.Window,
		TaxonomyDigest: current.TaxonomyDigest,
		PolicyRevision: current.PolicyRevision,
		LossTree:       newTree,
		ParetoDuration: newParetoDur,
		ParetoQuantity: newParetoQty,
		Contributions:  newContributions,
		History:        append(newHistory, histEntry),
		AuditLog:       newAuditLog,
	}

	if err := verifyReconciliation(newResult); err != nil {
		return nil, err
	}
	digest := computeClassificationResultDigest(newResult)
	newResult.Digest = digest
	newResult.History[len(newResult.History)-1].Digest = digest

	return newResult, nil
}
