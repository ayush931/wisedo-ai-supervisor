// Package feedback records OutcomeFeedbackV1 (WIS-279): a person's append-only feedback on one
// subject of a site, a chat answer, a knowledge claim revision, an operational source mapping
// revision, a case or a task. The server binds every reference to the site, the case where there
// is one, and the exact revisions current when the feedback is given; the reason is stored as data
// and never becomes an instruction. A correction names what it challenges and overwrites nothing:
// turning it into a reviewed candidate is the later memory-candidate increment (WIS-270 approval).
package feedback

import (
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/google/uuid"
)

var (
	// ErrNotFound: the site, the subject or the feedback is not available to the principal
	// (non-disclosing 404).
	ErrNotFound = errors.New("feedback subject unavailable")
	// ErrDenied: the principal's session or role does not allow this (403).
	ErrDenied = errors.New("feedback permission denied")
	// ErrInvalid: the request breaks the contract (422).
	ErrInvalid = errors.New("invalid feedback")
	// ErrEvidence: a cited or challenged evidence source is unknown, revoked or not of this site (422).
	ErrEvidence = errors.New("feedback evidence unavailable")
	// ErrSubjectState: the subject cannot take feedback in its current state, such as an answer
	// still pending (409).
	ErrSubjectState = errors.New("feedback subject not ready")
	// ErrIdempotency: the key was used for a different request (409).
	ErrIdempotency = errors.New("idempotency key reused for a different request")
	// ErrSuperseded: the feedback named as superseded already has a successor (409); a chain of
	// corrections never branches.
	ErrSuperseded = errors.New("feedback already superseded")
)

const (
	SchemaVersion   = 1
	MaxReason       = 2000
	MaxEvidence     = 20
	DefaultPageSize = 50
	MaxPageSize     = 100
)

// SubjectKind is the versioned discriminator of what the feedback is about.
type SubjectKind string

const (
	SubjectAnswer        SubjectKind = "answer"
	SubjectInvestigation SubjectKind = "investigation"
	SubjectClaim         SubjectKind = "claim"
	SubjectMapping       SubjectKind = "mapping"
	SubjectCase          SubjectKind = "case"
	SubjectTask          SubjectKind = "task"
)

// Kind is what sort of feedback it is.
type Kind string

const (
	KindObservation Kind = "observation"
	KindCorrection  Kind = "correction"
	KindUsefulness  Kind = "usefulness"
)

// Verdict is the person's judgement of the subject.
type Verdict string

const (
	VerdictHelpful       Verdict = "helpful"
	VerdictUnhelpful     Verdict = "unhelpful"
	VerdictUncertain     Verdict = "uncertain"
	VerdictNotApplicable Verdict = "not_applicable"
)

// SubjectInput names the subject. Exactly the kind's own fields are set: answer needs
// conversation_id and message_id; claim needs claim_id and claim_revision; mapping needs source_id
// and mapping_revision; case needs case_id; task needs task_id (its case is bound from the task).
type SubjectInput struct {
	RunID           *string     `json:"run_id,omitempty" contract:"uuid"`
	Kind            SubjectKind `json:"kind"`
	ConversationID  *string     `json:"conversation_id,omitempty" contract:"uuid"`
	MessageID       *string     `json:"message_id,omitempty" contract:"uuid"`
	ClaimID         *string     `json:"claim_id,omitempty" contract:"uuid"`
	ClaimRevision   *int64      `json:"claim_revision,omitempty"`
	SourceID        *string     `json:"source_id,omitempty" contract:"uuid"`
	MappingRevision *int64      `json:"mapping_revision,omitempty"`
	CaseID          *string     `json:"case_id,omitempty" contract:"uuid"`
	TaskID          *string     `json:"task_id,omitempty" contract:"uuid"`
}

// SourceRevision is one evidence source at one revision.
type SourceRevision struct {
	SourceID string `json:"source_id" contract:"uuid"`
	Revision int64  `json:"revision"`
}

