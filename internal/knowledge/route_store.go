package knowledge

import "context"

// ReadRoute returns the route summary of a process in one approved snapshot (WIS-642). Like
// ReadGraph it reads in one serialized site cut: the snapshot's sources are rechecked, a snapshot
// the inventory has moved past is refused (ErrSnapshotStale, 409 SNAPSHOT_STALE), and the read is
// audited. The summary is computed by the pure Route before commit.
func (s *Service) ReadRoute(ctx context.Context, a Actor, sc Scope, id string, q RouteQuery) (RouteSummary, error) {
	if !contractUUID(id) {
		return RouteSummary{}, ErrInvalidContract
	}
	if err := q.Validate(); err != nil {
		return RouteSummary{}, err
	}
	tx, err := s.beginLocked(ctx, a, sc, false, true)
	if err != nil {
		return RouteSummary{}, err
	}
	defer tx.Rollback()
	snapshot, err := loadSnapshot(ctx, tx, sc, id)
	if err != nil {
		return RouteSummary{}, err
	}
	if snapshot.Stale {
		return RouteSummary{}, ErrSnapshotStale
	}
	out, err := Route(snapshot, q)
	if err != nil {
		return RouteSummary{}, err
	}
	if err = audit(ctx, tx, a, sc, id, 1, "snapshot.read"); err != nil {
		return RouteSummary{}, err
	}
	if err = s.finish(ctx, tx, a); err != nil {
		return RouteSummary{}, err
	}
	return out, nil
}
