package oeeservice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/oee"
	"github.com/google/uuid"
)

// CreateRevision appends an immutable policy revision (ADR 0002 Sec. 6.2 oee_policy_revision).
// The first revision creates the policy identity with its policy code. Revisions are never
// edited; publication is a separate, supervisor-only step.
//
// Concurrency: begin takes the site write lock, so revision numbers and policy codes are
// allocated without races between writers at the same site.
func (s *Service) CreateRevision(ctx context.Context, a Actor, sc Scope, policyID string, in oee.OeePolicyRevisionInputV1, key, correlation string) (oee.OeePolicyV1, error) {
	var out oee.OeePolicyV1
	if !validID(policyID) || !validText(in.PolicyCode, 128) {
		return out, ErrInvalid
	}
	start, err := time.Parse(time.RFC3339Nano, in.EffectiveStart)
	if err != nil {
		return out, ErrInvalid
	}
	var end *time.Time
	if in.EffectiveEnd != "" {
		e, err := time.Parse(time.RFC3339Nano, in.EffectiveEnd)
		if err != nil || !e.After(start) {
			return out, ErrInvalid
		}
		e = e.UTC()
		end = &e
	}
	// Validate every rule before any side effect. The identifier passed here only labels the
	// kernel policy during validation; the stored revision gets its own ID below.
	if _, _, err = in.Policy.Policies(uuid.MustParse(policyID)); err != nil {
		return out, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	body, digest, err := in.Policy.Canonical(sc.TenantID, sc.SiteID)
	if err != nil {
		return out, ErrInvalid
	}
	policyJSON, err := json.Marshal(body)
	if err != nil {
		return out, ErrInvalid
	}
	in.Policy = body
	hash, err := requestDigest(sc, "OeePolicyRevisionRequestV1", struct {
		PolicyID string                       `json:"policy_id"`
		Input    oee.OeePolicyRevisionInputV1 `json:"input"`
	}{policyID, in})
	if err != nil {
		return out, ErrInvalid
	}
	tx, err := s.begin(ctx, a, sc, author)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if ref, ok, err := s.claimKey(ctx, tx, a, sc, "policy.revise", key, hash); err != nil {
		return out, err
	} else if ok {
		return s.replayPolicy(ctx, tx, a, sc, ref, correlation)
	}
	var code string
	err = tx.QueryRowContext(ctx, `SELECT policy_code FROM supervisor.oee_policy WHERE tenant_id=$1 AND site_id=$2 AND policy_id=$3 FOR UPDATE`, sc.TenantID, sc.SiteID, policyID).Scan(&code)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		var taken bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.oee_policy WHERE tenant_id=$1 AND site_id=$2 AND policy_code=$3)`, sc.TenantID, sc.SiteID, in.PolicyCode).Scan(&taken); err != nil {
			return out, err
		}
		if taken {
			return out, ErrPolicyConflict
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.oee_policy(tenant_id,site_id,policy_id,policy_code,created_by_issuer,created_by_subject) VALUES($1,$2,$3,$4,$5,$6)`, sc.TenantID, sc.SiteID, policyID, in.PolicyCode, a.Issuer, a.Subject); err != nil {
			return out, err
		}
	case err != nil:
		return out, err
	case code != in.PolicyCode:
		return out, ErrPolicyConflict
	}
	// ADR 0002 Sec. 6.2: UNIQUE(policy_id, policy_sha256). An identical document is not a new
	// revision.
	var duplicate bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.oee_policy_revision WHERE tenant_id=$1 AND site_id=$2 AND policy_id=$3 AND policy_sha256=$4)`, sc.TenantID, sc.SiteID, policyID, digest).Scan(&duplicate); err != nil {
		return out, err
	}
	if duplicate {
		return out, ErrPolicyConflict
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.oee_policy_revision(tenant_id,site_id,policy_revision_id,policy_id,revision_no,schema_version,effective_start,effective_end,policy_json,policy_sha256,created_by_issuer,created_by_subject)
SELECT $1,$2,$3,$4,COALESCE(MAX(revision_no),0)+1,1,$5,$6,$7,$8,$9,$10 FROM supervisor.oee_policy_revision WHERE tenant_id=$1 AND site_id=$2 AND policy_id=$4`,
		sc.TenantID, sc.SiteID, uuid.NewString(), policyID, start.UTC(), end, policyJSON, digest, a.Issuer, a.Subject); err != nil {
		return out, err
	}
	if err = audit(ctx, tx, sc, auditEntry{a.Issuer, a.Subject, "policy.revised", "policy", policyID, "", digest, "", correlation}); err != nil {
		return out, err
	}
	if err = completeKey(ctx, tx, a, sc, "policy.revise", key, 201, keyResponse{PolicyID: policyID}); err != nil {
		return out, err
	}
	if out, err = loadPolicy(ctx, tx, sc, policyID); err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}

