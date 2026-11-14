package knowledge

const DiscoveryTemplateVersion = 1

type DiscoveryInput struct {
	InventoryID     string `json:"inventory_id"`
	AreaID          string `json:"area_id"`
	AreaRevision    int64  `json:"area_revision"`
	ProcessID       string `json:"process_id"`
	ProcessRevision int64  `json:"process_revision"`
	Capability      string `json:"capability"`
	Reason          string `json:"reason"`
}

func (in *DiscoveryInput) Validate() error {
	if in == nil || !contractUUID(in.InventoryID) || !contractUUID(in.AreaID) || !contractUUID(in.ProcessID) || in.AreaRevision < 1 || in.ProcessRevision < 1 || !contractText(in.Reason, 1000) {
		return ErrInvalidContract
	}
	if !knownCapability(in.Capability) {
		return ErrInvalidContract
	}
	return nil
}

// knownCapability lists the discovery capabilities. process_understanding (WIS-611) walks the whole
// value stream for onboarding; it enables nothing on its own, like every capability's coverage.
func knownCapability(c string) bool {
	return c == "shortfall_investigation" || c == "internal_task_coordination" || c == "process_understanding"
}

func (in *DiscoveryInput) UnmarshalJSON(data []byte) error {
	type plain DiscoveryInput
	var decoded plain
	if err := contractDecode(data, &decoded); err != nil {
		return err
	}
	*in = DiscoveryInput(decoded)
	return in.Validate()
}

// InterviewRole routes a question; it never confers review or approval rights.
// Definitions name required evidence, not automatic claim-matching rules.
type DiscoveryRequirementDefinition struct {
	Key           string `json:"key"`
	Description   string `json:"description"`
	InterviewRole string `json:"interview_role"`
}

// DiscoveryTemplate returns a new version-one inventory on every call. These
// cells are the WIS-265 denominator, not inferred completeness of the site.
func DiscoveryTemplate(capability string) ([]DiscoveryRequirementDefinition, error) {
	if !knownCapability(capability) {
		return nil, ErrInvalidContract
	}
	if capability == "process_understanding" {
		// One cell per stage of the value stream, from supplier to dispatch, each routed to the role
		// that usually knows it. Migration 070 holds the same key set.
		return []DiscoveryRequirementDefinition{
			{"material_suppliers", "Critical purchased materials, who supplies each one and the usual lead time.", "owner"},
			{"receiving_inspection", "Where incoming material is received and how it is inspected before use.", "supervisor"},
			{"storage_locations", "Where raw material, work in progress and finished goods are stored.", "operator"},
			{"item_routing", "The operations each product goes through, in order, and the line or station for each.", "supervisor"},
			{"cycle_setup_times", "Cycle time and setup or changeover time for each operation or station.", "operator"},
			{"in_process_quality", "Quality checks during production: where, how and how many are sampled.", "supervisor"},
			{"packing_finished_goods", "How finished products are packed and moved to finished-goods storage.", "operator"},
			{"dispatch_delivery", "How orders are dispatched and delivered to customers, and the delivery commitments.", "owner"},
			{"stage_responsibility", "The role responsible for each stage and who to escalate to when it goes wrong.", "owner"},
		}, nil
	}
	definitions := []DiscoveryRequirementDefinition{
		{"asset_identity", "Identity of the assets in the scoped process.", "supervisor"},
		{"asset_process_routing", "Asset assignments and routing for the scoped process.", "supervisor"},
		{"responsible_role", "Role responsible for the scoped process.", "owner"},
		{"order_window", "Production order and its applicable production window.", "supervisor"},
		{"target_rate_and_breaks", "Explicitly approved target-rate segments and planned breaks.", "supervisor"},
		{"good_count_source", "Source of good production counts for the scoped process.", "operator"},
		{"timestamp_unit_mapping", "Timestamp interpretation and unit mapping for operational inputs.", "supervisor"},
		{"evidence_freshness_policy", "Versioned policy governing evidence freshness.", "owner"},
		{"exception_escalation_contact", "Contact responsible for handling escalated exceptions.", "owner"},
	}
	if capability == "internal_task_coordination" {
		definitions = append(definitions,
			DiscoveryRequirementDefinition{"recipient_authority", "Approved task recipient and the authority for the internal task.", "owner"},
			DiscoveryRequirementDefinition{"completion_evidence", "Evidence required to establish completion of the internal task.", "supervisor"},
		)
	}
	return definitions, nil
}
