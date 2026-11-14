package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrReviewConflict           = errors.New("claim revision already reviewed")
	ErrReviewBlocked            = errors.New("claim approval blocked")
	ErrReauthenticationRequired = errors.New("recent authentication required")
)

type ReviewInput struct {
	ClaimRevision int64  `json:"claim_revision"`
	Verdict       string `json:"verdict"`
	Reason        string `json:"reason"`
}
type Review struct {
	SchemaVersion           int               `json:"schema_version"`
	ReviewID                string            `json:"review_id"`
	ClaimID                 string            `json:"claim_id"`
	ClaimRevision           int64             `json:"claim_revision"`
	Verdict                 string            `json:"verdict"`
	Reason                  string            `json:"reason"`
	ActorIssuer             string            `json:"actor_issuer"`
	ActorSubject            string            `json:"actor_subject"`
	AuthenticatedAt         time.Time         `json:"authenticated_at"`
	CreatedAt               time.Time         `json:"created_at"`
	ApprovalEligible        bool              `json:"approval_eligible"`
	AuthorityDecisionID     string            `json:"authority_decision_id,omitempty"`
	AuthoritySourceID       string            `json:"authority_source_id,omitempty"`
	AuthoritySourceRevision int64             `json:"authority_source_revision,omitempty"`
	AuthorityRevision       int64             `json:"authority_revision,omitempty"`
	Revocation              *ReviewRevocation `json:"revocation,omitempty"`
	ResolutionRefs          []ResolutionRef   `json:"resolution_refs"`
}

func (s *Service) reviewAuthentication(ctx context.Context, tx *sql.Tx, a Actor) (time.Time, error) {
	if err := s.checkSession(ctx, tx, a); err != nil {
		return time.Time{}, err
	}
	var auth sql.NullTime
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT auth_time,clock_timestamp() FROM supervisor.browser_sessions WHERE token_hash=$1 FOR SHARE`, a.SessionHash).Scan(&auth, &now); err != nil {
		return time.Time{}, err
	}
	if !auth.Valid || auth.Time.After(now) || auth.Time.Before(now.Add(-s.sessions.RecentAuth)) {
		return time.Time{}, ErrReauthenticationRequired
	}
	return auth.Time, nil
}
func (s *Service) finishReview(ctx context.Context, tx *sql.Tx, a Actor) error {
	if _, err := s.reviewAuthentication(ctx, tx, a); err != nil {
		return err
	}
	return s.finish(ctx, tx, a)
}
func (s *Service) requireReviewer(ctx context.Context, tx *sql.Tx, a Actor, sc Scope) (time.Time, error) {
	var role string
	if err := tx.QueryRowContext(ctx, `SELECT role FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND active FOR SHARE`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject).Scan(&role); err != nil {
		return time.Time{}, err
	}
	if role != "owner" && role != "supervisor" {
		return time.Time{}, ErrDenied
	}
	return s.reviewAuthentication(ctx, tx, a)
}
func loadReview(ctx context.Context, tx *sql.Tx, sc Scope, id string, revision int64) (Review, error) {
	var out Review
	var decision, source sql.NullString
	var sourceRevision, authorityRevision sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT schema_version,review_id,claim_id,claim_revision,verdict,reason,actor_issuer,actor_subject,auth_time,created_at,authority_decision_id,authority_source_id,authority_source_revision,authority_revision FROM supervisor.knowledge_claim_reviews WHERE tenant_id=$1 AND site_id=$2 AND claim_id=$3 AND claim_revision=$4`, sc.TenantID, sc.SiteID, id, revision).Scan(&out.SchemaVersion, &out.ReviewID, &out.ClaimID, &out.ClaimRevision, &out.Verdict, &out.Reason, &out.ActorIssuer, &out.ActorSubject, &out.AuthenticatedAt, &out.CreatedAt, &decision, &source, &sourceRevision, &authorityRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	out.AuthorityDecisionID = decision.String
	out.AuthoritySourceID = source.String
	out.AuthoritySourceRevision = sourceRevision.Int64
	out.AuthorityRevision = authorityRevision.Int64
	out.Revocation, err = loadReviewRevocation(ctx, tx, sc, out.ReviewID)
	if err != nil {
		return out, err
	}
	out.ResolutionRefs, err = loadReviewResolutionRefs(ctx, tx, sc, out.ReviewID)
	return out, err
}
func reviewClaimState(ctx context.Context, tx *sql.Tx, sc Scope, id string, revision int64) (Claim, bool, error) {
	claim, err := loadClaim(ctx, tx, sc, id, revision)
	if err != nil {
		return claim, false, err
	}
	refs, _, _, err := claimRefs(ctx, tx, sc, claim.ClaimInput)
	if err != nil {
		return claim, false, err
	}
	states, err := lockSources(ctx, tx, sc, refs)
	if err != nil {
		return claim, false, err
	}
	current, err := eligible(refs, states, false)
	if err != nil {
		return claim, false, err
	}
	entities, err := lockEntities(ctx, tx, sc, []string{claim.SubjectID, claim.ObjectEntityID})
	if err != nil {
		return claim, false, err
	}
	claim.CurrentRevision, err = head(ctx, tx, sc, id, true)
	if err != nil {
		return claim, false, err
	}
	return claim, current && entityPinsCurrent(claim.ClaimInput, entities) && claim.Revision == claim.CurrentRevision, nil
}