// replayPolicy answers a replayed policy mutation with the policy's current state.
func (s *Service) replayPolicy(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, ref keyResponse, correlation string) (oee.OeePolicyV1, error) {
	if !validID(ref.PolicyID) {
		return oee.OeePolicyV1{}, errIntegrity
	}
	if err := audit(ctx, tx, sc, auditEntry{a.Issuer, a.Subject, "mutation.replayed", "policy", ref.PolicyID, "", "", "", correlation}); err != nil {
		return oee.OeePolicyV1{}, err
	}
	out, err := loadPolicy(ctx, tx, sc, ref.PolicyID)
	if err != nil {
		return out, err
	}
	out.Replayed = true
	return out, s.finish(ctx, tx, a)
}

// Publish points the policy at one of its revisions. It moves the pointer under optimistic
// concurrency: expectedRowVersion is the If-Match token, and a stale one is ErrRevision. The
// site write lock taken in begin serializes publications, so the overlap check below cannot
// race another publication at the same site.
func (s *Service) Publish(ctx context.Context, a Actor, sc Scope, policyID, revisionID string, expectedRowVersion int64, key, correlation string) (oee.OeePolicyV1, error) {
	var out oee.OeePolicyV1
	if !validID(policyID) || !validID(revisionID) {
		return out, ErrInvalid
	}
	hash, err := requestDigest(sc, "OeePolicyPublishRequestV1", struct {
		PolicyID         string `json:"policy_id"`
		PolicyRevisionID string `json:"policy_revision_id"`
		RowVersion       int64  `json:"row_version"`
	}{policyID, revisionID, expectedRowVersion})
	if err != nil {
		return out, ErrInvalid
	}
	tx, err := s.begin(ctx, a, sc, publish)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if ref, ok, err := s.claimKey(ctx, tx, a, sc, "policy.publish", key, hash); err != nil {
		return out, err
	} else if ok {
		return s.replayPolicy(ctx, tx, a, sc, ref, correlation)
	}
	var rowVersion int64
	var current sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT row_version,current_publication_id FROM supervisor.oee_policy WHERE tenant_id=$1 AND site_id=$2 AND policy_id=$3 FOR UPDATE`, sc.TenantID, sc.SiteID, policyID).Scan(&rowVersion, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if rowVersion != expectedRowVersion {
		return out, ErrRevision
	}
	var start time.Time
	var end sql.NullTime
	var digest string
	err = tx.QueryRowContext(ctx, `SELECT effective_start,effective_end,policy_sha256 FROM supervisor.oee_policy_revision WHERE tenant_id=$1 AND site_id=$2 AND policy_id=$3 AND policy_revision_id=$4`, sc.TenantID, sc.SiteID, policyID, revisionID).Scan(&start, &end, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	var overlap bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.oee_policy p
 JOIN supervisor.oee_policy_publication pub ON (pub.tenant_id,pub.site_id,pub.policy_id,pub.publication_id)=(p.tenant_id,p.site_id,p.policy_id,p.current_publication_id)
 JOIN supervisor.oee_policy_revision r ON (r.tenant_id,r.site_id,r.policy_id,r.policy_revision_id)=(pub.tenant_id,pub.site_id,pub.policy_id,pub.policy_revision_id)
 WHERE p.tenant_id=$1 AND p.site_id=$2 AND p.policy_id<>$3 AND pub.action IN ('PUBLISH','ROLLBACK')
   AND tstzrange(r.effective_start,r.effective_end,'[)') && tstzrange($4,$5,'[)'))`, sc.TenantID, sc.SiteID, policyID, start, end).Scan(&overlap); err != nil {
		return out, err
	}
	if overlap {
		return out, ErrPolicyOverlap
	}
	publicationID := uuid.NewString()
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.oee_policy_publication(tenant_id,site_id,publication_id,policy_id,policy_revision_id,publication_no,action,previous_publication_id,published_by_issuer,published_by_subject)
SELECT $1,$2,$3,$4,$5,COALESCE(MAX(publication_no),0)+1,'PUBLISH',NULLIF($6,'')::uuid,$7,$8 FROM supervisor.oee_policy_publication WHERE tenant_id=$1 AND site_id=$2 AND policy_id=$4`,
		sc.TenantID, sc.SiteID, publicationID, policyID, revisionID, current.String, a.Issuer, a.Subject); err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE supervisor.oee_policy SET current_publication_id=$4,row_version=row_version+1 WHERE tenant_id=$1 AND site_id=$2 AND policy_id=$3`, sc.TenantID, sc.SiteID, policyID, publicationID); err != nil {
		return out, err
	}
	var before string
	if current.Valid {
		if err = tx.QueryRowContext(ctx, `SELECT r.policy_sha256 FROM supervisor.oee_policy_publication pub JOIN supervisor.oee_policy_revision r ON (r.tenant_id,r.site_id,r.policy_id,r.policy_revision_id)=(pub.tenant_id,pub.site_id,pub.policy_id,pub.policy_revision_id) WHERE pub.tenant_id=$1 AND pub.site_id=$2 AND pub.publication_id=$3`, sc.TenantID, sc.SiteID, current.String).Scan(&before); err != nil {
			return out, err
		}
	}
	if err = audit(ctx, tx, sc, auditEntry{a.Issuer, a.Subject, "policy.published", "policy", policyID, before, digest, "", correlation}); err != nil {
		return out, err
	}
	if err = completeKey(ctx, tx, a, sc, "policy.publish", key, 200, keyResponse{PolicyID: policyID}); err != nil {
		return out, err
	}
	if out, err = loadPolicy(ctx, tx, sc, policyID); err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}

