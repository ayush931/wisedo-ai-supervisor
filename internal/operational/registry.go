// Package operational owns the reviewed registry for operational data inputs.
// It is separate from knowledge evidence sources and never stores credentials.
package operational

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
)

var (
	ErrInvalid          = errors.New("invalid operational source contract")
	ErrNotFound         = errors.New("operational source unavailable")
	ErrDenied           = errors.New("operational source permission denied")
	ErrConflict         = errors.New("operational source idempotency conflict")
	ErrRevision         = errors.New("operational source revision changed")
	ErrReauthentication = errors.New("recent authentication required")
	// ErrAccessPolicyRegression rejects a revision that lowers the access-policy revision:
	// contexts built under an older revision would become eligible again (WIS-272 revocation).
	ErrAccessPolicyRegression = errors.New("operational source access-policy revision cannot decrease")
)

type Actor struct{ Issuer, Subject, SessionHash string }
type Scope struct{ TenantID, SiteID string }
type Config struct {
	SourceID             string `json:"source_id"`
	Type                 string `json:"type"`
	AuthorityScope       string `json:"authority_scope"`
	CredentialRef        string `json:"credential_ref,omitempty"`
	Enabled              bool   `json:"enabled"`
	AccessPolicyRevision int64  `json:"access_policy_revision"`
	FreshnessSeconds     int64  `json:"freshness_seconds"`
	ClockSkewSeconds     int64  `json:"clock_skew_seconds"`
}
type Mapping struct {
	ExternalType   string `json:"external_type"`
	ExternalKey    string `json:"external_key"`
	EntityID       string `json:"entity_id"`
	EntityRevision int64  `json:"entity_revision"`
}
type Source struct {
	Config
	Revision              int64     `json:"revision"`
	CurrentRevision       int64     `json:"current_revision"`
	MappingRevision       int64     `json:"mapping_revision"`
	MappingSHA256         string    `json:"mapping_sha256"`
	ConfigSHA256          string    `json:"config_sha256"`
	ActorIssuer           string    `json:"actor_issuer"`
	ActorSubject          string    `json:"actor_subject"`
	CreatedAt             time.Time `json:"created_at"`
	Review                *Review   `json:"review,omitempty"`
	ReviewEligible        bool      `json:"review_eligible"`
	MappingTargetsCurrent bool      `json:"mapping_targets_current"`
}
type ReviewInput struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}
type Review struct {
	DecisionID      string    `json:"decision_id"`
	Revision        int64     `json:"revision"`
	CurrentRevision int64     `json:"current_revision"`
	SourceRevision  int64     `json:"source_revision"`
	ConfigSHA256    string    `json:"config_sha256"`
	MappingRevision int64     `json:"mapping_revision"`
	MappingSHA256   string    `json:"mapping_sha256"`
	Verdict         string    `json:"verdict"`
	Reason          string    `json:"reason"`
	ActorIssuer     string    `json:"actor_issuer"`
	ActorSubject    string    `json:"actor_subject"`
	AuthenticatedAt time.Time `json:"authenticated_at"`
	CreatedAt       time.Time `json:"created_at"`
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

// MaxAccessPolicyRevision bounds access_policy_revision (WIS-720). A configuration may use values up
// to one below it, so a revocation, which adds one, always has room; an unbounded value let a source
// be stored at MaxInt64, where the revocation's increment overflowed and the source could never be
// revoked. 2^53-1 is also the largest integer every JSON client reads exactly.
//
// A source already at or above the bound (stored before it existed, or revoked from the bound
// minus one) stays manageable but can never be enabled again: a revision with enabled=false and
// the unchanged revision is accepted, and Revoke keeps the revision instead of adding one. Every
// other configuration at or above the bound is refused.
const MaxAccessPolicyRevision int64 = 1<<53 - 1

func validID(v string) bool {
	id, e := uuid.Parse(v)
	return e == nil && id != uuid.Nil && id.String() == v
}
func validText(v string, max int) bool {
	return len(v) > 0 && len(v) <= max && strings.TrimSpace(v) == v && !strings.ContainsRune(v, 0)
}
func validCredentialRef(v string) bool {
	if !validText(v, 512) {
		return false
	}
	u, err := url.Parse(v)
	if err != nil || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Host == "" || strings.Trim(u.Path, "/") == "" {
		return false
	}
	switch u.Scheme {
	case "vault", "aws-secretsmanager", "gcp-secretmanager", "azure-keyvault":
		return true
	default:
		return false
	}
}

// Validate checks a new configuration, including the access-policy bound.
func (c Config) Validate() error {
	if c.validShape() != nil || c.AccessPolicyRevision >= MaxAccessPolicyRevision {
		return ErrInvalid
	}
	return nil
}

// validShape is Validate without the upper bound, which mutateSource applies once it has
// replayed any receipt and read the stored revision (WIS-720).
func (c Config) validShape() error {
	if !validID(c.SourceID) || !(c.Type == "csv" || c.Type == "json" || c.Type == "event_api" || c.Type == "manual") ||
		!validText(c.AuthorityScope, 255) || (c.CredentialRef != "" && !validCredentialRef(c.CredentialRef)) ||
		c.AccessPolicyRevision < 1 || c.FreshnessSeconds < 1 || c.FreshnessSeconds > 31536000 || c.ClockSkewSeconds < 0 || c.ClockSkewSeconds > 86400 {
		return ErrInvalid
	}
	return nil
}
func (m Mapping) Validate() error {
	if m.invalidMember() != "" {
		return ErrInvalid
	}
	return nil
}

// RowError names the request member that caused Err (for example mappings[3].external_key).
// The path holds closed member names and an input index only, never a submitted value.
// errors.Is(err, ErrInvalid/ErrNotFound/ErrRevision) still matches the wrapped sentinel.
type RowError struct {
	Path string
	Err  error
}

func (e *RowError) Error() string { return e.Err.Error() + " at " + e.Path }
func (e *RowError) Unwrap() error { return e.Err }
func rowError(i int, member string, err error) error {
	return &RowError{Path: fmt.Sprintf("mappings[%d].%s", i, member), Err: err}
}

// invalidMember is the first member of m that fails Validate, or "".
func (m Mapping) invalidMember() string {
	switch {
	case m.ExternalType != "asset" && m.ExternalType != "order":
		return "external_type"
	case !validText(m.ExternalKey, 512):
		return "external_key"
	case !validID(m.EntityID):
		return "entity_id"
	case m.EntityRevision < 1:
		return "entity_revision"
	}
	return ""
}

// canonicalMappings validates and sorts a mapping set. A row failure is a *RowError whose
// index is the caller's input position (validation runs before sorting).
func canonicalMappings(in []Mapping) ([]Mapping, error) {
	if len(in) > 10000 {
		return nil, &RowError{Path: "mappings", Err: ErrInvalid}
	}
	out := make([]Mapping, len(in))
	copy(out, in)
	seenExternal, seenTarget := map[string]bool{}, map[string]bool{}
	for i, m := range out {
		if member := m.invalidMember(); member != "" {
			return nil, rowError(i, member, ErrInvalid)
		}
		k := m.ExternalType + "\x00" + m.ExternalKey
		t := m.ExternalType + "\x00" + m.EntityID + "\x00" + fmt.Sprint(m.EntityRevision)
		if seenExternal[k] {
			return nil, rowError(i, "external_key", ErrInvalid)
		}
		if seenTarget[t] {
			return nil, rowError(i, "entity_id", ErrInvalid)
		}
		seenExternal[k] = true
		seenTarget[t] = true
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ExternalType != out[j].ExternalType {
			return out[i].ExternalType < out[j].ExternalType
		}
		return out[i].ExternalKey < out[j].ExternalKey
	})
	return out, nil
}
func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func configDigest(c Config) string      { return digest(c) }
func mappingDigest(ms []Mapping) string { return digest(ms) }

