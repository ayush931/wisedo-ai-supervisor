package chat

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
)

type CorrectionAttribution struct {
	ClaimID    string    `json:"claim_id" contract:"uuid"`
	Revision   int64     `json:"revision"`
	Reviewer   string    `json:"reviewer" contract:"max=200"`
	ReviewedAt time.Time `json:"reviewed_at"`
}

func withholdAnswer(m *Message, reason string) {
	m.State, m.ReasonCode, m.Body, m.Citations, m.Quantities, m.ClaimCitations, m.Corrections = "needs_input", &reason, nil, []Citation{}, nil, nil, nil
}

// Legacy span-only answers never acquire inferred claim references or correction labels.
func reauthorizeClaims(ctx context.Context, tx *sql.Tx, sc Scope, m *Message, refs []agent.RunRef) error {
	if len(m.ClaimCitations) == 0 {
		return nil
	}
	if m.SnapshotID == nil {
		withholdAnswer(m, agent.CodeFactNotApproved)
		return nil
	}
	snapshot, err := knowledge.LoadSnapshotTx(ctx, tx, knowledge.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, *m.SnapshotID)
	if errors.Is(err, knowledge.ErrNotFound) {
		withholdAnswer(m, agent.CodeEvidenceRevoked)
		return nil
	}
	if err != nil {
		return err
	}
	pinned := slices.ContainsFunc(refs, func(r agent.RunRef) bool {
		return r.Kind == agent.RefSnapshot && r.RefID == snapshot.SnapshotID && r.SHA256 == snapshot.SHA256 && r.Revision == snapshot.InventoryRevision
	})
	if !pinned {
		withholdAnswer(m, agent.CodeFactNotApproved)
		return nil
	}
	seen := map[agent.ChatClaimRef]bool{}
	for _, citation := range m.ClaimCitations {
		var reviewID string
		for _, c := range snapshot.Contents.Claims {
			if c.ClaimID == citation.ClaimID && c.Revision == citation.Revision && len(c.SourceSpanIDs) == len(citation.SourceSpanIDs) && !slices.ContainsFunc(c.SourceSpanIDs, func(id string) bool { return !slices.Contains(citation.SourceSpanIDs, id) }) {
				reviewID = c.ReviewID
				break
			}
		}
		stands := false
		if reviewID != "" {
			err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.knowledge_claim_reviews r WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.review_id=$3 AND `+knowledge.ApprovalStandsSQL("r")+`)`, sc.TenantID, sc.SiteID, reviewID).Scan(&stands)
			if err != nil {
				return err
			}
		}
		if !stands {
			withholdAnswer(m, agent.CodeFactNotApproved)
			return nil
		}
		if seen[citation.ChatClaimRef] {
			continue
		}
		seen[citation.ChatClaimRef] = true
		attribution := CorrectionAttribution{ClaimID: citation.ClaimID, Revision: citation.Revision}
		err = tx.QueryRowContext(ctx, `SELECT CASE WHEN p.display_name IS NOT NULL AND btrim(p.display_name)<>'' AND position('@' in p.display_name)=0 THEN p.display_name ELSE 'Site reviewer' END,r.created_at
   FROM supervisor.knowledge_memory_candidate_reviews r
   LEFT JOIN supervisor.memberships m ON (m.tenant_id,m.site_id,m.issuer,m.subject)=(r.tenant_id,r.site_id,r.actor_issuer,r.actor_subject) AND m.active
   LEFT JOIN supervisor.member_profiles p ON (p.tenant_id,p.issuer,p.subject)=(m.tenant_id,m.issuer,m.subject)
   WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.claim_id=$3 AND r.claim_revision=$4 AND r.canonical_review_id=$5 AND r.verdict='approve'`, sc.TenantID, sc.SiteID, citation.ClaimID, citation.Revision, reviewID).Scan(&attribution.Reviewer, &attribution.ReviewedAt)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		m.Corrections = append(m.Corrections, attribution)
	}
	return nil
}

// Every cited span must be readable under this browser reader, including on saved-answer replay.
func (s *Service) reauthorizeCitations(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, m *Message, refs []agent.RunRef) error {
	sources := evidence.New(s.db).WithSessionPolicy(s.sessions)
	seen := map[string]int64{}
	for _, citation := range m.Citations {
		var source string
		err := tx.QueryRowContext(ctx, `SELECT source_id::text FROM supervisor.evidence_spans WHERE tenant_id=$1 AND site_id=$2 AND id=$3 AND revision=$4`, sc.TenantID, sc.SiteID, citation.ID, citation.Revision).Scan(&source)
		if errors.Is(err, sql.ErrNoRows) || citation.Kind != "evidence_span" {
			withholdAnswer(m, agent.CodeEvidenceRevoked)
			return nil
		}
		if err != nil {
			return err
		}
		if seen[source] == citation.Revision {
			continue
		}
		read, err := sources.ReadParticipantTx(ctx, tx, evidence.Actor{Issuer: a.Issuer, Subject: a.Subject, SessionHash: a.SessionHash}, evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, source, citation.Revision)
		if errors.Is(err, evidence.ErrNotFound) || errors.Is(err, evidence.ErrDenied) {
			withholdAnswer(m, agent.CodeEvidenceRevoked)
			return nil
		}
		if err != nil {
			return err
		}
		if m.runID != "" && !slices.ContainsFunc(refs, func(r agent.RunRef) bool {
			return r.Kind == agent.RefEvidence && r.RefID == source && r.Revision == read.Revision && r.SHA256 == read.SHA256 && r.AccessEpoch == read.AccessEpoch
		}) {
			withholdAnswer(m, agent.CodeEvidenceRevoked)
			return nil
		}
		seen[source] = citation.Revision
	}
	return nil
}
