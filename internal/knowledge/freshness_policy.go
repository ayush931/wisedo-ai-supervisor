package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
)

const FreshnessPolicyBasis = "owner_currentness_verified_at"
const MaxFreshnessAgeSeconds = int64(math.MaxInt64) / int64(time.Second)

type FreshnessPolicyInput struct {
	Verdict       string `json:"verdict"`
	MaxAgeSeconds *int64 `json:"max_age_seconds,omitempty"`
	Reason        string `json:"reason"`
}

func (in *FreshnessPolicyInput) Validate() error {
	if in == nil || !contractText(in.Reason, 1000) {
		return ErrInvalidContract
	}
	switch in.Verdict {
	case "define":
		if in.MaxAgeSeconds == nil || *in.MaxAgeSeconds < 1 || *in.MaxAgeSeconds > MaxFreshnessAgeSeconds {
			return ErrInvalidContract
		}
	case "withdraw":
		if in.MaxAgeSeconds != nil {
			return ErrInvalidContract
		}
	default:
		return ErrInvalidContract
	}
	return nil
}

type FreshnessPolicy struct {
	SchemaVersion   int       `json:"schema_version"`
	DecisionID      string    `json:"decision_id"`
	Revision        int64     `json:"revision"`
	CurrentRevision int64     `json:"current_revision"`
	Verdict         string    `json:"verdict"`
	Basis           string    `json:"basis"`
	MaxAgeSeconds   *int64    `json:"max_age_seconds"`
	Reason          string    `json:"reason"`
	ActorIssuer     string    `json:"actor_issuer"`
	ActorSubject    string    `json:"actor_subject"`
	AuthenticatedAt time.Time `json:"authenticated_at"`
	CreatedAt       time.Time `json:"created_at"`
	PolicyCurrent   bool      `json:"policy_current"`
}

func freshnessPolicyHead(ctx context.Context, tx *sql.Tx, sc Scope) (int64, error) {
	var revision int64
	err := tx.QueryRowContext(ctx, `SELECT current_revision FROM supervisor.document_freshness_policy_heads WHERE tenant_id=$1 AND site_id=$2 FOR SHARE`, sc.TenantID, sc.SiteID).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return revision, err
}

