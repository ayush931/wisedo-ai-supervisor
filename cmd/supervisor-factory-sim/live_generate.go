package main

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/factorysim"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/production"
)

func datasetPaths(d dataset, prefix string) []string {
	var names []string
	for _, f := range d.manifest.Files {
		if strings.HasPrefix(f.Path, prefix) && strings.HasSuffix(f.Path, ".json") {
			names = append(names, f.Path)
		}
	}
	return names
}

// projectLive changes only dated contracts and planning rates. Machine ratings remain model-time values.
func projectLive(b []byte, schedule factorysim.Schedule) ([]byte, error) {
	var v any
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	decoder.UseNumber()
	if err := decoder.Decode(&v); err != nil {
		return nil, err
	}
	var walk func(any, string) (any, error)
	walk = func(v any, key string) (any, error) {
		switch x := v.(type) {
		case string:
			if at, err := time.Parse(time.RFC3339Nano, x); err == nil {
				return schedule.Wall(at).In(at.Location()).Format(time.RFC3339Nano), nil
			}
			if key == "target_cycle_time_seconds" || key == "target_good_units_per_second" {
				n, ok := new(big.Rat).SetString(x)
				if !ok {
					return nil, fmt.Errorf("invalid %s", key)
				}
				scale := big.NewRat(schedule.Speed, 1)
				if key == "target_cycle_time_seconds" {
					n.Quo(n, scale)
				} else {
					n.Mul(n, scale)
				}
				decimal, err := production.Decimal6HalfEven(n)
				return strings.TrimRight(strings.TrimRight(decimal, "0"), "."), err
			}
		case []any:
			for i := range x {
				next, err := walk(x[i], key)
				if err != nil {
					return nil, err
				}
				x[i] = next
			}
		case map[string]any:
			for k, value := range x {
				next, err := walk(value, k)
				if err != nil {
					return nil, err
				}
				x[k] = next
			}
		}
		return v, nil
	}
	v, err := walk(v, "")
	if err != nil {
		return nil, err
	}
	return wire(v), nil
}

func generateLive(seed int64, start string, wall time.Time, speed int64) (dataset, factorysim.Schedule, error) {
	zone, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		return dataset{}, factorysim.Schedule{}, err
	}
	model, err := time.ParseInLocation("2006-01-02", start, zone)
	if err != nil {
		return dataset{}, factorysim.Schedule{}, err
	}
	schedule := factorysim.Schedule{ModelStart: model.Add(6 * time.Hour), WallStart: wall, Speed: speed}
	if err = schedule.Validate(); err != nil {
		return dataset{}, schedule, err
	}
	d, err := generate(seed, 1, start)
	if err != nil {
		return d, schedule, err
	}
	// The normal live day omits the historical first-interval correction of three BD-101 parts.
	d.manifest.RevenueINR -= 3 * d.manifest.SKUs[0].PriceINR
	if err = addMachineModel(&d, schedule.ModelStart); err != nil {
		return d, schedule, err
	}
	var intervals [96][]ingestion.Event
	for _, path := range datasetPaths(d, "batches/") {
		var b ingestion.Batch
		if err = json.Unmarshal(d.files[path], &b); err != nil {
			return d, schedule, err
		}
		for _, e := range b.Events {
			// The append-only late correction belongs to its explicit edge switch, not the normal run.
			if e.Revision != 1 {
				continue
			}
			var day, line, q int
			if n, _ := fmt.Sscanf(e.ExternalKey, "d%d-l%d-q%d", &day, &line, &q); n != 3 {
				q = max(0, min(95, int(e.EventAt.Sub(schedule.ModelStart)/(15*time.Minute))))
			}
			if q < 0 || q >= len(intervals) {
				return d, schedule, fmt.Errorf("interval outside day: %s", e.ExternalKey)
			}
			e.EventAt = schedule.Wall(e.EventAt)
			e.Payload, err = projectLive(e.Payload, schedule)
			if err != nil {
				return d, schedule, err
			}
			intervals[q] = append(intervals[q], e)
		}
	}
	files := d.manifest.Files
	d.manifest.Files = nil
	for _, f := range files {
		if strings.HasPrefix(f.Path, "batches/") || strings.HasPrefix(f.Path, "oee/") || strings.HasPrefix(f.Path, "contexts/") || f.Path == "oee-policy.json" {
			delete(d.files, f.Path)
			continue
		}
		b := d.files[f.Path]
		if strings.HasPrefix(f.Path, "shifts/") || strings.HasPrefix(f.Path, "jobs/") || strings.HasPrefix(f.Path, "plans/") {
			b, err = projectLive(b, schedule)
			if err != nil {
				return d, schedule, err
			}
		}
		d.raw(f.Path, f.Schema, b)
	}
	var sequence int64
	for q, events := range intervals {
		at := schedule.IntervalEnd(q)
		for i := range events {
			sequence++
			events[i].SourceSequence = sequence
		}
		events = append(events,
			ingestion.Event{ExternalKey: fmt.Sprintf("live-heartbeat-%02d", q), Revision: 1, Kind: "heartbeat", SourceSequence: sequence + 1, EventAt: at, Payload: wire(map[string]string{"observed_at": at.Format(time.RFC3339Nano)})},
			ingestion.Event{ExternalKey: fmt.Sprintf("live-watermark-%02d", q), Revision: 1, Kind: "watermark", SourceSequence: sequence + 2, EventAt: at, Payload: wire(map[string]any{"complete_through": at.Format(time.RFC3339Nano), "through_sequence": sequence + 1})})
		sequence += 2
		batch := ingestion.Batch{SchemaVersion: 1, SourceID: d.id("telemetry"), SourceRevision: 2, MappingRevision: 2, Events: events}
		if err = batch.Validate(); err != nil {
			return d, schedule, fmt.Errorf("interval %d: %w", q, err)
		}
		d.put(fmt.Sprintf("batches/%06d.json", q), "OperationalBatchV1", batch)
	}
	d.manifest.Events = sequence
	d.put("live-schedule.json", "FactoryLiveScheduleV1", schedule)
	sort.Slice(d.manifest.Files, func(i, j int) bool { return d.manifest.Files[i].Path < d.manifest.Files[j].Path })
	d.files["manifest.json"] = wire(d.manifest)
	return d, schedule, nil
}
