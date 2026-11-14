// Package evidence stores bounded, reported plaintext provenance, never approved knowledge.
package evidence

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/approvals"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
)

var (
	ErrNotFound         = errors.New("evidence unavailable")
	ErrDenied           = errors.New("evidence permission denied")
	ErrInvalid          = errors.New("invalid evidence request")
	ErrConflict         = errors.New("idempotency conflict")
	ErrRevision         = errors.New("evidence revision changed")
	ErrReauthentication = errors.New("recent authentication required")
)

const MaxTextBytes = 262144

type Actor struct{ Issuer, Subject, SessionHash string }
type Scope struct{ TenantID, SiteID string }
type Input struct {
	SourceID string `json:"source_id"`
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	Text     string `json:"text"`
	// Optional. omitempty keeps pre-WIS-270 idempotency fingerprints stable.
	AuthorityClass      string `json:"authority_class,omitempty"`
	IndependenceGroupID string `json:"independence_group_id,omitempty"`
}
type Source struct {
	SourceID        string    `json:"source_id"`
	Revision        int64     `json:"revision"`
	Kind            string    `json:"kind"`
	Title           string    `json:"title"`
	Text            string    `json:"text"`
	SHA256          string    `json:"sha256"`
	ByteLength      int       `json:"byte_length"`
	RuneLength      int       `json:"rune_length"`
	Authority       string    `json:"authority"`
	AuthorityClass  string    `json:"authority_class"`
	GroupID         string    `json:"independence_group_id"`
	CreatedAt       time.Time `json:"created_at"`
	CurrentRevision int64     `json:"current_revision"`
	StateRevision   int64     `json:"state_revision"`
	AccessEpoch     int64     `json:"access_epoch"`
	// Extraction is set when a model extracted the text (WIS-625 OCR): untrusted extraction,
	// labelled wherever the source is shown. A typed or born-digital revision has none.
	Extraction *Extraction `json:"extraction,omitempty"`
}
type SpanInput struct {
	SourceID string `json:"source_id"`
	Revision int64  `json:"revision"`
	Start    int    `json:"start_char"`
	End      int    `json:"end_char"`
}
type Span struct {
	ID string `json:"id"`
	SpanInput
	Text         string `json:"text"`
	SourceSHA256 string `json:"source_sha256"`
	AccessEpoch  int64  `json:"access_epoch"`
}
type Revocation struct {
	SourceID      string `json:"source_id"`
	StateRevision int64  `json:"state_revision"`
	AccessEpoch   int64  `json:"access_epoch"`
	Revoked       bool   `json:"revoked"`
}
type Summary struct {
	SourceID       string `json:"source_id"`
	Revision       int64  `json:"revision"`
	StateRevision  int64  `json:"state_revision"`
	AccessEpoch    int64  `json:"access_epoch"`
	Kind           string `json:"kind"`
	Title          string `json:"title"`
	SHA256         string `json:"sha256"`
	ByteLength     int    `json:"byte_length"`
	RuneLength     int    `json:"rune_length"`
	Authority      string `json:"authority"`
	AuthorityClass string `json:"authority_class"`
	GroupID        string `json:"independence_group_id"`
	// ModelExtracted says a model extracted the current revision's text (WIS-625 OCR), so whoever
	// picks the source as evidence sees it before relying on it.
	ModelExtracted bool `json:"model_extracted"`
}
type Page struct {
	Sources    []Summary `json:"sources"`
	NextCursor string    `json:"next_cursor,omitempty"`
}
type Service struct {
	db       *sql.DB
	sessions session.Policy
	scanCap  int
}

func New(db *sql.DB) *Service {
	return &Service{db: db, sessions: session.Default(), scanCap: DefaultScanTenantCap}
}

// WithScanTenantCap returns a copy of the service that admits at most n running scan pages per
// tenant (WIS-715). n <= 0 keeps DefaultScanTenantCap.
func (s *Service) WithScanTenantCap(n int) *Service {
	c := *s
	if n > 0 {
		c.scanCap = n
	}
	return &c
}

