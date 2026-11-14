package plantcontext

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"
)

// Machine states a machine_state record may report (WIS-272 intake contract). Only Stopped can
// support a maintenance investigation, and only as a reason to inspect: a recorded stoppage is
// never a cause, and the absence of one is never proof that no fault exists (rule 06).
const (
	MachineRunning = "running"
	MachineStopped = "stopped"
	MachineUnknown = "unknown"
)

// MaxMachineStates bounds the stoppage chains read for one case window. More than this fails
// closed with ErrWindowTooLarge instead of silently dropping records. Only chains with a stopped
// revision count, so routine running telemetry never reaches the bound (WIS-276 review M3).
const MaxMachineStates = 200

// MachineState is the current revision of one machine_state record chain: a trustworthy record
// from a currently approved source whose reviewed scope covers machine_state.
type MachineState struct {
	RecordID       string    `json:"record_id"`
	SourceID       string    `json:"source_id"`
	SourceSequence int64     `json:"source_sequence"`
	ExternalKey    string    `json:"external_key"`
	Revision       int64     `json:"revision"`
	AssetID        string    `json:"asset_id"`
	EventAt        time.Time `json:"event_at"`
	State          string    `json:"state"`
	ReasonCode     string    `json:"reason_code,omitempty"`
	SHA256         string    `json:"sha256"`
}

// StoppagesTx returns the machine_state stoppages about assetIDs whose event time falls in
// [start, end): record chains whose latest revision reports "stopped", ordered by event time and
// record ID.
// A chain counts only when its latest revision is about one of the assets, inside the window and
// not quarantined, and its source is trustworthy for it the way production deltas are judged:
// approved now with machine_state in its reviewed scope, folded gap-free through the record's
// sequence, and with an "ok" clock (the event time is compared with the windows). A correction
// that moves a record out of scope, or to another state, therefore replaces it; a revoked or
// disabled source contributes nothing. Completeness is not claimed: a missing stoppage is never
// evidence that the machine ran.
func StoppagesTx(ctx context.Context, tx *sql.Tx, sc Scope, assetIDs []string, start, end time.Time) ([]MachineState, error) {
	if len(assetIDs) == 0 || !end.After(start) {
		return []MachineState{}, nil
	}
	// Step 1: the chains with a stopped revision about these assets in the window. A chain survives
	// step 2 only if its latest revision is stopped, in scope and in the window, and that revision
	// matches this filter itself, so nothing that could count is left out here.
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT source_id::text,external_key FROM supervisor.operational_event_records
 WHERE tenant_id=$1 AND site_id=$2 AND event_kind='machine_state' AND payload->>'state'='stopped' AND asset_entity_id=ANY($3::uuid[]) AND event_at>=$4 AND event_at<$5
 AND quarantine_reason IS DISTINCT FROM 'DUPLICATE_EVENT' LIMIT $6`, sc.TenantID, sc.SiteID, assetIDs, start, end, MaxMachineStates+1)
	if err != nil {
		return nil, err
	}
	type chain struct{ source, key string }
	var chains []chain
	for rows.Next() {
		var c chain
		if err = rows.Scan(&c.source, &c.key); err != nil {
			rows.Close()
			return nil, err
		}
		chains = append(chains, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(chains) > MaxMachineStates {
		return nil, ErrWindowTooLarge
	}

	inScope := map[string]bool{}
	for _, a := range assetIDs {
		inScope[a] = true
	}
	// complete is the source's folded, gap-free sequence when it is trustworthy for machine
	// state, or -1 when it is not.
	complete := map[string]int64{}
	out := []MachineState{}
	for _, c := range chains {
		through, seen := complete[c.source]
		if !seen {
			src, err := resolveSource(ctx, tx, sc, c.source)
			if err != nil {
				return nil, err
			}
			through = -1
			if src.Approved && src.ScopeAllowsMachineState && src.Progress != nil && src.Progress.ClockQuality == "ok" {
				through = src.Progress.CompleteSequence
			}
			complete[c.source] = through
		}
		if through < 1 {
			continue
		}
		// Step 2: the chain's latest revision, wherever it now points.
		var m MachineState
		var asset, state, reason, quarantine sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT r.source_sequence,r.revision,r.asset_entity_id::text,r.event_at,r.payload->>'state',r.payload->>'reason_code',r.event_sha256,r.quarantine_reason
 FROM supervisor.operational_event_records r
 WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.source_id=$3 AND r.external_key=$4 AND r.event_kind='machine_state' AND r.quarantine_reason IS DISTINCT FROM 'DUPLICATE_EVENT'
 ORDER BY r.revision DESC,r.source_sequence DESC LIMIT 1`, sc.TenantID, sc.SiteID, c.source, c.key).
			Scan(&m.SourceSequence, &m.Revision, &asset, &m.EventAt, &state, &reason, &m.SHA256, &quarantine)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		m.EventAt = m.EventAt.UTC()
		trusted := !quarantine.Valid && m.SourceSequence <= through
		if !trusted || state.String != MachineStopped || !inScope[asset.String] || m.EventAt.Before(start) || !m.EventAt.Before(end) {
			continue
		}
		m.RecordID, m.SourceID, m.ExternalKey = recordID(sc, c.source, m.SourceSequence), c.source, c.key
		m.AssetID, m.State, m.ReasonCode = asset.String, state.String, reason.String
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].EventAt.Equal(out[j].EventAt) {
			return out[i].EventAt.Before(out[j].EventAt)
		}
		return out[i].RecordID < out[j].RecordID
	})
	return out, nil
}
