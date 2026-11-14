package workstation

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
	"github.com/google/uuid"
)

// WIS-520 operator station records: what an authorized member enters on the workstation
// tablet against the job order an approved shift plan delivers there at the event time.
// Every record carries its actor, event and received time, workstation, plan, profile and
// job-order revision and its source. Records are immutable; a correction appends a record
// naming its exact predecessor. A quality record states what was observed and never
// releases or disposition-authorizes material.
var (
	ErrStationNotAuthorized   = errors.New("member has no current authorization for this workstation")
	ErrSelfAuthorization      = errors.New("a member cannot change their own station authorization")
	ErrGranteeNotEligible     = errors.New("grantee is not an active owner, supervisor or operator of this site")
	ErrAuthorizationUnchanged = errors.New("station authorization is already in the requested state")
	ErrStationNotConfigured   = errors.New("workstation has no station profile")
	ErrStationFormConflict    = errors.New("station profile and job order form conflict")
	ErrNoJobOrderToRecord     = errors.New("no approved shift plan assigns this workstation at the event time")
	ErrNeedsReplan            = errors.New("the job order pinned for this workstation was superseded")
	ErrInstructionUnavailable = errors.New("the job order instruction is no longer a confirmed-current document")
	ErrLifecycle              = errors.New("record kind is not allowed in the station job's lifecycle state")
	ErrStationJobCompleted    = errors.New("station job is completed")
	ErrCorrectionConflict     = errors.New("record already has a correction")
	ErrCorrectionNotPermitted = errors.New("only the original recorder, an owner or a supervisor may correct a record")
	// ErrInstructionRevoked is the revoked reason of ErrInstructionUnavailable (rule 06).
	ErrInstructionRevoked = fmt.Errorf("%w: its source was revoked", ErrInstructionUnavailable)
)

const (
	stationRecordSchema        = "StationRecordBodyV1"
	stationRecordDigestVersion = 1
	// maxEventSkew tolerates a tablet clock slightly ahead of the server; the database
	// enforces the same bound.
	maxEventSkew = 5 * time.Minute
	// maxEventAge bounds a late entry: an event older than this is refused, never stored.
	// The database enforces the same bound.
	maxEventAge = 7 * 24 * time.Hour
	maxChain    = 1000

	CodeInvalidStationRecord = "INVALID_STATION_RECORD"
	CodeInvalidCorrection    = "INVALID_CORRECTION"
	CodeReleaseForbidden     = "QUALITY_RECORD_CANNOT_RELEASE"
	CodeInvalidAuthorization = "INVALID_STATION_AUTHORIZATION"
	CodeEventTooOld          = "EVENT_TOO_OLD"
	CodeEventOutsideShift    = "EVENT_OUTSIDE_SHIFT"
)

func recordAt(path string) *ConfigError        { return &ConfigError{CodeInvalidStationRecord, path} }
func authorizationAt(path string) *ConfigError { return &ConfigError{CodeInvalidAuthorization, path} }

type RecordKind string

const (
	KindAcknowledged       RecordKind = "acknowledged"
	KindStarted            RecordKind = "started"
	KindPaused             RecordKind = "paused"
	KindBlocked            RecordKind = "blocked"
	KindResumed            RecordKind = "resumed"
	KindQuantity           RecordKind = "quantity"
	KindCycleTime          RecordKind = "cycle_time"
	KindQualityMeasurement RecordKind = "quality_measurement"
	KindQualityDisposition RecordKind = "quality_disposition_recorded"
	KindMaterialException  RecordKind = "material_exception"
	KindCompletionEvidence RecordKind = "completion_evidence"
	KindNote               RecordKind = "note"
)

// RecordKinds lists every kind in schema order.
func RecordKinds() []RecordKind {
	return []RecordKind{KindAcknowledged, KindStarted, KindPaused, KindBlocked, KindResumed, KindQuantity, KindCycleTime, KindQualityMeasurement, KindQualityDisposition, KindMaterialException, KindCompletionEvidence, KindNote}
}

// QualityDisposition is the disposition the operator observed or recorded, for example a
// hold tag on the bin. It is evidence for a later quality decision, not that decision.
type QualityDisposition string

const (
	DispositionAccept QualityDisposition = "accept"
	DispositionHold   QualityDisposition = "hold"
	DispositionReject QualityDisposition = "reject"
)

type MaterialIssue string

const (
	MaterialShortage MaterialIssue = "shortage"
	MaterialWrong    MaterialIssue = "wrong_material"
	MaterialDamaged  MaterialIssue = "damaged"
	MaterialOther    MaterialIssue = "other"
)

// RecordSource is the provenance of a record. Only manual tablet entry is qualified; a
// connected adapter is a separately qualified, later value.
type RecordSource string

const SourceManualTablet RecordSource = "manual_tablet"

// StationRecordBodyV1 is one operator entry. Members other than the common ones are allowed
// only for the kinds that use them (kindMembers); EventAt is when it happened, with an
// explicit offset. AuthorizesRelease is present only on quality_disposition_recorded and is
// always false: no record can release material.
type StationRecordBodyV1 struct {
	SchemaVersion     int                `json:"schema_version"`
	Kind              RecordKind         `json:"kind"`
	WorkstationID     string             `json:"workstation_id"`
	JobOrderID        string             `json:"job_order_id"`
	JobOrderRevision  int64              `json:"job_order_revision"`
	EventAt           string             `json:"event_at" contract:"datetime"`
	CorrectsRecordID  string             `json:"corrects_record_id,omitempty"`
	GoodQuantity      *int64             `json:"good_quantity,omitempty"`
	RejectQuantity    *int64             `json:"reject_quantity,omitempty"`
	CycleTimeSeconds  string             `json:"cycle_time_seconds,omitempty" contract:"decimal6"`
	Disposition       QualityDisposition `json:"disposition,omitempty"`
	AuthorizesRelease *bool              `json:"authorizes_release,omitempty"`
	MaterialCode      string             `json:"material_code,omitempty"`
	MaterialIssue     MaterialIssue      `json:"material_issue,omitempty"`
	Comment           string             `json:"comment,omitempty"`
	Values            []EntryValueV1     `json:"values,omitempty"`
}

