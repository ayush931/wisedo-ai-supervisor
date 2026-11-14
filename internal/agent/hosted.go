package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	_ "embed"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
)

// Hosted inference (WIS-623, rule 08 "Model endpoints"). A site uses it only while its owner has
// switched it on (internal/modelhosting); a run is pinned to it when it is created and re-checked
// before every call; it never falls back to another provider.
const (
	// ProviderHostedPrefix starts the provider_name of a run pinned to a hosted endpoint.
	ProviderHostedPrefix = "hosted-"

	CodeEndpointPrivate      = "ENDPOINT_PRIVATE"         // a hosted endpoint resolved to a private address
	CodeHostedDisabled       = "HOSTED_MODEL_DISABLED"    // the site's switch is off (or was turned off mid-run)
	CodeHostedUnavailable    = "HOSTED_MODEL_UNAVAILABLE" // the run is pinned to a hosted route this process does not have
	CodePseudonymsMissing    = "PSEUDONYMS_MISSING"       // a hosted call without its pseudonym map: never sent
	CodePseudonymUnknown     = "PSEUDONYM_UNKNOWN"        // the output names a token this request never issued
	CodePseudonymsIncomplete = "PSEUDONYMS_INCOMPLETE"    // a decoded view of the outbound text still names someone: never sent
	CodeImageNotHosted       = "IMAGE_NOT_HOSTED"         // a page image cannot be pseudonymised: never sent to a hosted model (WIS-625)
	maxHostedResponseBytes   = maxModelResponseBytes
	hostedMinTLS             = tls.VersionTLS12
	hostedDialTimeout        = 5 * time.Second
	hostedTLSHandshakeLimit  = 10 * time.Second
)

var errEndpointPrivate = errors.New("hosted endpoint resolved to a private address")

//go:embed hosted_prices.v1.json
var hostedPricesJSON []byte

type hostedPriceTable struct {
	SchemaVersion int    `json:"schema_version"`
	Version       string `json:"version"`
	Source        string `json:"source"`
	Prices        []struct {
		Provider                 string `json:"provider"`
		Model                    string `json:"model"`
		ModelRevision            string `json:"model_revision"`
		InputMicroUSDPerMillion  int64  `json:"input_micro_usd_per_million"`
		OutputMicroUSDPerMillion int64  `json:"output_micro_usd_per_million"`
	} `json:"prices"`
}

// HostedPrice returns the committed, versioned price of a hosted model revision. An unknown
// provider, model or revision has no price, and a call without a price is never made (rule 08).
func HostedPrice(provider, model, revision string) (TokenPrice, string, bool) {
	var t hostedPriceTable
	if strictjson.Decode(hostedPricesJSON, &t, len(hostedPricesJSON)) != nil || t.SchemaVersion != 1 || t.Version == "" {
		return TokenPrice{}, "", false
	}
	for _, p := range t.Prices {
		if p.Provider == provider && p.Model == model && p.ModelRevision == revision && p.InputMicroUSDPerMillion >= 0 && p.OutputMicroUSDPerMillion >= 0 {
			return TokenPrice{InputMicroUSDPerMillion: p.InputMicroUSDPerMillion, OutputMicroUSDPerMillion: p.OutputMicroUSDPerMillion}, t.Version, true
		}
	}
	return TokenPrice{}, "", false
}

// HostedRoute is this process's hosted inference route: the adapter and what a run pinned to it
// records. Price is nil when the table has none, and then every call is refused before dispatch.
type HostedRoute struct {
	Provider      Provider
	Name          string // provider_name of a pinned run: ProviderHostedPrefix + endpoint key
	Region        string
	Model         string
	ModelRevision string
	Price         *TokenPrice
	PriceVersion  string
}

var hostedRoute atomic.Pointer[HostedRoute]

// SetHostedRoute installs (or, with nil, removes) the process's hosted route. The API and the
// worker install the same route at start, from the same configuration.
func SetHostedRoute(r *HostedRoute) { hostedRoute.Store(r) }

// CurrentHostedRoute is the installed hosted route, or nil.
func CurrentHostedRoute() *HostedRoute { return hostedRoute.Load() }

// IsHostedProvider reports a provider_name pinned to a hosted endpoint.
func IsHostedProvider(name string) bool { return strings.HasPrefix(name, ProviderHostedPrefix) }

