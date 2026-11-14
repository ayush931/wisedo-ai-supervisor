package workstation

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
)

type suggestionRouting struct {
	id      string
	body    RoutingBodyV1
	step    int
	station string
}

// Read only the head revision and verify both its canonical body and normalized
// steps. Display names are never identifiers. No unbounded revision history read.
func currentRouting(ctx context.Context, tx *sql.Tx, sc Scope, id string) (RoutingBodyV1, error) {
	var b RoutingBodyV1
	var payload []byte
	var digest string
	var revision int64
	var version int
	err := tx.QueryRowContext(ctx, `SELECT h.current_revision,r.payload,r.canonical_payload_sha256,r.digest_schema_version FROM supervisor.routing_heads h JOIN supervisor.routing_revisions r ON (r.tenant_id,r.site_id,r.object_id,r.revision)=(h.tenant_id,h.site_id,h.id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.id=$3`, sc.TenantID, sc.SiteID, id).Scan(&revision, &payload, &digest, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	if err != nil {
		return b, err
	}
	if version != 1 || strictjson.Decode(payload, &b, limits.RequestBodyBytes) != nil {
		return b, errIntegrity
	}
	b, err = normalizeMasterBody("routing", b)
	if err != nil {
		return b, errIntegrity
	}
	if _, d, e := canonical.Envelope("RoutingBodyV1", 1, sc.TenantID, sc.SiteID, b); e != nil || d != digest {
		return b, errIntegrity
	}
	rows, err := tx.QueryContext(ctx, `SELECT step_number,workstation_id FROM supervisor.routing_steps WHERE tenant_id=$1 AND site_id=$2 AND routing_id=$3 AND routing_revision=$4 ORDER BY step_number LIMIT 257`, sc.TenantID, sc.SiteID, id, revision)
	if err != nil {
		return b, err
	}
	defer rows.Close()
	index := 0
	for rows.Next() {
		var step RoutingStepV1
		if err = rows.Scan(&step.StepNumber, &step.WorkstationID); err != nil {
			return b, err
		}
		if index >= len(b.Steps) || step != b.Steps[index] {
			return b, errIntegrity
		}
		index++
	}
	if err = rows.Err(); err != nil {
		return b, err
	}
	if index != len(b.Steps) {
		return b, errIntegrity
	}
	return b, nil
}

func validateJobRouting(ctx context.Context, tx *sql.Tx, sc Scope, o JobOrderBodyV1) error {
	if o.RoutingID == "" {
		return nil
	}
	b, err := currentRouting(ctx, tx, sc, o.RoutingID)
	if errors.Is(err, ErrNotFound) {
		return ErrInvalidJobOrder
	}
	if err != nil {
		return err
	}
	if !b.Active || b.ItemID != o.ItemID || *o.StepNumber > len(b.Steps) {
		return ErrInvalidJobOrder
	}
	return nil
}

func resolveSuggestionRouting(ctx context.Context, tx *sql.Tx, sc Scope, line string, o JobOrderBodyV1) (suggestionRouting, string, error) {
	out := suggestionRouting{id: o.RoutingID}
	if o.StepNumber != nil {
		out.step = *o.StepNumber
	}
	if out.id == "" {
		// Catalog items have no code field to match. Never infer their identity
		// from item_code, even when that display text happens to be a UUID.
		if o.ItemID == "" {
			return out, "ROUTING_UNLINKED", nil
		}
		rows, err := tx.QueryContext(ctx, `SELECT h.id FROM supervisor.routing_heads h JOIN supervisor.routing_revisions r ON (r.tenant_id,r.site_id,r.object_id,r.revision)=(h.tenant_id,h.site_id,h.id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND r.line_id=$3 AND r.item_id=$4 AND r.payload->>'active'='true' ORDER BY h.id LIMIT 2`, sc.TenantID, sc.SiteID, line, o.ItemID)
		if err != nil {
			return out, "", err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return out, "", err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, "", err
		}
		if len(ids) != 1 {
			return out, "ROUTING_UNLINKED", nil
		}
		out.id = ids[0]
	}
	b, err := currentRouting(ctx, tx, sc, out.id)
	if errors.Is(err, ErrNotFound) {
		return out, "ROUTING_UNLINKED", nil
	}
	if err != nil {
		return out, "", err
	}
	out.body = b
	if b.LineID != line {
		return out, "ROUTING_NOT_ON_LINE", nil
	}
	if !b.Active || b.ItemID != o.ItemID {
		return out, "ROUTING_UNLINKED", nil
	}
	// Revalidate active line/item as well: a retired line must not be planned.
	var active bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.line_heads h JOIN supervisor.line_revisions r ON (r.tenant_id,r.site_id,r.object_id,r.revision)=(h.tenant_id,h.site_id,h.id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.id=$3 AND r.payload->>'active'='true') AND EXISTS(SELECT 1 FROM supervisor.item_heads h JOIN supervisor.item_revisions r ON (r.tenant_id,r.site_id,r.object_id,r.revision)=(h.tenant_id,h.site_id,h.id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.id=$4 AND r.payload->>'active'='true')`, sc.TenantID, sc.SiteID, b.LineID, b.ItemID).Scan(&active)
	if err != nil {
		return out, "", err
	}
	if !active {
		return out, "ROUTING_UNLINKED", nil
	}
	if o.StepNumber != nil {
		out.step = *o.StepNumber
	} else if len(b.Steps) == 1 {
		out.step = 1
	} else {
		// A multi-step route cannot infer operation identity from step position or a
		// station label. Reuse only an unambiguous explicit link on current job heads.
		rows, e := tx.QueryContext(ctx, `SELECT h.id FROM supervisor.job_order_heads h JOIN supervisor.job_order_revisions r ON (r.tenant_id,r.site_id,r.job_order_id,r.revision)=(h.tenant_id,h.site_id,h.id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND r.payload->>'item_id'=$3 AND r.payload->>'operation_id'=$4 AND r.payload->>'routing_id'=$5 ORDER BY h.id LIMIT 101`, sc.TenantID, sc.SiteID, o.ItemID, o.OperationID, out.id)
		if e != nil {
			return out, "", e
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				rows.Close()
				return out, "", e
			}
			ids = append(ids, id)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return out, "", e
		}
		if len(ids) > 100 {
			return out, "ROUTING_UNLINKED", nil
		}
		for _, id := range ids {
			j, e := currentSuggestionOrder(ctx, tx, sc, id)
			if e != nil {
				return out, "", e
			}
			step := j.CurrentOrder.StepNumber
			if step == nil || out.step != 0 && out.step != *step {
				return out, "ROUTING_UNLINKED", nil
			}
			out.step = *step
		}
	}
	if out.step < 1 || out.step > len(b.Steps) {
		return out, "ROUTING_UNLINKED", nil
	}
	out.station = b.Steps[out.step-1].WorkstationID
	return out, "", nil
}

func optionalSuggestionSnapshot(ctx context.Context, tx *sql.Tx, sc Scope) (knowledge.Snapshot, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT snapshot_id FROM supervisor.knowledge_process_snapshots WHERE tenant_id=$1 AND site_id=$2 ORDER BY created_at DESC,snapshot_id DESC LIMIT 1`, sc.TenantID, sc.SiteID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return knowledge.Snapshot{}, nil
	}
	if err != nil {
		return knowledge.Snapshot{}, err
	}
	snapshot, err := knowledge.LoadSnapshotTx(ctx, tx, knowledge.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, id)
	if errors.Is(err, knowledge.ErrNotFound) || snapshot.Stale {
		return knowledge.Snapshot{}, nil
	}
	return snapshot, err
}

func claimOverlaps(c knowledge.SnapshotClaim, start, end time.Time) bool {
	lo, ok := validTime(c.EffectiveFrom)
	if !ok || !lo.Before(end) {
		return false
	}
	if c.EffectiveTo != nil {
		hi, ok := validTime(*c.EffectiveTo)
		if !ok || !start.Before(hi) {
			return false
		}
	}
	return true
}

// Knowledge is a conflict cross-check, never the source of a workstation link.
func suggestionRoute(snapshot knowledge.Snapshot, operation, station string, start, end time.Time) string {
	for _, c := range snapshot.Contents.Claims {
		if c.Predicate == "performs" && c.ObjectEntityID == operation && claimOverlaps(c, start, end) && c.SubjectID != station {
			return "ROUTING_AMBIGUOUS"
		}
	}
	return ""
}

// Edges are keyed by selected job IDs (successor -> predecessors), keeping
// revision pins and operation identities intact through both solver methods.
// precedenceEdge is one dependency of a selected job. Flow edges join consecutive
// routing steps of one production order: parts move on one at a time, so the steps
// overlap. Other edges (approved `precedes` claims) are finish-to-start.
type precedenceEdge struct {
	before string
	flow   bool
}

func suggestionPrecedence(jobs []JobOrderSummaryV1, routes map[string]suggestionRouting, snapshot knowledge.Snapshot, start, end time.Time) (map[string][]precedenceEdge, map[string]string, []string) {
	edges := map[string][]precedenceEdge{}
	reject := map[string]string{}
	warnings := map[string]bool{}
	add := func(before, after string, flow bool) {
		for i, e := range edges[after] {
			if e.before == before {
				// A claim on a routing pair keeps the stricter finish-to-start rule.
				edges[after][i].flow = e.flow && flow
				return
			}
		}
		edges[after] = append(edges[after], precedenceEdge{before, flow})
	}
	for _, a := range jobs {
		for _, b := range jobs {
			if a.ID == b.ID || a.CurrentOrder.OrderID != b.CurrentOrder.OrderID {
				continue
			}
			x, y := routes[a.ID], routes[b.ID]
			if x.step == 0 || y.step == 0 {
				continue
			}
			if x.id != y.id || x.step == y.step {
				reject[b.ID] = "ROUTING_PRECEDENCE_UNSUPPORTED"
			} else if x.step < y.step {
				add(a.ID, b.ID, true)
				warnings["ROUTING_FLOW_OVERLAP_ASSUMED"] = true
			}
		}
	}
	for _, c := range snapshot.Contents.Claims {
		if c.Predicate != "precedes" || !claimOverlaps(c, start, end) {
			continue
		}
		before, after := []string{}, []string{}
		for _, j := range jobs {
			if j.CurrentOrder.OperationID == c.SubjectID {
				before = append(before, j.ID)
			}
			if j.CurrentOrder.OperationID == c.ObjectEntityID {
				after = append(after, j.ID)
			}
		}
		if len(before) == 0 || len(after) == 0 {
			if len(before)+len(after) > 0 {
				warnings["EXTERNAL_PRECEDENCE_NOT_SCHEDULED"] = true
			}
			continue
		}
		if len(before) != 1 || len(after) != 1 {
			for _, id := range after {
				reject[id] = "ROUTING_PRECEDENCE_UNSUPPORTED"
			}
			continue
		}
		add(before[0], after[0], false)
	}
	for _, j := range jobs {
		r := routes[j.ID]
		if r.step > 1 {
			previousSelected := false
			for _, p := range edges[j.ID] {
				if routes[p.before].step == r.step-1 {
					previousSelected = true
				}
			}
			if !previousSelected {
				warnings["EXTERNAL_PRECEDENCE_NOT_SCHEDULED"] = true
			}
		}
	}
	out := []string{}
	for w := range warnings {
		out = append(out, w)
	}
	sort.Strings(out)
	return edges, reject, out
}

// unitSeconds is one part's cycle, rounded up to whole seconds like operation durations.
func unitSeconds(o PlanningOrderV1) time.Duration {
	micros, _ := rateMicros(o.Operations[0].SecondsPerUnit)
	return time.Duration((micros+999_999)/1_000_000) * time.Second
}

// edgeHolds checks one dependency on placed times. Finish-to-start: the successor
// starts after the predecessor ends. Flow (one-piece transfer): the successor starts
// after the predecessor's first part and ends after its last part plus one own cycle.
// ponytail: unit transfer, unlimited buffer, equal step quantities and no mid-run
// starvation from breaks;
// add transfer-batch sizes to routing steps when a plant needs them.
func edgeHolds(e precedenceEdge, before, after PlannedOperationV1, beforeCycle, afterCycle time.Duration) bool {
	beforeStart, _ := validTime(before.StartsAt)
	beforeEnd, _ := validTime(before.EndsAt)
	afterStart, _ := validTime(after.StartsAt)
	afterEnd, _ := validTime(after.EndsAt)
	if !e.flow {
		return !afterStart.Before(beforeEnd)
	}
	return !afterStart.Before(beforeStart.Add(beforeCycle)) && !afterEnd.Before(beforeEnd.Add(afterCycle))
}

// With one job per station, an acyclic dependency graph has an earliest feasible
// schedule independent of dispatch ties. Encode each job's earliest start allowed by
// its placed predecessors as a ready time for the unchanged native interface. Validate
// actual returned precedence too: CP-SAT may delay a predecessor within its optimum.
func prepareSuggestionPrecedence(p PlanningRequestV1, edges map[string][]precedenceEdge, reject map[string]string) (PlanningRequestV1, map[string]string) {
	pending := map[string]PlanningOrderV1{}
	cycles := map[string]time.Duration{}
	for _, o := range p.Orders {
		pending[o.JobOrderID] = o
		cycles[o.JobOrderID] = unitSeconds(o)
	}
	done := map[string]PlannedOperationV1{}
	accepted := []PlanningOrderV1{}
	start, _ := validTime(p.StartsAt)
	for len(pending) > 0 {
		progressed := false
		for _, original := range p.Orders {
			o, ok := pending[original.JobOrderID]
			if !ok {
				continue
			}
			ready := start
			waiting := false
			for _, e := range edges[o.JobOrderID] {
				if reject[e.before] != "" {
					reject[o.JobOrderID] = "ROUTING_PREDECESSOR_UNPLACED"
					break
				}
				if _, ok := pending[e.before]; ok {
					waiting = true
					continue
				}
				before, ok := done[e.before]
				if !ok {
					reject[o.JobOrderID] = "ROUTING_PREDECESSOR_UNPLACED"
					break
				}
				beforeStart, _ := validTime(before.StartsAt)
				beforeEnd, _ := validTime(before.EndsAt)
				bound := beforeEnd
				if e.flow {
					// Start after the first part arrives, and late enough that the run
					// cannot finish before the last part arrives plus one own cycle.
					seconds, _ := operationDurationSeconds(o.Operations[0])
					bound = beforeStart.Add(cycles[e.before])
					if finish := beforeEnd.Add(cycles[o.JobOrderID] - time.Duration(seconds)*time.Second); finish.After(bound) {
						bound = finish
					}
				}
				if bound.After(ready) {
					ready = bound
				}
			}
			if reject[o.JobOrderID] != "" {
				delete(pending, o.JobOrderID)
				progressed = true
				continue
			}
			if waiting {
				continue
			}
			// Breaks only delay a run, so a start at or after the bound keeps every edge.
			o.ReadyAt = p.StartsAt
			if ready.After(start) {
				o.ReadyAt = ready.UTC().Format(time.RFC3339)
			}
			single := p
			single.Orders = []PlanningOrderV1{o}
			if result, err := PlanGreedy(single); err != nil {
				reject[o.JobOrderID] = "SHIFT_CAPACITY_EXCEEDED"
			} else {
				done[o.JobOrderID] = result.Assignments[0]
				accepted = append(accepted, o)
			}
			delete(pending, o.JobOrderID)
			progressed = true
		}
		if !progressed {
			for id := range pending {
				reject[id] = "ROUTING_PRECEDENCE_UNSUPPORTED"
			}
			break
		}
	}
	p.Orders = accepted
	return p, reject
}

func suggestionPrecedenceValid(plan PlanSuggestionV1, edges map[string][]precedenceEdge, request PlanningRequestV1) bool {
	assignments := map[string]PlannedOperationV1{}
	for _, op := range plan.Assignments {
		assignments[op.JobOrderID] = op
	}
	cycles := map[string]time.Duration{}
	for _, o := range request.Orders {
		cycles[o.JobOrderID] = unitSeconds(o)
	}
	for id, op := range assignments {
		for _, e := range edges[id] {
			before, ok := assignments[e.before]
			if !ok || !edgeHolds(e, before, op, cycles[e.before], cycles[id]) {
				return false
			}
		}
	}
	return true
}
