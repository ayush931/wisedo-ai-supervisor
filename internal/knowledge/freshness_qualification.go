package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrFreshnessBlocked = errors.New("document freshness qualification is unavailable or stale")

type verificationPin struct {
	VerificationRef
	SourceRevision    int64
	SourceSHA256      string
	SourceAccessEpoch int64
}

func freshnessWindow(verified time.Time, seconds int64, now time.Time) (string, *time.Time) {
	if seconds <= 0 || seconds > int64((1<<63-1)/int64(time.Second)) {
		return "policy_changed", nil
	}
	expiry := verified.Add(time.Duration(seconds) * time.Second)
	if now.Before(verified) {
		return "confirmation_in_future", &expiry
	}
	if !now.Before(expiry) {
		return "document_verification_expired", &expiry
	}
	return "", &expiry
}
func qualifyRequirement(ctx context.Context, tx *sql.Tx, sc Scope, d RequirementDecision, now *time.Time) (string, *time.Time, []verificationPin, error) {
	if d.FreshnessPolicyRevision == 0 {
		return "freshness_policy_missing", nil, nil, nil
	}
	policy, err := loadFreshnessPolicy(ctx, tx, sc, d.FreshnessPolicyRevision)
	if errors.Is(err, ErrNotFound) {
		return "policy_changed", nil, nil, nil
	}
	if err != nil {
		return "", nil, nil, err
	}
	if !policy.PolicyCurrent || policy.MaxAgeSeconds == nil {
		return "policy_changed", nil, nil, nil
	}
	if d.SnapshotID == "" {
		return "", nil, nil, nil
	}
	snapshot, err := loadSnapshot(ctx, tx, sc, d.SnapshotID)
	if err != nil {
		return "", nil, nil, err
	}
	if snapshot.Stale {
		return "decision_version_stale", nil, nil, nil
	}
	if len(snapshot.Contents.Sources) != len(d.VerificationRefs) || len(d.VerificationRefs) > 500 {
		return "confirmation_changed", nil, nil, nil
	}
	refs := map[string]VerificationRef{}
	for _, ref := range d.VerificationRefs {
		if _, exists := refs[ref.SourceID]; exists {
			return "confirmation_changed", nil, nil, nil
		}
		refs[ref.SourceID] = ref
	}
	var expiry *time.Time
	pins := []verificationPin{}
	ageReason := ""
	for _, source := range snapshot.Contents.Sources {
		var sourceKind string
		err = tx.QueryRowContext(ctx, `SELECT kind FROM supervisor.evidence_revisions WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND revision=$4`, sc.TenantID, sc.SiteID, source.SourceID, source.Revision).Scan(&sourceKind)
		if err != nil {
			return "", nil, nil, err
		}
		if sourceKind != "document" {
			return "unsupported_source_kind", nil, nil, nil
		}
		ref, found := refs[source.SourceID]
		if !found {
			return "confirmation_changed", nil, nil, nil
		}
		var pin verificationPin
		pin.VerificationRef = ref
		var verified sql.NullTime
		var kind string
		var current bool
		err = tx.QueryRowContext(ctx, `SELECT d.source_revision,d.source_sha256,d.source_access_epoch,d.verified_at,r.kind,
 (d.verdict='confirm_current' AND ch.current_revision=d.revision AND eh.current_revision=d.source_revision AND eh.access_epoch=d.source_access_epoch AND NOT eh.revoked AND ah.current_revision=d.authority_revision AND a.verdict='designate' AND (r.extraction IS NULL OR a.model_extracted_confirmed) AND NOT EXISTS(SELECT 1 FROM supervisor.evidence_revocations rv WHERE (rv.tenant_id,rv.site_id,rv.source_id)=(d.tenant_id,d.site_id,d.source_id)))
 FROM supervisor.evidence_currentness_decisions d JOIN supervisor.evidence_currentness_heads ch USING(tenant_id,site_id,source_id)
 JOIN supervisor.evidence_heads eh USING(tenant_id,site_id,source_id)
 JOIN supervisor.evidence_revisions r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=(d.tenant_id,d.site_id,d.source_id,d.source_revision)
 JOIN supervisor.evidence_authority_heads ah ON (ah.tenant_id,ah.site_id,ah.source_id)=(d.tenant_id,d.site_id,d.source_id)
 JOIN supervisor.evidence_authority_decisions a ON (a.tenant_id,a.site_id,a.source_id,a.revision,a.decision_id)=(d.tenant_id,d.site_id,d.source_id,d.authority_revision,d.authority_decision_id)
 WHERE d.tenant_id=$1 AND d.site_id=$2 AND d.source_id=$3 AND d.revision=$4 AND d.verification_id=$5`, sc.TenantID, sc.SiteID, ref.SourceID, ref.VerificationRevision, ref.VerificationID).Scan(&pin.SourceRevision, &pin.SourceSHA256, &pin.SourceAccessEpoch, &verified, &kind, &current)
		if errors.Is(err, sql.ErrNoRows) {
			return "confirmation_changed", nil, nil, nil
		}
		if err != nil {
			return "", nil, nil, err
		}
		if kind != "document" {
			return "unsupported_source_kind", nil, nil, nil
		}
		if !current || !verified.Valid || pin.SourceRevision != source.Revision || pin.SourceSHA256 != source.SHA256 || pin.SourceAccessEpoch != source.AccessEpoch {
			return "confirmation_changed", nil, nil, nil
		}
		pins = append(pins, pin)
		if now != nil {
			reason, until := freshnessWindow(verified.Time, *policy.MaxAgeSeconds, *now)
			if until != nil && (expiry == nil || until.Before(*expiry)) {
				expiry = until
			}
			if reason != "" && ageReason == "" {
				ageReason = reason
			}
		}
	}
	if now != nil {
		reason, until := qualificationClaimTime(snapshot, d.ClaimRefs, *now)
		if until != nil && (expiry == nil || until.Before(*expiry)) {
			expiry = until
		}
		if ageReason == "" {
			ageReason = reason
		}
	}
	return ageReason, expiry, pins, nil
}
func qualificationClaimTime(snapshot Snapshot, refs []ConflictClaimRef, now time.Time) (string, *time.Time) {
	var expiry *time.Time
	reason := ""
	claims := map[ConflictClaimRef]SnapshotClaim{}
	for _, c := range snapshot.Contents.Claims {
		claims[ConflictClaimRef{ClaimID: c.ClaimID, Revision: c.Revision}] = c
	}
	for _, ref := range refs {
		c, found := claims[ref]
		if !found {
			return "claim_outside_effective_interval", expiry
		}
		from, _, e := normalizedTime(c.EffectiveFrom)
		if e != nil {
			return "claim_outside_effective_interval", expiry
		}
		if now.Before(from) {
			reason = "claim_outside_effective_interval"
		}
		if c.EffectiveTo != nil {
			until, _, e := normalizedTime(*c.EffectiveTo)
			if e != nil || !until.After(from) {
				return "claim_outside_effective_interval", expiry
			}
			if expiry == nil || until.Before(*expiry) {
				expiry = &until
			}
			if !now.Before(until) {
				reason = "claim_outside_effective_interval"
			}
		}
	}
	return reason, expiry
}
