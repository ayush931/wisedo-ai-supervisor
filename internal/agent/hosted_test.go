package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
)

// WIS-623: names the system knows become request-local tokens, and come back only from that
// request's own map.
func TestPseudonymsHideAndRestore(t *testing.T) {
	p := NewPseudonyms([]string{"Rahul Deshmukh", "Chakan Plant", "Acme Motors", "राहुल", "Ram", "ab", "42"})
	in := "Rahul Deshmukh (rahul.d@plant.example, +91 98765 43210, mobile 9876543210) of Chakan Plant ships to ACME MOTORS. " +
		"राहुल checked it. Ramp 3 runs; Ram signed. Output 100.000000 parts, counter 1234567890. A planted ⟦P1⟧ stays harmless."
	out := p.hide(in)
	for _, raw := range []string{"Rahul Deshmukh", "rahul.d@plant.example", "+91 98765 43210", "9876543210", "Chakan Plant", "ACME MOTORS", "राहुल", "Ram signed"} {
		if strings.Contains(out, raw) {
			t.Fatalf("%q survived pseudonymisation: %s", raw, out)
		}
	}
	for _, kept := range []string{"Ramp 3", "100.000000", "1234567890", "[P1]"} {
		if !strings.Contains(out, kept) {
			t.Fatalf("%q was changed (only known names are replaced; numbers stay exact): %s", kept, out)
		}
	}
	// A response naming two issued tokens is restored, JSON-escaped, to exactly the text each
	// replaced ("ACME MOTORS" as written, not the recorded "Acme Motors": WIS-623 security review).
	tokRahul, tokAcme := p.token["Rahul Deshmukh"], p.token["ACME MOTORS"]
	resp, err := p.RestoreResponse(ModelResponse{Content: `{"who":"` + tokRahul + `","customer":"` + tokAcme + `"}`,
		ToolCalls: []ToolCall{{ID: "c", Name: ToolEvidenceGet, Arguments: json.RawMessage(`{"q":"` + tokRahul + `"}`)}}})
	if err != nil || resp.Content != `{"who":"Rahul Deshmukh","customer":"ACME MOTORS"}` || string(resp.ToolCalls[0].Arguments) != `{"q":"Rahul Deshmukh"}` {
		t.Fatalf("restore: %+v %v", resp, err)
	}
	// A token this request never issued refuses the whole output.
	if _, err = p.RestoreResponse(ModelResponse{Content: `{"who":"⟦P999⟧"}`}); !isProviderCode(err, CodePseudonymUnknown) {
		t.Fatalf("unknown token: %v", err)
	}
	// A term with a quote is restored as valid JSON text.
	q := NewPseudonyms([]string{`O"Brien Tools`})
	hidden := q.hide(`Supplier O"Brien Tools`)
	tok := q.token[`O"Brien Tools`]
	if !strings.Contains(hidden, tok) {
		t.Fatalf("quoted term not hidden: %s", hidden)
	}
	got, err := q.RestoreResponse(ModelResponse{Content: `{"s":"` + tok + `"}`})
	if err != nil || !json.Valid([]byte(got.Content)) || !strings.Contains(got.Content, `O\"Brien Tools`) {
		t.Fatalf("escaped restore: %q %v", got.Content, err)
	}
}

// NewPseudonyms is the test shorthand for a map whose terms are all the site's own.
func NewPseudonyms(terms []string) *Pseudonyms {
	p, err := NewPseudonymsFor(terms)
	if err != nil {
		panic(err)
	}
	return p
}

func isProviderCode(err error, code string) bool {
	var p *ProviderError
	return errors.As(err, &p) && p.Code == code
}

func TestHostedPriceTable(t *testing.T) {
	price, version, ok := HostedPrice("qwen-intl", "qwen-plus-2025-09-11", "qwen-plus-2025-09-11")
	if !ok || version == "" || price.InputMicroUSDPerMillion <= 0 || price.OutputMicroUSDPerMillion <= 0 {
		t.Fatalf("priced model: %+v %q %v", price, version, ok)
	}
	for _, c := range [][3]string{{"qwen-intl", "qwen-plus-2025-09-11", "other-revision"}, {"qwen-intl", "qwen-max", "qwen-max"}, {"deepseek", "qwen-plus-2025-09-11", "qwen-plus-2025-09-11"}} {
		if _, _, ok := HostedPrice(c[0], c[1], c[2]); ok {
			t.Fatalf("%v has a price", c)
		}
	}
	// A configured model without a price fails the boot, not each call.
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewHostedRoute(config.HostedAgent{Provider: "qwen-intl", Model: "qwen-max", ModelRevision: "qwen-max", APIKeyFile: key}, 0, slog.Default(), nil); err == nil {
		t.Fatal("a model without a price built a route")
	}
}

// The hosted dialer refuses every non-public address, so DNS cannot point plant data back into
// the network or at cloud metadata.
func TestHostedDialRefusesPrivateAddresses(t *testing.T) {
	// WIS-699 (security audit run 1): special-purpose blocks too, NAT64, 6to4 and Teredo among them.
	for _, a := range []string{"127.0.0.1", "10.1.2.3", "192.168.1.1", "100.64.0.1", "169.254.169.254", "::1", "fd00::1", "fe80::1", "0.0.0.0", "224.0.0.1",
		"0.1.2.3", "192.0.0.8", "192.0.2.1", "198.18.0.1", "203.0.113.5", "240.0.0.1", "255.255.255.255", "64:ff9b::a00:1", "2002:a00:1::1", "2001::1", "2001:db8::1", "100::1"} {
		ip := netip.MustParseAddr(a)
		dial := publicDialer(func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{ip}, nil })
		if _, err := dial(context.Background(), "tcp", "dashscope-intl.aliyuncs.com:443"); !errors.Is(err, errEndpointPrivate) {
			t.Fatalf("%s: %v", a, err)
		}
		if _, err := dial(context.Background(), "tcp", hostPort(a)+":443"); !errors.Is(err, errEndpointPrivate) {
			t.Fatalf("literal %s: %v", a, err)
		}
	}
	for _, a := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		if !publicAddr(netip.MustParseAddr(a)) {
			t.Fatalf("public address %s is refused", a)
		}
	}
}

func hostPort(a string) string {
	if strings.Contains(a, ":") {
		return "[" + a + "]"
	}
	return a
}

// standIn is an OpenAI-compatible HTTPS stand-in that records every request body and answers
// with reply(seenBody).
type standIn struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
	auth   []string
}

func newStandIn(t *testing.T, model string, reply func(body string) string) *standIn {
	s := &standIn{}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, string(b))
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		s.mu.Unlock()
		content, _ := json.Marshal(reply(string(b)))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"` + model + `","choices":[{"message":{"content":` + string(content) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *standIn) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

func hostedForTest(t *testing.T, s *standIn, logs io.Writer) *HostedModel {
	t.Helper()
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("sk-secret-test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse(s.srv.URL)
	m, err := newHostedModel(config.HostedAgent{Provider: "qwen-intl", Model: "qwen-plus-2025-09-11", ModelRevision: "qwen-plus-2025-09-11", APIKeyFile: key},
		base, s.srv.Client().Transport, 10*time.Second, slog.New(slog.NewTextHandler(logs, nil)), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestHostedModelPseudonymisesTheWire(t *testing.T) {
	var logs bytes.Buffer
	s := newStandIn(t, "qwen-plus-2025-09-11", func(body string) string {
		// The model refers to everyone by the tokens it was given.
		return `{"named":"` + strings.Join(pseudonymToken.FindAllString(body, -1), " ") + `"}`
	})
	m := hostedForTest(t, s, &logs)
	req := ModelRequest{SchemaVersion: ModelSchemaVersion, Model: "qwen-plus-2025-09-11", PromptVersion: "p", ToolVersion: "t", MaxOutputTokens: 100,
		Messages: []Message{{Role: "system", Content: "You assist Chakan Plant."}, {Role: "user", Content: "Rahul Deshmukh (rahul.d@plant.example) reported the press stopped."}}}

	// Without the runtime's pseudonym map the call is refused and nothing is sent.
	if _, err := m.Complete(context.Background(), req); !isProviderCode(err, CodePseudonymsMissing) || len(s.calls()) != 0 {
		t.Fatalf("no map: %v, %d calls", err, len(s.calls()))
	}
	ctx := WithPseudonyms(context.Background(), NewPseudonyms([]string{"Rahul Deshmukh", "Chakan Plant"}))
	resp, err := m.Complete(ctx, req)
	if err != nil || resp.Content != `{"named":"Chakan Plant Rahul Deshmukh rahul.d@plant.example"}` || resp.ProviderActual != "hosted-qwen-intl" {
		t.Fatalf("hosted call: %+v %v", resp, err)
	}
	wire := s.calls()[0]
	for _, raw := range []string{"Rahul Deshmukh", "rahul.d@plant.example", "Chakan Plant"} {
		if strings.Contains(wire, raw) {
			t.Fatalf("%q reached the hosted endpoint: %s", raw, wire)
		}
	}
	if !strings.Contains(wire, "⟦P") || s.auth[0] != "Bearer sk-secret-test-key" {
		t.Fatalf("wire %s auth %q", wire, s.auth[0])
	}
	if strings.Contains(logs.String(), "sk-secret-test-key") || strings.Contains(logs.String(), "Rahul") {
		t.Fatalf("a key or a name was logged: %s", logs.String())
	}
	// A response naming a token this request never issued refuses the whole output.
	// WIS-623 post-merge review (P2): the refused output was billed, so its reported usage comes
	// back with the error (none of its text does) and the runtime charges it instead of refunding.
	bad := newStandIn(t, "qwen-plus-2025-09-11", func(string) string { return `{"assignee":"⟦P77⟧"}` })
	if resp, err = hostedForTest(t, bad, io.Discard).Complete(ctx, req); !isProviderCode(err, CodePseudonymUnknown) ||
		resp.InputTokens != 10 || resp.OutputTokens != 5 || resp.Content != "" {
		t.Fatalf("unknown token: %+v %v", resp, err)
	}
}

// hostedRequest is one completion request with the given messages, for the pinned test model.
func hostedRequest(msgs ...Message) ModelRequest {
	return ModelRequest{SchemaVersion: ModelSchemaVersion, Model: "qwen-plus-2025-09-11", PromptVersion: "p", ToolVersion: "t", MaxOutputTokens: 100, Messages: msgs}
}

// WIS-623 review finding 1: known names shorter than three characters ("Li", "GE") and names in
// scripts written without spaces (a two-character Han name inside a sentence) used to reach the
// hosted endpoint unredacted. Ordinary words that merely contain them stay as written.
func TestWIS623ShortAndUnspacedNamesNeverReachTheWire(t *testing.T) {
	s := newStandIn(t, "qwen-plus-2025-09-11", func(string) string { return `{}` })
	m := hostedForTest(t, s, io.Discard)
	ctx := WithPseudonyms(context.Background(), NewPseudonyms([]string{"Li", "GE", "王伟", "P1", "Do"}))
	req := hostedRequest(Message{Role: "system", Content: "Assist the plant."},
		Message{Role: "user", Content: "Li logged a fault for GE at plant P1; 王伟检查了机器, Li说好. Lima and Germany shipped; LIGHT stayed on. LI, li and Ge signed; GEAR age. Do checked it."})
	if _, err := m.Complete(ctx, req); err != nil {
		t.Fatal(err)
	}
	wire := s.calls()[0]
	var sent struct {
		Messages []struct{ Content string } `json:"messages"`
	}
	if err := json.Unmarshal([]byte(wire), &sent); err != nil || len(sent.Messages) != 2 {
		t.Fatalf("wire %s: %v", wire, err)
	}
	user := sent.Messages[1].Content
	for _, raw := range []string{"Li ", "Li说", "for GE", "plant P1", "王伟", "Do ", "LI,", " li ", "Ge "} {
		if strings.Contains(user, raw) {
			t.Fatalf("%q reached the hosted endpoint: %s", raw, user)
		}
	}
	for _, kept := range []string{"Lima", "Germany", "LIGHT", "检查了机器", "GEAR age"} {
		if !strings.Contains(user, kept) {
			t.Fatalf("%q was changed: %s", kept, user)
		}
	}
	// Each spelling has its own token and restores to exactly that spelling (WIS-623 security
	// review: a token never restores to a recorded spelling the request did not contain).
	pp := pseudonymsFrom(ctx)
	for spelling, n := range map[string]int{"Li": 2, "LI": 1, "li": 1} {
		if tok := pp.token[spelling]; tok == "" || strings.Count(user, tok) != n || pp.term[tok] != spelling {
			t.Fatalf("spelling %q: token %q in %s", spelling, tok, user)
		}
	}
	// Every token on the wire is one this request issued (a short term never splits a token).
	for _, tok := range pseudonymToken.FindAllString(user, -1) {
		if _, ok := pseudonymsFrom(ctx).term[tok]; !ok {
			t.Fatalf("token %s was not issued: %s", tok, user)
		}
	}
}

// WIS-623 review finding 2: tool results and earlier tool-call arguments are JSON text, so a name
// is on the wire JSON-escaped ("A&B Motors", "O\"Brien Tools") and a raw dictionary match
// never saw it. Names are now replaced in the decoded string values; keys and numbers stay exact.
func TestWIS623EscapedNamesInToolJSONNeverReachTheWire(t *testing.T) {
	s := newStandIn(t, "qwen-plus-2025-09-11", func(string) string { return `{}` })
	m := hostedForTest(t, s, io.Discard)
	// O'Brien with an apostrophe: a name with a double quote is JSON syntax and is refused in JSON
	// requests instead (Codex round 11, TestWIS623CodexRound11).
	ctx := WithPseudonyms(context.Background(), NewPseudonyms([]string{"A&B Motors", "O'Brien Tools", "Rahul <QA>"}))
	// The tool result is marshalled exactly as tools.go marshals evidence.
	result, err := json.Marshal(struct {
		EvidenceID string        `json:"evidence_id"`
		Revision   int64         `json:"revision"`
		Title      untrustedText `json:"title"`
		Passages   []string      `json:"passages"`
		Count      json.Number
	}{"ev-1", 3, untrustedText{"A&B Motors complaint"}, []string{"O'Brien Tools supplied the die; Rahul <QA> signed."}, "1234567890123"})
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]string{"query": "A&B Motors"})
	req := hostedRequest(Message{Role: "system", Content: "Assist the plant."}, Message{Role: "user", Content: `{"case":"c"}`},
		Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: ToolEvidenceGet, Arguments: args}}},
		Message{Role: "tool", ToolCallID: "c1", Content: string(result)})
	// Security review of the first fix: a known name used as an object key, and a phone number
	// stored as a JSON number, also left the host.
	req.Messages = append(req.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "c2", Name: ToolEvidenceGet, Arguments: json.RawMessage(`{"q":"x"}`)}}},
		Message{Role: "tool", ToolCallID: "c2", Content: `{"by_customer":{"A&B Motors":{"orders":4}},"contact":{"phone":9876543210,"extension":4021}}`})
	if _, err = m.Complete(ctx, req); err != nil {
		t.Fatal(err)
	}
	wire := s.calls()[0]
	for _, raw := range []string{"9876543210", "A&B", `A\u0026B`, `A\\u0026B`, "O\\\"Brien", `O\\\"Brien`, "Brien", "Rahul", "Motors"} {
		if strings.Contains(wire, raw) {
			t.Fatalf("%q reached the hosted endpoint: %s", raw, wire)
		}
	}
	for _, kept := range []string{"evidence_id", "ev-1", "1234567890123", `\"revision\":3`, "supplied the die", `\"orders\":4`, `\"extension\":4021`} {
		if !strings.Contains(wire, kept) {
			t.Fatalf("%q was changed on the wire: %s", kept, wire)
		}
	}
}

// WIS-623 review finding 3: the compiled base URL already ended in /v1 and the adapter adds
// /v1/chat/completions, so production posted to /compatible-mode/v1/v1/chat/completions. The
// stand-in serves the production base path and refuses any other.
func TestWIS623HostedPostsToTheProductionPath(t *testing.T) {
	prod, err := url.Parse(config.HostedEndpoints["qwen-intl"].BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Method != http.MethodPost || r.URL.Path != "/compatible-mode/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"model":"qwen-plus-2025-09-11","choices":[{"message":{"content":"{}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)
	key := filepath.Join(t.TempDir(), "key")
	if err = os.WriteFile(key, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse(srv.URL)
	base.Path = prod.Path // the compiled endpoint's path on the stand-in's host
	m, err := newHostedModel(config.HostedAgent{Provider: "qwen-intl", Model: "qwen-plus-2025-09-11", ModelRevision: "qwen-plus-2025-09-11", APIKeyFile: key},
		base, srv.Client().Transport, 10*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithPseudonyms(context.Background(), NewPseudonyms(nil))
	if _, err = m.Complete(ctx, hostedRequest(Message{Role: "user", Content: "hello"})); err != nil || len(paths) != 1 {
		t.Fatalf("posted to %v: %v", paths, err)
	}
}

// WIS-623 Codex review of 926f66a: phone numbers leaked when (a) stored as an equivalent JSON
// number ("9876543210.0", "9.876543210e9"), (b) adjacent in a contact list, the first match
// consuming the shared separator, or (c) ending a sentence. Asserted on the outbound wire.
func TestWIS623PhonesInListsSentencesAndJSONNumbersNeverReachTheWire(t *testing.T) {
	s := newStandIn(t, "qwen-plus-2025-09-11", func(string) string { return `{}` })
	m := hostedForTest(t, s, io.Discard)
	ctx := WithPseudonyms(context.Background(), NewPseudonyms(nil))
	phones := []string{"9876543210", "8765432109", "7654321098", "6543210987", "9123456780", "8123456790"}
	text := "Call 9876543210 8765432109, 7654321098/6543210987\n9123456780. Phone: 8123456790. Kept: 9876543210.5 1.9876543210 19876543210 100.000000"
	args, _ := json.Marshal(map[string]string{"q": text})
	tool := `{"a":9876543210.0,"b":9.876543210e9,"c":8765432109,"d":[7654321098E0,98765432100e-1,1.5,42,100.000000],"list":` + string(args) + `}`
	req := hostedRequest(Message{Role: "system", Content: "Assist the plant."}, Message{Role: "user", Content: text},
		Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: ToolEvidenceGet, Arguments: args}}},
		Message{Role: "tool", ToolCallID: "c1", Content: tool})
	if _, err := m.Complete(ctx, req); err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Messages []struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Function struct{ Arguments string } `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(s.calls()[0]), &sent); err != nil || len(sent.Messages) != 4 {
		t.Fatalf("wire: %v", err)
	}
	plain, toolJSON, argJSON := sent.Messages[1].Content, sent.Messages[3].Content, sent.Messages[2].ToolCalls[0].Function.Arguments
	notPhones := strings.NewReplacer("9876543210.5", "", "1.9876543210", "", "19876543210", "") // decimals and an 11-digit count
	for _, body := range []string{plain, toolJSON, argJSON} {
		for _, ph := range phones {
			if strings.Contains(notPhones.Replace(body), ph) {
				t.Fatalf("%s reached the hosted endpoint: %s", ph, body)
			}
		}
	}
	for _, kept := range []string{"9876543210.5", "1.9876543210", "19876543210", "100.000000"} {
		if !strings.Contains(plain, kept) {
			t.Fatalf("%q was changed: %s", kept, plain)
		}
	}
	for _, raw := range []string{"9876543210.0", "9.876543210e9", "8765432109", "7654321098E0", "98765432100e-1"} {
		if strings.Contains(toolJSON, raw) {
			t.Fatalf("JSON number %s reached the hosted endpoint: %s", raw, toolJSON)
		}
	}
	for _, kept := range []string{"1.5", "42", "100.000000"} {
		if !strings.Contains(toolJSON, kept) {
			t.Fatalf("number %s was changed: %s", kept, toolJSON)
		}
	}
	// The same value written two ways is one token.
	var nums struct{ A, B string }
	if err := json.Unmarshal([]byte(toolJSON), &nums); err != nil || nums.A == "" || nums.A != nums.B || !pseudonymToken.MatchString(nums.A) {
		t.Fatalf("phone numbers as tokens: %+v %v (%s)", nums, err, toolJSON)
	}
}

// WIS-623 Codex review of 926f66a: a token the model wrote JSON-escaped ("⟦P1⟧")
// passed the literal-token scan and decoded downstream to raw token text. Response strings are
// now restored and validated decoded: a known escaped token is restored, an unknown one refuses.
func TestWIS623EscapedResponseTokensAreRestoredOrRefused(t *testing.T) {
	ctx := WithPseudonyms(context.Background(), NewPseudonyms([]string{"Rahul Deshmukh"}))
	req := hostedRequest(Message{Role: "user", Content: "Rahul Deshmukh reported it."})
	known := newStandIn(t, "qwen-plus-2025-09-11", func(string) string { return `{"who":"\u27e6P1\u27e7","note":"\u27e6P1\u27e7 again"}` })
	resp, err := hostedForTest(t, known, io.Discard).Complete(ctx, req)
	if err != nil || resp.Content != `{"who":"Rahul Deshmukh","note":"Rahul Deshmukh again"}` {
		t.Fatalf("escaped known token: %q %v", resp.Content, err)
	}
	for _, reply := range []string{`{"who":"\u27e6P999\u27e7"}`, `{"\u27e6P999\u27e7":"x"}`, `["ok","\u27e6P1\u27e7","\u27e6P42\u27e7"]`} {
		unknown := newStandIn(t, "qwen-plus-2025-09-11", func(string) string { return reply })
		if _, err = hostedForTest(t, unknown, io.Discard).Complete(ctx, req); !isProviderCode(err, CodePseudonymUnknown) {
			t.Fatalf("escaped unknown token %s: %v", reply, err)
		}
	}
}

// WIS-623 Codex round 2, finding 1: with "Li" and "Li Wei" both known, "Li weighs" used to match
// "Li wei", fail the boundary check, and skip the valid "Li". Plain text and decoded tool JSON.
func TestWIS623OverlappingNamesNeverReachTheWire(t *testing.T) {
	s := newStandIn(t, "qwen-plus-2025-09-11", func(string) string { return `{}` })
	m := hostedForTest(t, s, io.Discard)
	ctx := WithPseudonyms(context.Background(), NewPseudonyms([]string{"Li", "Li Wei", "Ram", "Ramesh Kumar"}))
	text := "Li weighs 5 kg. Li Wei signed. Ramesh checked; Ram Kumar too. Ramp 3 stays."
	tool, _ := json.Marshal(map[string]string{"untrusted_source": text})
	req := hostedRequest(Message{Role: "user", Content: text},
		Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: ToolEvidenceGet, Arguments: json.RawMessage(`{"q":"x"}`)}}},
		Message{Role: "tool", ToolCallID: "c1", Content: string(tool)})
	if _, err := m.Complete(ctx, req); err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Messages []struct{ Content string } `json:"messages"`
	}
	if err := json.Unmarshal([]byte(s.calls()[0]), &sent); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(sent.Messages[2].Content), &decoded); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{sent.Messages[0].Content, decoded["untrusted_source"]} {
		for _, raw := range []string{"Li ", "Ram "} {
			if strings.Contains(body, raw) {
				t.Fatalf("%q reached the hosted endpoint: %s", raw, body)
			}
		}
		for _, kept := range []string{"weighs 5 kg", "Ramesh checked", "Ramp 3"} {
			if !strings.Contains(body, kept) {
				t.Fatalf("%q was changed: %s", kept, body)
			}
		}
	}
}

// WIS-623 Codex round 2, finding 2: JSON fragments inside prose. A phone number written as a
// number in text ("9.876543210e9", "9876543210.0") is replaced like a JSON number; a name that is
// only recognisable after decoding (JSON \uXXXX or \" escapes, HTML entities) refuses the call
// before anything is sent. Huge numbers are not expanded.
func TestWIS623ProseJSONFragmentsNeverReachTheWire(t *testing.T) {
	terms := []string{"A&B Motors", `O"Brien Tools`}
	s := newStandIn(t, "qwen-plus-2025-09-11", func(string) string { return `{}` })
	m := hostedForTest(t, s, io.Discard)
	huge := strings.Repeat("9", 10000)
	plain := `Contact record: {"customer":"A&B Motors","phone":9.876543210e9} and 8765432109.0; ` + "```json\n{\"owner\":\"O\"Brien Tools\",\"mobile\":9876543210E0}\n```" + ` overflow 1e999999999 ` + huge
	ctx := WithPseudonyms(context.Background(), NewPseudonyms(terms))
	if _, err := m.Complete(ctx, hostedRequest(Message{Role: "user", Content: plain})); err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Messages []struct{ Content string } `json:"messages"`
	}
	if err := json.Unmarshal([]byte(s.calls()[0]), &sent); err != nil {
		t.Fatal(err)
	}
	got := sent.Messages[0].Content
	for _, raw := range []string{"A&B", "9.876543210e9", "8765432109", "Brien"} {
		if strings.Contains(got, raw) {
			t.Fatalf("%q reached the hosted endpoint: %s", raw, got[:200])
		}
	}
	if !strings.Contains(got, "1e999999999") || !strings.Contains(got, huge) {
		t.Fatal("a huge number was changed")
	}
	for name, content := range map[string]string{
		"json unicode escape": `Contact record: {"customer":"A` + "BS" + `u0026B Motors"}`,
		"json quote escape":   "```json\n{\"owner\":\"O" + "BS" + `"Brien Tools"}` + "\n```",
		"html entity":         `<td>A&amp;B Motors</td>`,
	} {
		content = strings.ReplaceAll(content, "BS", "\\")
		before := len(s.calls())
		ctx := WithPseudonyms(context.Background(), NewPseudonyms(terms))
		if _, err := m.Complete(ctx, hostedRequest(Message{Role: "user", Content: content})); !isProviderCode(err, CodePseudonymsIncomplete) || len(s.calls()) != before {
			t.Fatalf("%s: %v, sent %d", name, err, len(s.calls())-before)
		}
	}
	// JSON-in-JSON is valid JSON all the way down: it is decoded and hidden, not refused.
	nested := strings.ReplaceAll(`{"note":"{`+"BS"+`"c`+"BS"+`":`+"BS"+`"A`+"BS"+"BS"+`u0026B Motors`+"BS"+`"}"}`, "BS", "\\")
	before := len(s.calls())
	if _, err := m.Complete(WithPseudonyms(context.Background(), NewPseudonyms(terms)), hostedRequest(Message{Role: "user", Content: nested})); err != nil || len(s.calls()) != before+1 {
		t.Fatalf("nested JSON: %v", err)
	}
	if wire := s.calls()[before]; strings.Contains(wire, "u0026B") || strings.Contains(wire, "Motors") {
		t.Fatalf("nested JSON name reached the hosted endpoint: %s", wire)
	}
}