// NewHostedRoute builds the hosted route from validated configuration. A model without a price
// is refused here, so a misconfigured deployment fails at start rather than per call.
func NewHostedRoute(h config.HostedAgent, timeout time.Duration, logger *slog.Logger, now func() time.Time) (*HostedRoute, error) {
	ep, ok := config.HostedEndpoints[h.Provider]
	if !ok {
		return nil, errors.New("hosted provider is not allowlisted")
	}
	price, version, ok := HostedPrice(h.Provider, h.Model, h.ModelRevision)
	if !ok {
		return nil, errors.New("hosted model has no price in the committed price table")
	}
	m, err := NewHostedModel(h, timeout, logger, now)
	if err != nil {
		return nil, err
	}
	return &HostedRoute{Provider: m, Name: ProviderHostedPrefix + h.Provider, Region: ep.Region, Model: h.Model, ModelRevision: h.ModelRevision, Price: &price, PriceVersion: version}, nil
}

// HostedModel posts to one allowlisted hosted OpenAI-compatible endpoint over HTTPS. Unlike the
// local adapter it refuses private, loopback and link-local addresses (DNS cannot turn it back
// into the network), and every request is pseudonymised on the way out and restored on the way
// back from that request's own token map.
type HostedModel struct {
	base     *url.URL
	name     string
	model    string
	revision string
	apiKey   string
	client   *http.Client
	breaker  *Breaker
	log      *slog.Logger
	now      func() time.Time
}

// NewHostedModel builds the adapter for a compiled-in endpoint with the guarded transport.
func NewHostedModel(h config.HostedAgent, timeout time.Duration, logger *slog.Logger, now func() time.Time) (*HostedModel, error) {
	ep, ok := config.HostedEndpoints[h.Provider]
	if !ok {
		return nil, errors.New("hosted provider is not allowlisted")
	}
	base, err := url.Parse(ep.BaseURL)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return nil, errors.New("invalid compiled hosted endpoint")
	}
	resolver := func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
	transport := &http.Transport{
		Proxy:                 nil, // HTTPS_PROXY must not reroute plant data
		DialContext:           publicDialer(resolver),
		TLSClientConfig:       &tls.Config{MinVersion: hostedMinTLS},
		TLSHandshakeTimeout:   hostedTLSHandshakeLimit,
		ResponseHeaderTimeout: timeout,
		MaxIdleConns:          4,
		IdleConnTimeout:       90 * time.Second,
	}
	return newHostedModel(h, base, transport, timeout, logger, now)
}

// newHostedModel takes the base URL and transport explicitly; tests pass a stand-in TLS server.
// Production goes through NewHostedModel only.
func newHostedModel(h config.HostedAgent, base *url.URL, transport http.RoundTripper, timeout time.Duration, logger *slog.Logger, now func() time.Time) (*HostedModel, error) {
	if h.Model == "" || h.ModelRevision == "" || h.APIKeyFile == "" || timeout <= 0 || logger == nil || now == nil {
		return nil, errors.New("invalid hosted model configuration")
	}
	key, err := os.ReadFile(h.APIKeyFile)
	apiKey := strings.TrimSpace(string(key))
	if err != nil || apiKey == "" {
		return nil, errors.New("invalid SUPERVISOR_AGENT_HOSTED_API_KEY_FILE")
	}
	return &HostedModel{base: base, name: ProviderHostedPrefix + h.Provider, model: h.Model, revision: h.ModelRevision, apiKey: apiKey,
		client:  &http.Client{Timeout: timeout, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirect }},
		breaker: NewBreaker(now), log: logger, now: now}, nil
}

// publicDialer resolves the host itself and refuses unless every address is public, then dials
// the vetted IP literal so a second lookup cannot rebind.
func publicDialer(lookup func(context.Context, string) ([]netip.Addr, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		var addrs []netip.Addr
		if ip, e := netip.ParseAddr(host); e == nil {
			addrs = []netip.Addr{ip}
		} else if addrs, err = lookup(ctx, host); err != nil {
			return nil, err
		}
		if len(addrs) == 0 {
			return nil, errEndpointPrivate
		}
		for _, a := range addrs {
			if !publicAddr(a) {
				return nil, errEndpointPrivate
			}
		}
		d := net.Dialer{Timeout: hostedDialTimeout}
		return d.DialContext(ctx, network, net.JoinHostPort(addrs[0].Unmap().String(), port))
	}
}

// publicAddr is the inverse of the local endpoint rule: not loopback, private, shared, link-local
// (cloud metadata), multicast or unspecified.
func publicAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || config.LocalAddr(a) || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsMulticast() || a.IsUnspecified() || a.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range specialPurpose {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// specialPurpose are IANA special-purpose blocks that are never a hosted provider's public
// address: some embed an IPv4 address that may be private (NAT64, 6to4, Teredo), the others are
// reserved, documentation or benchmarking ranges (security audit run 1, WIS-699).
var specialPurpose = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, broadcast included
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64
	netip.MustParsePrefix("64:ff9b:1::/48"),  // local NAT64
	netip.MustParsePrefix("100::/64"),        // discard
	netip.MustParsePrefix("2001::/23"),       // IETF protocol assignments, Teredo included
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4
}

