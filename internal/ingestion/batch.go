// Package ingestion durably accepts tenant-scoped operational event batches.
package ingestion

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/operational"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/production"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
	"github.com/google/uuid"
)

var (
	ErrInvalid                = errors.New("invalid operational event batch")
	ErrNotFound               = errors.New("operational source unavailable")
	ErrDenied                 = errors.New("operational ingestion permission denied")
	ErrConflict               = errors.New("operational batch idempotency conflict")
	ErrSequence               = errors.New("operational source sequence conflict")
	ErrRevision               = errors.New("operational source revision changed")
	ErrSourceRevisionConflict = errors.New("operational event revision conflicts with prior content")
	// ErrImportRunning means a running import owns this source stream; live batches wait for it.
	ErrImportRunning = errors.New("an import is running for this operational source")
	// ErrImportCancelled means the import was cancelled while it ran: its operational source was
	// revoked, or it was abandoned after importLease without finishing (WIS-699).
	ErrImportCancelled = errors.New("the import was cancelled: its operational source was revoked or the import was abandoned")
)

const (
	MaxEvents = 100
	MaxBody   = limits.RequestBodyBytes
	MaxEvent  = limits.EventBytes
)

type Actor struct{ Issuer, Subject, SessionHash string }
type Scope struct{ TenantID, SiteID string }
type Event struct {
	ExternalKey      string          `json:"external_key"`
	Revision         int64           `json:"revision"`
	Kind             string          `json:"kind"`
	SourceSequence   int64           `json:"source_sequence"`
	EventAt          time.Time       `json:"event_at"`
	Payload          json.RawMessage `json:"payload"`
	AssetExternalKey string          `json:"asset_external_key,omitempty"`
	OrderExternalKey string          `json:"order_external_key,omitempty"`
}
type Batch struct {
	SchemaVersion   int     `json:"schema_version"`
	SourceID        string  `json:"source_id"`
	SourceRevision  int64   `json:"source_revision"`
	MappingRevision int64   `json:"mapping_revision"`
	Events          []Event `json:"events"`
}
type Result struct {
	BatchID         string    `json:"batch_id"`
	SourceID        string    `json:"source_id"`
	SourceRevision  int64     `json:"source_revision"`
	MappingRevision int64     `json:"mapping_revision"`
	FirstSequence   int64     `json:"first_sequence"`
	LastSequence    int64     `json:"last_sequence"`
	EventCount      int       `json:"event_count"`
	Replayed        bool      `json:"replayed"`
	AcceptedAt      time.Time `json:"accepted_at"`
}
type Service struct {
	db       *sql.DB
	sessions session.Policy
}

func New(db *sql.DB) *Service { return &Service{db: db, sessions: session.Default()} }

// WithSessionPolicy returns a copy of the service that enforces p (WIS-290: one configurable
// session policy for every service). New uses session.Default.
func (s *Service) WithSessionPolicy(p session.Policy) *Service {
	c := *s
	c.sessions = p.OrDefault()
	return &c
}

