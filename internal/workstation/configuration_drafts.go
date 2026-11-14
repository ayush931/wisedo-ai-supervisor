package workstation

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
	"github.com/google/uuid"
)

// Proposal F configuration drafts (human path). A draft is never live configuration: it holds
// immutable candidate revisions with field-level citations, and a human promotes it through
// the normal master-data or station-profile revision path. Promotion is the approval act
// (rule 05): the promoter may not be the author or any human editor of the draft.

type DraftKind string
type DraftStatus string
type DraftAuthorKind string

const (
	DraftKindLine              DraftKind       = "line"
	DraftKindItem              DraftKind       = "item"
	DraftKindRouting           DraftKind       = "routing"
	DraftKindProfile           DraftKind       = "workstation_profile"
	DraftOpen                  DraftStatus     = "draft"
	DraftPromoted              DraftStatus     = "promoted"
	DraftDiscarded             DraftStatus     = "discarded"
	DraftAuthorAgent           DraftAuthorKind = "agent"
	DraftAuthorHuman           DraftAuthorKind = "human"
	CodeInvalidDraft                           = "INVALID_CONFIGURATION_DRAFT"
	CodeCitationNotInScope                     = "CITATION_NOT_IN_SCOPE"
	CodeFieldUncited                           = "FIELD_UNCITED"
	CodeDraftLimitExceeded                     = "LIMIT_EXCEEDED"
	MaxDraftFieldSpans                         = 500
	maxDraftFieldPath                          = 256
	configurationDraftPageSize                 = 100
)

var (
	ErrDraftNotOpen           = errors.New("configuration draft is not open")
	ErrPromoterIsEditor       = errors.New("configuration draft promoter authored or edited the draft")
	ErrDraftSourceUnavailable = errors.New("configuration draft cites an unavailable source")
	ErrDraftUnchanged         = errors.New("configuration draft revision is identical to the current revision")
	// ErrTargetRevision means the configuration object moved past the revision the draft was
	// written against: the draft must be discarded and recreated. A stale If-Match on the draft
	// itself stays ErrRevision.
	ErrTargetRevision = errors.New("configuration draft target revision changed")
)

func DraftKinds() []DraftKind {
	return []DraftKind{DraftKindItem, DraftKindLine, DraftKindRouting, DraftKindProfile}
}
func DraftStatuses() []DraftStatus { return []DraftStatus{DraftDiscarded, DraftOpen, DraftPromoted} }
func DraftAuthorKinds() []DraftAuthorKind {
	return []DraftAuthorKind{DraftAuthorAgent, DraftAuthorHuman}
}

func draftInvalid(path string) error { return &ConfigError{CodeInvalidDraft, path} }
func draftKindAllowed(k DraftKind) bool {
	return k == DraftKindLine || k == DraftKindItem || k == DraftKindRouting || k == DraftKindProfile
}

// ConfigurationDraftFieldSpanInputV1 cites one evidence span for one leaf of the body, named by
// a JSON pointer such as /display_name or /steps/0/workstation_id.
type ConfigurationDraftFieldSpanInputV1 struct {
	Path   string `json:"path" contract:"max=256"`
	SpanID string `json:"span_id" contract:"uuid"`
}

// ConfigurationDraftCreateInputV1 creates a human draft. Both target fields absent creates a new
// line, item or routing; a workstation_profile draft always names its workstation in
// target_object_id, with target_expected_revision 0 to create its first profile revision.
type ConfigurationDraftCreateInputV1 struct {
	Kind                   DraftKind                            `json:"kind"`
	TargetObjectID         *string                              `json:"target_object_id,omitempty" contract:"uuid"`
	TargetExpectedRevision *int64                               `json:"target_expected_revision,omitempty"`
	Body                   json.RawMessage                      `json:"body"`
	FieldSpans             []ConfigurationDraftFieldSpanInputV1 `json:"field_spans"`
}

// ConfigurationDraftReviseInputV1 appends a human edit. Citations of leaves whose value is
// unchanged are carried forward; field_spans adds citations to the new revision.
type ConfigurationDraftReviseInputV1 struct {
	Body       json.RawMessage                      `json:"body"`
	FieldSpans []ConfigurationDraftFieldSpanInputV1 `json:"field_spans"`
}

// ConfigurationDraftEmptyInputV1 is the body of a promotion or discard: {}.
type ConfigurationDraftEmptyInputV1 struct{}

type CitationReason string

const (
	// CitationSourceRevoked: the cited source is revoked; the citation is gone for good.
	CitationSourceRevoked CitationReason = "SOURCE_REVOKED"
	// CitationAccessChanged: the source is readable but its access epoch moved since the
	// citation was recorded. Promotion fails closed; any human edit refreshes the epoch.
	CitationAccessChanged CitationReason = "ACCESS_EPOCH_CHANGED"
	// CitationFieldChanged: the edit changed the cited field's value and did not re-cite it.
	CitationFieldChanged CitationReason = "FIELD_CHANGED"
)

func CitationReasons() []CitationReason {
	return []CitationReason{CitationAccessChanged, CitationFieldChanged, CitationSourceRevoked}
}

// ConfigurationDraftCitationV1 is a stored field citation. Available is re-evaluated against the
// evidence ledger on every read; when false, UnavailableReason says why.
type ConfigurationDraftCitationV1 struct {
	Path              string         `json:"path"`
	SpanID            string         `json:"span_id" contract:"uuid"`
	SourceID          string         `json:"source_id" contract:"uuid"`
	SourceRevision    int64          `json:"source_revision"`
	Available         bool           `json:"available"`
	UnavailableReason CitationReason `json:"unavailable_reason,omitempty"`
}

// ConfigurationDraftDroppedCitationV1 is a citation of the previous revision that this revision
// did not carry forward, with the reason (rule 06: no silent provenance loss).
type ConfigurationDraftDroppedCitationV1 struct {
	Path   string         `json:"path"`
	SpanID string         `json:"span_id" contract:"uuid"`
	Reason CitationReason `json:"reason"`
}

type ConfigurationDraftRevisionV1 struct {
	Revision     int64                          `json:"revision"`
	Body         json.RawMessage                `json:"body"`
	BodySHA256   string                         `json:"body_digest" contract:"sha256"`
	EditedByKind DraftAuthorKind                `json:"edited_by_kind"`
	Editor       *ActorRefV1                    `json:"editor,omitempty"`
	RunID        string                         `json:"run_id,omitempty" contract:"uuid"`
	CreatedAt    time.Time                      `json:"created_at"`
	Citations    []ConfigurationDraftCitationV1 `json:"citations"`
	// DroppedCitations lists the previous revision's citations this revision did not keep.
	DroppedCitations []ConfigurationDraftDroppedCitationV1 `json:"dropped_citations"`
}

