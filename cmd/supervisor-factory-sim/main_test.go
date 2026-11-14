package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/factorysim"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/oee"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plans"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/production"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestDeterministicDatasetEconomicsAndValidation(t *testing.T) {
	a, e := generate(278, 2, "2026-09-01")
	if e != nil {
		t.Fatal(e)
	}
	b, e := generate(278, 2, "2026-09-01")
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(a.files, b.files) {
		t.Fatal("seeded files differ")
	}
	// The manifest includes every file hash; update only for a reviewed generator change.
	const golden = "41672a1296b3b7031c9fdf3949aed7cd36a4e0488b4f00f3f0422a63e8773375"
	if got := sha(a.files["manifest.json"]); got != golden {
		t.Fatalf("seed 278, 2 days, 2026-09-01 manifest SHA-256 = %s; want %s", got, golden)
	}
	if e = validate(a); e != nil {
		t.Fatal(e)
	}
	annual := a.manifest.RevenueINR * 300 / 2
	if annual < 950000000 || annual > 1050000000 {
		t.Fatalf("annual revenue INR %d outside frozen 95..105 crore", annual)
	}
	if len(a.manifest.Lines) != 6 || len(a.manifest.SKUs) != 10 || a.manifest.PlannedDailyRevenueINR != 3339600 {
		t.Fatalf("unexpected model: %+v", a.manifest)
	}
	c, _ := generate(279, 1, "2026-09-01")
	if bytes.Equal(a.files["batches/000000.json"], c.files["batches/000000.json"]) {
		t.Fatal("seed has no effect")
	}
	var revisions []int64
	materialFaults := 0
	materialStops, toolStops, bottlenecks, correctionUnits := 0, 0, 0, int64(0)
	var revenue int64
	latest := map[string]ingestion.Event{}
	for _, f := range a.manifest.Files {
		if f.Schema != "OperationalBatchV1" {
			continue
		}
		var batch ingestion.Batch
		if e = json.Unmarshal(a.files[f.Path], &batch); e != nil {
			t.Fatal(e)
		}
		if len(batch.Events) > 100 {
			t.Fatal("batch too large")
		}
		for _, event := range batch.Events {
			if event.Kind == "production_delta" {
				latest[event.ExternalKey] = event
			}
			if event.ExternalKey == "d00-l0-q00" {
				revisions = append(revisions, event.Revision)
			}
			if event.Kind == "operator_report" {
				var obs factorysim.Observation
				_ = json.Unmarshal(event.Payload, &obs)
				if obs.Reason == "material_shortage" && obs.FaultCode != "" {
					materialFaults++
				}
			}
			if event.Kind == "machine_state" {
				var obs factorysim.Observation
				_ = json.Unmarshal(event.Payload, &obs)
				if obs.Reason == "NO_MATERIAL" {
					materialStops++
					if obs.State != "stopped" || obs.FaultCode != "" || obs.Good != 0 {
						t.Fatalf("material became equipment fault: %+v", obs)
					}
				}
				if obs.Reason == "TOOL_WEAR_17" {
					toolStops++
					if obs.State != "stopped" || obs.FaultCode != "TOOL_WEAR_17" {
						t.Fatalf("lost tool fault: %+v", obs)
					}
				}
				if obs.Reason == "constraint_capacity" && obs.EffectiveCycleSeconds == 180 {
					bottlenecks++
					if obs.Good > obs.RunSeconds/180 || obs.WIP <= 0 {
						t.Fatalf("constraint does not cap output: %+v", obs)
					}
				}
			}
		}
	}
	if !reflect.DeepEqual(revisions, []int64{1, 2}) || materialFaults != 0 || materialStops != 32 || toolStops != 4 || bottlenecks != 4 {
		t.Fatalf("correction/fault boundary: %v %d", revisions, materialFaults)
	}
	for key, event := range latest {
		var day, line, q int
		if _, e := fmt.Sscanf(key, "d%02d-l%d-q%02d", &day, &line, &q); e != nil {
			t.Fatal(e)
		}
		var delta production.Delta
		if e := json.Unmarshal(event.Payload, &delta); e != nil {
			t.Fatal(e)
		}
		good, e := strconv.ParseInt(delta.GoodUnits, 10, 64)
		if e != nil {
			t.Fatal(e)
		}
		var skus []factorysim.SKU
		for _, sku := range a.manifest.SKUs {
			if sku.Line == line {
				skus = append(skus, sku)
			}
		}
		sku := skus[0]
		if len(skus) == 2 && q/32 == 1 {
			sku = skus[1]
		}
		revenue += good * sku.PriceINR
		if key == "d00-l0-q00" {
			correctionUnits = good
		}
	}
	if revenue != a.manifest.RevenueINR || correctionUnits < 3 {
		t.Fatalf("economics does not reconcile to corrected production: %d vs %d", revenue, a.manifest.RevenueINR)
	}
	// Validation must reject both type-invalid values and bytes changed after generation.
	changed := a
	changed.files = map[string][]byte{}
	for k, v := range a.files {
		changed.files[k] = append([]byte(nil), v...)
	}
	changed.files["answer-key.json"] = []byte(`{"synthetic":"yes","scenarios":[],"gaps":[]}`)
	for i, f := range changed.manifest.Files {
		if f.Path == "answer-key.json" {
			changed.manifest.Files[i].SHA256 = sha(changed.files[f.Path])
		}
	}
	changed.files["manifest.json"] = wire(changed.manifest)
	if validate(changed) == nil {
		t.Fatal("schema-invalid boolean accepted")
	}
}
func TestGenerateCLIAndTamperDetection(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := filepath.Join(t.TempDir(), "data")
	var out bytes.Buffer
	if e := run(context.Background(), []string{"generate", "--days", "1", "--seed", "278", "--out", dir}, &out); e != nil {
		t.Fatal(e)
	}
	if _, e := readDataset(dir); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, "answer-key.json"), []byte("{}"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := readDataset(dir); e == nil {
		t.Fatal("tampered dataset accepted")
	}
	if e := run(context.Background(), []string{"generate", "--out", dir, "--days", "1"}, &out); e == nil {
		t.Fatal("overwrote dataset")
	}
}
func TestLocalGuards(t *testing.T) {
	for _, bad := range []string{"https://customer.example", "http://localhost:9999", "http://127.0.0.1:48176", "http://127.0.0.1@evil.example", "http://127.0.0.1/path", "http://127.0.0.1?target=prod", "http://0.0.0.0:9911"} {
		if _, e := localURL(bad); e == nil {
			t.Fatalf("unsafe target accepted %s", bad)
		}
	}
	if _, e := localURL("http://[::1]:9999"); e != nil {
		t.Fatal(e)
	}
	if _, e := newLoader("http://127.0.0.1:9999", func(string) string { return "" }); e == nil {
		t.Fatal("non-local environment accepted")
	}
	for _, n := range []int{-1, 0, 32} {
		if _, e := generate(1, n, "2026-09-01"); e == nil {
			t.Fatal("unbounded days")
		}
	}
}

