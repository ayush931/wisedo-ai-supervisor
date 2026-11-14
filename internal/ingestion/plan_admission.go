package ingestion

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/production"
)

type planAdmission struct {
	reason                     string
	planRevisionID, approvalID string
}

// scopeAllowsKind reports whether a reviewed authority scope names a record kind. A
// scope is a comma- or space-separated list of "kind" or "kind:qualifier" tokens.
func scopeAllowsKind(scope, kind string) bool {
	for _, token := range strings.FieldsFunc(scope, func(r rune) bool { return r == ',' || r == ' ' }) {
		if name, _, _ := strings.Cut(token, ":"); name == kind {
			return true
		}
	}
	return false
}

// admitPlan decides whether a production_plan event may contribute. It is admitted
// only when every condition holds; otherwise it is durably quarantined with zero
// eligible contribution:
//   - the source's reviewed authority scope names production_plan (else OUT_OF_SCOPE):
//     an ingestion credential cannot self-expand into plan authority;
//   - the event names both its asset and order, and the payload's internal IDs equal
//     the entities the source's approved mapping resolves them to (else MAPPING_MISMATCH
//     or UNKNOWN_MAPPING) — a source cannot post a plan for an asset it is not mapped to;
//   - the cited approval passes planApproval (else UNAPPROVED_PLAN or SUPERSEDED_PLAN).
func admitPlan(ctx context.Context, tx *sql.Tx, sc Scope, authorityScope string, ev Event, asset, order sql.NullString) (planAdmission, error) {
	plan, err := production.DecodePlan(ev.Payload)
	if err != nil {
		return planAdmission{}, ErrInvalid
	}
	if !scopeAllowsKind(authorityScope, "production_plan") {
		return planAdmission{reason: "OUT_OF_SCOPE"}, nil
	}
	if ev.AssetExternalKey == "" || ev.OrderExternalKey == "" || !asset.Valid || !order.Valid {
		return planAdmission{reason: "UNKNOWN_MAPPING"}, nil
	}
	if asset.String != plan.AssetID || order.String != plan.OrderID {
		return planAdmission{reason: "MAPPING_MISMATCH"}, nil
	}
	reason, planRevisionID, err := planApproval(ctx, tx, sc, plan)
	if err != nil || reason != "" {
		return planAdmission{reason: reason}, err
	}
	return planAdmission{planRevisionID: planRevisionID, approvalID: plan.ApprovalRef}, nil
}

