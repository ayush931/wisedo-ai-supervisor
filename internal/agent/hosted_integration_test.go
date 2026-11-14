//go:build integration

package agent_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/approvals"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/modelhosting"
	"github.com/google/uuid"
)

// TestWIS623HostedRoutePinsAndRechecks drives the runtime's real per-call path against a
// stand-in HTTPS endpoint (the compiled allowlist is not reachable from a test):
//   - with the site's switch off a run stays on the default provider and nothing reaches the
//     hosted endpoint;
//   - an owner turns it on: a new run is pinned (provider, region, model, revision and price
//     version recorded), its call is priced and charged, and the outbound body carries no name
//     the system knows; the response is restored from that request's own tokens;
//   - turning the switch off stops that pinned run at its next call (HOSTED_MODEL_DISABLED),
//     never falling back to the default provider;
//   - a pinned run in a process without its route is HOSTED_MODEL_UNAVAILABLE, and a route with
//     no price is PRICE_MISSING; neither reaches the endpoint.
func TestWIS623HostedRoutePinsAndRechecks(t *testing.T) {
	w := newWorld(t)
	// Names the system knows: a member's display name, and the site's own name.
	w.exec(t, `INSERT INTO supervisor.member_profiles(tenant_id,issuer,subject,display_name) VALUES($1,$2,$3,'Rahul Deshmukh')`, w.tenant, w.reviewer.Issuer, w.reviewer.Subject)

	var mu sync.Mutex
	var wire []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		wire = append(wire, string(b))
		mu.Unlock()
		tokens := regexp.MustCompile(`⟦P[0-9]+⟧`).FindAllString(string(b), -1)
		content, _ := json.Marshal(`{"named":"` + strings.Join(tokens, " ") + `"}`)
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"model":"qwen-plus-2025-09-11","choices":[{"message":{"content":` + string(content) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":100}}`))
	}))
	t.Cleanup(srv.Close)
	calls := func() int { mu.Lock(); defer mu.Unlock(); return len(wire) }
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("sk-test"), 0o600); err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse(srv.URL)
	hosted := config.HostedAgent{Provider: "qwen-intl", Model: "qwen-plus-2025-09-11", ModelRevision: "qwen-plus-2025-09-11", APIKeyFile: key}
	m, err := agent.NewHostedModelForTest(hosted, base, srv.Client().Transport, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	price, version, ok := agent.HostedPrice(hosted.Provider, hosted.Model, hosted.ModelRevision)
	if !ok {
		t.Fatal("no committed price")
	}
	route := &agent.HostedRoute{Provider: m, Name: "hosted-qwen-intl", Region: "ap-southeast-1", Model: hosted.Model, ModelRevision: hosted.ModelRevision, Price: &price, PriceVersion: version}
	agent.SetHostedRoute(route)
	t.Cleanup(func() { agent.SetHostedRoute(nil) })

	sc := agent.RunScope{TenantID: w.tenant, SiteID: w.site}
	newRun := func() (agent.Run, []agent.RunRef) {
		t.Helper()
		revision := w.fresh(t) // one run per case revision
		run := agent.Run{ID: uuid.NewString(), RequestKind: agent.RequestCase, CaseID: w.caseID, CaseRevision: revision, TraceID: agent.NewTraceID(),
			ProviderName: agent.ProviderFixture, Model: "fixture-model", ModelRevision: "sha256:fixture", PromptVersion: agent.PromptVersion, ToolVersion: agent.ToolVersion, PolicyRevision: approvals.PolicyRevision}
		var refs []agent.RunRef
		if err := w.store.InTx(w.ctx, sc, func(tx *sql.Tx) error {
			in, e := agent.LoadCaseInput(w.ctx, tx, sc, w.caseID, agent.LoaderOptions{ModeEnabled: true})
			if e != nil {
				return e
			}
			if e = agent.CreateRunTx(w.ctx, tx, sc, run, agent.DefaultLimits()); e != nil {
				return e
			}
			refs = in.Refs
			return agent.AddRefsTx(w.ctx, tx, sc, run.ID, in.Refs)
		}); err != nil {
			t.Fatal(err)
		}
		return run, refs
	}
	stored := func(id string) agent.Run {
		t.Helper()
		var r agent.Run
		if err := w.store.InTx(w.ctx, sc, func(tx *sql.Tx) error { var e error; r, e = agent.GetRunTx(w.ctx, tx, sc, id); return e }); err != nil {
			t.Fatal(err)
		}
		return r
	}
	charged := func(id string) (n int, micro int64) {
		t.Helper()
		if err := w.control.QueryRowContext(w.ctx, `SELECT count(*),COALESCE(sum(charged_microusd),0) FROM supervisor.agent_model_calls WHERE tenant_id=$1 AND run_id=$2`, w.tenant, id).Scan(&n, &micro); err != nil {
			t.Fatal(err)
		}
		return
	}
	req := agent.ModelRequest{SchemaVersion: agent.ModelSchemaVersion, PromptVersion: agent.PromptVersion, ToolVersion: agent.ToolVersion, MaxOutputTokens: 200,
		Messages: []agent.Message{{Role: "system", Content: "You assist agent site."}, {Role: "user", Content: "Rahul Deshmukh (rahul.d@plant.example) reported the press stopped."}}}
	fixture := agent.NewFixtureProvider(agent.FixtureStep{Response: agent.ModelResponse{Content: `{"named":"fixture"}`}})
	rt := w.runtime(fixture, nil)
	call := func(run agent.Run, refs []agent.RunRef, seq int) (agent.ModelResponse, error, error) {
		r := req
		r.Model = rt.Model
		return rt.CallForTest(w.ctx, sc, run, refs, r, seq)
	}

	// Switch off (never decided): the run stays on the default provider; nothing is sent out.
	local, localRefs := newRun()
	if got := stored(local.ID); got.ProviderName != agent.ProviderFixture || got.PriceVersion != "" || got.ProviderRegion != "" {
		t.Fatalf("run pinned while the switch is off: %+v", got)
	}
	if resp, perr, err := call(local, localRefs, 1); err != nil || perr != nil || resp.Content != `{"named":"fixture"}` || calls() != 0 {
		t.Fatalf("default-provider call: %+v %v %v, hosted calls %d", resp, perr, err, calls())
	}
	if n, micro := charged(local.ID); n != 1 || micro != 0 {
		t.Fatalf("default-provider charge: %d calls, %d micro-USD", n, micro)
	}

	// An owner switches hosting on, with a DPA reference.
	owner := modelhosting.Principal{Issuer: w.ea.Issuer, Subject: w.ea.Subject, SessionHash: w.ea.SessionHash}
	hosting := modelhosting.New(w.runtimeDB)
	ms := modelhosting.Scope{TenantID: w.tenant, SiteID: w.site}
	on, err := hosting.Decide(w.ctx, owner, ms, modelhosting.DecisionInput{Enabled: true, DPAReference: "DPA-2026-11-05", Reason: "Pilot on hosted Qwen"}, uuid.NewString(), 0)
	if err != nil || !on.Enabled || on.Revision != 1 {
		t.Fatalf("switch on: %+v %v", on, err)
	}
	pinned, pinnedRefs := newRun()
	got := stored(pinned.ID)
	if got.ProviderName != "hosted-qwen-intl" || !got.ProviderActual || got.Model != hosted.Model || got.ModelRevision != hosted.ModelRevision || got.ProviderRegion != "ap-southeast-1" || got.PriceVersion != version {
		t.Fatalf("hosted pin: %+v", got)
	}
	resp, perr, err := call(pinned, pinnedRefs, 1)
	if err != nil || perr != nil || resp.Content != `{"named":"agent site Rahul Deshmukh rahul.d@plant.example"}` || calls() != 1 {
		t.Fatalf("hosted call: %+v %v %v, %d calls", resp, perr, err, calls())
	}
	for _, raw := range []string{"Rahul Deshmukh", "rahul.d@plant.example", "agent site"} {
		if strings.Contains(wire[0], raw) {
			t.Fatalf("%q reached the hosted endpoint: %s", raw, wire[0])
		}
	}
	if !strings.Contains(wire[0], `"model":"qwen-plus-2025-09-11"`) {
		t.Fatalf("the pinned model was not sent: %s", wire[0])
	}
	// 1000 input and 100 output tokens at the committed price, rounded up.
	want := (1000*price.InputMicroUSDPerMillion + 100*price.OutputMicroUSDPerMillion + 999_999) / 1_000_000
	if n, micro := charged(pinned.ID); n != 1 || micro != want {
		t.Fatalf("hosted charge: %d calls, %d micro-USD, want %d", n, micro, want)
	}

	// Review finding 4: onboarding interview, configuration-draft and chat runs are created with
	// InterviewLimits (a local, zero-cost ceiling). Pinned to the paid route they used to keep that
	// zero ceiling, so their first priced reservation failed. All three entry points create their
	// run through CreateRunTx with InterviewLimits, which this drives.
	conversation := agent.Run{ID: uuid.NewString(), RequestKind: agent.RequestConversation, ConversationID: uuid.NewString(), RequestID: uuid.NewString(),
		TraceID: agent.NewTraceID(), ProviderName: agent.ProviderFixture, Model: "fixture-model", ModelRevision: "sha256:fixture", PromptVersion: agent.PromptVersion,
		ToolVersion: agent.ToolVersion, PolicyRevision: approvals.PolicyRevision}
	if err = w.store.InTx(w.ctx, sc, func(tx *sql.Tx) error { return agent.CreateRunTx(w.ctx, tx, sc, conversation, agent.InterviewLimits()) }); err != nil {
		t.Fatal(err)
	}
	il := agent.InterviewLimits()
	maxPrice := max(price.InputMicroUSDPerMillion, price.OutputMicroUSDPerMillion)
	ceiling := (il.Tokens*maxPrice+999_999)/1_000_000 + int64(il.Requests)
	if got := stored(conversation.ID); got.ProviderName != "hosted-qwen-intl" || got.CostLeftMicroUSD != ceiling {
		t.Fatalf("conversation run on the hosted route: %+v, want cost ceiling %d", got, ceiling)
	}
	for i := 1; i <= il.Requests; i++ { // every request the token budget allows can be priced
		if _, err = agent.ReserveRequest(w.ctx, w.store, sc, conversation.ID, il, agent.RequestPlan{Seq: i, Attempt: 1, SpanID: agent.NewSpanID(), ProviderActual: true,
			Model: hosted.Model, ModelRevision: hosted.ModelRevision, RequestSHA256: strings.Repeat("ab", 32), RequestBytes: 3000, Pricing: agent.Pricing{Price: &price}}); err != nil {
			t.Fatalf("priced reservation %d on a hosted conversation run: %v", i, err)
		}
	}

	// The owner turns it off: the pinned run's next call is refused before dispatch, and it is
	// never served by the default provider instead.
	if _, err = hosting.Decide(w.ctx, owner, ms, modelhosting.DecisionInput{Enabled: false, Reason: "Pause hosted inference"}, uuid.NewString(), 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err = call(pinned, pinnedRefs, 2); agent.PreDispatchCode(err) != agent.CodeHostedDisabled || calls() != 1 || fixture.Calls() != 1 {
		t.Fatalf("switched off mid-run: %v, hosted %d, default %d", err, calls(), fixture.Calls())
	}

	// Back on; a process without the route cannot serve a pinned run, and a route without a
	// price refuses before dispatch.
	if _, err = hosting.Decide(w.ctx, owner, ms, modelhosting.DecisionInput{Enabled: true, DPAReference: "DPA-2026-11-05", Reason: "Resume"}, uuid.NewString(), 2); err != nil {
		t.Fatal(err)
	}
	agent.SetHostedRoute(nil)
	if _, _, err = call(pinned, pinnedRefs, 3); agent.PreDispatchCode(err) != agent.CodeHostedUnavailable || calls() != 1 {
		t.Fatalf("no route: %v, %d calls", err, calls())
	}
	unpriced := *route
	unpriced.Price = nil
	agent.SetHostedRoute(&unpriced)
	if _, _, err = call(pinned, pinnedRefs, 4); agent.PreDispatchCode(err) != agent.CodePriceMissing || calls() != 1 {
		t.Fatalf("no price: %v, %d calls", err, calls())
	}
	if fixture.Calls() != 1 {
		t.Fatalf("a refused hosted call fell back to the default provider (%d calls)", fixture.Calls())
	}
}

// TestWIS623ToolResultEscapedNamesNeverReachTheWire (Codex review test gap): the JSON-escaped
// identity case driven through the real path. A captured source titled with two known member
// names that JSON escapes ("A&B Motors" -> A\u0026B, "O'Brien <Tools>" -> \u003c) is read with
// Tools.Call, its serialized result goes into the conversation as a tool message, and the pinned
// hosted call (terms read by PseudonymTermsTx) must carry neither name in any form.
func TestWIS623ToolResultEscapedNamesNeverReachTheWire(t *testing.T) {
	w := newWorldOpts(t, worldOpts{scheduleTitle: `Plan for A&B Motors and O'Brien <Tools>`})
	w.exec(t, `INSERT INTO supervisor.member_profiles(tenant_id,issuer,subject,display_name) VALUES($1,$2,$3,'A&B Motors'),($1,$4,$5,'O''Brien <Tools>')`,
		w.tenant, w.reviewer.Issuer, w.reviewer.Subject, w.ea.Issuer, w.ea.Subject)
	var mu sync.Mutex
	var wire []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		wire = append(wire, string(b))
		mu.Unlock()
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"model":"qwen-plus-2025-09-11","choices":[{"message":{"content":"{}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	t.Cleanup(srv.Close)
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("sk-test"), 0o600); err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse(srv.URL)
	hosted := config.HostedAgent{Provider: "qwen-intl", Model: "qwen-plus-2025-09-11", ModelRevision: "qwen-plus-2025-09-11", APIKeyFile: key}
	m, err := agent.NewHostedModelForTest(hosted, base, srv.Client().Transport, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	price, version, _ := agent.HostedPrice(hosted.Provider, hosted.Model, hosted.ModelRevision)
	agent.SetHostedRoute(&agent.HostedRoute{Provider: m, Name: "hosted-qwen-intl", Region: "ap-southeast-1", Model: hosted.Model, ModelRevision: hosted.ModelRevision, Price: &price, PriceVersion: version})
	t.Cleanup(func() { agent.SetHostedRoute(nil) })
	owner := modelhosting.Principal{Issuer: w.ea.Issuer, Subject: w.ea.Subject, SessionHash: w.ea.SessionHash}
	if _, err = modelhosting.New(w.runtimeDB).Decide(w.ctx, owner, modelhosting.Scope{TenantID: w.tenant, SiteID: w.site},
		modelhosting.DecisionInput{Enabled: true, DPAReference: "DPA-2026-11-05", Reason: "Pilot on hosted Qwen"}, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}

	// openRun creates the run while the switch is on, so it is pinned to the hosted route.
	tsc, env := w.openRun(t, agent.DefaultLimits())
	sc := agent.RunScope{TenantID: w.tenant, SiteID: w.site}
	var run agent.Run
	var refs []agent.RunRef
	if err = w.store.InTx(w.ctx, sc, func(tx *sql.Tx) error {
		var e error
		if run, e = agent.GetRunTx(w.ctx, tx, sc, tsc.RunID); e != nil {
			return e
		}
		refs, e = agent.RefsTx(w.ctx, tx, sc, tsc.RunID)
		return e
	}); err != nil || run.ProviderName != "hosted-qwen-intl" {
		t.Fatalf("pinned run: %+v %v", run, err)
	}
	rt := w.runtime(agent.NewFixtureProvider(), nil)
	args := json.RawMessage(`{"evidence_id":"` + w.schedule + `"}`)
	out, err := rt.Tools.Call(w.ctx, tsc, env, agent.ToolEvidenceGet, args)
	if err != nil || !strings.Contains(string(out.Content), `A\u0026B Motors`) || !strings.Contains(string(out.Content), `O'Brien \u003cTools\u003e`) {
		t.Fatalf("tool result does not carry the escaped names: %v %s", err, out.Content)
	}
	req := agent.ModelRequest{SchemaVersion: agent.ModelSchemaVersion, Model: rt.Model, PromptVersion: agent.PromptVersion, ToolVersion: agent.ToolVersion, MaxOutputTokens: 200,
		Messages: []agent.Message{{Role: "system", Content: "Investigate the case."}, {Role: "user", Content: `{"case_id":"` + w.caseID + `"}`},
			{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "read", Name: agent.ToolEvidenceGet, Arguments: args}}},
			{Role: "tool", ToolCallID: "read", Content: string(out.Content)}}}
	if _, perr, err := rt.CallForTest(w.ctx, sc, run, refs, req, 1); err != nil || perr != nil || len(wire) != 1 {
		t.Fatalf("hosted call: %v %v, %d calls", perr, err, len(wire))
	}
	for _, raw := range []string{"A&B", `A\\u0026B`, `A\u0026B`, "Brien", "Motors", "Tools"} {
		if strings.Contains(wire[0], raw) {
			t.Fatalf("%q reached the hosted endpoint: %s", raw, wire[0])
		}
	}
	if !strings.Contains(wire[0], "Plan for") || !strings.Contains(wire[0], w.schedule) {
		t.Fatalf("the tool result did not reach the wire: %s", wire[0])
	}
}

// TestWIS623ProseJSONInSourcesThroughTools (Codex round 2, finding 2): a captured source whose
// text carries JSON inside prose and a fenced block, read with the real Tools.Call and sent on a
// pinned hosted call. Names and phones written plainly are hidden on the wire; a name that is
// only visible after decoding an escape refuses the call (PSEUDONYMS_INCOMPLETE) and nothing is sent.
func TestWIS623ProseJSONInSourcesThroughTools(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		refused     bool
	}{
		{"plain", ` Contact record: {"customer":"A&B Motors","phone":9.876543210e9}` + "\n```json\n{\"owner\":\"O'Brien Tools\",\"mobile\":8765432109.0}\n```", false},
		{"escaped", ` Contact record: {"customer":"A` + "BSu0026" + `B Motors"}` + "\n```json\n{\"owner\":\"O" + "BSu0027" + `Brien Tools"}` + "\n```", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorldOpts(t, worldOpts{scheduleExtra: strings.ReplaceAll(tc.extra, "BS", "\\")})
			w.exec(t, `INSERT INTO supervisor.member_profiles(tenant_id,issuer,subject,display_name) VALUES($1,$2,$3,'A&B Motors'),($1,$4,$5,'O''Brien Tools')`,
				w.tenant, w.reviewer.Issuer, w.reviewer.Subject, w.ea.Issuer, w.ea.Subject)
			wire, _, perr := hostedToolCall(t, w)
			if tc.refused {
				if agent.PreDispatchCode(perr) != agent.CodePseudonymsIncomplete && !isCode(perr, agent.CodePseudonymsIncomplete) || len(wire) != 0 {
					t.Fatalf("escaped name: %v, %d sent", perr, len(wire))
				}
				return
			}
			if perr != nil || len(wire) != 1 {
				t.Fatalf("plain: %v, %d sent", perr, len(wire))
			}
			for _, raw := range []string{"A&B", "Motors", "Brien", "9.876543210e9", "8765432109"} {
				if strings.Contains(wire[0], raw) {
					t.Fatalf("%q reached the hosted endpoint: %s", raw, wire[0])
				}
			}
			if !strings.Contains(wire[0], "Contact record") {
				t.Fatalf("the source text did not reach the wire: %s", wire[0])
			}
		})
	}
}

