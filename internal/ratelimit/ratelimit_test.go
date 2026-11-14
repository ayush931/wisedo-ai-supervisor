package ratelimit

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// WIS-699: a key gets its burst, then is refused until the bucket refills.
func TestLimiterBurstRefusalAndRefill(t *testing.T) {
	c := &clock{time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)}
	l := New(1, 3, 100, c.now)
	for i := range 3 {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("request %d of the burst refused", i+1)
		}
	}
	ok, wait := l.Allow("a")
	if ok || wait != time.Second {
		t.Fatalf("over the burst: ok=%v wait=%s, want refused for 1s", ok, wait)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("another key shared the first key's bucket")
	}
	c.t = c.t.Add(time.Second)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("a refilled token was not granted")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("more than the refilled token was granted")
	}
}

// WIS-699: a refused request never reaches the handler and gets a 429 problem with Retry-After.
func TestWrapRefusesBeforeTheHandler(t *testing.T) {
	c := &clock{time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)}
	l := New(0.5, 1, 100, c.now)
	calls := 0
	h := l.Wrap(func(*http.Request) string { return "k" }, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(204) }))
	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest("GET", "/auth/login", nil))
	second := httptest.NewRecorder()
	h.ServeHTTP(second, httptest.NewRequest("GET", "/auth/login", nil))
	if first.Code != 204 || calls != 1 {
		t.Fatalf("first: %d calls %d", first.Code, calls)
	}
	var body struct {
		CorrelationID string `json:"correlation_id"`
	}
	var problem map[string]any
	_ = json.Unmarshal(second.Body.Bytes(), &problem)
	_ = json.Unmarshal(second.Body.Bytes(), &body)
	if second.Code != 429 || calls != 1 || problem["code"] != "RATE_LIMITED" || second.Header().Get("Retry-After") != "2" ||
		second.Header().Get("Content-Type") != "application/problem+json" || body.CorrelationID == "" || body.CorrelationID != second.Header().Get("X-Correlation-ID") {
		t.Fatalf("refusal: %d calls %d headers %v body %s", second.Code, calls, second.Header(), second.Body.String())
	}
}

// WIS-699: the key map is capped; idle (refilled) buckets go first, then the least recently used.
func TestLimiterKeyMapIsBounded(t *testing.T) {
	c := &clock{time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)}
	l := New(1, 10, 50, c.now)
	for i := range 5000 {
		l.Allow(fmt.Sprintf("client-%d", i))
		c.t = c.t.Add(time.Millisecond)
	}
	if n := l.Keys(); n > 50 {
		t.Fatalf("%d keys held, cap 50", n)
	}
	// A client that is still being limited keeps its bucket while newer keys arrive.
	for range 10 {
		l.Allow("hot")
	}
	l.Allow("fresh")
	if ok, _ := l.Allow("hot"); ok {
		t.Fatal("an exhausted bucket was evicted and refilled by new keys")
	}
}

// WIS-699: the client key is the connection address. X-Forwarded-For is believed only from a
// trusted proxy, and then the rightmost hop that is not a trusted proxy is the client.
func TestClientKey(t *testing.T) {
	proxy := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	for _, c := range []struct {
		name, remote, xff string
		trusted           []netip.Prefix
		want              string
	}{
		{"direct", "203.0.113.7:4000", "", nil, "203.0.113.7"},
		{"spoofed header without a trusted proxy", "203.0.113.7:4000", "198.51.100.1", nil, "203.0.113.7"},
		{"spoofed header from an untrusted peer", "203.0.113.7:4000", "198.51.100.1", proxy, "203.0.113.7"},
		{"through a trusted proxy", "10.0.0.5:4000", "198.51.100.1", proxy, "198.51.100.1"},
		{"a spoofed left hop is not the client", "10.0.0.5:4000", "192.0.2.99, 198.51.100.1", proxy, "198.51.100.1"},
		{"two trusted proxies", "10.0.0.5:4000", "198.51.100.1, 10.0.0.9", proxy, "198.51.100.1"},
		// Review of #288: a hop some load balancers write with its port is still its address, and an
		// unreadable hop keys this connection, never the proxy, which every user would share.
		{"hop with a port", "10.0.0.5:4000", "198.51.100.1:5555", proxy, "198.51.100.1"},
		{"IPv6 hop with a port", "10.0.0.5:4000", "[2001:db8:9:9::1]:443", proxy, "2001:db8:9:9::/64"},
		{"malformed hop keys the connection, not the shared proxy", "10.0.0.5:4000", "not-an-ip", proxy, "conn:10.0.0.5:4000"},
		{"IPv6 grouped by /64", "[2001:db8:1:2:aaaa::1]:4000", "", nil, "2001:db8:1:2::/64"},
		{"IPv4-mapped IPv6", "[::ffff:203.0.113.7]:4000", "", nil, "203.0.113.7"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/auth/login", nil)
			r.RemoteAddr = c.remote
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			if got := ClientKey(c.trusted)(r); got != c.want {
				t.Fatalf("key %q, want %q", got, c.want)
			}
		})
	}
}