func (s *Service) GetPolicy(ctx context.Context, a Actor, sc Scope, policyID string) (oee.OeePolicyV1, error) {
	if !validID(policyID) {
		return oee.OeePolicyV1{}, ErrInvalid
	}
	tx, err := s.begin(ctx, a, sc, read)
	if err != nil {
		return oee.OeePolicyV1{}, err
	}
	defer tx.Rollback()
	out, err := loadPolicy(ctx, tx, sc, policyID)
	if err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}

func loadPolicy(ctx context.Context, tx *sql.Tx, sc Scope, policyID string) (oee.OeePolicyV1, error) {
	out := oee.OeePolicyV1{PolicyID: policyID, Revisions: []oee.OeePolicyRevisionV1{}}
	var current sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT policy_code,row_version,current_publication_id FROM supervisor.oee_policy WHERE tenant_id=$1 AND site_id=$2 AND policy_id=$3`, sc.TenantID, sc.SiteID, policyID).Scan(&out.PolicyCode, &out.RowVersion, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if current.Valid {
		var p oee.OeePolicyPublicationV1
		var at time.Time
		if err = tx.QueryRowContext(ctx, `SELECT publication_id,publication_no,action,policy_revision_id,published_at FROM supervisor.oee_policy_publication WHERE tenant_id=$1 AND site_id=$2 AND publication_id=$3`, sc.TenantID, sc.SiteID, current.String).Scan(&p.PublicationID, &p.PublicationNo, &p.Action, &p.PolicyRevisionID, &at); err != nil {
			return out, err
		}
		p.PublishedAt = wireTime(at)
		out.CurrentPublication = &p
	}
	rows, err := tx.QueryContext(ctx, `SELECT policy_revision_id,revision_no,effective_start,effective_end,policy_json,policy_sha256,created_at FROM supervisor.oee_policy_revision WHERE tenant_id=$1 AND site_id=$2 AND policy_id=$3 ORDER BY revision_no`, sc.TenantID, sc.SiteID, policyID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var r oee.OeePolicyRevisionV1
		var start, created time.Time
		var end sql.NullTime
		var body []byte
		if err = rows.Scan(&r.PolicyRevisionID, &r.RevisionNo, &start, &end, &body, &r.Digest, &created); err != nil {
			return out, err
		}
		if r.Policy, err = verifiedPolicy(sc, body, r.Digest); err != nil {
			return out, err
		}
		r.EffectiveStart, r.CreatedAt = wireTime(start), wireTime(created)
		if end.Valid {
			r.EffectiveEnd = wireTime(end.Time)
		}
		out.Revisions = append(out.Revisions, r)
	}
	return out, rows.Err()
}

// verifiedPolicy decodes a stored policy document and re-verifies its digest, so a stored row
// that no longer matches its digest is never used.
func verifiedPolicy(sc Scope, body []byte, digest string) (oee.OeePolicyBodyV1, error) {
	var p oee.OeePolicyBodyV1
	if err := json.Unmarshal(body, &p); err != nil {
		return p, errIntegrity
	}
	canonicalBody, got, err := p.Canonical(sc.TenantID, sc.SiteID)
	if err != nil || got != digest {
		return p, errIntegrity
	}
	return canonicalBody, nil
}
