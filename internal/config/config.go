package config

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"net"
	"net/netip"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// TrustedProxies are the proxy ranges whose X-Forwarded-For is believed when keying the
	// auth-route rate limit (SUPERVISOR_TRUSTED_PROXY_CIDRS, WIS-699). Empty: the header is ignored.
	TrustedProxies                                                                    []netip.Prefix
	WorkstationCPSATPath                                                              string
	Environment, DatabaseURL, Issuer, ClientID, ClientSecret, Origin, Address, WebDir string
	TemporalAddress, TemporalNamespace, TemporalQueue, TemporalAPIKey                 string
	TemporalTLS                                                                       bool
	Tenants                                                                           []string // optional worker relay allowlist
	// OEE service limits (WIS-229, ADR 0002 Sec. 10). Provisional until the performance
	// envelope is measured; each can be overridden per deployment.
	OEERunDeadline             time.Duration
	OEELeaseDuration           time.Duration
	OEEMaxOpenRuns             int
	OEEMaxConcurrentExecutions int
	OEEIdempotencyRetention    time.Duration
	// Sessions is the one browser-session policy every service enforces (WIS-290). Each limit
	// may be set stricter than the default, never looser.
	Sessions session.Policy
	// Agent is the WIS-275 model runtime. An empty Provider disables the runtime (the rollback).
	Agent Agent
	// Embeddings is the optional WIS-627 semantic retrieval (off by default).
	Embeddings Embeddings
	// TaskExecution lets the WIS-277 executor consume approved proposals into internal tasks.
	// Off by default: until a site policy commissions execution, an approval is never consumed
	// and expires at the end of its window.
	TaskExecution bool
	// AdvisorSuggestionsDisabled switches the read-only WIS-629 advisor suggestion off (its rollback).
	// The suggestion is assembled on read and stores nothing, so switching it off loses nothing.
	AdvisorSuggestionsDisabled bool
}

// Agent configures the pinned local model service (WIS-275, design section 2). There is one
// endpoint and no fallback. APIKeyFile is a path; the key itself never enters Config.
type Agent struct {
	Provider          string // "" (disabled) | AgentProviderLocal | AgentProviderFixture
	FixtureScript     string
	BaseURL           string
	Model             string
	ModelRevision     string
	APIKeyFile        string
	EndpointAllowlist []string // host names allowed in BaseURL; IP literals need no entry
	// EndpointAddrs pins the destination identity: the exact IP (any port) or IP:port set the
	// dialer may connect to. A resolved address outside it is refused even when it is private,
	// so DNS cannot move the key and prompts to another local service. Required when enabled.
	EndpointAddrs []netip.AddrPort
	// VisionModel and VisionModelRevision pin the vision-language model OCR uses (WIS-625), served
	// by the same local endpoint. Both or neither; without them scans are refused.
	VisionModel         string
	VisionModelRevision string
	// ReasoningEffort is sent as reasoning_effort on the local provider's chat requests
	// (SUPERVISOR_AGENT_REASONING_EFFORT: none, low, medium or high). Empty sends nothing. A
	// thinking model such as qwen3.5 otherwise spends its output budget reasoning (WIS-631).
	ReasoningEffort string
	Timeout         time.Duration
	MaxConcurrent   int
	MaxQueued       int
	// ScanTenantCap is the running OCR scan pages one tenant may hold (WIS-715,
	// SUPERVISOR_AGENT_SCAN_TENANT_CAP, default 40). At least one full scanned PDF (20 pages).
	ScanTenantCap int
	ModeEnabled   bool
	// ChatAnswering commissions WIS-517 chat answers. It is an optional capability enabled only
	// after its own WIS-280 qualification, and it requires a provider and agent mode.
	ChatAnswering bool
	// Hosted is the optional hosted inference route (WIS-623, rule 08). Configured or not, a
	// site uses it only while its owner has switched it on; Provider stays the default route.
	Hosted HostedAgent
}

// HostedAgent selects one compiled-in hosted endpoint and pins its model snapshot. The URL and
// region are never configuration: they come from HostedEndpoints.
type HostedAgent struct {
	Provider      string // "" (no hosted route) | a HostedEndpoints key
	Model         string // the snapshot sent as "model", e.g. qwen-plus-2025-09-11
	ModelRevision string
	APIKeyFile    string
}

