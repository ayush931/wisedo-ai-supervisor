package knowledge

import (
	"encoding/json"
	"sort"
	"time"
)

type GraphQuery struct {
	Direction   string   `json:"direction"`
	Roots       []string `json:"roots"`
	Predicates  []string `json:"predicates"`
	AsOf        string   `json:"as_of"`
	Hops        int      `json:"hops"`
	MaxEntities int      `json:"max_entities"`
	MaxClaims   int      `json:"max_claims"`
}

func (q *GraphQuery) Validate() error {
	if q == nil || (q.Direction != "outgoing" && q.Direction != "incoming" && q.Direction != "both") || len(q.Roots) < 1 || len(q.Roots) > 20 || len(q.Predicates) < 1 || len(q.Predicates) > 32 || q.Hops < 0 || q.Hops > 2 || q.MaxEntities < 1 || q.MaxEntities > 200 || q.MaxClaims < 1 || q.MaxClaims > 500 {
		return ErrInvalidContract
	}
	for _, id := range q.Roots {
		if !contractUUID(id) {
			return ErrInvalidContract
		}
	}
	for _, p := range q.Predicates {
		if !relationship(p) && !property(p) {
			return ErrInvalidContract
		}
	}
	_, normalized, err := normalizedTime(q.AsOf)
	if err != nil {
		return err
	}
	q.AsOf = normalized
	unique := func(values []string) []string {
		values = append([]string(nil), values...)
		sort.Strings(values)
		out := values[:0]
		for _, v := range values {
			if len(out) == 0 || out[len(out)-1] != v {
				out = append(out, v)
			}
		}
		return out
	}
	q.Roots = unique(q.Roots)
	q.Predicates = unique(q.Predicates)
	return nil
}

type GraphResult struct {
	SchemaVersion     int    `json:"schema_version"`
	SnapshotID        string `json:"snapshot_id"`
	SHA256            string `json:"sha256"`
	InventoryRevision int64  `json:"inventory_revision"`
	GraphQuery
	Truncated bool             `json:"truncated"`
	Entities  []SnapshotEntity `json:"entities"`
	Claims    []SnapshotClaim  `json:"claims"`
}

// Graph returns the exact response bytes. Authorization, provenance access and
// snapshot freshness are enforced by the service before this pure projection.
func Graph(snapshot Snapshot, q GraphQuery) ([]byte, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	if len(snapshot.Contents.Entities) > 200 || len(snapshot.Contents.Claims) > 500 {
		return nil, ErrTraversalLimit
	}
	entities := map[string]SnapshotEntity{}
	for _, entity := range snapshot.Contents.Entities {
		if _, exists := entities[entity.EntityID]; exists {
			return nil, ErrInvalidContract
		}
		entities[entity.EntityID] = entity
	}
	out := GraphResult{SchemaVersion: 1, SnapshotID: snapshot.SnapshotID, SHA256: snapshot.SHA256, InventoryRevision: snapshot.InventoryRevision, GraphQuery: q, Entities: []SnapshotEntity{}, Claims: []SnapshotClaim{}}
	admitted := map[string]bool{}
	for _, id := range q.Roots {
		entity, found := entities[id]
		if !found {
			return nil, ErrNotFound
		}
		admitted[id] = true
		out.Entities = append(out.Entities, entity)
	}
	encode := func() ([]byte, error) {
		sort.Slice(out.Entities, func(i, j int) bool { return out.Entities[i].EntityID < out.Entities[j].EntityID })
		sort.Slice(out.Claims, func(i, j int) bool { return out.Claims[i].ClaimID < out.Claims[j].ClaimID })
		return json.Marshal(out)
	}
	initial, err := encode()
	if err != nil {
		return nil, err
	}
	if len(out.Entities) > q.MaxEntities || len(initial) > 65536 {
		return nil, ErrTraversalLimit
	}
	allowed := map[string]bool{}
	for _, p := range q.Predicates {
		allowed[p] = true
	}
	at, _, _ := normalizedTime(q.AsOf)
	outgoing := map[string][]SnapshotClaim{}
	seen := map[string]bool{}
	for _, claim := range snapshot.Contents.Claims {
		if seen[claim.ClaimID] {
			return nil, ErrInvalidContract
		}
		seen[claim.ClaimID] = true
		if !allowed[claim.Predicate] {
			continue
		}
		lo, _, e := normalizedTime(claim.EffectiveFrom)
		if e != nil {
			return nil, e
		}
		var hi *time.Time
		if claim.EffectiveTo != nil {
			end, _, e := normalizedTime(*claim.EffectiveTo)
			if e != nil || !end.After(lo) {
				return nil, ErrInvalidContract
			}
			hi = &end
		}
		if at.Before(lo) || (hi != nil && !at.Before(*hi)) {
			continue
		}
		if claim.ObjectEntityID == "" || q.Direction != "incoming" {
			outgoing[claim.SubjectID] = append(outgoing[claim.SubjectID], claim)
		}
		if claim.ObjectEntityID != "" && q.Direction != "outgoing" {
			outgoing[claim.ObjectEntityID] = append(outgoing[claim.ObjectEntityID], claim)
		}
	}
	for id := range outgoing {
		sort.Slice(outgoing[id], func(i, j int) bool { return outgoing[id][i].ClaimID < outgoing[id][j].ClaimID })
	}
	frontier := append([]string(nil), q.Roots...)
	omitted := map[string]bool{}
	emitted := map[string]bool{}
	for depth := 0; depth <= q.Hops && len(frontier) > 0; depth++ {
		next := []string{}
		for _, id := range frontier {
			for _, claim := range outgoing[id] {
				if emitted[claim.ClaimID] {
					continue
				}
				if claim.ObjectEntityID != "" && depth == q.Hops {
					continue
				}
				var added *SnapshotEntity
				target := claim.ObjectEntityID
				if target != "" && target == id {
					target = claim.SubjectID
				}
				if target != "" && !admitted[target] {
					entity, found := entities[target]
					if !found {
						return nil, ErrInvalidContract
					}
					added = &entity
				}
				if len(out.Claims) >= q.MaxClaims || (added != nil && len(out.Entities) >= q.MaxEntities) {
					omitted[claim.ClaimID] = true
					continue
				}
				// Preserve the previous sorted slices: a rejected edge and its new endpoint
				// are an atomic unit, and cannot become a traversal route.
				previousEntities, previousClaims := out.Entities, out.Claims
				out.Entities = append([]SnapshotEntity(nil), out.Entities...)
				if added != nil {
					out.Entities = append(out.Entities, *added)
				}
				out.Claims = append(append([]SnapshotClaim(nil), out.Claims...), claim)
				body, e := encode()
				if e != nil {
					return nil, e
				}
				// false is one byte longer than true, so the final truncated envelope fits.
				if len(body) > 65536 {
					out.Entities, out.Claims = previousEntities, previousClaims
					omitted[claim.ClaimID] = true
					continue
				}
				emitted[claim.ClaimID] = true
				delete(omitted, claim.ClaimID)
				if added != nil {
					admitted[added.EntityID] = true
					next = append(next, added.EntityID)
				}
			}
		}
		sort.Strings(next)
		frontier = next
	}
	out.Truncated = len(omitted) > 0
	return encode()
}
