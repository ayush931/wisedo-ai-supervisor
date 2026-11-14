package workstation

import (
	"reflect"
	"testing"
)

// Citations name leaves by JSON pointer; nested arrays, empty containers and ~ / escaping must
// produce stable paths, and /schema_version is never citable.
func TestDraftLeavesNamesEveryCitablePointer(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       map[string]string
	}{
		{"routing", `{"schema_version":1,"line_id":"L","steps":[{"step_number":1,"workstation_id":"W"}]}`,
			map[string]string{"/line_id": `"L"`, "/steps/0/step_number": "1", "/steps/0/workstation_id": `"W"`}},
		{"escaping and empty containers", `{"a/b":true,"c~d":[],"e":{}}`,
			map[string]string{"/a~1b": "true", "/c~0d": "[]", "/e": "{}"}},
		{"numbers keep their literal", `{"x":2.50}`, map[string]string{"/x": "2.50"}},
	} {
		got, err := draftLeaves([]byte(tc.body))
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v %v, want %v", tc.name, got, err, tc.want)
		}
	}
}
