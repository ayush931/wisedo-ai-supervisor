package evidence

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

var ErrSourceChanged = errors.New("source changed before authority decision")

// ErrModelExtractedUnconfirmed: designating a revision whose text a model extracted (WIS-625 OCR)
// needs the owner's explicit confirmation that they checked it against the original.
var ErrModelExtractedUnconfirmed = errors.New("MODEL_EXTRACTED_UNCONFIRMED")

type AuthorityInput struct {
	SourceRevision              int64  `json:"source_revision"`
	ExpectedSourceStateRevision int64  `json:"expected_source_state_revision"`
	Verdict                     string `json:"verdict"`
	Reason                      string `json:"reason"`
	// ConfirmModelExtracted (WIS-625) is the owner's statement that they checked a model-extracted
	// revision against the original page. Designating such a revision requires it; it is refused
	// on any other source or verdict, so it can never be sent by habit.
	ConfirmModelExtracted bool `json:"confirm_model_extracted,omitempty"`
}
type Authority struct {
	DecisionID             string    `json:"decision_id"`
	SourceID               string    `json:"source_id"`
	Revision               int64     `json:"revision"`
	CurrentRevision        int64     `json:"current_revision"`
	SourceRevision         int64     `json:"source_revision"`
	SourceSHA256           string    `json:"source_sha256"`
	SourceAccessEpoch      int64     `json:"source_access_epoch"`
	Verdict                string    `json:"verdict"`
	Reason                 string    `json:"reason"`
	ActorIssuer            string    `json:"actor_issuer"`
	ActorSubject           string    `json:"actor_subject"`
	AuthenticatedAt        time.Time `json:"authenticated_at"`
	CreatedAt              time.Time `json:"created_at"`
	ReviewEvidenceEligible bool      `json:"review_evidence_eligible"`
	// SourceModelExtracted says the decided revision's text was extracted by a model (WIS-625).
	SourceModelExtracted bool `json:"source_model_extracted"`
}

