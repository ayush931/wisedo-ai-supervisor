package main

// WIS-647: chat passage retrieval as an explicit, recorded choice. Word mode (the default) never
// enables semantic retrieval. The hybrid modes enable the pinned local embedding endpoint (and
// the rerank model, for hybrid_rerank) in the in-process API exactly as cmd/supervisor does, embed
// every span of a chat world before its first question, and then hold each reply to its mode: a
// word_fallback reply, or a rerank that did not answer, fails the run instead of being counted as
// a hybrid result (rule 09: never a green fallback).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/chat"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/retrieval"
)

const (
	RetrievalWord         = "word"
	RetrievalHybrid       = "hybrid"
	RetrievalHybridRerank = "hybrid_rerank"

	// FailureRetrievalMode: a reply was not chosen the way the run's retrieval mode requires.
	FailureRetrievalMode = "RETRIEVAL_MODE_MISMATCH"
)

var retrievalModes = []string{RetrievalWord, RetrievalHybrid, RetrievalHybridRerank}

// callLog counts the endpoint's answers by path: ok is a 200, failed is anything else or no answer.
type callLog struct {
	mu                                           sync.Mutex
	embedOK, embedFailed, rerankOK, rerankFailed int
}

func (l *callLog) snapshot() (embedOK, embedFailed, rerankOK, rerankFailed int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.embedOK, l.embedFailed, l.rerankOK, l.rerankFailed
}

type countingTransport struct {
	next http.RoundTripper
	log  *callLog
}

func (t countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := t.next.RoundTrip(req)
	ok := err == nil && res.StatusCode == http.StatusOK
	t.log.mu.Lock()
	defer t.log.mu.Unlock()
	switch {
	case strings.HasSuffix(req.URL.Path, "/v1/rerank") && ok:
		t.log.rerankOK++
	case strings.HasSuffix(req.URL.Path, "/v1/rerank"):
		t.log.rerankFailed++
	case ok:
		t.log.embedOK++
	default:
		t.log.embedFailed++
	}
	return res, err
}

// retrievalSetup is the run's retrieval: nil client in word mode.
type retrievalSetup struct {
	mode   string
	client *retrieval.Client
	calls  *callLog
}

// RetrievalReport records how chat passages were chosen and what that cost.
type RetrievalReport struct {
	Mode                string                    `json:"mode"`
	Model               string                    `json:"model,omitempty"`
	ModelRevision       string                    `json:"model_revision,omitempty"`
	RerankModel         string                    `json:"rerank_model,omitempty"`
	RerankModelRevision string                    `json:"rerank_model_revision,omitempty"`
	Labels              map[string]map[string]int `json:"reply_labels"`   // suite -> reply retrieval label -> count
	AskLatencyMs        map[string]Dist           `json:"ask_latency_ms"` // suite -> question post latency (includes question-time embed and rerank)
	EmbedCalls          [2]int                    `json:"embed_calls_ok_failed"`
	RerankCalls         [2]int                    `json:"rerank_calls_ok_failed"`
}

// newRetrievalSetup checks the mode and, for the hybrid modes, builds and probes the client. Every
// error is a missing prerequisite: the caller reports BLOCKED.
func newRetrievalSetup(ctx context.Context, mode string, load func() (config.Embeddings, error)) (*retrievalSetup, error) {
	switch mode {
	case RetrievalWord:
		return &retrievalSetup{mode: mode}, nil
	case RetrievalHybrid, RetrievalHybridRerank:
	default:
		return nil, fmt.Errorf("unknown -retrieval %q (known: %s)", mode, strings.Join(retrievalModes, ","))
	}
	e, err := load()
	if err != nil {
		return nil, fmt.Errorf("retrieval %s: %w", mode, err)
	}
	switch {
	case !e.Enabled:
		return nil, fmt.Errorf("retrieval %s: set SUPERVISOR_EMBEDDINGS_ENABLED=true and the pinned SUPERVISOR_EMBEDDINGS_* settings", mode)
	case mode == RetrievalHybridRerank && e.RerankModel == "":
		return nil, errors.New("retrieval hybrid_rerank: SUPERVISOR_EMBEDDINGS_RERANK_MODEL and _RERANK_MODEL_REVISION are required")
	case mode == RetrievalHybrid && e.RerankModel != "":
		return nil, errors.New("retrieval hybrid: unset SUPERVISOR_EMBEDDINGS_RERANK_MODEL (that is hybrid_rerank)")
	}
	calls := &callLog{}
	c, err := retrieval.NewObserved(e, func(next http.RoundTripper) http.RoundTripper { return countingTransport{next: next, log: calls} })
	if err != nil {
		return nil, fmt.Errorf("retrieval %s: %w", mode, err)
	}
	pctx, cancel := context.WithTimeout(ctx, e.IndexTimeout)
	defer cancel()
	if _, err = c.Embed(pctx, []string{"probe"}); err != nil {
		return nil, fmt.Errorf("retrieval %s: embedding endpoint probe: %w", mode, err)
	}
	if mode == RetrievalHybridRerank {
		if _, err = c.Rerank(pctx, "probe", []string{"probe"}); err != nil {
			return nil, fmt.Errorf("retrieval hybrid_rerank: rerank endpoint probe: %w", err)
		}
	}
	return &retrievalSetup{mode: mode, client: c, calls: calls}, nil
}

