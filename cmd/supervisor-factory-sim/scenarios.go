package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/factorysim"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/oee"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/operational"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/production"
)

type fixtureProblem struct {
	Status             int
	Code, Method, Path string
}

func (p *fixtureProblem) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d %s", p.Method, p.Path, p.Status, p.Code)
}

type scenarioObservation struct {
	Status    int    `json:"http_status,omitempty"`
	Code      string `json:"code,omitempty"`
	Readiness string `json:"readiness,omitempty"`
	Evidence  any    `json:"evidence,omitempty"`
}
type scenarioResult struct {
	Expected factorysim.ScenarioOutcome `json:"expected"`
	Observed scenarioObservation        `json:"observed"`
	Passed   bool                       `json:"passed"`
	Failure  string                     `json:"failure,omitempty"`
}
type scenarioReport struct {
	SchemaVersion   int              `json:"schema_version"`
	Seed            int64            `json:"seed"`
	StartedAt       time.Time        `json:"started_at"`
	AnswerKeySHA256 string           `json:"answer_key_sha256"`
	Results         []scenarioResult `json:"results"`
	// Facility self-checks test simulator code, not the Supervisor; they are outside AC2.
	FacilityChecks []scenarioResult `json:"facility_model_checks"`
	ProductGaps    []string         `json:"product_gaps"`
	Excluded       []string         `json:"excluded"`
	Passed         bool             `json:"passed"`
}

// productGaps are expectations of the WIS-709 issue that no server rule implements yet. They are
// reported, never gated through a model.
var productGaps = []string{
	"contradictory-interview: the issue expects a contradiction to become needs_input. No server rule produces that for chat. The server withdraws both conflicting values from the approved snapshot (reviewEligibility), and chat's needs_input/MISSING_INPUT is the model's own status (internal/agent/chat.go). CONTRADICTORY_EVIDENCE exists only for case runs, and only when the model itself lists contradictions (internal/agent/runtime.go decide). A server rule that answers needs_input when the question's fact is under an open conflict would be a product change.",
}

type scenarioRunner struct {
	l      *loader
	d      dataset
	prefix string
}

func (s scenarioRunner) id(name string) string {
	return factorysim.ID(s.d.manifest.Seed, s.prefix+"/"+name)
}
func (s scenarioRunner) request(ctx context.Context, role, method, path, key, match, media string, body []byte, out any) (scenarioObservation, error) {
	status, err := s.l.request(ctx, role, method, "/v1/sites/"+factorysim.SiteID+path, s.prefix+key, match, media, body, out)
	o := scenarioObservation{Status: status}
	var p *fixtureProblem
	if errors.As(err, &p) {
		o.Code = p.Code
		return o, nil
	}
	return o, err
}

