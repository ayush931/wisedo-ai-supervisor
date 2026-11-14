package ingestion

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const csvHeader = "external_key,revision,kind,event_at,asset_external_key,order_external_key,payload_json\n"

const validUUID = "00000000-0000-4000-8000-000000000226"

func validTime() time.Time { return time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC) }

func TestParseCSVImportAcceptsValidRows(t *testing.T) {
	data := csvHeader +
		`machine-1,1,machine_state,2026-09-27T00:00:00Z,press-01,,"{""state"":""running""}"` + "\n" +
		`delta-1,1,production_delta,2026-09-27T00:15:00+05:30,press-01,order-7,"{""good_units"":""80""}"` + "\n"
	events, err := ParseImport("csv", []byte(data))
	if err != nil {
		t.Fatalf("valid CSV rejected: %v", err)
	}
	if len(events) != 2 || events[0].ExternalKey != "machine-1" || events[0].AssetExternalKey != "press-01" || events[1].OrderExternalKey != "order-7" {
		t.Fatalf("events=%+v", events)
	}
	if string(events[0].Payload) != `{"state":"running"}` {
		t.Fatalf("payload=%s", events[0].Payload)
	}
}

func TestParseImportRejectsMalformedFilesBeforePersistence(t *testing.T) {
	bad := map[string]struct {
		format string
		data   string
	}{
		"missing required header": {"csv", "external_key,revision,kind,event_at\nk,1,machine_state,2026-09-27T00:00:00Z\n"},
		"unknown column":          {"csv", "external_key,revision,kind,event_at,payload_json,extra\nk,1,machine_state,2026-09-27T00:00:00Z,{},x\n"},
		"duplicate header":        {"csv", "external_key,external_key,revision,kind,event_at,payload_json\n"},
		"numeric timestamp":       {"csv", csvHeader + "k,1,machine_state,1767225600,,,{}\n"},
		"impossible date":         {"csv", csvHeader + "k,1,machine_state,2026-02-31T00:00:00Z,,,{}\n"},
		"revision zero":           {"csv", csvHeader + "k,0,machine_state,2026-09-27T00:00:00Z,,,{}\n"},
		"revision with plus sign": {"csv", csvHeader + "k,+1,machine_state,2026-09-27T00:00:00Z,,,{}\n"},
		"invalid payload json":    {"csv", csvHeader + "k,1,machine_state,2026-09-27T00:00:00Z,,,{not json}\n"},
		"ragged row":              {"csv", csvHeader + "k,1,machine_state\n"},
		"invalid UTF-8":           {"csv", csvHeader + "k\xff,1,machine_state,2026-09-27T00:00:00Z,,,{}\n"},
		"empty file":              {"csv", ""},
		"unknown format":          {"xml", csvHeader},
		"json numeric timestamp":  {"json", `[{"external_key":"k","revision":1,"kind":"machine_state","event_at":1767225600,"payload":{}}]`},
		"json unknown field":      {"json", `[{"external_key":"k","revision":1,"kind":"machine_state","event_at":"2026-09-27T00:00:00Z","payload":{},"source_sequence":9}]`},
		"json not an array":       {"json", `{"external_key":"k"}`},
	}
	for name, c := range bad {
		t.Run(name, func(t *testing.T) {
			if events, err := ParseImport(c.format, []byte(c.data)); !errors.Is(err, ErrInvalid) || events != nil {
				t.Fatalf("malformed import: events=%d err=%v", len(events), err)
			}
		})
	}
}

func TestParseImportEnforcesRowAndByteLimits(t *testing.T) {
	var b strings.Builder
	b.WriteString(csvHeader)
	for i := 0; i <= MaxImportRows; i++ {
		b.WriteString("k,1,machine_state,2026-09-27T00:00:00Z,,,{}\n")
	}
	if _, err := ParseImport("csv", []byte(b.String())); !errors.Is(err, ErrImportTooLarge) {
		t.Fatalf("over-row file: %v", err)
	}
	huge := make([]byte, MaxImportBytes+1)
	if _, err := ParseImport("csv", huge); !errors.Is(err, ErrImportTooLarge) {
		t.Fatalf("over-byte file: %v", err)
	}
}

func TestImportChunksAssignContiguousSequencesAndValidate(t *testing.T) {
	events := make([]Event, 250)
	for i := range events {
		events[i] = Event{ExternalKey: "k" + strings.Repeat("x", i%3), Revision: 1, Kind: "machine_state", EventAt: validTime(), Payload: []byte(`{}`)}
	}
	req := ImportRequest{SourceID: validUUID, SourceRevision: 1, MappingRevision: 1}
	batches, err := importChunks(req, events, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 3 || len(batches[0].Events) != 100 || len(batches[2].Events) != 50 {
		t.Fatalf("chunks: %d", len(batches))
	}
	if batches[0].Events[0].SourceSequence != 10 || batches[2].Events[49].SourceSequence != 259 {
		t.Fatalf("sequences: first=%d last=%d", batches[0].Events[0].SourceSequence, batches[2].Events[49].SourceSequence)
	}
}

// The request hash names the file, not the registry state at the time: a retry after a source
// revision change replays the job instead of conflicting with itself.
func TestImportRequestHashIgnoresRevisions(t *testing.T) {
	base := ImportRequest{SourceID: "0a0a0a0a-0a0a-4a0a-8a0a-0a0a0a0a0a0a", SourceRevision: 1, MappingRevision: 1, Format: "csv"}
	later := base
	later.SourceRevision, later.MappingRevision = 7, 9
	if digestImport(base, "f", 3) != digestImport(later, "f", 3) {
		t.Fatal("revisions changed the request hash")
	}
	for name, other := range map[string]string{
		"file":   digestImport(base, "g", 3),
		"rows":   digestImport(base, "f", 4),
		"format": digestImport(ImportRequest{SourceID: base.SourceID, Format: "json"}, "f", 3),
		"source": digestImport(ImportRequest{SourceID: "1a1a1a1a-1a1a-4a1a-8a1a-1a1a1a1a1a1a", Format: "csv"}, "f", 3),
	} {
		if other == digestImport(base, "f", 3) {
			t.Errorf("%s did not change the request hash", name)
		}
	}
}
