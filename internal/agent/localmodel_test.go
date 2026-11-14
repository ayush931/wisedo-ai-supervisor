package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
)

const testModel = "qwen2.5-7b-instruct"

func testRequest(prompt string) ModelRequest {
	return ModelRequest{SchemaVersion: ModelSchemaVersion, Model: testModel, PromptVersion: "p1", ToolVersion: "t1",
		Messages: []Message{{Role: "user", Content: prompt}}, Tools: []ToolDef{{Name: ToolEvidenceGet, Parameters: json.RawMessage(`{"type":"object"}`)}}, MaxOutputTokens: 100}
}

func chatBody(model, content string) string {
	b, _ := json.Marshal(map[string]any{"id": "x", "object": "chat.completion", "model": model, "system_fingerprint": "vendor-extra",
		"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": content}}},
		"usage":   map[string]any{"prompt_tokens": 11, "completion_tokens": 7}})
	return string(b)
}

// modelServer answers /v1/models and /v1/chat/completions with handler, counting chat calls.
func modelServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprintf(w, `{"data":[{"id":%q}]}`, testModel)
			return
		}
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func newTestModel(t *testing.T, base string, logs *bytes.Buffer, apiKeyFile string, allow ...string) *LocalModel {
	t.Helper()
	if logs == nil {
		logs = &bytes.Buffer{}
	}
	m, err := NewLocalModel(config.Agent{Provider: config.AgentProviderLocal, BaseURL: base, Model: testModel, ModelRevision: "sha256:pinned",
		APIKeyFile: apiKeyFile, EndpointAllowlist: allow, EndpointAddrs: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:0")}, Timeout: 5 * time.Second}, slog.New(slog.NewJSONHandler(logs, nil)), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func code(err error) string {
	var p *ProviderError
	if errors.As(err, &p) {
		return p.Code
	}
	return fmt.Sprint(err)
}

// The pinned local server answering with the pinned model is the positive control for every case below.
func TestLocalModelHappyPath(t *testing.T) {
	srv, calls := modelServer(t, func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["model"] != testModel || in["stream"] != false {
			w.WriteHeader(400)
			return
		}
		fmt.Fprint(w, chatBody(testModel, `{"ok":true}`))
	})
	m := newTestModel(t, srv.URL, nil, "")
	resp, err := m.Complete(t.Context(), testRequest("hi"))
	if err != nil || resp.Content != `{"ok":true}` || resp.ProviderActual != ProviderLocal || resp.ProviderModel != testModel ||
		resp.ModelRevision != "sha256:pinned" || resp.InputTokens != 11 || resp.OutputTokens != 7 || calls.Load() != 1 {
		t.Fatalf("resp=%+v err=%v calls=%d", resp, err, calls.Load())
	}
	if err := m.Probe(t.Context()); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

func TestPublicEndpointRefusedAtConstruction(t *testing.T) {
	pin := []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:8000")}
	for base, addrs := range map[string][]netip.AddrPort{"http://8.8.8.8:8000": pin, "http://api.openai.com": pin,
		"http://127.0.0.1:8000": nil, "http://127.0.0.1:9000": pin, "http://10.0.0.5:8000": pin} {
		if _, err := NewLocalModel(config.Agent{Provider: config.AgentProviderLocal, BaseURL: base, Model: testModel, ModelRevision: "r", EndpointAddrs: addrs, Timeout: time.Second},
			slog.New(slog.DiscardHandler), time.Now); err == nil {
			t.Errorf("%s accepted", base)
		}
	}
}

// DNS rebinding: an allowlisted name that resolves to a public IP is refused at dial time
// and no byte reaches any server. The resolver is a test hook; no real DNS is used.
func TestDialRefusesNameResolvingToPublicIP(t *testing.T) {
	srv, calls := modelServer(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, chatBody(testModel, "{}")) })
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	m := newTestModel(t, fmt.Sprintf("http://model.plant.internal:%d", port), nil, "", "model.plant.internal")
	for _, answer := range [][]netip.Addr{
		{netip.MustParseAddr("8.8.8.8")},
		{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("8.8.8.8")}, // one public answer taints the set
		{netip.MustParseAddr("::ffff:8.8.8.8")},
	} {
		m.lookup = func(context.Context, string) ([]netip.Addr, error) { return answer, nil }
		if _, err := m.Complete(t.Context(), testRequest("hi")); code(err) != CodeEndpointNotLocal || Retryable(err) {
			t.Errorf("%v: %v", answer, err)
		}
	}
	// Private-to-private rebinding: the name moves to another local service outside
	// SUPERVISOR_AGENT_ENDPOINT_ADDRS. It is refused even though the address is private. (A fresh
	// client: the refusals above opened the first one's breaker.)
	m = newTestModel(t, fmt.Sprintf("http://model.plant.internal:%d", port), nil, "", "model.plant.internal")
	for _, answer := range [][]netip.Addr{
		{netip.MustParseAddr("10.1.2.3")},
		{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("192.168.7.9")},
		{netip.MustParseAddr("127.0.0.2")},
	} {
		m.lookup = func(context.Context, string) ([]netip.Addr, error) { return answer, nil }
		if _, err := m.Complete(t.Context(), testRequest("hi")); code(err) != CodeEndpointNotPinned || Retryable(err) {
			t.Errorf("%v: %v", answer, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("%d requests reached the server", calls.Load())
	}
	// Negative control: the same name resolving to the pinned loopback reaches the server.
	m = newTestModel(t, fmt.Sprintf("http://model.plant.internal:%d", port), nil, "", "model.plant.internal")
	m.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	if _, err := m.Complete(t.Context(), testRequest("hi")); err != nil || calls.Load() != 1 {
		t.Fatalf("loopback resolution: %v calls=%d", err, calls.Load())
	}
}

// HTTP(S)_PROXY must not route model traffic. The target is an allowlisted name (not a
// loopback literal, which Go's environment proxy would bypass anyway).
func TestProxyEnvironmentIgnored(t *testing.T) {
	var proxied atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxied.Add(1)
		fmt.Fprint(w, chatBody(testModel, `{"via":"proxy"}`))
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	srv, calls := modelServer(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, chatBody(testModel, `{"via":"direct"}`)) })
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	m := newTestModel(t, fmt.Sprintf("http://model.plant.internal:%d", port), nil, "", "model.plant.internal")
	m.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	resp, err := m.Complete(t.Context(), testRequest("hi"))
	if err != nil || resp.Content != `{"via":"direct"}` || proxied.Load() != 0 || calls.Load() != 1 {
		t.Fatalf("resp=%q err=%v proxied=%d direct=%d", resp.Content, err, proxied.Load(), calls.Load())
	}
}

