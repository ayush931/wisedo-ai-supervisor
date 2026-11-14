package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
)

// TestChatPassageManifests: the committed WIS-626 manifests seed only valid approved-claim shapes
// (WIS-626 review: the bound fixture used 38-span claims, which ClaimInput.Validate refuses). The
// bound manifest sits at 500 claims / 10,000 spans, where every claim cites 20 spans and none is
// selectable; the mixed manifest keeps a selectable claim size so the comparison is not vacuous.
func TestChatPassageManifests(t *testing.T) {
	for _, tc := range []struct {
		path           string
		claims, spans  int
		expectSelected bool
	}{
		{"../../deploy/qualification/chat-passages-load.json", 500, 10000, false},
		{"../../deploy/qualification/chat-passages-load-mixed.json", 500, 5500, true},
	} {
		m, _, err := loadPassageManifest(tc.path)
		if err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		if m.Claims != tc.claims || m.spans() != tc.spans || m.selects() != tc.expectSelected {
			t.Fatalf("%s: %d claims, %d spans, selects %v", tc.path, m.Claims, m.spans(), m.selects())
		}
		if m.Thresholds.IndexedP95Ms != 250 || m.Thresholds.SelectionMismatchesMax != 0 || m.Thresholds.CrossTenantRankedMax != 0 || m.Thresholds.ErrorsMax != 0 {
			t.Fatalf("%s: frozen thresholds changed: %+v", tc.path, m.Thresholds)
		}
	}
}

// TestChatPassageManifestRefusesInvalidClaimShapes: a manifest whose claims cite more spans than an
// approved claim may is refused before anything is measured (WIS-626 review).
func TestChatPassageManifestRefusesInvalidClaimShapes(t *testing.T) {
	m, _, err := loadPassageManifest("../../deploy/qualification/chat-passages-load-mixed.json")
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*PassageManifest){
		"38-span large claim":    func(m *PassageManifest) { m.LargeClaimSpans = 38 },
		"21-span large claim":    func(m *PassageManifest) { m.LargeClaimSpans = 21 },
		"100 foreign spans":      func(m *PassageManifest) { m.ForeignTenantSpans = 100 },
		"over 10,000 spans":      func(m *PassageManifest) { m.Claims = 1000 },
		"large below small":      func(m *PassageManifest) { m.LargeClaimSpans = 1 },
		"zero small claim spans": func(m *PassageManifest) { m.SmallClaimSpans = 0 },
	} {
		bad := m
		mutate(&bad)
		if bad.validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err = fixtureClaim("00000000-0000-4000-8000-000000000002", fixtureSpanIDs(20)); err != nil {
		t.Fatalf("20 spans: %v", err)
	}
	if _, err = fixtureClaim("00000000-0000-4000-8000-000000000002", fixtureSpanIDs(21)); !errors.Is(err, knowledge.ErrInvalidContract) {
		t.Fatalf("21 spans: %v, want ErrInvalidContract", err)
	}
}

// TestChatPassageBoundNeedsSelectingCompanion: a fixture where no claim fits the passage limit
// compares empty selections, so it is refused unless it names a companion that selects under the
// same frozen thresholds and reference host (WIS-626 review).
func TestChatPassageBoundNeedsSelectingCompanion(t *testing.T) {
	read := func(name string) map[string]any {
		raw, err := os.ReadFile("../../deploy/qualification/" + name)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err = json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	for name, mutate := range map[string]func(bound, mixed map[string]any){
		"committed pair":             func(bound, mixed map[string]any) {},
		"no companion":               func(bound, mixed map[string]any) { delete(bound, "selection_companion") },
		"missing companion file":     func(bound, mixed map[string]any) { bound["selection_companion"] = "absent.json" },
		"companion cannot select":    func(bound, mixed map[string]any) { mixed["small_claim_spans"] = 20 },
		"companion threshold looser": func(bound, mixed map[string]any) { mixed["thresholds"].(map[string]any)["indexed_p95_ms"] = 500 },
		"companion on a selecting fixture": func(bound, mixed map[string]any) {
			mixed["selection_companion"] = "chat-passages-load.json"
		},
		"companion cycle": func(bound, mixed map[string]any) {
			mixed["small_claim_spans"], mixed["selection_companion"] = 20, "chat-passages-load.json"
		},
	} {
		dir := t.TempDir()
		bound, mixed := read("chat-passages-load.json"), read("chat-passages-load-mixed.json")
		mutate(bound, mixed)
		for file, m := range map[string]map[string]any{"chat-passages-load.json": bound, "chat-passages-load-mixed.json": mixed} {
			raw, _ := json.Marshal(m)
			if err := os.WriteFile(filepath.Join(dir, file), raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		_, _, err := loadPassageManifest(filepath.Join(dir, "chat-passages-load.json"))
		if (name == "committed pair") != (err == nil) {
			t.Errorf("%s: err %v", name, err)
		}
	}
}
