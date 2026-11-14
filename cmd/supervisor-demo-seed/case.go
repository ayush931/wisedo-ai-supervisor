package main

import (
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/cases"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/production"
	"github.com/google/uuid"
)

// seedCase opens one synthetic production-shortfall case through the real WIS-272/273 path: an
// approved plan, telemetry intake, the fold, two READY context windows and the worker's case
// evaluator. The line entity is stable; if a case already exists for it, nothing is written.
// Otherwise the order, plan, source and contexts are fresh for this run, so a rerun after a
// partial failure starts clean instead of colliding with half-written telemetry.
func (d *demo) seedCase() (string, error) {
	register := d.id("source:asset-register")
	if err := d.call(ownerUsername, "POST", "/sources", "demo-seed:case:register", "0",
		map[string]string{"source_id": register, "kind": "manual", "title": "Asset and order register", "text": "Bracket line 1 and its open orders"}, nil, 200, 201); err != nil {
		return "", err
	}
	type entity struct {
		EntityID string `json:"entity_id"`
		Revision int64  `json:"revision"`
	}
	var line entity
	if err := d.call(ownerUsername, "POST", "/entities", "demo-seed:case:line", "0",
		map[string]any{"entity_id": d.id("entity:bracket-line-1"), "type": "asset", "asset_kind": "line", "display_name": "Bracket line 1", "source_id": register, "source_revision": 1}, &line, 200, 201); err != nil {
		return "", err
	}
	if id, err := d.newestCase(line.EntityID); err != nil || id != "" {
		return id, err
	}

	run := uuid.NewString()
	var order entity
	if err := d.call(ownerUsername, "POST", "/entities", "demo-seed:case:order:"+run, "0",
		map[string]any{"entity_id": uuid.NewString(), "type": "order", "display_name": "Order A-100-" + strings.ToUpper(run[:4]), "source_id": register, "source_revision": 1}, &order, 200, 201); err != nil {
		return "", err
	}
	var inventory struct {
		Revision int64 `json:"revision"`
	}
	if err := d.call(ownerUsername, "GET", "/knowledge-inventory", "", "", nil, &inventory, 200); err != nil {
		return "", err
	}
	var snapshot struct {
		SnapshotID string `json:"snapshot_id"`
	}
	if err := d.call(ownerUsername, "POST", "/process-snapshots", "demo-seed:case:snapshot:"+run, itoa(inventory.Revision),
		map[string]string{"snapshot_id": uuid.NewString(), "reason": "Approved process cut for the current shift"}, &snapshot, 200, 201); err != nil {
		return "", err
	}

	// Two completed 15-minute windows ending at least 15 minutes ago. The plan expects 100 good
	// units per window (1 unit/s for 100 productive seconds); 80 were delivered in each, a
	// deficit of 20 twice, which meets the two-window shortfall rule.
	end := d.now().UTC().Truncate(15 * time.Minute).Add(-15 * time.Minute)
	windows := []time.Time{end.Add(-30 * time.Minute), end.Add(-15 * time.Minute), end}
	stamp := func(t time.Time) string { return t.Format(time.RFC3339) }
	breaks := []production.Interval{}
	for i := 0; i < 2; i++ {
		breaks = append(breaks, production.Interval{Start: stamp(windows[i].Add(100 * time.Second)), End: stamp(windows[i+1])})
	}
	var schedule struct {
		Revision      int64 `json:"revision"`
		StateRevision int64 `json:"state_revision"`
	}
	scheduleID, scheduleText := uuid.NewString(), "Order A-100 on bracket line 1 at one unit per second"
	if err := d.call(ownerUsername, "POST", "/sources", "demo-seed:case:schedule:"+run, "0",
		map[string]string{"source_id": scheduleID, "kind": "manual", "title": "Bracket line 1 shift plan", "text": scheduleText}, &schedule, 200, 201); err != nil {
		return "", err
	}
	var span struct {
		ID string `json:"id"`
	}
	if err := d.call(ownerUsername, "POST", "/sources/"+scheduleID+"/spans", "demo-seed:case:span:"+run, itoa(schedule.StateRevision),
		map[string]any{"revision": schedule.Revision, "start_char": 0, "end_char": len(scheduleText)}, &span, 200, 201); err != nil {
		return "", err
	}
	plan := production.PlanBody{OrderID: order.EntityID, AssetID: line.EntityID, WindowStart: stamp(windows[0]), WindowEnd: stamp(windows[2]), PlanRevision: 1, Unit: "count",
		RateSegments: []production.RateSegment{{Start: stamp(windows[0]), End: stamp(windows[2]), TargetGoodUnitsPerSecond: "1"}}, PlannedBreaks: breaks}
	var captured struct {
		ID string `json:"id"`
	}
	if err := d.call("shift-lead", "POST", "/plan-revisions", "demo-seed:case:plan:"+run, "", map[string]any{"plan": plan, "source_span_ids": []string{span.ID}}, &captured, 201); err != nil {
		return "", err
	}
	if err := d.call("shift-approver", "POST", "/plan-revisions/"+captured.ID+"/reviews", "demo-seed:case:plan-review:"+run, "0",
		map[string]string{"decision": "approve", "reason": "Matches the shift plan"}, nil, 200); err != nil {
		return "", err
	}

	sourceID := uuid.NewString()
	src := "/operational-sources/" + sourceID
	if err := d.call(ownerUsername, "POST", "/operational-sources", "demo-seed:case:source:"+run, "0",
		map[string]any{"source_id": sourceID, "type": "event_api", "authority_scope": "production_delta,heartbeat,watermark", "enabled": true, "access_policy_revision": 1, "freshness_seconds": 900, "clock_skew_seconds": 30}, nil, 200); err != nil {
		return "", err
	}
	if err := d.call(ownerUsername, "POST", src+"/mappings", "demo-seed:case:mappings:"+run, "1", map[string]any{"mappings": []map[string]any{
		{"external_type": "asset", "external_key": "bracket-line-1", "entity_id": line.EntityID, "entity_revision": line.Revision},
		{"external_type": "order", "external_key": "order-a-100", "entity_id": order.EntityID, "entity_revision": order.Revision},
	}}, nil, 200); err != nil {
		return "", err
	}
	if err := d.call("shift-approver", "POST", src+"/reviews", "demo-seed:case:source-review:"+run, "2",
		map[string]string{"verdict": "approved", "reason": "Telemetry scope verified"}, nil, 200); err != nil {
		return "", err
	}
	sequence := int64(0)
	send := func(events ...map[string]any) error {
		for _, e := range events {
			sequence++
			e["source_sequence"], e["revision"] = sequence, 1
		}
		return d.call(ownerUsername, "POST", src+"/batches", "demo-seed:case:batch:"+run+":"+itoa(sequence),
			"", map[string]any{"schema_version": 1, "source_id": sourceID, "source_revision": 2, "mapping_revision": 2, "events": events}, nil, 202)
	}
	folder := ingestion.New(d.runtime.DB)
	for i := 0; i < 2; i++ {
		delta, _ := json.Marshal(production.Delta{OrderID: order.EntityID, AssetID: line.EntityID, IntervalStart: stamp(windows[i]), IntervalEnd: stamp(windows[i+1]), GoodUnits: "80", Unit: "count"})
		now := d.now().UTC().Format(time.RFC3339Nano)
		if err := send(map[string]any{"external_key": "delta-" + itoa(int64(i)), "kind": "production_delta", "event_at": stamp(windows[i+1]), "asset_external_key": "bracket-line-1", "order_external_key": "order-a-100", "payload": json.RawMessage(delta)},
			map[string]any{"external_key": "heartbeat-" + itoa(int64(i)), "kind": "heartbeat", "event_at": now, "payload": map[string]any{"observed_at": now}}); err != nil {
			return "", err
		}
		if err := send(map[string]any{"external_key": "watermark-" + itoa(int64(i)), "kind": "watermark", "event_at": now, "payload": map[string]any{"complete_through": stamp(windows[i+1]), "through_sequence": sequence}}); err != nil {
			return "", err
		}
		if _, err := folder.Fold(d.ctx, ingestion.SourceRef{TenantID: d.tenant, SiteID: d.site, SourceID: sourceID}, 500); err != nil {
			return "", errors.New("demo telemetry fold failed")
		}
		var context struct {
			Readiness string `json:"readiness"`
		}
		if err := d.call(ownerUsername, "POST", "/contexts", "demo-seed:case:context:"+run+":"+itoa(int64(i)), "",
			map[string]string{"order_id": order.EntityID, "asset_id": line.EntityID, "window_start": stamp(windows[i]), "window_end": stamp(windows[i+1]), "process_snapshot_id": snapshot.SnapshotID}, &context, 201); err != nil {
			return "", err
		}
		if context.Readiness != "READY" {
			return "", errors.New("demo context window " + itoa(int64(i)) + " is " + context.Readiness)
		}
	}
	// WIS-635: what a maintenance investigation needs, through the public API. A supervisor
	// confirms operator-1 as the line's maintenance assignee, and a separately reviewed
	// machine_state feed records an unexplained stoppage of the line inside the second window
	// (machine-fault evidence). The case itself is unchanged: contexts select production deltas.
	if err := d.call("shift-approver", "POST", "/maintenance-roster/decisions", "demo-seed:case:roster:"+run, "0",
		map[string]any{"line_id": line.EntityID, "assignee_subject": d.subjects["operator-1"], "decision": "confirm", "reason": "Bracket line 1 maintenance on call"}, nil, 200, 201); err != nil {
		return "", err
	}
	machineID := uuid.NewString()
	machine := "/operational-sources/" + machineID
	if err := d.call(ownerUsername, "POST", "/operational-sources", "demo-seed:case:machine-source:"+run, "0",
		map[string]any{"source_id": machineID, "type": "event_api", "authority_scope": "machine_state,heartbeat", "enabled": true, "access_policy_revision": 1, "freshness_seconds": 900, "clock_skew_seconds": 30}, nil, 200); err != nil {
		return "", err
	}
	if err := d.call(ownerUsername, "POST", machine+"/mappings", "demo-seed:case:machine-mappings:"+run, "1", map[string]any{"mappings": []map[string]any{
		{"external_type": "asset", "external_key": "bracket-line-1", "entity_id": line.EntityID, "entity_revision": line.Revision},
	}}, nil, 200); err != nil {
		return "", err
	}
	if err := d.call("shift-approver", "POST", machine+"/reviews", "demo-seed:case:machine-review:"+run, "2",
		map[string]string{"verdict": "approved", "reason": "Machine state feed verified"}, nil, 200); err != nil {
		return "", err
	}
	now := d.now().UTC().Format(time.RFC3339Nano)
	if err := d.call(ownerUsername, "POST", machine+"/batches", "demo-seed:case:machine-batch:"+run, "", map[string]any{"schema_version": 1, "source_id": machineID, "source_revision": 2, "mapping_revision": 2, "events": []map[string]any{
		{"external_key": "stop-1", "kind": "machine_state", "event_at": stamp(windows[1].Add(5 * time.Minute)), "asset_external_key": "bracket-line-1", "payload": map[string]any{"state": "stopped"}, "source_sequence": 1, "revision": 1},
		{"external_key": "heartbeat-0", "kind": "heartbeat", "event_at": now, "payload": map[string]any{"observed_at": now}, "source_sequence": 2, "revision": 1},
	}}, nil, 202); err != nil {
		return "", err
	}
	if _, err := folder.Fold(d.ctx, ingestion.SourceRef{TenantID: d.tenant, SiteID: d.site, SourceID: machineID}, 500); err != nil {
		return "", errors.New("demo machine fold failed")
	}
	// The worker's evaluator opens the case, exactly as it would on a running worker tick.
	if _, err := cases.NewService(d.runtime.DB).EvaluateCreatedContexts(d.ctx, d.tenant, cases.EvaluatorActor, 100); err != nil {
		return "", errors.New("demo case evaluation failed")
	}
	if id, err := d.newestCase(line.EntityID); err != nil || id != "" {
		return id, err
	}
	return "", errors.New("the demo shortfall did not open a case")
}

// newestCase walks every page of the site's cases (ordered by ID, so a line's episodes can be on
// any page) and returns the line's latest episode, or "" when it has none.
func (d *demo) newestCase(line string) (string, error) {
	id, episode, cursor := "", int64(0), ""
	for n := 0; n < 1000; n++ {
		var page cases.PageV1
		path := "/cases?limit=100"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		if err := d.call("shift-lead", "GET", path, "", "", nil, &page, 200); err != nil {
			return "", err
		}
		for _, c := range page.Items {
			if c.LineID == line && c.Episode > episode {
				id, episode = c.ID, c.Episode
			}
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			return id, nil
		}
		cursor = *page.NextCursor
	}
	return "", errors.New("the case list did not end")
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