func TestOEEUsesSharedCountsAndLosses(t *testing.T) {
	d, e := generate(278, 2, "2026-09-01")
	if e != nil {
		t.Fatal(e)
	}
	policy, digest, e := factoryPolicy().Canonical(factorysim.TenantID, factorysim.SiteID)
	if e != nil {
		t.Fatal(e)
	}
	ref := oee.OeePolicyRefV1{PolicyID: d.id("oee-policy"), PolicyRevisionID: d.id("oee-policy-revision"), Revision: 1, Digest: digest}
	lo, hi := big.NewRat(55, 100), big.NewRat(75, 100)
	for _, f := range d.manifest.Files {
		if f.Schema != "OeeCalculationRequestV1" {
			continue
		}
		var request oee.OeeCalculationRequestV1
		if e = json.Unmarshal(d.files[f.Path], &request); e != nil {
			t.Fatal(e)
		}
		ev, e := oee.CalculateDataset(factorysim.TenantID, factorysim.SiteID, request.Dataset, ref, policy)
		if e != nil {
			t.Fatal(e)
		}
		if ev.Status != oee.StatusReady {
			t.Fatalf("%s: %s %v %+v", f.Path, ev.Status, ev.ReasonCodes, ev.KPIs)
		}
		value, ok := new(big.Rat).SetString(ev.KPIs.OEE.Value)
		if !ok || value.Cmp(lo) < 0 || value.Cmp(hi) > 0 {
			t.Fatalf("%s OEE %s outside frozen 55..75%%", f.Path, ev.KPIs.OEE.Value)
		}
	}
}