// TestWIS623JSONSyntaxNameInToolResult (WIS-623 Codex rounds 11 and 12): a member name with
// JSON syntax (a double quote) in a source read with Tools.Call sits inside one string of the JSON
// tool result, so it is hidden there; the audit of what goes on the wire finds nothing of it.
func TestWIS623JSONSyntaxNameInToolResult(t *testing.T) {
	w := newWorldOpts(t, worldOpts{scheduleExtra: ` Supplier O"Brien Tools confirmed.`})
	w.exec(t, `INSERT INTO supervisor.member_profiles(tenant_id,issuer,subject,display_name) VALUES($1,$2,$3,'O"Brien Tools')`,
		w.tenant, w.reviewer.Issuer, w.reviewer.Subject)
	wire, _, perr := hostedToolCall(t, w)
	if perr != nil || len(wire) != 1 {
		t.Fatalf("JSON-syntax name: %v, %d sent", perr, len(wire))
	}
	if strings.Contains(wire[0], "Brien") || !strings.Contains(wire[0], "confirmed") {
		t.Fatalf("the name reached the hosted endpoint, or the text did not: %s", wire[0])
	}
}

// TestWIS623RenamedCustomerKeepsItsOldNameHidden (WIS-623 post-merge review, P1): a customer
// renamed from "Acme Motors" to "Zenith Motors" is still called Acme in retained evidence. The
// pseudonym dictionary read only the current revision, so the old name left on the hosted wire.
// Every revision's display name and aliases are hidden now.
func TestWIS623RenamedCustomerKeepsItsOldNameHidden(t *testing.T) {
	w := newWorldOpts(t, worldOpts{scheduleExtra: " Acme Motors (also Acme Auto) asked for order seven; Zenith Motors confirmed."})
	seedCustomer(t, w, w.site, w.schedule, []string{"Acme Motors", "Acme Auto"}, []string{"Zenith Motors"})
	wire, _, perr := hostedToolCall(t, w)
	if perr != nil || len(wire) != 1 {
		t.Fatalf("hosted call: %v, %d sent", perr, len(wire))
	}
	for _, raw := range []string{"Acme", "Zenith", "Motors", "Auto"} {
		if strings.Contains(wire[0], raw) {
			t.Fatalf("%q reached the hosted endpoint: %s", raw, wire[0])
		}
	}
	if !strings.Contains(wire[0], "asked for order seven") {
		t.Fatalf("the source text did not reach the wire: %s", wire[0])
	}
}

