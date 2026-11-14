package knowledge

import (
	"encoding/json"
	"errors"
	"math/big"
	"reflect"
	"strings"
	"testing"
)

// WIS-642: the route summary of a process from an approved snapshot.
func TestRoute(t *testing.T) {
	id := func(n byte) string {
		return "00000000-0000-4000-8000-0000000000" + string([]byte{'0' + n/10, '0' + n%10})
	}
	const from = "2026-09-27T00:00:00Z"
	const process, area = 1, 2
	ent := func(n byte, typ, name string) SnapshotEntity {
		return SnapshotEntity{EntityInput: EntityInput{EntityID: id(n), Type: typ, Name: name}, Revision: 1}
	}
	// Operations 10..14, materials 20..21, suppliers 30..31.
	entities := []SnapshotEntity{ent(process, "process", "Bracket"), ent(area, "area", "Press area")}
	for i, name := range []string{"Blank", "Press", "Trim", "Pack", "Elsewhere"} {
		entities = append(entities, ent(byte(10+i), "operation", name))
	}
	entities = append(entities, ent(20, "material", "Coil"), ent(21, "material", "Box"), ent(30, "supplier", "Deccan Steel"), ent(31, "supplier", "Pune Packaging"))
	claimN := byte(40)
	edge := func(predicate string, a, b byte) SnapshotClaim {
		claimN++
		return SnapshotClaim{ClaimInput: ClaimInput{ClaimID: id(claimN), SubjectID: id(a), Predicate: predicate, ObjectEntityID: id(b), EffectiveFrom: from, SourceSpanIDs: []string{"span-" + id(claimN)}}, Revision: 1}
	}
	value := func(predicate string, a byte, v string) SnapshotClaim {
		claimN++
		return SnapshotClaim{ClaimInput: ClaimInput{ClaimID: id(claimN), SubjectID: id(a), Predicate: predicate, Literal: &Literal{Type: "decimal", Value: json.RawMessage(`"` + v + `"`)}, EffectiveFrom: from, SourceSpanIDs: []string{"span-" + id(claimN)}}, Revision: 1}
	}
	partOf := func(ops ...byte) []SnapshotClaim {
		var out []SnapshotClaim
		for _, o := range ops {
			out = append(out, edge("part_of", o, process))
		}
		return out
	}
	chain := func(ops ...byte) []SnapshotClaim {
		var out []SnapshotClaim
		for i := 0; i+1 < len(ops); i++ {
			out = append(out, edge("precedes", ops[i], ops[i+1]))
		}
		return out
	}
	times := func(op byte, cycle, setup string) []SnapshotClaim {
		var out []SnapshotClaim
		if cycle != "" {
			out = append(out, value("cycle_time_seconds", op, cycle))
		}
		if setup != "" {
			out = append(out, value("setup_time_seconds", op, setup))
		}
		return out
	}
	join := func(parts ...[]SnapshotClaim) []SnapshotClaim {
		var out []SnapshotClaim
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	timed := join(times(10, "60", "30"), times(11, "90.5", "30"), times(12, "120", "30"))
	supply := []SnapshotClaim{edge("consumes", 10, 20), edge("supplied_by", 20, 30), value("lead_time_days", 30, "14"), value("lead_time_days", 20, "7"), edge("consumes", 12, 21)}

	type want struct {
		readiness string
		reasons   []string
		order     []string // display names in position order; nil when not ordered
		cycle     *string
		setup     *string
		missing   int
		lead      *string
		leadFrom  string
		without   int
	}
	s := func(v string) *string { return &v }
	cases := []struct {
		name   string
		claims []SnapshotClaim
		want   want
	}{
		{"linear route sums exactly and cites the longest supplier lead time", join(partOf(10, 11, 12), chain(10, 11, 12), timed, supply),
			want{"READY", []string{}, []string{"Blank", "Press", "Trim"}, s("270.5"), s("90"), 0, s("14"), id(30), 1}},
		{"order comes from precedes, not from names or IDs", join(partOf(10, 11, 12), chain(12, 10, 11), timed),
			want{"READY", []string{}, []string{"Trim", "Blank", "Press"}, s("270.5"), s("90"), 0, nil, "", 0}},
		{"a single operation is a complete route", join(partOf(11), times(11, "0.1", "")),
			want{"READY", []string{}, []string{"Press"}, s("0.1"), nil, 0, nil, "", 0}},
		{"a gap is incomplete and has no totals", join(partOf(10, 11, 12), chain(10, 11), timed, supply),
			want{"NOT_READY", []string{RouteIncomplete}, nil, nil, nil, 0, nil, "", 1}},
		{"a branch is reported, never ordered", join(partOf(10, 11, 12), []SnapshotClaim{edge("precedes", 10, 11), edge("precedes", 10, 12)}, timed),
			want{"NOT_READY", []string{RouteBranched}, nil, nil, nil, 0, nil, "", 0}},
		{"a loop is a cycle", join(partOf(10, 11, 12), chain(10, 11, 12, 10), timed),
			want{"NOT_READY", []string{RouteCycle}, nil, nil, nil, 0, nil, "", 0}},
		{"a loop beside a chain is a cycle", join(partOf(10, 11, 12, 13), chain(10, 11), chain(12, 13, 12)),
			want{"NOT_READY", []string{RouteCycle}, nil, nil, nil, 4, nil, "", 0}},
		{"no operation is an empty route", join(chain(10, 11)),
			want{"NOT_READY", []string{RouteEmpty}, nil, nil, nil, 0, nil, "", 0}},
		{"an order edge to an operation of another process is reported", join(partOf(10, 11), chain(10, 11, 14), times(10, "1", "1"), times(11, "1", "1")),
			want{"NOT_READY", []string{RouteLeavesProcess}, nil, nil, nil, 0, nil, "", 0}},
		{"a missing cycle time is missing, never zero, and blocks only that total", join(partOf(10, 11, 12), chain(10, 11, 12), times(10, "60", "30"), times(11, "", "30"), times(12, "120", "30")),
			want{"READY", []string{}, []string{"Blank", "Press", "Trim"}, nil, s("90"), 1, nil, "", 0}},
		{"approved values that disagree are a conflict, not a pick", join(partOf(10, 11), chain(10, 11), times(10, "60", ""), times(10, "61", ""), times(11, "1", "")),
			want{"READY", []string{}, []string{"Blank", "Press"}, nil, nil, 1, nil, "", 0}},
		{"the same value approved twice is one value citing both", join(partOf(10, 11), chain(10, 11), times(10, "60.00", ""), times(10, "60", ""), times(11, "0.25", "")),
			want{"READY", []string{}, []string{"Blank", "Press"}, s("60.25"), nil, 0, nil, "", 0}},
		{"a material with no lead time is counted, a known one is the longest", join(partOf(10, 12), chain(10, 12), supply),
			want{"READY", []string{}, []string{"Blank", "Trim"}, nil, nil, 2, s("14"), id(30), 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			snap := Snapshot{SchemaVersion: 1, SnapshotID: id(90), SHA256: strings.Repeat("a", 64), InventoryRevision: 3, Contents: SnapshotContents{Entities: entities, Claims: c.claims}}
			before, _ := json.Marshal(snap)
			got, err := Route(snap, RouteQuery{ProcessID: id(process), AsOf: "2026-10-01T00:00:00+05:30"})
			if err != nil {
				t.Fatal(err)
			}
			if after, _ := json.Marshal(snap); string(after) != string(before) {
				t.Fatal("snapshot mutated")
			}
			if got.Readiness != c.want.readiness || !reflect.DeepEqual(got.ReasonCodes, c.want.reasons) {
				t.Fatalf("readiness %s %v, want %s %v", got.Readiness, got.ReasonCodes, c.want.readiness, c.want.reasons)
			}
			if got.AsOf != "2026-09-30T18:30:00Z" || got.ProcessName != "Bracket" {
				t.Fatalf("as_of %s name %s", got.AsOf, got.ProcessName)
			}
			var order []string
			for i, st := range got.Steps {
				if c.want.order == nil {
					if st.Position != nil {
						t.Fatalf("step %s has a position on an unordered route", st.DisplayName)
					}
					continue
				}
				if st.Position == nil || *st.Position != i+1 {
					t.Fatalf("step %d position %v", i, st.Position)
				}
				order = append(order, st.DisplayName)
			}
			if c.want.order != nil && !reflect.DeepEqual(order, c.want.order) {
				t.Fatalf("order %v, want %v", order, c.want.order)
			}
			if got.StepCount != len(got.Steps) {
				t.Fatalf("step_count %d, steps %d", got.StepCount, len(got.Steps))
			}
			eq := func(what string, a, b *string) {
				if (a == nil) != (b == nil) || (a != nil && *a != *b) {
					t.Fatalf("%s = %v, want %v", what, deref(a), deref(b))
				}
			}
			eq("cycle total", got.CycleTimeSeconds.Value, c.want.cycle)
			eq("setup total", got.SetupTimeSeconds.Value, c.want.setup)
			eq("lead time", got.LongestLeadTimeDays.Value, c.want.lead)
			if c.want.readiness == "READY" && got.CycleTimeSeconds.StepsMissing != c.want.missing {
				t.Fatalf("cycle steps missing %d, want %d", got.CycleTimeSeconds.StepsMissing, c.want.missing)
			}
			if c.want.lead != nil && (got.LongestLeadTimeDays.EntityID == nil || *got.LongestLeadTimeDays.EntityID != c.want.leadFrom || got.LongestLeadTimeDays.EntityName == nil || *got.LongestLeadTimeDays.EntityName != "Deccan Steel" || len(got.LongestLeadTimeDays.ClaimIDs) != 1 || len(got.LongestLeadTimeDays.SourceSpanIDs) != 1) {
				t.Fatalf("lead time cites %+v", got.LongestLeadTimeDays)
			}
			if got.LongestLeadTimeDays.MaterialsWithoutLeadTime != c.want.without {
				t.Fatalf("materials without lead time %d, want %d", got.LongestLeadTimeDays.MaterialsWithoutLeadTime, c.want.without)
			}
			// Every total that is shown cites the claims it came from (one per step value).
			if v := got.CycleTimeSeconds; v.Value != nil && len(v.ClaimIDs) < len(got.Steps) {
				t.Fatalf("cycle total cites %d claims for %d steps", len(v.ClaimIDs), len(got.Steps))
			}
		})
	}

	t.Run("step values: missing, conflict and known cite their claims and spans", func(t *testing.T) {
		snap := Snapshot{SnapshotID: id(90), Contents: SnapshotContents{Entities: entities, Claims: join(partOf(10, 11, 12), chain(10, 11, 12), times(10, "60", ""), times(11, "5", ""), times(11, "6", ""))}}
		got, err := Route(snap, RouteQuery{ProcessID: id(process), AsOf: "2026-10-01T00:00:00Z"})
		if err != nil {
			t.Fatal(err)
		}
		statuses := []string{got.Steps[0].CycleTimeSeconds.Status, got.Steps[1].CycleTimeSeconds.Status, got.Steps[2].CycleTimeSeconds.Status}
		if !reflect.DeepEqual(statuses, []string{RouteValueKnown, RouteValueConflict, RouteValueMissing}) {
			t.Fatalf("statuses %v", statuses)
		}
		if got.Steps[1].CycleTimeSeconds.Value != nil || len(got.Steps[1].CycleTimeSeconds.ClaimIDs) != 2 || len(got.Steps[1].CycleTimeSeconds.SourceSpanIDs) != 2 {
			t.Fatalf("conflict %+v", got.Steps[1].CycleTimeSeconds)
		}
		if got.Steps[2].CycleTimeSeconds.Value != nil || len(got.Steps[2].CycleTimeSeconds.ClaimIDs) != 0 {
			t.Fatalf("missing %+v", got.Steps[2].CycleTimeSeconds)
		}
	})

	t.Run("claims outside their effective interval do not count", func(t *testing.T) {
		ended := value("cycle_time_seconds", 10, "999")
		to := "2026-09-28T00:00:00Z"
		ended.EffectiveTo = &to
		later := value("cycle_time_seconds", 11, "1")
		later.EffectiveFrom = "2026-12-01T00:00:00Z"
		snap := Snapshot{SnapshotID: id(90), Contents: SnapshotContents{Entities: entities, Claims: join(partOf(10, 11), chain(10, 11), []SnapshotClaim{ended, later}, times(10, "60", ""))}}
		got, err := Route(snap, RouteQuery{ProcessID: id(process), AsOf: "2026-10-01T00:00:00Z"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Steps[0].CycleTimeSeconds.Status != RouteValueKnown || *got.Steps[0].CycleTimeSeconds.Value != "60" || got.Steps[1].CycleTimeSeconds.Status != RouteValueMissing {
			t.Fatalf("steps %+v", got.Steps)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		snap := Snapshot{SnapshotID: id(90), Contents: SnapshotContents{Entities: entities}}
		for name, q := range map[string]RouteQuery{"bad process id": {ProcessID: "x", AsOf: from}, "bad as_of": {ProcessID: id(process), AsOf: "2026-10-01"}} {
			if _, err := Route(snap, q); !errors.Is(err, ErrInvalidContract) {
				t.Fatalf("%s: %v", name, err)
			}
		}
		for name, q := range map[string]RouteQuery{"unknown entity": {ProcessID: id(99), AsOf: from}, "not a process": {ProcessID: id(area), AsOf: from}} {
			if _, err := Route(snap, q); !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s: %v", name, err)
			}
		}
	})
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func TestCanonicalDecimal(t *testing.T) {
	for in, want := range map[string]string{"270.50": "270.5", "0.1": "0.1", "90": "90", "0.000001": "0.000001", "12.300": "12.3"} {
		r, ok := new(big.Rat).SetString(in)
		if !ok {
			t.Fatal(in)
		}
		if got := canonicalDecimal(r); got != want {
			t.Fatalf("%s -> %s, want %s", in, got, want)
		}
	}
}
