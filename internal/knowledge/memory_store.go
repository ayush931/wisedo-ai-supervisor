package knowledge

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/google/uuid"
)

// CreateMemoryCandidateTx is called only after feedback has been inserted in the same transaction.
// It derives the author and subject from that ledger, never from model output or caller provenance.
func (s *Service) CreateMemoryCandidateTx(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, feedbackID string, in MemoryRevisionInput) (string, error) {
	if err := in.Validate(); err != nil {
		return "", err
	}
	if err := s.checkSession(ctx, tx, a); err != nil {
		return "", err
	}
	var allowed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.feedback WHERE tenant_id=$1 AND site_id=$2 AND id=$3 AND kind='correction' AND subject_kind IN ('answer','investigation') AND actor_issuer=$4 AND actor_subject=$5)`, sc.TenantID, sc.SiteID, feedbackID, a.Issuer, a.Subject).Scan(&allowed); err != nil {
		return "", err
	}
	if !allowed {
		return "", ErrNotFound
	}
	if err := validateMemoryReferences(ctx, tx, sc, feedbackID, in); err != nil {
		return "", err
	}
	id := uuid.NewString()
	if _, err := tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_memory_candidates(tenant_id,site_id,candidate_id,source_feedback_id,proposer_issuer,proposer_subject) VALUES($1,$2,$3,$4,$5,$6)`, sc.TenantID, sc.SiteID, id, feedbackID, a.Issuer, a.Subject); err != nil {
		return "", err
	}
	if err := appendMemoryRevision(ctx, tx, a, sc, id, 1, in); err != nil {
		return "", err
	}
	return id, audit(ctx, tx, a, sc, id, 1, "memory.created")
}