// maxIndexPasses bounds indexing a world: each pass embeds up to 64 spans.
const maxIndexPasses = 64

// index embeds every span of the tenant before its first question, as the worker's indexing
// loop would. A span the endpoint could not embed stays unembedded, and the question that needs
// it is answered with word_fallback, which fails its run.
func (rs *retrievalSetup) index(ctx context.Context, db *sql.DB, tenant string) error {
	if rs == nil || rs.client == nil {
		return nil
	}
	for range maxIndexPasses {
		n, err := retrieval.EmbedPending(ctx, db, rs.client, tenant, 64)
		if err != nil {
			return fmt.Errorf("index spans: %w", err)
		}
		if n == 0 {
			return nil
		}
	}
	return errors.New("index spans: still pending after the bound")
}

// wantLabel is the retrieval label every reply of the mode must carry.
func (rs *retrievalSetup) wantLabel() string {
	if rs == nil || rs.client == nil {
		return chat.RetrievalWord
	}
	return chat.RetrievalHybrid
}

// judge checks one reply against the mode: its label, and in hybrid_rerank that a rerank answered
// for this question and none failed. A skipped rerank is labelled hybrid_unreranked, which already
// fails the label check; the call log checks it independently of the product's own labelling.
// before is the call log taken just before the question was posted.
func (rs *retrievalSetup) judge(label string, before [4]int) []string {
	var out []string
	if label != rs.wantLabel() {
		out = append(out, fmt.Sprintf("%s: reply retrieval = %q, want %q", FailureRetrievalMode, label, rs.wantLabel()))
	}
	if rs == nil || rs.calls == nil {
		return out
	}
	eOK, eFail, rOK, rFail := rs.calls.snapshot()
	switch {
	case eFail > before[1]:
		out = append(out, fmt.Sprintf("%s: %d embedding call(s) failed for this question", FailureRetrievalMode, eFail-before[1]))
	case eOK == before[0]:
		out = append(out, FailureRetrievalMode+": no embedding call answered for this question")
	}
	if rs.mode == RetrievalHybridRerank {
		if rFail > before[3] {
			out = append(out, fmt.Sprintf("%s: %d rerank call(s) failed for this question", FailureRetrievalMode, rFail-before[3]))
		} else if rOK == before[2] {
			out = append(out, FailureRetrievalMode+": no rerank call answered for this question")
		}
	}
	return out
}

// judgeChat judges a corpus reply against the mode. A gate control is not judged (WIS-700): it
// must fail with its own expected code, and a retrieval mismatch (an assertion failure) would
// otherwise let a control expecting ASSERTION_MISMATCH fail for the wrong reason, or add a second
// code to one expecting another. Controls still run on the indexed world, with the same retrieval.
func (rs *retrievalSetup) judgeChat(c ChatCase, label string, before [4]int) []string {
	if c.ExpectedFailure != "" {
		return nil
	}
	return rs.judge(label, before)
}

func (rs *retrievalSetup) mark() [4]int {
	if rs == nil || rs.calls == nil {
		return [4]int{}
	}
	a, b, c, d := rs.calls.snapshot()
	return [4]int{a, b, c, d}
}