// kindMembers names the optional members each kind takes: true when required. Values are
// checked against the resolved station form; a measurement may be partial, completion
// evidence must carry every required form value.
var kindMembers = map[RecordKind]map[string]bool{
	KindAcknowledged: {}, KindStarted: {}, KindResumed: {},
	KindPaused:             {"comment": true},
	KindBlocked:            {"comment": true},
	KindQuantity:           {"good_quantity": true, "reject_quantity": true},
	KindCycleTime:          {"cycle_time_seconds": true},
	KindQualityMeasurement: {"values": true},
	KindQualityDisposition: {"disposition": true, "authorizes_release": true, "comment": false},
	KindMaterialException:  {"material_code": true, "material_issue": true, "comment": false},
	KindCompletionEvidence: {"values": true},
	KindNote:               {"comment": true},
}

var memberOrder = []string{"good_quantity", "reject_quantity", "cycle_time_seconds", "disposition", "authorizes_release", "material_code", "material_issue", "comment", "values"}

func lifecycle(k RecordKind) bool {
	switch k {
	case KindAcknowledged, KindStarted, KindPaused, KindBlocked, KindResumed, KindCompletionEvidence:
		return true
	}
	return false
}

// correctable kinds carry an observed value; lifecycle transitions are never corrected.
func correctable(k RecordKind) bool {
	switch k {
	case KindAcknowledged, KindStarted, KindPaused, KindBlocked, KindResumed:
		return false
	}
	return true
}

// Validate returns the first offending member. A release claim is refused with its own code
// before any other member check.
func (b StationRecordBodyV1) Validate() *ConfigError {
	members, known := kindMembers[b.Kind]
	eventAt, timeOK := validTime(b.EventAt)
	switch {
	case b.AuthorizesRelease != nil && *b.AuthorizesRelease:
		return &ConfigError{CodeReleaseForbidden, "authorizes_release"}
	case b.SchemaVersion != 1:
		return recordAt("schema_version")
	case !known:
		return recordAt("kind")
	case !canonicalID(b.WorkstationID):
		return recordAt("workstation_id")
	case !canonicalID(b.JobOrderID):
		return recordAt("job_order_id")
	case b.JobOrderRevision < 1:
		return recordAt("job_order_revision")
	case !timeOK || eventAt.Nanosecond()%1000 != 0:
		// Sub-microsecond precision cannot be stored, so it is refused rather than rounded.
		return recordAt("event_at")
	case b.CorrectsRecordID != "" && !canonicalID(b.CorrectsRecordID):
		return recordAt("corrects_record_id")
	case b.CorrectsRecordID != "" && !correctable(b.Kind):
		return &ConfigError{CodeInvalidCorrection, "corrects_record_id"}
	}
	present := map[string]bool{"good_quantity": b.GoodQuantity != nil, "reject_quantity": b.RejectQuantity != nil, "cycle_time_seconds": b.CycleTimeSeconds != "",
		"disposition": b.Disposition != "", "authorizes_release": b.AuthorizesRelease != nil, "material_code": b.MaterialCode != "", "material_issue": b.MaterialIssue != "",
		"comment": b.Comment != "", "values": len(b.Values) > 0}
	for _, m := range memberOrder {
		required, allowed := members[m]
		if (present[m] && !allowed) || (required && !present[m]) {
			return recordAt(m)
		}
	}
	switch {
	case b.GoodQuantity != nil && *b.GoodQuantity < 0:
		return recordAt("good_quantity")
	case b.RejectQuantity != nil && *b.RejectQuantity < 0:
		return recordAt("reject_quantity")
	case b.CycleTimeSeconds != "" && (!validDecimal6(b.CycleTimeSeconds) || b.CycleTimeSeconds == "0" || len(b.CycleTimeSeconds) > 32):
		return recordAt("cycle_time_seconds")
	case b.Disposition != "" && b.Disposition != DispositionAccept && b.Disposition != DispositionHold && b.Disposition != DispositionReject:
		return recordAt("disposition")
	case b.MaterialCode != "" && (!validText(b.MaterialCode, 128) || executable(b.MaterialCode)):
		return recordAt("material_code")
	case b.MaterialIssue != "" && b.MaterialIssue != MaterialShortage && b.MaterialIssue != MaterialWrong && b.MaterialIssue != MaterialDamaged && b.MaterialIssue != MaterialOther:
		return recordAt("material_issue")
	case b.Comment != "" && (!validText(b.Comment, 1024) || executable(b.Comment)):
		return recordAt("comment")
	}
	return nil
}

// DecodeStationRecord strictly decodes and validates a record body.
func DecodeStationRecord(data []byte) (StationRecordBodyV1, error) {
	var b StationRecordBodyV1
	if strictjson.Decode(data, &b, limits.RequestBodyBytes) != nil {
		return StationRecordBodyV1{}, recordAt("")
	}
	if e := b.Validate(); e != nil {
		return StationRecordBodyV1{}, e
	}
	return b, nil
}

// recordEnvelope normalizes the event time to UTC and returns the canonical envelope.
func recordEnvelope(sc Scope, b StationRecordBodyV1) (StationRecordBodyV1, []byte, string, error) {
	if t, ok := validTime(b.EventAt); ok {
		b.EventAt = t.UTC().Format(time.RFC3339Nano)
	}
	encoded, digest, err := canonical.Envelope(stationRecordSchema, stationRecordDigestVersion, sc.TenantID, sc.SiteID, b)
	return b, encoded, digest, err
}

// lifecycleAllows reports whether kind may follow last, the station job's latest
// non-correction lifecycle record ("" when there is none):
//
//	none → acknowledged → started → (paused | blocked → resumed)* → completion_evidence
//
// Quantities, cycle times, quality and material records need a started job (paused or
// blocked included); a note is allowed any time. Completion closes the station job: only
// corrections, which skip this check, may follow it.
func lifecycleAllows(last, kind RecordKind) error {
	if last == KindCompletionEvidence {
		return ErrStationJobCompleted
	}
	running := last == KindStarted || last == KindResumed
	var ok bool
	switch kind {
	case KindAcknowledged:
		ok = last == ""
	case KindStarted:
		ok = last == KindAcknowledged
	case KindPaused, KindBlocked, KindCompletionEvidence:
		ok = running
	case KindResumed:
		ok = last == KindPaused || last == KindBlocked
	case KindNote:
		ok = true
	default:
		ok = running || last == KindPaused || last == KindBlocked
	}
	if !ok {
		return ErrLifecycle
	}
	return nil
}

