package workstation

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestMasterDataBodyValidationAndCanonicalOrdering(t *testing.T) {
	stationA, stationB := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	input := RoutingBodyV1{SchemaVersion: 1, DisplayName: "R-1", Active: true, LineID: stationA, ItemID: stationB,
		Steps: []RoutingStepV1{{StepNumber: 2, WorkstationID: stationB}, {StepNumber: 1, WorkstationID: stationA}}}
	got, err := normalizeMasterBody("routing", input)
	if err != nil || !reflect.DeepEqual([]int{got.Steps[0].StepNumber, got.Steps[1].StepNumber}, []int{1, 2}) {
		t.Fatalf("routing steps were not sorted: %+v, %v", got.Steps, err)
	}
	input.Steps[0].StepNumber = 1
	if _, err = normalizeMasterBody("routing", input); err == nil {
		t.Fatal("duplicate step numbers were accepted")
	}
	if _, err = normalizeMasterBody("item", ItemBodyV1{SchemaVersion: 1, DisplayName: "Part", Unit: "kg", Active: true}); err == nil {
		t.Fatal("unsupported item unit was accepted")
	}
	calendar := ShiftCalendarBodyV1{SchemaVersion: 1, DisplayName: "Day", TimeZone: "America/Detroit", Shifts: []ShiftCalendarShiftV1{{DayOfWeek: 1, StartLocal: "14:00", EndLocal: "16:00"}, {DayOfWeek: 1, StartLocal: "15:00", EndLocal: "17:00"}}}
	if _, err = normalizeMasterBody("shift_calendar", calendar); err == nil {
		t.Fatal("overlapping calendar shifts were accepted")
	}
	for _, zone := range []string{"", "Local", "EST"} {
		calendar.TimeZone = zone
		calendar.Shifts = []ShiftCalendarShiftV1{{DayOfWeek: 1, StartLocal: "07:00", EndLocal: "15:00"}}
		if _, err = normalizeMasterBody("shift_calendar", calendar); err == nil {
			t.Errorf("non-IANA timezone %q was accepted", zone)
		}
	}
}

func TestMasterDataSettingsBodyIsClosedAndRequiresFlag(t *testing.T) {
	for _, raw := range []string{
		`{"schema_version":1}`,
		`{"schema_version":1,"station_profile_line_validation_enabled":false,"unexpected":true}`,
	} {
		if _, err := DecodeMasterDataBody("site_settings", []byte(raw)); err == nil {
			t.Fatalf("invalid settings body was accepted: %s", raw)
		}
	}
	value, err := DecodeMasterDataBody("site_settings", []byte(`{"schema_version":1,"station_profile_line_validation_enabled":false}`))
	if err != nil {
		t.Fatal(err)
	}
	body := value.(*SiteMasterDataSettingsBodyV1)
	encoded, err := json.Marshal(body)
	if err != nil || string(encoded) != `{"schema_version":1,"station_profile_line_validation_enabled":false}` {
		t.Fatalf("site settings wire body changed: %s, %v", encoded, err)
	}
}

func TestMasterDataDisplayNamesRejectExecutableText(t *testing.T) {
	lineID, itemID, workstationID := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"
	validators := []struct {
		kind     string
		validate func(string) error
	}{
		{"line", func(name string) error {
			_, err := normalizeMasterBody("line", LineBodyV1{SchemaVersion: 1, DisplayName: name, Active: true})
			return err
		}},
		{"item", func(name string) error {
			_, err := normalizeMasterBody("item", ItemBodyV1{SchemaVersion: 1, DisplayName: name, Unit: MasterDataUnitCount, Active: true})
			return err
		}},
		{"routing", func(name string) error {
			_, err := normalizeMasterBody("routing", RoutingBodyV1{SchemaVersion: 1, DisplayName: name, Active: true, LineID: lineID, ItemID: itemID, Steps: []RoutingStepV1{{StepNumber: 1, WorkstationID: workstationID}}})
			return err
		}},
		{"shift_calendar", func(name string) error {
			_, err := normalizeMasterBody("shift_calendar", ShiftCalendarBodyV1{SchemaVersion: 1, DisplayName: name, TimeZone: "UTC", Shifts: []ShiftCalendarShiftV1{{DayOfWeek: 1, StartLocal: "07:00", EndLocal: "15:00"}}})
			return err
		}},
	}
	for _, tc := range validators {
		t.Run(tc.kind, func(t *testing.T) {
			for _, name := range []string{"=HYPERLINK(...)", "=1+1", "JavaScript:alert(1)", "{{x}}", "${x}", "a `b`"} {
				var invalid *MasterDataValidationError
				if err := tc.validate(name); !errors.As(err, &invalid) || invalid.Path != "display_name" {
					t.Errorf("display name %q: got %v, want display_name validation error", name, err)
				}
			}
			for _, name := range []string{"Torque > 25 Nm", "See https://sop.example", "-12.5 offset"} {
				if err := tc.validate(name); err != nil {
					t.Errorf("ordinary display name %q rejected: %v", name, err)
				}
			}
		})
	}
}
