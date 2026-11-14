package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

var ErrSnapshotStale = errors.New("snapshot inventory revision is stale")

// ReadGraph keeps authority, inventory and traversal in one serialized site cut.
// Pure traversal is not an authorization boundary and must not run after commit.
func (s *Service) ReadGraph(ctx context.Context, a Actor, sc Scope, id string, q GraphQuery) ([]byte, error) {
	if !contractUUID(id) {
		return nil, ErrInvalidContract
	}
	tx, err := s.beginLocked(ctx, a, sc, false, true)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	snapshot, err := loadSnapshot(ctx, tx, sc, id)
	if err != nil {
		return nil, err
	}
	if snapshot.Stale {
		return nil, ErrSnapshotStale
	}
	result, err := Graph(snapshot, q)
	if err != nil {
		return nil, err
	}
	if err = audit(ctx, tx, a, sc, id, 1, "snapshot.read"); err != nil {
		return nil, err
	}
	if err = s.finish(ctx, tx, a); err != nil {
		return nil, err
	}
	return result, nil
}

// Readiness reason codes for KnowledgeResult. Each NOT_READY answer carries
// exactly one; none of them is ever accompanied by graph content.
const (
	ReasonNoSnapshot          = "NO_SNAPSHOT"
	ReasonSnapshotUnavailable = "SNAPSHOT_UNAVAILABLE"
	ReasonSnapshotStale       = "SNAPSHOT_STALE"
	ReasonEntityNotInSnapshot = "ENTITY_NOT_IN_SNAPSHOT"
)

// KnowledgeResult is the GET /knowledge response. Graph is present only when
// Readiness is READY.
type KnowledgeResult struct {
	SchemaVersion     int        `json:"schema_version"`
	EntityID          string     `json:"entity_id"`
	AsOf              string     `json:"as_of"`
	Depth             int        `json:"depth"`
	Readiness         string     `json:"readiness"`
	ReasonCode        string     `json:"reason_code,omitempty"`
	SnapshotID        string     `json:"snapshot_id,omitempty"`
	SHA256            string     `json:"sha256,omitempty"`
	CapturedAt        string     `json:"captured_at,omitempty"`
	InventoryRevision *int64     `json:"inventory_revision,omitempty"`
	Graph             *Neighbors `json:"graph,omitempty"`
}
type Neighbors struct {
	Truncated bool             `json:"truncated"`
	Entities  []SnapshotEntity `json:"entities"`
	Claims    []SnapshotClaim  `json:"claims"`
}