// TestWIS623RefusedHostedOutputIsCharged (WIS-623 post-merge review, P2): a hosted answer that
// names a pseudonym token the request never issued is refused, but the endpoint billed it. The
// failure path used to record zero tokens, so Charge refunded the whole reservation.
func TestWIS623RefusedHostedOutputIsCharged(t *testing.T) {
	w := newWorld(t)
	_, _, perr := hostedToolCall(t, w, func(string) string { return `{"assignee":"⟦P77⟧"}` })
	if !isCode(perr, agent.CodePseudonymUnknown) {
		t.Fatalf("unknown token: %v", perr)
	}
	price, _, _ := agent.HostedPrice("qwen-intl", "qwen-plus-2025-09-11", "qwen-plus-2025-09-11")
	want := (10*price.InputMicroUSDPerMillion + 5*price.OutputMicroUSDPerMillion + 999_999) / 1_000_000
	var status, reason string
	var in, out, charged int64
	if err := w.control.QueryRowContext(w.ctx, `SELECT status,reason_code,input_tokens,output_tokens,charged_microusd FROM supervisor.agent_model_calls WHERE tenant_id=$1`, w.tenant).
		Scan(&status, &reason, &in, &out, &charged); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || reason != agent.CodePseudonymUnknown || in != 10 || out != 5 || want == 0 || charged != want {
		t.Fatalf("refused call settled as %s/%s, %d+%d tokens, %d micro-USD (want %d)", status, reason, in, out, charged, want)
	}
}

