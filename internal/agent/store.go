package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/modelhosting"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
)

// ServiceActor is the server identity the agent runtime acts and audits as. It holds no
// session, membership or approval capability (rule 05).
var ServiceActor = struct{ Issuer, Subject string }{Issuer: "supervisor:agent", Subject: "investigation-runtime"}

// Run states (agent_runs.state).
const (
	RunRunning   = "running"
	RunCompleted = "completed"
	RunDegraded  = "degraded"
	RunCanceled  = "canceled"
	RunFailed    = "failed"
)

// Captured scope kinds (agent_run_refs.kind).
const (
	RefContext     = "context"
	RefMeasurement = "measurement"
	RefSnapshot    = "snapshot"
	RefEvidence    = "evidence"
	// RefMachineState is one machine_state record revision of an approved source (WIS-276). A
	// recorded stoppage is machine-fault evidence: a reason to inspect, never a cause.
	RefMachineState = "machine_state"
)

var (
	ErrRunNotFound = errors.New("agent run not found")
	ErrRunStore    = errors.New("invalid agent run record")
)

// RunScope is the server-verified tenant and site every agent row is bound to.
type RunScope struct{ TenantID, SiteID string }

// Run is one agent_runs row. Budget counters start from Limits.
type Run struct {
	ID                  string
	RequestKind         string
	CaseID              string // case runs only
	CaseRevision        int64  // case runs only
	ConversationID      string // conversation runs only
	RequestID           string // conversation runs only
	InterviewGeneration int64  // only set for explicitly captured interview turns
	State               string
	TraceID             string
	ProviderName        string
	ProviderActual      bool
	Model               string
	ModelRevision       string
	// ProviderRegion and PriceVersion are set only on a run pinned to a hosted endpoint (WIS-623).
	ProviderRegion      string
	PriceVersion        string
	PromptVersion       string
	ToolVersion         string
	PolicyRevision      int64
	LimitProfileID      string
	LimitProfileVersion int
	LocalContextTokens  int64 // server probe input at creation only; the selected profile is persisted

	InvocationsLeft  int
	ToolCallsLeft    int
	RequestsLeft     int
	TokensLeft       int64
	CostLeftMicroUSD int64

	OutcomeStatus        string
	OutcomeReasons       []string
	ArtifactID           string
	ProposalID           string
	RevokedAfterDispatch bool
	RevocationReason     string
	StartedAt            time.Time
	EndedAt              *time.Time
}

// Generation is the case revision or interview session revision that fences this run.
func (r Run) Generation() int64 {
	if r.RequestKind == RequestCase {
		return r.CaseRevision
	}
	return r.InterviewGeneration
}

// RunRef is one captured scope reference.
type RunRef struct {
	Kind        string
	RefID       string
	Revision    int64
	SHA256      string
	AccessEpoch int64 // 0 when the kind has no access epoch
}

// Step is one finished specialist or orchestrator span.
type Step struct {
	Seq            int
	SpanID         string
	ParentSpanID   string
	Role           string
	Hop            int
	EnvelopeSHA256 string
	Status         string // completed | failed | canceled | rejected
	ResultStatus   string
	ReasonCode     string
	StartedAt      time.Time
	EndedAt        time.Time
}

// ToolCallRecord is one finished tool call. Results are stored as a hash and byte count only.
type ToolCallRecord struct {
	Seq          int
	SpanID       string
	ParentSpanID string
	Tool         string
	ToolVersion  string
	ArgsSHA256   string
	RefIDs       []string
	RefRevisions []int64
	ResultSHA256 string
	ResultBytes  int
	Status       string // ok | rejected | unavailable | canceled | failed
	Code         string
	StartedAt    time.Time
	EndedAt      time.Time
}

// Artifact is validated model output stored for a run. Body must strict-decode as Schema; the
// store computes SHA256 itself (a caller-supplied SHA256 must match it, or the write is refused).
type Artifact struct {
	ID     string
	Schema string
	Body   []byte
	SHA256 string
}

// ArtifactSchema is the one registered artifact body in slice 1: a judged RecoveryProposalV1.
const ArtifactSchema = "recovery_proposal_v1"

