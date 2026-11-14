package config

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigFailsClosed(t *testing.T) {
	// Everything but the issuer is production-valid, so the plain-HTTP issuer is the only reason to refuse.
	c := Config{Environment: "production", DatabaseURL: "postgresql://localhost/db?sslmode=verify-full", Issuer: "http://localhost/realm", Origin: "https://app.example", ClientID: "test", ClientSecret: "fixture", TemporalTLS: true}
	if c.Validate() == nil {
		t.Fatal("insecure issuer accepted")
	}
	c.Issuer = "https://localhost/realm"
	if e := c.Validate(); e != nil {
		t.Fatalf("production config with HTTPS issuer: %v", e)
	}
	c.TemporalTLS = false
	if c.Validate() == nil {
		t.Fatal("production Temporal without TLS accepted")
	}
	// WIS-699: local development uses a local origin; a public one under "local" is refused.
	c.Issuer, c.TemporalTLS, c.Origin = "http://localhost/realm", true, "http://localhost:58176"
	c.Environment = "local"
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
	c.Tenants = []string{"bad"}
	if c.Validate() == nil {
		t.Fatal("invalid tenant accepted")
	}
}

func baseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SUPERVISOR_ENVIRONMENT", "local")
	t.Setenv("SUPERVISOR_DATABASE_URL", "postgresql://localhost/db")
	t.Setenv("SUPERVISOR_OIDC_ISSUER", "http://localhost/realm")
	t.Setenv("SUPERVISOR_PUBLIC_ORIGIN", "http://localhost:58176")
	t.Setenv("SUPERVISOR_OIDC_CLIENT_SECRET", "fixture")
	t.Setenv("SUPERVISOR_RELAY_TENANT_IDS", "")
	t.Setenv("SUPERVISOR_AGENT_FIXTURE_SCRIPT", "")
}

func TestFixtureScriptRequiresFixtureProvider(t *testing.T) {
	baseEnv(t)
	script := filepath.Join(t.TempDir(), "fixture.json")
	if err := os.WriteFile(script, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUPERVISOR_AGENT_FIXTURE_SCRIPT", script)
	for _, provider := range []string{"", AgentProviderLocal} {
		t.Setenv("SUPERVISOR_AGENT_PROVIDER", provider)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "requires SUPERVISOR_AGENT_PROVIDER=fixture") {
			t.Errorf("fixture script with provider %q: %v", provider, err)
		}
	}
	t.Setenv("SUPERVISOR_AGENT_PROVIDER", AgentProviderFixture)
	t.Setenv("SUPERVISOR_AGENT_MODEL", "fixture-model")
	t.Setenv("SUPERVISOR_AGENT_MODEL_REVISION", "r1")
	if c, err := Load(); err != nil || c.Agent.FixtureScript != script {
		t.Fatalf("fixture script config=%+v err=%v", c.Agent, err)
	}
	// Review of 104b474: a script path the worker cannot read fails at load, not per run.
	for name, path := range map[string]string{"missing": filepath.Join(t.TempDir(), "missing.json"), "directory": t.TempDir()} {
		t.Setenv("SUPERVISOR_AGENT_FIXTURE_SCRIPT", path)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SUPERVISOR_AGENT_FIXTURE_SCRIPT") || !strings.Contains(err.Error(), path) {
			t.Errorf("%s fixture script accepted or unnamed: %v", name, err)
		}
	}
}