// TestWIS623LocalRunRefusesAnotherModel (WIS-623 post-merge review, P2): a run pinned to one
// local model and picked up by a worker configured for another used to run on the worker's
// model while its provenance named the pinned one. It is refused before dispatch, like a hosted
// run without its route, and the worker's provider is never called.
func TestWIS623LocalRunRefusesAnotherModel(t *testing.T) {
	w := newWorld(t)
	sc := agent.RunScope{TenantID: w.tenant, SiteID: w.site}
	for _, tc := range []struct {
		name, provider, model, revision string
		actual                          bool
	}{
		{"same pin", agent.ProviderFixture, "fixture-model", "sha256:fixture", false},
		{"other model", agent.ProviderFixture, "other-model", "sha256:fixture", false},
		{"other revision", agent.ProviderFixture, "fixture-model", "sha256:other", false},
		{"other provider", agent.ProviderLocal, "fixture-model", "sha256:fixture", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			revision := w.fresh(t)
			run := agent.Run{ID: uuid.NewString(), RequestKind: agent.RequestCase, CaseID: w.caseID, CaseRevision: revision, TraceID: agent.NewTraceID(),
				ProviderName: tc.provider, ProviderActual: tc.actual, Model: tc.model, ModelRevision: tc.revision, PromptVersion: agent.PromptVersion, ToolVersion: agent.ToolVersion, PolicyRevision: approvals.PolicyRevision}
			var refs []agent.RunRef
			if err := w.store.InTx(w.ctx, sc, func(tx *sql.Tx) error {
				in, e := agent.LoadCaseInput(w.ctx, tx, sc, w.caseID, agent.LoaderOptions{ModeEnabled: true})
				if e != nil {
					return e
				}
				if e = agent.CreateRunTx(w.ctx, tx, sc, run, agent.DefaultLimits()); e != nil {
					return e
				}
				refs = in.Refs
				return agent.AddRefsTx(w.ctx, tx, sc, run.ID, in.Refs)
			}); err != nil {
				t.Fatal(err)
			}
			fixture := agent.NewFixtureProvider(agent.FixtureStep{Response: agent.ModelResponse{Content: `{}`}})
			rt := w.runtime(fixture, nil) // the worker: fixture provider, fixture-model, sha256:fixture
			req := agent.ModelRequest{SchemaVersion: agent.ModelSchemaVersion, Model: rt.Model, PromptVersion: agent.PromptVersion, ToolVersion: agent.ToolVersion, MaxOutputTokens: 200,
				Messages: []agent.Message{{Role: "system", Content: "Investigate the case."}, {Role: "user", Content: "Press one stopped."}}}
			_, perr, err := rt.CallForTest(w.ctx, sc, run, refs, req, 1)
			var rows int
			if e := w.control.QueryRowContext(w.ctx, `SELECT count(*) FROM supervisor.agent_model_calls WHERE tenant_id=$1 AND run_id=$2`, w.tenant, run.ID).Scan(&rows); e != nil {
				t.Fatal(e)
			}
			if tc.name == "same pin" {
				if err != nil || perr != nil || fixture.Calls() != 1 || rows != 1 {
					t.Fatalf("pinned call: %v %v, %d provider calls, %d rows", perr, err, fixture.Calls(), rows)
				}
				return
			}
			if agent.PreDispatchCode(err) != agent.CodeModelNotPinned || fixture.Calls() != 0 || rows != 0 {
				t.Fatalf("mismatched pin: %v %v, %d provider calls, %d rows", perr, err, fixture.Calls(), rows)
			}
		})
	}
}

