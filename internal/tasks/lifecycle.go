package tasks

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/roster"
)

var (
	// ErrRevision: If-Match named a stale task revision (412).
	ErrRevision = errors.New("task revision changed")
	// ErrIdempotency: the key was used for a different request (409).
	ErrIdempotency = errors.New("idempotency key reused for a different request")
	// ErrReauthentication: a supervisor action needs a recent sign-in (428).
	ErrReauthentication = errors.New("recent authentication required")
	// ErrEvidence: a cited evidence source is not usable for this completion (422).
	ErrEvidence = errors.New("completion evidence not usable")
)

const (
	MaxReason         = 2000
	MaxCompletionNote = 4000
	MaxEvidence       = 20
)

// TransitionInput is the body of acknowledge, start, block, cancel and reopen. Reason is required
// for block, cancel, reopen and a start from blocked.
type TransitionInput struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Reason           string `json:"reason,omitempty" contract:"max=2000"`
}

// CompleteInput is the body of complete: a written note and the immutable evidence the assignee
// recorded, at least one of it a written ("manual") report.
type CompleteInput struct {
	ExpectedRevision int64    `json:"expected_revision"`
	CompletionNote   string   `json:"completion_note" contract:"max=4000"`
	EvidenceIDs      []string `json:"evidence_ids" contract:"uuid"`
}

func requestDigest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// validText requires valid UTF-8, 1..max characters with some non-space content and no control
// characters other than tab and newline.
func validText(s string, max int) bool {
	n := utf8.RuneCountInString(s)
	if !utf8.ValidString(s) || n < 1 || n > max || strings.TrimSpace(s) == "" {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return false
		}
	}
	return true
}

// Acknowledge, Start, Block, Cancel and Reopen apply one transition. ifMatch is the If-Match
// revision and must equal in.ExpectedRevision.
func (s *Service) Acknowledge(ctx context.Context, a Principal, sc Scope, id string, in TransitionInput, key string, ifMatch int64) (TaskV1, error) {
	return s.transition(ctx, a, sc, id, Acknowledge, in, key, ifMatch)
}

func (s *Service) Start(ctx context.Context, a Principal, sc Scope, id string, in TransitionInput, key string, ifMatch int64) (TaskV1, error) {
	return s.transition(ctx, a, sc, id, Start, in, key, ifMatch)
}

func (s *Service) Block(ctx context.Context, a Principal, sc Scope, id string, in TransitionInput, key string, ifMatch int64) (TaskV1, error) {
	return s.transition(ctx, a, sc, id, Block, in, key, ifMatch)
}

func (s *Service) Cancel(ctx context.Context, a Principal, sc Scope, id string, in TransitionInput, key string, ifMatch int64) (TaskV1, error) {
	return s.transition(ctx, a, sc, id, Cancel, in, key, ifMatch)
}

// Reopen moves a completed task back to in_progress as a correction. The completion stays in
// the history; the next completion is a new epoch.
func (s *Service) Reopen(ctx context.Context, a Principal, sc Scope, id string, in TransitionInput, key string, ifMatch int64) (TaskV1, error) {
	return s.transition(ctx, a, sc, id, Reopen, in, key, ifMatch)
}

func (s *Service) transition(ctx context.Context, a Principal, sc Scope, id string, ev Event, in TransitionInput, key string, ifMatch int64) (TaskV1, error) {
	if in.Reason != "" && !validText(in.Reason, MaxReason) {
		return TaskV1{}, ErrInvalid
	}
	return s.mutate(ctx, a, sc, id, ev, in.ExpectedRevision, in.Reason, in, key, ifMatch, nil)
}

