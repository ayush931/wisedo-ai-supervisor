package agent

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plantcontext"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
)

// MachineObservation is a machine_state record as measurements.get returns it. EvidenceClass is
// assigned by the server, never by the model (see machineClass): "machine_fault" for a stoppage with
// no stated reason, "machine_stoppage" for one whose source stated a reason. Either is a reason to
// inspect, not a finding about cause.
type MachineObservation struct {
	EvidenceID    string `json:"evidence_id"`
	AssetID       string `json:"asset_id"`
	EventAt       string `json:"event_at"`
	State         string `json:"state"`
	ReasonCode    string `json:"reason_code,omitempty"`
	EvidenceClass string `json:"evidence_class"`
}

// machineScope is everything that selects a case's machine evidence: the case line, the machines
// the pinned approved process snapshot places on it, and the span of the bound windows. The case
// input and measurements.get both derive it from the same bound contexts, so a tool can only
// ever return what the run captured.
type machineScope struct {
	assets     []string
	start, end time.Time
}

// newMachineScope returns the scope for contexts of one line, in any order, under the given
// approved process snapshot, which may be nil: from the earliest window start to the latest window
// end. ok is false when contexts disagree on the line or a window does not parse: the case then
// has no machine evidence (fail closed).
func newMachineScope(contexts []plantcontext.Snapshot, snap *knowledge.Snapshot) (machineScope, bool) {
	if len(contexts) == 0 {
		return machineScope{}, false
	}
	line := contexts[0].AssetID
	var start, end time.Time
	for i, c := range contexts {
		from, e1 := time.Parse(time.RFC3339, c.Window.Start)
		to, e2 := time.Parse(time.RFC3339, c.Window.End)
		if c.AssetID != line || e1 != nil || e2 != nil || !to.After(from) {
			return machineScope{}, false
		}
		if i == 0 || from.Before(start) {
			start = from
		}
		if i == 0 || to.After(end) {
			end = to
		}
	}
	s := machineScope{assets: []string{line}, start: start.UTC(), end: end.UTC()}
	if snap != nil {
		for _, c := range snap.Contents.Claims {
			if c.Predicate != "part_of_asset" || c.ObjectEntityID != line || !EffectiveDuring(c.EffectiveFrom, c.EffectiveTo, s.start, s.end) {
				continue
			}
			s.assets = append(s.assets, c.SubjectID)
		}
	}
	return s, true
}

// EffectiveDuring reports whether an approved claim's validity overlaps [start, end). An
// unparseable bound excludes the claim.
func EffectiveDuring(from string, to *string, start, end time.Time) bool {
	f, err := time.Parse(time.RFC3339, from)
	if err != nil || !f.Before(end) {
		return false
	}
	if to == nil {
		return true
	}
	t, err := time.Parse(time.RFC3339, *to)
	return err == nil && t.After(start)
}

// machineEvidence returns the stoppages in scope. Running and unknown states are not evidence
// of anything a maintenance investigation could address, and are never read.
func machineEvidence(ctx context.Context, tx *sql.Tx, sc RunScope, s machineScope) ([]plantcontext.MachineState, error) {
	return plantcontext.StoppagesTx(ctx, tx, plantcontext.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, s.assets, s.start, s.end)
}

// machineClass is the server's classification of a recorded stop (WIS-276 review M1). A stop with
// no stated reason is an unexplained stoppage, which an inspect-and-report task exists to explain:
// machine_fault. A stop whose source stated a reason (NO_MATERIAL, BREAK, CHANGEOVER, ...) names a
// cause the Supervisor cannot confirm as a machine fault: there is no reviewed stop-reason
// taxonomy yet, so it is a machine_stoppage, shown to the model and the approver but not enough on
// its own for a maintenance investigation. A reviewed fault-reason list can widen this later.
func machineClass(m plantcontext.MachineState) string {
	if m.ReasonCode == "" {
		return recovery.EvidenceMachineFault
	}
	return recovery.EvidenceMachineStoppage
}

func machineObservation(m plantcontext.MachineState) MachineObservation {
	return MachineObservation{EvidenceID: m.RecordID, AssetID: m.AssetID, EventAt: m.EventAt.Format(time.RFC3339), State: m.State, ReasonCode: m.ReasonCode, EvidenceClass: machineClass(m)}
}

// machineSHA pins exactly what the model is shown about a record: the record's own content digest
// and the server's classification.
func machineSHA(m plantcontext.MachineState) string {
	b, _ := json.Marshal(struct {
		Record      string             `json:"record_sha256"`
		Observation MachineObservation `json:"observation"`
	}{m.SHA256, machineObservation(m)})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// MachineEvidenceCurrent reports whether every machine stoppage a run captured is still selected,
// at the same revision and with the same classification, from the run's captured contexts and
// pinned snapshot. A revoked or corrected stoppage, a changed context or snapshot, or an evidence
// read past its bound all make it false. It is the bound-evidence check for machine_state refs
// before an approval is submitted (WIS-276 review M2), and before an approved action is executed.
func MachineEvidenceCurrent(ctx context.Context, tx *sql.Tx, sc RunScope, refs []RunRef) (bool, error) {
	ok, _, err := machineEvidenceCurrent(ctx, tx, sc, refs)
	return ok, err
}

// machineEvidenceCurrent is MachineEvidenceCurrent that also returns the operational sources the
// cited stoppages come from, for the dispatch freshness check.
func machineEvidenceCurrent(ctx context.Context, tx *sql.Tx, sc RunScope, refs []RunRef) (bool, []string, error) {
	want := map[string]RunRef{}
	for _, r := range refs {
		if r.Kind == RefMachineState {
			want[r.RefID] = r
		}
	}
	if len(want) == 0 {
		return true, nil, nil
	}
	now, err := dbNow(ctx, tx)
	if err != nil {
		return false, nil, err
	}
	var contexts []plantcontext.Snapshot
	var process *knowledge.Snapshot
	for _, r := range refs {
		switch r.Kind {
		case RefContext:
			view, err := plantcontext.LoadTx(ctx, tx, plantcontext.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, r.RefID, now)
			if errors.Is(err, plantcontext.ErrNotFound) {
				return false, nil, nil
			}
			if err != nil {
				return false, nil, err
			}
			if view.Revision != r.Revision || view.Digest != r.SHA256 {
				return false, nil, nil
			}
			contexts = append(contexts, view.Snapshot)
		case RefSnapshot:
			snap, err := knowledge.LoadSnapshotTx(ctx, tx, knowledge.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, r.RefID)
			if errors.Is(err, knowledge.ErrNotFound) {
				return false, nil, nil
			}
			if err != nil {
				return false, nil, err
			}
			if snap.SHA256 != r.SHA256 {
				return false, nil, nil
			}
			process = &snap
		}
	}
	scope, ok := newMachineScope(contexts, process)
	if !ok {
		return false, nil, nil
	}
	found, err := machineEvidence(ctx, tx, sc, scope)
	if errors.Is(err, plantcontext.ErrWindowTooLarge) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	var sources []string
	for _, m := range found {
		if r, ok := want[m.RecordID]; ok && r.Revision == m.Revision && r.SHA256 == machineSHA(m) {
			delete(want, m.RecordID)
			sources = append(sources, m.SourceID)
		}
	}
	return len(want) == 0, sources, nil
}