// WithSessionPolicy returns a copy of the service that enforces p (WIS-290: one configurable
// session policy for every service). New uses session.Default.
func (s *Service) WithSessionPolicy(p session.Policy) *Service {
	c := *s
	c.sessions = p.OrDefault()
	return &c
}

func hash(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
func text(s string, n int) bool {
	return utf8.ValidString(s) && !strings.ContainsRune(s, 0) && len(s) > 0 && utf8.RuneCountInString(s) <= n
}
func identifier(s string) bool {
	id, e := uuid.Parse(s)
	return e == nil && id != uuid.Nil && id.String() == s
}
func validScope(s Scope) bool { return identifier(s.TenantID) && identifier(s.SiteID) }
func validActor(a Actor) bool {
	h, e := hex.DecodeString(a.SessionHash)
	return e == nil && len(h) == 32 && a.SessionHash == strings.ToLower(a.SessionHash) && text(a.Issuer, 512) && text(a.Subject, 255)
}
func mutation(key string, expected int64) bool {
	return text(key, 128) && len(key) <= 128 && expected >= 0 && expected < math.MaxInt64
}
func fingerprint(op string, input any, expected int64) string {
	b, _ := json.Marshal(struct {
		Op       string
		Input    any
		Expected int64
	}{op, input, expected})
	return hash(b)
}
func (s *Service) checkSession(ctx context.Context, tx *sql.Tx, a Actor, recent bool) error {
	var issuer, subject string
	var exp, seen, now time.Time
	var auth sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT issuer,subject,expires_at,last_seen_at,auth_time,clock_timestamp() FROM supervisor.browser_sessions WHERE token_hash=$1 FOR SHARE`, a.SessionHash).Scan(&issuer, &subject, &exp, &seen, &auth, &now)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrDenied
	}
	if err != nil {
		return err
	}
	if issuer != a.Issuer || subject != a.Subject || !exp.After(now) || !seen.After(now.Add(-s.sessions.Idle)) {
		return ErrDenied
	}
	if recent && (!auth.Valid || auth.Time.After(now) || auth.Time.Before(now.Add(-s.sessions.RecentAuth))) {
		return ErrReauthentication
	}
	return nil
}

// Site lock precedes membership/head locks, matching membership administration.
func (s *Service) begin(ctx context.Context, a Actor, sc Scope, write, revoke bool) (*sql.Tx, error) {
	if !validScope(sc) {
		return nil, ErrInvalid
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
	if err = s.checkSession(ctx, tx, a, revoke); err != nil {
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
	if revoke && role != "owner" {
		return fail(ErrDenied)
	}
	if write && !revoke && role != "owner" && role != "supervisor" && role != "operator" {
		return fail(ErrDenied)
	}
	if err = s.checkSession(ctx, tx, a, revoke); err != nil {
		return fail(err)
	}
	return tx, nil
}

type head struct {
	revision, state, epoch int64
	revoked                bool
}

func getHead(ctx context.Context, tx *sql.Tx, sc Scope, id string, write bool) (head, error) {
	var h head
	lock := " FOR SHARE"
	if write {
		lock = " FOR UPDATE"
	}
	err := tx.QueryRowContext(ctx, `SELECT current_revision,state_revision,access_epoch,(revoked OR EXISTS(SELECT 1 FROM supervisor.evidence_revocations v WHERE (v.tenant_id,v.site_id,v.source_id)=(supervisor.evidence_heads.tenant_id,supervisor.evidence_heads.site_id,supervisor.evidence_heads.source_id))) FROM supervisor.evidence_heads WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`+lock, sc.TenantID, sc.SiteID, id).Scan(&h.revision, &h.state, &h.epoch, &h.revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return h, ErrNotFound
	}
	return h, err
}
func readSource(ctx context.Context, tx *sql.Tx, sc Scope, id string, revision int64, h head) (Source, error) {
	var r Source
	if h.revoked {
		return r, ErrNotFound
	}
	if revision == 0 {
		revision = h.revision
	}
	var x struct{ method, model, revision, prompt, run sql.NullString }
	err := tx.QueryRowContext(ctx, `SELECT source_id,revision,kind,title,body,sha256,byte_length,rune_length,authority,authority_class,independence_group_id,created_at,
 extraction,extractor_model,extractor_model_revision,extractor_prompt_sha256,extraction_run_id::text FROM supervisor.evidence_revisions WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND revision=$4`, sc.TenantID, sc.SiteID, id, revision).Scan(&r.SourceID, &r.Revision, &r.Kind, &r.Title, &r.Text, &r.SHA256, &r.ByteLength, &r.RuneLength, &r.Authority, &r.AuthorityClass, &r.GroupID, &r.CreatedAt,
		&x.method, &x.model, &x.revision, &x.prompt, &x.run)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if x.method.Valid {
		r.Extraction = &Extraction{Method: x.method.String, Model: x.model.String, ModelRevision: x.revision.String, PromptSHA256: x.prompt.String, RunID: x.run.String}
	}
	r.CurrentRevision = h.revision
	r.StateRevision = h.state
	r.AccessEpoch = h.epoch
	return r, err
}
func audit(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, source any, revision any, event string, reason any) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO supervisor.evidence_audit(tenant_id,site_id,source_id,revision,actor_issuer,actor_subject,event_type,reason) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, sc.TenantID, sc.SiteID, source, revision, a.Issuer, a.Subject, event, reason)
	return err
}
func (s *Service) finish(ctx context.Context, tx *sql.Tx, a Actor, recent bool) error {
	if err := s.checkSession(ctx, tx, a, recent); err != nil {
		return err
	}
	return tx.Commit()
}

// Receipts contain only revision/span identifiers, never raw source content.
type receipt struct {
	CurrentnessRevision int64       `json:"currentness_revision,omitempty"`
	AuthorityRevision   int64       `json:"authority_revision,omitempty"`
	Revision            int64       `json:"revision,omitempty"`
	SpanID              string      `json:"span_id,omitempty"`
	Revocation          *Revocation `json:"revocation,omitempty"`
	// A document receipt (WIS-613) names its source and its spans in text order.
	SourceID string   `json:"source_id,omitempty"`
	SpanIDs  []string `json:"span_ids,omitempty"`
	// FigurePolicy and Figures (WIS-625 post-merge) record which figures a document upload queued
	// for the vision model: the policy version and each figure's SHA-256, in order. A receipt
	// without a policy was stored before figures were read, and replays without them.
	FigurePolicy int      `json:"figure_policy,omitempty"`
	Figures      []string `json:"figures,omitempty"`
}

func replay(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, key, digest string) (receipt, bool, error) {
	var r receipt
	var stored string
	var b []byte
	err := tx.QueryRowContext(ctx, `SELECT request_hash,result FROM supervisor.evidence_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&stored, &b)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	if stored != digest {
		return r, false, ErrConflict
	}
	err = json.Unmarshal(b, &r)
	return r, true, err
}
func saveReceipt(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, id, key, digest string, r receipt) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.evidence_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_hash,source_id,result) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, digest, id, b)
	return err
}
func (s *Service) CreateOrRevise(ctx context.Context, a Actor, sc Scope, in Input, key string, expected int64) (Source, error) {
	var out Source
	if !identifier(in.SourceID) || !mutation(key, expected) || !text(in.Title, 200) || !text(in.Text, MaxTextBytes) || len(in.Text) > MaxTextBytes {
		return out, ErrInvalid
	}
	if in.Kind != "manual" && in.Kind != "interview" && in.Kind != "document" && in.Kind != "event" {
		return out, ErrInvalid
	}
	// Callers may only describe how they obtained the source. approved_document is reserved for
	// designated revisions (set by the 039 backfill) so the label can never be self-asserted.
	if (in.AuthorityClass != "" && in.AuthorityClass != "reported" && in.AuthorityClass != "observed") || (in.IndependenceGroupID != "" && !identifier(in.IndependenceGroupID)) {
		return out, ErrInvalid
	}
	tx, err := s.begin(ctx, a, sc, true, false)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	h, err := getHead(ctx, tx, sc, in.SourceID, true)
	exists := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return out, err
	}
	if exists && h.revoked {
		return out, ErrNotFound
	}
	digest := fingerprint("source", in, expected)
	prior, ok, err := replay(ctx, tx, a, sc, key, digest)
	if err != nil {
		return out, err
	}
	if ok {
		out, err = readSource(ctx, tx, sc, in.SourceID, prior.Revision, h)
		if err != nil {
			return out, err
		}
		if err = audit(ctx, tx, a, sc, in.SourceID, prior.Revision, "mutation.replayed", nil); err != nil {
			return out, err
		}
		return out, s.finish(ctx, tx, a, false)
	}
	if (!exists && expected != 0) || (exists && h.state != expected) {
		return out, ErrRevision
	}
	group, err := independenceGroup(ctx, tx, sc, in, exists, h.revision)
	if err != nil {
		return out, err
	}
	class := in.AuthorityClass
	if class == "" {
		class = "reported"
	}
	revision := int64(1)
	event := "source.created"
	var extraction *Extraction
	if exists {
		old, e := readSource(ctx, tx, sc, in.SourceID, h.revision, h)
		if e != nil {
			return out, e
		}
		if old.Kind != in.Kind {
			return out, ErrInvalid
		}
		// WIS-625 fail closed: a revision of model-extracted text stays model-extracted, so
		// re-saving it cannot shed the confirmation gate or count as independent corroboration.
		extraction = old.Extraction
		revision = h.revision + 1
		event = "source.revised"
		_, err = tx.ExecContext(ctx, `UPDATE supervisor.evidence_heads SET current_revision=current_revision+1,state_revision=state_revision+1 WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, sc.TenantID, sc.SiteID, in.SourceID)
		h.revision++
		h.state++
	} else {
		h, err = insertHead(ctx, tx, sc, in.SourceID)
	}
	if err != nil {
		return out, err
	}
	if err = insertRevision(ctx, tx, a, sc, in, revision, class, group, extraction); err != nil {
		return out, err
	}
	if err = saveReceipt(ctx, tx, a, sc, in.SourceID, key, digest, receipt{Revision: revision}); err != nil {
		return out, err
	}
	if err = audit(ctx, tx, a, sc, in.SourceID, revision, event, nil); err != nil {
		return out, err
	}
	out, err = readSource(ctx, tx, sc, in.SourceID, revision, h)
	if err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a, false)
}

// insertHead creates the head of a new source at its first revision.
func insertHead(ctx context.Context, tx *sql.Tx, sc Scope, id string) (head, error) {
	_, err := tx.ExecContext(ctx, `INSERT INTO supervisor.evidence_heads(tenant_id,site_id,source_id,current_revision,state_revision) VALUES($1,$2,$3,1,1)`, sc.TenantID, sc.SiteID, id)
	return head{1, 1, 1, false}, err
}

// insertRevision stores one immutable source revision with its digest and lengths, and the
// model extraction it carries forward (nil for typed or born-digital text).
func insertRevision(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, in Input, revision int64, class string, group any, x *Extraction) error {
	var method, model, modelRevision, prompt, run any
	if x != nil {
		method, model, modelRevision, prompt, run = x.Method, x.Model, x.ModelRevision, x.PromptSHA256, x.RunID
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO supervisor.evidence_revisions(tenant_id,site_id,source_id,revision,kind,title,body,sha256,byte_length,rune_length,actor_issuer,actor_subject,authority_class,independence_group_id,
 extraction,extractor_model,extractor_model_revision,extractor_prompt_sha256,extraction_run_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`, sc.TenantID, sc.SiteID, in.SourceID, revision, in.Kind, in.Title, in.Text, hash([]byte(in.Text)), len(in.Text), utf8.RuneCountInString(in.Text), a.Issuer, a.Subject, class, group,
		method, model, modelRevision, prompt, run)
	return err
}

