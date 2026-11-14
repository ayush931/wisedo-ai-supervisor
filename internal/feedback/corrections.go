package feedback

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
)

func correctionError(err error) error {
	switch {
	case errors.Is(err, knowledge.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, knowledge.ErrDenied):
		return ErrDenied
	case errors.Is(err, knowledge.ErrInvalidContract):
		return ErrInvalid
	default:
		return err
	}
}

func bindRunProvenance(ctx context.Context, tx *sql.Tx, sc Scope, run string) (knowledge.CorrectionProvenance, error) {
	out := knowledge.CorrectionProvenance{RunID: &run}
	var state string
	var reasons []byte
	var outcome struct {
		Status  string   `json:"status"`
		Reasons []string `json:"reasons"`
	}
	err := tx.QueryRowContext(ctx, `SELECT r.state,coalesce(r.outcome_status,''),to_json(r.outcome_reasons),r.artifact_id::text,a.sha256 FROM supervisor.agent_runs r LEFT JOIN supervisor.agent_artifacts a ON (a.tenant_id,a.site_id,a.run_id,a.id)=(r.tenant_id,r.site_id,r.id,r.artifact_id) WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.id=$3`, sc.TenantID, sc.SiteID, run).Scan(&state, &outcome.Status, &reasons, &out.ArtifactID, &out.ArtifactSHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if err = json.Unmarshal(reasons, &outcome.Reasons); err != nil {
		return out, err
	}
	if state == "running" {
		return out, ErrSubjectState
	}
	if out.ArtifactID == nil {
		_, digest, e := canonical.Envelope("CorrectionTerminalOutcomeV1", 1, sc.TenantID, sc.SiteID, outcome)
		if e != nil {
			return out, e
		}
		out.TerminalSHA256 = &digest
	}
	type ref struct {
		Kind        string `json:"kind"`
		ID          string `json:"id"`
		Revision    int64  `json:"revision"`
		SHA256      string `json:"sha256"`
		AccessEpoch *int64 `json:"access_epoch"`
	}
	refs := []ref{}
	rows, err := tx.QueryContext(ctx, `SELECT kind,ref_id,revision,sha256,access_epoch FROM supervisor.agent_run_refs WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3 ORDER BY kind,ref_id`, sc.TenantID, sc.SiteID, run)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var r ref
		if err = rows.Scan(&r.Kind, &r.ID, &r.Revision, &r.SHA256, &r.AccessEpoch); err != nil {
			rows.Close()
			return out, err
		}
		refs = append(refs, r)
		if r.Kind == "snapshot" {
			if out.ProcessSnapshotID != nil {
				rows.Close()
				return out, ErrSubjectState
			}
			id := r.ID
			out.ProcessSnapshotID = &id
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return out, err
	}
	_, digest, err := canonical.Envelope("CorrectionRunReferencesV1", 1, sc.TenantID, sc.SiteID, refs)
	out.RunRefsSHA256 = &digest
	return out, err
}

// The run captures the triggering context and optionally the preceding window (caseinput.go).
// Select the latest captured window, never the case's mutable current context.
func bindInvestigation(ctx context.Context, tx *sql.Tx, a Principal, sc Scope, role, run string) (bound, error) {
	var b bound
	var caseID, contextID string
	var revision int64
	err := tx.QueryRowContext(ctx, `SELECT r.case_id::text,r.case_revision,x.ref_id FROM supervisor.agent_runs r JOIN supervisor.agent_run_refs x ON (x.tenant_id,x.site_id,x.run_id)=(r.tenant_id,r.site_id,r.id) AND x.kind='context'
 JOIN supervisor.plant_contexts p ON p.tenant_id=x.tenant_id AND p.site_id=x.site_id AND p.id::text=x.ref_id AND p.digest=x.sha256
 WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.id=$3 AND r.request_kind='case'
 ORDER BY p.window_end DESC,p.id DESC LIMIT 1`, sc.TenantID, sc.SiteID, run).Scan(&caseID, &revision, &contextID)
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	if err != nil {
		return b, err
	}
	if !supervises(role) {
		var assigned bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.tasks WHERE tenant_id=$1 AND site_id=$2 AND case_id=$3 AND assignee_issuer=$4 AND assignee_subject=$5)`, sc.TenantID, sc.SiteID, caseID, a.Issuer, a.Subject).Scan(&assigned); err != nil {
			return b, err
		}
		if !assigned {
			return b, ErrNotFound
		}
	}
	b.caseID, b.caseRevision, b.contextID = &caseID, &revision, &contextID
	b.provenance, err = bindRunProvenance(ctx, tx, sc, run)
	return b, err
}
