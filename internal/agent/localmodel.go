package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
)

const maxModelResponseBytes = 1 << 20

var (
	errEndpointNotLocal  = errors.New("endpoint not local")
	errEndpointNotPinned = errors.New("endpoint not pinned")
	errRedirect          = errors.New("redirect refused")
)

// LocalModel posts to a pinned, local OpenAI-compatible /v1/chat/completions (vLLM,
// llama.cpp server, Ollama). There is one endpoint and no fallback. The base URL must be a
// local literal or an allowlisted name, and every dial re-checks the resolved address, so
// DNS cannot route a call to a public IP. Proxies and redirects are never followed.
type LocalModel struct {
	base          *url.URL
	pin           config.Agent // EndpointAddrs: the only addresses a dial may reach
	model         string
	revision      string
	reasoning     string // reasoning_effort sent on each chat request; "" sends none (WIS-631)
	apiKey        string
	client        *http.Client
	contextTokens atomic.Int64
	breaker       *Breaker
	log           *slog.Logger
	now           func() time.Time
	// lookup resolves a host name at dial time; tests replace it so no real DNS is used.
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)
}

// NewLocalModel builds the real provider from validated agent settings. now drives the
// breaker and the recorded latency.
func NewLocalModel(a config.Agent, logger *slog.Logger, now func() time.Time) (*LocalModel, error) {
	if a.Provider != config.AgentProviderLocal || a.Model == "" || a.ModelRevision == "" || a.Timeout <= 0 || logger == nil || now == nil {
		return nil, errors.New("invalid local model configuration")
	}
	base, err := a.PinnedEndpoint()
	if err != nil {
		return nil, err
	}
	m := &LocalModel{base: base, pin: config.Agent{EndpointAddrs: a.EndpointAddrs}, model: a.Model, revision: a.ModelRevision, reasoning: a.ReasoningEffort, breaker: NewBreaker(now), log: logger, now: now,
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}}
	if a.APIKeyFile != "" {
		key, err := os.ReadFile(a.APIKeyFile)
		if m.apiKey = strings.TrimSpace(string(key)); err != nil || m.apiKey == "" {
			return nil, errors.New("invalid SUPERVISOR_AGENT_API_KEY_FILE")
		}
	}
	m.client = &http.Client{
		Timeout: a.Timeout,
		Transport: &http.Transport{
			Proxy:                 nil, // HTTPS_PROXY must not route a call to a cloud endpoint
			DialContext:           m.dial,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: a.Timeout,
			MaxIdleConns:          4,
			IdleConnTimeout:       90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirect },
	}
	return m, nil
}

func (m *LocalModel) Name() string { return ProviderLocal }

// ErrVisionUnavailable: no vision model is pinned, so a scan cannot be read (WIS-625). Callers
// refuse the file with EXTRACTION_UNAVAILABLE; no text is ever substituted (rule 01).
var ErrVisionUnavailable = errors.New("no vision model is configured")

// NewVisionModel is the local provider pinned to the vision model, on the same endpoint, address
// pin, key and limits as the agent's model. Only a local model reads a page image: images cannot
// be pseudonymised, so no hosted route ever receives one.
func NewVisionModel(a config.Agent, logger *slog.Logger, now func() time.Time) (*LocalModel, error) {
	if a.VisionModel == "" || a.VisionModelRevision == "" || a.Provider != config.AgentProviderLocal {
		return nil, ErrVisionUnavailable
	}
	// The reasoning effort is the agent model's setting; a vision model may refuse it (WIS-631).
	a.Model, a.ModelRevision, a.ReasoningEffort = a.VisionModel, a.VisionModelRevision, ""
	return NewLocalModel(a, logger, now)
}
func (m *LocalModel) Actual() bool { return true }

