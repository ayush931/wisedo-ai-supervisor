package knowledge

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"testing"
)

func TestApprovedGraph(t *testing.T) {
	entity := func(kind, asset string) Entity {
		return Entity{EntityInput: EntityInput{EntityID: uuid.NewString(), Type: kind, AssetKind: asset, Name: "Synthetic", SourceID: uuid.NewString(), SourceRevision: 1}, Revision: 1}
	}
	a, b, c := entity("operation", ""), entity("operation", ""), entity("operation", "")
	stamp := func(hour int) string { return fmt.Sprintf("2026-09-27T%02d:00:00Z", hour) }
	edge := func(a, b Entity, predicate string, lo, hi int) Claim {
		in := ClaimInput{ClaimID: uuid.NewString(), SubjectID: a.EntityID, SubjectRevision: 1, ObjectEntityID: b.EntityID, ObjectRevision: 1, Predicate: predicate, EffectiveFrom: stamp(lo), EvidenceClass: "report", SourceSpanIDs: []string{uuid.NewString()}}
		if hi >= 0 {
			end := stamp(hi)
			in.EffectiveTo = &end
		}
		return Claim{ClaimInput: in, Revision: 1}
	}
	check := func(t *testing.T, entities []Entity, claims []Claim, want error) {
		t.Helper()
		before, _ := json.Marshal(struct {
			E []Entity
			C []Claim
		}{entities, claims})
		got := ValidateApprovedGraph(entities, claims)
		if !errors.Is(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
		after, _ := json.Marshal(struct {
			E []Entity
			C []Claim
		}{entities, claims})
		if string(before) != string(after) {
			t.Fatal("input mutated")
		}
	}
	for _, tc := range []struct {
		name   string
		claims []Claim
		want   error
	}{
		{"overlap", []Claim{edge(a, b, "precedes", 0, 3), edge(b, a, "precedes", 2, 4)}, ErrCycle},
		{"touching", []Claim{edge(a, b, "precedes", 0, 2), edge(b, a, "precedes", 2, 4)}, nil},
		{"disjoint", []Claim{edge(a, b, "precedes", 0, 1), edge(b, a, "precedes", 2, 4)}, nil},
		{"no full intersection", []Claim{edge(a, b, "precedes", 0, 3), edge(b, c, "precedes", 2, 5), edge(c, a, "precedes", 4, 6)}, nil},
		{"full intersection", []Claim{edge(a, b, "precedes", 0, 5), edge(b, c, "precedes", 2, 6), edge(c, a, "precedes", 4, 7)}, ErrCycle},
		{"infinity", []Claim{edge(a, b, "precedes", 0, -1), edge(b, a, "precedes", 2, -1)}, ErrCycle},
	} {
		t.Run(tc.name, func(t *testing.T) { check(t, []Entity{a, b, c}, tc.claims, tc.want) })
	}
	t.Run("canonical times and immutable input", func(t *testing.T) {
		x, y := edge(a, b, "precedes", 0, 1), edge(b, a, "precedes", 1, 2)
		x.EffectiveFrom = "2026-09-27T01:00:00+01:00"
		end := "2026-09-27T01:00:00.000001Z"
		x.EffectiveTo = &end
		check(t, []Entity{a, b}, []Claim{x, y}, ErrCycle)
	})
	machine, l1, l2 := entity("asset", "machine"), entity("asset", "line"), entity("asset", "line")
	for _, tc := range []struct {
		name   string
		claims []Claim
		want   error
	}{
		{"two lines", []Claim{edge(machine, l1, "part_of_asset", 0, 3), edge(machine, l2, "part_of_asset", 2, 4)}, ErrReviewBlocked},
		{"line handover", []Claim{edge(machine, l1, "part_of_asset", 0, 2), edge(machine, l2, "part_of_asset", 2, 4)}, nil},
		{"same line", []Claim{edge(machine, l1, "part_of_asset", 0, 3), edge(machine, l1, "part_of_asset", 2, 4)}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) { check(t, []Entity{machine, l1, l2}, tc.claims, tc.want) })
	}
	t.Run("location cycle", func(t *testing.T) {
		x, y := entity("area", ""), entity("area", "")
		check(t, []Entity{x, y}, []Claim{edge(x, y, "located_in", 0, 3), edge(y, x, "located_in", 1, 4)}, ErrCycle)
	})
	for _, tc := range []string{"missing entity", "entity revision", "claim revision", "duplicate claim", "duplicate entity", "invalid interval", "legacy pin", "wrong asset"} {
		t.Run(tc, func(t *testing.T) {
			entities := []Entity{a, b}
			claims := []Claim{edge(a, b, "precedes", 0, 3)}
			switch tc {
			case "missing entity":
				entities = entities[:1]
			case "entity revision":
				entities[0].Revision = 2
			case "claim revision":
				claims[0].Revision = 0
			case "duplicate claim":
				claims = append(claims, claims[0])
			case "duplicate entity":
				entities = append(entities, a)
			case "invalid interval":
				claims[0].EffectiveFrom = stamp(4)
			case "legacy pin":
				claims[0].SubjectRevision = 0
			case "wrong asset":
				entities = []Entity{l1, l2}
				claims = []Claim{edge(l1, l2, "part_of_asset", 0, 3)}
			}
			check(t, entities, claims, ErrInvalidContract)
		})
	}
	t.Run("work budget fails closed", func(t *testing.T) {
		if err := validateApprovedGraph([]Entity{a, b}, []Claim{edge(a, b, "precedes", 0, -1)}, 3); !errors.Is(err, ErrTraversalLimit) {
			t.Fatalf("budget error=%v", err)
		}
	})
	t.Run("empty graph", func(t *testing.T) { check(t, nil, nil, nil) })
	t.Run("bounds and dense DAG", func(t *testing.T) {
		entities := make([]Entity, 200)
		for i := range entities {
			entities[i] = entity("operation", "")
		}
		claims := make([]Claim, 0, 500)
		for i := 0; len(claims) < 500; i++ {
			for j := i + 1; j < len(entities) && len(claims) < 500; j++ {
				claims = append(claims, edge(entities[i], entities[j], "precedes", 0, -1))
			}
		}
		check(t, entities, claims, nil)
		// Distinct microsecond starts exercise the maximum checkpoint count.
		for i := range claims {
			claims[i].EffectiveFrom = fmt.Sprintf("2026-09-27T00:00:00.%06dZ", i)
		}
		check(t, entities, claims, nil)
		check(t, entities, append(claims, edge(entities[0], entities[199], "precedes", 0, -1)), ErrTraversalLimit)
		check(t, append(entities, entity("operation", "")), claims, ErrTraversalLimit)
	})
}
