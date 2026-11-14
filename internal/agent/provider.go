package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
)

// ModelSchemaVersion versions ModelRequest and ModelResponse.
const ModelSchemaVersion = 1

// Provider labels, recorded as provider_actual on every response.
const (
	ProviderLocal   = "local-openai-compatible"
	ProviderFixture = "deterministic-fixture"
)

// Provider failure codes. PROVIDER_FAILED and SCHEMA_INVALID are the recovery package's codes;
// DEADLINE_EXCEEDED matches internal/oeeservice. The rest are provider-specific.
const (
	CodeProviderFailed         = recovery.ReasonProviderFailed
	CodeSchemaInvalid          = recovery.ReasonSchemaInvalid
	CodeDeadlineExceeded       = "DEADLINE_EXCEEDED"
	CodeEndpointNotLocal       = "ENDPOINT_NOT_LOCAL"
	CodeEndpointNotPinned      = "ENDPOINT_NOT_PINNED" // resolved to a local address outside SUPERVISOR_AGENT_ENDPOINT_ADDRS
	CodeModelNotPinned         = "MODEL_NOT_PINNED"
	CodeProviderUnavailable    = "PROVIDER_UNAVAILABLE" // breaker open; no call made
	CodeProviderResponseLarge  = "PROVIDER_RESPONSE_TOO_LARGE"
	CodeProviderToolNotOffered = "PROVIDER_TOOL_NOT_OFFERED" // the model asked for a tool it was not given
	CodeProviderCanceled       = "PROVIDER_CANCELED"
	CodeFixtureExhausted       = "FIXTURE_SCRIPT_EXHAUSTED"
)

const (
	maxOutputTokens  = 2000 // WIS-275 per-request output ceiling
	maxModelMessages = 64
)

// Provider is a model backend. Implementations never retry; the runtime owns the single
// narrowed retry (see Retryable). Complete returns *ProviderError on failure.
type Provider interface {
	Name() string // ProviderLocal | ProviderFixture
	Actual() bool // false for the fixture adapter: it can never satisfy an actual-model gate
	Complete(ctx context.Context, r ModelRequest) (ModelResponse, error)
}

// RequireActual is the actual-model gate check (rule 09): a deterministic adapter never qualifies.
func RequireActual(p Provider) error {
	if p == nil || !p.Actual() {
		return errors.New("BLOCKED: an actual model provider is required")
	}
	return nil
}