// WIS-623 Codex round 3: matching works on a canonical view, and whatever cannot be canonicalised
// with certainty is refused. Each case is one user message; "hidden" cases must be sent without
// the name, "refused" cases must not be sent at all (PSEUDONYMS_INCOMPLETE).
func TestWIS623CanonicalViewAndLayeredAudit(t *testing.T) {
	terms := []string{"Alice", "A&B Motors", "𠮷田", "Rahul Deshmukh"}
	s := newStandIn(t, "qwen-plus-2025-09-11", func(string) string { return `{}` })
	m := hostedForTest(t, s, io.Discard)
	for _, c := range []struct {
		name, content string
		refused       bool
	}{
		{"full-width", "Ａｌｉｃｅ signed the sheet.", false},
		{"zero-width inside", "A" + "\u200b" + "lice signed; A" + "\u00ad" + "lice too; A" + "\ufeff" + "lice again.", false},
		{"cyrillic look-alike", "\u0410lice signed.", false},
		{"han surrogate-pair name", "𠮷田 signed.", false},
		{"html nested beyond the bound", "Customer A&amp;amp;amp;amp;amp;amp;amp;amp;amp;B Motors.", true},
		{"json escaped surrogate pair", `Owner: BSud842BSudfb7田.`, true},
		{"lone surrogate is lossy", `Owner: BSud842田.`, true},
		{"percent-encoded", "Owner: %41lice.", true},
		{"hex escape", `Owner: BSx41lice.`, true},
		{"brace escape", `Owner: BSu{41}lice.`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			content := strings.ReplaceAll(c.content, "BS", "\\")
			before := len(s.calls())
			_, err := m.Complete(WithPseudonyms(context.Background(), NewPseudonyms(terms)), hostedRequest(Message{Role: "user", Content: content}))
			if c.refused {
				if !isProviderCode(err, CodePseudonymsIncomplete) || len(s.calls()) != before {
					t.Fatalf("not refused: %v, sent %d", err, len(s.calls())-before)
				}
				return
			}
			if err != nil || len(s.calls()) != before+1 {
				t.Fatalf("not sent: %v", err)
			}
			var sent struct {
				Messages []struct{ Content string } `json:"messages"`
			}
			if err = json.Unmarshal([]byte(s.calls()[before]), &sent); err != nil {
				t.Fatal(err)
			}
			got := sent.Messages[0].Content
			if !strings.HasPrefix(got, "⟦P") || strings.Contains(strings.ToLower(got), "lice") || strings.Contains(got, "ｌｉｃｅ") || strings.Contains(got, "田") {
				t.Fatalf("a variant reached the hosted endpoint: %q", got)
			}
		})
	}
}

// WIS-623 Codex round 3, finding 4: a known name equal to a JSON literal ("True", "Null") must not
// refuse a JSON document that uses the literal; JSON is audited by type.
func TestWIS623JSONLiteralsAreNotNames(t *testing.T) {
	s := newStandIn(t, "qwen-plus-2025-09-11", func(string) string { return `{}` })
	m := hostedForTest(t, s, io.Discard)
	ctx := WithPseudonyms(context.Background(), NewPseudonyms([]string{"True", "Null", "False"}))
	req := hostedRequest(Message{Role: "user", Content: `{"ready":true,"x":null,"done":false,"n":[1,2.5]}`},
		Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: ToolEvidenceGet, Arguments: json.RawMessage(`{"strict":true,"limit":null}`)}}},
		Message{Role: "tool", ToolCallID: "c1", Content: `[true,false,null,{"ok":true}]`})
	if _, err := m.Complete(ctx, req); err != nil || len(s.calls()) != 1 {
		t.Fatalf("JSON literals refused: %v", err)
	}
	if !strings.Contains(s.calls()[0], `{\"ready\":true,\"x\":null`) {
		t.Fatalf("literals changed: %s", s.calls()[0])
	}
}

// Ordinary factory content with a realistic dictionary is sent, not refused, and keeps its
// numbers, units and identifiers exact (the audit must not cry wolf).
func TestWIS623OrdinaryFactoryContentIsSent(t *testing.T) {
	terms := []string{"Rahul Deshmukh", "Priya Nair", "Sunil Patil", "Anjali Rao", "Chakan Plant", "Acme Motors", "Tata Autocomp", "Bharat Forge", "Li", "GE", "Wisedo Demo"}
	s := newStandIn(t, "qwen-plus-2025-09-11", func(string) string { return `{}` })
	m := hostedForTest(t, s, io.Discard)
	body, _ := json.Marshal(map[string]any{
		"evidence_id": "5f0c2b7e-8a41-4c3e-9d2a-1b6f0e7c3a90", "revision": 3,
		"sha256": "a3f1c9e0b47d2e5f8a6c1d3b9e0f7a2c4d6e8f0a1b3c5d7e9f0a2b4c6d8e0f1a",
		"title":  "Infeed press SOP rev. C (R&D <-> Production)",
		"passages": []string{
			"Torque the infeed bolts to 25 Nm ±2 Nm before every shift; lubricate the chain with ISO VG 68 oil.",
			"Cycle time target: 42.5 s/part at 98.7% availability; scrap above 1.5% triggers a stop. Batch 2026-11-06T06:00:00Z.",
			"Checked by Rahul Deshmukh with Priya Nair; customer Acme Motors expects 1,200 parts/week. Line LI-04 uses GE drives.",
			"Encoder counts 1234567890 per revolution; spindle 3000 rpm; base URL path /compatible-mode/v1; temperature 85°C.",
			"Maintenance window: Sunday 02:00-04:00 IST. Contact the shift lead via the plant intercom, ext. 4021.",
		},
		"untrusted_source": "Lubrication and torque checks are mandatory. Calibrate the gauge every 30 days (ISO 9001:2015 §7.1.5).",
	})
	req := hostedRequest(Message{Role: "system", Content: "You assist the plant supervisor. Answer only from the evidence; cite span IDs."},
		Message{Role: "user", Content: `{"question":"What torque do the infeed bolts need, and who checked it?"}`},
		Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: ToolEvidenceGet, Arguments: json.RawMessage(`{"evidence_id":"5f0c2b7e-8a41-4c3e-9d2a-1b6f0e7c3a90"}`)}}},
		Message{Role: "tool", ToolCallID: "c1", Content: string(body)})
	if _, err := m.Complete(WithPseudonyms(context.Background(), NewPseudonyms(terms)), req); err != nil || len(s.calls()) != 1 {
		t.Fatalf("ordinary content refused: %v", err)
	}
	wire := s.calls()[0]
	for _, kept := range []string{"25 Nm", "42.5 s/part", "98.7%", "1234567890", "a3f1c9e0b47d2e5f", "ISO 9001:2015", "ext. 4021", "-04 uses"} { // "LI" itself is a known name: tokenised by design
		if !strings.Contains(wire, kept) {
			t.Fatalf("%q was changed: %s", kept, wire)
		}
	}
	for _, raw := range []string{"Rahul", "Priya", "Acme"} {
		if strings.Contains(wire, raw) {
			t.Fatalf("%q reached the hosted endpoint", raw)
		}
	}
}

// WIS-623 security review of c0ed05f: (1) look-alike folding ran before lower-casing and missed
// lower-case Cyrillic/Greek letters, so mixed-case look-alike names escaped; (2) encoded runs that
// did not decode cleanly were audited as written. Asserted on the outbound wire.
func TestWIS623MixedCaseLookalikesAndNoisyEncodings(t *testing.T) {
	terms := []string{"Nina", "Victor", "Hema", "Bob", "Alice"}
	s := newStandIn(t, "qwen-plus-2025-09-11", func(string) string { return `{}` })
	m := hostedForTest(t, s, io.Discard)
	for _, c := range []struct {
		name, content string
		refused       bool
	}{
		{"greek capitals", "ΝΙΝΑ signed.", false},
		{"greek small nu", "νictor signed.", false},
		{"cyrillic capitals", "НЕМА signed.", false},
		{"cyrillic mixed case", "hЕмА signed.", false},
		{"cyrillic small ve", "вОв signed.", false},
		{"greek small beta, capital eta and mu", "βοβ signed; ΗΕΜΑ too.", false},
		{"percent next to an invalid byte", "Owner: %41%6C%69%63%65 and %FF.", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := len(s.calls())
			_, err := m.Complete(WithPseudonyms(context.Background(), NewPseudonyms(terms)), hostedRequest(Message{Role: "user", Content: c.content}))
			if c.refused {
				if !isProviderCode(err, CodePseudonymsIncomplete) || len(s.calls()) != before {
					t.Fatalf("not refused: %v, sent %d", err, len(s.calls())-before)
				}
				return
			}
			if err != nil || len(s.calls()) != before+1 {
				t.Fatalf("not sent: %v", err)
			}
			var sent struct {
				Messages []struct{ Content string } `json:"messages"`
			}
			if err = json.Unmarshal([]byte(s.calls()[before]), &sent); err != nil {
				t.Fatal(err)
			}
			if got := sent.Messages[0].Content; !strings.HasPrefix(got, "⟦P") || strings.TrimPrefix(got[strings.Index(got, "⟧")+len("⟧"):], " ") != "signed." && !strings.HasSuffix(got, "too.") || strings.ContainsAny(got, "βοΗΕΜΑ") {
				t.Fatalf("a look-alike reached the hosted endpoint: %q", got)
			}
		})
	}
}

// WIS-623 Codex round 4: (1) a byte escape that is not UTF-8 ("Jos\xE9", "Jos%E9") was dropped,
// so the audit saw "Jos"; it is now read as Latin-1. (2) Token delimiters recovered by decoding
// were stripped as if they were issued tokens; only this request's own tokens, found exactly in
// the outbound text, are set aside, and any other delimiter refuses. (3) Speculative base64
// decoding is removed: ordinary words are not decoded into names (a stated limit, rule 08).
func TestWIS623Latin1EscapesForgedTokensAndNoBase64(t *testing.T) {
	terms := []string{"José", "DBD", "Rahul Deshmukh", "Alice"}
	s := newStandIn(t, "qwen-plus-2025-09-11", func(string) string { return `{}` })
	m := hostedForTest(t, s, io.Discard)
	for _, c := range []struct {
		name, content string
		refused       bool
	}{
		{"latin-1 hex escape", `Owner: JosBSxE9.`, true},
		{"latin-1 percent escape", `Owner: Jos%E9.`, true},
		{"latin-1 escape is refused (accepted false refusal)", `Owner: Ren%E9 signed.`, true},
		{"invalid byte between letters", `Owner: Al%FFice.`, true},
		{"invalid byte after a name", `Owner: Alice%FF.`, true},
		{"invalid hex byte between letters", `Owner: AlBSxFFice.`, true},
		{"invalid byte glued to a name (security review of 1222e5b)", `Owner: %41%6C%69%63%65%FF.`, true},
		{"invalid byte inside a name", `Owner: %41%6C%FF%69%63%65.`, true},
		{"non-letter invalid byte is lossy", `Owner: shift %80 report.`, true},
		{"invalid hex byte glued to a name", `Owner: BSx41BSx6cBSx69BSx63BSx65BSxff.`, true},
		{"utf-8 percent escape", `Owner: Jos%C3%A9.`, true},
		{"escaped token delimiters", `Owner: BSu27e6P1BSu27e7 approved.`, true},
		{"html token delimiters", `Owner: &#x27e6;P1&#x27e7; approved.`, true},
		{"ordinary word that is valid base64", `{"status":"REJECTED","reason":"torque out of range"}`, false},
		{"issued token stays", `Rahul Deshmukh approved.`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			content := strings.ReplaceAll(c.content, "BS", "\\")
			before := len(s.calls())
			_, err := m.Complete(WithPseudonyms(context.Background(), NewPseudonyms(terms)), hostedRequest(Message{Role: "user", Content: content}))
			if c.refused {
				if !isProviderCode(err, CodePseudonymsIncomplete) || len(s.calls()) != before {
					t.Fatalf("not refused: %v, sent %d", err, len(s.calls())-before)
				}
				return
			}
			if err != nil || len(s.calls()) != before+1 {
				t.Fatalf("refused: %v", err)
			}
		})
	}
	if !strings.Contains(strings.Join(s.calls(), "\n"), `\"status\":\"REJECTED\"`) {
		t.Fatal("REJECTED was not sent as written")
	}
}

// Security review of 6f76551: the per-reading recursive re-audit could take exponential work.
// The audit has one decode path and one pass budget: a thousand nested and ambiguous escapes are
// refused quickly. (A wall-clock bound is the point of this test. One run takes 10-20 ms; the
// exponential re-audit this guards against takes minutes on the same input. The bound is 2 s, not
// 100 ms, because the full integration run (every package at once) took 115 ms on a loaded
// machine and failed with the right refusal code.)
func TestWIS623AuditBudgetUnderNestedEscapes(t *testing.T) {
	p := NewPseudonyms([]string{"Alice", "José"})
	for name, content := range map[string]string{
		"nested percent":  strings.Repeat("%25", 1000) + "41lice",
		"ambiguous bytes": strings.Repeat("%E9%FF%41", 1000),
		"nested html":     "&" + strings.Repeat("amp;", 1000) + "#65;lice",
	} {
		start := time.Now()
		_, err := p.HideRequest(hostedRequest(Message{Role: "user", Content: content}))
		if elapsed := time.Since(start); !isProviderCode(err, CodePseudonymsIncomplete) || elapsed > 2*time.Second {
			t.Fatalf("%s: %v after %v", name, err, elapsed)
		}
	}
}

// TestWIS623PseudonymTermBounds (WIS-623 security review): customer names of every site of the
// tenant feed the matcher, so its input is bounded on distinct terms. Past a bound the hosted call
// is refused (nothing is dropped and sent in clear). The SQL read caps distinct stored spellings;
// here, after it, spellings that read the same on the canonical view (case variants) count once.
func TestWIS623PseudonymTermBounds(t *testing.T) {
	defer func(n int) { maxPseudonymTerms = n }(maxPseudonymTerms)
	maxPseudonymTerms = 50
	var repeated []string // the same 21 names stored in two cases (the SQL read returns distinct spellings)
	for i := range 2 {
		for k := range 21 {
			name := fmt.Sprintf("Customer %c%c", 'a'+k, 'a'+k)
			if i%2 == 1 {
				name = strings.ToUpper(name)
			}
			repeated = append(repeated, name)
		}
	}
	greek := make([]string, 17) // 17 stored, each one trie entry (its readings are sets per slot)
	for i := range greek {
		greek[i] = fmt.Sprintf("Νινα%c", 'a'+i)
	}
	distinct := make([]string, 51)
	for i := range distinct {
		distinct[i] = fmt.Sprintf("Customer %c%c", 'a'+i/26, 'a'+i%26)
	}
	for name, c := range map[string]struct {
		terms   []string
		refused bool
	}{
		"case variants count once on the canonical view":   {terms: repeated},
		"more distinct than the cap":                       {terms: distinct, refused: true},
		"a term longer than the contract":                  {terms: []string{strings.Repeat("é", MaxTermRunes+1)}, refused: true},
		"short names are kept":                             {terms: []string{"GE", "Li"}},
		"one entry per stored term, whatever its readings": {terms: greek},
		"runes canonicalised are bounded":                  {terms: slices.Repeat([]string{strings.Repeat("x", MaxTermRunes)}, 51), refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			p, err := NewPseudonymsFor(c.terms)
			if c.refused != (err != nil) || c.refused && !errors.Is(err, ErrPseudonymTerms) {
				t.Fatalf("%v %v", p, err)
			}
		})
	}
}

// worstCaseTerms is the largest term set allowed: maxPseudonymTerms distinct 200-letter terms
// without a shared prefix (seeded, so every run builds the same set).
func worstCaseTerms() []string {
	r := rand.New(rand.NewPCG(1, 2))
	terms := make([]string, maxPseudonymTerms)
	for i := range terms {
		b := make([]byte, MaxTermRunes)
		for k := range b {
			b[k] = byte('a' + r.IntN(26))
		}
		terms[i] = string(b)
	}
	return terms
}