// TestWIS623SiblingSiteCustomerNameHidden (WIS-623 Codex review of 7527f602, P1): a customer
// registered only at a sibling site of the same tenant is still a customer of that tenant, and a
// mention of it in this site's evidence must not leave unredacted, however short ("GE", WIS-623
// Codex round 2). Restoration re-inserts only the exact text this request itself replaced.
func TestWIS623SiblingSiteCustomerNameHidden(t *testing.T) {
	w := newWorldOpts(t, worldOpts{scheduleExtra: " Orbit Castings (also Orbit Cast, GE) asked for order seven."})
	seedSiblingCustomer(t, w, []string{"Orbit Castings", "Orbit Cast", "GE"})
	wire, _, perr := hostedToolCall(t, w)
	if perr != nil || len(wire) != 1 {
		t.Fatalf("hosted call: %v, %d sent", perr, len(wire))
	}
	for _, raw := range []string{"Orbit", "Castings", "Cast,", "GE)"} {
		if strings.Contains(wire[0], raw) {
			t.Fatalf("%q reached the hosted endpoint: %s", raw, wire[0])
		}
	}
	if !strings.Contains(wire[0], "asked for order seven") {
		t.Fatalf("the source text did not reach the wire: %s", wire[0])
	}
}

// TestWIS623SiblingCustomerTreatedAsOwn (WIS-623 Codex round 2, lead decision): a customer of a
// sibling site is handled exactly like this site's own names. Its plain mention is hidden and the
// answer restores the exact spelling sent ("ORBIT CASTINGS", not the recorded "Orbit Castings");
// an encoded mention the pseudonymiser cannot rewrite ("Orbit%20Castings") refuses the call.
func TestWIS623SiblingCustomerTreatedAsOwn(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		refused     bool
	}{
		{"plain mention", " ORBIT CASTINGS asked for order seven.", false},
		{"encoded mention", " See Orbit%20Castings/orders for order seven.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorldOpts(t, worldOpts{scheduleExtra: tc.extra})
			seedSiblingCustomer(t, w, []string{"Orbit Castings"})
			wire, resp, perr := hostedToolCall(t, w, func(body string) string {
				return `{"named":"` + strings.Join(regexp.MustCompile(`⟦P[0-9]+⟧`).FindAllString(body, -1), " ") + `"}`
			})
			if tc.refused {
				if !isCode(perr, agent.CodePseudonymsIncomplete) || len(wire) != 0 {
					t.Fatalf("encoded sibling name: %v, %d sent", perr, len(wire))
				}
				return
			}
			if perr != nil || len(wire) != 1 || strings.Contains(wire[0], "ORBIT") {
				t.Fatalf("plain sibling name: %v, %d sent %v", perr, len(wire), wire)
			}
			if !strings.Contains(resp.Content, "ORBIT CASTINGS") || strings.Contains(resp.Content, "Orbit Castings") {
				t.Fatalf("restored answer is not the request's own spelling: %s", resp.Content)
			}
		})
	}
}

// TestWIS623GreekCapitalCustomerHidden (WIS-623 Codex rounds 3 to 5, P1): the canonical view
// folds Greek lookalikes per case: "ΝΙΝΑ" reads "nina", "νινα" reads "viva". A name stored in one
// case and mentioned in the other used to match neither the redaction nor the audit and left in
// clear: first because the read lower-cased stored names, then because only the stored case was
// matched.
func TestWIS623GreekCapitalCustomerHidden(t *testing.T) {
	for _, c := range []struct{ stored, mention string }{{"ΝΙΝΑ", "ΝΙΝΑ"}, {"ΝΙΝΑ", "νινα"}, {"νινα", "ΝΙΝΑ"},
		// Codex round 5: final sigma, dotless i, U+1C80, a format character between a letter and its mark.
		{"ΝΙΚΟΣ", "νικος"}, {"ırmak", "IRMAK"}, {"Вера", "ᲀера"}, {"A\u200b\u0301lice", "\u00c1lice"},
		// Security review of 8aafb153: the Turkish dotted capital İ.
		{"Ilker", "İLKER"}, {"İlker", "ilker"},
		// Codex round 7: a mixed Latin/Greek spelling, and a Latin spelling of a mixed-case Greek name.
		{"ΝΙΝΑ", "Nινα"}, {"ΝΙΝΑ", "Niva"}} {
		t.Run(c.stored+" mentioned as "+c.mention, func(t *testing.T) {
			w := newWorldOpts(t, worldOpts{scheduleExtra: " " + c.mention + " asked for order seven."})
			seedSiblingCustomer(t, w, []string{c.stored})
			wire, _, perr := hostedToolCall(t, w)
			if perr != nil || len(wire) != 1 {
				t.Fatalf("hosted call: %v, %d sent", perr, len(wire))
			}
			if strings.Contains(wire[0], c.mention) || !strings.Contains(wire[0], "asked for order seven") {
				t.Fatalf("the customer's name reached the hosted endpoint: %s", wire[0])
			}
		})
	}
}

// TestWIS623PseudonymTermCap (WIS-623 Codex round 2): the bound is on distinct terms, so many
// revisions repeating the same names do not exhaust it; more distinct terms than the bound, or a
// term longer than the contract allows, refuse the hosted call before any budget is reserved.
func TestWIS623PseudonymTermCap(t *testing.T) {
	defer agent.SetMaxPseudonymTermsForTest(50)()
	same := make([][]string, 100) // 100 revisions x 21 names, the same 21 every time
	for i := range same {
		same[i] = []string{"Orbit Castings"}
		for k := range 20 {
			same[i] = append(same[i], fmt.Sprintf("Orbit Plant %c", 'A'+k))
		}
	}
	var distinct []string
	for k := range 60 {
		distinct = append(distinct, fmt.Sprintf("Customer %02d", k))
	}
	for _, tc := range []struct {
		name      string
		revisions [][]string
		refused   bool
	}{
		{"repeated names", same, false},
		{"too many distinct names", [][]string{distinct[:20], distinct[20:40], distinct[40:]}, true},
		{"name too long", [][]string{{strings.Repeat("Orbit", 41)}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			seedSiblingCustomer(t, w, tc.revisions...)
			wire, _, perr := hostedToolCall(t, w)
			var calls int
			if err := w.control.QueryRowContext(w.ctx, `SELECT count(*) FROM supervisor.agent_model_calls WHERE tenant_id=$1`, w.tenant).Scan(&calls); err != nil {
				t.Fatal(err)
			}
			if tc.refused {
				if agent.PreDispatchCode(perr) != agent.CodePseudonymsIncomplete || len(wire) != 0 || calls != 0 {
					t.Fatalf("out of bounds: %v, %d sent, %d reserved", perr, len(wire), calls)
				}
				return
			}
			if perr != nil || len(wire) != 1 || calls != 1 {
				t.Fatalf("within bounds: %v, %d sent, %d reserved", perr, len(wire), calls)
			}
		})
	}
}

