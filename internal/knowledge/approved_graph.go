package knowledge

import (
	"sort"
	"time"
)

// ValidateApprovedGraph checks structure, not approval or source authority. Its
// caller supplies exact reviewed revisions under the site's consistency lock.
func ValidateApprovedGraph(entities []Entity, claims []Claim) error {
	return validateApprovedGraph(entities, claims, 1_000_000)
}

func validateApprovedGraph(entities []Entity, claims []Claim, budget int) error {
	if len(entities) > 200 || len(claims) > 500 {
		return ErrTraversalLimit
	}
	spend := func() bool { budget--; return budget >= 0 }
	byID := make(map[string]Entity, len(entities))
	for _, e := range entities {
		if !spend() {
			return ErrTraversalLimit
		}
		if e.Revision <= 0 || e.EntityInput.Validate() != nil {
			return ErrInvalidContract
		}
		if _, exists := byID[e.EntityID]; exists {
			return ErrInvalidContract
		}
		byID[e.EntityID] = e
	}
	type edge struct {
		from, to  string
		predicate int
		lo        time.Time
		hi        *time.Time
	}
	edges := make([]edge, 0, len(claims))
	starts := make([]time.Time, 0, len(claims))
	seen := map[string]bool{}
	for _, claim := range claims {
		if !spend() {
			return ErrTraversalLimit
		}
		if claim.Revision <= 0 || seen[claim.ClaimID] {
			return ErrInvalidContract
		}
		seen[claim.ClaimID] = true
		// Contract normalization may replace literal bytes and timestamps: own all
		// mutable input before invoking it, preserving the immutable snapshot payload.
		in := claim.ClaimInput
		in.SourceSpanIDs = append([]string(nil), in.SourceSpanIDs...)
		if in.Literal != nil {
			literal := *in.Literal
			literal.Value = append([]byte(nil), literal.Value...)
			in.Literal = &literal
		}
		if in.Extractor != nil {
			extractor := *in.Extractor
			in.Extractor = &extractor
		}
		if in.Validate() != nil || in.ValidateEntityRevisions() != nil {
			return ErrInvalidContract
		}
		subject, ok := byID[in.SubjectID]
		if !ok || subject.Revision != in.SubjectRevision {
			return ErrInvalidContract
		}
		if in.ObjectEntityID == "" {
			if ValidateLiteralSubject(in.Predicate, subject.Type) != nil {
				return ErrInvalidContract
			}
			continue
		}
		object, ok := byID[in.ObjectEntityID]
		if !ok || object.Revision != in.ObjectRevision || ValidateEntityPair(in.Predicate, subject.Type, object.Type) != nil {
			return ErrInvalidContract
		}
		if in.Predicate == "part_of_asset" && (subject.AssetKind != "machine" || object.AssetKind != "line") {
			return ErrInvalidContract
		}
		predicate := -1
		switch in.Predicate {
		case "precedes":
			predicate = 0
		case "located_in":
			predicate = 1
		case "part_of_asset":
			predicate = 2
		}
		if predicate < 0 {
			continue
		}
		lo, _, err := normalizedTime(in.EffectiveFrom)
		if err != nil {
			return ErrInvalidContract
		}
		var hi *time.Time
		if in.EffectiveTo != nil {
			end, _, err := normalizedTime(*in.EffectiveTo)
			if err != nil {
				return ErrInvalidContract
			}
			hi = &end
		}
		edges = append(edges, edge{in.SubjectID, in.ObjectEntityID, predicate, lo, hi})
		starts = append(starts, lo)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })
	// A violation exists at the maximum start of its participating edges. End-only
	// checkpoints cannot introduce one. Never enumerate paths or their intervals.
	for i, at := range starts {
		if i > 0 && at.Equal(starts[i-1]) {
			continue
		}
		if !spend() {
			return ErrTraversalLimit
		}
		graphs := [3]map[string][]string{{}, {}, {}}
		lines := map[string]string{}
		for _, e := range edges {
			if !spend() {
				return ErrTraversalLimit
			}
			if at.Before(e.lo) || (e.hi != nil && !at.Before(*e.hi)) {
				continue
			}
			if e.predicate == 2 {
				if line, exists := lines[e.from]; exists && line != e.to {
					return ErrReviewBlocked
				}
				lines[e.from] = e.to
			}
			graphs[e.predicate][e.from] = append(graphs[e.predicate][e.from], e.to)
		}
		for _, graph := range graphs {
			colors := map[string]uint8{}
			var visit func(string) error
			visit = func(id string) error {
				if colors[id] == 1 {
					return ErrCycle
				}
				if colors[id] == 2 {
					return nil
				}
				if !spend() {
					return ErrTraversalLimit
				}
				colors[id] = 1
				for _, next := range graph[id] {
					if !spend() {
						return ErrTraversalLimit
					}
					if err := visit(next); err != nil {
						return err
					}
				}
				colors[id] = 2
				return nil
			}
			for id := range graph {
				if err := visit(id); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