// independenceGroup picks the WIS-270 independence group for a new revision (nil = the source
// itself). Bytes already stored in this site join that revision's group, whatever the caller
// asks: copies and re-extractions of one SOP are one group, so the caller cannot split them.
// Otherwise a caller group must already exist in this site (404 otherwise), else a revision
// keeps its source's previous group. The site write lock serializes concurrent creates.
// Pre-039 duplicate pairs stay in separate groups; a new copy joins the earliest same-hash group.
func independenceGroup(ctx context.Context, tx *sql.Tx, sc Scope, in Input, exists bool, current int64) (any, error) {
	var matched string
	err := tx.QueryRowContext(ctx, `SELECT independence_group_id FROM supervisor.evidence_revisions WHERE tenant_id=$1 AND site_id=$2 AND sha256=$3 ORDER BY created_at,source_id,revision LIMIT 1`, sc.TenantID, sc.SiteID, hash([]byte(in.Text))).Scan(&matched)
	switch {
	case err == nil:
		if in.IndependenceGroupID != "" && in.IndependenceGroupID != matched {
			return nil, ErrInvalid
		}
		return matched, nil
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	case in.IndependenceGroupID != "":
		var found bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.evidence_revisions WHERE tenant_id=$1 AND site_id=$2 AND independence_group_id=$3)`, sc.TenantID, sc.SiteID, in.IndependenceGroupID).Scan(&found); err != nil {
			return nil, err
		}
		if !found {
			return nil, ErrNotFound
		}
		return in.IndependenceGroupID, nil
	case exists:
		var previous string
		err = tx.QueryRowContext(ctx, `SELECT independence_group_id FROM supervisor.evidence_revisions WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND revision=$4`, sc.TenantID, sc.SiteID, in.SourceID, current).Scan(&previous)
		return previous, err
	}
	return nil, nil
}
func (s *Service) Read(ctx context.Context, a Actor, sc Scope, id string, revision int64) (Source, error) {
	var out Source
	if !identifier(id) || revision < 0 {
		return out, ErrInvalid
	}
	tx, err := s.begin(ctx, a, sc, false, false)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	h, err := getHead(ctx, tx, sc, id, false)
	if err != nil {
		return out, err
	}
	out, err = readSource(ctx, tx, sc, id, revision, h)
	if err != nil {
		return out, err
	}
	if err = audit(ctx, tx, a, sc, id, out.Revision, "source.read", nil); err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a, false)
}

// ReadParticipantTx applies Read's browser-session and membership checks inside a caller's
// already site-locked transaction, so captured agent scope is authorized by the participant.
func (s *Service) ReadParticipantTx(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, id string, revision int64) (Source, error) {
	var out Source
	if tx == nil || !validScope(sc) || !validActor(a) || !identifier(id) || revision < 0 {
		return out, ErrInvalid
	}
	if err := s.checkSession(ctx, tx, a, false); err != nil {
		return out, err
	}
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND active)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject).Scan(&active); err != nil {
		return out, err
	} else if !active {
		return out, ErrNotFound
	}
	h, err := getHead(ctx, tx, sc, id, false)
	if err != nil {
		return out, err
	}
	if out, err = readSource(ctx, tx, sc, id, revision, h); err != nil {
		return Source{}, err
	}
	return out, audit(ctx, tx, a, sc, id, out.Revision, "source.read", nil)
}

func (s *Service) List(ctx context.Context, a Actor, sc Scope, limit int, cursor string) (Page, error) {
	out := Page{Sources: []Summary{}}
	if limit < 1 || limit > 100 || (cursor != "" && !identifier(cursor)) {
		return out, ErrInvalid
	}
	tx, err := s.begin(ctx, a, sc, false, false)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if cursor == "" {
		cursor = uuid.Nil.String()
	}
	rows, err := tx.QueryContext(ctx, `SELECT h.source_id,h.current_revision,h.state_revision,h.access_epoch,r.kind,r.title,r.sha256,r.byte_length,r.rune_length,r.authority,r.authority_class,r.independence_group_id,r.extraction IS NOT NULL FROM supervisor.evidence_heads h JOIN supervisor.evidence_revisions r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=(h.tenant_id,h.site_id,h.source_id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND NOT h.revoked AND NOT EXISTS(SELECT 1 FROM supervisor.evidence_revocations v WHERE (v.tenant_id,v.site_id,v.source_id)=(h.tenant_id,h.site_id,h.source_id)) AND h.source_id>$3 ORDER BY h.source_id LIMIT $4 FOR SHARE OF h`, sc.TenantID, sc.SiteID, cursor, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var r Summary
		if err = rows.Scan(&r.SourceID, &r.Revision, &r.StateRevision, &r.AccessEpoch, &r.Kind, &r.Title, &r.SHA256, &r.ByteLength, &r.RuneLength, &r.Authority, &r.AuthorityClass, &r.GroupID, &r.ModelExtracted); err != nil {
			return out, err
		}
		out.Sources = append(out.Sources, r)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(out.Sources) > limit {
		out.Sources = out.Sources[:limit]
		out.NextCursor = out.Sources[limit-1].SourceID
	}
	if err = audit(ctx, tx, a, sc, nil, nil, "source.listed", nil); err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a, false)
}
func (s *Service) CreateSpan(ctx context.Context, a Actor, sc Scope, in SpanInput, key string, expected int64) (Span, error) {
	var out Span
	if !identifier(in.SourceID) || in.Revision < 1 || in.Start < 0 || in.End <= in.Start || in.End > MaxTextBytes || in.End-in.Start > 4096 || !mutation(key, expected) {
		return out, ErrInvalid
	}
	tx, err := s.begin(ctx, a, sc, true, false)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	h, err := getHead(ctx, tx, sc, in.SourceID, true)
	if err != nil {
		return out, err
	}
	if h.revoked {
		return out, ErrNotFound
	}
	digest := fingerprint("span", in, expected)
	prior, ok, err := replay(ctx, tx, a, sc, key, digest)
	if err != nil {
		return out, err
	}
	if ok {
		out, err = readSpan(ctx, tx, sc, prior.SpanID, h)
		if err != nil {
			return out, err
		}
		if err = audit(ctx, tx, a, sc, in.SourceID, in.Revision, "mutation.replayed", nil); err != nil {
			return out, err
		}
		return out, s.finish(ctx, tx, a, false)
	}
	if h.state != expected {
		return out, ErrRevision
	}
	src, err := readSource(ctx, tx, sc, in.SourceID, in.Revision, h)
	if err != nil {
		return out, err
	}
	if in.End > src.RuneLength {
		return out, ErrInvalid
	}
	id, err := insertSpan(ctx, tx, a, sc, in)
	if err != nil {
		return out, err
	}
	if err = saveReceipt(ctx, tx, a, sc, in.SourceID, key, digest, receipt{SpanID: id}); err != nil {
		return out, err
	}
	out, err = readSpan(ctx, tx, sc, id, h)
	if err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a, false)
}

// insertSpan stores one span of a source revision as the actor and audits it.
func insertSpan(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, in SpanInput) (string, error) {
	id := uuid.NewString()
	if _, err := tx.ExecContext(ctx, `INSERT INTO supervisor.evidence_spans(tenant_id,site_id,id,source_id,revision,start_rune,end_rune,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, sc.TenantID, sc.SiteID, id, in.SourceID, in.Revision, in.Start, in.End, a.Issuer, a.Subject); err != nil {
		return "", err
	}
	return id, audit(ctx, tx, a, sc, in.SourceID, in.Revision, "span.created", nil)
}

func readSpan(ctx context.Context, tx *sql.Tx, sc Scope, id string, h head) (Span, error) {
	if h.revoked {
		return Span{}, ErrNotFound
	}
	var in SpanInput
	var body, sha string
	err := tx.QueryRowContext(ctx, `SELECT s.source_id,s.revision,s.start_rune,s.end_rune,r.body,r.sha256 FROM supervisor.evidence_spans s JOIN supervisor.evidence_revisions r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=(s.tenant_id,s.site_id,s.source_id,s.revision) WHERE s.tenant_id=$1 AND s.site_id=$2 AND s.id=$3`, sc.TenantID, sc.SiteID, id).Scan(&in.SourceID, &in.Revision, &in.Start, &in.End, &body, &sha)
	if errors.Is(err, sql.ErrNoRows) {
		return Span{}, ErrNotFound
	}
	if err != nil {
		return Span{}, err
	}
	return spanOf(id, in, []rune(body), sha, h.epoch)
}

// spanOf builds a span's view from its revision's text, refusing a stored range that does not fit.
func spanOf(id string, in SpanInput, runes []rune, sha string, epoch int64) (Span, error) {
	if in.Start < 0 || in.End > len(runes) || in.Start >= in.End || in.End-in.Start > 4096 {
		return Span{}, ErrInvalid
	}
	return Span{ID: id, SpanInput: in, Text: string(runes[in.Start:in.End]), SourceSHA256: sha, AccessEpoch: epoch}, nil
}
func (s *Service) ReadSpan(ctx context.Context, a Actor, sc Scope, id string) (Span, error) {
	var out Span
	if !identifier(id) {
		return out, ErrInvalid
	}
	tx, err := s.begin(ctx, a, sc, false, false)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var source string
	err = tx.QueryRowContext(ctx, `SELECT source_id FROM supervisor.evidence_spans WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, id).Scan(&source)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	h, err := getHead(ctx, tx, sc, source, false)
	if err != nil {
		return out, err
	}
	out, err = readSpan(ctx, tx, sc, id, h)
	if err != nil {
		return out, err
	}
	if err = audit(ctx, tx, a, sc, source, out.Revision, "span.read", nil); err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a, false)
}
func (s *Service) Revoke(ctx context.Context, a Actor, sc Scope, id, reason, key string, expected int64) (Revocation, error) {
	var out Revocation
	if !identifier(id) || !text(reason, 1000) || !mutation(key, expected) || expected == 0 {
		return out, ErrInvalid
	}
	tx, err := s.begin(ctx, a, sc, true, true)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	h, err := getHead(ctx, tx, sc, id, true)
	if err != nil {
		return out, err
	}
	digest := fingerprint("revoke", struct{ ID, Reason string }{id, reason}, expected)
	prior, ok, err := replay(ctx, tx, a, sc, key, digest)
	if err != nil {
		return out, err
	}
	if ok {
		if prior.Revocation == nil {
			return out, ErrConflict
		}
		out = *prior.Revocation
		if err = audit(ctx, tx, a, sc, id, h.revision, "mutation.replayed", nil); err != nil {
			return out, err
		}
		return out, s.finish(ctx, tx, a, true)
	}
	if h.revoked {
		return out, ErrNotFound
	}
	if h.state != expected {
		return out, ErrRevision
	}
	_, err = tx.ExecContext(ctx, `UPDATE supervisor.evidence_heads SET revoked=true,state_revision=state_revision+1,access_epoch=access_epoch+1 WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, sc.TenantID, sc.SiteID, id)
	if err != nil {
		return out, err
	}
	out = Revocation{id, h.state + 1, h.epoch + 1, true}
	_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.evidence_revocations(tenant_id,site_id,source_id,state_revision,access_epoch,actor_issuer,actor_subject,reason) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, sc.TenantID, sc.SiteID, id, out.StateRevision, out.AccessEpoch, a.Issuer, a.Subject, reason)
	if err != nil {
		return out, err
	}
	// WIS-274: an approval that cites this source can no longer be consumed. The same
	// transaction marks every such approved proposal stale (source row, then case, then
	// proposal: the global lock order).
	// This direct call (evidence -> approvals) is deliberate: a hook wired elsewhere could be
	// forgotten and fail open. The WIS-277 executor re-check must therefore read evidence heads
	// through SQL, not by importing evidence into approvals, or the packages would cycle.
	if _, err = approvals.MarkEvidenceProposalsStale(ctx, tx, sc.TenantID, sc.SiteID, id, approvals.ActorRef{Issuer: a.Issuer, Subject: a.Subject}); err != nil {
		return out, err
	}

	if err = saveReceipt(ctx, tx, a, sc, id, key, digest, receipt{Revocation: &out}); err != nil {
		return out, err
	}
	if err = audit(ctx, tx, a, sc, id, h.revision, "source.revoked", reason); err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a, true)
}