func validateMemoryReferences(ctx context.Context, tx *sql.Tx, sc Scope, feedbackID string, in MemoryRevisionInput) error {
	if in.Claim == nil {
		return nil
	}
	refs, _, _, err := claimRefs(ctx, tx, sc, *in.Claim)
	if err != nil {
		return err
	}
	states, err := lockSources(ctx, tx, sc, refs)
	if err != nil {
		return err
	}
	if _, err = eligible(refs, states, false); err != nil {
		return err
	}
	if in.ExpectedBaseRevision > 0 {
		// A target must be an exact fact in the original output's captured snapshot.
		var found bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.feedback f JOIN supervisor.knowledge_snapshot_claims c ON (c.tenant_id,c.site_id,c.snapshot_id)=(f.tenant_id,f.site_id,f.process_snapshot_id) WHERE f.tenant_id=$1 AND f.site_id=$2 AND f.id=$3 AND c.claim_id=$4 AND c.claim_revision=$5
 AND EXISTS(SELECT 1 FROM supervisor.knowledge_claim_spans sp WHERE (sp.tenant_id,sp.site_id,sp.claim_id,sp.claim_revision)=(c.tenant_id,c.site_id,c.claim_id,c.claim_revision) AND (
  (f.subject_kind='answer' AND EXISTS(SELECT 1 FROM supervisor.chat_messages m CROSS JOIN LATERAL jsonb_array_elements(m.citations) ci WHERE (m.tenant_id,m.site_id,m.conversation_id,m.message_id)=(f.tenant_id,f.site_id,f.conversation_id,f.message_id) AND ci->>'kind'='evidence_span' AND ci->>'id'=sp.span_id::text AND (ci->>'revision')::bigint=sp.source_revision))
  OR (f.subject_kind='investigation' AND EXISTS(SELECT 1 FROM supervisor.agent_artifacts a WHERE (a.tenant_id,a.site_id,a.run_id,a.id)=(f.tenant_id,f.site_id,f.run_id,f.artifact_id) AND jsonb_path_query_array(a.body,'$.**.evidence_ids[*]') @> to_jsonb(ARRAY[sp.source_id::text])))
 )))`, sc.TenantID, sc.SiteID, feedbackID, in.Claim.ClaimID, in.ExpectedBaseRevision).Scan(&found)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
	}
	return nil
}

func appendMemoryRevision(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, id string, revision int64, in MemoryRevisionInput) error {
	_, digest, err := canonical.Envelope("MemoryCandidateRevisionV1", 1, sc.TenantID, sc.SiteID, in)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	var target, base any
	if in.ExpectedBaseRevision > 0 {
		target, base = in.Claim.ClaimID, in.ExpectedBaseRevision
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_memory_candidate_revisions(tenant_id,site_id,candidate_id,revision,payload,sha256,target_claim_id,base_revision,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, sc.TenantID, sc.SiteID, id, revision, raw, digest, target, base, a.Issuer, a.Subject)
	return err
}

// memorySuperseded is true once the author replaced the candidate's feedback (supersedes_feedback_id).
const memorySuperseded = `EXISTS(SELECT 1 FROM supervisor.feedback s WHERE (s.tenant_id,s.site_id,s.supersedes_feedback_id)=(c.tenant_id,c.site_id,c.source_feedback_id))`

// memoryVisible grants reviewers only the selected corrected turn, never its conversation.
const memoryVisible = `(c.proposer_issuer=$4 AND c.proposer_subject=$5 OR EXISTS(SELECT 1 FROM supervisor.memberships m WHERE m.tenant_id=c.tenant_id AND m.site_id=c.site_id AND m.issuer=$4 AND m.subject=$5 AND m.active AND m.role IN ('owner','supervisor')))`

func (s *Service) loadMemory(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, id string, revision int64) (MemoryCandidate, error) {
	out := MemoryCandidate{CandidateID: id, SiteID: sc.SiteID, Kind: "process_claim", SourceFeedbackIDs: []string{}}
	var raw []byte
	var feedbackID string
	var superseded bool
	err := tx.QueryRowContext(ctx, `SELECT c.source_feedback_id,c.proposer_issuer,c.proposer_subject,r.revision,c.created_at,r.payload,r.sha256,`+memorySuperseded+`
 FROM supervisor.knowledge_memory_candidates c JOIN supervisor.knowledge_memory_candidate_revisions r ON (r.tenant_id,r.site_id,r.candidate_id)=(c.tenant_id,c.site_id,c.candidate_id)
 WHERE c.tenant_id=$1 AND c.site_id=$2 AND c.candidate_id=$3 AND `+memoryVisible+` AND r.revision=CASE WHEN $6=0 THEN c.current_revision ELSE $6 END`, sc.TenantID, sc.SiteID, id, a.Issuer, a.Subject, revision).Scan(&feedbackID, &out.ProposerIssuer, &out.ProposerSubject, &out.Revision, &out.CreatedAt, &raw, &out.ContentSHA256, &superseded)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if err = json.Unmarshal(raw, &out.Content); err != nil {
		return out, err
	}
	out.SourceFeedbackIDs = append(out.SourceFeedbackIDs, feedbackID)
	out.Original, err = s.loadCorrectionOriginal(ctx, tx, a, sc, feedbackID)
	if err != nil {
		return out, err
	}
	out.MissingRequirements = out.Content.Missing()
	if out.Original.Provenance.RunID == nil || out.Original.Provenance.ProcessSnapshotID == nil {
		out.MissingRequirements = append(out.MissingRequirements, "original_run_snapshot")
	}
	out.ReviewState = "candidate"
	if len(out.MissingRequirements) == 0 {
		out.ReviewState = "in_review"
	}
	if superseded {
		// Design §9: a withdrawn expectation is never promoted; a recorded decision still wins below.
		out.ReviewState = "superseded"
	}
	var review MemoryReview
	err = tx.QueryRowContext(ctx, `SELECT review_id,verdict,reason,actor_issuer,actor_subject,created_at,claim_id::text,claim_revision,canonical_review_id::text FROM supervisor.knowledge_memory_candidate_reviews WHERE tenant_id=$1 AND site_id=$2 AND candidate_id=$3 AND revision=$4`, sc.TenantID, sc.SiteID, id, out.Revision).Scan(&review.ReviewID, &review.Verdict, &review.Reason, &review.ActorIssuer, &review.ActorSubject, &review.CreatedAt, &review.ClaimID, &review.ClaimRevision, &review.CanonicalReviewID)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Review = &review
	out.ReviewState = "rejected"
	if review.Verdict == "approve" {
		out.ReviewState = "approved"
		err = tx.QueryRowContext(ctx, `SELECT s.snapshot_id::text FROM supervisor.knowledge_snapshot_claims c JOIN supervisor.knowledge_process_snapshots s USING(tenant_id,site_id,snapshot_id) WHERE c.tenant_id=$1 AND c.site_id=$2 AND c.claim_id=$3 AND c.claim_revision=$4 AND c.review_id=$5 ORDER BY s.created_at DESC,s.snapshot_id DESC LIMIT 1`, sc.TenantID, sc.SiteID, *review.ClaimID, *review.ClaimRevision, *review.CanonicalReviewID).Scan(&out.PublishedSnapshotID)
		if errors.Is(err, sql.ErrNoRows) {
			err = nil
		}
	}
	return out, err
}

func (s *Service) loadCorrectionOriginal(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, id string) (CorrectionOriginal, error) {
	var out CorrectionOriginal
	err := tx.QueryRowContext(ctx, `SELECT f.subject_kind,f.run_id::text,f.artifact_id::text,f.artifact_sha256,f.terminal_sha256,f.run_refs_sha256,f.process_snapshot_id::text,
 q.body,m.body,
 NOT EXISTS(SELECT 1 FROM jsonb_array_elements(coalesce(m.citations,'[]'::jsonb)) c JOIN supervisor.evidence_spans sp ON sp.tenant_id=f.tenant_id AND sp.site_id=f.site_id AND sp.id::text=c->>'id' JOIN supervisor.evidence_heads h ON (h.tenant_id,h.site_id,h.source_id)=(sp.tenant_id,sp.site_id,sp.source_id) WHERE c->>'kind'='evidence_span' AND (h.revoked OR EXISTS(SELECT 1 FROM supervisor.evidence_revocations v WHERE (v.tenant_id,v.site_id,v.source_id)=(h.tenant_id,h.site_id,h.source_id))))
 AND NOT EXISTS(SELECT 1 FROM supervisor.agent_run_refs r LEFT JOIN supervisor.evidence_heads h ON h.tenant_id=r.tenant_id AND h.site_id=r.site_id AND h.source_id::text=r.ref_id
 WHERE r.tenant_id=f.tenant_id AND r.site_id=f.site_id AND r.run_id=f.run_id AND r.kind='evidence' AND (h.source_id IS NULL OR h.revoked OR h.access_epoch<>r.access_epoch OR EXISTS(SELECT 1 FROM supervisor.evidence_revocations v WHERE (v.tenant_id,v.site_id,v.source_id)=(h.tenant_id,h.site_id,h.source_id))))
 FROM supervisor.feedback f LEFT JOIN supervisor.chat_messages m ON (m.tenant_id,m.site_id,m.conversation_id,m.message_id)=(f.tenant_id,f.site_id,f.conversation_id,f.message_id)
 LEFT JOIN supervisor.chat_messages q ON (q.tenant_id,q.site_id,q.conversation_id,q.message_id)=(m.tenant_id,m.site_id,m.conversation_id,m.reply_to)
 WHERE f.tenant_id=$1 AND f.site_id=$2 AND f.id=$3`, sc.TenantID, sc.SiteID, id).Scan(&out.Kind, &out.Provenance.RunID, &out.Provenance.ArtifactID, &out.Provenance.ArtifactSHA256, &out.Provenance.TerminalSHA256, &out.Provenance.RunRefsSHA256, &out.Provenance.ProcessSnapshotID, &out.Question, &out.Answer, &out.Available)
	if err != nil {
		return out, err
	}
	if out.Available && out.Provenance.RunID != nil {
		rows, e := tx.QueryContext(ctx, `SELECT ref_id,revision,sha256,access_epoch FROM supervisor.agent_run_refs WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3 AND kind='evidence' ORDER BY ref_id`, sc.TenantID, sc.SiteID, *out.Provenance.RunID)
		if e != nil {
			return out, e
		}
		refs := []SnapshotSource{}
		for rows.Next() {
			var r SnapshotSource
			if e = rows.Scan(&r.SourceID, &r.Revision, &r.SHA256, &r.AccessEpoch); e != nil {
				rows.Close()
				return out, e
			}
			refs = append(refs, r)
		}
		if e = errors.Join(rows.Err(), rows.Close()); e != nil {
			return out, e
		}
		for _, r := range refs {
			src, e := evidence.New(s.db).WithSessionPolicy(s.sessions).ReadParticipantTx(ctx, tx, evidence.Actor{Issuer: a.Issuer, Subject: a.Subject, SessionHash: a.SessionHash}, evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, r.SourceID, r.Revision)
			if errors.Is(e, evidence.ErrNotFound) || errors.Is(e, evidence.ErrDenied) {
				out.Available = false
				break
			}
			if e != nil {
				return out, e
			}
			if src.SHA256 != r.SHA256 || src.AccessEpoch != r.AccessEpoch {
				out.Available = false
				break
			}
		}
	}
	if out.Available && out.Kind == "investigation" && out.Provenance.RunID != nil {
		var reasons []byte
		err = tx.QueryRowContext(ctx, `SELECT coalesce(r.outcome_status,''),to_json(r.outcome_reasons),a.body->>'rationale' FROM supervisor.agent_runs r LEFT JOIN supervisor.agent_artifacts a ON (a.tenant_id,a.site_id,a.run_id,a.id)=(r.tenant_id,r.site_id,r.id,r.artifact_id) WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.id=$3`, sc.TenantID, sc.SiteID, *out.Provenance.RunID).Scan(&out.OutcomeStatus, &reasons, &out.InvestigationRationale)
		if err != nil {
			return out, err
		}
		if err = json.Unmarshal(reasons, &out.OutcomeReasons); err != nil {
			return out, err
		}
	}
	if !out.Available {
		out.Question = nil
		out.Answer = nil
	}
	return out, err
}

func (s *Service) ReadMemoryCandidate(ctx context.Context, a Actor, sc Scope, id string) (MemoryCandidate, error) {
	if !contractUUID(id) {
		return MemoryCandidate{}, ErrInvalidContract
	}
	tx, err := s.begin(ctx, a, sc, false)
	if err != nil {
		return MemoryCandidate{}, err
	}
	defer tx.Rollback()
	out, err := s.loadMemory(ctx, tx, a, sc, id, 0)
	if err != nil {
		return out, err
	}
	if err = audit(ctx, tx, a, sc, id, out.Revision, "memory.read"); err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}

func (s *Service) ReviseMemoryCandidate(ctx context.Context, a Actor, sc Scope, id string, in MemoryRevisionInput, key string, expected int64) (MemoryCandidate, error) {
	var out MemoryCandidate
	if !contractUUID(id) || !contractUUID(key) || expected < 1 {
		return out, ErrInvalidContract
	}
	if err := in.Validate(); err != nil {
		return out, err
	}
	tx, err := s.beginLocked(ctx, a, sc, true, true)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	out, err = s.loadMemory(ctx, tx, a, sc, id, 0)
	if err != nil {
		return out, err
	}
	if out.ProposerIssuer != a.Issuer || out.ProposerSubject != a.Subject {
		return out, ErrDenied
	}
	digest := fingerprint("memory_revision", struct {
		ID    string
		Input MemoryRevisionInput
	}{id, in}, expected)
	prior, replayed, err := replay(ctx, tx, a, sc, key, digest)
	if err != nil {
		return out, err
	}
	if replayed {
		out, err = s.loadMemory(ctx, tx, a, sc, id, prior)
		if err != nil {
			return out, err
		}
		return out, s.finish(ctx, tx, a)
	}
	if out.Revision != expected {
		return out, ErrRevision
	}
	if out.Review != nil {
		return out, ErrReviewConflict
	}
	if err = validateMemoryReferences(ctx, tx, sc, out.SourceFeedbackIDs[0], in); err != nil {
		return out, err
	}
	if err = appendMemoryRevision(ctx, tx, a, sc, id, expected+1, in); err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE supervisor.knowledge_memory_candidates SET current_revision=current_revision+1 WHERE tenant_id=$1 AND site_id=$2 AND candidate_id=$3 AND current_revision=$4`, sc.TenantID, sc.SiteID, id, expected); err != nil {
		return out, err
	}
	if err = saveReceipt(ctx, tx, a, sc, key, digest, "memory_revision", id, expected+1); err != nil {
		return out, err
	}
	if err = audit(ctx, tx, a, sc, id, expected+1, "memory.revised"); err != nil {
		return out, err
	}
	out, err = s.loadMemory(ctx, tx, a, sc, id, 0)
	if err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}

