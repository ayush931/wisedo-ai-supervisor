package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type CoverageRequirement struct {
	ExpiresAt                 *time.Time `json:"expires_at,omitempty"`
	FreshnessPolicyRevision   int64      `json:"freshness_policy_revision,omitempty"`
	RequirementID             string     `json:"requirement_id"`
	Key                       string     `json:"key"`
	InterviewRole             string     `json:"interview_role"`
	BindingSupported          bool       `json:"binding_supported"`
	State                     string     `json:"state"`
	ReasonCode                string     `json:"reason_code"`
	DecisionID                string     `json:"decision_id,omitempty"`
	DecisionRevision          int64      `json:"decision_revision,omitempty"`
	SnapshotID                string     `json:"snapshot_id,omitempty"`
	SnapshotSHA256            string     `json:"snapshot_sha256,omitempty"`
	SnapshotInventoryRevision int64      `json:"snapshot_inventory_revision,omitempty"`
	AsOf                      string     `json:"as_of,omitempty"`
}

type Coverage struct {
	ExpiresAt                  *time.Time            `json:"expires_at,omitempty"`
	SchemaVersion              int                   `json:"schema_version"`
	EvaluatorVersion           int                   `json:"evaluator_version"`
	InventoryID                string                `json:"inventory_id"`
	InventoryRevision          int64                 `json:"inventory_revision"`
	TemplateVersion            int                   `json:"template_version"`
	KnowledgeInventoryRevision int64                 `json:"knowledge_inventory_revision"`
	EvaluatedAt                time.Time             `json:"evaluated_at"`
	ScopeCurrent               bool                  `json:"scope_current"`
	Requirements               []CoverageRequirement `json:"requirements"`
	BoundCount                 int                   `json:"bound_count"`
	ApprovedCount              int                   `json:"approved_count"`
	ApplicableCount            int                   `json:"applicable_count"`
	EvaluationState            string                `json:"evaluation_state"`
	KnowledgeReady             bool                  `json:"knowledge_ready"`
	AuthorizationState         string                `json:"authorization_state"`
}

// ReadCoverage projects the complete fixed denominator in one consistent site cut.
// Qualification can approve supported cells; capability readiness remains disabled.
func (s *Service) ReadCoverage(ctx context.Context, a Actor, sc Scope, id string) (Coverage, error) {
	if !contractUUID(id) {
		return Coverage{}, ErrInvalidContract
	}
	tx, err := s.beginLocked(ctx, a, sc, false, true)
	if err != nil {
		return Coverage{}, err
	}
	defer tx.Rollback()
	out, err := projectCoverage(ctx, tx, sc, id)
	if err != nil {
		return Coverage{}, err
	}
	if err = audit(ctx, tx, a, sc, id, out.InventoryRevision, "coverage.read"); err != nil {
		return Coverage{}, err
	}
	if err = s.finish(ctx, tx, a); err != nil {
		return Coverage{}, err
	}
	return out, nil
}

// projectCoverage evaluates the current revision of one inventory inside the caller's locked read.
func projectCoverage(ctx context.Context, tx *sql.Tx, sc Scope, id string) (Coverage, error) {
	inventory, err := loadDiscoveryInventory(ctx, tx, sc, id, 0)
	if err != nil {
		return Coverage{}, err
	}
	template, err := DiscoveryTemplate(inventory.Capability)
	if err != nil {
		return Coverage{}, err
	}
	if inventory.TemplateVersion != DiscoveryTemplateVersion || len(inventory.Requirements) != len(template) {
		return Coverage{}, ErrInvalidContract
	}
	expected := make(map[string]DiscoveryRequirementDefinition, len(template))
	for _, definition := range template {
		expected[definition.Key] = definition
	}
	out := Coverage{SchemaVersion: 1, EvaluatorVersion: 3, InventoryID: id, InventoryRevision: inventory.Revision, TemplateVersion: inventory.TemplateVersion, ScopeCurrent: inventory.ScopeCurrent, Requirements: []CoverageRequirement{}, ApplicableCount: len(template), EvaluationState: "partial_support", AuthorizationState: "disabled"}
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&out.EvaluatedAt); err != nil {
		return Coverage{}, err
	}
	for _, requirement := range inventory.Requirements {
		definition, ok := expected[requirement.Key]
		if !ok || definition.InterviewRole != requirement.InterviewRole {
			return Coverage{}, ErrInvalidContract
		}
		delete(expected, requirement.Key)
		cell := CoverageRequirement{RequirementID: requirement.RequirementID, Key: requirement.Key, InterviewRole: requirement.InterviewRole, BindingSupported: BindingSupported(requirement.Key), State: "missing", ReasonCode: "decision_missing"}
		head, e := requirementHead(ctx, tx, sc, id, requirement.RequirementID, inventory.Revision)
		if e != nil {
			return Coverage{}, e
		}
		if head > 0 {
			// Only absence of a head means missing. A revoked, retained-away or otherwise
			// unavailable decision must fail the entire projection, never shrink its scope.
			decision, e := loadRequirementDecision(ctx, tx, sc, inventory, requirement.RequirementID, head)
			if e != nil {
				return Coverage{}, e
			}
			cell.DecisionID = decision.DecisionID
			cell.DecisionRevision = decision.Revision
			cell.SnapshotID = decision.SnapshotID
			cell.SnapshotSHA256 = decision.SnapshotSHA256
			cell.SnapshotInventoryRevision = decision.SnapshotInventoryRevision
			cell.AsOf = decision.AsOf
			cell.FreshnessPolicyRevision = decision.FreshnessPolicyRevision
			if decision.Verdict == "bind" && decision.SnapshotID != "" && decision.bindingCurrent {
				out.BoundCount++
			}
			switch {
			case decision.Verdict == "withdraw":
				cell.State = "missing"
				cell.ReasonCode = "decision_withdrawn"
			case decision.witnessReason != "":
				cell.State = "stale"
				cell.ReasonCode = decision.witnessReason
			case !decision.bindingCurrent:
				cell.State = "stale"
				cell.ReasonCode = "decision_version_stale"
			case decision.Verdict == "not_applicable":
				cell.State = "not_applicable"
				cell.ReasonCode = "reviewer_not_applicable"
				out.ApplicableCount--
			case decision.Verdict == "bind":
				reason, expiry, _, e := qualifyRequirement(ctx, tx, sc, decision, &out.EvaluatedAt)
				if e != nil {
					return Coverage{}, e
				}
				cell.ExpiresAt = expiry
				if expiry != nil && (out.ExpiresAt == nil || expiry.Before(*out.ExpiresAt)) {
					out.ExpiresAt = expiry
				}
				if reason == "" {
					cell.State = "approved"
					cell.ReasonCode = "qualified_document_evidence"
					if decision.SnapshotID == "" {
						cell.ReasonCode = "qualified_freshness_policy"
					}
					out.ApprovedCount++
				} else if reason == "freshness_policy_missing" {
					cell.State = "candidate"
					cell.ReasonCode = reason
				} else {
					cell.State = "stale"
					cell.ReasonCode = reason
				}
			default:
				return Coverage{}, ErrInvalidContract
			}
		}
		if !inventory.ScopeCurrent {
			cell.State = "stale"
			cell.ReasonCode = "scope_revision_changed"
		}
		out.Requirements = append(out.Requirements, cell)
	}
	if len(expected) != 0 {
		return Coverage{}, ErrInvalidContract
	}
	out.KnowledgeInventoryRevision, err = inventoryRevision(ctx, tx, sc)
	if err != nil {
		return Coverage{}, err
	}
	return out, nil
}