// runScenarios runs one case or all of them; exclude (comma-separated IDs, only with "all") leaves
// cases out of this run, recorded in the report, so a merged report can take them from another run.
func runScenarios(ctx context.Context, l *loader, d dataset, selection, exclude, dir string, out io.Writer) error {
	catalog, facility := factorysim.Scenarios(), factorysim.FacilityModelChecks()
	known := func(id string) bool {
		return slices.ContainsFunc(append(slices.Clone(catalog), facility...), func(c factorysim.ScenarioOutcome) bool { return c.ID == id })
	}
	if selection != "all" && !known(selection) {
		return errors.New("unknown --scenario (see scenario-answer-key.json)")
	}
	excluded := []string{}
	if exclude != "" {
		excluded = strings.Split(exclude, ",")
	}
	for _, id := range excluded {
		if selection != "all" || !known(id) {
			return errors.New("--exclude needs --scenario all and known case IDs")
		}
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	key := wire(map[string]any{"product_cases": catalog, "facility_model_checks": facility})
	if err := os.WriteFile(filepath.Join(dir, "scenario-answer-key.json"), key, 0600); err != nil {
		return err
	}
	if err := l.identify(ctx); err != nil {
		return err
	}
	report := scenarioReport{SchemaVersion: 1, Seed: d.manifest.Seed, StartedAt: time.Now().UTC(), AnswerKeySHA256: sha(key), Passed: true, Results: []scenarioResult{}, FacilityChecks: []scenarioResult{}, ProductGaps: productGaps, Excluded: excluded}
	for i, c := range append(slices.Clone(catalog), facility...) {
		if selection != "all" && selection != c.ID || slices.Contains(excluded, c.ID) {
			continue
		}
		s := scenarioRunner{l: l, d: d, prefix: "edge-" + report.StartedAt.Format("20060102T150405.000000000") + "-" + c.ID}
		var observation scenarioObservation
		var err error
		if c.Layer != "http" {
			observation, err = scenarioKernel(c.ID, d)
		} else {
			observation, err = s.execute(ctx, c.ID)
		}
		result := scenarioResult{Expected: c, Observed: observation, Passed: err == nil && observation.Status == c.Status && observation.Code == c.Code && observation.Readiness == c.Readiness}
		if err != nil {
			result.Failure = err.Error()
		} else if !result.Passed {
			result.Failure = "observed outcome differs from frozen answer key"
		}
		if i < len(catalog) {
			report.Results = append(report.Results, result)
		} else {
			report.FacilityChecks = append(report.FacilityChecks, result)
		}
		report.Passed = report.Passed && result.Passed
		if err = os.WriteFile(filepath.Join(dir, "scenario-report.json"), wire(report), 0600); err != nil {
			return err
		}
		fmt.Fprintf(out, "SYNTHETIC scenario %s passed=%t %s\n", c.ID, result.Passed, result.Failure)
	}
	if !report.Passed {
		return errors.New("edge catalogue has failures; see scenario-report.json (expectations unchanged)")
	}
	return nil
}
func (s scenarioRunner) execute(ctx context.Context, id string) (scenarioObservation, error) {
	switch id {
	case "duplicate-batch", "out-of-order-batch", "late-batch", "clock-skew", "liveness-gap", "counter-reset":
		return s.ingest(ctx, id)
	case "encrypted-pdf", "active-pdf":
		return s.pdf(ctx, id)
	case "self-approval", "overnight-shift", "late-count-correction", "membership-revoked":
		return s.shift(ctx, id)
	case "cross-tenant", "expired-session":
		return s.identity(ctx, id)
	case "stale-snapshot", "revoked-document", "contradictory-interview":
		return s.knowledge(ctx, id)
	}
	return scenarioObservation{}, errors.New("unimplemented scenario")
}
func (s scenarioRunner) ingest(ctx context.Context, id string) (scenarioObservation, error) {
	source := s.id("source")
	path := "/operational-sources/" + source
	cfg := operational.Config{SourceID: source, Type: "event_api", AuthorityScope: "machine_state,heartbeat,watermark", Enabled: true, AccessPolicyRevision: 1, FreshnessSeconds: 120, ClockSkewSeconds: 30}
	if id == "counter-reset" {
		cfg.AuthorityScope = "production_delta"
	}
	if id == "liveness-gap" {
		cfg.FreshnessSeconds = 1
	}
	if err := s.l.call(ctx, "owner", "/operational-sources", source, "0", cfg, nil); err != nil {
		return scenarioObservation{}, err
	}
	maps := operational.MappingsInput{Mappings: []operational.Mapping{{ExternalType: "asset", ExternalKey: "machine", EntityID: s.d.manifest.Lines[0].Stations[0].ID, EntityRevision: 1}}}
	if id == "counter-reset" {
		maps.Mappings = append(maps.Mappings, operational.Mapping{ExternalType: "order", ExternalKey: "order", EntityID: s.d.id("order/d00-l0"), EntityRevision: 1})
	}
	if err := s.l.call(ctx, "owner", path+"/mappings", source+"map", "1", maps, nil); err != nil {
		return scenarioObservation{}, err
	}
	if err := s.l.call(ctx, "approver", path+"/reviews", source+"review", "2", operational.ReviewInput{Verdict: "approved", Reason: "Scripted isolated edge fixture"}, nil); err != nil {
		return scenarioObservation{}, err
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	if id == "clock-skew" {
		at = at.Add(2 * time.Minute)
	}
	if id == "late-batch" {
		at = at.Add(-24 * time.Hour)
	}
	batch := ingestion.Batch{SchemaVersion: 1, SourceID: source, SourceRevision: 2, MappingRevision: 2, Events: []ingestion.Event{{ExternalKey: "edge-heartbeat", Revision: 1, Kind: "heartbeat", SourceSequence: 1, EventAt: at, Payload: wire(map[string]string{"observed_at": at.Format(time.RFC3339Nano)})}}}
	var normalized scenarioObservation
	if id == "counter-reset" {
		var err error
		normalized, err = scenarioKernel(id, s.d)
		if err != nil {
			return normalized, err
		}
		deltas := normalized.Evidence.([]oee.NormalizedCounterDelta)
		batch.Events[0].Kind = "production_delta"
		batch.Events[0].AssetExternalKey = "machine"
		batch.Events[0].OrderExternalKey = "order"
		batch.Events[0].Payload = wire(production.Delta{OrderID: s.d.id("order/d00-l0"), AssetID: maps.Mappings[0].EntityID, IntervalStart: stamp(at.Add(-time.Minute)), IntervalEnd: stamp(at), GoodUnits: deltas[0].DeltaUnits.RatString(), Unit: "count"})
	}
	if id == "late-batch" {
		batch.Events[0].Kind = "machine_state"
		batch.Events[0].AssetExternalKey = "machine"
		batch.Events[0].Payload = wire(factorysim.Observation{State: "running", Good: 3})
	}
	if id == "out-of-order-batch" {
		batch.Events[0].SourceSequence = 2
	}
	var receipt ingestion.Result
	observed, err := s.request(ctx, "owner", "POST", path+"/batches", "batch", "", "application/json", wire(batch), &receipt)
	if err != nil || observed.Status != 202 {
		return observed, err
	}
	if id == "duplicate-batch" {
		var replay ingestion.Result
		observed, err = s.request(ctx, "owner", "POST", path+"/batches", "batch", "", "application/json", wire(batch), &replay)
		if err != nil {
			return observed, err
		}
		if !replay.Replayed || replay.BatchID != receipt.BatchID || replay.LastSequence != 1 {
			return observed, errors.New("duplicate changed receipt or sequence")
		}
		observed.Evidence = map[string]any{"original": receipt, "replay": replay}
	}
	deadline := time.Now().Add(30 * time.Second)
	var live ingestion.Liveness
	for {
		if err = s.l.call(ctx, "owner", path+"/liveness", "", "", nil, &live); err != nil {
			return observed, err
		}
		if live.Ingest.ProcessedSequence != nil && *live.Ingest.ProcessedSequence == 1 {
			break
		}
		if time.Now().After(deadline) {
			return observed, errors.New("edge source did not fold")
		}
		if err = (factorysim.WallClock{}).WaitUntil(ctx, time.Now().Add(100*time.Millisecond)); err != nil {
			return observed, err
		}
	}
	if live.Ingest.AcceptedSequence != 1 || live.Completeness == nil || live.Completeness.QuarantinedCount != 0 {
		return observed, errors.New("unexpected edge event growth or quarantine")
	}
	if id == "counter-reset" {
		observed.Evidence = map[string]any{"normalization": normalized, "submitted": batch.Events[0], "receipt": receipt, "liveness": live}
		return observed, nil
	}
	if id == "late-batch" {
		if live.Liveness == nil || live.Liveness.LastEventAt == nil || !live.Liveness.LastEventAt.Equal(at) || !receipt.AcceptedAt.After(at.Add(23*time.Hour)) {
			return observed, errors.New("late event time not preserved")
		}
		observed.Evidence = map[string]any{"receipt": receipt, "liveness": live}
		return observed, nil
	}
	if id == "liveness-gap" {
		if err = (factorysim.WallClock{}).WaitUntil(ctx, time.Now().Add(1500*time.Millisecond)); err != nil {
			return observed, err
		}
		if err = s.l.call(ctx, "owner", path+"/liveness", "", "", nil, &live); err != nil {
			return observed, err
		}
	}
	if id == "clock-skew" || id == "liveness-gap" {
		observed.Status = 200
		observed.Readiness = live.Readiness
		want := "CLOCK_SUSPECT"
		if id == "liveness-gap" {
			want = "HEARTBEAT_STALE"
		}
		if slices.Contains(live.Reasons, want) {
			observed.Code = want
		} else {
			observed.Code = strings.Join(live.Reasons, ",")
		}
		observed.Evidence = live
	}
	return observed, nil
}
