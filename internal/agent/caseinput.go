package agent

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/approvals"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plantcontext"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/roster"
)

// ErrCaseNotFound: the case does not exist in this tenant and site (non-disclosing).
var ErrCaseNotFound = errors.New("case unavailable")

// LoaderOptions are the deployment-level inputs the loader cannot read from the database yet.
type LoaderOptions struct {
	// ModeEnabled is SUPERVISOR_AGENT_MODE_ENABLED. Until WIS-290/WIS-276 adds a site policy row,
	// investigation mode is a deployment flag and defaults to disabled.
	ModeEnabled bool
}

// CaseInput is the server's view of one case at one moment: the recovery.Input the specialist is
// judged against, and the scope a run may read.
type CaseInput struct {
	Input recovery.Input
	// Refs is the scope a run captures: the bound contexts, their measurements, the pinned process
	// snapshot and the evidence sources the approved plan cites. Unrevoked sources only.
	Refs []RunRef
	// Passages are the exact spans cited by the approved plan, never the whole source document.
	Passages []RunPassage
	// ContextID is the current (last) bound context.
	ContextID string
	// MachineIDs are the entities the pinned approved process snapshot places on the case line
	// (part_of_asset) during the bound windows, in claim order: the machine scope minus the line.
	MachineIDs []string
}

