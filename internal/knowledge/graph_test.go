package knowledge

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestGraph(t *testing.T) {
	id := func(n byte) string {
		return "00000000-0000-4000-8000-0000000000" + string([]byte{'0' + n/10, '0' + n%10})
	}
	entities := []SnapshotEntity{}
	for i := byte(1); i <= 4; i++ {
		entities = append(entities, SnapshotEntity{EntityInput: EntityInput{EntityID: id(i), Type: "operation", Name: "Synthetic"}, Revision: 1})
	}
	edge := func(n, a, b byte) SnapshotClaim {
		return SnapshotClaim{ClaimInput: ClaimInput{ClaimID: id(n), SubjectID: id(a), ObjectEntityID: id(b), Predicate: "precedes", EffectiveFrom: "2026-09-27T00:00:00Z"}, Revision: 1}
	}
	literal := func(n, a byte) SnapshotClaim {
		return SnapshotClaim{ClaimInput: ClaimInput{ClaimID: id(n), SubjectID: id(a), Predicate: "actual_step", Literal: &Literal{Type: "string", Value: json.RawMessage(`"Instruction"`)}, EffectiveFrom: "2026-09-27T00:00:00Z"}, Revision: 1}
	}
	snapshot := Snapshot{SchemaVersion: 1, SnapshotID: id(90), SHA256: strings.Repeat("a", 64), InventoryRevision: 10, Contents: SnapshotContents{Entities: entities, Claims: []SnapshotClaim{edge(10, 1, 2), edge(11, 1, 3), edge(12, 2, 4), edge(13, 3, 4), literal(14, 4)}}}
	query := GraphQuery{Direction: "outgoing", Roots: []string{id(1)}, Predicates: []string{"precedes", "actual_step"}, AsOf: "2026-09-27T01:00:00Z", Hops: 2, MaxEntities: 200, MaxClaims: 500}
	run := func(t *testing.T, s Snapshot, q GraphQuery) GraphResult {
		t.Helper()
		before, _ := json.Marshal(s)
		qb, _ := json.Marshal(q)
		body, e := Graph(s, q)
		if e != nil {
			t.Fatal(e)
		}
		if len(body) > 65536 {
			t.Fatal("oversized")
		}
		var result GraphResult
		if e = json.Unmarshal(body, &result); e != nil {
			t.Fatal(e)
		}
		after, _ := json.Marshal(s)
		qa, _ := json.Marshal(q)
		if string(before) != string(after) || string(qb) != string(qa) {
			t.Fatal("inputs mutated")
		}
		return result
	}
	t.Run("shared descendants and boundary literals", func(t *testing.T) {
		r := run(t, snapshot, query)
		if len(r.Entities) != 4 || len(r.Claims) != 5 || r.Truncated {
			t.Fatalf("result=%+v", r)
		}
	})
	t.Run("zero hops", func(t *testing.T) {
		q := query
		q.Hops = 0
		r := run(t, snapshot, q)
		if len(r.Entities) != 1 || len(r.Claims) != 0 || r.Truncated {
			t.Fatalf("result=%+v", r)
		}
	})
	t.Run("outgoing only", func(t *testing.T) {
		q := query
		q.Roots = []string{id(4)}
		r := run(t, snapshot, q)
		if len(r.Entities) != 1 || len(r.Claims) != 1 {
			t.Fatalf("result=%+v", r)
		}
	})
	t.Run("incoming preserves facts", func(t *testing.T) {
		q := query
		q.Direction = "incoming"
		q.Roots = []string{id(4)}
		q.Hops = 1
		r := run(t, snapshot, q)
		if len(r.Entities) != 3 || len(r.Claims) != 3 {
			t.Fatalf("incoming=%+v", r)
		}
		for _, c := range r.Claims {
			if c.ObjectEntityID != "" && (c.ObjectEntityID != id(4) || c.SubjectID == id(4)) {
				t.Fatal("inverse fact invented")
			}
		}
	})
	t.Run("both visits once", func(t *testing.T) {
		q := query
		q.Direction = "both"
		q.Roots = []string{id(2)}
		r := run(t, snapshot, q)
		if len(r.Entities) != 4 || len(r.Claims) != 5 || r.Truncated {
			t.Fatalf("both=%+v", r)
		}
	})
	t.Run("atomic count admission", func(t *testing.T) {
		q := query
		q.MaxEntities = 2
		r := run(t, snapshot, q)
		if !r.Truncated || len(r.Entities) != 2 || len(r.Claims) != 1 {
			t.Fatalf("result=%+v", r)
		}
		q = query
		q.MaxClaims = 1
		r = run(t, snapshot, q)
		if !r.Truncated || len(r.Claims) != 1 || len(r.Entities) != 2 {
			t.Fatalf("result=%+v", r)
		}
	})
	t.Run("order and dedup", func(t *testing.T) {
		q := query
		q.Roots = []string{id(1), id(1)}
		q.Predicates = []string{"precedes", "actual_step", "precedes"}
		a, e := Graph(snapshot, q)
		if e != nil {
			t.Fatal(e)
		}
		s := snapshot
		s.Contents.Entities = append([]SnapshotEntity(nil), entities...)
		s.Contents.Claims = append([]SnapshotClaim(nil), snapshot.Contents.Claims...)
		for i, j := 0, len(s.Contents.Claims)-1; i < j; i, j = i+1, j-1 {
			s.Contents.Claims[i], s.Contents.Claims[j] = s.Contents.Claims[j], s.Contents.Claims[i]
		}
		s.Contents.Entities[0], s.Contents.Entities[3] = s.Contents.Entities[3], s.Contents.Entities[0]
		b, e := Graph(s, query)
		if e != nil || string(a) != string(b) {
			t.Fatalf("order changed: %v", e)
		}
	})
	t.Run("half open canonical time", func(t *testing.T) {
		s := snapshot
		s.Contents.Claims = []SnapshotClaim{edge(10, 1, 2)}
		end := "2026-09-27T01:00:00Z"
		s.Contents.Claims[0].EffectiveTo = &end
		q := query
		q.AsOf = "2026-09-27T02:00:00+01:00"
		r := run(t, s, q)
		if len(r.Claims) != 0 || r.AsOf != end || r.Truncated {
			t.Fatalf("result=%+v", r)
		}
		q.AsOf = "2026-09-27T00:00:00Z"
		if len(run(t, s, q).Claims) != 1 {
			t.Fatal("start excluded")
		}
	})
	t.Run("invalid and missing roots", func(t *testing.T) {
		for _, mutate := range []func(*GraphQuery){func(q *GraphQuery) { q.Roots = nil }, func(q *GraphQuery) { q.Roots = make([]string, 21) }, func(q *GraphQuery) { q.Predicates = make([]string, 33) }, func(q *GraphQuery) { q.Predicates = []string{"invented"} }, func(q *GraphQuery) { q.Hops = 3 }, func(q *GraphQuery) { q.Direction = "sideways" }, func(q *GraphQuery) { q.MaxClaims = 0 }, func(q *GraphQuery) { q.MaxEntities = 201 }, func(q *GraphQuery) { q.AsOf = "now" }} {
			q := query
			mutate(&q)
			if _, e := Graph(snapshot, q); !errors.Is(e, ErrInvalidContract) {
				t.Fatalf("error=%v", e)
			}
		}
		q := query
		q.Roots = []string{id(80)}
		if _, e := Graph(snapshot, q); !errors.Is(e, ErrNotFound) {
			t.Fatal(e)
		}
		q.Roots = []string{id(1), id(2)}
		q.MaxEntities = 1
		if _, e := Graph(snapshot, q); !errors.Is(e, ErrTraversalLimit) {
			t.Fatal(e)
		}
	})
	t.Run("byte rejected edge does not leak endpoint or traversal", func(t *testing.T) {
		s := snapshot
		s.Contents.Entities = append([]SnapshotEntity(nil), entities...)
		s.Contents.Entities[1].Name = strings.Repeat("<世界>", 20000)
		s.Contents.Claims = []SnapshotClaim{edge(10, 1, 2), edge(11, 1, 3), edge(12, 2, 4), literal(14, 3)}
		r := run(t, s, query)
		if !r.Truncated || len(r.Entities) != 2 || len(r.Claims) != 2 {
			t.Fatalf("atomic result=%+v", r)
		}
		for _, e := range r.Entities {
			if e.EntityID == id(2) || e.EntityID == id(4) {
				t.Fatal("traversed omitted edge")
			}
		}
	})
	t.Run("exact bytes escaped unicode atomicity", func(t *testing.T) {
		s := snapshot
		s.Contents.Claims = []SnapshotClaim{literal(14, 1)}
		s.Contents.Entities = entities[:1]
		q := query
		base := run(t, s, q)
		encoded, _ := json.Marshal(base)
		padding := 65536 - len(encoded)
		s.Contents.Entities = append([]SnapshotEntity(nil), s.Contents.Entities...)
		s.Contents.Entities[0].Name += strings.Repeat("x", padding)
		body, e := Graph(s, q)
		if e != nil || len(body) != 65536 {
			t.Fatalf("exact limit bytes=%d err=%v", len(body), e)
		}
		s.Contents.Claims[0].Literal = &Literal{Type: "string", Value: json.RawMessage(`"<世界>"`)}
		r := run(t, s, q)
		if len(r.Claims) != 0 || !r.Truncated {
			t.Fatal("escaped byte overflow admitted")
		}
		s.Contents.Entities[0].Name += strings.Repeat("x", 1024)
		if _, e = Graph(s, q); !errors.Is(e, ErrTraversalLimit) {
			t.Fatalf("oversized roots=%v", e)
		}
	})
}