// HostedEndpoint is one allowlisted hosted inference endpoint, compiled into the binary.
type HostedEndpoint struct {
	BaseURL string
	Region  string
}

// HostedEndpoints is the static allowlist of hosted inference endpoints (rule 08): Qwen on
// Alibaba Cloud Model Studio, International (Singapore) only. Adding a host is a code change
// and a review, never a setting. BaseURL stops before the API version: the OpenAI-compatible
// adapter adds /v1/chat/completions itself, as it does for a local endpoint.
var HostedEndpoints = map[string]HostedEndpoint{
	"qwen-intl": {BaseURL: "https://dashscope-intl.aliyuncs.com/compatible-mode", Region: "ap-southeast-1"},
}

const (
	AgentProviderLocal   = "local"
	AgentProviderFixture = "fixture"
)

func Load() (Config, error) {
	c := Config{Environment: env("SUPERVISOR_ENVIRONMENT", "production"), DatabaseURL: os.Getenv("SUPERVISOR_DATABASE_URL"), Issuer: os.Getenv("SUPERVISOR_OIDC_ISSUER"), ClientID: env("SUPERVISOR_OIDC_CLIENT_ID", "supervisor"), ClientSecret: os.Getenv("SUPERVISOR_OIDC_CLIENT_SECRET"), Origin: os.Getenv("SUPERVISOR_PUBLIC_ORIGIN"), Address: env("SUPERVISOR_LISTEN_ADDRESS", "127.0.0.1:58176"), WebDir: env("SUPERVISOR_WEB_DIST", "web/dist"), TemporalAddress: env("SUPERVISOR_TEMPORAL_ADDRESS", "localhost:7233"), TemporalNamespace: env("SUPERVISOR_TEMPORAL_NAMESPACE", "default"), TemporalQueue: env("SUPERVISOR_TEMPORAL_TASK_QUEUE", "supervisor-foundation-go"), TemporalAPIKey: os.Getenv("SUPERVISOR_TEMPORAL_API_KEY"), TemporalTLS: os.Getenv("SUPERVISOR_TEMPORAL_TLS") == "true"}
	c.WorkstationCPSATPath = env("SUPERVISOR_WORKSTATION_CPSAT_PATH", "/usr/local/bin/supervisor_workstation_cpsat")
	var err error
	if c.OEERunDeadline, err = time.ParseDuration(env("SUPERVISOR_OEE_RUN_DEADLINE", "60s")); err != nil {
		return c, errors.New("invalid OEE run deadline")
	}
	// Default lease: 15s, but never longer than the run deadline, so deployments with a short
	// deadline keep booting without a new setting.
	defaultLease := min(15*time.Second, c.OEERunDeadline)
	if c.OEELeaseDuration, err = time.ParseDuration(env("SUPERVISOR_OEE_LEASE", defaultLease.String())); err != nil {
		return c, errors.New("invalid OEE execution lease")
	}
	if c.OEEMaxOpenRuns, err = strconv.Atoi(env("SUPERVISOR_OEE_MAX_OPEN_RUNS", "8")); err != nil {
		return c, errors.New("invalid OEE open-run budget")
	}
	// Default: one executing run per available CPU, since calculation is CPU-bound.
	if c.OEEMaxConcurrentExecutions, err = strconv.Atoi(env("SUPERVISOR_OEE_MAX_CONCURRENT_EXECUTIONS", strconv.Itoa(runtime.GOMAXPROCS(0)))); err != nil {
		return c, errors.New("invalid OEE execution budget")
	}
	if c.OEEIdempotencyRetention, err = time.ParseDuration(env("SUPERVISOR_OEE_IDEMPOTENCY_RETENTION", "2160h")); err != nil {
		return c, errors.New("invalid OEE idempotency retention")
	}
	if c.OEERunDeadline <= 0 || c.OEERunDeadline > time.Hour || c.OEELeaseDuration < time.Second || c.OEELeaseDuration > c.OEERunDeadline ||
		c.OEEMaxOpenRuns < 1 || c.OEEMaxOpenRuns > 1024 || c.OEEMaxConcurrentExecutions < 1 || c.OEEMaxConcurrentExecutions > 1024 || c.OEEIdempotencyRetention <= 0 {
		return c, errors.New("invalid OEE service limits")
	}
	c.TaskExecution = os.Getenv("SUPERVISOR_TASK_EXECUTION_ENABLED") == "true"
	c.AdvisorSuggestionsDisabled = os.Getenv("SUPERVISOR_ADVISOR_SUGGESTIONS_DISABLED") == "true"
	c.Sessions = session.Default()
	for _, setting := range []struct {
		name   string
		target *time.Duration
	}{{"SUPERVISOR_SESSION_IDLE", &c.Sessions.Idle}, {"SUPERVISOR_SESSION_ABSOLUTE", &c.Sessions.Absolute}, {"SUPERVISOR_SESSION_RECENT_AUTH", &c.Sessions.RecentAuth}} {
		if v := os.Getenv(setting.name); v != "" {
			if *setting.target, err = time.ParseDuration(v); err != nil {
				return c, errors.New("invalid " + setting.name)
			}
		}
	}
	if err = c.Sessions.Validate(); err != nil {
		return c, err
	}
	if s := os.Getenv("SUPERVISOR_RELAY_TENANT_IDS"); s != "" {
		if err := json.Unmarshal([]byte(s), &c.Tenants); err != nil {
			return c, errors.New("invalid relay tenant list")
		}
	}
	if c.Agent, err = loadAgent(); err != nil {
		return c, err
	}
	for _, raw := range strings.Split(os.Getenv("SUPERVISOR_TRUSTED_PROXY_CIDRS"), ",") {
		if raw = strings.TrimSpace(raw); raw == "" {
			continue
		}
		p, e := netip.ParsePrefix(raw)
		if e != nil {
			return c, errors.New("invalid SUPERVISOR_TRUSTED_PROXY_CIDRS")
		}
		c.TrustedProxies = append(c.TrustedProxies, p.Masked())
	}
	if c.Embeddings, err = loadEmbeddings(); err != nil {
		return c, err
	}
	return c, c.Validate()
}

