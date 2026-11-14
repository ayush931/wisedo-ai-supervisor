package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
)

func standIn(t *testing.T, reply func(path string, body map[string]any) any) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		_ = json.NewEncoder(w).Encode(reply(r.URL.Path, in))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return NewWithTransport(config.Embeddings{Model: "emb", ModelRevision: "r1", RerankModel: "rr", RerankModelRevision: "r1", Timeout: 2 * time.Second}, u, srv.Client().Transport)
}

func vec(fill float32) []float32 {
	v := make([]float32, Dimensions)
	v[0] = fill
	return v
}

// The endpoint's answer is untrusted: a wrong count, dimension, index or model refuses the
// whole batch; vectors come back unit length.
func TestEmbedValidatesTheEndpoint(t *testing.T) {
	for name, c := range map[string]struct {
		reply any
		want  error
	}{
		"ok":              {map[string]any{"model": "emb", "data": []any{map[string]any{"index": 0, "embedding": vec(3)}}}, nil},
		"other model":     {map[string]any{"model": "x", "data": []any{map[string]any{"index": 0, "embedding": vec(3)}}}, ErrEndpoint},
		"missing vector":  {map[string]any{"model": "emb", "data": []any{}}, ErrEndpoint},
		"wrong dimension": {map[string]any{"model": "emb", "data": []any{map[string]any{"index": 0, "embedding": []float32{1, 2}}}}, ErrDimension},
		"bad index":       {map[string]any{"model": "emb", "data": []any{map[string]any{"index": 4, "embedding": vec(3)}}}, ErrDimension},
		"zero vector":     {map[string]any{"model": "emb", "data": []any{map[string]any{"index": 0, "embedding": vec(0)}}}, ErrDimension},
	} {
		c := c
		cl := standIn(t, func(string, map[string]any) any { return c.reply })
		got, err := cl.Embed(context.Background(), []string{"q"})
		if !errors.Is(err, c.want) {
			t.Fatalf("%s: %v, want %v", name, err, c.want)
		}
		if c.want == nil && (len(got) != 1 || got[0][0] != 1) {
			t.Fatalf("%s: not unit length: %v", name, got[0][:1])
		}
	}
}

func TestRerankReturnsScoresInDocumentOrder(t *testing.T) {
	cl := standIn(t, func(path string, body map[string]any) any {
		if path != "/v1/rerank" || body["model"] != "rr" {
			return map[string]any{}
		}
		return map[string]any{"results": []any{map[string]any{"index": 1, "relevance_score": 0.9}, map[string]any{"index": 0, "relevance_score": 0.1}}}
	})
	got, err := cl.Rerank(context.Background(), "q", []string{"a", "b"})
	if err != nil || len(got) != 2 || got[0] != 0.1 || got[1] != 0.9 {
		t.Fatalf("rerank: %v %v", got, err)
	}
	missing := standIn(t, func(string, map[string]any) any {
		return map[string]any{"results": []any{map[string]any{"index": 0, "relevance_score": 0.5}}}
	})
	if _, err = missing.Rerank(context.Background(), "q", []string{"a", "b"}); !errors.Is(err, ErrEndpoint) {
		t.Fatalf("a missing score was accepted: %v", err)
	}
}

// The endpoint must be local or private and pinned, like the local model.
// WIS-627: a CPU endpoint answers slower than the question-time budget. The question-time call
// times out (ErrTimeout, so the worker can say so), while indexing has its own longer budget and
// embeds. Scaled down: 200ms answer, 50ms question budget, 2s index budget.
func TestIndexingOutlastsTheQuestionTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "emb", "data": []map[string]any{{"index": 0, "embedding": vec(1)}}})
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	c := NewWithTransport(config.Embeddings{Model: "emb", ModelRevision: "r1", Timeout: 50 * time.Millisecond, IndexTimeout: 2 * time.Second}, u, srv.Client().Transport)
	if _, err := c.Embed(context.Background(), []string{"question"}); !errors.Is(err, ErrTimeout) || !errors.Is(err, errUnreachable) {
		t.Fatalf("question-time embed: %v", err)
	}
	vecs, err := c.embed(context.Background(), c.index, []string{"span"})
	if err != nil || len(vecs) != 1 || vecs[0][0] != 1 {
		t.Fatalf("indexing embed: %v %v", vecs, err)
	}
	if c.IndexTimeout() != 2*time.Second {
		t.Fatalf("index timeout %v", c.IndexTimeout())
	}
	// The index budget is the configured one, even when it is shorter than the question budget
	// (WIS-627 review: no max of the two).
	c = NewWithTransport(config.Embeddings{Model: "emb", ModelRevision: "r1", Timeout: 2 * time.Second, IndexTimeout: 50 * time.Millisecond}, u, srv.Client().Transport)
	if _, err := c.embed(context.Background(), c.index, []string{"span"}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("index budget shorter than question budget: %v", err)
	}
}

