package evidence

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

type CurrentnessInput struct {
	SourceRevision              int64  `json:"source_revision"`
	ExpectedSourceStateRevision int64  `json:"expected_source_state_revision"`
	AuthorityRevision           int64  `json:"authority_revision"`
	AuthorityDecisionID         string `json:"authority_decision_id"`
	Verdict                     string `json:"verdict"`
	Reason                      string `json:"reason"`
}
type Currentness struct {
	SchemaVersion       int        `json:"schema_version"`
	VerificationID      string     `json:"verification_id"`
	SourceID            string     `json:"source_id"`
	Revision            int64      `json:"revision"`
	CurrentRevision     int64      `json:"current_revision"`
	SourceRevision      int64      `json:"source_revision"`
	SourceSHA256        string     `json:"source_sha256"`
	SourceAccessEpoch   int64      `json:"source_access_epoch"`
	AuthorityDecisionID string     `json:"authority_decision_id"`
	AuthorityRevision   int64      `json:"authority_revision"`
	Verdict             string     `json:"verdict"`
	Reason              string     `json:"reason"`
	ActorIssuer         string     `json:"actor_issuer"`
	ActorSubject        string     `json:"actor_subject"`
	AuthenticatedAt     time.Time  `json:"authenticated_at"`
	CreatedAt           time.Time  `json:"created_at"`
	VerifiedAt          *time.Time `json:"verified_at"`
	VerificationCurrent bool       `json:"verification_current"`
}