// ReadKnowledge answers "what did approved knowledge say about entity_id as of
// as_of", without ever serving a cut that is not readable now.
//
//   - Snapshot selection: the site's snapshot with the greatest server-stamped
//     created_at <= as_of (ties broken by greatest snapshot_id). An omitted as_of
//     is clock_timestamp() read after the site lock, the same clock that stamps
//     created_at, so a capture this read serialized behind is always visible
//     (now() is transaction start and would miss it). An as_of later than that
//     clock is rejected (ErrInvalidContract): the future has no recorded cut yet.
//     The same instant filters claim effective intervals.
//   - No qualifying snapshot: NOT_READY/NO_SNAPSHOT.
//   - A cited source revoked or missing: NOT_READY/SNAPSHOT_UNAVAILABLE, with no
//     snapshot identity, because a direct read of that snapshot is a
//     non-disclosing 404 (the id stays only in the audit target). The
//     inventory moved on since capture: NOT_READY/SNAPSHOT_STALE (the same
//     refusals ReadGraph makes; content is only served for the current cut).
//   - Entity absent from a readable snapshot: NOT_READY/ENTITY_NOT_IN_SNAPSHOT.
//     Entities enter a snapshot only through approved claims, so "unknown to
//     approved knowledge" cannot be told apart from "isolated"; an empty READY
//     answer would conflate unknown with empty (rule 06).
//
// Every answered read, READY or NOT_READY, writes one snapshot.read audit row
// (target = resolved snapshot, NULL when none resolved). An error (413 limit,
// 422, 404 scope, storage failure) rolls the transaction back with no audit
// row, consistent with the other knowledge reads. Neighbourhood is the
// pure Graph traversal in both directions over every predicate.
func (s *Service) ReadKnowledge(ctx context.Context, a Actor, sc Scope, entityID, asOf string, depth int) (KnowledgeResult, error) {
	var at *time.Time
	if asOf != "" {
		v, _, err := normalizedTime(asOf)
		if err != nil {
			return KnowledgeResult{}, err
		}
		at = &v
	}
	if !contractUUID(entityID) || depth < 0 || depth > 2 {
		return KnowledgeResult{}, ErrInvalidContract
	}
	tx, err := s.beginLocked(ctx, a, sc, false, true)
	if err != nil {
		return KnowledgeResult{}, err
	}
	defer tx.Rollback()
	var effective time.Time
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&effective); err != nil {
		return KnowledgeResult{}, err
	}
	if at != nil {
		if at.After(effective) {
			return KnowledgeResult{}, ErrInvalidContract
		}
		effective = *at
	}
	var id, digest sql.NullString
	var captured sql.NullTime
	// ponytail: per-site scan on created_at; add an index if snapshot counts per site grow large.
	err = tx.QueryRowContext(ctx, `SELECT snapshot_id,created_at,sha256 FROM supervisor.knowledge_process_snapshots WHERE tenant_id=$1 AND site_id=$2 AND created_at<=$3 ORDER BY created_at DESC,snapshot_id DESC LIMIT 1`, sc.TenantID, sc.SiteID, effective).Scan(&id, &captured, &digest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return KnowledgeResult{}, err
	}
	out := KnowledgeResult{SchemaVersion: 1, EntityID: entityID, AsOf: effective.UTC().Format(time.RFC3339Nano), Depth: depth, Readiness: "NOT_READY"}
	var target any
	if !id.Valid {
		out.ReasonCode = ReasonNoSnapshot
	} else {
		target = id.String
		identify := func() {
			out.SnapshotID, out.SHA256, out.CapturedAt = id.String, digest.String, captured.Time.UTC().Format(time.RFC3339Nano)
		}
		// The row exists under this serialized site cut, so ErrNotFound from the
		// load can only be a revoked or missing cited source.
		snapshot, e := loadSnapshot(ctx, tx, sc, id.String)
		switch {
		case errors.Is(e, ErrNotFound):
			out.ReasonCode = ReasonSnapshotUnavailable
		case e != nil:
			return KnowledgeResult{}, e
		case snapshot.Stale:
			identify()
			out.ReasonCode = ReasonSnapshotStale
			out.InventoryRevision = &snapshot.InventoryRevision
		default:
			identify()
			out.InventoryRevision = &snapshot.InventoryRevision
			if out.Graph, e = neighbors(snapshot, entityID, out.AsOf, depth); e != nil && !errors.Is(e, ErrNotFound) {
				return KnowledgeResult{}, e
			}
			if out.Graph == nil {
				out.ReasonCode = ReasonEntityNotInSnapshot
			} else {
				out.Readiness = "READY"
			}
		}
	}
	if err = audit(ctx, tx, a, sc, target, 1, "snapshot.read"); err != nil {
		return KnowledgeResult{}, err
	}
	if err = s.finish(ctx, tx, a); err != nil {
		return KnowledgeResult{}, err
	}
	return out, nil
}

// neighbors returns nil, ErrNotFound when the entity is not in the snapshot.
func neighbors(snapshot Snapshot, entityID, asOf string, depth int) (*Neighbors, error) {
	q := GraphQuery{Direction: "both", Roots: []string{entityID}, AsOf: asOf, Hops: depth, MaxEntities: 200, MaxClaims: 500,
		Predicates: Predicates()}
	body, err := Graph(snapshot, q)
	if err != nil {
		return nil, err
	}
	var g GraphResult
	if err = json.Unmarshal(body, &g); err != nil {
		return nil, err
	}
	return &Neighbors{Truncated: g.Truncated, Entities: g.Entities, Claims: g.Claims}, nil
}