// LoadCaseInput builds the minimal recovery.Input (design §5.4) inside the caller's tenant
// transaction. The Roster is the case line's supervisor-confirmed maintenance assignees who are
// still active (WIS-276). Evidence is classified by the server, never by the model: the bound
// contexts' measurements are "production", and each recorded stoppage of the case line, or of a
// machine the approved process snapshot places on it, inside the bound windows is
// "machine_fault" when no reason was stated and "machine_stoppage" when the source stated one (see
// machineClass), from a trustworthy record of a currently approved source. Nothing else is
// classified yet, so materials, staffing and quality restrictions still cannot support a
// maintenance investigation.
func LoadCaseInput(ctx context.Context, tx *sql.Tx, sc RunScope, caseID string, opt LoaderOptions) (CaseInput, error) {
	var out CaseInput
	if !validRunScope(sc) || !isUUID(caseID) {
		return out, ErrCaseNotFound
	}
	now, err := dbNow(ctx, tx)
	if err != nil {
		return out, err
	}
	in := recovery.Input{ModeEnabled: opt.ModeEnabled, PolicyVersion: approvals.PolicyRevision, Now: now, Evidence: []recovery.Evidence{}, Roster: []recovery.Assignee{}}
	var contextID string
	err = tx.QueryRowContext(ctx, `SELECT id,revision,state,line_id,order_id,context_snapshot_id FROM supervisor.cases WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, caseID).
		Scan(&in.Case.ID, &in.Case.Revision, &in.Case.State, &in.Case.LineID, &in.Case.OrderID, &contextID)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrCaseNotFound
	}
	if err != nil {
		return out, err
	}
	confirmed, err := roster.ConfirmedTx(ctx, tx, sc.TenantID, sc.SiteID, in.Case.LineID)
	if err != nil {
		return out, err
	}
	for _, c := range confirmed {
		in.Roster = append(in.Roster, recovery.Assignee{ID: c.AssigneeID, LineID: in.Case.LineID, ConfirmedBy: c.ConfirmedBy})
	}
	pc := plantcontext.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}
	current, err := plantcontext.LoadTx(ctx, tx, pc, contextID, now)
	if err != nil {
		return out, err
	}
	// The case binds its triggering window; the qualifying previous window is the context head
	// 15 minutes earlier under the same process snapshot, exactly as cases.Trigger selected it.
	views := []plantcontext.View{current}
	if start, e := time.Parse(time.RFC3339, current.Window.Start); e == nil {
		previousID, e := plantcontext.HeadTx(ctx, tx, pc, current.OrderID, current.AssetID, start.Add(-15*time.Minute).UTC().Format(time.RFC3339), current.ProcessSnapshotID)
		if e != nil {
			return out, e
		}
		if previousID != "" {
			previous, e := plantcontext.LoadTx(ctx, tx, pc, previousID, now)
			if e != nil {
				return out, e
			}
			views = []plantcontext.View{previous, current}
		}
	}
	snapshots := make([]plantcontext.Snapshot, 0, len(views))
	for _, v := range views {
		in.Contexts = append(in.Contexts, recovery.ContextView{Snapshot: v.Snapshot, Eligible: v.CurrentStatus.Eligible})
		snapshots = append(snapshots, v.Snapshot)
		out.Refs = append(out.Refs, RunRef{Kind: RefContext, RefID: v.ContextID, Revision: v.Revision, SHA256: v.Digest})
	}
	// An eligible context's plan is, by plantcontext's current-status rule, the plan that is
	// approved now; an ineligible one already fails the precheck.
	if current.CurrentStatus.Eligible {
		in.CurrentPlan = current.ApprovedPlanRevision
	}
	for _, m := range recovery.Measurements(snapshots) {
		in.Evidence = append(in.Evidence, recovery.Evidence{ID: m.ID, Kind: recovery.EvidenceProduction})
		revision := int64(1)
		for _, v := range views {
			if v.ContextID == m.ContextID {
				revision = v.Revision
			}
		}
		out.Refs = append(out.Refs, RunRef{Kind: RefMeasurement, RefID: m.ID, Revision: revision, SHA256: measurementSHA(m)})
	}
	var process *knowledge.Snapshot
	if current.ProcessSnapshotID != "" {
		snap, e := knowledge.LoadSnapshotTx(ctx, tx, knowledge.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, current.ProcessSnapshotID)
		if e != nil && !errors.Is(e, knowledge.ErrNotFound) {
			return out, e
		}
		if e == nil && snap.InventoryRevision > 0 {
			process = &snap
			in.GraphVersion = snap.InventoryRevision
			out.Refs = append(out.Refs, RunRef{Kind: RefSnapshot, RefID: snap.SnapshotID, Revision: snap.InventoryRevision, SHA256: snap.SHA256})
		}
	}
	if scope, ok := newMachineScope(snapshots, process); ok {
		out.MachineIDs = scope.assets[1:]
		stops, e := machineEvidence(ctx, tx, sc, scope)
		switch {
		case errors.Is(e, plantcontext.ErrWindowTooLarge):
			in.EvidenceTooLarge, stops = true, nil // terminal needs_input via the precheck
		case e != nil:
			return out, e
		}
		for _, m := range stops {
			in.Evidence = append(in.Evidence, recovery.Evidence{ID: m.RecordID, Kind: machineClass(m)})
			out.Refs = append(out.Refs, RunRef{Kind: RefMachineState, RefID: m.RecordID, Revision: m.Revision, SHA256: machineSHA(m)})
		}
	}
	if in.CurrentPlan != nil {
		refs, e := planEvidence(ctx, tx, sc, in.CurrentPlan.ApprovalID)
		if e != nil {
			return out, e
		}
		out.Refs = append(out.Refs, refs...)
		rows, e := tx.QueryContext(ctx, `SELECT s.id::text,s.source_id::text,s.revision FROM supervisor.plan_approval_spans p
 JOIN supervisor.evidence_spans s ON (s.tenant_id,s.site_id,s.id)=(p.tenant_id,p.site_id,p.span_id)
 WHERE p.tenant_id=$1 AND p.site_id=$2 AND p.approval_id=$3 ORDER BY s.id LIMIT $4`, sc.TenantID, sc.SiteID, in.CurrentPlan.ApprovalID, MaxRunPassages+1)
		if e != nil {
			return out, e
		}
		defer rows.Close()
		for rows.Next() {
			var p RunPassage
			if e = rows.Scan(&p.SpanID, &p.SourceID, &p.SourceRevision); e != nil {
				return out, e
			}
			out.Passages = append(out.Passages, p)
		}
		if e = rows.Err(); e != nil {
			return out, e
		}
		if len(out.Passages) > MaxRunPassages {
			out.Passages = nil // Keep every required source; the model request budget still bounds the read.
		}
	}
	out.Input, out.ContextID = in, current.ContextID
	return out, nil
}

// planEvidence returns the unrevoked evidence sources the approved plan's decision cites, at the
// cited revision. This is the only join from a case to source text in slice 1.
func planEvidence(ctx context.Context, tx *sql.Tx, sc RunScope, approvalID string) ([]RunRef, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT es.source_id::text,es.revision,r.sha256 FROM supervisor.plan_approval_spans s
 JOIN supervisor.evidence_spans es ON (es.tenant_id,es.site_id,es.id)=(s.tenant_id,s.site_id,s.span_id)
 JOIN supervisor.evidence_revisions r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=(es.tenant_id,es.site_id,es.source_id,es.revision)
 WHERE s.tenant_id=$1 AND s.site_id=$2 AND s.approval_id=$3 ORDER BY 1`, sc.TenantID, sc.SiteID, approvalID)
	if err != nil {
		return nil, err
	}
	var refs []RunRef
	var ids []string
	for rows.Next() {
		var r RunRef
		if err = rows.Scan(&r.RefID, &r.Revision, &r.SHA256); err != nil {
			rows.Close()
			return nil, err
		}
		r.Kind = RefEvidence
		refs, ids = append(refs, r), append(ids, r.RefID)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	heads, err := evidence.HeadsTx(ctx, tx, evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, ids)
	if err != nil {
		return nil, err
	}
	out := []RunRef{}
	for _, r := range refs {
		if h, ok := heads[r.RefID]; ok && !h.Revoked {
			r.AccessEpoch = h.AccessEpoch
			out = append(out, r)
		}
	}
	return out, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