// FeedbackInputV1 is the POST /feedback body. The actor, site, case binding, context and evidence
// revisions are never accepted from the caller: the server derives them.
type FeedbackInputV1 struct {
	ProposedCorrection *knowledge.MemoryRevisionInput `json:"proposed_correction,omitempty"`
	SchemaVersion      int                            `json:"schema_version"`
	Subject            SubjectInput                   `json:"subject"`
	Kind               Kind                           `json:"kind"`
	Verdict            Verdict                        `json:"verdict"`
	Reason             string                         `json:"reason" contract:"max=2000"`
	// EvidenceIDs are evidence source IDs the person cites (0..20), bound at their current revision.
	EvidenceIDs []string `json:"evidence_ids"`
	// ProposalID optionally names a proposal of the same case (case and task subjects only).
	ProposalID *string `json:"proposal_id,omitempty" contract:"uuid"`
	// Challenged is the exact evidence source revision a correction disputes. It need not be
	// cited by the subject. A correction whose subject is a claim or mapping revision may instead
	// challenge that revision itself.
	Challenged *SourceRevision `json:"challenged,omitempty"`
	// SupersedesFeedbackID optionally names the author's own earlier feedback on the same subject
	// that this one replaces (a wrong verdict or a mistaken subject). The earlier record is kept.
	SupersedesFeedbackID *string `json:"supersedes_feedback_id,omitempty" contract:"uuid"`
	ObservationStart     *string `json:"observation_start,omitempty" contract:"datetime"`
	ObservationEnd       *string `json:"observation_end,omitempty" contract:"datetime"`
}

// normalized is a validated input with times parsed and evidence IDs sorted and unique.
type normalized struct {
	in                   FeedbackInputV1
	evidence             []string
	observationStart     *time.Time
	observationEnd       *time.Time
	conversation, answer string
	claim                string
	claimRevision        int64
	source               string
	mappingRevision      int64
	caseID, task         string
	run                  string
}

func validID(v string) bool {
	id, err := uuid.Parse(v)
	return err == nil && id != uuid.Nil && id.String() == v
}

func idOf(v *string) (string, bool) {
	if v == nil || !validID(*v) {
		return "", false
	}
	return *v, true
}

