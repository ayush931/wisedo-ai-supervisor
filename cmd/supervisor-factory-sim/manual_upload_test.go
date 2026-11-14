package main

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/chat"
)

// WIS-709 review finding 4: AC3 gates on the server-resolved quantity, not on the model's wording.
// The replies are the recorded AC3 answers of runs 20261111T160140Z (pilot fixture provider) and
// 20261111T162352Z (pinned local Qwen3.5-9B).
func TestWIS709ManualAnswerCheck(t *testing.T) {
	d, _, err := generateManuals(278, "2026-09-01")
	if err != nil {
		t.Fatal(err)
	}
	claim := d.id("manual-cycle/" + d.manifest.Lines[1].Stations[2].ID)
	b, err := os.ReadFile("testdata/wis709-ac3-recorded-replies.json")
	if err != nil {
		t.Fatal(err)
	}
	var recorded map[string]struct {
		ExpectedValue string       `json:"expected_value"`
		ExpectedSpan  string       `json:"expected_span"`
		Reply         chat.Message `json:"reply"`
	}
	if err = json.Unmarshal(b, &recorded); err != nil {
		t.Fatal(err)
	}
	local, fixture := recorded["local_qwen"], recorded["fixture_provider"]
	if local.ExpectedValue != "45" || len(local.Reply.Quantities) != 1 || local.Reply.Quantities[0].ClaimID != claim {
		t.Fatalf("generator no longer reproduces the recorded claim %s", claim)
	}
	if err = checkManualAnswer(local.Reply, claim, "45", local.ExpectedSpan); err != nil {
		t.Fatalf("correct local answer refused: %v", err)
	}
	// The replaced literal check refused this correct answer on wording alone.
	if strings.Contains(*local.Reply.Body, "Rated cycle time: 45 seconds.") {
		t.Fatal("recorded local answer no longer exercises the wording difference")
	}
	// Negative control: the fixture provider named Assembly HB-03 at 157 s with another span.
	if err = checkManualAnswer(fixture.Reply, claim, "45", fixture.ExpectedSpan); err == nil {
		t.Fatal("fixture-provider answer (wrong station and span) passed AC3")
	}
	body := func(s string) func(*chat.Message) { return func(m *chat.Message) { m.Body = &s } }
	quantity := func(f func(*agent.ChatAnswerQuantity)) func(*chat.Message) {
		return func(m *chat.Message) { f(&m.Quantities[0]) }
	}
	for name, mutate := range map[string]func(*chat.Message){
		"other claim":        quantity(func(q *agent.ChatAnswerQuantity) { q.ClaimID = d.id("other") }),
		"other value":        quantity(func(q *agent.ChatAnswerQuantity) { q.Value = "60" }),
		"other unit":         quantity(func(q *agent.ChatAnswerQuantity) { q.Unit = "min" }),
		"other span":         quantity(func(q *agent.ChatAnswerQuantity) { q.SpanID = fixture.ExpectedSpan }),
		"other predicate":    quantity(func(q *agent.ChatAnswerQuantity) { q.Predicate = "changeover_seconds" }),
		"no quantity":        func(m *chat.Message) { m.Quantities = nil },
		"duplicate quantity": func(m *chat.Message) { m.Quantities = append(m.Quantities, m.Quantities[0]) },
		"second cycle value": func(m *chat.Message) {
			q := m.Quantities[0]
			q.ClaimID, q.Value = d.id("other"), "157"
			m.Quantities = append(m.Quantities, q)
		},
		"body 145 seconds":              body("- [Approved process] The rated cycle time of CNC KN-03 is 145 seconds. [1]"),
		"body 450 seconds":              body("- [Approved process] The rated cycle time of CNC KN-03 is 450 seconds. [1]"),
		"body 145.45 seconds":           body("- [Approved process] The rated cycle time of CNC KN-03 is 145.45 seconds. [1]"),
		"body 0.45 seconds":             body("- [Approved process] The rated cycle time of CNC KN-03 is 0.45 seconds. [1]"),
		"body -45 seconds":              body("- [Approved process] The rated cycle time of CNC KN-03 is -45 seconds. [1]"),
		"body +45 seconds":              body("- [Approved process] The rated cycle time of CNC KN-03 is +45 seconds. [1]"),
		"body unicode minus 45 seconds": body("- [Approved process] The rated cycle time of CNC KN-03 is −45 seconds. [1]"),
		"body 45.0 seconds":             body("- [Approved process] The rated cycle time of CNC KN-03 is 45.0 seconds. [1]"),
		"body 1,045 seconds":            body("- [Approved process] The rated cycle time of CNC KN-03 is 1,045 seconds. [1]"),
		"body 045 seconds":              body("- [Approved process] The rated cycle time of CNC KN-03 is 045 seconds. [1]"),
		"body 45 secondsx":              body("- [Approved process] The rated cycle time of CNC KN-03 is 45 secondsx. [1]"),
		"body without unit":             body("- [Approved process] The rated cycle time of CNC KN-03 is 45. [1]"),
		"no body":                       func(m *chat.Message) { m.Body = nil },
		"needs input":                   func(m *chat.Message) { m.State = "needs_input" },
		"span not cited":                func(m *chat.Message) { m.Citations = nil },
		"other span cited":              func(m *chat.Message) { m.Citations[0].ID = fixture.ExpectedSpan },
		"span cited as non-span":        func(m *chat.Message) { m.Citations[0].Kind = "claim" },
	} {
		t.Run(name, func(t *testing.T) {
			m := local.Reply
			m.Quantities, m.Citations = slices.Clone(m.Quantities), slices.Clone(m.Citations)
			mutate(&m)
			if checkManualAnswer(m, claim, "45", local.ExpectedSpan) == nil {
				t.Fatal("mutated answer passed AC3")
			}
		})
	}
	// The refusal names the reason code, not a pointer (live run 20261111T221953Z printed 0x… ).
	asked := local.Reply
	asked.State, asked.Body = "needs_input", nil
	code := "MISSING_INPUT"
	asked.ReasonCode = &code
	if err = checkManualAnswer(asked, claim, "45", local.ExpectedSpan); err == nil || err.Error() != "manual chat did not answer: state=needs_input reason=MISSING_INPUT" {
		t.Fatalf("refusal message: %v", err)
	}
	// Case, the singular unit, a leading value and adjacent punctuation are accepted.
	for _, text := range []string{"CNC KN-03: 45 Second rated cycle.", "45 seconds is the rated cycle.", "Rated cycle (45 seconds).", "Rated cycle time: 45seconds"} {
		m := local.Reply
		m.Body = &text
		if err = checkManualAnswer(m, claim, "45", local.ExpectedSpan); err != nil {
			t.Fatalf("%q: %v", text, err)
		}
	}
}