// loadFreshnessPolicy participates in the caller's existing site-serialized cut.
// A historical definition never becomes current again after a replacement.
func loadFreshnessPolicy(ctx context.Context, tx *sql.Tx, sc Scope, revision int64) (FreshnessPolicy, error) {
	var out FreshnessPolicy
	current, err := freshnessPolicyHead(ctx, tx, sc)
	if err != nil {
		return out, err
	}
	if revision == 0 {
		revision = current
	}
	var age sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT schema_version,decision_id,revision,verdict,basis,max_age_seconds,reason,actor_issuer,actor_subject,auth_time,created_at FROM supervisor.document_freshness_policy_decisions WHERE tenant_id=$1 AND site_id=$2 AND revision=$3`, sc.TenantID, sc.SiteID, revision).Scan(&out.SchemaVersion, &out.DecisionID, &out.Revision, &out.Verdict, &out.Basis, &age, &out.Reason, &out.ActorIssuer, &out.ActorSubject, &out.AuthenticatedAt, &out.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return FreshnessPolicy{}, ErrNotFound
	}
	if err != nil {
		return FreshnessPolicy{}, err
	}
	if age.Valid {
		out.MaxAgeSeconds = &age.Int64
	}
	// Keep duration conversion safe even if a privileged importer violates the API.
	if out.Basis != FreshnessPolicyBasis {
		return FreshnessPolicy{}, ErrInvalidContract
	}
	input := FreshnessPolicyInput{Verdict: out.Verdict, MaxAgeSeconds: out.MaxAgeSeconds, Reason: out.Reason}
	if err = input.Validate(); err != nil {
		return FreshnessPolicy{}, err
	}
	out.CurrentRevision = current
	out.PolicyCurrent = out.Revision == current && out.Verdict == "define"
	return out, nil
}

func (s *Service) requireFreshnessPolicyOwner(ctx context.Context, tx *sql.Tx, a Actor, sc Scope) (time.Time, error) {
	var role string
	err := tx.QueryRowContext(ctx, `SELECT role FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND active FOR SHARE`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrDenied
	}
	if err != nil {
		return time.Time{}, err
	}
	if role != "owner" {
		return time.Time{}, ErrDenied
	}
	return s.reviewAuthentication(ctx, tx, a)
}

func (s *Service) DecideFreshnessPolicy(ctx context.Context, a Actor, sc Scope, in FreshnessPolicyInput, key string, expected int64) (FreshnessPolicy, error) {
	if err := in.Validate(); err != nil {
		return FreshnessPolicy{}, err
	}
	if !mutation(key, expected) {
		return FreshnessPolicy{}, ErrInvalidContract
	}
	tx, err := s.beginLocked(ctx, a, sc, true, true)
	if err != nil {
		return FreshnessPolicy{}, err
	}
	defer tx.Rollback()
	auth, err := s.requireFreshnessPolicyOwner(ctx, tx, a, sc)
	if err != nil {
		return FreshnessPolicy{}, err
	}
	digest := fingerprint("freshness_policy", in, expected)
	prior, replayed, err := replay(ctx, tx, a, sc, key, digest)
	if err != nil {
		return FreshnessPolicy{}, err
	}
	if replayed {
		out, e := loadFreshnessPolicy(ctx, tx, sc, prior)
		if e != nil {
			return FreshnessPolicy{}, e
		}
		if e = audit(ctx, tx, a, sc, sc.SiteID, prior, "mutation.replayed"); e != nil {
			return FreshnessPolicy{}, e
		}
		if e = s.finishReview(ctx, tx, a); e != nil {
			return FreshnessPolicy{}, e
		}
		return out, nil
	}
	current, err := freshnessPolicyHead(ctx, tx, sc)
	if err != nil {
		return FreshnessPolicy{}, err
	}
	if current != expected {
		return FreshnessPolicy{}, ErrRevision
	}
	if in.Verdict == "withdraw" {
		if current == 0 {
			return FreshnessPolicy{}, ErrInvalidContract
		}
		previous, e := loadFreshnessPolicy(ctx, tx, sc, current)
		if e != nil {
			return FreshnessPolicy{}, e
		}
		if previous.Verdict != "define" {
			return FreshnessPolicy{}, ErrInvalidContract
		}
	}
	revision := current + 1
	if current == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.document_freshness_policy_heads(tenant_id,site_id,current_revision) VALUES($1,$2,$3)`, sc.TenantID, sc.SiteID, revision)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE supervisor.document_freshness_policy_heads SET current_revision=$3 WHERE tenant_id=$1 AND site_id=$2`, sc.TenantID, sc.SiteID, revision)
	}
	if err != nil {
		return FreshnessPolicy{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.document_freshness_policy_decisions(tenant_id,site_id,revision,decision_id,verdict,basis,max_age_seconds,reason,actor_issuer,actor_subject,auth_time) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, sc.TenantID, sc.SiteID, revision, uuid.NewString(), in.Verdict, FreshnessPolicyBasis, in.MaxAgeSeconds, in.Reason, a.Issuer, a.Subject, auth)
	if err != nil {
		return FreshnessPolicy{}, err
	}
	if err = saveReceipt(ctx, tx, a, sc, key, digest, "freshness_policy", sc.SiteID, revision); err != nil {
		return FreshnessPolicy{}, err
	}
	if err = audit(ctx, tx, a, sc, sc.SiteID, revision, "freshness_policy.decided"); err != nil {
		return FreshnessPolicy{}, err
	}
	out, err := loadFreshnessPolicy(ctx, tx, sc, revision)
	if err != nil {
		return FreshnessPolicy{}, err
	}
	if err = s.finishReview(ctx, tx, a); err != nil {
		return FreshnessPolicy{}, err
	}
	return out, nil
}
func (s *Service) ReadFreshnessPolicy(ctx context.Context, a Actor, sc Scope, revision int64) (FreshnessPolicy, error) {
	if revision < 0 {
		return FreshnessPolicy{}, ErrInvalidContract
	}
	tx, err := s.beginLocked(ctx, a, sc, false, true)
	if err != nil {
		return FreshnessPolicy{}, err
	}
	defer tx.Rollback()
	out, err := loadFreshnessPolicy(ctx, tx, sc, revision)
	if err != nil {
		return FreshnessPolicy{}, err
	}
	if err = audit(ctx, tx, a, sc, sc.SiteID, out.Revision, "freshness_policy.read"); err != nil {
		return FreshnessPolicy{}, err
	}
	if err = s.finish(ctx, tx, a); err != nil {
		return FreshnessPolicy{}, err
	}
	return out, nil
}