// seedSiblingCustomer stores a customer at a new sibling site of w's tenant, cited to a source
// there, with one revision per entry of revisions.
func seedSiblingCustomer(t *testing.T, w *world, revisions ...[]string) {
	t.Helper()
	sibling := uuid.NewString()
	w.exec(t, `INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'sibling site')`, w.tenant, sibling)
	w.exec(t, `INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, w.tenant, sibling, w.ea.Issuer, w.ea.Subject)
	source, err := w.ev.CreateOrRevise(w.ctx, w.ea, evidence.Scope{TenantID: w.tenant, SiteID: sibling},
		evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Sibling customers", Text: "Customers of the sibling site."}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	seedCustomer(t, w, sibling, source.SourceID, revisions...)
}

// TestWIS623AnswerPhaseRestoresPlainDraft (WIS-623 Codex rounds 14 and 15): through the
// runtime's two-phase call, a prose draft naming "A&B Motors", O"Brien and a backslash name, and
// a second mention of a token, is carried to the answer-phase request in the form
// the model wrote it, tokens intact on the same map, and is not refused; no name reaches the wire.
func TestWIS623AnswerPhaseRestoresPlainDraft(t *testing.T) {
	w := newWorld(t)
	seedSiblingCustomer(t, w, []string{"A&B Motors", `O"Brien`, `Back\Slash Ltd`})
	var mu sync.Mutex
	var wire []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		wire = append(wire, string(b))
		n := len(wire)
		mu.Unlock()
		content := `{}`
		if n == 1 { // the draft names every token it was given
			toks := regexp.MustCompile(`⟦P[0-9]+⟧`).FindAllString(string(b), -1)
			content = "The suppliers are " + strings.Join(toks, ", ") + ". Code " + toks[0] + "."
		}
		answer, _ := json.Marshal(content)
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"model":"qwen-plus-2025-09-11","choices":[{"message":{"content":` + string(answer) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	t.Cleanup(srv.Close)
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("sk-test"), 0o600); err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse(srv.URL)
	hosted := config.HostedAgent{Provider: "qwen-intl", Model: "qwen-plus-2025-09-11", ModelRevision: "qwen-plus-2025-09-11", APIKeyFile: key}
	m, err := agent.NewHostedModelForTest(hosted, base, srv.Client().Transport, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	price, version, _ := agent.HostedPrice(hosted.Provider, hosted.Model, hosted.ModelRevision)
	agent.SetHostedRoute(&agent.HostedRoute{Provider: m, Name: "hosted-qwen-intl", Region: "ap-southeast-1", Model: hosted.Model, ModelRevision: hosted.ModelRevision, Price: &price, PriceVersion: version})
	t.Cleanup(func() { agent.SetHostedRoute(nil) })
	owner := modelhosting.Principal{Issuer: w.ea.Issuer, Subject: w.ea.Subject, SessionHash: w.ea.SessionHash}
	if _, err = modelhosting.New(w.runtimeDB).Decide(w.ctx, owner, modelhosting.Scope{TenantID: w.tenant, SiteID: w.site},
		modelhosting.DecisionInput{Enabled: true, DPAReference: "DPA-2026-11-05", Reason: "Pilot on hosted Qwen"}, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	tsc, _ := w.openRun(t, agent.DefaultLimits())
	sc := agent.RunScope{TenantID: w.tenant, SiteID: w.site}
	var run agent.Run
	var refs []agent.RunRef
	if err = w.store.InTx(w.ctx, sc, func(tx *sql.Tx) error {
		var e error
		if run, e = agent.GetRunTx(w.ctx, tx, sc, tsc.RunID); e != nil {
			return e
		}
		refs, e = agent.RefsTx(w.ctx, tx, sc, tsc.RunID)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	rt := w.runtime(agent.NewFixtureProvider(), nil)
	req := agent.ModelRequest{SchemaVersion: agent.ModelSchemaVersion, Model: rt.Model, PromptVersion: agent.PromptVersion, ToolVersion: agent.ToolVersion, MaxOutputTokens: 200,
		Tools:          []agent.ToolDef{{Name: agent.ToolEvidenceGet, Description: "Read evidence.", Parameters: json.RawMessage(`{"type":"object"}`)}},
		ResponseSchema: json.RawMessage(`{"type":"object"}`),
		Messages: []agent.Message{{Role: "system", Content: "Investigate the case."},
			{Role: "user", Content: `Who supplied the die? A&B Motors, O"Brien and Back\Slash Ltd were in the record.`}}}
	_, perr, err := rt.CallForTest(w.ctx, sc, run, refs, req, 1)
	if err != nil || perr != nil {
		t.Fatalf("two-phase call: %v %v", perr, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(wire) != 2 {
		t.Fatalf("%d calls, want the draft and the answer", len(wire))
	}
	for _, raw := range []string{"A&B", `A\u0026B`, "Brien", "Slash"} {
		if strings.Contains(wire[1], raw) {
			t.Fatalf("%q reached the hosted endpoint in the answer phase: %s", raw, wire[1])
		}
	}
	if !strings.Contains(wire[1], "The suppliers are ⟦P") || !strings.Contains(wire[1], "Code ⟦P") {
		t.Fatalf("the hidden draft is not in the answer request: %s", wire[1])
	}
}

// TestWIS623ToolLoopCarriesHiddenAssistant (WIS-623 Codex round 16): in a tool loop the
// assistant message sent back to the hosted model is the one it wrote, tokens intact on the same
// map, its tool call too; only the tool's own arguments are restored when it runs. Names added or
// renamed between the calls stay protected.
func TestWIS623ToolLoopCarriesHiddenAssistant(t *testing.T) {
	w := newWorld(t)
	w.exec(t, `INSERT INTO supervisor.member_profiles(tenant_id,issuer,subject,display_name) VALUES($1,$2,$3,'Alice Rao')`, w.tenant, w.reviewer.Issuer, w.reviewer.Subject)
	var mu sync.Mutex
	var wire []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		wire = append(wire, string(b))
		n := len(wire)
		mu.Unlock()
		msg := `{"content":"{}"}`
		if n == 1 {
			tok := regexp.MustCompile(`⟦P[0-9]+⟧`).FindString(string(b))
			content, _ := json.Marshal("Checking " + tok + ".")
			args, _ := json.Marshal(`{"evidence_id":"` + w.schedule + `","note":"` + tok + `"}`)
			msg = `{"content":` + string(content) + `,"tool_calls":[{"id":"c1","type":"function","function":{"name":"evidence.get","arguments":` + string(args) + `}}]}`
		}
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"model":"qwen-plus-2025-09-11","choices":[{"message":` + msg + `,"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	t.Cleanup(srv.Close)
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("sk-test"), 0o600); err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse(srv.URL)
	hosted := config.HostedAgent{Provider: "qwen-intl", Model: "qwen-plus-2025-09-11", ModelRevision: "qwen-plus-2025-09-11", APIKeyFile: key}
	m, err := agent.NewHostedModelForTest(hosted, base, srv.Client().Transport, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	price, version, _ := agent.HostedPrice(hosted.Provider, hosted.Model, hosted.ModelRevision)
	agent.SetHostedRoute(&agent.HostedRoute{Provider: m, Name: "hosted-qwen-intl", Region: "ap-southeast-1", Model: hosted.Model, ModelRevision: hosted.ModelRevision, Price: &price, PriceVersion: version})
	t.Cleanup(func() { agent.SetHostedRoute(nil) })
	owner := modelhosting.Principal{Issuer: w.ea.Issuer, Subject: w.ea.Subject, SessionHash: w.ea.SessionHash}
	if _, err = modelhosting.New(w.runtimeDB).Decide(w.ctx, owner, modelhosting.Scope{TenantID: w.tenant, SiteID: w.site},
		modelhosting.DecisionInput{Enabled: true, DPAReference: "DPA-2026-11-05", Reason: "Pilot on hosted Qwen"}, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	tsc, _ := w.openRun(t, agent.DefaultLimits())
	sc := agent.RunScope{TenantID: w.tenant, SiteID: w.site}
	var run agent.Run
	var refs []agent.RunRef
	if err = w.store.InTx(w.ctx, sc, func(tx *sql.Tx) error {
		var e error
		if run, e = agent.GetRunTx(w.ctx, tx, sc, tsc.RunID); e != nil {
			return e
		}
		refs, e = agent.RefsTx(w.ctx, tx, sc, tsc.RunID)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	rt := w.runtime(agent.NewFixtureProvider(), nil)
	req := agent.ModelRequest{SchemaVersion: agent.ModelSchemaVersion, Model: rt.Model, PromptVersion: agent.PromptVersion, ToolVersion: agent.ToolVersion, MaxOutputTokens: 200,
		Tools:          []agent.ToolDef{{Name: agent.ToolEvidenceGet, Description: "Read evidence.", Parameters: json.RawMessage(`{"type":"object"}`)}},
		ResponseSchema: json.RawMessage(`{"type":"object"}`),
		Messages: []agent.Message{{Role: "system", Content: "Investigate the case."},
			{Role: "user", Content: "Alice Rao reported the press stopped."}}}
	resp, perr, err := rt.CallForTest(w.ctx, sc, run, refs, req, 1)
	if err != nil || perr != nil || len(resp.ToolCalls) != 1 {
		t.Fatalf("first call: %+v %v %v", resp, perr, err)
	}
	// The tool runs with its arguments restored; the transcript carries the hidden message.
	if !strings.Contains(string(resp.ToolCalls[0].Arguments), "Alice Rao") || resp.Content != "Checking Alice Rao." {
		t.Fatalf("restored for the caller: %q %s", resp.Content, resp.ToolCalls[0].Arguments)
	}
	// A customer alias added between the tool call and the continuation is hidden there too
	// (security review of c0ded9d6: the carried map is extended with the names read again).
	seedSiblingCustomer(t, w, []string{"Zed Castings"})
	// The member is renamed meanwhile: the replayed user message still names "Alice Rao", which
	// stays protected (Codex round 17: carried names are kept with the names read again).
	w.exec(t, `UPDATE supervisor.member_profiles SET display_name='Bob Rao' WHERE tenant_id=$1 AND issuer=$2 AND subject=$3`, w.tenant, w.reviewer.Issuer, w.reviewer.Subject)
	req.Messages = append(req.Messages, resp.AssistantMessage(), agent.Message{Role: "tool", ToolCallID: "c1", Content: `{"ok":true,"supplier":"Zed Castings"}`})
	if _, perr, err = rt.CallForTest(w.ctx, sc, run, refs, req, 2); err != nil || perr != nil {
		t.Fatalf("continuation: %v %v", perr, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(wire) < 2 || strings.Contains(wire[1], "Alice") || strings.Contains(wire[1], "Rao") || strings.Contains(wire[1], "Zed") || !strings.Contains(wire[1], "Checking ⟦P") {
		t.Fatalf("continuation request: %v", wire)
	}
}

// seedCustomer stores a customer entity at site, cited to revision 1 of source, with one
// revision per entry of revisions ({display name, aliases...}); the last is current.
func seedCustomer(t *testing.T, w *world, site, source string, revisions ...[]string) {
	t.Helper()
	entity := uuid.NewString()
	payload := func(names []string) string {
		b, _ := json.Marshal(map[string]any{"entity_id": entity, "type": "customer", "display_name": names[0], "aliases": names[1:], "source_id": source, "source_revision": 1})
		return string(b)
	}
	tx, err := w.control.BeginTx(w.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := tx.ExecContext(w.ctx, q, args...); e != nil {
			t.Fatalf("%s: %v", q, e)
		}
	}
	exec(`SELECT set_config('supervisor.tenant_id',$1,true)`, w.tenant)
	exec(`INSERT INTO supervisor.knowledge_entities(tenant_id,site_id,id,entity_type,payload,source_id,source_revision,actor_issuer,actor_subject) VALUES($1,$2,$3,'customer',$4,$5,1,'test','operator')`, w.tenant, site, entity, payload(revisions[0]), source)
	exec(`INSERT INTO supervisor.knowledge_entity_heads(tenant_id,site_id,id,current_revision) VALUES($1,$2,$3,$4)`, w.tenant, site, entity, len(revisions))
	for i, names := range revisions {
		exec(`INSERT INTO supervisor.knowledge_entity_revisions(tenant_id,site_id,id,revision,entity_type,payload,source_id,source_revision,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,'customer',$5,$6,1,'test','operator')`, w.tenant, site, entity, i+1, payload(names), source)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func isCode(err error, code string) bool {
	var p *agent.ProviderError
	return errors.As(err, &p) && p.Code == code
}

// hostedToolCall switches hosting on for w's site, opens a run pinned to a stand-in hosted
// route, reads the captured source with Tools.Call and sends the result on one hosted call. It
// returns the request bodies the endpoint received, the restored response and the call's provider
// error. The endpoint answers reply(request body) (default "{}") with 10 input and 5 output tokens.
func hostedToolCall(t *testing.T, w *world, reply ...func(body string) string) ([]string, agent.ModelResponse, error) {
	t.Helper()
	var mu sync.Mutex
	var wire []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		wire = append(wire, string(b))
		mu.Unlock()
		content := "{}"
		if len(reply) > 0 {
			content = reply[0](string(b))
		}
		answer, _ := json.Marshal(content)
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"model":"qwen-plus-2025-09-11","choices":[{"message":{"content":` + string(answer) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	t.Cleanup(srv.Close)
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("sk-test"), 0o600); err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse(srv.URL)
	hosted := config.HostedAgent{Provider: "qwen-intl", Model: "qwen-plus-2025-09-11", ModelRevision: "qwen-plus-2025-09-11", APIKeyFile: key}
	m, err := agent.NewHostedModelForTest(hosted, base, srv.Client().Transport, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	price, version, _ := agent.HostedPrice(hosted.Provider, hosted.Model, hosted.ModelRevision)
	agent.SetHostedRoute(&agent.HostedRoute{Provider: m, Name: "hosted-qwen-intl", Region: "ap-southeast-1", Model: hosted.Model, ModelRevision: hosted.ModelRevision, Price: &price, PriceVersion: version})
	t.Cleanup(func() { agent.SetHostedRoute(nil) })
	owner := modelhosting.Principal{Issuer: w.ea.Issuer, Subject: w.ea.Subject, SessionHash: w.ea.SessionHash}
	if _, err = modelhosting.New(w.runtimeDB).Decide(w.ctx, owner, modelhosting.Scope{TenantID: w.tenant, SiteID: w.site},
		modelhosting.DecisionInput{Enabled: true, DPAReference: "DPA-2026-11-05", Reason: "Pilot on hosted Qwen"}, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	tsc, env := w.openRun(t, agent.DefaultLimits())
	sc := agent.RunScope{TenantID: w.tenant, SiteID: w.site}
	var run agent.Run
	var refs []agent.RunRef
	if err = w.store.InTx(w.ctx, sc, func(tx *sql.Tx) error {
		var e error
		if run, e = agent.GetRunTx(w.ctx, tx, sc, tsc.RunID); e != nil {
			return e
		}
		refs, e = agent.RefsTx(w.ctx, tx, sc, tsc.RunID)
		return e
	}); err != nil || run.ProviderName != "hosted-qwen-intl" {
		t.Fatalf("pinned run: %+v %v", run, err)
	}
	rt := w.runtime(agent.NewFixtureProvider(), nil)
	args := json.RawMessage(`{"evidence_id":"` + w.schedule + `"}`)
	out, err := rt.Tools.Call(w.ctx, tsc, env, agent.ToolEvidenceGet, args)
	if err != nil {
		t.Fatal(err)
	}
	req := agent.ModelRequest{SchemaVersion: agent.ModelSchemaVersion, Model: rt.Model, PromptVersion: agent.PromptVersion, ToolVersion: agent.ToolVersion, MaxOutputTokens: 200,
		Messages: []agent.Message{{Role: "system", Content: "Investigate the case."}, {Role: "user", Content: `{"case_id":"` + w.caseID + `"}`},
			{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "read", Name: agent.ToolEvidenceGet, Arguments: args}}},
			{Role: "tool", ToolCallID: "read", Content: string(out.Content)}}}
	resp, perr, err := rt.CallForTest(w.ctx, sc, run, refs, req, 1)
	if agent.PreDispatchCode(err) != "" {
		perr = err // refused before dispatch: reported like a provider refusal
	} else if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), wire...), resp, perr
}

// TestWIS276BriefEchoesPinnedHostedModel: the investigation brief's model_version is the model
// the run is pinned to, so a hosted proposal's versions.model_version never names the local model
// (Codex review P2 on 4a4ff77).
func TestWIS276BriefEchoesPinnedHostedModel(t *testing.T) {
	w := newWorld(t)
	var mu sync.Mutex
	var wire []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		wire = append(wire, string(b))
		mu.Unlock()
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"model":"qwen-plus-2025-09-11","choices":[{"message":{"content":"{}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":100}}`))
	}))
	t.Cleanup(srv.Close)
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("sk-test"), 0o600); err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse(srv.URL)
	hosted := config.HostedAgent{Provider: "qwen-intl", Model: "qwen-plus-2025-09-11", ModelRevision: "qwen-plus-2025-09-11", APIKeyFile: key}
	m, err := agent.NewHostedModelForTest(hosted, base, srv.Client().Transport, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	price, version, ok := agent.HostedPrice(hosted.Provider, hosted.Model, hosted.ModelRevision)
	if !ok {
		t.Fatal("no committed price")
	}
	agent.SetHostedRoute(&agent.HostedRoute{Provider: m, Name: "hosted-qwen-intl", Region: "ap-southeast-1", Model: hosted.Model, ModelRevision: hosted.ModelRevision, Price: &price, PriceVersion: version})
	t.Cleanup(func() { agent.SetHostedRoute(nil) })
	owner := modelhosting.Principal{Issuer: w.ea.Issuer, Subject: w.ea.Subject, SessionHash: w.ea.SessionHash}
	if _, err = modelhosting.New(w.runtimeDB).Decide(w.ctx, owner, modelhosting.Scope{TenantID: w.tenant, SiteID: w.site},
		modelhosting.DecisionInput{Enabled: true, DPAReference: "DPA-2026-11-05", Reason: "Pilot on hosted Qwen"}, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}

	rt := w.runtime(agent.NewFixtureProvider(), nil) // local model "fixture-model"
	rev := w.fresh(t)
	w.investigate(t, rt, rev)
	mu.Lock()
	defer mu.Unlock()
	if len(wire) == 0 || rt.Model == hosted.Model {
		t.Fatalf("hosted calls %d, local model %q", len(wire), rt.Model)
	}
	if !strings.Contains(wire[0], `\"model_version\":\"qwen-plus-2025-09-11\"`) || strings.Contains(wire[0], `\"model_version\":\"fixture-model\"`) {
		t.Fatalf("brief does not echo the pinned hosted model: %s", wire[0])
	}
}
