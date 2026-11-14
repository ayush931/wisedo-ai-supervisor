// Package retrieval is the optional semantic half of chat passage ranking (WIS-627): a client for
// a local OpenAI-compatible embedding and rerank endpoint, the job that embeds evidence spans for
// the configured model revision, and the similarity read the chat ranker fuses with word ranking.
//
// Embeddings carry no authority. Which passages may be offered is still decided from canonical
// state (approved snapshot claims, current source access) at question time; an embedding of a
// revoked source is never returned because the ranker only scores spans of approved claims and
// chat capture re-reads every source. The embedding job itself sends only spans an approved,
// unwithdrawn snapshot claim cites, from an unrevoked source (rule 06): nothing else reaches the model. Revisions are never mixed: only rows of the configured
// model and revision are read, and a span without one makes the question fall back to word ranking.
package retrieval

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
)

// Dimensions is the stored embedding size (migration 075: vector(1024), Qwen3-Embedding-0.6B).
const Dimensions = 1024

const maxResponseBytes = 8 << 20

var (
	ErrEndpoint  = errors.New("embedding endpoint refused or failed")
	ErrDimension = errors.New("embedding has the wrong dimension")
	errNotLocal  = errors.New("embedding endpoint not local or not pinned")
	// errUnreachable marks a request that never got an answer (dial, timeout, cancel): the
	// endpoint is down, so no single span is to blame.
	errUnreachable = errors.New("embedding endpoint unreachable")
	// ErrTimeout marks an unreachable request that ran out of its whole request budget (not a
	// dial failure, nor the caller's own deadline).
	ErrTimeout = errors.New("embedding endpoint timed out")
)

// Client calls the pinned local embedding endpoint. It never retries and never falls back to a
// hosted service.
type Client struct {
	base                   *url.URL
	model, revision        string
	rerank, rerankRevision string
	http                   *http.Client // question time: SUPERVISOR_EMBEDDINGS_TIMEOUT
	index                  *http.Client // span indexing: SUPERVISOR_EMBEDDINGS_INDEX_TIMEOUT

	mu       sync.Mutex
	deferred map[string]deferral // tenant/span -> next attempt, for spans the endpoint refuses
}

// deferral backs off a span the endpoint refused while its batch neighbours embedded.
type deferral struct {
	attempts int
	until    time.Time
}

var current atomic.Pointer[Client]

// SetDefault installs (nil removes) the process's client; the API and the worker install the same.
func SetDefault(c *Client) { current.Store(c) }

// Default is the installed client, or nil when semantic retrieval is off.
func Default() *Client { return current.Load() }

// New builds the client from validated configuration, with a dialer that reaches only the
// pinned local addresses (the local model rule: DNS cannot move it elsewhere).
func New(e config.Embeddings) (*Client, error) { return NewObserved(e, nil) }

// NewObserved is New with the pinned transport wrapped by wrap (nil: unwrapped). The wrapper
// sees every request after the endpoint and address checks; supervisor-eval uses it to count
// embed and rerank answers, because a failed rerank is silently skipped (WIS-647).
func NewObserved(e config.Embeddings, wrap func(http.RoundTripper) http.RoundTripper) (*Client, error) {
	base, err := e.Endpoint()
	if err != nil {
		return nil, err
	}
	lookup := func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		var addrs []netip.Addr
		if ip, e2 := netip.ParseAddr(host); e2 == nil {
			addrs = []netip.Addr{ip}
		} else if addrs, err = lookup(ctx, host); err != nil {
			return nil, err
		}
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || len(addrs) == 0 {
			return nil, errNotLocal
		}
		for _, a := range addrs {
			if !e.Pinned(a, uint16(n)) {
				return nil, errNotLocal
			}
		}
		d := net.Dialer{Timeout: 2 * time.Second}
		return d.DialContext(ctx, network, net.JoinHostPort(addrs[0].Unmap().String(), port))
	}
	var rt http.RoundTripper = &http.Transport{Proxy: nil, DialContext: dial, MaxIdleConns: 4, IdleConnTimeout: 90 * time.Second}
	if wrap != nil {
		rt = wrap(rt)
	}
	return NewWithTransport(e, base, rt), nil
}

// NewWithTransport takes the base URL and transport explicitly; tests pass a stand-in server.
// Each client's Timeout is its whole request budget (the transport sets no header timeout). A
// zero IndexTimeout (callers that predate it) indexes with Timeout.
func NewWithTransport(e config.Embeddings, base *url.URL, rt http.RoundTripper) *Client {
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	index := e.IndexTimeout
	if index == 0 {
		index = e.Timeout
	}
	return &Client{base: base, model: e.Model, revision: e.ModelRevision, rerank: e.RerankModel, rerankRevision: e.RerankModelRevision,
		http:  &http.Client{Timeout: e.Timeout, Transport: rt, CheckRedirect: noRedirect},
		index: &http.Client{Timeout: index, Transport: rt, CheckRedirect: noRedirect}}
}