// StationRecordV1 is a stored record. State is superseded once a correction names it;
// CorrectedByRecordID is that correction.
type StationRecordV1 struct {
	ID              string              `json:"id"`
	State           JobOrderState       `json:"state"`
	Record          StationRecordBodyV1 `json:"record"`
	PlanID          string              `json:"plan_id"`
	PlanRevision    int64               `json:"plan_revision"`
	ProfileRevision int64               `json:"profile_revision"`
	EventAt         time.Time           `json:"event_at"`
	ReceivedAt      time.Time           `json:"received_at"`
	RecordedBy      ActorRefV1          `json:"recorded_by"`
	Source          RecordSource        `json:"source"`
	// Late is true when the binding was no longer live on arrival: the plan was re-approved
	// or the job order revised after event_at.
	Late                   bool   `json:"late"`
	CorrectedByRecordID    string `json:"corrected_by_record_id,omitempty"`
	CanonicalPayloadSHA256 string `json:"canonical_payload_digest" contract:"sha256"`
	DigestSchemaVersion    int    `json:"digest_schema_version"`
	Replayed               bool   `json:"replayed"`
}

// StationRecordDetailV1 is a record with its whole correction chain, oldest first; the last
// chain entry is the record currently in force.
type StationRecordDetailV1 struct {
	Record StationRecordV1   `json:"record"`
	Chain  []StationRecordV1 `json:"chain"`
}

