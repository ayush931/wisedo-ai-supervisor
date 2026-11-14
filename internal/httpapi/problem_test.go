package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// One server-generated correlation ID per request: in the header and in the problem body, and
// never taken from the client.
func TestProblemCarriesTheRequestCorrelationID(t *testing.T) {
	h := Boundary(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { problem(w, 503, "STORAGE_UNAVAILABLE") }), false)
	r := httptest.NewRequest("GET", "/v1/x", nil)
	r.Header.Set(correlationHeader, "client-chosen")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var body struct {
		Type          string `json:"type"`
		Status        int    `json:"status"`
		Code          string `json:"code"`
		CorrelationID string `json:"correlation_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	header := w.Header().Get(correlationHeader)
	if _, err := uuid.Parse(header); err != nil || header != body.CorrelationID {
		t.Fatalf("header %q body %q", header, body.CorrelationID)
	}
	if w.Header().Get("Content-Type") != "application/problem+json" || body.Status != 503 || body.Code != "STORAGE_UNAVAILABLE" || body.Type != "about:blank" {
		t.Fatalf("problem document: %s %s", w.Header().Get("Content-Type"), w.Body.String())
	}
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, httptest.NewRequest("GET", "/v1/x", nil))
	if w2.Header().Get(correlationHeader) == header {
		t.Fatal("two requests shared a correlation ID")
	}
}

// WIS-290: a client that aborts its request (curl -m 0.005 /v1/me) was logged as
// "WARN request failed status=503". A cancelled request context is not a server failure; a real
// dependency failure on a live request still is.
func TestProblemDoesNotReportAClientCancelAsAServerFailure(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	h := Boundary(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { problem(w, 503, "WIS290_UNAVAILABLE") }), false)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/me", nil).WithContext(cancelled))
	if got := logs.String(); strings.Contains(got, "level=WARN") || !strings.Contains(got, "level=INFO msg=\"request cancelled by client\" status=503 code=WIS290_UNAVAILABLE") {
		t.Fatalf("client cancel not logged at INFO only: %q", got)
	}

	// A passed deadline is a server-side timeout, not a client that left: still a failure, so a
	// widening to ctx.Err() != nil fails here.
	for name, ctx := range map[string]context.Context{"live": context.Background(), "deadline passed": expired(t)} {
		logs.Reset()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/me", nil).WithContext(ctx))
		if !strings.Contains(logs.String(), "level=WARN msg=\"request failed\" status=503 code=WIS290_UNAVAILABLE") {
			t.Fatalf("%s: dependency failure not logged at WARN: %q", name, logs.String())
		}
	}
}

func expired(t *testing.T) context.Context {
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	t.Cleanup(cancel)
	return ctx
}
