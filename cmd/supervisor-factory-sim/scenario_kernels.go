package main

import (
	"errors"
	"math/big"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/factorysim"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/oee"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plantcontext"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/production"
	"github.com/google/uuid"
)

func scenarioKernel(id string, d dataset) (scenarioObservation, error) {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	o := scenarioObservation{}
	switch id {
	case "counter-reset":
		readings := []oee.CounterReading{{ReadingID: "before", CounterCode: "parts", RawValue: big.NewRat(100, 1), OccurredAt: at}, {ReadingID: "reset", CounterCode: "parts", RawValue: big.NewRat(3, 1), OccurredAt: at.Add(time.Minute), HasResetEvidence: true}}
		deltas, err := oee.ComputeMonotonicCounterDeltas(readings)
		if err != nil {
			return o, err
		}
		readings[1].HasResetEvidence = false
		_, err = oee.ComputeMonotonicCounterDeltas(readings)
		if len(deltas) != 1 || deltas[0].DeltaUnits.Cmp(big.NewRat(3, 1)) != 0 || !errors.Is(err, oee.ErrCounterDropNoEvidence) {
			return o, errors.New("counter reset result differs")
		}
		o.Code = "COUNTER_DROP_NO_EVIDENCE"
		o.Evidence = deltas
	case "zero-denominator":
		window, _ := oee.NewHalfOpenInterval(at, at.Add(time.Hour))
		result, err := oee.CalculateKPIs(oee.KPICalculationInput{Window: window, Timeline: oee.TimelineResolutionResult{Window: window, Segments: []oee.CanonicalFactSegment{{Interval: window, State: oee.StatePlannedBreak, EventID: "planned-break"}}, Readiness: oee.ReadinessDecision{Status: oee.StatusReady}}, Policy: oee.DefaultKPIPolicy(uuid.MustParse(d.id("edge-policy")))})
		if err != nil {
			return o, err
		}
		if !result.Availability.IsZeroDenom || result.Availability.Value != nil || result.Availability.Formatted != "" || result.Availability.Percentage != "" {
			return o, errors.New("zero denominator exposed a number")
		}
		o.Readiness = string(result.Readiness.Status)
		for _, code := range result.Readiness.ReasonCodes {
			if code == oee.ReasonZeroDenominator {
				o.Code = string(code)
			}
		}
		o.Evidence = result
	case "zero-planned-time", "missing-actuals":
		start, end := stamp(at), stamp(at.Add(15*time.Minute))
		heartbeat := at.Add(17 * time.Minute)
		source := d.id("edge-source")
		plan := production.PlanBody{OrderID: d.id("edge-order"), AssetID: d.id("edge-asset"), WindowStart: start, WindowEnd: end, PlanRevision: 1, Unit: "count", RateSegments: []production.RateSegment{{Start: start, End: end, TargetGoodUnitsPerSecond: "1"}}, PlannedBreaks: []production.Interval{}}
		if id == "zero-planned-time" {
			plan.PlannedBreaks = []production.Interval{{Start: start, End: end}}
		}
		input := plantcontext.Inputs{OrderID: plan.OrderID, AssetID: plan.AssetID, Window: plantcontext.Window{Start: start, End: end}, ProcessSnapshotID: d.id("edge-snapshot"), ProcessSnapshotSHA256: strings.Repeat("a", 64), Plan: &plantcontext.PlanInput{Ref: plantcontext.PlanRef{PlanRevisionID: d.id("edge-plan"), PlanRevision: 1, ApprovalID: d.id("edge-approval"), CanonicalPayloadDigest: strings.Repeat("b", 64)}, Body: plan}, EvaluatedAt: heartbeat.Add(time.Second), Sources: map[string]plantcontext.SourceInput{source: {SourceID: source, SourceRevision: 2, MappingRevision: 2, AccessPolicyRevision: 1, Approved: true, ScopeAllowsDeltas: true, FreshnessSeconds: 120, Progress: &plantcontext.ProgressInput{CompleteSequence: 2, ClockQuality: "ok", LastHeartbeatReceivedAt: &heartbeat, Watermark: &plantcontext.WatermarkInput{Ref: plantcontext.RecordRef{SourceID: source, SourceSequence: 2, Kind: "watermark", ExternalKey: "edge-watermark", Revision: 1, SHA256: strings.Repeat("c", 64)}, Payload: wire(map[string]any{"complete_through": end, "through_sequence": 1})}}}}}
		result, err := plantcontext.Assemble(factorysim.TenantID, factorysim.SiteID, input)
		if err != nil {
			return o, err
		}
		if result.ActualGoodUnits != nil {
			return o, errors.New("missing actual exposed a number")
		}
		o.Readiness = string(result.Readiness)
		o.Code = strings.Join(result.ReasonCodes, ",")
		o.Evidence = result
	case "overdue-gauge":
		register := factorysim.NewTools(d.manifest, at)
		if len(register.Gauges) == 0 {
			return o, errors.New("no gauges")
		}
		g := register.Gauges[0]
		o.Readiness, o.Code = g.Readiness(g.DueAt)
		o.Evidence = map[string]any{"gauge": g, "observation": "held: calibration overdue; no usable measurement"}
	default:
		return o, errors.New("unknown kernel scenario")
	}
	return o, nil
}
