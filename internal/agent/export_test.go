package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
)

// Test-only seams for the external agent_test package.

// RetryableProviderError builds the retryable failure only the local model client can produce
// (timeouts, transport errors, 429/5xx), so a fixture script can replay it.
func RetryableProviderError(code string, httpStatus int) error {
	return &ProviderError{Code: code, HTTPStatus: httpStatus, retryable: true}
}

// SetAfterRead runs f between a tool's read and its post-read cancellation check.
func SetAfterRead(t *Tools, f func()) { t.afterRead = f }

// HoldWorkerPlace takes one of the runtime's per-process investigation places, as an in-flight
// investigation would, so a test can fill the worker without a blocking provider.
func HoldWorkerPlace(r *Runtime) (release func(), err error) { return r.admitWorker() }

// Test-only access (compiled only into this package's tests, never the binary).

// CallForTest runs one model call through the runtime's real per-call path (pinned provider,
// switch re-check, reservation, pseudonymisation, charge) with seq/attempt chosen by the test.
func (r *Runtime) CallForTest(ctx context.Context, sc RunScope, run Run, refs []RunRef, req ModelRequest, seq int) (ModelResponse, error, error) {
	return r.call(ctx, sc, run, refs, NewSpanID(), req, &seq, 1)
}

// NewHostedModelForTest builds the hosted adapter against a stand-in server; production uses
// NewHostedModel and its compiled endpoint and guarded transport only.
func NewHostedModelForTest(h config.HostedAgent, base *url.URL, rt http.RoundTripper, logger *slog.Logger) (*HostedModel, error) {
	return newHostedModel(h, base, rt, 10*time.Second, logger, time.Now)
}

// PreDispatchCode exposes the stable reason of a call refused before dispatch.
func PreDispatchCode(err error) string { return preDispatchCode(err) }

// UntrustedViolations returns the JSON paths of every string in doc that contains needle but is
// not the value of an "untrusted_source" key: where source text would read as instructions.
func UntrustedViolations(doc []byte, needle string) []string {
	var v any
	if json.Unmarshal(doc, &v) != nil {
		return []string{"<invalid json>"}
	}
	var out []string
	var walk func(v any, path, key string)
	walk = func(v any, path, key string) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				walk(c, path+"."+k, k)
			}
		case []any:
			for i, c := range x {
				walk(c, path+"["+strconv.Itoa(i)+"]", key)
			}
		case string:
			if strings.Contains(x, needle) && key != "untrusted_source" {
				out = append(out, path)
			}
		}
	}
	walk(v, "$", "")
	return out
}

// SetMaxPseudonymTermsForTest lowers the distinct-term bound (WIS-623) and returns the restore.
func SetMaxPseudonymTermsForTest(n int) func() {
	old := maxPseudonymTerms
	maxPseudonymTerms = n
	return func() { maxPseudonymTerms = old }
}