type StationRecordPageV1 struct {
	Records    []StationRecordV1 `json:"records"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

type AuthorizationAction string

const (
	AuthorizationGrant  AuthorizationAction = "grant"
	AuthorizationRevoke AuthorizationAction = "revoke"
)

// StationAuthorizationInputV1 grants or revokes one member's authority to record at the
// workstation in the path. The grantee is a target, never the caller's own authority.
type StationAuthorizationInputV1 struct {
	GranteeIssuer  string              `json:"grantee_issuer"`
	GranteeSubject string              `json:"grantee_subject"`
	Action         AuthorizationAction `json:"action"`
}

type StationAuthorizationEventV1 struct {
	Revision  int64               `json:"revision"`
	Action    AuthorizationAction `json:"action"`
	Actor     ActorRefV1          `json:"actor"`
	CreatedAt time.Time           `json:"created_at"`
}

// StationAuthorizationV1 is one grantee's authorization history at a workstation. Active is
// whether the latest event is a grant; CurrentRevision is the If-Match value.
type StationAuthorizationV1 struct {
	WorkstationID   string                        `json:"workstation_id"`
	Grantee         ActorRefV1                    `json:"grantee"`
	CurrentRevision int64                         `json:"current_revision"`
	Active          bool                          `json:"active"`
	Events          []StationAuthorizationEventV1 `json:"events"`
	Replayed        bool                          `json:"replayed"`
}

type StationAuthorizationPageV1 struct {
	Authorizations []StationAuthorizationV1 `json:"authorizations"`
	NextCursor     string                   `json:"next_cursor,omitempty"`
}

type StationRecordService struct{ plans *ShiftPlanService }

func NewStationRecords(db *sql.DB) *StationRecordService {
	return &StationRecordService{NewShiftPlans(db)}
}

// WithSessionPolicy returns a copy of the service that enforces p (WIS-290).
func (s *StationRecordService) WithSessionPolicy(p session.Policy) *StationRecordService {
	return &StationRecordService{s.plans.WithSessionPolicy(p)}
}

func stationAudit(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, workstationID, recordID, event string) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO supervisor.station_record_audit(tenant_id,site_id,actor_issuer,actor_subject,workstation_id,record_id,event_type) VALUES($1,$2,$3,$4,$5,NULLIF($6,'')::uuid,$7)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, workstationID, recordID, event)
	return e
}

// stationAuthorized is re-read on every submission, inside the writing transaction: the
// latest authorization event is a grant and the member is still an active owner, supervisor
// or operator. A revocation or membership change applies to the next request.
func stationAuthorized(ctx context.Context, tx *sql.Tx, sc Scope, workstationID, issuer, subject string) (bool, error) {
	var ok bool
	err := tx.QueryRowContext(ctx, `SELECT coalesce((SELECT action='grant' FROM supervisor.station_authorizations WHERE tenant_id=$1 AND site_id=$2 AND workstation_id=$3 AND grantee_issuer=$4 AND grantee_subject=$5 ORDER BY revision DESC LIMIT 1),false)
 AND EXISTS(SELECT 1 FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$4 AND subject=$5 AND active AND role IN ('owner','supervisor','operator'))`, sc.TenantID, sc.SiteID, workstationID, issuer, subject).Scan(&ok)
	return ok, err
}

// Submit records one operator entry against the job order the approved shift plan in force
// at the event time delivers to this workstation. Submissions serialize on the site row.
// ponytail: the site-wide write lock caps record throughput per site; lock the workstation
// instead if tablets ever contend.
func (s *StationRecordService) Submit(ctx context.Context, a Actor, sc Scope, workstationID string, b StationRecordBodyV1, key string) (StationRecordV1, error) {
	if !canonicalID(workstationID) {
		return StationRecordV1{}, recordAt("workstation_id")
	}
	if e := b.Validate(); e != nil {
		return StationRecordV1{}, e
	}
	if b.WorkstationID != workstationID {
		return StationRecordV1{}, recordAt("workstation_id")
	}
	if !validText(key, 128) {
		return StationRecordV1{}, recordAt("")
	}
	b, encoded, digest, err := recordEnvelope(sc, b)
	if err != nil {
		return StationRecordV1{}, recordAt("")
	}
	hash := requestDigest([]any{"station_record.submit", workstationID, digest})
	tx, _, now, err := s.plans.begin(ctx, a, sc, stationOperate)
	if err != nil {
		return StationRecordV1{}, err
	}
	defer tx.Rollback()
	// Authority is checked before an idempotent replay returns the stored record.
	authorized, err := stationAuthorized(ctx, tx, sc, workstationID, a.Issuer, a.Subject)
	if err != nil {
		return StationRecordV1{}, err
	}
	if !authorized {
		return StationRecordV1{}, ErrStationNotAuthorized
	}
	var stored, recordID string
	e := tx.QueryRowContext(ctx, `SELECT request_sha256,record_id FROM supervisor.station_record_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&stored, &recordID)
	switch {
	case e == nil && stored != hash:
		return StationRecordV1{}, ErrConflict
	case e == nil:
		if err = stationAudit(ctx, tx, a, sc, workstationID, recordID, "mutation.replayed"); err != nil {
			return StationRecordV1{}, err
		}
		out, err := loadRecord(ctx, tx, sc, workstationID, recordID)
		if err != nil {
			return out, err
		}
		out.Replayed = true
		return out, s.plans.finish(ctx, tx, a)
	case !errors.Is(e, sql.ErrNoRows):
		return StationRecordV1{}, e
	}
	eventAt, _ := validTime(b.EventAt)
	switch {
	case eventAt.After(now.Add(maxEventSkew)):
		return StationRecordV1{}, recordAt("event_at")
	// A late entry's event_at is client-supplied: it is bounded by maxEventAge here, and only
	// reaches the as-of instruction check when its job-order revision changed after event_at.
	case eventAt.Before(now.Add(-maxEventAge)):
		return StationRecordV1{}, &ConfigError{CodeEventTooOld, "event_at"}
	}
	var bind binding
	if b.CorrectsRecordID != "" {
		bind, err = correctionBinding(ctx, tx, a, sc, workstationID, b, eventAt)
	} else {
		bind, err = eventBinding(ctx, tx, sc, workstationID, b, eventAt)
	}
	if err != nil {
		return StationRecordV1{}, err
	}
	// Records need a started shift run, judged at receipt (the site lock orders this against
	// End). After the end only a late entry for an instant before it is taken, and it appends
	// a summary revision below; a correction may also keep its predecessor's event time (a
	// record entered from a tablet clock ahead of the server can lie at or after the end). A
	// correction of a record made before shift runs existed has no run and is still accepted.
	run, err := loadRun(ctx, tx, sc, bind.PlanID)
	switch {
	case err != nil:
		return StationRecordV1{}, err
	case !run.started && b.CorrectsRecordID == "":
		return StationRecordV1{}, ErrShiftNotStarted
	case run.ended.Valid && !eventAt.Before(run.ended.Time) && !(b.CorrectsRecordID != "" && eventAt.Equal(bind.PredecessorEventAt)):
		return StationRecordV1{}, ErrShiftRunEnded
	}
	order, err := loadJobOrder(ctx, tx, sc, b.JobOrderID)
	if err != nil {
		return StationRecordV1{}, err
	}
	profile, err := loadStationProfile(ctx, tx, sc, workstationID)
	if err != nil {
		return StationRecordV1{}, err
	}
	form, conflicts := ResolveForm(profile.Revisions[bind.ProfileRevision-1].Profile, &order.Revisions[bind.JobOrderRevision-1].Order)
	if len(conflicts) > 0 {
		return StationRecordV1{}, ErrStationFormConflict
	}
	if len(b.Values) > 0 {
		for _, v := range ValidateEntry(form, b.Values) {
			// A measurement records some form values; completion evidence must be complete.
			if b.Kind != KindQualityMeasurement || v.Code != EntryMissingRequired {
				return StationRecordV1{}, &ConfigError{v.Code, v.Path}
			}
		}
	}
	if b.CorrectsRecordID == "" {
		// Lifecycle state is per shift plan, so a job order carried into the next shift
		// starts afresh, and it is read at the event time: lifecycle events are monotonic in
		// event_at within that scope.
		var last string
		var later bool
		if e = tx.QueryRowContext(ctx, `SELECT coalesce((SELECT kind FROM supervisor.station_records WHERE tenant_id=$1 AND site_id=$2 AND workstation_id=$3 AND job_order_id=$4 AND plan_id=$5 AND corrects_record_id IS NULL
  AND kind IN ('acknowledged','started','paused','blocked','resumed','completion_evidence') AND event_at<=$6 ORDER BY event_at DESC,received_at DESC,id DESC LIMIT 1),''),
 EXISTS(SELECT 1 FROM supervisor.station_records WHERE tenant_id=$1 AND site_id=$2 AND workstation_id=$3 AND job_order_id=$4 AND plan_id=$5 AND corrects_record_id IS NULL
  AND kind IN ('acknowledged','started','paused','blocked','resumed','completion_evidence') AND event_at>$6)`, sc.TenantID, sc.SiteID, workstationID, b.JobOrderID, bind.PlanID, eventAt).Scan(&last, &later); e != nil {
			return StationRecordV1{}, e
		}
		if later && lifecycle(b.Kind) {
			return StationRecordV1{}, ErrLifecycle
		}
		if err = lifecycleAllows(RecordKind(last), b.Kind); err != nil {
			return StationRecordV1{}, err
		}
	}
	payload, err := json.Marshal(b)
	if err != nil {
		return StationRecordV1{}, recordAt("")
	}
	recordID = uuid.NewString()
	// A concurrent correction of the same predecessor is a 409, never a unique violation.
	written, e := tx.ExecContext(ctx, `INSERT INTO supervisor.station_records(tenant_id,site_id,id,workstation_id,job_order_id,job_order_revision,plan_id,plan_revision,profile_revision,kind,event_at,actor_issuer,actor_subject,source,late,corrects_record_id,payload,canonical_payload,canonical_payload_sha256,digest_schema_version)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::timestamptz,$12,$13,$14,$15,NULLIF($16,'')::uuid,$17::jsonb,$18,$19,$20) ON CONFLICT(tenant_id,site_id,corrects_record_id) DO NOTHING`,
		sc.TenantID, sc.SiteID, recordID, workstationID, b.JobOrderID, b.JobOrderRevision, bind.PlanID, bind.PlanRevision, bind.ProfileRevision, string(b.Kind), b.EventAt,
		a.Issuer, a.Subject, string(SourceManualTablet), bind.Late, b.CorrectsRecordID, string(payload), string(encoded), digest, stationRecordDigestVersion)
	if e != nil {
		return StationRecordV1{}, e
	}
	if n, err := written.RowsAffected(); err != nil || n != 1 {
		return StationRecordV1{}, ErrCorrectionConflict
	}
	if written, e = tx.ExecContext(ctx, `INSERT INTO supervisor.station_record_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_sha256,record_id) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, hash, recordID); e != nil {
		return StationRecordV1{}, e
	}
	if n, err := written.RowsAffected(); err != nil || n != 1 {
		return StationRecordV1{}, ErrConflict
	}
	if e = stationAudit(ctx, tx, a, sc, workstationID, recordID, "station_record.submitted"); e != nil {
		return StationRecordV1{}, e
	}
	// An owner or supervisor overriding another member's entry is audited with that recorder.
	if b.CorrectsRecordID != "" && bind.Predecessor != (ActorRefV1{a.Issuer, a.Subject}) {
		if _, e = tx.ExecContext(ctx, `INSERT INTO supervisor.station_record_audit(tenant_id,site_id,actor_issuer,actor_subject,workstation_id,record_id,event_type,predecessor_actor_issuer,predecessor_actor_subject) VALUES($1,$2,$3,$4,$5,$6,'station_record.corrected_by_supervisor',$7,$8)`,
			sc.TenantID, sc.SiteID, a.Issuer, a.Subject, workstationID, recordID, bind.Predecessor.Issuer, bind.Predecessor.Subject); e != nil {
			return StationRecordV1{}, e
		}
	}
	// A late entry after the run ended appends a summary revision; the run's cited one stays.
	if run.ended.Valid {
		plan, err := loadShiftPlan(ctx, tx, sc, bind.PlanID)
		if err != nil {
			return StationRecordV1{}, err
		}
		revision, err := appendSummary(ctx, tx, a, sc, plan, now, run.endedAt())
		if err != nil {
			return StationRecordV1{}, err
		}
		if err = summaryAudit(ctx, tx, a, sc, bind.PlanID, revision, "shift_summary.generated"); err != nil {
			return StationRecordV1{}, err
		}
	}
	out, err := loadRecord(ctx, tx, sc, workstationID, recordID)
	if err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

// binding is what a record is entered against: the plan revision in force at its event
// time, the job-order revision that revision assigned, and the station profile revision
// current then. Late is set when that is no longer the live binding on arrival.
type binding struct {
	PlanID                                          string
	PlanRevision, JobOrderRevision, ProfileRevision int64
	Late                                            bool
	// Predecessor is a corrected record's recorder, PredecessorEventAt its event time; empty
	// for a new entry.
	Predecessor        ActorRefV1
	PredecessorEventAt time.Time
}

// eventBinding resolves a new entry historically at event_at: the latest approval, made by
// event_at, of the plan whose shift contains event_at and assigns this workstation. A pin
// already superseded at event_at is NEEDS_REPLAN. An entry whose binding is no longer live
// (plan re-approved or job order revised since) is accepted against the historical binding
// and marked late, so a re-approval or revision never loses shop-floor data.
func eventBinding(ctx context.Context, tx *sql.Tx, sc Scope, workstationID string, b StationRecordBodyV1, eventAt time.Time) (binding, error) {
	var out binding
	rows, err := tx.QueryContext(ctx, `SELECT h.id,ap.revision,x.job_order_id,x.job_order_revision,h.approved_revision<>ap.revision,j.current_revision<>x.job_order_revision,
 EXISTS(SELECT 1 FROM supervisor.job_order_revisions r WHERE (r.tenant_id,r.site_id,r.job_order_id)=(x.tenant_id,x.site_id,x.job_order_id) AND r.revision>x.job_order_revision AND r.created_at<=$4)
 FROM supervisor.shift_plan_heads h
 CROSS JOIN LATERAL (SELECT a.revision FROM supervisor.shift_plan_approvals a WHERE (a.tenant_id,a.site_id,a.plan_id)=(h.tenant_id,h.site_id,h.id) AND a.approved_at<=$4 ORDER BY a.approved_at DESC,a.revision DESC LIMIT 1) ap
 JOIN supervisor.shift_plan_assignments x ON (x.tenant_id,x.site_id,x.plan_id,x.revision)=(h.tenant_id,h.site_id,h.id,ap.revision) AND x.workstation_id=$3
 JOIN supervisor.job_order_heads j ON (j.tenant_id,j.site_id,j.id)=(x.tenant_id,x.site_id,x.job_order_id)
 WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.starts_at<=$4 AND $4<h.ends_at ORDER BY h.id LIMIT 2`, sc.TenantID, sc.SiteID, workstationID, eventAt)
	if err != nil {
		return out, err
	}
	found := 0
	var jobOrderID string
	var superseded, planLate, jobLate bool
	for rows.Next() {
		found++
		if err = rows.Scan(&out.PlanID, &out.PlanRevision, &jobOrderID, &out.JobOrderRevision, &planLate, &jobLate, &superseded); err != nil {
			rows.Close()
			return out, err
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return out, err
	}
	switch {
	case found == 0:
		return out, ErrNoJobOrderToRecord
	case found > 1:
		return out, ErrAssignmentConflict
	case superseded:
		return out, ErrNeedsReplan
	case jobOrderID != b.JobOrderID || out.JobOrderRevision != b.JobOrderRevision:
		return out, ErrJobOrderNotCurrent
	}
	if err = tx.QueryRowContext(ctx, `SELECT coalesce((SELECT revision FROM supervisor.station_profile_revisions WHERE tenant_id=$1 AND site_id=$2 AND workstation_id=$3 AND created_at<=$4 ORDER BY revision DESC LIMIT 1),0)`,
		sc.TenantID, sc.SiteID, workstationID, eventAt).Scan(&out.ProfileRevision); err != nil {
		return out, err
	}
	if out.ProfileRevision == 0 {
		return out, ErrStationNotConfigured
	}
	out.Late = planLate || jobLate
	// The instruction is checked now, or, when the job-order revision itself was superseded
	// after the event, as of event_at: the instruction that revision pinned must have been
	// confirmed current then. A plan re-approval alone keeps the current check, so an
	// instruction withdrawn at any point before arrival cannot be bound by re-approving the plan.
	order, err := loadJobOrder(ctx, tx, sc, b.JobOrderID)
	if err != nil {
		return out, err
	}
	instruction := orderInstruction(order.Revisions[out.JobOrderRevision-1].Order)
	var reason InstructionReason
	if jobLate {
		reason, err = instructionReasonAt(ctx, tx, sc, instruction, eventAt)
	} else {
		reason, err = instructionReason(ctx, tx, sc, instruction)
	}
	switch {
	case err != nil:
		return out, err
	case reason == InstructionRevoked:
		return out, ErrInstructionRevoked
	case reason != "":
		return out, ErrInstructionUnavailable
	}
	return out, nil
}

// instructionReasonAt applies instructionApproved's predicate to the document ledger as it
// stood at the instant at, from the append-only rows' database-stamped created_at: the latest
// currentness decision by then confirms exactly this revision and hash under the authority
// decision then in force (a designation), no later source revision and no revocation existed
// by then, and the decision pinned the access epoch then in force. The epoch moves only on
// the single revocation (evidence_head_guard), so it is the head's epoch less one when that
// revocation came after at. Every ledger class carries its timestamp, so none fails closed
// for want of history; a source unknown then is revoked, as in instructionReason.
func instructionReasonAt(ctx context.Context, tx *sql.Tx, sc Scope, o InstructionRefV1, at time.Time) (InstructionReason, error) {
	var revoked, superseded, approved bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.evidence_revocations rv WHERE (rv.tenant_id,rv.site_id,rv.source_id)=(eh.tenant_id,eh.site_id,eh.source_id) AND rv.created_at<=$6),
 EXISTS(SELECT 1 FROM supervisor.evidence_revisions r WHERE (r.tenant_id,r.site_id,r.source_id)=(eh.tenant_id,eh.site_id,eh.source_id) AND r.revision>$4 AND r.created_at<=$6),
 coalesce((SELECT d.verdict='confirm_current' AND d.source_revision=$4 AND d.source_sha256=$5 AND r.kind='document' AND r.created_at<=$6
   AND d.source_access_epoch=eh.access_epoch-(EXISTS(SELECT 1 FROM supervisor.evidence_revocations rv WHERE (rv.tenant_id,rv.site_id,rv.source_id)=(eh.tenant_id,eh.site_id,eh.source_id) AND rv.created_at>$6))::int
   AND (a.revision,a.decision_id)=(d.authority_revision,d.authority_decision_id) AND a.verdict='designate' AND (r.extraction IS NULL OR a.model_extracted_confirmed)
  FROM supervisor.evidence_currentness_decisions d
  JOIN supervisor.evidence_revisions r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=(d.tenant_id,d.site_id,d.source_id,d.source_revision)
  CROSS JOIN LATERAL (SELECT x.revision,x.decision_id,x.verdict,x.model_extracted_confirmed FROM supervisor.evidence_authority_decisions x WHERE (x.tenant_id,x.site_id,x.source_id)=(d.tenant_id,d.site_id,d.source_id) AND x.created_at<=$6 ORDER BY x.revision DESC LIMIT 1) a
  WHERE (d.tenant_id,d.site_id,d.source_id)=(eh.tenant_id,eh.site_id,eh.source_id) AND d.created_at<=$6 ORDER BY d.revision DESC LIMIT 1),false)
 FROM supervisor.evidence_heads eh WHERE eh.tenant_id=$1 AND eh.site_id=$2 AND eh.source_id=$3`, sc.TenantID, sc.SiteID, o.DocumentID, o.Revision, o.SHA256, at).Scan(&revoked, &superseded, &approved)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return InstructionRevoked, nil
	case err != nil:
		return "", err
	case revoked:
		return InstructionRevoked, nil
	case approved && !superseded:
		return "", nil
	case superseded:
		return InstructionSuperseded, nil
	}
	return InstructionNotConfirmed, nil
}

