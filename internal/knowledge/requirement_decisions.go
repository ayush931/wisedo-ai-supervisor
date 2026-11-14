package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

// DecisionEligible: the decision is the current head of a current inventory and snapshot, its
// stored witness still passes today's binding rules, and a qualified binding still qualifies.
// It never means readiness.
type RequirementDecision struct {
	SchemaVersion             int                `json:"schema_version"`
	DecisionID                string             `json:"decision_id"`
	InventoryID               string             `json:"inventory_id"`
	InventoryRevision         int64              `json:"inventory_revision"`
	RequirementID             string             `json:"requirement_id"`
	Revision                  int64              `json:"revision"`
	CurrentRevision           int64              `json:"current_revision"`
	Verdict                   string             `json:"verdict"`
	Reason                    string             `json:"reason"`
	ActorIssuer               string             `json:"actor_issuer"`
	ActorSubject              string             `json:"actor_subject"`
	AuthenticatedAt           time.Time          `json:"authenticated_at"`
	CreatedAt                 time.Time          `json:"created_at"`
	SnapshotID                string             `json:"snapshot_id,omitempty"`
	SnapshotSHA256            string             `json:"snapshot_sha256,omitempty"`
	SnapshotInventoryRevision int64              `json:"snapshot_inventory_revision,omitempty"`
	AsOf                      string             `json:"as_of,omitempty"`
	ClaimRefs                 []ConflictClaimRef `json:"claim_refs"`
	DecisionEligible          bool               `json:"decision_eligible"`
	FreshnessPolicyRevision   int64              `json:"freshness_policy_revision,omitempty"`
	VerificationRefs          []VerificationRef  `json:"verification_refs,omitempty"`
	bindingCurrent            bool
	// witnessReason is set when the stored witness fails the current ValidateRequirementBinding
	// rules (for example a decision accepted before the WIS-615 process-scope fix). Such a decision
	// is never approved coverage.
	witnessReason string
}

func requirementScope(ctx context.Context, tx *sql.Tx, sc Scope, inventoryID, requirementID string, revision int64) (DiscoveryInventory, string, error) {
	inventory, err := loadDiscoveryInventory(ctx, tx, sc, inventoryID, revision)
	if err != nil {
		return inventory, "", err
	}
	var key string
	err = tx.QueryRowContext(ctx, `SELECT requirement_key FROM supervisor.knowledge_discovery_requirements WHERE tenant_id=$1 AND site_id=$2 AND inventory_id=$3 AND revision=$4 AND requirement_id=$5`, sc.TenantID, sc.SiteID, inventoryID, revision, requirementID).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return inventory, key, err
}
func requirementHead(ctx context.Context, tx *sql.Tx, sc Scope, inventoryID, requirementID string, inventoryRevision int64) (int64, error) {
	var revision int64
	err := tx.QueryRowContext(ctx, `SELECT current_revision FROM supervisor.knowledge_requirement_decision_heads WHERE tenant_id=$1 AND site_id=$2 AND inventory_id=$3 AND inventory_revision=$4 AND requirement_id=$5`, sc.TenantID, sc.SiteID, inventoryID, inventoryRevision, requirementID).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return revision, err
}

