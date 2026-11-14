//go:build integration

package agent_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plantcontext"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
)

func TestWIS702RepeatedEvidenceFitsWithoutDroppingFacts(t *testing.T) {
	w := newWorld(t)
	step := agent.FixtureStep{Response: agent.ModelResponse{InputTokens: 900, OutputTokens: 500}}
	for i := 0; i < 12; i++ {
		step.Response.ToolCalls = append(step.Response.ToolCalls, agent.ToolCall{ID: fmt.Sprintf("context_%d", i), Name: agent.ToolContextGet, Arguments: json.RawMessage(fmt.Sprintf(`{"context_id":%q}`, w.contexts[1].ContextID))})
	}
	p := script(step, finalStep(t, w.result("success"), w.clarify(w.revision)))
	result := w.investigate(t, w.runtime(p, nil), w.revision)
	if result.Status != recovery.StatusNeedsInput || !reflect.DeepEqual(result.Reasons, []string{"CLARIFICATION_REQUESTED"}) {
		t.Fatalf("repeat reads did not reach a grounded result: %+v", result)
	}
	if len(p.requests) != 2 {
		t.Fatalf("provider requests=%d", len(p.requests))
	}
	var got plantcontext.Snapshot
	for _, m := range p.requests[1].Messages {
		if m.Role == "tool" && m.ToolCallID == "context_0" {
			if err := json.Unmarshal([]byte(m.Content), &got); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !reflect.DeepEqual(got, w.contexts[1]) {
		t.Fatal("required context facts were altered or truncated")
	}
	var reads int
	if err := w.control.QueryRowContext(w.ctx, `SELECT count(*) FROM supervisor.agent_tool_calls WHERE run_id=$1 AND status='ok'`, result.RunID).Scan(&reads); err != nil {
		t.Fatal(err)
	}
	if reads != 2 {
		t.Fatalf("fan-out executed %d reads, want two", reads)
	}
}

func TestWIS702InvestigationReadsOnlyPlanCitedPassage(t *testing.T) {
	w := newWorld(t)
	p := script(toolStep(agent.ToolEvidenceGet, fmt.Sprintf(`{"evidence_id":%q}`, w.schedule)), finalStep(t, w.result("success"), w.clarify(w.revision)))
	res := w.investigate(t, w.runtime(p, nil), w.revision)
	if res.Status != recovery.StatusNeedsInput || len(p.requests) != 2 {
		t.Fatalf("investigation: %+v", res)
	}
	for _, m := range p.requests[1].Messages {
		if m.Role != "tool" {
			continue
		}
		if strings.Contains(m.Content, injection) {
			t.Fatal("uncited source text reached the model")
		}
		var out struct {
			Passages []struct {
				SpanID string `json:"span_id"`
				Text   string `json:"untrusted_source"`
			} `json:"passages"`
		}
		if err := json.Unmarshal([]byte(m.Content), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Passages) != 1 || out.Passages[0].Text != "Order seven on press" || out.Passages[0].SpanID == "" {
			t.Fatalf("required cited passage not preserved: %+v", out)
		}
		return
	}
	t.Fatal("model never received the passage")
}

func TestWIS702DeferredReadRejectsForgedAuthority(t *testing.T) {
	w := newWorld(t)
	step := toolStep(agent.ToolMeasurementsGet, `{}`)
	step.Response.ToolCalls = append(step.Response.ToolCalls,
		agent.ToolCall{ID: "second", Name: agent.ToolMeasurementsGet, Arguments: json.RawMessage(`{}`)},
		agent.ToolCall{ID: "forged", Name: agent.ToolMeasurementsGet, Arguments: json.RawMessage(`{"tenant_id":"forged"}`)})
	res := w.investigate(t, w.runtime(script(step), nil), w.revision)
	if res.Status != recovery.StatusRefused || !reflect.DeepEqual(res.Reasons, []string{agent.CodeToolArgumentRejected}) {
		t.Fatalf("forged deferred read: %+v", res)
	}
	if got := w.row(t, res.RunID); got.toolCodes != "ok:,ok:,rejected:TOOL_ARGUMENT_REJECTED" {
		t.Fatalf("deferred refusal not audited: %s", got.toolCodes)
	}
}
