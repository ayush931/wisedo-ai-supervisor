package feedback

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/chat"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/membership"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
)

type Principal struct{ Issuer, Subject, SessionHash string }
type Scope struct{ TenantID, SiteID string }

type Service struct {
	db       *sql.DB
	sessions session.Policy
}

func New(db *sql.DB) *Service { return &Service{db: db, sessions: session.Default()} }

// WithSessionPolicy returns a copy that enforces p (WIS-290 shared session policy).
func (s *Service) WithSessionPolicy(p session.Policy) *Service {
	c := *s
	c.sessions = p.OrDefault()
	return &c
}

func validField(v string, max int) bool {
	return len(v) > 0 && len(v) <= max && strings.TrimSpace(v) == v && utf8.ValidString(v) && !strings.ContainsRune(v, 0)
}

// begin opens a tenant transaction for a live session of an active site member and returns the
// member's role. The site is locked for share: feedback never serializes the site's writers; the
// evidence heads it binds are locked for share instead, which a revocation waits for.
func (s *Service) begin(ctx context.Context, a Principal, sc Scope) (*sql.Tx, string, error) {
	if !validID(sc.TenantID) || !validID(sc.SiteID) || !validField(a.Issuer, 512) || !validField(a.Subject, 255) || !validField(a.SessionHash, 128) {
		return nil, "", ErrDenied
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", err
	}
	fail := func(e error) (*sql.Tx, string, error) {
		_ = tx.Rollback()
		return nil, "", e
	}
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true),set_config('lock_timeout','5s',true)`, sc.TenantID); err != nil {
		return fail(err)
	}
	var issuer, subject string
	var expiry, seen, now time.Time
	err = tx.QueryRowContext(ctx, `SELECT issuer,subject,expires_at,last_seen_at,clock_timestamp() FROM supervisor.browser_sessions WHERE token_hash=$1 FOR SHARE`, a.SessionHash).Scan(&issuer, &subject, &expiry, &seen, &now)
	if err != nil || issuer != a.Issuer || subject != a.Subject || !expiry.After(now) || !seen.After(now.Add(-s.sessions.Idle)) {
		return fail(ErrDenied)
	}
	var found bool
	if err = tx.QueryRowContext(ctx, `SELECT supervisor.lock_member_site($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, false).Scan(&found); err != nil {
		return fail(err)
	}
	if !found {
		return fail(ErrNotFound)
	}
	var role string
	err = tx.QueryRowContext(ctx, `SELECT role FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND active FOR SHARE`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return fail(ErrNotFound)
	}
	if err != nil {
		return fail(err)
	}
	return tx, role, nil
}

