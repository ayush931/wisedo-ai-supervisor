//go:build integration

package httpapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/chat"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/retrieval"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
)

// conceptEmbedding is the stand-in embedding model: words of one concept share a dimension, so
// "hot", "furnace" and "degrees" are close while sharing no word stem. It is a deterministic
// contract check of the fusion plumbing, not evidence of an embedding model's quality (rule 09).
func conceptEmbedding(text string) []float32 {
	heat := map[string]bool{"hot": true, "heat": true, "furnace": true, "oven": true, "degrees": true, "temperature": true}
	v := make([]float32, retrieval.Dimensions)
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !(r >= 'a' && r <= 'z') }) {
		if heat[w] {
			v[0] += 5
			continue
		}
		h := fnv.New32a()
		_, _ = h.Write([]byte(w))
		v[1+int(h.Sum32()%uint32(retrieval.Dimensions-1))]++
	}
	return v
}

// standIn is the stand-in embedding and rerank endpoint. It records every text sent for
// embedding, refuses (wrong dimension) any text containing POISON, and calls hook, when set,
// before it answers.
type standIn struct {
	*httptest.Server
	mu   sync.Mutex
	sent []string
	hook func(path string, input []string)
}

func (s *standIn) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.sent
	s.sent = nil
	return out
}

func (s *standIn) setHook(f func(path string, input []string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hook = f
}

func embeddingStandIn(t *testing.T, model string) *standIn {
	s := &standIn{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Model     string   `json:"model"`
			Input     []string `json:"input"`
			Documents []string `json:"documents"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil || (r.URL.Path == "/v1/embeddings" && in.Model != model) || (r.URL.Path != "/v1/embeddings" && r.URL.Path != "/v1/rerank") {
			http.Error(w, "bad", 400)
			return
		}
		s.mu.Lock()
		s.sent = append(s.sent, in.Input...)
		hook := s.hook
		s.mu.Unlock()
		if hook != nil {
			hook(r.URL.Path, in.Input)
		}
		if r.URL.Path == "/v1/rerank" {
			type result struct {
				Index int     `json:"index"`
				Score float64 `json:"relevance_score"`
			}
			out := struct {
				Results []result `json:"results"`
			}{}
			for i := range in.Documents {
				out.Results = append(out.Results, result{i, 0})
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		out := struct {
			Model string `json:"model"`
			Data  []item `json:"data"`
		}{Model: model}
		for i, text := range in.Input {
			v := conceptEmbedding(text)
			if strings.Contains(text, "POISON") {
				v = v[:3]
			}
			out.Data = append(out.Data, item{i, v})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(s.Close)
	return s
}

// TestWIS627HybridRetrievalPostgres: a synonym question ("how hot … furnace") shares no word with
// the passage that answers it ("oven … 180 degrees"). Ten long approved claims exceed the 12 KiB
// passage budget, so ranking decides which are captured:
//   - before the spans are embedded the question falls back to word ranking (labelled
//     word_fallback) and the answer passage is not captured;
//   - once embedded, hybrid ranking captures it (labelled hybrid);
//   - a revoked source is never captured, whatever its embedding;
//   - another embedding revision never reads this one's rows (word_fallback again);
//   - with semantic retrieval off the reply says word;
//   - the embedding job sends only approved, unwithdrawn, unrevoked spans, and a span the
//     endpoint refuses is deferred instead of stalling the backlog (WIS-627 review);
//   - the question's embedding and rerank calls hold no site lock: site writes and a
//     revocation go through while they are pending, and the revoked source is not captured.
func TestWIS627HybridRetrievalPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL admin and runtime URLs required")
	}
	control, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	name := "retrieval_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = control.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = control.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	in := func(raw string) string { u, _ := url.Parse(raw); u.Path = "/" + name; return u.String() }
	admin, err := sql.Open("pgx", in(adminURL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err = migrate.Apply(ctx, admin); err != nil {
		t.Fatal(err)
	}
	db, err := store.New(in(runtimeURL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tenant, site, site2, site3, owner, lead, operator := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, origin := "https://retrieval.invalid/"+uuid.NewString(), "http://127.0.0.1:58176"
	for _, q := range []string{
		`INSERT INTO supervisor.tenants(id,name) VALUES('` + tenant + `','retrieval')`,
		`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES('` + tenant + `','` + site + `','plant'),('` + tenant + `','` + site2 + `','second plant'),('` + tenant + `','` + site3 + `','third plant')`,
		`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES('` + tenant + `','` + site + `','` + issuer + `','` + owner + `','owner'),('` + tenant + `','` + site + `','` + issuer + `','` + lead + `','supervisor'),('` + tenant + `','` + site2 + `','` + issuer + `','` + owner + `','owner'),('` + tenant + `','` + site3 + `','` + issuer + `','` + owner + `','owner'),('` + tenant + `','` + site + `','` + issuer + `','` + operator + `','operator')`,
	} {
		if _, err = admin.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	type session struct{ token, csrf, hash string }
	seed := func(subject string) session {
		s := session{token: uuid.NewString(), csrf: uuid.NewString()}
		sum := sha256.Sum256([]byte(s.token))
		s.hash = hex.EncodeToString(sum[:])
		if _, e := admin.ExecContext(ctx, `INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, s.hash, subject, issuer, s.csrf); e != nil {
			t.Fatal(e)
		}
		return s
	}
	ownerS, leadS, operatorS := seed(owner), seed(lead), seed(operator)
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "retrieval-test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, db.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir(), Agent: config.Agent{Provider: config.AgentProviderFixture, Model: "fixture-model", ModelRevision: "sha256:fixture", ModeEnabled: true, ChatAnswering: true}}, db, auth)

	sc, es := knowledge.Scope{TenantID: tenant, SiteID: site}, evidence.Scope{TenantID: tenant, SiteID: site}
	ka, ea := knowledge.Actor{Issuer: issuer, Subject: owner, SessionHash: ownerS.hash}, evidence.Actor{Issuer: issuer, Subject: owner, SessionHash: ownerS.hash}
	ks, ev := knowledge.New(db.DB), evidence.New(db.DB)
	// seedPassage stores text as one source and span in siteID; with a claim ID it adds a claim
	// citing the span, approved when approve is set. It returns the source, span and review IDs.
	seedPassage := func(siteID, text, claimID string, approve bool) (string, string, string) {
		t.Helper()
		ksc, esc := knowledge.Scope{TenantID: tenant, SiteID: siteID}, evidence.Scope{TenantID: tenant, SiteID: siteID}
		src, e := ev.CreateOrRevise(ctx, ea, esc, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "SOP", Text: text}, uuid.NewString(), 0)
		if e != nil {
			t.Fatal(e)
		}
		span, e := ev.CreateSpan(ctx, ea, esc, evidence.SpanInput{SourceID: src.SourceID, Revision: 1, Start: 0, End: len([]rune(text))}, uuid.NewString(), 1)
		if e != nil {
			t.Fatal(e)
		}
		if claimID == "" {
			return src.SourceID, span.ID, ""
		}
		if _, e = ev.DecideAuthority(ctx, ea, esc, src.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "verified"}, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		op := uuid.NewString()
		if _, e = ks.CreateEntity(ctx, ka, ksc, knowledge.EntityInput{EntityID: op, Type: "operation", Name: "Step", SourceID: src.SourceID, SourceRevision: 1}, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		value, _ := json.Marshal("60")
		if _, e = ks.CreateClaim(ctx, ka, ksc, knowledge.ClaimInput{ClaimID: claimID, SubjectID: op, SubjectRevision: 1, Predicate: "cycle_time_seconds", Literal: &knowledge.Literal{Type: "decimal", Value: value, Unit: "s"},
			EffectiveFrom: "2026-01-01T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		if !approve {
			return src.SourceID, span.ID, ""
		}
		review, e := ks.DecideReview(ctx, ka, ksc, claimID, knowledge.ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "ok"}, uuid.NewString(), 1)
		if e != nil {
			t.Fatal(e)
		}
		return src.SourceID, span.ID, review.ReviewID
	}
	snapshot := func(ksc knowledge.Scope) {
		t.Helper()
		inv, e := ks.ReadInventory(ctx, ka, ksc)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = ks.CreateSnapshot(ctx, ka, ksc, knowledge.SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Reviewed"}, uuid.NewString(), inv.Revision); e != nil {
			t.Fatal(e)
		}
	}
	revokeSource := func(esc evidence.Scope, sourceID string, wctx context.Context) error {
		var state int64
		if e := admin.QueryRowContext(ctx, `SELECT state_revision FROM supervisor.evidence_heads WHERE source_id=$1`, sourceID).Scan(&state); e != nil {
			return e
		}
		_, e := ev.Revoke(wctx, ea, esc, sourceID, "Withdrawn", uuid.NewString(), state)
		return e
	}
	filler := strings.Repeat(" Keep the work area clear and log each check in the shift book.", 45) // ~2.9 KB per passage
	// Ten approved claims, each citing one long passage; the answer's claim sorts last by ID, so
	// word ranking (every claim scores 0 here) leaves it out of the 12 KiB budget.
	var targetSpan, targetSource string
	for i := 0; i < 10; i++ {
		text := "Operators wear gloves near station " + string(rune('A'+i)) + "." + filler
		claimID := uuid.NewString()
		claimID = "0000000" + string(rune('0'+i)) + claimID[8:]
		if i == 9 {
			text, claimID = "The oven runs at 180 degrees during the bake step."+filler, "ffffffff"+claimID[8:]
		}
		src, span, _ := seedPassage(site, text, claimID, true)
		if i == 9 {
			targetSpan, targetSource = span, src
		}
	}
	snapshot(sc)
	// Site two, same tenant: one healthy approved passage, one the endpoint refuses, and passages
	// that must never be sent for embedding: a span no claim cites, an unreviewed claim's, a
	// withdrawn approval's and a revoked source's.
	ksc2, esc2 := knowledge.Scope{TenantID: tenant, SiteID: site2}, evidence.Scope{TenantID: tenant, SiteID: site2}
	forkliftSource, _, _ := seedPassage(site2, "Forklifts park in bay four.", uuid.NewString(), true)
	seedPassage(site2, "POISON passage the endpoint refuses.", uuid.NewString(), true)
	seedPassage(site2, "Uncited span text delta-secret.", "", false)
	seedPassage(site2, "Unreviewed claim text echo-secret.", uuid.NewString(), false)
	withdrawnClaim := uuid.NewString()
	_, _, withdrawnReview := seedPassage(site2, "Withdrawn approval text bravo-secret.", withdrawnClaim, true)
	revokedSource, _, _ := seedPassage(site2, "Revoked source text alpha-secret.", uuid.NewString(), true)
	// WIS-517 review: approvals whose designation fell after the snapshot. On main pendingSQL
	// checked only review withdrawal and sent both passages to the endpoint.
	undesignatedSource, _, _ := seedPassage(site2, "Undesignated source text golf-secret.", uuid.NewString(), true)
	scannedSource, _, _ := seedPassage(site2, "Unconfirmed scan text hotel-secret.", uuid.NewString(), true)
	snapshot(ksc2)
	if _, err = ev.DecideAuthority(ctx, ea, esc2, undesignatedSource, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "withdraw", Reason: "No longer authoritative"}, uuid.NewString(), 1); err != nil {
		t.Fatal(err)
	}
	// What migration 077 does to a legacy OCR revision: its text is marked model-extracted and the
	// pre-077 designation (model_extracted_confirmed=false) no longer counts.
	scanRun := uuid.NewString()
	for _, q := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO supervisor.agent_runs(tenant_id,site_id,id,request_kind,conversation_id,request_id,trace_id,provider_name,provider_actual,model,model_revision,prompt_version,tool_version,policy_revision,invocations_left,tool_calls_left,requests_left,tokens_left,cost_left_microusd)
 VALUES($1,$2,$3,'extraction',gen_random_uuid(),gen_random_uuid(),$4,'fixture',false,'vision','r','p','t',1,1,1,1,1,1)`, []any{tenant, site2, scanRun, strings.Repeat("a", 32)}},
		{`ALTER TABLE supervisor.evidence_revisions DISABLE TRIGGER evidence_immutable, DISABLE TRIGGER knowledge_inventory_changed`, nil},
		{`UPDATE supervisor.evidence_revisions SET kind='document',extraction='model_ocr',extractor_model='vision',extractor_model_revision='r',extractor_prompt_sha256=repeat('b',64),extraction_run_id=$3 WHERE tenant_id=$1 AND source_id=$2`, []any{tenant, scannedSource, scanRun}},
		{`ALTER TABLE supervisor.evidence_revisions ENABLE TRIGGER evidence_immutable, ENABLE TRIGGER knowledge_inventory_changed`, nil},
	} {
		if _, err = admin.ExecContext(ctx, q.q, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = ks.RevokeReview(ctx, ka, ksc2, withdrawnClaim, knowledge.RevocationInput{ClaimRevision: 1, ReviewID: withdrawnReview, Reason: "Withdrawn"}, uuid.NewString(), 1); err != nil {
		t.Fatal(err)
	}
	if err = revokeSource(esc2, revokedSource, ctx); err != nil {
		t.Fatal(err)
	}
	base := "/v1/sites/" + site + "/chat-conversations"
	doAs := func(as session, method, p, key, match, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, p, strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: "supervisor", Value: as.token})
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", as.csrf)
		r.Header.Set("Content-Type", "application/json")
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		if match != "" {
			r.Header.Set("If-Match", match)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	do := func(method, p, key, match, body string) *httptest.ResponseRecorder {
		return doAs(leadS, method, p, key, match, body)
	}
	w := do("POST", base, uuid.NewString(), "", `{"title":"Oven"}`)
	var conv chat.Conversation
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &conv) != nil {
		t.Fatalf("conversation: %d %s", w.Code, w.Body.String())
	}
	revision := conv.Revision
	// post posts the synonym question; result returns the reply's retrieval label and whether
	// the answer passage was captured for the run; ask does both.
	post := func() *httptest.ResponseRecorder {
		return do("POST", base+"/"+conv.ConversationID+"/messages", uuid.NewString(), `"`+itoa(revision)+`"`, `{"request_id":"`+uuid.NewString()+`","text":"How hot should the furnace be?"}`)
	}
	result := func(w *httptest.ResponseRecorder) (string, bool) {
		t.Helper()
		var out chat.PostResult
		if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("ask: %d %s", w.Code, w.Body.String())
		}
		revision = out.Conversation.Revision
		var run sql.NullString
		if err := admin.QueryRowContext(ctx, `SELECT run_id::text FROM supervisor.chat_messages WHERE message_id=$1`, out.Reply.MessageID).Scan(&run); err != nil {
			t.Fatal(err)
		}
		var captured bool
		if !run.Valid {
			// No run started (the question was refused): nothing was captured.
			if out.Reply.Retrieval != nil {
				return *out.Reply.Retrieval, false
			}
			return "", false
		}
		if err := admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.agent_run_spans WHERE run_id=$1 AND span_id=$2)`, run.String, targetSpan).Scan(&captured); err != nil {
			t.Fatal(err)
		}
		// No worker answers these runs, and a site may have only chat.MaxRunningAnswersPerSite running
		// (WIS-722). Moving the run's start past its wall time stops it counting, as a run a lost
		// worker left behind stops counting, so the next question can start a run.
		if _, err := admin.ExecContext(ctx, `UPDATE supervisor.agent_runs SET started_at=started_at-interval '1 hour' WHERE id=$1`, run.String); err != nil {
			t.Fatal(err)
		}
		label := ""
		if out.Reply.Retrieval != nil {
			label = *out.Reply.Retrieval
		}
		return label, captured
	}
	ask := func() (string, bool) { t.Helper(); return result(post()) }

	stand := embeddingStandIn(t, "qwen3-embedding-0.6b")
	sURL, _ := url.Parse(stand.URL)
	cfgEmb := config.Embeddings{Enabled: true, Model: "qwen3-embedding-0.6b", ModelRevision: "r1", Timeout: 3 * time.Second}
	client := retrieval.NewWithTransport(cfgEmb, sURL, stand.Client().Transport)
	retrieval.SetDefault(client)
	t.Cleanup(func() { retrieval.SetDefault(nil) })

	if label, captured := ask(); label != chat.RetrievalWordFallback || captured {
		t.Fatalf("before embedding: label %q, answer captured %v", label, captured)
	}
	stand.take()
	// The batch call is held open while site two's healthy source is revoked; the batch is then
	// refused (it holds the POISON span) and the per-span retries must not resend the revoked text.
	batchEntered, batchRelease := make(chan struct{}), make(chan struct{})
	stand.setHook(func(path string, input []string) {
		if path == "/v1/embeddings" && len(input) > 1 {
			select {
			case batchEntered <- struct{}{}:
			case <-time.After(10 * time.Second):
				return
			}
			select {
			case <-batchRelease:
			case <-time.After(10 * time.Second):
			}
		}
	})
	type embedded struct {
		n   int
		err error
	}
	tick := make(chan embedded, 1)
	go func() { n, err := retrieval.EmbedPending(ctx, db.DB, client, tenant, 100); tick <- embedded{n, err} }()
	select {
	case <-batchEntered:
	case <-time.After(20 * time.Second):
		t.Fatal("no batch call")
	}
	err = revokeSource(esc2, forkliftSource, ctx)
	batchRelease <- struct{}{}
	if err != nil {
		t.Fatal(err)
	}
	got := <-tick
	stand.setHook(nil)
	n, err := got.n, got.err
	all := stand.take()
	sent := strings.Join(all, "\n")
	if k := slices.Index(all, "Forklifts park in bay four."); k < 0 || slices.Contains(all[k+1:], all[k]) {
		t.Fatal("a source revoked while the batch was pending was resent on retry")
	}
	for _, secret := range []string{"delta-secret", "echo-secret", "bravo-secret", "alpha-secret", "golf-secret", "hotel-secret"} {
		if strings.Contains(sent, secret) {
			t.Fatalf("the embedding job sent an ineligible passage (%s)", secret)
		}
	}
	// Site one's ten are stored; the refused one is deferred and the revoked one is not stored.
	if n != 10 || !errors.Is(err, retrieval.ErrEndpoint) {
		t.Fatalf("embedded %d (%v)", n, err)
	}
	// The next tick does not resend the deferred span and has nothing else to do.
	if n, err = retrieval.EmbedPending(ctx, db.DB, client, tenant, 100); n != 0 || err != nil || len(stand.take()) != 0 {
		t.Fatalf("after deferral: embedded %d (%v)", n, err)
	}
	// A full batch of refused spans is deferred, so the healthy span behind it embeds next tick.
	ksc3 := knowledge.Scope{TenantID: tenant, SiteID: site3}
	seedPassage(site3, "POISON first refused passage.", uuid.NewString(), true)
	seedPassage(site3, "POISON second refused passage.", uuid.NewString(), true)
	seedPassage(site3, "Conveyor belts stop at noon.", uuid.NewString(), true)
	snapshot(ksc3)
	if n, err = retrieval.EmbedPending(ctx, db.DB, client, tenant, 2); n != 0 || !errors.Is(err, retrieval.ErrEndpoint) {
		t.Fatalf("refused batch: embedded %d (%v)", n, err)
	}
	stand.take()
	if n, err = retrieval.EmbedPending(ctx, db.DB, client, tenant, 2); n != 1 || err != nil || !slices.Equal(stand.take(), []string{"Conveyor belts stop at noon."}) {
		t.Fatalf("behind a refused batch: embedded %d (%v)", n, err)
	}
	// A refused lone span (limit 1) is deferred too, so it cannot block the healthy one behind it
	// (Codex round 3: the old singleton early return left it pending forever).
	seedPassage(site3, "POISON lone refused passage.", uuid.NewString(), true)
	seedPassage(site3, "Hoists lift at most one tonne.", uuid.NewString(), true)
	snapshot(ksc3)
	if n, err = retrieval.EmbedPending(ctx, db.DB, client, tenant, 1); n != 0 || !errors.Is(err, retrieval.ErrEndpoint) || !slices.Equal(stand.take(), []string{"POISON lone refused passage."}) {
		t.Fatalf("refused singleton: embedded %d (%v)", n, err)
	}
	if n, err = retrieval.EmbedPending(ctx, db.DB, client, tenant, 1); n != 1 || err != nil || !slices.Equal(stand.take(), []string{"Hoists lift at most one tonne."}) {
		t.Fatalf("behind a refused singleton: embedded %d (%v)", n, err)
	}
	// Codex round 2: after a refused batch, eligibility is rechecked before each single-span
	// retry. Span B loses eligibility while span A's retry is pending; B must not be sent or stored.
	stored := func(span string) bool {
		t.Helper()
		var ok bool
		if e := admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.span_embeddings WHERE span_id=$1)`, span).Scan(&ok); e != nil {
			t.Fatal(e)
		}
		return ok
	}
	esc3 := evidence.Scope{TenantID: tenant, SiteID: site3}
	for _, tc := range []struct {
		name     string
		withdraw func(source, claim, review string) error
	}{
		{"source revoked", func(source, _, _ string) error { return revokeSource(esc3, source, ctx) }},
		{"approval withdrawn", func(_, claim, review string) error {
			_, e := ks.RevokeReview(ctx, ka, ksc3, claim, knowledge.RevocationInput{ClaimRevision: 1, ReviewID: review, Reason: "Withdrawn"}, uuid.NewString(), 1)
			return e
		}},
	} {
		textA, textB := "Pallets stack two high ("+tc.name+").", "Retry secret foxtrot ("+tc.name+")."
		seedPassage(site3, "POISON refused before retry ("+tc.name+").", uuid.NewString(), true)
		_, spanA, _ := seedPassage(site3, textA, uuid.NewString(), true)
		claimB := uuid.NewString()
		sourceB, spanB, reviewB := seedPassage(site3, textB, claimB, true)
		snapshot(ksc3)
		stand.take()
		entered, release := make(chan struct{}), make(chan struct{})
		stand.setHook(func(path string, input []string) {
			if path == "/v1/embeddings" && slices.Equal(input, []string{textA}) {
				select {
				case entered <- struct{}{}:
				case <-time.After(10 * time.Second):
					return
				}
				select {
				case <-release:
				case <-time.After(10 * time.Second):
				}
			}
		})
		go func() { n, err := retrieval.EmbedPending(ctx, db.DB, client, tenant, 3); tick <- embedded{n, err} }()
		select {
		case <-entered:
		case <-time.After(20 * time.Second):
			t.Fatalf("%s: no single-span retry for A", tc.name)
		}
		err = tc.withdraw(sourceB, claimB, reviewB)
		close(release)
		if err != nil {
			t.Fatal(err)
		}
		got := <-tick
		stand.setHook(nil)
		retried := stand.take()
		if k := slices.Index(retried, textB); k < 0 || slices.Contains(retried[k+1:], textB) {
			t.Fatalf("%s: span B absent from the batch or resent after it lost eligibility", tc.name)
		}
		if got.n != 1 || !errors.Is(got.err, retrieval.ErrEndpoint) || !stored(spanA) || stored(spanB) {
			t.Fatalf("%s: embedded %d (%v), A stored %v, B stored %v", tc.name, got.n, got.err, stored(spanA), stored(spanB))
		}
	}
	if label, captured := ask(); label != chat.RetrievalHybrid || !captured {
		t.Fatalf("hybrid: label %q, answer captured %v", label, captured)
	}
	// WIS-647: a configured rerank that does not answer leaves the fused order and says so. It used
	// to keep the hybrid label, so a skipped rerank was invisible. Control: when the rerank
	// answers, the label is hybrid.
	rerankCfg := config.Embeddings{Enabled: true, Model: "qwen3-embedding-0.6b", ModelRevision: "r1", RerankModel: "qwen3-reranker-0.6b", RerankModelRevision: "r1", Timeout: 3 * time.Second}
	retrieval.SetDefault(retrieval.NewWithTransport(rerankCfg, sURL, rerankFailing{stand.Client().Transport}))
	if label, captured := ask(); label != chat.RetrievalHybridUnreranked || !captured {
		t.Fatalf("rerank failed: label %q, answer captured %v", label, captured)
	}
	// (The stand-in's rerank scores every passage 0, so what it captures is only the ID tie-break;
	// the control checks the label.)
	retrieval.SetDefault(retrieval.NewWithTransport(rerankCfg, sURL, stand.Client().Transport))
	if label, _ := ask(); label != chat.RetrievalHybrid {
		t.Fatalf("control, rerank answered: label %q", label)
	}
	// WIS-700: the question's embed and rerank share one deadline of the configured timeout. Each
	// call alone fits its 1 s budget (0.6 s, then 0.9 s), so with separate budgets both answered
	// and the post waited about 1.5 s; with one deadline the rerank is cut at about 0.4 s and the
	// reply says hybrid_unreranked.
	t.Run("WIS700OneDeadlineForEmbedAndRerank", func(t *testing.T) {
		slowCfg := rerankCfg
		slowCfg.Timeout = time.Second
		retrieval.SetDefault(retrieval.NewWithTransport(slowCfg, sURL, slowCalls{next: stand.Client().Transport, embed: 600 * time.Millisecond, rerank: 900 * time.Millisecond}))
		started := time.Now()
		label, _ := ask()
		if took := time.Since(started); label != chat.RetrievalHybridUnreranked || took >= 1400*time.Millisecond {
			t.Fatalf("slow embed and rerank: label %q after %v, want %q within one 1 s timeout (plus posting)", label, took, chat.RetrievalHybridUnreranked)
		}
	})
	// PR #303 review: a zero timeout means no shared deadline, not an expired one: the embed and the
	// rerank both answer and the reply is hybrid (WithTimeout(ctx, 0) would make it word_fallback).
	t.Run("WIS700ZeroTimeoutEmbedsAndReranks", func(t *testing.T) {
		zeroCfg := rerankCfg
		zeroCfg.Timeout = 0
		retrieval.SetDefault(retrieval.NewWithTransport(zeroCfg, sURL, stand.Client().Transport))
		// Only the label is checked, as for the rerank control above: the stand-in's rerank scores
		// every passage 0. hybrid needs the embed and the rerank to have answered.
		if label, _ := ask(); label != chat.RetrievalHybrid {
			t.Fatalf("zero timeout: label %q, want %q", label, chat.RetrievalHybrid)
		}
	})
	// PR #303 reviews: the shared deadline derives from the caller's context, so a shorter caller
	// deadline still bounds the calls, with no client timeout and with a configured one longer than
	// the caller's (5 s against 1 s; a deadline built from context.Background() would ignore the
	// caller). The 0.9 s rerank after a 0.6 s embed is cut by the 1 s request deadline.
	for _, tc := range []struct {
		name    string
		timeout time.Duration
	}{{"WIS700KeepsCallerDeadline", 0}, {"WIS700KeepsCallerDeadlineUnderLongerTimeout", 5 * time.Second}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := rerankCfg
			cfg.Timeout = tc.timeout
			var cut atomic.Int32
			retrieval.SetDefault(retrieval.NewWithTransport(cfg, sURL, slowCalls{next: stand.Client().Transport, embed: 600 * time.Millisecond, rerank: 900 * time.Millisecond, cut: &cut}))
			dctx, dcancel := context.WithTimeout(ctx, time.Second)
			defer dcancel()
			body := `{"request_id":"` + uuid.NewString() + `","text":"How hot should the furnace be?"}`
			r := httptest.NewRequest("POST", base+"/"+conv.ConversationID+"/messages", strings.NewReader(body)).WithContext(dctx)
			r.AddCookie(&http.Cookie{Name: "supervisor", Value: leadS.token})
			r.Header.Set("Origin", origin)
			r.Header.Set("X-CSRF-Token", leadS.csrf)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", uuid.NewString())
			r.Header.Set("If-Match", `"`+itoa(revision)+`"`)
			w := httptest.NewRecorder()
			started := time.Now()
			handler.ServeHTTP(w, r)
			took := time.Since(started)
			if w.Code == http.StatusCreated { // the post may still be recorded; keep the revision current
				var out chat.PostResult
				if json.Unmarshal(w.Body.Bytes(), &out) == nil {
					revision = out.Conversation.Revision
				}
			}
			if cut.Load() != 1 || took >= 1400*time.Millisecond {
				t.Fatalf("client timeout %v, caller deadline 1 s: %d call(s) cut, post took %v (status %d); want the rerank cut within about 1 s", tc.timeout, cut.Load(), took, w.Code)
			}
		})
	}
	retrieval.SetDefault(client)
	// No model call before the preflight passes: an operator (no chat) is refused, and a replayed
	// post answers from its receipt, without either reaching the endpoint.
	stand.take()
	msg := base + "/" + conv.ConversationID + "/messages"
	question := `{"request_id":"` + uuid.NewString() + `","text":"How hot should the furnace be?"}`
	if w := doAs(operatorS, "POST", msg, uuid.NewString(), `"`+itoa(revision)+`"`, question); w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
		t.Fatalf("operator post: %d %s", w.Code, w.Body.String())
	}
	if sent := stand.take(); len(sent) != 0 {
		t.Fatalf("an operator's refused post reached the endpoint: %q", sent)
	}
	key, match := uuid.NewString(), `"`+itoa(revision)+`"`
	result(do("POST", msg, key, match, question))
	stand.take()
	if w := do("POST", msg, key, match, question); w.Code != 200 && w.Code != 201 {
		t.Fatalf("replay: %d %s", w.Code, w.Body.String())
	}
	if sent := stand.take(); len(sent) != 0 {
		t.Fatalf("a replayed post reached the endpoint: %q", sent)
	}
	// Another embedding revision never reads r1's rows.
	retrieval.SetDefault(retrieval.NewWithTransport(config.Embeddings{Enabled: true, Model: "qwen3-embedding-0.6b", ModelRevision: "r2", Timeout: 3 * time.Second}, sURL, stand.Client().Transport))
	if label, captured := ask(); label != chat.RetrievalWordFallback || captured {
		t.Fatalf("other revision: label %q, answer captured %v", label, captured)
	}
	// Semantic retrieval off: the reply says word.
	retrieval.SetDefault(nil)
	if label, _ := ask(); label != chat.RetrievalWord {
		t.Fatalf("off: label %q", label)
	}
	// Model calls hold no site lock (WIS-627 review). The question's embedding and then its rerank
	// call are held open; meanwhile an evidence write and then the answer source's revocation must
	// go through well within the 5 s lock timeout, and once the calls return the revoked source,
	// however well it ranked, is not captured.
	retrieval.SetDefault(retrieval.NewWithTransport(config.Embeddings{Enabled: true, Model: "qwen3-embedding-0.6b", ModelRevision: "r1", RerankModel: "qwen3-reranker-0.6b", RerankModelRevision: "r1", Timeout: 30 * time.Second}, sURL, stand.Client().Transport))
	entered, release := make(chan string), make(chan struct{})
	stand.setHook(func(path string, input []string) {
		if path == "/v1/rerank" || (len(input) == 1 && strings.Contains(input[0], "furnace")) {
			select {
			case entered <- path:
			case <-time.After(10 * time.Second):
				return
			}
			select {
			case <-release:
			case <-time.After(10 * time.Second):
			}
		}
	})
	posted := make(chan *httptest.ResponseRecorder, 1)
	go func() { posted <- post() }()
	for _, step := range []struct {
		call  string
		write func(context.Context) error
	}{
		{"/v1/embeddings", func(wctx context.Context) error {
			_, e := ev.CreateOrRevise(wctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Note", Text: "Written while the question is embedded."}, uuid.NewString(), 0)
			return e
		}},
		{"/v1/rerank", func(wctx context.Context) error { return revokeSource(es, targetSource, wctx) }},
	} {
		var call string
		select {
		case call = <-entered:
		case <-time.After(20 * time.Second):
			t.Fatalf("no %s call", step.call)
		}
		wctx, wcancel := context.WithTimeout(ctx, 3*time.Second)
		err := step.write(wctx)
		wcancel()
		release <- struct{}{}
		if call != step.call || err != nil {
			t.Fatalf("site write during the %s call (%s): %v", step.call, call, err)
		}
	}
	select {
	case w := <-posted:
		if _, captured := result(w); captured {
			t.Fatal("a source revoked during inference was captured")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("question not answered")
	}
}

// slowCalls delays each embedding and rerank call (until the request's context ends), then
// passes it on.
type slowCalls struct {
	next          http.RoundTripper
	embed, rerank time.Duration
	cut           *atomic.Int32 // when set, counts calls whose context ended during the delay
}

func (s slowCalls) RoundTrip(r *http.Request) (*http.Response, error) {
	d := s.embed
	if strings.HasSuffix(r.URL.Path, "/v1/rerank") {
		d = s.rerank
	}
	select {
	case <-time.After(d):
	case <-r.Context().Done():
		if s.cut != nil {
			s.cut.Add(1)
		}
		return nil, r.Context().Err()
	}
	return s.next.RoundTrip(r)
}

// rerankFailing answers every /v1/rerank call with 500 and passes everything else on.
type rerankFailing struct{ next http.RoundTripper }

func (f rerankFailing) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/v1/rerank") {
		return &http.Response{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: r}, nil
	}
	return f.next.RoundTrip(r)
}