// Timeout is the per-request budget of a question-time call (SUPERVISOR_EMBEDDINGS_TIMEOUT).
func (c *Client) Timeout() time.Duration { return c.http.Timeout }

// IndexTimeout is the per-request budget of span indexing.
func (c *Client) IndexTimeout() time.Duration { return c.index.Timeout }

func (c *Client) Model() string    { return c.model }
func (c *Client) Revision() string { return c.revision }

// Reranks reports whether a rerank model is configured.
func (c *Client) Reranks() bool { return c.rerank != "" }

func (c *Client) post(ctx context.Context, hc *http.Client, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base.JoinPath(path).String(), bytes.NewReader(body))
	if err != nil {
		return ErrEndpoint
	}
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	// unreachable classifies a request or body read that got no full answer: never a refusal.
	unreachable := func(err error) error {
		if hc.Timeout > 0 && time.Since(start) >= hc.Timeout && ctx.Err() == nil {
			return fmt.Errorf("%w: %w: %w: %w", ErrEndpoint, errUnreachable, ErrTimeout, err)
		}
		return fmt.Errorf("%w: %w: %w", ErrEndpoint, errUnreachable, err)
	}
	res, err := hc.Do(req)
	if err != nil {
		return unreachable(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil {
		return unreachable(err)
	}
	if res.StatusCode != http.StatusOK || len(data) > maxResponseBytes {
		return ErrEndpoint
	}
	if json.Unmarshal(data, out) != nil {
		return ErrEndpoint
	}
	return nil
}

// Embed returns one unit-length vector per text, in order. A wrong count or dimension refuses all.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return c.embed(ctx, c.http, texts)
}

func (c *Client) embed(ctx context.Context, hc *http.Client, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	var out struct {
		Model string `json:"model"`
		Data  []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := c.post(ctx, hc, "/v1/embeddings", map[string]any{"model": c.model, "input": texts}, &out); err != nil {
		return nil, err
	}
	if out.Model != c.model || len(out.Data) != len(texts) {
		return nil, ErrEndpoint
	}
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(texts) || vecs[d.Index] != nil || len(d.Embedding) != Dimensions {
			return nil, ErrDimension
		}
		vecs[d.Index] = normalize(d.Embedding)
		if vecs[d.Index] == nil {
			return nil, ErrDimension
		}
	}
	return vecs, nil
}

func normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return nil
		}
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return nil
	}
	n := math.Sqrt(sum)
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / n)
	}
	return out
}

// Rerank scores docs against the query with the rerank model; nil, nil when none is configured.
func (c *Client) Rerank(ctx context.Context, query string, docs []string) ([]float64, error) {
	if c.rerank == "" || len(docs) == 0 {
		return nil, nil
	}
	var out struct {
		Results []struct {
			Index int     `json:"index"`
			Score float64 `json:"relevance_score"`
		} `json:"results"`
	}
	if err := c.post(ctx, c.http, "/v1/rerank", map[string]any{"model": c.rerank, "query": query, "documents": docs}, &out); err != nil {
		return nil, err
	}
	scores, seen := make([]float64, len(docs)), make([]bool, len(docs))
	for _, r := range out.Results {
		if r.Index < 0 || r.Index >= len(docs) || seen[r.Index] || math.IsNaN(r.Score) || math.IsInf(r.Score, 0) {
			return nil, ErrEndpoint
		}
		scores[r.Index], seen[r.Index] = r.Score, true
	}
	for _, s := range seen {
		if !s {
			return nil, ErrEndpoint
		}
	}
	return scores, nil
}