// planApproval is the single authoritative eligibility predicate for a plan citing an
// approval. It returns "" and the plan revision ID only when:
//   - approval_ref is a PlanApprovalV1 in this tenant/site that is the current decision
//     for its plan revision and approves it;
//   - the approval's canonical payload digest equals the digest of this exact payload, so
//     a valid approval ID paired with altered rates, breaks or window never admits;
//   - no later plan revision for the same order and asset whose window overlaps this one
//     and that has not been rejected exists (a newer overlapping revision, approved or
//     still pending, contests and supersedes this one: SUPERSEDED_PLAN; a revision for
//     another shift, or a rejected one, does not);
//   - none of the approval's cited source spans has been revoked.
//
// The review head is share-locked so a concurrent rejection serializes against callers.
func planApproval(ctx context.Context, tx *sql.Tx, sc Scope, plan production.Plan) (string, string, error) {
	_, payloadDigest, err := production.CanonicalPlanPayload(sc.TenantID, sc.SiteID, plan.Body())
	if err != nil {
		return "", "", ErrInvalid
	}
	var planRevisionID, approvedDigest, decision string
	var current bool
	err = tx.QueryRowContext(ctx, `SELECT a.plan_revision_id,a.canonical_payload_sha256,a.decision,h.current_revision=a.review_revision FROM supervisor.plan_approvals a JOIN supervisor.plan_review_heads h ON (h.tenant_id,h.site_id,h.plan_revision_id)=(a.tenant_id,a.site_id,a.plan_revision_id) WHERE a.tenant_id=$1 AND a.site_id=$2 AND a.id=$3 FOR SHARE OF h`, sc.TenantID, sc.SiteID, plan.ApprovalRef).Scan(&planRevisionID, &approvedDigest, &decision, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return "UNAPPROVED_PLAN", "", nil
	}
	if err != nil {
		return "", "", err
	}
	if !current || decision != "approve" || approvedDigest != payloadDigest {
		return "UNAPPROVED_PLAN", "", nil
	}
	var superseded bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.plan_revisions p JOIN supervisor.plan_revisions later ON (later.tenant_id,later.site_id,later.order_id,later.asset_id)=(p.tenant_id,p.site_id,p.order_id,p.asset_id) AND later.plan_revision>p.plan_revision AND later.window_start<p.window_end AND later.window_end>p.window_start LEFT JOIN supervisor.plan_review_heads h ON (h.tenant_id,h.site_id,h.plan_revision_id)=(later.tenant_id,later.site_id,later.id) LEFT JOIN supervisor.plan_approvals a ON (a.tenant_id,a.site_id,a.plan_revision_id,a.review_revision)=(h.tenant_id,h.site_id,h.plan_revision_id,h.current_revision) WHERE p.tenant_id=$1 AND p.site_id=$2 AND p.id=$3 AND a.decision IS DISTINCT FROM 'reject')`, sc.TenantID, sc.SiteID, planRevisionID).Scan(&superseded); err != nil {
		return "", "", err
	}
	if superseded {
		return "SUPERSEDED_PLAN", "", nil
	}
	var revoked bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.plan_approval_spans s JOIN supervisor.evidence_spans es ON (es.tenant_id,es.site_id,es.id)=(s.tenant_id,s.site_id,s.span_id) JOIN supervisor.evidence_heads eh ON (eh.tenant_id,eh.site_id,eh.source_id)=(es.tenant_id,es.site_id,es.source_id) WHERE s.tenant_id=$1 AND s.site_id=$2 AND s.approval_id=$3 AND (eh.revoked OR EXISTS(SELECT 1 FROM supervisor.evidence_revocations v WHERE (v.tenant_id,v.site_id,v.source_id)=(eh.tenant_id,eh.site_id,eh.source_id))))`, sc.TenantID, sc.SiteID, plan.ApprovalRef).Scan(&revoked); err != nil {
		return "", "", err
	}
	if revoked {
		return "UNAPPROVED_PLAN", "", nil
	}
	return "", planRevisionID, nil
}

// PlanRecordEligibility re-evaluates a stored production_plan record against the
// authoritative ledger now. Admission at intake is only a durable receipt: a later
// rejection, re-approval, superseding revision or evidence revocation withdraws
// eligibility, so every consumer (contexts, cases) must call this on each retrieval
// rather than trusting the stored admission. It returns "" when eligible, otherwise
// the stable reason code. tx must already carry the tenant context.
func PlanRecordEligibility(ctx context.Context, tx *sql.Tx, sc Scope, sourceID string, sequence int64) (string, error) {
	var kind string
	var quarantine, approvalID sql.NullString
	var payload []byte
	err := tx.QueryRowContext(ctx, `SELECT event_kind,quarantine_reason,plan_approval_id,payload FROM supervisor.operational_event_records WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND source_sequence=$4`, sc.TenantID, sc.SiteID, sourceID, sequence).Scan(&kind, &quarantine, &approvalID, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if kind != "production_plan" {
		return "", ErrInvalid
	}
	if quarantine.Valid {
		return quarantine.String, nil
	}
	plan, err := production.DecodePlan(payload)
	if err != nil || !approvalID.Valid || plan.ApprovalRef != approvalID.String {
		return "", ErrInvalid
	}
	reason, _, err := planApproval(ctx, tx, sc, plan)
	return reason, err
}

// PlanApprovalEligibility checks a captured plan directly, before it has been
// ingested as an event. Advisory planning uses the same current approval and
// source-revocation rules as production-context intake. tx must carry tenant scope.
func PlanApprovalEligibility(ctx context.Context, tx *sql.Tx, sc Scope, plan production.Plan) (string, error) {
	reason, _, err := planApproval(ctx, tx, sc, plan)
	return reason, err
}
