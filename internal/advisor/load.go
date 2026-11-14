package advisor

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plantcontext"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
)

// ErrNotFound: the case does not exist in this tenant and site (non-disclosing).
var ErrNotFound = errors.New("case unavailable")

// LoadTx assembles the suggestion for one case inside the caller's tenant transaction. It reads
// the case input exactly as the investigation does (agent.LoadCaseInput), the pinned snapshot,
// the current text of its passages after checking every source against the revocation ledger,
// and the latest investigation run of the case. It writes nothing.
func LoadTx(ctx context.Context, tx *sql.Tx, sc agent.RunScope, caseID string) (SuggestionV1, error) {
	ci, err := agent.LoadCaseInput(ctx, tx, sc, caseID, agent.LoaderOptions{})
	if errors.Is(err, agent.ErrCaseNotFound) {
		return SuggestionV1{}, ErrNotFound
	}
	if err != nil {
		return SuggestionV1{}, err
	}
	in := Input{CaseID: ci.Input.Case.ID, CaseRevision: ci.Input.Case.Revision, LineID: ci.Input.Case.LineID, OrderID: ci.Input.Case.OrderID, PassageText: map[string]string{}}
	snapshots := make([]plantcontext.Snapshot, 0, len(ci.Input.Contexts))
	eligible := true
	for _, c := range ci.Input.Contexts {
		snapshots = append(snapshots, c.Snapshot)
		// Every window whose measurements are shown must be eligible now, as the investigation's
		// precheck requires (recovery.Preconditions): a correction or revocation of the earlier
		// window makes the whole suggestion NOT_READY, not only the current one (rule 06).
		eligible = eligible && c.Eligible
	}
	if n := len(ci.Input.Contexts); n > 0 {
		in.AssetID = ci.Input.Contexts[n-1].Snapshot.AssetID
		// The facts' validity window spans the bound windows, as the investigation's machine scope
		// does; an unparseable bound leaves both zero: no claim is selected and Build says
		// CASE_WINDOW_UNKNOWN.
		from, e1 := time.Parse(time.RFC3339, ci.Input.Contexts[0].Snapshot.Window.Start)
		to, e2 := time.Parse(time.RFC3339, ci.Input.Contexts[n-1].Snapshot.Window.End)
		if e1 == nil && e2 == nil && to.After(from) {
			in.WindowStart, in.WindowEnd = from.UTC(), to.UTC()
		}
	}
	in.ContextNotEligible = !eligible
	if p := ci.Input.CurrentPlan; p != nil {
		in.Plan = &PlanRefV1{PlanRevisionID: p.PlanRevisionID, PlanRevision: p.PlanRevision, ApprovalID: p.ApprovalID}
	}
	in.Measurements = recovery.Measurements(snapshots)

	es := evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}
	// The snapshot the current context pins, read directly: LoadCaseInput omits a snapshot whose
	// evidence is unavailable, and that case must say so rather than look like "no snapshot".
	if n := len(ci.Input.Contexts); n > 0 && ci.Input.Contexts[n-1].Snapshot.ProcessSnapshotID != "" {
		snap, e := knowledge.LoadSnapshotTx(ctx, tx, knowledge.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, ci.Input.Contexts[n-1].Snapshot.ProcessSnapshotID)
		switch {
		case errors.Is(e, knowledge.ErrSnapshotEvidenceUnavailable):
			in.SnapshotUnavailable = true // a pinned source was revoked: no fact can be shown from it
		case errors.Is(e, knowledge.ErrNotFound):
		case e != nil:
			return SuggestionV1{}, e
		default:
			in.Snapshot, in.SnapshotStale = &snap, snap.Stale
		}
	}
	if in.Snapshot != nil {
		if in.PassageText, err = passageText(ctx, tx, es, in); err != nil {
			return SuggestionV1{}, err
		}
	}
	if in.Run, err = latestRun(ctx, tx, sc, es, in.CaseID); err != nil {
		return SuggestionV1{}, err
	}
	return Build(in), nil
}