func TestResponseGuards(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		code    string
		retry   bool
	}{
		{"wrong model", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, chatBody("gpt-4o", "{}")) }, CodeModelNotPinned, false},
		{"missing model", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, chatBody("", "{}")) }, CodeModelNotPinned, false},
		{"oversized", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, chatBody(testModel, strings.Repeat("a", maxModelResponseBytes)))
		}, CodeProviderResponseLarge, false},
		{"malformed", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"model":`) }, CodeSchemaInvalid, true},
		{"no usage", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `{"model":%q,"choices":[{"message":{"content":"{}"}}]}`, testModel)
		}, CodeSchemaInvalid, true},
		{"tool not offered", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `{"model":%q,"choices":[{"message":{"tool_calls":[{"id":"1","type":"function","function":{"name":"shell.exec","arguments":"{}"}}]}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, testModel)
		}, CodeProviderToolNotOffered, false},
		{"503", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }, CodeProviderFailed, true},
		{"429", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(429) }, CodeProviderFailed, true},
		{"401", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) }, CodeProviderFailed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := modelServer(t, tc.handler)
			_, err := newTestModel(t, srv.URL, nil, "").Complete(t.Context(), testRequest("hi"))
			if code(err) != tc.code || Retryable(err) != tc.retry {
				t.Fatalf("got %v retryable=%v, want %s retryable=%v", err, Retryable(err), tc.code, tc.retry)
			}
		})
	}
}

func TestTimeoutIsRetryable(t *testing.T) {
	release := make(chan struct{})
	srv, _ := modelServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := newTestModel(t, srv.URL, nil, "").Complete(ctx, testRequest("hi"))
	if code(err) != CodeDeadlineExceeded || !Retryable(err) {
		t.Fatalf("timeout: %v retryable=%v", err, Retryable(err))
	}
}

func TestRedirectNeverFollowed(t *testing.T) {
	var second atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		second.Add(1)
		fmt.Fprint(w, chatBody(testModel, "{}"))
	}))
	defer target.Close()
	srv, _ := modelServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/chat/completions", http.StatusTemporaryRedirect)
	})
	key := filepath.Join(t.TempDir(), "key")
	_ = os.WriteFile(key, []byte("k"), 0o600)
	_, err := newTestModel(t, srv.URL, nil, key).Complete(t.Context(), testRequest("hi"))
	if code(err) != CodeProviderFailed || Retryable(err) || second.Load() != 0 {
		t.Fatalf("redirect: %v second=%d", err, second.Load())
	}
}