// finish re-checks the session inside the transaction so a sign-out or idle expiry that raced
// the request cannot commit it.
func (s *Service) finish(ctx context.Context, tx *sql.Tx, a Principal) error {
	var ok bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.browser_sessions WHERE token_hash=$1 AND issuer=$2 AND subject=$3 AND expires_at>clock_timestamp() AND last_seen_at>clock_timestamp()-make_interval(secs=>$4))`,
		a.SessionHash, a.Issuer, a.Subject, s.sessions.IdleSeconds()).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrDenied
	}
	return tx.Commit()
}

// supervises reports whether a role governs the whole site's feedback.
func supervises(role string) bool { return role == "owner" || role == "supervisor" }

// bound is everything the server binds for one subject.
type bound struct {
	provenance                 knowledge.CorrectionProvenance
	citations                  *string
	caseID, contextID          *string
	caseRevision, taskRevision *int64
}

// Submit records one feedback. Only an owner, supervisor or operator gives feedback, and only on a
// subject they may read: an answer only in their own conversation, a case or task as an operator
// only where they are the task's assignee. The same key with the same request replays the stored
// feedback; with another request it is ErrIdempotency. It reports whether the result is a replay.
func (s *Service) Submit(ctx context.Context, a Principal, sc Scope, in FeedbackInputV1, key string) (FeedbackV1, error) {
	n, err := validate(in)
	if err != nil {
		return FeedbackV1{}, err
	}
	if !validID(key) {
		return FeedbackV1{}, ErrInvalid
	}
	tx, role, err := s.begin(ctx, a, sc)
	if err != nil {
		return FeedbackV1{}, err
	}
	defer tx.Rollback()
	if role != "owner" && role != "supervisor" && role != "operator" {
		return FeedbackV1{}, ErrDenied
	}
	// Claims and mappings are the site's process model and source configuration: their review is
	// a supervisor's. An operator gives feedback on their own task and its case (review of #197 M1).
	if !supervises(role) && (in.Subject.Kind == SubjectClaim || in.Subject.Kind == SubjectMapping) {
		return FeedbackV1{}, ErrDenied
	}
	// Two copies of one request serialize here, so the second replays the first.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('supervisor.feedback_key:'||$1||':'||$2||':'||$3||':'||$4||':'||$5,0))`,
		sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key); err != nil {
		return FeedbackV1{}, err
	}
	digest, err := requestDigest(sc, n)
	if err != nil {
		return FeedbackV1{}, err
	}
	var stored, storedID string
	err = tx.QueryRowContext(ctx, `SELECT request_sha256,feedback_id::text FROM supervisor.feedback_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`,
		sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&stored, &storedID)
	switch {
	case err == nil:
		if stored != digest {
			return FeedbackV1{}, ErrIdempotency
		}
		out, err := load(ctx, tx, sc, storedID)
		if err != nil {
			return out, err
		}
		out.Replayed = true
		return out, s.finish(ctx, tx, a)
	case !errors.Is(err, sql.ErrNoRows):
		return FeedbackV1{}, err
	}

	b, err := bindSubject(ctx, tx, a, sc, role, n)
	if err != nil {
		return FeedbackV1{}, err
	}
	// A proposal is bound at its current revision, which must belong to this case.
	var proposalRevision *int64
	if in.ProposalID != nil {
		var revision int64
		err = tx.QueryRowContext(ctx, `SELECT r.revision FROM supervisor.approval_states s JOIN supervisor.proposal_revisions r ON (r.tenant_id,r.site_id,r.proposal_id,r.revision)=(s.tenant_id,s.site_id,s.proposal_id,s.current_revision)
 WHERE s.tenant_id=$1 AND s.site_id=$2 AND s.proposal_id=$3 AND r.case_id=$4`, sc.TenantID, sc.SiteID, *in.ProposalID, *b.caseID).Scan(&revision)
		if errors.Is(err, sql.ErrNoRows) {
			return FeedbackV1{}, ErrInvalid // a proposal of another case, or none
		}
		if err != nil {
			return FeedbackV1{}, err
		}
		proposalRevision = &revision
	}
	if in.SupersedesFeedbackID != nil {
		if err = checkSupersedes(ctx, tx, a, sc, *in.SupersedesFeedbackID, n); err != nil {
			return FeedbackV1{}, err
		}
	}
	// Cited and challenged sources must be readable now (present on this site and unrevoked); the
	// heads are locked for share, so a concurrent revocation waits for this commit or wins first.
	ids := append([]string{}, n.evidence...)
	if in.Challenged != nil {
		ids = append(ids, in.Challenged.SourceID)
	}
	heads, err := evidence.HeadsTx(ctx, tx, evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, ids)
	if err != nil {
		return FeedbackV1{}, err
	}
	for _, id := range ids {
		if h, ok := heads[id]; !ok || h.Revoked {
			return FeedbackV1{}, ErrEvidence
		}
	}
	if in.Challenged != nil && in.Challenged.Revision > heads[in.Challenged.SourceID].Revision {
		return FeedbackV1{}, ErrEvidence
	}

	id := uuid.NewString()
	var challengedSource, challengedRevision any
	if in.Challenged != nil {
		challengedSource, challengedRevision = in.Challenged.SourceID, in.Challenged.Revision
	}
	nullID := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	nullN := func(v int64) any {
		if v == 0 {
			return nil
		}
		return v
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.feedback(tenant_id,site_id,id,schema_version,subject_kind,conversation_id,message_id,answer_citations_sha256,claim_id,claim_revision,source_id,mapping_revision,
 case_id,case_revision,context_snapshot_id,task_id,task_revision,proposal_id,proposal_revision,kind,verdict,reason,challenged_source_id,challenged_source_revision,observation_start,observation_end,supersedes_feedback_id,actor_id,actor_issuer,actor_subject,run_id,artifact_id,artifact_sha256,terminal_sha256,run_refs_sha256,process_snapshot_id)
 VALUES($1,$2,$3,1,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35)`,
		sc.TenantID, sc.SiteID, id, string(in.Subject.Kind), nullID(n.conversation), nullID(n.answer), b.citations, nullID(n.claim), nullN(n.claimRevision), nullID(n.source), nullN(n.mappingRevision),
		b.caseID, b.caseRevision, b.contextID, nullID(n.task), b.taskRevision, in.ProposalID, proposalRevision, string(in.Kind), string(in.Verdict), in.Reason, challengedSource, challengedRevision,
		n.observationStart, n.observationEnd, in.SupersedesFeedbackID, membership.ActorID(a.Issuer, a.Subject).String(), a.Issuer, a.Subject, b.provenance.RunID, b.provenance.ArtifactID, b.provenance.ArtifactSHA256, b.provenance.TerminalSHA256, b.provenance.RunRefsSHA256, b.provenance.ProcessSnapshotID); err != nil {
		return FeedbackV1{}, err
	}
	for _, src := range n.evidence {
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.feedback_evidence(tenant_id,site_id,feedback_id,source_id,revision) VALUES($1,$2,$3,$4,$5)`,
			sc.TenantID, sc.SiteID, id, src, heads[src].Revision); err != nil {
			return FeedbackV1{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.feedback_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_sha256,feedback_id) VALUES($1,$2,$3,$4,$5,$6,$7)`,
		sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, digest, id); err != nil {
		return FeedbackV1{}, err
	}
	if in.Kind == KindCorrection && (in.Subject.Kind == SubjectAnswer || in.Subject.Kind == SubjectInvestigation) {
		proposal := knowledge.MemoryRevisionInput{Reason: in.Reason}
		if in.ProposedCorrection != nil {
			proposal = *in.ProposedCorrection
		}
		if _, err = knowledge.New(s.db).WithSessionPolicy(s.sessions).CreateMemoryCandidateTx(ctx, tx, knowledge.Actor{Issuer: a.Issuer, Subject: a.Subject, SessionHash: a.SessionHash}, knowledge.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, id, proposal); err != nil {
			return FeedbackV1{}, correctionError(err)
		}
	}
	out, err := load(ctx, tx, sc, id)
	if err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}

// bindSubject resolves the subject under the principal's read rule and returns what the server
// binds: an answer's citation digest, the case, its revision and context, and the task revision.
// A subject the principal may not read is ErrNotFound, exactly as one that does not exist.
func bindSubject(ctx context.Context, tx *sql.Tx, a Principal, sc Scope, role string, n normalized) (bound, error) {
	var b bound
	switch n.in.Subject.Kind {
	case SubjectInvestigation:
		return bindInvestigation(ctx, tx, a, sc, role, n.run)
	case SubjectAnswer:
		// Operators may correct their own answer; conversations remain author-only (WIS-703).
		if !supervises(role) && role != "operator" {
			return b, ErrNotFound
		}
		var kind, state string
		var raw []byte
		err := tx.QueryRowContext(ctx, `SELECT m.author_kind,m.state,m.citations
 FROM supervisor.chat_messages m JOIN supervisor.chat_conversations c ON (c.tenant_id,c.site_id,c.conversation_id)=(m.tenant_id,m.site_id,m.conversation_id)
 WHERE m.tenant_id=$1 AND m.site_id=$2 AND m.conversation_id=$3 AND m.message_id=$4 AND c.author_issuer=$5 AND c.author_subject=$6 FOR SHARE OF m`,
			sc.TenantID, sc.SiteID, n.conversation, n.answer, a.Issuer, a.Subject).Scan(&kind, &state, &raw)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && kind != "assistant") {
			return b, ErrNotFound
		}
		if err != nil {
			return b, err
		}
		// Only a finished reply is an answer; its citations no longer change, so the digest binds
		// the exact evidence revisions it cited.
		if state != "answered" && state != "needs_input" {
			return b, ErrSubjectState
		}
		digest, err := citationsDigest(sc, raw)
		if err != nil {
			return b, err
		}
		b.citations = &digest
		var run string
		err = tx.QueryRowContext(ctx, `SELECT run_id::text FROM supervisor.chat_answer_turns WHERE tenant_id=$1 AND site_id=$2 AND conversation_id=$3 AND reply_message_id=$4`, sc.TenantID, sc.SiteID, n.conversation, n.answer).Scan(&run)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return b, err
		}
		if err == nil {
			b.provenance, err = bindRunProvenance(ctx, tx, sc, run)
			if err != nil {
				return b, err
			}
		}
	case SubjectClaim:
		// A claim revision is readable while none of the sources its spans cite is revoked.
		var exists, revoked bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.knowledge_claim_revisions WHERE tenant_id=$1 AND site_id=$2 AND claim_id=$3 AND revision=$4),
 EXISTS(SELECT 1 FROM supervisor.knowledge_claim_spans s JOIN supervisor.evidence_heads h ON (h.tenant_id,h.site_id,h.source_id)=(s.tenant_id,s.site_id,s.source_id)
  WHERE s.tenant_id=$1 AND s.site_id=$2 AND s.claim_id=$3 AND s.claim_revision=$4
  AND (h.revoked OR EXISTS(SELECT 1 FROM supervisor.evidence_revocations v WHERE (v.tenant_id,v.site_id,v.source_id)=(h.tenant_id,h.site_id,h.source_id))))`,
			sc.TenantID, sc.SiteID, n.claim, n.claimRevision).Scan(&exists, &revoked)
		if err != nil {
			return b, err
		}
		if !exists || revoked {
			return b, ErrNotFound
		}
	case SubjectMapping:
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.operational_source_mapping_revisions WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND revision=$4)`,
			sc.TenantID, sc.SiteID, n.source, n.mappingRevision).Scan(&exists); err != nil {
			return b, err
		}
		if !exists {
			return b, ErrNotFound
		}
	case SubjectCase, SubjectTask:
		caseID := n.caseID
		if n.in.Subject.Kind == SubjectTask {
			var revision int64
			var issuer, subject string
			err := tx.QueryRowContext(ctx, `SELECT case_id::text,revision,assignee_issuer,assignee_subject FROM supervisor.tasks WHERE tenant_id=$1 AND site_id=$2 AND id=$3 FOR SHARE`,
				sc.TenantID, sc.SiteID, n.task).Scan(&caseID, &revision, &issuer, &subject)
			if errors.Is(err, sql.ErrNoRows) {
				return b, ErrNotFound
			}
			if err != nil {
				return b, err
			}
			if !supervises(role) && (issuer != a.Issuer || subject != a.Subject) {
				return b, ErrNotFound
			}
			b.taskRevision = &revision
		}
		var revision int64
		var contextID string
		err := tx.QueryRowContext(ctx, `SELECT revision,context_snapshot_id::text FROM supervisor.cases WHERE tenant_id=$1 AND site_id=$2 AND id=$3 FOR SHARE`,
			sc.TenantID, sc.SiteID, caseID).Scan(&revision, &contextID)
		if errors.Is(err, sql.ErrNoRows) {
			return b, ErrNotFound
		}
		if err != nil {
			return b, err
		}
		// An operator gives case feedback only on a case where they are a task's assignee.
		if n.in.Subject.Kind == SubjectCase && !supervises(role) {
			var assigned bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.tasks WHERE tenant_id=$1 AND site_id=$2 AND case_id=$3 AND assignee_issuer=$4 AND assignee_subject=$5)`,
				sc.TenantID, sc.SiteID, caseID, a.Issuer, a.Subject).Scan(&assigned); err != nil {
				return b, err
			}
			if !assigned {
				return b, ErrNotFound
			}
		}
		b.caseID, b.caseRevision, b.contextID = &caseID, &revision, &contextID
	}
	return b, nil
}