// correctionBinding binds a correction to its predecessor's plan, job-order and profile
// revisions without resolving the current order, so a value can be corrected after its
// shift ended or its job order was revised. The corrected event time stays in that shift.
// Only the predecessor's recorder corrects it, or an active owner or supervisor of the site
// (still station-authorized, checked by Submit); an operator never corrects another's record.
func correctionBinding(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, workstationID string, b StationRecordBodyV1, eventAt time.Time) (binding, error) {
	var out binding
	var kind, jobOrderID string
	var inShift, corrected, permitted bool
	e := tx.QueryRowContext(ctx, `SELECT r.kind,r.job_order_id,r.job_order_revision,r.plan_id,r.plan_revision,r.profile_revision,r.actor_issuer,r.actor_subject,r.event_at,p.starts_at<=$5 AND $5<p.ends_at,
 h.approved_revision<>r.plan_revision OR j.current_revision<>r.job_order_revision,
 EXISTS(SELECT 1 FROM supervisor.station_records c WHERE (c.tenant_id,c.site_id,c.corrects_record_id)=(r.tenant_id,r.site_id,r.id)),
 (r.actor_issuer,r.actor_subject)=($6,$7) OR EXISTS(SELECT 1 FROM supervisor.memberships m WHERE m.tenant_id=$1 AND m.site_id=$2 AND m.issuer=$6 AND m.subject=$7 AND m.active AND m.role IN ('owner','supervisor'))
 FROM supervisor.station_records r
 JOIN supervisor.shift_plan_revisions p ON (p.tenant_id,p.site_id,p.plan_id,p.revision)=(r.tenant_id,r.site_id,r.plan_id,r.plan_revision)
 JOIN supervisor.shift_plan_heads h ON (h.tenant_id,h.site_id,h.id)=(r.tenant_id,r.site_id,r.plan_id)
 JOIN supervisor.job_order_heads j ON (j.tenant_id,j.site_id,j.id)=(r.tenant_id,r.site_id,r.job_order_id)
 WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.workstation_id=$3 AND r.id=$4`, sc.TenantID, sc.SiteID, workstationID, b.CorrectsRecordID, eventAt, a.Issuer, a.Subject).Scan(
		&kind, &jobOrderID, &out.JobOrderRevision, &out.PlanID, &out.PlanRevision, &out.ProfileRevision, &out.Predecessor.Issuer, &out.Predecessor.Subject, &out.PredecessorEventAt, &inShift, &out.Late, &corrected, &permitted)
	switch {
	case errors.Is(e, sql.ErrNoRows) || (e == nil && (RecordKind(kind) != b.Kind || jobOrderID != b.JobOrderID || out.JobOrderRevision != b.JobOrderRevision)):
		return out, &ConfigError{CodeInvalidCorrection, "corrects_record_id"}
	case e != nil:
		return out, e
	case !permitted:
		return out, ErrCorrectionNotPermitted
	case corrected:
		return out, ErrCorrectionConflict
	case !inShift:
		return out, &ConfigError{CodeEventOutsideShift, "event_at"}
	}
	return out, nil
}