// TestWIS623PseudonymWorstCaseCompiles: the largest term set allowed builds a matcher, so the
// bound never turns into a refusal of its own.
func TestWIS623PseudonymWorstCaseCompiles(t *testing.T) {
	if p, err := NewPseudonymsFor(worstCaseTerms()); err != nil || p == nil || p.count != maxPseudonymTerms {
		t.Fatalf("worst case: %v", err)
	}
}

// BenchmarkPseudonymWorstCaseBuild measures the build time of the largest ASCII term set allowed
// (development workstation, 2026-11-08, reading-set trie with mark slots: about 0.33 s per build). 4000 terms of
// 200 random Greek and Cyrillic letters build in about 0.7 s, and a 33 KB mixed-script request
// is then hidden in about 0.08 s.
// Run with: go test -run '^$' -bench PseudonymWorstCaseBuild ./internal/agent
func BenchmarkPseudonymWorstCaseBuild(b *testing.B) {
	terms := worstCaseTerms()
	for b.Loop() {
		if _, err := NewPseudonymsFor(terms); err != nil {
			b.Fatal(err)
		}
	}
}

// TestWIS623CaseVariantsOfLookalikeNames (WIS-623 Codex rounds 3 and 4): the canonical view folds
// Greek and Cyrillic lookalikes per case ("ΝΙΝΑ" reads "nina", "νινα" reads "viva"), so a name
// stored in one case must still be found in the other, including a mixed Greek/Latin spelling.
func TestWIS623CaseVariantsOfLookalikeNames(t *testing.T) {
	for _, c := range []struct{ stored, mention string }{
		{"ΝΙΝΑ", "νινα"},
		{"νινα", "ΝΙΝΑ"},
		{"ΝΙΝΑ", "Νινα"},
		{"νινα", "νιΝα"},
		{"NΙνA Tools", "nινa tools"}, // Latin N and A around Greek iota and nu
		{"nινa tools", "NΙΝA TOOLS"},
		// Codex round 5: final sigma, dotless i, U+1C80 small rounded ve, and a format
		// character that kept a mark from composing.
		{"ΝΙΚΟΣ", "νικος"},
		{"νικος", "ΝΙΚΟΣ"},
		{"ırmak", "IRMAK"},
		{"IRMAK", "ırmak"},
		{"Вера", "ᲀера"},
		{"A\u200b\u0301lice", "\u00c1lice"},
		{"\u00c1lice", "A\u200b\u0301lice"},
		{"Nina", "ΝΙΝΑ"},
		{"Victor", "νictor"},
		// Security review of 8aafb153: full folding writes İ as "i" plus a combining dot.
		{"Ilker", "İlker"},
		{"Ilker", "İLKER"},
		{"İlker", "Ilker"},
		{"İLKER", "ilker"},
		{"ǰason", "J\u030cASON"}, // j plus caron composes to ǰ after folding
		{"ꭰavid", "Ꭰavid"},       // Cherokee: x/text folds small and capital the other way round
	} {
		p := NewPseudonyms([]string{c.stored})
		out, err := p.HideRequest(hostedRequest(Message{Role: "user", Content: "Order for " + c.mention + " shipped."}))
		if err != nil || strings.Contains(out.Messages[0].Content, c.mention) || !strings.Contains(out.Messages[0].Content, "⟦P1⟧ shipped") {
			t.Fatalf("stored %q, mentioned %q: %+v %v", c.stored, c.mention, out.Messages, err)
		}
	}
}

// TestWIS623TermWorkIsConstant (WIS-623 security reviews of d1ef27a7 and Codex round 6): the
// case-variant expansion is gone. Every term yields at most four keys (visual as stored, upper,
// lower; folded), so the work is linear in the terms whatever their letters; the Codex round 6
// set (six case spellings of a Greek name plus "Яa" to "Яe", cap 50) is no longer a budget edge.
func TestWIS623TermWorkIsConstant(t *testing.T) {
	defer func(n int) { maxPseudonymTerms = n }(maxPseudonymTerms)
	maxPseudonymTerms = 50
	terms := []string{"ΝΥΝΥ", "νΥΝΥ", "ΝυΝΥ", "ΝΥνΥ", "ΝΥΝυ", "νυνυ", "Яa", "Яb", "Яc", "Яd", "Яe"}
	p, err := NewPseudonymsFor(terms)
	if err != nil || p.count > len(terms) {
		t.Fatalf("%v, %d terms", err, p.count)
	}
	long := []string{strings.Repeat("Ν", MaxTermRunes)} // 2^200 case combinations before
	if p, err = NewPseudonymsFor(long); err != nil || p.count != 1 {
		t.Fatalf("long ambiguous name: %v", err)
	}
}

// TestWIS623CanonicalViewControls: what the views must not do. ASCII is never mapped through the
// UTS #39 data, so ordinary words that are confusable only in ASCII ("rn" and "m", "1" and "l",
// "0" and "O") do not meet a name; and each view of a view is itself, which the residual audit
// relies on.
func TestWIS623CanonicalViewControls(t *testing.T) {
	for _, c := range []struct{ term, text string }{
		{"Ram", "Rarn reported."},
		{"Bill", "Bi11 reported."},
		{"Otto", "0tto reported."},
		{"Nina", "vivid reported."},
		{"Ivan", "lvan reported."},
	} {
		p := NewPseudonyms([]string{c.term})
		if out := p.hide(c.text); out != c.text {
			t.Fatalf("term %q changed %q to %q", c.term, c.text, out)
		}
	}
	for _, s := range []string{"ΝΙΚΟΣ νικος", "ırmak IRMAK", "ᲀера", "A\u200b\u0301lice", "Ｒａｈｕｌ", "rnodern 10 m", "Straße ﬁle ǅ", "⟦P1⟧ said"} {
		for _, k := range []viewKind{visualView, foldedView} {
			if c := canonicalString(s, k); canonicalString(c, k) != c {
				t.Fatalf("view %d of %q is not stable: %q -> %q", k, s, c, canonicalString(c, k))
			}
		}
	}
}

// TestWIS623FoldedViewIsCaseConsistent (security review of 8aafb153): every letter in the
// Basic Multilingual Plane and the Supplementary Multilingual Plane reads the same on the folded
// view as each of its upper, lower and title case forms, so a name stored in one case is found
// in any other (İ against I, Cherokee small against capital letters).
func TestWIS623FoldedViewIsCaseConsistent(t *testing.T) {
	for r := rune(0); r <= 0x1FFFF; r++ {
		if !unicode.IsLetter(r) {
			continue
		}
		want := canonicalString(string(r)+"x", foldedView)
		for _, v := range []string{string(unicode.ToLower(r)), string(unicode.ToUpper(r)), string(unicode.ToTitle(r)), strings.ToUpper(string(r)), strings.ToLower(string(r))} {
			if got := canonicalString(v+"x", foldedView); got != want {
				t.Fatalf("U+%04X %q reads %q, its case form %q reads %q", r, string(r), want, v, got)
			}
		}
	}
}

// TestWIS623AuditRefusesMissedDottedCapital: an escaped İ that hiding cannot see is refused by
// the audit, whose folded view reads it like the stored name.
func TestWIS623AuditRefusesMissedDottedCapital(t *testing.T) {
	p := NewPseudonyms([]string{"Ilker"})
	for _, content := range []string{`Owner: %C4%B0lker.`, `Owner: &#304;LKER.`, `Owner: \u0130lker.`} {
		if _, err := p.HideRequest(hostedRequest(Message{Role: "user", Content: content})); !isProviderCode(err, CodePseudonymsIncomplete) {
			t.Fatalf("%s: %v", content, err)
		}
	}
}

// TestWIS623CodexRound7 (WIS-623 Codex round 7 on 8aafb153): each stored name is hidden in the
// request text exactly as asserted, and each case failed before its fix.
func TestWIS623CodexRound7(t *testing.T) {
	for _, c := range []struct {
		name   string
		stored []string
		text   string
		want   string
	}{
		// Finding 2: eligibility is decided on the normalised name.
		{"enclosed letters stored", []string{"Ⓐⓛⓘⓒⓔ"}, "Alice called.", "⟦P1⟧ called."},
		{"enclosed letters sent", []string{"Alice"}, "Ⓐⓛⓘⓒⓔ called.", "⟦P1⟧ called."},
		// Finding 3: a mapped mark composes with the letter before it, as in the request.
		{"mapped mark stored", []string{"A\u0e4dke"}, "Åke called.", "⟦P1⟧ called."},
		{"mapped mark sent", []string{"Åke"}, "A\u0e4dke called.", "⟦P1⟧ called."},
		// Finding 1: a shorter match must not hide a longer one that overlaps it.
		{"longest match wins", []string{"ΝΙΝΑ Smith", "Smith"}, "Νινα Smith called.", "⟦P1⟧ called."},
		{"shorter name alone", []string{"ΝΙΝΑ Smith", "Smith"}, "Smith called.", "⟦P1⟧ called."},
		// Finding 4: a mixed spelling of capital and small look-alikes of a Greek name.
		{"latin capital, greek small", []string{"ΝΙΝΑ"}, "Nινα called.", "⟦P1⟧ called."},
		{"latin spelling of mixed greek", []string{"ΝΙΝΑ"}, "Niva called.", "⟦P1⟧ called."},
		{"greek mixed against latin", []string{"Nina"}, "Νινα called.", "⟦P1⟧ called."},
		{"latin control", []string{"Nina"}, "Viva called.", "Viva called."},
		{"sharp s spans two slots", []string{"Strasse"}, "Straße called.", "⟦P1⟧ called."},
		// Security review of the cluster merge: a mark-like character appended to a name.
		{"mapped mark appended", []string{"Nina"}, "Nina\u0e4d called.", "⟦P1⟧ called."},
		{"combining accent appended", []string{"Nina"}, "Nina\u0301 called.", "⟦P1⟧ called."},
		{"accented name stored, written so", []string{"Ninå"}, "Ninå called.", "⟦P1⟧ called."},
		{"accented name stored, mark-built", []string{"Ninå"}, "Nina\u0e4d called.", "⟦P1⟧ called."},
		{"devanagari keeps its marks", []string{"राहुल"}, "राहल called.", "राहल called."},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, err := NewPseudonymsFor(c.stored)
			if err != nil {
				t.Fatal(err)
			}
			if got := p.hide(c.text); got != c.want {
				t.Fatalf("stored %q: %q became %q, want %q", c.stored, c.text, got, c.want)
			}
		})
	}
}

// TestWIS623LetterlessNames (WIS-623 security review of 010223a6): a stored name without a
// letter neither leaves in clear nor stops the site's hosted calls. One without digits is hidden
// byte for byte as a whole token and restored exactly; one with digits is not hidden (counts stay
// exact) but a request that holds it as a whole token is refused. Token delimiters in a stored
// name read as the plain brackets the request text is given.
func TestWIS623LetterlessNames(t *testing.T) {
	p := NewPseudonyms([]string{"Alice", "42", "★★", "⟦Bob⟧"})
	for _, c := range []struct {
		content, sent string
		refused       bool
	}{
		{"Alice counted 420 parts.", "⟦P1⟧ counted 420 parts.", false},
		{"Alice counted 42 parts.", "", true},
		{"Owner ★★ approved.", "Owner ⟦P1⟧ approved.", false},
		{"Owner ★★★ approved.", "Owner ★★★ approved.", false},
		{"Owner ⟦Bob⟧ approved.", "Owner ⟦P1⟧ approved.", false},
	} {
		q := p.fresh()
		out, err := q.HideRequest(hostedRequest(Message{Role: "user", Content: c.content}))
		if c.refused {
			if !isProviderCode(err, CodePseudonymsIncomplete) {
				t.Fatalf("%q: %v", c.content, err)
			}
			continue
		}
		if err != nil || out.Messages[0].Content != c.sent {
			t.Fatalf("%q: sent %+v %v, want %q", c.content, out.Messages, err, c.sent)
		}
		if back, err := q.RestoreResponse(ModelResponse{Content: out.Messages[0].Content}); err != nil || back.Content != tokenBrackets.Replace(c.content) {
			t.Fatalf("%q: restored %q %v", c.content, back.Content, err)
		}
	}
}

// TestWIS623TrailingMarksNeverHideAName (WIS-623 security review of 010223a6, HIGH): a mark
// appended to a known name used to lengthen the last cluster's reading so the match could not end
// there. Every nonspacing and enclosing mark appended to a stored name now leaves it hidden.
func TestWIS623TrailingMarksNeverHideAName(t *testing.T) {
	for _, c := range []struct{ stored, text string }{
		{"Max", "Max\u0e4d called."},
		{"Nina", "Nina\u0310 called."},
		{"Nina", "Nina\u0336 called."},
		{"राहुल", "राहुल\u0310 called."},
	} {
		if got := NewPseudonyms([]string{c.stored}).hide(c.text); got != "⟦P1⟧ called." {
			t.Fatalf("stored %q: %q became %q", c.stored, c.text, got)
		}
	}
	p := NewPseudonyms([]string{"Nina", "राहुल"})
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !unicode.In(r, unicode.Mn, unicode.Me) {
			continue
		}
		for _, name := range []string{"Nina", "राहुल"} {
			q := &Pseudonyms{names: p.names, count: p.count, token: map[string]string{}, term: map[string]string{}}
			if got := q.hide(name + string(r) + " called."); strings.Contains(got, name) || q.overBudget {
				t.Fatalf("U+%04X after %q: %q", r, name, got)
			}
		}
	}
}

// TestWIS623OverlappingNamesAreHiddenWhole (security review of 010223a6): overlapping candidates
// are replaced as one span, so no part of a known name stays in clear, and the span restores
// exactly.
func TestWIS623OverlappingNamesAreHiddenWhole(t *testing.T) {
	p := NewPseudonyms([]string{"Ram Kumar", "Kumar Singh"})
	if got := p.hide("Ram Kumar Singh called."); got != "⟦P1⟧ called." || p.term["⟦P1⟧"] != "Ram Kumar Singh" {
		t.Fatalf("%q, %v", got, p.term)
	}
}

// TestWIS623MergedMarksAreBounded (security review of the cluster merge): at most maxMergedMarks
// mark clusters merge into one letter, as stream-safe normalisation allows; a request or a stored
// name with more is refused, never read in part.
func TestWIS623MergedMarksAreBounded(t *testing.T) {
	marks := func(n int) string { return "Nina" + strings.Repeat("\u0e4d", n) }
	p := NewPseudonyms([]string{"Alice"})
	if _, err := p.HideRequest(hostedRequest(Message{Role: "user", Content: marks(maxMergedMarks) + " called."})); err != nil {
		t.Fatalf("within the bound: %v", err)
	}
	if _, err := p.HideRequest(hostedRequest(Message{Role: "user", Content: marks(maxMergedMarks+1) + " called."})); !isProviderCode(err, CodePseudonymsIncomplete) {
		t.Fatalf("past the bound: %v", err)
	}
	if _, err := NewPseudonymsFor([]string{marks(maxMergedMarks + 1)}); !errors.Is(err, ErrPseudonymTerms) {
		t.Fatalf("stored past the bound: %v", err)
	}
}

// TestWIS623LetterlessNameAudit (WIS-623 security review of e59e7736): a letterless name with
// digits is refused however the request writes it (full-width, with a zero-width space, a
// variation selector or a mark at its edge, or as a JSON number of the same value); and a name
// made only of the detectors' separators is skipped and counted.
func TestWIS623LetterlessNameAudit(t *testing.T) {
	p := NewPseudonyms([]string{"Alice", "42", "★"})
	for _, c := range []struct {
		content string
		refused bool
	}{
		{"Count ４２ parts.", true},
		{"Count 4​2 parts.", true},
		{"Count 42️ parts.", true},
		{"Count 42́ parts.", true},
		{`{"count":42}`, true},
		{`{"count":42.0}`, true},
		{`{"count":4.2e1}`, true},
		{`{"count":43}`, false},
		{"Count 420 parts.", false},
		{"Rated ★ by Alice.", false},
	} {
		q := p.fresh()
		_, err := q.HideRequest(hostedRequest(Message{Role: "user", Content: c.content}))
		if c.refused != isProviderCode(err, CodePseudonymsIncomplete) || !c.refused && err != nil {
			t.Fatalf("%q: %v", c.content, err)
		}
	}
	// A name made only of separators identifies no one: it is skipped and counted, and the site's
	// calls go on (lead decision on 3227f17a); a phone number keeps its separators and is hidden.
	q, err := NewPseudonymsFor([]string{"Alice", "-", "( )", "+", ".", "-.-"})
	if err != nil || q.SkippedSeparatorNames != 5 || q.count != 1 {
		t.Fatalf("separator-only names: %v, skipped %d, %d terms", err, q.SkippedSeparatorNames, q.count)
	}
	if out, err := q.HideRequest(hostedRequest(Message{Role: "user", Content: "Alice: +91 98765-43210."})); err != nil || out.Messages[0].Content != "⟦P1⟧: ⟦P2⟧." {
		t.Fatalf("phone with separators: %+v %v", out.Messages, err)
	}
}

// TestWIS623CodexRound8 (WIS-623 Codex round 8 on 010223a6). Trailing marks, overlapping names
// and walk dedup were fixed in e59e7736; a name around an e-mail address is fixed by reading
// every detector on the same text and joining overlapping places.
func TestWIS623CodexRound8(t *testing.T) {
	for _, c := range []struct {
		name   string
		stored []string
		text   string
		want   string
	}{
		{"round 8: Max+U+0E4D", []string{"Max"}, "Max\u0e4d called.", "⟦P1⟧ called."},
		{"round 8: Nina+U+0338", []string{"Nina"}, "Nina\u0338 called.", "⟦P1⟧ called."},
		{"round 8: Alice Beth / Beth Cooper", []string{"Alice Beth", "Beth Cooper"}, "Alice Beth Cooper called.", "⟦P1⟧ called."},
		{"round 8: dedup of trie paths", []string{"ΝΙΝΑ", "NINA", "nina", "νινα", "Nina", "vina", "viva"}, "Νινα called.", "⟦P1⟧ called."},
		{"round 8: name around an e-mail", []string{"Alice alice@example.com"}, "Write to Alice alice@example.com today.", "Write to ⟦P1⟧ today."},
		{"e-mail next to a name", []string{"Alice"}, "Alice alice@example.com wrote.", "⟦P1⟧ ⟦P2⟧ wrote."},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := NewPseudonyms(c.stored)
			if got := p.hide(c.text); got != c.want || p.overBudget {
				t.Fatalf("%q became %q (over budget %v), want %q", c.text, got, p.overBudget, c.want)
			}
		})
	}
}

// TestWIS623NestedJSONRestoresEachLayer (WIS-623 Codex round 8): a name inside JSON that is
// itself a string of JSON is hidden layer by layer, and an answer echoing it is restored layer
// by layer, so the inner quote stays escaped and both layers still parse.
func TestWIS623NestedJSONRestoresEachLayer(t *testing.T) {
	// "A&B <Tools>" is escaped at each layer (\u0026, \u003c); a name with JSON syntax is
	// refused in JSON instead (Codex round 11).
	inner, _ := json.Marshal(map[string]string{"supplier": "A&B <Tools>"})
	outer, _ := json.Marshal(map[string]string{"note": string(inner)})
	p := NewPseudonyms([]string{"A&B <Tools>"})
	hidden, err := p.HideRequest(hostedRequest(Message{Role: "user", Content: string(outer)}))
	if err != nil || strings.Contains(hidden.Messages[0].Content, "Tools") {
		t.Fatalf("hide: %+v %v", hidden.Messages, err)
	}
	back, err := p.RestoreResponse(ModelResponse{Content: hidden.Messages[0].Content})
	if err != nil {
		t.Fatal(err)
	}
	var o, i map[string]string
	if err = json.Unmarshal([]byte(back.Content), &o); err != nil {
		t.Fatalf("outer %q: %v", back.Content, err)
	}
	if err = json.Unmarshal([]byte(o["note"]), &i); err != nil || i["supplier"] != "A&B <Tools>" {
		t.Fatalf("inner %q: %v %v", o["note"], i, err)
	}
}

// TestWIS623RefusedNumberWorkIsBounded (security review of 3227f17a): comparing JSON numbers with
// number names never expands an exponent or parses a big number, and compares exact values: a long
// mantissa that offsets a large exponent still equals the name.
func TestWIS623RefusedNumberWorkIsBounded(t *testing.T) {
	huge := `{"n":` + strings.Repeat("7", 100_000) + `}`
	for _, c := range []struct {
		terms   []string
		content string
		refused bool
	}{
		{[]string{"Alice"}, `{"n":1e999999999}`, false},
		{[]string{"Alice"}, huge, false},
		{[]string{"Alice", "42"}, `{"n":1e999999999}`, false},
		{[]string{"Alice", "42"}, `{"n":0e-999999999}`, false},
		{[]string{"Alice", "0"}, `{"n":0e-999999999}`, true},
		{[]string{"Alice", "42"}, huge, false},
		{[]string{"Alice", "42"}, `{"n":4.2e1}`, true},
		// Security review of 2a73a787: a long mantissa offsets a large exponent.
		{[]string{"Alice", "42"}, `{"n":420e-1}`, true},
		{[]string{"Alice", "42"}, `{"n":0.42e2}`, true},
		{[]string{"Alice", "42"}, `{"n":4200000000e-8}`, true},
		{[]string{"Alice", "42"}, `{"n":42` + strings.Repeat("0", 200) + `e-200}`, true},
		{[]string{"Alice", "42"}, `{"n":0.` + strings.Repeat("0", 5000) + `42e5002}`, true},
		{[]string{"Alice", "42"}, `{"n":4.2e-999999999}`, false},
		{[]string{"Alice", "42"}, `{"n":-42}`, true}, // read as text too (Codex round 9): "42" stands there as a token
		{[]string{"Alice", "-42"}, `{"n":-42.0}`, true},
		{[]string{"Alice", "42"}, `{"n":43}`, false},
	} {
		p := NewPseudonyms(c.terms)
		_, err := p.HideRequest(hostedRequest(Message{Role: "user", Content: c.content}))
		if c.refused != isProviderCode(err, CodePseudonymsIncomplete) || !c.refused && err != nil {
			t.Fatalf("%q with %.40q: %v", c.terms, c.content, err)
		}
	}
}

