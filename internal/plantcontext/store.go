package plantcontext

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/production"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
	"github.com/google/uuid"
)

var (
	ErrInvalid         = errors.New("invalid context request")
	ErrNotFound        = errors.New("context unavailable")
	ErrDenied          = errors.New("context permission denied")
	ErrConflict        = errors.New("context idempotency conflict")
	ErrUnknownEntity   = errors.New("context order or asset is not a known entity")
	ErrUnknownSnapshot = errors.New("process snapshot unavailable")
	errIntegrity       = errors.New("context integrity check failed")
)

type Actor struct{ Issuer, Subject, SessionHash string }
type Scope struct{ TenantID, SiteID string }

// Request is the POST /contexts body.
type Request struct {
	OrderID           string `json:"order_id"`
	AssetID           string `json:"asset_id"`
	WindowStart       string `json:"window_start"`
	WindowEnd         string `json:"window_end"`
	ProcessSnapshotID string `json:"process_snapshot_id"`
}

// CurrentStatus is re-evaluated on every read against the authoritative ledgers. The
// stored snapshot is exact history; this says whether it may still be relied upon.
type CurrentStatus struct {
	Eligible     bool      `json:"eligible"`
	Reasons      []string  `json:"reasons"`
	SupersededBy *string   `json:"superseded_by"`
	EvaluatedAt  time.Time `json:"evaluated_at"`
}

type View struct {
	Snapshot
	CurrentStatus CurrentStatus `json:"current_status"`
	// Reused is true when an identical context (same digest) already existed.
	Reused   bool `json:"reused"`
	Replayed bool `json:"replayed"`
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

func validID(v string) bool {
	id, e := uuid.Parse(v)
	return e == nil && id != uuid.Nil && id.String() == v
}
func validText(v string, max int) bool {
	return len(v) > 0 && len(v) <= max && strings.TrimSpace(v) == v && !strings.ContainsRune(v, 0)
}

// recordNamespace names the record-ID namespace (a fixed, arbitrary UUID).
var recordNamespace = uuid.MustParse("6f1d2c3e-7a4b-4c5d-9e8f-0a1b2c3d4e5f")

// RecordID is the deterministic identifier of one source record, as cited in a context's
// source_record_refs and as a case trigger's source_event_id (WIS-273).
func RecordID(sc Scope, sourceID string, sequence int64) string {
	return recordID(sc, sourceID, sequence)
}

// recordID is the deterministic identifier of one source record.
func recordID(sc Scope, sourceID string, sequence int64) string {
	return uuid.NewSHA1(recordNamespace, []byte(sc.TenantID+"|"+sc.SiteID+"|"+sourceID+"|"+strconv.FormatInt(sequence, 10))).String()
}

// normalized rewrites window bounds to canonical UTC so every offset spelling of a request
// shares one idempotency key and one stored context.
func (r Request) normalized() Request {
	if start, ok := parseUTC(r.WindowStart); ok {
		r.WindowStart = start.Format(time.RFC3339Nano)
	}
	if end, ok := parseUTC(r.WindowEnd); ok {
		r.WindowEnd = end.Format(time.RFC3339Nano)
	}
	return r
}

// Validate checks IDs and a single canonical, epoch-aligned 15-minute UTC window.
func (r Request) Validate() error {
	if !validID(r.OrderID) || !validID(r.AssetID) || !validID(r.ProcessSnapshotID) {
		return ErrInvalid
	}
	start, okStart := parseUTC(r.WindowStart)
	end, okEnd := parseUTC(r.WindowEnd)
	if !okStart || !okEnd || start.Nanosecond() != 0 || start.Unix()%900 != 0 || end.Sub(start) != 15*time.Minute {
		return ErrInvalid
	}
	return nil
}

func (s *Service) begin(ctx context.Context, a Actor, sc Scope, write bool) (*sql.Tx, time.Time, error) {
	if !validID(sc.TenantID) || !validID(sc.SiteID) || !validText(a.Issuer, 255) || !validText(a.Subject, 255) || !validText(a.SessionHash, 128) {
		return nil, time.Time{}, ErrInvalid
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, time.Time{}, e
	}
	fail := func(err error) (*sql.Tx, time.Time, error) { _ = tx.Rollback(); return nil, time.Time{}, err }
	if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true),set_config('lock_timeout','5s',true)`, sc.TenantID); e != nil {
		return fail(e)
	}
	var issuer, subject string
	var expiry, seen, now time.Time
	e = tx.QueryRowContext(ctx, `SELECT issuer,subject,expires_at,last_seen_at,clock_timestamp() FROM supervisor.browser_sessions WHERE token_hash=$1 FOR SHARE`, a.SessionHash).Scan(&issuer, &subject, &expiry, &seen, &now)
	if e != nil || issuer != a.Issuer || subject != a.Subject || !expiry.After(now) || !seen.After(now.Add(-s.sessions.Idle)) {
		return fail(ErrDenied)
	}
	// Creation takes the site write lock, so intake, plan reviews and source changes
	// serialize against it and every input below is read from one consistent state.
	var found bool
	if e = tx.QueryRowContext(ctx, `SELECT supervisor.lock_member_site($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, write).Scan(&found); e != nil {
		return fail(e)
	}
	if !found {
		return fail(ErrNotFound)
	}
	var role string
	e = tx.QueryRowContext(ctx, `SELECT role FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND active FOR SHARE`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject).Scan(&role)
	if errors.Is(e, sql.ErrNoRows) {
		return fail(ErrNotFound)
	}
	if e != nil {
		return fail(e)
	}
	if write && role != "owner" && role != "supervisor" {
		return fail(ErrDenied)
	}
	return tx, now.UTC(), nil
}

