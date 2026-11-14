package approvals

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Dispatch codes name why the executor refused an approved proposal (WIS-277, rule 06). The
// proposal moves to stale with the reason recorded on the action; it is never retried blindly.
const (
	DispatchApproverIneligible = "APPROVER_INELIGIBLE"
	DispatchDigestMismatch     = "DIGEST_MISMATCH"
	DispatchBindingStale       = "BINDING_STALE"
	DispatchEvidenceRevoked    = "EVIDENCE_REVOKED"
	// DispatchEvidenceRevised: a bound evidence revision is no longer current (a source was
	// corrected, or a context or stoppage changed) since the approval (rule 05, rule 06).
	DispatchEvidenceRevised = "EVIDENCE_REVISED"
	// DispatchEvidenceStale: a source behind the bound evidence has not heartbeated within its
	// configured freshness at dispatch, so unchanged revisions no longer prove it is current
	// (rule 05: bound evidence freshness; WIS-277 review M4).
	DispatchEvidenceStale = "EVIDENCE_STALE"
	// DispatchPolicyChanged: the payload was approved under another policy revision than the
	// current one (rule 05: current policy is re-evaluated at dispatch).
	DispatchPolicyChanged = "POLICY_CHANGED"
	DispatchScopeRefused  = "SCOPE_REFUSED"
)

// Dispatch is the outcome of Settle for one proposal.
type Dispatch struct {
	ProposalID       string
	ProposalRevision int64
	// State is the approval state after settling: Consumed when the payload may be executed,
	// Pending while the decision is open, or the terminal state that ended it.
	State State
	// Digest is the canonical payload digest the reviewers approved.
	Digest string
	// Payload is set only when State is Consumed: exactly what the reviewers approved.
	Payload Payload
	// Reason is set when the executor itself refused an approved proposal.
	Reason string
}

// ScopeCheck runs the caller's current-scope predicates (assignee still confirmed, asset
// lineage) on the approved payload, after the approval checks and before consumption. A
// non-empty code refuses dispatch with that reason.
type ScopeCheck func(Payload) (code string, err error)