// Embeddings configures the local embedding and reranking endpoints for semantic retrieval
// (WIS-627): an OpenAI-compatible /v1/embeddings and /v1/rerank on a local or private, pinned
// address (the same endpoint rules as the local model). Off by default; without it chat ranks
// passages by word match (WIS-626).
type Embeddings struct {
	Enabled             bool
	BaseURL             string
	EndpointAllowlist   []string
	EndpointAddrs       []netip.AddrPort
	Model               string // e.g. Qwen3-Embedding-0.6B, 1024 dimensions
	ModelRevision       string
	RerankModel         string // optional
	RerankModelRevision string
	Timeout             time.Duration // question-time embed and rerank; chat must not wait long
	IndexTimeout        time.Duration // background span indexing; CPU endpoints need tens of seconds (WIS-627)
}

// LoadEmbeddings reads and validates the SUPERVISOR_EMBEDDINGS_* settings alone, for tools that
// need semantic retrieval without the whole service configuration (supervisor-eval, WIS-647).
func LoadEmbeddings() (Embeddings, error) {
	e, err := loadEmbeddings()
	if err == nil {
		err = e.validate()
	}
	return e, err
}

func loadEmbeddings() (Embeddings, error) {
	e := Embeddings{Enabled: os.Getenv("SUPERVISOR_EMBEDDINGS_ENABLED") == "true", BaseURL: os.Getenv("SUPERVISOR_EMBEDDINGS_BASE_URL"), Model: os.Getenv("SUPERVISOR_EMBEDDINGS_MODEL"),
		ModelRevision: os.Getenv("SUPERVISOR_EMBEDDINGS_MODEL_REVISION"), RerankModel: os.Getenv("SUPERVISOR_EMBEDDINGS_RERANK_MODEL"), RerankModelRevision: os.Getenv("SUPERVISOR_EMBEDDINGS_RERANK_MODEL_REVISION")}
	if !e.Enabled {
		return e, nil
	}
	if s := os.Getenv("SUPERVISOR_EMBEDDINGS_ENDPOINT_ALLOWLIST"); s != "" {
		e.EndpointAllowlist = strings.Split(s, ",")
	}
	for _, raw := range strings.Split(os.Getenv("SUPERVISOR_EMBEDDINGS_ENDPOINT_ADDRS"), ",") {
		if raw = strings.TrimSpace(raw); raw == "" {
			continue
		}
		ap, err := netip.ParseAddrPort(raw)
		if err != nil {
			ip, e2 := netip.ParseAddr(raw)
			if e2 != nil {
				return e, errors.New("invalid SUPERVISOR_EMBEDDINGS_ENDPOINT_ADDRS")
			}
			ap = netip.AddrPortFrom(ip, 0)
		}
		e.EndpointAddrs = append(e.EndpointAddrs, ap)
	}
	var err error
	if e.Timeout, err = time.ParseDuration(env("SUPERVISOR_EMBEDDINGS_TIMEOUT", "3s")); err != nil {
		return e, errors.New("invalid SUPERVISOR_EMBEDDINGS_TIMEOUT")
	}
	if e.IndexTimeout, err = time.ParseDuration(env("SUPERVISOR_EMBEDDINGS_INDEX_TIMEOUT", "60s")); err != nil {
		return e, errors.New("invalid SUPERVISOR_EMBEDDINGS_INDEX_TIMEOUT")
	}
	return e, nil
}