func authorityHead(ctx context.Context, tx *sql.Tx, sc Scope, id string) (int64, error) {
	var revision int64
	err := tx.QueryRowContext(ctx, `SELECT current_revision FROM supervisor.evidence_authority_heads WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 FOR SHARE`, sc.TenantID, sc.SiteID, id).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return revision, err
}
func readAuthority(ctx context.Context, tx *sql.Tx, sc Scope, id string, revision, current int64, h head) (Authority, error) {
	var out Authority
	if h.revoked {
		return out, ErrNotFound
	}
	if revision == 0 {
		revision = current
	}
	err := tx.QueryRowContext(ctx, `SELECT decision_id,source_id,revision,source_revision,source_sha256,source_access_epoch,verdict,reason,actor_issuer,actor_subject,auth_time,created_at FROM supervisor.evidence_authority_decisions WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND revision=$4`, sc.TenantID, sc.SiteID, id, revision).Scan(&out.DecisionID, &out.SourceID, &out.Revision, &out.SourceRevision, &out.SourceSHA256, &out.SourceAccessEpoch, &out.Verdict, &out.Reason, &out.ActorIssuer, &out.ActorSubject, &out.AuthenticatedAt, &out.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	var digest string
	if err = tx.QueryRowContext(ctx, `SELECT sha256 FROM supervisor.evidence_revisions WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND revision=$4`, sc.TenantID, sc.SiteID, id, h.revision).Scan(&digest); err != nil {
		return out, err
	}
	var extracted, confirmed bool
	if err = tx.QueryRowContext(ctx, `SELECT r.extraction IS NOT NULL,d.model_extracted_confirmed FROM supervisor.evidence_revisions r
 JOIN supervisor.evidence_authority_decisions d ON (d.tenant_id,d.site_id,d.source_id,d.source_revision)=(r.tenant_id,r.site_id,r.source_id,r.revision)
 WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.source_id=$3 AND r.revision=$4 AND d.revision=$5`, sc.TenantID, sc.SiteID, id, out.SourceRevision, out.Revision).Scan(&extracted, &confirmed); err != nil {
		return out, err
	}
	out.SourceModelExtracted = extracted
	out.CurrentRevision = current
	// A designation of model-extracted text made without the owner's confirmation (before the
	// WIS-625 rule) is not eligible: fail closed until the owner designates it again, confirmed.
	out.ReviewEvidenceEligible = out.Verdict == "designate" && out.Revision == current && out.SourceRevision == h.revision && out.SourceSHA256 == digest && out.SourceAccessEpoch == h.epoch && (!extracted || confirmed)
	return out, nil
}
func (s *Service) DecideAuthority(ctx context.Context, a Actor, sc Scope, id string, in AuthorityInput, key string, expected int64) (Authority, error) {
	var out Authority
	if !identifier(id) || !mutation(key, expected) || in.SourceRevision <= 0 || in.ExpectedSourceStateRevision <= 0 || !text(in.Reason, 1000) || strings.TrimSpace(in.Reason) == "" || (in.Verdict != "designate" && in.Verdict != "withdraw") {
		return out, ErrInvalid
	}
	tx, err := s.begin(ctx, a, sc, true, true)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	h, err := getHead(ctx, tx, sc, id, true)
	if err != nil {
		return out, err
	}
	if h.revoked {
		return out, ErrNotFound
	}
	current, err := authorityHead(ctx, tx, sc, id)
	if err != nil {
		return out, err
	}
	digest := fingerprint("authority", struct {
		SourceID string
		Input    AuthorityInput
	}{id, in}, expected)
	prior, ok, err := replay(ctx, tx, a, sc, key, digest)
	if err != nil {
		return out, err
	}
	if ok {
		out, err = readAuthority(ctx, tx, sc, id, prior.AuthorityRevision, current, h)
		if err != nil {
			return out, err
		}
		if err = audit(ctx, tx, a, sc, id, out.SourceRevision, "mutation.replayed", nil); err != nil {
			return out, err
		}
		return out, s.finish(ctx, tx, a, true)
	}
	if h.revision != in.SourceRevision || h.state != in.ExpectedSourceStateRevision {
		return out, ErrSourceChanged
	}
	if current != expected {
		return out, ErrRevision
	}
	if in.Verdict == "withdraw" {
		if current == 0 {
			return out, ErrInvalid
		}
		previous, e := readAuthority(ctx, tx, sc, id, current, current, h)
		if e != nil {
			return out, e
		}
		if previous.Verdict != "designate" {
			return out, ErrInvalid
		}
	}
	source, err := readSource(ctx, tx, sc, id, h.revision, h)
	if err != nil {
		return out, err
	}
	// WIS-625 lower authority: a model-extracted revision is designated only with the owner's
	// explicit confirmation, and the confirmation means nothing anywhere else.
	switch {
	case in.ConfirmModelExtracted && (in.Verdict != "designate" || source.Extraction == nil):
		return out, ErrInvalid
	case in.Verdict == "designate" && source.Extraction != nil && !in.ConfirmModelExtracted:
		return out, ErrModelExtractedUnconfirmed
	}
	revision := current + 1
	if current == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.evidence_authority_heads(tenant_id,site_id,source_id,current_revision) VALUES($1,$2,$3,$4)`, sc.TenantID, sc.SiteID, id, revision)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE supervisor.evidence_authority_heads SET current_revision=$4 WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, sc.TenantID, sc.SiteID, id, revision)
	}
	if err != nil {
		return out, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.evidence_authority_decisions(tenant_id,site_id,source_id,revision,decision_id,source_revision,source_sha256,source_access_epoch,verdict,reason,actor_issuer,actor_subject,auth_time,model_extracted_confirmed) SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,auth_time,$14 FROM supervisor.browser_sessions WHERE token_hash=$13`, sc.TenantID, sc.SiteID, id, revision, uuid.NewString(), h.revision, source.SHA256, h.epoch, in.Verdict, in.Reason, a.Issuer, a.Subject, a.SessionHash, in.ConfirmModelExtracted)
	if err != nil {
		return out, err
	}
	if err = saveReceipt(ctx, tx, a, sc, id, key, digest, receipt{AuthorityRevision: revision}); err != nil {
		return out, err
	}
	if err = audit(ctx, tx, a, sc, id, h.revision, "authority.decided", nil); err != nil {
		return out, err
	}
	out, err = readAuthority(ctx, tx, sc, id, revision, revision, h)
	if err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a, true)
}
func (s *Service) ReadAuthority(ctx context.Context, a Actor, sc Scope, id string, revision int64) (Authority, error) {
	var out Authority
	if !identifier(id) || revision < 0 {
		return out, ErrInvalid
	}
	tx, err := s.begin(ctx, a, sc, false, false)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	h, err := getHead(ctx, tx, sc, id, false)
	if err != nil {
		return out, err
	}
	if h.revoked {
		return out, ErrNotFound
	}
	current, err := authorityHead(ctx, tx, sc, id)
	if err != nil {
		return out, err
	}
	out, err = readAuthority(ctx, tx, sc, id, revision, current, h)
	if err != nil {
		return out, err
	}
	if err = audit(ctx, tx, a, sc, id, out.SourceRevision, "authority.read", nil); err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a, false)
}