// Settle is the executor's half of the approval lifecycle, inside the caller's transaction,
// which already holds the site write lock, the approvers' and assignee's memberships and the
// bound case row (lock order: scope, memberships, case, proposal). It records an expiry that is
// due, then for an approved proposal re-evaluates every dispatch predicate independently of the
// decision: the window, each approver's current eligibility, the exact digest the approvers
// signed, the case binding, the bound evidence and the caller's scope check. A pass consumes the
// approval; a failure marks it stale with the reason. Nothing here trusts that an earlier check
// still holds, and a Temporal or worker retry is never authority on its own.
//
// With consume false (execution not enabled for the scope) it only records a due expiry: an
// approval is never consumed or invalidated by a runtime that is switched off.
func Settle(ctx context.Context, tx *sql.Tx, tenantID, siteID, proposalID string, now time.Time, actor ActorRef, consume bool, check ScopeCheck) (Dispatch, error) {
	sc := Scope{TenantID: tenantID, SiteID: siteID}
	if !validID(tenantID) || !validID(siteID) || !validID(proposalID) || check == nil {
		return Dispatch{}, ErrNotFound
	}
	h, err := lockHead(ctx, tx, sc, proposalID)
	if err != nil {
		return Dispatch{}, err
	}
	out := Dispatch{ProposalID: proposalID, ProposalRevision: h.proposalRevision, Digest: h.digest}
	if _, err = expireIfDue(ctx, tx, sc, proposalID, &h, now); err != nil {
		return out, err
	}
	if out.State = h.state; h.state != Approved || !consume {
		return out, nil
	}
	refuse := func(code string) (Dispatch, error) {
		next, err := moveState(ctx, tx, sc, proposalID, h, Stale, MarkStale, "")
		if err != nil {
			return out, err
		}
		if err = audit(ctx, tx, &actor, sc, proposalID, next, "proposal.stale"); err != nil {
			return out, err
		}
		out.State, out.Reason = Stale, code
		return out, nil
	}

	payload, current, err := storedPayload(ctx, tx, sc, proposalID, h.proposalRevision)
	if err != nil {
		if errors.Is(err, errIntegrity) {
			return refuse(DispatchDigestMismatch)
		}
		return out, err
	}
	approvers, signed, err := currentApprovers(ctx, tx, sc, proposalID, h.proposalRevision)
	if err != nil {
		return out, err
	}
	editors, err := earlierAuthors(ctx, tx, sc, proposalID, h.proposalRevision)
	if err != nil {
		return out, err
	}
	// Every approval must have signed the digest that is stored now, and that digest must be
	// what the stored payload re-encodes to.
	stored := h.digest
	for _, d := range signed {
		if d != stored {
			stored = ""
		}
	}
	proposal := Proposal{State: h.state, Revision: h.stateRevision, ExpiresAt: h.expiresAt, Authority: ProposalAuthority{SubmittedBy: h.submittedBy, EditedBy: editors}}
	if _, err = CheckDispatch(now, proposal, approvers, stored, current); err != nil {
		switch {
		case errors.Is(err, ErrExpired):
			if _, err = expireIfDue(ctx, tx, sc, proposalID, &h, now); err != nil {
				return out, err
			}
			out.State = h.state
			return out, nil
		case errors.Is(err, ErrDigestMismatch):
			return refuse(DispatchDigestMismatch)
		case errors.Is(err, ErrNotEligible):
			return refuse(DispatchApproverIneligible)
		default:
			return out, err
		}
	}
	// The eligibility mapping above is the one PolicyRevision names; an approval granted under
	// another revision is not dispatched on it (WIS-277 review M1). It is judged after the digest,
	// so a tampered payload is refused as DIGEST_MISMATCH, not by what it claims.
	if payload.PolicyRevision != PolicyRevision {
		return refuse(DispatchPolicyChanged)
	}
	if err = checkDispatchBinding(ctx, tx, sc, h.caseID, h.contextID); err != nil {
		if errors.Is(err, ErrStaleBinding) {
			return refuse(DispatchBindingStale)
		}
		return out, err
	}
	if err = checkBoundEvidence(ctx, tx, sc, proposalID, h.proposalRevision); err != nil {
		if errors.Is(err, ErrEvidenceUnavailable) {
			return refuse(DispatchEvidenceRevoked)
		}
		return out, err
	}
	code, err := check(payload)
	if err != nil {
		return out, err
	}
	if code != "" {
		return refuse(code)
	}
	// now was read before this transaction waited for its locks; the window is judged again on
	// the database clock immediately before consumption, so an approval that expired during a
	// lock wait is expired, not consumed (WIS-277 review M2).
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return out, err
	}
	if expired, err := expireIfDue(ctx, tx, sc, proposalID, &h, now); err != nil || expired {
		out.State = h.state
		return out, err
	}
	next, err := moveState(ctx, tx, sc, proposalID, h, Consumed, Consume, "")
	if err != nil {
		return out, err
	}
	if err = audit(ctx, tx, &actor, sc, proposalID, next, "proposal.consumed"); err != nil {
		return out, err
	}
	out.State, out.Payload = Consumed, payload
	return out, nil
}

// checkDispatchBinding requires the case to be waiting on exactly this approval
// (awaiting_approval) and still bound to the context the proposal was generated for.
func checkDispatchBinding(ctx context.Context, tx *sql.Tx, sc Scope, caseID, contextID string) error {
	var state, current string
	err := tx.QueryRowContext(ctx, `SELECT state,context_snapshot_id FROM supervisor.cases WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, caseID).Scan(&state, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrStaleBinding
	}
	if err != nil {
		return err
	}
	if state != "awaiting_approval" || current != contextID {
		return ErrStaleBinding
	}
	return nil
}

// currentApprovers returns every approval of this revision with the approver's eligibility
// today (an active site supervisor: the interim proposal.approve grant), plus the digest each
// one signed. Memberships are read FOR SHARE; the executor already locked them in ID order.
func currentApprovers(ctx context.Context, tx *sql.Tx, sc Scope, proposalID string, revision int64) ([]Reviewer, []string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT d.reviewer_issuer,d.reviewer_subject,d.canonical_payload_sha256,COALESCE(m.active AND m.role='supervisor',false)
 FROM supervisor.approval_decisions d LEFT JOIN supervisor.memberships m ON (m.tenant_id,m.site_id,m.issuer,m.subject)=(d.tenant_id,d.site_id,d.reviewer_issuer,d.reviewer_subject)
 WHERE d.tenant_id=$1 AND d.site_id=$2 AND d.proposal_id=$3 AND d.proposal_revision=$4 AND d.decision='approve' ORDER BY d.reviewer_issuer,d.reviewer_subject`, sc.TenantID, sc.SiteID, proposalID, revision)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var reviewers []Reviewer
	var digests []string
	for rows.Next() {
		var r Reviewer
		var digest string
		if err = rows.Scan(&r.Actor.Issuer, &r.Actor.Subject, &digest, &r.HasProposalApprove); err != nil {
			return nil, nil, err
		}
		// Only humans can record a decision: Decide requires a browser session, and
		// approval_decisions.auth_time is NOT NULL (migration 044), so a decision row always
		// carries a human sign-in. A decided_via column would make this explicit (WIS-277 review).
		r.Kind = KindHuman
		reviewers = append(reviewers, r)
		digests = append(digests, digest)
	}
	return reviewers, digests, rows.Err()
}