// WIS-520: every generated job order links the routing step of the station it is assigned to,
// so shift-plan suggestions can place simulator jobs instead of returning ROUTING_UNLINKED.
func TestJobOrdersLinkAssignedRoutingStep(t *testing.T) {
	d, e := generate(278, 1, "2026-09-01")
	if e != nil {
		t.Fatal(e)
	}
	rows, e := workstation.ParseConfigurationCSV(d.files["routings.csv"])
	if e != nil {
		t.Fatal(e)
	}
	steps := map[string][]string{}
	for _, r := range rows {
		steps[d.id("routing/"+r.Ref)] = strings.Split(r.Steps, "|")
	}
	checked := 0
	for _, f := range d.manifest.Files {
		if f.Schema != "ShiftPlanBodyV1" {
			continue
		}
		var plan workstation.ShiftPlanBodyV1
		if e = json.Unmarshal(d.files[f.Path], &plan); e != nil {
			t.Fatal(e)
		}
		prefix := strings.TrimSuffix(strings.TrimPrefix(f.Path, "shifts/"), ".json")
		for si, a := range plan.Assignments {
			var job workstation.JobOrderBodyV1
			if e = json.Unmarshal(d.files[fmt.Sprintf("jobs/%s-%d.json", prefix, si)], &job); e != nil {
				t.Fatal(e)
			}
			route := steps[job.RoutingID]
			if job.ItemID != d.id("item/"+job.ItemCode) || job.StepNumber == nil || *job.StepNumber < 1 || *job.StepNumber > len(route) || route[*job.StepNumber-1] != a.WorkstationID {
				t.Fatalf("%s job %d: routing %q item %q step %v does not match station %s", f.Path, si, job.RoutingID, job.ItemID, job.StepNumber, a.WorkstationID)
			}
			checked++
		}
	}
	if checked != 6*3*8 {
		t.Fatalf("checked %d job orders, want 144", checked)
	}
}

// WIS-278: showcase data reads like a real plant. Visible names and text carry no
// "synthetic"/"fictional" labels, while the manifest, observations and answer key keep the
// machine-readable synthetic flag so the data can never be mistaken for customer evidence.
func TestShowcaseNamesAreRealisticAndFlagIsKept(t *testing.T) {
	d, e := generate(278, 1, "2026-09-01")
	if e != nil {
		t.Fatal(e)
	}
	flag := strings.NewReplacer(`"synthetic":`, "", `\"synthetic\":`, "")
	for path, b := range d.files {
		if path == "answer-key.json" {
			continue
		}
		text := strings.ToLower(flag.Replace(string(b)))
		for _, word := range []string{"synthetic", "fictional", "fixture"} {
			if i := strings.Index(text, word); i >= 0 {
				t.Errorf("%s shows %q: …%s…", path, word, text[max(0, i-40):min(len(text), i+40)])
			}
		}
	}
	var key factorysim.AnswerKey
	var manifest factorysim.Manifest
	if json.Unmarshal(d.files["answer-key.json"], &key) != nil || json.Unmarshal(d.files["manifest.json"], &manifest) != nil || !manifest.Synthetic || !key.Synthetic {
		t.Fatal("synthetic flag dropped from manifest or answer key")
	}
	reports := 0
	for _, f := range manifest.Files {
		if f.Schema != "OperationalBatchV1" {
			continue
		}
		var batch ingestion.Batch
		if e = json.Unmarshal(d.files[f.Path], &batch); e != nil {
			t.Fatal(e)
		}
		for _, event := range batch.Events {
			var obs factorysim.Observation
			if event.Kind != "operator_report" {
				continue
			}
			if e = json.Unmarshal(event.Payload, &obs); e != nil || !obs.Synthetic {
				t.Fatalf("%s observation lost its synthetic flag", event.ExternalKey)
			}
			reports++
		}
	}
	if reports == 0 {
		t.Fatal("no observations checked")
	}
	seen := map[string]bool{}
	for _, line := range manifest.Lines {
		for _, st := range line.Stations {
			if seen[st.Name] || strings.HasPrefix(st.Name, " ") {
				t.Fatalf("station name %q is duplicated or missing its machine type", st.Name)
			}
			seen[st.Name] = true
		}
	}
	for _, s := range key.Scenarios {
		for _, name := range regexp.MustCompile(`[A-Z][a-z]+ [A-Z]{2}-\d\d|CNC [A-Z]{2}-\d\d`).FindAllString(s.GroundTruth, -1) {
			if !seen[name] {
				t.Errorf("scenario %q names unknown station %q", s.Name, name)
			}
		}
	}
	if got := manifest.Lines[0].Stations[4].Name; got != "Grinder BD-05" {
		t.Fatalf("brake line constraint station = %q, want Grinder BD-05", got)
	}
}

