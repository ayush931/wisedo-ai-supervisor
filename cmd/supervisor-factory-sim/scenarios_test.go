package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/factorysim"
)

// answerKeySHA256 pins the frozen edge answer key (WIS-709 review finding 14). Changing an
// expectation changes this digest, so it needs a reviewed diff of this line.
const answerKeySHA256 = "9f72a06c841eee26712aa6c4c895d747c924029600a790e4a18d4a47b8f99392"

func TestWIS709ScenarioCatalogueAndKernels(t *testing.T) {
	d, err := generate(278, 1, "2026-09-01")
	if err != nil {
		t.Fatal(err)
	}
	if got := sha(wire(map[string]any{"product_cases": factorysim.Scenarios(), "facility_model_checks": factorysim.FacilityModelChecks()})); got != answerKeySHA256 {
		t.Fatalf("edge answer key changed without a reviewed golden update: %s", got)
	}
	seen := map[string]bool{}
	check := func(c factorysim.ScenarioOutcome) {
		if seen[c.ID] || c.ID == "" || c.Assertion == "" {
			t.Fatalf("invalid catalogue: %+v", c)
		}
		seen[c.ID] = true
		if c.Status == 0 && (c.Readiness == "" || c.Code == "") {
			t.Fatalf("missing HTTP outcome or readiness/reason: %s", c.ID)
		}
		if c.Layer == "http" {
			if c.Status < 200 {
				t.Fatalf("missing HTTP status: %s", c.ID)
			}
			return
		}
		t.Run(c.ID, func(t *testing.T) {
			o, e := scenarioKernel(c.ID, d)
			if e != nil || o.Code != c.Code || o.Readiness != c.Readiness {
				t.Fatalf("got %+v, %v; expected %+v", o, e, c)
			}
		})
	}
	for _, c := range factorysim.Scenarios() {
		if c.Layer == "facility model" {
			t.Fatalf("facility self-check counted as a product edge case: %s", c.ID)
		}
		check(c)
	}
	// AC2 counts product edge cases only; facility model self-checks are reported apart.
	if len(seen) != 20 {
		t.Fatalf("product catalogue has %d cases", len(seen))
	}
	for _, c := range factorysim.FacilityModelChecks() {
		if c.Layer != "facility model" {
			t.Fatalf("product case listed as a facility self-check: %s", c.ID)
		}
		check(c)
	}
	t.Run("counter-reset-normalization", func(t *testing.T) {
		o, err := scenarioKernel("counter-reset", d)
		if err != nil || o.Code != "COUNTER_DROP_NO_EVIDENCE" {
			t.Fatalf("reset normalization or missing-evidence control: %+v, %v", o, err)
		}
	})
}

// TestWIS709ScenarioMachineNameHasNoDigits: the scenario machine name reaches chat as approved
// passage text. Built from the hex entity ID, it carried exponent-like tokens ("265c13e1") that
// chat's number guard refuses, so revoked-document failed with STATEMENT_REJECTED on some runs
// (run 20261112T073838Z; the probe at 07:53:44Z and 07:53:47Z). The prefixes below are those runs'.
func TestWIS709ScenarioMachineNameHasNoDigits(t *testing.T) {
	d, err := generate(278, 1, "2026-09-01")
	if err != nil {
		t.Fatal(err)
	}
	prefixes := []string{"edge-20261112T075344.589334408-revoked-document", "edge-20261112T075347.483427102-revoked-document"}
	for i := 0; i < 200; i++ {
		prefixes = append(prefixes, "edge-20261112T0800"+strconv.Itoa(10+i%50)+"."+strconv.Itoa(100000000+i)+"-revoked-document")
	}
	seen := map[string]bool{}
	for _, p := range prefixes {
		tag := scenarioRunner{d: d, prefix: p}.machineTag()
		if len(tag) != 12 || strings.IndexFunc(tag, func(r rune) bool { return r < 'a' || r > 'p' }) >= 0 {
			t.Fatalf("%s: machine tag %q must be 12 letters a-p", p, tag)
		}
		seen[tag] = true
	}
	if len(seen) != len(prefixes) {
		t.Fatalf("machine tags collide: %d distinct of %d", len(seen), len(prefixes))
	}
}

// TestWIS709ScenarioManualPassageQuotesNoHexID (review of #304): the passage chat quotes, for the
// prefixes of the runs that failed, holds no digit other than the stated 45 seconds. Building the
// name from s.id("entity") again makes this fail.
func TestWIS709ScenarioManualPassageQuotesNoHexID(t *testing.T) {
	d, err := generate(278, 1, "2026-09-01")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"edge-20261112T075344.589334408-revoked-document", "edge-20261112T075347.483427102-revoked-document"} {
		machine, text := scenarioRunner{d: d, prefix: p}.manualPassage()
		if !strings.HasPrefix(text, machine+" ") || strings.ContainsAny(strings.Replace(text, "45 seconds", "", 1), "0123456789") {
			t.Fatalf("%s: passage %q must quote the machine name with no digits but the 45 s value", p, text)
		}
	}
}
