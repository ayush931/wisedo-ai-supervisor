// Package ratelimit is an in-process token-bucket limiter for the unauthenticated routes (WIS-699).
// Each process keeps its own buckets: with several replicas a client gets each replica's budget.
// It is not a correctness prerequisite (rule 00): a lost bucket only grants a fresh budget.
package ratelimit

import (
	"encoding/json"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Limiter grants each key a bucket of burst tokens, refilled at rate tokens per second. The
// number of keys is capped, so a flood of distinct clients cannot grow it without bound.
type Limiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	maxKeys int
	now     func() time.Time
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New returns a limiter. now is injected so tests are deterministic.
func New(rate, burst float64, maxKeys int, now func() time.Time) *Limiter {
	return &Limiter{rate: rate, burst: burst, maxKeys: maxKeys, now: now, buckets: map[string]*bucket{}}
}

// Allow takes one token for key. When none is left it reports how long until one is.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		l.makeRoom(now)
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(l.burst, b.tokens+elapsed*l.rate)
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}

// makeRoom keeps the map under maxKeys: it drops every bucket that has refilled (an idle client
// starts full anyway), and if none has, the least recently used one.
func (l *Limiter) makeRoom(now time.Time) {
	if len(l.buckets) < l.maxKeys {
		return
	}
	full := time.Duration(l.burst / l.rate * float64(time.Second))
	var oldestKey string
	var oldest time.Time
	for k, b := range l.buckets {
		if now.Sub(b.last) >= full {
			delete(l.buckets, k)
			continue
		}
		if oldestKey == "" || b.last.Before(oldest) {
			oldestKey, oldest = k, b.last
		}
	}
	if len(l.buckets) >= l.maxKeys && oldestKey != "" {
		delete(l.buckets, oldestKey)
	}
}

// Keys reports how many buckets are held (for tests and diagnostics).
func (l *Limiter) Keys() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// ClientKey returns the function that keys a request by its client address. The connection's
// address is used unless it is a trusted proxy, in which case the rightmost X-Forwarded-For hop
// that is not a trusted proxy is used. Without trusted proxies the header is ignored, so a client
// cannot pick its own bucket. IPv6 clients are keyed by their /64, so one host rotating through
// its addresses shares one bucket.
func ClientKey(trusted []netip.Prefix) func(*http.Request) string {
	isTrusted := func(a netip.Addr) bool {
		for _, p := range trusted {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
	return func(r *http.Request) string {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		client, err := netip.ParseAddr(host)
		if err != nil {
			return "unknown"
		}
		client = client.Unmap()
		if isTrusted(client) {
			hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
			for i := len(hops) - 1; i >= 0; i-- {
				hop, err := forwardedAddr(hops[i])
				if err != nil {
					// An unreadable hop ends the chain. Keying the proxy would put every user in one
					// bucket, so the key is this connection instead.
					return "conn:" + r.RemoteAddr
				}
				hop = hop.Unmap()
				client = hop
				if !isTrusted(hop) {
					break
				}
			}
		}
		if client.Is6() {
			p, _ := client.Prefix(64)
			return p.String()
		}
		return client.String()
	}
}

// forwardedAddr reads one X-Forwarded-For hop: an address, or an address with a port as some load
// balancers write it ("198.51.100.1:5555", "[2001:db8::1]:443").
func forwardedAddr(hop string) (netip.Addr, error) {
	hop = strings.TrimSpace(hop)
	if a, err := netip.ParseAddr(hop); err == nil {
		return a, nil
	}
	ap, err := netip.ParseAddrPort(hop)
	if err != nil {
		return netip.Addr{}, err
	}
	return ap.Addr(), nil
}

// Wrap refuses a request over its key's budget before next runs, so nothing is written for it:
// 429 application/problem+json with the code RATE_LIMITED and a Retry-After in whole seconds.
func (l *Limiter) Wrap(key func(*http.Request) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, wait := l.Allow(key(r))
		if ok {
			next.ServeHTTP(w, r)
			return
		}
		correlation := w.Header().Get("X-Correlation-ID")
		if correlation == "" {
			correlation = uuid.NewString()
			w.Header().Set("X-Correlation-ID", correlation)
		}
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(wait.Seconds())))))
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "about:blank", "title": "RATE_LIMITED", "status": http.StatusTooManyRequests, "code": "RATE_LIMITED", "correlation_id": correlation})
	})
}