type ConfigurationDraftPromotionV1 struct {
	ObjectID       string     `json:"object_id" contract:"uuid"`
	ObjectRevision int64      `json:"object_revision"`
	PromotedBy     ActorRefV1 `json:"promoted_by"`
	PromotedAt     time.Time  `json:"promoted_at"`
}

// ConfigurationDraftV1 is a draft with its full revision history. CurrentRevision is the
// If-Match value for revise, promote and discard.
type ConfigurationDraftV1 struct {
	DraftID                string                         `json:"draft_id" contract:"uuid"`
	Kind                   DraftKind                      `json:"kind"`
	AuthorKind             DraftAuthorKind                `json:"author_kind"`
	RunID                  string                         `json:"run_id,omitempty" contract:"uuid"`
	CreatedBy              *ActorRefV1                    `json:"created_by,omitempty"`
	TargetObjectID         string                         `json:"target_object_id,omitempty" contract:"uuid"`
	TargetExpectedRevision *int64                         `json:"target_expected_revision,omitempty"`
	Status                 DraftStatus                    `json:"status"`
	CurrentRevision        int64                          `json:"current_revision"`
	Revisions              []ConfigurationDraftRevisionV1 `json:"revisions"`
	Promotion              *ConfigurationDraftPromotionV1 `json:"promotion,omitempty"`
	CreatedAt              time.Time                      `json:"created_at"`
	UpdatedAt              time.Time                      `json:"updated_at"`
	Replayed               bool                           `json:"replayed"`
}

type ConfigurationDraftSummaryV1 struct {
	DraftID                  string          `json:"draft_id" contract:"uuid"`
	Kind                     DraftKind       `json:"kind"`
	AuthorKind               DraftAuthorKind `json:"author_kind"`
	TargetObjectID           string          `json:"target_object_id,omitempty" contract:"uuid"`
	TargetExpectedRevision   *int64          `json:"target_expected_revision,omitempty"`
	Status                   DraftStatus     `json:"status"`
	CurrentRevision          int64           `json:"current_revision"`
	CitationCount            int             `json:"citation_count"`
	UnavailableCitationCount int             `json:"unavailable_citation_count"`
	UpdatedAt                time.Time       `json:"updated_at"`
}

type ConfigurationDraftPageV1 struct {
	Drafts     []ConfigurationDraftSummaryV1 `json:"drafts"`
	NextCursor string                        `json:"next_cursor,omitempty"`
}

type ConfigurationDraftService struct{ plans *ShiftPlanService }

func NewConfigurationDrafts(db *sql.DB) *ConfigurationDraftService {
	return &ConfigurationDraftService{NewShiftPlans(db)}
}

func (s *ConfigurationDraftService) WithSessionPolicy(p session.Policy) *ConfigurationDraftService {
	return &ConfigurationDraftService{s.plans.WithSessionPolicy(p)}
}

func draftSpec(k DraftKind) masterDataSpec {
	switch k {
	case DraftKindLine:
		return lineMasterData
	case DraftKindItem:
		return itemMasterData
	default:
		return routingMasterData
	}
}

// decodeDraftBody runs the same strict decoders and validators as the direct revision routes and
// returns the normalized body, its stored JSON and its canonical digest.
func decodeDraftBody(sc Scope, kind DraftKind, raw []byte) (any, []byte, string, error) {
	var body any
	var digest string
	if kind == DraftKindProfile {
		p, err := DecodeStationProfile(raw)
		if err != nil {
			var c *ConfigError
			if errors.As(err, &c) {
				return nil, nil, "", draftInvalid(joinBodyPath(c.Path))
			}
			return nil, nil, "", draftInvalid("body")
		}
		if _, digest, err = profileEnvelope(sc, p); err != nil {
			return nil, nil, "", draftInvalid("body")
		}
		body = p
	} else {
		decoded, err := DecodeMasterDataBody(string(kind), raw)
		if err == nil {
			switch v := decoded.(type) {
			case *LineBodyV1:
				body = *v
			case *ItemBodyV1:
				body = *v
			case *RoutingBodyV1:
				body = *v
			}
			body, err = normalizeMasterBody[any](string(kind), body)
		}
		if err != nil {
			return nil, nil, "", masterDraftError(err)
		}
		if _, digest, err = canonical.Envelope(draftSpec(kind).schema, 1, sc.TenantID, sc.SiteID, body); err != nil {
			return nil, nil, "", draftInvalid("body")
		}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, nil, "", draftInvalid("body")
	}
	return body, payload, digest, nil
}

func joinBodyPath(path string) string {
	if path == "" {
		return "body"
	}
	return "body." + path
}

func masterDraftError(err error) error {
	var invalid *MasterDataValidationError
	if errors.As(err, &invalid) {
		return draftInvalid(joinBodyPath(invalid.Path))
	}
	return err
}

// draftLeaves maps every leaf JSON pointer of a stored body to its encoded value. Empty arrays
// and objects are leaves; /schema_version carries no fact and is never cited.
func draftLeaves(payload []byte) (map[string]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return nil, err
	}
	escape := strings.NewReplacer("~", "~0", "/", "~1")
	out := map[string]string{}
	var walk func(string, any)
	walk = func(path string, v any) {
		switch t := v.(type) {
		case map[string]any:
			if len(t) > 0 {
				for k, c := range t {
					walk(path+"/"+escape.Replace(k), c)
				}
				return
			}
		case []any:
			if len(t) > 0 {
				for i, c := range t {
					walk(path+"/"+strconv.Itoa(i), c)
				}
				return
			}
		}
		encoded, _ := json.Marshal(v)
		out[path] = string(encoded)
	}
	walk("", root)
	delete(out, "/schema_version")
	return out, nil
}

type draftSpan struct {
	path, spanID, sourceID string
	sourceRevision, epoch  int64
}

// checkDraftSpans validates requested citations against the body's leaves and the bound.
func checkDraftSpans(in []ConfigurationDraftFieldSpanInputV1, leaves map[string]string) error {
	if len(in) > MaxDraftFieldSpans {
		return &ConfigError{CodeDraftLimitExceeded, "field_spans"}
	}
	seen := map[[2]string]bool{}
	for i, span := range in {
		if _, ok := leaves[span.Path]; !ok || len(span.Path) > maxDraftFieldPath {
			return draftInvalid(fmt.Sprintf("field_spans[%d].path", i))
		}
		if !canonicalID(span.SpanID) {
			return draftInvalid(fmt.Sprintf("field_spans[%d].span_id", i))
		}
		if seen[[2]string{span.Path, span.SpanID}] {
			return draftInvalid(fmt.Sprintf("field_spans[%d]", i))
		}
		seen[[2]string{span.Path, span.SpanID}] = true
	}
	return nil
}