// HeadState is the access state of one evidence source head.
type HeadState struct {
	Revision    int64 `json:"revision"`
	AccessEpoch int64 `json:"access_epoch"`
	Revoked     bool  `json:"revoked"`
}

func validServiceActor(a Actor) bool { return text(a.Issuer, 512) && text(a.Subject, 255) }

// ReadTx reads one source revision inside the caller's transaction, which must already carry
// the transaction-local tenant context. It is for server-side service actors (no browser
// session); the caller has authorized the scope. A revoked source returns ErrNotFound and still
// records an audited source.read with reason EVIDENCE_UNAVAILABLE; the caller must commit the
// transaction to retain that audit row. An unknown source returns ErrNotFound without audit.
func ReadTx(ctx context.Context, tx *sql.Tx, sc Scope, a Actor, id string, revision int64) (Source, error) {
	var out Source
	if !validScope(sc) || !identifier(id) || revision < 0 {
		return out, ErrInvalid
	}
	if !validServiceActor(a) {
		return out, ErrDenied
	}
	h, err := getHead(ctx, tx, sc, id, false)
	if err != nil {
		return out, err
	}
	if h.revoked {
		if err = audit(ctx, tx, a, sc, id, nil, "source.read", "EVIDENCE_UNAVAILABLE"); err != nil {
			return out, err
		}
		return out, ErrNotFound
	}
	if out, err = readSource(ctx, tx, sc, id, revision, h); err != nil {
		return Source{}, err
	}
	return out, audit(ctx, tx, a, sc, id, out.Revision, "source.read", nil)
}