// requestDigest hashes the canonical request: evidence IDs sorted and times in UTC, so the same
// feedback sent with another evidence order or time offset replays rather than conflicts.
func requestDigest(sc Scope, n normalized) (string, error) {
	type window struct {
		Start *string `json:"start"`
		End   *string `json:"end"`
	}
	utc := func(t *time.Time) *string {
		if t == nil {
			return nil
		}
		v := t.UTC().Format(time.RFC3339Nano)
		return &v
	}
	in := n.in
	in.EvidenceIDs, in.ObservationStart, in.ObservationEnd = n.evidence, nil, nil
	_, digest, err := canonical.Envelope("OutcomeFeedbackInputV1", SchemaVersion, sc.TenantID, sc.SiteID, struct {
		Input  FeedbackInputV1 `json:"input"`
		Window window          `json:"window"`
	}{in, window{utc(n.observationStart), utc(n.observationEnd)}})
	return digest, err
}

// citationsDigest is the digest of an answer's exact citations, decoded as chat.Citation and
// serialized with the shared canonical serializer (rule 04), not the database's jsonb text.
func citationsDigest(sc Scope, raw []byte) (string, error) {
	citations := []chat.Citation{}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&citations); err != nil {
		return "", err
	}
	_, digest, err := canonical.Envelope("ChatCitationsV1", 1, sc.TenantID, sc.SiteID, citations)
	return digest, err
}