// Get returns a record of this workstation with its whole correction chain.
func (s *StationRecordService) Get(ctx context.Context, a Actor, sc Scope, workstationID, id string) (StationRecordDetailV1, error) {
	out := StationRecordDetailV1{Chain: []StationRecordV1{}}
	if !canonicalID(workstationID) || !canonicalID(id) {
		return out, ErrNotFound
	}
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftRead)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if out.Record, err = loadRecord(ctx, tx, sc, workstationID, id); err != nil {
		return out, err
	}
	// ponytail: one query per chain link; chains are a handful of corrections long. The
	// walk is bounded and refuses a revisited record, so damaged links cannot loop.
	seen := map[string]bool{id: true}
	link := func(next string) (StationRecordV1, error) {
		if seen[next] || len(seen) > maxChain {
			return StationRecordV1{}, errIntegrity
		}
		seen[next] = true
		return loadRecord(ctx, tx, sc, workstationID, next)
	}
	var before []StationRecordV1
	for r := out.Record; r.Record.CorrectsRecordID != ""; {
		if r, err = link(r.Record.CorrectsRecordID); err != nil {
			return out, err
		}
		before = append([]StationRecordV1{r}, before...)
	}
	out.Chain = append(before, out.Record)
	for r := out.Record; r.CorrectedByRecordID != ""; {
		if r, err = link(r.CorrectedByRecordID); err != nil {
			return out, err
		}
		out.Chain = append(out.Chain, r)
	}
	if err = stationAudit(ctx, tx, a, sc, workstationID, id, "station_record.read"); err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

