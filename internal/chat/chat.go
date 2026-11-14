// Package chat owns WIS-517 operations-chat conversations: durable, site-scoped, author-only
// threads of human questions and assistant replies. A reply is data, never authority: it cannot
// create, approve or change any production record (rule 05). A reply is an explicit non-answer
// unless answering is commissioned (local model, agent mode and SUPERVISOR_CHAT_ANSWERING_ENABLED,
// set only after WIS-280 qualification); then a captured conversation run answers it with cited,
// deterministically validated statements. The workstation workflow never depends on this package.
package chat

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
	"github.com/google/uuid"
)

var (
	ErrInvalid  = errors.New("invalid chat request")
	ErrDenied   = errors.New("chat permission denied")
	ErrNotFound = errors.New("conversation unavailable")
	ErrConflict = errors.New("chat idempotency conflict")
	ErrRevision = errors.New("conversation revision changed")
)

const (
	MaxTitleRunes    = 200
	MaxQuestionRunes = 4000

	AuthorHuman     = "human"
	AuthorAssistant = "assistant"

	StateReceived    = "received"
	StateUnavailable = "unavailable"

	// ReasonDisabled: no local model is configured or agent mode is off. ReasonNotQualified: a
	// model is configured but chat answering has not passed its WIS-280 qualification, so no
	// answer is attempted (an optional capability is enabled only after its own qualification).
	ReasonDisabled     = "ASSISTANT_DISABLED"
	ReasonNotQualified = "ASSISTANT_NOT_QUALIFIED"
)

type Actor struct{ Issuer, Subject, SessionHash string }
type Scope struct{ TenantID, SiteID string }

type ActorRef struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