func (m *HostedModel) Name() string { return m.name }
func (m *HostedModel) Actual() bool { return true }

// Complete performs one call. The request is pseudonymised with the map the runtime put on ctx
// (none: the call is refused, never sent raw); the response is restored from that same map, and
// a token it never issued refuses the whole output. It never retries.
// AnswerPhase: openAIChat sends the schema as text while tools are offered (see callRole).
func (m *HostedModel) AnswerPhase() bool { return true }

func (m *HostedModel) Complete(ctx context.Context, r ModelRequest) (ModelResponse, error) {
	start := m.now()
	if err := r.Validate(); err != nil {
		return ModelResponse{}, m.logged(ctx, r, &ProviderError{Code: CodeSchemaInvalid}, ModelResponse{}, start)
	}
	if r.Model != m.model {
		return ModelResponse{}, m.logged(ctx, r, &ProviderError{Code: CodeModelNotPinned}, ModelResponse{}, start)
	}
	// Names in an image cannot be found, let alone replaced: an image never leaves for a hosted
	// endpoint, whatever the site's switch says.
	if r.HasImages() {
		return ModelResponse{}, m.logged(ctx, r, &ProviderError{Code: CodeImageNotHosted}, ModelResponse{}, start)
	}
	p := pseudonymsFrom(ctx)
	if p == nil {
		return ModelResponse{}, m.logged(ctx, r, &ProviderError{Code: CodePseudonymsMissing}, ModelResponse{}, start)
	}
	// A request that cannot be fully pseudonymised is refused before the breaker: nothing was
	// sent, so it says nothing about the endpoint's health.
	hidden, err := p.HideRequestContext(ctx, r)
	if err != nil {
		return ModelResponse{}, m.logged(ctx, r, err, ModelResponse{}, start)
	}
	if _, err := m.breaker.Allow(); err != nil {
		return ModelResponse{}, m.logged(ctx, r, err, ModelResponse{}, start)
	}
	resp, err := openAIChat(ctx, m.do, m.model, m.revision, m.name, hidden, "")
	if err == nil {
		var restored ModelResponse
		if restored, err = p.RestoreResponse(resp); err == nil {
			restored.hiddenContent, restored.hiddenToolCalls = resp.Content, resp.ToolCalls
			resp = restored
		} else {
			// The endpoint answered; what the model wrote was refused (an unissued pseudonym, a
			// name not found again, an oversized restoration). Not an endpoint failure (WIS-696).
			err = contentFailure(err)
		}
	}
	m.breaker.Record(err)
	resp.WallMillis = m.now().Sub(start).Milliseconds()
	if err != nil {
		// A refused output was still billed: its usage goes back with the error (never its text).
		return billed(resp), m.logged(ctx, r, err, resp, start)
	}
	m.logged(ctx, r, nil, resp, start)
	return resp, nil
}

// logged records hashes, codes, tokens and latency only: never prompt, completion or key (rule 08).
func (m *HostedModel) logged(ctx context.Context, r ModelRequest, err error, resp ModelResponse, start time.Time) error {
	code, status, level := "OK", 0, slog.LevelInfo
	var p *ProviderError
	if errors.As(err, &p) {
		code, status, level = p.Code, p.HTTPStatus, slog.LevelWarn
	}
	m.log.LogAttrs(ctx, level, "agent model call",
		slog.String("provider", m.name), slog.String("model", m.model), slog.String("model_revision", m.revision),
		slog.String("request_sha256", r.Digest()), slog.String("prompt_version", r.PromptVersion),
		slog.String("code", code), slog.Int("http_status", status),
		slog.Int64("input_tokens", resp.InputTokens), slog.Int64("output_tokens", resp.OutputTokens),
		slog.Int64("wall_ms", m.now().Sub(start).Milliseconds()))
	return err
}

// do sends one request over HTTPS and returns the bounded body of a 200 response.
func (m *HostedModel) do(ctx context.Context, method, path string, payload []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, m.base.JoinPath(path).String(), bytes.NewReader(payload))
	if err != nil {
		return nil, &ProviderError{Code: CodeProviderFailed}
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	res, err := m.client.Do(req)
	if err != nil {
		if errors.Is(err, errEndpointPrivate) {
			return nil, &ProviderError{Code: CodeEndpointPrivate}
		}
		return nil, classify(ctx, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxHostedResponseBytes))
		retry := res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500
		return nil, &ProviderError{Code: CodeProviderFailed, HTTPStatus: res.StatusCode, retryable: retry}
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxHostedResponseBytes+1))
	if err != nil {
		return nil, classify(ctx, err)
	}
	if len(body) > maxHostedResponseBytes {
		return nil, &ProviderError{Code: CodeProviderResponseLarge, HTTPStatus: res.StatusCode}
	}
	return body, nil
}
