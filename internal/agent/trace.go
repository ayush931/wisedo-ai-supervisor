package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"regexp"
	"time"
)

var (
	traceIDPat = regexp.MustCompile(`^[0-9a-f]{32}$`)
	spanIDPat  = regexp.MustCompile(`^[0-9a-f]{16}$`)
	versionPat = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,254}$`)
)

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}

// NewTraceID returns a W3C-sized 16-byte trace ID as lowercase hex.
func NewTraceID() string { return randomHex(16) }

// NewSpanID returns an 8-byte span ID as lowercase hex.
func NewSpanID() string { return randomHex(8) }

func validTraceID(s string) bool {
	return traceIDPat.MatchString(s) && s != "00000000000000000000000000000000"
}
func validSpanID(s string) bool { return spanIDPat.MatchString(s) && s != "0000000000000000" }

// SpanSummary is the redacted record of one span: identifiers, versions, counts and reason
// codes only. It has no field for prompts, completions, source text, endpoint URLs or
// credentials, and Attrs drops any value that does not match its expected shape, so free text
// cannot reach the log through it.
type SpanSummary struct {
	TraceID, SpanID, ParentSpanID string
	RunID, ParentRunID            string
	TenantID, SiteID              string
	Kind                          string // step | model_call | tool_call
	Role                          string
	Tool                          string
	Model, ModelRevision          string
	PromptVersion, ToolVersion    string
	EvidenceIDs                   []string
	Status                        string // lowercase status word
	ReasonCode                    string // upper-case reason code
	Start, End                    time.Time
	InputTokens, OutputTokens     int64
	ChargedMicroUSD               int64
	Calls                         int
}

var lowerWord = regexp.MustCompile(`^[a-z][a-z0-9_./]{0,63}$`)

func keep(attrs []slog.Attr, key, value string, ok func(string) bool) []slog.Attr {
	if value != "" && ok(value) {
		return append(attrs, slog.String(key, value))
	}
	return attrs
}

// Attrs returns the summary as structured log attributes, dropping malformed values.
func (s SpanSummary) Attrs() []slog.Attr {
	var a []slog.Attr
	a = keep(a, "trace_id", s.TraceID, validTraceID)
	a = keep(a, "span_id", s.SpanID, validSpanID)
	a = keep(a, "parent_span_id", s.ParentSpanID, validSpanID)
	a = keep(a, "run_id", s.RunID, isUUID)
	a = keep(a, "parent_run_id", s.ParentRunID, isUUID)
	a = keep(a, "tenant_id", s.TenantID, isUUID)
	a = keep(a, "site_id", s.SiteID, isUUID)
	a = keep(a, "kind", s.Kind, lowerWord.MatchString)
	a = keep(a, "role", s.Role, validRole)
	a = keep(a, "tool", s.Tool, lowerWord.MatchString)
	a = keep(a, "model", s.Model, versionPat.MatchString)
	a = keep(a, "model_revision", s.ModelRevision, versionPat.MatchString)
	a = keep(a, "prompt_version", s.PromptVersion, versionPat.MatchString)
	a = keep(a, "tool_version", s.ToolVersion, versionPat.MatchString)
	a = keep(a, "status", s.Status, lowerWord.MatchString)
	a = keep(a, "reason_code", s.ReasonCode, reasonPat.MatchString)
	ids := make([]string, 0, len(s.EvidenceIDs))
	for _, id := range s.EvidenceIDs {
		if isUUID(id) {
			ids = append(ids, id)
		}
	}
	if len(ids) > 0 {
		a = append(a, slog.Any("evidence_ids", ids))
	}
	if !s.Start.IsZero() {
		a = append(a, slog.Time("start", s.Start.UTC()))
		if !s.End.IsZero() && !s.End.Before(s.Start) {
			a = append(a, slog.Time("end", s.End.UTC()), slog.Int64("latency_ms", s.End.Sub(s.Start).Milliseconds()))
		}
	}
	return append(a, slog.Int64("input_tokens", s.InputTokens), slog.Int64("output_tokens", s.OutputTokens),
		slog.Int64("charged_microusd", s.ChargedMicroUSD), slog.Int("calls", s.Calls))
}

// Log writes the summary at span end.
func (s SpanSummary) Log(ctx context.Context, l *slog.Logger) {
	if l == nil {
		l = slog.Default()
	}
	l.LogAttrs(ctx, slog.LevelInfo, "agent.span", s.Attrs()...)
}
