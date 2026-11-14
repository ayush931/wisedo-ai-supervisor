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
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/modelhosting"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestWIS702PinnedLocalLimitProfile(t *testing.T) {
	const model = "unsloth/Qwen3.5-9B-GGUF:Q4_K_M"
	const revision = "sha256:03b74727a860a56338e042c4420bb3f04b2fec5734175f4cb9fa853daf52b7e8"
	w := newWorld(t)
	sc := agent.RunScope{TenantID: w.tenant, SiteID: w.site}
	for _, tc := range []struct {
		name               string
		tokens             int
		provider, revision string
		fallback, hosted   bool
		want               error
	}{
		{"local 10k", 10000, agent.ProviderLocal, revision, false, false, nil},
		{"local exact ceiling", 12000, agent.ProviderLocal, revision, false, false, nil},
		{"local above ceiling", 12001, agent.ProviderLocal, revision, false, false, agent.ErrContextOverflow},
		{"local byte fallback", 10000, agent.ProviderLocal, revision, true, false, agent.ErrContextOverflow},
		{"different local revision", 10000, agent.ProviderLocal, "sha256:other", false, false, agent.ErrContextOverflow},
		{"default", 10000, agent.ProviderFixture, revision, false, false, agent.ErrContextOverflow},
		{"exact context capacity", 12000, agent.ProviderLocal, revision, false, false, nil},
		{"small context", 10000, agent.ProviderLocal, revision, false, false, agent.ErrContextOverflow},
		{"missing context", 10000, agent.ProviderLocal, revision, false, false, agent.ErrContextOverflow},
		{"hosted overrides local pin", 10000, agent.ProviderLocal, revision, false, true, agent.ErrContextOverflow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := []agent.Message{{Role: "system", Content: "fixed policy"}, {Role: "user", Content: "required first fact\n" + strings.Repeat("evidence ", 3330) + "\nrequired final fact"}}
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/models":
					fmt.Fprintf(rw, `{"data":[{"id":%q}]}`, model)
				case "/props":
					if tc.name == "missing context" {
						http.NotFound(rw, r)
						return
					}
					nctx := 16384
					if tc.name == "small context" {
						nctx = 13999
					}
					if tc.name == "exact context capacity" {
						nctx = 14000
					}
					fmt.Fprintf(rw, `{"default_generation_settings":{"n_ctx":%d}}`, nctx)
				case "/apply-template":
					if tc.fallback {
						http.NotFound(rw, r)
						return
					}
					fmt.Fprint(rw, `{"prompt":"templated"}`)
				case "/tokenize":
					_ = json.NewEncoder(rw).Encode(map[string]any{"tokens": make([]int, tc.tokens)})
				case "/v1/chat/completions":
					calls++
					var got struct {
						Messages  []agent.Message `json:"messages"`
						MaxTokens int             `json:"max_tokens"`
					}
					if err := json.NewDecoder(r.Body).Decode(&got); err != nil || !reflect.DeepEqual(got.Messages, messages) || got.MaxTokens != 2000 {
						t.Errorf("required content or output ceiling changed: %v", err)
					}
					_ = json.NewEncoder(rw).Encode(map[string]any{"model": model, "choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "{}"}}}, "usage": map[string]any{"prompt_tokens": tc.tokens, "completion_tokens": 10}})
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			p, err := agent.NewLocalModel(config.Agent{Provider: config.AgentProviderLocal, BaseURL: srv.URL, Model: model, ModelRevision: tc.revision, EndpointAddrs: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:0")}, Timeout: 5 * time.Second}, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now)
			if err != nil {
				t.Fatal(err)
			}
			if err = p.Probe(w.ctx); err != nil {
				t.Fatal(err)
			}
			var provider agent.Provider = p
			if tc.provider == agent.ProviderFixture {
				provider = agent.NewFixtureProvider()
			}
			if tc.hosted {
				owner := modelhosting.Principal{Issuer: w.ea.Issuer, Subject: w.ea.Subject, SessionHash: w.ea.SessionHash}
				if _, err = modelhosting.New(w.runtimeDB).Decide(w.ctx, owner, modelhosting.Scope{TenantID: w.tenant, SiteID: w.site}, modelhosting.DecisionInput{Enabled: true, DPAReference: "WIS-702-test", Reason: "Profile boundary test"}, uuid.NewString(), 0); err != nil {
					t.Fatal(err)
				}
				agent.SetHostedRoute(&agent.HostedRoute{Provider: p, Name: "hosted-qwen-intl", Model: model, ModelRevision: revision, Region: "ap-southeast-1", PriceVersion: "test-v1", Price: &agent.TokenPrice{InputMicroUSDPerMillion: 1, OutputMicroUSDPerMillion: 1}})
				defer agent.SetHostedRoute(nil)
			}
			run := agent.Run{ID: uuid.NewString(), RequestKind: agent.RequestCase, CaseID: w.caseID, CaseRevision: w.fresh(t), TraceID: agent.NewTraceID(), ProviderName: provider.Name(), ProviderActual: provider.Actual(), Model: model, ModelRevision: tc.revision, PromptVersion: agent.PromptVersion, ToolVersion: agent.ToolVersion, PolicyRevision: 1}
			run.LocalContextTokens = p.ContextWindowTokens()
			var refs []agent.RunRef
			if err = w.store.InTx(w.ctx, sc, func(tx *sql.Tx) error {
				in, e := agent.LoadCaseInput(w.ctx, tx, sc, w.caseID, agent.LoaderOptions{ModeEnabled: true})
				if e != nil {
					return e
				}
				refs = in.Refs
				if e = agent.CreateRunTx(w.ctx, tx, sc, run, agent.DefaultLimits()); e != nil {
					return e
				}
				return agent.AddRefsTx(w.ctx, tx, sc, run.ID, refs)
			}); err != nil {
				t.Fatal(err)
			}
			rt := w.runtime(provider, nil)
			rt.Model, rt.ModelRevision = model, tc.revision
			req := agent.ModelRequest{SchemaVersion: 1, Model: model, PromptVersion: agent.PromptVersion, ToolVersion: agent.ToolVersion, Messages: messages, MaxOutputTokens: 2000}
			_, perr, err := rt.CallForTest(w.ctx, sc, run, refs, req, 1)
			if !errors.Is(err, tc.want) || perr != nil {
				t.Fatalf("call error %v provider %v, want %v", err, perr, tc.want)
			}
			if tc.want != nil && calls != 0 || tc.want == nil && calls != 1 {
				t.Fatalf("dispatched calls=%d", calls)
			}
			var profile string
			var version, requests, tools int
			var tokens int64
			if err = w.control.QueryRow(`SELECT limit_profile_id,limit_profile_version,requests_left,tool_calls_left,tokens_left FROM supervisor.agent_runs WHERE id=$1`, run.ID).Scan(&profile, &version, &requests, &tools, &tokens); err != nil {
				t.Fatal(err)
			}
			wantProfile := "default"
			if tc.provider == agent.ProviderLocal && tc.revision == revision && !tc.hosted && tc.name != "small context" && tc.name != "missing context" {
				wantProfile = "wis702-local-qwen35-9b"
			}
			if profile != wantProfile || version != 1 || tools != 12 || tc.want != nil && (requests != 6 || tokens != 24000) || tc.want == nil && (requests != 5 || tokens != 24000-int64(tc.tokens)-10) {
				t.Fatalf("profile/counters: %s/%d req=%d tools=%d tokens=%d", profile, version, requests, tools, tokens)
			}
			if err = w.store.InTx(w.ctx, sc, func(tx *sql.Tx) error {
				_, e := tx.Exec(`UPDATE supervisor.agent_runs SET limit_profile_id='default',limit_profile_version=2 WHERE id=$1`, run.ID)
				return e
			}); err == nil {
				t.Fatal("profile provenance was mutable")
			} else {
				var pg *pgconn.PgError
				if !errors.As(err, &pg) || pg.Code != "42501" {
					t.Fatalf("profile update: %v", err)
				}
			}
		})
	}
}