func validID(s string) bool {
	id, e := uuid.Parse(s)
	return e == nil && id != uuid.Nil && id.String() == s
}
func validText(s string, n int) bool {
	return len(s) > 0 && len(s) <= n && strings.TrimSpace(s) == s && !strings.ContainsRune(s, 0)
}
func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// eventDigest identifies event content independent of transport position, so a
// retransmit at a new source_sequence is a DUPLICATE_EVENT, not a content conflict.
func eventDigest(e Event) string {
	e.SourceSequence = 0
	b, _ := json.Marshal(e)
	return digest(b)
}
func canonicalBatch(b Batch) (Batch, []byte, error) {
	if b.SchemaVersion != 1 || !validID(b.SourceID) || b.SourceRevision < 1 || b.MappingRevision < 1 || len(b.Events) == 0 || len(b.Events) > MaxEvents {
		return Batch{}, nil, ErrInvalid
	}
	total := 0
	for i := range b.Events {
		e := &b.Events[i]
		if !validText(e.ExternalKey, 512) || e.Revision < 1 || !validText(e.Kind, 64) || e.SourceSequence < 1 || e.EventAt.IsZero() || (e.AssetExternalKey != "" && !validText(e.AssetExternalKey, 512)) || (e.OrderExternalKey != "" && !validText(e.OrderExternalKey, 512)) {
			return Batch{}, nil, ErrInvalid
		}
		switch e.Kind {
		case "production_plan", "production_delta", "machine_state", "operator_report", "heartbeat", "watermark":
		default:
			return Batch{}, nil, ErrInvalid
		}
		if i > 0 && e.SourceSequence != b.Events[i-1].SourceSequence+1 {
			return Batch{}, nil, ErrSequence
		}
		if len(e.Payload) == 0 || len(e.Payload) > MaxEvent {
			return Batch{}, nil, ErrInvalid
		}
		var checked any
		if strictjson.Decode(e.Payload, &checked, MaxEvent) != nil {
			return Batch{}, nil, ErrInvalid
		}
		dec := json.NewDecoder(bytes.NewReader(e.Payload))
		dec.UseNumber()
		var v any
		if dec.Decode(&v) != nil {
			return Batch{}, nil, ErrInvalid
		}
		object, ok := v.(map[string]any)
		if !ok {
			return Batch{}, nil, ErrInvalid
		}
		production.NormalizePayloadTimes(e.Kind, object)
		p, err := json.Marshal(v)
		if err != nil {
			return Batch{}, nil, ErrInvalid
		}
		e.Payload = p
		e.EventAt = e.EventAt.UTC()
		total += len(p)
		if total > MaxBody {
			return Batch{}, nil, ErrInvalid
		}
	}
	body, err := json.Marshal(b)
	if err != nil || len(body) > MaxBody {
		return Batch{}, nil, ErrInvalid
	}
	return b, body, nil
}
func (b Batch) Validate() error { _, _, e := canonicalBatch(b); return e }

func (s *Service) AcceptBatch(ctx context.Context, a Actor, sc Scope, sourceID string, b Batch, key string) (Result, error) {
	return s.acceptBatch(ctx, a, sc, sourceID, b, key, "")
}

