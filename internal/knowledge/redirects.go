package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
)

// ErrEntityRedirected refuses a write to an entity that has been merged away: a second merge,
// a revision, or a new claim pin. It reports as 409 REVIEW_CONFLICT (a decision already exists).
// Reads of the merged entity stay allowed.
var ErrEntityRedirected = fmt.Errorf("entity merged into another entity: %w", ErrReviewConflict)

// MaxRedirectChain is the longest redirect chain a merge may create (resolution follows at most
// this many hops). The chain bound is enforced when merging, never by truncating a read.
const MaxRedirectChain = 16

func redirected(ctx context.Context, tx *sql.Tx, sc Scope, id string) (bool, error) {
	var found bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.knowledge_entity_redirects WHERE tenant_id=$1 AND site_id=$2 AND from_entity_id=$3)`, sc.TenantID, sc.SiteID, id).Scan(&found)
	return found, err
}

// chainHeight is the longest redirect chain ending at id (0 when nothing redirects into it).
const chainHeightSQL = `WITH RECURSIVE up(id,depth) AS (
 SELECT r.from_entity_id,1 FROM supervisor.knowledge_entity_redirects r WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.to_entity_id=$3
 UNION ALL
 SELECT r.from_entity_id,u.depth+1 FROM up u JOIN supervisor.knowledge_entity_redirects r ON r.tenant_id=$1 AND r.site_id=$2 AND r.to_entity_id=u.id WHERE u.depth<=$4
) SELECT coalesce(max(depth),0) FROM up`

// MergeInput redirects the path entity (at FromRevision) into IntoEntityID (at IntoRevision).
type MergeInput struct {
	IntoEntityID string `json:"into_entity_id"`
	FromRevision int64  `json:"from_revision"`
	IntoRevision int64  `json:"into_revision"`
	Reason       string `json:"reason"`
}

// EntityRedirect is the immutable merge decision (WIS-270: "merging identities creates redirect
// decision and preserves original IDs/history"). Neither entity, nor any claim or mapping that
// names them, is rewritten.
type EntityRedirect struct {
	SchemaVersion      int       `json:"schema_version"`
	DecisionID         string    `json:"decision_id"`
	FromEntityID       string    `json:"from_entity_id"`
	FromEntityRevision int64     `json:"from_entity_revision"`
	ToEntityID         string    `json:"to_entity_id"`
	ToEntityRevision   int64     `json:"to_entity_revision"`
	Reason             string    `json:"reason"`
	ActorIssuer        string    `json:"actor_issuer"`
	ActorSubject       string    `json:"actor_subject"`
	AuthenticatedAt    time.Time `json:"authenticated_at"`
	CreatedAt          time.Time `json:"created_at"`
}

func loadRedirect(ctx context.Context, tx *sql.Tx, sc Scope, from string) (EntityRedirect, error) {
	var out EntityRedirect
	err := tx.QueryRowContext(ctx, `SELECT schema_version,decision_id,from_entity_id,from_entity_revision,to_entity_id,to_entity_revision,reason,actor_issuer,actor_subject,auth_time,created_at FROM supervisor.knowledge_entity_redirects WHERE tenant_id=$1 AND site_id=$2 AND from_entity_id=$3`, sc.TenantID, sc.SiteID, from).Scan(&out.SchemaVersion, &out.DecisionID, &out.FromEntityID, &out.FromEntityRevision, &out.ToEntityID, &out.ToEntityRevision, &out.Reason, &out.ActorIssuer, &out.ActorSubject, &out.AuthenticatedAt, &out.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	return out, err
}

// MergeEntity records that entity `from` is the same identity as in.IntoEntityID. Only a
// reviewer (owner or supervisor) with recent authentication may merge; both revisions must be
// current. A second merge of `from` is 409 REVIEW_CONFLICT; merging into an entity that is
// itself redirected, or creating a chain longer than MaxRedirectChain, is 409 KNOWLEDGE_CYCLE;
// a different entity type or asset kind is 422.
func (s *Service) MergeEntity(ctx context.Context, a Actor, sc Scope, from string, in MergeInput, key string, expected int64) (EntityRedirect, error) {
	var out EntityRedirect
	if !contractUUID(from) || !contractUUID(in.IntoEntityID) || from == in.IntoEntityID || !mutation(key, expected) || expected <= 0 || in.FromRevision <= 0 || in.IntoRevision <= 0 || !contractText(in.Reason, 2000) {
		return out, ErrInvalidContract
	}
	if in.FromRevision != expected {
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
	// Lock both heads in UUID order so concurrent merges of the same pair cannot deadlock, and
	// read redirects only afterwards so a concurrent winner is seen as a conflict, not a 500.
	ids := []string{from, in.IntoEntityID}
	sort.Strings(ids)
	heads := map[string]int64{}
	for _, id := range ids {
		var revision int64
		err = tx.QueryRowContext(ctx, `SELECT current_revision FROM supervisor.knowledge_entity_heads WHERE tenant_id=$1 AND site_id=$2 AND id=$3 FOR UPDATE`, sc.TenantID, sc.SiteID, id).Scan(&revision)
		if errors.Is(err, sql.ErrNoRows) {
			return out, ErrNotFound
		}
		if err != nil {
			return out, err
		}
		heads[id] = revision
	}
	digest := fingerprint("merge", struct {
		From  string
		Input MergeInput
	}{from, in}, expected)
	_, ok, err := replay(ctx, tx, a, sc, key, digest)
	if err != nil {
		return out, err
	}
	if ok {
		if out, err = loadRedirect(ctx, tx, sc, from); err != nil {
			return out, err
		}
		if err = audit(ctx, tx, a, sc, from, out.FromEntityRevision, "mutation.replayed"); err != nil {
			return out, err
		}
		return out, s.finishReview(ctx, tx, a)
	}
	if heads[from] != in.FromRevision || heads[in.IntoEntityID] != in.IntoRevision {
		return out, ErrRevision
	}
	if merged, e := redirected(ctx, tx, sc, from); e != nil || merged {
		if e != nil {
			return out, e
		}
		return out, ErrEntityRedirected
	}
	if merged, e := redirected(ctx, tx, sc, in.IntoEntityID); e != nil || merged {
		if e != nil {
			return out, e
		}
		return out, ErrCycle
	}
	// The target is terminal, so every chain through `from` grows to height(from)+1 hops.
	var height int
	if err = tx.QueryRowContext(ctx, chainHeightSQL, sc.TenantID, sc.SiteID, from, MaxRedirectChain).Scan(&height); err != nil {
		return out, err
	}
	if height+1 > MaxRedirectChain {
		return out, ErrCycle
	}
	source, err := loadEntity(ctx, tx, sc, from, in.FromRevision)
	if err != nil {
		return out, err
	}
	target, err := loadEntity(ctx, tx, sc, in.IntoEntityID, in.IntoRevision)
	if err != nil {
		return out, err
	}
	if source.Type != target.Type || source.AssetKind != target.AssetKind {
		return out, ErrInvalidContract
	}
	// Same evidence gate as every other review decision: both revisions' sources must be current
	// and unrevoked (revoked is 404, revised is 409 SOURCE_CHANGED). Evidence heads are only
	// share-locked here, after the entity heads; Revoke takes them FOR UPDATE and no entity lock,
	// so the order cannot cycle.
	refs := []sourceRef{{id: source.SourceID, revision: source.SourceRevision}, {id: target.SourceID, revision: target.SourceRevision}}
	states, err := lockSources(ctx, tx, sc, refs)
	if err != nil {
		return out, err
	}
	if _, err = eligible(refs, states, true); err != nil {
		return out, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_entity_redirects(tenant_id,site_id,decision_id,from_entity_id,from_entity_revision,to_entity_id,to_entity_revision,reason,actor_issuer,actor_subject,auth_time) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, sc.TenantID, sc.SiteID, uuid.NewString(), from, in.FromRevision, in.IntoEntityID, in.IntoRevision, in.Reason, a.Issuer, a.Subject, auth)
	if err != nil {
		return out, err
	}
	if err = saveReceipt(ctx, tx, a, sc, key, digest, "merge", from, in.FromRevision); err != nil {
		return out, err
	}
	if err = audit(ctx, tx, a, sc, from, in.FromRevision, "entity.merged"); err != nil {
		return out, err
	}
	if out, err = loadRedirect(ctx, tx, sc, from); err != nil {
		return out, err
	}
	return out, s.finishReview(ctx, tx, a)
}