// WIS-272 regression: every answer-key incident window qualifies for a shortfall case
// exactly when the incident is a shortfall. The "Changeover quality spike" (line 3,
// q34-q35) loses 5 units against 28 planned per 15 minutes; a half-even six-decimal rate
// (0.031111) made expected 27.9999 and the deficit 4.9999 < 5, so no case could open.
func TestAnswerKeyShortfallWindowsQualify(t *testing.T) {
	d, e := generate(278, 2, "2026-09-01")
	if e != nil {
		t.Fatal(e)
	}
	var key factorysim.AnswerKey
	if e = json.Unmarshal(d.files["answer-key.json"], &key); e != nil {
		t.Fatal(e)
	}
	// Latest revision of each production delta, as the context builder folds them.
	latest := map[string]ingestion.Event{}
	for path, b := range d.files {
		if !strings.HasPrefix(path, "batches/") {
			continue
		}
		var batch ingestion.Batch
		if e = json.Unmarshal(b, &batch); e != nil {
			t.Fatal(e)
		}
		for _, ev := range batch.Events {
			if ev.Kind == "production_delta" && ev.Revision >= latest[ev.ExternalKey].Revision {
				latest[ev.ExternalKey] = ev
			}
		}
	}
	type window struct {
		key            string
		expected, good *big.Rat
	}
	windowOf := func(key string) window {
		t.Helper()
		var delta production.Delta
		if e := json.Unmarshal(latest[key].Payload, &delta); e != nil {
			t.Fatalf("%s: %v", key, e)
		}
		var capture plans.CaptureInput
		if e := json.Unmarshal(d.files["plans/"+key[:strings.LastIndex(key, "-")]+".json"], &capture); e != nil {
			t.Fatalf("%s plan: %v", key, e)
		}
		expected, e := production.ExpectedGoodUnits(capture.Plan.WithApproval("00000000-0000-4000-8000-000000000001"), delta.IntervalStart, delta.IntervalEnd)
		if e != nil {
			t.Fatalf("%s: %v", key, e)
		}
		good, ok := new(big.Rat).SetString(delta.GoodUnits)
		if !ok {
			t.Fatalf("%s good_units %q", key, delta.GoodUnits)
		}
		return window{key, expected, good}
	}
	qualifies := func(w window) bool {
		deficit := new(big.Rat).Sub(w.expected, w.good)
		if deficit.Sign() < 0 {
			deficit = new(big.Rat)
		}
		return production.WindowQualifies(w.expected, deficit)
	}
	// Whether the answer key's incident is a production shortfall that must open a case.
	shortfall := map[string]bool{"Throughput constraint": true, "Tool wear": true, "Late castings": true, "Changeover quality spike": true, "Late correction": false, "Threshold recovery": false}
	if len(key.Scenarios) != len(shortfall) {
		t.Fatalf("answer key has %d scenarios; classify each one here", len(key.Scenarios))
	}
	for _, s := range key.Scenarios {
		want, known := shortfall[s.Name]
		if !known {
			t.Fatalf("unclassified answer-key scenario %q", s.Name)
		}
		for _, k := range s.EvidenceKeys {
			w := windowOf(k)
			if got := qualifies(w); got != want {
				t.Errorf("%s %s: expected %s, actual %s, qualifies = %v; want %v", s.Name, k, w.expected.FloatString(6), w.good.FloatString(0), got, want)
			}
		}
	}
	// Near-threshold negatives on the changeover line: an on-target window, and the q34
	// window with a true loss of 4 units (28 planned, 24 good), must not qualify.
	onTarget := windowOf("d00-l3-q33")
	if onTarget.good.Cmp(big.NewRat(28, 1)) != 0 || qualifies(onTarget) {
		t.Errorf("on-target d00-l3-q33 good %s expected %s must not qualify", onTarget.good.FloatString(0), onTarget.expected.FloatString(6))
	}
	fourShort := windowOf("d00-l3-q34")
	fourShort.good = big.NewRat(24, 1)
	if qualifies(fourShort) {
		t.Errorf("4-unit loss against expected %s must not qualify", fourShort.expected.FloatString(6))
	}
}
