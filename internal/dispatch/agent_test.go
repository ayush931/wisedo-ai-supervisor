package dispatch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
)

type registry struct {
	worker.ActivityRegistry
	names []string
}

func (r *registry) RegisterActivityWithOptions(_ interface{}, o activity.RegisterOptions) {
	r.names = append(r.names, o.Name)
}

func nonRetryableType(err error) string {
	var app *temporal.ApplicationError
	if errors.As(err, &app) && app.NonRetryable() {
		return app.Type()
	}
	return ""
}

// WIS-275 slice 1: InvestigateCase exists only when the agent runtime is configured; unset
// provider (the rollback) changes nothing, and the activity still enforces the worker scope.
func TestInvestigateCaseRegisteredOnlyWhenConfigured(t *testing.T) {
	fixture := config.Agent{Provider: config.AgentProviderFixture, Model: "fixture-model", ModelRevision: "r1", Timeout: 30 * time.Second, MaxConcurrent: 2, MaxQueued: 8}
	allowed := uuid.NewString()
	for _, tc := range []struct {
		name       string
		agent      config.Agent
		registered bool
		callType   string // non-retryable type for a call on a tenant outside the allowlist
	}{
		{"unset provider", config.Agent{}, false, "disabled"},
		{"local without a pinned address", config.Agent{Provider: config.AgentProviderLocal, BaseURL: "http://127.0.0.1:8000", Model: "m", ModelRevision: "r", Timeout: time.Second}, false, "disabled"},
		{"fixture provider", fixture, true, "scope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &registry{}
			a := &Activities{Allow: map[string]bool{allowed: true}}
			got, err := registerAgent(r, a, config.Config{Agent: tc.agent}, &store.Store{})
			if err != nil || got != tc.registered || (len(r.names) == 4) != tc.registered || (tc.registered && (r.names[0] != "InvestigateCase" || r.names[1] != "RunInterviewTurn" || r.names[2] != "RunConfigurationDraft" || r.names[3] != "RunChatAnswer")) {
				t.Fatalf("registered=%v names=%v err=%v", got, r.names, err)
			}
			if tc.registered && (a.Agent == nil || a.Agent.Provider.Actual()) {
				t.Fatalf("fixture runtime must be present and not actual: %+v", a.Agent)
			}
			_, err = a.InvestigateCase(context.Background(), InvestigateRef{Tenant: uuid.NewString(), Site: uuid.NewString(), Case: uuid.NewString(), Revision: 2})
			if nonRetryableType(err) != tc.callType {
				t.Fatalf("call: %v", err)
			}
		})
	}
}

func TestAgentRuntimeLoadsScriptedFixture(t *testing.T) {
	path, err := filepath.Abs(filepath.Join("..", "..", "deploy", "supervisor", "agent-fixture-script.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agentRuntime(config.Config{Agent: config.Agent{Provider: config.AgentProviderFixture, FixtureScript: path,
		Model: "fixture-model", ModelRevision: "r1", Timeout: time.Second, MaxConcurrent: 1}}, &store.Store{})
	if err != nil {
		t.Fatal(err)
	}
	if runtime == nil || runtime.Provider.Name() != agent.ProviderFixture || runtime.Provider.Actual() || agent.RequireActual(runtime.Provider) == nil {
		t.Fatalf("scripted provider does not remain visibly non-actual: %+v", runtime)
	}
}

// Review of 104b474: a configured fixture script that does not load stops worker startup with
// an error naming the configured path, instead of starting with no agent and failing every run.
func TestWorkerRefusesUnloadableFixtureScript(t *testing.T) {
	dir := t.TempDir()
	invalid := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(invalid, []byte(`{"schema_version":1,"purposes":[{"purpose":"interview","steps":[{"tool_calls":[{"id":"q","name":"claims.search","arguments":{}}]}]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"missing": filepath.Join(dir, "missing.json"), "invalid": invalid} {
		t.Run(name, func(t *testing.T) {
			cfg := config.Config{Agent: config.Agent{Provider: config.AgentProviderFixture, FixtureScript: path, Model: "fixture-model", ModelRevision: "r1", Timeout: time.Second, MaxConcurrent: 1}}
			r := &registry{}
			a := &Activities{}
			got, err := registerAgent(r, a, cfg, &store.Store{})
			if err == nil || got || len(r.names) != 0 || a.Agent != nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("registered=%v names=%v agent=%v err=%v", got, r.names, a.Agent, err)
			}
			lazy, err := client.NewLazyClient(client.Options{HostPort: "127.0.0.1:1"}) // never dialed
			if err != nil {
				t.Fatal(err)
			}
			defer lazy.Close()
			if w, err := NewWorker(lazy, &store.Store{}, cfg); err == nil || w != nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("worker started with unloadable fixture script: worker=%v err=%v", w, err)
			}
		})
	}
}

func TestWIS702WorkerProbesLocalContext(t *testing.T) {
	const model = "unsloth/Qwen3.5-9B-GGUF:Q4_K_M"
	const revision = "sha256:03b74727a860a56338e042c4420bb3f04b2fec5734175f4cb9fa853daf52b7e8"
	for _, available := range []bool{true, false} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/models":
				fmt.Fprintf(w, `{"data":[{"id":%q}]}`, model)
			case "/props":
				if available {
					fmt.Fprint(w, `{"default_generation_settings":{"n_ctx":16384}}`)
				} else {
					http.NotFound(w, r)
				}
			default:
				t.Errorf("unexpected startup request %s", r.URL.Path)
			}
		}))
		rt, err := agentRuntime(config.Config{Agent: config.Agent{Provider: config.AgentProviderLocal, BaseURL: srv.URL, Model: model, ModelRevision: revision, EndpointAddrs: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:0")}, Timeout: time.Second, MaxConcurrent: 1}}, &store.Store{})
		srv.Close()
		if err != nil || rt == nil {
			t.Fatalf("worker creation: %v", err)
		}
		want := int64(0)
		if available {
			want = 16384
		}
		if got := rt.Provider.(*agent.LocalModel).ContextWindowTokens(); got != want {
			t.Fatalf("startup capacity=%d want=%d", got, want)
		}
	}
}