func (s *Service) finish(ctx context.Context, tx *sql.Tx, a Actor) error {
	var ok bool
	if e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.browser_sessions WHERE token_hash=$1 AND issuer=$2 AND subject=$3 AND expires_at>clock_timestamp() AND last_seen_at>clock_timestamp()-make_interval(secs=>$4))`, a.SessionHash, a.Issuer, a.Subject, s.sessions.IdleSeconds()).Scan(&ok); e != nil {
		return e
	}
	if !ok {
		return ErrDenied
	}
	return tx.Commit()
}

func audit(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, contextID, event string) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO supervisor.plant_context_audit(tenant_id,site_id,actor_issuer,actor_subject,context_id,event_type) VALUES($1,$2,$3,$4,$5,$6)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, contextID, event)
	return e
}

func requestHash(r Request) string {
	b, _ := json.Marshal(struct {
		Schema        string
		SchemaVersion int
		Policy        int
		Request       Request
	}{SchemaName, SchemaVersion, ReadinessPolicyRevision, r})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Create assembles a context from the authoritative state now. An identical result
// (same digest) for the same scope reuses the current context; a different one is a new
// immutable context that supersedes it and notifies dependents through the outbox.
func (s *Service) Create(ctx context.Context, a Actor, sc Scope, r Request, key string) (View, error) {
	r = r.normalized()
	if r.Validate() != nil || !validText(key, 128) {
		return View{}, ErrInvalid
	}
	tx, now, err := s.begin(ctx, a, sc, true)
	if err != nil {
		return View{}, err
	}
	defer tx.Rollback()
	hash := requestHash(r)
	var storedHash, storedID string
	e := tx.QueryRowContext(ctx, `SELECT request_sha256,context_id FROM supervisor.plant_context_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&storedHash, &storedID)
	if e == nil {
		if storedHash != hash {
			return View{}, ErrConflict
		}
		view, err := load(ctx, tx, sc, storedID, now)
		if err != nil {
			return View{}, err
		}
		view.Replayed = true
		if err = audit(ctx, tx, a, sc, storedID, "mutation.replayed"); err != nil {
			return View{}, err
		}
		return view, s.finish(ctx, tx, a)
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return View{}, e
	}
	in, err := resolve(ctx, tx, sc, r, now)
	if err != nil {
		return View{}, err
	}
	snapshot, err := Assemble(sc.TenantID, sc.SiteID, in)
	if err != nil {
		return View{}, ErrInvalid
	}
	var headID string
	var headRevision int64
	e = tx.QueryRowContext(ctx, `SELECT context_id,revision FROM supervisor.plant_context_heads WHERE tenant_id=$1 AND site_id=$2 AND order_id=$3 AND asset_id=$4 AND window_start=$5 AND process_snapshot_id=$6 FOR UPDATE`, sc.TenantID, sc.SiteID, r.OrderID, r.AssetID, r.WindowStart, r.ProcessSnapshotID).Scan(&headID, &headRevision)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return View{}, e
	}
	if e == nil {
		var headDigest string
		if e = tx.QueryRowContext(ctx, `SELECT digest FROM supervisor.plant_contexts WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, headID).Scan(&headDigest); e != nil {
			return View{}, e
		}
		if headDigest == snapshot.Digest {
			// Repeated requests against identical revisions yield the same context.
			if e = audit(ctx, tx, a, sc, headID, "context.reused"); e != nil {
				return View{}, e
			}
			if e = saveReceipt(ctx, tx, a, sc, key, hash, headID); e != nil {
				return View{}, e
			}
			view, err := load(ctx, tx, sc, headID, now)
			if err != nil {
				return View{}, err
			}
			view.Reused = true
			return view, s.finish(ctx, tx, a)
		}
		snapshot.SupersedesContextID = &headID
	}
	snapshot.ContextID = uuid.NewString()
	snapshot.CreatedAt = now
	body, err := json.Marshal(snapshot)
	if err != nil {
		return View{}, err
	}
	var planRevisionID, approvalID any
	if snapshot.ApprovedPlanRevision != nil {
		planRevisionID, approvalID = snapshot.ApprovedPlanRevision.PlanRevisionID, snapshot.ApprovedPlanRevision.ApprovalID
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO supervisor.plant_contexts(tenant_id,site_id,id,order_id,asset_id,window_start,window_end,process_snapshot_id,process_snapshot_sha256,plan_revision_id,plan_approval_id,readiness,snapshot,digest,supersedes_context_id,actor_issuer,actor_subject,evaluated_at,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13::jsonb,$14,$15,$16,$17,$18,$18)`, sc.TenantID, sc.SiteID, snapshot.ContextID, r.OrderID, r.AssetID, r.WindowStart, r.WindowEnd, r.ProcessSnapshotID, snapshot.ProcessSnapshotSHA256, planRevisionID, approvalID, snapshot.Readiness, string(body), snapshot.Digest, snapshot.SupersedesContextID, a.Issuer, a.Subject, now); e != nil {
		return View{}, e
	}
	for _, ref := range snapshot.SourceRecordRefs {
		if _, e = tx.ExecContext(ctx, `INSERT INTO supervisor.plant_context_records(tenant_id,site_id,context_id,source_id,source_sequence) VALUES($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, snapshot.ContextID, ref.SourceID, ref.SourceSequence); e != nil {
			return View{}, e
		}
	}
	if headID == "" {
		_, e = tx.ExecContext(ctx, `INSERT INTO supervisor.plant_context_heads(tenant_id,site_id,order_id,asset_id,window_start,process_snapshot_id,context_id,revision) VALUES($1,$2,$3,$4,$5,$6,$7,1)`, sc.TenantID, sc.SiteID, r.OrderID, r.AssetID, r.WindowStart, r.ProcessSnapshotID, snapshot.ContextID)
	} else {
		_, e = tx.ExecContext(ctx, `UPDATE supervisor.plant_context_heads SET context_id=$7,revision=$8 WHERE tenant_id=$1 AND site_id=$2 AND order_id=$3 AND asset_id=$4 AND window_start=$5 AND process_snapshot_id=$6`, sc.TenantID, sc.SiteID, r.OrderID, r.AssetID, r.WindowStart, r.ProcessSnapshotID, snapshot.ContextID, headRevision+1)
		if e == nil {
			_, e = tx.ExecContext(ctx, `INSERT INTO supervisor.plant_context_outbox(tenant_id,site_id,context_id,event_type) VALUES($1,$2,$3,'context.superseded')`, sc.TenantID, sc.SiteID, headID)
		}
	}
	if e != nil {
		return View{}, e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO supervisor.plant_context_outbox(tenant_id,site_id,context_id,event_type) VALUES($1,$2,$3,'context.created')`, sc.TenantID, sc.SiteID, snapshot.ContextID); e != nil {
		return View{}, e
	}
	if e = audit(ctx, tx, a, sc, snapshot.ContextID, "context.created"); e != nil {
		return View{}, e
	}
	if e = saveReceipt(ctx, tx, a, sc, key, hash, snapshot.ContextID); e != nil {
		return View{}, e
	}
	view, err := load(ctx, tx, sc, snapshot.ContextID, now)
	if err != nil {
		return View{}, err
	}
	return view, s.finish(ctx, tx, a)
}

// AssessmentReason names an AssessTx error that will recur for the same window whatever time it is
// retried (too many records, an unknown order or asset, failed integrity, an invalid window) with a
// stable reason code. A transient error (database, timeout) returns "" and is retried.
func AssessmentReason(err error) string {
	switch {
	case errors.Is(err, ErrWindowTooLarge):
		return "WINDOW_TOO_LARGE"
	case errors.Is(err, ErrUnknownEntity):
		return "UNKNOWN_ENTITY"
	case errors.Is(err, errIntegrity):
		return "INTEGRITY_FAILED"
	case errors.Is(err, ErrInvalid):
		return "INVALID_WINDOW"
	}
	return ""
}

// AssessTx assembles exactly the snapshot Create would build for r at now, inside the caller's
// tenant transaction, without persisting it: no context row, head, outbox event or receipt is
// written, so assessing a window never opens or joins a case. Its digest is the one a created
// context would carry for the same revisions (WIS-277 outcome observation).
func AssessTx(ctx context.Context, tx *sql.Tx, sc Scope, r Request, now time.Time) (Snapshot, error) {
	r = r.normalized()
	if r.Validate() != nil {
		return Snapshot{}, ErrInvalid
	}
	in, err := resolve(ctx, tx, sc, r, now)
	if err != nil {
		return Snapshot{}, err
	}
	return Assemble(sc.TenantID, sc.SiteID, in)
}

func saveReceipt(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, key, hash, contextID string) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO supervisor.plant_context_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_sha256,context_id) VALUES($1,$2,$3,$4,$5,$6,$7)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, hash, contextID)
	return e
}

