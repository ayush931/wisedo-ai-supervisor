//go:build integration

package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/retrieval"
	"github.com/google/uuid"
)

// WIS-724: span indexing picks the tenant's oldest eligible spans, across all its sites, and sends
// them as one batch. A batch too slow for the index budget used to read as "endpoint down": it was
// neither split nor deferred, so it timed out on every pass and a newer span of another site was
// never embedded. The endpoint here stalls on what the mode says; the index budget is 300ms.
func TestSpanIndexingTimedOutBatchPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	admin, db := tenantDiscoveryDatabase(t, ctx)
	defer db.Close()
	tenant, siteX, siteY, subject := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer := "https://span-timeout.invalid/realm"
	insertDiscoveryTenant(t, ctx, admin, tenant, siteX, issuer, subject)
	for _, q := range []string{`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'site y')`, `INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`} {
		args := []any{tenant, siteY}
		if strings.Contains(q, "memberships") {
			args = append(args, issuer, subject)
		}
		if _, err := admin.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	token := uuid.NewString()
	sum := sha256.Sum256([]byte(token))
	sessionHash := hex.EncodeToString(sum[:])
	if _, err := admin.ExecContext(ctx, `INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, sessionHash, subject, issuer, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	ks, ev := knowledge.New(db.DB), evidence.New(db.DB)
	ka, ea := knowledge.Actor{Issuer: issuer, Subject: subject, SessionHash: sessionHash}, evidence.Actor{Issuer: issuer, Subject: subject, SessionHash: sessionHash}
	// approve stores one source with one span per text and approves a snapshot claim citing them,
	// which makes the spans eligible for indexing.
	approve := func(site string, texts []string) []string {
		t.Helper()
		ksc, esc := knowledge.Scope{TenantID: tenant, SiteID: site}, evidence.Scope{TenantID: tenant, SiteID: site}
		src, err := ev.CreateOrRevise(ctx, ea, esc, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "SOP", Text: strings.Join(texts, " ")}, uuid.NewString(), 0)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		pos := 0
		for _, text := range texts {
			l := len([]rune(text))
			sp, err := ev.CreateSpan(ctx, ea, esc, evidence.SpanInput{SourceID: src.SourceID, Revision: 1, Start: pos, End: pos + l}, uuid.NewString(), 1)
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, sp.ID)
			pos += l + 1
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
			EffectiveFrom: "2026-01-01T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: ids}, uuid.NewString(), 0); err != nil {
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
		return ids
	}
	embedded := func(ids ...string) int {
		t.Helper()
		var n int
		if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.span_embeddings WHERE tenant_id=$1 AND span_id=ANY($2::uuid[])`, tenant, ids).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// mode: "batch" stalls every multi-input request (a batch too slow for the budget),
	// "poison" also stalls any request containing POISON, "down" stalls everything.
	var mode atomic.Value
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var in struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		m := mode.Load().(string)
		stall := m == "down" || len(in.Input) > 1 && m != "fast"
		for _, s := range in.Input {
			stall = stall || m == "poison" && strings.Contains(s, "POISON")
		}
		if stall {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
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
	client := func() *retrieval.Client {
		c, err := retrieval.New(config.Embeddings{Enabled: true, BaseURL: srv.URL, EndpointAddrs: []netip.AddrPort{netip.MustParseAddrPort(srv.Listener.Addr().String())},
			Model: "qwen3-embedding-0.6b", ModelRevision: "r1", Timeout: 100 * time.Millisecond, IndexTimeout: 300 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	texts := func(word string, n int) []string {
		var out []string
		for i := range n {
			out = append(out, word+strings.Repeat("x", 40+i))
		}
		return out
	}

	t.Run("an endpoint that is down defers nothing", func(t *testing.T) {
		ids := approve(siteX, texts("DOWN", 2))
		c := client()
		mode.Store("down")
		hits.Store(0)
		if n, err := retrieval.EmbedPending(ctx, db.DB, c, tenant, 8); n != 0 || err == nil {
			t.Fatalf("down endpoint: stored=%d err=%v", n, err)
		}
		if h := hits.Load(); h != 2 {
			t.Fatalf("down endpoint: %d requests, want the batch and one probe", h)
		}
		mode.Store("fast")
		if n, err := retrieval.EmbedPending(ctx, db.DB, c, tenant, 8); n != 2 || err != nil || embedded(ids...) != 2 {
			t.Fatalf("after recovery: stored=%d err=%v; a stalled endpoint must not defer the spans", n, err)
		}
	})

	t.Run("a batch too slow for the budget no longer holds back newer spans", func(t *testing.T) {
		approve(siteX, texts("SITEX", 8))
		time.Sleep(20 * time.Millisecond)
		y := approve(siteY, texts("SITEY", 1))
		c := client()
		mode.Store("batch")
		for pass := 1; pass <= 2 && embedded(y...) == 0; pass++ {
			if _, err := retrieval.EmbedPending(ctx, db.DB, c, tenant, 8); err != nil && pass > 1 {
				t.Logf("pass %d: %v", pass, err)
			}
		}
		if embedded(y...) != 1 {
			t.Fatal("the newer site-Y span was not embedded within two passes: the slow batch still holds it back")
		}
	})

	t.Run("a span too slow on its own is deferred and the rest proceed", func(t *testing.T) {
		poison := approve(siteX, []string{"POISON" + strings.Repeat("p", 40)})
		time.Sleep(20 * time.Millisecond)
		after := approve(siteY, texts("AFTER", 2))
		c := client()
		mode.Store("poison")
		for range 3 {
			_, _ = retrieval.EmbedPending(ctx, db.DB, c, tenant, 8)
		}
		if embedded(poison...) != 0 || embedded(after...) != 2 {
			t.Fatalf("poison embedded=%d after embedded=%d, want 0 and 2", embedded(poison...), embedded(after...))
		}
		hits.Store(0)
		if _, err := retrieval.EmbedPending(ctx, db.DB, c, tenant, 8); err != nil || hits.Load() != 0 {
			t.Fatalf("the deferred span was sent again at once: %d requests, err=%v", hits.Load(), err)
		}
	})
}
