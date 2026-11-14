package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/factorysim"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/oee"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/onboarding"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plans"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plantcontext"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/production"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
)

type dataset struct {
	manifest factorysim.Manifest
	files    map[string][]byte
}

func wire(v any) []byte {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		panic(e)
	}
	return append(b, '\n')
}
func (d *dataset) put(path, schema string, v any) { d.raw(path, schema, wire(v)) }
func (d *dataset) raw(path, schema string, b []byte) {
	d.files[path] = b
	d.manifest.Files = append(d.manifest.Files, factorysim.File{Path: path, Schema: schema, SHA256: sha(b)})
}
func (d *dataset) id(name string) string { return factorysim.ID(d.manifest.Seed, name) }
func stamp(t time.Time) string           { return t.Format(time.RFC3339) }
func (d *dataset) source(name, title, text string) evidence.Input {
	v := evidence.Input{SourceID: d.id("source/" + name), Kind: "document", Title: title, Text: text}
	d.put("sources/"+name+".json", "EvidenceSourceInputV1", v)
	d.raw("sources/"+name+".md", "EvidenceSourceInputV1", []byte(text))
	return v
}
func (d *dataset) entity(name, kind, assetKind, title, source string) {
	d.put("entities/"+name+".json", "KnowledgeEntityInputV1", knowledge.EntityInput{EntityID: d.id(name), Type: kind, AssetKind: assetKind, Name: title, SourceID: source, SourceRevision: 1})
}
func csvBytes(rows [][]string) []byte {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	_ = w.WriteAll(rows)
	if e := w.Error(); e != nil {
		panic(e)
	}
	return b.Bytes()
}

var columns = []string{"kind", "ref", "id", "revision", "display_name", "active", "unit", "line", "item", "steps", "time_zone", "shifts"}