// Endpoint returns the pinned base URL, validated like the local model endpoint. Its errors name
// the SUPERVISOR_EMBEDDINGS_* settings (WIS-647); the shared check is given that prefix, rather
// than its agent wording being rewritten afterwards (WIS-700).
func (e Embeddings) Endpoint() (*url.URL, error) {
	return Agent{BaseURL: e.BaseURL, EndpointAllowlist: e.EndpointAllowlist, EndpointAddrs: e.EndpointAddrs}.pinnedEndpoint("SUPERVISOR_EMBEDDINGS_")
}

// Pinned reports whether a resolved address may be dialled (local and in the pinned set).
func (e Embeddings) Pinned(ip netip.Addr, port uint16) bool {
	return Agent{EndpointAddrs: e.EndpointAddrs}.Pinned(ip, port)
}

func (e Embeddings) validate() error {
	if !e.Enabled {
		return nil
	}
	if _, err := e.Endpoint(); err != nil {
		return fmt.Errorf("SUPERVISOR_EMBEDDINGS_BASE_URL: %w", err)
	}
	if !pin(e.Model) || !pin(e.ModelRevision) || (e.RerankModel != "") != (e.RerankModelRevision != "") || (e.RerankModel != "" && (!pin(e.RerankModel) || !pin(e.RerankModelRevision))) {
		return errors.New("SUPERVISOR_EMBEDDINGS_MODEL and _MODEL_REVISION are required; a rerank model needs its revision")
	}
	if e.Timeout < 100*time.Millisecond || e.Timeout > time.Minute {
		return errors.New("invalid SUPERVISOR_EMBEDDINGS_TIMEOUT")
	}
	if e.IndexTimeout < 100*time.Millisecond || e.IndexTimeout > 5*time.Minute {
		return errors.New("invalid SUPERVISOR_EMBEDDINGS_INDEX_TIMEOUT")
	}
	return nil
}