func (s *Service) ListMemoryCandidates(ctx context.Context, a Actor, sc Scope, state, cursor string, limit int) (MemoryCandidatePage, error) {
	out := MemoryCandidatePage{Candidates: []MemoryCandidate{}}
	if limit < 1 || limit > 100 || (state != "" && state != "candidate" && state != "in_review" && state != "approved" && state != "rejected" && state != "superseded") {
		return out, ErrInvalidContract
	}
	at := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	after := "ffffffff-ffff-ffff-ffff-ffffffffffff"
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		parts := strings.Split(string(raw), "|")
		if err != nil || len(parts) != 2 || !contractUUID(parts[1]) {
			return out, ErrInvalidContract
		}
		at, err = time.Parse(time.RFC3339Nano, parts[0])
		if err != nil {
			return out, ErrInvalidContract
		}
		after = parts[1]
	}
	tx, err := s.begin(ctx, a, sc, false)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	// Completeness is derived from the stored envelope and original pins, as on single reads
	// (MemoryRevisionInput.Missing: char_length counts characters, like utf8.RuneCountInString).
	rows, err := tx.QueryContext(ctx, `SELECT c.candidate_id::text FROM supervisor.knowledge_memory_candidates c JOIN supervisor.knowledge_memory_candidate_revisions v ON (v.tenant_id,v.site_id,v.candidate_id,v.revision)=(c.tenant_id,c.site_id,c.candidate_id,c.current_revision)
 JOIN supervisor.feedback f ON (f.tenant_id,f.site_id,f.id)=(c.tenant_id,c.site_id,c.source_feedback_id)
 LEFT JOIN supervisor.knowledge_memory_candidate_reviews d ON (d.tenant_id,d.site_id,d.candidate_id,d.revision)=(c.tenant_id,c.site_id,c.candidate_id,c.current_revision)
 WHERE c.tenant_id=$1 AND c.site_id=$2 AND ($3='' OR $3=CASE WHEN d.verdict='approve' THEN 'approved' WHEN d.verdict='reject' THEN 'rejected' WHEN `+memorySuperseded+` THEN 'superseded' WHEN v.payload->'claim' IS NOT NULL AND btrim(v.payload->>'question')<>'' AND btrim(v.payload->>'expected_answer')<>'' AND char_length(v.payload->>'expected_answer')<=1000 AND f.run_id IS NOT NULL AND f.process_snapshot_id IS NOT NULL THEN 'in_review' ELSE 'candidate' END)
 AND `+memoryVisible+` AND (c.created_at,c.candidate_id)<($6,$7::uuid) ORDER BY c.created_at DESC,c.candidate_id DESC LIMIT $8`, sc.TenantID, sc.SiteID, state, a.Issuer, a.Subject, at, after, limit+1)
	if err != nil {
		return out, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return out, err
		}
		ids = append(ids, id)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return out, err
	}
	more := len(ids) > limit
	if more {
		ids = ids[:limit]
	}
	for _, id := range ids {
		v, e := s.loadMemory(ctx, tx, a, sc, id, 0)
		if e != nil {
			return out, e
		}
		out.Candidates = append(out.Candidates, v)
	}
	if more {
		last := out.Candidates[len(out.Candidates)-1]
		out.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(last.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + last.CandidateID))
	}
	if err = audit(ctx, tx, a, sc, nil, nil, "memory.listed"); err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}