func (d *dataset) config(name string, rows [][]string) error {
	data := csvBytes(append([][]string{columns}, rows...))
	parsed, e := workstation.ParseConfigurationCSV(data)
	if e != nil {
		return e
	}
	d.put(name+".json", "ConfigurationCSVRowsV1", factorysim.ConfigurationRows{Rows: parsed})
	d.raw(name+".csv", "ConfigurationCSVRowsV1", data)
	return nil
}
func generate(seed int64, days int, start string) (dataset, error) {
	d := dataset{files: map[string][]byte{}, manifest: factorysim.Manifest{SchemaVersion: 1, Synthetic: true, Seed: seed, Start: start, Days: days, Plant: "Sahyadri Precision Components", Location: "Chakan, Pune, India", Currency: "INR", Timezone: "Asia/Kolkata", WorkingDays: 300, Files: []factorysim.File{}}}
	zone, _ := time.LoadLocation("Asia/Kolkata")
	date, e := time.ParseInLocation("2006-01-02", start, zone)
	if e != nil || days < 1 || days > 31 {
		return d, fmt.Errorf("start must be YYYY-MM-DD; days must be 1..31")
	}
	date = date.Add(6 * time.Hour)
	rng := rand.New(rand.NewPCG(uint64(seed), 278))
	names := []string{"Brake casting machining", "Knuckle and rod machining", "Shaft heat treatment", "Press and bracket finishing", "Wheel hub assembly", "Drum subassembly"}
	classes := [][]string{{"turning", "turning", "VMC", "HMC", "grinding", "washing", "CMM", "packing"}, {"VMC", "HMC", "CNC", "induction hardening", "grinding", "washing", "CMM", "packing"}, {"turning", "heat treatment", "induction hardening", "grinding", "grinding", "washing", "CMM", "packing"}, {"blanking press", "forming press", "piercing press", "deburring", "washing", "assembly", "inspection", "packing"}, {"washing", "press fitting", "assembly", "assembly", "torque", "balancing", "CMM", "packing"}, {"turning", "VMC", "washing", "assembly", "assembly", "balancing", "CMM", "packing"}}
	cycles := []int64{50, 65, 50, 30, 225, 225}
	lineCodes := []string{"BD", "KN", "SH", "BR", "HB", "DR"}
	machineNames := map[string]string{"turning": "Lathe", "VMC": "VMC", "HMC": "HMC", "CNC": "CNC", "grinding": "Grinder", "washing": "Washer", "CMM": "CMM", "packing": "Packing", "induction hardening": "Induction hardener", "heat treatment": "Furnace", "blanking press": "Blanking press", "forming press": "Forming press", "piercing press": "Piercing press", "deburring": "Deburring", "assembly": "Assembly", "press fitting": "Press-fit", "torque": "Torque station", "balancing": "Balancer", "inspection": "Inspection"}
	d.manifest.SKUs = []factorysim.SKU{
		{Code: "BD-101", Name: "Ventilated brake disc", Customer: "Deccan Mobility", Line: 0, DailyUnits: 600, PriceINR: 700, Material: "grey iron casting", ToolLife: 400},
		{Code: "BD-102", Name: "Solid brake disc", Customer: "Saffron Commercial Vehicles", Line: 0, DailyUnits: 480, PriceINR: 900, Material: "grey iron casting", ToolLife: 450},
		{Code: "KN-201", Name: "Steering knuckle", Customer: "Deccan Mobility", Line: 1, DailyUnits: 360, PriceINR: 1100, Material: "ductile iron casting", ToolLife: 300},
		{Code: "CR-202", Name: "Connecting rod", Customer: "Vistara Utility Motors", Line: 1, DailyUnits: 480, PriceINR: 650, Material: "steel forging", ToolLife: 350},
		{Code: "GS-301", Name: "Transmission shaft", Customer: "Vistara Utility Motors", Line: 2, DailyUnits: 600, PriceINR: 450, Material: "alloy steel bar", ToolLife: 500},
		{Code: "GS-302", Name: "Pinion shaft", Customer: "Saffron Commercial Vehicles", Line: 2, DailyUnits: 480, PriceINR: 400, Material: "steel forging", ToolLife: 450},
		{Code: "BR-401", Name: "Mounting bracket", Customer: "Deccan Mobility", Line: 3, DailyUnits: 960, PriceINR: 140, Material: "steel coil", ToolLife: 20000},
		{Code: "BR-402", Name: "Crossmember pressing", Customer: "Saffron Commercial Vehicles", Line: 3, DailyUnits: 840, PriceINR: 180, Material: "steel coil", ToolLife: 15000},
		{Code: "HB-501", Name: "Wheel hub assembly", Customer: "Vistara Utility Motors", Line: 4, DailyUnits: 240, PriceINR: 3100, Material: "hub casting and purchased bearing", ToolLife: 800},
		{Code: "DR-601", Name: "Brake drum subassembly", Customer: "Saffron Commercial Vehicles", Line: 5, DailyUnits: 240, PriceINR: 1200, Material: "drum casting", ToolLife: 500},
	}
	for i := range d.manifest.SKUs {
		sku := &d.manifest.SKUs[i]
		seconds := int64(81000)
		if i < 8 {
			seconds = 54000
			if i%2 == 1 {
				seconds = 27000
			}
		}
		sku.CycleSeconds = seconds * 68 / (100 * sku.DailyUnits)
		d.manifest.PlannedDailyRevenueINR += sku.DailyUnits * sku.PriceINR
	}
	var rows [][]string
	for li, name := range names {
		line := factorysim.Line{ID: d.id(fmt.Sprintf("line/%d", li)), Name: name, CycleSeconds: cycles[li], Stations: []factorysim.Station{}}
		rows = append(rows, []string{"line", fmt.Sprintf("line%d", li), "", "", name, "true", "", "", "", "", "", ""})
		for si, class := range classes[li] {
			ct := cycles[li] * (int64(60 + si*5)) / 100
			if si == 4 {
				ct = cycles[li]
			}
			mtbf, mttr := int64(80), int64(35)
			if strings.Contains(class, "press") {
				mtbf, mttr = 120, 25
			}
			if strings.Contains(class, "heat") {
				mtbf, mttr = 160, 60
			}
			line.Stations = append(line.Stations, factorysim.Station{ID: d.id(fmt.Sprintf("station/%d/%d", li, si)), Name: fmt.Sprintf("%s %s-%02d", machineNames[class], lineCodes[li], si+1), Class: class, CycleSeconds: ct, MTBFHours: mtbf, MTTRMinutes: mttr})
		}
		d.manifest.Lines = append(d.manifest.Lines, line)
	}
	for _, sku := range d.manifest.SKUs {
		rows = append(rows, []string{"item", sku.Code, "", "", sku.Code + " " + sku.Name, "true", "count", "", "", "", "", ""})
	}
	var shifts []string
	for day := 1; day <= 7; day++ {
		for _, s := range []string{"06:00-14:00", "14:00-22:00"} {
			shifts = append(shifts, fmt.Sprintf("%d %s", day, s))
		}
	}
	rows = append(rows, []string{"shift_calendar", "calendar", "", "", "Day shifts only - night uses dated plans", "", "", "", "", "", "Asia/Kolkata", strings.Join(shifts, "|")})
	if e = d.config("configuration", rows); e != nil {
		return d, e
	}
	// Import refs are bound by the loader's dry-run result, never assumed database IDs.
	model := d.source("plant", "Plant model", string(wire(d.manifest))+"\nPlanning assumptions, not market quotations. Annual revenue assumes all good units ship; no GST, returns or inventory recognition. 300 operating days, 65 non-working days. The plant manager owns the site; shift leads plan, a second supervisor approves, operators record. Maintenance is a responsibility assigned to a supervisor, not an invented authorization role.\n")
	for li, line := range d.manifest.Lines {
		lname := fmt.Sprintf("line/%d", li)
		sop := d.source(fmt.Sprintf("line-%d", li), line.Name+" SOP", fmt.Sprintf("# SOP: %s\nRevision 1. Asia/Kolkata; three eight-hour shifts starting 06:00, 14:00, 22:00, each with a 30 minute break at shift +3h30.\nRoute: %s. Nameplate constraint %d seconds/part. Count only accepted final parts; scrap is excluded, rework is held until accepted.\nCheck material lot, guards and offsets before start. Change tools at the SKU tool life in plant model; record insert wear as TOOL_WEAR_17 only with equipment observation. Do not infer machine faults from a material shortage. Escalate missing castings to stores and shift supervisor.\nAfter a changeover hold the first five parts for CMM approval; mark bore_size scrap and burr rework separately. Maintenance requires an authorized human; verify restart and counts, and call subsequent improvement threshold recovery, never causation.\n", line.Name, strings.Join(classes[li], " -> "), line.CycleSeconds))
		d.entity(lname, "asset", "line", line.Name, sop.SourceID)
		d.entity(fmt.Sprintf("area/%d", li), "area", "", line.Name+" area", sop.SourceID)
		d.entity(fmt.Sprintf("process/%d", li), "process", "", line.Name+" process", sop.SourceID)
		for _, st := range line.Stations {
			d.put("entities/station-"+st.ID+".json", "KnowledgeEntityInputV1", knowledge.EntityInput{EntityID: st.ID, Type: "asset", AssetKind: "machine", Name: st.Name, SourceID: sop.SourceID, SourceRevision: 1})
			d.put("profiles/"+st.ID+".json", "StationProfileBodyV1", workstation.StationProfileBodyV1{SchemaVersion: 1, WorkstationID: st.ID, LineID: line.ID, DisplayName: st.Name, Template: "production", Fields: []workstation.ProfileFieldV1{{Name: "good_count", Label: "Accepted parts", Kind: "decimal", Required: true, Unit: "count", Min: "0"}}, InstructionReferences: []workstation.InstructionRefV1{{DocumentID: sop.SourceID, Revision: 1, SHA256: sha([]byte(sop.Text))}}})
			d.put("claims/"+st.ID+".json", "KnowledgeClaimInputV1", knowledge.ClaimInput{ClaimID: d.id("claim/" + st.ID), SubjectID: st.ID, SubjectRevision: 1, Predicate: "part_of_asset", ObjectEntityID: line.ID, ObjectRevision: 1, EffectiveFrom: stamp(date), EvidenceClass: "report", SourceSpanIDs: []string{d.id("span/plant")}})
		}
		d.put(fmt.Sprintf("discovery/%d.json", li), "KnowledgeDiscoveryInputV1", knowledge.DiscoveryInput{InventoryID: d.id(fmt.Sprintf("discovery/%d", li)), AreaID: d.id(fmt.Sprintf("area/%d", li)), AreaRevision: 1, ProcessID: d.id(fmt.Sprintf("process/%d", li)), ProcessRevision: 1, Capability: "shortfall_investigation", Reason: "Line onboarding scope"})
		for _, role := range []onboarding.InterviewRole{onboarding.RoleOwner, onboarding.RoleSupervisor, onboarding.RoleOperator} {
			text := fmt.Sprintf("%s answer for %s. %s Counts are final accepted units in count, offsets are Asia/Kolkata. The shift lead is responsible; stores handles material delays. Use source revision 2 with freshness 120 seconds; review SOP each shift. Schedule and station identities are in the plant model.", role, line.Name, sop.Text)
			d.put(fmt.Sprintf("answers/%d-%s.json", li, role), "InterviewAnswerInputV1", onboarding.AnswerInput{QuestionID: uuid.MustParse(d.id("question-placeholder")), QuestionRevision: 1, Text: text, ClientAnswerID: uuid.MustParse(d.id(fmt.Sprintf("answer/%d/%s", li, role)))})
		}
	}
	var routingRows [][]string
	for _, sku := range d.manifest.SKUs {
		line := d.manifest.Lines[sku.Line]
		steps := []string{}
		for _, s := range line.Stations {
			steps = append(steps, s.ID)
		}
		routingRows = append(routingRows, []string{"routing", sku.Code, "", "", sku.Code + " route", "true", "", line.ID, d.id("item/" + sku.Code), strings.Join(steps, "|"), "", ""})
	}
	if e = d.config("routings", routingRows); e != nil {
		return d, e
	}
	mappings := []map[string]any{}
	for li, line := range d.manifest.Lines {
		mappings = append(mappings, map[string]any{"external_type": "asset", "external_key": fmt.Sprintf("line-%d", li), "entity_id": line.ID, "entity_revision": 1})
		for _, st := range line.Stations {
			mappings = append(mappings, map[string]any{"external_type": "asset", "external_key": st.ID, "entity_id": st.ID, "entity_revision": 1})
		}
	}
	events := []ingestion.Event{}
	sourceID := d.id("telemetry")
	add := func(key, kind, asset, order string, at time.Time, payload any, rev int64) {
		events = append(events, ingestion.Event{ExternalKey: key, Revision: rev, Kind: kind, SourceSequence: int64(len(events) + 1), EventAt: at, Payload: wire(payload), AssetExternalKey: asset, OrderExternalKey: order})
	}
	var corrections []ingestion.Event
	for day := 0; day < days; day++ {
		base := date.AddDate(0, 0, day)
		for li, line := range d.manifest.Lines {
			prefix := fmt.Sprintf("d%02d-l%d", day, li)
			order := d.id("order/" + prefix)
			orderKey := "order-" + prefix
			assetKey := fmt.Sprintf("line-%d", li)
			d.entity("order/"+prefix, "order", "", fmt.Sprintf("WO-%s-%s", base.Format("0102"), lineCodes[li]), model.SourceID)
			mappings = append(mappings, map[string]any{"external_type": "order", "external_key": orderKey, "entity_id": order, "entity_revision": 1})
			skus := []factorysim.SKU{}
			for _, sku := range d.manifest.SKUs {
				if sku.Line == li {
					skus = append(skus, sku)
				}
			}
			plan := production.PlanBody{OrderID: order, AssetID: line.ID, WindowStart: stamp(base), WindowEnd: stamp(base.Add(24 * time.Hour)), PlanRevision: 1, Unit: "count", RateSegments: []production.RateSegment{}, PlannedBreaks: []production.Interval{}}
			for shift := 0; shift < 3; shift++ {
				sku := skus[0]
				slots := int64(90)
				if len(skus) == 2 {
					slots = 60
					if shift == 1 {
						sku = skus[1]
						slots = 30
					}
				}
				st := base.Add(time.Duration(shift) * 8 * time.Hour)
				end := st.Add(8 * time.Hour)
				// WIS-272: the plan contract carries only a six-decimal rate per second, and
				// most per-window targets (28 units/900 s = 7/225) do not terminate. Round the
				// rate up, never to nearest, so a whole-unit deficit of 5 still qualifies
				// (28.0008 - 23 >= 5) instead of falling to 4.9999. This fixes the synthetic
				// data only: the product contract (internal/production/domain.go:40-44) still
				// misses a real whole-unit shortfall exactly on the threshold whenever the
				// per-window target is not a multiple of 9. Follow-up: a quantity-per-segment
				// field (needs a WIS-272 decision). ponytail: on-target windows here show a
				// deficit below 0.001, which that field also removes.
				den := slots * 900
				rate, _ := production.Decimal6HalfEven(big.NewRat((sku.DailyUnits*1_000_000+den-1)/den, 1_000_000))
				rate = strings.TrimRight(strings.TrimRight(rate, "0"), ".")
				plan.RateSegments = append(plan.RateSegments, production.RateSegment{Start: stamp(st), End: stamp(end), TargetGoodUnitsPerSecond: rate})
				plan.PlannedBreaks = append(plan.PlannedBreaks, production.Interval{Start: stamp(st.Add(210 * time.Minute)), End: stamp(st.Add(240 * time.Minute))})
				sp := workstation.ShiftPlanBodyV1{SchemaVersion: 1, LineID: line.ID, StartsAt: stamp(st), EndsAt: stamp(end), Timezone: "Asia/Kolkata", Assignments: []workstation.AssignmentV1{}}
				var sop evidence.Input
				_ = json.Unmarshal(d.files[fmt.Sprintf("sources/line-%d.json", li)], &sop)
				for si, station := range line.Stations {
					jp := fmt.Sprintf("%s-s%d-%d", prefix, shift, si)
					step := si + 1
					job := workstation.JobOrderBodyV1{SchemaVersion: 1, OrderID: order, OperationID: d.id("operation/" + station.ID), ItemCode: sku.Code, ItemID: d.id("item/" + sku.Code), RoutingID: d.id("routing/" + sku.Code), StepNumber: &step, TargetQuantity: sku.DailyUnits * 30 / slots, TargetCycleTimeSeconds: strconv.FormatInt(station.CycleSeconds*sku.CycleSeconds/line.CycleSeconds, 10), InstructionDocumentID: sop.SourceID, InstructionRevision: 1, InstructionSHA256: sha([]byte(sop.Text)), Template: "production", Fields: []workstation.FieldV1{{Name: "good_count", Label: "Accepted parts", Kind: "decimal", Required: true, Unit: "count"}}}
					d.put("jobs/"+jp+".json", "JobOrderBodyV1", job)
					sp.Assignments = append(sp.Assignments, workstation.AssignmentV1{WorkstationID: station.ID, JobOrderID: d.id("job/" + jp), JobOrderRevision: 1, OrderID: order, OperationID: job.OperationID, ItemCode: sku.Code, TargetQuantity: job.TargetQuantity, TargetCycleTimeSeconds: job.TargetCycleTimeSeconds, InstructionDocumentID: sop.SourceID, InstructionRevision: 1, InstructionSHA256: job.InstructionSHA256, Template: "production", Fields: job.Fields})
				}
				d.put(fmt.Sprintf("shifts/%s-s%d.json", prefix, shift), "ShiftPlanBodyV1", sp)
			}
			d.put("plans/"+prefix+".json", "PlanCaptureInputV1", plans.CaptureInput{Plan: plan, SourceSpanIDs: []string{d.id("span/plant")}})
			totals := map[string]int64{}
			for _, sku := range skus {
				totals[sku.Code] = sku.DailyUnits + int64(rng.IntN(11)-5)*sku.DailyUnits/1000
			}
			position := map[string]int64{}
			wip := int64(0)
			repairRemaining := int64(0)
			oeeData := oee.OeeDatasetV1{DatasetID: d.id("oee/" + prefix), SchemaID: oee.DatasetSchemaID, AssetID: line.ID, WindowStart: stamp(base), WindowEnd: stamp(base.Add(24 * time.Hour)), Sources: []oee.OeeSourceV1{{SourceKey: "line-counts", OwnerService: "supervisor-factory-sim", SourceRevision: "1", ConsistencyMode: "SNAPSHOT", SnapshotRef: prefix}}, Facts: []oee.OeeFactV1{}, Exclusions: []oee.OeeExclusionV1{}, Counts: []oee.OeeCountV1{}, IdealCycles: []oee.OeeIdealCycleV1{}}
			for _, sku := range skus {
				oeeData.IdealCycles = append(oeeData.IdealCycles, oee.OeeIdealCycleV1{SourceKey: "line-counts", ProductID: sku.Code, Revision: 1, SecondsPerUnit: strconv.FormatInt(sku.CycleSeconds, 10), Unit: "s/unit"})
			}
			for q := 0; q < 96; q++ {
				shift := q / 32
				sku := skus[0]
				slots := int64(90)
				if len(skus) == 2 {
					slots = 60
					if shift == 1 {
						sku = skus[1]
						slots = 30
					}
				}
				st := base.Add(time.Duration(q) * 15 * time.Minute)
				end := st.Add(15 * time.Minute)
				good := int64(0)
				reason := "normal"
				state := "running"
				fault := ""
				scrap, rework := int64(0), int64(0)
				effective := sku.CycleSeconds
				if q%32 == 14 || q%32 == 15 {
					reason = "planned_break"
					state = "planned_stop"
				} else {
					pos := position[sku.Code]
					good = (pos+1)*totals[sku.Code]/slots - pos*totals[sku.Code]/slots
					position[sku.Code]++
					if q%9 == 0 {
						scrap = 1
					}
					if q%13 == 0 {
						rework = 1
					}
				}
				// A sampled constraint-machine repair reduces line output for its MTTR.
				downSeconds := int64(0)
				if reason == "normal" {
					constraint := line.Stations[4]
					if repairRemaining == 0 && rng.IntN(int(constraint.MTBFHours)*4) == 0 {
						repairRemaining = constraint.MTTRMinutes * 60
					}
					downSeconds = min(repairRemaining, 900)
					repairRemaining -= downSeconds
					if downSeconds > 0 {
						good = good * (900 - downSeconds) / 900
						reason = "equipment_repair"
					}
				}
				if day == 0 {
					switch {
					case li == 0 && q >= 4 && q < 8:
						reason = "constraint_capacity"
						effective = 180
						cap := int64(792) / effective
						wip += good - cap
						if good > cap {
							good = cap
						}
					case li == 1 && q >= 8 && q < 12:
						reason = "tool_wear_microstops"
						fault = "TOOL_WEAR_17"
						good = min(good, 1)
					case li == 0 && q >= 16 && q < 20:
						reason = "material_shortage"
						state = "starved"
						good = 0
					case li == 3 && q >= 34 && q < 36:
						reason = "changeover_bore_size"
						scrap = good / 5
						good -= scrap
						rework = 2
					case li == 1 && q >= 12 && q < 14:
						reason = "threshold_recovery_after_maintenance"
					}
				}
				if reason == "normal" && q%32 == 0 && q > 0 && len(skus) == 2 {
					reason = "changeover"
					good = good * 4 / 5
				}
				if reason == "normal" && good > 0 {
					before := (position[sku.Code] - 1) * totals[sku.Code] / slots
					after := position[sku.Code] * totals[sku.Code] / slots
					if after/sku.ToolLife > before/sku.ToolLife {
						reason = "tool_change"
						good = good * 4 / 5
					}
				}
				key := fmt.Sprintf("%s-q%02d", prefix, q)
				delta := production.Delta{OrderID: order, AssetID: line.ID, IntervalStart: stamp(st), IntervalEnd: stamp(end), GoodUnits: strconv.FormatInt(good, 10), Unit: "count"}
				add(key, "production_delta", assetKey, orderKey, end, delta, 1)
				d.manifest.RevenueINR += good * sku.PriceINR
				if day == 0 && li == 0 && q == 0 {
					delta.GoodUnits = strconv.FormatInt(good+3, 10)
					corrections = append(corrections, ingestion.Event{ExternalKey: key, Revision: 2, Kind: "production_delta", EventAt: base.Add(9 * time.Hour), Payload: wire(delta), AssetExternalKey: assetKey, OrderExternalKey: orderKey})
					d.manifest.RevenueINR += 3 * sku.PriceINR
				}
				obs := factorysim.Observation{Synthetic: true, Start: stamp(st), End: stamp(end), State: state, Reason: reason, FaultCode: fault, Good: good, Scrap: scrap, Rework: rework, RunSeconds: (900 - downSeconds) * 88 / 100, MicrostopSeconds: int64(15 + rng.IntN(25)), IdealCycleSeconds: sku.CycleSeconds, EffectiveCycleSeconds: effective, WIP: wip, Material: sku.Material, Note: "Interval summary"}
				if state != "running" {
					obs.RunSeconds = 0
					obs.MicrostopSeconds = 0
				}
				if fault != "" {
					obs.MicrostopSeconds = 720
					obs.RunSeconds = 180
				}
				if reason == "changeover" || reason == "tool_change" {
					obs.RunSeconds = 600
				}
				if q%32 == 15 {
					obs.Note = "Planned lubrication and safety checks during the break; 10 minutes, no extra lost production"
				}
				if day == 0 && li == 1 && q == 12 {
					add(prefix+"-maintenance-observation", "operator_report", assetKey, orderKey, st, factorysim.Observation{Synthetic: true, Start: stamp(st.Add(-5 * time.Minute)), End: stamp(st), State: "reported_complete", Reason: "insert_inspection", Note: "Operator reported insert inspection by maintenance; no work order reference"}, 1)
				}
				add(key+"-quality", "operator_report", assetKey, orderKey, end, obs, 1)
				// One OEE dataset per line/day uses the same counts, losses and exact cycles.
				countGood := good
				if day == 0 && li == 0 && q == 0 {
					countGood += 3
				}
				oeeData.Counts = append(oeeData.Counts, oee.OeeCountV1{ContributionID: key, SourceKey: "line-counts", ProductID: sku.Code, Start: stamp(st), End: stamp(end), Total: strconv.FormatInt(countGood+scrap+rework, 10), Conforming: strconv.FormatInt(countGood, 10), Scrap: strconv.FormatInt(scrap, 10), Rework: strconv.FormatInt(rework, 10), Unit: "count"})
				fact := func(s, e time.Time, state oee.TimelineState, suffix string) {
					if !e.After(s) {
						return
					}
					oeeData.Facts = append(oeeData.Facts, oee.OeeFactV1{EventID: key + suffix, SourceKey: "line-counts", State: state, Start: stamp(s), End: stamp(e), EventAt: stamp(s), RecordedAt: stamp(end), Revision: 1, PayloadDigest: sha(wire(obs))})
				}
				if reason == "planned_break" {
					fact(st, end, oee.StatePlannedBreak, "-break")
				} else {
					split := st.Add(time.Duration(obs.RunSeconds) * time.Second)
					fact(st, split, oee.StateRunning, "-run")
					fact(split, end, oee.StateUnplannedDowntime, "-stop")
				}
				for si, station := range line.Stations {
					machine := obs
					machine.IdealCycleSeconds = station.CycleSeconds * sku.CycleSeconds / line.CycleSeconds
					machine.EffectiveCycleSeconds = machine.IdealCycleSeconds
					if reason == "constraint_capacity" && si == 4 {
						machine.EffectiveCycleSeconds = effective
					}
					if reason == "constraint_capacity" && si < 4 {
						machine.Good = sku.DailyUnits / slots
					}
					if reason == "tool_wear_microstops" && si != 2 {
						machine.FaultCode = ""
						machine.Reason = "downstream_wait"
					}
					if reason == "material_shortage" {
						machine.State = "stopped"
						machine.Reason = "NO_MATERIAL"
					}
					if reason == "tool_wear_microstops" && si == 2 {
						machine.State = "stopped"
						machine.Reason = "TOOL_WEAR_17"
					}
					if reason == "equipment_repair" && si == 4 {
						machine.State = "stopped"
						machine.FaultCode = "DRIVE_TRIP_04"
					}
					add(key+"-"+station.ID, "machine_state", station.ID, orderKey, st.Add(30*time.Second), machine, 1)
				}
				if day == 0 && (q == 4 || q == 5 || q == 12 || q == 13 || li == 0 && (q == 0 || q == 1 || q == 16 || q == 17) || li == 1 && (q == 8 || q == 9) || li == 3 && (q == 34 || q == 35)) {
					d.put(fmt.Sprintf("contexts/%s-q%02d.json", prefix, q), "PlantContextRequestV1", plantcontext.Request{OrderID: order, AssetID: line.ID, WindowStart: stamp(st), WindowEnd: stamp(end), ProcessSnapshotID: d.id("snapshot")})
				}
			}
			oeeData.ProvenanceDigest, e = oee.DatasetProvenanceDigest(oeeData)
			if e != nil {
				return d, e
			}
			d.put("oee/"+prefix+".json", "OeeCalculationRequestV1", oee.OeeCalculationRequestV1{Dataset: oeeData, ExpectedPolicyRevision: 1})
			arrival := base
			note := "Bhima Metals daily lot released after incoming inspection"
			if day == 0 && li == 0 {
				arrival = base.Add(5 * time.Hour)
				note = "Grey iron casting replenishment due 10:00 arrives 11:00; opening stock exhausted at 10:00"
			}
			add(prefix+"-material-arrival", "operator_report", assetKey, orderKey, arrival, factorysim.Observation{Synthetic: true, Start: stamp(arrival), End: stamp(arrival.Add(time.Minute)), State: "received", Reason: "material_arrival", Material: skus[0].Material, Note: note}, 1)
		}
		// A delayed correction follows its closed shift in arrival order, preserving revision 1.
		if day == 0 {
			for _, c := range corrections {
				add(c.ExternalKey, c.Kind, c.AssetExternalKey, c.OrderExternalKey, c.EventAt, json.RawMessage(c.Payload), c.Revision)
			}
		}
	}
	d.put("operational-source.json", "OperationalSourceConfigV1", map[string]any{"source_id": sourceID, "type": "event_api", "authority_scope": "production_delta,machine_state,operator_report,heartbeat,watermark", "enabled": true, "access_policy_revision": 1, "freshness_seconds": 120, "clock_skew_seconds": 30})
	d.put("mappings.json", "OperationalSourceMappingsInputV1", map[string]any{"mappings": mappings})
	for pos := 0; pos < len(events); pos += ingestion.MaxEvents {
		end := min(pos+ingestion.MaxEvents, len(events))
		d.put(fmt.Sprintf("batches/%06d.json", pos/ingestion.MaxEvents), "OperationalBatchV1", ingestion.Batch{SchemaVersion: 1, SourceID: sourceID, SourceRevision: 2, MappingRevision: 2, Events: events[pos:end]})
	}
	d.manifest.Events = int64(len(events))
	d.put("oee-policy.json", "OeePolicyRevisionInputV1", oee.OeePolicyRevisionInputV1{PolicyCode: "chakan-oee-" + strconv.FormatInt(seed, 10), EffectiveStart: stamp(date), Policy: factoryPolicy()})
	d.put("snapshot.json", "KnowledgeSnapshotInputV1", knowledge.SnapshotInput{SnapshotID: d.id("snapshot"), Reason: "Process snapshot after line onboarding"})
	key := factorysim.AnswerKey{Synthetic: true, Scenarios: []factorysim.Scenario{}, Gaps: []string{"Approvals, claim reviews and source reviews use scripted synthetic fixture identities; they are not independent human review.", "No real OEM, customer, hardware, maintenance execution or causality evidence.", "Historical station records cannot satisfy current shift approval and clock checks; use operational observations.", "Case evaluation, proposal approval, task execution and outcome observation require the Supervisor workers and authorized humans; this loader never fabricates them.", "Revenue assumes accepted output ships; no inventory, tax, receivables, energy or finite supplier model.", "Operational observation payloads are evidence; matching OEE inputs are generated explicitly and production contexts use typed approved plans and count deltas."}}
	scenario := func(name string, line, a, b int, truth, reaction string) {
		keys := []string{}
		for q := a; q < b; q++ {
			keys = append(keys, fmt.Sprintf("d00-l%d-q%02d", line, q))
		}
		key.Scenarios = append(key.Scenarios, factorysim.Scenario{Name: name, Start: stamp(date.Add(time.Duration(a) * 15 * time.Minute)), End: stamp(date.Add(time.Duration(b) * 15 * time.Minute)), EvidenceKeys: keys, GroundTruth: truth, ExpectedReaction: reaction})
	}
	scenario("Throughput constraint", 0, 4, 8, "Grinder BD-05 effective cycle 180s caps throughput at 4/15min; upstream WIP rises while it runs.", "Investigate capacity and routing, not simply stopped state; propose a human-reviewed rebalance.")
	scenario("Tool wear", 1, 8, 12, "CNC KN-03 three recurring 240s microstops per interval with TOOL_WEAR_17.", "Inspect corroborating fault observation; propose authorized insert inspection/replacement.")
	scenario("Late castings", 0, 16, 20, "Grey iron castings arrive an hour late; zero good output, starved state, no equipment fault code.", "Escalate to stores, never classify as maintenance.")
	scenario("Late correction", 0, 0, 1, "d00-l0-q00 revision 2 adds three accepted parts, delivered after the shift; revision 1 stays immutable.", "Rebuild affected context and retain supersession provenance.")
	scenario("Changeover quality spike", 3, 34, 36, "First-shift-to-second-SKU changeover; 20 percent bore_size rejects plus two burr rework parts per interval.", "Contain lot and inspect setup before proposing restart.")
	scenario("Threshold recovery", 1, 12, 14, "Counts recover after a scripted maintenance observation; no executable task or causal experiment exists.", "Describe threshold recovery only; do not claim the task caused improvement.")
	d.put("answer-key.json", "FactoryAnswerKeyV1", key)
	sort.Slice(d.manifest.Files, func(i, j int) bool { return d.manifest.Files[i].Path < d.manifest.Files[j].Path })
	d.files["manifest.json"] = wire(d.manifest)
	return d, nil
}