// artifactSchemas registers the strict decoder of every storable artifact body. An unknown
// schema, or a body its decoder rejects, is never stored.
var artifactSchemas = map[string]func([]byte) (any, error){
	ArtifactSchema:          func(b []byte) (any, error) { return recovery.DecodeProposal(b) },
	InterviewArtifactSchema: decodeInterviewArtifact,
	DraftArtifactSchema:     decodeConfigurationDraftArtifact,
	"chat_answer_v1":        decodeChatArtifact,
	ChatArtifactSchema:      decodeChatArtifactV2,
}

var (
	hex64     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reasonPat = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
)

func validRunScope(sc RunScope) bool { return isUUID(sc.TenantID) && isUUID(sc.SiteID) }

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Store persists agent runs. Every statement runs in a transaction that carries the
// transaction-local tenant context, so forced RLS bounds every read and write.
type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// InTx runs fn in one transaction scoped to sc and commits when fn returns nil. The tenant
// context is set with is_local=true, so a pooled connection never carries it further.
func (s *Store) InTx(ctx context.Context, sc RunScope, fn func(*sql.Tx) error) error {
	if !validRunScope(sc) {
		return ErrRunStore
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true),set_config('lock_timeout','5s',true)`, sc.TenantID); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateRunTx inserts a running run whose counters come from limits.
func CreateRunTx(ctx context.Context, tx *sql.Tx, sc RunScope, r Run, limits Limits) error {
	if !validRunScope(sc) || !isUUID(r.ID) || !validTraceID(r.TraceID) || r.PolicyRevision <= 0 || limits.Validate() != nil {
		return ErrRunStore
	}
	switch r.RequestKind {
	case RequestCase:
		if !isUUID(r.CaseID) || r.CaseRevision <= 0 || r.ConversationID != "" || r.RequestID != "" {
			return ErrRunStore
		}
	case RequestConversation:
		if !isUUID(r.ConversationID) || !isUUID(r.RequestID) || r.CaseID != "" || r.CaseRevision != 0 {
			return ErrRunStore
		}
	case RequestExtraction:
		if !isUUID(r.ConversationID) || r.RequestID != r.ConversationID || r.CaseID != "" || r.CaseRevision != 0 || r.ProviderName != ProviderLocal && r.ProviderName != ProviderFixture {
			return ErrRunStore
		}
	default:
		return ErrRunStore
	}
	var caseRevision any
	if r.CaseRevision > 0 {
		caseRevision = r.CaseRevision
	}
	// WIS-623: a run of a site whose owner has switched hosted inference on is pinned, here and
	// once, to this process's hosted route. Every call re-checks the switch (callRole); the run
	// never moves to another provider.
	// An extraction run reads an image, which cannot be pseudonymised: it is never pinned to a
	// hosted route, whatever the site's switch says (WIS-625). It stays on the local vision model.
	if route := CurrentHostedRoute(); route != nil && r.RequestKind != RequestExtraction {
		on, err := modelhosting.EnabledTx(ctx, tx, sc.TenantID, sc.SiteID)
		if err != nil {
			return err
		}
		if on {
			r.ProviderName, r.ProviderActual, r.Model, r.ModelRevision = route.Name, true, route.Model, route.ModelRevision
			r.ProviderRegion, r.PriceVersion = route.Region, route.PriceVersion
			// A zero cost ceiling means "local only" (InterviewLimits: onboarding interviews,
			// configuration drafts and chat). On the paid route the run instead gets the most its
			// token budget can cost at this route's price, plus one micro-USD of rounding per
			// request: bounded, and never the limit that stops a call the token budget allows.
			if limits.CostMicroUSD == 0 && route.Price != nil {
				perToken := max(route.Price.InputMicroUSDPerMillion, route.Price.OutputMicroUSDPerMillion)
				ceiling, err := CallCost(Pricing{Price: &TokenPrice{perToken, perToken}}, limits.Tokens, 0)
				if err != nil {
					return err
				}
				limits.CostMicroUSD = ceiling + int64(limits.Requests)
			}
		}
	}
	r.LimitProfileID, r.LimitProfileVersion = defaultLimitProfile, 1
	if localQwenProfileEligible(r) && r.LocalContextTokens >= 12000+int64(limits.MaxOutputTokens) {
		r.LimitProfileID = localQwenLimitProfile
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO supervisor.agent_runs(tenant_id,site_id,id,request_kind,case_id,case_revision,conversation_id,request_id,trace_id,provider_name,provider_actual,model,model_revision,prompt_version,tool_version,policy_revision,invocations_left,tool_calls_left,requests_left,tokens_left,cost_left_microusd,provider_region,price_version,limit_profile_id,limit_profile_version)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)`,
		sc.TenantID, sc.SiteID, r.ID, r.RequestKind, nullString(r.CaseID), caseRevision, nullString(r.ConversationID), nullString(r.RequestID),
		r.TraceID, r.ProviderName, r.ProviderActual, r.Model, r.ModelRevision, r.PromptVersion, r.ToolVersion, r.PolicyRevision,
		limits.Invocations, limits.ToolCalls, limits.Requests, limits.Tokens, limits.CostMicroUSD, nullString(r.ProviderRegion), nullString(r.PriceVersion), r.LimitProfileID, r.LimitProfileVersion)
	return err
}

