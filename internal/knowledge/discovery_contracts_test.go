package knowledge

import (
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestDiscoveryTemplate(t *testing.T) {
	base := []string{"asset_identity", "asset_process_routing", "responsible_role", "order_window", "target_rate_and_breaks", "good_count_source", "timestamp_unit_mapping", "evidence_freshness_policy", "exception_escalation_contact"}
	roles := []string{"supervisor", "supervisor", "owner", "supervisor", "supervisor", "operator", "supervisor", "owner", "owner"}
	for _, capability := range []string{"shortfall_investigation", "internal_task_coordination"} {
		t.Run(capability, func(t *testing.T) {
			got, err := DiscoveryTemplate(capability)
			if err != nil {
				t.Fatal(err)
			}
			keys := append([]string(nil), base...)
			expectedRoles := append([]string(nil), roles...)
			if capability == "internal_task_coordination" {
				keys = append(keys, "recipient_authority", "completion_evidence")
				expectedRoles = append(expectedRoles, "owner", "supervisor")
			}
			if len(got) != len(keys) || DiscoveryTemplateVersion != 1 {
				t.Fatalf("template=%+v version=%d", got, DiscoveryTemplateVersion)
			}
			for i, row := range got {
				if row.Key != keys[i] || row.InterviewRole != expectedRoles[i] || strings.TrimSpace(row.Description) == "" {
					t.Fatalf("cell%d=%+v", i, row)
				}
			}
			original := append([]DiscoveryRequirementDefinition(nil), got...)
			got[0].Key = "forged"
			got[1].Description = "forged"
			got[2].InterviewRole = "operator"
			fresh, err := DiscoveryTemplate(capability)
			if err != nil || !reflect.DeepEqual(fresh, original) {
				t.Fatal("caller modified future template")
			}
		})
	}
	if _, err := DiscoveryTemplate("unknown"); !errors.Is(err, ErrInvalidContract) {
		t.Fatal(err)
	}
}

// TestProcessUnderstandingTemplate (WIS-611): the value-stream template walks supplier to dispatch,
// and its key set is exactly the one migration 070's guard requires, so the database and the
// application can never disagree on a complete template.
func TestProcessUnderstandingTemplate(t *testing.T) {
	got, err := DiscoveryTemplate("process_understanding")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"material_suppliers", "receiving_inspection", "storage_locations", "item_routing", "cycle_setup_times", "in_process_quality", "packing_finished_goods", "dispatch_delivery", "stage_responsibility"}
	var keys []string
	for _, row := range got {
		if strings.TrimSpace(row.Description) == "" || (row.InterviewRole != "owner" && row.InterviewRole != "supervisor" && row.InterviewRole != "operator") {
			t.Fatalf("cell %+v", row)
		}
		keys = append(keys, row.Key)
	}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys %v, want the value stream in order %v", keys, want)
	}
	raw, err := os.ReadFile("../migrate/070_value_stream_vocabulary.sql")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`WHEN 'process_understanding' THEN ARRAY\[([^\]]*)\]`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("process_understanding key set not found in migration 070")
	}
	var sqlKeys []string
	for _, k := range regexp.MustCompile(`'([a-z_]+)'`).FindAllSubmatch(m[1], -1) {
		sqlKeys = append(sqlKeys, string(k[1]))
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, sqlKeys) {
		t.Fatalf("Go template %v, migration 070 guard %v", keys, sqlKeys)
	}
	in := DiscoveryInput{InventoryID: uuid.NewString(), AreaID: uuid.NewString(), AreaRevision: 1, ProcessID: uuid.NewString(), ProcessRevision: 1, Capability: "process_understanding", Reason: "Onboard the press line"}
	if in.Validate() != nil {
		t.Fatal("process_understanding is a discovery capability")
	}
}

func TestDiscoveryInput(t *testing.T) {
	valid := DiscoveryInput{InventoryID: uuid.NewString(), AreaID: uuid.NewString(), AreaRevision: 1, ProcessID: uuid.NewString(), ProcessRevision: 1, Capability: "shortfall_investigation", Reason: "Define synthetic scope"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*DiscoveryInput)
	}{
		{"missing inventory", func(v *DiscoveryInput) { v.InventoryID = "" }},
		{"noncanonical UUID", func(v *DiscoveryInput) { v.AreaID = "00000000-0000-4000-8000-00000000000A" }},
		{"nil process", func(v *DiscoveryInput) { v.ProcessID = "00000000-0000-0000-0000-000000000000" }},
		{"area revision", func(v *DiscoveryInput) { v.AreaRevision = 0 }},
		{"process revision", func(v *DiscoveryInput) { v.ProcessRevision = -1 }},
		{"capability", func(v *DiscoveryInput) { v.Capability = "entire_site" }},
		{"blank reason", func(v *DiscoveryInput) { v.Reason = " \t\n" }},
		{"long reason", func(v *DiscoveryInput) { v.Reason = strings.Repeat("界", 1001) }},
		{"NUL reason", func(v *DiscoveryInput) { v.Reason = "a\x00b" }},
		{"invalid UTF8", func(v *DiscoveryInput) { v.Reason = string([]byte{255}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := valid
			tc.edit(&v)
			if err := v.Validate(); !errors.Is(err, ErrInvalidContract) {
				t.Fatalf("error=%v", err)
			}
		})
	}
	var absent *DiscoveryInput
	if err := absent.Validate(); !errors.Is(err, ErrInvalidContract) {
		t.Fatal(err)
	}
	boundary := valid
	boundary.Reason = strings.Repeat("界", 1000)
	if err := boundary.Validate(); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(valid)
	for _, field := range []string{`"requirements":[]`, `"weights":{}`, `"ready":true`, `"status":"approved"`} {
		var decoded DiscoveryInput
		raw := strings.TrimSuffix(string(body), "}") + "," + field + "}"
		if err := json.Unmarshal([]byte(raw), &decoded); !errors.Is(err, ErrInvalidContract) {
			t.Fatalf("accepted client authority field %s: %v", field, err)
		}
	}
	var decoded DiscoveryInput
	if err := json.Unmarshal(body, &decoded); err != nil || !reflect.DeepEqual(decoded, valid) {
		t.Fatalf("roundtrip=%+v %v", decoded, err)
	}
}