// resolveDraftSpans reads each cited span under the tenant's RLS scope. An unknown,
// cross-tenant or other-site span, or one whose source is revoked, is not in scope.
func resolveDraftSpans(ctx context.Context, tx *sql.Tx, sc Scope, in []ConfigurationDraftFieldSpanInputV1) ([]draftSpan, error) {
	out := make([]draftSpan, 0, len(in))
	if len(in) == 0 {
		return out, nil
	}
	ids := make([]string, len(in))
	for i, span := range in {
		ids[i] = span.SpanID
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,source_id,revision FROM supervisor.evidence_spans WHERE tenant_id=$1 AND site_id=$2 AND id=ANY($3::uuid[])`, sc.TenantID, sc.SiteID, ids)
	if err != nil {
		return nil, err
	}
	found := map[string]draftSpan{}
	for rows.Next() {
		var s draftSpan
		if err = rows.Scan(&s.spanID, &s.sourceID, &s.sourceRevision); err != nil {
			rows.Close()
			return nil, err
		}
		found[s.spanID] = s
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	sources := []string{}
	for _, s := range found {
		sources = append(sources, s.sourceID)
	}
	heads, err := evidence.HeadsTx(ctx, tx, evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, sources)
	if err != nil {
		return nil, err
	}
	for i, span := range in {
		s, ok := found[span.SpanID]
		head, live := heads[s.sourceID]
		if !ok || !live || head.Revoked {
			return nil, &ConfigError{CodeCitationNotInScope, fmt.Sprintf("field_spans[%d].span_id", i)}
		}
		s.path, s.epoch = span.Path, head.AccessEpoch
		out = append(out, s)
	}
	return out, nil
}

func draftAudit(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, draftID string, revision int64, event string) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO supervisor.configuration_draft_audit(tenant_id,site_id,draft_id,revision,actor_issuer,actor_subject,event_type) VALUES($1,$2,NULLIF($3,'')::uuid,NULLIF($4,0),$5,$6,$7)`, sc.TenantID, sc.SiteID, draftID, revision, a.Issuer, a.Subject, event)
	return e
}

// draftReceipt returns the draft of an earlier identical request (with ok=true), or
// ErrConflict when the key was used for a different request.
func draftReceipt(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, key, op, hash string) (string, bool, error) {
	var storedOp, storedHash, draftID string
	err := tx.QueryRowContext(ctx, `SELECT operation,request_sha256,draft_id FROM supervisor.configuration_draft_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&storedOp, &storedHash, &draftID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if storedOp != op || storedHash != hash {
		return "", false, ErrConflict
	}
	return draftID, true, nil
}

func (s *ConfigurationDraftService) replay(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, draftID string) (ConfigurationDraftV1, error) {
	out, err := loadDraft(ctx, tx, sc, draftID)
	if err != nil {
		return out, err
	}
	if err = draftAudit(ctx, tx, a, sc, draftID, out.CurrentRevision, "mutation.replayed"); err != nil {
		return out, err
	}
	out.Replayed = true
	return out, s.plans.finish(ctx, tx, a)
}

func insertDraftRevision(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, draftID string, revision int64, payload []byte, digest string, spans []draftSpan, dropped []ConfigurationDraftDroppedCitationV1, authorKind DraftAuthorKind, runID string) error {
	if dropped == nil {
		dropped = []ConfigurationDraftDroppedCitationV1{}
	}
	droppedJSON, err := json.Marshal(dropped)
	if err != nil {
		return err
	}
	var editorIssuer, editorSubject, agentRun any
	if authorKind == DraftAuthorAgent {
		agentRun = runID
	} else {
		editorIssuer, editorSubject = a.Issuer, a.Subject
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO supervisor.configuration_draft_revisions(tenant_id,site_id,draft_id,revision,body,body_sha256,edited_by_kind,editor_issuer,editor_subject,run_id,dropped_citations) VALUES($1,$2,$3,$4,$5::jsonb,$6,$7,$8,$9,$10,$11::jsonb)`, sc.TenantID, sc.SiteID, draftID, revision, string(payload), digest, string(authorKind), editorIssuer, editorSubject, agentRun, string(droppedJSON)); err != nil {
		return err
	}
	for _, span := range spans {
		if _, err := tx.ExecContext(ctx, `INSERT INTO supervisor.configuration_draft_field_spans(tenant_id,site_id,draft_id,revision,field_path,span_id,source_id,source_revision,source_access_epoch) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, sc.TenantID, sc.SiteID, draftID, revision, span.path, span.spanID, span.sourceID, span.sourceRevision, span.epoch); err != nil {
			return err
		}
	}
	return nil
}

// createDraftTx is the shared row-writing core for human and agent drafts. Callers keep their
// distinct target checks, citation-resolution order, audit actor and transaction ownership.
func createDraftTx(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, draftID string, kind DraftKind, authorKind DraftAuthorKind, runID, target string, expected int64, payload []byte, digest string, spans []draftSpan) error {
	var runArg, createdByIssuer, createdBySubject, targetArg, expectedArg any
	if authorKind == DraftAuthorAgent {
		runArg = runID
	} else {
		createdByIssuer, createdBySubject = a.Issuer, a.Subject
	}
	if target != "" {
		targetArg, expectedArg = target, expected
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO supervisor.configuration_drafts(tenant_id,site_id,draft_id,kind,author_kind,run_id,created_by_issuer,created_by_subject,target_object_id,target_expected_revision,status,current_revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'draft',1)`, sc.TenantID, sc.SiteID, draftID, string(kind), string(authorKind), runArg, createdByIssuer, createdBySubject, targetArg, expectedArg); err != nil {
		return err
	}
	return insertDraftRevision(ctx, tx, a, sc, draftID, 1, payload, digest, spans, nil, authorKind, runID)
}

func spanHash(in []ConfigurationDraftFieldSpanInputV1) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = s.Path + "\x00" + s.SpanID
	}
	sort.Strings(out)
	return out
}