// Only a directly cited, currently designated exact source can support approval.
// Supplying decision pins on reads prevents withdrawal/redesignation from reviving an old review.
func reviewAuthority(ctx context.Context, tx *sql.Tx, sc Scope, id string, revision int64, decision string) (Review, bool, error) {
	var pin Review
	var exact any
	if decision != "" {
		exact = decision
	}
	err := tx.QueryRowContext(ctx, `SELECT d.decision_id,d.source_id,d.source_revision,d.revision
 FROM supervisor.knowledge_claim_spans s
 JOIN supervisor.evidence_heads h ON (h.tenant_id,h.site_id,h.source_id,h.current_revision)=(s.tenant_id,s.site_id,s.source_id,s.source_revision)
 JOIN supervisor.evidence_revisions e ON (e.tenant_id,e.site_id,e.source_id,e.revision)=(s.tenant_id,s.site_id,s.source_id,s.source_revision)
 JOIN supervisor.evidence_authority_heads ah ON (ah.tenant_id,ah.site_id,ah.source_id)=(s.tenant_id,s.site_id,s.source_id)
 JOIN supervisor.evidence_authority_decisions d ON (d.tenant_id,d.site_id,d.source_id,d.revision,d.source_revision,d.source_sha256,d.source_access_epoch)=(ah.tenant_id,ah.site_id,ah.source_id,ah.current_revision,e.revision,e.sha256,h.access_epoch)
 WHERE s.tenant_id=$1 AND s.site_id=$2 AND s.claim_id=$3 AND s.claim_revision=$4 AND d.verdict='designate' AND NOT h.revoked
 AND (e.extraction IS NULL OR d.model_extracted_confirmed)
 AND NOT EXISTS(SELECT 1 FROM supervisor.evidence_revocations rv WHERE (rv.tenant_id,rv.site_id,rv.source_id)=(h.tenant_id,h.site_id,h.source_id))
 AND ($5::uuid IS NULL OR d.decision_id=$5) ORDER BY d.source_id,d.revision,d.decision_id LIMIT 1`, sc.TenantID, sc.SiteID, id, revision, exact).Scan(&pin.AuthorityDecisionID, &pin.AuthoritySourceID, &pin.AuthoritySourceRevision, &pin.AuthorityRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return pin, false, nil
	}
	return pin, err == nil, err
}
func reviewEligibility(ctx context.Context, tx *sql.Tx, sc Scope, out Review, current bool) (bool, error) {
	if !current || out.Verdict != "approve" || out.Revocation != nil {
		return false, nil
	}
	_, resolved, err := reviewResolutionPins(ctx, tx, sc, out.ClaimID, out.ClaimRevision, out.ResolutionRefs, true)
	if err != nil || !resolved {
		return false, err
	}
	_, found, err := reviewAuthority(ctx, tx, sc, out.ClaimID, out.ClaimRevision, out.AuthorityDecisionID)
	return found, err
}
func (s *Service) DecideReview(ctx context.Context, a Actor, sc Scope, id string, in ReviewInput, key string, expected int64) (Review, error) {
	var out Review
	if !contractUUID(id) || !mutation(key, expected) || expected <= 0 || in.ClaimRevision <= 0 || !contractText(in.Reason, decisionReasonChars) || strings.TrimSpace(in.Reason) == "" || (in.Verdict != "approve" && in.Verdict != "reject") {
		return out, ErrInvalidContract
	}
	if in.ClaimRevision != expected {
		return out, ErrRevision
	}
	tx, err := s.beginLocked(ctx, a, sc, true, true)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	auth, err := s.requireReviewer(ctx, tx, a, sc)
	if err != nil {
		return out, err
	}
	out, err = s.decideReviewTx(ctx, tx, a, sc, id, in, key, expected, auth)
	if err != nil {
		return out, err
	}
	return out, s.finishReview(ctx, tx, a)
}

