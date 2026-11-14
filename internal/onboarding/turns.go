package onboarding

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/approvals"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
)

const (
	interviewTurnDeliveredReapAfter   = agent.InterviewTurnScheduleToClose + time.Minute
	interviewTurnUndeliveredReapAfter = 30 * time.Minute
)

type StartAgentTurnInput struct {
	RequestID uuid.UUID `json:"request_id"`
	// SourceSpanIDs are selected passages (evidence spans) the turn may read, in order, instead of
	// their whole sources (WIS-614): a document larger than one model request is read a few
	// passages at a time. Each must be a span of the current revision of a source the participant
	// may read, and together they hold at most MaxTurnPassageBytes of text.
	SourceSpanIDs []uuid.UUID `json:"source_span_ids,omitempty"`
}

// MaxTurnPassageBytes bounds the text of the passages given to one agent turn, so they fit one
// model request next to the prompt, the requirements and the answers.
const MaxTurnPassageBytes = 12 << 10

// ErrSourceChanged: a selected passage belongs to a source revision that is no longer current.
var ErrSourceChanged = errors.New("selected passage source changed")

type AgentTurnSettings struct {
	Provider, Model, ModelRevision string
	ProviderActual                 bool
	Enabled                        bool
}

type AgentTurn struct {
	RunID          string   `json:"run_id"`
	State          string   `json:"state"`
	OutcomeStatus  string   `json:"outcome_status,omitempty"`
	OutcomeReasons []string `json:"outcome_reasons"`
	QuestionIDs    []string `json:"question_ids"`
	ClaimIDs       []string `json:"claim_ids"`
	// EntityIDs are the candidate entities the turn proposed (WIS-612); like the claims they await
	// review.
	EntityIDs []string `json:"entity_ids"`
}

type StartConfigurationDraftsInput struct {
	SessionID uuid.UUID               `json:"session_id"`
	SourceIDs []uuid.UUID             `json:"source_ids"`
	Kinds     []workstation.DraftKind `json:"kinds"`
}

type ConfigurationDraftRequest struct {
	RunID string `json:"run_id"`
}

func (in StartAgentTurnInput) Validate() error {
	if in.RequestID == uuid.Nil || len(in.SourceSpanIDs) > agent.MaxRunPassages {
		return ErrInterviewInvalid
	}
	seen := map[uuid.UUID]bool{}
	for _, id := range in.SourceSpanIDs {
		if id == uuid.Nil || seen[id] {
			return ErrInterviewInvalid
		}
		seen[id] = true
	}
	return nil
}