func (s *Service) begin(ctx context.Context, a Actor, sc Scope, write bool, reviewer bool) (*sql.Tx, time.Time, error) {
	if !validID(sc.TenantID) || !validID(sc.SiteID) || a.Issuer == "" || a.Subject == "" {
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
	if e != nil {
		return fail(ErrDenied)
	}
	if issuer != a.Issuer || subject != a.Subject || !expiry.After(now) || !seen.After(now.Add(-s.sessions.Idle)) {
		return fail(ErrDenied)
	}
	var found bool
	if e = tx.QueryRowContext(ctx, `SELECT supervisor.lock_member_site($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, write).Scan(&found); e != nil {
		return fail(e)
	}
	if !found {
		return fail(ErrNotFound)
	}
	var role string
	e = tx.QueryRowContext(ctx, `SELECT role FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND active FOR SHARE`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject).Scan(&role)
	if e != nil {
		return fail(ErrNotFound)
	}
	if write && role != "owner" && role != "supervisor" {
		return fail(ErrDenied)
	}
	var auth time.Time
	e = tx.QueryRowContext(ctx, `SELECT auth_time FROM supervisor.browser_sessions WHERE token_hash=$1`, a.SessionHash).Scan(&auth)
	if e != nil {
		return fail(ErrDenied)
	}
	if reviewer && (auth.After(now) || auth.Before(now.Add(-s.sessions.RecentAuth))) {
		return fail(ErrReauthentication)
	}
	return tx, auth, nil
}
func (s *Service) finish(ctx context.Context, tx *sql.Tx, a Actor) error {
	var ok bool
	e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.browser_sessions WHERE token_hash=$1 AND issuer=$2 AND subject=$3 AND expires_at>clock_timestamp() AND last_seen_at>clock_timestamp()-make_interval(secs=>$4))`, a.SessionHash, a.Issuer, a.Subject, s.sessions.IdleSeconds()).Scan(&ok)
	if e != nil {
		return e
	}
	if !ok {
		return ErrDenied
	}
	return tx.Commit()
}

type auditRefs struct {
	source, mapping, review int64
	operation               string
}

func recordAudit(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, id string, revision int64, event string, refs auditRefs) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO supervisor.operational_source_audit(tenant_id,site_id,actor_issuer,actor_subject,source_id,revision,event_type,operation,source_revision,mapping_revision,review_revision) VALUES($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),NULLIF($9,0),NULLIF($10,0),NULLIF($11,0))`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, id, revision, event, refs.operation, refs.source, refs.mapping, refs.review)
	return e
}
func receipt(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, key, hash, op string) (string, int64, bool, error) {
	var stored, source, storedOp string
	var rev int64
	e := tx.QueryRowContext(ctx, `SELECT request_sha256,operation,source_id,revision FROM supervisor.operational_source_receipts WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&stored, &storedOp, &source, &rev)
	if errors.Is(e, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	if e != nil {
		return "", 0, false, e
	}
	if stored != hash || storedOp != op {
		return "", 0, false, ErrConflict
	}
	refs := auditRefs{operation: storedOp}
	if storedOp == "review" {
		refs.review = rev
	} else {
		refs.source = rev
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_source_audit(tenant_id,site_id,actor_issuer,actor_subject,source_id,revision,event_type,operation,source_revision,review_revision) VALUES($1,$2,$3,$4,$5,$6,'mutation.replayed',$7,NULLIF($8,0),NULLIF($9,0))`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, source, rev, storedOp, refs.source, refs.review); e != nil {
		return "", 0, false, e
	}
	return source, rev, true, nil
}
func saveReceipt(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, key, hash, op, id string, rev int64) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO supervisor.operational_source_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_sha256,operation,source_id,revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key, hash, op, id, rev)
	return e
}
func (s *Service) CreateOrRevise(ctx context.Context, a Actor, sc Scope, c Config, key string, expected int64) (Source, error) {
	var out Source
	if c.validShape() != nil || !validText(key, 128) || expected < 0 {
		return out, ErrInvalid
	}
	return s.mutateSource(ctx, a, sc, c, key, expected)
}
func (s *Service) mutateSource(ctx context.Context, a Actor, sc Scope, c Config, key string, expected int64) (Source, error) {
	var out Source
	tx, auth, e := s.begin(ctx, a, sc, true, true)
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	hash := digest(struct {
		Config   Config
		Expected int64
	}{c, expected})
	prior, rev, replayed, e := receipt(ctx, tx, a, sc, key, hash, "source")
	if e != nil {
		return out, e
	}
	if replayed {
		if e = loadSource(ctx, tx, sc, prior, rev, &out); e != nil {
			return out, e
		}
		return out, s.finish(ctx, tx, a)
	}
	var current int64
	e = tx.QueryRowContext(ctx, `SELECT current_revision FROM supervisor.operational_source_heads WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 FOR UPDATE`, sc.TenantID, sc.SiteID, c.SourceID).Scan(&current)
	exists := e == nil
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return out, e
	}
	if (exists && expected != current) || (!exists && expected != 0) {
		return out, ErrRevision
	}
	var previous int64
	if exists {
		if e = tx.QueryRowContext(ctx, `SELECT access_policy_revision FROM supervisor.operational_source_revisions WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND revision=$4`, sc.TenantID, sc.SiteID, c.SourceID, current).Scan(&previous); e != nil {
			return out, e
		}
		if c.AccessPolicyRevision < previous {
			return out, ErrAccessPolicyRegression
		}
	}
	// The bound applies after the receipt, so an identical retry of a request accepted before the
	// bound existed replays. At or above it only a disable that keeps the stored revision passes.
	if c.AccessPolicyRevision >= MaxAccessPolicyRevision && (c.Enabled || c.AccessPolicyRevision != previous) {
		return out, ErrInvalid
	}
	newRev := current + 1
	cfgHash := configDigest(c)
	if !exists {
		_, e = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_source_heads(tenant_id,site_id,source_id,current_revision) VALUES($1,$2,$3,1)`, sc.TenantID, sc.SiteID, c.SourceID)
		if e != nil {
			return out, e
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_source_mapping_heads(tenant_id,site_id,source_id,current_revision) VALUES($1,$2,$3,1)`, sc.TenantID, sc.SiteID, c.SourceID)
		if e != nil {
			return out, e
		}
		mapsHash := mappingDigest([]Mapping{})
		_, e = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_source_mapping_revisions(tenant_id,site_id,source_id,revision,mapping_sha256,actor_issuer,actor_subject) VALUES($1,$2,$3,1,$4,$5,$6)`, sc.TenantID, sc.SiteID, c.SourceID, mapsHash, a.Issuer, a.Subject)
		if e != nil {
			return out, e
		}
	} else {
		_, e = tx.ExecContext(ctx, `UPDATE supervisor.operational_source_heads SET current_revision=$4 WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, sc.TenantID, sc.SiteID, c.SourceID, newRev)
		if e != nil {
			return out, e
		}
	}
	var mapRev int64
	var mapHash string
	e = tx.QueryRowContext(ctx, `SELECT h.current_revision,r.mapping_sha256 FROM supervisor.operational_source_mapping_heads h JOIN supervisor.operational_source_mapping_revisions r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=(h.tenant_id,h.site_id,h.source_id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.source_id=$3`, sc.TenantID, sc.SiteID, c.SourceID).Scan(&mapRev, &mapHash)
	if e != nil {
		return out, e
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_source_revisions(tenant_id,site_id,source_id,revision,source_type,authority_scope,credential_ref,mapping_revision,mapping_sha256,enabled,access_policy_revision,freshness_seconds,clock_skew_seconds,config_sha256,actor_issuer,actor_subject,auth_time) VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`, sc.TenantID, sc.SiteID, c.SourceID, newRev, c.Type, c.AuthorityScope, c.CredentialRef, mapRev, mapHash, c.Enabled, c.AccessPolicyRevision, c.FreshnessSeconds, c.ClockSkewSeconds, cfgHash, a.Issuer, a.Subject, auth)
	if e != nil {
		return out, e
	}
	if e = recordAudit(ctx, tx, a, sc, c.SourceID, newRev, map[bool]string{true: "source.created", false: "source.revised"}[!exists], auditRefs{source: newRev}); e != nil {
		return out, e
	}
	if e = saveReceipt(ctx, tx, a, sc, key, hash, "source", c.SourceID, newRev); e != nil {
		return out, e
	}
	if e = loadSource(ctx, tx, sc, c.SourceID, 0, &out); e != nil {
		return out, e
	}
	return out, s.finish(ctx, tx, a)
}

// Revoke is the WIS-272 revocation transaction ("POST .../operational-sources/{id}/revoke"):
// in one transaction it appends a source revision that is disabled and carries the next
// access-policy revision, and cancels any running import for the source. Intake refuses the
// source at once (its current revision is no longer the approved one), and every context
// built under the previous access-policy revision reads SOURCE_NO_LONGER_APPROVED. History is
// kept: the earlier revisions, records and contexts are immutable.
//
// Like every source-authority change it needs an owner or supervisor with a recent identity
// proof, If-Match on the current source revision and an Idempotency-Key. A later revision may
// re-enable the source only with an access-policy revision at least as high as the revoked
// one, so contexts from before the revocation never become eligible again.
func (s *Service) Revoke(ctx context.Context, a Actor, sc Scope, id, key string, expected int64) (Source, error) {
	var out Source
	if !validID(id) || !validText(key, 128) || expected < 1 {
		return out, ErrInvalid
	}
	tx, auth, e := s.begin(ctx, a, sc, true, true)
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	hash := digest(struct {
		Operation string
		SourceID  string
		Expected  int64
	}{"revoke", id, expected})
	prior, rev, replayed, e := receipt(ctx, tx, a, sc, key, hash, "source")
	if e != nil {
		return out, e
	}
	if replayed {
		if e = loadSource(ctx, tx, sc, prior, rev, &out); e != nil {
			return out, e
		}
		return out, s.finish(ctx, tx, a)
	}
	var current int64
	e = tx.QueryRowContext(ctx, `SELECT current_revision FROM supervisor.operational_source_heads WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 FOR UPDATE`, sc.TenantID, sc.SiteID, id).Scan(&current)
	if errors.Is(e, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if e != nil {
		return out, e
	}
	if expected != current {
		return out, ErrRevision
	}
	var old Source
	if e = loadSource(ctx, tx, sc, id, current, &old); e != nil {
		return out, e
	}
	next := old.AccessPolicyRevision + 1
	if old.AccessPolicyRevision >= MaxAccessPolicyRevision {
		// At the bound the revision is kept, so it never overflows (WIS-720). The new source
		// revision still invalidates every context built before it, and the source can never be
		// enabled again (see MaxAccessPolicyRevision).
		next = old.AccessPolicyRevision
	}
	revoked := Config{
		SourceID: id, Type: old.Type, AuthorityScope: old.AuthorityScope, CredentialRef: old.CredentialRef,
		Enabled: false, AccessPolicyRevision: next,
		FreshnessSeconds: old.FreshnessSeconds, ClockSkewSeconds: old.ClockSkewSeconds,
	}
	newRev := current + 1
	if _, e = tx.ExecContext(ctx, `UPDATE supervisor.operational_source_heads SET current_revision=$4 WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, sc.TenantID, sc.SiteID, id, newRev); e != nil {
		return out, e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_source_revisions(tenant_id,site_id,source_id,revision,source_type,authority_scope,credential_ref,mapping_revision,mapping_sha256,enabled,access_policy_revision,freshness_seconds,clock_skew_seconds,config_sha256,actor_issuer,actor_subject,auth_time) VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		sc.TenantID, sc.SiteID, id, newRev, revoked.Type, revoked.AuthorityScope, revoked.CredentialRef, old.MappingRevision, old.MappingSHA256, revoked.Enabled, revoked.AccessPolicyRevision, revoked.FreshnessSeconds, revoked.ClockSkewSeconds, configDigest(revoked), a.Issuer, a.Subject, auth); e != nil {
		return out, e
	}
	if e = CancelImports(ctx, tx, sc.TenantID, sc.SiteID, id, "", 0); e != nil {
		return out, e
	}
	if e = recordAudit(ctx, tx, a, sc, id, newRev, "source.revoked", auditRefs{source: newRev}); e != nil {
		return out, e
	}
	if e = saveReceipt(ctx, tx, a, sc, key, hash, "source", id, newRev); e != nil {
		return out, e
	}
	if e = loadSource(ctx, tx, sc, id, 0, &out); e != nil {
		return out, e
	}
	return out, s.finish(ctx, tx, a)
}

// CancelImports ends, as 'cancelled', the source's imports still running and created at least
// olderThan ago (only jobID when it is not empty), with their counts frozen from the rows recorded
// so far. It first takes the job row lock, which every chunk and outcome write holds FOR SHARE until
// it commits, and only then counts the rows in a second statement (WIS-719): in one statement the
// counts would come from the snapshot taken before the lock wait and miss the rows just committed.
func CancelImports(ctx context.Context, tx *sql.Tx, tenantID, siteID, sourceID, jobID string, olderThan time.Duration) error {
	rows, err := tx.QueryContext(ctx, `SELECT id::text FROM supervisor.operational_import_jobs
 WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND status='running' AND created_at <= clock_timestamp() - make_interval(secs => $4)
   AND ($5 = '' OR id::text = $5) FOR UPDATE`, tenantID, siteID, sourceID, olderThan.Seconds(), jobID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if err = rows.Err(); err != nil || len(ids) == 0 {
		return err
	}
	_, err = tx.ExecContext(ctx, `WITH counts AS (
  SELECT j.id,
    count(x.row_index) FILTER (WHERE x.status='accepted') AS accepted,
    count(x.row_index) FILTER (WHERE x.status='duplicate') AS duplicate,
    count(x.row_index) FILTER (WHERE x.status='quarantined') AS quarantined,
    count(x.row_index) FILTER (WHERE x.status='rejected') AS rejected
   FROM unnest($3::uuid[]) AS j(id) LEFT JOIN supervisor.operational_import_rows x ON (x.tenant_id,x.site_id,x.job_id)=($1,$2,j.id)
   GROUP BY j.id
)
UPDATE supervisor.operational_import_jobs j SET status='cancelled',accepted_rows=c.accepted,duplicate_rows=c.duplicate,
  quarantined_rows=c.quarantined,rejected_rows=c.rejected,finished_at=clock_timestamp()
 FROM counts c WHERE j.tenant_id=$1 AND j.site_id=$2 AND j.id=c.id AND j.status='running'`, tenantID, siteID, ids)
	return err
}

func loadSource(ctx context.Context, tx *sql.Tx, sc Scope, id string, revision int64, out *Source) error {
	var cred sql.NullString
	e := tx.QueryRowContext(ctx, `SELECT r.source_id,r.source_type,r.authority_scope,r.credential_ref,r.enabled,r.access_policy_revision,r.freshness_seconds,r.clock_skew_seconds,r.revision,h.current_revision,r.mapping_revision,r.mapping_sha256,r.config_sha256,r.actor_issuer,r.actor_subject,r.created_at FROM supervisor.operational_source_heads h JOIN supervisor.operational_source_revisions r ON (r.tenant_id,r.site_id,r.source_id)=(h.tenant_id,h.site_id,h.source_id) WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.source_id=$3 AND r.revision=CASE WHEN $4=0 THEN h.current_revision ELSE $4 END`, sc.TenantID, sc.SiteID, id, revision).Scan(&out.SourceID, &out.Type, &out.AuthorityScope, &cred, &out.Enabled, &out.AccessPolicyRevision, &out.FreshnessSeconds, &out.ClockSkewSeconds, &out.Revision, &out.CurrentRevision, &out.MappingRevision, &out.MappingSHA256, &out.ConfigSHA256, &out.ActorIssuer, &out.ActorSubject, &out.CreatedAt)
	if errors.Is(e, sql.ErrNoRows) {
		return ErrNotFound
	}
	if e != nil {
		return e
	}
	out.CredentialRef = cred.String
	rows, err := tx.QueryContext(ctx, `SELECT external_type,external_key,entity_id,entity_revision FROM supervisor.operational_source_mappings WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND mapping_revision=$4 ORDER BY external_type,external_key`, sc.TenantID, sc.SiteID, id, out.MappingRevision)
	if err != nil {
		return err
	}
	mappings := make([]Mapping, 0)
	for rows.Next() {
		var m Mapping
		if err = rows.Scan(&m.ExternalType, &m.ExternalKey, &m.EntityID, &m.EntityRevision); err != nil {
			rows.Close()
			return err
		}
		mappings = append(mappings, m)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	canonical, err := canonicalMappings(mappings)
	if err != nil || mappingDigest(canonical) != out.MappingSHA256 {
		return ErrInvalid
	}
	var stale int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.operational_source_mappings m JOIN supervisor.knowledge_entity_heads h ON (h.tenant_id,h.site_id,h.id)=(m.tenant_id,m.site_id,m.entity_id) WHERE m.tenant_id=$1 AND m.site_id=$2 AND m.source_id=$3 AND m.mapping_revision=$4 AND h.current_revision<>m.entity_revision`, sc.TenantID, sc.SiteID, id, out.MappingRevision).Scan(&stale); err != nil {
		return err
	}
	out.MappingTargetsCurrent = stale == 0
	var review Review
	e = tx.QueryRowContext(ctx, `SELECT r.decision_id,r.revision,h.current_revision,r.source_revision,r.config_sha256,r.mapping_revision,r.mapping_sha256,r.verdict,r.reason,r.actor_issuer,r.actor_subject,r.auth_time,r.created_at FROM supervisor.operational_source_review_heads h JOIN supervisor.operational_source_reviews r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=(h.tenant_id,h.site_id,h.source_id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.source_id=$3`, sc.TenantID, sc.SiteID, id).Scan(&review.DecisionID, &review.Revision, &review.CurrentRevision, &review.SourceRevision, &review.ConfigSHA256, &review.MappingRevision, &review.MappingSHA256, &review.Verdict, &review.Reason, &review.ActorIssuer, &review.ActorSubject, &review.AuthenticatedAt, &review.CreatedAt)
	if e == nil {
		out.Review = &review
		out.ReviewEligible = out.MappingTargetsCurrent && out.Revision == out.CurrentRevision && review.Verdict == "approved" && review.SourceRevision == out.Revision && review.ConfigSHA256 == out.ConfigSHA256 && review.MappingRevision == out.MappingRevision && review.MappingSHA256 == out.MappingSHA256
	} else if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	return nil
}
func (s *Service) Get(ctx context.Context, a Actor, sc Scope, id string, revision int64) (Source, []Mapping, error) {
	var out Source
	tx, _, e := s.begin(ctx, a, sc, false, false)
	if e != nil {
		return out, nil, e
	}
	defer tx.Rollback()
	if !validID(id) || revision < 0 {
		return out, nil, ErrInvalid
	}
	if e = loadSource(ctx, tx, sc, id, revision, &out); e != nil {
		return out, nil, e
	}
	rows, e := tx.QueryContext(ctx, `SELECT external_type,external_key,entity_id,entity_revision FROM supervisor.operational_source_mappings WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND mapping_revision=$4 ORDER BY external_type,external_key`, sc.TenantID, sc.SiteID, id, out.MappingRevision)
	if e != nil {
		return out, nil, e
	}
	maps := []Mapping{}
	for rows.Next() {
		var m Mapping
		if e = rows.Scan(&m.ExternalType, &m.ExternalKey, &m.EntityID, &m.EntityRevision); e != nil {
			rows.Close()
			return out, nil, e
		}
		maps = append(maps, m)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, nil, e
	}
	if e = recordAudit(ctx, tx, a, sc, id, out.Revision, "source.read", auditRefs{source: out.Revision}); e != nil {
		return out, nil, e
	}
	if e = recordAudit(ctx, tx, a, sc, id, out.MappingRevision, "mapping.read", auditRefs{source: out.Revision, mapping: out.MappingRevision}); e != nil {
		return out, nil, e
	}
	return out, maps, s.finish(ctx, tx, a)
}
func (s *Service) ReviseMappings(ctx context.Context, a Actor, sc Scope, id string, maps []Mapping, key string, expected int64) (Source, error) {
	var out Source
	if !validID(id) || !validText(key, 128) || expected < 1 {
		return out, ErrInvalid
	}
	sorted, e := canonicalMappings(maps)
	if e != nil {
		return out, e
	}
	// position maps a canonical row back to the caller's index for row-level problem paths.
	position := make(map[string]int, len(maps))
	for i, m := range maps {
		position[m.ExternalType+"\x00"+m.ExternalKey] = i
	}
	tx, auth, e := s.begin(ctx, a, sc, true, true)
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	requestHash := digest(struct {
		SourceID string
		Mappings []Mapping
		Expected int64
	}{id, sorted, expected})
	prior, priorRev, replayed, e := receipt(ctx, tx, a, sc, key, requestHash, "mapping")
	if e != nil {
		return out, e
	}
	if replayed {
		if e = loadSource(ctx, tx, sc, prior, priorRev, &out); e != nil {
			return out, e
		}
		if e = recordAudit(ctx, tx, a, sc, id, out.MappingRevision, "mapping.read", auditRefs{source: out.Revision, mapping: out.MappingRevision}); e != nil {
			return out, e
		}
		return out, s.finish(ctx, tx, a)
	}
	var cur int64
	e = tx.QueryRowContext(ctx, `SELECT current_revision FROM supervisor.operational_source_mapping_heads WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 FOR UPDATE`, sc.TenantID, sc.SiteID, id).Scan(&cur)
	if errors.Is(e, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if e != nil {
		return out, e
	}
	if cur != expected {
		return out, ErrRevision
	}
	for _, m := range sorted {
		at := position[m.ExternalType+"\x00"+m.ExternalKey]
		var targetRevision int64
		e = tx.QueryRowContext(ctx, `SELECT current_revision FROM supervisor.knowledge_entity_heads WHERE tenant_id=$1 AND site_id=$2 AND id=$3 FOR SHARE`, sc.TenantID, sc.SiteID, m.EntityID).Scan(&targetRevision)
		if errors.Is(e, sql.ErrNoRows) {
			return out, rowError(at, "entity_id", ErrNotFound)
		}
		if e != nil {
			return out, e
		}
		if targetRevision != m.EntityRevision {
			return out, rowError(at, "entity_revision", ErrRevision)
		}
		// WIS-270: a merged (redirected) entity is no longer a valid mapping target; map the
		// surviving entity instead. The head share lock above serializes with MergeEntity.
		var redirected bool
		if e = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.knowledge_entity_redirects WHERE tenant_id=$1 AND site_id=$2 AND from_entity_id=$3)`, sc.TenantID, sc.SiteID, m.EntityID).Scan(&redirected); e != nil {
			return out, e
		}
		if redirected {
			return out, rowError(at, "entity_id", ErrUnknownMapping)
		}
	}
	next := cur + 1
	hash := mappingDigest(sorted)
	_, e = tx.ExecContext(ctx, `UPDATE supervisor.operational_source_mapping_heads SET current_revision=$4 WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, sc.TenantID, sc.SiteID, id, next)
	if e != nil {
		return out, e
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_source_mapping_revisions(tenant_id,site_id,source_id,revision,mapping_sha256,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,$6,$7)`, sc.TenantID, sc.SiteID, id, next, hash, a.Issuer, a.Subject)
	if e != nil {
		return out, e
	}
	for _, m := range sorted {
		_, e = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_source_mappings(tenant_id,site_id,source_id,mapping_revision,external_type,external_key,entity_id,entity_revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, sc.TenantID, sc.SiteID, id, next, m.ExternalType, m.ExternalKey, m.EntityID, m.EntityRevision)
		if e != nil {
			return out, e
		}
	}
	// The source revision atomically pins the new mapping revision, invalidating any prior approval.
	var old Source
	if e = loadSource(ctx, tx, sc, id, 0, &old); e != nil {
		return out, e
	}
	newSourceRev := old.CurrentRevision + 1
	_, e = tx.ExecContext(ctx, `UPDATE supervisor.operational_source_heads SET current_revision=$4 WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, sc.TenantID, sc.SiteID, id, newSourceRev)
	if e != nil {
		return out, e
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_source_revisions(tenant_id,site_id,source_id,revision,source_type,authority_scope,credential_ref,mapping_revision,mapping_sha256,enabled,access_policy_revision,freshness_seconds,clock_skew_seconds,config_sha256,actor_issuer,actor_subject,auth_time) VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`, sc.TenantID, sc.SiteID, id, newSourceRev, old.Type, old.AuthorityScope, old.CredentialRef, next, hash, old.Enabled, old.AccessPolicyRevision, old.FreshnessSeconds, old.ClockSkewSeconds, old.ConfigSHA256, a.Issuer, a.Subject, auth)
	if e != nil {
		return out, e
	}
	if e = recordAudit(ctx, tx, a, sc, id, next, "mapping.revised", auditRefs{source: newSourceRev, mapping: next}); e != nil {
		return out, e
	}
	if e = saveReceipt(ctx, tx, a, sc, key, requestHash, "mapping", id, newSourceRev); e != nil {
		return out, e
	}
	if e = loadSource(ctx, tx, sc, id, 0, &out); e != nil {
		return out, e
	}
	return out, s.finish(ctx, tx, a)
}
func (s *Service) Decide(ctx context.Context, a Actor, sc Scope, id string, in ReviewInput, key string, expected int64) (Review, error) {
	var out Review
	if !validID(id) || !validText(key, 128) || expected < 1 || !validText(in.Reason, 1000) || len(strings.TrimSpace(in.Reason)) == 0 || (in.Verdict != "approved" && in.Verdict != "rejected") {
		return out, ErrInvalid
	}
	tx, auth, e := s.begin(ctx, a, sc, true, true)
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	requestHash := digest(struct {
		SourceID string
		Input    ReviewInput
		Expected int64
	}{id, in, expected})
	prior, priorRev, replayed, e := receipt(ctx, tx, a, sc, key, requestHash, "review")
	if e != nil {
		return out, e
	}
	if replayed {
		e = tx.QueryRowContext(ctx, `SELECT decision_id,revision,source_revision,config_sha256,mapping_revision,mapping_sha256,verdict,reason,actor_issuer,actor_subject,auth_time,created_at FROM supervisor.operational_source_reviews WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND revision=$4`, sc.TenantID, sc.SiteID, prior, priorRev).Scan(&out.DecisionID, &out.Revision, &out.SourceRevision, &out.ConfigSHA256, &out.MappingRevision, &out.MappingSHA256, &out.Verdict, &out.Reason, &out.ActorIssuer, &out.ActorSubject, &out.AuthenticatedAt, &out.CreatedAt)
		if e != nil {
			return out, e
		}
		if e = tx.QueryRowContext(ctx, `SELECT current_revision FROM supervisor.operational_source_review_heads WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, sc.TenantID, sc.SiteID, id).Scan(&out.CurrentRevision); e != nil {
			return out, e
		}
		return out, s.finish(ctx, tx, a)
	}
	var src Source
	if e = loadSource(ctx, tx, sc, id, expected, &src); e != nil {
		return out, e
	}
	if src.Revision != src.CurrentRevision {
		return out, ErrRevision
	}
	if !src.MappingTargetsCurrent {
		return out, ErrRevision
	}
	var current int64
	e = tx.QueryRowContext(ctx, `SELECT current_revision FROM supervisor.operational_source_review_heads WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 FOR UPDATE`, sc.TenantID, sc.SiteID, id).Scan(&current)
	if errors.Is(e, sql.ErrNoRows) {
		current = 0
	} else if e != nil {
		return out, e
	}
	next := current + 1
	if current == 0 {
		if _, e = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_source_review_heads(tenant_id,site_id,source_id,current_revision) VALUES($1,$2,$3,1)`, sc.TenantID, sc.SiteID, id); e != nil {
			return out, e
		}
	} else {
		if _, e = tx.ExecContext(ctx, `UPDATE supervisor.operational_source_review_heads SET current_revision=$4 WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, sc.TenantID, sc.SiteID, id, next); e != nil {
			return out, e
		}
	}
	out.DecisionID = uuid.NewString()
	out.SourceRevision = src.Revision
	out.ConfigSHA256 = src.ConfigSHA256
	out.MappingRevision = src.MappingRevision
	out.MappingSHA256 = src.MappingSHA256
	out.Verdict = in.Verdict
	out.Reason = strings.TrimSpace(in.Reason)
	out.ActorIssuer = a.Issuer
	out.ActorSubject = a.Subject
	out.AuthenticatedAt = auth
	_, e = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_source_reviews(tenant_id,site_id,source_id,revision,decision_id,source_revision,config_sha256,mapping_revision,mapping_sha256,verdict,reason,actor_issuer,actor_subject,auth_time) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, sc.TenantID, sc.SiteID, id, next, out.DecisionID, out.SourceRevision, out.ConfigSHA256, out.MappingRevision, out.MappingSHA256, out.Verdict, out.Reason, a.Issuer, a.Subject, auth)
	if e != nil {
		return out, e
	}
	out.Revision = next
	out.CurrentRevision = next
	out.CreatedAt = time.Now().UTC()
	if e = recordAudit(ctx, tx, a, sc, id, next, "review.decided", auditRefs{source: src.Revision, review: next}); e != nil {
		return out, e
	}
	if e = saveReceipt(ctx, tx, a, sc, key, requestHash, "review", id, next); e != nil {
		return out, e
	}
	return out, s.finish(ctx, tx, a)
}
