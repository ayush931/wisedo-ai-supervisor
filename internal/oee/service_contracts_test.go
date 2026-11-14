package oee

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	svcTenant = "0a0a0a0a-0a0a-4a0a-8a0a-0a0a0a0a0a0a"
	svcSite   = "1a1a1a1a-1a1a-4a1a-8a1a-1a1a1a1a1a1a"
)

func loadServiceFixture(t *testing.T, name string, v any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "service", name))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, v); err != nil {
		t.Fatal(err)
	}
}

func svcPolicy(t *testing.T) (OeePolicyBodyV1, OeePolicyRefV1) {
	t.Helper()
	var body OeePolicyBodyV1
	loadServiceFixture(t, "policy.json", &body)
	canonicalBody, digest, err := body.Canonical(svcTenant, svcSite)
	if err != nil {
		t.Fatal(err)
	}
	return canonicalBody, OeePolicyRefV1{PolicyID: "12121212-1212-4212-8212-121212121212", PolicyRevisionID: "13131313-1313-4313-8313-131313131313", Revision: 1, Digest: digest}
}

// sealDataset sets the dataset's provenance digest, as its assembler does.
func sealDataset(t *testing.T, d *OeeDatasetV1) {
	t.Helper()
	digest, err := DatasetProvenanceDigest(*d)
	if err != nil {
		t.Fatal(err)
	}
	d.ProvenanceDigest = digest
}

func svcDataset(t *testing.T) OeeDatasetV1 {
	t.Helper()
	var d OeeDatasetV1
	loadServiceFixture(t, "dataset_adr0005.json", &d)
	sealDataset(t, &d)
	return d
}