// Complete records the assignee's completion with its note and evidence. Every cited source must
// be same-site, unrevoked, at a current revision written by the completing assignee, and at least
// one must be a written report (kind "manual"); a photo or document is optional.
func (s *Service) Complete(ctx context.Context, a Principal, sc Scope, id string, in CompleteInput, key string, ifMatch int64) (TaskV1, error) {
	if !validText(in.CompletionNote, MaxCompletionNote) || len(in.EvidenceIDs) < 1 || len(in.EvidenceIDs) > MaxEvidence {
		return TaskV1{}, ErrInvalid
	}
	seen := map[string]bool{}
	for _, e := range in.EvidenceIDs {
		if !validID(e) || seen[e] {
			return TaskV1{}, ErrInvalid
		}
		seen[e] = true
	}
	ids := slices.Clone(in.EvidenceIDs)
	slices.Sort(ids)
	record := func(tx *sql.Tx, revision int64, actor ActorRef) error {
		report := false
		type cited struct {
			source   string
			revision int64
		}
		refs := make([]cited, 0, len(ids))
		// A completion reports on this task's current work: every cited revision must be written
		// after the task was created and, after a reopen, after the latest reopen, so an old
		// generic note or the report a supervisor just sent back cannot satisfy it (WIS-277 review).
		var since time.Time
		if err := tx.QueryRowContext(ctx, `SELECT GREATEST(t.created_at,COALESCE((SELECT max(e.created_at) FROM supervisor.task_events e
 WHERE (e.tenant_id,e.site_id,e.task_id)=(t.tenant_id,t.site_id,t.id) AND e.event='reopen'),t.created_at))
 FROM supervisor.tasks t WHERE t.tenant_id=$1 AND t.site_id=$2 AND t.id=$3`, sc.TenantID, sc.SiteID, id).Scan(&since); err != nil {
			return err
		}
		for _, e := range ids {
			var current int64
			var kind, issuer, subject string
			var revoked bool
			var written time.Time
			err := tx.QueryRowContext(ctx, `SELECT h.current_revision,r.kind,r.actor_issuer,r.actor_subject,r.created_at,
 h.revoked OR EXISTS(SELECT 1 FROM supervisor.evidence_revocations v WHERE (v.tenant_id,v.site_id,v.source_id)=(h.tenant_id,h.site_id,h.source_id))
 FROM supervisor.evidence_heads h JOIN supervisor.evidence_revisions r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=(h.tenant_id,h.site_id,h.source_id,h.current_revision)
 WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.source_id=$3 FOR SHARE OF h`, sc.TenantID, sc.SiteID, e).Scan(&current, &kind, &issuer, &subject, &written, &revoked)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrEvidence // missing, or another site's or tenant's: indistinguishable
			}
			if err != nil {
				return err
			}
			if revoked || issuer != actor.Issuer || subject != actor.Subject || written.Before(since) {
				return ErrEvidence
			}
			report = report || kind == "manual"
			refs = append(refs, cited{e, current})
		}
		if !report {
			return ErrEvidence
		}
		var epoch int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(max(epoch),0)+1 FROM supervisor.task_completions WHERE tenant_id=$1 AND site_id=$2 AND task_id=$3`, sc.TenantID, sc.SiteID, id).Scan(&epoch); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO supervisor.task_completions(tenant_id,site_id,task_id,epoch,task_revision,completion_note,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
			sc.TenantID, sc.SiteID, id, epoch, revision, in.CompletionNote, actor.Issuer, actor.Subject); err != nil {
			return err
		}
		for _, r := range refs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO supervisor.task_completion_evidence(tenant_id,site_id,task_id,epoch,source_id,revision) VALUES($1,$2,$3,$4,$5,$6)`,
				sc.TenantID, sc.SiteID, id, epoch, r.source, r.revision); err != nil {
				return err
			}
		}
		return nil
	}
	normalized := in
	normalized.EvidenceIDs = ids
	return s.mutate(ctx, a, sc, id, Complete, in.ExpectedRevision, "", normalized, key, ifMatch, record)
}

