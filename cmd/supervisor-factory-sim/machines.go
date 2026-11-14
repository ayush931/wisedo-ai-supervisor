package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/factorysim"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
)

// addMachineModel preserves the historical fixture while making live wear counter-driven.
func addMachineModel(d *dataset, start time.Time) error {
	tools := factorysim.NewTools(d.manifest, start)
	wornStation := d.manifest.Lines[1].Stations[2].ID
	skuByStationShift := map[string]string{}
	for li, line := range d.manifest.Lines {
		for shift := range 3 {
			for si, s := range line.Stations {
				var job workstation.JobOrderBodyV1
				if err := json.Unmarshal(d.files[fmt.Sprintf("jobs/d00-l%d-s%d-%d.json", li, shift, si)], &job); err != nil {
					return err
				}
				skuByStationShift[fmt.Sprintf("%s/%d", s.ID, shift)] = job.ItemCode
			}
		}
	}
	wearSKU := skuByStationShift[wornStation+"/0"]
	var beforeWear int64
	paths := datasetPaths(*d, "batches/")
	for _, path := range paths {
		var batch ingestion.Batch
		if err := json.Unmarshal(d.files[path], &batch); err != nil {
			return err
		}
		for _, event := range batch.Events {
			if event.Kind != "machine_state" || event.AssetExternalKey != wornStation {
				continue
			}
			var observation factorysim.Observation
			if err := json.Unmarshal(event.Payload, &observation); err != nil {
				return err
			}
			at, err := time.Parse(time.RFC3339, observation.Start)
			if err != nil {
				return err
			}
			if at.Before(start.Add(2 * time.Hour)) {
				beforeWear += observation.Good + observation.Scrap + observation.Rework
			}
		}
	}
	tool, err := tools.Tool(wornStation, wearSKU)
	if err != nil {
		return err
	}
	// Calibrate fictional opening age to one remaining part at the existing two-hour incident.
	tool.InitialParts = tool.LifeParts - beforeWear - 1
	if tool.InitialParts < 0 {
		return fmt.Errorf("opening tool age cannot preserve the incident for seed %d", d.manifest.Seed)
	}
	tool.UsedParts = tool.InitialParts
	for _, path := range paths {
		var batch ingestion.Batch
		if err = json.Unmarshal(d.files[path], &batch); err != nil {
			return err
		}
		for i := range batch.Events {
			event := &batch.Events[i]
			if event.Kind != "machine_state" {
				continue
			}
			var observation factorysim.Observation
			if err = json.Unmarshal(event.Payload, &observation); err != nil {
				return err
			}
			at, e := time.Parse(time.RFC3339, observation.Start)
			if e != nil {
				return e
			}
			shift := int(at.Sub(start) / (8 * time.Hour))
			sku := skuByStationShift[fmt.Sprintf("%s/%d", event.AssetExternalKey, shift)]
			if event.AssetExternalKey == wornStation && at.Equal(start.Add(3*time.Hour)) {
				if err = tools.Replace(wornStation, sku, at); err != nil {
					return err
				}
			}
			worn, e := tools.Process(event.AssetExternalKey, sku, observation.Good+observation.Scrap+observation.Rework, at)
			if e != nil {
				return e
			}
			if event.AssetExternalKey == wornStation && shift == 0 {
				observation = factorysim.WearObservation(observation, worn)
			} else if worn {
				if err = tools.Replace(event.AssetExternalKey, sku, at.Add(15*time.Minute)); err != nil {
					return err
				}
			}
			event.Payload = wire(observation)
		}
		d.files[path] = wire(batch)
	}
	sort.Slice(tools.Events, func(i, j int) bool {
		a, b := tools.Events[i], tools.Events[j]
		if a.At.Equal(b.At) {
			return a.ToolID < b.ToolID
		}
		return a.At.Before(b.At)
	})
	d.put("machine-sheets.json", "FactoryMachineSheetsV1", factorysim.MachineSheets(d.manifest))
	d.put("tool-register.json", "FactoryToolRegisterV1", tools)
	return nil
}
