package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
)

func TestWIS709LiveBatchesDeterministicAndComplete(t *testing.T) {
	wall := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	a, schedule, err := generateLive(278, "2026-09-01", wall, 60)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := generateLive(278, "2026-09-01", wall, 60)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.files["manifest.json"], b.files["manifest.json"]) {
		t.Fatal("same schedule changed manifest")
	}
	historical, err := generate(278, 1, "2026-09-01")
	if err != nil {
		t.Fatal(err)
	}
	if a.manifest.RevenueINR != historical.manifest.RevenueINR-3*historical.manifest.SKUs[0].PriceINR {
		t.Fatal("normal day revenue includes the excluded late correction")
	}
	var sequence int64
	for q, path := range datasetPaths(a, "batches/") {
		if !bytes.Equal(a.files[path], b.files[path]) {
			t.Fatal(path, "nondeterministic")
		}
		var batch ingestion.Batch
		if err = json.Unmarshal(a.files[path], &batch); err != nil {
			t.Fatal(err)
		}
		if err = batch.Validate(); err != nil {
			t.Fatal(err)
		}
		machines := map[string]bool{}
		deltas, heartbeats, watermarks := 0, 0, 0
		for _, event := range batch.Events {
			sequence++
			if event.SourceSequence != sequence || event.Revision != 1 {
				t.Fatal("sequence/correction", event)
			}
			if event.EventAt.After(schedule.IntervalEnd(q)) {
				t.Fatal("future event", event.ExternalKey)
			}
			switch event.Kind {
			case "machine_state":
				machines[event.AssetExternalKey] = true
			case "production_delta":
				deltas++
			case "heartbeat":
				heartbeats++
			case "watermark":
				watermarks++
			}
		}
		if len(machines) != 48 || deltas != 6 || heartbeats != 1 || watermarks != 1 {
			t.Fatalf("interval %d machines=%d deltas=%d heartbeat=%d watermark=%d", q, len(machines), deltas, heartbeats, watermarks)
		}
	}
	if sequence != a.manifest.Events || len(datasetPaths(a, "batches/")) != 96 {
		t.Fatal("missing intervals")
	}
	for _, path := range datasetPaths(a, "shifts/") {
		plan, err := workstation.DecodeShiftPlan(a.files[path])
		if err != nil {
			t.Fatal(path, err)
		}
		start, _ := time.Parse(time.RFC3339Nano, plan.StartsAt)
		end, _ := time.Parse(time.RFC3339Nano, plan.EndsAt)
		if end.Sub(start) != 8*time.Minute || len(plan.Assignments) != 8 {
			t.Fatal(path, "shift projection")
		}
	}
	changed, _, err := generateLive(278, "2026-09-01", wall.Add(time.Second), 60)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(changed.files["manifest.json"], a.files["manifest.json"]) {
		t.Fatal("changed schedule not pinned")
	}
}

func TestWIS709LiveProjectionKeepsExactNumbers(t *testing.T) {
	_, s, err := generateLive(278, "2026-09-01", time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC), 60)
	if err != nil {
		t.Fatal(err)
	}
	b, err := projectLive([]byte(`{"target_cycle_time_seconds":"50","target_good_units_per_second":"0.031112","count":9007199254740993}`), s)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Cycle string      `json:"target_cycle_time_seconds"`
		Rate  string      `json:"target_good_units_per_second"`
		Count json.Number `json:"count"`
	}
	if err = json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Cycle != "0.833333" || got.Rate != "1.86672" || got.Count.String() != "9007199254740993" {
		t.Fatalf("lossy projection %s", b)
	}
	for _, raw := range []string{"http://127.0.0.1:64376", "http://127.0.0.1:65376", "http://127.0.0.1:58376", "http://127.0.0.1:48176", "http://example.com:62376", "http://127.0.0.1:62376/private"} {
		if facilityURL(raw) == nil {
			t.Fatal("unsafe facility URL", raw)
		}
	}
	if err = facilityURL("http://127.0.0.1:62376"); err != nil {
		t.Fatal(err)
	}
}
