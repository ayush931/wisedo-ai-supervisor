package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/retrieval"
)

// standIn answers /v1/embeddings with 1024-dimension vectors and /v1/rerank with scores, unless
// told to fail the rerank.
func standIn(t *testing.T, model string, failRerank bool) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Input     []string `json:"input"`
			Documents []string `json:"documents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		switch r.URL.Path {
		case "/v1/embeddings":
			type item struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			}
			out := struct {
				Model string `json:"model"`
				Data  []item `json:"data"`
			}{Model: model}
			for i := range in.Input {
				v := make([]float32, retrieval.Dimensions)
				v[i%retrieval.Dimensions] = 1
				out.Data = append(out.Data, item{Index: i, Embedding: v})
			}
			_ = json.NewEncoder(w).Encode(out)
		case "/v1/rerank":
			if failRerank {
				http.Error(w, "no", http.StatusInternalServerError)
				return
			}
			type res struct {
				Index int     `json:"index"`
				Score float64 `json:"relevance_score"`
			}
			var out struct {
				Results []res `json:"results"`
			}
			for i := range in.Documents {
				out.Results = append(out.Results, res{Index: i, Score: 1})
			}
			_ = json.NewEncoder(w).Encode(out)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func embeddingsFor(t *testing.T, s *httptest.Server, rerank bool) config.Embeddings {
	t.Helper()
	ap, err := netip.ParseAddrPort(strings.TrimPrefix(s.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	e := config.Embeddings{Enabled: true, BaseURL: s.URL, EndpointAllowlist: []string{"127.0.0.1"}, EndpointAddrs: []netip.AddrPort{ap},
		Model: "emb", ModelRevision: "r1", Timeout: 2 * time.Second, IndexTimeout: 2 * time.Second}
	if rerank {
		e.RerankModel, e.RerankModelRevision = "rr", "r1"
	}
	return e
}

// WIS-647: a hybrid mode runs only against a reachable, pinned endpoint configured for exactly that
// mode; anything else is a missing prerequisite (BLOCKED), never a silent word-mode run.
func TestRetrievalSetup(t *testing.T) {
	ok := standIn(t, "emb", false)
	noRerank := standIn(t, "emb", true)
	closed := standIn(t, "emb", false)
	closedCfg := embeddingsFor(t, closed, false)
	closed.Close()
	load := func(e config.Embeddings, err error) func() (config.Embeddings, error) {
		return func() (config.Embeddings, error) { return e, err }
	}
	for _, c := range []struct {
		name, mode, want string
		load             func() (config.Embeddings, error)
	}{
		{"unknown mode", "semantic", "unknown -retrieval", load(config.Embeddings{}, nil)},
		{"settings invalid", RetrievalHybrid, "bad settings", load(config.Embeddings{}, errors.New("bad settings"))},
		{"embeddings off", RetrievalHybrid, "SUPERVISOR_EMBEDDINGS_ENABLED", load(config.Embeddings{}, nil)},
		{"hybrid with a rerank model", RetrievalHybrid, "that is hybrid_rerank", load(embeddingsFor(t, ok, true), nil)},
		{"hybrid_rerank without one", RetrievalHybridRerank, "RERANK_MODEL", load(embeddingsFor(t, ok, false), nil)},
		{"endpoint unreachable", RetrievalHybrid, "embedding endpoint probe", load(closedCfg, nil)},
		{"rerank failing", RetrievalHybridRerank, "rerank endpoint probe", load(embeddingsFor(t, noRerank, true), nil)},
		// An endpoint whose address is not pinned is refused before any request.
		{"address not pinned", RetrievalHybrid, "not in SUPERVISOR_EMBEDDINGS_ENDPOINT_ADDRS", load(func() config.Embeddings {
			e := embeddingsFor(t, ok, false)
			e.EndpointAddrs = []netip.AddrPort{netip.AddrPortFrom(e.EndpointAddrs[0].Addr(), e.EndpointAddrs[0].Port()+1)}
			return e
		}(), nil)},
		{"wrong model answers", RetrievalHybrid, "embedding endpoint probe", load(func() config.Embeddings { e := embeddingsFor(t, ok, false); e.Model = "other"; return e }(), nil)},
	} {
		t.Run(c.name, func(t *testing.T) {
			rs, err := newRetrievalSetup(context.Background(), c.mode, c.load)
			if err == nil || !strings.Contains(err.Error(), c.want) || rs != nil {
				t.Fatalf("setup = %v, %v; want an error with %q", rs, err, c.want)
			}
		})
	}
	// Controls: word mode needs nothing and has no client; each hybrid mode builds one.
	if rs, err := newRetrievalSetup(context.Background(), RetrievalWord, load(config.Embeddings{}, errors.New("unused"))); err != nil || rs.client != nil || rs.wantLabel() != "word" {
		t.Fatalf("word: %+v %v", rs, err)
	}
	for mode, rerank := range map[string]bool{RetrievalHybrid: false, RetrievalHybridRerank: true} {
		rs, err := newRetrievalSetup(context.Background(), mode, load(embeddingsFor(t, ok, rerank), nil))
		if err != nil || rs.client == nil || rs.wantLabel() != "hybrid" {
			t.Fatalf("%s: %+v %v", mode, rs, err)
		}
	}
}

// WIS-647: each reply is held to its mode. The call log also decides whether a rerank happened,
// independently of the label the product gives a skipped rerank (hybrid_unreranked).
func TestRetrievalJudge(t *testing.T) {
	word := &retrievalSetup{mode: RetrievalWord}
	for _, c := range []struct {
		name  string
		rs    *retrievalSetup
		label string
		calls func(l *callLog) // what the endpoint answered for this question
		want  []string         // substrings of the failures; nil: none
	}{
		{"word reply in word mode", word, "word", nil, nil},
		{"hybrid reply in word mode", word, "hybrid", nil, []string{`reply retrieval = "hybrid", want "word"`}},
		{"hybrid reply with its embedding", &retrievalSetup{mode: RetrievalHybrid, calls: &callLog{}}, "hybrid", func(l *callLog) { l.embedOK++ }, nil},
		{"word_fallback in hybrid", &retrievalSetup{mode: RetrievalHybrid, calls: &callLog{}}, "word_fallback", func(l *callLog) { l.embedOK++ }, []string{`want "hybrid"`}},
		{"no question embedding", &retrievalSetup{mode: RetrievalHybrid, calls: &callLog{}}, "hybrid", nil, []string{"no embedding call answered"}},
		{"failed question embedding", &retrievalSetup{mode: RetrievalHybrid, calls: &callLog{}}, "hybrid", func(l *callLog) { l.embedOK++; l.embedFailed++ }, []string{"embedding call(s) failed"}},
		{"reranked", &retrievalSetup{mode: RetrievalHybridRerank, calls: &callLog{}}, "hybrid", func(l *callLog) { l.embedOK++; l.rerankOK++ }, nil},
		{"rerank failed, label still hybrid", &retrievalSetup{mode: RetrievalHybridRerank, calls: &callLog{}}, "hybrid", func(l *callLog) { l.embedOK++; l.rerankFailed++ }, []string{"rerank call(s) failed"}},
		{"no rerank call", &retrievalSetup{mode: RetrievalHybridRerank, calls: &callLog{}}, "hybrid", func(l *callLog) { l.embedOK++ }, []string{"no rerank call answered"}},
		{"skipped rerank labelled by the product", &retrievalSetup{mode: RetrievalHybridRerank, calls: &callLog{}}, "hybrid_unreranked", func(l *callLog) { l.embedOK++; l.rerankFailed++ }, []string{`want "hybrid"`, "rerank call(s) failed"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.rs.client == nil && c.rs.mode != RetrievalWord {
				c.rs.client = &retrieval.Client{} // a hybrid setup has a client; judge never calls it
			}
			if c.rs.calls != nil {
				c.rs.calls.embedOK = 3 // calls before this question (indexing) never count for it
			}
			before := c.rs.mark()
			if c.calls != nil {
				c.calls(c.rs.calls)
			}
			got := c.rs.judge(c.label, before)
			if len(got) != len(c.want) {
				t.Fatalf("failures %q, want %d matching %q", got, len(c.want), c.want)
			}
			for i, w := range c.want {
				if !strings.Contains(got[i], FailureRetrievalMode) || !strings.Contains(got[i], w) {
					t.Fatalf("failure %q, want %s with %q", got[i], FailureRetrievalMode, w)
				}
			}
		})
	}
}

// WIS-647: the synonym set runs only when named; an empty CASE keeps the default suites.
func TestSynonymSuiteOnlyByName(t *testing.T) {
	if got, err := selectSuites(""); err != nil || slices.Contains(got, SuiteChatSynonyms) {
		t.Fatalf("default suites %v %v", got, err)
	}
	if got, err := selectSuites("chat,chat_synonyms"); err != nil || !slices.Equal(got, []string{SuiteChat, SuiteChatSynonyms}) {
		t.Fatalf("named: %v %v", got, err)
	}
	if _, err := selectSuites("synonyms"); err == nil {
		t.Fatal("an unknown suite was accepted")
	}
}

// WIS-700: chat gate controls run on the same retrieval as the corpus but are not judged on it.
// A control expects ASSERTION_MISMATCH, so a retrieval mismatch would make it fail as required
// even when the gate itself caught nothing. Control: a corpus reply in the same state is judged.
func TestWIS700ChatControlsSkipRetrievalJudge(t *testing.T) {
	rs := &retrievalSetup{mode: RetrievalHybrid, client: &retrieval.Client{}, calls: &callLog{}}
	control := ChatCase{CaseID: "chat-control", ExpectedFailure: FailureAssertionMismatch}
	before := rs.mark() // no embedding answered and the reply says word: a mismatch twice over
	res := CaseResult{CaseID: control.CaseID}
	for _, f := range rs.judgeChat(control, "word_fallback", before) {
		res.Failures = append(res.Failures, "run 1: "+f)
	}
	if cr := knowledgeControl(control.CaseID, control.ExpectedFailure, res); cr.Outcome != "PASS" || cr.Problem == "" {
		t.Fatalf("a control whose gate caught nothing: outcome %q (%q), want PASS with a problem", cr.Outcome, cr.Problem)
	}
	got := rs.judgeChat(ChatCase{CaseID: "chat-corpus"}, "word_fallback", before)
	if len(got) != 2 || !strings.Contains(got[0], FailureRetrievalMode) {
		t.Fatalf("corpus reply: %q, want two %s failures", got, FailureRetrievalMode)
	}
}

// WIS-700: chat_synonyms recall is reported for comparison and never fails a run; the same recall
// on the release chat corpus does.
func TestWIS700SynonymRecallIsAComparison(t *testing.T) {
	syn := measure(SuiteChatSynonyms, "retrieval_recall_at_10", 33, 45, "1")
	syn.Comparison = true
	gated := measure(SuiteChat, "retrieval_recall_at_10", 33, 45, "1")
	if syn.Pass || gated.Pass {
		t.Fatalf("33/45 against 1 passed: %+v %+v", syn, gated)
	}
	if got := measureFailures([]Measure{syn, gated}); len(got) != 1 || !strings.HasPrefix(got[0], SuiteChat+" ") {
		t.Fatalf("failures %q, want only the chat suite's", got)
	}
}
