package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestWIS702TokenizerUsesInferenceTemplateAndLogsNoContent(t *testing.T) {
	const secret = "private source text"
	var template, inference map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apply-template":
			if err := json.NewDecoder(r.Body).Decode(&template); err != nil {
				t.Error(err)
			}
			fmt.Fprintf(w, `{"prompt":%q}`, "formatted "+secret)
		case "/tokenize":
			var body struct {
				Content      string `json:"content"`
				AddSpecial   bool   `json:"add_special"`
				ParseSpecial bool   `json:"parse_special"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Content != "formatted "+secret || !body.AddSpecial || !body.ParseSpecial {
				t.Errorf("bad tokenizer input: %v", err)
			}
			fmt.Fprint(w, `{"tokens":[12,34,56]}`)
		case "/v1/chat/completions":
			if err := json.NewDecoder(r.Body).Decode(&inference); err != nil {
				t.Error(err)
			}
			fmt.Fprint(w, chatBody(testModel, `{}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	var logs bytes.Buffer
	m := newTestModel(t, srv.URL, &logs, "")
	req := testRequest(secret)
	req.Messages = append([]Message{{Role: "system", Content: "fixed"}}, req.Messages...)
	req.ResponseSchema = json.RawMessage(`{"type":"object"}`)
	n, err := m.inputTokens(t.Context(), req)
	if err != nil || n != 3 {
		t.Fatalf("count=%d err=%v", n, err)
	}
	if _, err = m.Complete(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(template, inference) {
		t.Fatal("tokenizer and inference saw different payloads")
	}
	rt := Runtime{Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	rt.logContextSize(t.Context(), Run{ID: "run"}, req, 1, 1, 99, n)
	if strings.Contains(logs.String(), secret) || !strings.Contains(logs.String(), `"real_input_tokens":3`) {
		t.Fatal("diagnostics leaked source text or lost the measured count")
	}
}

func TestWIS702TokenizerCannotTreatInvalidResponseAsZero(t *testing.T) {
	for _, body := range []string{`{}`, `{"tokens":[]}`, `{"tokens":[-1]}`, `{"tokens":[null]}`, `{"tokens":["12"]}`, `not JSON`} {
		t.Run(body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/apply-template" {
					fmt.Fprint(w, `{"prompt":"text"}`)
				} else {
					fmt.Fprint(w, body)
				}
			}))
			defer srv.Close()
			m := newTestModel(t, srv.URL, nil, "")
			if _, err := m.inputTokens(t.Context(), testRequest("text")); err == nil {
				t.Fatal("invalid token response accepted")
			}
		})
	}
}

func TestWIS702ProbeClearsUnverifiedCapacity(t *testing.T) {
	response := `{"default_generation_settings":{"n_ctx":16384}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprintf(w, `{"data":[{"id":%q}]}`, localQwenModel)
			return
		}
		if r.URL.Path != "/props" {
			t.Errorf("unexpected probe %s", r.URL.Path)
		}
		fmt.Fprint(w, response)
	}))
	defer srv.Close()
	m := newTestModel(t, srv.URL, nil, "")
	m.model, m.revision = localQwenModel, localQwenRevision
	for _, bad := range []string{`{}`, `{"default_generation_settings":{"n_ctx":null}}`, `{"default_generation_settings":{"n_ctx":-1}}`, `{"default_generation_settings":{"n_ctx":16384.5}}`, `not JSON`} {
		response = `{"default_generation_settings":{"n_ctx":16384}}`
		if err := m.Probe(t.Context()); err != nil || m.ContextWindowTokens() != 16384 {
			t.Fatalf("valid capacity: %v", err)
		}
		response = bad
		if err := m.Probe(t.Context()); err != nil || m.ContextWindowTokens() != 0 {
			t.Fatalf("invalid props retained raised capacity: %v, %d", err, m.ContextWindowTokens())
		}
	}
}