func TestWorkerTenantAllowlistIsOptional(t *testing.T) {
	baseEnv(t)
	c, err := Load()
	if err != nil || len(c.Tenants) != 0 {
		t.Fatalf("empty allowlist: tenants=%v err=%v", c.Tenants, err)
	}
	t.Setenv("SUPERVISOR_RELAY_TENANT_IDS", `[]`)
	if c, err = Load(); err != nil || len(c.Tenants) != 0 {
		t.Fatalf("empty JSON allowlist: tenants=%v err=%v", c.Tenants, err)
	}
	t.Setenv("SUPERVISOR_RELAY_TENANT_IDS", `["00000000-0000-4000-8000-000000000290"]`)
	if c, err = Load(); err != nil || len(c.Tenants) != 1 {
		t.Fatalf("configured allowlist: tenants=%v err=%v", c.Tenants, err)
	}
	t.Setenv("SUPERVISOR_RELAY_TENANT_IDS", `["invalid"]`)
	if _, err = Load(); err == nil {
		t.Fatal("non-UUID allowlist accepted")
	}
	t.Setenv("SUPERVISOR_RELAY_TENANT_IDS", "not-json")
	if _, err = Load(); err == nil {
		t.Fatal("malformed allowlist accepted")
	}
}

// WIS-290: the session limits are configurable conservative defaults. A deployment may make
// them stricter; looser or malformed values stop startup.
func TestSessionPolicyIsConfigurableOnlyStricter(t *testing.T) {
	baseEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Sessions.Idle != 30*time.Minute || c.Sessions.Absolute != 8*time.Hour || c.Sessions.RecentAuth != 15*time.Minute {
		t.Fatalf("default sessions: %+v", c.Sessions)
	}
	t.Setenv("SUPERVISOR_SESSION_IDLE", "10m")
	t.Setenv("SUPERVISOR_SESSION_ABSOLUTE", "4h")
	t.Setenv("SUPERVISOR_SESSION_RECENT_AUTH", "5m")
	if c, err = Load(); err != nil || c.Sessions.Idle != 10*time.Minute || c.Sessions.Absolute != 4*time.Hour || c.Sessions.RecentAuth != 5*time.Minute {
		t.Fatalf("stricter sessions: %+v %v", c.Sessions, err)
	}
	for name, value := range map[string]string{"SUPERVISOR_SESSION_IDLE": "45m", "SUPERVISOR_SESSION_ABSOLUTE": "12h", "SUPERVISOR_SESSION_RECENT_AUTH": "soon"} {
		t.Setenv(name, value)
		if _, err := Load(); err == nil {
			t.Errorf("%s=%s accepted", name, value)
		}
		t.Setenv(name, "")
	}
}

// ADR 0002 Sec. 15.3: production database connections require TLS with server certificate
// verification; sslmode=require encrypts without verifying and is refused.
func TestProductionDatabaseRequiresCertificateVerification(t *testing.T) {
	t.Setenv("PGSSLMODE", "") // the DSNs below decide; the host's PGSSLMODE must not
	c := Config{Environment: "production", Issuer: "https://id.example/realm", Origin: "https://app.example", ClientID: "test", ClientSecret: "fixture", TemporalTLS: true}
	for dsn, ok := range map[string]bool{
		"postgresql://db.example/supervisor":                     false,
		"postgresql://db.example/supervisor?sslmode=require":     false,
		"postgresql://db.example/supervisor?sslmode=disable":     false,
		"postgresql://db.example/supervisor?sslmode=verify-ca":   true,
		"postgresql://db.example/supervisor?sslmode=verify-full": true,
		// WIS-699 (security audit run 1): the effective configuration decides, not the first
		// query value. pgx takes the last sslmode, and prefer falls back to plaintext.
		"postgresql://db.example/supervisor?sslmode=verify-full&sslmode=disable": false,
		"postgresql://db.example/supervisor?sslmode=verify-full&sslmode=require": false,
		"postgresql://db.example/supervisor?sslmode=prefer":                      false,
		// The keyword form is refused outright: only a postgresql:// URL is accepted.
		"host=db.example dbname=supervisor sslmode=verify-full": false,
	} {
		c.DatabaseURL = dsn
		if err := c.Validate(); (err == nil) != ok {
			t.Errorf("%s: %v", dsn, err)
		}
	}
	// WIS-699: local hosts only under "local".
	c.Environment, c.DatabaseURL, c.Issuer, c.Origin = "local", "postgresql://localhost/supervisor", "http://localhost/realm", "http://localhost:58176"
	if err := c.Validate(); err != nil {
		t.Fatalf("local development without TLS: %v", err)
	}
}