// TestWIS623CodexRound9 (WIS-623 Codex round 9 on 2a73a787): each case failed before its fix.
func TestWIS623CodexRound9(t *testing.T) {
	sent := func(t *testing.T, terms []string, content string) (string, error) {
		t.Helper()
		p := NewPseudonyms(terms)
		out, err := p.HideRequest(hostedRequest(Message{Role: "user", Content: content}))
		if err != nil {
			return "", err
		}
		return out.Messages[0].Content, nil
	}
	// Finding 2: a stored name shaped like JSON is read as text as well.
	if _, err := sent(t, []string{"[42]"}, `{"customer":"[42]"}`); !isProviderCode(err, CodePseudonymsIncomplete) {
		t.Fatalf("[42]: %v", err)
	}
	// A name shaped like JSON inside a string that is JSON is refused, not guessed (Codex round 10).
	for _, name := range []string{"[]", "{}"} {
		if got, err := sent(t, []string{name}, `{"customer":"`+name+`"}`); !isProviderCode(err, CodePseudonymsIncomplete) {
			t.Fatalf("%s: %q %v", name, got, err)
		}
	}
	// Finding 3: the refusal reads the original text, before a phone token can take part of it.
	if _, err := sent(t, []string{"42 9876543210"}, "Call 42 9876543210 today."); !isProviderCode(err, CodePseudonymsIncomplete) {
		t.Fatalf("42 9876543210: %v", err)
	}
	// Finding 4: overlapping occurrences of a letterless name.
	if got, err := sent(t, []string{"★ ★"}, "Rated ★ ★ ★ today."); err != nil || got != "Rated ⟦P1⟧ today." {
		t.Fatalf("★ ★: %q %v", got, err)
	}
	// Finding 6: one token stands for at most maxTokenSpan bytes; restoration is bounded.
	long := strings.Repeat("Alice ", maxTokenSpan/6+1)
	if _, err := sent(t, []string{"Alice"}, long); err != nil {
		t.Fatalf("separate names are separate tokens: %v", err)
	}
	// "Ann Bob" and "Bob Ann" overlap all along "Ann Bob Ann Bob ...": one join, too long.
	if _, err := sent(t, []string{"Ann Bob", "Bob Ann"}, strings.Repeat("Ann Bob ", maxTokenSpan/8+1)); !isProviderCode(err, CodePseudonymsIncomplete) {
		t.Fatalf("one join past maxTokenSpan: %v", err)
	}
	p := NewPseudonyms([]string{"Alice"})
	hidden := p.hide(strings.Repeat("Alice ", 400))
	tok := hidden[:strings.Index(hidden, "⟧")+len("⟧")]
	p.term[tok] = strings.Repeat("x", maxTokenSpan)
	if _, err := p.RestoreResponse(ModelResponse{Content: strings.Repeat(tok, 100_000)}); !isProviderCode(err, CodeProviderResponseLarge) {
		t.Fatalf("restore expansion: %v", err)
	}
	// A real name repeated within the bound restores (a term that no name matches, as the
	// synthetic one above, would fail the round-trip check since Codex round 18).
	r := NewPseudonyms([]string{"Alice"})
	rt := r.hide("Alice")
	if _, err := r.RestoreResponse(ModelResponse{Content: strings.Repeat(rt+" ", 10)}); err != nil {
		t.Fatalf("restore within the bound: %v", err)
	}
}

// TestWIS623AttachedMarksNeverHideAName extends the trailing-mark sweep (WIS-623 Codex round 9):
// every mark of any kind (Mn, Mc, Me) and every character with a non-zero combining class,
// appended to a Latin or a Devanagari name, leaves it hidden; Devanagari vowel signs inside a
// name still tell names apart.
func TestWIS623AttachedMarksNeverHideAName(t *testing.T) {
	p := NewPseudonyms([]string{"Nina", "राहुल"})
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !attaches(r) {
			continue
		}
		for _, name := range []string{"Nina", "राहुल"} {
			q := p.fresh()
			if got := q.hide(name + string(r) + " called."); strings.Contains(got, name) || q.overBudget {
				t.Fatalf("U+%04X after %q: %q", r, name, got)
			}
		}
	}
	if got := NewPseudonyms([]string{"Nina"}).hide("Nina〮 called."); got != "⟦P1⟧ called." {
		t.Fatalf("U+302E: %q", got)
	}
	// Inside the name, vowel signs (Mc and Mn) are required; after its last letter they are
	// optional, so "राहुला" (an appended sign) is hidden as "राहुल" plus a mark.
	for _, text := range []string{"राहल called.", "रहुल called.", "रहल called."} {
		if got := NewPseudonyms([]string{"राहुल"}).hide(text); got != text {
			t.Fatalf("Devanagari %q became %q", text, got)
		}
	}
}

// TestWIS623AuditWorkIsBounded (WIS-623 Codex round 9): 4,000 symbol-only names and a request
// with 1,000 e-mail addresses are hidden and audited (exact-name membership is a set, the token
// replacers are built once per batch of issued tokens, and the work is charged to a budget).
func TestWIS623AuditWorkIsBounded(t *testing.T) {
	names := []string{"Alice"}
	for i := range 4000 {
		name := "★"
		for n := i; n > 0; n /= 3 {
			name += string([]rune("☆✦✧")[n%3])
		}
		names = append(names, name)
	}
	p := NewPseudonyms(names)
	var b strings.Builder
	for i := range 1000 {
		fmt.Fprintf(&b, "user%d@example.com, ", i)
	}
	out, err := p.HideRequest(hostedRequest(Message{Role: "user", Content: b.String()}))
	if err != nil || strings.Contains(out.Messages[0].Content, "@") {
		t.Fatalf("%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = NewPseudonyms(names).HideRequestContext(ctx, hostedRequest(Message{Role: "user", Content: b.String()})); !isProviderCode(err, CodePseudonymsIncomplete) {
		t.Fatalf("canceled: %v", err)
	}
}

// TestWIS623CodexRound10 (WIS-623 Codex round 10 on 04b07545): JSON nests at most two layers,
// every layer is audited as text first, restoration is written against one output bound, and
// letterless-name enumeration is budgeted.
func TestWIS623CodexRound10(t *testing.T) {
	refused := func(t *testing.T, terms []string, content string) {
		t.Helper()
		if _, err := NewPseudonyms(terms).HideRequest(hostedRequest(Message{Role: "user", Content: content})); !isProviderCode(err, CodePseudonymsIncomplete) {
			t.Fatalf("%q in %.60q: %v", terms, content, err)
		}
	}
	nest := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	// Finding 1: a stored name shaped like JSON, inside a JSON string, is seen whole.
	refused(t, []string{`["Alice"]`}, nest(map[string]string{"note": `["Alice"]`}))
	// Finding 5: a name that spans JSON syntax inside nested JSON is refused, not rewritten.
	refused(t, []string{`Alice","role":"admin`}, nest(map[string]string{"msg": nest(map[string]string{"name": "Alice", "role": "admin"})}))
	// Nesting deeper than two layers is refused.
	refused(t, []string{"Alice"}, nest(map[string]string{"a": nest(map[string]string{"b": nest(map[string]string{"c": "x"})})}))
	if _, err := NewPseudonyms([]string{"Alice"}).HideRequest(hostedRequest(Message{Role: "user", Content: nest(map[string]string{"a": nest(map[string]string{"b": "Alice"})})})); err != nil {
		t.Fatalf("two layers: %v", err)
	}
	// Finding 4: overlapping letterless names are enumerated within the work budget.
	var colons []string
	for n := 1; n <= 200; n++ {
		colons = append(colons, strings.Repeat(":", n))
	}
	refused(t, colons, strings.Repeat(":", 20_000))
	// Findings 2 and 3: restoration of re-escaped tokens is bounded on the written output.
	quotes := "Ann" + strings.Repeat(`"`, 150) + "Lee" // a stored name, so it round-trips
	p := NewPseudonyms([]string{quotes, "Alice"})
	p.hide(quotes)
	// 4,000 tokens of 150 quotes, escaped twice: about 2.4 MB from 36 KB, past input + 1 MiB.
	inner := nest(map[string]string{"v": strings.Repeat("⟦P1⟧ ", 4000)})
	if _, err := p.RestoreResponse(ModelResponse{Content: nest(map[string]string{"x": inner})}); !isProviderCode(err, CodeProviderResponseLarge) {
		t.Fatalf("re-escaped expansion: %v", err)
	}
	deep := nest(map[string]string{"a": nest(map[string]string{"b": nest(map[string]string{"c": "⟦P1⟧"})})})
	if _, err := p.RestoreResponse(ModelResponse{Content: deep}); !isProviderCode(err, CodePseudonymsIncomplete) {
		t.Fatalf("restore past two layers: %v", err)
	}
	if out, err := p.RestoreResponse(ModelResponse{Content: nest(map[string]string{"x": nest(map[string]string{"v": "⟦P1⟧"})})}); err != nil || !strings.Contains(out.Content, `\\\"`) {
		t.Fatalf("restore within bounds: %q %v", out.Content, err)
	}
}

// TestWIS623CodexRound11 (WIS-623 Codex round 11 on e1641613): names with JSON syntax are refused
// on occurrence in JSON requests and hidden as usual elsewhere; JSON is audited by structure, so a
// name equal to a JSON literal never meets the literal; letterless names walk the same trie as
// names, with a budget per input position.
func TestWIS623CodexRound11(t *testing.T) {
	try := func(terms []string, msgs ...Message) (ModelRequest, error) {
		return NewPseudonyms(terms).HideRequest(hostedRequest(msgs...))
	}
	user := func(c string) Message { return Message{Role: "user", Content: c} }
	// P1: a stored "[true]" in a JSON request.
	if _, err := try([]string{"[true]"}, user(`{"x":[true]}`)); !isProviderCode(err, CodePseudonymsIncomplete) {
		t.Fatalf("[true]: %v", err)
	}
	// P2: O"Brien Tools inside one JSON string is hidden there and restored in the same shape
	// (Codex round 12: only a name left in the output refuses); in plain text it is hidden too.
	if out, err := try([]string{`O"Brien Tools`}, user(`{"supplier":"O\"Brien Tools"}`)); err != nil || out.Messages[0].Content != `{"supplier":"⟦P1⟧"}` {
		t.Fatalf("O\"Brien in JSON: %+v %v", out.Messages, err)
	}
	p := NewPseudonyms([]string{`O"Brien Tools`})
	out, err := p.HideRequest(hostedRequest(user(`Supplier O"Brien Tools called.`)))
	if err != nil || out.Messages[0].Content != "Supplier ⟦P1⟧ called." {
		t.Fatalf("O\"Brien in text: %+v %v", out.Messages, err)
	}
	if back, err := p.RestoreResponse(ModelResponse{Content: `{"who":"⟦P1⟧"}`}); err != nil || back.Content != `{"who":"O\"Brien Tools"}` {
		t.Fatalf("restore: %q %v", back.Content, err)
	}
	// P2: a name equal to a JSON literal never refuses a literal, nested or not, and is hidden
	// inside strings.
	nested := `{"a":"{\"b\":true,\"c\":null}","d":false}`
	if out, err := try([]string{"True", "Null", "False"}, user(nested)); err != nil || out.Messages[0].Content != nested {
		t.Fatalf("literals: %+v %v", out.Messages, err)
	}
	if out, err := try([]string{"True"}, user(`{"name":"True","ok":true}`)); err != nil || out.Messages[0].Content != `{"name":"⟦P1⟧","ok":true}` {
		t.Fatalf("name in a string: %+v %v", out.Messages, err)
	}
	// P2: 65 symbol names cost nothing on an empty or tool-call-only message.
	var symbols []string
	for i := range 65 {
		symbols = append(symbols, "★"+strings.Repeat("☆", i+1))
	}
	if _, err := try(symbols, user("")); err != nil {
		t.Fatalf("empty message: %v", err)
	}
	if _, err := try(symbols, Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "c", Name: ToolEvidenceGet, Arguments: json.RawMessage(`{}`)}}}); err != nil {
		t.Fatalf("tool-call-only message: %v", err)
	}
	// P2: candidates rejected at a boundary are charged to the same walk; a long run of a symbol
	// that is never a whole token is sent as written.
	run := strings.Repeat("★", 20_000) + " done."
	if out, err := try([]string{"★"}, user(run)); err != nil || out.Messages[0].Content != run {
		t.Fatalf("symbol run: %v", err)
	}
	if out, err := try([]string{"★"}, user("Rated ★ today.")); err != nil || out.Messages[0].Content != "Rated ⟦P1⟧ today." {
		t.Fatalf("symbol token: %+v %v", out.Messages, err)
	}
}

// TestWIS623SecurityReview89e4bb50: a name with a full-width or look-alike JSON character is
// classed like one with the ASCII character; a JSON document is also audited as a whole text with
// only its bare literals left out; and the phone and e-mail recheck removes every placeholder,
// including one from a symbol name matched through a reading.
func TestWIS623SecurityReview89e4bb50(t *testing.T) {
	try := func(terms []string, content string) (string, error) {
		out, err := NewPseudonyms(terms).HideRequest(hostedRequest(Message{Role: "user", Content: content}))
		if err != nil {
			return "", err
		}
		return out.Messages[0].Content, nil
	}
	for _, name := range []string{"O＂Brien Tools", "Ann［1］", "Ann｛x｝", "Ann：Bob"} {
		if got, err := try([]string{name}, `{"s":"`+name+`"}`); err != nil || got != `{"s":"⟦P1⟧"}` {
			t.Fatalf("%q in one JSON string: %q %v", name, got, err)
		}
		if got, err := try([]string{name}, "Called "+name+" today."); err != nil || got != "Called ⟦P1⟧ today." {
			t.Fatalf("%q in text: %q %v", name, got, err)
		}
	}
	// A name with a full-width bracket is classed by its readings: "［true］" left in the JSON
	// structure refuses (Codex round 12).
	if _, err := try([]string{"［true］"}, `{"x":[true]}`); !isProviderCode(err, CodePseudonymsIncomplete) {
		t.Fatalf("［true］: %v", err)
	}
	// The whole-text audit never reads a bare literal as a name; a literal inside a string is text.
	if got, err := try([]string{"True", "Null"}, `{"ok":true,"v":null,"s":"True"}`); err != nil || got != `{"ok":true,"v":null,"s":"⟦P1⟧"}` {
		t.Fatalf("literals: %q %v", got, err)
	}
	// A symbol name matched through a reading ("＊＊" for "**") splitting a phone number.
	if _, err := try([]string{"**"}, "Call +91 98765 ＊＊ 43210 now."); !isProviderCode(err, CodePseudonymsIncomplete) {
		t.Fatalf("placeholder splitting a phone: %v", err)
	}
	if got, err := try([]string{"**"}, "Rated ＊＊ today."); err != nil || got != "Rated ⟦P1⟧ today." {
		t.Fatalf("symbol by reading: %q %v", got, err)
	}
}

// TestWIS623CodexRound12 (WIS-623 Codex round 12 on 89e4bb50): the audit reads exactly what goes
// on the wire. A name produced by re-encoding, a name written as a JSON number, and number names
// found by the one budgeted walk.
func TestWIS623CodexRound12(t *testing.T) {
	try := func(terms []string, content string) (string, error) {
		out, err := NewPseudonyms(terms).HideRequest(hostedRequest(Message{Role: "user", Content: content}))
		if err != nil {
			return "", err
		}
		return out.Messages[0].Content, nil
	}
	// P1 :1358: an escape that re-encoding removes; the wire would carry the name.
	for _, content := range []string{`{"name":"Alice","role":"admin"}`, `{"name":"Al\u0069ce","role":"adm\u0069n"}`} {
		if got, err := try([]string{`Alice","role":"admin`}, content); !isProviderCode(err, CodePseudonymsIncomplete) {
			t.Fatalf("%s: %q %v", content, got, err)
		}
	}
	// P1 :1363: a name written as a JSON number is hidden like a name.
	if got, err := try([]string{"1e3"}, `{"customer":1e3,"n":2}`); err != nil || got != `{"customer":"⟦P1⟧","n":2}` {
		t.Fatalf("1e3: %q %v", got, err)
	}
	// P2 :238: number names come from the walk, on readings, as whole tokens.
	for content, refused := range map[string]bool{
		"Count ４２ parts.": true, "Count 4\u200b2 parts.": true, "Count 42\u0301 parts.": true,
		"Count 420 parts.": false, "Count A42 parts.": false, "Count 42.5 parts.": true,
	} {
		if _, err := try([]string{"Alice", "42"}, content); refused != isProviderCode(err, CodePseudonymsIncomplete) || !refused && err != nil {
			t.Fatalf("%q: %v", content, err)
		}
	}
}

// TestWIS623CodexRound13 (WIS-623 Codex round 13 on 911f2a2c).
func TestWIS623CodexRound13(t *testing.T) {
	refused := func(t *testing.T, terms []string, content string) {
		t.Helper()
		if out, err := NewPseudonyms(terms).HideRequest(hostedRequest(Message{Role: "user", Content: content})); !isProviderCode(err, CodePseudonymsIncomplete) {
			t.Fatalf("%q with %q: %+v %v", terms, content, out.Messages, err)
		}
	}
	// P1 :252: a number name with an interior mark, and any attaching mark in a request number.
	refused(t, []string{"4́2"}, "Count 42 parts.")
	p := NewPseudonyms([]string{"Alice", "4́2"})
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !attaches(r) {
			continue
		}
		for _, text := range []string{"Count 4" + string(r) + "2 parts.", "Count 42" + string(r) + " parts."} {
			q := p.fresh()
			if _, err := q.HideRequest(hostedRequest(Message{Role: "user", Content: text})); !isProviderCode(err, CodePseudonymsIncomplete) {
				t.Fatalf("U+%04X: %q was not refused: %v", r, text, err)
			}
		}
	}
	// P1 :1387: a syntax-spanning name that re-encoding escapes ("&" as &).
	refused(t, []string{`A&B":true`}, `{"A&B":true}`)
	// P1 :137: token brackets are indexed as plain brackets, and classified so.
	refused(t, []string{"⟦true⟧"}, `{"x":[true]}`)
	// P2 :1576: hiding must not turn a malformed snippet into JSON.
	refused(t, []string{`O"Brien Tools`}, `{"supplier":"O"Brien Tools"}`)
	if out, err := NewPseudonyms([]string{`O"Brien Tools`}).HideRequest(hostedRequest(Message{Role: "user", Content: `Supplier: O"Brien Tools.`})); err != nil || out.Messages[0].Content != "Supplier: ⟦P1⟧." {
		t.Fatalf("plain text: %+v %v", out.Messages, err)
	}
}

// TestWIS623JSONSyntaxClassificationProperty (WIS-623 Codex round 13): a stored term is kept
// with the JSON-syntax names exactly when its indexed form, a canonical view of it or one of its
// slot readings holds JSON syntax. Terms are drawn from letters, digits, ASCII and full-width
// JSON characters, token brackets and look-alikes (seeded).
func TestWIS623JSONSyntaxClassificationProperty(t *testing.T) {
	alphabet := []rune(`ab1 "\{}[]:,＂＼｛｝［］：，⟦⟧〚〛꞉ˏ‚`)
	r := rand.New(rand.NewPCG(13, 13))
	fold, memo := cases.Fold(), map[string][]string{}
	for range 2000 {
		b := make([]rune, 1+r.IntN(6))
		for i := range b {
			b[i] = alphabet[r.IntN(len(alphabet))]
		}
		term := "n" + string(b) // a letter, so the term is a name
		p, err := NewPseudonymsFor([]string{term})
		if err != nil {
			continue
		}
		idx := tokenBrackets.Replace(strings.TrimSpace(term))
		want := strings.ContainsAny(idx+canonicalString(idx, visualView)+canonicalString(idx, foldedView)+strings.Join(analyze(idx, fold, memo).slots, ""), jsonSyntax)
		if got := p.jsonish != nil; got != want {
			t.Fatalf("%q: classified %v, indexed form %q has JSON syntax %v", term, got, idx, want)
		}
	}
}