// vectorLiteral writes a pgvector text literal.
func vectorLiteral(v []float32) string {
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = strconv.FormatFloat(float64(x), 'g', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// pendingSQL selects the tenant's spans that still need an embedding of the client's model
// revision and that chat could ever offer (rule 06, WIS-627 review): cited by a claim of a process
// snapshot whose approval still stands (knowledge.ApprovalStandsSQL: not withdrawn, designation
// not fallen, resolution not withdrawn or superseded), from a source that is not revoked.
// Unapproved, fallen and revoked text is never read for the endpoint. Deferred spans are skipped ($5); a
// non-empty $6 restricts the check to those spans.
var pendingSQL = `SELECT s.site_id::text,s.id::text,substr(r.body,s.start_rune+1,s.end_rune-s.start_rune)
 FROM supervisor.evidence_spans s
 JOIN supervisor.evidence_revisions r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=(s.tenant_id,s.site_id,s.source_id,s.revision)
 JOIN supervisor.evidence_heads h ON (h.tenant_id,h.site_id,h.source_id)=(s.tenant_id,s.site_id,s.source_id)
 WHERE s.tenant_id=$1 AND NOT h.revoked AND s.id<>ALL($5::uuid[]) AND (cardinality($6::uuid[])=0 OR s.id=ANY($6::uuid[]))
 AND NOT EXISTS(SELECT 1 FROM supervisor.evidence_revocations v WHERE (v.tenant_id,v.site_id,v.source_id)=(s.tenant_id,s.site_id,s.source_id))
 AND EXISTS(SELECT 1 FROM supervisor.knowledge_snapshot_claims k
  JOIN supervisor.knowledge_claim_spans cs ON (cs.tenant_id,cs.site_id,cs.claim_id,cs.claim_revision)=(k.tenant_id,k.site_id,k.claim_id,k.claim_revision)
  JOIN supervisor.knowledge_claim_reviews rv ON (rv.tenant_id,rv.site_id,rv.review_id)=(k.tenant_id,k.site_id,k.review_id)
  WHERE (cs.tenant_id,cs.site_id,cs.span_id)=(s.tenant_id,s.site_id,s.id) AND ` + knowledge.ApprovalStandsSQL("rv") + `)
 AND NOT EXISTS(SELECT 1 FROM supervisor.span_embeddings em WHERE (em.tenant_id,em.site_id,em.span_id)=(s.tenant_id,s.site_id,s.id) AND em.model=$2 AND em.model_revision=$3)
 ORDER BY s.created_at,s.id LIMIT $4`

type pending struct{ site, span, text string }

// pending reads up to limit eligible spans (pendingSQL), only those in only when it is non-empty.
func (c *Client) pending(ctx context.Context, db *sql.DB, tenantID string, limit int, only []string) ([]pending, error) {
	var todo []pending
	err := inTenant(ctx, db, tenantID, true, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, pendingSQL, tenantID, c.model, c.revision, limit, c.deferredSpans(tenantID), only)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p pending
			if err = rows.Scan(&p.site, &p.span, &p.text); err != nil {
				return err
			}
			todo = append(todo, p)
		}
		return rows.Err()
	})
	return todo, err
}