// passageText reads, for every span of a candidate claim, the passage text at the pinned
// revision of a source that is still present and not revoked. Others stay absent (unavailable).
func passageText(ctx context.Context, tx *sql.Tx, es evidence.Scope, in Input) (map[string]string, error) {
	spans := map[string]knowledge.SnapshotSpan{}
	for _, sp := range in.Snapshot.Contents.Spans {
		spans[sp.SpanID] = sp
	}
	var wanted []knowledge.SnapshotSpan
	sources := map[string]bool{}
	for _, c := range candidates(in) {
		for _, id := range c.SourceSpanIDs {
			if sp, ok := spans[id]; ok {
				wanted = append(wanted, sp)
				sources[sp.SourceID] = true
			}
		}
	}
	heads := map[string]evidence.HeadState{}
	ids := keys(sources)
	for start := 0; start < len(ids); start += 1000 {
		end := min(start+1000, len(ids))
		part, err := evidence.HeadsTx(ctx, tx, es, ids[start:end])
		if err != nil {
			return nil, err
		}
		for k, v := range part {
			heads[k] = v
		}
	}
	out := map[string]string{}
	for _, sp := range wanted {
		h, ok := heads[sp.SourceID]
		if !ok || h.Revoked || sp.End <= sp.Start {
			continue
		}
		var text string
		err := tx.QueryRowContext(ctx, `SELECT substring(body FROM $5::int+1 FOR $6::int) FROM supervisor.evidence_revisions WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND revision=$4`,
			es.TenantID, es.SiteID, sp.SourceID, sp.Revision, sp.Start, sp.End-sp.Start).Scan(&text)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[sp.SpanID] = text
	}
	return out, nil
}

// latestRun returns the case's investigation run at its highest case revision, with its stored
// validated output and what it captured; nil when the case was never investigated.
func latestRun(ctx context.Context, tx *sql.Tx, sc agent.RunScope, es evidence.Scope, caseID string) (*Run, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM supervisor.agent_runs WHERE tenant_id=$1 AND site_id=$2 AND request_kind='case' AND case_id=$3 ORDER BY case_revision DESC, started_at DESC, id LIMIT 1`, sc.TenantID, sc.SiteID, caseID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r, err := agent.GetRunTx(ctx, tx, sc, id)
	if err != nil {
		return nil, err
	}
	run := &Run{ID: r.ID, CaseRevision: r.CaseRevision, State: r.State, Status: r.OutcomeStatus, Reasons: r.OutcomeReasons, Model: r.Model, Actual: r.ProviderActual, Captured: map[string]bool{}, Revoked: map[string]bool{}}
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.case_transitions WHERE tenant_id=$1 AND site_id=$2 AND case_id=$3 AND revision>$4 AND to_state='investigating')`,
		sc.TenantID, sc.SiteID, caseID, r.CaseRevision).Scan(&run.Reinvestigated); err != nil {
		return nil, err
	}
	if r.ArtifactID != "" {
		a, e := agent.LoadArtifactTx(ctx, tx, sc, r.ID, r.ArtifactID)
		if e != nil {
			return nil, e
		}
		// The artifact passed strict decoding when it was stored; a body that no longer decodes is
		// treated as no output rather than shown.
		if p, e := recovery.DecodeProposal(a.Body); e == nil {
			run.Output = &p
		}
	}
	refs, err := agent.RefsTx(ctx, tx, sc, r.ID)
	if err != nil {
		return nil, err
	}
	var sources, machines []string
	for _, ref := range refs {
		run.Captured[ref.RefID] = true
		switch ref.Kind {
		case agent.RefEvidence:
			sources = append(sources, ref.RefID)
		case agent.RefMachineState:
			machines = append(machines, ref.RefID)
		}
	}
	// Machine stoppages come from operational sources; the executor's own check says whether every
	// one the run captured is still selected unchanged (a revoked or corrected record, or a changed
	// context, makes it false). When it is not, none of them may be cited (rule 06).
	if len(machines) > 0 {
		current, e := agent.MachineEvidenceCurrent(ctx, tx, sc, refs)
		if e != nil {
			return nil, e
		}
		if !current {
			for _, m := range machines {
				run.Revoked[m] = true
			}
		}
	}
	if len(sources) > 0 {
		heads, e := evidence.HeadsTx(ctx, tx, es, sources)
		if e != nil {
			return nil, e
		}
		for _, s := range sources {
			if h, ok := heads[s]; !ok || h.Revoked {
				run.Revoked[s] = true
			}
		}
	}
	return run, nil
}
