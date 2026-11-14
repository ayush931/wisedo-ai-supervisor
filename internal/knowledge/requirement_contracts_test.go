package knowledge

import (
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"strings"
	"testing"
)

func TestRequirementContracts(t *testing.T) {
	base := RequirementDecisionInput{InventoryRevision: 1, Verdict: "bind", Reason: "Synthetic binding", SnapshotID: uuid.NewString(), AsOf: "2026-09-27T02:00:00+01:00", ClaimRefs: []ConflictClaimRef{{ClaimID: uuid.NewString(), Revision: 1}}}
	if err := base.Validate(); err != nil || base.AsOf != "2026-09-27T01:00:00Z" {
		t.Fatalf("normalize=%+v %v", base, err)
	}
	for _, edit := range []func(*RequirementDecisionInput){func(v *RequirementDecisionInput) { v.InventoryRevision = 0 }, func(v *RequirementDecisionInput) { v.Reason = " " }, func(v *RequirementDecisionInput) { v.Reason = strings.Repeat("界", 1001) }, func(v *RequirementDecisionInput) { v.Verdict = "approve" }, func(v *RequirementDecisionInput) { v.ClaimRefs = append(v.ClaimRefs, v.ClaimRefs[0]) }, func(v *RequirementDecisionInput) { v.ClaimRefs = nil }, func(v *RequirementDecisionInput) { v.SnapshotID = "" }, func(v *RequirementDecisionInput) { v.AsOf = "now" }, func(v *RequirementDecisionInput) { v.Verdict = "not_applicable" }} {
		v := base
		edit(&v)
		if err := v.Validate(); !errors.Is(err, ErrInvalidContract) {
			t.Fatalf("error=%v", err)
		}
	}
	for _, verdict := range []string{"not_applicable", "withdraw"} {
		v := RequirementDecisionInput{InventoryRevision: 1, Verdict: verdict, Reason: "Explicit decision"}
		if err := v.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	var v RequirementDecisionInput
	if err := json.Unmarshal([]byte(`{"inventory_revision":1,"verdict":"not_applicable","reason":"test","ready":true}`), &v); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("accepted authority field: %v", err)
	}
}