// GetRunTx reads one run; a run in another tenant or site is ErrRunNotFound.
func GetRunTx(ctx context.Context, tx *sql.Tx, sc RunScope, id string) (Run, error) {
	var r Run
	if !validRunScope(sc) || !isUUID(id) {
		return r, ErrRunNotFound
	}
	var caseID, conversationID, requestID, outcome, artifact, proposal, revocation, region, priceVersion sql.NullString
	var caseRevision sql.NullInt64
	var reasons []string
	var ended sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT id,request_kind,case_id,case_revision,conversation_id,request_id,state,trace_id,provider_name,provider_actual,model,model_revision,prompt_version,tool_version,policy_revision,
 invocations_left,tool_calls_left,requests_left,tokens_left,cost_left_microusd,outcome_status,outcome_reasons,artifact_id,proposal_id,revoked_after_dispatch,revocation_reason,started_at,ended_at,provider_region,price_version,limit_profile_id,limit_profile_version
 FROM supervisor.agent_runs WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, id).Scan(
		&r.ID, &r.RequestKind, &caseID, &caseRevision, &conversationID, &requestID, &r.State, &r.TraceID, &r.ProviderName, &r.ProviderActual, &r.Model, &r.ModelRevision, &r.PromptVersion, &r.ToolVersion, &r.PolicyRevision,
		&r.InvocationsLeft, &r.ToolCallsLeft, &r.RequestsLeft, &r.TokensLeft, &r.CostLeftMicroUSD, &outcome, &reasons, &artifact, &proposal, &r.RevokedAfterDispatch, &revocation, &r.StartedAt, &ended, &region, &priceVersion, &r.LimitProfileID, &r.LimitProfileVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrRunNotFound
	}
	if err != nil {
		return Run{}, err
	}
	r.CaseID, r.CaseRevision, r.ConversationID, r.RequestID = caseID.String, caseRevision.Int64, conversationID.String, requestID.String
	r.OutcomeStatus, r.OutcomeReasons, r.ArtifactID, r.ProposalID, r.RevocationReason = outcome.String, reasons, artifact.String, proposal.String, revocation.String
	r.ProviderRegion, r.PriceVersion = region.String, priceVersion.String
	if r.RequestKind == RequestConversation {
		if err = tx.QueryRowContext(ctx, `SELECT session_revision FROM supervisor.interview_agent_turns WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3`, sc.TenantID, sc.SiteID, id).Scan(&r.InterviewGeneration); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Run{}, err
		}
		// A WIS-517 chat answer run is fenced by its reply message revision (1 while pending).
		if r.InterviewGeneration == 0 {
			if err = tx.QueryRowContext(ctx, `SELECT m.revision FROM supervisor.chat_answer_turns t JOIN supervisor.chat_messages m ON (m.tenant_id,m.site_id,m.message_id)=(t.tenant_id,t.site_id,t.reply_message_id) WHERE t.tenant_id=$1 AND t.site_id=$2 AND t.run_id=$3`, sc.TenantID, sc.SiteID, id).Scan(&r.InterviewGeneration); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return Run{}, err
			}
		}
	}
	if ended.Valid {
		t := ended.Time.UTC()
		r.EndedAt = &t
	}
	r.StartedAt = r.StartedAt.UTC()
	return r, nil
}

