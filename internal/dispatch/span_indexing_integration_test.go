//go:build integration

package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/retrieval"
	"github.com/google/uuid"
	"go.temporal.io/sdk/client"
)

// TestWIS627SpanIndexingPostgres covers the CPU embedding endpoint defect (WIS-627):
//   - while an eligible span is pending and the endpoint blocks, dispatch.Tick still relays work
//     promptly and never calls the endpoint (indexing runs on its own worker loop);
//   - with the production constructor (New, pinned loopback endpoint), an endpoint slower than
//     the question-time budget but within the index budget still gets the span embedded and
//     stored by EmbedPending. It fails if indexing used the question client or a short header
//     timeout came back. Times are scaled down: 100ms question budget, 400ms answer, 30s index;
//   - an endpoint that sends headers and then stalls past the index budget is one timed-out
//     batch call (ErrTimeout), not a refusal: no span is resent alone or deferred.
func TestWIS627SpanIndexingPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	admin, db := tenantDiscoveryDatabase(t, ctx)
	defer db.Close()
	address := os.Getenv("SUPERVISOR_TEST_TEMPORAL_ADDRESS")
	if address == "" {
		t.Fatal("BLOCKED: actual Temporal service address required")
	}
	c, err := client.Dial(client.Options{HostPort: address, Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	tenant, site, subject := uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer := "https://span-indexing.invalid/realm"
	insertDiscoveryTenant(t, ctx, admin, tenant, site, issuer, subject)
	token := uuid.NewString()
	sum := sha256.Sum256([]byte(token))
	sessionHash := hex.EncodeToString(sum[:])
	if _, err = admin.ExecContext(ctx, `INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, sessionHash, subject, issuer, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	// One approved snapshot claim citing one span: the span is eligible for embedding.
	ksc, esc := knowledge.Scope{TenantID: tenant, SiteID: site}, evidence.Scope{TenantID: tenant, SiteID: site}
	ka, ea := knowledge.Actor{Issuer: issuer, Subject: subject, SessionHash: sessionHash}, evidence.Actor{Issuer: issuer, Subject: subject, SessionHash: sessionHash}
	ks, ev := knowledge.New(db.DB), evidence.New(db.DB)
	text := "The oven runs at 180 degrees during the bake step. Gloves are worn at the oven."
	src, err := ev.CreateOrRevise(ctx, ea, esc, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "SOP", Text: text}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	span, err := ev.CreateSpan(ctx, ea, esc, evidence.SpanInput{SourceID: src.SourceID, Revision: 1, Start: 0, End: 50}, uuid.NewString(), 1)
	if err != nil {
		t.Fatal(err)
	}
	span2, err := ev.CreateSpan(ctx, ea, esc, evidence.SpanInput{SourceID: src.SourceID, Revision: 1, Start: 51, End: len([]rune(text))}, uuid.NewString(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ev.DecideAuthority(ctx, ea, esc, src.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "verified"}, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	op, claim := uuid.NewString(), uuid.NewString()
	if _, err = ks.CreateEntity(ctx, ka, ksc, knowledge.EntityInput{EntityID: op, Type: "operation", Name: "Bake", SourceID: src.SourceID, SourceRevision: 1}, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	value, _ := json.Marshal("60")
	if _, err = ks.CreateClaim(ctx, ka, ksc, knowledge.ClaimInput{ClaimID: claim, SubjectID: op, SubjectRevision: 1, Predicate: "cycle_time_seconds", Literal: &knowledge.Literal{Type: "decimal", Value: value, Unit: "s"},
		EffectiveFrom: "2026-01-01T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span.ID, span2.ID}}, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err = ks.DecideReview(ctx, ka, ksc, claim, knowledge.ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "ok"}, uuid.NewString(), 1); err != nil {
		t.Fatal(err)
	}
	inv, err := ks.ReadInventory(ctx, ka, ksc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ks.CreateSnapshot(ctx, ka, ksc, knowledge.SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Reviewed"}, uuid.NewString(), inv.Revision); err != nil {
		t.Fatal(err)
	}

	// The stand-in endpoint counts hits; it blocks until released, sends headers and stalls, or
	// answers after 400ms.
	var hits atomic.Int32
	var blocking, stalling atomic.Bool
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var in struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if blocking.Load() {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		if stalling.Load() {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		time.Sleep(400 * time.Millisecond)
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		out := struct {
			Model string `json:"model"`
			Data  []item `json:"data"`
		}{Model: "qwen3-embedding-0.6b"}
		for i := range in.Input {
			v := make([]float32, retrieval.Dimensions)
			v[0] = 1
			out.Data = append(out.Data, item{i, v})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	emb, err := retrieval.New(config.Embeddings{Enabled: true, BaseURL: srv.URL, EndpointAddrs: []netip.AddrPort{netip.MustParseAddrPort(srv.Listener.Addr().String())},
		Model: "qwen3-embedding-0.6b", ModelRevision: "r1", Timeout: 100 * time.Millisecond, IndexTimeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	retrieval.SetDefault(emb)
	t.Cleanup(func() { retrieval.SetDefault(nil) })

	// The tick relays a probe without waiting on the blocked endpoint (before the fix it waited
	// up to the 30s index budget inside EmbedSpans).
	queue := "span-indexing-" + uuid.NewString()
	cfg := config.Config{TemporalQueue: queue}
	w, err := NewWorker(c, db, cfg)
	if err == nil {
		err = w.Start()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	if _, err = db.SubmitProbe(ctx, tenant, site, issuer, subject, uuid.NewString(), "relayed while the embedding endpoint blocks"); err != nil {
		t.Fatal(err)
	}
	blocking.Store(true)
	start := time.Now()
	if n, err := Tick(ctx, db, c, cfg, w.AgentEnabled); err != nil || n < 1 {
		t.Fatalf("tick with a pending span and a blocked endpoint: relayed=%d err=%v", n, err)
	}
	if took, h := time.Since(start), hits.Load(); h != 0 || took > 10*time.Second {
		t.Fatalf("tick called the embedding endpoint %d time(s) and took %v", h, took)
	}

	// Headers then a stall past a 300ms index budget: one batch call times out; before the
	// review fix the body-read failure read as a refusal, each span was resent alone and deferred.
	// Since WIS-724 a timed-out batch is followed by one tiny probe; the stalled endpoint does not
	// answer it either, so it reads as down: two requests, nothing resent alone, nothing deferred
	// (the embed below still stores both spans).
	blocking.Store(false)
	stalling.Store(true)
	hits.Store(0)
	short, err := retrieval.New(config.Embeddings{Enabled: true, BaseURL: srv.URL, EndpointAddrs: []netip.AddrPort{netip.MustParseAddrPort(srv.Listener.Addr().String())},
		Model: "qwen3-embedding-0.6b", ModelRevision: "r1", Timeout: 100 * time.Millisecond, IndexTimeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := retrieval.EmbedPending(ctx, db.DB, short, tenant, 8); n != 0 || !errors.Is(err, retrieval.ErrTimeout) || hits.Load() != 2 {
		t.Fatalf("stalled body: stored=%d err=%v endpoint hits=%d, want 0, ErrTimeout, 2 (batch and probe)", n, err, hits.Load())
	}
	stalling.Store(false)

	// Indexing with the production client: the 400ms answer exceeds the question budget...
	if _, err = emb.Embed(ctx, []string{"How hot is the oven?"}); !errors.Is(err, retrieval.ErrTimeout) {
		t.Fatalf("question-time embed against a 400ms endpoint: %v", err)
	}
	// ...but fits the index budget, so the span is embedded and stored.
	if n, err := retrieval.EmbedPending(ctx, db.DB, emb, tenant, 8); n != 2 || err != nil {
		t.Fatalf("EmbedPending: stored=%d err=%v", n, err)
	}
	var stored int
	if err = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.span_embeddings WHERE tenant_id=$1 AND span_id IN ($2,$3) AND model='qwen3-embedding-0.6b' AND model_revision='r1'`, tenant, span.ID, span2.ID).Scan(&stored); err != nil || stored != 2 {
		t.Fatalf("stored embeddings=%d err=%v", stored, err)
	}
}