// dial resolves the host itself and refuses unless every address is local and in the pinned
// SUPERVISOR_AGENT_ENDPOINT_ADDRS set, then dials the vetted IP literal (never the name, so a
// second lookup cannot rebind, not even to another private service).
func (m *LocalModel) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var addrs []netip.Addr
	if ip, e := netip.ParseAddr(host); e == nil {
		addrs = []netip.Addr{ip}
	} else if addrs, err = m.lookup(ctx, host); err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, errEndpointNotLocal
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return nil, errEndpointNotPinned
	}
	for _, a := range addrs {
		if !config.LocalAddr(a) {
			return nil, errEndpointNotLocal
		}
		if !m.pin.Pinned(a, uint16(n)) {
			return nil, errEndpointNotPinned
		}
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	return d.DialContext(ctx, network, net.JoinHostPort(addrs[0].Unmap().String(), port))
}

// Complete performs one call. It never retries; on failure it returns *ProviderError.
// AnswerPhase: openAIChat sends the schema as text while tools are offered (see callRole).
func (m *LocalModel) AnswerPhase() bool { return true }

func (m *LocalModel) Complete(ctx context.Context, r ModelRequest) (ModelResponse, error) {
	start := m.now()
	if err := r.Validate(); err != nil {
		return ModelResponse{}, m.logged(ctx, r, &ProviderError{Code: CodeSchemaInvalid}, ModelResponse{}, start)
	}
	if r.Model != m.model {
		return ModelResponse{}, m.logged(ctx, r, &ProviderError{Code: CodeModelNotPinned}, ModelResponse{}, start)
	}
	probe, err := m.breaker.Allow()
	if err != nil {
		return ModelResponse{}, m.logged(ctx, r, err, ModelResponse{}, start)
	}
	if probe {
		err = m.Probe(ctx)
	}
	var resp ModelResponse
	if err == nil {
		resp, err = m.chat(ctx, r)
	}
	m.breaker.Record(err)
	resp.WallMillis = m.now().Sub(start).Milliseconds()
	if err != nil {
		return billed(resp), m.logged(ctx, r, err, resp, start)
	}
	m.logged(ctx, r, nil, resp, start)
	return resp, nil
}

// logged records hashes, codes, tokens and latency only: never prompt, completion or key (rule 08).
func (m *LocalModel) logged(ctx context.Context, r ModelRequest, err error, resp ModelResponse, start time.Time) error {
	code, status, level := "OK", 0, slog.LevelInfo
	var p *ProviderError
	if errors.As(err, &p) {
		code, status, level = p.Code, p.HTTPStatus, slog.LevelWarn
	}
	m.log.LogAttrs(ctx, level, "agent model call",
		slog.String("provider", ProviderLocal), slog.String("model", m.model), slog.String("model_revision", m.revision),
		slog.String("request_sha256", r.Digest()), slog.String("prompt_version", r.PromptVersion),
		slog.String("code", code), slog.Int("http_status", status),
		slog.Int64("input_tokens", resp.InputTokens), slog.Int64("output_tokens", resp.OutputTokens),
		slog.Int64("wall_ms", m.now().Sub(start).Milliseconds()))
	return err
}

// ContextWindowTokens is the last probed llama.cpp per-slot context, or zero when unknown.
func (m *LocalModel) ContextWindowTokens() int64 { return m.contextTokens.Load() }

// Probe checks the pinned model and, for the local experiment, its per-slot context capacity.
// Missing or malformed /props disables the raised profile without disabling ordinary inference.
func (m *LocalModel) Probe(ctx context.Context) error {
	m.contextTokens.Store(0)
	body, err := m.do(ctx, http.MethodGet, "/v1/models", nil)
	if err != nil {
		return err
	}
	var w struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &w) != nil {
		return &ProviderError{Code: CodeSchemaInvalid, retryable: true}
	}
	if !slices.ContainsFunc(w.Data, func(d struct {
		ID string `json:"id"`
	}) bool {
		return d.ID == m.model
	}) {
		return &ProviderError{Code: CodeModelNotPinned}
	}
	if m.model == localQwenModel && m.revision == localQwenRevision {
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		props, err := m.do(probeCtx, http.MethodGet, "/props", nil)
		var properties struct {
			Settings struct {
				Context int64 `json:"n_ctx"`
			} `json:"default_generation_settings"`
		}
		if err == nil && json.Unmarshal(props, &properties) == nil && properties.Settings.Context > 0 {
			m.contextTokens.Store(properties.Settings.Context)
		}
	}

	return nil
}

type wireMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"` // a string, or text and image parts for a page image
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []wireCall `json:"tool_calls,omitempty"`
}

func firstSystemText(msgs []wireMessage) (string, bool) {
	if len(msgs) == 0 || msgs[0].Role != "system" {
		return "", false
	}
	text, ok := msgs[0].Content.(string)
	return text, ok
}

type wireCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func (m *LocalModel) chat(ctx context.Context, r ModelRequest) (ModelResponse, error) {
	return openAIChat(ctx, m.do, m.model, m.revision, ProviderLocal, r, m.reasoning)
}

// openAIChat is one OpenAI-compatible /v1/chat/completions call through do, shared by the local
// and hosted adapters: the pinned model, temperature 0, the request's tools and response schema.
// The response envelope must name the pinned model and carry exactly one choice and the usage.
func openAIChat(ctx context.Context, do func(context.Context, string, string, []byte) ([]byte, error), model, revision, provider string, r ModelRequest, reasoning string) (ModelResponse, error) {
	payload, err := openAIRequest(model, r, reasoning)
	if err != nil {
		return ModelResponse{}, &ProviderError{Code: CodeSchemaInvalid}
	}
	body, err := do(ctx, http.MethodPost, "/v1/chat/completions", payload)
	if err != nil {
		return ModelResponse{}, err
	}
	return decodeOpenAIResponse(body, model, revision, provider, r)
}

func openAIRequest(model string, r ModelRequest, reasoning string) ([]byte, error) {
	in := map[string]any{"model": model, "max_tokens": r.MaxOutputTokens, "temperature": 0, "stream": false}
	if reasoning != "" {
		in["reasoning_effort"] = reasoning
	}
	msgs := make([]wireMessage, len(r.Messages))
	for i, msg := range r.Messages {
		msgs[i] = wireMessage{Role: msg.Role, Content: msg.Content, ToolCallID: msg.ToolCallID}
		if len(msg.Images) > 0 {
			parts := []map[string]any{{"type": "text", "text": msg.Content}}
			for _, img := range msg.Images {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:" + img.MediaType + ";base64," + base64.StdEncoding.EncodeToString(img.Data)}})
			}
			msgs[i].Content = parts
		}
		for _, c := range msg.ToolCalls {
			w := wireCall{ID: c.ID, Type: "function"}
			w.Function.Name, w.Function.Arguments = c.Name, string(c.Arguments)
			msgs[i].ToolCalls = append(msgs[i].ToolCalls, w)
		}
	}
	// A response_format grammar (llama.cpp) admits only the schema's JSON, so a model offered tools
	// could never call one. With tools the schema travels as system text instead; strict decoding
	// and the validators remain the enforcement either way.
	schemaAsText := len(r.Tools) > 0 && len(r.ResponseSchema) > 0
	if schemaAsText {
		line := "Your final answer, after any tool calls, must be one JSON object matching this JSON Schema: " + string(r.ResponseSchema)
		if text, ok := firstSystemText(msgs); ok {
			msgs[0].Content = text + "\n" + line
		} else {
			msgs = append([]wireMessage{{Role: "system", Content: line}}, msgs...)
		}
	}
	in["messages"] = msgs
	if len(r.Tools) > 0 {
		tools := make([]map[string]any, len(r.Tools))
		for i, t := range r.Tools {
			tools[i] = map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.Parameters}}
		}
		in["tools"] = tools
	}
	if len(r.ResponseSchema) > 0 && !schemaAsText {
		in["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "result", "strict": true, "schema": r.ResponseSchema}}
	}
	return json.Marshal(in)
}