// AddRefsTx records the captured scope of a run.
func AddRefsTx(ctx context.Context, tx *sql.Tx, sc RunScope, runID string, refs []RunRef) error {
	if !validRunScope(sc) || !isUUID(runID) {
		return ErrRunStore
	}
	for _, ref := range refs {
		switch ref.Kind {
		case RefContext, RefMeasurement, RefSnapshot, RefEvidence, RefMachineState:
		default:
			return ErrRunStore
		}
		if ref.RefID == "" || len(ref.RefID) > 255 || ref.Revision <= 0 || !hex64.MatchString(ref.SHA256) || ref.AccessEpoch < 0 {
			return ErrRunStore
		}
		var epoch any
		if ref.AccessEpoch > 0 {
			epoch = ref.AccessEpoch
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO supervisor.agent_run_refs(tenant_id,site_id,run_id,kind,ref_id,revision,sha256,access_epoch) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
			sc.TenantID, sc.SiteID, runID, ref.Kind, ref.RefID, ref.Revision, ref.SHA256, epoch); err != nil {
			return err
		}
	}
	return nil
}

// RefsTx lists the captured scope of a run in a stable order.
func RefsTx(ctx context.Context, tx *sql.Tx, sc RunScope, runID string) ([]RunRef, error) {
	if !validRunScope(sc) || !isUUID(runID) {
		return nil, ErrRunStore
	}
	rows, err := tx.QueryContext(ctx, `SELECT kind,ref_id,revision,sha256,COALESCE(access_epoch,0) FROM supervisor.agent_run_refs WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3 ORDER BY kind,ref_id`, sc.TenantID, sc.SiteID, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RunRef{}
	for rows.Next() {
		var ref RunRef
		if err = rows.Scan(&ref.Kind, &ref.RefID, &ref.Revision, &ref.SHA256, &ref.AccessEpoch); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// MaxRunPassages bounds the document passages one run may be given (migration 071).
const MaxRunPassages = 8

// RunPassage is one selected passage (evidence span) of a captured source revision. When a run
// has passages of a source, evidence.get returns only those passages of it.
type RunPassage struct {
	SpanID, SourceID string
	SourceRevision   int64
}

// AddPassagesTx records the passages a run may read, in order. Each passage's source must already
// be in the run's captured evidence scope at the same revision, so a passage can never widen what
// the run may read; the database also requires the span to belong to that source revision.
func AddPassagesTx(ctx context.Context, tx *sql.Tx, sc RunScope, runID string, passages []RunPassage) error {
	if !validRunScope(sc) || !isUUID(runID) || len(passages) > MaxRunPassages {
		return ErrRunStore
	}
	for i, p := range passages {
		if !isUUID(p.SpanID) || !isUUID(p.SourceID) || p.SourceRevision <= 0 {
			return ErrRunStore
		}
		var captured bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.agent_run_refs WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3 AND kind='evidence' AND ref_id=$4 AND revision=$5)`,
			sc.TenantID, sc.SiteID, runID, p.SourceID, p.SourceRevision).Scan(&captured); err != nil {
			return err
		}
		if !captured {
			return ErrRunStore
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO supervisor.agent_run_spans(tenant_id,site_id,run_id,ordinal,span_id,source_id,source_revision) VALUES($1,$2,$3,$4,$5,$6,$7)`,
			sc.TenantID, sc.SiteID, runID, i+1, p.SpanID, p.SourceID, p.SourceRevision); err != nil {
			return err
		}
	}
	return nil
}

// AddStepTx records one finished span.
func AddStepTx(ctx context.Context, tx *sql.Tx, sc RunScope, runID string, s Step) error {
	if !validRunScope(sc) || !isUUID(runID) || s.Seq <= 0 || !validSpanID(s.SpanID) || (s.ParentSpanID != "" && !validSpanID(s.ParentSpanID)) ||
		!validRole(s.Role) || !hex64.MatchString(s.EnvelopeSHA256) || (s.ReasonCode != "" && !reasonPat.MatchString(s.ReasonCode)) {
		return ErrRunStore
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO supervisor.agent_steps(tenant_id,site_id,run_id,seq,span_id,parent_span_id,role,hop,envelope_sha256,status,result_status,reason_code,started_at,ended_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		sc.TenantID, sc.SiteID, runID, s.Seq, s.SpanID, nullString(s.ParentSpanID), s.Role, s.Hop, s.EnvelopeSHA256, s.Status, nullString(s.ResultStatus), nullString(s.ReasonCode), s.StartedAt.UTC(), s.EndedAt.UTC())
	return err
}

// AddToolCallTx records one finished tool call.
func AddToolCallTx(ctx context.Context, tx *sql.Tx, sc RunScope, runID string, c ToolCallRecord) error {
	if !validRunScope(sc) || !isUUID(runID) || c.Seq <= 0 || !validSpanID(c.SpanID) || (c.ParentSpanID != "" && !validSpanID(c.ParentSpanID)) ||
		!hex64.MatchString(c.ArgsSHA256) || (c.ResultSHA256 != "" && !hex64.MatchString(c.ResultSHA256)) || len(c.RefIDs) != len(c.RefRevisions) ||
		(c.Code != "" && !reasonPat.MatchString(c.Code)) {
		return ErrRunStore
	}
	refIDs, refRevisions := c.RefIDs, c.RefRevisions
	if refIDs == nil {
		refIDs, refRevisions = []string{}, []int64{}
	}
	var resultBytes any
	if c.ResultSHA256 != "" {
		resultBytes = c.ResultBytes
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO supervisor.agent_tool_calls(tenant_id,site_id,run_id,seq,span_id,parent_span_id,tool,tool_version,args_sha256,ref_ids,ref_revisions,result_sha256,result_bytes,status,code,started_at,ended_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		sc.TenantID, sc.SiteID, runID, c.Seq, c.SpanID, nullString(c.ParentSpanID), c.Tool, c.ToolVersion, c.ArgsSHA256, refIDs, refRevisions,
		nullString(c.ResultSHA256), resultBytes, c.Status, nullString(c.Code), c.StartedAt.UTC(), c.EndedAt.UTC())
	return err
}

// AddArtifactTx strict-decodes the body against its registered schema, stores its schema-ordered
// serialization with the digest canonical.Envelope("agent_artifact", 1, tenant, site, body)
// computed here (design §6), binds it to the run and returns the digest.
func AddArtifactTx(ctx context.Context, tx *sql.Tx, sc RunScope, runID string, a Artifact) (string, error) {
	decode, ok := artifactSchemas[a.Schema]
	if !validRunScope(sc) || !isUUID(runID) || !isUUID(a.ID) || !ok || len(a.Body) == 0 || len(a.Body) > MaxContractBytes {
		return "", ErrRunStore
	}
	value, err := decode(a.Body)
	if err != nil {
		return "", ErrRunStore
	}
	_, digest, err := canonical.Envelope("agent_artifact", 1, sc.TenantID, sc.SiteID, value)
	if err != nil || (a.SHA256 != "" && a.SHA256 != digest) {
		return "", ErrRunStore
	}
	body, err := json.Marshal(value)
	if err != nil {
		return "", ErrRunStore
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.agent_artifacts(tenant_id,site_id,id,run_id,schema,body,sha256) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7)`,
		sc.TenantID, sc.SiteID, a.ID, runID, a.Schema, string(body), digest); err != nil {
		return "", err
	}
	res, err := tx.ExecContext(ctx, `UPDATE supervisor.agent_runs SET artifact_id=$4 WHERE tenant_id=$1 AND site_id=$2 AND id=$3 AND state='running'`, sc.TenantID, sc.SiteID, runID, a.ID)
	return digest, oneRow(res, err)
}

// LoadArtifactTx returns a stored artifact only after its schema and canonical digest verify.
func LoadArtifactTx(ctx context.Context, tx *sql.Tx, sc RunScope, runID, id string) (Artifact, error) {
	var a Artifact
	if !validRunScope(sc) || !isUUID(runID) || !isUUID(id) {
		return a, ErrRunStore
	}
	var body string
	err := tx.QueryRowContext(ctx, `SELECT schema,body::text,sha256 FROM supervisor.agent_artifacts WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3 AND id=$4`, sc.TenantID, sc.SiteID, runID, id).Scan(&a.Schema, &body, &a.SHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrRunNotFound
	}
	if err != nil {
		return a, err
	}
	decode, ok := artifactSchemas[a.Schema]
	if !ok {
		return Artifact{}, ErrRunStore
	}
	value, err := decode([]byte(body))
	if err != nil {
		return Artifact{}, ErrRunStore
	}
	_, digest, err := canonical.Envelope("agent_artifact", 1, sc.TenantID, sc.SiteID, value)
	if err != nil || digest != a.SHA256 {
		return Artifact{}, ErrRunStore
	}
	a.ID, a.Body = id, []byte(body)
	return a, nil
}

// LinkProposalTx records the proposal created from this run's terminal proposed artifact.
func LinkProposalTx(ctx context.Context, tx *sql.Tx, sc RunScope, runID, proposalID string) error {
	if !validRunScope(sc) || !isUUID(runID) || !isUUID(proposalID) {
		return ErrRunStore
	}
	res, err := tx.ExecContext(ctx, `UPDATE supervisor.agent_runs r SET proposal_id=$4 WHERE tenant_id=$1 AND site_id=$2 AND id=$3
 AND state='completed' AND outcome_status='proposed' AND artifact_id IS NOT NULL AND (proposal_id IS NULL OR proposal_id=$4)
 AND EXISTS(SELECT 1 FROM supervisor.proposal_revisions p WHERE p.tenant_id=r.tenant_id AND p.site_id=r.site_id AND p.proposal_id=$4
 AND p.case_id=r.case_id AND p.case_revision=r.case_revision AND p.generated_by_run_id=r.id)`, sc.TenantID, sc.SiteID, runID, proposalID)
	return oneRow(res, err)
}

// Finish moves a running run to a terminal state with its outcome. A run that is already
// terminal is not changed (ErrRunNotFound).
type Finish struct {
	State                string
	OutcomeStatus        string
	OutcomeReasons       []string
	ProposalID           string
	RevokedAfterDispatch bool
	RevocationReason     string
}

func FinishRunTx(ctx context.Context, tx *sql.Tx, sc RunScope, runID string, f Finish) error {
	switch f.State {
	case RunCompleted, RunDegraded, RunCanceled, RunFailed:
	default:
		return ErrRunStore
	}
	if !validRunScope(sc) || !isUUID(runID) || (f.ProposalID != "" && !isUUID(f.ProposalID)) || (f.RevocationReason != "" && !reasonPat.MatchString(f.RevocationReason)) {
		return ErrRunStore
	}
	reasons := f.OutcomeReasons
	if reasons == nil {
		reasons = []string{}
	}
	for _, code := range reasons {
		if !reasonPat.MatchString(code) {
			return ErrRunStore
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE supervisor.agent_runs SET state=$4,outcome_status=$5,outcome_reasons=$6,proposal_id=$7,revoked_after_dispatch=$8,revocation_reason=$9,ended_at=clock_timestamp()
 WHERE tenant_id=$1 AND site_id=$2 AND id=$3 AND state='running'`, sc.TenantID, sc.SiteID, runID, f.State, nullString(f.OutcomeStatus), reasons, nullString(f.ProposalID), f.RevokedAfterDispatch, nullString(f.RevocationReason))
	return oneRow(res, err)
}

func oneRow(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrRunNotFound
	}
	return nil
}