// TestWIS623PlainTextRestoresLiterally (WIS-623 Codex round 14): a prose draft is restored by
// literal substitution, not JSON-escaped, so the runtime's answer phase (the draft appended to
// the next request) is hidden again and not refused; JSON is still restored with each layer's
// escaping.
func TestWIS623PlainTextRestoresLiterally(t *testing.T) {
	names := []string{"A&B Motors", `O"Brien`, `Back\Slash Ltd`}
	var calls int
	s := newStandIn(t, "qwen-plus-2025-09-11", func(body string) string {
		calls++
		if calls == 1 { // the draft names every token it was given
			return "The suppliers are " + strings.Join(pseudonymToken.FindAllString(body, -1), ", ") + "."
		}
		return `{}`
	})
	m := hostedForTest(t, s, io.Discard)
	p := NewPseudonyms(names)
	ctx := WithPseudonyms(context.Background(), p)
	req := hostedRequest(Message{Role: "user", Content: `Who supplied the die? A&B Motors, O"Brien and Back\Slash Ltd were in the record.`})
	draft, err := m.Complete(ctx, req)
	want := `The suppliers are A&B Motors, O"Brien, Back\Slash Ltd.`
	if err != nil || draft.Content != want {
		t.Fatalf("draft restored as %q (%v), want %q", draft.Content, err, want)
	}
	// The runtime's answer phase (Codex round 15): the same conversation, the draft as the model
	// wrote it (tokens intact, never restored and hidden again) and the answer prompt, on the
	// same token map.
	answer := req
	answer.Messages = append(slices.Clone(req.Messages), Message{Role: "assistant", Content: draft.hiddenContent, hidden: true}, Message{Role: "user", Content: answerPhasePrompt})
	if _, err = m.Complete(ctx, answer); err != nil {
		t.Fatalf("answer phase refused: %v", err)
	}
	if !strings.Contains(s.calls()[1], "The suppliers are ⟦P") {
		t.Fatalf("the hidden draft is not in the answer request: %s", s.calls()[1])
	}
	for _, raw := range []string{"A&B", "Brien", "Slash"} {
		if strings.Contains(s.calls()[1], raw) {
			t.Fatalf("%q reached the hosted endpoint in the answer phase: %s", raw, s.calls()[1])
		}
	}
	// JSON is still restored with each layer's escaping.
	p = NewPseudonyms(names)
	p.hide(`O"Brien`)
	if back, err := p.RestoreResponse(ModelResponse{Content: `{"who":"⟦P1⟧"}`}); err != nil || back.Content != `{"who":"O\"Brien"}` {
		t.Fatalf("JSON restore: %q %v", back.Content, err)
	}
}

// TestWIS623CodexRound15 (WIS-623 Codex round 15 on fd36e843): the answer phase carries the
// draft hidden; a token quoted inside text that is not JSON is restored JSON-escaped; a token
// written JSON-escaped is restored, and an unissued one refused, in every form.
func TestWIS623CodexRound15(t *testing.T) {
	// P1: the draft is carried hidden to the answer phase on the same map. (A token glued
	// between letters, "X⟦P1⟧X", is invalid output since Codex round 17: TestWIS623CodexRound17.)
	var calls int
	s := newStandIn(t, "qwen-plus-2025-09-11", func(body string) string {
		calls++
		if calls == 1 {
			return "Owner: " + pseudonymToken.FindString(body) + "."
		}
		return `{}`
	})
	m := hostedForTest(t, s, io.Discard)
	p := NewPseudonyms([]string{"Alice"})
	ctx := WithPseudonyms(context.Background(), p)
	req := hostedRequest(Message{Role: "user", Content: "Alice owns the press."})
	draft, err := m.Complete(ctx, req)
	if err != nil || draft.Content != "Owner: Alice." {
		t.Fatalf("draft: %q %v", draft.Content, err)
	}
	answer := req
	answer.Messages = append(slices.Clone(req.Messages), Message{Role: "assistant", Content: draft.hiddenContent, hidden: true}, Message{Role: "user", Content: answerPhasePrompt})
	if _, err = m.Complete(ctx, answer); err != nil || strings.Contains(s.calls()[1], "Alice") {
		t.Fatalf("answer phase: %v %s", err, s.calls()[1])
	}
	// P2 :1625: a token quoted in text that is not JSON as a whole is restored JSON-escaped.
	q := NewPseudonyms([]string{`O"Brien`, "Alice"})
	q.hide(`O"Brien and Alice`)
	for in, want := range map[string]string{
		`Details: {"supplier":"⟦P1⟧"}`:             `Details: {"supplier":"O\"Brien"}`,
		`Supplier ⟦P1⟧ and ⟦P2⟧.`:                  `Supplier O"Brien and Alice.`,
		`Details: {"owner":"⟦P2⟧"}`:                `Details: {"owner":"Alice"}`,
		`Details: {"supplier":"\u27e6P1\u27e7"}`:   `Details: {"supplier":"O\"Brien"}`,
		`Details: {"supplier":"\u27E6P1\u27E7"} x`: `Details: {"supplier":"O\"Brien"} x`,
	} {
		if back, err := q.RestoreResponse(ModelResponse{Content: in}); err != nil || back.Content != want {
			t.Fatalf("%q restored as %q (%v), want %q", in, back.Content, err, want)
		}
	}
	if _, err := q.RestoreResponse(ModelResponse{Content: `Details: {"supplier":"O\"Brien"}`}); err != nil {
		t.Fatalf("control: %v", err)
	}
	// P2 :1630: an unissued token is refused in every written form.
	for _, in := range []string{`Owner ⟦P9⟧.`, `Owner \u27e6P9\u27e7.`, `Owner \u27E6P9⟧.`, `Owner ⟦P9\u27e7.`} {
		if _, err := q.RestoreResponse(ModelResponse{Content: in}); !isProviderCode(err, CodePseudonymUnknown) {
			t.Fatalf("%q: %v", in, err)
		}
	}
}

// TestWIS623CodexRound16 (WIS-623 Codex round 16 on 8187d81c): restore by string context, the
// token grammar in every encoding, and escaped issued tokens in carried hidden messages.
func TestWIS623CodexRound16(t *testing.T) {
	p := NewPseudonyms([]string{`O"Brien`, `Back\Slash`})
	p.hide(`O"Brien and Back\Slash`)
	// String context: a token inside a quoted region (prefix, suffix, several in one string) is
	// JSON-escaped, so the embedded JSON stays valid; outside quotes it is written as it is.
	back, err := p.RestoreResponse(ModelResponse{Content: `Details: {"note":"Supplier ⟦P1⟧ and ⟦P2⟧ ok"} and ⟦P1⟧.`})
	if err != nil {
		t.Fatal(err)
	}
	embedded := back.Content[len("Details: ") : strings.Index(back.Content, "} and ")+1]
	var v map[string]string
	if json.Unmarshal([]byte(embedded), &v) != nil || v["note"] != `Supplier O"Brien and Back\Slash ok` || !strings.HasSuffix(back.Content, `} and O"Brien.`) {
		t.Fatalf("restored %q", back.Content)
	}
	// Every encoding of the token grammar: issued tokens restore, unissued ones refuse.
	for in, want := range map[string]string{
		`Owner \u27e6\u0050\u0031\u27e7.`: `Owner O\"Brien.`,
		`Owner ⟦\u0050\u0032⟧.`:           `Owner Back\\Slash.`,
	} {
		if back, err := p.RestoreResponse(ModelResponse{Content: in}); err != nil || back.Content != want {
			t.Fatalf("%q restored as %q (%v), want %q", in, back.Content, err, want)
		}
	}
	for _, in := range []string{`Owner \u27e6\u0050\u0039\u27e7.`, `Owner ⟦P\u0039⟧.`, `Owner %E2%9F%A6P9%E2%9F%A7.`, `Owner &#x27e6;P9&#x27e7;.`} {
		if _, err := p.RestoreResponse(ModelResponse{Content: in}); !isProviderCode(err, CodePseudonymUnknown) {
			t.Fatalf("%q: %v", in, err)
		}
	}
	// A carried hidden message may hold issued tokens escaped: they are placeholders there.
	req := hostedRequest(Message{Role: "user", Content: `Ask O"Brien.`},
		Message{Role: "assistant", Content: `{"owner":"\u27e6P1\u27e7"}`, hidden: true, pseudonyms: p},
		Message{Role: "user", Content: answerPhasePrompt})
	if _, err := p.HideRequest(req); err != nil {
		t.Fatalf("escaped issued token in a carried message: %v", err)
	}
	req.Messages[1].Content = `{"owner":"\u27e6P9\u27e7"}`
	if _, err := p.HideRequest(req); !isProviderCode(err, CodePseudonymsIncomplete) {
		t.Fatalf("unissued token in a carried message: %v", err)
	}
}

// TestWIS623CarriedDraftCommonWord (#278 review): the draft carried to the answer phase is hidden
// around its tokens, not only audited. With a member called Will, the model's own "I will check"
// no longer refuses the answer phase: "will" is tokenised like it is in any other text. A known
// name the model wrote in clear is hidden too, and the answer restores from the same map.
func TestWIS623CarriedDraftCommonWord(t *testing.T) {
	var calls int
	s := newStandIn(t, "qwen-plus-2025-09-11", func(body string) string {
		calls++
		if calls == 1 { // the draft uses the common word and writes a known name in clear
			return "I will check what " + pseudonymToken.FindString(body) + " said to Rahul Deshmukh."
		}
		return "Answer: " + strings.Join(pseudonymToken.FindAllString(body, -1), " ")
	})
	m := hostedForTest(t, s, io.Discard)
	p := NewPseudonyms([]string{"Will", "Mark", "Alice", "Rahul Deshmukh"})
	ctx := WithPseudonyms(context.Background(), p)
	req := hostedRequest(Message{Role: "user", Content: "What did Alice report?"})
	draft, err := m.Complete(ctx, req)
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	answer := req
	// Carried as the runtime carries it (callOnce sets the map; AssistantMessage then keeps the hidden form).
	draft.pseudonyms = p
	answer.Messages = append(slices.Clone(req.Messages), draft.AssistantMessage(), Message{Role: "user", Content: answerPhasePrompt})
	if !answer.Messages[1].hidden {
		t.Fatal("the draft is not carried hidden")
	}
	got, err := m.Complete(ctx, answer)
	if err != nil {
		t.Fatalf("answer phase refused (before the fix: PSEUDONYMS_INCOMPLETE): %v", err)
	}
	wire := s.calls()[1]
	for _, raw := range []string{"will check", "Will", "Alice", "Rahul", "Deshmukh"} {
		if strings.Contains(wire, raw) {
			t.Fatalf("%q reached the hosted endpoint in the answer phase: %s", raw, wire)
		}
	}
	if !strings.Contains(got.Content, "Alice") || !strings.Contains(got.Content, "Rahul Deshmukh") || !strings.Contains(got.Content, "will") {
		t.Fatalf("answer not restored from the same map: %q", got.Content)
	}
	// An unissued token in a carried draft is still refused (Codex round 16).
	bad := answer
	bad.Messages = slices.Clone(answer.Messages)
	bad.Messages[1].Content = "I will check ⟦P99⟧."
	if _, err := m.Complete(ctx, bad); !isProviderCode(err, CodePseudonymsIncomplete) {
		t.Fatalf("unissued token in a carried draft: %v", err)
	}
}

// TestWIS623ContinuationsDoNotShareTokens (WIS-623 security review of 233ae21e): two
// continuations of one carried map each issue new tokens on their own copies; each restores its
// own tokens, the carried map is unchanged, and no token is issued twice for different texts.
func TestWIS623ContinuationsDoNotShareTokens(t *testing.T) {
	carried := NewPseudonyms([]string{"Alice", "Bob", "Carol"})
	carried.hide("Alice called.")
	before := maps.Clone(carried.term)
	a, b := NewPseudonyms([]string{"Alice", "Bob", "Carol"}), NewPseudonyms([]string{"Alice", "Bob", "Carol"})
	a.continueFrom(carried)
	b.continueFrom(carried)
	if got := a.hide("Alice and Bob."); got != "⟦P1⟧ and ⟦P2⟧." {
		t.Fatalf("a: %q", got)
	}
	if got := b.hide("Alice and Carol."); got != "⟦P1⟧ and ⟦P2⟧." {
		t.Fatalf("b: %q", got)
	}
	for p, want := range map[*Pseudonyms]string{a: "Alice and Bob.", b: "Alice and Carol."} {
		if back, err := p.RestoreResponse(ModelResponse{Content: "⟦P1⟧ and ⟦P2⟧."}); err != nil || back.Content != want {
			t.Fatalf("restore: %q %v, want %q", back.Content, err, want)
		}
	}
	if !maps.Equal(carried.term, before) || len(carried.token) != 1 {
		t.Fatalf("the carried map changed: %v", carried.term)
	}
	// A stale counter never reissues a token that already stands for a text.
	c := NewPseudonyms([]string{"Alice", "Bob"})
	c.hide("Alice")
	c.issued = 0
	if tok := c.tokenFor("Bob"); tok == "⟦P1⟧" || c.term["⟦P1⟧"] != "Alice" {
		t.Fatalf("reissued %s: %v", tok, c.term)
	}
}

// TestWIS623CodexRound17 (WIS-623 Codex round 17 on c0ded9d6).
func TestWIS623CodexRound17(t *testing.T) {
	p := NewPseudonyms([]string{"Alice", `O"Brien`})
	p.hide(`Alice and O"Brien`)
	// P1: a token glued to a letter, digit or mark cannot be restored safely: invalid output, in
	// the final answer and in tool arguments alike.
	// A combining mark after a Latin name is accepted since round 19: the matcher reads Latin
	// letters without marks, so hiding the restored text finds the name again.
	for _, in := range []string{"Owner: X⟦P1⟧X.", "Owner: ⟦P1⟧s.", "Owner: 9⟦P1⟧."} {
		if _, err := p.RestoreResponse(ModelResponse{Content: in}); !isProviderCode(err, CodeSchemaInvalid) {
			t.Fatalf("%q: %v", in, err)
		}
	}
	if _, err := p.RestoreResponse(ModelResponse{ToolCalls: []ToolCall{{ID: "c", Name: ToolClaimsSearch, Arguments: json.RawMessage(`{"q":"X⟦P1⟧X"}`)}}}); !isProviderCode(err, CodeSchemaInvalid) {
		t.Fatalf("tool arguments: %v", err)
	}
	if back, err := p.RestoreResponse(ModelResponse{Content: "Owners: ⟦P1⟧, ⟦P2⟧ (⟦P1⟧)."}); err != nil || back.Content != `Owners: Alice, O"Brien (Alice).` {
		t.Fatalf("tokens next to punctuation: %q %v", back.Content, err)
	}
	// Security review of c33e974e: the neighbour is the restored text, decoded, past invisible
	// characters: adjacent tokens, an escaped letter and a format character all glue.
	BS := string(rune(92))
	for _, in := range []string{"Owners: ⟦P1⟧⟦P2⟧.", "Owner: " + BS + "u0058⟦P1⟧.", "Owner: ⟦P1⟧%58.", "Owner: ⟦P1⟧&#88;.", "Owner: X\u200b⟦P1⟧.", "Owner: ⟦P1⟧\u2060Y."} {
		if _, err := p.RestoreResponse(ModelResponse{Content: in}); !isProviderCode(err, CodeSchemaInvalid) {
			t.Fatalf("%q: %v", in, err)
		}
	}
	// P2: token-shaped text a user wrote, literal or encoded, never passes as a placeholder.
	q := NewPseudonyms([]string{"Alice"})
	q.hide("Alice")
	for _, in := range []string{`Alice owns \u27e6P1\u27e7.`, "Alice owns %E2%9F%A6P1%E2%9F%A7."} {
		if _, err := q.HideRequest(hostedRequest(Message{Role: "user", Content: in})); !isProviderCode(err, CodePseudonymsIncomplete) {
			t.Fatalf("%q: %v", in, err)
		}
	}
	if out, err := q.HideRequest(hostedRequest(Message{Role: "user", Content: "Alice owns ⟦P1⟧."})); err != nil || out.Messages[0].Content != "⟦P1⟧ owns [P1]." {
		t.Fatalf("literal placeholder-shaped text: %+v %v", out.Messages, err)
	}
	// P2: in a decoded JSON value an escaped token restores to the raw name, escaped once.
	if back, err := p.RestoreResponse(ModelResponse{Content: `{"s":"\\u27e6P2\\u27e7"}`}); err != nil || back.Content != `{"s":"O\"Brien"}` {
		t.Fatalf("escaped token in a JSON value: %q %v", back.Content, err)
	}
	// P2: embedded JSON inside a decoded JSON string keeps its shape.
	inner := `Details: {"supplier":"⟦P2⟧"}`
	outer, _ := json.Marshal(map[string]string{"note": inner})
	back, err := p.RestoreResponse(ModelResponse{Content: string(outer)})
	var o map[string]string
	if err != nil || json.Unmarshal([]byte(back.Content), &o) != nil {
		t.Fatalf("outer: %q %v", back.Content, err)
	}
	var e map[string]string
	if json.Unmarshal([]byte(strings.TrimPrefix(o["note"], "Details: ")), &e) != nil || e["supplier"] != `O"Brien` {
		t.Fatalf("embedded JSON: %q", o["note"])
	}
}

// TestWIS623CodexRound18 (WIS-623 Codex round 18 on c33e974e): restored output must round-trip
// through hiding (each restored name found again, exactly, in context); JSON is written without
// HTML escaping; a continuation counts each name once.
func TestWIS623CodexRound18(t *testing.T) {
	bs := string(rune(92))
	p := NewPseudonyms([]string{"Alice", "Bob", "A&B Motors"})
	p.hide("Alice, Bob and A&B Motors")
	for in, want := range map[string]string{
		"Owners: ⟦P1⟧, ⟦P2⟧.":                  "Owners: Alice, Bob.",
		"Owner: ⟦P1⟧说好.":                       "Owner: Alice说好.",
		`He said "⟦P3⟧ called".`:               `He said "A&B Motors called".`,
		`{"s":"⟦P3⟧"}`:                         `{"s":"A&B Motors"}`,
		`Details: {"supplier":"⟦P3⟧ ltd"} ok.`: `Details: {"supplier":"A&B Motors ltd"} ok.`,
	} {
		if back, err := p.RestoreResponse(ModelResponse{Content: in}); err != nil || back.Content != want {
			t.Fatalf("%q restored as %q (%v), want %q", in, back.Content, err, want)
		}
	}
	for _, in := range []string{
		"Owners: ⟦P1⟧⟦P2⟧.",                    // AliceBob
		"Owner: X\u200b⟦P1⟧\u200bX.",           // a format character between letters
		"Owner: " + bs + "u0058⟦P1⟧.",          // an escaped letter before
		"Owner: ⟦P1⟧%58.", "Owner: ⟦P1⟧&#88;.", // percent and HTML letters after
		"Owner: X⟦P1⟧X.", "Owner: ⟦P1⟧s.",
	} {
		if _, err := p.RestoreResponse(ModelResponse{Content: in}); !isProviderCode(err, CodeSchemaInvalid) {
			t.Fatalf("%q: %v", in, err)
		}
	}
	// The union of carried and current names counts each name once.
	var names []string
	for i := range 3000 {
		names = append(names, fmt.Sprintf("Name %c%c%c", 'a'+i%26, 'a'+i/26%26, 'a'+i/676))
	}
	u := unionTerms(names, slices.Clone(names))
	if len(u) != 3000 {
		t.Fatalf("union of the same names: %d", len(u))
	}
	if _, err := NewPseudonymsFor(u); err != nil {
		t.Fatalf("3,000 names carried and read again: %v", err)
	}
}

// TestWIS623CodexRound19 (WIS-623 Codex round 19 on 5b6b0964): each restored name is followed
// through decoding by position, so a separate copy of the name cannot vouch for one a decoded
// neighbour glues; and a longer stored name around a restored one is accepted.
func TestWIS623CodexRound19(t *testing.T) {
	p := NewPseudonyms([]string{`O"Brien`, "Acme Ltd", "Acme Ltd."})
	p.hide(`O"Brien and Acme Ltd`)
	tokOB, tokAcme := p.token[`O"Brien`], p.token["Acme Ltd"]
	// P1: the decoy O"Brien written in clear does not let the glued one through.
	if _, err := p.RestoreResponse(ModelResponse{Content: `"` + tokOB + `%58"; O"Brien.`}); !isProviderCode(err, CodeSchemaInvalid) {
		t.Fatalf("decoy: %v", err)
	}
	// P2: "Acme Ltd" inside the stored "Acme Ltd." round-trips.
	if back, err := p.RestoreResponse(ModelResponse{Content: "Supplier: " + tokAcme + "."}); err != nil || back.Content != "Supplier: Acme Ltd." {
		t.Fatalf("containment: %q %v", back.Content, err)
	}
	// Since Codex round 26 no marker characters are used: U+206A (an ignorable) beside the name
	// is ordinary text, and the name is still found after it.
	if back, err := p.RestoreResponse(ModelResponse{Content: "Supplier: \u206a" + tokAcme + "."}); err != nil || back.Content != "Supplier: \u206aAcme Ltd." {
		t.Fatalf("former marker in text: %q %v", back.Content, err)
	}
}

