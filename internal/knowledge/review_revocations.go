package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

type RevocationInput struct {
	ClaimRevision int64  `json:"claim_revision"`
	ReviewID      string `json:"review_id"`
	Reason        string `json:"reason"`
}
type ReviewRevocation struct {
	SchemaVersion   int       `json:"schema_version"`
	RevocationID    string    `json:"revocation_id"`
	ReviewID        string    `json:"review_id"`
	ClaimID         string    `json:"claim_id"`
	ClaimRevision   int64     `json:"claim_revision"`
	Reason          string    `json:"reason"`
	ActorIssuer     string    `json:"actor_issuer"`
	ActorSubject    string    `json:"actor_subject"`
	AuthenticatedAt time.Time `json:"authenticated_at"`
	CreatedAt       time.Time `json:"created_at"`
}

func loadReviewRevocation(ctx context.Context, tx *sql.Tx, sc Scope, id string) (*ReviewRevocation, error) {
	var out ReviewRevocation
	err := tx.QueryRowContext(ctx, `SELECT schema_version,revocation_id,review_id,claim_id,claim_revision,reason,actor_issuer,actor_subject,auth_time,created_at FROM supervisor.knowledge_review_revocations WHERE tenant_id=$1 AND site_id=$2 AND review_id=$3`, sc.TenantID, sc.SiteID, id).Scan(&out.SchemaVersion, &out.RevocationID, &out.ReviewID, &out.ClaimID, &out.ClaimRevision, &out.Reason, &out.ActorIssuer, &out.ActorSubject, &out.AuthenticatedAt, &out.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}
func (s *Service) RevokeReview(ctx context.Context, a Actor, sc Scope, id string, in RevocationInput, key string, expected int64) (Review, error) {
	var out Review
	if !contractUUID(id) || !contractUUID(in.ReviewID) || !mutation(key, expected) || expected <= 0 || in.ClaimRevision <= 0 || !contractText(in.Reason, decisionReasonChars) || strings.TrimSpace(in.Reason) == "" {
		return out, ErrInvalidContract
	}
	if expected != in.ClaimRevision {
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
	// Withdrawing a historical approval is allowed even after its pins become stale;
	// revocation of the underlying source still denies access to its review metadata.
	if _, _, err = reviewClaimState(ctx, tx, sc, id, in.ClaimRevision); err != nil {
		return out, err
	}
	out, err = loadReview(ctx, tx, sc, id, in.ClaimRevision)
	if err != nil {
		return out, err
	}
	digest := fingerprint("review_revoke", struct {
		ClaimID string
		Input   RevocationInput
	}{id, in}, expected)
	_, ok, err := replay(ctx, tx, a, sc, key, digest)
	if err != nil {
		return Review{}, err
	}
	if out.ReviewID != in.ReviewID {
		return Review{}, ErrReviewConflict
	}
	if out.Verdict != "approve" {
		return Review{}, ErrInvalidContract
	}
	if ok {
		if out.Revocation == nil {
			return Review{}, ErrReviewConflict
		}
		if err = audit(ctx, tx, a, sc, id, in.ClaimRevision, "mutation.replayed"); err != nil {
			return Review{}, err
		}
		return out, s.finishReview(ctx, tx, a)
	}
	if out.Revocation != nil {
		return Review{}, ErrReviewConflict
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_review_revocations(tenant_id,site_id,revocation_id,review_id,claim_id,claim_revision,reason,actor_issuer,actor_subject,auth_time) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, sc.TenantID, sc.SiteID, uuid.NewString(), in.ReviewID, id, in.ClaimRevision, in.Reason, a.Issuer, a.Subject, auth)
	if err != nil {
		return Review{}, err
	}
	if err = saveReceipt(ctx, tx, a, sc, key, digest, "review_revoke", id, in.ClaimRevision); err != nil {
		return Review{}, err
	}
	if err = audit(ctx, tx, a, sc, id, in.ClaimRevision, "review.revoked"); err != nil {
		return Review{}, err
	}
	out, err = loadReview(ctx, tx, sc, id, in.ClaimRevision)
	if err != nil {
		return Review{}, err
	}
	return out, s.finishReview(ctx, tx, a)
}