// decideReviewTx preserves the canonical review checks inside a caller's authorized transaction.
func (s *Service) decideReviewTx(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, id string, in ReviewInput, key string, expected int64, auth time.Time) (Review, error) {
	var out Review
	claim, current, err := reviewClaimState(ctx, tx, sc, id, in.ClaimRevision)
	if err != nil {
		return out, err
	}
	digest := fingerprint("review", struct {
		ClaimID string
		Input   ReviewInput
	}{id, in}, expected)
	prior, ok, err := replay(ctx, tx, a, sc, key, digest)
	if err != nil {
		return out, err
	}
	if ok {
		out, err = loadReview(ctx, tx, sc, id, prior)
		if err != nil {
			return out, err
		}
		out.ApprovalEligible, err = reviewEligibility(ctx, tx, sc, out, current)
		if err != nil {
			return out, err
		}
		if err = audit(ctx, tx, a, sc, id, prior, "mutation.replayed"); err != nil {
			return out, err
		}
		return out, nil
	}
	if _, err = loadReview(ctx, tx, sc, id, in.ClaimRevision); err == nil {
		return out, ErrReviewConflict
	} else if !errors.Is(err, ErrNotFound) {
		return out, err
	}
	if claim.CurrentRevision != expected {
		return out, ErrRevision
	}
	if !current {
		return out, ErrSourceChanged
	}
	var pin Review
	var resolutionRefs []ResolutionRef
	if in.Verdict == "approve" {
		var resolved bool
		var e error
		resolutionRefs, resolved, e = reviewResolutionPins(ctx, tx, sc, id, in.ClaimRevision, nil, false)
		if e != nil {
			return out, e
		}
		if !resolved {
			return out, ErrReviewBlocked
		}
		var found bool
		pin, found, err = reviewAuthority(ctx, tx, sc, id, in.ClaimRevision, "")
		if err != nil {
			return out, err
		}
		if !found {
			return out, ErrReviewBlocked
		}
	}
	var decision, source, sourceRevision, authorityRevision any
	if pin.AuthorityDecisionID != "" {
		decision = pin.AuthorityDecisionID
		source = pin.AuthoritySourceID
		sourceRevision = pin.AuthoritySourceRevision
		authorityRevision = pin.AuthorityRevision
	}
	reviewID := uuid.NewString()
	_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_claim_reviews(tenant_id,site_id,review_id,claim_id,claim_revision,verdict,reason,actor_issuer,actor_subject,auth_time,authority_decision_id,authority_source_id,authority_source_revision,authority_revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, sc.TenantID, sc.SiteID, reviewID, id, in.ClaimRevision, in.Verdict, in.Reason, a.Issuer, a.Subject, auth, decision, source, sourceRevision, authorityRevision)
	if err != nil {
		return out, err
	}
	for _, ref := range resolutionRefs {
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_review_resolution_refs(tenant_id,site_id,review_id,claim_id,claim_revision,conflict_id,resolution_id,resolution_revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, sc.TenantID, sc.SiteID, reviewID, id, in.ClaimRevision, ref.ConflictID, ref.ResolutionID, ref.Revision); err != nil {
			return out, err
		}
	}
	if err = saveReceipt(ctx, tx, a, sc, key, digest, "review", id, in.ClaimRevision); err != nil {
		return out, err
	}
	if err = audit(ctx, tx, a, sc, id, in.ClaimRevision, "review.decided"); err != nil {
		return out, err
	}
	out, err = loadReview(ctx, tx, sc, id, in.ClaimRevision)
	if err != nil {
		return out, err
	}
	out.ApprovalEligible = in.Verdict == "approve"
	return out, nil
}
func (s *Service) ReadReview(ctx context.Context, a Actor, sc Scope, id string, revision int64) (Review, error) {
	var out Review
	if !contractUUID(id) || revision < 0 {
		return out, ErrInvalidContract
	}
	// ponytail: serialize review reads per site to prevent conflict phantoms; replace
	// with revisioned site snapshots only if measured review-read contention warrants it.
	tx, err := s.beginLocked(ctx, a, sc, false, true)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	claim, current, err := reviewClaimState(ctx, tx, sc, id, revision)
	if err != nil {
		return out, err
	}
	out, err = loadReview(ctx, tx, sc, id, claim.Revision)
	if err != nil {
		return out, err
	}
	out.ApprovalEligible, err = reviewEligibility(ctx, tx, sc, out, current)
	if err != nil {
		return out, err
	}
	if err = audit(ctx, tx, a, sc, id, claim.Revision, "review.read"); err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}