// The OEE execution lease defaults to 15s but never exceeds the run deadline, so a deployment
// with a short deadline keeps booting without a new setting.
func TestOEELeaseDefaultFollowsShortDeadline(t *testing.T) {
	baseEnv(t)
	t.Setenv("SUPERVISOR_OEE_RUN_DEADLINE", "10s")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.OEELeaseDuration != 10*time.Second {
		t.Fatalf("lease %v with a 10s deadline", c.OEELeaseDuration)
	}
	t.Setenv("SUPERVISOR_OEE_RUN_DEADLINE", "60s")
	if c, err = Load(); err != nil || c.OEELeaseDuration != 15*time.Second {
		t.Fatalf("default lease %v: %v", c.OEELeaseDuration, err)
	}
}

// WIS-275: the model endpoint is a pinned local service. Public literals, names outside the
// allowlist, and URLs that could carry credentials or redirect paths are refused at load.
func TestAgentEndpointMustBePinnedLocal(t *testing.T) {
	baseEnv(t)
	if c, err := Load(); err != nil || c.Agent.Provider != "" {
		t.Fatalf("unset provider must disable the runtime: %+v %v", c.Agent, err)
	}
	t.Setenv("SUPERVISOR_AGENT_PROVIDER", "local")
	t.Setenv("SUPERVISOR_AGENT_MODEL", "qwen2.5-7b-instruct")
	t.Setenv("SUPERVISOR_AGENT_MODEL_REVISION", "sha256:abc")
	t.Setenv("SUPERVISOR_AGENT_ENDPOINT_ALLOWLIST", "model.plant.internal")
	t.Setenv("SUPERVISOR_AGENT_ENDPOINT_ADDRS", "127.0.0.1,10.20.0.5:8000,fd00::5,100.64.1.1")
	for raw, ok := range map[string]bool{
		"http://127.0.0.1:8000":            true,
		"http://10.20.0.5:8000":            true,
		"https://[fd00::5]:8000":           true,
		"http://100.64.1.1:8000":           true,
		"http://model.plant.internal:8000": true,
		"http://8.8.8.8:8000":              false, // public literal
		"http://[::ffff:8.8.8.8]:8000":     false, // public behind a v4-mapped literal
		"http://169.254.169.254":           false, // link-local cloud metadata
		"http://0.0.0.0:8000":              false,
		"http://api.openai.com":            false, // name not allowlisted
		"http://user:pw@127.0.0.1:8000":    false,
		"http://127.0.0.1:8000/proxy":      false,
		"http://127.0.0.1:8000?x=1":        false,
		"http://10.20.0.6:8000":            false, // private but not in SUPERVISOR_AGENT_ENDPOINT_ADDRS
		"http://10.20.0.5:9000":            false, // pinned address, other port
		"ftp://127.0.0.1":                  false,
		"":                                 false,
	} {
		t.Setenv("SUPERVISOR_AGENT_BASE_URL", raw)
		if _, err := Load(); (err == nil) != ok {
			t.Errorf("%q: accepted=%v err=%v", raw, err == nil, err)
		}
	}
	t.Setenv("SUPERVISOR_AGENT_BASE_URL", "http://127.0.0.1:8000")
	for name, value := range map[string]string{"SUPERVISOR_AGENT_MODEL": "", "SUPERVISOR_AGENT_MODEL_REVISION": "a b", "SUPERVISOR_AGENT_TIMEOUT": "10m", "SUPERVISOR_AGENT_PROVIDER": "openai", "SUPERVISOR_AGENT_API_KEY_FILE": "/nonexistent/key"} {
		old := os.Getenv(name)
		t.Setenv(name, value)
		if _, err := Load(); err == nil {
			t.Errorf("%s=%q accepted", name, value)
		}
		t.Setenv(name, old)
	}
	// The destination pin is required and must itself be local.
	for _, addrs := range []string{"", "8.8.8.8", "not-an-ip", "127.0.0.1:99999"} {
		t.Setenv("SUPERVISOR_AGENT_ENDPOINT_ADDRS", addrs)
		if _, err := Load(); err == nil {
			t.Errorf("SUPERVISOR_AGENT_ENDPOINT_ADDRS=%q accepted", addrs)
		}
	}
	t.Setenv("SUPERVISOR_AGENT_ENDPOINT_ADDRS", "127.0.0.1")
	c, err := Load()
	if err != nil || c.Agent.Timeout != 30*time.Second || c.Agent.MaxConcurrent != 2 || c.Agent.MaxQueued != 8 || c.Agent.ScanTenantCap != 40 || c.Agent.ModeEnabled {
		t.Fatalf("defaults: %+v %v", c.Agent, err)
	}
	// WIS-715: the tenant scan cap holds at least one full scanned PDF (20 pages).
	for value, want := range map[string]int{"20": 20, "19": 0, "0": 0, "-1": 0, "10001": 0, "many": 0} {
		t.Setenv("SUPERVISOR_AGENT_SCAN_TENANT_CAP", value)
		c, err := Load()
		if want == 0 && err == nil || want != 0 && (err != nil || c.Agent.ScanTenantCap != want) {
			t.Errorf("SUPERVISOR_AGENT_SCAN_TENANT_CAP=%q: cap %d, err %v", value, c.Agent.ScanTenantCap, err)
		}
	}
}

