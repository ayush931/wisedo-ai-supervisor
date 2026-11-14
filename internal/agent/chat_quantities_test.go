package agent

import (
	"encoding/json"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
)

func decimalClaim(id, predicate, value, unit string, spans ...string) knowledge.SnapshotClaim {
	raw, _ := json.Marshal(value)
	return knowledge.SnapshotClaim{ClaimInput: knowledge.ClaimInput{ClaimID: id, Predicate: predicate, SourceSpanIDs: spans,
		Literal: &knowledge.Literal{Type: "decimal", Value: raw, Unit: unit}}}
}

// WIS-640: a quantity resolves to exactly one standing approved claim whose value the statement
// states at a span it cites, and the reply carries the claim's own value and unit. Anything else
// refuses the whole answer with QUANTITY_NOT_APPROVED, never a partial result.
func TestResolveChatQuantities(t *testing.T) {
	const s1, s2, s3, s4 = "00000000-0000-4000-8000-0000000000a1", "00000000-0000-4000-8000-0000000000a2", "00000000-0000-4000-8000-0000000000a3", "00000000-0000-4000-8000-0000000000a4"
	standing := []knowledge.SnapshotClaim{
		decimalClaim("c-cycle", "cycle_time_seconds", "45", "s", s1),
		decimalClaim("c-sample", "sample_size", "8", "count", s2),
		decimalClaim("c-yield", "yield_ratio", "0.97", "", s3),
		decimalClaim("c-a", "setup_time_seconds", "7", "s", s4),
		decimalClaim("c-b", "cycle_time_seconds", "7", "s", s4),
	}
	stmt := func(kind, text string, spans []string, qs ...ChatQuantity) ChatStatement {
		return ChatStatement{Kind: kind, Text: text, SourceSpanIDs: spans, Quantities: qs}
	}
	answer := func(ss ...ChatStatement) ChatAnswerOutputV2 {
		return ChatAnswerOutputV2{Status: "answered", Statements: ss}
	}
	many := make([]ChatQuantity, maxChatQuantities+1)
	for i := range many {
		many[i] = ChatQuantity{SpanID: s1, Value: "45"}
	}
	for _, c := range []struct {
		name string
		out  ChatAnswerOutputV2
		want []ChatAnswerQuantity // nil: refused
	}{
		{"cycle time", answer(stmt("approved_fact", "The press takes 45 seconds a part.", []string{s1}, ChatQuantity{s1, "45"})),
			[]ChatAnswerQuantity{{Statement: 0, ClaimID: "c-cycle", Predicate: "cycle_time_seconds", Value: "45", Unit: "s", SpanID: s1}}},
		{"a count with its noun", answer(stmt("reported_observation", "Incoming QC samples 8 pieces from each lot.", []string{s2}, ChatQuantity{s2, "8"})),
			[]ChatAnswerQuantity{{Statement: 0, ClaimID: "c-sample", Predicate: "sample_size", Value: "8", Unit: "count", SpanID: s2}}},
		{"an equal decimal gets the claim's own value", answer(stmt("approved_fact", "The booth yield ratio is 0.970.", []string{s3}, ChatQuantity{s3, "0.970"})),
			[]ChatAnswerQuantity{{Statement: 0, ClaimID: "c-yield", Predicate: "yield_ratio", Value: "0.97", Unit: "", SpanID: s3}}},
		{"second statement", answer(stmt("approved_fact", "Steel coil comes from the mill.", []string{s1}), stmt("approved_fact", "The press takes 45 seconds a part.", []string{s1}, ChatQuantity{s1, "45"})),
			[]ChatAnswerQuantity{{Statement: 1, ClaimID: "c-cycle", Predicate: "cycle_time_seconds", Value: "45", Unit: "s", SpanID: s1}}},
		{"no quantities", answer(stmt("approved_fact", "The press takes 45 seconds a part.", []string{s1})), []ChatAnswerQuantity{}},
		// Negative controls.
		{"wrong value", answer(stmt("approved_fact", "The press takes 46 seconds a part.", []string{s1}, ChatQuantity{s1, "46"})), nil},
		{"value not stated in the text", answer(stmt("approved_fact", "The press takes forty-five seconds a part.", []string{s1}, ChatQuantity{s1, "45"})), nil},
		{"span the statement does not cite", answer(stmt("approved_fact", "The press takes 45 seconds a part.", []string{s2}, ChatQuantity{s1, "45"})), nil},
		{"span that is not the claim's", answer(stmt("approved_fact", "The press takes 45 seconds a part.", []string{s1, s2}, ChatQuantity{s2, "45"})), nil},
		{"value of no standing claim", answer(stmt("approved_fact", "The welder takes 50 seconds.", []string{s1}, ChatQuantity{s1, "50"})), nil},
		{"two claims fit: ambiguous", answer(stmt("approved_fact", "It takes 7 seconds.", []string{s4}, ChatQuantity{s4, "7"})), nil},
		{"a hypothesis never carries an approved value", answer(stmt("hypothesis", "The press takes 45 seconds a part.", []string{s1}, ChatQuantity{s1, "45"})), nil},
		{"non-canonical value", answer(stmt("approved_fact", "The press takes 045 seconds a part.", []string{s1}, ChatQuantity{s1, "045"})), nil},
		{"more than six decimals", answer(stmt("approved_fact", "Yield is 0.9700001.", []string{s3}, ChatQuantity{s3, "0.9700001"})), nil},
		{"exponent form", answer(stmt("approved_fact", "The press takes 45 seconds a part.", []string{s1}, ChatQuantity{s1, "4.5e1"})), nil},
		{"duplicate", answer(stmt("approved_fact", "The press takes 45 seconds a part.", []string{s1}, ChatQuantity{s1, "45"}, ChatQuantity{s1, "45"})), nil},
		{"too many", answer(stmt("approved_fact", "The press takes 45 seconds a part.", []string{s1}, many...)), nil},
		// Review of #289: a unit written after the number must agree with the claim's unit, so the
		// reply never shows an approved value its own sentence contradicts.
		{"minutes for a seconds claim", answer(stmt("approved_fact", "The press takes 45 minutes a part.", []string{s1}, ChatQuantity{s1, "45"})), nil},
		{"hours for a seconds claim", answer(stmt("approved_fact", "The press takes 45 h per part.", []string{s1}, ChatQuantity{s1, "45"})), nil},
		{"minutes for a count claim", answer(stmt("reported_observation", "Incoming QC samples 8 minutes per lot.", []string{s2}, ChatQuantity{s2, "8"})), nil},
		{"abbreviated seconds agree", answer(stmt("approved_fact", "The press takes 45 sec a part.", []string{s1}, ChatQuantity{s1, "45"})),
			[]ChatAnswerQuantity{{Statement: 0, ClaimID: "c-cycle", Predicate: "cycle_time_seconds", Value: "45", Unit: "s", SpanID: s1}}},
		{"no unit word after the number", answer(stmt("approved_fact", "Cycle time: 45.", []string{s1}, ChatQuantity{s1, "45"})),
			[]ChatAnswerQuantity{{Statement: 0, ClaimID: "c-cycle", Predicate: "cycle_time_seconds", Value: "45", Unit: "s", SpanID: s1}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, code := resolveChatQuantities(c.out, standing)
			if c.want == nil {
				if code != CodeQuantityNotApproved || got != nil {
					t.Fatalf("got %+v %q, want refused with %s", got, code, CodeQuantityNotApproved)
				}
				return
			}
			if code != "" || len(got) != len(c.want) {
				t.Fatalf("got %+v %q, want %+v", got, code, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("quantity %d = %+v, want %+v", i, got[i], c.want[i])
				}
			}
		})
	}
	// A claim whose approval no longer stands resolves nothing: commit passes only the standing set.
	if _, code := resolveChatQuantities(answer(stmt("approved_fact", "The press takes 45 seconds a part.", []string{s1}, ChatQuantity{s1, "45"})), standing[1:]); code != CodeQuantityNotApproved {
		t.Fatalf("withdrawn approval resolved: %q", code)
	}
}

// WIS-640: the response schema the providers enforce stays valid JSON and carries the new field.
func TestChatOutputSchemaListsQuantities(t *testing.T) {
	var schema struct {
		Properties struct {
			Statements struct {
				Items struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"items"`
			} `json:"statements"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(chatOutputSchema, &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if _, ok := schema.Properties.Statements.Items.Properties["quantities"]; !ok {
		t.Fatal("statement schema has no quantities")
	}
}
