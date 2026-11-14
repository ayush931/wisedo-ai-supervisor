package knowledge

import (
	"context"
	"database/sql"
)

func cycle(ctx context.Context, tx *sql.Tx, sc Scope, in ClaimInput) error {
	if in.Predicate != "precedes" && in.Predicate != "part_of_asset" && in.Predicate != "located_in" {
		return nil
	}
	// Carry the intersection of the entire path, not merely adjacent edge overlaps.
	// UNION deduplicates equal vertex/interval states, including existing temporal cycles.
	// Reuse the bounded, provenance-current claim predicate and its RLS planner fences.
	query := `WITH RECURSIVE reachable(id,lo,hi) AS (
 SELECT $3::uuid,$6::timestamptz,COALESCE($7::timestamptz,'infinity'::timestamptz)
 UNION
 SELECT r.object_entity_id,GREATEST(x.lo,(r.payload->>'effective_from')::timestamptz),LEAST(x.hi,COALESCE((r.payload->>'effective_to')::timestamptz,'infinity'::timestamptz))
 FROM reachable x JOIN supervisor.knowledge_claim_revisions r ON r.subject_id=x.id
 WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.predicate=$4 AND r.claim_id<>$5 AND r.object_entity_id IS NOT NULL
 AND GREATEST(x.lo,(r.payload->>'effective_from')::timestamptz)<LEAST(x.hi,COALESCE((r.payload->>'effective_to')::timestamptz,'infinity'::timestamptz))
 AND ` + currentClaimSQL("r") + `
 ) SELECT id FROM reachable LIMIT 1001`
	rows, err := tx.QueryContext(ctx, query, sc.TenantID, sc.SiteID, in.ObjectEntityID, in.Predicate, in.ClaimID, in.EffectiveFrom, in.EffectiveTo)
	if err != nil {
		return err
	}
	defer rows.Close()
	n := 0
	found := false
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return err
		}
		n++
		found = found || id == in.SubjectID
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if found {
		return ErrCycle
	}
	if n > 1000 {
		return ErrTraversalLimit
	}
	return nil
}