// Get returns the stored snapshot and its current eligibility.
func (s *Service) Get(ctx context.Context, a Actor, sc Scope, id string) (View, error) {
	if !validID(id) {
		return View{}, ErrNotFound
	}
	tx, now, err := s.begin(ctx, a, sc, false)
	if err != nil {
		return View{}, err
	}
	defer tx.Rollback()
	view, err := load(ctx, tx, sc, id, now)
	if err != nil {
		return View{}, err
	}
	if err = audit(ctx, tx, a, sc, id, "context.read"); err != nil {
		return View{}, err
	}
	return view, s.finish(ctx, tx, a)
}

// LoadTx reads a stored context and re-evaluates its current status inside the caller's
// tenant transaction. Case triggers and resume (WIS-273) bind only contexts that are
// eligible now, judged by exactly the rules a context read applies.
func LoadTx(ctx context.Context, tx *sql.Tx, sc Scope, id string, now time.Time) (View, error) {
	return load(ctx, tx, sc, id, now)
}

// HeadTx returns the current context ID for an order, asset, window start and process
// snapshot, or "" when there is none.
func HeadTx(ctx context.Context, tx *sql.Tx, sc Scope, orderID, assetID, windowStart, processSnapshotID string) (string, error) {
	var head string
	e := tx.QueryRowContext(ctx, `SELECT context_id FROM supervisor.plant_context_heads WHERE tenant_id=$1 AND site_id=$2 AND order_id=$3 AND asset_id=$4 AND window_start=$5 AND process_snapshot_id=$6`, sc.TenantID, sc.SiteID, orderID, assetID, windowStart, processSnapshotID).Scan(&head)
	if errors.Is(e, sql.ErrNoRows) {
		return "", nil
	}
	return head, e
}