// Rule 09: the deterministic-fixture adapter is refused in production.
func TestFixtureRejectedInProduction(t *testing.T) {
	c := Config{Environment: "production", DatabaseURL: "postgresql://localhost/db?sslmode=verify-full", Issuer: "https://localhost/realm", Origin: "https://app.example", ClientID: "test", ClientSecret: "fixture", TemporalTLS: true,
		Agent: Agent{Provider: AgentProviderFixture, Model: "fixture-model", ModelRevision: "r1", Timeout: 30 * time.Second, MaxConcurrent: 2, MaxQueued: 8}}
	if c.Validate() == nil {
		t.Fatal("fixture provider accepted in production")
	}
	c.Environment, c.Origin = "local", "http://localhost:58176" // WIS-699: local hosts only under "local"
	if err := c.Validate(); err != nil {
		t.Fatalf("fixture provider in local development: %v", err)
	}
}

// WIS-517: chat answering is an optional capability; it cannot be switched on without a model
// provider and agent mode, so a misconfiguration fails at startup instead of silently answering.
func TestChatAnsweringRequiresProviderAndMode(t *testing.T) {
	base := func() Config {
		return Config{Environment: "local", DatabaseURL: "postgresql://localhost/db", Issuer: "http://localhost/realm", Origin: "http://localhost", ClientID: "test", ClientSecret: "fixture",
			Agent: Agent{Provider: AgentProviderFixture, Model: "fixture-model", ModelRevision: "r1", Timeout: 30 * time.Second, MaxConcurrent: 2, MaxQueued: 8, ModeEnabled: true, ChatAnswering: true}}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("provider, mode and answering together: %v", err)
	}
	noMode := base()
	noMode.Agent.ModeEnabled = false
	if noMode.Validate() == nil {
		t.Fatal("chat answering accepted with agent mode off")
	}
	noProvider := base()
	noProvider.Agent = Agent{ChatAnswering: true, ModeEnabled: true}
	if noProvider.Validate() == nil {
		t.Fatal("chat answering accepted without a model provider")
	}
	off := base()
	off.Agent.ChatAnswering = false
	if err := off.Validate(); err != nil {
		t.Fatalf("answering off: %v", err)
	}
}