func loadAgent() (Agent, error) {
	a := Agent{Provider: os.Getenv("SUPERVISOR_AGENT_PROVIDER"), FixtureScript: os.Getenv("SUPERVISOR_AGENT_FIXTURE_SCRIPT"), BaseURL: os.Getenv("SUPERVISOR_AGENT_BASE_URL"), Model: os.Getenv("SUPERVISOR_AGENT_MODEL"), ModelRevision: os.Getenv("SUPERVISOR_AGENT_MODEL_REVISION"), APIKeyFile: os.Getenv("SUPERVISOR_AGENT_API_KEY_FILE"), ModeEnabled: os.Getenv("SUPERVISOR_AGENT_MODE_ENABLED") == "true", ChatAnswering: os.Getenv("SUPERVISOR_CHAT_ANSWERING_ENABLED") == "true"}
	a.VisionModel, a.VisionModelRevision = os.Getenv("SUPERVISOR_AGENT_VISION_MODEL"), os.Getenv("SUPERVISOR_AGENT_VISION_MODEL_REVISION")
	a.Hosted = HostedAgent{Provider: os.Getenv("SUPERVISOR_AGENT_HOSTED_PROVIDER"), Model: os.Getenv("SUPERVISOR_AGENT_HOSTED_MODEL"), ModelRevision: os.Getenv("SUPERVISOR_AGENT_HOSTED_MODEL_REVISION"), APIKeyFile: os.Getenv("SUPERVISOR_AGENT_HOSTED_API_KEY_FILE")}
	if a.Hosted.APIKeyFile != "" {
		if info, e := os.Stat(a.Hosted.APIKeyFile); e != nil || !info.Mode().IsRegular() {
			return a, errors.New("invalid SUPERVISOR_AGENT_HOSTED_API_KEY_FILE")
		}
	}
	if a.Provider == "" {
		return a, nil
	}
	if s := os.Getenv("SUPERVISOR_AGENT_ENDPOINT_ALLOWLIST"); s != "" {
		a.EndpointAllowlist = strings.Split(s, ",")
	}
	var err error
	if s := os.Getenv("SUPERVISOR_AGENT_ENDPOINT_ADDRS"); s != "" {
		for _, raw := range strings.Split(s, ",") {
			raw = strings.TrimSpace(raw)
			ap, e := netip.ParseAddrPort(raw)
			if e != nil {
				ip, e2 := netip.ParseAddr(raw)
				if e2 != nil {
					return a, errors.New("invalid SUPERVISOR_AGENT_ENDPOINT_ADDRS")
				}
				ap = netip.AddrPortFrom(ip, 0)
			}
			a.EndpointAddrs = append(a.EndpointAddrs, ap)
		}
	}
	a.ReasoningEffort = os.Getenv("SUPERVISOR_AGENT_REASONING_EFFORT")
	if a.Timeout, err = time.ParseDuration(env("SUPERVISOR_AGENT_TIMEOUT", "30s")); err != nil {
		return a, errors.New("invalid SUPERVISOR_AGENT_TIMEOUT")
	}
	if a.MaxConcurrent, err = strconv.Atoi(env("SUPERVISOR_AGENT_MAX_CONCURRENT", "2")); err != nil {
		return a, errors.New("invalid SUPERVISOR_AGENT_MAX_CONCURRENT")
	}
	if a.MaxQueued, err = strconv.Atoi(env("SUPERVISOR_AGENT_MAX_QUEUED", "8")); err != nil {
		return a, errors.New("invalid SUPERVISOR_AGENT_MAX_QUEUED")
	}
	if a.ScanTenantCap, err = strconv.Atoi(env("SUPERVISOR_AGENT_SCAN_TENANT_CAP", "40")); err != nil || a.ScanTenantCap < 20 || a.ScanTenantCap > 10000 {
		return a, errors.New("invalid SUPERVISOR_AGENT_SCAN_TENANT_CAP")
	}
	if a.APIKeyFile != "" {
		if info, e := os.Stat(a.APIKeyFile); e != nil || !info.Mode().IsRegular() {
			return a, errors.New("invalid SUPERVISOR_AGENT_API_KEY_FILE")
		}
	}
	if a.FixtureScript != "" {
		// The worker parses it at startup; a missing or unreadable path fails here, in every mode.
		f, e := os.Open(a.FixtureScript)
		if e != nil {
			return a, fmt.Errorf("SUPERVISOR_AGENT_FIXTURE_SCRIPT %q is not readable", a.FixtureScript)
		}
		info, e := f.Stat()
		f.Close()
		if e != nil || !info.Mode().IsRegular() {
			return a, fmt.Errorf("SUPERVISOR_AGENT_FIXTURE_SCRIPT %q is not a regular file", a.FixtureScript)
		}
	}
	return a, nil
}

// LocalAddr reports whether a resolved model-service address is loopback, RFC 1918/ULA
// (netip IsPrivate) or RFC 6598 shared space. Everything else, including link-local cloud
// metadata and unspecified addresses, is public for the agent endpoint.
func LocalAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || a.IsPrivate() || sharedAddressSpace.Contains(a)
}

var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

// AgentEndpoint validates the model base URL: http(s), no userinfo, query, fragment or path,
// and a host that is a local IP literal or an allowlisted name. The provider re-checks the
// resolved address at dial time, so an allowlisted name cannot resolve to a public IP.
func AgentEndpoint(raw string, allowlist []string) (*url.URL, error) {
	return endpoint("SUPERVISOR_AGENT_", raw, allowlist)
}

