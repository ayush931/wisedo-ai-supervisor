package contracts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/chat"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/oee"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
)

func TestGenerationIsDeterministic(t *testing.T) {
	a, b := Files(), Files()
	if len(a) == 0 || len(a) != len(b) {
		t.Fatalf("file sets differ: %d vs %d", len(a), len(b))
	}
	for name, data := range a {
		if !bytes.Equal(data, b[name]) {
			t.Fatalf("%s is not deterministic", name)
		}
	}
}

// The committed tree must equal the generator output: no drift, no missing and
// no stale files. Regenerate with `make supervisor-contracts`.
func TestCommittedContractsMatchGenerator(t *testing.T) {
	root := filepath.Join("..", "..", "contracts")
	want := Files()
	got := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		data, err := os.ReadFile(path)
		got[filepath.ToSlash(rel)] = data
		return err
	})
	if err != nil {
		t.Fatalf("contracts directory unreadable (run make supervisor-contracts): %v", err)
	}
	names := map[string]bool{}
	for n := range want {
		names[n] = true
	}
	for n := range got {
		names[n] = true
	}
	var drift []string
	for n := range names {
		if !bytes.Equal(want[n], got[n]) {
			drift = append(drift, n)
		}
	}
	sort.Strings(drift)
	if len(drift) > 0 {
		t.Fatalf("contract drift (run make supervisor-contracts and commit): %s", strings.Join(drift, ", "))
	}
}

func TestRuntimeChatRepliesMatchGeneratedSchema(t *testing.T) {
	created := time.Date(2026, 10, 28, 14, 0, 0, 0, time.UTC)
	snapshotID, stale, body := "4a4a4a4a-4a4a-4a4a-8a4a-4a4a4a4a4a4a", true,
		"- [Approved process] Bolts are torqued. [1]\n\nBased on the approved process snapshot of 2026-10-28T14:00:00Z; newer knowledge edits are not yet approved."
	replyTo := "2b2b2b2b-2b2b-4b2b-8b2b-2b2b2b2b2b2b"
	reason := chat.ReasonSnapshotEvidenceUnavailable
	fixtures := []chat.Message{
		{MessageID: "1a1a1a1a-1a1a-4a1a-8a1a-1a1a1a1a1a1a", Seq: 2, AuthorKind: chat.AuthorAssistant, ReplyTo: &replyTo,
			Body: &body, State: "answered", Citations: []chat.Citation{{Kind: "evidence_span", ID: "3c3c3c3c-3c3c-4c3c-8c3c-3c3c3c3c3c3c", Revision: 1}},
			SnapshotID: &snapshotID, SnapshotCreatedAt: &created, SnapshotStale: &stale, CreatedAt: created},
		{MessageID: "5d5d5d5d-5d5d-4d5d-8d5d-5d5d5d5d5d5d", Seq: 4, AuthorKind: chat.AuthorAssistant, ReplyTo: &replyTo,
			State: chat.StateUnavailable, ReasonCode: &reason, Citations: []chat.Citation{}, CreatedAt: created},
	}
	var schema map[string]any
	if err := json.Unmarshal(Files()["schemas/ChatMessageV1.schema.json"], &schema); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		raw, err := json.Marshal(fixture)
		if err != nil {
			t.Fatal(err)
		}
		var value any
		if err = json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		if err = validateSchemaValue(schema, value); err != nil {
			t.Errorf("runtime chat reply does not match generated schema: %v\n%s", err, raw)
		}
	}
}

func validateSchemaValue(schema map[string]any, value any) error {
	if choices, ok := schema["anyOf"].([]any); ok {
		for _, choice := range choices {
			if validateSchemaValue(choice.(map[string]any), value) == nil {
				return nil
			}
		}
		return fmt.Errorf("value %v matches no anyOf branch", value)
	}
	if enum, ok := schema["enum"].([]any); ok {
		found := false
		for _, item := range enum {
			found = found || item == value
		}
		if !found {
			return fmt.Errorf("value %v is outside enum %v", value, enum)
		}
	}
	switch schema["type"] {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("expected object, got %T", value)
		}
		properties := schema["properties"].(map[string]any)
		for _, required := range schema["required"].([]any) {
			if _, ok := object[required.(string)]; !ok {
				return fmt.Errorf("missing required property %q", required)
			}
		}
		for name, got := range object {
			property, ok := properties[name]
			if !ok {
				if schema["additionalProperties"] == false {
					return fmt.Errorf("unknown property %q", name)
				}
				continue
			}
			if err := validateSchemaValue(property.(map[string]any), got); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	case "array":
		array, ok := value.([]any)
		if !ok {
			return fmt.Errorf("expected array, got %T", value)
		}
		for i, item := range array {
			if err := validateSchemaValue(schema["items"].(map[string]any), item); err != nil {
				return fmt.Errorf("item %d: %w", i, err)
			}
		}
	case "string":
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("expected string, got %T", value)
		}
		switch schema["format"] {
		case "uuid":
			if _, err := uuid.Parse(text); err != nil {
				return fmt.Errorf("invalid UUID %q", text)
			}
		case "date-time":
			if _, err := time.Parse(time.RFC3339, text); err != nil {
				return fmt.Errorf("invalid date-time %q", text)
			}
		}
	case "integer":
		if number, ok := value.(float64); !ok || number != float64(int64(number)) {
			return fmt.Errorf("expected integer, got %v", value)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("expected boolean, got %T", value)
		}
	case "null":
		if value != nil {
			return fmt.Errorf("expected null, got %T", value)
		}
	}
	return nil
}