type Conversation struct {
	ConversationID string    `json:"conversation_id" contract:"uuid"`
	Title          string    `json:"title" contract:"max=200"`
	Revision       int64     `json:"revision"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type ConversationPage struct {
	Conversations []Conversation `json:"conversations"`
	NextCursor    string         `json:"next_cursor,omitempty" contract:"uuid"`
}

// Citation names an authorized revision an answer relied on. Slice 1 records none.
type Citation struct {
	Kind     string `json:"kind"`
	ID       string `json:"id" contract:"uuid"`
	Revision int64  `json:"revision"`
}

type Message struct {
	MessageID         string     `json:"message_id" contract:"uuid"`
	Seq               int64      `json:"seq"`
	AuthorKind        string     `json:"author_kind"`
	Author            *ActorRef  `json:"author,omitempty"`
	RequestID         *string    `json:"request_id,omitempty" contract:"uuid"`
	ReplyTo           *string    `json:"reply_to,omitempty" contract:"uuid"`
	Body              *string    `json:"body,omitempty" contract:"max=8000"`
	State             string     `json:"state"`
	ReasonCode        *string    `json:"reason_code,omitempty"`
	Citations         []Citation `json:"citations"`
	SnapshotID        *string    `json:"snapshot_id,omitempty" contract:"uuid"`
	SnapshotCreatedAt *time.Time `json:"snapshot_created_at,omitempty"`
	// UIs must render the stale label from this field; body text is not authoritative.
	SnapshotStale *bool `json:"snapshot_stale,omitempty"`
	// Retrieval says how the passages behind the answer were chosen (WIS-627): "word" (semantic
	// retrieval off), "hybrid" (word and embedding order, reranked when a rerank model is
	// configured), "hybrid_unreranked" (hybrid, but the configured rerank model did not answer;
	// WIS-647) or "word_fallback" (semantic retrieval on but not usable for this question). UIs
	// label word_fallback and hybrid_unreranked.
	Retrieval *string `json:"retrieval,omitempty"`
	// Quantities are the numbers the answer states that the server resolved to approved claims
	// (WIS-640): each with the claim's own canonical value and unit, the claim and the span stating
	// it, and the index of its statement. UIs show these as the approved values.
	Quantities     []agent.ChatAnswerQuantity `json:"quantities,omitempty"`
	ClaimCitations []agent.ChatClaimCitation  `json:"claim_citations,omitempty"`
	Corrections    []CorrectionAttribution    `json:"corrections,omitempty"`
	CreatedAt      time.Time                  `json:"created_at"`

	runID string // the answer run, whose captured evidence a stored answer is re-authorized against
}

type MessagePage struct {
	Messages   []Message `json:"messages"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

type CreateInput struct {
	Title string `json:"title" contract:"max=200"`
}

type PostInput struct {
	RequestID string `json:"request_id" contract:"uuid"`
	Text      string `json:"text" contract:"max=4000"`
}

// PostResult is the question as recorded and the assistant's reply to it.
type PostResult struct {
	Conversation Conversation `json:"conversation"`
	Question     Message      `json:"question"`
	Reply        Message      `json:"reply"`
	Replayed     bool         `json:"replayed"`
}

type Service struct {
	db         *sql.DB
	sessions   session.Policy
	assistant  bool
	snapshotID string // explicit published snapshot for offline correction regression; HTTP uses latest
	answer     AnswerSettings
}

func NewService(db *sql.DB) *Service { return &Service{db: db, sessions: session.Default()} }

// WithSessionPolicy returns a copy that enforces p (WIS-290 shared session policy).
func (s *Service) WithSessionPolicy(p session.Policy) *Service {
	c := *s
	c.sessions = p.OrDefault()
	return &c
}

// WithAssistant returns a copy that knows whether a local model is configured and agent mode
// is on. It only selects the reason an unanswered reply records; slice 1 never answers.
func (s *Service) WithAssistant(enabled bool) *Service {
	c := *s
	c.assistant = enabled
	return &c
}

func validID(v string) bool {
	id, err := uuid.Parse(v)
	return err == nil && id != uuid.Nil && id.String() == v
}

func validText(v string, maxRunes int) bool {
	return utf8.ValidString(v) && strings.TrimSpace(v) != "" && utf8.RuneCountInString(v) <= maxRunes && !strings.ContainsRune(v, 0)
}

func validKey(key string) bool {
	return len(key) >= 1 && len(key) <= 128 && strings.TrimSpace(key) == key
}

func (in CreateInput) Validate() error {
	if !validText(in.Title, MaxTitleRunes) || strings.TrimSpace(in.Title) != in.Title {
		return ErrInvalid
	}
	return nil
}

func (in PostInput) Validate() error {
	if !validID(in.RequestID) || !validText(in.Text, MaxQuestionRunes) {
		return ErrInvalid
	}
	return nil
}

func validActor(a Actor) bool {
	b, err := hex.DecodeString(a.SessionHash)
	return err == nil && len(b) == sha256.Size && a.SessionHash == strings.ToLower(a.SessionHash) &&
		len(a.Issuer) > 0 && len(a.Issuer) <= 512 && len(a.Subject) > 0 && len(a.Subject) <= 255
}

func requestHash(op string, value any) string {
	body, _ := json.Marshal(struct {
		Operation string `json:"operation"`
		Value     any    `json:"value"`
	}{op, value})
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

func (s *Service) checkSession(ctx context.Context, tx *sql.Tx, a Actor) error {
	var issuer, subject string
	var expiry, seen, now time.Time
	err := tx.QueryRowContext(ctx, `SELECT issuer,subject,expires_at,last_seen_at,clock_timestamp() FROM supervisor.browser_sessions WHERE token_hash=$1 FOR SHARE`, a.SessionHash).Scan(&issuer, &subject, &expiry, &seen, &now)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (issuer != a.Issuer || subject != a.Subject || !expiry.After(now) || !seen.After(now.Add(-s.sessions.Idle))) {
		return ErrDenied
	}
	return err
}

// begin opens a tenant-scoped transaction and re-authorizes the caller on every call: a live
// session, the site (locked for writes) and active owner, supervisor or operator membership.
// A non-member gets the non-disclosing ErrNotFound; viewers cannot chat.
func (s *Service) begin(ctx context.Context, a Actor, sc Scope, write bool) (*sql.Tx, error) {
	if !validID(sc.TenantID) || !validID(sc.SiteID) {
		return nil, ErrNotFound
	}
	if !validActor(a) {
		return nil, ErrDenied
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	fail := func(e error) (*sql.Tx, error) { _ = tx.Rollback(); return nil, e }
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true),set_config('lock_timeout','5s',true)`, sc.TenantID); err != nil {
		return fail(err)
	}
	if err = s.checkSession(ctx, tx, a); err != nil {
		return fail(err)
	}
	var found bool
	if err = tx.QueryRowContext(ctx, `SELECT supervisor.lock_member_site($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, write).Scan(&found); err != nil {
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
	if role != "owner" && role != "supervisor" && role != "operator" {
		return fail(ErrDenied)
	}
	return tx, nil
}

func (s *Service) finish(ctx context.Context, tx *sql.Tx, a Actor) error {
	if err := s.checkSession(ctx, tx, a); err != nil {
		return err
	}
	return tx.Commit()
}

func audit(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, conversationID, event string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO supervisor.chat_audit(tenant_id,site_id,conversation_id,actor_issuer,actor_subject,event_type) VALUES($1,$2,$3,$4,$5,$6)`, sc.TenantID, sc.SiteID, conversationID, a.Issuer, a.Subject, event)
	return err
}

// receipt returns the stored result for an idempotency key, or ErrConflict when the key was used
// for a different request.
func receipt(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, key, digest string) ([]byte, bool, error) {
	var saved string
	var result []byte
	err := tx.QueryRowContext(ctx, `SELECT request_sha256,result FROM supervisor.chat_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&saved, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if saved != digest {
		return nil, false, ErrConflict
	}
	return result, true, nil
}

func saveReceipt(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, key, operation, digest, conversationID string, result any) error {
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.chat_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,operation,request_sha256,conversation_id,result) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, operation, digest, conversationID, body)
	return err
}

// loadConversation returns the caller's own conversation. Another member's conversation, or one
// on another site or tenant, is the same non-disclosing ErrNotFound.
func loadConversation(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, id string, lock bool) (Conversation, error) {
	var c Conversation
	if !validID(id) {
		return c, ErrNotFound
	}
	q := `SELECT conversation_id::text,title,revision,created_at,updated_at FROM supervisor.chat_conversations WHERE tenant_id=$1 AND site_id=$2 AND conversation_id=$3 AND author_issuer=$4 AND author_subject=$5`
	if lock {
		q += ` FOR UPDATE`
	}
	err := tx.QueryRowContext(ctx, q, sc.TenantID, sc.SiteID, id, a.Issuer, a.Subject).Scan(&c.ConversationID, &c.Title, &c.Revision, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	c.CreatedAt, c.UpdatedAt = c.CreatedAt.UTC(), c.UpdatedAt.UTC()
	return c, err
}

// Create starts a conversation owned by the caller.
func (s *Service) Create(ctx context.Context, a Actor, sc Scope, in CreateInput, key string) (Conversation, error) {
	if in.Validate() != nil || !validKey(key) {
		return Conversation{}, ErrInvalid
	}
	tx, err := s.begin(ctx, a, sc, true)
	if err != nil {
		return Conversation{}, err
	}
	defer tx.Rollback()
	digest := requestHash("create", in)
	if saved, ok, err := receipt(ctx, tx, a, sc, key, digest); err != nil || ok {
		if err != nil {
			return Conversation{}, err
		}
		var out Conversation
		if err = json.Unmarshal(saved, &out); err != nil {
			return Conversation{}, err
		}
		out, err = loadConversation(ctx, tx, a, sc, out.ConversationID, false)
		if err != nil {
			return Conversation{}, err
		}
		if err = audit(ctx, tx, a, sc, out.ConversationID, "mutation.replayed"); err != nil {
			return Conversation{}, err
		}
		return out, s.finish(ctx, tx, a)
	}
	id := uuid.NewString()
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.chat_conversations(tenant_id,site_id,conversation_id,author_issuer,author_subject,title,revision) VALUES($1,$2,$3,$4,$5,$6,1)`, sc.TenantID, sc.SiteID, id, a.Issuer, a.Subject, in.Title); err != nil {
		return Conversation{}, err
	}
	out, err := loadConversation(ctx, tx, a, sc, id, false)
	if err != nil {
		return Conversation{}, err
	}
	if err = audit(ctx, tx, a, sc, id, "conversation.created"); err != nil {
		return Conversation{}, err
	}
	if err = saveReceipt(ctx, tx, a, sc, key, "create", digest, id, out); err != nil {
		return Conversation{}, err
	}
	return out, s.finish(ctx, tx, a)
}

// List returns the caller's own conversations by ID with a stable cursor.
func (s *Service) List(ctx context.Context, a Actor, sc Scope, limit int, cursor string) (ConversationPage, error) {
	out := ConversationPage{Conversations: []Conversation{}}
	if limit < 1 || limit > 100 || (cursor != "" && !validID(cursor)) {
		return out, ErrInvalid
	}
	if cursor == "" {
		cursor = uuid.Nil.String()
	}
	tx, err := s.begin(ctx, a, sc, false)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT conversation_id::text,title,revision,created_at,updated_at FROM supervisor.chat_conversations
 WHERE tenant_id=$1 AND site_id=$2 AND author_issuer=$3 AND author_subject=$4 AND conversation_id>$5
 ORDER BY conversation_id LIMIT $6`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, cursor, limit+1)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var c Conversation
		if err = rows.Scan(&c.ConversationID, &c.Title, &c.Revision, &c.CreatedAt, &c.UpdatedAt); err != nil {
			rows.Close()
			return out, err
		}
		c.CreatedAt, c.UpdatedAt = c.CreatedAt.UTC(), c.UpdatedAt.UTC()
		out.Conversations = append(out.Conversations, c)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	if len(out.Conversations) > limit {
		out.Conversations = out.Conversations[:limit]
		out.NextCursor = out.Conversations[limit-1].ConversationID
	}
	return out, s.finish(ctx, tx, a)
}

// Get re-authorizes the caller and returns their conversation.
func (s *Service) Get(ctx context.Context, a Actor, sc Scope, id string) (Conversation, error) {
	tx, err := s.begin(ctx, a, sc, false)
	if err != nil {
		return Conversation{}, err
	}
	defer tx.Rollback()
	out, err := loadConversation(ctx, tx, a, sc, id, false)
	if err != nil {
		return Conversation{}, err
	}
	if err = audit(ctx, tx, a, sc, id, "conversation.read"); err != nil {
		return Conversation{}, err
	}
	return out, s.finish(ctx, tx, a)
}

const messageColumns = `message_id::text,seq,author_kind,request_id::text,reply_to::text,body,state,reason_code,citations,dependencies,created_at,COALESCE(run_id::text,'')`

func scanMessage(row interface{ Scan(...any) error }, author *ActorRef) (Message, error) {
	var m Message
	var request, reply, body, reason sql.NullString
	var citations, dependencies []byte
	if err := row.Scan(&m.MessageID, &m.Seq, &m.AuthorKind, &request, &reply, &body, &m.State, &reason, &citations, &dependencies, &m.CreatedAt, &m.runID); err != nil {
		return m, err
	}
	str := func(v sql.NullString) *string {
		if !v.Valid {
			return nil
		}
		s := v.String
		return &s
	}
	m.RequestID, m.ReplyTo, m.Body, m.ReasonCode = str(request), str(reply), str(body), str(reason)
	m.CreatedAt = m.CreatedAt.UTC()
	m.Citations = []Citation{}
	if err := json.Unmarshal(citations, &m.Citations); err != nil {
		return m, err
	}
	var provenance agent.ChatSnapshotProvenance
	if err := json.Unmarshal(dependencies, &provenance); err != nil {
		return m, err
	}
	if provenance.SnapshotID != "" {
		m.SnapshotID, m.SnapshotCreatedAt = &provenance.SnapshotID, &provenance.SnapshotCreatedAt
		m.SnapshotStale = &provenance.SnapshotStale
		if provenance.Retrieval != "" {
			m.Retrieval = &provenance.Retrieval
		}
	}
	if len(provenance.Quantities) > 0 {
		m.Quantities = provenance.Quantities
	}
	m.ClaimCitations = provenance.ClaimCitations
	if m.AuthorKind == AuthorHuman {
		m.Author = author
	}
	return m, nil
}

// reauthorize re-checks a stored answer's captured evidence on every read and replay (rules 06
// and 07, WIS-517 review). When a source the run captured has been revoked or its access epoch
// has moved since, the saved body and citations are withheld and the reply reads as the
// needs_input EVIDENCE_REVOKED it would have ended as had the revocation come before the answer.
// The stored row is untouched: replies are immutable once terminal.
func (s *Service) reauthorize(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, m *Message) error {
	if m.AuthorKind != AuthorAssistant || (m.Body == nil && len(m.Citations) == 0 && len(m.Quantities) == 0 && len(m.ClaimCitations) == 0) {
		return nil
	}
	rs := agent.RunScope{TenantID: sc.TenantID, SiteID: sc.SiteID}
	var refs []agent.RunRef
	var err error
	if m.runID != "" {
		refs, err = agent.RefsTx(ctx, tx, rs, m.runID)
	}
	if err != nil {
		return err
	}
	var ids []string
	for _, r := range refs {
		if r.Kind == agent.RefEvidence {
			ids = append(ids, r.RefID)
		}
	}
	heads, err := evidence.HeadsTx(ctx, tx, evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, ids)
	if err != nil {
		return err
	}
	for _, r := range refs {
		if h, ok := heads[r.RefID]; r.Kind == agent.RefEvidence && (!ok || h.Revoked || h.AccessEpoch != r.AccessEpoch) {
			reason := agent.CodeEvidenceRevoked
			// Quantities are drawn from the same evidence, so they are withheld with the body (rule 06).
			withholdAnswer(m, reason)
			return nil
		}
	}
	if err := s.reauthorizeCitations(ctx, tx, a, sc, m, refs); err != nil || m.Body == nil {
		return err
	}
	return reauthorizeClaims(ctx, tx, sc, m, refs)
}

// Messages returns one page of a conversation in order, after re-authorizing the caller.
func (s *Service) Messages(ctx context.Context, a Actor, sc Scope, id string, limit int, cursor string) (MessagePage, error) {
	out := MessagePage{Messages: []Message{}}
	after := int64(0)
	if cursor != "" {
		n, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || n < 1 || strconv.FormatInt(n, 10) != cursor {
			return out, ErrInvalid
		}
		after = n
	}
	if limit < 1 || limit > 100 {
		return out, ErrInvalid
	}
	tx, err := s.begin(ctx, a, sc, false)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if _, err = loadConversation(ctx, tx, a, sc, id, false); err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+messageColumns+` FROM supervisor.chat_messages WHERE tenant_id=$1 AND site_id=$2 AND conversation_id=$3 AND seq>$4 ORDER BY seq LIMIT $5`, sc.TenantID, sc.SiteID, id, after, limit+1)
	if err != nil {
		return out, err
	}
	author := &ActorRef{Issuer: a.Issuer, Subject: a.Subject}
	for rows.Next() {
		m, err := scanMessage(rows, author)
		if err != nil {
			rows.Close()
			return out, err
		}
		out.Messages = append(out.Messages, m)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	for i := range out.Messages {
		if err = s.reauthorize(ctx, tx, a, sc, &out.Messages[i]); err != nil {
			return out, err
		}
	}
	if len(out.Messages) > limit {
		out.Messages = out.Messages[:limit]
		out.NextCursor = strconv.FormatInt(out.Messages[limit-1].Seq, 10)
	}
	if err = audit(ctx, tx, a, sc, id, "messages.read"); err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}

// newQuestion is Post's read-only preflight before any model call: the caller is an owner or
// chat-authorized member of the site (begin), the key is no replay, the conversation is the caller's, the
// request ID is new and the revision matches. Post rechecks all of it in its write transaction.
func (s *Service) newQuestion(ctx context.Context, a Actor, sc Scope, id, requestID, key, digest string, expected int64) bool {
	tx, err := s.begin(ctx, a, sc, false)
	if err != nil {
		return false
	}
	defer tx.Rollback()
	if _, replay, err := receipt(ctx, tx, a, sc, key, digest); err != nil || replay {
		return false
	}
	conv, err := loadConversation(ctx, tx, a, sc, id, false)
	if err != nil || conv.Revision != expected {
		return false
	}
	var asked bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.chat_messages WHERE tenant_id=$1 AND site_id=$2 AND conversation_id=$3 AND request_id=$4)`, sc.TenantID, sc.SiteID, id, requestID).Scan(&asked)
	return err == nil && !asked
}

// Post records a question and its reply in one transaction. The client request ID makes a retry
// after a lost response replay the original pair instead of asking twice; the same request ID
// with different text is ErrConflict. If-Match (expected) is the conversation revision.
func (s *Service) Post(ctx context.Context, a Actor, sc Scope, id string, in PostInput, key string, expected int64) (PostResult, error) {
	if in.Validate() != nil || !validKey(key) || expected < 1 {
		return PostResult{}, ErrInvalid
	}
	digest := requestHash("post", struct {
		ConversationID, RequestID, Text string
		Expected                        int64
	}{id, in.RequestID, in.Text, expected})
	// Model inference runs before the write transaction, never under its locks, and only for a
	// post the read-only preflight shows would record a new question (WIS-627 review).
	var plan *retrievalPlan
	if s.assistant && s.answer.Enabled && s.newQuestion(ctx, a, sc, id, in.RequestID, key, digest, expected) {
		plan = s.planRetrieval(ctx, a, sc, in.Text)
	}
	tx, err := s.begin(ctx, a, sc, true)
	if err != nil {
		return PostResult{}, err
	}
	defer tx.Rollback()
	if saved, ok, err := receipt(ctx, tx, a, sc, key, digest); err != nil || ok {
		if err != nil {
			return PostResult{}, err
		}
		var out PostResult
		if err = json.Unmarshal(saved, &out); err != nil {
			return PostResult{}, err
		}
		out.Replayed = true
		if err = audit(ctx, tx, a, sc, out.Conversation.ConversationID, "mutation.replayed"); err != nil {
			return PostResult{}, err
		}
		return out, s.finish(ctx, tx, a)
	}
	conv, err := loadConversation(ctx, tx, a, sc, id, true)
	if err != nil {
		return PostResult{}, err
	}
	author := &ActorRef{Issuer: a.Issuer, Subject: a.Subject}
	// A retried request (new key, same client request ID) replays the recorded pair.
	question, err := scanMessage(tx.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM supervisor.chat_messages WHERE tenant_id=$1 AND site_id=$2 AND conversation_id=$3 AND request_id=$4`, sc.TenantID, sc.SiteID, id, in.RequestID), author)
	if err == nil {
		if question.Body == nil || *question.Body != in.Text {
			return PostResult{}, ErrConflict
		}
		reply, err := scanMessage(tx.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM supervisor.chat_messages WHERE tenant_id=$1 AND site_id=$2 AND conversation_id=$3 AND reply_to=$4`, sc.TenantID, sc.SiteID, id, question.MessageID), author)
		if err != nil {
			return PostResult{}, err
		}
		if err = s.reauthorize(ctx, tx, a, sc, &reply); err != nil {
			return PostResult{}, err
		}
		if err = audit(ctx, tx, a, sc, id, "mutation.replayed"); err != nil {
			return PostResult{}, err
		}
		return PostResult{Conversation: conv, Question: question, Reply: reply, Replayed: true}, s.finish(ctx, tx, a)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return PostResult{}, err
	}
	if conv.Revision != expected {
		return PostResult{}, ErrRevision
	}
	var last int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(max(seq),0) FROM supervisor.chat_messages WHERE tenant_id=$1 AND site_id=$2 AND conversation_id=$3`, sc.TenantID, sc.SiteID, id).Scan(&last); err != nil {
		return PostResult{}, err
	}
	questionID, replyID := uuid.NewString(), uuid.NewString()
	replyState, reason, runID := StateUnavailable, ReasonDisabled, ""
	var provenance *agent.ChatSnapshotProvenance
	switch {
	case !s.assistant:
	case !s.answer.Enabled:
		reason = ReasonNotQualified
	default:
		// Slice 2: a commissioned site answers through a captured conversation run.
		if runID, reason, provenance, err = s.startAnswer(ctx, tx, a, sc, id, in.RequestID, in.Text, plan); err != nil {
			return PostResult{}, err
		}
		if runID != "" {
			replyState, reason = StatePending, ""
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.chat_messages(tenant_id,site_id,conversation_id,message_id,seq,author_kind,request_id,body,state,revision) VALUES($1,$2,$3,$4,$5,'human',$6,$7,'received',1)`,
		sc.TenantID, sc.SiteID, id, questionID, last+1, in.RequestID, in.Text); err != nil {
		return PostResult{}, err
	}
	dependencies := []byte(`{}`)
	if provenance != nil {
		dependencies, err = json.Marshal(provenance)
		if err != nil {
			return PostResult{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.chat_messages(tenant_id,site_id,conversation_id,message_id,seq,author_kind,reply_to,state,reason_code,run_id,dependencies,revision) VALUES($1,$2,$3,$4,$5,'assistant',$6,$7,NULLIF($8,''),NULLIF($9,'')::uuid,$10,1)`,
		sc.TenantID, sc.SiteID, id, replyID, last+2, questionID, replyState, reason, runID, dependencies); err != nil {
		return PostResult{}, err
	}
	if runID != "" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.chat_answer_turns(tenant_id,site_id,run_id,conversation_id,question_message_id,reply_message_id) VALUES($1,$2,$3,$4,$5,$6)`, sc.TenantID, sc.SiteID, runID, id, questionID, replyID); err != nil {
			return PostResult{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE supervisor.chat_conversations SET revision=revision+1,updated_at=clock_timestamp() WHERE tenant_id=$1 AND site_id=$2 AND conversation_id=$3`, sc.TenantID, sc.SiteID, id); err != nil {
		return PostResult{}, err
	}
	out := PostResult{}
	if out.Conversation, err = loadConversation(ctx, tx, a, sc, id, false); err != nil {
		return PostResult{}, err
	}
	if out.Question, err = scanMessage(tx.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM supervisor.chat_messages WHERE tenant_id=$1 AND site_id=$2 AND message_id=$3`, sc.TenantID, sc.SiteID, questionID), author); err != nil {
		return PostResult{}, err
	}
	if out.Reply, err = scanMessage(tx.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM supervisor.chat_messages WHERE tenant_id=$1 AND site_id=$2 AND message_id=$3`, sc.TenantID, sc.SiteID, replyID), author); err != nil {
		return PostResult{}, err
	}
	if err = audit(ctx, tx, a, sc, id, "message.posted"); err != nil {
		return PostResult{}, err
	}
	if err = saveReceipt(ctx, tx, a, sc, key, "post", digest, id, out); err != nil {
		return PostResult{}, err
	}
	return out, s.finish(ctx, tx, a)
}