// acceptBatch is AcceptBatch for a caller that may own the source stream: importJob is the
// running import job whose chunks are allowed through, or "" for live intake.
func (s *Service) acceptBatch(ctx context.Context, a Actor, sc Scope, sourceID string, b Batch, key, importJob string) (Result, error) {
	var out Result
	if !validID(sc.TenantID) || !validID(sc.SiteID) || !validID(sourceID) || sourceID != b.SourceID || !validText(key, 128) || !validText(a.Issuer, 255) || !validText(a.Subject, 255) || !validText(a.SessionHash, 128) {
		return out, ErrInvalid
	}
	b, body, err := canonicalBatch(b)
	if err != nil {
		return out, err
	}
	hash := digest(body)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true),set_config('lock_timeout','5s',true)`, sc.TenantID); err != nil {
		return out, err
	}
	var issuer, subject string
	var expires, seen, now time.Time
	err = tx.QueryRowContext(ctx, `SELECT issuer,subject,expires_at,last_seen_at,clock_timestamp() FROM supervisor.browser_sessions WHERE token_hash=$1 FOR SHARE`, a.SessionHash).Scan(&issuer, &subject, &expires, &seen, &now)
	if err != nil || issuer != a.Issuer || subject != a.Subject || !expires.After(now) || !seen.After(now.Add(-s.sessions.Idle)) {
		return out, ErrDenied
	}
	var found bool
	if err = tx.QueryRowContext(ctx, `SELECT supervisor.lock_member_site($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, true).Scan(&found); err != nil {
		return out, err
	}
	if !found {
		return out, ErrNotFound
	}
	var role string
	err = tx.QueryRowContext(ctx, `SELECT role FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND active FOR SHARE`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	// Intake is deliberately open to operators: shop-floor devices submit under operator
	// sessions. Authority stays with the reviewed registry; nothing here approves anything.
	if role != "owner" && role != "supervisor" && role != "operator" {
		return out, ErrDenied
	}
	var stored string
	err = tx.QueryRowContext(ctx, `SELECT request_sha256,batch_id,source_id,source_revision,mapping_revision,first_sequence,last_sequence,event_count,accepted_at FROM supervisor.operational_event_batch_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&stored, &out.BatchID, &out.SourceID, &out.SourceRevision, &out.MappingRevision, &out.FirstSequence, &out.LastSequence, &out.EventCount, &out.AcceptedAt)
	if err == nil {
		if stored != hash {
			return Result{}, ErrConflict
		}
		out.Replayed = true
		if err = tx.Commit(); err != nil {
			return Result{}, err
		}
		return out, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	var srcRev, mapRev, headMapRev, reviewSrc, reviewMap int64
	var enabled bool
	var configHash, mapHash, reviewConfig, reviewMapping, verdict, authorityScope string
	err = tx.QueryRowContext(ctx, operational.SourceStateSQL, sc.TenantID, sc.SiteID, sourceID).Scan(&srcRev, &mapRev, &headMapRev, &enabled, &authorityScope, &configHash, &mapHash, &reviewSrc, &reviewMap, &reviewConfig, &reviewMapping, &verdict)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_event_sequence_heads(tenant_id,site_id,source_id,last_sequence) VALUES($1,$2,$3,0) ON CONFLICT DO NOTHING`, sc.TenantID, sc.SiteID, sourceID); err != nil {
		return out, err
	}
	var last int64
	if err = tx.QueryRowContext(ctx, `SELECT last_sequence FROM supervisor.operational_event_sequence_heads WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 FOR UPDATE`, sc.TenantID, sc.SiteID, sourceID).Scan(&last); err != nil {
		return out, err
	}
	err = tx.QueryRowContext(ctx, `SELECT request_sha256,batch_id,source_id,source_revision,mapping_revision,first_sequence,last_sequence,event_count,accepted_at FROM supervisor.operational_event_batch_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&stored, &out.BatchID, &out.SourceID, &out.SourceRevision, &out.MappingRevision, &out.FirstSequence, &out.LastSequence, &out.EventCount, &out.AcceptedAt)
	if err == nil {
		if stored != hash {
			return Result{}, ErrConflict
		}
		out.Replayed = true
		if err = tx.Commit(); err != nil {
			return Result{}, err
		}
		return out, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	// A running import owns the stream: its start sequence was reserved under this lock. A live
	// batch first ends an import abandoned past its lease (WIS-699): one interrupted import used to
	// block the source's live intake until its author retried it. An import's own chunks never
	// abandon anything, and a chunk whose job is no longer running is refused, so a cancelled job
	// never gains events its rows would not record. The job row is held FOR SHARE until this chunk
	// commits (WIS-719): a cancellation takes it FOR UPDATE, so it cannot commit between this check
	// and the chunk's events.
	if importJob == "" {
		if err = cancelStaleImports(ctx, tx, sc, sourceID); err != nil {
			return out, err
		}
	}
	var running string
	var jobStart int64
	switch err = tx.QueryRowContext(ctx, `SELECT id::text,start_sequence FROM supervisor.operational_import_jobs WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND status='running' FOR SHARE`, sc.TenantID, sc.SiteID, sourceID).Scan(&running, &jobStart); {
	case errors.Is(err, sql.ErrNoRows):
		if importJob != "" {
			return out, ErrImportCancelled
		}
	case err != nil:
		return out, err
	case running != importJob:
		return out, ErrImportRunning
	}
	if !operational.SourceCurrent(enabled, srcRev, mapRev, headMapRev, reviewSrc, reviewMap, verdict, configHash, mapHash, reviewConfig, reviewMapping, b.SourceRevision, b.MappingRevision) {
		return out, ErrRevision
	}
	// A malformed plan is a format error before persistence, never a stored quarantine.
	// It is checked after the receipt lookup so a batch accepted before plan payloads
	// were typed still replays idempotently instead of failing.
	for _, ev := range b.Events {
		if ev.Kind == "production_plan" {
			if _, err = production.DecodePlan(ev.Payload); err != nil {
				return out, ErrInvalid
			}
		}
	}
	if b.Events[0].SourceSequence != last+1 {
		return out, ErrSequence
	}
	for _, ev := range b.Events {
		if ev.SourceSequence != last+1 {
			return out, ErrSequence
		}
		last++
	}
	var stale int
	if err = tx.QueryRowContext(ctx, operational.StaleMappingSQL, sc.TenantID, sc.SiteID, sourceID, mapRev).Scan(&stale); err != nil {
		return out, err
	}
	if stale > 0 {
		return out, ErrRevision
	}
	duplicate := make(map[int64]string)
	seenExternal := make(map[string]string)
	for _, ev := range b.Events {
		identity := ev.ExternalKey + "\x00" + strconv.FormatInt(ev.Revision, 10)
		eventHash := eventDigest(ev)
		if prior, ok := seenExternal[identity]; ok {
			if prior != eventHash {
				return Result{}, ErrSourceRevisionConflict
			}
			duplicate[ev.SourceSequence] = "DUPLICATE_EVENT"
			continue
		}
		seenExternal[identity] = eventHash
		var prior string
		e := tx.QueryRowContext(ctx, `SELECT event_sha256 FROM supervisor.operational_event_records WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND external_key=$4 AND revision=$5 ORDER BY source_sequence DESC LIMIT 1`, sc.TenantID, sc.SiteID, sourceID, ev.ExternalKey, ev.Revision).Scan(&prior)
		if e == nil {
			current := eventHash
			if current != prior {
				return Result{}, ErrSourceRevisionConflict
			}
			duplicate[ev.SourceSequence] = "DUPLICATE_EVENT"
		} else if !errors.Is(e, sql.ErrNoRows) {
			return Result{}, e
		}
	}
	out = Result{BatchID: uuid.NewString(), SourceID: sourceID, SourceRevision: srcRev, MappingRevision: mapRev, FirstSequence: b.Events[0].SourceSequence, LastSequence: last, EventCount: len(b.Events)}
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&out.AcceptedAt); err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_event_batches(tenant_id,site_id,batch_id,source_id,source_revision,mapping_revision,first_sequence,last_sequence,event_count,body_sha256,accepted_by_issuer,accepted_by_subject,accepted_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, sc.TenantID, sc.SiteID, out.BatchID, sourceID, srcRev, mapRev, out.FirstSequence, out.LastSequence, out.EventCount, hash, a.Issuer, a.Subject, out.AcceptedAt); err != nil {
		return Result{}, err
	}
	for _, ev := range b.Events {
		reason := duplicate[ev.SourceSequence]
		var assetID, assetRev, orderID, orderRev any
		var assetEntity, orderEntity sql.NullString
		if ev.AssetExternalKey != "" {
			e := tx.QueryRowContext(ctx, operational.MappingEntitySQL, sc.TenantID, sc.SiteID, sourceID, mapRev, "asset", ev.AssetExternalKey).Scan(&assetEntity, &assetRev)
			if errors.Is(e, sql.ErrNoRows) {
				// An unresolved mapping always wins: the row stores no entity for this key.
				reason = "UNKNOWN_MAPPING"
			} else if e != nil {
				return Result{}, e
			} else if current, e := mappedCurrent(ctx, tx, sc, assetEntity.String, assetRev); e != nil {
				return Result{}, e
			} else if !current {
				reason, assetRev = "UNKNOWN_MAPPING", nil
			} else {
				assetID = assetEntity.String
			}
		}
		if ev.OrderExternalKey != "" {
			e := tx.QueryRowContext(ctx, operational.MappingEntitySQL, sc.TenantID, sc.SiteID, sourceID, mapRev, "order", ev.OrderExternalKey).Scan(&orderEntity, &orderRev)
			if errors.Is(e, sql.ErrNoRows) {
				// An unresolved mapping always wins: the row stores no entity for this key.
				reason = "UNKNOWN_MAPPING"
			} else if e != nil {
				return Result{}, e
			} else if current, e := mappedCurrent(ctx, tx, sc, orderEntity.String, orderRev); e != nil {
				return Result{}, e
			} else if !current {
				reason, orderRev = "UNKNOWN_MAPPING", nil
			} else {
				orderID = orderEntity.String
			}
		}
		var planRevisionID, planApprovalID any
		if ev.Kind == "production_plan" && reason == "" {
			admitted, err := admitPlan(ctx, tx, sc, authorityScope, ev, assetEntity, orderEntity)
			if err != nil {
				return Result{}, err
			}
			if admitted.reason != "" {
				reason = admitted.reason
			} else {
				planRevisionID, planApprovalID = admitted.planRevisionID, admitted.approvalID
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_event_records(tenant_id,site_id,source_id,source_revision,mapping_revision,source_sequence,external_key,revision,event_kind,event_at,payload,payload_sha256,event_sha256,asset_external_key,order_external_key,asset_entity_id,asset_entity_revision,order_entity_id,order_entity_revision,quarantine_reason,batch_id,plan_revision_id,plan_approval_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,$12,$13,NULLIF($14,''),NULLIF($15,''),$16,$17,$18,$19,NULLIF($20,''),$21,$22,$23)`, sc.TenantID, sc.SiteID, sourceID, srcRev, mapRev, ev.SourceSequence, ev.ExternalKey, ev.Revision, ev.Kind, ev.EventAt, string(ev.Payload), digest(ev.Payload), eventDigest(ev), ev.AssetExternalKey, ev.OrderExternalKey, assetID, assetRev, orderID, orderRev, reason, out.BatchID, planRevisionID, planApprovalID); err != nil {
			return Result{}, err
		}
		state := "pending"
		if reason != "" {
			state = "quarantined"
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_event_validations(tenant_id,site_id,source_id,source_sequence,validation_revision,status,reason,assessed_by) VALUES($1,$2,$3,$4,1,$5,NULLIF($6,''),'intake')`, sc.TenantID, sc.SiteID, sourceID, ev.SourceSequence, state, reason); err != nil {
			return Result{}, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_event_inbox(tenant_id,site_id,source_id,source_sequence,batch_id,state,reason) VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''))`, sc.TenantID, sc.SiteID, sourceID, ev.SourceSequence, out.BatchID, state, reason); err != nil {
			return Result{}, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_event_outbox(tenant_id,site_id,source_id,source_sequence,batch_id,event_type) VALUES($1,$2,$3,$4,$5,'operational.event.received')`, sc.TenantID, sc.SiteID, sourceID, ev.SourceSequence, out.BatchID); err != nil {
			return Result{}, err
		}
		// An import chunk records each row's outcome with its event (WIS-719): an interruption after
		// the commit can no longer leave stored events without outcomes in the job.
		if importJob != "" {
			status := "accepted"
			switch {
			case reason == "DUPLICATE_EVENT":
				status = "duplicate"
			case reason != "":
				status = "quarantined"
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_import_rows(tenant_id,site_id,job_id,row_index,status,reason,source_sequence) VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7) ON CONFLICT DO NOTHING`, sc.TenantID, sc.SiteID, importJob, ev.SourceSequence-jobStart+1, status, reason, ev.SourceSequence); err != nil {
				return Result{}, err
			}
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE supervisor.operational_event_sequence_heads SET last_sequence=$4 WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, sc.TenantID, sc.SiteID, sourceID, last); err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_event_batch_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_sha256,batch_id,source_id,source_revision,mapping_revision,first_sequence,last_sequence,event_count,accepted_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, hash, out.BatchID, sourceID, srcRev, mapRev, out.FirstSequence, out.LastSequence, out.EventCount, out.AcceptedAt); err != nil {
		return Result{}, err
	}
	if err = tx.Commit(); err != nil {
		return Result{}, err
	}
	return out, nil
}

// mappedCurrent fails a mapped entity closed when it was merged into another or its evidence
// was revoked, so the event is quarantined as UNKNOWN_MAPPING instead of being recorded
// against an identity GET /external-identities/resolve refuses (WIS-270).
func mappedCurrent(ctx context.Context, tx *sql.Tx, sc Scope, entityID string, revision any) (bool, error) {
	pinned, ok := revision.(int64)
	if !ok {
		return false, errors.New("mapping entity revision has an unexpected type")
	}
	return operational.MappedEntityCurrent(ctx, tx, sc.TenantID, sc.SiteID, entityID, pinned)
}