// checkSupersedes requires the named feedback to be the author's own on the same subject, with no
// successor yet. Concurrent successors of one record serialize on its lock, so a chain never
// branches (the unique index is the backstop).
func checkSupersedes(ctx context.Context, tx *sql.Tx, a Principal, sc Scope, id string, n normalized) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('supervisor.feedback_supersedes:'||$1||':'||$2||':'||$3,0))`, sc.TenantID, sc.SiteID, id); err != nil {
		return err
	}
	var kind string
	var message, claim, source, caseID, task, run sql.NullString
	var successor bool
	err := tx.QueryRowContext(ctx, `SELECT subject_kind,message_id::text,claim_id::text,source_id::text,case_id::text,task_id::text,run_id::text,
 EXISTS(SELECT 1 FROM supervisor.feedback s WHERE (s.tenant_id,s.site_id,s.supersedes_feedback_id)=(f.tenant_id,f.site_id,f.id))
 FROM supervisor.feedback f WHERE f.tenant_id=$1 AND f.site_id=$2 AND f.id=$3 AND f.actor_issuer=$4 AND f.actor_subject=$5`,
		sc.TenantID, sc.SiteID, id, a.Issuer, a.Subject).Scan(&kind, &message, &claim, &source, &caseID, &task, &run, &successor)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound // not the author's own
	}
	if err != nil {
		return err
	}
	same := SubjectKind(kind) == n.in.Subject.Kind && message.String == n.answer && claim.String == n.claim && source.String == n.source && task.String == n.task
	if n.in.Subject.Kind == SubjectInvestigation {
		same = same && run.String == n.run
	}
	if n.in.Subject.Kind == SubjectCase {
		same = same && caseID.String == n.caseID
	}
	if !same {
		return ErrInvalid
	}
	if successor {
		return ErrSuperseded
	}
	return nil
}

// visible is the read rule as SQL over a feedback row f: the author always; an owner or supervisor
// for every subject but a chat answer (a conversation is its author's alone); anyone else for
// feedback on a task assigned to them, and on a case where they hold a task (the scope in which
// an operator gives case feedback). $3/$4 are the reader, $5 whether they supervise the site.
const visible = `(f.actor_issuer=$3 AND f.actor_subject=$4
 OR ($5 AND f.subject_kind<>'answer')
 OR (f.subject_kind='task' AND EXISTS(SELECT 1 FROM supervisor.tasks t WHERE (t.tenant_id,t.site_id,t.id)=(f.tenant_id,f.site_id,f.task_id) AND t.assignee_issuer=$3 AND t.assignee_subject=$4))
 OR (f.subject_kind='case' AND EXISTS(SELECT 1 FROM supervisor.tasks t WHERE (t.tenant_id,t.site_id,t.case_id)=(f.tenant_id,f.site_id,f.case_id) AND t.assignee_issuer=$3 AND t.assignee_subject=$4)))`

// Get returns one feedback the principal may read; anything else is the non-disclosing ErrNotFound.
func (s *Service) Get(ctx context.Context, a Principal, sc Scope, id string) (FeedbackV1, error) {
	if !validID(id) {
		return FeedbackV1{}, ErrNotFound
	}
	tx, role, err := s.begin(ctx, a, sc)
	if err != nil {
		return FeedbackV1{}, err
	}
	defer tx.Rollback()
	var ok bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.feedback f WHERE f.tenant_id=$1 AND f.site_id=$2 AND f.id=$6 AND `+visible+`)`,
		sc.TenantID, sc.SiteID, a.Issuer, a.Subject, supervises(role), id).Scan(&ok); err != nil {
		return FeedbackV1{}, err
	}
	if !ok {
		return FeedbackV1{}, ErrNotFound
	}
	out, err := load(ctx, tx, sc, id)
	if err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}

