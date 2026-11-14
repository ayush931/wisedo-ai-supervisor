//go:build integration

package agent_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
	"github.com/google/uuid"
)

func TestWIS702ReadTurnsAndPassageFallback(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spans int
	}{{"selected passage", 1}, {"nine citations retain whole source", 9}} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorldOpts(t, worldOpts{planSpanCount: tc.spans})
			call := func(name, args string) agent.ToolCall {
				return agent.ToolCall{Name: name, Arguments: json.RawMessage(args)}
			}
			// Empty IDs in both turns must become distinct, durable transcript references.
			first := agent.FixtureStep{Response: agent.ModelResponse{InputTokens: 900, OutputTokens: 40, ToolCalls: []agent.ToolCall{
				call(agent.ToolEvidenceGet, fmt.Sprintf(`{"evidence_id":%q}`, w.schedule)),
				call(agent.ToolMeasurementsGet, `{}`), call(agent.ToolMeasurementsGet, `{}`),
			}}}
			second := agent.FixtureStep{Response: agent.ModelResponse{InputTokens: 900, OutputTokens: 40, ToolCalls: []agent.ToolCall{
				call(agent.ToolEvidenceGet, fmt.Sprintf(`{"evidence_id":%q}`, w.schedule)),
				call(agent.ToolMeasurementsGet, `{}`),
			}}}
			p := script(first, second, finalStep(t, w.result("success"), w.clarify(w.revision)))
			result := w.investigate(t, w.runtime(p, nil), w.revision)
			if result.Status != recovery.StatusNeedsInput || len(p.requests) != 3 {
				t.Fatalf("read turns: %+v calls=%d", result, len(p.requests))
			}
			final := p.requests[2]
			if len(p.requests[0].Tools) == 0 || len(p.requests[1].Tools) == 0 || len(final.Tools) != 0 || !strings.Contains(final.Messages[len(final.Messages)-1].Content, "Give your final answer now:") {
				t.Fatalf("read cutoff not applied: tools=%d final=%q", len(final.Tools), final.Messages[len(final.Messages)-1].Content)
			}
			assistantIDs := map[string]bool{}
			toolMessages := []agent.Message{}
			for _, msg := range final.Messages {
				if msg.Role == "assistant" {
					for _, c := range msg.ToolCalls {
						if c.ID == "" || assistantIDs[c.ID] {
							t.Fatalf("ambiguous tool ID %q", c.ID)
						}
						assistantIDs[c.ID] = true
					}
				}
				if msg.Role == "tool" {
					toolMessages = append(toolMessages, msg)
				}
			}
			if len(toolMessages) != 5 || toolMessages[2].Content != `{"error":"TOOL_TURN_LIMIT"}` {
				t.Fatalf("deferred result: %+v", toolMessages)
			}
			for i, original := range []int{0, 1} {
				var ref struct {
					ID string `json:"tool_result_ref"`
				}
				if err := json.Unmarshal([]byte(toolMessages[3+i].Content), &ref); err != nil || ref.ID != toolMessages[original].ToolCallID {
					t.Fatalf("dedup ref %s: %v", toolMessages[3+i].Content, err)
				}
			}
			var source struct {
				Text     string `json:"untrusted_source"`
				Passages []struct {
					Text string `json:"untrusted_source"`
				} `json:"passages"`
			}
			if err := json.Unmarshal([]byte(toolMessages[0].Content), &source); err != nil {
				t.Fatal(err)
			}
			if tc.spans == 9 {
				if source.Text != "Order seven on press one at one unit per second. "+injection || len(source.Passages) != 0 {
					t.Fatal("whole-source fallback dropped required text")
				}
			} else if len(source.Passages) != 1 || source.Passages[0].Text != "Order seven on press" {
				t.Fatal("selected passage changed")
			}
			if got := w.row(t, result.RunID); got.toolCodes != "ok:,ok:,rejected:TOOL_TURN_LIMIT,ok:,ok:" {
				t.Fatalf("audit trail %s", got.toolCodes)
			}
		})
	}
}

func TestWIS702RevokedCapturedPassageRefusesRead(t *testing.T) {
	w := newWorld(t)
	sc, env := w.openRun(t, agent.DefaultLimits())
	rs := agent.RunScope{TenantID: w.tenant, SiteID: w.site}
	if err := w.store.InTx(w.ctx, rs, func(tx *sql.Tx) error {
		return agent.AddPassagesTx(w.ctx, tx, rs, sc.RunID, []agent.RunPassage{{SpanID: w.scheduleSpan, SourceID: w.schedule, SourceRevision: 1}})
	}); err != nil {
		t.Fatal(err)
	}
	args := []byte(fmt.Sprintf(`{"evidence_id":%q}`, w.schedule))
	tools := &agent.Tools{Store: w.store}
	if out, err := tools.Call(w.ctx, sc, env, agent.ToolEvidenceGet, args); err != nil || !strings.Contains(string(out.Content), `"passages"`) {
		t.Fatalf("captured passage control: %v %s", err, out.Content)
	}
	es := evidence.Scope{TenantID: w.tenant, SiteID: w.site}
	head, err := w.ev.Read(w.ctx, w.ea, es, w.schedule, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.ev.Revoke(w.ctx, w.ea, es, w.schedule, "Withdrawn after passage capture", uuid.NewString(), head.StateRevision); err != nil {
		t.Fatal(err)
	}
	out, err := tools.Call(w.ctx, sc, env, agent.ToolEvidenceGet, args)
	if toolCode(err) != agent.CodeEvidenceUnavailable || len(out.Content) != 0 {
		t.Fatalf("revoked passage leaked: %v %s", err, out.Content)
	}
	if got := w.row(t, sc.RunID); got.toolCodes != "ok:,unavailable:EVIDENCE_UNAVAILABLE" {
		t.Fatalf("revoked passage audit: %s", got.toolCodes)
	}
}