// WIS-625: the vision model is pinned with its revision, and only on the local provider: a page
// image is never routed to a hosted endpoint.
func TestVisionModelPinnedOnTheLocalProvider(t *testing.T) {
	baseEnv(t)
	t.Setenv("SUPERVISOR_AGENT_PROVIDER", "local")
	t.Setenv("SUPERVISOR_AGENT_MODEL", "qwen2.5-7b-instruct")
	t.Setenv("SUPERVISOR_AGENT_MODEL_REVISION", "sha256:abc")
	t.Setenv("SUPERVISOR_AGENT_BASE_URL", "http://127.0.0.1:8000")
	t.Setenv("SUPERVISOR_AGENT_ENDPOINT_ADDRS", "127.0.0.1")
	if c, err := Load(); err != nil || c.Agent.VisionModel != "" {
		t.Fatalf("no vision model: %+v %v", c.Agent, err)
	}
	t.Setenv("SUPERVISOR_AGENT_VISION_MODEL", "qwen2.5-vl-7b-instruct")
	t.Setenv("SUPERVISOR_AGENT_VISION_MODEL_REVISION", "sha256:vision")
	if c, err := Load(); err != nil || c.Agent.VisionModel != "qwen2.5-vl-7b-instruct" || c.Agent.VisionModelRevision != "sha256:vision" {
		t.Fatalf("pinned vision model: %+v %v", c.Agent, err)
	}
	for name, value := range map[string]string{"SUPERVISOR_AGENT_VISION_MODEL_REVISION": "", "SUPERVISOR_AGENT_VISION_MODEL": "a b"} {
		old := os.Getenv(name)
		t.Setenv(name, value)
		if _, err := Load(); err == nil {
			t.Errorf("%s=%q accepted", name, value)
		}
		t.Setenv(name, old)
	}
}

// WIS-627: a CPU embedding server needs ~4s for one short text, so the 3s question-time budget
// timed out every indexing batch. Indexing has its own, longer budget; both are bounded.
func TestEmbeddingIndexTimeoutDefaultsLongerThanQuestionTime(t *testing.T) {
	baseEnv(t)
	t.Setenv("SUPERVISOR_EMBEDDINGS_ENABLED", "true")
	t.Setenv("SUPERVISOR_EMBEDDINGS_BASE_URL", "http://127.0.0.1:8081")
	t.Setenv("SUPERVISOR_EMBEDDINGS_ENDPOINT_ADDRS", "127.0.0.1")
	t.Setenv("SUPERVISOR_EMBEDDINGS_MODEL", "qwen3-embedding-0.6b")
	t.Setenv("SUPERVISOR_EMBEDDINGS_MODEL_REVISION", "r1")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Embeddings.Timeout != 3*time.Second || c.Embeddings.IndexTimeout != 60*time.Second {
		t.Fatalf("timeouts question=%v index=%v", c.Embeddings.Timeout, c.Embeddings.IndexTimeout)
	}
	for _, v := range []string{"50ms", "10m", "soon"} {
		t.Setenv("SUPERVISOR_EMBEDDINGS_INDEX_TIMEOUT", v)
		if _, err := Load(); err == nil || err.Error() != "invalid SUPERVISOR_EMBEDDINGS_INDEX_TIMEOUT" {
			t.Errorf("SUPERVISOR_EMBEDDINGS_INDEX_TIMEOUT=%q: %v", v, err)
		}
	}
}