// List pages a workstation's records in received order, optionally for one job order.
// Superseded records stay listed with their state and correction.
func (s *StationRecordService) List(ctx context.Context, a Actor, sc Scope, workstationID, jobOrderID string, limit int, cursor string) (StationRecordPageV1, error) {
	out := StationRecordPageV1{Records: []StationRecordV1{}}
	if !canonicalID(workstationID) {
		return out, ErrNotFound
	}
	if limit < 1 || limit > 100 || (cursor != "" && !canonicalID(cursor)) || (jobOrderID != "" && !canonicalID(jobOrderID)) {
		return out, recordAt("")
	}
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftRead)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, recordSelect+` AND ($4='' OR r.job_order_id::text=$4)
 AND ($5='' OR (r.received_at,r.id)>(SELECT received_at,id FROM supervisor.station_records WHERE tenant_id=$1 AND site_id=$2 AND id::text=$5)) ORDER BY r.received_at,r.id LIMIT $6`, sc.TenantID, sc.SiteID, workstationID, jobOrderID, cursor, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanRecord(rows, sc)
		if err != nil {
			return out, err
		}
		out.Records = append(out.Records, r)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(out.Records) > limit {
		out.Records = out.Records[:limit]
		out.NextCursor = out.Records[limit-1].ID
	}
	if err = stationAudit(ctx, tx, a, sc, workstationID, "", "station_record.listed"); err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

const recordScope = `SELECT r.id,r.kind,r.plan_id,r.plan_revision,r.profile_revision,r.event_at,r.received_at,r.actor_issuer,r.actor_subject,r.source,r.late,coalesce(c.id::text,''),r.payload,r.canonical_payload_sha256,r.digest_schema_version
 FROM supervisor.station_records r LEFT JOIN supervisor.station_records c ON (c.tenant_id,c.site_id,c.corrects_record_id)=(r.tenant_id,r.site_id,r.id)
 WHERE r.tenant_id=$1 AND r.site_id=$2`

const recordSelect = recordScope + ` AND r.workstation_id=$3`

func scanRecord(row interface{ Scan(...any) error }, sc Scope) (StationRecordV1, error) {
	var r StationRecordV1
	var kind string
	var payload []byte
	if e := row.Scan(&r.ID, &kind, &r.PlanID, &r.PlanRevision, &r.ProfileRevision, &r.EventAt, &r.ReceivedAt, &r.RecordedBy.Issuer, &r.RecordedBy.Subject, &r.Source, &r.Late, &r.CorrectedByRecordID, &payload, &r.CanonicalPayloadSHA256, &r.DigestSchemaVersion); e != nil {
		return r, e
	}
	// Stored bodies are re-validated and re-verified against their digest on every read.
	body, e := DecodeStationRecord(payload)
	if e != nil || string(body.Kind) != kind {
		return r, errIntegrity
	}
	if _, _, recomputed, err := recordEnvelope(sc, body); err != nil || recomputed != r.CanonicalPayloadSHA256 {
		return r, errIntegrity
	}
	r.Record, r.EventAt, r.ReceivedAt, r.State = body, r.EventAt.UTC(), r.ReceivedAt.UTC(), JobOrderCurrent
	if r.CorrectedByRecordID != "" {
		r.State = JobOrderSuperseded
	}
	return r, nil
}