// endpoint is AgentEndpoint with its errors naming the settings under prefix.
func endpoint(prefix, raw string, allowlist []string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("invalid " + prefix + "BASE_URL")
	}
	if ip, e := netip.ParseAddr(u.Hostname()); e == nil {
		if !LocalAddr(ip) || ip.Zone() != "" {
			return nil, errors.New(prefix + "BASE_URL must be a loopback or private address")
		}
		return u, nil
	}
	for _, name := range allowlist {
		if strings.EqualFold(strings.TrimSpace(name), u.Hostname()) {
			return u, nil
		}
	}
	return nil, errors.New(prefix + "BASE_URL host is not in " + prefix + "ENDPOINT_ALLOWLIST")
}

// Pinned reports whether the dialer may connect to ip:port: the address (unmapped) is local and
// in EndpointAddrs, with a matching port unless the entry has port 0.
func (a Agent) Pinned(ip netip.Addr, port uint16) bool {
	ip = ip.Unmap()
	if !LocalAddr(ip) {
		return false
	}
	for _, p := range a.EndpointAddrs {
		if p.Addr().Unmap() == ip && (p.Port() == 0 || p.Port() == port) {
			return true
		}
	}
	return false
}

// PinnedEndpoint validates the base URL (AgentEndpoint) and the destination pin: EndpointAddrs is
// non-empty and local, and an IP-literal base URL is itself in the set.
func (a Agent) PinnedEndpoint() (*url.URL, error) { return a.pinnedEndpoint("SUPERVISOR_AGENT_") }

// pinnedEndpoint is PinnedEndpoint with its errors naming the settings under prefix, so the
// embeddings endpoint, checked the same way, names SUPERVISOR_EMBEDDINGS_* (WIS-700).
func (a Agent) pinnedEndpoint(prefix string) (*url.URL, error) {
	u, err := endpoint(prefix, a.BaseURL, a.EndpointAllowlist)
	if err != nil {
		return nil, err
	}
	if len(a.EndpointAddrs) == 0 {
		return nil, errors.New(prefix + "ENDPOINT_ADDRS is required")
	}
	for _, p := range a.EndpointAddrs {
		if !p.Addr().IsValid() || !LocalAddr(p.Addr()) || p.Addr().Zone() != "" {
			return nil, errors.New(prefix + "ENDPOINT_ADDRS must list loopback or private addresses")
		}
	}
	if ip, e := netip.ParseAddr(u.Hostname()); e == nil {
		port := u.Port()
		if port == "" {
			port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
		}
		n, e := strconv.ParseUint(port, 10, 16)
		if e != nil || !a.Pinned(ip, uint16(n)) {
			return nil, errors.New(prefix + "BASE_URL address is not in " + prefix + "ENDPOINT_ADDRS")
		}
	}
	return u, nil
}

func (a Agent) validate(environment string) error {
	if err := a.Hosted.validate(a.Provider); err != nil {
		return err
	}
	if a.ChatAnswering && (a.Provider == "" || !a.ModeEnabled) {
		return errors.New("SUPERVISOR_CHAT_ANSWERING_ENABLED requires SUPERVISOR_AGENT_PROVIDER and SUPERVISOR_AGENT_MODE_ENABLED")
	}
	if a.FixtureScript != "" && a.Provider != AgentProviderFixture {
		return errors.New("SUPERVISOR_AGENT_FIXTURE_SCRIPT requires SUPERVISOR_AGENT_PROVIDER=fixture")
	}
	if a.ReasoningEffort != "" && (a.Provider != AgentProviderLocal || !slices.Contains([]string{"none", "low", "medium", "high"}, a.ReasoningEffort)) {
		return errors.New("SUPERVISOR_AGENT_REASONING_EFFORT must be none, low, medium or high, and requires SUPERVISOR_AGENT_PROVIDER=local")
	}
	switch a.Provider {
	case "":
		return nil
	case AgentProviderFixture:
		// Rule 09: the deterministic adapter never runs in production and never passes an actual-model gate.
		if environment == "production" {
			return errors.New("the deterministic-fixture agent provider is refused in production")
		}
	case AgentProviderLocal:
		if _, err := a.PinnedEndpoint(); err != nil {
			return err
		}
	default:
		return errors.New("invalid SUPERVISOR_AGENT_PROVIDER")
	}
	if !pin(a.Model) || !pin(a.ModelRevision) {
		return errors.New("SUPERVISOR_AGENT_MODEL and SUPERVISOR_AGENT_MODEL_REVISION are required")
	}
	// WIS-625: OCR runs only on a local vision model; a hosted route never receives an image.
	if a.VisionModel != "" || a.VisionModelRevision != "" {
		if a.Provider != AgentProviderLocal && a.Provider != AgentProviderFixture || !pin(a.VisionModel) || !pin(a.VisionModelRevision) {
			return errors.New("SUPERVISOR_AGENT_VISION_MODEL and SUPERVISOR_AGENT_VISION_MODEL_REVISION are set together, with SUPERVISOR_AGENT_PROVIDER=local (or the fixture outside production)")
		}
	}
	if a.Timeout < time.Second || a.Timeout > 5*time.Minute || a.MaxConcurrent < 1 || a.MaxConcurrent > 64 || a.MaxQueued < 0 || a.MaxQueued > 1024 {
		return errors.New("invalid agent limits")
	}
	return nil
}