// targetRevision returns the current revision of a draft's target object, or 0 when absent.
func targetRevision(ctx context.Context, tx *sql.Tx, sc Scope, kind DraftKind, id string) (int64, error) {
	query := `SELECT current_revision FROM supervisor.station_profile_heads WHERE tenant_id=$1 AND site_id=$2 AND workstation_id=$3`
	if kind != DraftKindProfile {
		query = `SELECT current_revision FROM supervisor.` + draftSpec(kind).heads + ` WHERE tenant_id=$1 AND site_id=$2 AND id=$3`
	}
	var current int64
	err := tx.QueryRowContext(ctx, query, sc.TenantID, sc.SiteID, id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return current, err
}

// validateDraftReferences applies the direct route's reference checks to a draft body.
func validateDraftReferences(ctx context.Context, tx *sql.Tx, sc Scope, kind DraftKind, objectID string, body any) error {
	if kind == DraftKindProfile {
		allowed, err := SiteProfileLineAllowed(ctx, tx, sc, body.(StationProfileBodyV1).LineID)
		if err == nil && !allowed {
			err = draftInvalid("body.line_id")
		}
		return err
	}
	return masterDraftError(validateMasterDataReference[any](ctx, tx, sc, objectID, body))
}

// Create stores a human-authored draft at revision 1.
func (s *ConfigurationDraftService) Create(ctx context.Context, a Actor, sc Scope, in ConfigurationDraftCreateInputV1, key string) (ConfigurationDraftV1, error) {
	if !draftKindAllowed(in.Kind) {
		return ConfigurationDraftV1{}, draftInvalid("kind")
	}
	target, expected := "", int64(-1)
	switch {
	case (in.TargetObjectID == nil) != (in.TargetExpectedRevision == nil):
		return ConfigurationDraftV1{}, draftInvalid("target_expected_revision")
	case in.TargetObjectID != nil:
		target, expected = *in.TargetObjectID, *in.TargetExpectedRevision
		if !canonicalID(target) {
			return ConfigurationDraftV1{}, draftInvalid("target_object_id")
		}
		// A new line, item or routing has no caller-chosen ID; only a profile names its
		// workstation before its first revision.
		if expected < 0 || (expected == 0 && in.Kind != DraftKindProfile) {
			return ConfigurationDraftV1{}, draftInvalid("target_expected_revision")
		}
	case in.Kind == DraftKindProfile:
		return ConfigurationDraftV1{}, draftInvalid("target_object_id")
	}
	if !validText(key, 128) {
		return ConfigurationDraftV1{}, ErrInvalidShiftPlan
	}
	body, payload, digest, err := decodeDraftBody(sc, in.Kind, in.Body)
	if err != nil {
		return ConfigurationDraftV1{}, err
	}
	if in.Kind == DraftKindProfile && body.(StationProfileBodyV1).WorkstationID != target {
		return ConfigurationDraftV1{}, draftInvalid("body.workstation_id")
	}
	leaves, err := draftLeaves(payload)
	if err != nil {
		return ConfigurationDraftV1{}, draftInvalid("body")
	}
	if err = checkDraftSpans(in.FieldSpans, leaves); err != nil {
		return ConfigurationDraftV1{}, err
	}
	hash := requestDigest([]any{"configuration_draft.create", in.Kind, target, expected, digest, spanHash(in.FieldSpans)})
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftWrite)
	if err != nil {
		return ConfigurationDraftV1{}, err
	}
	defer tx.Rollback()
	if prior, ok, err := draftReceipt(ctx, tx, a, sc, key, "create", hash); err != nil || ok {
		if err != nil {
			return ConfigurationDraftV1{}, err
		}
		return s.replay(ctx, tx, a, sc, prior)
	}
	draftID := uuid.NewString()
	objectID := draftID
	if target != "" {
		current, err := targetRevision(ctx, tx, sc, in.Kind, target)
		if err != nil {
			return ConfigurationDraftV1{}, err
		}
		if current == 0 && expected > 0 {
			return ConfigurationDraftV1{}, draftInvalid("target_object_id")
		}
		if current != expected {
			return ConfigurationDraftV1{}, ErrTargetRevision
		}
		objectID = target
	}
	if err = validateDraftReferences(ctx, tx, sc, in.Kind, objectID, body); err != nil {
		return ConfigurationDraftV1{}, err
	}
	spans, err := resolveDraftSpans(ctx, tx, sc, in.FieldSpans)
	if err != nil {
		return ConfigurationDraftV1{}, err
	}
	if err = createDraftTx(ctx, tx, a, sc, draftID, in.Kind, DraftAuthorHuman, "", target, expected, payload, digest, spans); err != nil {
		return ConfigurationDraftV1{}, err
	}
	return s.commit(ctx, tx, a, sc, key, "create", hash, draftID, 1, "draft.created")
}

// CreateAgentDraftTx writes one validated, cited agent draft inside the caller's run transaction.
func (s *ConfigurationDraftService) CreateAgentDraftTx(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, runID string, in ConfigurationDraftCreateInputV1) (ConfigurationDraftV1, error) {
	if !canonicalID(runID) || !draftKindAllowed(in.Kind) {
		return ConfigurationDraftV1{}, draftInvalid("kind")
	}
	target := ""
	if in.TargetObjectID != nil {
		target = *in.TargetObjectID
		if !canonicalID(target) {
			return ConfigurationDraftV1{}, draftInvalid("target_object_id")
		}
	}
	body, payload, digest, err := decodeDraftBody(sc, in.Kind, in.Body)
	if err != nil {
		return ConfigurationDraftV1{}, err
	}
	if in.Kind == DraftKindProfile && (target == "" || body.(StationProfileBodyV1).WorkstationID != target) {
		return ConfigurationDraftV1{}, draftInvalid("body.workstation_id")
	}
	leaves, err := draftLeaves(payload)
	if err != nil {
		return ConfigurationDraftV1{}, draftInvalid("body")
	}
	if err = checkDraftSpans(in.FieldSpans, leaves); err != nil {
		return ConfigurationDraftV1{}, err
	}
	cited := make(map[string]bool, len(in.FieldSpans))
	for _, span := range in.FieldSpans {
		cited[span.Path] = true
	}
	for path := range leaves {
		if !cited[path] {
			return ConfigurationDraftV1{}, &ConfigError{CodeFieldUncited, path}
		}
	}
	spans, err := resolveDraftSpans(ctx, tx, sc, in.FieldSpans)
	if err != nil {
		return ConfigurationDraftV1{}, err
	}
	draftID := uuid.NewString()
	objectID := draftID
	expected := int64(0)
	if target != "" {
		current, err := targetRevision(ctx, tx, sc, in.Kind, target)
		if err != nil {
			return ConfigurationDraftV1{}, err
		}
		if current == 0 && in.Kind != DraftKindProfile {
			return ConfigurationDraftV1{}, draftInvalid("target_object_id")
		}
		objectID, expected = target, current
	}
	if err = validateDraftReferences(ctx, tx, sc, in.Kind, objectID, body); err != nil {
		return ConfigurationDraftV1{}, err
	}
	if err = createDraftTx(ctx, tx, a, sc, draftID, in.Kind, DraftAuthorAgent, runID, target, expected, payload, digest, spans); err != nil {
		return ConfigurationDraftV1{}, err
	}
	if err = draftAudit(ctx, tx, Actor{Issuer: "supervisor:agent", Subject: runID}, sc, draftID, 1, "draft.created"); err != nil {
		return ConfigurationDraftV1{}, err
	}
	return loadDraft(ctx, tx, sc, draftID)
}