func loadRequirementDecision(ctx context.Context, tx *sql.Tx, sc Scope, inventory DiscoveryInventory, requirementID string, revision int64) (RequirementDecision, error) {
	var out RequirementDecision
	current, err := requirementHead(ctx, tx, sc, inventory.InventoryID, requirementID, inventory.Revision)
	if err != nil {
		return out, err
	}
	if revision == 0 {
		revision = current
	}
	var snapshotID, hash sql.NullString
	var epoch sql.NullInt64
	var at sql.NullTime
	var expectedClaims, expectedVerifications int
	var policyRevision sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT schema_version,decision_id,inventory_id,inventory_revision,requirement_id,revision,verdict,reason,actor_issuer,actor_subject,auth_time,created_at,snapshot_id,snapshot_sha256,snapshot_inventory_revision,as_of,claim_count,freshness_policy_revision,verification_count FROM supervisor.knowledge_requirement_decisions WHERE tenant_id=$1 AND site_id=$2 AND inventory_id=$3 AND inventory_revision=$4 AND requirement_id=$5 AND revision=$6`, sc.TenantID, sc.SiteID, inventory.InventoryID, inventory.Revision, requirementID, revision).Scan(&out.SchemaVersion, &out.DecisionID, &out.InventoryID, &out.InventoryRevision, &out.RequirementID, &out.Revision, &out.Verdict, &out.Reason, &out.ActorIssuer, &out.ActorSubject, &out.AuthenticatedAt, &out.CreatedAt, &snapshotID, &hash, &epoch, &at, &expectedClaims, &policyRevision, &expectedVerifications)
	if errors.Is(err, sql.ErrNoRows) {
		return RequirementDecision{}, ErrNotFound
	}
	if err != nil {
		return RequirementDecision{}, err
	}
	out.FreshnessPolicyRevision = policyRevision.Int64
	out.CurrentRevision = current
	out.SnapshotID = snapshotID.String
	out.SnapshotSHA256 = hash.String
	out.SnapshotInventoryRevision = epoch.Int64
	if at.Valid {
		out.AsOf = at.Time.UTC().Format(time.RFC3339Nano)
	}
	out.ClaimRefs = []ConflictClaimRef{}
	rows, err := tx.QueryContext(ctx, `SELECT claim_id,claim_revision FROM supervisor.knowledge_requirement_decision_claims WHERE tenant_id=$1 AND site_id=$2 AND inventory_id=$3 AND inventory_revision=$4 AND requirement_id=$5 AND decision_revision=$6 ORDER BY claim_id LIMIT 101`, sc.TenantID, sc.SiteID, out.InventoryID, out.InventoryRevision, out.RequirementID, out.Revision)
	if err != nil {
		return RequirementDecision{}, err
	}
	for rows.Next() {
		var ref ConflictClaimRef
		if err = rows.Scan(&ref.ClaimID, &ref.Revision); err != nil {
			rows.Close()
			return RequirementDecision{}, err
		}
		out.ClaimRefs = append(out.ClaimRefs, ref)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return RequirementDecision{}, err
	}
	if len(out.ClaimRefs) != expectedClaims || len(out.ClaimRefs) > 100 || (out.Verdict == "bind" && out.SnapshotID != "" && len(out.ClaimRefs) == 0) || (out.Verdict != "bind" && len(out.ClaimRefs) > 0) {
		return RequirementDecision{}, ErrInvalidContract
	}
	rows, err = tx.QueryContext(ctx, `SELECT source_id,verification_id,verification_revision FROM supervisor.knowledge_requirement_decision_verifications WHERE tenant_id=$1 AND site_id=$2 AND inventory_id=$3 AND inventory_revision=$4 AND requirement_id=$5 AND decision_revision=$6 ORDER BY source_id LIMIT 501`, sc.TenantID, sc.SiteID, out.InventoryID, out.InventoryRevision, out.RequirementID, out.Revision)
	if err != nil {
		return RequirementDecision{}, err
	}
	for rows.Next() {
		var ref VerificationRef
		if err = rows.Scan(&ref.SourceID, &ref.VerificationID, &ref.VerificationRevision); err != nil {
			rows.Close()
			return RequirementDecision{}, err
		}
		out.VerificationRefs = append(out.VerificationRefs, ref)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return RequirementDecision{}, err
	}
	if len(out.VerificationRefs) != expectedVerifications || len(out.VerificationRefs) > 500 {
		return RequirementDecision{}, ErrInvalidContract
	}
	snapshotCurrent := true
	if out.Verdict == "bind" && out.SnapshotID != "" {
		snapshot, e := loadSnapshot(ctx, tx, sc, out.SnapshotID)
		if e != nil {
			return RequirementDecision{}, e
		}
		snapshotCurrent = !snapshot.Stale
		// Re-check the witness against today's scope rules on every read: a decision written under
		// an older, looser rule stays in history but must not count as coverage (WIS-637 review).
		key := ""
		for _, r := range inventory.Requirements {
			if r.RequirementID == out.RequirementID {
				key = r.Key
			}
		}
		in := RequirementDecisionInput{InventoryRevision: out.InventoryRevision, Verdict: out.Verdict, Reason: out.Reason, SnapshotID: out.SnapshotID, AsOf: out.AsOf, ClaimRefs: out.ClaimRefs, FreshnessPolicyRevision: out.FreshnessPolicyRevision, VerificationRefs: out.VerificationRefs}
		out.witnessReason, err = storedWitnessReason(ValidateRequirementBinding(inventory, key, snapshot, in))
		if err != nil {
			return RequirementDecision{}, err
		}
	}
	if out.Verdict == "withdraw" {
		// A withdrawal retains the access boundary of the immediately withdrawn record.
		var priorVerdict string
		var priorSnapshot sql.NullString
		e := tx.QueryRowContext(ctx, `SELECT verdict,snapshot_id FROM supervisor.knowledge_requirement_decisions WHERE tenant_id=$1 AND site_id=$2 AND inventory_id=$3 AND inventory_revision=$4 AND requirement_id=$5 AND revision=$6`, sc.TenantID, sc.SiteID, out.InventoryID, out.InventoryRevision, out.RequirementID, out.Revision-1).Scan(&priorVerdict, &priorSnapshot)
		if e != nil {
			return RequirementDecision{}, e
		}
		if priorVerdict == "withdraw" {
			return RequirementDecision{}, ErrInvalidContract
		}
		if priorVerdict == "bind" && priorSnapshot.Valid {
			if _, e = loadSnapshot(ctx, tx, sc, priorSnapshot.String); e != nil {
				return RequirementDecision{}, e
			}
		}
	}
	out.DecisionEligible = out.Verdict != "withdraw" && out.Revision == current && inventory.Revision == inventory.CurrentRevision && inventory.ScopeCurrent && snapshotCurrent
	out.DecisionEligible = out.DecisionEligible && out.witnessReason == ""
	out.bindingCurrent = out.DecisionEligible
	if out.Verdict == "bind" && out.FreshnessPolicyRevision > 0 {
		reason, _, _, e := qualifyRequirement(ctx, tx, sc, out, nil)
		if e != nil {
			return RequirementDecision{}, e
		}
		out.DecisionEligible = out.DecisionEligible && reason == ""
	}
	return out, nil
}

// storedWitnessReason maps a re-validation failure of a stored witness to its coverage reason
// code. Any other error fails the read rather than inventing a reason (rule 06).
func storedWitnessReason(err error) (string, error) {
	switch {
	case err == nil:
		return "", nil
	case errors.Is(err, ErrBindingBlocked):
		return "witness_scope_invalid", nil
	case errors.Is(err, ErrInvalidContract):
		return "witness_contract_invalid", nil
	case errors.Is(err, ErrBindingUnsupported):
		return "binding_unsupported", nil
	}
	return "", err
}

func (s *Service) DecideRequirement(ctx context.Context, a Actor, sc Scope, inventoryID, requirementID string, in RequirementDecisionInput, key string, expected int64) (RequirementDecision, error) {
	if !contractUUID(inventoryID) || !contractUUID(requirementID) || !mutation(key, expected) {
		return RequirementDecision{}, ErrInvalidContract
	}
	if err := in.Validate(); err != nil {
		return RequirementDecision{}, err
	}
	tx, err := s.beginLocked(ctx, a, sc, true, true)
	if err != nil {
		return RequirementDecision{}, err
	}
	defer tx.Rollback()
	auth, err := s.requireReviewer(ctx, tx, a, sc)
	if err != nil {
		return RequirementDecision{}, err
	}
	inventory, requirementKey, err := requirementScope(ctx, tx, sc, inventoryID, requirementID, in.InventoryRevision)
	if err != nil {
		return RequirementDecision{}, err
	}
	digest := fingerprint("requirement", struct {
		InventoryID, RequirementID string
		Input                      RequirementDecisionInput
	}{inventoryID, requirementID, in}, expected)
	prior, replayed, err := replay(ctx, tx, a, sc, key, digest)
	if err != nil {
		return RequirementDecision{}, err
	}
	if replayed {
		out, e := loadRequirementDecision(ctx, tx, sc, inventory, requirementID, prior)
		if e != nil {
			return RequirementDecision{}, e
		}
		if e = audit(ctx, tx, a, sc, requirementID, prior, "mutation.replayed"); e != nil {
			return RequirementDecision{}, e
		}
		if e = s.finishReview(ctx, tx, a); e != nil {
			return RequirementDecision{}, e
		}
		return out, nil
	}
	head, err := requirementHead(ctx, tx, sc, inventoryID, requirementID, in.InventoryRevision)
	if err != nil {
		return RequirementDecision{}, err
	}
	if in.Verdict == "withdraw" {
		if head == 0 {
			return RequirementDecision{}, ErrInvalidContract
		}
		prior, e := loadRequirementDecision(ctx, tx, sc, inventory, requirementID, head)
		if e != nil {
			return RequirementDecision{}, e
		}
		if prior.Verdict == "withdraw" {
			return RequirementDecision{}, ErrInvalidContract
		}
	}
	if head != expected {
		return RequirementDecision{}, ErrRevision
	}
	if in.Verdict != "withdraw" && (!inventory.ScopeCurrent || inventory.Revision != inventory.CurrentRevision) {
		return RequirementDecision{}, ErrSourceChanged
	}
	var snapshot Snapshot
	var verificationPins []verificationPin
	if in.Verdict == "bind" && in.SnapshotID != "" {
		snapshot, err = loadSnapshot(ctx, tx, sc, in.SnapshotID)
		if err != nil {
			return RequirementDecision{}, err
		}
		if snapshot.Stale {
			return RequirementDecision{}, ErrSnapshotStale
		}
		if err = ValidateRequirementBinding(inventory, requirementKey, snapshot, in); err != nil {
			return RequirementDecision{}, err
		}
	}
	if in.Verdict == "bind" {
		policyOnly := in.SnapshotID == ""
		if policyOnly && requirementKey != "evidence_freshness_policy" {
			return RequirementDecision{}, ErrBindingBlocked
		}
		if !policyOnly && requirementKey == "evidence_freshness_policy" {
			return RequirementDecision{}, ErrBindingBlocked
		}
		if in.FreshnessPolicyRevision > 0 {
			var now time.Time
			if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
				return RequirementDecision{}, err
			}
			d := RequirementDecision{FreshnessPolicyRevision: in.FreshnessPolicyRevision, SnapshotID: in.SnapshotID, ClaimRefs: in.ClaimRefs, VerificationRefs: in.VerificationRefs}
			reason, _, pins, e := qualifyRequirement(ctx, tx, sc, d, &now)
			if e != nil {
				return RequirementDecision{}, e
			}
			if reason != "" {
				return RequirementDecision{}, ErrFreshnessBlocked
			}
			verificationPins = pins
		}
	}
	revision := head + 1
	if head == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_requirement_decision_heads(tenant_id,site_id,inventory_id,inventory_revision,requirement_id,current_revision) VALUES($1,$2,$3,$4,$5,1)`, sc.TenantID, sc.SiteID, inventoryID, in.InventoryRevision, requirementID)
		if err != nil {
			return RequirementDecision{}, err
		}
	}
	var snapshotID, hash, epoch, at, policy any
	if in.FreshnessPolicyRevision > 0 {
		policy = in.FreshnessPolicyRevision
	}
	if in.Verdict == "bind" && in.SnapshotID != "" {
		snapshotID = snapshot.SnapshotID
		hash = snapshot.SHA256
		epoch = snapshot.InventoryRevision
		at = in.AsOf
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_requirement_decisions(tenant_id,site_id,inventory_id,inventory_revision,requirement_id,revision,decision_id,verdict,reason,actor_issuer,actor_subject,auth_time,snapshot_id,snapshot_sha256,snapshot_inventory_revision,as_of,claim_count,freshness_policy_revision,verification_count) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`, sc.TenantID, sc.SiteID, inventoryID, in.InventoryRevision, requirementID, revision, uuid.NewString(), in.Verdict, in.Reason, a.Issuer, a.Subject, auth, snapshotID, hash, epoch, at, len(in.ClaimRefs), policy, len(verificationPins))
	if err != nil {
		return RequirementDecision{}, err
	}
	for _, ref := range in.ClaimRefs {
		_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_requirement_decision_claims(tenant_id,site_id,inventory_id,inventory_revision,requirement_id,decision_revision,snapshot_id,claim_id,claim_revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, sc.TenantID, sc.SiteID, inventoryID, in.InventoryRevision, requirementID, revision, snapshot.SnapshotID, ref.ClaimID, ref.Revision)
		if err != nil {
			return RequirementDecision{}, err
		}
	}
	for _, pin := range verificationPins {
		_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_requirement_decision_verifications(tenant_id,site_id,inventory_id,inventory_revision,requirement_id,decision_revision,source_id,source_revision,source_sha256,source_access_epoch,verification_id,verification_revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, sc.TenantID, sc.SiteID, inventoryID, in.InventoryRevision, requirementID, revision, pin.SourceID, pin.SourceRevision, pin.SourceSHA256, pin.SourceAccessEpoch, pin.VerificationID, pin.VerificationRevision)
		if err != nil {
			return RequirementDecision{}, err
		}
	}
	if head > 0 {
		_, err = tx.ExecContext(ctx, `UPDATE supervisor.knowledge_requirement_decision_heads SET current_revision=$6 WHERE tenant_id=$1 AND site_id=$2 AND inventory_id=$3 AND inventory_revision=$4 AND requirement_id=$5`, sc.TenantID, sc.SiteID, inventoryID, in.InventoryRevision, requirementID, revision)
		if err != nil {
			return RequirementDecision{}, err
		}
	}
	if err = saveReceipt(ctx, tx, a, sc, key, digest, "requirement", requirementID, revision); err != nil {
		return RequirementDecision{}, err
	}
	if err = audit(ctx, tx, a, sc, requirementID, revision, "requirement.decided"); err != nil {
		return RequirementDecision{}, err
	}
	out, err := loadRequirementDecision(ctx, tx, sc, inventory, requirementID, revision)
	if err != nil {
		return RequirementDecision{}, err
	}
	if err = s.finishReview(ctx, tx, a); err != nil {
		return RequirementDecision{}, err
	}
	return out, nil
}
func (s *Service) ReadRequirementDecision(ctx context.Context, a Actor, sc Scope, inventoryID, requirementID string, inventoryRevision, decisionRevision int64) (RequirementDecision, error) {
	if !contractUUID(inventoryID) || !contractUUID(requirementID) || inventoryRevision < 1 || decisionRevision < 0 {
		return RequirementDecision{}, ErrInvalidContract
	}
	tx, err := s.beginLocked(ctx, a, sc, false, true)
	if err != nil {
		return RequirementDecision{}, err
	}
	defer tx.Rollback()
	inventory, _, err := requirementScope(ctx, tx, sc, inventoryID, requirementID, inventoryRevision)
	if err != nil {
		return RequirementDecision{}, err
	}
	out, err := loadRequirementDecision(ctx, tx, sc, inventory, requirementID, decisionRevision)
	if err != nil {
		return RequirementDecision{}, err
	}
	if err = audit(ctx, tx, a, sc, requirementID, out.Revision, "requirement.read"); err != nil {
		return RequirementDecision{}, err
	}
	if err = s.finish(ctx, tx, a); err != nil {
		return RequirementDecision{}, err
	}
	return out, nil
}
