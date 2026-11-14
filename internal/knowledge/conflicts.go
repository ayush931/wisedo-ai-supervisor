package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

type ConflictClaimRef struct {
	ClaimID  string `json:"claim_id"`
	Revision int64  `json:"revision"`
}
type Conflict struct {
	ID            string           `json:"id"`
	SchemaVersion int              `json:"schema_version"`
	Left          ConflictClaimRef `json:"left"`
	Right         ConflictClaimRef `json:"right"`
	Kind          string           `json:"kind"`
	EffectiveFrom time.Time        `json:"effective_from"`
	EffectiveTo   *time.Time       `json:"effective_to,omitempty"`
	CreatedAt     time.Time        `json:"created_at"`
}
type ConflictPage struct {
	Conflicts  []Conflict `json:"conflicts"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

// Alias must be a code-owned SQL identifier. Shared by detection and active reads.
// ponytail: OFFSET 0 keeps exact-revision lookups correlated; flattening these RLS
// subqueries scanned over a million rows for 1001 candidates with fresh statistics.
func currentClaimSQL(alias string) string {
	return strings.ReplaceAll(`EXISTS(SELECT 1 FROM supervisor.knowledge_claim_heads ch WHERE (ch.tenant_id,ch.site_id,ch.id,ch.current_revision)=(R.tenant_id,R.site_id,R.claim_id,R.revision) OFFSET 0)
 AND EXISTS(SELECT 1 FROM supervisor.knowledge_claim_entity_refs p JOIN supervisor.knowledge_entity_heads sh ON (sh.tenant_id,sh.site_id,sh.id,sh.current_revision)=(p.tenant_id,p.site_id,p.subject_id,p.subject_revision) WHERE (p.tenant_id,p.site_id,p.claim_id,p.claim_revision)=(R.tenant_id,R.site_id,R.claim_id,R.revision) AND (p.object_entity_id IS NULL OR EXISTS(SELECT 1 FROM supervisor.knowledge_entity_heads oh WHERE (oh.tenant_id,oh.site_id,oh.id,oh.current_revision)=(p.tenant_id,p.site_id,p.object_entity_id,p.object_revision))) OFFSET 0)
 AND NOT EXISTS(SELECT 1 FROM supervisor.knowledge_claim_entity_refs p JOIN supervisor.knowledge_entity_revisions e ON e.tenant_id=p.tenant_id AND e.site_id=p.site_id AND ((e.id=p.subject_id AND e.revision=p.subject_revision) OR (e.id=p.object_entity_id AND e.revision=p.object_revision)) JOIN supervisor.evidence_heads h ON (h.tenant_id,h.site_id,h.source_id)=(e.tenant_id,e.site_id,e.source_id) WHERE (p.tenant_id,p.site_id,p.claim_id,p.claim_revision)=(R.tenant_id,R.site_id,R.claim_id,R.revision) AND (h.current_revision<>e.source_revision OR h.revoked OR EXISTS(SELECT 1 FROM supervisor.evidence_revocations rv WHERE (rv.tenant_id,rv.site_id,rv.source_id)=(h.tenant_id,h.site_id,h.source_id))) OFFSET 0)
 AND NOT EXISTS(SELECT 1 FROM supervisor.knowledge_claim_spans s JOIN supervisor.evidence_heads h ON (h.tenant_id,h.site_id,h.source_id)=(s.tenant_id,s.site_id,s.source_id) WHERE (s.tenant_id,s.site_id,s.claim_id,s.claim_revision)=(R.tenant_id,R.site_id,R.claim_id,R.revision) AND (h.current_revision<>s.source_revision OR h.revoked OR EXISTS(SELECT 1 FROM supervisor.evidence_revocations rv WHERE (rv.tenant_id,rv.site_id,rv.source_id)=(h.tenant_id,h.site_id,h.source_id))) OFFSET 0)`, "R.", alias+".")
}
func conflictingValues(a, b ClaimInput) string {
	if !singleValued(a.Predicate) {
		return ""
	}
	if a.ObjectEntityID != "" {
		if a.ObjectEntityID != b.ObjectEntityID {
			return "value_conflict"
		}
		return ""
	}
	if a.Literal == nil || b.Literal == nil {
		return ""
	}
	if a.Literal.Unit != b.Literal.Unit {
		return "unit_review"
	}
	var av, bv any
	if json.Unmarshal(a.Literal.Value, &av) != nil || json.Unmarshal(b.Literal.Value, &bv) != nil {
		return "text_review"
	}
	if a.Literal.Type == b.Literal.Type && av == bv {
		return ""
	}
	if a.Literal.Type == "decimal" && b.Literal.Type == "decimal" {
		return "value_conflict"
	}
	return "text_review"
}
func conflictOverlap(a, b ClaimInput) (time.Time, *time.Time, bool) {
	start, _ := time.Parse(time.RFC3339Nano, a.EffectiveFrom)
	other, _ := time.Parse(time.RFC3339Nano, b.EffectiveFrom)
	if other.After(start) {
		start = other
	}
	var end *time.Time
	for _, raw := range []*string{a.EffectiveTo, b.EffectiveTo} {
		if raw != nil {
			v, _ := time.Parse(time.RFC3339Nano, *raw)
			if end == nil || v.Before(*end) {
				end = &v
			}
		}
	}
	return start, end, end == nil || start.Before(*end)
}
func recordConflicts(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, in ClaimInput, revision int64) error {
	if !singleValued(in.Predicate) {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT r.payload,r.revision FROM supervisor.knowledge_claim_revisions r WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.subject_id=$3 AND r.predicate=$4 AND r.claim_id<>$5
 AND ($7::timestamptz IS NULL OR (r.payload->>'effective_from')::timestamptz<$7::timestamptz)
 AND ((r.payload->>'effective_to') IS NULL OR $6::timestamptz<(r.payload->>'effective_to')::timestamptz)
 AND `+currentClaimSQL("r")+` ORDER BY r.claim_id LIMIT 1001`, sc.TenantID, sc.SiteID, in.SubjectID, in.Predicate, in.ClaimID, in.EffectiveFrom, in.EffectiveTo)
	if err != nil {
		return err
	}
	type candidate struct {
		in       ClaimInput
		revision int64
	}
	others := []candidate{}
	for rows.Next() {
		var payload []byte
		var c candidate
		if err = rows.Scan(&payload, &c.revision); err != nil {
			rows.Close()
			return err
		}
		if err = json.Unmarshal(payload, &c.in); err != nil {
			rows.Close()
			return err
		}
		others = append(others, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(others) > 1000 {
		return ErrTraversalLimit
	}
	for _, other := range others {
		kind := conflictingValues(in, other.in)
		if kind == "" {
			continue
		}
		start, end, overlap := conflictOverlap(in, other.in)
		if !overlap {
			continue
		}
		left, right := ConflictClaimRef{in.ClaimID, revision}, ConflictClaimRef{other.in.ClaimID, other.revision}
		if left.ClaimID > right.ClaimID {
			left, right = right, left
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_conflicts(tenant_id,site_id,id,left_claim_id,left_revision,right_claim_id,right_revision,kind,effective_from,effective_to) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, sc.TenantID, sc.SiteID, uuid.NewString(), left.ClaimID, left.Revision, right.ClaimID, right.Revision, kind, start, end)
		if err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) ListConflicts(ctx context.Context, a Actor, sc Scope, id string, revision int64, limit int, cursor string) (ConflictPage, error) {
	for range 3 {
		out, err := s.listConflicts(ctx, a, sc, id, revision, limit, cursor)
		if !errors.Is(err, errPageChanged) {
			return out, err
		}
	}
	return ConflictPage{}, ErrSourceChanged
}
func (s *Service) listConflicts(ctx context.Context, a Actor, sc Scope, id string, revision int64, limit int, cursor string) (ConflictPage, error) {
	out := ConflictPage{Conflicts: []Conflict{}}
	if !contractUUID(id) || revision < 0 || !pageArgs(limit, cursor) {
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
	target, err := loadClaim(ctx, tx, sc, id, revision)
	if err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.id,c.schema_version,c.left_claim_id,c.left_revision,c.right_claim_id,c.right_revision,c.kind,c.effective_from,c.effective_to,c.created_at FROM supervisor.knowledge_conflicts c JOIN supervisor.knowledge_claim_revisions l ON (l.tenant_id,l.site_id,l.claim_id,l.revision)=(c.tenant_id,c.site_id,c.left_claim_id,c.left_revision) JOIN supervisor.knowledge_claim_revisions r ON (r.tenant_id,r.site_id,r.claim_id,r.revision)=(c.tenant_id,c.site_id,c.right_claim_id,c.right_revision) WHERE c.tenant_id=$1 AND c.site_id=$2 AND ((c.left_claim_id=$3 AND c.left_revision=$4) OR (c.right_claim_id=$3 AND c.right_revision=$4)) AND c.id>$5 AND `+currentClaimSQL("l")+` AND `+currentClaimSQL("r")+` ORDER BY c.id LIMIT $6`, sc.TenantID, sc.SiteID, id, target.Revision, cursor, limit+1)
	if err != nil {
		return out, err
	}
	selected := []Conflict{}
	for rows.Next() {
		var c Conflict
		var end sql.NullTime
		if err = rows.Scan(&c.ID, &c.SchemaVersion, &c.Left.ClaimID, &c.Left.Revision, &c.Right.ClaimID, &c.Right.Revision, &c.Kind, &c.EffectiveFrom, &end, &c.CreatedAt); err != nil {
			rows.Close()
			return out, err
		}
		if end.Valid {
			c.EffectiveTo = &end.Time
		}
		selected = append(selected, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	claims := map[ConflictClaimRef]Claim{{target.ClaimID, target.Revision}: target}
	for _, c := range selected {
		for _, ref := range []ConflictClaimRef{c.Left, c.Right} {
			if _, ok := claims[ref]; ok {
				continue
			}
			claim, e := loadClaim(ctx, tx, sc, ref.ClaimID, ref.Revision)
			if e != nil {
				return out, e
			}
			claims[ref] = claim
		}
	}
	refs := []sourceRef{}
	byClaim := map[ConflictClaimRef][]sourceRef{}
	entityIDs := []string{}
	claimIDs := []string{}
	for ref, c := range claims {
		r, _, _, e := claimRefs(ctx, tx, sc, c.ClaimInput)
		if e != nil {
			return out, e
		}
		refs = append(refs, r...)
		byClaim[ref] = r
		entityIDs = append(entityIDs, c.SubjectID, c.ObjectEntityID)
		claimIDs = append(claimIDs, c.ClaimID)
	}
	states, err := lockSources(ctx, tx, sc, refs)
	if err != nil {
		return out, err
	}
	entities, err := lockEntities(ctx, tx, sc, entityIDs)
	if err != nil {
		return out, err
	}
	sort.Strings(claimIDs)
	heads := map[string]int64{}
	for _, cid := range claimIDs {
		if heads[cid] != 0 {
			continue
		}
		r, e := head(ctx, tx, sc, cid, true)
		if e != nil {
			return out, e
		}
		heads[cid] = r
	}
	if revision == 0 && heads[id] != target.Revision {
		return out, errPageChanged
	}
	for ref, c := range claims {
		current, e := eligible(byClaim[ref], states, false)
		if e != nil {
			if ref.ClaimID == id {
				return out, e
			}
			return out, errPageChanged
		}
		current = current && entityPinsCurrent(c.ClaimInput, entities) && heads[ref.ClaimID] == ref.Revision
		if !current {
			if len(selected) > 0 {
				return out, errPageChanged
			}
		}
	}
	out.Conflicts = selected
	if len(out.Conflicts) > limit {
		out.Conflicts = out.Conflicts[:limit]
		out.NextCursor = out.Conflicts[limit-1].ID
	}
	if err = audit(ctx, tx, a, sc, id, target.Revision, "conflicts.read"); err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}