// WIS-627 review: an endpoint that sends headers and then stalls (or drops the body) gave no
// answer; it is unreachable, timed out when the budget ran out, and never a refusal (which made
// EmbedPending resend every span alone and defer them).
func TestBodyReadFailureIsUnreachableNotRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stall/v1/embeddings" {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte(`{"mo`)) // short body, then the handler returns
	}))
	t.Cleanup(srv.Close)
	for path, timedOut := range map[string]bool{"/stall": true, "/short": false} {
		u, _ := url.Parse(srv.URL + path)
		c := NewWithTransport(config.Embeddings{Model: "emb", ModelRevision: "r1", Timeout: time.Second, IndexTimeout: 100 * time.Millisecond}, u, srv.Client().Transport)
		_, err := c.embed(context.Background(), c.index, []string{"span"})
		if !errors.Is(err, errUnreachable) || errors.Is(err, ErrTimeout) != timedOut {
			t.Errorf("%s: %v", path, err)
		}
	}
}

type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

// WIS-627 review: a dial that fails fast (even with a timeout error) is unreachable, not a spent
// request budget, so the worker does not log "timed out after <budget>".
func TestDialFailureIsNotABudgetTimeout(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:1")
	dialTimeout := &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}
	c := NewWithTransport(config.Embeddings{Model: "emb", ModelRevision: "r1", Timeout: time.Second, IndexTimeout: time.Minute}, u, failingTransport{dialTimeout})
	_, err := c.embed(context.Background(), c.index, []string{"span"})
	if !errors.Is(err, errUnreachable) || errors.Is(err, ErrTimeout) {
		t.Fatalf("dial failure: %v", err)
	}
}

func TestNewRefusesAPublicEndpoint(t *testing.T) {
	_, err := New(config.Embeddings{Enabled: true, BaseURL: "http://8.8.8.8:8000", Model: "emb", ModelRevision: "r1", Timeout: time.Second,
		EndpointAddrs: []netip.AddrPort{netip.MustParseAddrPort("8.8.8.8:8000")}})
	if err == nil {
		t.Fatal("a public embedding endpoint was accepted")
	}
}

// The refusal backoff map stays bounded: a span that embeds is forgotten, an entry idle for a day
// is dropped, and a recently expired one keeps its count (so a repeat refusal backs off longer)
// without being listed as deferred.
func TestDeferralsAreForgottenAndPruned(t *testing.T) {
	c := &Client{}
	c.deferSpan("t", "embedded")
	c.deferSpan("t", "waiting")
	c.deferSpan("u", "other-tenant")
	if got := c.deferredSpans("t"); len(got) != 2 {
		t.Fatalf("deferred %v", got)
	}
	c.forget("t", "embedded")
	if got := c.deferredSpans("t"); len(got) != 1 || got[0] != "waiting" {
		t.Fatalf("after forget: %v", got)
	}
	c.deferred["t/stale"] = deferral{attempts: 3, until: time.Now().Add(-deferralMemory - time.Minute)}
	c.deferred["t/expired"] = deferral{attempts: 2, until: time.Now().Add(-time.Minute)}
	if got := c.deferredSpans("t"); len(got) != 1 || got[0] != "waiting" {
		t.Fatalf("listed %v", got)
	}
	if _, kept := c.deferred["t/stale"]; kept {
		t.Fatal("an entry idle for over a day was kept")
	}
	if d, kept := c.deferred["t/expired"]; !kept || d.attempts != 2 {
		t.Fatal("a recently expired entry lost its refusal count")
	}
	c.deferSpan("t", "expired")
	if d := c.deferred["t/expired"]; d.attempts != 3 || time.Until(d.until) < 7*time.Minute {
		t.Fatalf("a repeat refusal did not back off longer: %+v", d)
	}
}
