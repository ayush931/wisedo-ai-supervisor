package knowledge

import (
	"errors"
	"sort"
)

var ErrBindingUnsupported = errors.New("requirement evidence binding is not supported")
var ErrBindingBlocked = errors.New("requirement evidence witness is incomplete or out of scope")

type VerificationRef struct {
	SourceID             string `json:"source_id"`
	VerificationID       string `json:"verification_id"`
	VerificationRevision int64  `json:"verification_revision"`
}
type RequirementDecisionInput struct {
	InventoryRevision       int64              `json:"inventory_revision"`
	Verdict                 string             `json:"verdict"`
	Reason                  string             `json:"reason"`
	SnapshotID              string             `json:"snapshot_id,omitempty"`
	AsOf                    string             `json:"as_of,omitempty"`
	ClaimRefs               []ConflictClaimRef `json:"claim_refs,omitempty"`
	FreshnessPolicyRevision int64              `json:"freshness_policy_revision,omitempty"`
	VerificationRefs        []VerificationRef  `json:"verification_refs,omitempty"`
}

func (in *RequirementDecisionInput) Validate() error {
	if in == nil || in.InventoryRevision < 1 || in.FreshnessPolicyRevision < 0 || !contractText(in.Reason, 1000) {
		return ErrInvalidContract
	}
	switch in.Verdict {
	case "not_applicable", "withdraw":
		if in.SnapshotID != "" || in.AsOf != "" || len(in.ClaimRefs) != 0 || in.FreshnessPolicyRevision != 0 || len(in.VerificationRefs) != 0 {
			return ErrInvalidContract
		}
		return nil
	case "bind":
	default:
		return ErrInvalidContract
	}
	if in.SnapshotID == "" && in.FreshnessPolicyRevision > 0 && in.AsOf == "" && len(in.ClaimRefs) == 0 && len(in.VerificationRefs) == 0 {
		return nil
	}
	if (in.FreshnessPolicyRevision == 0 && len(in.VerificationRefs) != 0) || (in.FreshnessPolicyRevision > 0 && (len(in.VerificationRefs) < 1 || len(in.VerificationRefs) > 500)) {
		return ErrInvalidContract
	}
	refs := map[string]bool{}
	for _, ref := range in.VerificationRefs {
		if !contractUUID(ref.SourceID) || !contractUUID(ref.VerificationID) || ref.VerificationRevision < 1 || refs[ref.SourceID] {
			return ErrInvalidContract
		}
		refs[ref.SourceID] = true
	}
	if len(in.VerificationRefs) > 0 {
		in.VerificationRefs = append([]VerificationRef(nil), in.VerificationRefs...)
		sort.Slice(in.VerificationRefs, func(i, j int) bool { return in.VerificationRefs[i].SourceID < in.VerificationRefs[j].SourceID })
	}
	if !contractUUID(in.SnapshotID) || len(in.ClaimRefs) < 1 || len(in.ClaimRefs) > 100 {
		return ErrInvalidContract
	}
	_, normalized, err := normalizedTime(in.AsOf)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, ref := range in.ClaimRefs {
		if !contractUUID(ref.ClaimID) || ref.Revision < 1 || seen[ref.ClaimID] {
			return ErrInvalidContract
		}
		seen[ref.ClaimID] = true
	}
	in.AsOf = normalized
	in.ClaimRefs = append([]ConflictClaimRef(nil), in.ClaimRefs...)
	sort.Slice(in.ClaimRefs, func(i, j int) bool { return in.ClaimRefs[i].ClaimID < in.ClaimRefs[j].ClaimID })
	return nil
}
func (in *RequirementDecisionInput) UnmarshalJSON(data []byte) error {
	type plain RequirementDecisionInput
	var out plain
	if err := contractDecode(data, &out); err != nil {
		return err
	}
	*in = RequirementDecisionInput(out)
	return in.Validate()
}

// valueStreamBinding is what a witness for one process_understanding cell (WIS-615) must hold: each
// required predicate at least once, and nothing outside required, allowed and the linking
// predicates that tie a stage's facts to the interviewed process.
type valueStreamBinding struct{ requires, allows []string }

var valueStreamBindings = map[string]valueStreamBinding{
	"material_suppliers":     {requires: []string{"supplied_by"}, allows: []string{"lead_time_days"}},
	"receiving_inspection":   {requires: []string{"received_at", "inspected_at"}, allows: []string{"inspection_method", "sample_size"}},
	"storage_locations":      {requires: []string{"stored_at"}},
	"item_routing":           {requires: []string{"part_of"}},
	"cycle_setup_times":      {requires: []string{"cycle_time_seconds"}, allows: []string{"setup_time_seconds", "batch_size"}},
	"in_process_quality":     {requires: []string{"inspected_at"}, allows: []string{"inspection_method", "sample_size", "yield_ratio"}},
	"packing_finished_goods": {requires: []string{"produces", "stored_at"}, allows: []string{"actual_step"}},
	"dispatch_delivery":      {requires: []string{"ships_to"}},
	"stage_responsibility":   {requires: []string{"responsible_role"}, allows: []string{"escalation_after_seconds"}},
}