// EmbedPending embeds up to limit of the tenant's eligible spans (pendingSQL) that have no
// embedding for the client's model revision, oldest first, and stores them. It reads the span
// texts, calls the endpoint outside any transaction, then inserts; a concurrent writer's row is
// kept (no overwrite). Only an unreachable endpoint (no answer) fails the tick as a whole. When
// the endpoint refuses a batch, each span is retried alone, its eligibility re-read just before
// its own call (a source may be revoked or an approval withdrawn while earlier calls are
// pending; a span no longer eligible is skipped, neither sent nor stored): the ones that embed are stored
// and the ones refused are deferred with backoff, even when none succeeds, so refused spans never
// hold back the rest. A lone span that is refused is deferred without a resend. A source revoked
// between a read and the following call can still be sent once (no lock is held across
// inference, by design).
//
// A batch that times out is not always a down endpoint (WIS-724): a batch of long spans can be too
// slow for the index budget while the endpoint is fine, and since the oldest spans are picked
// first the same batch would time out on every pass and hold back every newer span of the tenant,
// at every site. So a timed-out batch is followed by one tiny probe within the question budget. If
// the probe fails too, the endpoint is down and nothing is deferred (as before: a slow or stalled
// endpoint must not push every span into backoff). If it answers, the spans are sent one by one;
// a single span that still times out is deferred with backoff and ends the pass, which bounds a
// pass to one extra timeout.
func EmbedPending(ctx context.Context, db *sql.DB, c *Client, tenantID string, limit int) (int, error) {
	if c == nil || limit < 1 {
		return 0, nil
	}
	todo, err := c.pending(ctx, db, tenantID, limit, []string{})
	if err != nil || len(todo) == 0 {
		return 0, err
	}
	texts := make([]string, len(todo))
	for i, p := range todo {
		texts[i] = p.text
	}
	vecs, err := c.embed(ctx, c.index, texts)
	var refused error
	if err != nil {
		if errors.Is(err, errUnreachable) && (!errors.Is(err, ErrTimeout) || !c.answers(ctx)) {
			return 0, err
		}
		retry := len(todo) > 1
		vecs = make([][]float32, len(todo))
		bad := 0
		for i, p := range todo {
			if retry {
				// Recheck right before each send: a span that lost eligibility while an earlier
				// retry was pending is neither sent nor stored. It is not deferred: pendingSQL
				// already excludes it, and it embeds normally if it becomes eligible again.
				now, e := c.pending(ctx, db, tenantID, 1, []string{p.span})
				if e != nil {
					return 0, e
				}
				if len(now) == 0 {
					continue
				}
				v, e := c.embed(ctx, c.index, []string{now[0].text})
				if errors.Is(e, ErrTimeout) {
					// The endpoint answers, but this span alone is too slow for the budget.
					c.deferSpan(tenantID, p.span)
					bad++
					todo, vecs, refused = todo[:i+1], vecs[:i+1], e
					break
				}
				if errors.Is(e, errUnreachable) {
					todo, vecs, refused = todo[:i], vecs[:i], e
					break
				}
				if e == nil {
					vecs[i] = v[0]
					continue
				}
			}
			c.deferSpan(tenantID, p.span)
			bad++
		}
		if bad > 0 {
			refused = errors.Join(fmt.Errorf("%w: %d span(s) refused and deferred", ErrEndpoint, bad), refused)
		}
	}
	n := 0
	err = inTenant(ctx, db, tenantID, false, func(tx *sql.Tx) error {
		for i, p := range todo {
			if vecs[i] == nil {
				continue
			}
			c.forget(tenantID, p.span) // embedded: a past refusal no longer backs it off
			res, err := tx.ExecContext(ctx, `INSERT INTO supervisor.span_embeddings(tenant_id,site_id,span_id,model,model_revision,embedding) VALUES($1,$2,$3,$4,$5,$6::public.vector) ON CONFLICT DO NOTHING`,
				tenantID, p.site, p.span, c.model, c.revision, vectorLiteral(vecs[i]))
			if err != nil {
				return err
			}
			if k, _ := res.RowsAffected(); k == 1 {
				n++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, refused
}

// answers reports whether the endpoint embeds a tiny text within the question budget: a batch that
// timed out against an endpoint that answers was too slow, not sent to a down endpoint.
func (c *Client) answers(ctx context.Context) bool {
	_, err := c.embed(ctx, c.http, []string{"ok"})
	return err == nil
}

// deferSpan backs a refused span off for 2, 4, 8 ... minutes, at most an hour, so a span the
// endpoint always refuses costs at most one call an hour.
// ponytail: per-process memory; a restart retries every deferred span once. Persist it if the
// worker ever runs as several replicas or restarts often.
func (c *Client) deferSpan(tenantID, spanID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deferred == nil {
		c.deferred = map[string]deferral{}
	}
	d := c.deferred[tenantID+"/"+spanID]
	d.attempts++
	d.until = time.Now().Add(min(time.Minute<<min(d.attempts, 6), time.Hour))
	c.deferred[tenantID+"/"+spanID] = d
}

// forget drops a span's backoff once it has embedded.
func (c *Client) forget(tenantID, spanID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.deferred, tenantID+"/"+spanID)
}

// deferralMemory is how long a span's refusal count is kept after its backoff ends, so a span
// refused again soon backs off longer, while the map stays bounded by the spans refused lately.
const deferralMemory = 24 * time.Hour

// deferredSpans lists the tenant's spans still backing off (never nil: SQL ALL over NULL is NULL)
// and drops entries idle for longer than deferralMemory.
func (c *Client) deferredSpans(tenantID string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out, now := []string{}, time.Now()
	for k, d := range c.deferred {
		if now.Sub(d.until) > deferralMemory {
			delete(c.deferred, k)
			continue
		}
		if span, ok := strings.CutPrefix(k, tenantID+"/"); ok && now.Before(d.until) {
			out = append(out, span)
		}
	}
	return out
}

func inTenant(ctx context.Context, db *sql.DB, tenantID string, readOnly bool, f func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: readOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','20s',true)`, tenantID); err != nil {
		return err
	}
	if err = f(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// SimilarityTx returns, inside the caller's tenant transaction, the cosine similarity of each
// given span to the query vector for the client's model revision. complete is false when any span
// has no embedding of that revision: the caller then ranks by words only.
func SimilarityTx(ctx context.Context, tx *sql.Tx, c *Client, tenantID, siteID string, spanIDs []string, query []float32) (map[string]float64, bool, error) {
	out := map[string]float64{}
	if len(spanIDs) == 0 {
		return out, true, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT span_id::text,1-(embedding <=> $4::public.vector) FROM supervisor.span_embeddings
 WHERE tenant_id=$1 AND site_id=$2 AND span_id=ANY($3::uuid[]) AND model=$5 AND model_revision=$6`, tenantID, siteID, spanIDs, vectorLiteral(query), c.model, c.revision)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var sim float64
		if err = rows.Scan(&id, &sim); err != nil {
			return nil, false, err
		}
		out[id] = sim
	}
	if err = rows.Err(); err != nil {
		return nil, false, err
	}
	for _, id := range spanIDs {
		if _, ok := out[id]; !ok {
			return out, false, nil
		}
	}
	return out, true, nil
}