// ADR 0005 Sec. 9 reference shift through the service contract: 480 min calendar, 30 min
// planned breaks, 40 min breakdown plus a 20 min changeover, 19,500 total, 300 scrap, 1 s/unit.
// Expected values are the ADR's worked example. The evidence is CANDIDATE only (ADR 0002
// Sec. 18) and identical input gives identical evidence (ADR 0002 Sec. 12 deterministic replay).
func TestCalculateDatasetADR0005Golden(t *testing.T) {
	body, ref := svcPolicy(t)
	d := svcDataset(t)
	ev, err := CalculateDataset(svcTenant, svcSite, d, ref, body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Status != StatusReady || ev.KPIs.Availability.Value != "0.866667" || ev.KPIs.Performance.Value != "0.833333" ||
		ev.KPIs.Quality.Value != "0.984615" || ev.KPIs.OEE.Value != "0.711111" || ev.KPIs.TEEP.Value != "0.666667" {
		t.Fatalf("golden: %s %v %+v", ev.Status, ev.ReasonCodes, ev.KPIs)
	}
	if ev.Durations.PlannedProduction != "27000" || ev.Durations.Run != "23400" || ev.Timeline.CoverageRatio != "1" {
		t.Fatalf("durations: %+v %+v", ev.Durations, ev.Timeline)
	}
	if ev.TruthState != TruthStateCandidate || ev.Policy != ref || ev.Digests.Dataset != d.ProvenanceDigest {
		t.Fatalf("provenance: %+v %+v", ev.Policy, ev.Digests)
	}
	again, _ := CalculateDataset(svcTenant, svcSite, d, ref, body)
	a, _ := json.Marshal(ev)
	b, _ := json.Marshal(again)
	if string(a) != string(b) || len(ev.Digests.Timeline) != 64 || len(ev.Digests.Calculation) != 64 {
		t.Fatal("not deterministic or digests missing")
	}
}

// oee_run_source keeps per source only its identity, snapshot or watermark, a contribution
// digest and a fact count, never the facts (ADR 0002 Sec. 4.2, 6.2).
func TestRunSourcesAreMinimized(t *testing.T) {
	body, ref := svcPolicy(t)
	ev, err := CalculateDataset(svcTenant, svcSite, svcDataset(t), ref, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Sources) != 1 {
		t.Fatalf("sources: %+v", ev.Sources)
	}
	s := ev.Sources[0]
	// 4 state facts, 1 count and 1 ideal cycle from the single declared source.
	if s.SourceKey != "plc" || s.OwnerService != "mes" || s.SnapshotRef != "snapshot-1" || s.FactCount != 6 || !evidenceSHA256.MatchString(s.ContributionSHA256) {
		t.Fatalf("run source: %+v", s)
	}
	raw, _ := json.Marshal(ev)
	for _, leaked := range []string{"payload_digest", "run-1", "shift-total"} {
		if strings.Contains(string(raw), leaked) {
			t.Fatalf("evidence carries source fact content %q", leaked)
		}
	}
}

// ADR 0002 Sec. 5 stage 2 and Sec. 12: source schema, provenance and digest violations and
// strict time/decimal rules reject the dataset before any side effect.
func TestVerifyDatasetRejectsContractViolations(t *testing.T) {
	for name, change := range map[string]func(*OeeDatasetV1){
		"schema id":            func(d *OeeDatasetV1) { d.SchemaID = "other" },
		"asset id":             func(d *OeeDatasetV1) { d.AssetID = "press-1" },
		"no sources":           func(d *OeeDatasetV1) { d.Sources = nil },
		"duplicate source":     func(d *OeeDatasetV1) { d.Sources = append(d.Sources, d.Sources[0]) },
		"missing owner":        func(d *OeeDatasetV1) { d.Sources[0].OwnerService = "" },
		"missing revision":     func(d *OeeDatasetV1) { d.Sources[0].SourceRevision = "" },
		"unknown mode":         func(d *OeeDatasetV1) { d.Sources[0].ConsistencyMode = "LIVE" },
		"snapshot without ref": func(d *OeeDatasetV1) { d.Sources[0].SnapshotRef = "" },
		"undeclared fact":      func(d *OeeDatasetV1) { d.Facts[0].SourceKey = "historian" },
		"undeclared count":     func(d *OeeDatasetV1) { d.Counts[0].SourceKey = "historian" },
		"undeclared cycle":     func(d *OeeDatasetV1) { d.IdealCycles[0].SourceKey = "historian" },
		"naive window":         func(d *OeeDatasetV1) { d.WindowStart = "2026-10-01T08:00:00" },
		"reversed window":      func(d *OeeDatasetV1) { d.WindowEnd = d.WindowStart },
	} {
		d := svcDataset(t)
		change(&d)
		sealDataset(t, &d)
		if err := VerifyDataset(d); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A dataset changed after its digest was computed fails provenance verification.
	d := svcDataset(t)
	d.Counts[0].Scrap = "0"
	if err := VerifyDataset(d); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("tampered dataset: %v", err)
	}
	d = svcDataset(t)
	d.Counts = make([]OeeCountV1, MaxRequestCounts+1)
	if err := VerifyDataset(d); !errors.Is(err, ErrTimelineLimit) {
		t.Fatalf("count limit: %v", err)
	}
	// Facts and exclusions share the timeline's limit, and the window has a hard ceiling;
	// both are rejected at acceptance rather than failing a reserved run.
	d = svcDataset(t)
	d.Facts = make([]OeeFactV1, MaxRequestFacts)
	if err := VerifyDataset(d); !errors.Is(err, ErrTimelineLimit) {
		t.Fatalf("combined fact and exclusion limit: %v", err)
	}
	d = svcDataset(t)
	d.WindowEnd = "2026-11-02T08:00:00Z"
	sealDataset(t, &d)
	if err := VerifyDataset(d); !errors.Is(err, ErrTimelineLimit) {
		t.Fatalf("window ceiling: %v", err)
	}
}

// CalculateDataset re-verifies the dataset itself (the element rules are covered case by case
// in TestVerifyDatasetRejectsMalformedElements, which shares verifyAndPrepare) and validates
// the policy body.
func TestCalculateDatasetRejectsMalformedFacts(t *testing.T) {
	body, ref := svcPolicy(t)
	d := svcDataset(t)
	d.Facts[0].Start = "2026-10-01T08:00:00"
	sealDataset(t, &d)
	if _, err := CalculateDataset(svcTenant, svcSite, d, ref, body); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("naive fact time: %v", err)
	}
	tampered := svcDataset(t)
	tampered.Counts[0].Scrap = "0"
	if _, err := CalculateDataset(svcTenant, svcSite, tampered, ref, body); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("dataset changed after its provenance digest: %v", err)
	}
	bad := body
	bad.RoundingMode = "BANKERS"
	if _, err := CalculateDataset(svcTenant, svcSite, svcDataset(t), ref, bad); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("bad policy: %v", err)
	}
}