// valueStreamLinks connect operations, materials and assets to the interviewed process.
var valueStreamLinks = []string{"part_of", "consumes", "produces", "precedes", "performs"}

// BindingSupported reports the requirement cells a reviewer can bind: claims from an approved
// snapshot, or for evidence_freshness_policy a policy alone.
func BindingSupported(key string) bool {
	switch key {
	case "asset_identity", "asset_process_routing", "responsible_role", "evidence_freshness_policy":
		return true
	}
	_, ok := valueStreamBindings[key]
	return ok
}

// ValidateRequirementBinding validates typed scope witnesses, never approval or
// freshness. The service owns current inventory/snapshot access and authority.
func ValidateRequirementBinding(inv DiscoveryInventory, key string, snapshot Snapshot, in RequirementDecisionInput) error {
	if err := in.Validate(); err != nil {
		return err
	}
	if in.Verdict != "bind" {
		return ErrInvalidContract
	}
	_, valueStream := valueStreamBindings[key]
	if key != "asset_identity" && key != "asset_process_routing" && key != "responsible_role" && !valueStream {
		return ErrBindingUnsupported
	}
	if in.InventoryRevision != inv.Revision || in.SnapshotID != snapshot.SnapshotID {
		return ErrBindingBlocked
	}
	if len(snapshot.Contents.Entities) > 200 || len(snapshot.Contents.Claims) > 500 {
		return ErrTraversalLimit
	}
	entities := map[string]SnapshotEntity{}
	for _, e := range snapshot.Contents.Entities {
		if _, exists := entities[e.EntityID]; exists {
			return ErrBindingBlocked
		}
		entities[e.EntityID] = e
	}
	process, ok := entities[inv.ProcessID]
	if !ok || process.Type != "process" || process.Revision != inv.ProcessRevision {
		return ErrBindingBlocked
	}
	if key != "responsible_role" && !valueStream {
		area, ok := entities[inv.AreaID]
		if !ok || area.Type != "area" || area.Revision != inv.AreaRevision {
			return ErrBindingBlocked
		}
	}
	available := map[string]SnapshotClaim{}
	for _, c := range snapshot.Contents.Claims {
		if _, exists := available[c.ClaimID]; exists {
			return ErrBindingBlocked
		}
		available[c.ClaimID] = c
	}
	at, _, _ := normalizedTime(in.AsOf)
	selected := make([]Claim, 0, len(in.ClaimRefs))
	graphEntities := map[string]Entity{}
	for _, ref := range in.ClaimRefs {
		c, ok := available[ref.ClaimID]
		if !ok || c.Revision != ref.Revision {
			return ErrBindingBlocked
		}
		lo, _, err := normalizedTime(c.EffectiveFrom)
		if err != nil || at.Before(lo) {
			return ErrBindingBlocked
		}
		if c.EffectiveTo != nil {
			hi, _, err := normalizedTime(*c.EffectiveTo)
			if err != nil || !hi.After(lo) || !at.Before(hi) {
				return ErrBindingBlocked
			}
		}
		for _, id := range []string{c.SubjectID, c.ObjectEntityID} {
			if id == "" {
				continue
			}
			e, ok := entities[id]
			if !ok {
				return ErrBindingBlocked
			}
			graphEntities[id] = Entity{EntityInput: e.EntityInput, Revision: e.Revision}
		}
		selected = append(selected, Claim{ClaimInput: c.ClaimInput, Revision: c.Revision})
	}
	typed := make([]Entity, 0, len(graphEntities))
	for _, e := range graphEntities {
		typed = append(typed, e)
	}
	if err := ValidateApprovedGraph(typed, selected); err != nil {
		return ErrBindingBlocked
	}
	if valueStream {
		return validateValueStreamWitness(key, inv, entities, selected)
	}
	if key == "responsible_role" {
		for _, c := range selected {
			if c.Predicate != "responsible_role" || c.SubjectID != inv.ProcessID || c.SubjectRevision != inv.ProcessRevision {
				return ErrBindingBlocked
			}
		}
		return nil
	}
	located := map[string]bool{}
	performs := map[string][]string{}
	operations := map[string]bool{}
	adjacent := map[string][]string{}
	assets := map[string]bool{}
	for _, c := range selected {
		switch c.Predicate {
		case "located_in":
			if entities[c.SubjectID].Type != "asset" || c.ObjectEntityID != inv.AreaID || c.ObjectRevision != inv.AreaRevision {
				return ErrBindingBlocked
			}
			located[c.SubjectID] = true
			assets[c.SubjectID] = true
		case "performs":
			performs[c.SubjectID] = append(performs[c.SubjectID], c.ObjectEntityID)
			assets[c.SubjectID] = true
		case "part_of":
			if c.ObjectEntityID != inv.ProcessID || c.ObjectRevision != inv.ProcessRevision {
				return ErrBindingBlocked
			}
			operations[c.SubjectID] = true
		case "precedes":
			if key != "asset_process_routing" {
				return ErrBindingBlocked
			}
			adjacent[c.SubjectID] = append(adjacent[c.SubjectID], c.ObjectEntityID)
			adjacent[c.ObjectEntityID] = append(adjacent[c.ObjectEntityID], c.SubjectID)
		default:
			return ErrBindingBlocked
		}
	}
	if len(assets) == 0 || len(operations) == 0 {
		return ErrBindingBlocked
	}
	reached := map[string]bool{}
	queue := []string{}
	for asset := range assets {
		if !located[asset] || len(performs[asset]) == 0 {
			return ErrBindingBlocked
		}
		for _, op := range performs[asset] {
			if !operations[op] {
				return ErrBindingBlocked
			}
			if !reached[op] {
				reached[op] = true
				queue = append(queue, op)
			}
		}
	}
	for op, neighbors := range adjacent {
		if !operations[op] {
			return ErrBindingBlocked
		}
		for _, other := range neighbors {
			if !operations[other] {
				return ErrBindingBlocked
			}
		}
	}
	for i := 0; i < len(queue); i++ {
		for _, other := range adjacent[queue[i]] {
			if !reached[other] {
				reached[other] = true
				queue = append(queue, other)
			}
		}
	}
	for op := range operations {
		if !reached[op] {
			return ErrBindingBlocked
		}
	}
	return nil
}