// Rule 08: prompts, completions, upstream bodies and the key never reach logs or errors;
// the log carries the request hash and reason code instead.
func TestPromptsAndSecretsNotLogged(t *testing.T) {
	const prompt, key, completion, echoed = "CANARY-PROMPT-7f3a", "CANARY-KEY-91c2", "CANARY-COMPLETION-55d0", "CANARY-UPSTREAM-e81b"
	var sawKey atomic.Bool
	fail := atomic.Bool{}
	srv, _ := modelServer(t, func(w http.ResponseWriter, r *http.Request) {
		sawKey.Store(r.Header.Get("Authorization") == "Bearer "+key)
		if fail.Load() {
			w.WriteHeader(500)
			fmt.Fprint(w, echoed+" "+prompt)
			return
		}
		fmt.Fprint(w, chatBody(testModel, completion))
	})
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	m := newTestModel(t, srv.URL, &logs, keyFile)
	req := testRequest(prompt)
	if resp, err := m.Complete(t.Context(), req); err != nil || resp.Content != completion || !sawKey.Load() {
		t.Fatalf("success call: %v key sent=%v", err, sawKey.Load())
	}
	fail.Store(true)
	_, err := m.Complete(t.Context(), req)
	if code(err) != CodeProviderFailed {
		t.Fatalf("500 call: %v", err)
	}
	out := logs.String() + err.Error()
	for _, canary := range []string{prompt, key, completion, echoed} {
		if strings.Contains(out, canary) {
			t.Errorf("%s leaked into logs or errors", canary)
		}
	}
	if !strings.Contains(out, req.Digest()) || !strings.Contains(out, CodeProviderFailed) || strings.Count(logs.String(), "agent model call") != 2 {
		t.Fatalf("log lacks the request hash or reason code: %s", logs.String())
	}
	if u, _ := url.Parse(srv.URL); strings.Contains(out, u.Host) {
		t.Error("endpoint address leaked")
	}
}

// llama.cpp's response_format grammar constrains every token to the schema, so a model offered
// tools could never emit a tool call (seen live with Qwen3 8B: the investigation could not read
// measurements.get). With tools the schema travels as system text; without tools it stays the
// strict response_format. Shared by the local and hosted adapters.
func TestResponseSchemaTravelsAsTextWhenToolsOffered(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"],"additionalProperties":false}`)
	for _, tc := range []struct {
		name  string
		tools []ToolDef
	}{{"with tools", ToolDefs([]string{ToolMeasurementsGet})}, {"without tools", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			var wire map[string]any
			do := func(_ context.Context, _, _ string, body []byte) ([]byte, error) {
				if err := json.Unmarshal(body, &wire); err != nil {
					t.Fatal(err)
				}
				return nil, errors.New("captured")
			}
			req := ModelRequest{SchemaVersion: ModelSchemaVersion, Model: testModel, ResponseSchema: schema, Tools: tc.tools, MaxOutputTokens: 10,
				Messages: []Message{{Role: "system", Content: "be brief"}, {Role: "user", Content: "hi"}}}
			_, _ = openAIChat(context.Background(), do, testModel, "r", ProviderLocal, req, "")
			system := wire["messages"].([]any)[0].(map[string]any)["content"].(string)
			_, hasFormat := wire["response_format"]
			if tc.tools != nil {
				if hasFormat || !strings.HasPrefix(system, "be brief\n") || !strings.HasSuffix(system, string(schema)) {
					t.Fatalf("tools offered: response_format=%v system=%q", hasFormat, system)
				}
				if _, ok := wire["tools"]; !ok {
					t.Fatal("tools dropped")
				}
				return
			}
			format, _ := wire["response_format"].(map[string]any)
			js, _ := format["json_schema"].(map[string]any)
			var want any
			_ = json.Unmarshal(schema, &want)
			if format["type"] != "json_schema" || js["strict"] != true || !reflect.DeepEqual(js["schema"], want) || system != "be brief" {
				t.Fatalf("no tools: response_format=%v system=%q", format, system)
			}
		})
	}
}

// WIS-631: a configured reasoning effort is sent on every chat request, so a thinking model does
// not spend its output budget reasoning; unset, the request carries no such field.
func TestReasoningEffortIsSentOnlyWhenConfigured(t *testing.T) {
	for _, effort := range []string{"", "none", "high"} {
		var wire map[string]any
		do := func(_ context.Context, _, _ string, body []byte) ([]byte, error) {
			if err := json.Unmarshal(body, &wire); err != nil {
				t.Fatal(err)
			}
			return nil, errors.New("captured")
		}
		req := ModelRequest{SchemaVersion: ModelSchemaVersion, Model: testModel, MaxOutputTokens: 10, Messages: []Message{{Role: "user", Content: "hi"}}}
		_, _ = openAIChat(context.Background(), do, testModel, "r", ProviderLocal, req, effort)
		got, present := wire["reasoning_effort"]
		if effort == "" && present || effort != "" && got != effort {
			t.Errorf("effort %q: request reasoning_effort=%v (present %v)", effort, got, present)
		}
	}
}