func decodeOpenAIResponse(body []byte, model, revision, provider string, r ModelRequest) (ModelResponse, error) {
	// The envelope is decoded leniently because servers add vendor fields; the hostile part,
	// the model-authored content, is strict-decoded by the runtime against its contract.
	var w struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   *string    `json:"content"`
				ToolCalls []wireCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	schemaErr := &ProviderError{Code: CodeSchemaInvalid, retryable: true}
	if json.Unmarshal(body, &w) != nil {
		return ModelResponse{}, schemaErr
	}
	if w.Model != model {
		return ModelResponse{}, &ProviderError{Code: CodeModelNotPinned}
	}
	if len(w.Choices) != 1 || w.Usage == nil || w.Usage.PromptTokens < 0 || w.Usage.CompletionTokens < 0 {
		return ModelResponse{}, schemaErr
	}
	choice := w.Choices[0]
	resp := ModelResponse{SchemaVersion: ModelSchemaVersion, FinishReason: choice.FinishReason, InputTokens: w.Usage.PromptTokens,
		OutputTokens: w.Usage.CompletionTokens, ProviderActual: provider, ProviderModel: w.Model, ModelRevision: revision}
	if choice.Message.Content != nil {
		resp.Content = *choice.Message.Content
	}
	for _, c := range choice.Message.ToolCalls {
		if c.Type != "function" {
			return billed(resp), contentFailure(schemaErr) // the model's tool call, not the envelope
		}
		resp.ToolCalls = append(resp.ToolCalls, ToolCall{ID: c.ID, Name: c.Function.Name, Arguments: json.RawMessage(c.Function.Arguments)})
	}
	if err := checkToolCalls(r, resp.ToolCalls); err != nil {
		return billed(resp), err
	}
	return resp, nil
}

// billed keeps only the reported token usage of a response that is refused after the endpoint
// answered: the call was billed, so the runtime charges it, but none of its content is used.
func billed(r ModelResponse) ModelResponse {
	return ModelResponse{InputTokens: r.InputTokens, OutputTokens: r.OutputTokens}
}

// do sends one request and returns the bounded body of a 200 response.
func (m *LocalModel) do(ctx context.Context, method, path string, payload []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, m.base.JoinPath(path).String(), bytes.NewReader(payload))
	if err != nil {
		return nil, &ProviderError{Code: CodeProviderFailed}
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if m.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+m.apiKey)
	}
	res, err := m.client.Do(req)
	if err != nil {
		return nil, classify(ctx, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxModelResponseBytes))
		retry := res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500
		return nil, &ProviderError{Code: CodeProviderFailed, HTTPStatus: res.StatusCode, retryable: retry}
	}
	if res.ContentLength > maxModelResponseBytes {
		return nil, &ProviderError{Code: CodeProviderResponseLarge, HTTPStatus: res.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxModelResponseBytes+1))
	if err != nil {
		return nil, classify(ctx, err)
	}
	if len(body) > maxModelResponseBytes {
		return nil, &ProviderError{Code: CodeProviderResponseLarge, HTTPStatus: res.StatusCode}
	}
	return body, nil
}

func classify(ctx context.Context, err error) *ProviderError {
	var ne net.Error
	switch {
	case errors.Is(err, errEndpointNotLocal):
		return &ProviderError{Code: CodeEndpointNotLocal}
	case errors.Is(err, errEndpointNotPinned):
		return &ProviderError{Code: CodeEndpointNotPinned}
	case errors.Is(err, errRedirect):
		return &ProviderError{Code: CodeProviderFailed}
	case errors.Is(ctx.Err(), context.Canceled):
		return &ProviderError{Code: CodeProviderCanceled}
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout():
		return &ProviderError{Code: CodeDeadlineExceeded, retryable: true}
	default:
		return &ProviderError{Code: CodeProviderFailed, retryable: true}
	}
}