func TestRequirementBinding(t *testing.T) {
	entity := func(kind, asset string) SnapshotEntity {
		return SnapshotEntity{EntityInput: EntityInput{EntityID: uuid.NewString(), Type: kind, AssetKind: asset, Name: "Synthetic", SourceID: uuid.NewString(), SourceRevision: 1}, Revision: 1}
	}
	area, process, asset, op, op2, role := entity("area", ""), entity("process", ""), entity("asset", "machine"), entity("operation", ""), entity("operation", ""), entity("role", "")
	edge := func(a, b SnapshotEntity, p string) SnapshotClaim {
		return SnapshotClaim{ClaimInput: ClaimInput{ClaimID: uuid.NewString(), SubjectID: a.EntityID, SubjectRevision: 1, ObjectEntityID: b.EntityID, ObjectRevision: 1, Predicate: p, EffectiveFrom: "2026-09-27T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{uuid.NewString()}}, Revision: 1}
	}
	located, performs, part, part2, precedes, responsible := edge(asset, area, "located_in"), edge(asset, op, "performs"), edge(op, process, "part_of"), edge(op2, process, "part_of"), edge(op, op2, "precedes"), edge(process, role, "responsible_role")
	inv := DiscoveryInventory{Revision: 1, AreaID: area.EntityID, AreaRevision: 1, ProcessID: process.EntityID, ProcessRevision: 1}
	snapshot := Snapshot{SnapshotID: uuid.NewString(), Contents: SnapshotContents{Entities: []SnapshotEntity{area, process, asset, op, op2, role}, Claims: []SnapshotClaim{located, performs, part, part2, precedes, responsible}}}
	input := func(claims ...SnapshotClaim) RequirementDecisionInput {
		v := RequirementDecisionInput{InventoryRevision: 1, Verdict: "bind", Reason: "Reviewed synthetic requirement", SnapshotID: snapshot.SnapshotID, AsOf: "2026-09-27T01:00:00Z"}
		for _, c := range claims {
			v.ClaimRefs = append(v.ClaimRefs, ConflictClaimRef{ClaimID: c.ClaimID, Revision: c.Revision})
		}
		return v
	}
	check := func(t *testing.T, key string, s Snapshot, v RequirementDecisionInput, want error) {
		t.Helper()
		before, _ := json.Marshal([]any{inv, s, v})
		err := ValidateRequirementBinding(inv, key, s, v)
		if !errors.Is(err, want) {
			t.Fatalf("got%v want%v", err, want)
		}
		after, _ := json.Marshal([]any{inv, s, v})
		if string(before) != string(after) {
			t.Fatal("input mutated")
		}
	}
	t.Run("single operation", func(t *testing.T) {
		for _, key := range []string{"asset_identity", "asset_process_routing"} {
			check(t, key, snapshot, input(located, performs, part), nil)
		}
	})
	t.Run("connected routing", func(t *testing.T) {
		check(t, "asset_process_routing", snapshot, input(located, performs, part, part2, precedes), nil)
	})
	t.Run("direct responsible role", func(t *testing.T) {
		s := snapshot
		s.Contents.Entities = []SnapshotEntity{process, role}
		check(t, "responsible_role", s, input(responsible), nil)
	})
	t.Run("unsupported", func(t *testing.T) { check(t, "order_window", snapshot, input(located), ErrBindingUnsupported) })
	t.Run("disconnected operation", func(t *testing.T) {
		check(t, "asset_process_routing", snapshot, input(located, performs, part, part2), ErrBindingBlocked)
	})
	t.Run("missing witness", func(t *testing.T) { check(t, "asset_identity", snapshot, input(located, performs), ErrBindingBlocked) })
	t.Run("irrelevant selected claim", func(t *testing.T) {
		check(t, "asset_identity", snapshot, input(located, performs, part, responsible), ErrBindingBlocked)
	})
	t.Run("wrong scope", func(t *testing.T) {
		s := snapshot
		s.Contents.Claims = append([]SnapshotClaim(nil), snapshot.Contents.Claims...)
		s.Contents.Claims[0].ObjectEntityID = process.EntityID
		check(t, "asset_identity", s, input(located, performs, part), ErrBindingBlocked)
	})
	t.Run("wrong revision", func(t *testing.T) {
		v := input(responsible)
		v.ClaimRefs[0].Revision = 2
		check(t, "responsible_role", snapshot, v, ErrBindingBlocked)
		s := snapshot
		s.Contents.Entities = append([]SnapshotEntity(nil), snapshot.Contents.Entities...)
		s.Contents.Entities[1].Revision = 2
		check(t, "responsible_role", s, input(responsible), ErrBindingBlocked)
	})
	t.Run("effective end excluded", func(t *testing.T) {
		s := snapshot
		s.Contents.Claims = []SnapshotClaim{responsible}
		end := "2026-09-27T01:00:00Z"
		s.Contents.Claims[0].EffectiveTo = &end
		check(t, "responsible_role", s, input(responsible), ErrBindingBlocked)
	})
	t.Run("multiple assets require all witnesses", func(t *testing.T) {
		a2 := entity("asset", "machine")
		l2, p2 := edge(a2, area, "located_in"), edge(a2, op, "performs")
		s := snapshot
		s.Contents.Entities = append(append([]SnapshotEntity(nil), snapshot.Contents.Entities...), a2)
		s.Contents.Claims = append(append([]SnapshotClaim(nil), snapshot.Contents.Claims...), l2, p2)
		check(t, "asset_identity", s, input(located, performs, part, l2, p2), nil)
		check(t, "asset_identity", s, input(located, performs, part, p2), ErrBindingBlocked)
	})
	t.Run("disconnected routing island", func(t *testing.T) {
		o3 := entity("operation", "")
		p3 := edge(o3, process, "part_of")
		isolated := edge(op2, o3, "precedes")
		s := snapshot
		s.Contents.Entities = append(append([]SnapshotEntity(nil), snapshot.Contents.Entities...), o3)
		s.Contents.Claims = append(append([]SnapshotClaim(nil), snapshot.Contents.Claims...), p3, isolated)
		check(t, "asset_process_routing", s, input(located, performs, part, part2, p3, isolated), ErrBindingBlocked)
	})
	t.Run("claim entity pin mismatch", func(t *testing.T) {
		s := snapshot
		s.Contents.Claims = []SnapshotClaim{responsible}
		s.Contents.Claims[0].SubjectRevision = 2
		check(t, "responsible_role", s, input(responsible), ErrBindingBlocked)
	})
	t.Run("operation role unsupported", func(t *testing.T) {
		c := edge(op, role, "responsible_role")
		s := snapshot
		s.Contents.Claims = []SnapshotClaim{c}
		check(t, "responsible_role", s, input(c), ErrBindingBlocked)
	})
}

// TestValueStreamBinding (WIS-615): each process_understanding cell binds a witness holding its
// required predicates, connected to the interviewed process; a missing or foreign predicate, an
// entity not linked to the process, another process, the wrong inspection subject or stored goods
// nothing produces are blocked, and the input is never mutated.
func TestValueStreamBinding(t *testing.T) {
	entity := func(kind string) SnapshotEntity {
		return SnapshotEntity{EntityInput: EntityInput{EntityID: uuid.NewString(), Type: kind, Name: "Synthetic " + kind, SourceID: uuid.NewString(), SourceRevision: 1}, Revision: 1}
	}
	process, other := entity("process"), entity("process")
	press, pack, foreignOp := entity("operation"), entity("operation"), entity("operation")
	coil, bracket := entity("material"), entity("material")
	mill, oem, role := entity("supplier"), entity("customer"), entity("role")
	dock, rack, fg := entity("location"), entity("location"), entity("location")
	gate, pressQC := entity("inspection_point"), entity("inspection_point")
	claim := func(subject SnapshotEntity, predicate string) ClaimInput {
		return ClaimInput{ClaimID: uuid.NewString(), SubjectID: subject.EntityID, SubjectRevision: 1, Predicate: predicate, EffectiveFrom: "2026-09-27T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{uuid.NewString()}}
	}
	edge := func(a SnapshotEntity, p string, b SnapshotEntity) SnapshotClaim {
		c := claim(a, p)
		c.ObjectEntityID, c.ObjectRevision = b.EntityID, 1
		return SnapshotClaim{ClaimInput: c, Revision: 1}
	}
	literal := func(a SnapshotEntity, p, kind, value, unit string) SnapshotClaim {
		c := claim(a, p)
		raw, _ := json.Marshal(value)
		c.Literal = &Literal{Type: kind, Value: raw, Unit: unit}
		return SnapshotClaim{ClaimInput: c, Revision: 1}
	}
	pressPart, packPart, foreignPart := edge(press, "part_of", process), edge(pack, "part_of", process), edge(foreignOp, "part_of", other)
	precedes, consumes, produces := edge(press, "precedes", pack), edge(press, "consumes", coil), edge(pack, "produces", bracket)
	supplied, lead := edge(coil, "supplied_by", mill), literal(coil, "lead_time_days", "decimal", "14", "day")
	received, coilInspected, pressInspected := edge(coil, "received_at", dock), edge(coil, "inspected_at", gate), edge(press, "inspected_at", pressQC)
	method := literal(gate, "inspection_method", "string", "Visual check of coil edges", "")
	coilStored, bracketStored, ships := edge(coil, "stored_at", rack), edge(bracket, "stored_at", fg), edge(bracket, "ships_to", oem)
	cycle, setup := literal(press, "cycle_time_seconds", "decimal", "42", "s"), literal(press, "setup_time_seconds", "decimal", "900", "s")
	packed := literal(pack, "actual_step", "string", "Pack 20 brackets per box", "")
	responsible := edge(process, "responsible_role", role)
	// Claims about another process's operation that reach this process only through a shared
	// material or a precedes edge (audit defect: they bound as this process's evidence).
	foreignConsumes, foreignInspected := edge(foreignOp, "consumes", coil), edge(foreignOp, "inspected_at", pressQC)
	toForeign, foreignCycle := edge(press, "precedes", foreignOp), literal(foreignOp, "cycle_time_seconds", "decimal", "42", "s")
	all := []SnapshotClaim{pressPart, packPart, foreignPart, precedes, consumes, produces, supplied, lead, received, coilInspected, pressInspected, method, coilStored, bracketStored, ships, cycle, setup, packed, responsible,
		foreignConsumes, foreignInspected, toForeign, foreignCycle}
	snapshot := Snapshot{SnapshotID: uuid.NewString(), Contents: SnapshotContents{
		Entities: []SnapshotEntity{process, other, press, pack, foreignOp, coil, bracket, mill, oem, role, dock, rack, fg, gate, pressQC},
		Claims:   all}}
	inv := DiscoveryInventory{Revision: 1, AreaID: uuid.NewString(), AreaRevision: 1, ProcessID: process.EntityID, ProcessRevision: 1}
	input := func(claims ...SnapshotClaim) RequirementDecisionInput {
		v := RequirementDecisionInput{InventoryRevision: 1, Verdict: "bind", Reason: "Reviewed value stream", SnapshotID: snapshot.SnapshotID, AsOf: "2026-09-27T01:00:00Z"}
		for _, c := range claims {
			v.ClaimRefs = append(v.ClaimRefs, ConflictClaimRef{ClaimID: c.ClaimID, Revision: c.Revision})
		}
		return v
	}
	for _, c := range []struct {
		name, key string
		claims    []SnapshotClaim
		want      error
	}{
		{"suppliers", "material_suppliers", []SnapshotClaim{pressPart, consumes, supplied, lead}, nil},
		{"receiving", "receiving_inspection", []SnapshotClaim{pressPart, consumes, received, coilInspected, method}, nil},
		{"storage", "storage_locations", []SnapshotClaim{pressPart, consumes, coilStored}, nil},
		{"routing", "item_routing", []SnapshotClaim{pressPart, packPart, precedes}, nil},
		{"times", "cycle_setup_times", []SnapshotClaim{pressPart, cycle, setup}, nil},
		{"in-process quality", "in_process_quality", []SnapshotClaim{pressPart, pressInspected}, nil},
		{"packing", "packing_finished_goods", []SnapshotClaim{packPart, produces, bracketStored, packed}, nil},
		{"dispatch", "dispatch_delivery", []SnapshotClaim{packPart, produces, ships}, nil},
		{"responsibility", "stage_responsibility", []SnapshotClaim{responsible}, nil},

		{"required predicate missing", "material_suppliers", []SnapshotClaim{pressPart, consumes, lead}, ErrBindingBlocked},
		{"predicate of another stage", "material_suppliers", []SnapshotClaim{pressPart, consumes, supplied, coilStored}, ErrBindingBlocked},
		{"supplier not linked to the process", "material_suppliers", []SnapshotClaim{supplied, lead}, ErrBindingBlocked},
		{"operation of another process", "item_routing", []SnapshotClaim{pressPart, foreignPart}, ErrBindingBlocked},
		{"receiving inspection of an operation", "receiving_inspection", []SnapshotClaim{pressPart, consumes, received, pressInspected}, ErrBindingBlocked},
		{"in-process inspection of a material", "in_process_quality", []SnapshotClaim{pressPart, consumes, coilInspected}, ErrBindingBlocked},
		{"packed goods nothing produces", "packing_finished_goods", []SnapshotClaim{pressPart, consumes, packPart, produces, coilStored}, ErrBindingBlocked},
		{"times without a cycle time", "cycle_setup_times", []SnapshotClaim{pressPart, setup}, ErrBindingBlocked},
		// Audit defect (WIS-615): every operation a witness mentions must itself be part_of the
		// interviewed process; reaching it through a shared material or precedes is not enough.
		{"another process's inspection via a shared material", "in_process_quality", []SnapshotClaim{pressPart, consumes, foreignConsumes, foreignInspected}, ErrBindingBlocked},
		{"another process's cycle time via precedes", "cycle_setup_times", []SnapshotClaim{pressPart, toForeign, foreignCycle}, ErrBindingBlocked},
		{"another process's operation in the routing via precedes", "item_routing", []SnapshotClaim{pressPart, toForeign}, ErrBindingBlocked},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := input(c.claims...)
			before, _ := json.Marshal([]any{inv, snapshot, v})
			if err := ValidateRequirementBinding(inv, c.key, snapshot, v); !errors.Is(err, c.want) {
				t.Fatalf("got %v want %v", err, c.want)
			}
			if after, _ := json.Marshal([]any{inv, snapshot, v}); string(before) != string(after) {
				t.Fatal("input mutated")
			}
		})
	}
	// The process pinned at another revision is not the interviewed process. The snapshot holds the
	// process at revision 1, so this is refused by the inventory pin check before any claim is read.
	t.Run("process at another revision", func(t *testing.T) {
		moved := inv
		moved.ProcessRevision = 2
		if err := ValidateRequirementBinding(moved, "stage_responsibility", snapshot, input(responsible)); !errors.Is(err, ErrBindingBlocked) {
			t.Fatalf("got %v", err)
		}
	})
	// WIS-637 review: a stored witness re-validated on read maps each refusal to its own coverage
	// reason; anything else (here the traversal limit) fails the read instead of inventing one.
	oversized := snapshot
	oversized.Contents.Entities = make([]SnapshotEntity, 201)
	noReason := input(pressPart, cycle)
	noReason.Reason = ""
	for _, c := range []struct {
		name, key string
		snapshot  Snapshot
		in        RequirementDecisionInput
		reason    string
		err       error
	}{
		{"still valid", "cycle_setup_times", snapshot, input(pressPart, cycle), "", nil},
		{"out-of-scope witness", "cycle_setup_times", snapshot, input(pressPart, toForeign, foreignCycle), "witness_scope_invalid", nil},
		{"contract no longer met", "cycle_setup_times", snapshot, noReason, "witness_contract_invalid", nil},
		{"cell no longer bindable", "order_window", snapshot, input(pressPart, cycle), "binding_unsupported", nil},
		{"other failure", "cycle_setup_times", oversized, input(pressPart, cycle), "", ErrTraversalLimit},
	} {
		t.Run("stored witness "+c.name, func(t *testing.T) {
			reason, err := storedWitnessReason(ValidateRequirementBinding(inv, c.key, c.snapshot, c.in))
			if reason != c.reason || !errors.Is(err, c.err) {
				t.Fatalf("got %q, %v; want %q, %v", reason, err, c.reason, c.err)
			}
		})
	}
	for _, key := range []string{"material_suppliers", "receiving_inspection", "storage_locations", "item_routing", "cycle_setup_times", "in_process_quality", "packing_finished_goods", "dispatch_delivery", "stage_responsibility"} {
		if !BindingSupported(key) {
			t.Errorf("%s: binding not supported", key)
		}
	}
	for _, key := range []string{"order_window", "good_count_source", "unknown"} {
		if BindingSupported(key) {
			t.Errorf("%s: binding supported", key)
		}
	}
}