// HeadsTx reports the head state of each known source, locked FOR SHARE so a concurrent
// revocation waits for the caller's transaction. Unknown IDs are omitted, never reported as
// revoked or current.
func HeadsTx(ctx context.Context, tx *sql.Tx, sc Scope, ids []string) (map[string]HeadState, error) {
	if !validScope(sc) || len(ids) > 1000 {
		return nil, ErrInvalid
	}
	for _, id := range ids {
		if !identifier(id) {
			return nil, ErrInvalid
		}
	}
	out := make(map[string]HeadState, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT source_id,current_revision,access_epoch,(revoked OR EXISTS(SELECT 1 FROM supervisor.evidence_revocations v WHERE (v.tenant_id,v.site_id,v.source_id)=(supervisor.evidence_heads.tenant_id,supervisor.evidence_heads.site_id,supervisor.evidence_heads.source_id))) FROM supervisor.evidence_heads WHERE tenant_id=$1 AND site_id=$2 AND source_id=ANY($3::uuid[]) ORDER BY source_id FOR SHARE`, sc.TenantID, sc.SiteID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var h HeadState
		if err = rows.Scan(&id, &h.Revision, &h.AccessEpoch, &h.Revoked); err != nil {
			return nil, err
		}
		out[id] = h
	}
	return out, rows.Err()
}