// TestWIS623CodexRound20 (WIS-623 Codex round 20 on 5b8b1add): the round-trip check reads the
// restored text without its markers, joins coverage as hiding does, and sweeps it once.
func TestWIS623CodexRound20(t *testing.T) {
	// P1: no false boundary at a marker: "X6e9X" holds no name, nor "19876543210" a phone.
	p := NewPseudonyms([]string{"6e9", "Alice"})
	p.hide("6e9 and 9876543210 and Alice")
	tokName, tokPhone := p.token["6e9"], p.token["9876543210"]
	for _, in := range []string{"Code X" + tokName + "X.", "Call 1" + tokPhone + " now."} {
		if _, err := p.RestoreResponse(ModelResponse{Content: in}); !isProviderCode(err, CodeSchemaInvalid) {
			t.Fatalf("%q: %v", in, err)
		}
	}
	if back, err := p.RestoreResponse(ModelResponse{Content: "Code " + tokName + ", call " + tokPhone + "."}); err != nil || back.Content != "Code 6e9, call 9876543210." {
		t.Fatalf("clean: %q %v", back.Content, err)
	}
	// P2: a token hiding stood for a joined span (a name touching an e-mail) round-trips.
	q := NewPseudonyms([]string{"Alice Smith"})
	hidden := q.hide("Write to Alice Smith@example.com today.")
	if hidden != "Write to ⟦P1⟧ today." {
		t.Fatalf("joined hide: %q", hidden)
	}
	if back, err := q.RestoreResponse(ModelResponse{Content: hidden}); err != nil || back.Content != "Write to Alice Smith@example.com today." {
		t.Fatalf("joined restore: %q %v", back.Content, err)
	}
	// P2: 100,000 tokens are checked in one sweep.
	r := NewPseudonyms([]string{"Alice"})
	tok := r.hide("Alice")
	if _, err := r.RestoreResponse(ModelResponse{Content: strings.Repeat(tok+" ", 100_000)}); err != nil {
		t.Fatalf("100,000 tokens: %v", err)
	}
}

// TestWIS623CodexRound21 (WIS-623 Codex round 21 on 53ec3558): a name holding characters a
// decoder reads round-trips; phone classification is linear in the literal.
func TestWIS623CodexRound21(t *testing.T) {
	bs := string(rune(92))
	name := "ACME" + bs + "tools"
	p := NewPseudonyms([]string{name, "Alice"})
	tok := p.hide("Supplier " + name)[len("Supplier "):]
	for in, want := range map[string]string{
		"Supplier " + tok + ".":      "Supplier " + name + ".",
		`{"customer":"` + tok + `"}`: `{"customer":"ACME` + bs + bs + `tools"}`,
	} {
		if back, err := p.RestoreResponse(ModelResponse{Content: in}); err != nil || back.Content != want {
			t.Fatalf("%q restored as %q (%v), want %q", in, back.Content, err, want)
		}
	}
	// Still refused: a neighbour glued to the escaped name, also one that only becomes a letter
	// in the layer where the name itself decodes (security review of 6b078eba).
	// Codex round 22: the same inside embedded JSON in prose (escaped as emitted), and when the
	// response as a whole is JSON.
	for _, in := range []string{"Supplier X" + tok + ".", "Supplier " + tok + "%58.", "Supplier &#88;" + tok + ".",
		`Details: {"s":"` + tok + `%58"}`, `{"s":"` + tok + `%58"}`} {
		if _, err := p.RestoreResponse(ModelResponse{Content: in}); !isProviderCode(err, CodeSchemaInvalid) {
			t.Fatalf("glued %q: %v", in, err)
		}
	}
	// Phone classification never parses a big number: 900,000 digits next to a token.
	long := strings.Repeat("7", 900_000)
	if _, err := p.RestoreResponse(ModelResponse{Content: `{"n":` + long + `,"s":"` + tok + `"}`}); err != nil {
		t.Fatalf("long number: %v", err)
	}
	for lit, want := range map[string]string{"9876543210": "9876543210", "9.87654321e9": "9876543210", "98765432100": "", "1234567890": "", long: ""} {
		if got, _ := p.phoneValue(lit); got != want {
			t.Fatalf("phoneValue(%.20s) = %q, want %q", lit, got, want)
		}
	}
}

// TestWIS623CodexRound23 (WIS-623 Codex round 23 on 8b1d0174): a decoded name's neighbours are
// judged by the matcher's own reading and boundary rule, found in one pass per layer.
func TestWIS623CodexRound23(t *testing.T) {
	bs := string(rune(92))
	name := "ACME" + bs + "tools"
	p := NewPseudonyms([]string{name, "Alice"})
	tok := p.hide("Supplier " + name)[len("Supplier "):]
	// P1: a circled A after decoding reads "a" to the matcher: refused.
	if _, err := p.RestoreResponse(ModelResponse{Content: "Supplier " + tok + "%E2%92%B6."}); !isProviderCode(err, CodeSchemaInvalid) {
		t.Fatalf("circled A: %v", err)
	}
	// A Han neighbour of a name the decoding changes is an accepted false refusal since Codex
	// round 25: only explicit separators may stand beside such a name.
	for _, in := range []string{"Supplier " + tok + "说好.", "Supplier " + tok + "%E8%AF%B4."} {
		if _, err := p.RestoreResponse(ModelResponse{Content: in}); !isProviderCode(err, CodeSchemaInvalid) {
			t.Fatalf("Han %q: %v", in, err)
		}
	}
	// P2: names that decode to nothing visible are judged in one pass, not one scan per span.
	q := NewPseudonyms([]string{"Alice", "&shy;&shy;&shy;"})
	stok := q.hide("&shy;&shy;&shy; and Alice")[:len("⟦P1⟧")]
	// 20,000 adjacent tokens of a name that decodes to soft hyphens only: one linear pass, and a
	// name the matcher cannot see once decoded is refused (on 8b1d0174: accepted after 6.4 s).
	if _, err := q.RestoreResponse(ModelResponse{Content: "x " + strings.Repeat(stok, 20_000)}); !isProviderCode(err, CodeSchemaInvalid) {
		t.Fatalf("many decoded spans: %v", err)
	}
}

// TestWIS623CodexRound24 (WIS-623 Codex round 24 on 1e7abe1c): the decoded-name check runs the
// matcher itself around the span, so mark attachment and term classification are its own.
func TestWIS623CodexRound24(t *testing.T) {
	bs := string(rune(92))
	name := "ACME" + bs + "tools"
	p := NewPseudonyms([]string{name, "Ⓐ%26Ⓑ", "Alice"})
	tok := p.hide("Supplier " + name)[len("Supplier "):]
	// P1: a combining acute decoded after the name attaches to its last letter; the X after it
	// then joins the name.
	if _, err := p.RestoreResponse(ModelResponse{Content: "Supplier " + tok + "%CC%81X."}); !isProviderCode(err, CodeSchemaInvalid) {
		t.Fatalf("mark then letter: %v", err)
	}
	// Since Codex round 25 a mark beside such a name refuses too (accepted false refusal).
	if _, err := p.RestoreResponse(ModelResponse{Content: "Supplier " + tok + "%CC%81 ok."}); !isProviderCode(err, CodeSchemaInvalid) {
		t.Fatalf("mark then space: %v", err)
	}
	// Security review of ebb84732: 200 zero-width characters do not push the X out of reach,
	// before or after the name; more marks than one cluster holds refuse.
	zw := strings.Repeat(string(rune(0x200b)), 200)
	for _, in := range []string{"Supplier " + tok + zw + "X.", "Supplier X" + zw + tok + ".", "Supplier " + tok + zw + "%58.", "Supplier &#88;" + zw + tok + ".",
		"Supplier " + tok + strings.Repeat("%CC%81", maxMergedMarks+1) + " ok."} {
		if _, err := p.RestoreResponse(ModelResponse{Content: in}); !isProviderCode(err, CodeSchemaInvalid) {
			t.Fatalf("%.40q: %v", in, err)
		}
	}
	if _, err := p.RestoreResponse(ModelResponse{Content: "Supplier " + tok + zw + " ok."}); !isProviderCode(err, CodeSchemaInvalid) {
		t.Fatalf("zero-width run then space (an accepted false refusal since round 25): %v", err)
	}
	// "Ⓐ%26Ⓑ" decodes to "Ⓐ&Ⓑ": a star beside it refuses since Codex round 25 (accepted false
	// refusal); a space beside it restores.
	stok := p.hide("Rated Ⓐ%26Ⓑ")[len("Rated "):]
	if _, err := p.RestoreResponse(ModelResponse{Content: "Rated " + stok + "★ today."}); !isProviderCode(err, CodeSchemaInvalid) {
		t.Fatalf("circled name before a star: %v", err)
	}
	if back, err := p.RestoreResponse(ModelResponse{Content: "Rated " + stok + " today."}); err != nil || back.Content != "Rated Ⓐ%26Ⓑ today." {
		t.Fatalf("circled name before a space: %q %v", back.Content, err)
	}
}

// TestWIS623CodexRound25 (WIS-623 Codex round 25 on ebb84732): a restored name the decoding
// changes may stand only between explicit separators in the full decoded text.
func TestWIS623CodexRound25(t *testing.T) {
	bs := string(rune(92))
	name := "ACME" + bs + "tools"
	p := NewPseudonyms([]string{name, "Alice", "★&shy;★", "★★"})
	tok := p.hide("Supplier " + name)[len("Supplier "):]
	// P1: 90 decoded combining marks then X: refused (no window to truncate).
	if _, err := p.RestoreResponse(ModelResponse{Content: "Supplier " + tok + strings.Repeat("%CC%81", 90) + "X."}); !isProviderCode(err, CodeSchemaInvalid) {
		t.Fatalf("90 marks then X: %v", err)
	}
	// P1: "★&shy;★" decodes to stars around a soft hyphen; a star beside it refuses.
	star := p.hide("Rated ★&shy;★")[len("Rated "):]
	if _, err := p.RestoreResponse(ModelResponse{Content: "Rated " + star + "★ today."}); !isProviderCode(err, CodeSchemaInvalid) {
		t.Fatalf("star beside a decoded star name: %v", err)
	}
	// P2: a token for a name joined with an e-mail, the name changing under decoding, restores.
	q := NewPseudonyms([]string{"A%26B Smith"})
	hidden := q.hide("Write to A%26B Smith@example.com today.")
	if back, err := q.RestoreResponse(ModelResponse{Content: hidden}); err != nil || back.Content != "Write to A%26B Smith@example.com today." {
		t.Fatalf("joined name and e-mail: %q %q %v", hidden, back.Content, err)
	}
	// Security review of 9d94d276: half-width U+FF9E attaches only after NFKC, so the window
	// walk ended the cluster early; it is not a safe separator, on either side.
	c := NewPseudonyms([]string{"Ⓐ%26Ⓑ"})
	ctok := c.hide("Rated Ⓐ%26Ⓑ")[len("Rated "):]
	for _, in := range []string{"Rated " + ctok + "\uFF9EX today.", "Rated X\uFF9E" + ctok + " today.",
		"Rated " + ctok + "%EF%BE%9EX today.", "Rated X%EF%BE%9E" + ctok + " today."} {
		if _, err := c.RestoreResponse(ModelResponse{Content: in}); !isProviderCode(err, CodeSchemaInvalid) {
			t.Fatalf("half-width voiced mark beside %q: %v", in, err)
		}
	}
	// Controls: separators around the decoded name restore, in text and in a JSON value.
	for in, want := range map[string]string{
		"Supplier " + tok + ".":                "Supplier " + name + ".",
		"(" + tok + ")":                        "(" + name + ")",
		`{"customer":"` + tok + `"}`:           `{"customer":"ACME` + bs + bs + `tools"}`,
		`Details: {"s":"` + tok + `", "n": 1}`: `Details: {"s":"ACME` + bs + bs + `tools", "n": 1}`,
	} {
		if back, err := p.RestoreResponse(ModelResponse{Content: in}); err != nil || back.Content != want {
			t.Fatalf("%q restored as %q (%v), want %q", in, back.Content, err, want)
		}
	}
}

// TestWIS623CodexRound26 (WIS-623 Codex round 26 on 256644b4): span edges are decided on the
// output as the decoder reads it. An edge inside an escape the decoder consumes as one unit
// (the entity "&#101;", a percent triplet, a \u escape) refuses, whatever character follows.
func TestWIS623CodexRound26(t *testing.T) {
	bs := string(rune(92))
	for _, c := range []struct{ name, after string }{
		{"Alic&#101", ";X."},          // Codex: "Alic&#101;X." decodes to "AliceX."
		{"Alic&#x65", ";X."},          // hex entity
		{"Alic&#101", "; ok."},        // the separator is consumed even with a space after
		{"Data%4", "1 ok."},           // a percent triplet straddles the end
		{"Data%", "41 ok."},           // and with both hex digits after it
		{"Al" + bs + "u00", "65 ok."}, // a \u escape split by the end
		{"Al" + bs + "u006", "5 ok."}, // and one digit later
	} {
		p := NewPseudonyms([]string{c.name})
		h := p.hide("Supplier " + c.name + " x")
		tok := h[len("Supplier ") : len(h)-2]
		if _, ok := p.token[c.name]; !ok {
			t.Fatalf("%q was not hidden: %q", c.name, h)
		}
		if _, err := p.RestoreResponse(ModelResponse{Content: "Supplier " + tok + c.after}); !isProviderCode(err, CodeSchemaInvalid) {
			t.Fatalf("%q then %q: %v", c.name, c.after, err)
		}
		// Control: a separator that is not part of any escape restores the name exactly. A name
		// ending in "\\u" and digits is an RTF "\\uN" whose fallback character takes the space
		// and the next letter (Codex round 38: RTF in the catalogue), so it is refused there.
		back, err := p.RestoreResponse(ModelResponse{Content: "Supplier " + tok + " ok."})
		if regexp.MustCompile(`\\u[0-9]+$`).MatchString(c.name) {
			if !isProviderCode(err, CodeSchemaInvalid) {
				t.Fatalf("%q then a space (RTF): %v", c.name, err)
			}
		} else if err != nil || back.Content != "Supplier "+c.name+" ok." {
			t.Fatalf("%q then a space: %q %v", c.name, back.Content, err)
		}
	}
	// Control: a name that ends in a complete entity has an exact edge, so a separator after it
	// is read as a separator and the name restores.
	p := NewPseudonyms([]string{"Acme&#38;Co"})
	tok := p.hide("Supplier Acme&#38;Co x")[len("Supplier ") : len("Supplier ")+len(p.token["Acme&#38;Co"])]
	if back, err := p.RestoreResponse(ModelResponse{Content: "Supplier " + tok + "; ok."}); err != nil || back.Content != "Supplier Acme&#38;Co; ok." {
		t.Fatalf("complete entity then a separator: %q %v", back.Content, err)
	}
}

// TestWIS623CodexRound27 (WIS-623 Codex round 27 on 58ea01fc): every decoding is its own stage.
// "ACME\\x41Tools" (JSON) is the stored "ACME\x41Tools" after JSON unescaping alone; decoding the
// \x41 in the same step skipped that stage, so a later request carried the name unseen. A name
// restored into JSON either refuses or is hidden again by the next request; so is the name
// whose backslash comes from %5C or &#92;.
func TestWIS623CodexRound27(t *testing.T) {
	bs := string(rune(92))
	name := "ACME" + bs + "x41Tools"
	hiddenNext := func(text string) {
		t.Helper()
		q := NewPseudonyms([]string{name})
		out, err := q.HideRequest(hostedRequest(Message{Role: "user", Content: text}))
		if err != nil {
			if !isProviderCode(err, CodePseudonymsIncomplete) {
				t.Fatalf("%q: %v", text, err)
			}
			return
		}
		if c := out.Messages[0].Content; strings.Contains(c, "ACME") {
			t.Fatalf("%q went out as %q", text, c)
		}
	}
	p := NewPseudonyms([]string{name})
	h := p.hide("Supplier " + name + " x")
	tok := h[len("Supplier ") : len(h)-2]
	back, err := p.RestoreResponse(ModelResponse{Content: `Details: {"supplier":"` + tok + `"}`})
	switch {
	case err == nil:
		hiddenNext(back.Content)
	case !isProviderCode(err, CodeSchemaInvalid):
		t.Fatalf("restore: %v", err)
	}
	for _, text := range []string{
		`Details: {"supplier":"ACME` + bs + bs + `x41Tools"}`,
		"Supplier ACME%5Cx41Tools today.",
		"Supplier ACME&#92;x41Tools today.",
		`Details: {"supplier":"ACME%5Cx41Tools"}`,
		`Details: {"supplier":"ACME&#92;x41Tools"}`,
	} {
		hiddenNext(text)
	}
}

// TestWIS623CodexRound28 (WIS-623 Codex round 28 on 3110230b).
func TestWIS623CodexRound28(t *testing.T) {
	bs := string(rune(92))
	// P1: full-width escapes are escapes once canonicalised (NFKC): "Al％69ce" is "Alice" to a
	// reader, so the request is hidden or refused, never sent with the name.
	for _, form := range []string{"Al％69ce", "Al＆#105;ce", "Al＼u0069ce", "Al＆＃１０５；ce"} {
		p := NewPseudonyms([]string{"Alice"})
		out, err := p.HideRequest(hostedRequest(Message{Role: "user", Content: "Write to " + form + " today."}))
		if err == nil && strings.Contains(out.Messages[0].Content, form) {
			t.Fatalf("%q went out as %q", form, out.Messages[0].Content)
		}
		if err != nil && !isProviderCode(err, CodePseudonymsIncomplete) {
			t.Fatalf("%q: %v", form, err)
		}
	}
	// P2: ordinary mixed escaping reaches far more than 64 distinct stages (this one 192) and is
	// neither refused going out nor coming back: only the per-byte audit budget bounds the walk.
	// The full-width "＆amp;" this sample held is refused since Codex round 35 (an escape spelled
	// outside ASCII; TestWIS623CodexRound35); here it is written in ASCII.
	mixed := `Tom &amp; Jerry ` + bs + `u00e9 caf%C3%A9 ` + bs + `x41 ` + bs + `u{42} &lt; ` + bs + `"q` + bs + `" %26amp%3B &#37;41 ` + bs + bs + `u0041 x`
	p := NewPseudonyms([]string{"Alice"})
	out, err := p.HideRequest(hostedRequest(Message{Role: "user", Content: "Alice wrote: " + mixed}))
	if err != nil {
		t.Fatalf("mixed escaping refused going out: %v", err)
	}
	tok := strings.TrimSuffix(out.Messages[0].Content, " wrote: "+mixed)
	if back, err := p.RestoreResponse(ModelResponse{Content: tok + " wrote: " + mixed}); err != nil || back.Content != "Alice wrote: "+mixed {
		t.Fatalf("mixed escaping refused coming back: %q %v", back.Content, err)
	}
}