// validReason accepts 1..2000 characters of text with line breaks but no other control
// characters, and not only whitespace.
func validReason(v string) bool {
	n := utf8.RuneCountInString(v)
	if !utf8.ValidString(v) || n < 1 || n > MaxReason || strings.TrimSpace(v) == "" {
		return false
	}
	return strings.IndexFunc(v, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' }) == -1
}

func parseTime(v *string) (*time.Time, bool) {
	if v == nil {
		return nil, true
	}
	t, err := time.Parse(time.RFC3339Nano, *v)
	if err != nil {
		return nil, false
	}
	t = t.UTC()
	return &t, true
}

// validate checks the contract without the database: the discriminated subject, the enums, the
// reason, the evidence list, the paired observation window and the correction target.
func validate(in FeedbackInputV1) (normalized, error) {
	out := normalized{in: in}
	if in.SchemaVersion != SchemaVersion || !validReason(in.Reason) || in.EvidenceIDs == nil || len(in.EvidenceIDs) > MaxEvidence {
		return out, ErrInvalid
	}
	switch in.Kind {
	case KindObservation, KindCorrection, KindUsefulness:
	default:
		return out, ErrInvalid
	}
	switch in.Verdict {
	case VerdictHelpful, VerdictUnhelpful, VerdictUncertain, VerdictNotApplicable:
	default:
		return out, ErrInvalid
	}
	seen := map[string]bool{}
	for _, id := range in.EvidenceIDs {
		if !validID(id) || seen[id] {
			return out, ErrInvalid
		}
		seen[id] = true
		out.evidence = append(out.evidence, id)
	}
	slices.Sort(out.evidence)
	var ok bool
	if out.observationStart, ok = parseTime(in.ObservationStart); !ok {
		return out, ErrInvalid
	}
	if out.observationEnd, ok = parseTime(in.ObservationEnd); !ok {
		return out, ErrInvalid
	}
	if (out.observationStart == nil) != (out.observationEnd == nil) || (out.observationEnd != nil && !out.observationEnd.After(*out.observationStart)) {
		return out, ErrInvalid
	}
	s := in.Subject
	if s.Kind != SubjectInvestigation && s.RunID != nil {
		return out, ErrInvalid
	}
	if in.ProposedCorrection != nil {
		if in.Kind != KindCorrection || (s.Kind != SubjectAnswer && s.Kind != SubjectInvestigation) || in.ProposedCorrection.Validate() != nil {
			return out, ErrInvalid
		}
	}
	set := func(v *string) bool { return v != nil }
	setN := func(v *int64) bool { return v != nil }
	// Exactly the kind's own fields: anything else set is a contract error, not ignored.
	switch s.Kind {
	case SubjectInvestigation:
		out.run, ok = idOf(s.RunID)
		if !ok || set(s.ConversationID) || set(s.MessageID) || set(s.ClaimID) || setN(s.ClaimRevision) || set(s.SourceID) || setN(s.MappingRevision) || set(s.CaseID) || set(s.TaskID) {
			return out, ErrInvalid
		}
	case SubjectAnswer:
		out.conversation, ok = idOf(s.ConversationID)
		var okM bool
		out.answer, okM = idOf(s.MessageID)
		if !ok || !okM || set(s.ClaimID) || setN(s.ClaimRevision) || set(s.SourceID) || setN(s.MappingRevision) || set(s.CaseID) || set(s.TaskID) {
			return out, ErrInvalid
		}
	case SubjectClaim:
		out.claim, ok = idOf(s.ClaimID)
		if !ok || s.ClaimRevision == nil || *s.ClaimRevision < 1 || set(s.ConversationID) || set(s.MessageID) || set(s.SourceID) || setN(s.MappingRevision) || set(s.CaseID) || set(s.TaskID) {
			return out, ErrInvalid
		}
		out.claimRevision = *s.ClaimRevision
	case SubjectMapping:
		out.source, ok = idOf(s.SourceID)
		if !ok || s.MappingRevision == nil || *s.MappingRevision < 1 || set(s.ConversationID) || set(s.MessageID) || set(s.ClaimID) || setN(s.ClaimRevision) || set(s.CaseID) || set(s.TaskID) {
			return out, ErrInvalid
		}
		out.mappingRevision = *s.MappingRevision
	case SubjectCase:
		out.caseID, ok = idOf(s.CaseID)
		if !ok || set(s.ConversationID) || set(s.MessageID) || set(s.ClaimID) || setN(s.ClaimRevision) || set(s.SourceID) || setN(s.MappingRevision) || set(s.TaskID) {
			return out, ErrInvalid
		}
	case SubjectTask:
		out.task, ok = idOf(s.TaskID)
		if !ok || set(s.ConversationID) || set(s.MessageID) || set(s.ClaimID) || setN(s.ClaimRevision) || set(s.SourceID) || setN(s.MappingRevision) || set(s.CaseID) {
			return out, ErrInvalid
		}
	default:
		return out, ErrInvalid
	}
	if in.SupersedesFeedbackID != nil && !validID(*in.SupersedesFeedbackID) {
		return out, ErrInvalid
	}
	if in.ProposalID != nil {
		if _, ok := idOf(in.ProposalID); !ok || (s.Kind != SubjectCase && s.Kind != SubjectTask) {
			return out, ErrInvalid
		}
	}
	// A correction names what it challenges: the claim or mapping revision that is its subject, or
	// an exact evidence source revision. Only a correction challenges anything.
	if in.Challenged != nil {
		if in.Kind != KindCorrection || !validID(in.Challenged.SourceID) || in.Challenged.Revision < 1 {
			return out, ErrInvalid
		}
	} else if in.Kind == KindCorrection && s.Kind != SubjectClaim && s.Kind != SubjectMapping && s.Kind != SubjectAnswer && s.Kind != SubjectInvestigation {
		return out, ErrInvalid
	}
	return out, nil
}

// ChallengedRefV1 is the evidence source revision a correction challenges. Available is false once
// the source is revoked: the reference stays, explicitly unavailable (rule 06).
type ChallengedRefV1 struct {
	SourceID  string `json:"source_id" contract:"uuid"`
	Revision  int64  `json:"revision"`
	Available bool   `json:"available"`
}

// EvidenceRefV1 is one cited evidence source at its bound revision. Available is false once the
// source is revoked: the citation stays, explicitly unavailable, and no content is returned.
type EvidenceRefV1 struct {
	SourceID  string `json:"source_id" contract:"uuid"`
	Revision  int64  `json:"revision"`
	Available bool   `json:"available"`
}

// SubjectV1 is the subject as bound by the server.
type SubjectV1 struct {
	Kind                  SubjectKind `json:"kind"`
	ConversationID        *string     `json:"conversation_id" contract:"uuid"`
	MessageID             *string     `json:"message_id" contract:"uuid"`
	AnswerCitationsSHA256 *string     `json:"answer_citations_sha256" contract:"sha256"`
	ClaimID               *string     `json:"claim_id" contract:"uuid"`
	ClaimRevision         *int64      `json:"claim_revision"`
	SourceID              *string     `json:"source_id" contract:"uuid"`
	MappingRevision       *int64      `json:"mapping_revision"`
	CaseID                *string     `json:"case_id" contract:"uuid"`
	CaseRevision          *int64      `json:"case_revision"`
	TaskID                *string     `json:"task_id" contract:"uuid"`
	TaskRevision          *int64      `json:"task_revision"`
}

// FeedbackV1 is one stored feedback record (OutcomeFeedbackV1).
type FeedbackV1 struct {
	CandidateID       *string                         `json:"candidate_id,omitempty" contract:"uuid"`
	Provenance        *knowledge.CorrectionProvenance `json:"provenance,omitempty"`
	ID                string                          `json:"id" contract:"uuid"`
	SiteID            string                          `json:"site_id" contract:"uuid"`
	SchemaVersion     int                             `json:"schema_version"`
	Subject           SubjectV1                       `json:"subject"`
	CaseID            *string                         `json:"case_id" contract:"uuid"`
	ContextSnapshotID *string                         `json:"context_snapshot_id" contract:"uuid"`
	ProposalID        *string                         `json:"proposal_id" contract:"uuid"`
	ProposalRevision  *int64                          `json:"proposal_revision"`
	TaskID            *string                         `json:"task_id" contract:"uuid"`
	Kind              Kind                            `json:"kind"`
	Verdict           Verdict                         `json:"verdict"`
	Reason            string                          `json:"reason" contract:"max=2000"`
	Evidence          []EvidenceRefV1                 `json:"evidence"`
	Challenged        *ChallengedRefV1                `json:"challenged"`
	// SupersedesFeedbackID is the earlier feedback this one replaces; SupersededBy is the later
	// feedback that replaced this one, if any. Only the last of a chain is the author's current view.
	SupersedesFeedbackID *string `json:"supersedes_feedback_id" contract:"uuid"`
	SupersededBy         *string `json:"superseded_by" contract:"uuid"`
	ObservationStart     *string `json:"observation_start" contract:"datetime"`
	ObservationEnd       *string `json:"observation_end" contract:"datetime"`
	ActorID              string  `json:"actor_id" contract:"uuid"`
	ActorSubject         string  `json:"actor_subject" contract:"max=255"`
	CreatedAt            string  `json:"created_at" contract:"datetime"`
	Revision             int64   `json:"revision"`
	Replayed             bool    `json:"replayed"`
}

// FeedbackPageV1 is one page of feedback, newest first.
type FeedbackPageV1 struct {
	Feedback   []FeedbackV1 `json:"feedback"`
	NextCursor *string      `json:"next_cursor" contract:"max=128"`
}
