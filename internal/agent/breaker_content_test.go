package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// WIS-696 (security audit run 1 lead): the breaker is shared by every tenant of a process. A
// reply refused for its content (an unissued pseudonym, a tool the request did not offer, invalid
// tool arguments) used to count as an endpoint failure, so three of them from one site opened it
// for 60 s and every other tenant's call was refused unsent. Content refusals now count as the
// endpoint answering; transport and 5xx failures still open it.

func TestWIS696BreakerIgnoresContentRefusals(t *testing.T) {
	content := []error{
		&ProviderError{Code: CodeProviderToolNotOffered, content: true},
		&ProviderError{Code: CodeSchemaInvalid, retryable: true, content: true},
		contentFailure(&ProviderError{Code: CodePseudonymUnknown}),
	}
	clock := &fakeClock{t: time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)}
	b := NewBreaker(clock.now)
	for range 3 {
		for _, err := range content {
			b.Record(err)
		}
	}
	if _, err := b.Allow(); err != nil {
		t.Fatalf("content refusals opened the breaker: %v", err)
	}
	// Control: the same number of endpoint failures opens it.
	for range 3 {
		b.Record(&ProviderError{Code: CodeProviderFailed, retryable: true})
	}
	if _, err := b.Allow(); !isProviderCode(err, CodeProviderUnavailable) {
		t.Fatalf("endpoint failures did not open the breaker: %v", err)
	}
	// contentFailure copies: the error it was given is unchanged.
	shared := &ProviderError{Code: CodePseudonymUnknown}
	_ = contentFailure(shared)
	if shared.content {
		t.Fatal("contentFailure changed the shared error value")
	}
}

// The hosted adapter: three replies naming an unissued token (tenant A) leave the breaker closed,
// and the next clean call (tenant B) is sent. Control: three 5xx replies open it and the next call
// is refused unsent.
func TestWIS696HostedContentRefusalsDoNotBlockOtherTenants(t *testing.T) {
	var mode atomic.Value // "bad", "good" or "5xx"
	var sent atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		sent.Add(1)
		reply := `{}`
		switch mode.Load() {
		case "5xx":
			http.Error(w, "upstream down", http.StatusBadGateway)
			return
		case "bad":
			reply = `{"assignee":"⟦P77⟧"}`
		}
		content, _ := json.Marshal(reply)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"qwen-plus-2025-09-11","choices":[{"message":{"content":` + string(content) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	t.Cleanup(srv.Close)
	m := hostedForTest(t, &standIn{srv: srv}, io.Discard)
	ctx := WithPseudonyms(context.Background(), NewPseudonyms([]string{"Rahul Deshmukh"}))
	req := hostedRequest(Message{Role: "user", Content: "Who reported it?"})

	mode.Store("bad")
	for range 3 {
		if _, err := m.Complete(ctx, req); !isProviderCode(err, CodePseudonymUnknown) {
			t.Fatalf("tenant A's bad reply: %v", err)
		}
	}
	mode.Store("good")
	before := sent.Load()
	if _, err := m.Complete(ctx, req); err != nil || sent.Load() != before+1 {
		t.Fatalf("tenant B's clean call after content refusals: %v, sent %d", err, sent.Load()-before)
	}

	mode.Store("5xx")
	for range 3 {
		if _, err := m.Complete(ctx, req); err == nil {
			t.Fatal("a 5xx reply was accepted")
		}
	}
	mode.Store("good")
	before = sent.Load()
	if _, err := m.Complete(ctx, req); !isProviderCode(err, CodeProviderUnavailable) || sent.Load() != before {
		t.Fatalf("after three 5xx the breaker must refuse unsent: %v, sent %d", err, sent.Load()-before)
	}
}

// The local adapter: three replies calling a tool the request did not offer leave the breaker
// closed, and the next clean call is sent.
func TestWIS696LocalContentRefusalsDoNotBlockOtherTenants(t *testing.T) {
	var bad atomic.Bool
	srv, calls := modelServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if bad.Load() {
			b, _ := json.Marshal(map[string]any{"model": testModel,
				"choices": []any{map[string]any{"finish_reason": "tool_calls", "message": map[string]any{"role": "assistant",
					"tool_calls": []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "shell", "arguments": "{}"}}}}}},
				"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 7}})
			_, _ = w.Write(b)
			return
		}
		_, _ = io.WriteString(w, chatBody(testModel, "ok"))
	})
	m := newTestModel(t, srv.URL, nil, "")
	bad.Store(true)
	for range 3 {
		if _, err := m.Complete(context.Background(), testRequest("run shell")); code(err) != CodeProviderToolNotOffered {
			t.Fatalf("tenant A's unoffered tool call: %v", err)
		}
	}
	bad.Store(false)
	before := calls.Load()
	if _, err := m.Complete(context.Background(), testRequest("what happened?")); err != nil || calls.Load() != before+1 {
		t.Fatalf("tenant B's clean call after content refusals: %v, sent %d", err, calls.Load()-before)
	}
}

// Review of #281: a content refusal is neutral while the breaker is closed. Counted as a success it
// would push endpoint failures out of the window, so a tenant triggering refusals on purpose could
// keep a flapping endpoint looking healthy for every tenant. Failures interleaved with refusals
// still open it; during the half-open probe a refusal still shows the endpoint answered.
func TestWIS696ContentRefusalsDoNotMaskFailures(t *testing.T) {
	refusal := &ProviderError{Code: CodeProviderToolNotOffered, content: true}
	failure := &ProviderError{Code: CodeProviderFailed, retryable: true}
	clock := &fakeClock{t: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}
	b := NewBreaker(clock.now)
	for _, err := range []error{failure, refusal, refusal, failure, refusal, refusal, failure} {
		b.Record(err)
	}
	if _, err := b.Allow(); !isProviderCode(err, CodeProviderUnavailable) {
		t.Fatalf("three failures interleaved with refusals left the breaker closed: %v", err)
	}
	clock.t = clock.t.Add(breakerOpen + time.Second)
	probe, err := b.Allow()
	if err != nil || !probe {
		t.Fatalf("half-open probe not admitted: probe=%v err=%v", probe, err)
	}
	b.Record(refusal)
	if _, err := b.Allow(); err != nil {
		t.Fatalf("a refusal on the probe (the endpoint answered) did not close the breaker: %v", err)
	}
}