// validate checks the hosted route: an allowlisted provider, a pinned model and revision and an
// API key file, and only alongside a default provider (agent runs need one).
func (h HostedAgent) validate(defaultProvider string) error {
	if h == (HostedAgent{}) {
		return nil
	}
	if _, ok := HostedEndpoints[h.Provider]; !ok {
		return errors.New("SUPERVISOR_AGENT_HOSTED_PROVIDER is not an allowlisted hosted endpoint")
	}
	if !pin(h.Model) || !pin(h.ModelRevision) || h.APIKeyFile == "" {
		return errors.New("SUPERVISOR_AGENT_HOSTED_MODEL, SUPERVISOR_AGENT_HOSTED_MODEL_REVISION and SUPERVISOR_AGENT_HOSTED_API_KEY_FILE are required")
	}
	if defaultProvider == "" {
		return errors.New("SUPERVISOR_AGENT_HOSTED_PROVIDER requires SUPERVISOR_AGENT_PROVIDER")
	}
	return nil
}

// pin accepts a bounded printable identifier with no whitespace.
func pin(s string) bool {
	if s == "" || len(s) > 256 {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

func (c Config) Validate() error {
	if c.Environment != "local" && c.Environment != "production" {
		return errors.New("invalid environment")
	}
	if c.ClientID == "" || c.ClientSecret == "" {
		return errors.New("OIDC client configuration required")
	}
	for _, value := range []string{c.Issuer, c.Origin} {
		u, e := url.Parse(value)
		if e != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("invalid identity URL")
		}
		if u.Scheme != "https" {
			if c.Environment != "local" || u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "keycloak") {
				return errors.New("identity URLs require HTTPS outside local development")
			}
		}
	}
	origin, _ := url.Parse(c.Origin)
	if origin.Path != "" && origin.Path != "/" {
		return errors.New("origin cannot contain path")
	}
	if !strings.HasPrefix(c.DatabaseURL, "postgresql://") && !strings.HasPrefix(c.DatabaseURL, "postgres://") {
		return errors.New("PostgreSQL DSN required")
	}
	// WIS-699: "local" drops database and Temporal TLS verification, Secure/__Host- cookies and
	// the fixture-provider refusal, so it is accepted only for hosts that are local: loopback or
	// private addresses, localhost, or a single-label service name (compose). A public host under
	// "local" was silently a production deployment without those protections.
	if c.Environment == "local" {
		for _, check := range []struct {
			setting string
			local   bool
		}{
			{"SUPERVISOR_PUBLIC_ORIGIN", localHost(origin.Hostname())},
			{"SUPERVISOR_OIDC_ISSUER", urlHostLocal(c.Issuer)},
			{"SUPERVISOR_DATABASE_URL", dsnHostsLocal(c.DatabaseURL)},
			{"SUPERVISOR_TEMPORAL_ADDRESS", addressHostLocal(c.TemporalAddress)},
		} {
			if !check.local {
				return errors.New(check.setting + " must use a loopback, private or service-name host when SUPERVISOR_ENVIRONMENT=local")
			}
		}
	}
	if c.Environment == "production" && !verifiesServerCertificate(c.DatabaseURL) {
		return errors.New("production PostgreSQL DSN must verify the server certificate (sslmode=verify-ca or verify-full)")
	}
	if c.Environment == "production" && !c.TemporalTLS {
		return errors.New("production Temporal requires TLS")
	}
	for _, id := range c.Tenants {
		if _, e := uuid.Parse(id); e != nil {
			return errors.New("invalid relay tenant UUID")
		}
	}
	if err := c.Embeddings.validate(); err != nil {
		return err
	}
	return c.Agent.validate(c.Environment)
}