func (s *ConfigurationDraftService) commit(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, key, op, hash, draftID string, revision int64, event string) (ConfigurationDraftV1, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO supervisor.configuration_draft_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,operation,request_sha256,draft_id,revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, op, hash, draftID, revision); err != nil {
		return ConfigurationDraftV1{}, err
	}
	if err := draftAudit(ctx, tx, a, sc, draftID, revision, event); err != nil {
		return ConfigurationDraftV1{}, err
	}
	out, err := loadDraft(ctx, tx, sc, draftID)
	if err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

type draftHead struct {
	kind                    DraftKind
	status                  DraftStatus
	current                 int64
	target                  sql.NullString
	expected                sql.NullInt64
	currentBody, currentSHA string
}

// lockDraft locks an open draft at the expected revision. A closed draft is ErrDraftNotOpen.
func lockDraft(ctx context.Context, tx *sql.Tx, sc Scope, id string, expected int64) (draftHead, error) {
	var h draftHead
	err := tx.QueryRowContext(ctx, `SELECT d.kind,d.status,d.current_revision,d.target_object_id,d.target_expected_revision,r.body::text,r.body_sha256 FROM supervisor.configuration_drafts d JOIN supervisor.configuration_draft_revisions r ON (r.tenant_id,r.site_id,r.draft_id,r.revision)=(d.tenant_id,d.site_id,d.draft_id,d.current_revision) WHERE d.tenant_id=$1 AND d.site_id=$2 AND d.draft_id=$3 FOR UPDATE OF d`, sc.TenantID, sc.SiteID, id).Scan(&h.kind, &h.status, &h.current, &h.target, &h.expected, &h.currentBody, &h.currentSHA)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return h, ErrNotFound
	case err != nil:
		return h, err
	case h.status != DraftOpen:
		return h, ErrDraftNotOpen
	case h.current != expected:
		return h, ErrRevision
	}
	return h, nil
}

func loadDraftSpans(ctx context.Context, tx *sql.Tx, sc Scope, id string, revision int64) ([]draftSpan, error) {
	rows, err := tx.QueryContext(ctx, `SELECT field_path,span_id,source_id,source_revision,source_access_epoch FROM supervisor.configuration_draft_field_spans WHERE tenant_id=$1 AND site_id=$2 AND draft_id=$3 AND revision=$4 ORDER BY field_path,span_id`, sc.TenantID, sc.SiteID, id, revision)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []draftSpan{}
	for rows.Next() {
		var span draftSpan
		if err = rows.Scan(&span.path, &span.spanID, &span.sourceID, &span.sourceRevision, &span.epoch); err != nil {
			return nil, err
		}
		out = append(out, span)
	}
	return out, rows.Err()
}

func sameDraftSpans(a, b []draftSpan) bool {
	if len(a) != len(b) {
		return false
	}
	set := map[draftSpan]bool{}
	for _, span := range a {
		set[span] = true
	}
	for _, span := range b {
		if !set[span] {
			return false
		}
	}
	return true
}

func draftKind(ctx context.Context, tx *sql.Tx, sc Scope, id string) (DraftKind, error) {
	var kind DraftKind
	err := tx.QueryRowContext(ctx, `SELECT kind FROM supervisor.configuration_drafts WHERE tenant_id=$1 AND site_id=$2 AND draft_id=$3`, sc.TenantID, sc.SiteID, id).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return kind, ErrNotFound
	}
	return kind, err
}

// Revise appends a human edit as a new immutable revision.
func (s *ConfigurationDraftService) Revise(ctx context.Context, a Actor, sc Scope, id string, in ConfigurationDraftReviseInputV1, key string, expected int64) (ConfigurationDraftV1, error) {
	if !canonicalID(id) {
		return ConfigurationDraftV1{}, ErrNotFound
	}
	if !validText(key, 128) || expected < 1 {
		return ConfigurationDraftV1{}, ErrInvalidShiftPlan
	}
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftWrite)
	if err != nil {
		return ConfigurationDraftV1{}, err
	}
	defer tx.Rollback()
	kind, err := draftKind(ctx, tx, sc, id)
	if err != nil {
		return ConfigurationDraftV1{}, err
	}
	body, payload, digest, err := decodeDraftBody(sc, kind, in.Body)
	if err != nil {
		return ConfigurationDraftV1{}, err
	}
	leaves, err := draftLeaves(payload)
	if err != nil {
		return ConfigurationDraftV1{}, draftInvalid("body")
	}
	if err = checkDraftSpans(in.FieldSpans, leaves); err != nil {
		return ConfigurationDraftV1{}, err
	}
	hash := requestDigest([]any{"configuration_draft.revise", id, expected, digest, spanHash(in.FieldSpans)})
	if prior, ok, err := draftReceipt(ctx, tx, a, sc, key, "revise", hash); err != nil || ok {
		if err != nil {
			return ConfigurationDraftV1{}, err
		}
		return s.replay(ctx, tx, a, sc, prior)
	}
	head, err := lockDraft(ctx, tx, sc, id, expected)
	if err != nil {
		return ConfigurationDraftV1{}, err
	}
	objectID := id
	if head.target.Valid {
		objectID = head.target.String
	}
	if kind == DraftKindProfile && body.(StationProfileBodyV1).WorkstationID != objectID {
		return ConfigurationDraftV1{}, draftInvalid("body.workstation_id")
	}
	if err = validateDraftReferences(ctx, tx, sc, kind, objectID, body); err != nil {
		return ConfigurationDraftV1{}, err
	}
	added, err := resolveDraftSpans(ctx, tx, sc, in.FieldSpans)
	if err != nil {
		return ConfigurationDraftV1{}, err
	}
	// A newly cited span replaces any stored citation of the same field and span, with the
	// source's current access epoch. Stored citations of leaves whose value did not change carry
	// forward only while their source is still available: an unavailable citation is dropped, so
	// an edit can recover a draft whose source was revoked. A changed leaf keeps only what this
	// edit cites, so its provenance is this human revision.
	spans := append([]draftSpan{}, added...)
	seen := map[[2]string]bool{}
	for _, span := range added {
		seen[[2]string{span.path, span.spanID}] = true
	}
	previous, err := draftLeaves([]byte(head.currentBody))
	if err != nil {
		return ConfigurationDraftV1{}, errIntegrity
	}
	stored, err := loadDraftSpans(ctx, tx, sc, id, head.current)
	if err != nil {
		return ConfigurationDraftV1{}, err
	}
	sources := []string{}
	for _, span := range stored {
		sources = append(sources, span.sourceID)
	}
	heads, err := evidence.HeadsTx(ctx, tx, evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, sources)
	if err != nil {
		return ConfigurationDraftV1{}, err
	}
	// A kept citation is refreshed to the source's current access epoch: the editor is an
	// owner or supervisor who can read every unrevoked source of the site. Every citation not
	// kept or re-cited is recorded on the new revision with its reason.
	dropped := []ConfigurationDraftDroppedCitationV1{}
	for _, span := range stored {
		if seen[[2]string{span.path, span.spanID}] {
			continue
		}
		h, live := heads[span.sourceID]
		reason := CitationReason("")
		if value, ok := leaves[span.path]; !ok || value != previous[span.path] {
			reason = CitationFieldChanged
		} else if !live || h.Revoked {
			reason = CitationSourceRevoked
		}
		if reason != "" {
			dropped = append(dropped, ConfigurationDraftDroppedCitationV1{Path: span.path, SpanID: span.spanID, Reason: reason})
			continue
		}
		span.epoch = h.AccessEpoch
		spans = append(spans, span)
	}
	// Unchanged means the same body and exactly the same citations at the same epochs.
	if head.currentSHA == digest && sameDraftSpans(stored, spans) {
		return ConfigurationDraftV1{}, ErrDraftUnchanged
	}
	if len(spans) > MaxDraftFieldSpans {
		return ConfigurationDraftV1{}, &ConfigError{CodeDraftLimitExceeded, "field_spans"}
	}
	next := head.current + 1
	if _, err = tx.ExecContext(ctx, `UPDATE supervisor.configuration_drafts SET current_revision=$4,updated_at=clock_timestamp() WHERE tenant_id=$1 AND site_id=$2 AND draft_id=$3`, sc.TenantID, sc.SiteID, id, next); err != nil {
		return ConfigurationDraftV1{}, err
	}
	if err = insertDraftRevision(ctx, tx, a, sc, id, next, payload, digest, spans, dropped, DraftAuthorHuman, ""); err != nil {
		return ConfigurationDraftV1{}, err
	}
	return s.commit(ctx, tx, a, sc, key, "revise", hash, id, next, "draft.revised")
}

