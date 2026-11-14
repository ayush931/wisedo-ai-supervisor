package agent

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/approvals"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plantcontext"
)

// DispatchEvidenceCode re-evaluates, at dispatch, every evidence revision an approved payload
// binds (rule 05: bound evidence revisions and freshness; WIS-277 review B1). The payload's
// evidence_revisions are what the approvers signed; the generating run's captured refs carry the
// digests and access epochs behind them. It returns a refusal code, or "" when all still hold:
//   - each signed ref is one the run captured, at the same revision;
//   - an evidence source is unrevoked, at the same access epoch, and still at the bound revision;
//   - the run's contexts are unchanged and still eligible (the measurements are computed from them);
//   - every captured machine stoppage is still selected unchanged (MachineEvidenceCurrent);
//   - every source behind those contexts and stoppages heartbeated within its freshness now.
//
// It runs inside the executor's transaction and has the cases.BoundEvidenceCheck signature.
func DispatchEvidenceCode(ctx context.Context, tx *sql.Tx, tenantID, siteID string, p approvals.Payload) (string, error) {
	sc := RunScope{TenantID: tenantID, SiteID: siteID}
	if !validRunScope(sc) || !isUUID(p.GeneratedByRun) {
		return approvals.DispatchEvidenceRevoked, nil
	}
	refs, err := RefsTx(ctx, tx, sc, p.GeneratedByRun)
	if errors.Is(err, ErrRunNotFound) {
		return approvals.DispatchEvidenceRevoked, nil
	}
	if err != nil {
		return "", err
	}
	captured := map[string]RunRef{}
	for _, r := range refs {
		captured[r.Kind+"/"+r.RefID] = r
	}
	var sources []string
	for _, b := range p.Evidence {
		r, ok := captured[b.Kind+"/"+b.ID]
		if !ok || r.Revision != b.Revision {
			return approvals.DispatchEvidenceRevised, nil
		}
		if b.Kind == RefEvidence {
			sources = append(sources, b.ID)
		}
	}
	heads, err := evidence.HeadsTx(ctx, tx, evidence.Scope{TenantID: tenantID, SiteID: siteID}, sources)
	if err != nil {
		return "", err
	}
	for _, id := range sources {
		h, ok := heads[id]
		r := captured[RefEvidence+"/"+id]
		switch {
		case !ok || h.Revoked || h.AccessEpoch != r.AccessEpoch:
			return approvals.DispatchEvidenceRevoked, nil
		case h.Revision != r.Revision:
			return approvals.DispatchEvidenceRevised, nil
		}
	}
	// The contexts behind the measurements must still be what selection would use now: the
	// same check as before submission (scopeFactsCode), so a late record, correction, withdrawn
	// source approval or changed plan refuses here too (WIS-277 review, B1 remainder).
	current, contextSources, err := contextsCurrent(ctx, tx, sc, refs)
	if err != nil {
		return "", err
	}
	if !current {
		return approvals.DispatchEvidenceRevised, nil
	}
	current, machineSources, err := machineEvidenceCurrent(ctx, tx, sc, refs)
	if err != nil {
		return "", err
	}
	if !current {
		return approvals.DispatchEvidenceRevoked, nil
	}
	// Unchanged revisions do not prove the sources behind them are still current: each must have
	// heartbeated within its freshness when the task would be created (rule 05; review M4).
	now, err := dbNow(ctx, tx)
	if err != nil {
		return "", err
	}
	live, err := plantcontext.SourcesLiveTx(ctx, tx, plantcontext.Scope{TenantID: tenantID, SiteID: siteID}, append(contextSources, machineSources...), now)
	if err != nil {
		return "", err
	}
	if !live {
		return approvals.DispatchEvidenceStale, nil
	}
	return "", nil
}

// contextsCurrent reports whether every context a run captured is unchanged and still eligible
// now: not superseded, its sources and plan still approved, and selection still choosing the
// same records. It is the one check both before submission and at dispatch, and it returns the
// operational sources the contexts were built from.
func contextsCurrent(ctx context.Context, tx *sql.Tx, sc RunScope, refs []RunRef) (bool, []string, error) {
	now, err := dbNow(ctx, tx)
	if err != nil {
		return false, nil, err
	}
	var sources []string
	for _, ref := range refs {
		if ref.Kind != RefContext {
			continue
		}
		view, err := plantcontext.LoadTx(ctx, tx, plantcontext.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, ref.RefID, now)
		if errors.Is(err, plantcontext.ErrNotFound) {
			return false, nil, nil
		}
		if err != nil {
			return false, nil, err
		}
		if !view.CurrentStatus.Eligible || view.Revision != ref.Revision || view.Digest != ref.SHA256 {
			return false, nil, nil
		}
		for _, p := range view.SourcePolicyRevisionIDs {
			sources = append(sources, p.SourceID)
		}
	}
	return true, sources, nil
}
