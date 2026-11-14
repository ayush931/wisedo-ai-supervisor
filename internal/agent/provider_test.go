package agent

import (
	"errors"
	"testing"
	"time"
)

// Error classes decide whether the runtime may spend its one narrowed retry. Anything that
// is not a retryable *ProviderError (a scope or tenant denial from elsewhere) fails closed.
func TestProviderErrorClasses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"timeout", classify(t.Context(), errors.Join(errTimeout{}, errors.New("x"))), true},
		{"schema", &ProviderError{Code: CodeSchemaInvalid, retryable: true}, true},
		{"5xx", &ProviderError{Code: CodeProviderFailed, HTTPStatus: 503, retryable: true}, true},
		{"endpoint not local", classify(t.Context(), errEndpointNotLocal), false},
		{"redirect", classify(t.Context(), errRedirect), false},
		{"model not pinned", &ProviderError{Code: CodeModelNotPinned}, false},
		{"tool not offered", checkToolCalls(ModelRequest{Tools: []ToolDef{{Name: ToolEvidenceGet}}}, []ToolCall{{Name: "shell.exec", Arguments: []byte(`{}`)}}), false},
		{"breaker open", &ProviderError{Code: CodeProviderUnavailable}, false},
		{"scope denial", errors.New("SCOPE_DENIED"), false},
		{"nil", nil, false},
	} {
		if got := Retryable(tc.err); got != tc.retry {
			t.Errorf("%s: Retryable=%v want %v (%v)", tc.name, got, tc.retry, tc.err)
		}
	}
	if c := classify(t.Context(), errTimeout{}).Code; c != CodeDeadlineExceeded {
		t.Errorf("timeout code %s", c)
	}
	if c := checkToolCalls(ModelRequest{Tools: []ToolDef{{Name: ToolEvidenceGet}}}, []ToolCall{{Name: ToolEvidenceGet, Arguments: []byte(`{`)}}); !Retryable(c) {
		t.Errorf("malformed tool arguments must be a retryable schema failure: %v", c)
	}
}

type errTimeout struct{}

func (errTimeout) Error() string   { return "timeout" }
func (errTimeout) Timeout() bool   { return true }
func (errTimeout) Temporary() bool { return true }

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

// WIS-275: 3 failures within the last 5 calls open the breaker for 60 s; then exactly one
// half-open probe; a failed probe reopens it, a successful one closes it.
func TestBreakerOpensAndProbes(t *testing.T) {
	fail := &ProviderError{Code: CodeProviderFailed}
	clock := &fakeClock{t: time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)}
	record := func(b *Breaker, outcomes ...bool) {
		for _, failed := range outcomes {
			if _, err := b.Allow(); err != nil {
				t.Fatalf("closed breaker refused: %v", err)
			}
			if failed {
				b.Record(fail)
			} else {
				b.Record(nil)
			}
		}
	}

	// Negative control: 2 failures within the last 5 (the window slides) stays closed.
	b := NewBreaker(clock.now)
	record(b, true, true, false, false, false, true)
	if _, err := b.Allow(); err != nil {
		t.Fatalf("2 failures in last 5 opened the breaker: %v", err)
	}

	b = NewBreaker(clock.now)
	record(b, true, false, true, false, true)
	if _, err := b.Allow(); err == nil || err.(*ProviderError).Code != CodeProviderUnavailable {
		t.Fatalf("3 failures in 5: %v", err)
	}
	clock.t = clock.t.Add(59 * time.Second)
	if _, err := b.Allow(); err == nil {
		t.Fatal("admitted before 60 s")
	}
	clock.t = clock.t.Add(time.Second)
	if probe, err := b.Allow(); err != nil || !probe {
		t.Fatalf("at 60 s: probe=%v err=%v", probe, err)
	}
	if _, err := b.Allow(); err == nil {
		t.Fatal("second call admitted while the probe is in flight")
	}
	b.Record(fail)
	if _, err := b.Allow(); err == nil {
		t.Fatal("failed probe did not reopen")
	}
	clock.t = clock.t.Add(60 * time.Second)
	if probe, err := b.Allow(); err != nil || !probe {
		t.Fatalf("second probe: probe=%v err=%v", probe, err)
	}
	b.Record(nil)
	if probe, err := b.Allow(); err != nil || probe {
		t.Fatalf("successful probe did not close: probe=%v err=%v", probe, err)
	}
}