// Discard closes an open draft without changing configuration.
func (s *ConfigurationDraftService) Discard(ctx context.Context, a Actor, sc Scope, id, key string, expected int64) (ConfigurationDraftV1, error) {
	if !canonicalID(id) {
		return ConfigurationDraftV1{}, ErrNotFound
	}
	if !validText(key, 128) || expected < 1 {
		return ConfigurationDraftV1{}, ErrInvalidShiftPlan
	}
	hash := requestDigest([]any{"configuration_draft.discard", id, expected})
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftWrite)
	if err != nil {
		return ConfigurationDraftV1{}, err
	}
	defer tx.Rollback()
	if prior, ok, err := draftReceipt(ctx, tx, a, sc, key, "discard", hash); err != nil || ok {
		if err != nil {
			return ConfigurationDraftV1{}, err
		}
		return s.replay(ctx, tx, a, sc, prior)
	}
	if _, err = lockDraft(ctx, tx, sc, id, expected); err != nil {
		return ConfigurationDraftV1{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE supervisor.configuration_drafts SET status='discarded',updated_at=clock_timestamp() WHERE tenant_id=$1 AND site_id=$2 AND draft_id=$3`, sc.TenantID, sc.SiteID, id); err != nil {
		return ConfigurationDraftV1{}, err
	}
	return s.commit(ctx, tx, a, sc, key, "discard", hash, id, expected, "draft.discarded")
}

// Promote applies the current revision through the normal revision path in one transaction.
// Only an authenticated human owner or supervisor who neither created nor edited the draft may
// promote it, and every cited source must still be available.
func (s *ConfigurationDraftService) Promote(ctx context.Context, a Actor, sc Scope, id, key string, expected int64) (ConfigurationDraftV1, error) {
	if !canonicalID(id) {
		return ConfigurationDraftV1{}, ErrNotFound
	}
	if !validText(key, 128) || expected < 1 {
		return ConfigurationDraftV1{}, ErrInvalidShiftPlan
	}
	// Service and model identities are never promoters, even with a session row (rule 05).
	if strings.HasPrefix(a.Issuer, "supervisor:") {
		return ConfigurationDraftV1{}, ErrDenied
	}
	hash := requestDigest([]any{"configuration_draft.promote", id, expected})
	tx, _, _, err := s.plans.begin(ctx, a, sc, configPromote)
	if err != nil {
		return ConfigurationDraftV1{}, err
	}
	defer tx.Rollback()
	if prior, ok, err := draftReceipt(ctx, tx, a, sc, key, "promote", hash); err != nil || ok {
		if err != nil {
			return ConfigurationDraftV1{}, err
		}
		return s.replay(ctx, tx, a, sc, prior)
	}
	head, err := lockDraft(ctx, tx, sc, id, expected)
	if err != nil {
		return ConfigurationDraftV1{}, err
	}
	var editor bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.configuration_drafts d WHERE d.tenant_id=$1 AND d.site_id=$2 AND d.draft_id=$3 AND d.created_by_issuer=$4 AND d.created_by_subject=$5)
		OR EXISTS(SELECT 1 FROM supervisor.configuration_draft_revisions r WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.draft_id=$3 AND r.editor_issuer=$4 AND r.editor_subject=$5)
		OR EXISTS(SELECT 1 FROM supervisor.configuration_drafts d JOIN supervisor.configuration_draft_requests q ON (q.tenant_id,q.site_id,q.run_id)=(d.tenant_id,d.site_id,d.run_id) WHERE d.tenant_id=$1 AND d.site_id=$2 AND d.draft_id=$3 AND q.requested_by_issuer=$4 AND q.requested_by_subject=$5)`, sc.TenantID, sc.SiteID, id, a.Issuer, a.Subject).Scan(&editor); err != nil {
		return ConfigurationDraftV1{}, err
	}
	if editor {
		return ConfigurationDraftV1{}, ErrPromoterIsEditor
	}
	if err = draftSourcesAvailable(ctx, tx, sc, id, head.current); err != nil {
		return ConfigurationDraftV1{}, err
	}
	body, _, digest, err := decodeDraftBody(sc, head.kind, []byte(head.currentBody))
	if err != nil {
		return ConfigurationDraftV1{}, err
	}
	if digest != head.currentSHA {
		return ConfigurationDraftV1{}, errIntegrity
	}
	objectKey := DraftObjectKeyPrefix + id + ":" + strconv.FormatInt(head.current, 10)
	var objectID string
	var objectRevision int64
	if head.kind == DraftKindProfile {
		profile, err := reviseStationProfileTx(ctx, tx, a, sc, head.target.String, body.(StationProfileBodyV1), objectKey, head.expected.Int64)
		if err != nil {
			var c *ConfigError
			if errors.As(err, &c) {
				return ConfigurationDraftV1{}, draftInvalid(joinBodyPath(c.Path))
			}
			return ConfigurationDraftV1{}, promotionError(err)
		}
		// The promoter already used the derived key for another write: fail closed.
		if profile.Replayed {
			return ConfigurationDraftV1{}, ErrConflict
		}
		objectID, objectRevision = profile.WorkstationID, profile.CurrentRevision
	} else {
		var entity MasterDataEntityV1[any]
		if head.target.Valid {
			entity, err = appendMasterDataTx[any](ctx, tx, a, sc, head.target.String, body, objectKey, head.expected.Int64, draftSpec(head.kind))
		} else {
			entity, err = appendMasterDataTx[any](ctx, tx, a, sc, "", body, objectKey, 0, draftSpec(head.kind), id)
		}
		if err != nil {
			return ConfigurationDraftV1{}, promotionError(masterDraftError(err))
		}
		if entity.Replayed {
			return ConfigurationDraftV1{}, ErrConflict
		}
		objectID, objectRevision = entity.ID, entity.CurrentRevision
	}
	if _, err = tx.ExecContext(ctx, `UPDATE supervisor.configuration_drafts SET status='promoted',promoted_by_issuer=$4,promoted_by_subject=$5,promoted_object_id=$6,promoted_object_revision=$7,promoted_at=clock_timestamp(),updated_at=clock_timestamp() WHERE tenant_id=$1 AND site_id=$2 AND draft_id=$3`, sc.TenantID, sc.SiteID, id, a.Issuer, a.Subject, objectID, objectRevision); err != nil {
		return ConfigurationDraftV1{}, err
	}
	return s.commit(ctx, tx, a, sc, key, "promote", hash, id, head.current, "draft.promoted")
}