func loadRecord(ctx context.Context, tx *sql.Tx, sc Scope, workstationID, id string) (StationRecordV1, error) {
	r, e := scanRecord(tx.QueryRowContext(ctx, recordSelect+` AND r.id=$4`, sc.TenantID, sc.SiteID, workstationID, id), sc)
	if errors.Is(e, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, e
}

// Authorize grants or revokes one member's station authorization. Owners and supervisors
// decide; nobody changes their own. If-Match carries the grantee's current revision ("0"
// before the first grant). Only an active owner, supervisor or operator can be granted.
func (s *StationRecordService) Authorize(ctx context.Context, a Actor, sc Scope, workstationID string, in StationAuthorizationInputV1, key string, expected int64) (StationAuthorizationV1, error) {
	switch {
	case !canonicalID(workstationID):
		return StationAuthorizationV1{}, authorizationAt("workstation_id")
	case !validText(in.GranteeIssuer, 255):
		return StationAuthorizationV1{}, authorizationAt("grantee_issuer")
	case !validText(in.GranteeSubject, 255):
		return StationAuthorizationV1{}, authorizationAt("grantee_subject")
	case in.Action != AuthorizationGrant && in.Action != AuthorizationRevoke:
		return StationAuthorizationV1{}, authorizationAt("action")
	case !validText(key, 128) || expected < 0:
		return StationAuthorizationV1{}, authorizationAt("")
	}
	hash := requestDigest([]any{"station_authorization", workstationID, expected, in})
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftWrite)
	if err != nil {
		return StationAuthorizationV1{}, err
	}
	defer tx.Rollback()
	var stored string
	e := tx.QueryRowContext(ctx, `SELECT request_sha256 FROM supervisor.station_authorization_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&stored)
	switch {
	case e == nil && stored != hash:
		return StationAuthorizationV1{}, ErrConflict
	case e == nil:
		if err = stationAudit(ctx, tx, a, sc, workstationID, "", "mutation.replayed"); err != nil {
			return StationAuthorizationV1{}, err
		}
		out, err := loadAuthorization(ctx, tx, sc, workstationID, in.GranteeIssuer, in.GranteeSubject)
		if err != nil {
			return out, err
		}
		out.Replayed = true
		return out, s.plans.finish(ctx, tx, a)
	case !errors.Is(e, sql.ErrNoRows):
		return StationAuthorizationV1{}, e
	}
	if in.GranteeIssuer == a.Issuer && in.GranteeSubject == a.Subject {
		return StationAuthorizationV1{}, ErrSelfAuthorization
	}
	var configured, eligible bool
	var current int64
	var last string
	if e = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.station_profile_heads WHERE tenant_id=$1 AND site_id=$2 AND workstation_id=$3),
 EXISTS(SELECT 1 FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$4 AND subject=$5 AND active AND role IN ('owner','supervisor','operator')),
 coalesce((SELECT revision FROM supervisor.station_authorizations WHERE tenant_id=$1 AND site_id=$2 AND workstation_id=$3 AND grantee_issuer=$4 AND grantee_subject=$5 ORDER BY revision DESC LIMIT 1),0),
 coalesce((SELECT action FROM supervisor.station_authorizations WHERE tenant_id=$1 AND site_id=$2 AND workstation_id=$3 AND grantee_issuer=$4 AND grantee_subject=$5 ORDER BY revision DESC LIMIT 1),'')`,
		sc.TenantID, sc.SiteID, workstationID, in.GranteeIssuer, in.GranteeSubject).Scan(&configured, &eligible, &current, &last); e != nil {
		return StationAuthorizationV1{}, e
	}
	switch {
	case !configured:
		return StationAuthorizationV1{}, ErrNotFound
	case current != expected:
		return StationAuthorizationV1{}, ErrRevision
	case (in.Action == AuthorizationGrant) == (last == string(AuthorizationGrant)):
		return StationAuthorizationV1{}, ErrAuthorizationUnchanged
	case in.Action == AuthorizationGrant && !eligible:
		return StationAuthorizationV1{}, ErrGranteeNotEligible
	}
	written, e := tx.ExecContext(ctx, `INSERT INTO supervisor.station_authorizations(tenant_id,site_id,workstation_id,grantee_issuer,grantee_subject,revision,action,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`,
		sc.TenantID, sc.SiteID, workstationID, in.GranteeIssuer, in.GranteeSubject, current+1, string(in.Action), a.Issuer, a.Subject)
	if e != nil {
		return StationAuthorizationV1{}, e
	}
	if n, err := written.RowsAffected(); err != nil || n != 1 {
		return StationAuthorizationV1{}, ErrRevision
	}
	if written, e = tx.ExecContext(ctx, `INSERT INTO supervisor.station_authorization_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_sha256,workstation_id,grantee_issuer,grantee_subject,revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT DO NOTHING`,
		sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, hash, workstationID, in.GranteeIssuer, in.GranteeSubject, current+1); e != nil {
		return StationAuthorizationV1{}, e
	}
	if n, err := written.RowsAffected(); err != nil || n != 1 {
		return StationAuthorizationV1{}, ErrConflict
	}
	event := "station_authorization.granted"
	if in.Action == AuthorizationRevoke {
		event = "station_authorization.revoked"
	}
	if e = stationAudit(ctx, tx, a, sc, workstationID, "", event); e != nil {
		return StationAuthorizationV1{}, e
	}
	out, err := loadAuthorization(ctx, tx, sc, workstationID, in.GranteeIssuer, in.GranteeSubject)
	if err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

// Authorizations pages a workstation's grantees by issuer and subject. The cursor is opaque.
func (s *StationRecordService) Authorizations(ctx context.Context, a Actor, sc Scope, workstationID string, limit int, cursor string) (StationAuthorizationPageV1, error) {
	out := StationAuthorizationPageV1{Authorizations: []StationAuthorizationV1{}}
	if !canonicalID(workstationID) {
		return out, ErrNotFound
	}
	var afterIssuer, afterSubject string
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		var ok bool
		afterIssuer, afterSubject, ok = strings.Cut(string(raw), "\n")
		if err != nil || !ok || !validText(afterIssuer, 255) || !validText(afterSubject, 255) {
			return out, authorizationAt("cursor")
		}
	}
	if limit < 1 || limit > 100 {
		return out, authorizationAt("limit")
	}
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftRead)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT grantee_issuer,grantee_subject FROM supervisor.station_authorizations WHERE tenant_id=$1 AND site_id=$2 AND workstation_id=$3 AND (grantee_issuer,grantee_subject)>($4,$5) ORDER BY 1,2 LIMIT $6`,
		sc.TenantID, sc.SiteID, workstationID, afterIssuer, afterSubject, limit+1)
	if err != nil {
		return out, err
	}
	var grantees []ActorRefV1
	for rows.Next() {
		var g ActorRefV1
		if err = rows.Scan(&g.Issuer, &g.Subject); err != nil {
			rows.Close()
			return out, err
		}
		grantees = append(grantees, g)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(grantees) > limit {
		grantees = grantees[:limit]
		out.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(grantees[limit-1].Issuer + "\n" + grantees[limit-1].Subject))
	}
	for _, g := range grantees {
		auth, err := loadAuthorization(ctx, tx, sc, workstationID, g.Issuer, g.Subject)
		if err != nil {
			return out, err
		}
		out.Authorizations = append(out.Authorizations, auth)
	}
	if err = stationAudit(ctx, tx, a, sc, workstationID, "", "station_authorization.listed"); err != nil {
		return out, err
	}
	return out, s.plans.finish(ctx, tx, a)
}

func loadAuthorization(ctx context.Context, tx *sql.Tx, sc Scope, workstationID, issuer, subject string) (StationAuthorizationV1, error) {
	out := StationAuthorizationV1{WorkstationID: workstationID, Grantee: ActorRefV1{Issuer: issuer, Subject: subject}, Events: []StationAuthorizationEventV1{}}
	rows, e := tx.QueryContext(ctx, `SELECT revision,action,actor_issuer,actor_subject,created_at FROM supervisor.station_authorizations WHERE tenant_id=$1 AND site_id=$2 AND workstation_id=$3 AND grantee_issuer=$4 AND grantee_subject=$5 ORDER BY revision`, sc.TenantID, sc.SiteID, workstationID, issuer, subject)
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		var ev StationAuthorizationEventV1
		if e = rows.Scan(&ev.Revision, &ev.Action, &ev.Actor.Issuer, &ev.Actor.Subject, &ev.CreatedAt); e != nil {
			return out, e
		}
		ev.CreatedAt = ev.CreatedAt.UTC()
		out.Events = append(out.Events, ev)
	}
	if e = rows.Err(); e != nil {
		return out, e
	}
	if len(out.Events) == 0 {
		return out, ErrNotFound
	}
	last := out.Events[len(out.Events)-1]
	out.CurrentRevision, out.Active = last.Revision, last.Action == AuthorizationGrant
	return out, nil
}
