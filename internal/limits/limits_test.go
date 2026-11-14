package limits

import (
	"testing"
	"time"
)

// The WIS-290/271/272 limits, stated once. A change here is a deliberate contract change.
func TestLimitsMatchTheSpecification(t *testing.T) {
	for name, c := range map[string]struct{ got, want int }{
		"request body":   {RequestBodyBytes, 1 << 20},
		"event":          {EventBytes, 256 << 10},
		"import bytes":   {ImportBytes, 10 << 20},
		"import rows":    {ImportRows, 50000},
		"document":       {DocumentBytes, 20 << 20},
		"extracted text": {ExtractedTextBytes, 2 << 20},
		"decompressed":   {DecompressedBytes, 100 << 20},
	} {
		if c.got != c.want {
			t.Errorf("%s limit %d, want %d", name, c.got, c.want)
		}
	}
	if EventBytes > RequestBodyBytes || RequestBodyBytes > ImportBytes {
		t.Error("an event must fit a request body and a request body an import")
	}
}

// WIS-272: liveness, readiness and dispatch share this cap; a non-positive freshness must not
// disable it.
func TestHeartbeatLimit(t *testing.T) {
	for _, c := range []struct {
		freshness int64
		want      time.Duration
	}{
		{-1, 120 * time.Second},
		{0, 120 * time.Second},
		{60, 60 * time.Second},
		{120, 120 * time.Second},
		{900, 120 * time.Second},
	} {
		if got := HeartbeatLimit(c.freshness); got != c.want {
			t.Errorf("HeartbeatLimit(%d) = %s, want %s", c.freshness, got, c.want)
		}
	}
}
