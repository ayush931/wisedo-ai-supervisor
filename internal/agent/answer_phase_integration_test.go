//go:build integration

package agent_test

import (
	"strings"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
)

// phased is the scripted provider marked as an answer-phase provider, like the local and hosted
// OpenAI-compatible adapters: while tools are offered they send the schema only as text (a
// llama.cpp json_schema grammar forbids tool calls), so a reply without a tool call is a draft.
type phased struct{ *scripted }

func (phased) AnswerPhase() bool { return true }

// TestAnswerPhase (WIS-276, live Qwen3 8B): without the grammar the model's final JSON was
// malformed, so a turn that offers tools and returns no tool call is followed by exactly one
// grammar call without tools, and only that reply is decoded and validated.
func TestAnswerPhase(t *testing.T) {
	w := newWorld(t)
	draft := func() agent.FixtureStep { return finalStep(t, w.result("needs_input"), nil) } // a valid answer that must not be used

	t.Run("one grammar call without tools follows the draft, and the draft is never the answer", func(t *testing.T) {
		rev := w.fresh(t)
		p := phased{script(toolStep(agent.ToolMeasurementsGet, `{}`), draft(), finalStep(t, w.result("success"), w.clarify(rev)))}
		res := w.investigate(t, w.runtime(p, nil), rev)
		got := w.row(t, res.RunID)
		// The draft alone would have ended needs_input/SPECIALIST_NEEDS_INPUT with no artifact.
		want(t, got, agent.RunCompleted, recovery.StatusNeedsInput, "CLARIFICATION_REQUESTED")
		if !got.artifact || got.modelCalls != 3 || len(p.requests) != 3 {
			t.Fatalf("artifact=%v model calls=%d requests=%d", got.artifact, got.modelCalls, len(p.requests))
		}
		tool, draftReq, answer := p.requests[0], p.requests[1], p.requests[2]
		if len(tool.Tools) == 0 || len(draftReq.Tools) == 0 || len(answer.Tools) != 0 || len(answer.ResponseSchema) == 0 {
			t.Fatalf("tools per request = %d,%d,%d; answer schema %d bytes", len(tool.Tools), len(draftReq.Tools), len(answer.Tools), len(answer.ResponseSchema))
		}
		n := len(answer.Messages)
		if n != len(draftReq.Messages)+2 || answer.Messages[n-2].Role != "assistant" || !strings.Contains(answer.Messages[n-2].Content, `"needs_input"`) || answer.Messages[n-1].Role != "user" {
			t.Fatalf("answer conversation: %+v", answer.Messages[len(draftReq.Messages):])
		}
	})

	// Codex review P2 on 8ff2ab9: draft (1,1), answer (2,1) fails retryably, the retry and the
	// next tool turn must not reuse model-call key (2,1).
	t.Run("a retry after a failed answer call does not reuse its model-call key", func(t *testing.T) {
		rev := w.fresh(t)
		unavailable := agent.FixtureStep{Err: agent.RetryableProviderError(agent.CodeProviderFailed, 503)}
		p := phased{script(draft(), unavailable, toolStep(agent.ToolMeasurementsGet, `{}`), draft(), finalStep(t, w.result("success"), w.clarify(rev)))}
		res := w.investigate(t, w.runtime(p, nil), rev)
		got := w.row(t, res.RunID)
		want(t, got, agent.RunCompleted, recovery.StatusNeedsInput, "CLARIFICATION_REQUESTED")
		if got.modelCalls != 5 || len(p.requests) != 5 {
			t.Fatalf("model calls=%d requests=%d", got.modelCalls, len(p.requests))
		}
	})

	t.Run("no request left for the answer phase fails closed", func(t *testing.T) {
		rev := w.fresh(t)
		p := phased{script(draft(), finalStep(t, w.result("success"), w.clarify(rev)))}
		rt := w.runtime(p, nil)
		rt.Limits.Requests = 1
		res := w.investigate(t, rt, rev)
		got := w.row(t, res.RunID)
		want(t, got, agent.RunCompleted, recovery.StatusNeedsInput, agent.CodeBudgetExhausted)
		if got.artifact || len(p.requests) != 1 {
			t.Fatalf("artifact=%v requests=%d", got.artifact, len(p.requests))
		}
	})

	t.Run("control: a provider without an answer phase is used as scripted", func(t *testing.T) {
		rev := w.fresh(t)
		p := script(finalStep(t, w.result("success"), w.clarify(rev)))
		res := w.investigate(t, w.runtime(p, nil), rev)
		if got := w.row(t, res.RunID); got.reasons != "CLARIFICATION_REQUESTED" || len(p.requests) != 1 {
			t.Fatalf("%+v requests=%d", got, len(p.requests))
		}
	})
}