// StartAgentTurn persists a participant-authorized conversation run and its captured scope.
func (s *Service) StartAgentTurn(ctx context.Context, a Actor, sc Scope, id string, in StartAgentTurnInput, key string, expected int64, settings AgentTurnSettings) (AgentTurn, error) {
	if !validID(id) || in.Validate() != nil || !validMutationKey(key) || expected < 1 || !settings.Enabled || settings.Provider == "" || settings.Model == "" || settings.ModelRevision == "" {
		return AgentTurn{}, ErrInterviewInvalid
	}
	tx, memberRole, err := s.begin(ctx, a, sc, true)
	if err != nil {
		return AgentTurn{}, err
	}
	defer tx.Rollback()
	if memberRole != string(RoleOwner) && memberRole != string(RoleSupervisor) {
		return AgentTurn{}, ErrDenied
	}
	current, err := loadSession(ctx, tx, sc, a, id, true)
	if err != nil {
		return AgentTurn{}, err
	}
	// A turn without passages keeps its original fingerprint, so a replay of a key stored before
	// passages existed still matches.
	var digest string
	if len(in.SourceSpanIDs) == 0 {
		digest = requestHash("agent_turn", struct {
			RequestID string
			Expected  int64
		}{in.RequestID.String(), expected})
	} else {
		digest = requestHash("agent_turn", struct {
			RequestID     string
			Expected      int64
			SourceSpanIDs []uuid.UUID
		}{in.RequestID.String(), expected, in.SourceSpanIDs})
	}
	if body, replayed, e := receipt(ctx, tx, a, sc, key, digest); e != nil {
		return AgentTurn{}, e
	} else if replayed {
		var out AgentTurn
		if e = json.Unmarshal(body, &out); e != nil {
			return AgentTurn{}, e
		}
		if e = audit(ctx, tx, a, sc, id, nil, "agent_turn.replayed"); e != nil {
			return AgentTurn{}, e
		}
		return out, s.finish(ctx, tx, a)
	}
	if current.Status != StatusActive {
		return AgentTurn{}, ErrLifecycle
	}
	if current.Revision != expected {
		return AgentTurn{}, ErrRevision
	}
	if err = reapStaleAgentTurns(ctx, tx, sc, id); err != nil {
		return AgentTurn{}, err
	}
	var running bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.interview_agent_turns t JOIN supervisor.agent_runs r ON (r.tenant_id,r.site_id,r.id)=(t.tenant_id,t.site_id,t.run_id)
 WHERE t.tenant_id=$1 AND t.site_id=$2 AND t.session_id=$3 AND r.state='running')`, sc.TenantID, sc.SiteID, id).Scan(&running); err != nil {
		return AgentTurn{}, err
	}
	if running {
		return AgentTurn{}, ErrLifecycle
	}
	if current.inventoryID == "" || current.inventoryRevision < 1 {
		return AgentTurn{}, ErrInventoryChanged
	}
	sources := evidence.New(s.db).WithSessionPolicy(s.sessions)
	passages, err := selectPassages(ctx, tx, a, sc, sources, in.SourceSpanIDs)
	if err != nil {
		return AgentTurn{}, err
	}
	var passageSources []string
	for _, p := range passages {
		if !slices.Contains(passageSources, p.SourceID) {
			passageSources = append(passageSources, p.SourceID)
		}
	}
	refs, err := captureInterviewRefs(ctx, tx, a, sc, current, sources, passageSources)
	if err != nil {
		if errors.Is(err, evidence.ErrNotFound) {
			return AgentTurn{}, ErrNotFound
		}
		if errors.Is(err, evidence.ErrDenied) {
			return AgentTurn{}, ErrDenied
		}
		return AgentTurn{}, err
	}
	run := agent.Run{ID: uuid.NewString(), RequestKind: agent.RequestConversation, ConversationID: id, RequestID: in.RequestID.String(), State: agent.RunRunning,
		TraceID: agent.NewTraceID(), ProviderName: settings.Provider, ProviderActual: settings.ProviderActual, Model: settings.Model, ModelRevision: settings.ModelRevision,
		PromptVersion: agent.InterviewPromptVersion, ToolVersion: agent.ToolVersion, PolicyRevision: approvals.PolicyRevision}
	if err = agent.CreateRunTx(ctx, tx, agent.RunScope{TenantID: sc.TenantID, SiteID: sc.SiteID}, run, agent.InterviewLimits()); err != nil {
		return AgentTurn{}, err
	}
	if err = agent.ReserveInvocationTx(ctx, tx, agent.RunScope{TenantID: sc.TenantID, SiteID: sc.SiteID}, run.ID); err != nil {
		return AgentTurn{}, err
	}
	if err = agent.AddRefsTx(ctx, tx, agent.RunScope{TenantID: sc.TenantID, SiteID: sc.SiteID}, run.ID, refs); err != nil {
		return AgentTurn{}, err
	}
	if err = agent.AddPassagesTx(ctx, tx, agent.RunScope{TenantID: sc.TenantID, SiteID: sc.SiteID}, run.ID, passages); err != nil {
		return AgentTurn{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.interview_agent_turns(tenant_id,site_id,session_id,run_id,session_revision,purpose) VALUES($1,$2,$3,$4,$5,'questions')`, sc.TenantID, sc.SiteID, id, run.ID, current.Revision); err != nil {
		return AgentTurn{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.interview_agent_turn_outbox(tenant_id,site_id,id,session_id,run_id) VALUES($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, uuid.NewString(), id, run.ID); err != nil {
		return AgentTurn{}, err
	}
	out := AgentTurn{RunID: run.ID, State: agent.RunRunning, OutcomeReasons: []string{}, QuestionIDs: []string{}, ClaimIDs: []string{}, EntityIDs: []string{}}
	if err = audit(ctx, tx, a, sc, id, nil, "agent_turn.started"); err != nil {
		return AgentTurn{}, err
	}
	if err = saveReceipt(ctx, tx, a, sc, key, "agent_turn", digest, id, out); err != nil {
		return AgentTurn{}, err
	}
	return out, s.finish(ctx, tx, a)
}

func (s *Service) StartConfigurationDrafts(ctx context.Context, a Actor, sc Scope, in StartConfigurationDraftsInput, key string, settings AgentTurnSettings) (ConfigurationDraftRequest, error) {
	if in.SessionID == uuid.Nil || !validMutationKey(key) || !settings.Enabled || settings.Provider == "" || settings.Model == "" || settings.ModelRevision == "" || len(in.SourceIDs) > 20 || len(in.Kinds) == 0 || len(in.Kinds) > len(workstation.DraftKinds()) {
		return ConfigurationDraftRequest{}, ErrInterviewInvalid
	}
	seenSources := map[uuid.UUID]bool{}
	for _, id := range in.SourceIDs {
		if id == uuid.Nil || seenSources[id] {
			return ConfigurationDraftRequest{}, ErrInterviewInvalid
		}
		seenSources[id] = true
	}
	allowedKinds := map[workstation.DraftKind]bool{}
	for _, kind := range workstation.DraftKinds() {
		allowedKinds[kind] = true
	}
	seenKinds := map[workstation.DraftKind]bool{}
	for _, kind := range in.Kinds {
		if !allowedKinds[kind] || seenKinds[kind] {
			return ConfigurationDraftRequest{}, ErrInterviewInvalid
		}
		seenKinds[kind] = true
	}
	tx, memberRole, err := s.begin(ctx, a, sc, true)
	if err != nil {
		return ConfigurationDraftRequest{}, err
	}
	defer tx.Rollback()
	if memberRole != string(RoleOwner) && memberRole != string(RoleSupervisor) {
		return ConfigurationDraftRequest{}, ErrDenied
	}
	digest := requestHash("configuration_draft_request", in)
	var savedHash, savedRun string
	err = tx.QueryRowContext(ctx, `SELECT request_sha256,run_id::text FROM supervisor.configuration_draft_request_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&savedHash, &savedRun)
	if err == nil {
		if savedHash != digest {
			return ConfigurationDraftRequest{}, ErrConflict
		}
		return ConfigurationDraftRequest{RunID: savedRun}, s.finish(ctx, tx, a)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ConfigurationDraftRequest{}, err
	}
	current, err := loadSession(ctx, tx, sc, a, in.SessionID.String(), true)
	if err != nil {
		return ConfigurationDraftRequest{}, err
	}
	if current.Status != StatusActive {
		return ConfigurationDraftRequest{}, ErrLifecycle
	}
	if err = reapStaleAgentTurns(ctx, tx, sc, in.SessionID.String()); err != nil {
		return ConfigurationDraftRequest{}, err
	}
	var running bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.interview_agent_turns t JOIN supervisor.agent_runs r ON (r.tenant_id,r.site_id,r.id)=(t.tenant_id,t.site_id,t.run_id) WHERE t.tenant_id=$1 AND t.site_id=$2 AND t.session_id=$3 AND r.state='running')`, sc.TenantID, sc.SiteID, in.SessionID).Scan(&running); err != nil {
		return ConfigurationDraftRequest{}, err
	}
	if running {
		return ConfigurationDraftRequest{}, ErrLifecycle
	}
	sourceIDs := make([]string, len(in.SourceIDs))
	for i, id := range in.SourceIDs {
		sourceIDs[i] = id.String()
	}
	refs, err := captureInterviewRefs(ctx, tx, a, sc, current, evidence.New(s.db).WithSessionPolicy(s.sessions), sourceIDs)
	if err != nil {
		if errors.Is(err, evidence.ErrNotFound) {
			return ConfigurationDraftRequest{}, ErrNotFound
		}
		if errors.Is(err, evidence.ErrDenied) {
			return ConfigurationDraftRequest{}, ErrDenied
		}
		return ConfigurationDraftRequest{}, err
	}
	run := agent.Run{ID: uuid.NewString(), RequestKind: agent.RequestConversation, ConversationID: in.SessionID.String(), RequestID: uuid.NewString(), State: agent.RunRunning,
		TraceID: agent.NewTraceID(), ProviderName: settings.Provider, ProviderActual: settings.ProviderActual, Model: settings.Model, ModelRevision: settings.ModelRevision,
		PromptVersion: agent.DraftPromptVersion, ToolVersion: agent.ToolVersion, PolicyRevision: approvals.PolicyRevision}
	runScope := agent.RunScope{TenantID: sc.TenantID, SiteID: sc.SiteID}
	if err = agent.CreateRunTx(ctx, tx, runScope, run, agent.InterviewLimits()); err != nil {
		return ConfigurationDraftRequest{}, err
	}
	if err = agent.ReserveInvocationTx(ctx, tx, runScope, run.ID); err != nil {
		return ConfigurationDraftRequest{}, err
	}
	if err = agent.AddRefsTx(ctx, tx, runScope, run.ID, refs); err != nil {
		return ConfigurationDraftRequest{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.interview_agent_turns(tenant_id,site_id,session_id,run_id,session_revision,purpose) VALUES($1,$2,$3,$4,$5,'drafts')`, sc.TenantID, sc.SiteID, in.SessionID, run.ID, current.Revision); err != nil {
		return ConfigurationDraftRequest{}, err
	}
	kinds := make([]string, len(in.Kinds))
	for i, kind := range in.Kinds {
		kinds[i] = string(kind)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.configuration_draft_requests(tenant_id,site_id,session_id,run_id,requested_by_issuer,requested_by_subject,source_ids,kinds,request_sha256) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, sc.TenantID, sc.SiteID, in.SessionID, run.ID, a.Issuer, a.Subject, sourceIDs, kinds, digest); err != nil {
		return ConfigurationDraftRequest{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.configuration_draft_request_outbox(tenant_id,site_id,id,session_id,run_id) VALUES($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, uuid.NewString(), in.SessionID, run.ID); err != nil {
		return ConfigurationDraftRequest{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.configuration_draft_request_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_sha256,run_id) VALUES($1,$2,$3,$4,$5,$6,$7)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, digest, run.ID); err != nil {
		return ConfigurationDraftRequest{}, err
	}
	return ConfigurationDraftRequest{RunID: run.ID}, s.finish(ctx, tx, a)
}

// selectPassages resolves the selected spans of an agent turn, in their requested order. A span
// that does not exist in the site, or whose source the participant may not read (including a
// revoked one), is the same non-disclosing not-found; a span of a revision that is no longer
// current is ErrSourceChanged; passages over MaxTurnPassageBytes in total are ErrTooLarge.
func selectPassages(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, sources *evidence.Service, ids []uuid.UUID) ([]agent.RunPassage, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	type span struct {
		sourceID   string
		revision   int64
		current    int64
		start, end int
	}
	rows, err := tx.QueryContext(ctx, `SELECT s.id,s.source_id::text,s.revision,h.current_revision,s.start_rune,s.end_rune FROM supervisor.evidence_spans s
 JOIN supervisor.evidence_heads h ON (h.tenant_id,h.site_id,h.source_id)=(s.tenant_id,s.site_id,s.source_id)
 WHERE s.tenant_id=$1 AND s.site_id=$2 AND s.id=ANY($3::uuid[])`, sc.TenantID, sc.SiteID, ids)
	if err != nil {
		return nil, err
	}
	found := map[uuid.UUID]span{}
	for rows.Next() {
		var id uuid.UUID
		var sp span
		if err = rows.Scan(&id, &sp.sourceID, &sp.revision, &sp.current, &sp.start, &sp.end); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		found[id] = sp
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if len(found) != len(ids) {
		return nil, ErrNotFound
	}
	texts := map[string][]rune{}
	out := make([]agent.RunPassage, 0, len(ids))
	total := 0
	for _, id := range ids {
		sp := found[id]
		text, ok := texts[sp.sourceID]
		if !ok {
			source, e := sources.ReadParticipantTx(ctx, tx, evidence.Actor{Issuer: a.Issuer, Subject: a.Subject, SessionHash: a.SessionHash}, evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, sp.sourceID, sp.current)
			if errors.Is(e, evidence.ErrNotFound) {
				return nil, ErrNotFound
			}
			if errors.Is(e, evidence.ErrDenied) {
				return nil, ErrDenied
			}
			if e != nil {
				return nil, e
			}
			text = []rune(source.Text)
			texts[sp.sourceID] = text
		}
		if sp.revision != sp.current {
			return nil, ErrSourceChanged
		}
		if sp.start < 0 || sp.end > len(text) || sp.start >= sp.end {
			return nil, ErrStoredCorrupt
		}
		if total += len(string(text[sp.start:sp.end])); total > MaxTurnPassageBytes {
			return nil, ErrTooLarge
		}
		out = append(out, agent.RunPassage{SpanID: id.String(), SourceID: sp.sourceID, SourceRevision: sp.revision})
	}
	return out, nil
}

func reapStaleAgentTurns(ctx context.Context, tx *sql.Tx, sc Scope, sessionID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT r.id::text FROM supervisor.interview_agent_turns t
JOIN supervisor.agent_runs r ON (r.tenant_id,r.site_id,r.id)=(t.tenant_id,t.site_id,t.run_id)
LEFT JOIN supervisor.interview_agent_turn_outbox io ON (io.tenant_id,io.site_id,io.session_id,io.run_id)=(t.tenant_id,t.site_id,t.session_id,t.run_id)
LEFT JOIN supervisor.configuration_draft_request_outbox co ON (co.tenant_id,co.site_id,co.session_id,co.run_id)=(t.tenant_id,t.site_id,t.session_id,t.run_id)
WHERE t.tenant_id=$1 AND t.site_id=$2 AND t.session_id=$3 AND r.state='running'
AND ((COALESCE(io.delivered_at,co.delivered_at) IS NOT NULL AND COALESCE(io.delivered_at,co.delivered_at) < clock_timestamp() - ($4::bigint * interval '1 second'))
 OR (io.delivered_at IS NULL AND co.delivered_at IS NULL AND r.started_at < clock_timestamp() - ($5::bigint * interval '1 second')))
ORDER BY r.id FOR UPDATE OF r`, sc.TenantID, sc.SiteID, sessionID, int64(interviewTurnDeliveredReapAfter/time.Second), int64(interviewTurnUndeliveredReapAfter/time.Second))
	if err != nil {
		return err
	}
	var staleRunIDs []string
	for rows.Next() {
		var runID string
		if err = rows.Scan(&runID); err != nil {
			return errors.Join(err, rows.Close())
		}
		staleRunIDs = append(staleRunIDs, runID)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, runID := range staleRunIDs {
		if err = agent.FinishRunTx(ctx, tx, agent.RunScope{TenantID: sc.TenantID, SiteID: sc.SiteID}, runID, agent.Finish{State: agent.RunCompleted, OutcomeStatus: recovery.StatusNeedsInput, OutcomeReasons: []string{agent.CodeActivityFailed}}); err != nil {
			return err
		}
	}
	return nil
}

func captureInterviewRefs(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, in InterviewSession, sources *evidence.Service, selectedSourceIDs []string) ([]agent.RunRef, error) {
	// Answers and selected sources together remain under the requesting participant's evidence authority.
	rows, err := tx.QueryContext(ctx, `WITH candidates AS (
 SELECT source_id,source_revision FROM supervisor.interview_answers WHERE tenant_id=$1 AND site_id=$2 AND session_id=$3
 UNION ALL
 SELECT r.source_id,r.source_revision FROM supervisor.knowledge_entity_revisions r JOIN supervisor.knowledge_entity_heads h ON (h.tenant_id,h.site_id,h.id,h.current_revision)=(r.tenant_id,r.site_id,r.id,r.revision)
 WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.id=$4
 UNION ALL
 SELECT s.source_id,s.source_revision FROM supervisor.knowledge_claim_heads h JOIN supervisor.knowledge_claim_spans s ON (s.tenant_id,s.site_id,s.claim_id,s.claim_revision)=(h.tenant_id,h.site_id,h.id,h.current_revision)
 WHERE h.tenant_id=$1 AND h.site_id=$2 AND (h.id IN (SELECT claim_id FROM supervisor.knowledge_claim_entity_refs WHERE tenant_id=$1 AND site_id=$2 AND subject_id=$4)
 OR h.id IN (SELECT claim_id FROM supervisor.knowledge_claim_entity_refs WHERE tenant_id=$1 AND site_id=$2 AND object_entity_id=$4))
 UNION ALL
 SELECT h.source_id,h.current_revision FROM supervisor.evidence_heads h WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.source_id=ANY($5::uuid[]) AND NOT h.revoked
), latest AS (
 SELECT DISTINCT ON (c.source_id) c.source_id,c.source_revision,r.sha256,r.created_at,e.access_epoch
 FROM candidates c JOIN supervisor.evidence_revisions r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=($1,$2,c.source_id,c.source_revision)
 JOIN supervisor.evidence_heads e ON (e.tenant_id,e.site_id,e.source_id)=($1,$2,c.source_id)
 ORDER BY c.source_id,c.source_revision DESC
), limited AS (
 SELECT * FROM latest ORDER BY created_at DESC,source_id LIMIT 50
), selected AS (
 SELECT l.* FROM latest l WHERE l.source_id=ANY($5::uuid[])
 AND NOT EXISTS(SELECT 1 FROM limited x WHERE x.source_id=l.source_id)
), refs AS (
 SELECT * FROM limited UNION ALL SELECT * FROM selected
)
SELECT source_id::text,source_revision,sha256,access_epoch FROM refs ORDER BY created_at DESC,source_id`, sc.TenantID, sc.SiteID, in.SessionID.String(), nullableID(in.ProcessID), selectedSourceIDs)
	if err != nil {
		return nil, err
	}
	candidates := []agent.RunRef{}
	for rows.Next() {
		var r agent.RunRef
		r.Kind = agent.RefEvidence
		if err = rows.Scan(&r.RefID, &r.Revision, &r.SHA256, &r.AccessEpoch); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		candidates = append(candidates, r)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	refs := []agent.RunRef{}
	required := make(map[string]bool, len(selectedSourceIDs))
	for _, id := range selectedSourceIDs {
		required[id] = true
	}
	for _, r := range candidates {
		source, e := sources.ReadParticipantTx(ctx, tx, evidence.Actor{Issuer: a.Issuer, Subject: a.Subject, SessionHash: a.SessionHash}, evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, r.RefID, r.Revision)
		if errors.Is(e, evidence.ErrNotFound) {
			continue
		}
		if e != nil {
			return nil, e
		}
		if source.Revision != r.Revision || source.SHA256 != r.SHA256 {
			return nil, ErrInventoryChanged
		}
		r.AccessEpoch = source.AccessEpoch
		refs = append(refs, r)
		delete(required, r.RefID)
	}
	if len(required) > 0 {
		return nil, evidence.ErrNotFound
	}
	var snapshotID, digest sql.NullString
	var revision sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT snapshot_id::text,inventory_revision,sha256 FROM supervisor.knowledge_process_snapshots WHERE tenant_id=$1 AND site_id=$2 ORDER BY created_at DESC,snapshot_id DESC LIMIT 1`, sc.TenantID, sc.SiteID).Scan(&snapshotID, &revision, &digest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if snapshotID.Valid {
		refs = append(refs, agent.RunRef{Kind: agent.RefSnapshot, RefID: snapshotID.String, Revision: revision.Int64, SHA256: digest.String})
	}
	return refs, nil
}

func nullableID(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return id.String()
}

func (s *Service) GetAgentTurn(ctx context.Context, a Actor, sc Scope, id, runID string) (AgentTurn, error) {
	if !validID(id) || !validID(runID) {
		return AgentTurn{}, ErrInterviewInvalid
	}
	tx, memberRole, err := s.begin(ctx, a, sc, false)
	if err != nil {
		return AgentTurn{}, err
	}
	defer tx.Rollback()
	if memberRole != string(RoleOwner) && memberRole != string(RoleSupervisor) {
		return AgentTurn{}, ErrDenied
	}
	if _, err = loadSession(ctx, tx, sc, a, id, false); err != nil {
		return AgentTurn{}, err
	}
	// A stuck run reads back terminal with its reason; the UI never waits on it forever.
	if err = reapStaleAgentTurns(ctx, tx, sc, id); err != nil {
		return AgentTurn{}, err
	}
	var out AgentTurn
	var reasons []string
	var artifactID sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT r.state,COALESCE(r.outcome_status,''),r.outcome_reasons,r.artifact_id::text FROM supervisor.interview_agent_turns t JOIN supervisor.agent_runs r ON (r.tenant_id,r.site_id,r.id)=(t.tenant_id,t.site_id,t.run_id)
 WHERE t.tenant_id=$1 AND t.site_id=$2 AND t.session_id=$3 AND t.run_id=$4 AND t.purpose='questions'`, sc.TenantID, sc.SiteID, id, runID).Scan(&out.State, &out.OutcomeStatus, &reasons, &artifactID)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentTurn{}, ErrNotFound
	}
	if err != nil {
		return AgentTurn{}, err
	}
	out.RunID, out.OutcomeReasons, out.QuestionIDs, out.ClaimIDs, out.EntityIDs = runID, reasons, []string{}, []string{}, []string{}
	if artifactID.Valid {
		var body string
		if err = tx.QueryRowContext(ctx, `SELECT body::text FROM supervisor.agent_artifacts WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, artifactID.String).Scan(&body); err != nil {
			return AgentTurn{}, err
		}
		var artifact agent.InterviewTurnArtifact
		if json.Unmarshal([]byte(body), &artifact) != nil {
			return AgentTurn{}, ErrStoredCorrupt
		}
		out.QuestionIDs, out.ClaimIDs = artifact.QuestionIDs, artifact.ClaimIDs
		if out.QuestionIDs == nil {
			out.QuestionIDs = []string{}
		}
		if out.ClaimIDs == nil {
			out.ClaimIDs = []string{}
		}
		if artifact.EntityIDs != nil {
			out.EntityIDs = artifact.EntityIDs
		}
	}
	if err = auditRead(ctx, tx, a, sc, id, "agent_turn.read"); err != nil {
		return AgentTurn{}, err
	}
	return out, s.finish(ctx, tx, a)
}

// ConfigurationDraftRun is the requester's view of a configuration draft run: its state, the
// specific refusal or needs_input reason (rule 06), and the drafts it created, if any.
type ConfigurationDraftRun struct {
	RunID          string   `json:"run_id"`
	SessionID      string   `json:"session_id"`
	State          string   `json:"state"`
	OutcomeStatus  string   `json:"outcome_status,omitempty"`
	OutcomeReasons []string `json:"outcome_reasons"`
	DraftIDs       []string `json:"draft_ids"`
}

// GetConfigurationDraftRun reads a draft run for its requester only. Another member, another site
// or tenant, and a question-turn run ID all read as the same non-disclosing not-found.
func (s *Service) GetConfigurationDraftRun(ctx context.Context, a Actor, sc Scope, runID string) (ConfigurationDraftRun, error) {
	if !validID(runID) {
		return ConfigurationDraftRun{}, ErrInterviewInvalid
	}
	tx, memberRole, err := s.begin(ctx, a, sc, false)
	if err != nil {
		return ConfigurationDraftRun{}, err
	}
	defer tx.Rollback()
	if memberRole != string(RoleOwner) && memberRole != string(RoleSupervisor) {
		return ConfigurationDraftRun{}, ErrDenied
	}
	out := ConfigurationDraftRun{RunID: runID, DraftIDs: []string{}}
	err = tx.QueryRowContext(ctx, `SELECT q.session_id::text FROM supervisor.configuration_draft_requests q
 JOIN supervisor.interview_agent_turns t ON (t.tenant_id,t.site_id,t.session_id,t.run_id)=(q.tenant_id,q.site_id,q.session_id,q.run_id) AND t.purpose='drafts'
 WHERE q.tenant_id=$1 AND q.site_id=$2 AND q.run_id=$3 AND q.requested_by_issuer=$4 AND q.requested_by_subject=$5`, sc.TenantID, sc.SiteID, runID, a.Issuer, a.Subject).Scan(&out.SessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return ConfigurationDraftRun{}, ErrNotFound
	}
	if err != nil {
		return ConfigurationDraftRun{}, err
	}
	// The requester must still be the session's participant.
	if _, err = loadSession(ctx, tx, sc, a, out.SessionID, false); err != nil {
		return ConfigurationDraftRun{}, err
	}
	// A stuck run reads back terminal with its reason; the UI never waits on it forever.
	if err = reapStaleAgentTurns(ctx, tx, sc, out.SessionID); err != nil {
		return ConfigurationDraftRun{}, err
	}
	var artifactID sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT state,COALESCE(outcome_status,''),outcome_reasons,artifact_id::text FROM supervisor.agent_runs WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, runID).
		Scan(&out.State, &out.OutcomeStatus, &out.OutcomeReasons, &artifactID); err != nil {
		return ConfigurationDraftRun{}, err
	}
	if out.OutcomeReasons == nil {
		out.OutcomeReasons = []string{}
	}
	if artifactID.Valid {
		var body string
		if err = tx.QueryRowContext(ctx, `SELECT body::text FROM supervisor.agent_artifacts WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, artifactID.String).Scan(&body); err != nil {
			return ConfigurationDraftRun{}, err
		}
		var artifact agent.ConfigurationDraftArtifact
		if json.Unmarshal([]byte(body), &artifact) != nil || artifact.DraftIDs == nil {
			return ConfigurationDraftRun{}, ErrStoredCorrupt
		}
		out.DraftIDs = artifact.DraftIDs
	}
	// A draft run is an interview agent turn (purpose 'drafts'); it shares the turn read event.
	if err = auditRead(ctx, tx, a, sc, out.SessionID, "agent_turn.read"); err != nil {
		return ConfigurationDraftRun{}, err
	}
	return out, s.finish(ctx, tx, a)
}

// ErrStoredCorrupt is a stored artifact that no longer decodes: a server fault, never the caller's.
var ErrStoredCorrupt = errors.New("stored interview artifact is corrupt")

// auditRead records a polled read at most once per actor, session and event per minute. Run
// state changes are recorded on the run itself (agent_runs); this bounds a polling client to
// 60 audit rows an hour instead of one per poll.
func auditRead(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, sessionID, event string) error {
	_, err := tx.ExecContext(ctx, AuditReadSQL, sc.TenantID, sc.SiteID, sessionID, a.Issuer, a.Subject, event)
	return err
}

// AuditReadSQL is auditRead's statement, exported so the migration 060 test plans exactly it. The
// window uses now() (this request's transaction start, milliseconds from the read) rather than the
// volatile clock_timestamp(), so the created_at range can be an index condition.
const AuditReadSQL = `INSERT INTO supervisor.interview_audit(tenant_id,site_id,session_id,question_id,actor_issuer,actor_subject,event_type)
 SELECT $1,$2,$3,NULL,$4,$5,$6 WHERE NOT EXISTS(SELECT 1 FROM supervisor.interview_audit WHERE tenant_id=$1 AND site_id=$2 AND session_id=$3
 AND actor_issuer=$4 AND actor_subject=$5 AND event_type=$6 AND created_at > now()-interval '1 minute')`