// ListFilter narrows a list: one case (case and task subjects), one subject kind, or both.
type ListFilter struct {
	CaseID      string
	SubjectKind SubjectKind
}

// List returns the feedback the principal may read, newest first, in pages of limit.
func (s *Service) List(ctx context.Context, a Principal, sc Scope, f ListFilter, cursor string, limit int) (FeedbackPageV1, error) {
	if limit < 1 || limit > MaxPageSize || (f.CaseID != "" && !validID(f.CaseID)) {
		return FeedbackPageV1{}, ErrInvalid
	}
	switch f.SubjectKind {
	case "", SubjectAnswer, SubjectInvestigation, SubjectClaim, SubjectMapping, SubjectCase, SubjectTask:
	default:
		return FeedbackPageV1{}, ErrInvalid
	}
	afterAt, afterID := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), "ffffffff-ffff-ffff-ffff-ffffffffffff"
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		parts := strings.Split(string(raw), "|")
		if err != nil || len(parts) != 2 || !validID(parts[1]) {
			return FeedbackPageV1{}, ErrInvalid
		}
		if afterAt, err = time.Parse(time.RFC3339Nano, parts[0]); err != nil {
			return FeedbackPageV1{}, ErrInvalid
		}
		afterID = parts[1]
	}
	tx, role, err := s.begin(ctx, a, sc)
	if err != nil {
		return FeedbackPageV1{}, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT f.id::text,f.created_at FROM supervisor.feedback f WHERE f.tenant_id=$1 AND f.site_id=$2 AND `+visible+`
 AND ($6='' OR f.case_id=nullif($6,'')::uuid) AND ($7='' OR f.subject_kind=$7) AND (f.created_at,f.id)<($8,$9::uuid)
 ORDER BY f.created_at DESC,f.id DESC LIMIT $10`,
		sc.TenantID, sc.SiteID, a.Issuer, a.Subject, supervises(role), f.CaseID, string(f.SubjectKind), afterAt, afterID, limit+1)
	if err != nil {
		return FeedbackPageV1{}, err
	}
	type key struct {
		id string
		at time.Time
	}
	var keys []key
	for rows.Next() {
		var k key
		if err = rows.Scan(&k.id, &k.at); err != nil {
			rows.Close()
			return FeedbackPageV1{}, err
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return FeedbackPageV1{}, err
	}
	page := FeedbackPageV1{Feedback: []FeedbackV1{}}
	if len(keys) > limit {
		keys = keys[:limit]
		last := keys[limit-1]
		c := base64.RawURLEncoding.EncodeToString([]byte(last.at.UTC().Format(time.RFC3339Nano) + "|" + last.id))
		page.NextCursor = &c
	}
	for _, k := range keys {
		v, err := load(ctx, tx, sc, k.id)
		if err != nil {
			return FeedbackPageV1{}, err
		}
		page.Feedback = append(page.Feedback, v)
	}
	return page, s.finish(ctx, tx, a)
}

// load reads one feedback record and its evidence; a cited source revoked since is shown, as
// unavailable.
func load(ctx context.Context, tx *sql.Tx, sc Scope, id string) (FeedbackV1, error) {
	v := FeedbackV1{ID: id, SiteID: sc.SiteID, Evidence: []EvidenceRefV1{}}
	var provenance knowledge.CorrectionProvenance
	if err := tx.QueryRowContext(ctx, `SELECT f.run_id::text,f.artifact_id::text,f.artifact_sha256,f.terminal_sha256,f.run_refs_sha256,f.process_snapshot_id::text,c.candidate_id::text FROM supervisor.feedback f LEFT JOIN supervisor.knowledge_memory_candidates c ON (c.tenant_id,c.site_id,c.source_feedback_id)=(f.tenant_id,f.site_id,f.id) WHERE f.tenant_id=$1 AND f.site_id=$2 AND f.id=$3`, sc.TenantID, sc.SiteID, id).Scan(&provenance.RunID, &provenance.ArtifactID, &provenance.ArtifactSHA256, &provenance.TerminalSHA256, &provenance.RunRefsSHA256, &provenance.ProcessSnapshotID, &v.CandidateID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return v, ErrNotFound
		}
		return v, err
	}
	if provenance.RunID != nil {
		v.Provenance = &provenance
	}

	var conversation, message, citations, claim, source, caseID, contextID, task, proposal, challengedSource sql.NullString
	var claimRevision, mappingRevision, caseRevision, taskRevision, proposalRevision, challengedRevision sql.NullInt64
	var supersedes, supersededBy sql.NullString
	var challengedAvailable sql.NullBool
	var start, end sql.NullTime
	var created time.Time
	var subjectKind, kind, verdict string
	err := tx.QueryRowContext(ctx, `SELECT f.schema_version,f.subject_kind,f.conversation_id::text,f.message_id::text,f.answer_citations_sha256,f.claim_id::text,f.claim_revision,f.source_id::text,f.mapping_revision,
 f.case_id::text,f.case_revision,f.context_snapshot_id::text,f.task_id::text,f.task_revision,f.proposal_id::text,f.proposal_revision,f.kind,f.verdict,f.reason,f.challenged_source_id::text,f.challenged_source_revision,
 NOT (h.revoked OR EXISTS(SELECT 1 FROM supervisor.evidence_revocations r WHERE (r.tenant_id,r.site_id,r.source_id)=(h.tenant_id,h.site_id,h.source_id))),
 f.observation_start,f.observation_end,f.supersedes_feedback_id::text,(SELECT s.id::text FROM supervisor.feedback s WHERE (s.tenant_id,s.site_id,s.supersedes_feedback_id)=(f.tenant_id,f.site_id,f.id)),
 f.actor_id::text,f.actor_subject,f.created_at,f.revision
 FROM supervisor.feedback f LEFT JOIN supervisor.evidence_heads h ON (h.tenant_id,h.site_id,h.source_id)=(f.tenant_id,f.site_id,f.challenged_source_id)
 WHERE f.tenant_id=$1 AND f.site_id=$2 AND f.id=$3`, sc.TenantID, sc.SiteID, id).Scan(&v.SchemaVersion, &subjectKind, &conversation, &message, &citations, &claim, &claimRevision,
		&source, &mappingRevision, &caseID, &caseRevision, &contextID, &task, &taskRevision, &proposal, &proposalRevision, &kind, &verdict, &v.Reason, &challengedSource, &challengedRevision,
		&challengedAvailable, &start, &end, &supersedes, &supersededBy, &v.ActorID, &v.ActorSubject, &created, &v.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	str := func(n sql.NullString) *string {
		if !n.Valid {
			return nil
		}
		s := n.String
		return &s
	}
	num := func(n sql.NullInt64) *int64 {
		if !n.Valid {
			return nil
		}
		x := n.Int64
		return &x
	}
	at := func(n sql.NullTime) *string {
		if !n.Valid {
			return nil
		}
		s := n.Time.UTC().Format(time.RFC3339Nano)
		return &s
	}
	v.Subject = SubjectV1{Kind: SubjectKind(subjectKind), ConversationID: str(conversation), MessageID: str(message), AnswerCitationsSHA256: str(citations),
		ClaimID: str(claim), ClaimRevision: num(claimRevision), SourceID: str(source), MappingRevision: num(mappingRevision),
		CaseID: str(caseID), CaseRevision: num(caseRevision), TaskID: str(task), TaskRevision: num(taskRevision)}
	v.CaseID, v.ContextSnapshotID, v.ProposalID, v.ProposalRevision, v.TaskID = str(caseID), str(contextID), str(proposal), num(proposalRevision), str(task)
	v.SupersedesFeedbackID, v.SupersededBy = str(supersedes), str(supersededBy)
	v.Kind, v.Verdict = Kind(kind), Verdict(verdict)
	if challengedSource.Valid {
		v.Challenged = &ChallengedRefV1{SourceID: challengedSource.String, Revision: challengedRevision.Int64, Available: challengedAvailable.Bool}
	}
	v.ObservationStart, v.ObservationEnd = at(start), at(end)
	v.CreatedAt = created.UTC().Format(time.RFC3339Nano)
	rows, err := tx.QueryContext(ctx, `SELECT e.source_id::text,e.revision,NOT (h.revoked OR EXISTS(SELECT 1 FROM supervisor.evidence_revocations r WHERE (r.tenant_id,r.site_id,r.source_id)=(h.tenant_id,h.site_id,h.source_id)))
 FROM supervisor.feedback_evidence e JOIN supervisor.evidence_heads h ON (h.tenant_id,h.site_id,h.source_id)=(e.tenant_id,e.site_id,e.source_id)
 WHERE e.tenant_id=$1 AND e.site_id=$2 AND e.feedback_id=$3 ORDER BY e.source_id`, sc.TenantID, sc.SiteID, id)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	for rows.Next() {
		var r EvidenceRefV1
		if err = rows.Scan(&r.SourceID, &r.Revision, &r.Available); err != nil {
			return v, err
		}
		v.Evidence = append(v.Evidence, r)
	}
	return v, rows.Err()
}
