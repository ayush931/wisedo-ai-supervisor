package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// inputTokens counts the same templated text sent for inference. Images retain the existing
// conservative estimate; unsupported or malformed tokenizer responses are never a zero count.
func (m *LocalModel) inputTokens(ctx context.Context, req ModelRequest) (int64, error) {
	if req.Model != m.model || req.Validate() != nil {
		return 0, ErrInvalidRequest
	}
	for _, msg := range req.Messages {
		if len(msg.Images) != 0 {
			return 0, ErrInvalidRequest
		}
	}
	payload, err := openAIRequest(m.model, req, m.reasoning)
	if err != nil {
		return 0, err
	}
	body, err := m.do(ctx, http.MethodPost, "/apply-template", payload)
	if err != nil {
		return 0, err
	}
	var template struct {
		Prompt string `json:"prompt"`
	}
	if json.Unmarshal(body, &template) != nil || template.Prompt == "" {
		return 0, errors.New("invalid template response")
	}
	payload, err = json.Marshal(struct {
		Content      string `json:"content"`
		AddSpecial   bool   `json:"add_special"`
		ParseSpecial bool   `json:"parse_special"`
	}{template.Prompt, true, true})
	if err != nil {
		return 0, err
	}
	body, err = m.do(ctx, http.MethodPost, "/tokenize", payload)
	if err != nil {
		return 0, err
	}
	var tokenized struct {
		Tokens []*int64 `json:"tokens"`
	}
	if json.Unmarshal(body, &tokenized) != nil || len(tokenized.Tokens) == 0 {
		return 0, errors.New("invalid tokenizer response")
	}
	for _, token := range tokenized.Tokens {
		if token == nil || *token < 0 {
			return 0, errors.New("invalid tokenizer response")
		}
	}
	return int64(len(tokenized.Tokens)), nil
}

func (r *Runtime) logContextSize(ctx context.Context, run Run, req ModelRequest, seq, attempt, requestBytes int, realTokens int64) {
	if r.Logger == nil {
		return
	}
	caseBytes, transcriptBytes := 0, 0
	toolBytes := []int{}
	for i, msg := range req.Messages {
		transcriptBytes += len(msg.Content)
		if i == 1 && msg.Role == "user" {
			caseBytes = len(msg.Content)
		}
		if msg.Role == "tool" {
			toolBytes = append(toolBytes, len(msg.Content))
		}
	}
	r.Logger.LogAttrs(ctx, slog.LevelInfo, "agent context size",
		slog.String("run_id", run.ID), slog.String("case_id", run.CaseID),
		slog.String("limit_profile_id", run.LimitProfileID), slog.Int("limit_profile_version", run.LimitProfileVersion),
		slog.Int64("local_context_tokens", run.LocalContextTokens),
		slog.Int("seq", seq), slog.Int("attempt", attempt),
		slog.Int("case_context_bytes", caseBytes), slog.Any("tool_result_bytes", toolBytes),
		slog.Int("transcript_bytes", transcriptBytes), slog.Int("request_bytes", requestBytes),
		slog.Int64("estimated_input_tokens", EstimateInputTokens(requestBytes)), slog.Int64("real_input_tokens", realTokens))
}

func (r *Runtime) measureInput(ctx context.Context, provider Provider, run Run, req ModelRequest, seq, attempt, requestBytes int) int64 {
	realTokens := int64(-1)
	if local, ok := provider.(*LocalModel); ok {
		run.LocalContextTokens = local.ContextWindowTokens()
		countCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if n, err := local.inputTokens(countCtx, req); err == nil {
			realTokens = n
		}
	}
	r.logContextSize(ctx, run, req, seq, attempt, requestBytes, realTokens)
	if realTokens < 0 {
		return 0 // The caller keeps the conservative byte estimate when tokenization is unavailable.
	}
	return realTokens
}