// mutate applies one event in a write transaction with the global lock order: session, site
// scope, the principal's membership, the bound case (share), then the task. The same key with the
// same request replays the stored result; with a different request it is 409.
func (s *Service) mutate(ctx context.Context, a Principal, sc Scope, id string, ev Event, expected int64, reason string, body any, key string, ifMatch int64,
	record func(*sql.Tx, int64, ActorRef) error) (TaskV1, error) {
	if !validID(id) {
		return TaskV1{}, ErrNotFound
	}
	if !validID(key) || expected < 1 || ifMatch != expected {
		return TaskV1{}, ErrInvalid
	}
	if !validID(sc.TenantID) || !validID(sc.SiteID) || !validField(a.Issuer, 512) || !validField(a.Subject, 255) || !validField(a.SessionHash, 128) {
		return TaskV1{}, ErrDenied
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TaskV1{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true),set_config('lock_timeout','5s',true)`, sc.TenantID); err != nil {
		return TaskV1{}, err
	}
	var issuer, subject string
	var expiry, seen, auth, now time.Time
	err = tx.QueryRowContext(ctx, `SELECT issuer,subject,expires_at,last_seen_at,auth_time,clock_timestamp() FROM supervisor.browser_sessions WHERE token_hash=$1 FOR SHARE`, a.SessionHash).Scan(&issuer, &subject, &expiry, &seen, &auth, &now)
	if err != nil || issuer != a.Issuer || subject != a.Subject || !expiry.After(now) || !seen.After(now.Add(-s.sessions.Idle)) {
		return TaskV1{}, ErrDenied
	}
	var found bool
	if err = tx.QueryRowContext(ctx, `SELECT supervisor.lock_member_site($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, true).Scan(&found); err != nil {
		return TaskV1{}, err
	}
	if !found {
		return TaskV1{}, ErrNotFound
	}
	var role string
	err = tx.QueryRowContext(ctx, `SELECT role FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND active FOR SHARE`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskV1{}, ErrNotFound
	}
	if err != nil {
		return TaskV1{}, err
	}

	var caseID string
	if err = tx.QueryRowContext(ctx, `SELECT case_id FROM supervisor.tasks WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, id).Scan(&caseID); errors.Is(err, sql.ErrNoRows) {
		return TaskV1{}, ErrNotFound
	} else if err != nil {
		return TaskV1{}, err
	}
	var caseState string
	if err = tx.QueryRowContext(ctx, `SELECT state FROM supervisor.cases WHERE tenant_id=$1 AND site_id=$2 AND id=$3 FOR SHARE`, sc.TenantID, sc.SiteID, caseID).Scan(&caseState); err != nil {
		return TaskV1{}, err
	}
	var state State
	var revision int64
	var assigneeIssuer, assigneeSubject string
	if err = tx.QueryRowContext(ctx, `SELECT state,revision,assignee_issuer,assignee_subject FROM supervisor.tasks WHERE tenant_id=$1 AND site_id=$2 AND id=$3 FOR UPDATE`, sc.TenantID, sc.SiteID, id).
		Scan(&state, &revision, &assigneeIssuer, &assigneeSubject); err != nil {
		return TaskV1{}, err
	}

	// The receipt is read under the task lock, so a concurrent copy of the same request waits
	// for the first and then replays it instead of failing the revision check (WIS-277 review).
	op := "task." + string(ev)
	hash := requestDigest(struct {
		Op, ID string
		Body   any
	}{op, id, body})
	var stored, storedOp, storedTask string
	err = tx.QueryRowContext(ctx, `SELECT request_sha256,operation,task_id FROM supervisor.task_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`,
		sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&stored, &storedOp, &storedTask)
	switch {
	case err == nil:
		if stored != hash || storedOp != op {
			return TaskV1{}, ErrIdempotency
		}
		// Rule 07: read authority is checked again before a stored body is replayed. An operator
		// sees only the tasks assigned to them, exactly as Get does, so a supervisor since demoted
		// to operator cannot read another person's task through an old key (WIS-694). The digest
		// binds the task id, so storedTask is the task locked above.
		if role == "operator" && (a.Issuer != assigneeIssuer || a.Subject != assigneeSubject) {
			return TaskV1{}, ErrNotFound
		}
		out, err := Load(ctx, tx, sc, storedTask)
		if err != nil {
			return out, err
		}
		out.Replayed = true
		return out, s.finish(ctx, tx, a)
	case !errors.Is(err, sql.ErrNoRows):
		return TaskV1{}, err
	}
	if revision != expected {
		return TaskV1{}, ErrRevision
	}
	// A resolved or canceled case is closed history: its task does not move any more.
	if caseState == "resolved" || caseState == "canceled" {
		return TaskV1{}, ErrIllegalTransition
	}

	// The event decides which role is needed; the principal must hold exactly that one. Owner
	// is never an implicit bypass, and a supervisor action needs a recent sign-in.
	rule, ok := rules[state][ev]
	if !ok {
		return TaskV1{}, ErrIllegalTransition
	}
	acting := Role("")
	switch rule.role {
	case RoleAssignee:
		// The assignee acts only while still an active operator or supervisor, the roles the
		// roster admits (rule 03: a role revocation applies on the next request).
		if a.Issuer == assigneeIssuer && a.Subject == assigneeSubject && slices.Contains(roster.EligibleRoles, role) {
			acting = RoleAssignee
		}
	case RoleSupervisor:
		if role == "supervisor" {
			acting = RoleSupervisor
			if auth.After(now) || auth.Before(now.Add(-s.sessions.RecentAuth)) {
				return TaskV1{}, ErrReauthentication
			}
		}
	}
	next, err := Apply(state, ev, acting, reason != "")
	if err != nil {
		return TaskV1{}, err
	}
	set := ""
	switch {
	case ev == Acknowledge:
		set = ",acknowledged_at=$6"
	case next == InProgress && ev == Start:
		set = ",started_at=COALESCE(started_at,$6)"
	case next == Completed:
		set = ",completed_at=$6"
	case ev == Reopen:
		set = ",completed_at=NULL"
	}
	args := []any{sc.TenantID, sc.SiteID, id, string(next), revision + 1}
	if strings.Contains(set, "$6") {
		args = append(args, now)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE supervisor.tasks SET state=$4,revision=$5`+set+` WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, args...); err != nil {
		return TaskV1{}, err
	}
	var reasonArg any
	if reason != "" {
		reasonArg = reason
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.task_events(tenant_id,site_id,task_id,revision,from_state,to_state,event,reason,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		sc.TenantID, sc.SiteID, id, revision+1, string(state), string(next), string(ev), reasonArg, a.Issuer, a.Subject); err != nil {
		return TaskV1{}, err
	}
	if record != nil {
		if err = record(tx, revision+1, ActorRef{Issuer: a.Issuer, Subject: a.Subject}); err != nil {
			return TaskV1{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.task_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,operation,request_sha256,task_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
		sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, op, hash, id); err != nil {
		return TaskV1{}, err
	}
	out, err := Load(ctx, tx, sc, id)
	if err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
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