// promotionError reports a stale target object as ErrTargetRevision; the draft's own If-Match was
// already checked by lockDraft.
func promotionError(err error) error {
	if errors.Is(err, ErrRevision) {
		return ErrTargetRevision
	}
	return err
}

// DraftObjectKeyPrefix prefixes the internal Idempotency-Key a promotion uses on the target
// object's revision path. Direct routes refuse client keys with this prefix.
const DraftObjectKeyPrefix = "cfgdraft:"

// draftSourcesAvailable fails closed when any cited source of the revision is revoked or its
// access epoch changed since the citation was recorded (rule 06).
func draftSourcesAvailable(ctx context.Context, tx *sql.Tx, sc Scope, id string, revision int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT source_id,source_access_epoch FROM supervisor.configuration_draft_field_spans WHERE tenant_id=$1 AND site_id=$2 AND draft_id=$3 AND revision=$4`, sc.TenantID, sc.SiteID, id, revision)
	if err != nil {
		return err
	}
	epochs := map[string][]int64{}
	for rows.Next() {
		var source string
		var epoch int64
		if err = rows.Scan(&source, &epoch); err != nil {
			rows.Close()
			return err
		}
		epochs[source] = append(epochs[source], epoch)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	ids := make([]string, 0, len(epochs))
	for source := range epochs {
		ids = append(ids, source)
	}
	heads, err := evidence.HeadsTx(ctx, tx, evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, ids)
	if err != nil {
		return err
	}
	for source, stored := range epochs {
		head, ok := heads[source]
		for _, epoch := range stored {
			if !ok || head.Revoked || head.AccessEpoch != epoch {
				return ErrDraftSourceUnavailable
			}
		}
	}
	return nil
}

// Get returns a draft with every revision and citation; owners and supervisors only.
func (s *ConfigurationDraftService) Get(ctx context.Context, a Actor, sc Scope, id string) (ConfigurationDraftV1, error) {
	if !canonicalID(id) {
		return ConfigurationDraftV1{}, ErrNotFound
	}
	tx, _, _, err := s.plans.begin(ctx, a, sc, configRead)
	if err != nil {
		return ConfigurationDraftV1{}, err
	}
	defer tx.Rollback()
	out, err := loadDraft(ctx, tx, sc, id)
	if err != nil {
		return out, err
	}
	if err = draftAudit(ctx, tx, a, sc, id, out.CurrentRevision, "draft.read"); err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

// citationReason is the SQL expression for why citation row s is unavailable, or NULL.
const citationReason = `(SELECT CASE WHEN h.source_id IS NULL OR h.revoked OR EXISTS(SELECT 1 FROM supervisor.evidence_revocations v WHERE (v.tenant_id,v.site_id,v.source_id)=(h.tenant_id,h.site_id,h.source_id)) THEN 'SOURCE_REVOKED' WHEN h.access_epoch<>s.source_access_epoch THEN 'ACCESS_EPOCH_CHANGED' END FROM (SELECT 1) one LEFT JOIN supervisor.evidence_heads h ON (h.tenant_id,h.site_id,h.source_id)=(s.tenant_id,s.site_id,s.source_id))`

// citationAvailable is the SQL predicate for a citation row s whose source is still readable.
const citationAvailable = `EXISTS(SELECT 1 FROM supervisor.evidence_heads h WHERE (h.tenant_id,h.site_id,h.source_id)=(s.tenant_id,s.site_id,s.source_id) AND NOT h.revoked AND h.access_epoch=s.source_access_epoch AND NOT EXISTS(SELECT 1 FROM supervisor.evidence_revocations v WHERE (v.tenant_id,v.site_id,v.source_id)=(h.tenant_id,h.site_id,h.source_id)))`

// List pages drafts by ID, optionally filtered by status and kind; owners and supervisors only.
func (s *ConfigurationDraftService) List(ctx context.Context, a Actor, sc Scope, status DraftStatus, kind DraftKind, limit int, cursor string) (ConfigurationDraftPageV1, error) {
	out := ConfigurationDraftPageV1{Drafts: []ConfigurationDraftSummaryV1{}}
	statusOK := status == "" || status == DraftOpen || status == DraftPromoted || status == DraftDiscarded
	if limit < 1 || limit > configurationDraftPageSize || (cursor != "" && !canonicalID(cursor)) || !statusOK || (kind != "" && !draftKindAllowed(kind)) {
		return out, ErrInvalidShiftPlan
	}
	if cursor == "" {
		cursor = uuid.Nil.String()
	}
	tx, _, _, err := s.plans.begin(ctx, a, sc, configRead)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT d.draft_id,d.kind,d.author_kind,COALESCE(d.target_object_id::text,''),d.target_expected_revision,d.status,d.current_revision,d.updated_at,
		(SELECT count(*) FROM supervisor.configuration_draft_field_spans s WHERE (s.tenant_id,s.site_id,s.draft_id,s.revision)=(d.tenant_id,d.site_id,d.draft_id,d.current_revision)),
		(SELECT count(*) FROM supervisor.configuration_draft_field_spans s WHERE (s.tenant_id,s.site_id,s.draft_id,s.revision)=(d.tenant_id,d.site_id,d.draft_id,d.current_revision) AND NOT `+citationAvailable+`)
		FROM supervisor.configuration_drafts d WHERE d.tenant_id=$1 AND d.site_id=$2 AND d.draft_id>$3 AND ($4='' OR d.status=$4) AND ($5='' OR d.kind=$5) ORDER BY d.draft_id LIMIT $6`, sc.TenantID, sc.SiteID, cursor, string(status), string(kind), limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var d ConfigurationDraftSummaryV1
		var expected sql.NullInt64
		if err = rows.Scan(&d.DraftID, &d.Kind, &d.AuthorKind, &d.TargetObjectID, &expected, &d.Status, &d.CurrentRevision, &d.UpdatedAt, &d.CitationCount, &d.UnavailableCitationCount); err != nil {
			return out, err
		}
		if expected.Valid {
			d.TargetExpectedRevision = &expected.Int64
		}
		d.UpdatedAt = d.UpdatedAt.UTC()
		out.Drafts = append(out.Drafts, d)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	rows.Close()
	if len(out.Drafts) > limit {
		out.Drafts = out.Drafts[:limit]
		out.NextCursor = out.Drafts[limit-1].DraftID
	}
	if err = draftAudit(ctx, tx, a, sc, "", 0, "draft.listed"); err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

func loadDraft(ctx context.Context, tx *sql.Tx, sc Scope, id string) (ConfigurationDraftV1, error) {
	out := ConfigurationDraftV1{DraftID: id, Revisions: []ConfigurationDraftRevisionV1{}}
	var run, createdIssuer, createdSubject, target, promotedIssuer, promotedSubject, promotedObject sql.NullString
	var expected, promotedRevision sql.NullInt64
	var promotedAt sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT kind,author_kind,run_id::text,created_by_issuer,created_by_subject,target_object_id::text,target_expected_revision,status,current_revision,promoted_by_issuer,promoted_by_subject,promoted_object_id::text,promoted_object_revision,promoted_at,created_at,updated_at FROM supervisor.configuration_drafts WHERE tenant_id=$1 AND site_id=$2 AND draft_id=$3`, sc.TenantID, sc.SiteID, id).Scan(
		&out.Kind, &out.AuthorKind, &run, &createdIssuer, &createdSubject, &target, &expected, &out.Status, &out.CurrentRevision, &promotedIssuer, &promotedSubject, &promotedObject, &promotedRevision, &promotedAt, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	out.RunID, out.TargetObjectID = run.String, target.String
	out.CreatedAt, out.UpdatedAt = out.CreatedAt.UTC(), out.UpdatedAt.UTC()
	if createdIssuer.Valid {
		out.CreatedBy = &ActorRefV1{Issuer: createdIssuer.String, Subject: createdSubject.String}
	}
	if expected.Valid {
		out.TargetExpectedRevision = &expected.Int64
	}
	if promotedAt.Valid {
		out.Promotion = &ConfigurationDraftPromotionV1{ObjectID: promotedObject.String, ObjectRevision: promotedRevision.Int64, PromotedBy: ActorRefV1{Issuer: promotedIssuer.String, Subject: promotedSubject.String}, PromotedAt: promotedAt.Time.UTC()}
	}
	rows, err := tx.QueryContext(ctx, `SELECT revision,body::text,body_sha256,edited_by_kind,editor_issuer,editor_subject,run_id::text,created_at,dropped_citations::text FROM supervisor.configuration_draft_revisions WHERE tenant_id=$1 AND site_id=$2 AND draft_id=$3 ORDER BY revision`, sc.TenantID, sc.SiteID, id)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		r := ConfigurationDraftRevisionV1{Citations: []ConfigurationDraftCitationV1{}}
		var body string
		var editorIssuer, editorSubject, revisionRun sql.NullString
		var dropped string
		if err = rows.Scan(&r.Revision, &body, &r.BodySHA256, &r.EditedByKind, &editorIssuer, &editorSubject, &revisionRun, &r.CreatedAt, &dropped); err != nil {
			rows.Close()
			return out, err
		}
		if err = json.Unmarshal([]byte(dropped), &r.DroppedCitations); err != nil || r.DroppedCitations == nil {
			rows.Close()
			return out, errIntegrity
		}
		r.Body, r.RunID, r.CreatedAt = json.RawMessage(body), revisionRun.String, r.CreatedAt.UTC()
		if editorIssuer.Valid {
			r.Editor = &ActorRefV1{Issuer: editorIssuer.String, Subject: editorSubject.String}
		}
		out.Revisions = append(out.Revisions, r)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	if int64(len(out.Revisions)) != out.CurrentRevision {
		return out, errIntegrity
	}
	spans, err := tx.QueryContext(ctx, `SELECT s.revision,s.field_path,s.span_id,s.source_id,s.source_revision,COALESCE(`+citationReason+`,'') FROM supervisor.configuration_draft_field_spans s WHERE s.tenant_id=$1 AND s.site_id=$2 AND s.draft_id=$3 ORDER BY s.revision,s.field_path,s.span_id`, sc.TenantID, sc.SiteID, id)
	if err != nil {
		return out, err
	}
	defer spans.Close()
	for spans.Next() {
		var revision int64
		var c ConfigurationDraftCitationV1
		if err = spans.Scan(&revision, &c.Path, &c.SpanID, &c.SourceID, &c.SourceRevision, &c.UnavailableReason); err != nil {
			return out, err
		}
		c.Available = c.UnavailableReason == ""
		if revision < 1 || revision > int64(len(out.Revisions)) {
			return out, errIntegrity
		}
		out.Revisions[revision-1].Citations = append(out.Revisions[revision-1].Citations, c)
	}
	return out, spans.Err()
}