// verifiesServerCertificate reports whether a PostgreSQL DSN requires TLS with server
// certificate verification (ADR 0002 Sec. 15.3: "require TLS certificate verification").
// sslmode=require encrypts but does not verify the server, so it does not qualify.
//
// It judges the configuration pgx will actually use (pgconn.ParseConfig), not the URL query: a
// repeated sslmode (pgx takes the last), the keyword form ("host=... sslmode=...") and PGSSLMODE
// all decide the real connection. The primary connection and every fallback must use TLS that
// verifies the server: verify-full leaves Go's verification on, and verify-ca installs pgx's own
// chain verifier (security audit run 1, WIS-699).
func verifiesServerCertificate(dsn string) bool {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return false
	}
	verifying := func(t *tls.Config) bool {
		return t != nil && (!t.InsecureSkipVerify || t.VerifyPeerCertificate != nil)
	}
	if !verifying(cfg.TLSConfig) {
		return false
	}
	for _, f := range cfg.Fallbacks {
		if !verifying(f.TLSConfig) {
			return false
		}
	}
	return true
}

// SecureCookies reports whether session cookies must be Secure (and __Host- prefixed): always
// in production, and whenever the public origin is HTTPS, whatever the label (WIS-699).
func (c Config) SecureCookies() bool {
	return c.Environment == "production" || strings.HasPrefix(strings.ToLower(c.Origin), "https://")
}

// ValidateAdminDSN applies the environment's database rule to a migration or provisioning URL.
// The migrate, provision and bootstrap-invite commands read those URLs without loading the full
// configuration, so the production TLS rule never reached them (WIS-699). setting names the
// variable in the error; the DSN itself is never echoed.
func ValidateAdminDSN(setting, dsn string) error {
	if !strings.HasPrefix(dsn, "postgresql://") && !strings.HasPrefix(dsn, "postgres://") {
		return &AdminDSNError{setting + " must be a postgresql:// URL"}
	}
	switch env("SUPERVISOR_ENVIRONMENT", "production") {
	case "production":
		if !verifiesServerCertificate(dsn) {
			return &AdminDSNError{setting + " must verify the server certificate in production (sslmode=verify-ca or verify-full)"}
		}
	case "local":
		if !dsnHostsLocal(dsn) {
			return &AdminDSNError{setting + " must use a loopback, private or service-name host when SUPERVISOR_ENVIRONMENT=local"}
		}
	default:
		return &AdminDSNError{"invalid environment"}
	}
	return nil
}

// AdminDSNError is a refused administrator DSN. Its text names the setting, never the DSN, so the
// commands may log it.
type AdminDSNError struct{ msg string }

func (e *AdminDSNError) Error() string { return e.msg }

// localHost reports whether a host is one local development may use: localhost, a loopback,
// private or shared address, or a single-label name such as a compose service ("postgres").
// A dotted name or a public address is not local.
func localHost(host string) bool {
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return LocalAddr(ip)
	}
	// Only the local stack's own service names (deploy/supervisor/compose.yml). Any other dotless
	// name is not local: a resolver search domain can turn it into a public host (WIS-699).
	return composeServices[strings.ToLower(host)]
}

var composeServices = map[string]bool{"postgres": true, "keycloak": true, "temporal": true, "migrate": true}

func urlHostLocal(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && localHost(u.Hostname())
}

// addressHostLocal: an unset address names no remote host (Load always sets a default).
func addressHostLocal(address string) bool {
	if address == "" {
		return true
	}
	host, _, err := net.SplitHostPort(address)
	return err == nil && localHost(host)
}

// dsnHostsLocal reports whether every host a PostgreSQL URL may connect to (its fallbacks too) is
// local; a Unix socket directory is local.
func dsnHostsLocal(dsn string) bool {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return false
	}
	local := func(h string) bool { return strings.HasPrefix(h, "/") || localHost(h) }
	if !local(cfg.Host) {
		return false
	}
	for _, f := range cfg.Fallbacks {
		if !local(f.Host) {
			return false
		}
	}
	return true
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