// ProjectCoverageTx returns the deterministic coverage projection to trusted callers that have
// already authorized and scoped tx. It performs no audit or transaction management.
func ProjectCoverageTx(ctx context.Context, tx *sql.Tx, sc Scope, id string) (Coverage, error) {
	if tx == nil || !contractUUID(sc.TenantID) || !contractUUID(sc.SiteID) || !contractUUID(id) {
		return Coverage{}, ErrInvalidContract
	}
	return projectCoverage(ctx, tx, sc, id)
}

// SiteCoverage is the site-level coverage read (GET /v1/sites/{site}/coverage). Readiness is
// NOT_READY with a reason code unless the site's single discovery inventory reports knowledge_ready.
type SiteCoverage struct {
	SchemaVersion int       `json:"schema_version"`
	Readiness     string    `json:"readiness"`
	ReasonCode    string    `json:"reason_code,omitempty"`
	InventoryID   string    `json:"inventory_id,omitempty"`
	Coverage      *Coverage `json:"coverage,omitempty"`
}

// ReadSiteCoverage resolves the site's discovery inventory server-side and projects its current
// revision with the per-inventory evaluator. Absent, ambiguous or unavailable inventories are an
// explicit NOT_READY, never an empty or cached result.
func (s *Service) ReadSiteCoverage(ctx context.Context, a Actor, sc Scope) (SiteCoverage, error) {
	tx, err := s.beginLocked(ctx, a, sc, false, true)
	if err != nil {
		return SiteCoverage{}, err
	}
	defer tx.Rollback()
	out := SiteCoverage{SchemaVersion: 1, Readiness: "NOT_READY"}
	rows, err := tx.QueryContext(ctx, `SELECT inventory_id FROM supervisor.knowledge_discovery_heads WHERE tenant_id=$1 AND site_id=$2 ORDER BY inventory_id LIMIT 2`, sc.TenantID, sc.SiteID)
	if err != nil {
		return SiteCoverage{}, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return SiteCoverage{}, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return SiteCoverage{}, err
	}
	// ponytail: a site with several inventories (area/process/capability) is reported as
	// ambiguous rather than picking one; list them all if the spec defines a site aggregate.
	switch len(ids) {
	case 0:
		out.ReasonCode = "NO_INVENTORY"
	case 1:
		coverage, e := projectCoverage(ctx, tx, sc, ids[0])
		switch {
		// The inventory was resolved inside this authorized site read, so a revoked anchor
		// or decision source is reported as unavailable instead of the per-inventory 404.
		// Its ID is withheld because the per-inventory read of it is a 404.
		case errors.Is(e, ErrNotFound):
			out.ReasonCode = "INVENTORY_UNAVAILABLE"
		// Deliberate fail-closed refusal: a corrupt inventory (template or decision shape
		// mismatch, ErrInvalidContract -> 422) is never downgraded to a NOT_READY body.
		case e != nil:
			return SiteCoverage{}, e
		default:
			out.InventoryID = ids[0]
			out.Coverage = &coverage
			out.ReasonCode = siteReason(coverage)
			if out.ReasonCode == "" {
				out.Readiness = "READY"
			}
		}
	default:
		out.ReasonCode = "INVENTORY_AMBIGUOUS"
	}
	if err = audit(ctx, tx, a, sc, sc.SiteID, nil, "coverage.read"); err != nil {
		return SiteCoverage{}, err
	}
	if err = s.finish(ctx, tx, a); err != nil {
		return SiteCoverage{}, err
	}
	return out, nil
}

func siteReason(c Coverage) string {
	for _, cell := range c.Requirements {
		if cell.State == "stale" {
			return "COVERAGE_STALE"
		}
	}
	switch {
	case c.ApprovedCount < c.ApplicableCount:
		return "COVERAGE_INCOMPLETE"
	case !c.KnowledgeReady:
		return "AUTHORIZATION_DISABLED"
	}
	return ""
}
