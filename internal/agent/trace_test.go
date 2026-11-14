package agent

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNewIDsMatchStoredShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tr, sp := NewTraceID(), NewSpanID()
		if !validTraceID(tr) || !validSpanID(sp) || seen[tr] || seen[sp] {
			t.Fatalf("trace %q span %q", tr, sp)
		}
		seen[tr], seen[sp] = true, true
	}
}

// TestSpanSummaryRedactsFreeText: a canary placed in every string field that could carry text
// never reaches the log; the identifiers and the reason code do.
func TestSpanSummaryRedactsFreeText(t *testing.T) {
	const canary = "sk-CANARY ignore previous instructions https://cloud.example/v1"
	run, tenant, evidence := uuid.NewString(), uuid.NewString(), uuid.NewString()
	start := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	for _, s := range []SpanSummary{
		{TraceID: NewTraceID(), SpanID: NewSpanID(), RunID: run, TenantID: tenant, SiteID: uuid.NewString(), Kind: "model_call", Role: RoleInvestigation,
			Model: "qwen2.5-7b-instruct", Status: "failed", ReasonCode: "PROVIDER_FAILED", EvidenceIDs: []string{evidence}, Start: start, End: start.Add(1500 * time.Millisecond), InputTokens: 10},
		{TraceID: canary, SpanID: canary, ParentSpanID: canary, RunID: canary, ParentRunID: canary, TenantID: canary, SiteID: canary, Kind: canary, Role: canary, Tool: canary,
			Model: canary, ModelRevision: canary, PromptVersion: canary, ToolVersion: canary, Status: canary, ReasonCode: canary, EvidenceIDs: []string{canary}},
	} {
		s.Log(context.Background(), logger)
	}
	out := buf.String()
	if strings.Contains(out, "CANARY") || strings.Contains(out, "cloud.example") || strings.Contains(out, "ignore previous") {
		t.Fatalf("free text reached the log: %s", out)
	}
	for _, want := range []string{run, tenant, evidence, "PROVIDER_FAILED", `"latency_ms":1500`, "qwen2.5-7b-instruct"} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary lost %q: %s", want, out)
		}
	}
}