// TestWIS623CodexRound29 (WIS-623 Codex round 29 on 3df43c8d).
func TestWIS623CodexRound29(t *testing.T) {
	bs := string(rune(92))
	notSent := func(t *testing.T, names []string, text string) {
		t.Helper()
		out, err := NewPseudonyms(names).HideRequest(hostedRequest(Message{Role: "user", Content: text}))
		if err != nil {
			if !isProviderCode(err, CodePseudonymsIncomplete) {
				t.Fatalf("%q: %v", text, err)
			}
			return
		}
		if c := out.Messages[0].Content; c == text {
			t.Fatalf("%q went out unchanged", text)
		}
	}
	// P1: look-alike characters inside escape syntax read as the matcher reads them: Cyrillic
	// з is 3 and С is c, Greek Ε is E ("%6з" is "%63", "c").
	for _, text := range []string{
		"Write to Ali%6зe today.",
		"Write to Ali&#x6з;e today.",
		"Write to Ali" + bs + "u006зe today.",
		"Write to A%6Сice today.",
		"Write to A&#x6С;ice today.",
		"Write to A%6Εne today.",
	} {
		notSent(t, []string{"Alice", "Anne"}, text)
	}
	// P2: a name whose decoded form differs only canonically keeps the containment check, and
	// Unicode punctuation beside a changed name is a separator unless NFKC makes it an escape
	// introducer.
	for _, c := range []struct{ name, before, after string }{
		{"José", "“", "”"},
		{"José", "«", "»"},
		{"ACME" + bs + "tools", "“", "”"},
		{"ACME" + bs + "tools", "Supplier — ", " — ok"},
	} {
		p := NewPseudonyms([]string{c.name})
		h := p.hide("Supplier " + c.name + " x")
		tok := h[len("Supplier ") : len(h)-2]
		in := c.before + tok + c.after
		if back, err := p.RestoreResponse(ModelResponse{Content: in}); err != nil || back.Content != c.before+c.name+c.after {
			t.Fatalf("%q: %q %v", in, back.Content, err)
		}
	}
	p := NewPseudonyms([]string{"ACME" + bs + "tools"})
	h := p.hide("Supplier ACME" + bs + "tools x")
	tok := h[len("Supplier ") : len(h)-2]
	for _, after := range []string{"；", "＃", "％", "＆"} { // NFKC ; # % &: escape introducers
		if _, err := p.RestoreResponse(ModelResponse{Content: "Supplier " + tok + after + " ok."}); !isProviderCode(err, CodeSchemaInvalid) {
			t.Fatalf("%q after a changed name: %v", after, err)
		}
	}
	// P3: the budget is not a stage cap. One densely escaped snippet (194 stages) in 6 KB of
	// prose, and 64 KB of prose with a few escaped snippets, go out; a nested chain still refuses.
	mixed := `Tom &amp; Jerry ` + bs + `u00e9 caf%C3%A9 ` + bs + `x41 ` + bs + `u{42} &lt; ` + bs + `"q` + bs + `" %26amp%3B &#37;41 ` + bs + bs + `u0041 x`
	snippet := `{"summary":"Line 3 stopped ` + bs + `u2014 see <a href=` + bs + `"https://plant.test/case?id=42&amp;view=full%20log` + bs + `">log</a>. Operator said ` + bs + `"check %3Cvalve%3E` + bs + `"; caf` + bs + `u00e9 ok."}`
	prose := strings.Repeat("The line stopped because the hopper ran empty and the operator refilled it. ", 220)
	for name, text := range map[string]string{
		"dense snippet in 6 KB":   "Alice wrote: " + mixed + " " + prose[:6000],
		"64 KB with 4 snippets":   "Alice wrote: " + strings.Repeat(prose[:16000]+" "+snippet+" ", 4),
		"64 KB with 4 dense ones": "Alice wrote: " + strings.Repeat(prose[:16000]+" "+mixed+" ", 4),
	} {
		out, err := NewPseudonyms([]string{"Alice"}).HideRequest(hostedRequest(Message{Role: "user", Content: text}))
		if err != nil || strings.Contains(out.Messages[0].Content, "Alice") {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// TestWIS623CodexRound30 (WIS-623 Codex round 30 on 69c32d34).
func TestWIS623CodexRound30(t *testing.T) {
	notSent := func(t *testing.T, names []string, text string) {
		t.Helper()
		out, err := NewPseudonyms(names).HideRequest(hostedRequest(Message{Role: "user", Content: text}))
		if err != nil {
			if !isProviderCode(err, CodePseudonymsIncomplete) {
				t.Fatalf("%q: %v", text, err)
			}
			return
		}
		if c := out.Messages[0].Content; c == text {
			t.Fatalf("%q went out unchanged", text)
		}
	}
	// P1: NFKC with case kept is its own stage: the entity "&ZeroWidthSpace;" needs its "Z".
	notSent(t, []string{"Alice"}, "Write to Ali&ＺeroWidthSpace;ce today.")
	// P1: a window holds the whitespace at its edges: names that begin or end with whitespace.
	notSent(t, []string{"Alice ​"}, "Write to Al%69ce ")
	notSent(t, []string{"★ ​"}, "Rated &#9733; ")
	notSent(t, []string{"​ Alice"}, " Al%69ce wrote.")
	// P2: Greek capitals read "nina" in one view and "viva" in the other; a decoded form that
	// matches either view keeps the containment check (Han beside it is a boundary there).
	g := NewPseudonyms([]string{"ΝΙΝΑ"})
	h := g.hide("Supplier ΝΙΝΑ x")
	tok := h[len("Supplier ") : len(h)-2]
	if back, err := g.RestoreResponse(ModelResponse{Content: "Supplier " + tok + "说好 %41."}); err != nil || back.Content != "Supplier ΝΙΝΑ说好 %41." {
		t.Fatalf("Greek name beside Han: %q %v", back.Content, err)
	}
	// P2: building the windows is charged to the audit budget (it was unbudgeted, and quadratic
	// in restored spans: 64,000 tokens took 1.0 s, now 0.19 s).
	p := NewPseudonyms([]string{"Alice"})
	text := strings.Repeat("Alice ", 16000)
	spans := make([][2]int, 16000)
	for i := range spans {
		spans[i] = [2]int{6 * i, 6*i + 5}
	}
	p.auditBudget = costStep * len(text) / 2
	if _, ok := p.windows(text, spans); ok {
		t.Fatal("windows over a spent budget were built")
	}
	p.auditBudget = auditWorkBase + auditWorkPerByte*len(text)
	wins, ok := p.windows(text, spans)
	if !ok || len(wins) != 1 || wins[0].span != [2]int{0, len(spans)} {
		t.Fatalf("windows: %v %v", ok, wins)
	}
}

// TestWIS623CodexRound31 (WIS-623 Codex round 31 on 9195ac41).
func TestWIS623CodexRound31(t *testing.T) {
	bs := string(rune(92))
	// P1: every reading the matcher uses, and a case-kept form of each, is decoded: Greek
	// capital zeta is "Z" in a named entity, Greek capital upsilon is "u" in the folded view.
	for _, text := range []string{"Write to Ali&ΖeroWidthSpace;ce today.", "Write to Al" + bs + "Υ0069ce today."} {
		out, err := NewPseudonyms([]string{"Alice"}).HideRequest(hostedRequest(Message{Role: "user", Content: text}))
		if err == nil && out.Messages[0].Content == text {
			t.Fatalf("%q went out unchanged", text)
		} else if err != nil && !isProviderCode(err, CodePseudonymsIncomplete) {
			t.Fatalf("%q: %v", text, err)
		}
	}
	// P1 sweep: in every escape syntax, every ASCII letter or digit of the syntax replaced by
	// each UTS #39 confusable whose prototype is that character, or its other case where the
	// decoder accepts both, still decodes to the name in some stage. Read at the stage level
	// (bounded: no matcher run per variant).
	byProto := map[string][]rune{}
	for r, proto := range confusables {
		byProto[proto] = append(byProto[proto], r)
	}
	for _, rs := range byProto {
		slices.Sort(rs)
	}
	decodes := func(esc string) bool { // the escape reads as the name with escapes alone
		p := NewPseudonyms(nil)
		return !p.walkStages(stage{x: "Al" + esc + "ce"}, p.decodeSteps()[:escapeSteps], escapeSteps, nil, func(st *stage) bool {
			return !strings.Contains(strings.ReplaceAll(st.x, "​", ""), "Alice")
		})
	}
	variants := 0
	for _, esc := range []string{bs + "u0069", bs + "u{69}", bs + "x69", "%69", "&#105;", "&#x69;", "i&ZeroWidthSpace;"} {
		if !decodes(esc) {
			t.Fatalf("template %q does not decode", esc)
		}
		for i := 0; i < len(esc); i++ {
			c := rune(esc[i])
			if !unicode.IsLetter(c) && !unicode.IsDigit(c) || esc == "i&ZeroWidthSpace;" && i == 0 {
				continue
			}
			targets := []rune{c}
			if o := unicode.SimpleFold(c); o != c && decodes(esc[:i]+string(o)+esc[i+1:]) {
				targets = append(targets, o)
			}
			for _, target := range targets {
				for _, r := range byProto[string(target)] {
					text := "Al" + esc[:i] + string(r) + esc[i+1:] + "ce"
					variants++
					p := NewPseudonyms([]string{"Alice"})
					if p.stages(text, nil, func(v string) bool { // the name read, or an ambiguous escape refused (Codex round 34)
						return !strings.Contains(strings.ReplaceAll(v, "​", ""), "Alice")
					}) {
						t.Fatalf("%q (%U for %q) never reads Alice", text, r, target)
					}
				}
			}
		}
	}
	if variants < 100 {
		t.Fatalf("the sweep tried only %d variants", variants)
	}
	t.Logf("%d confusable variants decoded", variants)
	// A name no escape changed is checked as itself between its context as each reading reads
	// it: a Greek name keeps its containment check (its own reading "NlNA" is no view of it),
	// and a context that only a reading decodes still glues ("%5з" is "%53", "S").
	g := NewPseudonyms([]string{"ΝΙΝΑ", "Alice"})
	g.hide("Supplier ΝΙΝΑ and Alice x")
	if back, err := g.RestoreResponse(ModelResponse{Content: "Supplier " + g.token["ΝΙΝΑ"] + "说好 %41."}); err != nil || back.Content != "Supplier ΝΙΝΑ说好 %41." {
		t.Fatalf("Greek name: %q %v", back.Content, err)
	}
	if _, err := g.RestoreResponse(ModelResponse{Content: "Note %5з" + g.token["Alice"] + " ok."}); !isProviderCode(err, CodeSchemaInvalid) {
		t.Fatalf("context glued by a reading: %v", err)
	}
	// P2: a window edge inside the text is a piece edge, never a boundary the text lacks:
	// "Alice " (a stored name ending in whitespace) is not before "X".
	text := "%41 " + strings.Repeat("q ", 24) + "Alice X"
	out, err := NewPseudonyms([]string{"Alice ​"}).HideRequest(hostedRequest(Message{Role: "user", Content: text}))
	if err != nil || out.Messages[0].Content != text {
		t.Fatalf("window edge: %v %v", out.Messages, err)
	}
}

// TestWIS623CodexRound32 (WIS-623 Codex round 32 on 49dc265e): escapes are parsed over the
// readings of each character, so one escape may need a different reading for each.
func TestWIS623CodexRound32(t *testing.T) {
	bs := string(rune(92))
	for _, text := range []string{"Write to Al" + bs + "ú0069ce today.", "Write to Ali&ΖεroWidthSpace;ce today."} {
		out, err := NewPseudonyms([]string{"Alice"}).HideRequest(hostedRequest(Message{Role: "user", Content: text}))
		if err == nil && out.Messages[0].Content == text {
			t.Fatalf("%q went out unchanged", text)
		} else if err != nil && !isProviderCode(err, CodePseudonymsIncomplete) {
			t.Fatalf("%q: %v", text, err)
		}
	}
	// Sweep: two places of each escape replaced at once, each by a confusable whose prototype is
	// the character there (or its other case where the decoder takes both), and every place
	// written with a combining mark after it. Every variant reads as the name in some stage.
	// Bounded: the first four confusables of each character, sorted.
	byProto := map[string][]rune{}
	for r, proto := range confusables {
		byProto[proto] = append(byProto[proto], r)
	}
	for k := range byProto {
		slices.Sort(byProto[k])
		byProto[k] = byProto[k][:min(4, len(byProto[k]))]
	}
	reads := func(text string) bool { // as the matcher reads Latin letters: without marks
		return !NewPseudonyms(nil).stages(text, nil, func(v string) bool { // the name read, or an ambiguous escape refused (Codex round 34)
			return !strings.Contains(stripMarks(strings.ReplaceAll(v, "\u200b", "")), "Alice")
		})
	}
	variants := 0
	for _, esc := range []string{bs + "u0069", bs + "u{69}", bs + "x69", "%69", "&#105;", "&#x69;", "i&ZeroWidthSpace;"} {
		alts := make([][]string, len(esc)) // the replacements of each place
		for i := 0; i < len(esc); i++ {
			c := rune(esc[i])
			if esc == "i&ZeroWidthSpace;" && i == 0 {
				continue
			}
			for _, m := range []string{"́", "̈"} {
				alts[i] = append(alts[i], string(c)+m)
			}
			if !unicode.IsLetter(c) && !unicode.IsDigit(c) {
				continue
			}
			targets := []rune{c}
			if o := unicode.SimpleFold(c); o != c && reads("Al"+esc[:i]+string(o)+esc[i+1:]+"ce") {
				targets = append(targets, o)
			}
			for _, target := range targets {
				for _, r := range byProto[string(target)] {
					alts[i] = append(alts[i], string(r))
				}
			}
		}
		for i := range esc {
			for _, a := range alts[i] {
				text := "Al" + esc[:i] + a + esc[i+1:] + "ce"
				if variants++; !reads(text) {
					t.Fatalf("%q never reads Alice", text)
				}
				for j := i + 1; j < len(esc); j++ {
					for _, b := range alts[j] {
						text := "Al" + esc[:i] + a + esc[i+1:j] + b + esc[j+1:] + "ce"
						if esc == "%69" && i == 1 && j == 2 && scriptLetters(a+b) {
							// "%69" with both digits letters of another script ("%б৭"): no escape
							// syntax but its lead is left in ASCII, so no decoder reads it and it
							// is not refused (Codex round 35, an accepted gap).
							continue
						}
						if variants++; !reads(text) {
							t.Fatalf("%q never reads Alice", text)
						}
					}
				}
			}
		}
	}
	if variants < 1000 {
		t.Fatalf("the sweep tried only %d variants", variants)
	}
	t.Logf("%d single and paired variants decoded", variants)
}

// TestWIS623ReadingsOfSeveralCharacters (security review of cdb8cafa): a character whose reading
// is several ASCII characters ("⑩" is "10", "Æ" is "AE") is read across as many places of an
// escape. Sweep: every character whose readings include a string of several escape characters,
// put in place of that string in every escape spelling of each letter of the name.
func TestWIS623ReadingsOfSeveralCharacters(t *testing.T) {
	bs := string(rune(92))
	fold := cases.Fold()
	var spellings [][2]string // the name with one letter escaped
	for i := 0; i < len("Alice"); i++ {
		c := int("Alice"[i])
		for _, f := range []string{"%%%02x", "%%%02X", bs + "x%02x", bs + "x%02X", bs + "u%04x", bs + "u%04X", bs + "u{%x}", bs + "u{%04X}", "&#%d;", "&#00%d;", "&#x%x;", "&#X%X;", "&#x00%x;"} {
			spellings = append(spellings, [2]string{"Alice"[:i], fmt.Sprintf(f, c) + "Alice"[i+1:]})
		}
	}
	variants := 0
	for r := rune(0x80); r < 0x30000; r++ {
		x := string(r)
		if _, ok := confusables[r]; !ok && norm.NFKC.String(x) == x {
			continue
		}
		rd, _ := readingsOf(x, fold)
		for _, m := range rd.multi {
			if !strings.ContainsAny(m, "0123456789abcdefABCDEF&#%"+bs+"x{};") {
				continue
			}
			for _, sp := range spellings {
				for at := strings.Index(sp[1], m); at >= 0; {
					text := "Write to " + sp[0] + sp[1][:at] + x + sp[1][at+len(m):] + " today."
					variants++
					if NewPseudonyms(nil).stages(text, nil, func(v string) bool { // the name read, or an ambiguous escape refused (Codex round 34)
						return !strings.Contains(stripMarks(v), "Alice")
					}) {
						t.Fatalf("%q (%U as %q) never reads Alice", text, r, m)
					}
					if n := strings.Index(sp[1][at+1:], m); n >= 0 {
						at += 1 + n
					} else {
						at = -1
					}
				}
			}
		}
	}
	if variants < 20 { // 23 with the Unicode 16 data
		t.Fatalf("the sweep tried only %d variants", variants)
	}
	t.Logf("%d variants of several-character readings decoded", variants)
}

// TestWIS623EscapeSearchCharged (security review of cdb8cafa): the escape search over reading
// sets charges the audit budget for every character it visits and every way it tries, and stops
// as soon as the budget refuses. Inputs full of escape leads go out within the budget.
func TestWIS623EscapeSearchCharged(t *testing.T) {
	bs := string(rune(92))
	for lead, isEscape := range map[string]bool{"&\u0301": false, "%\u0301": false, "&#\u0301": false, "é&": false, bs + "\u0301": true} {
		in := strings.Repeat(lead, 2000)
		calls := 0
		found := lookalikeEscape(func(int) bool { calls++; return true }, in)
		if found != isEscape {
			t.Fatalf("%q: found %v", lead, found) // "\" U+0301 "\" is "\\" spelled with a mark
		}
		// The search charges as it goes: a budget spent halfway stops it on the refusing call,
		// and the text counts as refused.
		limit, after := calls/2, 0
		if got := lookalikeEscape(func(int) bool {
			if limit--; limit < 0 {
				after++
				return false
			}
			return true
		}, in); calls > 1 && (!got || after != 1) {
			t.Fatalf("%q: %d calls, %d after the refusing one, found %v", lead, calls, after-1, got)
		}
		_, err := NewPseudonyms([]string{"Alice"}).HideRequest(hostedRequest(Message{Role: "user", Content: in}))
		if isEscape != isProviderCode(err, CodePseudonymsIncomplete) || !isEscape && err != nil {
			t.Fatalf("%q: %v", lead, err)
		}
	}
}

// TestWIS623CodexRound33 (WIS-623 Codex round 33 on 338a6640).
func TestWIS623CodexRound33(t *testing.T) {
	bs := string(rune(92))
	notSent := func(t *testing.T, names []string, text string) {
		t.Helper()
		out, err := NewPseudonyms(names).HideRequest(hostedRequest(Message{Role: "user", Content: text}))
		if err == nil && out.Messages[0].Content == text {
			t.Fatalf("%q went out unchanged", text)
		} else if err != nil && !isProviderCode(err, CodePseudonymsIncomplete) {
			t.Fatalf("%q: %v", text, err)
		}
	}
	// P1: the literal decoder on the text as written is a stage of its own: it stops before
	// Cyrillic "с" ("Aliсe" reads Alice), where the readings take "с" as the hex digit "c".
	notSent(t, []string{"Alice"}, "Write to Al&#x69сe today.")
	// P1: an escape that ends inside a reading of several characters keeps the rest of it:
	// "%4Æan" is "%4A" then "E" and "an": "JEan".
	notSent(t, []string{"Jean"}, "Write to %4Æan today.")
	// P1: case before mapping: Greek "ν" upper-cased is "Ν", whose prototype is "N".
	notSent(t, []string{"Alice"}, "Write to Ali&νegativeThinSpace;ce today.")
	// P1: restoration refuses a reading that is ambiguous: "Б" is "B" or "6" in "\u{202Б}".
	p := NewPseudonyms([]string{"Alice"})
	h := p.hide("Supplier Alice x")
	tok := h[len("Supplier ") : len(h)-2]
	if _, err := p.RestoreResponse(ModelResponse{Content: "X" + bs + "u{202Б}" + tok + " ok."}); !isProviderCode(err, CodeSchemaInvalid) {
		t.Fatalf("ambiguous reading beside a restored name: %v", err)
	}
	// P2: no fixed cap per escape search: one character outside ASCII does not make a long run
	// of "%41" refuse.
	text := "é " + strings.Repeat("%41", 86)
	if _, err := NewPseudonyms([]string{"Alice"}).HideRequest(hostedRequest(Message{Role: "user", Content: text})); err != nil {
		t.Fatalf("a long run after one accented letter: %v", err)
	}
	// Sweep with case-swapped originals: every confusable whose upper- or lower-case form has the
	// character of the escape as its prototype (first four per character), in place of it.
	byProto := map[string][]rune{}
	for r, proto := range confusables {
		byProto[proto] = append(byProto[proto], r)
	}
	variants := 0
	for _, esc := range []string{bs + "u0069", bs + "x69", "%69", "&#105;", "&#x69;", "i&ZeroWidthSpace;"} {
		for i := 0; i < len(esc); i++ {
			if esc == "i&ZeroWidthSpace;" && i == 0 {
				continue
			}
			rs := slices.Clone(byProto[string(esc[i])])
			slices.Sort(rs)
			for _, r := range rs[:min(4, len(rs))] {
				for _, o := range []rune{unicode.ToUpper(r), unicode.ToLower(r)} {
					if o == r || o < utf8.RuneSelf { // an ASCII letter reads as itself only
						continue
					}
					text := "Al" + esc[:i] + string(o) + esc[i+1:] + "ce"
					variants++
					if NewPseudonyms(nil).stages(text, nil, func(v string) bool { // the name read, or an ambiguous escape refused (Codex round 34)
						return !strings.Contains(stripMarks(strings.ReplaceAll(v, "​", "")), "Alice")
					}) {
						t.Fatalf("%q (%U, the other case of %U for %q) never reads Alice", text, o, r, esc[i])
					}
				}
			}
		}
	}
	t.Logf("%d case-swapped variants decoded", variants)
}

// TestWIS623LongRunLinear (security review of 84def728): a run of escapes after one character
// outside ASCII is read one escape at a time, so its work grows linearly with its length (it
// recorded every prefix, copying ever longer spellings: four times the run allocated 8.3 times
// the memory), and the longer run goes out at full budget.
func TestWIS623LongRunLinear(t *testing.T) {
	run := func(n int) uint64 {
		text := "é " + strings.Repeat("%41", n)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		if _, err := NewPseudonyms([]string{"Alice"}).HideRequest(hostedRequest(Message{Role: "user", Content: text})); err != nil {
			t.Fatalf("%d escapes: %v", n, err)
		}
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	if small, large := run(2500), run(10000); large > small*6 { // four times the run
		t.Fatalf("2,500 escapes allocated %d bytes, 10,000 allocated %d: not linear", small, large)
	}
}

// TestWIS623CodexRound34 (WIS-623 Codex round 34 on 5f36a6bd): an escape is decoded over
// readings only where every reading agrees on one extent and one value; otherwise it is lossy,
// refused going out and coming back.
func TestWIS623CodexRound34(t *testing.T) {
	for _, text := range []string{
		"Write to Al&#х69сe today.",       // "&#x69" then "сe", or "&#x69ce": two extents
		"Write to &#х41lice&#х9Ас today.", // each escape has two extents, so no combination is picked
	} {
		out, err := NewPseudonyms([]string{"Alice"}).HideRequest(hostedRequest(Message{Role: "user", Content: text}))
		if err == nil && out.Messages[0].Content == text {
			t.Fatalf("%q went out unchanged", text)
		} else if err != nil && !isProviderCode(err, CodePseudonymsIncomplete) {
			t.Fatalf("%q: %v", text, err)
		}
	}
	p := NewPseudonyms([]string{"Alice"})
	h := p.hide("Supplier Alice x")
	tok := h[len("Supplier ") : len(h)-2]
	if _, err := p.RestoreResponse(ModelResponse{Content: tok + "&#х202А ok."}); !isProviderCode(err, CodeSchemaInvalid) {
		t.Fatalf("an ambiguous escape beside a restored name: %v", err)
	}
	// Since Codex round 35 an escape spelled with any character outside ASCII is refused, not
	// decoded: "&#xﬀ;" (round 34 decoded it, its readings "ff" and "FF" agreeing) is refused too.
	if !lookalikeEscape(nil, "a&#xﬀ;b") {
		t.Fatal("&#xﬀ; was not found")
	}
}

// TestWIS623AmbiguousEscapeRefusesEveryPath (security review of eb57aa1b): an escape whose
// readings do not agree is left as written and marked lossy; every check refuses lossy text, so
// none reads past it. "&#х69сe" (Cyrillic х and с) reads "&#x69" then "сe", or "&#x69ce".
func TestWIS623AmbiguousEscapeRefusesEveryPath(t *testing.T) {
	const amb = "Note &#х69сe today."
	// The number-name read of the original text.
	if p := NewPseudonyms([]string{"42"}); !p.refusedLayers(amb) {
		t.Fatal("number-name read went past an ambiguous escape")
	}
	// The audit of a JSON document, with a name holding JSON syntax (its JSON-syntax read).
	if p, err := NewPseudonymsFor([]string{`A"B`}); err != nil || !p.leaks(`{"note":"`+amb+`"}`) {
		t.Fatalf("JSON audit went past an ambiguous escape (%v)", err)
	}
	// The leftover-token check of restored text, alone and through RestoreResponse.
	p := NewPseudonyms([]string{"Alice"})
	if err := p.leftoverToken(amb); !isProviderCode(err, CodePseudonymUnknown) {
		t.Fatalf("leftover-token check: %v", err)
	}
	if _, err := p.RestoreResponse(ModelResponse{Content: amb}); err == nil {
		t.Fatal("a response with an ambiguous escape and no token was restored")
	}
}

// TestWIS623CodexRound35 (WIS-623 Codex round 35 on 408652a8): an escape spelled with any
// character outside ASCII is refused, never decoded under some reading; plain text in other
// scripts next to "&", "%" or "\" still goes out.
func TestWIS623CodexRound35(t *testing.T) {
	bs := string(rune(92))
	for _, text := range []string{
		"Write to Al&#х69;ce&#х58; today.",            // Cyrillic х in two numeric references
		"Write &nοtiп; today.",                        // Greek ο and Cyrillic п in a named reference
		"Write " + bs + "u{D8Б0} today.",              // P2: every reading of it is skipped, still lossy
		"Write " + bs + "ud800" + bs + "ufffd today.", // P2: an unpaired surrogate beside a written U+FFFD
		"Write to Al＆amp;ice today.",                  // a full-width ampersand
	} {
		_, err := NewPseudonyms([]string{"Alice"}).HideRequest(hostedRequest(Message{Role: "user", Content: text}))
		if !isProviderCode(err, CodePseudonymsIncomplete) {
			t.Fatalf("%q: %v", text, err)
		}
	}
	if _, lossy := asciiDecoders[escJSON](bs + "ud800" + bs + "ufffd"); !lossy {
		t.Fatal("the decoder's own U+FFFD was not lossy beside a written one")
	}
	// No false refusal: other scripts next to escape leads that spell no escape.
	for _, text := range []string{
		"Ромео&Джульетта и R&D отдел, AT&T.",
		"Скидка 50%на всё, 100%бесплатно, рост 7%в год.",
		`Путь C:\Документы\отчёт и \новый файл.`,
		`Папки C:\восток\бюджет и D:\таблицы.`, // "\в" looks like "\b", "\б" like "\6"
		"Α&Ω, Τιμή 10%ΦΠΑ, Ελλάδα\\Αθήνα.",
		"Цена &нбсп; не escape, &амп тоже.",
	} {
		if _, err := NewPseudonyms([]string{"Alice"}).HideRequest(hostedRequest(Message{Role: "user", Content: text})); err != nil {
			t.Fatalf("%q refused: %v", text, err)
		}
	}
}

// scriptLetters reports that every character of s is outside ASCII and stays outside ASCII once
// normalised and without its marks: a letter or digit of another script.
func scriptLetters(s string) bool {
	for _, c := range s {
		n := norm.NFKC.String(string(c))
		if c < utf8.RuneSelf || attaches(c) || isASCII(n) || isASCII(stripMarks(n)) {
			return false
		}
	}
	return true
}

// TestWIS623EscapeGapsAndLookalikeSyntax (security review of d63081e7): an ignorable inside an
// escape, and an escape spelled wholly in look-alikes (lead and syntax included), are refused.
func TestWIS623EscapeGapsAndLookalikeSyntax(t *testing.T) {
	bs := string(rune(92))
	for _, ig := range []string{"​", "­"} { // zero-width space, soft hyphen
		for _, text := range []string{"Al%6" + ig + "9ce", "Al" + bs + "x6" + ig + "9ce", "Al" + bs + "u00" + ig + "69ce", "Al&#x6" + ig + "9;ce"} {
			if _, err := NewPseudonyms([]string{"Alice"}).HideRequest(hostedRequest(Message{Role: "user", Content: "Write to " + text + " today."})); !isProviderCode(err, CodePseudonymsIncomplete) {
				t.Fatalf("%q: %v", text, err)
			}
		}
	}
	// Every character of the escape a look-alike: the first UTS #39 confusable of each, or its
	// full-width form where the table has none.
	byProto := map[string][]rune{}
	for r, proto := range confusables {
		byProto[proto] = append(byProto[proto], r)
	}
	look := func(c byte, nth int) string {
		rs := byProto[string(c)]
		slices.Sort(rs)
		if nth < len(rs) {
			return string(rs[nth])
		}
		return string(rune(c) - 0x21 + 0xFF01) // full-width
	}
	variants := 0
	for _, esc := range []string{bs + "u0069", bs + "u{69}", bs + "x69", "%69", "&#105;", "&#x69;", "&ZeroWidthSpace;"} {
		for nth := range 3 {
			var b strings.Builder
			for i := 0; i < len(esc); i++ {
				b.WriteString(look(esc[i], nth))
			}
			text := "Ali" + b.String() + "ce"
			variants++
			if NewPseudonyms(nil).stages(text, nil, func(string) bool { return true }) {
				t.Fatalf("%q, an escape spelled in look-alikes, was not refused", text)
			}
		}
	}
	t.Logf("%d escapes spelled wholly in look-alikes refused", variants)
}

// TestWIS623CodexRound36 (WIS-623 Codex round 36 on 6975d812).
func TestWIS623CodexRound36(t *testing.T) {
	bs := string(rune(92))
	refused := func(t *testing.T, names []string, text string) {
		t.Helper()
		out, err := NewPseudonyms(names).HideRequest(hostedRequest(Message{Role: "user", Content: text}))
		if err == nil && out.Messages[0].Content == text {
			t.Fatalf("%q went out unchanged", text)
		} else if err != nil && !isProviderCode(err, CodePseudonymsIncomplete) {
			t.Fatalf("%q: %v", text, err)
		}
	}
	// P1: JavaScript reads "\в" as "в" (an identity escape): "Ле\в" is "Лев".
	refused(t, []string{"Лев"}, "Write to Ле"+bs+"в today.")
	// P1: Java's URLDecoder reads any Unicode decimal digit: "%৬৯" is "%69", "i".
	refused(t, []string{"Alice"}, "Write to Al%৬৯ce today.")
	// P1: a numeric reference with 100,000 digits after a character outside ASCII is read in
	// linear time (each digit rescanned the whole reference: 512 ms for 40,000 digits).
	start := time.Now()
	if lookalikeEscape(nil, "é&#"+strings.Repeat("0", 100000)+"65;") {
		t.Fatal("an ASCII numeric reference was taken for one spelled outside ASCII")
	}
	if d := time.Since(start); d > 2*time.Second { // linear: about 30 ms; quadratic: seconds
		t.Fatalf("100,000 digits took %v", d)
	}
	if _, err := NewPseudonyms([]string{"Alice"}).HideRequest(hostedRequest(Message{Role: "user", Content: "é&#" + strings.Repeat("0", 10000) + "65;"})); err != nil {
		t.Fatalf("10,000 digits: %v", err)
	}
	// P2: an unpaired surrogate in a JSON document, as written, is refused before json.Unmarshal
	// writes it as U+FFFD, going out and coming back.
	doc := `{"note":"` + bs + `ud800` + bs + `ufffd"}`
	if _, err := NewPseudonyms([]string{"Alice"}).HideRequest(hostedRequest(Message{Role: "user", Content: doc})); !isProviderCode(err, CodePseudonymsIncomplete) {
		t.Fatalf("request: %v", err)
	}
	if _, err := NewPseudonyms([]string{"Alice"}).RestoreResponse(ModelResponse{Content: doc}); !isProviderCode(err, CodeSchemaInvalid) {
		t.Fatalf("response: %v", err)
	}
	// P2: removing ignorables is no escape: a name with a zero-width space still reads as
	// itself after "=" and keeps its containment check.
	p := NewPseudonyms([]string{"A​lice"})
	h := p.hide("operator=A​lice x")
	tok := h[len("operator=") : len(h)-2]
	if back, err := p.RestoreResponse(ModelResponse{Content: "operator=" + tok}); err != nil || back.Content != "operator=A​lice" {
		t.Fatalf("name with a zero-width space: %q %v", back.Content, err)
	}
}

// TestWIS623JavaScriptEscapes (security review of f7ba2518): every JavaScript string escape is
// read, by its own step or another: octal, "\'", "\v", line continuations, identity escapes.
func TestWIS623JavaScriptEscapes(t *testing.T) {
	bs := string(rune(92))
	for _, text := range []string{
		"Al" + bs + "151ce",   // octal: "\151" is "i"
		"Al" + bs + "x69ce",   // a control, read by the \x step
		"Al" + bs + "\nice",   // line continuations, in each line terminator
		"Al" + bs + "\r\nice", //
		"Al" + bs + "\rice",   //
		"Al" + bs + "\u2028ice",
		"Al" + bs + "\u2029ice",
	} {
		_, err := NewPseudonyms([]string{"Alice"}).HideRequest(hostedRequest(Message{Role: "user", Content: "Write to " + text + " today."}))
		if !isProviderCode(err, CodePseudonymsIncomplete) {
			t.Fatalf("%q: %v", text, err)
		}
	}
	// No false leak: "Al\'ice" is "Al'ice", and the Russian path stays harmless.
	for _, text := range []string{"Write to Al" + bs + "'ice today.", `Папки C:\восток\бюджет.`} {
		if _, err := NewPseudonyms([]string{"Alice"}).HideRequest(hostedRequest(Message{Role: "user", Content: text})); err != nil {
			t.Fatalf("%q: %v", text, err)
		}
	}
	if got := jsEscapes(`a\'b\vc\0d\101\7z\8\в`); got != "a'b\vc\x00dA\x07z8в" {
		t.Fatalf("jsEscapes: %q", got)
	}
}

// TestWIS623EscapeCatalogue (WIS-623 Codex round 37 on 884db700): every syntax of the escape
// catalogue (see decodeSteps) is read, spelled in ASCII, and refused when spelled outside ASCII.
func TestWIS623EscapeCatalogue(t *testing.T) {
	bs := string(rune(92))
	notSent := func(t *testing.T, names []string, text string) {
		t.Helper()
		out, err := NewPseudonyms(names).HideRequest(hostedRequest(Message{Role: "user", Content: "Write to " + text + " today."}))
		if err == nil && out.Messages[0].Content == "Write to "+text+" today." {
			t.Fatalf("%q went out unchanged", text)
		} else if err != nil && !isProviderCode(err, CodePseudonymsIncomplete) {
			t.Fatalf("%q: %v", text, err)
		}
	}
	for _, text := range []string{
		"Al" + bs + "u0069ce",                   // JSON
		"Al" + bs + "u{0000069}ce",              // JavaScript \u{} with leading zeros (Codex)
		"Al" + bs + "x69ce",                     // \x as UTF-8 and as Latin-1
		"Al" + bs + "151ce",                     // octal
		"Al" + bs + "ice",                       // a JavaScript identity escape
		"Al" + bs + "\nice",                     // a line continuation
		"Al" + bs + "U00000069ce",               // Python \U
		"Al" + bs + "N{LATIN SMALL LETTER I}ce", // Python \N{}: refused
		"Al" + bs + "69 ce",                     // CSS, with its whitespace
		"Al" + bs + "000069ce",                  // CSS, six digits
		"Al%u0069ce",                            // JavaScript unescape
		"Al%69ce",                               // URL
		"Al%৬৯ce",                               // URL with Unicode digits (Java)
		"Al&#105;ce", "Al&#x69;ce",              // HTML
		"Al＼151ce", "Al％u0069ce", "Al" + bs + "Ｕ00000069ce", "Al" + bs + "u｛69｝ce", // spelled outside ASCII (Codex)
	} {
		notSent(t, []string{"Alice"}, text)
	}
	// \xHH read both ways (Codex): "\xC3\xAD" is "í" as UTF-8, "Ã" and a soft hyphen as Latin-1.
	notSent(t, []string{"ílice"}, bs+"xC3"+bs+"xADlice")
	notSent(t, []string{"Ãlice"}, bs+"xC3"+bs+"xADlice")
	// P2: an unpaired surrogate in a JSON string inside a JSON string (its backslash doubled).
	doc := `{"a":"{` + bs + `"b` + bs + `":` + bs + `"` + bs + bs + `ud800` + bs + `"}"}`
	if _, err := NewPseudonyms([]string{"Alice"}).HideRequest(hostedRequest(Message{Role: "user", Content: doc})); !isProviderCode(err, CodePseudonymsIncomplete) {
		t.Fatalf("nested request: %v", err)
	}
	if _, err := NewPseudonyms([]string{"Alice"}).RestoreResponse(ModelResponse{Content: doc}); !isProviderCode(err, CodeSchemaInvalid) {
		t.Fatalf("nested response: %v", err)
	}
	// No false refusal: Windows paths, read as every syntax above reads them.
	for _, text := range []string{`Open C:\Users\Ann\report.txt today.`, `Open C:\1\b and C:\Program Files\App.`} {
		if _, err := NewPseudonyms([]string{"Alice"}).HideRequest(hostedRequest(Message{Role: "user", Content: text})); err != nil {
			t.Fatalf("%q: %v", text, err)
		}
	}
}

// TestWIS623CatalogueFamilies (WIS-623 Codex round 38 on 8d87a22a): every language family of
// the catalogue (see decodeSteps) is read, Codex's findings included, and ordinary text, paths
// and code still go out.
func TestWIS623CatalogueFamilies(t *testing.T) {
	bs := string(rune(92))
	notSent := func(t *testing.T, name, text string) {
		t.Helper()
		out, err := NewPseudonyms([]string{name}).HideRequest(hostedRequest(Message{Role: "user", Content: "Write to " + text + " today."}))
		if err == nil && out.Messages[0].Content == "Write to "+text+" today." {
			t.Fatalf("%q went out unchanged (name %q)", text, name)
		} else if err != nil && !isProviderCode(err, CodePseudonymsIncomplete) {
			t.Fatalf("%q: %v", text, err)
		}
	}
	for _, c := range []struct{ name, text string }{
		// Codex's findings.
		{"O'Brien", "O＼'Brien"},           // a look-alike \' (discovery)
		{"Alice", "Al＼\nice"},             // a look-alike line continuation
		{"Alice", "Al" + bs + "69\r\nce"}, // CSS reads CRLF as one newline
		{"Łukasz", bs + "501ukasz"},       // Python octal to \777
		// Families.
		{"Ãlice", "%C3%ADlice"},                // JavaScript unescape: Latin-1 %HH
		{"ílice", bs + "303" + bs + "255lice"}, // octal byte runs as UTF-8
		{"John", "Joh" + bs + "n"},             // CSS: \ + another character is it
		{"Alice", "Al" + bs + "uuuu0069ce"},    // Java
		{"Bobi", "Bob" + bs + "x0069"},         // C variable-length \x
		{"Bobi", "Bob" + bs + "x069"},          // C# \x
		{"Bobi", "Bob" + bs + "u69"},           // Bash \u
		{"Bobi", "Bob" + bs + "U69"},           // Bash \U
		{"Alice", "Al" + bs + "U{69}ce"},       // R
		{"Alice", "Al" + bs + "u{6_9}ce"},      // Rust
		{"Alice", bs + "u{41 6c 69 63 65}"},    // Ruby, several code points
		{"José", "Jos" + bs + "M-i"},           // Ruby meta
		{"Alice", "Al" + bs + "x{69}ce"},       // Perl \x{}
		{"Alice", "Al" + bs + "o{151}ce"},      // Perl \o{}
		{"Alice", "Al" + bs + "N{U+69}ce"},     // Perl \N{U+}
		{"Alice", "Al" + bs + "105ce"},         // Lua decimal
		{"Alice", "Al" + bs + "z   ice"},       // Lua \z
		{"Alice", "Al" + bs + "\n    ice"},     // YAML escaped line break
		{"Alice", "Al" + bs + "   \n   ice"},   // TOML line-ending backslash
		{"Mary Ann", "Mary+Ann"},               // form-urlencoded
		{"O'Brien", "O''Brien"},                // doubled quotes
		{"Alice", "U&'Al" + bs + "0069ce'"},    // SQL U&
		{"Alice", "U&'Al!0069ce' UESCAPE '!'"}, // SQL UESCAPE
		{"Alice", "U&'Al" + bs + "+000069ce'"}, // SQL \+XXXXXX
		{"Alice", "Al`u{69}ce"},                // PowerShell
		{"Alice", "Al" + bs + "#u{69}ce"},      // Swift raw
		{"Alice", "Al" + bs + "(\"i\")ce"},     // Swift interpolation of a literal
		{"Alice", "Al#(0069)ce"},               // Power Query
		{"Alice", "Al#(00000069)ce"},           //
		{"Alice", "Al" + bs + "'69ce"},         // RTF cp1252
		{"Alice", "Al" + bs + "u105?ce"},       // RTF \uN with its fallback
		{"Alice", "Al^^69ce"},                  // TeX ^^hh
		{"Alice", "Al^^)ce"},                   // TeX ^^X
		{"Alice", "Al" + bs + "U00000069ce"},   // Python, C#
		{"Alice", "Al%u0069ce"},                // JavaScript unescape %u
		{"Alice", "Al＾＾69ce"},                  // a family spelled outside ASCII
		{"Alice", "Al｀u{69}ce"},                //
	} {
		notSent(t, c.name, c.text)
	}
	// Restore reads CSS newlines as CSS does: "\58" CRLF before a name glues an "X" to it.
	p := NewPseudonyms([]string{"Alice"})
	h := p.hide("Supplier Alice x")
	tok := h[len("Supplier ") : len(h)-2]
	if _, err := p.RestoreResponse(ModelResponse{Content: "Note " + bs + "58\r\n" + tok + " ok."}); !isProviderCode(err, CodeSchemaInvalid) {
		t.Fatalf("CSS CRLF before a restored name: %v", err)
	}
	// CSS discovery reads at most six hex digits: a seventh character is no part of the escape.
	if _, err := NewPseudonyms([]string{"Bob"}).HideRequest(hostedRequest(Message{Role: "user", Content: "Code " + bs + "000041é here."})); err != nil {
		t.Fatalf("seven characters after a backslash: %v", err)
	}
	// Perl case escapes change only case.
	for _, f := range families() {
		if strings.HasPrefix(f.name, "Perl case") {
			if got, _ := f.decode(bs + "uali" + bs + "Ece"); got != "Alice" {
				t.Fatalf("Perl case escapes: %q", got)
			}
		}
	}
	// No false refusal: paths and code in several languages.
	for _, text := range []string{
		`Open C:\Users\Ann\report.txt and C:\1\b today.`,
		"def f(x):\n    print(\"line\\n\", x, '\\t', r'\\d+')\n    return re.sub(r'\\s+', ' ', x)\n",
		"const s = `a\\tb ${x}`; console.log('it\\'s', \"q\\\"\", s.replace(/\\\\/g, '/'));",
		"SELECT name FROM t WHERE note LIKE '%it''s%' AND code = U&'\\0041';",
		"Write-Host \"Tab`there`n\" -ForegroundColor Red; $p = \"C:\\Temp\\$name\"",
	} {
		if _, err := NewPseudonyms([]string{"Alice"}).HideRequest(hostedRequest(Message{Role: "user", Content: text})); err != nil {
			t.Fatalf("%q: %v", text, err)
		}
	}
}

// TestWIS623FamiliesAfterCoreAndWhitespaceLeads (security review of 3f0de2dd): a family reads
// what the core decoders produce, and a lead that takes a raw whitespace character is never cut
// from it.
func TestWIS623FamiliesAfterCoreAndWhitespaceLeads(t *testing.T) {
	bs := string(rune(92))
	for _, c := range []struct{ name, text string }{
		{"Alice", `{"note":"Al` + bs + bs + `U00000069ce"}`}, // JSON gives "\U00000069", Python "i"
		{"Alice", "Al&#92;U00000069ce"},                      // HTML gives "\U00000069"
		{"Alice", "Al&#92;69 ce"},                            // HTML gives a CSS escape
		{"Alice", "Al%5Cx69ce"},                              // URL gives a Latin-1 \x
		{"Jane", "^^\nane"},                                  // TeX ^^ and a newline: "J"
		{"Alice", "Al^^\tce"},                                // TeX ^^ and a tab: "I"
		{"Alice", "Al`\nice"},                                // a PowerShell line continuation
		{"Alice", "Al" + bs + "z \n ice"},                    // Lua \z skips whitespace
	} {
		out, err := NewPseudonyms([]string{c.name}).HideRequest(hostedRequest(Message{Role: "user", Content: "Write to " + c.text + " today."}))
		if err == nil && out.Messages[0].Content == "Write to "+c.text+" today." {
			t.Fatalf("%q went out unchanged", c.text)
		} else if err != nil && !isProviderCode(err, CodePseudonymsIncomplete) {
			t.Fatalf("%q: %v", c.text, err)
		}
	}
}