// WIS-699: SUPERVISOR_TRUSTED_PROXY_CIDRS lists the proxies whose X-Forwarded-For the auth-route
// rate limit believes. Unset trusts none; a malformed entry stops the boot.
func TestTrustedProxyCIDRs(t *testing.T) {
	baseEnv(t)
	t.Setenv("SUPERVISOR_TRUSTED_PROXY_CIDRS", "")
	if c, err := Load(); err != nil || len(c.TrustedProxies) != 0 {
		t.Fatalf("unset: %v %v", c.TrustedProxies, err)
	}
	t.Setenv("SUPERVISOR_TRUSTED_PROXY_CIDRS", "10.1.2.3/8, fd00::/8")
	c, err := Load()
	if err != nil || len(c.TrustedProxies) != 2 || c.TrustedProxies[0].String() != "10.0.0.0/8" || c.TrustedProxies[1].String() != "fd00::/8" {
		t.Fatalf("parsed %v %v", c.TrustedProxies, err)
	}
	for _, bad := range []string{"10.0.0.1", "proxy.example", "10.0.0.0/33"} {
		t.Setenv("SUPERVISOR_TRUSTED_PROXY_CIDRS", bad)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SUPERVISOR_TRUSTED_PROXY_CIDRS") {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}

// WIS-699: "local" drops database and Temporal TLS verification, secure cookies and the fixture
// refusal, so it is refused for any public host; production is not affected by this rule.
func TestLocalEnvironmentRequiresLocalHosts(t *testing.T) {
	local := func() Config {
		return Config{Environment: "local", DatabaseURL: "postgresql://127.0.0.1:55477/supervisor", Issuer: "http://127.0.0.1:58077/realms/supervisor-local",
			Origin: "http://127.0.0.1:58176", ClientID: "test", ClientSecret: "fixture", TemporalAddress: "127.0.0.1:57277"}
	}
	if err := local().Validate(); err != nil {
		t.Fatalf("the documented local setup: %v", err)
	}
	for _, c := range []struct {
		name, want string
		edit       func(*Config)
	}{
		{"public HTTPS origin", "SUPERVISOR_PUBLIC_ORIGIN", func(c *Config) { c.Origin = "https://supervisor.example.com" }},
		{"public HTTPS issuer", "SUPERVISOR_OIDC_ISSUER", func(c *Config) { c.Issuer = "https://login.example.com/realms/x" }},
		{"public database host", "SUPERVISOR_DATABASE_URL", func(c *Config) { c.DatabaseURL = "postgresql://db.example.com/supervisor" }},
		{"public database fallback", "SUPERVISOR_DATABASE_URL", func(c *Config) { c.DatabaseURL = "postgresql://127.0.0.1,db.example.com/supervisor" }},
		{"public database address", "SUPERVISOR_DATABASE_URL", func(c *Config) { c.DatabaseURL = "postgresql://8.8.8.8/supervisor" }},
		{"public Temporal host", "SUPERVISOR_TEMPORAL_ADDRESS", func(c *Config) { c.TemporalAddress = "temporal.example.com:7233" }},
		// Review of #286: a dotless name is not local by itself; a resolver search domain can turn
		// "db" into a public host while local mode skips TLS verification.
		{"dotless database host", "SUPERVISOR_DATABASE_URL", func(c *Config) { c.DatabaseURL = "postgresql://db/supervisor" }},
		{"dotless Temporal host", "SUPERVISOR_TEMPORAL_ADDRESS", func(c *Config) { c.TemporalAddress = "frontend:7233" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := local()
			c.edit(&cfg)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one naming %s", err, c.want)
			}
		})
	}
	// Local hosts that are accepted: compose service names, private and loopback addresses,
	// localhost, a socket directory, and HTTPS on a local host.
	for _, edit := range []func(*Config){
		func(c *Config) {
			c.DatabaseURL = "postgresql://postgres:5432/supervisor"
			c.TemporalAddress = "temporal:7233"
		},
		func(c *Config) {
			c.DatabaseURL = "postgresql://10.253.77.5/supervisor"
			c.Issuer = "http://keycloak:8080/realms/x"
		},
		func(c *Config) { c.DatabaseURL = "postgresql:///supervisor?host=/var/run/postgresql" },
		func(c *Config) { c.Origin = "https://localhost:8443"; c.TemporalAddress = "[::1]:7233" },
	} {
		cfg := local()
		edit(&cfg)
		if err := cfg.Validate(); err != nil {
			t.Errorf("local host refused: %+v: %v", cfg, err)
		}
	}
	// Control: production keeps its own rules and is not restricted to local hosts.
	prod := Config{Environment: "production", DatabaseURL: "postgresql://db.example.com/supervisor?sslmode=verify-full", Issuer: "https://login.example.com/realms/x",
		Origin: "https://supervisor.example.com", ClientID: "test", ClientSecret: "fixture", TemporalTLS: true, TemporalAddress: "temporal.example.com:7233"}
	if err := prod.Validate(); err != nil {
		t.Fatalf("production with public hosts: %v", err)
	}
}

// WIS-699: cookies are Secure whenever the origin is HTTPS, not only under the production label.
func TestSecureCookiesFollowHTTPS(t *testing.T) {
	for _, c := range []struct {
		env, origin string
		want        bool
	}{
		{"production", "https://supervisor.example.com", true},
		{"local", "http://127.0.0.1:58176", false},
		{"local", "https://localhost:8443", true},
		{"local", "HTTPS://localhost:8443", true},
	} {
		if got := (Config{Environment: c.env, Origin: c.origin}).SecureCookies(); got != c.want {
			t.Errorf("%s %s: secure cookies %v, want %v", c.env, c.origin, got, c.want)
		}
	}
}

// WIS-699: migrate, provision and bootstrap-invite read their URLs without loading the full
// configuration, so the environment's DSN rule is applied to them here. An unset environment is
// production.
func TestAdminDSNFollowsTheEnvironment(t *testing.T) {
	for _, c := range []struct {
		env, dsn, want string // want: "" accepted, else a substring of the refusal
	}{
		{"", "postgresql://db.example.com/supervisor?sslmode=disable", "verify the server certificate"},
		{"production", "postgresql://db.example.com/supervisor?sslmode=require", "verify the server certificate"},
		{"production", "postgresql://db.example.com/supervisor?sslmode=verify-full&sslmode=disable", "verify the server certificate"},
		{"production", "postgresql://db.example.com/supervisor?sslmode=verify-full", ""},
		{"local", "postgresql://supervisor_migrator:pw@127.0.0.1:55477/supervisor", ""},
		{"local", "postgresql://supervisor_migrator:pw@postgres:5432/supervisor", ""},
		{"local", "postgresql://supervisor_migrator:pw@db.example.com/supervisor", "service-name host"},
		{"staging", "postgresql://127.0.0.1/supervisor", "invalid environment"},
		{"local", "host=127.0.0.1 dbname=supervisor", "must be a postgresql:// URL"},
		{"local", "", "must be a postgresql:// URL"},
	} {
		t.Setenv("SUPERVISOR_ENVIRONMENT", c.env)
		err := ValidateAdminDSN("SUPERVISOR_MIGRATION_URL", c.dsn)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%q %q refused: %v", c.env, c.dsn, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%q %q: err = %v, want %q", c.env, c.dsn, err, c.want)
		case err != nil && (strings.Contains(err.Error(), "pw@") || strings.Contains(err.Error(), "example.com")):
			t.Errorf("refusal echoes the DSN: %v", err)
		}
	}
}