// ADR 0005 Sec. 2 and 11: a microstop is a short stop *event*. A breakdown that started before
// the window and is clipped by it keeps its true length, so it stays an availability loss;
// a genuinely short stop that merely crosses the edge is still a microstop.
func TestStopCrossingWindowEdgeIsClassifiedByTrueLength(t *testing.T) {
	body, ref := svcPolicy(t)
	shift := func(downStart, downEnd, runStart string) OeeDatasetV1 {
		return func() OeeDatasetV1 {
			d := svcDataset(t)
			d.WindowStart, d.WindowEnd = "2026-10-01T08:00:00Z", "2026-10-01T09:00:00Z"
			d.Exclusions = nil
			d.Facts = []OeeFactV1{
				{EventID: "down", SourceKey: "plc", State: StateUnplannedDowntime, Start: downStart, End: downEnd, EventAt: downStart, RecordedAt: downEnd, Revision: 1, PayloadDigest: strings.Repeat("1", 64)},
				{EventID: "run", SourceKey: "plc", State: StateRunning, Start: runStart, End: "2026-10-01T09:00:00Z", EventAt: runStart, RecordedAt: "2026-10-01T09:00:00Z", Revision: 1, PayloadDigest: strings.Repeat("2", 64)},
			}
			d.Counts[0].Start, d.Counts[0].End, d.Counts[0].Total, d.Counts[0].Scrap = d.WindowStart, d.WindowEnd, "3000", "0"
			sealDataset(t, &d)
			return d
		}()
	}

	// A two-hour breakdown (06:02-08:02) clipped to its last two minutes in the window.
	ev, err := CalculateDataset(svcTenant, svcSite, shift("2026-10-01T06:02:00Z", "2026-10-01T08:02:00Z", "2026-10-01T08:02:00Z"), ref, body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Durations.Stop != "120" || ev.Durations.Microstop != "0" || ev.KPIs.Availability.Value != "0.966667" {
		t.Fatalf("clipped breakdown: stop %s microstop %s availability %s", ev.Durations.Stop, ev.Durations.Microstop, ev.KPIs.Availability.Value)
	}

	// A one-minute stop (07:59:30-08:00:30) crossing the edge is a microstop: 30 s in window.
	ev, err = CalculateDataset(svcTenant, svcSite, shift("2026-10-01T07:59:30Z", "2026-10-01T08:00:30Z", "2026-10-01T08:00:30Z"), ref, body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Durations.Stop != "0" || ev.Durations.Microstop != "30" || ev.KPIs.Availability.Value != "1.000000" {
		t.Fatalf("short edge stop: stop %s microstop %s availability %s", ev.Durations.Stop, ev.Durations.Microstop, ev.KPIs.Availability.Value)
	}
}

// ADR 0002 Sec. 5 stage 2 and Sec. 9: element-level violations are part of contract
// verification, so acceptance rejects them before a run is reserved, not later at execution.
func TestVerifyDatasetRejectsMalformedElements(t *testing.T) {
	for name, change := range map[string]func(*OeeDatasetV1){
		"naive fact time":        func(d *OeeDatasetV1) { d.Facts[0].Start = "2026-10-01T08:00:00" },
		"reversed fact":          func(d *OeeDatasetV1) { d.Facts[0].End = d.Facts[0].Start },
		"fact payload digest":    func(d *OeeDatasetV1) { d.Facts[0].PayloadDigest = "x" },
		"unbounded event id":     func(d *OeeDatasetV1) { d.Facts[0].EventID = strings.Repeat("e", maxWireIDLength+1) },
		"exclusion state":        func(d *OeeDatasetV1) { d.Exclusions[0].State = StateRunning },
		"missing exclusion id":   func(d *OeeDatasetV1) { d.Exclusions[0].ExclusionID = "" },
		"count decimal":          func(d *OeeDatasetV1) { d.Counts[0].Total = "19500.0" },
		"missing count unit":     func(d *OeeDatasetV1) { d.Counts[0].Unit = "" },
		"cycle decimal":          func(d *OeeDatasetV1) { d.IdealCycles[0].SecondsPerUnit = "-1" },
		"half cycle range":       func(d *OeeDatasetV1) { d.IdealCycles[0].EffectiveStart = "2026-10-01T08:00:00Z" },
		"missing cycle product":  func(d *OeeDatasetV1) { d.IdealCycles[0].ProductID = "" },
		"output-only fact state": func(d *OeeDatasetV1) { d.Facts[0].State = StateGap },
		"fact revision 0":        func(d *OeeDatasetV1) { d.Facts[0].Revision = 0 },
		"exclusion revision 0":   func(d *OeeDatasetV1) { d.Exclusions[0].Revision = 0 },
		"count outside window":   func(d *OeeDatasetV1) { d.Counts[0].End = "2026-10-01T17:00:00Z" },
		"repeated contribution":  func(d *OeeDatasetV1) { d.Counts = append(d.Counts, d.Counts[0]) },
		"padded routing id":      func(d *OeeDatasetV1) { d.Counts[0].RoutingID = " R1" },
		"padded snapshot ref":    func(d *OeeDatasetV1) { d.Sources[0].SnapshotRef = "snapshot-1 " },
		"reserved source key": func(d *OeeDatasetV1) {
			d.Sources[0].SourceKey = calendarSource
			for i := range d.Facts {
				d.Facts[i].SourceKey = calendarSource
			}
			d.Counts[0].SourceKey, d.IdealCycles[0].SourceKey = calendarSource, calendarSource
		},
	} {
		d := svcDataset(t)
		change(&d)
		sealDataset(t, &d)
		if err := VerifyDataset(d); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Rule 04: the evidence digest covers the schema-ordered body plus schema version and
// tenant/site, excludes generated IDs and the seal time, and changes when the body changes.
func TestEvidenceDigestIsDeterministicAndScoped(t *testing.T) {
	body, ref := svcPolicy(t)
	ev, err := CalculateDataset(svcTenant, svcSite, svcDataset(t), ref, body)
	if err != nil {
		t.Fatal(err)
	}
	first, second := ev, ev
	first.EvidenceID, first.RunID, first.SealedAt = "11111111-1111-4111-8111-111111111111", "21111111-1111-4111-8111-111111111111", "2026-10-01T16:00:01Z"
	second.EvidenceID, second.RunID, second.SealedAt = "31111111-1111-4111-8111-111111111111", "41111111-1111-4111-8111-111111111111", "2026-10-02T09:30:00Z"
	a, err := EvidenceDigest(svcTenant, svcSite, first)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := EvidenceDigest(svcTenant, svcSite, second)
	if a != b || !evidenceSHA256.MatchString(a) {
		t.Fatalf("generated identifiers or seal time changed the digest: %s %s", a, b)
	}
	otherTenant, _ := EvidenceDigest("2b2b2b2b-2b2b-4b2b-8b2b-2b2b2b2b2b2b", svcSite, first)
	otherSite, _ := EvidenceDigest(svcTenant, "3c3c3c3c-3c3c-4c3c-8c3c-3c3c3c3c3c3c", first)
	if otherTenant == a || otherSite == a {
		t.Fatal("digest is not bound to tenant and site")
	}
	changed := first
	changed.KPIs.OEE.Value = "0.700000"
	if c, _ := EvidenceDigest(svcTenant, svcSite, changed); c == a {
		t.Fatal("a changed result kept the same digest")
	}
}
