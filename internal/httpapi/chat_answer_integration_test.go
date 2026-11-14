//go:build integration

package httpapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/chat"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/dispatch"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestChatAnsweringPostgres covers WIS-517 slice 2 end to end on real PostgreSQL: a commissioned
// site answers a question through a captured conversation run with the deterministic fixture
// provider. It proves the answer is rebuilt from cited, validated statements; that approval is
// derived server-side from the captured snapshot and its readable sources; that an unsourced
// number, a lost membership and a revoked source each end the reply without content; and that a
// site without an approved process starts no run at all.
func TestChatAnsweringPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: real PostgreSQL admin and runtime URLs required")
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	database := "chat_answer_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.ExecContext(ctx, "CREATE DATABASE "+database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if _, e := admin.ExecContext(cleanup, "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	})
	connect := func(raw string) *sql.DB {
		t.Helper()
		u, e := url.Parse(raw)
		if e != nil {
			t.Fatal(e)
		}
		u.Path = "/" + database
		db, e := sql.Open("pgx", u.String())
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	control, runtime := connect(adminURL), connect(runtimeURL)
	if err = migrate.Apply(ctx, control); err != nil {
		t.Fatal(err)
	}
	db, err := store.New(runtimeURLFor(runtimeURL, database))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := control.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	tenant, site := uuid.NewString(), uuid.NewString()
	owner, lead := uuid.NewString(), uuid.NewString()
	issuer, origin := "https://chat-answer.invalid/"+uuid.NewString(), "http://127.0.0.1:58180"
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic chat answering')`, tenant)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'chat answer site')`, tenant, site)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner'),($1,$2,$3,$5,'supervisor')`, tenant, site, issuer, owner, lead)
	type session struct{ token, csrf, hash string }
	seed := func(subject string) session {
		t.Helper()
		v := session{token: uuid.NewString(), csrf: uuid.NewString()}
		sum := sha256.Sum256([]byte(v.token))
		v.hash = hex.EncodeToString(sum[:])
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, v.hash, subject, issuer, v.csrf)
		return v
	}
	ownerS, leadS := seed(owner), seed(lead)
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "chat-answer-test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir(), Agent: config.Agent{Provider: config.AgentProviderFixture, Model: "fixture-model", ModelRevision: "sha256:fixture", ModeEnabled: true, ChatAnswering: true}}, db, auth)
	call := func(actor session, method, path, key, revision, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: "supervisor", Value: actor.token})
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", actor.csrf)
		r.Header.Set("Content-Type", "application/json")
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		if revision != "" {
			r.Header.Set("If-Match", revision)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	expect := func(w *httptest.ResponseRecorder, status int) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("HTTP %d want %d: %s", w.Code, status, w.Body.String())
		}
	}
	base := "/v1/sites/" + site + "/chat-conversations"
	w := call(leadS, "POST", base, uuid.NewString(), "", `{"title":"Infeed questions"}`)
	expect(w, 201)
	var conv chat.Conversation
	if err = json.Unmarshal(w.Body.Bytes(), &conv); err != nil {
		t.Fatal(err)
	}
	revision := conv.Revision
	ask := func(text string) chat.PostResult {
		t.Helper()
		w := call(leadS, "POST", base+"/"+conv.ConversationID+"/messages", uuid.NewString(), `"`+itoa(revision)+`"`, `{"request_id":"`+uuid.NewString()+`","text":"`+text+`"}`)
		expect(w, 201)
		var out chat.PostResult
		if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil {
			t.Fatal(e)
		}
		revision = out.Conversation.Revision
		return out
	}
	runOf := func(reply chat.Message) string {
		t.Helper()
		var run sql.NullString
		if e := control.QueryRowContext(ctx, `SELECT run_id::text FROM supervisor.chat_messages WHERE message_id=$1`, reply.MessageID).Scan(&run); e != nil {
			t.Fatal(e)
		}
		return run.String
	}
	sc, es := knowledge.Scope{TenantID: tenant, SiteID: site}, evidence.Scope{TenantID: tenant, SiteID: site}
	ka, ea := knowledge.Actor{Issuer: issuer, Subject: owner, SessionHash: ownerS.hash}, evidence.Actor{Issuer: issuer, Subject: owner, SessionHash: ownerS.hash}
	ks, ev := knowledge.New(db.DB), evidence.New(db.DB)

	// No approved process yet: the reply is an explicit non-answer and no run starts.
	early := ask("How tight are the infeed bolts?")
	if early.Reply.State != chat.StateUnavailable || early.Reply.ReasonCode == nil || *early.Reply.ReasonCode != chat.ReasonNoApprovedProcess || runOf(early.Reply) != "" {
		t.Fatalf("no snapshot: %+v", early.Reply)
	}
	// An approved but empty snapshot has no readable evidence and must not start an agent run.
	orphanText := "An indexed process source without an approved claim."
	orphan, err := ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Unclaimed process source", Text: orphanText}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: orphan.SourceID, Revision: 1, Start: 0, End: len(orphanText)}, uuid.NewString(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err = ev.DecideAuthority(ctx, ea, es, orphan.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Owner verified source"}, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err = ks.CreateEntity(ctx, ka, sc, knowledge.EntityInput{EntityID: uuid.NewString(), Type: "operation", Name: "Unclaimed operation", SourceID: orphan.SourceID, SourceRevision: 1}, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	emptyInventory, err := ks.ReadInventory(ctx, ka, sc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ks.CreateSnapshot(ctx, ka, sc, knowledge.SnapshotInput{SnapshotID: uuid.NewString(), Reason: "No approved claims yet"}, uuid.NewString(), emptyInventory.Revision); err != nil {
		t.Fatal(err)
	}
	noSources := ask("What does the approved process say?")
	if noSources.Reply.State != chat.StateUnavailable || noSources.Reply.ReasonCode == nil || *noSources.Reply.ReasonCode != "NO_READABLE_EVIDENCE" || runOf(noSources.Reply) != "" {
		t.Errorf("empty approved snapshot: %+v run=%s", noSources.Reply, runOf(noSources.Reply))
	}
	// WIS-517 regression: the only approved claim cites 16 KiB of readable spans, over
	// chat.MaxChatPassageBytes. The reply is CAPTURE_LIMIT, not NO_READABLE_EVIDENCE, and no run starts.
	bigText := strings.Repeat("Plant model line. ", 1000)[:4*4096]
	big, err := ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Plant model", Text: bigText}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var bigSpans []string
	for i := 0; i < 4; i++ {
		span, e := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: big.SourceID, Revision: 1, Start: i * 4096, End: (i + 1) * 4096}, uuid.NewString(), 1)
		if e != nil {
			t.Fatal(e)
		}
		bigSpans = append(bigSpans, span.ID)
	}
	if _, err = ev.DecideAuthority(ctx, ea, es, big.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Owner verified plant model"}, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	bigOps := []string{uuid.NewString(), uuid.NewString()}
	for _, id := range bigOps {
		if _, err = ks.CreateEntity(ctx, ka, sc, knowledge.EntityInput{EntityID: id, Type: "operation", Name: "Plant model operation", SourceID: big.SourceID, SourceRevision: 1}, uuid.NewString(), 0); err != nil {
			t.Fatal(err)
		}
	}
	bigClaim := knowledge.ClaimInput{ClaimID: uuid.NewString(), SubjectID: bigOps[0], SubjectRevision: 1, Predicate: "precedes", ObjectEntityID: bigOps[1], ObjectRevision: 1, EffectiveFrom: "2026-09-27T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: bigSpans}
	if _, err = ks.CreateClaim(ctx, ka, sc, bigClaim, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err = ks.DecideReview(ctx, ka, sc, bigClaim.ClaimID, knowledge.ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Verified plant model"}, uuid.NewString(), 1); err != nil {
		t.Fatal(err)
	}
	bigInventory, err := ks.ReadInventory(ctx, ka, sc)
	if err != nil {
		t.Fatal(err)
	}
	bigSnapshot := uuid.NewString()
	if _, err = ks.CreateSnapshot(ctx, ka, sc, knowledge.SnapshotInput{SnapshotID: bigSnapshot, Reason: "Oversized plant model claim"}, uuid.NewString(), bigInventory.Revision); err != nil {
		t.Fatal(err)
	}
	oversized := ask("What does the plant model say?")
	if oversized.Reply.State != chat.StateUnavailable || oversized.Reply.ReasonCode == nil || *oversized.Reply.ReasonCode != chat.ReasonCaptureLimit || runOf(oversized.Reply) != "" {
		t.Errorf("oversized approved claim: %+v run=%s", oversized.Reply, runOf(oversized.Reply))
	}

	// Approve a process. The SOP backs an approved precedence claim with its whole text; a second
	// SOP span backs only a candidate claim that is never approved. A second approved claim cites
	// one span on the lubrication SOP and one on the torque-check memo.
	addSource := func(title, text string, spans ...[2]int) (string, []string) {
		t.Helper()
		src, e := ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: title, Text: text}, uuid.NewString(), 0)
		if e != nil {
			t.Fatal(e)
		}
		ids := []string{}
		for _, r := range spans {
			span, e := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: src.SourceID, Revision: 1, Start: r[0], End: r[1]}, uuid.NewString(), 1)
			if e != nil {
				t.Fatal(e)
			}
			ids = append(ids, span.ID)
		}
		if _, e = ev.DecideAuthority(ctx, ea, es, src.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Owner verified " + title}, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		return src.SourceID, ids
	}
	sopText, lubeText, memoText := "Torque the infeed bolts to 25 Nm before every shift.", "Lubricate the infeed chain before the torque check.", "The torque check follows chain lubrication."
	sop, sopSpans := addSource("Infeed SOP", sopText, [2]int{0, len(sopText)}, [2]int{0, len("Torque the infeed bolts")})
	lube, lubeSpans := addSource("Lubrication SOP", lubeText, [2]int{0, len(lubeText)})
	memo, memoSpans := addSource("Torque check memo", memoText, [2]int{0, len(memoText)})
	approvedSpan, candidateSpan, lubeSpan := sopSpans[0], sopSpans[1], lubeSpans[0]
	ops := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	for i, id := range ops {
		src := sop
		if i == 2 {
			src = lube
		}
		if _, err = ks.CreateEntity(ctx, ka, sc, knowledge.EntityInput{EntityID: id, Type: "operation", Name: "Infeed operation", SourceID: src, SourceRevision: 1}, uuid.NewString(), 0); err != nil {
			t.Fatal(err)
		}
	}
	addClaim := func(subject, object string, approve bool, spans ...string) {
		t.Helper()
		claim := knowledge.ClaimInput{ClaimID: uuid.NewString(), SubjectID: subject, SubjectRevision: 1, Predicate: "precedes", ObjectEntityID: object, ObjectRevision: 1, EffectiveFrom: "2026-09-27T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: spans}
		if _, e := ks.CreateClaim(ctx, ka, sc, claim, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		if !approve {
			return
		}
		if _, e := ks.DecideReview(ctx, ka, sc, claim.ClaimID, knowledge.ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Verified SOP step"}, uuid.NewString(), 1); e != nil {
			t.Fatal(e)
		}
	}
	addClaim(ops[0], ops[1], true, approvedSpan)
	addClaim(ops[2], ops[0], true, lubeSpan, memoSpans[0])
	addClaim(ops[2], ops[1], false, candidateSpan)
	inventory, err := ks.ReadInventory(ctx, ka, sc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ks.CreateSnapshot(ctx, ka, sc, knowledge.SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Reviewed infeed process"}, uuid.NewString(), inventory.Revision); err != nil {
		t.Fatal(err)
	}
	revoke := func(source, reason string) {
		t.Helper()
		var state int64
		if e := control.QueryRowContext(ctx, `SELECT state_revision FROM supervisor.evidence_heads WHERE tenant_id=$1 AND source_id=$2`, tenant, source).Scan(&state); e != nil {
			t.Fatal(e)
		}
		expect(call(ownerS, "POST", "/v1/sites/"+site+"/sources/"+source+"/revocations", uuid.NewString(), `"`+itoa(state)+`"`, `{"reason":"`+reason+`"}`), 200)
	}

	runtimeFor := func(p agent.Provider) *agent.Runtime {
		return &agent.Runtime{Store: agent.NewStore(db.DB), Provider: p, Tools: &agent.Tools{Store: agent.NewStore(db.DB)}, Slots: agent.NewSlots(2, 8),
			Limits: agent.DefaultLimits(), Model: "fixture-model", ModelRevision: "sha256:fixture", Options: agent.LoaderOptions{ModeEnabled: true}}
	}
	answerStep := func(run string, statement agent.ChatStatement) agent.FixtureStep {
		body, _ := json.Marshal(agent.ChatAnswerOutputV2{SchemaVersion: agent.ChatSchemaVersion, RunID: uuid.NewSHA1(uuid.MustParse(run), []byte(agent.RoleDiscoveryMapping)).String(),
			Role: agent.RoleDiscoveryMapping, Status: "answered", Statements: []agent.ChatStatement{statement}, MissingInputs: []string{}})
		return agent.FixtureStep{Response: agent.ModelResponse{Content: string(body)}}
	}
	// The scripted model knows no entity IDs: it reads one captured source and cites span IDs the
	// test pins. claimsFor, when set, makes it also run claims.search on that entity.
	answerRun := func(run, source, claimsFor string, statement agent.ChatStatement) (agent.ChatAnswerResult, error) {
		calls := []agent.ToolCall{{ID: "read", Name: agent.ToolEvidenceGet, Arguments: []byte(`{"evidence_id":"` + source + `"}`)}}
		if claimsFor != "" {
			calls = append(calls, agent.ToolCall{ID: "claims", Name: agent.ToolClaimsSearch, Arguments: []byte(`{"entity_id":"` + claimsFor + `"}`)})
		}
		p := agent.NewFixtureProvider(agent.FixtureStep{Response: agent.ModelResponse{ToolCalls: calls}}, answerStep(run, statement))
		return runtimeFor(p).ChatAnswer(ctx, agent.ChatAnswerRef{TenantID: tenant, SiteID: site, RunID: run})
	}
	reply := func(id string) chat.Message {
		t.Helper()
		w := call(leadS, "GET", base+"/"+conv.ConversationID+"/messages?limit=100", "", "", "")
		expect(w, 200)
		var page chat.MessagePage
		if e := json.Unmarshal(w.Body.Bytes(), &page); e != nil {
			t.Fatal(e)
		}
		for _, m := range page.Messages {
			if m.MessageID == id {
				return m
			}
		}
		t.Fatalf("reply %s not listed", id)
		return chat.Message{}
	}
	// WIS-517 regression: an unapproved draft entity makes the latest approved snapshot stale, but
	// chat still answers from that immutable snapshot and labels its provenance.
	if _, err = ks.CreateEntity(ctx, ka, sc, knowledge.EntityInput{EntityID: uuid.NewString(), Type: "operation", Name: "Unapproved draft operation", SourceID: sop, SourceRevision: 1}, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	staleQ := ask("What does the process say after its inventory changed?")
	staleRun := runOf(staleQ.Reply)
	if staleRun == "" || staleQ.Reply.State != chat.StatePending {
		t.Fatalf("stale-snapshot question did not start a run: %+v", staleQ.Reply)
	}
	staleProvider := agent.NewFixtureProvider(
		agent.FixtureStep{Response: agent.ModelResponse{ToolCalls: []agent.ToolCall{
			{ID: "read-sop", Name: agent.ToolEvidenceGet, Arguments: []byte(`{"evidence_id":"` + sop + `"}`)},
			{ID: "claims", Name: agent.ToolClaimsSearch, Arguments: []byte(`{"entity_id":"` + ops[0] + `"}`)},
		}}},
		answerStep(staleRun, agent.ChatStatement{Kind: "approved_fact", Text: "Infeed bolts are torqued to 25 Nm before every shift.", SourceSpanIDs: []string{approvedSpan}}),
	)
	staleResult, err := runtimeFor(staleProvider).ChatAnswer(ctx, agent.ChatAnswerRef{TenantID: tenant, SiteID: site, RunID: staleRun})
	staleReply := reply(staleQ.Reply.MessageID)
	if err != nil || staleResult.ReplyState != "answered" || staleResult.ReasonCode != "" || staleProvider.Calls() != 2 ||
		staleReply.State != "answered" || staleReply.Body == nil || !strings.Contains(*staleReply.Body, "Based on the approved process snapshot of ") || staleReply.ReasonCode != nil {
		t.Errorf("stale snapshot: result=%+v reply=%+v model_calls=%d err=%v", staleResult, staleReply, staleProvider.Calls(), err)
	}
	if staleReply.SnapshotID == nil || staleReply.SnapshotCreatedAt == nil || staleReply.SnapshotStale == nil || !*staleReply.SnapshotStale {
		t.Errorf("stale snapshot provenance: %+v", staleReply)
	} else {
		want := "Based on the approved process snapshot of " + staleReply.SnapshotCreatedAt.UTC().Format(time.RFC3339) + "; newer knowledge edits are not yet approved."
		if !strings.HasSuffix(*staleReply.Body, want) {
			t.Errorf("stale snapshot label %q, want suffix %q", *staleReply.Body, want)
		}
	}
	inventory, err = ks.ReadInventory(ctx, ka, sc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ks.CreateSnapshot(ctx, ka, sc, knowledge.SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Refreshed after stale-snapshot test"}, uuid.NewString(), inventory.Revision); err != nil {
		t.Fatal(err)
	}

	// 1. WIS-517 M2: without calling claims.search, a statement citing a span of an approved claim
	// is an approved fact; the server derives approval from the captured snapshot.
	q := ask("How tight are the infeed bolts?")
	run := runOf(q.Reply)
	if q.Reply.State != chat.StatePending || run == "" {
		t.Fatalf("commissioned question did not start a run: %+v", q.Reply)
	}
	// WIS-722: a site has at most chat.MaxRunningAnswersPerSite answers running, because chat answers
	// share the worker's model slots with every tenant's investigations. With two open, the next
	// question is refused as ANSWERS_BUSY and starts no run; once one finishes, questions start runs
	// again. Before the fix every question started a run, without bound.
	finish := func(r string) {
		t.Helper()
		if res, e := answerRun(r, sop, "", agent.ChatStatement{Kind: "approved_fact", Text: "Infeed bolts are torqued to 25 Nm before every shift.", SourceSpanIDs: []string{approvedSpan}}); e != nil || res.ReplyState != "answered" {
			t.Fatalf("finishing answer %s: %+v %v", r, res, e)
		}
	}
	second := ask("How tight are the infeed bolts?")
	secondRun := runOf(second.Reply)
	if second.Reply.State != chat.StatePending || secondRun == "" {
		t.Fatalf("second running answer: %+v", second.Reply)
	}
	busy := ask("How tight are the infeed bolts?")
	if busy.Reply.State != chat.StateUnavailable || busy.Reply.ReasonCode == nil || *busy.Reply.ReasonCode != chat.ReasonAnswersBusy || runOf(busy.Reply) != "" {
		t.Fatalf("third answer at a busy site: %+v run=%s", busy.Reply, runOf(busy.Reply))
	}
	finish(secondRun)
	freed := ask("How tight are the infeed bolts?")
	if freedRun := runOf(freed.Reply); freed.Reply.State != chat.StatePending || freedRun == "" {
		t.Fatalf("question after an answer finished: %+v", freed.Reply)
	} else {
		finish(freedRun)
	}
	subtest := func(name string, f func(t *testing.T)) {
		if !t.Run(name, f) {
			t.FailNow()
		}
	}
	// WIS-722 review High: admission stops counting a run WallTime after it started, so the run must
	// not execute after that either. Fill the site (run + aged), age one run past the window, admit
	// a replacement, then dispatch the aged run: it ends DEADLINE_EXCEEDED without a model call.
	// Before the fix ChatAnswer took a fresh deadline from time.Now() and answered with 2 model calls
	// while 3 answers were running at a cap of 2.
	subtest("WIS722ExpiredRunDoesNotExecute", func(t *testing.T) {
		aged := runOf(ask("How tight are the infeed bolts?").Reply)
		if aged == "" {
			t.Fatal("aged run not started")
		}
		if b := ask("How tight are the infeed bolts?"); runOf(b.Reply) != "" {
			t.Fatalf("site not full before ageing: %+v", b.Reply)
		}
		exec(`UPDATE supervisor.agent_runs SET started_at=started_at-make_interval(secs=>$2) WHERE id=$1`, aged, agent.InterviewLimits().WallTime.Seconds()+60)
		replacement := runOf(ask("How tight are the infeed bolts?").Reply)
		if replacement == "" {
			t.Fatal("an expired run still holds an admission place")
		}
		agedProvider := agent.NewFixtureProvider(agent.FixtureStep{Response: agent.ModelResponse{ToolCalls: []agent.ToolCall{{ID: "read", Name: agent.ToolEvidenceGet, Arguments: []byte(`{"evidence_id":"` + sop + `"}`)}}}},
			answerStep(aged, agent.ChatStatement{Kind: "approved_fact", Text: "Infeed bolts are torqued to 25 Nm before every shift.", SourceSpanIDs: []string{approvedSpan}}))
		agedRes, err := runtimeFor(agedProvider).ChatAnswer(ctx, agent.ChatAnswerRef{TenantID: tenant, SiteID: site, RunID: aged})
		var agedState, agedReply, agedReason string
		if e := control.QueryRowContext(ctx, `SELECT r.state,m.state,COALESCE(m.reason_code,'') FROM supervisor.agent_runs r JOIN supervisor.chat_answer_turns t ON t.run_id=r.id
	 JOIN supervisor.chat_messages m ON m.message_id=t.reply_message_id WHERE r.id=$1`, aged).Scan(&agedState, &agedReply, &agedReason); e != nil {
			t.Fatal(e)
		}
		if err != nil || agedProvider.Calls() != 0 || agedRes.ReplyState != "needs_input" || agedRes.ReasonCode != agent.CodeDeadlineExceeded ||
			agedState != agent.RunCompleted || agedReply != "needs_input" || agedReason != agent.CodeDeadlineExceeded {
			t.Fatalf("expired run executed: result=%+v err=%v model_calls=%d run=%s reply=%s reason=%s", agedRes, err, agedProvider.Calls(), agedState, agedReply, agedReason)
		}
		finish(replacement)
	})

	// Helpers for the WIS-722 admission cases below: posts in fresh conversations, by site.
	running := func(s string) int {
		t.Helper()
		var n int
		if e := control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.chat_answer_turns t JOIN supervisor.agent_runs r ON r.id=t.run_id WHERE t.site_id=$1 AND r.state='running'`, s).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	type conversation struct {
		id       string
		revision int64
	}
	newConv := func(actor session, s string) conversation {
		t.Helper()
		w := call(actor, "POST", "/v1/sites/"+s+"/chat-conversations", uuid.NewString(), "", `{"title":"Admission"}`)
		expect(w, 201)
		var c chat.Conversation
		if e := json.Unmarshal(w.Body.Bytes(), &c); e != nil {
			t.Fatal(e)
		}
		return conversation{c.ConversationID, c.Revision}
	}
	post := func(actor session, s string, c conversation) *httptest.ResponseRecorder {
		return call(actor, "POST", "/v1/sites/"+s+"/chat-conversations/"+c.id+"/messages", uuid.NewString(), `"`+itoa(c.revision)+`"`, `{"request_id":"`+uuid.NewString()+`","text":"How tight are the infeed bolts?"}`)
	}
	postRun := func(w *httptest.ResponseRecorder) (string, string) {
		t.Helper()
		expect(w, 201)
		var out chat.PostResult
		if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil {
			t.Fatal(e)
		}
		reason := ""
		if out.Reply.ReasonCode != nil {
			reason = *out.Reply.ReasonCode
		}
		return runOf(out.Reply), reason
	}
	commission := func(tn, s string) {
		t.Helper()
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner'),($1,$2,$3,$5,'supervisor')`, tn, s, issuer, owner, lead)
		ksc, esc := knowledge.Scope{TenantID: tn, SiteID: s}, evidence.Scope{TenantID: tn, SiteID: s}
		src, e := ev.CreateOrRevise(ctx, ea, esc, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Infeed SOP", Text: sopText}, uuid.NewString(), 0)
		if e != nil {
			t.Fatal(e)
		}
		span, e := ev.CreateSpan(ctx, ea, esc, evidence.SpanInput{SourceID: src.SourceID, Revision: 1, Start: 0, End: len(sopText)}, uuid.NewString(), 1)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = ev.DecideAuthority(ctx, ea, esc, src.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Owner verified SOP"}, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		pair := []string{uuid.NewString(), uuid.NewString()}
		for _, id := range pair {
			if _, e = ks.CreateEntity(ctx, ka, ksc, knowledge.EntityInput{EntityID: id, Type: "operation", Name: "Infeed operation", SourceID: src.SourceID, SourceRevision: 1}, uuid.NewString(), 0); e != nil {
				t.Fatal(e)
			}
		}
		claim := knowledge.ClaimInput{ClaimID: uuid.NewString(), SubjectID: pair[0], SubjectRevision: 1, Predicate: "precedes", ObjectEntityID: pair[1], ObjectRevision: 1, EffectiveFrom: "2026-09-27T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
		if _, e = ks.CreateClaim(ctx, ka, ksc, claim, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		if _, e = ks.DecideReview(ctx, ka, ksc, claim.ClaimID, knowledge.ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Verified SOP step"}, uuid.NewString(), 1); e != nil {
			t.Fatal(e)
		}
		inv, e := ks.ReadInventory(ctx, ka, ksc)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = ks.CreateSnapshot(ctx, ka, ksc, knowledge.SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Reviewed infeed process"}, uuid.NewString(), inv.Revision); e != nil {
			t.Fatal(e)
		}
	}

	// WIS-722 review Medium, concurrency: with one place left, parallel posts from two users in
	// separate conversations admit exactly one; the rest are ANSWERS_BUSY and start no run.
	var admitted []string
	subtest("WIS722ParallelPostsAdmitOnePlace", func(t *testing.T) {
		if n := running(site); n != 1 {
			t.Fatalf("running answers before the parallel posts: %d, want 1", n)
		}
		const parallel = 10
		convs, results := make([]conversation, parallel), make([]*httptest.ResponseRecorder, parallel)
		actors := []session{leadS, ownerS}
		for i := range convs {
			convs[i] = newConv(actors[i%2], site)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range convs {
			wg.Add(1)
			go func() { defer wg.Done(); <-start; results[i] = post(actors[i%2], site, convs[i]) }()
		}
		close(start)
		wg.Wait()
		busyN := 0
		for _, w := range results {
			r, reason := postRun(w)
			switch {
			case r != "":
				admitted = append(admitted, r)
			case reason == chat.ReasonAnswersBusy:
				busyN++
			default:
				t.Fatalf("parallel post neither admitted nor busy: %s", w.Body.String())
			}
		}
		if len(admitted) != 1 || busyN != parallel-1 || running(site) != chat.MaxRunningAnswersPerSite {
			t.Fatalf("parallel posts with one place left: admitted=%d busy=%d running=%d (cap %d)", len(admitted), busyN, running(site), chat.MaxRunningAnswersPerSite)
		}
	})

	// WIS-722 decision A (2026-11-13): no per-tenant cap. With this site full, a second site of the
	// same tenant still admits its own MaxRunningAnswersPerSite answers, and another tenant's site
	// is admitted too. A cap counted per tenant (or across all sites) fails here.
	subtest("WIS722SisterSiteAdmittedNoTenantCap", func(t *testing.T) {
		if r, reason := postRun(post(leadS, site, newConv(leadS, site))); r != "" || reason != chat.ReasonAnswersBusy {
			t.Fatalf("full site admitted another answer: run=%s reason=%s", r, reason)
		}
		sisterSite := uuid.NewString()
		exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'chat answer sister site')`, tenant, sisterSite)
		commission(tenant, sisterSite)
		for i := 0; i < chat.MaxRunningAnswersPerSite; i++ {
			if r, reason := postRun(post(leadS, sisterSite, newConv(leadS, sisterSite))); r == "" {
				t.Fatalf("sister site of a full site refused answer %d: %s", i+1, reason)
			}
		}
		if r, reason := postRun(post(leadS, sisterSite, newConv(leadS, sisterSite))); r != "" || reason != chat.ReasonAnswersBusy {
			t.Fatalf("sister site past its own cap: run=%s reason=%s", r, reason)
		}
	})
	subtest("WIS722OtherTenantAdmitted", func(t *testing.T) {
		otherTenant, otherSite := uuid.NewString(), uuid.NewString()
		exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic other chat tenant')`, otherTenant)
		exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'other tenant chat site')`, otherTenant, otherSite)
		commission(otherTenant, otherSite)
		if r, reason := postRun(post(leadS, otherSite, newConv(leadS, otherSite))); r == "" {
			t.Fatalf("another tenant refused while this tenant is full: %s", reason)
		}
		if n := running(site); n != chat.MaxRunningAnswersPerSite {
			t.Fatalf("other sites changed this site's running answers: %d", n)
		}
	})
	finish(admitted[0])
	// This draft arrives after capture: the reply keeps the fresh snapshot state recorded on the
	// question, even though the live inventory is stale by dispatch time.
	if _, err = ks.CreateEntity(ctx, ka, sc, knowledge.EntityInput{EntityID: uuid.NewString(), Type: "operation", Name: "Post-capture draft operation", SourceID: sop, SourceRevision: 1}, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	if got := reply(q.Reply.MessageID); got.State != chat.StatePending || got.Body != nil {
		t.Fatalf("pending reply before the run: %+v", got)
	}
	result, err := answerRun(run, sop, "", agent.ChatStatement{Kind: "approved_fact", Text: "Infeed bolts are torqued to 25 Nm before every shift.", SourceSpanIDs: []string{approvedSpan}})
	if err != nil || result.ReplyState != "answered" {
		t.Fatalf("answer: %+v %v", result, err)
	}
	answered := reply(q.Reply.MessageID)
	answeredQ := q
	if answered.State != "answered" || answered.Body == nil || *answered.Body != "- [Approved process] Infeed bolts are torqued to 25 Nm before every shift. [1]" ||
		answered.SnapshotID == nil || answered.SnapshotCreatedAt == nil || answered.SnapshotStale == nil || *answered.SnapshotStale || strings.Contains(*answered.Body, "Based on the approved process snapshot") ||
		len(answered.Citations) != 1 || answered.Citations[0].ID != approvedSpan || answered.Citations[0].Revision != 1 || answered.ReasonCode != nil {
		t.Fatalf("answered reply: %+v body=%v", answered, answered.Body)
	}
	// WIS-647: the finished answer keeps how its passages were chosen. Finishing used to rewrite
	// the provenance without it, so a word_fallback label vanished once the answer completed.
	if answered.Retrieval == nil || *answered.Retrieval != chat.RetrievalWord {
		t.Fatalf("answered reply lost its retrieval label: %v", answered.Retrieval)
	}
	var deps string
	if err = control.QueryRowContext(ctx, `SELECT dependencies::text FROM supervisor.chat_messages WHERE message_id=$1`, q.Reply.MessageID).Scan(&deps); err != nil || !strings.Contains(deps, run) || !strings.Contains(deps, agent.ChatPromptVersion) {
		t.Fatalf("dependency manifest %s (%v)", deps, err)
	}
	// WIS-623 review finding 4, through the chat answer entry point: with the site's hosted switch
	// on, a chat run is pinned to the paid route when the question is asked. It used to keep the
	// local zero-cost ceiling, so its first priced reservation failed and no call was made.
	price, version, ok := agent.HostedPrice("qwen-intl", "qwen-plus-2025-09-11", "qwen-plus-2025-09-11")
	if !ok {
		t.Fatal("no committed price")
	}
	expect(call(ownerS, "POST", "/v1/sites/"+site+"/model-hosting/decisions", uuid.NewString(), `"0"`, `{"enabled":true,"dpa_reference":"DPA-2026-11-05","reason":"Pilot on hosted Qwen"}`), 200)
	// The route is installed before the question, as at process start: the run is pinned when created.
	route := &agent.HostedRoute{Name: "hosted-qwen-intl", Region: "ap-southeast-1", Model: "qwen-plus-2025-09-11", ModelRevision: "qwen-plus-2025-09-11", Price: &price, PriceVersion: version}
	agent.SetHostedRoute(route)
	t.Cleanup(func() { agent.SetHostedRoute(nil) })
	hostedQ := ask("How tight are the infeed bolts on the hosted model?")
	hostedRun := runOf(hostedQ.Reply)
	hostedStep := answerStep(hostedRun, agent.ChatStatement{Kind: "approved_fact", Text: "Infeed bolts are torqued to 25 Nm before every shift.", SourceSpanIDs: []string{approvedSpan}})
	hostedStep.Response.InputTokens, hostedStep.Response.OutputTokens = 1000, 100
	hostedProvider := agent.NewFixtureProvider(agent.FixtureStep{Response: agent.ModelResponse{InputTokens: 1000, OutputTokens: 100, ToolCalls: []agent.ToolCall{{ID: "read", Name: agent.ToolEvidenceGet, Arguments: []byte(`{"evidence_id":"` + sop + `"}`)}}}}, hostedStep)
	route.Provider = hostedProvider
	local := agent.NewFixtureProvider()
	hostedResult, err := runtimeFor(local).ChatAnswer(ctx, agent.ChatAnswerRef{TenantID: tenant, SiteID: site, RunID: hostedRun})
	agent.SetHostedRoute(nil)
	var pinnedTo string
	var calls int
	var charged int64
	if e := control.QueryRowContext(ctx, `SELECT r.provider_name,count(c.*),COALESCE(sum(c.charged_microusd),0) FROM supervisor.agent_runs r LEFT JOIN supervisor.agent_model_calls c ON (c.tenant_id,c.run_id)=(r.tenant_id,r.id) WHERE r.id=$1 GROUP BY r.provider_name`, hostedRun).Scan(&pinnedTo, &calls, &charged); e != nil {
		t.Fatal(e)
	}
	if err != nil || hostedResult.ReplyState != "answered" || hostedProvider.Calls() != 2 || local.Calls() != 0 || pinnedTo != "hosted-qwen-intl" || calls != 2 || charged <= 0 {
		t.Fatalf("hosted chat answer: %+v err=%v hosted=%d local=%d pinned=%s calls=%d charged=%d", hostedResult, err, hostedProvider.Calls(), local.Calls(), pinnedTo, calls, charged)
	}
	expect(call(ownerS, "POST", "/v1/sites/"+site+"/model-hosting/decisions", uuid.NewString(), `"1"`, `{"enabled":false,"reason":"End of hosted test"}`), 200)

	// A finished run replays its stored outcome and calls no model.
	idle := agent.NewFixtureProvider()
	if again, e := runtimeFor(idle).ChatAnswer(ctx, agent.ChatAnswerRef{TenantID: tenant, SiteID: site, RunID: run}); e != nil || !again.Replayed || again.ReplyState != "answered" || idle.Calls() != 0 {
		t.Fatalf("replay: %+v calls=%d %v", again, idle.Calls(), e)
	}

	// WIS-517 review MAJOR: an in-flight reply created before provenance was added has only the
	// migration default dependencies object. It must complete without a label or snapshot fields.
	legacy := ask("What does the process say for a reply captured before snapshot labels?")
	legacyRun := runOf(legacy.Reply)
	if legacyRun == "" || legacy.Reply.State != chat.StatePending {
		t.Fatalf("legacy question did not start a run: %+v", legacy.Reply)
	}
	exec(`ALTER TABLE supervisor.chat_messages DISABLE TRIGGER chat_message_guard`)
	exec(`UPDATE supervisor.chat_messages SET dependencies='{}'::jsonb WHERE message_id=$1`, legacy.Reply.MessageID)
	exec(`ALTER TABLE supervisor.chat_messages ENABLE TRIGGER chat_message_guard`)
	legacyResult, err := answerRun(legacyRun, sop, "", agent.ChatStatement{Kind: "approved_fact", Text: "Infeed bolts are torqued to 25 Nm before every shift.", SourceSpanIDs: []string{approvedSpan}})
	if err != nil || legacyResult.State != agent.RunCompleted || legacyResult.ReplyState != "answered" {
		t.Fatalf("legacy pending run: result=%+v err=%v", legacyResult, err)
	}
	legacyReply := reply(legacy.Reply.MessageID)
	if legacyReply.State != "answered" || legacyReply.Body == nil || *legacyReply.Body != "- [Approved process] Infeed bolts are torqued to 25 Nm before every shift. [1]" ||
		legacyReply.SnapshotID != nil || legacyReply.SnapshotCreatedAt != nil || legacyReply.SnapshotStale != nil || strings.Contains(*legacyReply.Body, "Based on the approved process snapshot") {
		t.Fatalf("legacy reply with unknown provenance: %+v", legacyReply)
	}
	idle = agent.NewFixtureProvider()
	if again, e := runtimeFor(idle).ChatAnswer(ctx, agent.ChatAnswerRef{TenantID: tenant, SiteID: site, RunID: legacyRun}); e != nil || !again.Replayed || again.State != agent.RunCompleted || again.ReplyState != "answered" || idle.Calls() != 0 {
		t.Fatalf("legacy completed-run replay: %+v calls=%d %v", again, idle.Calls(), e)
	}
	replayedLegacy := reply(legacy.Reply.MessageID)
	if replayedLegacy.Body == nil || *replayedLegacy.Body != *legacyReply.Body || replayedLegacy.SnapshotID != nil || replayedLegacy.SnapshotCreatedAt != nil || replayedLegacy.SnapshotStale != nil {
		t.Fatalf("legacy replay changed stored reply: %+v", replayedLegacy)
	}

	// 2. Not approved, with no content: a span that backs only a candidate claim, and a readable
	// span of an approved claim whose other cited source is outside the asker's captured read
	// scope. Since WIS-620 the candidate-only span is never given to the model (only the passages of
	// selected approved claims are), so citing it is out of scope. The second case also runs
	// claims.search, which returns that claim but grants no approval (it did before: the search
	// result was the approval).
	for _, tc := range []struct {
		name, source, claimsFor, text, span, uncaptured, code string
	}{
		{"candidate claim only", sop, "", "Torque the infeed bolts.", candidateSpan, "", agent.CodeCitationNotInScope},
		{"approved claim with an uncaptured source", lube, ops[2], "Lubricate the infeed chain before the torque check.", lubeSpan, memo, agent.CodeFactNotApproved},
	} {
		q = ask("Is this approved: " + tc.name + "?")
		run = runOf(q.Reply)
		if tc.uncaptured != "" {
			// Every member reads every unrevoked source today, a revoked one kills the snapshot and
			// capture takes whole claims (WIS-620), so inject the uncaptured source directly.
			exec(`DELETE FROM supervisor.agent_run_refs WHERE run_id=$1 AND kind='evidence' AND ref_id=$2`, run, tc.uncaptured)
		}
		if result, err = answerRun(run, tc.source, tc.claimsFor, agent.ChatStatement{Kind: "approved_fact", Text: tc.text, SourceSpanIDs: []string{tc.span}}); err != nil || result.ReplyState != "failed" || result.ReasonCode != tc.code {
			t.Fatalf("%s: %+v %v", tc.name, result, err)
		}
		if got := reply(q.Reply.MessageID); got.State != "failed" || got.Body != nil || got.ReasonCode == nil || *got.ReasonCode != tc.code {
			t.Fatalf("%s reply: %+v", tc.name, got)
		}
	}

	// 3. A number that is not in the cited source fails the reply with no content.
	q = ask("How tight are the infeed bolts now?")
	run = runOf(q.Reply)
	if result, err = answerRun(run, sop, "", agent.ChatStatement{Kind: "approved_fact", Text: "Infeed bolts are torqued to 30 Nm.", SourceSpanIDs: []string{approvedSpan}}); err != nil || result.ReplyState != "failed" || result.ReasonCode != agent.CodeNumberNotSourced {
		t.Fatalf("unsourced number: %+v %v", result, err)
	}
	if got := reply(q.Reply.MessageID); got.State != "failed" || got.Body != nil || got.ReasonCode == nil || *got.ReasonCode != agent.CodeNumberNotSourced {
		t.Fatalf("failed reply: %+v", got)
	}

	deactivateLead := func() {
		exec(`UPDATE supervisor.memberships SET active=false WHERE tenant_id=$1 AND site_id=$2 AND subject=$3`, tenant, site, lead)
	}
	revokedReply := func(messageID, label string) {
		t.Helper()
		var state string
		var body, reason sql.NullString
		if e := control.QueryRowContext(ctx, `SELECT state,body,reason_code FROM supervisor.chat_messages WHERE message_id=$1`, messageID).Scan(&state, &body, &reason); e != nil || state != "failed" || body.Valid || !reason.Valid || reason.String != agent.CodeMembershipRevoked {
			t.Errorf("%s: state=%s body=%v reason=%v (%v)", label, state, body, reason, e)
		}
		exec(`UPDATE supervisor.memberships SET active=true WHERE tenant_id=$1 AND site_id=$2 AND subject=$3`, tenant, site, lead)
	}
	readSteps := []agent.FixtureStep{
		{Response: agent.ModelResponse{ToolCalls: []agent.ToolCall{{ID: "read-sop", Name: agent.ToolEvidenceGet, Arguments: []byte(`{"evidence_id":"` + sop + `"}`)}}}},
		{Response: agent.ModelResponse{ToolCalls: []agent.ToolCall{{ID: "claims", Name: agent.ToolClaimsSearch, Arguments: []byte(`{"entity_id":"` + ops[0] + `"}`)}}}},
	}

	// 4. The asker loses access before the run is dispatched: the pre-dispatch recheck refuses the
	// run with the exact code, no model is called and no content is stored.
	q = ask("Which operation comes first?")
	run = runOf(q.Reply)
	deactivateLead()
	provider := agent.NewFixtureProvider(append(append([]agent.FixtureStep{}, readSteps...), answerStep(run, agent.ChatStatement{Kind: "approved_fact", Text: "Bolts are torqued before every shift.", SourceSpanIDs: []string{approvedSpan}}))...)
	if result, err = runtimeFor(provider).ChatAnswer(ctx, agent.ChatAnswerRef{TenantID: tenant, SiteID: site, RunID: run}); err != nil || result.ReasonCode != agent.CodeMembershipRevoked || result.ReplyState != "failed" || provider.Calls() != 0 {
		t.Errorf("pre-dispatch membership recheck: %+v calls=%d %v", result, provider.Calls(), err)
	}
	revokedReply(q.Reply.MessageID, "reply after the asker lost access before dispatch")

	// 5. Membership revoked after after_dispatch but before finish must not store an answer.
	q = ask("Which operation comes first now?")
	run = runOf(q.Reply)
	provider = agent.NewFixtureProvider(append(append([]agent.FixtureStep{}, readSteps...), answerStep(run, agent.ChatStatement{Kind: "approved_fact", Text: "Bolts are torqued before every shift.", SourceSpanIDs: []string{approvedSpan}}))...)
	rt := runtimeFor(provider)
	agent.SetBeforeChatAnswerFinishTx(rt, deactivateLead)
	if result, err = rt.ChatAnswer(ctx, agent.ChatAnswerRef{TenantID: tenant, SiteID: site, RunID: run}); err != nil || result.ReasonCode != agent.CodeMembershipRevoked || result.ReplyState != "failed" || provider.Calls() != 3 {
		t.Errorf("finish-transaction membership recheck: %+v calls=%d %v", result, provider.Calls(), err)
	}
	revokedReply(q.Reply.MessageID, "reply after the asker lost access before finish")

	// Tool membership revocation before and after a read both return the exact denial and no reply body.
	for _, tc := range []struct {
		name       string
		beforeRead bool
	}{
		{"pre_read", true},
		{"post_read", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := ask("Which source can you read?")
			run := runOf(q.Reply)
			provider := agent.NewFixtureProvider(agent.FixtureStep{Response: agent.ModelResponse{ToolCalls: []agent.ToolCall{{
				ID: "read-sop", Name: agent.ToolEvidenceGet, Arguments: []byte(`{"evidence_id":"` + sop + `"}`),
			}}}})
			rt := runtimeFor(provider)
			if tc.beforeRead {
				agent.SetChatToolHooks(rt.Tools, deactivateLead, nil)
			} else {
				agent.SetChatToolHooks(rt.Tools, nil, deactivateLead)
			}
			got, e := rt.ChatAnswer(ctx, agent.ChatAnswerRef{TenantID: tenant, SiteID: site, RunID: run})
			if e != nil || got.ReasonCode != agent.CodeMembershipRevoked || got.ReplyState != "failed" || provider.Calls() != 1 {
				t.Errorf("tool revocation result: %+v calls=%d %v", got, provider.Calls(), e)
			}
			var state string
			var body, reason sql.NullString
			if e = control.QueryRowContext(ctx, `SELECT state,body,reason_code FROM supervisor.chat_messages WHERE message_id=$1`, q.Reply.MessageID).Scan(&state, &body, &reason); e != nil || state != "failed" || body.Valid || !reason.Valid || reason.String != agent.CodeMembershipRevoked {
				t.Errorf("tool revocation reply: state=%s body=%v reason=%v (%v)", state, body, reason, e)
			}
			var toolStatus, toolCode string
			var resultBytes sql.NullInt64
			e = control.QueryRowContext(ctx, `SELECT status,code,result_bytes FROM supervisor.agent_tool_calls WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3 ORDER BY seq LIMIT 1`, tenant, site, run).Scan(&toolStatus, &toolCode, &resultBytes)
			if e != nil || toolStatus != "rejected" || toolCode != agent.CodeMembershipRevoked || (resultBytes.Valid && resultBytes.Int64 != 0) {
				t.Errorf("tool revocation audit: status=%s code=%s result_bytes=%v (%v)", toolStatus, toolCode, resultBytes, e)
			}
			exec(`UPDATE supervisor.memberships SET active=true WHERE tenant_id=$1 AND site_id=$2 AND subject=$3`, tenant, site, lead)
		})
	}

	// 6. The cited source is revoked after the question: the reply ends needs_input, no content.
	q = ask("Are the infeed bolts torqued every shift?")
	run = runOf(q.Reply)
	revoke(sop, "SOP withdrawn")
	if result, err = answerRun(run, sop, "", agent.ChatStatement{Kind: "approved_fact", Text: "Bolts are torqued before every shift.", SourceSpanIDs: []string{approvedSpan}}); err != nil || result.ReplyState != "needs_input" || result.ReasonCode != agent.CodeEvidenceRevoked {
		t.Fatalf("revoked source: %+v %v", result, err)
	}
	if got := reply(q.Reply.MessageID); got.State != "needs_input" || got.Body != nil || len(got.Citations) != 0 {
		t.Fatalf("reply after revocation: %+v", got)
	}
	// WIS-517 post-merge review P1: an answer stored before the revocation quoted the SOP. History
	// and a retry of its request ID must not serve the saved body or citations any more; the reply
	// reads as the needs_input EVIDENCE_REVOKED it would have ended as had the revocation come first.
	redacted := func(label string, got chat.Message) {
		t.Helper()
		if got.State != "needs_input" || got.Body != nil || len(got.Citations) != 0 || got.ReasonCode == nil || *got.ReasonCode != agent.CodeEvidenceRevoked {
			t.Fatalf("%s after revocation still serves the answer: state=%s body=%v citations=%+v reason=%v", label, got.State, got.Body, got.Citations, got.ReasonCode)
		}
	}
	redacted("stored answer in history", reply(answeredQ.Reply.MessageID))
	w = call(leadS, "POST", base+"/"+conv.ConversationID+"/messages", uuid.NewString(), `"`+itoa(revision)+`"`, `{"request_id":"`+*answeredQ.Question.RequestID+`","text":"How tight are the infeed bolts?"}`)
	expect(w, 200)
	var retried chat.PostResult
	if err = json.Unmarshal(w.Body.Bytes(), &retried); err != nil || !retried.Replayed {
		t.Fatalf("request-ID retry: %s %v", w.Body.String(), err)
	}
	redacted("request-ID retry", retried.Reply)
	var runsBefore, runsAfter int
	if err = control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.agent_runs WHERE tenant_id=$1 AND site_id=$2`, tenant, site).Scan(&runsBefore); err != nil {
		t.Fatal(err)
	}
	withdrawn := ask("Can I still rely on the withdrawn SOP?")
	withdrawnReason := ""
	if withdrawn.Reply.ReasonCode != nil {
		withdrawnReason = *withdrawn.Reply.ReasonCode
	}
	if withdrawn.Reply.State != chat.StateUnavailable || withdrawn.Reply.ReasonCode == nil || *withdrawn.Reply.ReasonCode != chat.ReasonSnapshotEvidenceUnavailable || runOf(withdrawn.Reply) != "" {
		t.Fatalf("question against revoked snapshot: state=%s reason=%q run=%q", withdrawn.Reply.State, withdrawnReason, runOf(withdrawn.Reply))
	}
	if err = control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.agent_runs WHERE tenant_id=$1 AND site_id=$2`, tenant, site).Scan(&runsAfter); err != nil || runsAfter != runsBefore {
		t.Fatalf("revoked snapshot started a run: runs before=%d after=%d err=%v", runsBefore, runsAfter, err)
	}

	// WIS-620: a site citing 71 sources, over the old 70-source bound, is answered, not refused. The
	// question captures only the passages of its most relevant approved claims: the two-source
	// oven claim first (four question words), whole, then the one-word oven-door claim, then
	// unmatched claims by claim ID while they fit agent.MaxRunPassages passages.
	var sourceIDs, spanIDs []string
	for i := 0; i < 71; i++ {
		text := fmt.Sprintf("Verified capture limit source %02d.", i)
		switch i {
		case 5:
			text = "The oven door is closed before start."
		case 37:
			text = "The curing oven runs at 180 degrees."
		case 38:
			text = "Oven temperature is logged each hour."
		}
		src, e := ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: fmt.Sprintf("Capture source %02d", i), Text: text}, uuid.NewString(), 0)
		if e != nil {
			t.Fatal(e)
		}
		sp, e := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: src.SourceID, Revision: 1, Start: 0, End: len(text)}, uuid.NewString(), 1)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = ev.DecideAuthority(ctx, ea, es, src.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Owner verified process source"}, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		sourceIDs, spanIDs = append(sourceIDs, src.SourceID), append(spanIDs, sp.ID)
	}
	// Approved claims, all from operation 0 so no chain reaches the cycle-search bound: four
	// one-source claims cite sources 0-3, a two-source claim sources 4 and 6, the oven claim
	// sources 37 and 38 together, the oven-door claim source 5, and four 16-span claims every other
	// source. Claim IDs ascend in this order, so only the question ranking can put the oven claims
	// first. Seven passages are taken before the two-source claim, which would need a ninth: it is
	// passed over whole, never split, and so is every 16-span claim.
	groups := [][]int{{0}, {1}, {2}, {3}, {4, 6}, {37, 38}, {5}}
	var rest []int
	for i := 7; i < 71; i++ {
		if i != 37 && i != 38 {
			rest = append(rest, i)
		}
	}
	for len(rest) > 0 {
		n := min(16, len(rest))
		groups, rest = append(groups, rest[:n]), rest[n:]
	}
	entities := make([]string, len(groups)+1)
	for i := range entities {
		entities[i] = uuid.NewString()
		if _, e := ks.CreateEntity(ctx, ka, sc, knowledge.EntityInput{EntityID: entities[i], Type: "operation", Name: fmt.Sprintf("Capture operation %d", i), SourceID: sourceIDs[i], SourceRevision: 1}, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
	}
	claimIDs := make([]string, len(groups))
	for g := range claimIDs {
		claimIDs[g] = uuid.NewString()
	}
	slices.Sort(claimIDs)
	for g, members := range groups {
		var cited []string
		for _, i := range members {
			cited = append(cited, spanIDs[i])
		}
		claim := knowledge.ClaimInput{ClaimID: claimIDs[g], SubjectID: entities[0], SubjectRevision: 1, Predicate: "precedes", ObjectEntityID: entities[g+1], ObjectRevision: 1,
			EffectiveFrom: "2026-10-28T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: cited}
		if _, e := ks.CreateClaim(ctx, ka, sc, claim, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		if _, e := ks.DecideReview(ctx, ka, sc, claim.ClaimID, knowledge.ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Verified capture source set"}, uuid.NewString(), 1); e != nil {
			t.Fatal(e)
		}
	}
	inventory, err = ks.ReadInventory(ctx, ka, sc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ks.CreateSnapshot(ctx, ka, sc, knowledge.SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Reviewed large source set"}, uuid.NewString(), inventory.Revision); err != nil {
		t.Fatal(err)
	}
	q = ask("What temperature does the curing oven run at?")
	run = runOf(q.Reply)
	if q.Reply.State != chat.StatePending || q.Reply.ReasonCode != nil || run == "" {
		t.Fatalf("large site was not answered: %+v run=%s", q.Reply, run)
	}
	var passages []string
	rows, err := control.QueryContext(ctx, `SELECT span_id::text FROM supervisor.agent_run_spans WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3 ORDER BY ordinal`, tenant, site, run)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		passages = append(passages, id)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	// A claim's spans keep the snapshot's order, which sorts them by ID.
	oven := []string{spanIDs[37], spanIDs[38]}
	slices.Sort(oven)
	if want := append(oven, spanIDs[5], spanIDs[0], spanIDs[1], spanIDs[2], spanIDs[3]); !slices.Equal(passages, want) {
		t.Fatalf("captured passages %v, want %v", passages, want)
	}
	var evidenceRefs int
	if err = control.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.agent_run_refs WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3 AND kind='evidence'`, tenant, site, run).Scan(&evidenceRefs); err != nil || evidenceRefs != len(passages) {
		t.Fatalf("captured evidence refs %d, want one per passage source (%v)", evidenceRefs, err)
	}
	// The oven fact stays approved because both of its claim's sources were captured.
	result, err = answerRun(run, sourceIDs[37], "", agent.ChatStatement{Kind: "approved_fact", Text: "The curing oven runs at 180 degrees.", SourceSpanIDs: []string{spanIDs[37]}})
	if err != nil || result.ReplyState != "answered" || result.ReasonCode != "" {
		t.Fatalf("large-site answer: %+v %v", result, err)
	}
	if got := reply(q.Reply.MessageID); got.State != "answered" || len(got.Citations) != 1 || got.Citations[0].ID != spanIDs[37] {
		t.Fatalf("large-site reply: %+v", got)
	}

	// WIS-517 review (Codex, 35ccd69c P1): approval is re-read when the answer commits. One approved
	// text-literal claim; a first answer restating it is approved, then the reviewer withdraws the
	// approval while the model answers a second question, which the run had loaded as approved.
	greaseText := "Grease the outfeed rollers every week."
	greaseSource, err := ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Outfeed SOP", Text: greaseText}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	greaseSpan, err := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: greaseSource.SourceID, Revision: 1, Start: 0, End: len(greaseText)}, uuid.NewString(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ev.DecideAuthority(ctx, ea, es, greaseSource.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Owner verified outfeed SOP"}, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	grease := knowledge.ClaimInput{ClaimID: uuid.NewString(), SubjectID: entities[0], SubjectRevision: 1, Predicate: "actual_step", Literal: &knowledge.Literal{Type: "string", Value: json.RawMessage(`"` + greaseText + `"`)},
		EffectiveFrom: "2026-10-28T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{greaseSpan.ID}}
	if _, err = ks.CreateClaim(ctx, ka, sc, grease, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	greaseReview, err := ks.DecideReview(ctx, ka, sc, grease.ClaimID, knowledge.ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Verified outfeed step"}, uuid.NewString(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if inventory, err = ks.ReadInventory(ctx, ka, sc); err != nil {
		t.Fatal(err)
	}
	if _, err = ks.CreateSnapshot(ctx, ka, sc, knowledge.SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Reviewed outfeed step"}, uuid.NewString(), inventory.Revision); err != nil {
		t.Fatal(err)
	}
	greaseAnswer := func(withdraw bool) (chat.PostResult, agent.ChatAnswerResult, error) {
		t.Helper()
		q := ask("How often are the outfeed rollers greased?")
		run := runOf(q.Reply)
		step := answerStep(run, agent.ChatStatement{Kind: "approved_fact", Text: greaseText, SourceSpanIDs: []string{greaseSpan.ID}})
		if withdraw {
			step.OnRequest = func(agent.ModelRequest) {
				if _, e := ks.RevokeReview(ctx, ka, sc, grease.ClaimID, knowledge.RevocationInput{ClaimRevision: 1, ReviewID: greaseReview.ReviewID, Reason: "Step under review again"}, uuid.NewString(), 1); e != nil {
					t.Error(e)
				}
			}
		}
		p := agent.NewFixtureProvider(agent.FixtureStep{Response: agent.ModelResponse{ToolCalls: []agent.ToolCall{{ID: "read", Name: agent.ToolEvidenceGet, Arguments: []byte(`{"evidence_id":"` + greaseSource.SourceID + `"}`)}}}}, step)
		result, e := runtimeFor(p).ChatAnswer(ctx, agent.ChatAnswerRef{TenantID: tenant, SiteID: site, RunID: run})
		return q, result, e
	}
	greased, result, err := greaseAnswer(false)
	if got := reply(greased.Reply.MessageID); err != nil || result.ReplyState != "answered" || got.Body == nil || *got.Body != "- [Approved process] "+greaseText+" [1]" {
		t.Fatalf("approved outfeed step: %+v %v body=%v", result, err, got.Body)
	}
	if _, result, err = greaseAnswer(true); err != nil || result.ReplyState != "failed" || result.ReasonCode != agent.CodeFactNotApproved {
		t.Fatalf("approved_fact after a withdrawal during inference: %+v %v", result, err)
	}

	// WIS-699: after a tenant is deactivated, the answer activity refuses before any model call,
	// and the finalizer still closes the run, refused, with the reply failed. The finalizer skips
	// the active-tenant check on purpose: it only moves a running run to a terminal state (no
	// model call, no source read, no content), and refusing here would leave the run running and
	// the reply pending forever.
	stuck := ask("How often are the outfeed rollers greased?")
	stuckRun := runOf(stuck.Reply)
	if stuckRun == "" {
		t.Fatalf("no run for the deactivation case: %+v", stuck.Reply)
	}
	exec(`UPDATE supervisor.tenants SET deactivated_at=clock_timestamp() WHERE id=$1`, tenant)
	noModel := agent.NewFixtureProvider()
	acts := &dispatch.Activities{Store: db, Agent: runtimeFor(noModel)}
	stuckRef := dispatch.ChatAnswerRef{Tenant: tenant, Site: site, Run: stuckRun}
	if _, err := acts.RunChatAnswer(ctx, stuckRef); err == nil || !strings.Contains(err.Error(), "unprovisioned scope") || noModel.Calls() != 0 {
		t.Fatalf("answer for a deactivated tenant: err=%v model calls=%d", err, noModel.Calls())
	}
	finalized, err := acts.FinalizeChatAnswerFailure(ctx, stuckRef)
	if err != nil || finalized.ReplyState != "failed" || finalized.ReasonCode != agent.CodeChatActivity {
		t.Fatalf("finalizer for a deactivated tenant: %+v %v", finalized, err)
	}
	var stuckState, stuckReply string
	if err = control.QueryRowContext(ctx, `SELECT r.state,m.state FROM supervisor.agent_runs r JOIN supervisor.chat_messages m ON (m.tenant_id,m.site_id,m.message_id)=(r.tenant_id,r.site_id,$3::uuid) WHERE r.tenant_id=$1 AND r.id=$2`, tenant, stuckRun, stuck.Reply.MessageID).Scan(&stuckState, &stuckReply); err != nil ||
		stuckState != agent.RunCompleted || stuckReply != "failed" || noModel.Calls() != 0 {
		t.Fatalf("deactivated tenant's run %q reply %q calls %d (%v), want completed, failed, 0", stuckState, stuckReply, noModel.Calls(), err)
	}
	exec(`UPDATE supervisor.tenants SET deactivated_at=NULL WHERE id=$1`, tenant)
	// WIS-640: a statement may list the quantities it states. The server resolves each to the one
	// standing approved claim at the cited span with exactly that value, and the finished reply
	// carries the claim's own value and unit. A quantity that does not resolve refuses the answer.
	pressText := "The press takes 45 seconds a part."
	pressSource, pressSpans := addSource("Press SOP", pressText, [2]int{0, len(pressText)})
	press := uuid.NewString()
	if _, err = ks.CreateEntity(ctx, ka, sc, knowledge.EntityInput{EntityID: press, Type: "operation", Name: "Press", SourceID: pressSource, SourceRevision: 1}, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	pressClaim := knowledge.ClaimInput{ClaimID: uuid.NewString(), SubjectID: press, SubjectRevision: 1, Predicate: "cycle_time_seconds",
		Literal: &knowledge.Literal{Type: "decimal", Value: json.RawMessage(`"45"`), Unit: "s"}, EffectiveFrom: "2026-09-27T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: pressSpans}
	if _, err = ks.CreateClaim(ctx, ka, sc, pressClaim, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err = ks.DecideReview(ctx, ka, sc, pressClaim.ClaimID, knowledge.ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Verified press cycle"}, uuid.NewString(), 1); err != nil {
		t.Fatal(err)
	}
	if inventory, err = ks.ReadInventory(ctx, ka, sc); err != nil {
		t.Fatal(err)
	}
	if _, err = ks.CreateSnapshot(ctx, ka, sc, knowledge.SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Reviewed press cycle"}, uuid.NewString(), inventory.Revision); err != nil {
		t.Fatal(err)
	}
	pressQ := ask("How long does the press take a part?")
	stated := agent.ChatStatement{Kind: "approved_fact", Text: pressText, SourceSpanIDs: pressSpans, Quantities: []agent.ChatQuantity{{SpanID: pressSpans[0], Value: "45"}}}
	if result, err = answerRun(runOf(pressQ.Reply), pressSource, "", stated); err != nil || result.ReplyState != "answered" {
		t.Fatalf("answer with a quantity: %+v %v", result, err)
	}
	// Read back as the asker sees it, after the answer finished (not the pending reply).
	if got := reply(pressQ.Reply.MessageID); len(got.Quantities) != 1 || got.Quantities[0] != (agent.ChatAnswerQuantity{Statement: 0, ClaimID: pressClaim.ClaimID, Predicate: "cycle_time_seconds", Value: "45", Unit: "s", SpanID: pressSpans[0]}) {
		t.Fatalf("finished reply quantities: %+v", got.Quantities)
	}
	// Negative control: a hypothesis never carries an approved value; the whole answer is refused.
	wrongQ := ask("How long does the press take a part?")
	hypothesis := stated
	hypothesis.Kind = "hypothesis"
	if result, err = answerRun(runOf(wrongQ.Reply), pressSource, "", hypothesis); err != nil || result.ReplyState != "failed" || result.ReasonCode != agent.CodeQuantityNotApproved {
		t.Fatalf("quantity in a hypothesis: %+v %v", result, err)
	}
	if got := reply(wrongQ.Reply.MessageID); got.State != "failed" || got.Quantities != nil {
		t.Fatalf("refused reply: %+v", got)
	}
	// WIS-631: a chat answer's model calls get the runtime's configured per-call deadline
	// (SUPERVISOR_AGENT_TIMEOUT, as the worker sets it), not a fixed 30 s: a slower local model
	// could otherwise never finish a call. Control: the default limits still give 30 s.
	for _, c := range []struct {
		callTimeout time.Duration
		atLeast     time.Duration
		atMost      time.Duration
	}{{3 * time.Minute, 2 * time.Minute, 3 * time.Minute}, {0, 0, 30 * time.Second}} {
		q := ask("How often are the outfeed rollers greased?")
		run := runOf(q.Reply)
		rec := &deadlineRecorder{Provider: agent.NewFixtureProvider(answerStep(run, agent.ChatStatement{Kind: "reported_observation", Text: greaseText, SourceSpanIDs: []string{greaseSpan.ID}}))}
		rt := runtimeFor(rec)
		if c.callTimeout > 0 {
			rt.Limits.CallTimeout = c.callTimeout
		}
		if _, err := rt.ChatAnswer(ctx, agent.ChatAnswerRef{TenantID: tenant, SiteID: site, RunID: run}); err != nil {
			t.Fatal(err)
		}
		if rec.remaining < c.atLeast || rec.remaining > c.atMost {
			t.Fatalf("configured call timeout %s: the model call had %s left, want between %s and %s", c.callTimeout, rec.remaining, c.atLeast, c.atMost)
		}
	}
	// Rule 06 (review of #289), after the steps that still ask questions: once the cited source is revoked, the finished reply reads as
	// EVIDENCE_REVOKED and returns neither its body nor the quantities drawn from that evidence.
	revoke(pressSource, "Press SOP withdrawn")
	if got := reply(pressQ.Reply.MessageID); got.State != "needs_input" || got.Body != nil || got.Quantities != nil || got.ReasonCode == nil || *got.ReasonCode != agent.CodeEvidenceRevoked {
		t.Fatalf("reply after its source was revoked: state=%s body=%v quantities=%+v", got.State, got.Body, got.Quantities)
	}

	// WIS-626 rule 03: a claim citing another tenant's span, even with its exact ID, is never
	// ranked. Under forced RLS the indexed search row of tenant B is invisible to tenant A, so the
	// claim is passed over whole and only tenant A's matching claim is selected.
	foreignTenant, foreignSite, foreignSource, foreignSpan := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	foreignText := "The curing oven runs at 180 degrees in tenant B."
	sum := sha256.Sum256([]byte(foreignText))
	seedForeign, err := control.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		q    string
		args []any
	}{
		{`SELECT set_config('supervisor.tenant_id',$1,true)`, []any{foreignTenant}},
		{`INSERT INTO supervisor.tenants(id,name) VALUES($1,'foreign search tenant')`, []any{foreignTenant}},
		{`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'foreign site')`, []any{foreignTenant, foreignSite}},
		{`INSERT INTO supervisor.evidence_heads(tenant_id,site_id,source_id,current_revision,state_revision) VALUES($1,$2,$3,1,1)`, []any{foreignTenant, foreignSite, foreignSource}},
		{`INSERT INTO supervisor.evidence_revisions(tenant_id,site_id,source_id,revision,kind,title,body,sha256,byte_length,rune_length,actor_issuer,actor_subject) VALUES($1,$2,$3,1,'manual','B',$4,$5,$6,$6,'i','b')`,
			[]any{foreignTenant, foreignSite, foreignSource, foreignText, hex.EncodeToString(sum[:]), len(foreignText)}},
		{`INSERT INTO supervisor.evidence_spans(tenant_id,site_id,id,source_id,revision,start_rune,end_rune,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,1,0,$5,'i','b')`,
			[]any{foreignTenant, foreignSite, foreignSpan, foreignSource, len(foreignText)}},
	} {
		if _, err = seedForeign.ExecContext(ctx, step.q, step.args...); err != nil {
			t.Fatalf("seed foreign tenant: %v", err)
		}
	}
	if err = seedForeign.Commit(); err != nil {
		t.Fatal(err)
	}
	rank, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rank.Rollback()
	if _, err = rank.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		t.Fatal(err)
	}
	claims := []knowledge.SnapshotClaim{
		{ClaimInput: knowledge.ClaimInput{ClaimID: "00000000-0000-4000-8000-000000000001", SourceSpanIDs: []string{foreignSpan}}},
		{ClaimInput: knowledge.ClaimInput{ClaimID: "00000000-0000-4000-8000-000000000002", SourceSpanIDs: []string{spanIDs[37]}}},
	}
	picked, err := chat.SelectPassagesUsing(ctx, rank, chat.Scope{TenantID: tenant, SiteID: site}, claims, nil, "What temperature does the curing oven run at?", chat.RankSQL)
	if err != nil || len(picked) != 1 || picked[0].SpanID != spanIDs[37] {
		t.Fatalf("foreign span ranked: %+v %v", picked, err)
	}
	// WIS-646: a claim is also ranked by its own terms, the predicate's words and the snapshot's
	// names for its subject and object. The cabinet's cycle-time claim cites a span with none of the
	// question's words ("Verified capture limit source 10.") and its ID sorts after nine claims that
	// tie with it on span words, so word-of-span ranking alone fills the eight passages without it.
	cabinet := knowledge.SnapshotEntity{EntityInput: knowledge.EntityInput{EntityID: uuid.NewString(), Type: "operation", Name: "Shot blast cabinet"}}
	other := knowledge.SnapshotEntity{EntityInput: knowledge.EntityInput{EntityID: uuid.NewString(), Type: "operation", Name: "Capture operation"}}
	termClaims := []knowledge.SnapshotClaim{{ClaimInput: knowledge.ClaimInput{ClaimID: "ffffffff-0000-4000-8000-000000000001", SubjectID: cabinet.EntityID, Predicate: "cycle_time_seconds", SourceSpanIDs: []string{spanIDs[10]}}}}
	for i := 11; i < 20; i++ {
		termClaims = append(termClaims, knowledge.SnapshotClaim{ClaimInput: knowledge.ClaimInput{ClaimID: fmt.Sprintf("00000000-0000-4000-8000-0000000000%02d", i), SubjectID: other.EntityID, Predicate: "precedes", ObjectEntityID: other.EntityID, SourceSpanIDs: []string{spanIDs[i]}}})
	}
	scope := chat.Scope{TenantID: tenant, SiteID: site}
	firstIs := func(question string, entities []knowledge.SnapshotEntity) bool {
		t.Helper()
		picked, err := chat.SelectPassagesUsing(ctx, rank, scope, termClaims, entities, question, chat.RankSQL)
		if err != nil {
			t.Fatal(err)
		}
		return len(picked) > 0 && picked[0].SpanID == spanIDs[10]
	}
	both := []knowledge.SnapshotEntity{cabinet, other}
	// By the predicate's words ("cycle_time_seconds").
	if !firstIs("What is the cycle time?", both) {
		t.Fatal("a claim named by its predicate's words was not ranked first")
	}
	// By the subject's snapshot name.
	if !firstIs("Tell me about the shot blast cabinet.", both) {
		t.Fatal("a claim named by its subject was not ranked first")
	}
	// Control: names come only from the snapshot entities given. Without the cabinet's entity the
	// same claim has no question word, loses every tie, and is not selected at all.
	if picked, err := chat.SelectPassagesUsing(ctx, rank, scope, termClaims, []knowledge.SnapshotEntity{other}, "Tell me about the shot blast cabinet.", chat.RankSQL); err != nil ||
		slices.ContainsFunc(picked, func(p agent.RunPassage) bool { return p.SpanID == spanIDs[10] }) {
		t.Fatalf("control: a name the snapshot does not hold ranked the claim: %+v %v", picked, err)
	}
	// WIS-646: a word weighs by how few claims hold it. "Fasteners: supplier and lead time?": eight
	// lead-time claims all hold the generic "lead" and "time" (their predicate's words), and fill the
	// eight passages under plain word counting. Only two claims hold "fasteners", so the fasteners'
	// supplier claim, whose ID sorts last, must still be selected.
	fasteners := knowledge.SnapshotEntity{EntityInput: knowledge.EntityInput{EntityID: uuid.NewString(), Type: "material", Name: "Fasteners"}}
	vendor := knowledge.SnapshotEntity{EntityInput: knowledge.EntityInput{EntityID: uuid.NewString(), Type: "supplier", Name: "Bhima Works"}}
	leadClaims := []knowledge.SnapshotClaim{{ClaimInput: knowledge.ClaimInput{ClaimID: "00000000-0000-4000-8000-000000000020", SubjectID: fasteners.EntityID, Predicate: "lead_time_days", SourceSpanIDs: []string{spanIDs[20]}}}}
	leadEntities := []knowledge.SnapshotEntity{fasteners, vendor}
	for k := 1; k <= 7; k++ {
		mat := knowledge.SnapshotEntity{EntityInput: knowledge.EntityInput{EntityID: uuid.NewString(), Type: "material", Name: fmt.Sprintf("Material %d", k)}}
		leadEntities = append(leadEntities, mat)
		leadClaims = append(leadClaims, knowledge.SnapshotClaim{ClaimInput: knowledge.ClaimInput{ClaimID: fmt.Sprintf("00000000-0000-4000-8000-0000000000%02d", 20+k), SubjectID: mat.EntityID, Predicate: "lead_time_days", SourceSpanIDs: []string{spanIDs[20+k]}}})
	}
	leadClaims = append(leadClaims, knowledge.SnapshotClaim{ClaimInput: knowledge.ClaimInput{ClaimID: "ffffffff-0000-4000-8000-000000000002", SubjectID: fasteners.EntityID, Predicate: "supplied_by", ObjectEntityID: vendor.EntityID, SourceSpanIDs: []string{spanIDs[28]}}})
	picked, err = chat.SelectPassagesUsing(ctx, rank, scope, leadClaims, leadEntities, "Fasteners: supplier and lead time?", chat.RankSQL)
	if err != nil || len(picked) < 2 || picked[0].SpanID != spanIDs[20] || picked[1].SpanID != spanIDs[28] {
		t.Fatalf("rare word did not outweigh shared generic words: %+v %v", picked, err)
	}
	// Control: in tenant B's own scope the same claim is ranked.
	if _, err = rank.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, foreignTenant); err != nil {
		t.Fatal(err)
	}
	picked, err = chat.SelectPassagesUsing(ctx, rank, chat.Scope{TenantID: foreignTenant, SiteID: foreignSite}, claims[:1], nil, "What temperature does the curing oven run at?", chat.RankSQL)
	if err != nil || len(picked) != 1 || picked[0].SpanID != foreignSpan {
		t.Fatalf("control: tenant B's own span not ranked: %+v %v", picked, err)
	}
	_ = rank.Rollback()

	// WIS-517 rule 03: CAPTURE_LIMIT must not disclose over-budget claims the asker cannot read.
	// Every member reads every unrevoked source today and a revoked one kills the snapshot, so
	// inject the case: revoke the plant model source and make newest a copy of the oversized
	// snapshot that no longer lists it. The claim is still ranked and over budget, but its source
	// reads as not found, so the reply is NO_READABLE_EVIDENCE.
	revoke(big.SourceID, "Plant model withheld")
	forge, err := control.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer forge.Rollback()
	if _, err = forge.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err = forge.ExecContext(ctx, `INSERT INTO supervisor.knowledge_process_snapshots(tenant_id,site_id,snapshot_id,inventory_revision,sha256,canonical_contents,reason,actor_issuer,actor_subject,auth_time)
SELECT tenant_id,site_id,$3,inventory_revision,encode(sha256(convert_to(c,'UTF8')),'hex'),c,'Plant model source withheld',actor_issuer,actor_subject,clock_timestamp()
FROM (SELECT p.*,jsonb_set(p.canonical_contents::jsonb,'{sources}',(SELECT coalesce(jsonb_agg(s),'[]'::jsonb) FROM jsonb_array_elements(p.canonical_contents::jsonb->'sources') s WHERE s->>'source_id'<>$4))::text AS c
 FROM supervisor.knowledge_process_snapshots p WHERE p.tenant_id=$1 AND p.site_id=$2 AND p.snapshot_id=$5) x`, tenant, site, uuid.NewString(), big.SourceID, bigSnapshot); err != nil {
		t.Fatal(err)
	}
	if err = forge.Commit(); err != nil {
		t.Fatal(err)
	}
	hidden := ask("What does the plant model say now?")
	if hidden.Reply.State != chat.StateUnavailable || hidden.Reply.ReasonCode == nil || *hidden.Reply.ReasonCode != chat.ReasonNoReadableEvidence || runOf(hidden.Reply) != "" {
		t.Errorf("unreadable oversized claim: %+v run=%s", hidden.Reply, runOf(hidden.Reply))
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// deadlineRecorder records how long the first model call had before its deadline.
type deadlineRecorder struct {
	agent.Provider
	remaining time.Duration
	seen      bool
}

func (d *deadlineRecorder) Complete(ctx context.Context, r agent.ModelRequest) (agent.ModelResponse, error) {
	if dl, ok := ctx.Deadline(); ok && !d.seen {
		d.remaining, d.seen = time.Until(dl), true
	}
	return d.Provider.Complete(ctx, r)
}