// WIS-631: SUPERVISOR_AGENT_REASONING_EFFORT is optional, local-provider only, and one of four
// values; anything else stops the boot.
func TestReasoningEffortSetting(t *testing.T) {
	local := func(effort, provider string) Config {
		return Config{Environment: "local", DatabaseURL: "postgresql://localhost/db", Issuer: "http://localhost/realm", Origin: "http://localhost", ClientID: "test", ClientSecret: "fixture",
			Agent: Agent{Provider: provider, BaseURL: "http://127.0.0.1:11434", EndpointAddrs: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:11434")}, Model: "qwen3.5:9b", ModelRevision: "r1", ReasoningEffort: effort, Timeout: 30 * time.Second, MaxConcurrent: 2, MaxQueued: 8}}
	}
	for _, c := range []struct {
		effort, provider string
		ok               bool
	}{
		{"", AgentProviderLocal, true}, {"none", AgentProviderLocal, true}, {"low", AgentProviderLocal, true}, {"high", AgentProviderLocal, true},
		{"off", AgentProviderLocal, false}, {"NONE", AgentProviderLocal, false}, {"none", AgentProviderFixture, false},
	} {
		if err := local(c.effort, c.provider).Validate(); (err == nil) != c.ok {
			t.Errorf("effort %q provider %q: err = %v, want ok=%v", c.effort, c.provider, err, c.ok)
		}
	}
}

// WIS-700: the embeddings endpoint check names the SUPERVISOR_EMBEDDINGS_* setting at fault on
// every branch (it is given the prefix; it used to rewrite the agent check's wording), and the
// agent check keeps naming its own settings.
func TestWIS700EmbeddingsEndpointErrorsNameTheirSettings(t *testing.T) {
	local := []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:8081")}
	for _, c := range []struct {
		name string
		e    Embeddings
		want string
	}{
		{"malformed URL", Embeddings{BaseURL: "ftp://127.0.0.1", EndpointAddrs: local}, "invalid SUPERVISOR_EMBEDDINGS_BASE_URL"},
		{"public address", Embeddings{BaseURL: "http://8.8.8.8:8081", EndpointAddrs: local}, "SUPERVISOR_EMBEDDINGS_BASE_URL must be a loopback or private address"},
		{"host not allowlisted", Embeddings{BaseURL: "http://embed.internal:8081", EndpointAddrs: local}, "SUPERVISOR_EMBEDDINGS_BASE_URL host is not in SUPERVISOR_EMBEDDINGS_ENDPOINT_ALLOWLIST"},
		{"no pinned address", Embeddings{BaseURL: "http://127.0.0.1:8081"}, "SUPERVISOR_EMBEDDINGS_ENDPOINT_ADDRS is required"},
		{"public pinned address", Embeddings{BaseURL: "http://127.0.0.1:8081", EndpointAddrs: []netip.AddrPort{netip.MustParseAddrPort("8.8.8.8:8081")}}, "SUPERVISOR_EMBEDDINGS_ENDPOINT_ADDRS must list loopback or private addresses"},
		{"address not pinned", Embeddings{BaseURL: "http://127.0.0.2:8081", EndpointAddrs: local}, "SUPERVISOR_EMBEDDINGS_BASE_URL address is not in SUPERVISOR_EMBEDDINGS_ENDPOINT_ADDRS"},
	} {
		if _, err := c.e.Endpoint(); err == nil || err.Error() != c.want {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	if _, err := (Embeddings{BaseURL: "http://127.0.0.1:8081", EndpointAddrs: local}).Endpoint(); err != nil {
		t.Fatalf("valid endpoint: %v", err)
	}
	if _, err := (Agent{BaseURL: "http://127.0.0.1:8081"}).PinnedEndpoint(); err == nil || err.Error() != "SUPERVISOR_AGENT_ENDPOINT_ADDRS is required" {
		t.Fatalf("agent check: %v", err)
	} // Loading keeps the endpoint error wrapped (%w), not flattened into a new string.
	t.Setenv("SUPERVISOR_EMBEDDINGS_ENABLED", "true")
	t.Setenv("SUPERVISOR_EMBEDDINGS_BASE_URL", "http://127.0.0.2:8081")
	t.Setenv("SUPERVISOR_EMBEDDINGS_ENDPOINT_ADDRS", "127.0.0.1")
	t.Setenv("SUPERVISOR_EMBEDDINGS_MODEL", "qwen3-embedding-0.6b")
	t.Setenv("SUPERVISOR_EMBEDDINGS_MODEL_REVISION", "r1")
	_, err := LoadEmbeddings()
	if inner := errors.Unwrap(err); inner == nil || inner.Error() != "SUPERVISOR_EMBEDDINGS_BASE_URL address is not in SUPERVISOR_EMBEDDINGS_ENDPOINT_ADDRS" {
		t.Fatalf("loaded: %v, wrapping %v", err, inner)
	}
}
