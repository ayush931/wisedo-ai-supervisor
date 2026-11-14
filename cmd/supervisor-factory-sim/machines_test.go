package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/factorysim"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
)

func TestWIS709SheetsAndWearMatchPlant(t *testing.T) {
	d, _, err := generateLive(278, "2026-09-01", time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC), 60)
	if err != nil {
		t.Fatal(err)
	}
	var sheets []factorysim.MachineSheet
	if err = json.Unmarshal(d.files["machine-sheets.json"], &sheets); err != nil {
		t.Fatal(err)
	}
	classes := map[string]bool{}
	stations := map[string]factorysim.MachineParameters{}
	for _, sheet := range sheets {
		classes[sheet.Class] = true
		for _, s := range sheet.Stations {
			stations[s.StationID] = s
		}
	}
	if len(classes) != 19 || len(stations) != 48 {
		t.Fatal("incomplete coverage", len(classes), len(stations))
	}
	for li, line := range d.manifest.Lines {
		for _, s := range line.Stations {
			p := stations[s.ID]
			if !classes[s.Class] || p.RatedCycleSeconds != s.CycleSeconds || p.MTBFHours != s.MTBFHours || p.RepairMinutes != s.MTTRMinutes {
				t.Fatal("parameter mismatch", s, p)
			}
			for _, cycle := range p.SKUCycles {
				for _, sku := range d.manifest.SKUs {
					if sku.Code == cycle.SKU && (sku.Line != li || cycle.Seconds != s.CycleSeconds*sku.CycleSeconds/line.CycleSeconds) {
						t.Fatal("SKU cycle mismatch")
					}
				}
			}
		}
	}
	var register factorysim.ToolRegister
	if err = json.Unmarshal(d.files["tool-register.json"], &register); err != nil {
		t.Fatal(err)
	}
	if len(register.Tools) != 80 || len(register.Gauges) != 6 {
		t.Fatal("tool/gauge coverage", len(register.Tools), len(register.Gauges))
	}
	station := d.manifest.Lines[1].Stations[2].ID
	tool, err := register.Tool(station, "KN-201")
	if err != nil {
		t.Fatal(err)
	}
	var reached, replaced bool
	for _, event := range register.Events {
		if event.ToolID == tool.ID {
			reached = reached || event.Kind == "life_reached"
			replaced = replaced || event.Kind == "replacement"
		}
	}
	if !reached || !replaced || tool.InitialParts == 0 {
		t.Fatal("wear history missing", tool)
	}
	for q, path := range datasetPaths(d, "batches/") {
		var batch ingestion.Batch
		if err = json.Unmarshal(d.files[path], &batch); err != nil {
			t.Fatal(err)
		}
		for _, event := range batch.Events {
			if event.Kind == "machine_state" && event.AssetExternalKey == station {
				var o factorysim.Observation
				if err = json.Unmarshal(event.Payload, &o); err != nil {
					t.Fatal(err)
				}
				if (o.FaultCode == "TOOL_WEAR_17") != (q >= 8 && q < 12) {
					t.Fatalf("counter-driven wear interval %d: %+v", q, o)
				}
			}
		}
	}
}