func currentnessHead(ctx context.Context, tx *sql.Tx, sc Scope, id string) (int64, error) {
	var revision int64
	err := tx.QueryRowContext(ctx, `SELECT current_revision FROM supervisor.evidence_currentness_heads WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 FOR SHARE`, sc.TenantID, sc.SiteID, id).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return revision, err
}
func readCurrentness(ctx context.Context, tx *sql.Tx, sc Scope, id string, revision, current int64, h head) (Currentness, error) {
	var out Currentness
	if h.revoked {
		return out, ErrNotFound
	}
	if revision == 0 {
		revision = current
	}
	var verified sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT schema_version,verification_id,source_id,revision,source_revision,source_sha256,source_access_epoch,authority_decision_id,authority_revision,verdict,reason,actor_issuer,actor_subject,auth_time,created_at,verified_at FROM supervisor.evidence_currentness_decisions WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND revision=$4`, sc.TenantID, sc.SiteID, id, revision).Scan(&out.SchemaVersion, &out.VerificationID, &out.SourceID, &out.Revision, &out.SourceRevision, &out.SourceSHA256, &out.SourceAccessEpoch, &out.AuthorityDecisionID, &out.AuthorityRevision, &out.Verdict, &out.Reason, &out.ActorIssuer, &out.ActorSubject, &out.AuthenticatedAt, &out.CreatedAt, &verified)
	if errors.Is(err, sql.ErrNoRows) {
		return Currentness{}, ErrNotFound
	}
	if err != nil {
		return Currentness{}, err
	}
	if verified.Valid {
		out.VerifiedAt = &verified.Time
	}
	out.CurrentRevision = current
	// Currency is independent of age: no TTL or freshness policy is asserted here.
	if out.Verdict == "confirm_current" && out.Revision == current && out.SourceRevision == h.revision && out.SourceAccessEpoch == h.epoch {
		authorityRevision, e := authorityHead(ctx, tx, sc, id)
		if e != nil {
			return Currentness{}, e
		}
		authority, e := readAuthority(ctx, tx, sc, id, out.AuthorityRevision, authorityRevision, h)
		if e != nil {
			return Currentness{}, e
		}
		out.VerificationCurrent = authority.ReviewEvidenceEligible && authority.DecisionID == out.AuthorityDecisionID && authority.SourceSHA256 == out.SourceSHA256
	}
	return out, nil
}

func (s *Service) DecideCurrentness(ctx context.Context, a Actor, sc Scope, id string, in CurrentnessInput, key string, expected int64) (Currentness, error) {
	if !identifier(id) || !mutation(key, expected) || in.SourceRevision < 1 || in.ExpectedSourceStateRevision < 1 || in.AuthorityRevision < 1 || !identifier(in.AuthorityDecisionID) || !text(in.Reason, 1000) || strings.TrimSpace(in.Reason) == "" || (in.Verdict != "confirm_current" && in.Verdict != "withdraw") {
		return Currentness{}, ErrInvalid
	}
	tx, err := s.begin(ctx, a, sc, true, true)
	if err != nil {
		return Currentness{}, err
	}
	defer tx.Rollback()
	h, err := getHead(ctx, tx, sc, id, true)
	if err != nil {
		return Currentness{}, err
	}
	if h.revoked {
		return Currentness{}, ErrNotFound
	}
	current, err := currentnessHead(ctx, tx, sc, id)
	if err != nil {
		return Currentness{}, err
	}
	digest := fingerprint("currentness", struct {
		SourceID string
		Input    CurrentnessInput
	}{id, in}, expected)
	prior, replayed, err := replay(ctx, tx, a, sc, key, digest)
	if err != nil {
		return Currentness{}, err
	}
	if replayed {
		out, e := readCurrentness(ctx, tx, sc, id, prior.CurrentnessRevision, current, h)
		if e != nil {
			return Currentness{}, e
		}
		if e = audit(ctx, tx, a, sc, id, out.SourceRevision, "mutation.replayed", nil); e != nil {
			return Currentness{}, e
		}
		if e = s.finish(ctx, tx, a, true); e != nil {
			return Currentness{}, e
		}
		return out, nil
	}
	if h.state != in.ExpectedSourceStateRevision {
		return Currentness{}, ErrSourceChanged
	}
	if current != expected {
		return Currentness{}, ErrRevision
	}
	var digestSource string
	var epoch int64
	if in.Verdict == "withdraw" {
		if current == 0 {
			return Currentness{}, ErrInvalid
		}
		previous, e := readCurrentness(ctx, tx, sc, id, current, current, h)
		if e != nil {
			return Currentness{}, e
		}
		if previous.Verdict != "confirm_current" {
			return Currentness{}, ErrInvalid
		}
		if in.SourceRevision != previous.SourceRevision || in.AuthorityRevision != previous.AuthorityRevision || in.AuthorityDecisionID != previous.AuthorityDecisionID {
			return Currentness{}, ErrSourceChanged
		}
		digestSource = previous.SourceSHA256
		epoch = previous.SourceAccessEpoch
	} else {
		if h.revision != in.SourceRevision {
			return Currentness{}, ErrSourceChanged
		}
		source, e := readSource(ctx, tx, sc, id, h.revision, h)
		if e != nil {
			return Currentness{}, e
		}
		if source.Kind != "document" {
			return Currentness{}, ErrInvalid
		}
		authorityRevision, e := authorityHead(ctx, tx, sc, id)
		if e != nil {
			return Currentness{}, e
		}
		if authorityRevision != in.AuthorityRevision {
			return Currentness{}, ErrSourceChanged
		}
		authority, e := readAuthority(ctx, tx, sc, id, in.AuthorityRevision, authorityRevision, h)
		if e != nil {
			return Currentness{}, e
		}
		if !authority.ReviewEvidenceEligible || authority.DecisionID != in.AuthorityDecisionID {
			return Currentness{}, ErrSourceChanged
		}
		digestSource = source.SHA256
		epoch = h.epoch
	}
	revision := current + 1
	if current == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.evidence_currentness_heads(tenant_id,site_id,source_id,current_revision) VALUES($1,$2,$3,$4)`, sc.TenantID, sc.SiteID, id, revision)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE supervisor.evidence_currentness_heads SET current_revision=$4 WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, sc.TenantID, sc.SiteID, id, revision)
	}
	if err != nil {
		return Currentness{}, err
	}
	// The database insert trigger stamps creation and verification exactly once;
	// receipt replay never executes it and cannot renew an existing verification.
	_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.evidence_currentness_decisions(tenant_id,site_id,source_id,revision,verification_id,source_revision,source_sha256,source_access_epoch,authority_decision_id,authority_revision,verdict,reason,actor_issuer,actor_subject,auth_time)
 SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,s.auth_time FROM supervisor.browser_sessions s WHERE s.token_hash=$15`, sc.TenantID, sc.SiteID, id, revision, uuid.NewString(), in.SourceRevision, digestSource, epoch, in.AuthorityDecisionID, in.AuthorityRevision, in.Verdict, in.Reason, a.Issuer, a.Subject, a.SessionHash)
	if err != nil {
		return Currentness{}, err
	}
	if err = saveReceipt(ctx, tx, a, sc, id, key, digest, receipt{CurrentnessRevision: revision}); err != nil {
		return Currentness{}, err
	}
	if err = audit(ctx, tx, a, sc, id, in.SourceRevision, "currentness.decided", nil); err != nil {
		return Currentness{}, err
	}
	out, err := readCurrentness(ctx, tx, sc, id, revision, revision, h)
	if err != nil {
		return Currentness{}, err
	}
	if err = s.finish(ctx, tx, a, true); err != nil {
		return Currentness{}, err
	}
	return out, nil
}
func (s *Service) ReadCurrentness(ctx context.Context, a Actor, sc Scope, id string, revision int64) (Currentness, error) {
	if !identifier(id) || revision < 0 {
		return Currentness{}, ErrInvalid
	}
	tx, err := s.begin(ctx, a, sc, false, false)
	if err != nil {
		return Currentness{}, err
	}
	defer tx.Rollback()
	h, err := getHead(ctx, tx, sc, id, false)
	if err != nil {
		return Currentness{}, err
	}
	if h.revoked {
		return Currentness{}, ErrNotFound
	}
	current, err := currentnessHead(ctx, tx, sc, id)
	if err != nil {
		return Currentness{}, err
	}
	out, err := readCurrentness(ctx, tx, sc, id, revision, current, h)
	if err != nil {
		return Currentness{}, err
	}
	if err = audit(ctx, tx, a, sc, id, out.SourceRevision, "currentness.read", nil); err != nil {
		return Currentness{}, err
	}
	if err = s.finish(ctx, tx, a, false); err != nil {
		return Currentness{}, err
	}
	return out, nil
}