func load(ctx context.Context, tx *sql.Tx, sc Scope, id string, now time.Time) (View, error) {
	var body []byte
	var digest string
	e := tx.QueryRowContext(ctx, `SELECT snapshot,digest FROM supervisor.plant_contexts WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, id).Scan(&body, &digest)
	if errors.Is(e, sql.ErrNoRows) {
		return View{}, ErrNotFound
	}
	if e != nil {
		return View{}, e
	}
	var view View
	if err := json.Unmarshal(body, &view.Snapshot); err != nil || view.Digest != digest || view.ContextID != id {
		return View{}, errIntegrity
	}
	status, err := currentStatus(ctx, tx, sc, view.Snapshot, now)
	if err != nil {
		return View{}, err
	}
	view.CurrentStatus = status
	return view, nil
}

// currentStatus checks, now, whether the stored context may still be relied on:
// it is still the scope's current context, its plan approval is still the eligible one,
// every contributing source is still approved, and no later record changed its window.
func currentStatus(ctx context.Context, tx *sql.Tx, sc Scope, snap Snapshot, now time.Time) (CurrentStatus, error) {
	status := CurrentStatus{Reasons: []string{}, EvaluatedAt: now}
	why := reasons{}
	var head string
	e := tx.QueryRowContext(ctx, `SELECT context_id FROM supervisor.plant_context_heads WHERE tenant_id=$1 AND site_id=$2 AND order_id=$3 AND asset_id=$4 AND window_start=$5 AND process_snapshot_id=$6`, sc.TenantID, sc.SiteID, snap.OrderID, snap.AssetID, snap.Window.Start, snap.ProcessSnapshotID).Scan(&head)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return status, e
	}
	if head != snap.ContextID {
		why["SUPERSEDED"] = true
		if head != "" {
			status.SupersededBy = &head
		}
	}
	plan, _, err := resolvePlan(ctx, tx, sc, snap.OrderID, snap.AssetID, snap.Window)
	if err != nil {
		return status, err
	}
	switch {
	case snap.ApprovedPlanRevision == nil && plan != nil:
		why["PLAN_CHANGED"] = true
	case snap.ApprovedPlanRevision != nil && (plan == nil || plan.Ref != *snap.ApprovedPlanRevision):
		why["PLAN_NO_LONGER_ELIGIBLE"] = true
	}
	seen := map[string]bool{}
	for _, ref := range snap.SourcePolicyRevisionIDs {
		if seen[ref.SourceID] {
			continue
		}
		seen[ref.SourceID] = true
		source, err := resolveSource(ctx, tx, sc, ref.SourceID)
		if err != nil {
			return status, err
		}
		if !source.Approved || source.SourceRevision != ref.SourceRevision || source.AccessPolicyRevision != ref.AccessPolicyRevision {
			why["SOURCE_NO_LONGER_APPROVED"] = true
		}
	}
	for _, ref := range snap.MappingRevisionIDs {
		source, err := resolveSource(ctx, tx, sc, ref.SourceID)
		if err != nil {
			return status, err
		}
		if source.MappingRevision != ref.MappingRevision {
			why["SOURCE_NO_LONGER_APPROVED"] = true
		}
	}
	// Late arrivals, corrections and quarantines change what selection would use now.
	// Re-run the same resolution and assembly and compare the selected delta records,
	// so a correction that moved a fact out of this window is not a change here.
	current, err := resolve(ctx, tx, sc, Request{OrderID: snap.OrderID, AssetID: snap.AssetID, WindowStart: snap.Window.Start, WindowEnd: snap.Window.End, ProcessSnapshotID: snap.ProcessSnapshotID}, now)
	switch {
	// An input that can no longer be resolved (too many records, or an order or asset that is no
	// longer a known entity) makes the stored context unverifiable now, not unreadable.
	case errors.Is(err, ErrWindowTooLarge), errors.Is(err, ErrUnknownEntity):
		why["INPUTS_UNVERIFIABLE"] = true
	case err != nil:
		return status, err
	default:
		recomputed, err := Assemble(sc.TenantID, sc.SiteID, current)
		if err != nil {
			return status, errIntegrity
		}
		if !sameRefs(recomputed.DeltaRefs(), snap.DeltaRefs()) || (recomputed.ApprovedPlanRevision == nil) != (snap.ApprovedPlanRevision == nil) {
			why["INPUTS_CHANGED"] = true
		}
		for _, code := range recomputed.ReasonCodes {
			if code == "CORRECTION_QUARANTINED" || code == "SOURCE_CONFLICT" {
				why["INPUTS_CHANGED"] = true
			}
		}
	}
	status.Reasons = why.list()
	status.Eligible = len(why) == 0 && snap.Readiness != NotReady
	if snap.Readiness == NotReady {
		status.Reasons = append(status.Reasons, "NOT_READY")
		sort.Strings(status.Reasons)
	}
	return status, nil
}

// sameRefs compares the records two selections cite by durable identity and content.
// RecordID is derived from that identity, so a context stored before record_id existed (with
// an empty RecordID) still matches the same records selected today.
func sameRefs(a, b []RecordRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		x.RecordID, y.RecordID = "", ""
		if x != y {
			return false
		}
	}
	return true
}

// resolve reads every input for one context from the authoritative tables.
func resolve(ctx context.Context, tx *sql.Tx, sc Scope, r Request, now time.Time) (Inputs, error) {
	in := Inputs{OrderID: r.OrderID, AssetID: r.AssetID, Window: Window{r.WindowStart, r.WindowEnd}, ProcessSnapshotID: r.ProcessSnapshotID, Sources: map[string]SourceInput{}, EvaluatedAt: now}
	// A slice, not a map: an order and asset given the same ID must check both types.
	for _, check := range [][2]string{{r.OrderID, "order"}, {r.AssetID, "asset"}} {
		id, want := check[0], check[1]
		var kind string
		e := tx.QueryRowContext(ctx, `SELECT e.entity_type FROM supervisor.knowledge_entities e JOIN supervisor.knowledge_entity_heads h ON (h.tenant_id,h.site_id,h.id)=(e.tenant_id,e.site_id,e.id) WHERE e.tenant_id=$1 AND e.site_id=$2 AND e.id=$3`, sc.TenantID, sc.SiteID, id).Scan(&kind)
		if errors.Is(e, sql.ErrNoRows) || (e == nil && kind != want) {
			return in, ErrUnknownEntity
		}
		if e != nil {
			return in, e
		}
	}
	e := tx.QueryRowContext(ctx, `SELECT sha256 FROM supervisor.knowledge_process_snapshots WHERE tenant_id=$1 AND site_id=$2 AND snapshot_id=$3`, sc.TenantID, sc.SiteID, r.ProcessSnapshotID).Scan(&in.ProcessSnapshotSHA256)
	if errors.Is(e, sql.ErrNoRows) {
		return in, ErrUnknownSnapshot
	}
	if e != nil {
		return in, e
	}
	plan, reason, err := resolvePlan(ctx, tx, sc, r.OrderID, r.AssetID, in.Window)
	if err != nil {
		return in, err
	}
	in.Plan, in.PlanReason = plan, reason
	if in.Deltas, err = deltaRecords(ctx, tx, sc, r.OrderID, r.AssetID, in.Window); err != nil {
		return in, err
	}
	for _, record := range in.Deltas {
		if _, ok := in.Sources[record.Ref.SourceID]; ok {
			continue
		}
		source, err := resolveSource(ctx, tx, sc, record.Ref.SourceID)
		if err != nil {
			return in, err
		}
		in.Sources[record.Ref.SourceID] = source
	}
	return in, nil
}

// resolvePlan selects the exact approved plan for the order and asset: the latest
// captured revision, whose current decision approves it and whose cited evidence is
// unrevoked. A newer unapproved revision contests the plan, so nothing older is used.
// resolvePlan selects the plan revision that governs one context window: the highest
// revision for the order and asset whose window overlaps this one and whose current review
// is not a rejection. A newer revision for another shift does not displace the plan for
// this window, and a rejected revision contests nothing. A newer revision that is still
// pending does contest (the plan is being changed), so the window is PLAN_NOT_APPROVED
// until it is decided. If only rejected revisions overlap, the window is PLAN_NOT_APPROVED;
// with no overlapping revision the latest other one is returned so the snapshot reports
// PLAN_NOT_COVERING_WINDOW.
func resolvePlan(ctx context.Context, tx *sql.Tx, sc Scope, orderID, assetID string, w Window) (*PlanInput, string, error) {
	var plan PlanInput
	var payload []byte
	e := tx.QueryRowContext(ctx, `SELECT p.id,p.plan_revision,p.payload,p.canonical_payload_sha256 FROM supervisor.plan_revisions p
 LEFT JOIN supervisor.plan_review_heads h ON (h.tenant_id,h.site_id,h.plan_revision_id)=(p.tenant_id,p.site_id,p.id)
 LEFT JOIN supervisor.plan_approvals a ON (a.tenant_id,a.site_id,a.plan_revision_id,a.review_revision)=(h.tenant_id,h.site_id,h.plan_revision_id,h.current_revision)
 WHERE p.tenant_id=$1 AND p.site_id=$2 AND p.order_id=$3 AND p.asset_id=$4
 ORDER BY (p.window_start<$6::timestamptz AND p.window_end>$5::timestamptz) DESC, (a.decision IS DISTINCT FROM 'reject') DESC, p.plan_revision DESC LIMIT 1`, sc.TenantID, sc.SiteID, orderID, assetID, w.Start, w.End).Scan(&plan.Ref.PlanRevisionID, &plan.Ref.PlanRevision, &payload, &plan.Ref.CanonicalPayloadDigest)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, "NO_APPROVED_PLAN", nil
	}
	if e != nil {
		return nil, "", e
	}
	var decision string
	e = tx.QueryRowContext(ctx, `SELECT a.id,a.decision FROM supervisor.plan_review_heads h JOIN supervisor.plan_approvals a ON (a.tenant_id,a.site_id,a.plan_revision_id,a.review_revision)=(h.tenant_id,h.site_id,h.plan_revision_id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.plan_revision_id=$3`, sc.TenantID, sc.SiteID, plan.Ref.PlanRevisionID).Scan(&plan.Ref.ApprovalID, &decision)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, "PLAN_NOT_APPROVED", nil
	}
	if e != nil {
		return nil, "", e
	}
	if decision != "approve" {
		return nil, "PLAN_NOT_APPROVED", nil
	}
	var revoked bool
	if e = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.plan_approval_spans s JOIN supervisor.evidence_spans es ON (es.tenant_id,es.site_id,es.id)=(s.tenant_id,s.site_id,s.span_id) JOIN supervisor.evidence_heads eh ON (eh.tenant_id,eh.site_id,eh.source_id)=(es.tenant_id,es.site_id,es.source_id) WHERE s.tenant_id=$1 AND s.site_id=$2 AND s.approval_id=$3 AND (eh.revoked OR EXISTS(SELECT 1 FROM supervisor.evidence_revocations v WHERE (v.tenant_id,v.site_id,v.source_id)=(eh.tenant_id,eh.site_id,eh.source_id))))`, sc.TenantID, sc.SiteID, plan.Ref.ApprovalID).Scan(&revoked); e != nil {
		return nil, "", e
	}
	if revoked {
		return nil, "PLAN_PROVENANCE_REVOKED", nil
	}
	body, err := production.DecodePlanBody(payload)
	if err != nil {
		return nil, "", errIntegrity
	}
	if _, digest, err := production.CanonicalPlanPayload(sc.TenantID, sc.SiteID, body); err != nil || digest != plan.Ref.CanonicalPayloadDigest {
		return nil, "", errIntegrity
	}
	plan.Body = body
	return &plan, "", nil
}

// deltaRecords returns every revision of every production_delta chain that names this
// order and asset in any revision and whose interval intersects the window (or is
// unparseable, which then fails closed as INVALID_DELTA). Quarantined revisions are
// included (except DUPLICATE_EVENT retransmits) so a quarantined correction blocks its
// chain instead of silently reviving the revision it replaced. It fails with
// ErrWindowTooLarge rather than silently truncating totals.
func deltaRecords(ctx context.Context, tx *sql.Tx, sc Scope, orderID, assetID string, w Window) ([]DeltaRecord, error) {
	// Two linear steps, never a per-chain probe. A join (or LATERAL) from candidate chains
	// to their revisions lets stale statistics pick a per-chain scan of the whole source:
	// measured 5-16s for 10k records right after a bulk load. Here the worst case for
	// any plan is one pass per step.
	chains, err := tx.QueryContext(ctx, `SELECT DISTINCT source_id,external_key FROM supervisor.operational_event_records
WHERE tenant_id=$1 AND site_id=$2 AND event_kind='production_delta' AND quarantine_reason IS DISTINCT FROM 'DUPLICATE_EVENT'
  AND payload->>'order_id'=$3 AND payload->>'asset_id'=$4
  AND (supervisor.try_utc_timestamp(payload->>'interval_start') IS NULL OR supervisor.try_utc_timestamp(payload->>'interval_end') IS NULL
    OR (supervisor.try_utc_timestamp(payload->>'interval_start')<$6::timestamptz AND supervisor.try_utc_timestamp(payload->>'interval_end')>$5::timestamptz))
LIMIT $7`, sc.TenantID, sc.SiteID, orderID, assetID, w.Start, w.End, MaxRecords+1)
	if err != nil {
		return nil, err
	}
	keys := map[string][]string{}
	count := 0
	for chains.Next() {
		var source, key string
		if err = chains.Scan(&source, &key); err != nil {
			chains.Close()
			return nil, err
		}
		keys[source] = append(keys[source], key)
		count++
	}
	if err = chains.Err(); err != nil {
		chains.Close()
		return nil, err
	}
	chains.Close()
	if count > MaxRecords {
		return nil, ErrWindowTooLarge
	}
	sources := make([]string, 0, len(keys))
	for source := range keys {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	out := make([]DeltaRecord, 0, count)
	for _, source := range sources {
		rows, err := tx.QueryContext(ctx, `SELECT source_sequence,external_key,revision,event_sha256,payload,coalesce(asset_entity_id::text,''),coalesce(order_entity_id::text,''),coalesce(quarantine_reason,'')
FROM supervisor.operational_event_records
WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND external_key=ANY($4::text[]) AND event_kind='production_delta' AND quarantine_reason IS DISTINCT FROM 'DUPLICATE_EVENT'
ORDER BY source_sequence LIMIT $5`, sc.TenantID, sc.SiteID, source, keys[source], MaxRecords+1-len(out))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			record := DeltaRecord{Ref: RecordRef{SourceID: source, Kind: "production_delta"}}
			if err = rows.Scan(&record.Ref.SourceSequence, &record.Ref.ExternalKey, &record.Ref.Revision, &record.Ref.SHA256, &record.Payload, &record.AssetEntityID, &record.OrderEntityID, &record.Quarantine); err != nil {
				rows.Close()
				return nil, err
			}
			record.Ref.RecordID = recordID(sc, source, record.Ref.SourceSequence)
			out = append(out, record)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		if len(out) > MaxRecords {
			return nil, ErrWindowTooLarge
		}
	}
	return out, nil
}

// resolveSource reads a source's current revision, review eligibility, reviewed scope
// and folded progress, including the watermark record that progress points at.
// SourcesLiveTx reports whether every listed operational source heartbeated within its configured
// freshness (bounded by limits.MaxHeartbeatAge, as at snapshot time) at now. A source that is unknown, never
// processed or never heartbeated is not live. Dispatch uses it so that unchanged revisions are not
// taken as proof that the sources behind them are still current (WIS-277 review M4).
func SourcesLiveTx(ctx context.Context, tx *sql.Tx, sc Scope, sourceIDs []string, now time.Time) (bool, error) {
	for _, id := range sourceIDs {
		source, err := resolveSource(ctx, tx, sc, id)
		if err != nil {
			return false, err
		}
		p := source.Progress
		if p == nil || p.LastHeartbeatReceivedAt == nil || p.LastHeartbeatReceivedAt.Before(now.Add(-limits.HeartbeatLimit(source.FreshnessSeconds))) {
			return false, nil
		}
	}
	return true, nil
}

func resolveSource(ctx context.Context, tx *sql.Tx, sc Scope, sourceID string) (SourceInput, error) {
	source := SourceInput{SourceID: sourceID}
	var enabled bool
	var scope, configHash, mapHash string
	var reviewSource, reviewMapping sql.NullInt64
	var reviewConfig, reviewMapHash, verdict sql.NullString
	var mappingHead int64
	e := tx.QueryRowContext(ctx, `SELECT sr.revision,sr.mapping_revision,sr.access_policy_revision,sr.enabled,sr.authority_scope,sr.freshness_seconds,sr.config_sha256,sr.mapping_sha256,mh.current_revision,rv.source_revision,rv.mapping_revision,rv.config_sha256,rv.mapping_sha256,rv.verdict
FROM supervisor.operational_source_heads sh
JOIN supervisor.operational_source_revisions sr ON (sr.tenant_id,sr.site_id,sr.source_id,sr.revision)=(sh.tenant_id,sh.site_id,sh.source_id,sh.current_revision)
JOIN supervisor.operational_source_mapping_heads mh ON (mh.tenant_id,mh.site_id,mh.source_id)=(sh.tenant_id,sh.site_id,sh.source_id)
LEFT JOIN supervisor.operational_source_review_heads rh ON (rh.tenant_id,rh.site_id,rh.source_id)=(sh.tenant_id,sh.site_id,sh.source_id)
LEFT JOIN supervisor.operational_source_reviews rv ON (rv.tenant_id,rv.site_id,rv.source_id,rv.revision)=(rh.tenant_id,rh.site_id,rh.source_id,rh.current_revision)
WHERE sh.tenant_id=$1 AND sh.site_id=$2 AND sh.source_id=$3`, sc.TenantID, sc.SiteID, sourceID).Scan(&source.SourceRevision, &source.MappingRevision, &source.AccessPolicyRevision, &enabled, &scope, &source.FreshnessSeconds, &configHash, &mapHash, &mappingHead, &reviewSource, &reviewMapping, &reviewConfig, &reviewMapHash, &verdict)
	if errors.Is(e, sql.ErrNoRows) {
		return source, nil
	}
	if e != nil {
		return source, e
	}
	var stale int
	if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.operational_source_mappings m JOIN supervisor.knowledge_entity_heads h ON (h.tenant_id,h.site_id,h.id)=(m.tenant_id,m.site_id,m.entity_id) WHERE m.tenant_id=$1 AND m.site_id=$2 AND m.source_id=$3 AND m.mapping_revision=$4 AND h.current_revision<>m.entity_revision`, sc.TenantID, sc.SiteID, sourceID, source.MappingRevision).Scan(&stale); e != nil {
		return source, e
	}
	source.Approved = enabled && stale == 0 && mappingHead == source.MappingRevision && verdict.String == "approved" &&
		reviewSource.Int64 == source.SourceRevision && reviewMapping.Int64 == source.MappingRevision &&
		reviewConfig.String == configHash && reviewMapHash.String == mapHash
	source.ScopeAllowsDeltas = scopeAllowsKind(scope, "production_delta")
	source.ScopeAllowsMachineState = scopeAllowsKind(scope, "machine_state")
	var progress ProgressInput
	var watermarkSequence sql.NullInt64
	e = tx.QueryRowContext(ctx, `SELECT r.complete_sequence,r.clock_quality,r.last_heartbeat_received_at,r.watermark_sequence FROM supervisor.operational_source_progress_heads h JOIN supervisor.operational_source_progress_revisions r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=(h.tenant_id,h.site_id,h.source_id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.source_id=$3`, sc.TenantID, sc.SiteID, sourceID).Scan(&progress.CompleteSequence, &progress.ClockQuality, &progress.LastHeartbeatReceivedAt, &watermarkSequence)
	if errors.Is(e, sql.ErrNoRows) {
		return source, nil
	}
	if e != nil {
		return source, e
	}
	if watermarkSequence.Valid {
		mark := WatermarkInput{Ref: RecordRef{RecordID: recordID(sc, sourceID, watermarkSequence.Int64), SourceID: sourceID, SourceSequence: watermarkSequence.Int64, Kind: "watermark"}}
		if e = tx.QueryRowContext(ctx, `SELECT external_key,revision,event_sha256,payload FROM supervisor.operational_event_records WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND source_sequence=$4 AND event_kind='watermark'`, sc.TenantID, sc.SiteID, sourceID, watermarkSequence.Int64).Scan(&mark.Ref.ExternalKey, &mark.Ref.Revision, &mark.Ref.SHA256, &mark.Payload); e != nil {
			return source, e
		}
		progress.Watermark = &mark
	}
	source.Progress = &progress
	return source, nil
}

// scopeAllowsKind mirrors intake: a reviewed scope is a list of "kind" or
// "kind:qualifier" tokens separated by commas or spaces.
func scopeAllowsKind(scope, kind string) bool {
	for _, token := range strings.FieldsFunc(scope, func(r rune) bool { return r == ',' || r == ' ' }) {
		if name, _, _ := strings.Cut(token, ":"); name == kind {
			return true
		}
	}
	return false
}