// validateValueStreamWitness checks a process_understanding binding: only the stage's predicates
// and the linking ones, every required predicate present, receiving inspection about a material and
// in-process inspection about an operation, packed goods stored only if a selected claim produces
// them, every operation selected as part_of the interviewed process, and every entity the claims
// mention connected to that process (at its pinned revision) through the selected claims.
func validateValueStreamWitness(key string, inv DiscoveryInventory, entities map[string]SnapshotEntity, claims []Claim) error {
	rule := valueStreamBindings[key]
	allowed := map[string]bool{}
	for _, list := range [][]string{valueStreamLinks, rule.requires, rule.allows} {
		for _, p := range list {
			allowed[p] = true
		}
	}
	present, produced := map[string]bool{}, map[string]bool{}
	neighbors := map[string][]string{}
	for _, c := range claims {
		if !allowed[c.Predicate] {
			return ErrBindingBlocked
		}
		present[c.Predicate] = true
		for _, pin := range []struct {
			id       string
			revision int64
		}{{c.SubjectID, c.SubjectRevision}, {c.ObjectEntityID, c.ObjectRevision}} {
			if pin.id != "" && entities[pin.id].Type == "process" && (pin.id != inv.ProcessID || pin.revision != inv.ProcessRevision) {
				return ErrBindingBlocked
			}
		}
		if c.Predicate == "produces" {
			produced[c.ObjectEntityID] = true
		}
		if c.ObjectEntityID != "" {
			neighbors[c.SubjectID] = append(neighbors[c.SubjectID], c.ObjectEntityID)
			neighbors[c.ObjectEntityID] = append(neighbors[c.ObjectEntityID], c.SubjectID)
		}
	}
	for _, p := range rule.requires {
		if !present[p] {
			return ErrBindingBlocked
		}
	}
	for _, c := range claims {
		switch {
		case c.Predicate == "inspected_at" && key == "receiving_inspection" && entities[c.SubjectID].Type != "material",
			c.Predicate == "inspected_at" && key == "in_process_quality" && entities[c.SubjectID].Type != "operation",
			c.Predicate == "stored_at" && key == "packing_finished_goods" && !produced[c.SubjectID]:
			return ErrBindingBlocked
		}
	}
	// Every operation the witness mentions must itself be part_of the interviewed process at its
	// pinned revision, by a selected claim. Reaching an operation only through a shared material,
	// location or a precedes edge would let another process's operation (its cycle time, its
	// inspection) bind as this process's evidence (WIS-615 audit), as asset_process_routing
	// already refuses.
	ownOperation := map[string]bool{}
	for _, c := range claims {
		if c.Predicate == "part_of" && entities[c.SubjectID].Type == "operation" && c.ObjectEntityID == inv.ProcessID && c.ObjectRevision == inv.ProcessRevision {
			ownOperation[c.SubjectID] = true
		}
	}
	for _, c := range claims {
		for _, id := range []string{c.SubjectID, c.ObjectEntityID} {
			if id != "" && entities[id].Type == "operation" && !ownOperation[id] {
				return ErrBindingBlocked
			}
		}
	}
	reached := map[string]bool{inv.ProcessID: true}
	queue := []string{inv.ProcessID}
	for i := 0; i < len(queue); i++ {
		for _, next := range neighbors[queue[i]] {
			if !reached[next] {
				reached[next] = true
				queue = append(queue, next)
			}
		}
	}
	for _, c := range claims {
		if !reached[c.SubjectID] || (c.ObjectEntityID != "" && !reached[c.ObjectEntityID]) {
			return ErrBindingBlocked
		}
	}
	return nil
}