// Approvers returns the actors whose approvals the executor must re-check, so it can lock their
// memberships in ID order before the case (global lock order).
func Approvers(ctx context.Context, tx *sql.Tx, tenantID, siteID, proposalID string) ([]ActorRef, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT d.reviewer_issuer,d.reviewer_subject FROM supervisor.approval_decisions d JOIN supervisor.approval_states s ON (s.tenant_id,s.site_id,s.proposal_id)=(d.tenant_id,d.site_id,d.proposal_id)
 WHERE d.tenant_id=$1 AND d.site_id=$2 AND d.proposal_id=$3 AND d.proposal_revision=s.current_revision AND d.decision='approve' ORDER BY 1,2`, tenantID, siteID, proposalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActorRef
	for rows.Next() {
		var a ActorRef
		if err = rows.Scan(&a.Issuer, &a.Subject); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// storedPayload decodes the immutable canonical payload of a proposal revision and returns it
// with the digest it re-encodes to. A payload that does not decode or re-validate is an
// integrity failure, never a client error.
func storedPayload(ctx context.Context, tx *sql.Tx, sc Scope, proposalID string, revision int64) (Payload, string, error) {
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT canonical_payload FROM supervisor.proposal_revisions WHERE tenant_id=$1 AND site_id=$2 AND proposal_id=$3 AND revision=$4`, sc.TenantID, sc.SiteID, proposalID, revision).Scan(&raw); err != nil {
		return Payload{}, "", err
	}
	var env struct {
		TenantID string           `json:"tenant_id"`
		SiteID   string           `json:"site_id"`
		Body     canonicalPayload `json:"body"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil || env.TenantID != sc.TenantID || env.SiteID != sc.SiteID {
		return Payload{}, "", errIntegrity
	}
	b := env.Body
	parse := func(v string) (time.Time, error) { return time.Parse(timeLayout, v) }
	due, e1 := parse(b.DueAt)
	issued, e2 := parse(b.IssuedAt)
	expires, e3 := parse(b.ExpiresAt)
	if e1 != nil || e2 != nil || e3 != nil {
		return Payload{}, "", errIntegrity
	}
	p := Payload{ActionKind: b.ActionKind, Template: b.Template, ProposalID: b.ProposalID, ProposalRevision: b.ProposalRevision,
		AssetID: b.AssetID, LineID: b.LineID, OrderID: b.OrderID, AssigneeID: b.AssigneeID, Instructions: b.Instructions, DueAt: due,
		CaseID: b.CaseID, CaseRevision: b.CaseRevision, ContextSnapshotID: b.ContextSnapshotID, ContextRevision: b.ContextRevision,
		ContextDigest: b.ContextDigest, ConfigRevision: b.ConfigRevision, GraphRevision: b.GraphRevision, PolicyRevision: b.PolicyRevision,
		Prerequisites: b.Prerequisites, Evidence: b.Evidence, IssuedAt: issued, ExpiresAt: expires, GeneratedByRun: b.GeneratedByRun}
	ttl := expires.Sub(issued)
	digest, err := Digest(sc.TenantID, sc.SiteID, p, WindowProfile{TTL: ttl, Commissioned: ttl > ApprovalTTL})
	if err != nil {
		return Payload{}, "", errIntegrity
	}
	return p, digest, nil
}
