package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/google/uuid"
)

var ErrCorrectionIncomplete = errors.New("correction needs a claim, original snapshot and regression expectation")

// ErrCorrectionSuperseded refuses approval of a correction its author has since replaced.
var ErrCorrectionSuperseded = errors.New("correction was replaced by its author")

func (s *Service) DecideMemoryReview(ctx context.Context, a Actor, sc Scope, id string, in MemoryReviewInput, key string, expected int64) (MemoryCandidate, error) {
	var out MemoryCandidate
	if !contractUUID(id) || !contractUUID(key) || expected < 1 || in.ExpectedRevision < 1 || !contractText(in.Reason, 2000) || strings.TrimSpace(in.Reason) == "" || (in.Verdict != "approve" && in.Verdict != "reject") {
		return out, ErrInvalidContract
	}
	if in.ExpectedRevision != expected {
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
	out, err = s.loadMemory(ctx, tx, a, sc, id, 0)
	if err != nil {
		return out, err
	}
	if out.ProposerIssuer == a.Issuer && out.ProposerSubject == a.Subject {
		return out, ErrSelfReview
	}
	digest := fingerprint("memory_review", struct {
		ID    string
		Input MemoryReviewInput
	}{id, in}, expected)
	prior, replayed, err := replay(ctx, tx, a, sc, key, digest)
	if err != nil {
		return out, err
	}
	if replayed {
		out, err = s.loadMemory(ctx, tx, a, sc, id, prior)
		if err != nil {
			return out, err
		}
		return out, s.finishReview(ctx, tx, a)
	}
	if out.Revision != expected {
		return out, ErrRevision
	}
	if out.Review != nil {
		return out, ErrReviewConflict
	}
	reviewID := uuid.NewString()
	var claimID, claimRevision, canonicalReviewID any
	var eval CorrectionEvaluationCase
	if in.Verdict == "approve" {
		if out.ReviewState == "superseded" {
			return out, ErrCorrectionSuperseded
		}
		if len(out.MissingRequirements) > 0 {
			return out, ErrCorrectionIncomplete
		}
		if err = validateMemoryReferences(ctx, tx, sc, out.SourceFeedbackIDs[0], out.Content); err != nil {
			return out, err
		}
		// This private operation records the original human author, without impersonating an agent.
		proposer := Actor{Issuer: out.ProposerIssuer, Subject: out.ProposerSubject}
		claim, e := s.createClaimTx(ctx, tx, proposer, sc, *out.Content.Claim, uuid.NewString(), out.Content.ExpectedBaseRevision)
		if e != nil {
			return out, e
		}
		review, e := s.decideReviewTx(ctx, tx, a, sc, claim.ClaimID, ReviewInput{ClaimRevision: claim.Revision, Verdict: "approve", Reason: in.Reason}, uuid.NewString(), claim.Revision, auth)
		if e != nil {
			return out, e
		}
		claimID, claimRevision, canonicalReviewID = claim.ClaimID, claim.Revision, review.ReviewID
		pins, e := correctionSourcePins(ctx, tx, sc, claim.ClaimID, claim.Revision)
		if e != nil {
			return out, e
		}
		eval = CorrectionEvaluationCase{CaseID: uuid.NewString(), CandidateID: id, CandidateRevision: expected, SourceFeedbackID: out.SourceFeedbackIDs[0], ReviewID: reviewID, CanonicalReviewID: review.ReviewID, Question: out.Content.Question, ExpectedAnswer: out.Content.ExpectedAnswer, ClaimID: claim.ClaimID, ClaimRevision: claim.Revision, Claim: claim.ClaimInput, SourcePins: pins, ProposerIssuer: out.ProposerIssuer, ProposerSubject: out.ProposerSubject, ReviewerIssuer: a.Issuer, ReviewerSubject: a.Subject}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_memory_candidate_reviews(tenant_id,site_id,candidate_id,revision,review_id,verdict,reason,actor_issuer,actor_subject,auth_time,claim_id,claim_revision,canonical_review_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, sc.TenantID, sc.SiteID, id, expected, reviewID, in.Verdict, in.Reason, a.Issuer, a.Subject, auth, claimID, claimRevision, canonicalReviewID)
	if err != nil {
		return out, err
	}
	if in.Verdict == "approve" {
		raw, e := json.Marshal(eval)
		if e != nil {
			return out, e
		}
		_, sha, e := canonical.Envelope("CorrectionEvaluationCaseV1", 1, sc.TenantID, sc.SiteID, eval)
		if e != nil {
			return out, e
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.correction_evaluation_cases(tenant_id,site_id,case_id,candidate_id,candidate_revision,review_id,source_feedback_id,payload,sha256) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, sc.TenantID, sc.SiteID, eval.CaseID, id, expected, reviewID, eval.SourceFeedbackID, raw, sha)
		if err != nil {
			return out, err
		}
	}
	if err = saveReceipt(ctx, tx, a, sc, key, digest, "memory_review", id, expected); err != nil {
		return out, err
	}
	if err = audit(ctx, tx, a, sc, id, expected, "memory.decided"); err != nil {
		return out, err
	}
	out, err = s.loadMemory(ctx, tx, a, sc, id, expected)
	if err != nil {
		return out, err
	}
	return out, s.finishReview(ctx, tx, a)
}

func correctionSourcePins(ctx context.Context, tx *sql.Tx, sc Scope, id string, revision int64) ([]SnapshotSource, error) {
	out := []SnapshotSource{}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT e.source_id::text,e.revision,e.sha256,h.access_epoch FROM supervisor.knowledge_claim_spans c JOIN supervisor.evidence_revisions e ON (e.tenant_id,e.site_id,e.source_id,e.revision)=(c.tenant_id,c.site_id,c.source_id,c.source_revision) JOIN supervisor.evidence_heads h ON (h.tenant_id,h.site_id,h.source_id)=(e.tenant_id,e.site_id,e.source_id) WHERE c.tenant_id=$1 AND c.site_id=$2 AND c.claim_id=$3 AND c.claim_revision=$4 ORDER BY e.source_id::text,e.revision`, sc.TenantID, sc.SiteID, id, revision)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var pin SnapshotSource
		if err = rows.Scan(&pin.SourceID, &pin.Revision, &pin.SHA256, &pin.AccessEpoch); err != nil {
			rows.Close()
			return out, err
		}
		out = append(out, pin)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

func correctionMonth(now time.Time, zone string) (time.Time, time.Time, error) {
	if zone == "" {
		zone = "UTC"
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	local := now.In(loc)
	start := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)
	return start.UTC(), start.AddDate(0, 1, 0).UTC(), nil
}

func (s *Service) CorrectionSummary(ctx context.Context, a Actor, sc Scope) (CorrectionSummary, error) {
	var out CorrectionSummary
	tx, err := s.begin(ctx, a, sc, false)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var now time.Time
	if err = tx.QueryRowContext(ctx, `SELECT coalesce(timezone,'UTC'),clock_timestamp() FROM supervisor.sites WHERE tenant_id=$1 AND id=$2`, sc.TenantID, sc.SiteID).Scan(&out.Timezone, &now); err != nil {
		return out, err
	}
	out.MonthStart, out.MonthEnd, err = correctionMonth(now, out.Timezone)
	if err != nil {
		return out, err
	}
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_memory_candidate_reviews WHERE tenant_id=$1 AND site_id=$2 AND verdict='approve' AND created_at >= $3 AND created_at < $4`, sc.TenantID, sc.SiteID, out.MonthStart, out.MonthEnd).Scan(&out.ApprovedCount)
	if err != nil {
		return out, err
	}
	if err = audit(ctx, tx, a, sc, nil, nil, "corrections.read"); err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}

func (s *Service) ListCorrectionEvaluationCases(ctx context.Context, a Actor, sc Scope, cursor string, limit int) (CorrectionEvaluationPage, error) {
	out := CorrectionEvaluationPage{Cases: []CorrectionEvaluationCase{}}
	if limit < 1 || limit > 100 || (cursor != "" && !contractUUID(cursor)) {
		return out, ErrInvalidContract
	}
	if cursor == "" {
		cursor = uuid.Nil.String()
	}
	tx, err := s.begin(ctx, a, sc, false)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var permitted bool
	if err = tx.QueryRowContext(ctx, `SELECT role IN ('owner','supervisor') FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND active`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject).Scan(&permitted); err != nil {
		return out, err
	}
	if !permitted {
		return out, ErrDenied
	}
	rows, err := tx.QueryContext(ctx, `SELECT payload,sha256,created_at FROM supervisor.correction_evaluation_cases WHERE tenant_id=$1 AND site_id=$2 AND case_id>$3::uuid ORDER BY case_id LIMIT $4`, sc.TenantID, sc.SiteID, cursor, limit+1)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var raw []byte
		var c CorrectionEvaluationCase
		var digest string
		var at time.Time
		if err = rows.Scan(&raw, &digest, &at); err != nil {
			rows.Close()
			return out, err
		}
		if err = json.Unmarshal(raw, &c); err != nil {
			rows.Close()
			return out, err
		}
		c.CaseSHA256, c.CreatedAt = digest, at
		out.Cases = append(out.Cases, c)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return out, err
	}
	if len(out.Cases) > limit {
		out.Cases = out.Cases[:limit]
		out.NextCursor = out.Cases[limit-1].CaseID
	}
	if err = audit(ctx, tx, a, sc, nil, nil, "corrections.read"); err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}