// Runtime JSON keys and the published schema must agree.
func TestSchemaMatchesRuntimeJSON(t *testing.T) {
	for _, c := range Catalog() {
		s := Schema(c.Type, enums())
		props := s["properties"].(map[string]any)
		required := s["required"].([]any)
		raw, err := json.Marshal(reflect.New(c.Type).Elem().Interface())
		if err != nil {
			t.Fatal(err)
		}
		var keys map[string]any
		if err = json.Unmarshal(raw, &keys); err != nil {
			t.Fatal(err)
		}
		for k := range keys {
			if _, ok := props[k]; !ok {
				t.Errorf("%s emits %q absent from schema", c.Name, k)
			}
		}
		for _, r := range required {
			if _, ok := keys[r.(string)]; !ok {
				t.Errorf("%s requires %q but runtime omits it", c.Name, r)
			}
		}
		if s["additionalProperties"] != false {
			t.Errorf("%s must reject unknown fields", c.Name)
		}
	}
}

func TestStationProfileListDocumentsOptionalFields(t *testing.T) {
	schema := Schema(reflect.TypeOf(workstation.StationProfilePageV1{}), enums())
	items := schema["properties"].(map[string]any)["workstations"].(map[string]any)["items"].(map[string]any)
	properties := items["properties"].(map[string]any)
	displayName := properties["display_name"].(map[string]any)
	lineID := properties["line_id"].(map[string]any)
	if displayName["description"] != "Current human-readable name for this configured workstation." {
		t.Fatalf("display_name description=%v", displayName["description"])
	}
	if lineID["description"] != "UUID of the production line configured for this workstation." || lineID["format"] != "uuid" {
		t.Fatalf("line_id contract=%v", lineID)
	}
}

func TestCallerAuthorityIsNotPublished(t *testing.T) {
	for _, c := range Catalog() {
		if c.Name == "RequestContext" {
			t.Fatal("RequestContext carries tenant authority and must not be a wire contract")
		}
	}
	for name, data := range Files() {
		if strings.Contains(name, "RequestContext") || bytes.Contains(data, []byte(`"RequestContext"`)) {
			t.Fatalf("%s publishes RequestContext", name)
		}
	}
}

func TestSchemaFailsClosedOnUndescribedContracts(t *testing.T) {
	cases := map[string]any{
		"unknown rule": struct {
			A string `json:"a" contract:"bogus"`
		}{},
		"empty description": struct {
			A string `json:"a" contract:"description="`
		}{},
		"unsupported kind": struct {
			A float64 `json:"a"`
		}{},
		"missing name": struct{ A string }{},
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			Schema(reflect.TypeOf(v), nil)
		})
	}
}

// internal/oee validates sealed packs with its own copy of the patterns (it cannot import
// this package); they must stay identical to the published schema.
func TestEvidencePatternsMatchOEE(t *testing.T) {
	if sha256Pattern != oee.EvidenceSHA256Pattern || decimal6Pattern != oee.EvidenceDecimal6Pattern {
		t.Fatal("internal/oee evidence patterns drifted from the published contract")
	}
}

// WIS-700: the published chat message schema accepts exactly the four retrieval labels the
// server records, in every place a message appears, and refuses any other.
func TestWIS700ChatRetrievalLabelIsAnEnum(t *testing.T) {
	created := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	replyTo := "2b2b2b2b-2b2b-4b2b-8b2b-2b2b2b2b2b2b"
	for _, file := range []string{"ChatMessageV1", "ChatMessagePageV1", "ChatPostResultV1"} {
		var schema map[string]any
		if err := json.Unmarshal(Files()["schemas/"+file+".schema.json"], &schema); err != nil {
			t.Fatal(err)
		}
		for _, label := range []string{chat.RetrievalWord, chat.RetrievalHybrid, chat.RetrievalHybridUnreranked, chat.RetrievalWordFallback, "semantic"} {
			msg := chat.Message{MessageID: "1a1a1a1a-1a1a-4a1a-8a1a-1a1a1a1a1a1a", Seq: 2, AuthorKind: chat.AuthorAssistant, ReplyTo: &replyTo,
				State: "pending", Citations: []chat.Citation{}, Retrieval: &label, CreatedAt: created}
			var doc any = msg
			switch file {
			case "ChatMessagePageV1":
				doc = chat.MessagePage{Messages: []chat.Message{msg}}
			case "ChatPostResultV1":
				doc = chat.PostResult{Conversation: chat.Conversation{ConversationID: replyTo, Title: "Oven", Revision: 2, CreatedAt: created, UpdatedAt: created}, Question: msg, Reply: msg}
			}
			raw, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			var value any
			if err = json.Unmarshal(raw, &value); err != nil {
				t.Fatal(err)
			}
			err = validateSchemaValue(schema, value)
			if (label == "semantic") != (err != nil) {
				t.Errorf("%s retrieval %q: %v", file, label, err)
			}
		}
	}
}