type Message struct {
	Role       string     `json:"role"` // system | user | assistant | tool
	Content    string     `json:"content"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	// Images are page images for OCR (WIS-625): untrusted data like any source text. Only a user
	// message carries them, at most one per request, so each page is reserved and charged alone.
	Images []Image `json:"images,omitempty"`

	// hidden marks an assistant message carried in the form a hosted model wrote it, tokens
	// intact (see ModelResponse.AssistantMessage): it is audited but not hidden again, and the
	// next call reuses pseudonyms, the map those tokens belong to (WIS-623 Codex rounds 15, 16).
	hidden     bool
	pseudonyms *Pseudonyms
}

// Image is one bounded page image. MediaType must be what the bytes are (PNG or JPEG).
type Image struct {
	MediaType string `json:"media_type"`
	Data      []byte `json:"data"`
}

// MaxImageBytes bounds one page image sent to a vision model. MaxImagePixels bounds its declared
// size, read from the header before anything decodes it (a small PNG can declare a huge canvas).
const (
	MaxImageBytes  = 4 << 20
	MaxImagePixels = 40_000_000
	// imagePatch and MaxImageTokens follow the Qwen2.5-VL family: one token per 28x28 pixel patch,
	// and a server that downscales above 12,845,056 pixels (its default max_pixels), which caps a
	// page at 16,384 tokens. A server configured with a larger max_pixels is not supported.
	imagePatch     = 28
	MaxImageTokens = 16384
)

// ImageSize reads a PNG's or JPEG's declared size without decoding it.
func ImageSize(b []byte) (width, height int, ok bool) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width)*int64(cfg.Height) > MaxImagePixels {
		return 0, 0, false
	}
	return cfg.Width, cfg.Height, true
}

// ImageTokens is the reserved input estimate of one page: a token per patch plus the image's
// start and end markers, at most MaxImageTokens. The server's reported usage is what is charged.
func ImageTokens(img Image) int64 {
	w, h, ok := ImageSize(img.Data)
	if !ok {
		return MaxImageTokens + 2
	}
	n := int64((w+imagePatch-1)/imagePatch) * int64((h+imagePatch-1)/imagePatch)
	return min(n, MaxImageTokens) + 2
}

// imageMediaType detects PNG or JPEG from the bytes; anything else is "".
func imageMediaType(b []byte) string {
	switch t := http.DetectContentType(b); t {
	case "image/png", "image/jpeg":
		return t
	}
	return ""
}

// HasImages reports whether any message carries an image.
func (r ModelRequest) HasImages() bool {
	for _, m := range r.Messages {
		if len(m.Images) > 0 {
			return true
		}
	}
	return false
}

type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// AssistantMessage is the message that carries this response back to the model in a later call
// of the same exchange (a tool continuation, the answer phase, chat, interview and drafts). For a
// hosted call it is the content and tool calls as the model wrote them, tokens intact, on the
// same token map: restoring and hiding them again could lose a name ("X⟦P1⟧X" is "XAliceX").
// Restoration happens only where text leaves the model boundary: the tool arguments a tool runs
// with (ToolCalls) and the final answer (Content) (WIS-623 Codex round 16).
func (r ModelResponse) AssistantMessage() Message {
	if r.pseudonyms != nil {
		return Message{Role: "assistant", Content: r.hiddenContent, ToolCalls: slices.Clone(r.hiddenToolCalls), hidden: true, pseudonyms: r.pseudonyms}
	}
	return Message{Role: "assistant", Content: r.Content, ToolCalls: slices.Clone(r.ToolCalls)}
}

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ModelRequest struct {
	SchemaVersion   int             `json:"schema_version"`
	Model           string          `json:"model"`
	PromptVersion   string          `json:"prompt_version"`
	ToolVersion     string          `json:"tool_version"`
	Messages        []Message       `json:"messages"`
	Tools           []ToolDef       `json:"tools"`
	ResponseSchema  json.RawMessage `json:"response_schema"`
	MaxOutputTokens int             `json:"max_output_tokens"`
}

// Validate checks the closed request shape. Only the four read tools may be offered.
func (r ModelRequest) Validate() error {
	if r.SchemaVersion != ModelSchemaVersion || r.Model == "" || r.PromptVersion == "" || r.ToolVersion == "" ||
		len(r.Messages) == 0 || len(r.Messages) > maxModelMessages || r.MaxOutputTokens < 1 || r.MaxOutputTokens > maxOutputTokens ||
		(len(r.ResponseSchema) > 0 && !json.Valid(r.ResponseSchema)) {
		return errors.New("invalid model request")
	}
	images := 0
	for _, m := range r.Messages {
		if !slices.Contains([]string{"system", "user", "assistant", "tool"}, m.Role) || (m.Role == "tool") != (m.ToolCallID != "") {
			return errors.New("invalid model request")
		}
		if len(m.Images) > 0 && m.Role != "user" {
			return errors.New("invalid model request")
		}
		for _, img := range m.Images {
			images++
			if len(img.Data) == 0 || len(img.Data) > MaxImageBytes || img.MediaType == "" || imageMediaType(img.Data) != img.MediaType {
				return errors.New("invalid model request")
			}
			if _, _, ok := ImageSize(img.Data); !ok {
				return errors.New("invalid model request")
			}
		}
	}
	// One page per request; a page is never sent with tools (OCR only transcribes).
	if images > 1 || images == 1 && len(r.Tools) > 0 {
		return errors.New("invalid model request")
	}
	seen := map[string]bool{}
	for _, t := range r.Tools {
		if seen[t.Name] || !slices.Contains([]string{ToolContextGet, ToolEvidenceGet, ToolClaimsSearch, ToolMeasurementsGet}, t.Name) || !json.Valid(t.Parameters) {
			return errors.New("invalid model request")
		}
		seen[t.Name] = true
	}
	return nil
}

// Digest is a SHA-256 of the request, the only form of the prompt that may be logged (rule 08).
func (r ModelRequest) Digest() string {
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type ModelResponse struct {
	SchemaVersion  int        `json:"schema_version"`
	Content        string     `json:"content"` // untrusted model text; the runtime strict-decodes it
	ToolCalls      []ToolCall `json:"tool_calls"`
	InputTokens    int64      `json:"input_tokens"`
	OutputTokens   int64      `json:"output_tokens"`
	FinishReason   string     `json:"finish_reason"`
	ProviderActual string     `json:"provider_actual"`
	ProviderModel  string     `json:"provider_model"`
	ModelRevision  string     `json:"model_revision"`
	WallMillis     int64      `json:"wall_millis"`

	// requestSHA256 is the digest of the request this response answers, set by the runtime: with
	// the answer phase it is the answer request, not the caller's tool-phase request.
	requestSHA256 string
	// hiddenContent is the model's content as it came back, before restoration (hosted calls
	// only); pseudonyms is the map the call used. The answer phase carries both forward.
	hiddenContent   string
	hiddenToolCalls []ToolCall
	pseudonyms      *Pseudonyms
}

// DecodeModelResponse strictly decodes a versioned response (fixture scripts, stored replays).
func DecodeModelResponse(data []byte) (ModelResponse, error) {
	var r ModelResponse
	if err := strictjson.Decode(data, &r, MaxContractBytes); err != nil || r.SchemaVersion != ModelSchemaVersion ||
		r.InputTokens < 0 || r.OutputTokens < 0 || r.WallMillis < 0 {
		return ModelResponse{}, errors.New("invalid model response")
	}
	return r, nil
}

// checkToolCalls rejects a call to a tool the request did not offer (non-retryable: a
// malicious or confused tool request) and malformed arguments (schema failure, retryable).
func checkToolCalls(r ModelRequest, calls []ToolCall) error {
	for _, c := range calls {
		if !slices.ContainsFunc(r.Tools, func(t ToolDef) bool { return t.Name == c.Name }) {
			return &ProviderError{Code: CodeProviderToolNotOffered, content: true}
		}
		if !json.Valid(c.Arguments) {
			return &ProviderError{Code: CodeSchemaInvalid, retryable: true, content: true}
		}
	}
	return nil
}

// ProviderError carries only a stable code and the upstream HTTP status: never a body,
// URL, key or prompt (rules 07 and 08).
type ProviderError struct {
	Code       string
	HTTPStatus int
	retryable  bool
	// content marks a refusal of what a well-formed reply said (a tool the request did not
	// offer, invalid tool arguments, an unissued pseudonym): the endpoint answered, so the
	// failure says nothing about its health and never counts against the shared breaker (WIS-696).
	content bool
}

func (e *ProviderError) Error() string { return "model provider: " + e.Code }

// contentFailure marks err, when it is a *ProviderError, as a refusal of a reply's content (see
// ProviderError.content). It returns a copy, so a shared error value is never changed.
func contentFailure(err error) error {
	var p *ProviderError
	if !errors.As(err, &p) {
		return err
	}
	q := *p
	q.content = true
	return &q
}

// Retryable reports whether the caller may spend its single narrowed retry: timeouts,
// transport failures, 429/5xx and schema failures. Endpoint, pin, 4xx, tool-request and
// breaker failures are not.
func (e *ProviderError) Retryable() bool { return e.retryable }

// Retryable is false for anything that is not a retryable *ProviderError, so a tenant,
// scope or capability denial from elsewhere fails closed.
func Retryable(err error) bool {
	var p *ProviderError
	return errors.As(err, &p) && p.retryable
}

// Breaker opens after breakerFailures failures within the last breakerWindow calls, stays
// open for breakerOpen, then admits exactly one half-open probe. Success closes it; a
// failed probe reopens it. It is per process and per endpoint.
type Breaker struct {
	mu        sync.Mutex
	now       func() time.Time
	outcomes  []bool // last calls, true = failure
	openUntil time.Time
	open      bool
	probing   bool
}

const (
	breakerWindow   = 5
	breakerFailures = 3
	breakerOpen     = 60 * time.Second
)

func NewBreaker(now func() time.Time) *Breaker { return &Breaker{now: now} }

// Allow admits a call. probe is true for the single half-open call; while it is in
// flight, and while open, Allow returns PROVIDER_UNAVAILABLE.
func (b *Breaker) Allow() (probe bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.open {
		return false, nil
	}
	if b.probing || b.now().Before(b.openUntil) {
		return false, &ProviderError{Code: CodeProviderUnavailable}
	}
	b.probing = true
	return true, nil
}

// Record reports a call outcome. A canceled call (caller gave up) releases a probe without
// counting either way.
func (b *Breaker) Record(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var p *ProviderError
	// One tenant's reply refused for its content must not open the breaker every tenant of the
	// process shares (WIS-696, security audit run 1). While closed it is neutral: counted as a
	// success it would push real failures out of the window, so refusals could mask a flapping
	// endpoint. On the half-open probe it shows the endpoint answered, so it closes the breaker.
	if errors.As(err, &p) && p.content {
		if !b.open {
			return
		}
		err = nil
	}
	if errors.As(err, &p) && p.Code == CodeProviderCanceled {
		b.probing = false
		return
	}
	if b.open {
		if !b.probing {
			return
		}
		b.probing = false
		if err == nil {
			b.open, b.outcomes = false, nil
		} else {
			b.openUntil = b.now().Add(breakerOpen)
		}
		return
	}
	b.outcomes = append(b.outcomes, err != nil)
	if len(b.outcomes) > breakerWindow {
		b.outcomes = b.outcomes[1:]
	}
	failures := 0
	for _, f := range b.outcomes {
		if f {
			failures++
		}
	}
	if failures >= breakerFailures {
		b.open, b.openUntil, b.outcomes = true, b.now().Add(breakerOpen), nil
	}
}
