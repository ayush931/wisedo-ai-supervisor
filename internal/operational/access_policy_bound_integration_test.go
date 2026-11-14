//go:build integration

package operational

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type aprFixture struct {
	t                 *testing.T
	ctx               context.Context
	control, runtime  *sql.DB
	svc               *Service
	sc                Scope
	owner, supervisor Actor
}

func newAPRFixture(t *testing.T) *aprFixture {
	t.Helper()
	if os.Getenv("SUPERVISOR_TEST_ADMIN_URL") == "" || os.Getenv("SUPERVISOR_TEST_DATABASE_URL") == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL admin/runtime URLs required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	admin, err := sql.Open("pgx", os.Getenv("SUPERVISOR_TEST_ADMIN_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	database := "aprbound_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.ExecContext(ctx, "CREATE DATABASE "+database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)") })
	connect := func(raw string) *sql.DB {
		u, _ := url.Parse(raw)
		u.Path = "/" + database
		db, e := sql.Open("pgx", u.String())
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	f := &aprFixture{t: t, ctx: ctx, control: connect(os.Getenv("SUPERVISOR_TEST_ADMIN_URL")), runtime: connect(os.Getenv("SUPERVISOR_TEST_DATABASE_URL"))}
	if err = migrate.Apply(ctx, f.control); err != nil {
		t.Fatal(err)
	}
	f.sc = Scope{uuid.NewString(), uuid.NewString()}
	f.exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'apr bound')`, f.sc.TenantID)
	f.exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'apr bound site')`, f.sc.TenantID, f.sc.SiteID)
	actor := func(role string) Actor {
		sum := sha256.Sum256([]byte(uuid.NewString()))
		a := Actor{"https://apr-bound.example", uuid.NewString(), hex.EncodeToString(sum[:])}
		f.exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'fixture',now(),now(),now()+interval '1 hour',now())`, a.SessionHash, a.Subject, a.Issuer)
		f.exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, f.sc.TenantID, f.sc.SiteID, a.Issuer, a.Subject, role)
		return a
	}
	f.owner, f.supervisor = actor("owner"), actor("supervisor")
	f.svc = New(f.runtime)
	return f
}

func (f *aprFixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, e := f.control.ExecContext(f.ctx, q, args...); e != nil {
		f.t.Fatal(e)
	}
}

func (f *aprFixture) count(q string, args ...any) int {
	f.t.Helper()
	var n int
	if e := f.control.QueryRowContext(f.ctx, q, args...).Scan(&n); e != nil {
		f.t.Fatal(e)
	}
	return n
}

// asPreBound runs statements as admin with triggers off, to stand in for data written before
// the WIS-720 bound existed.
func (f *aprFixture) asPreBound(qs ...string) {
	f.t.Helper()
	tx, e := f.control.BeginTx(f.ctx, nil)
	if e != nil {
		f.t.Fatal(e)
	}
	defer tx.Rollback()
	for _, q := range append([]string{`SET LOCAL session_replication_role = replica`}, qs...) {
		if _, e = tx.ExecContext(f.ctx, q); e != nil {
			f.t.Fatal(e)
		}
	}
	if e = tx.Commit(); e != nil {
		f.t.Fatal(e)
	}
}

func (f *aprFixture) storeAPR(sourceID string, apr int64) {
	f.t.Helper()
	f.asPreBound(`UPDATE supervisor.operational_source_revisions SET access_policy_revision=` + strconv.FormatInt(apr, 10) + ` WHERE tenant_id='` + f.sc.TenantID + `' AND source_id='` + sourceID + `'`)
}

// head reads the current source revision as stored.
func (f *aprFixture) head(sourceID string) (revision int64, enabled bool, apr int64) {
	f.t.Helper()
	if e := f.control.QueryRowContext(f.ctx, `SELECT r.revision,r.enabled,r.access_policy_revision FROM supervisor.operational_source_heads h JOIN supervisor.operational_source_revisions r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=(h.tenant_id,h.site_id,h.source_id,h.current_revision) WHERE h.tenant_id=$1 AND h.source_id=$2`, f.sc.TenantID, sourceID).Scan(&revision, &enabled, &apr); e != nil {
		f.t.Fatal(e)
	}
	return
}

func aprConfig(apr int64) Config {
	return Config{SourceID: uuid.NewString(), Type: "event_api", AuthorityScope: "production_delta", Enabled: true, AccessPolicyRevision: apr, FreshnessSeconds: 120, ClockSkewSeconds: 30}
}

// WIS-720: access_policy_revision had no upper bound. A source stored at MaxInt64 could never be
// revoked: the revocation's +1 overflowed, the database refused the decrease, and the API answered
// 503 on every retry. New configurations are now bounded below MaxAccessPolicyRevision, so a
// revocation always fits. A source revoked from the bound minus one reaches the bound; it stays
// manageable (revoked again, revised while disabled) but can never be enabled again.
func TestAccessPolicyRevisionBoundKeepsRevocationPossiblePostgres(t *testing.T) {
	f := newAPRFixture(t)
	var dec Config
	body := `{"source_id":"` + uuid.NewString() + `","type":"event_api","authority_scope":"production_delta","enabled":true,"access_policy_revision":9223372036854775807,"freshness_seconds":120,"clock_skew_seconds":30}`
	if e := strictjson.Decode([]byte(body), &dec, 1<<20); e != nil || dec.AccessPolicyRevision != math.MaxInt64 {
		t.Fatalf("decode: %v %+v", e, dec)
	}
	// Control: an ordinary source revokes and its revision goes up by one.
	ctl := aprConfig(1)
	if _, err := f.svc.CreateOrRevise(f.ctx, f.supervisor, f.sc, ctl, "ctl", 0); err != nil {
		t.Fatal(err)
	}
	r, err := f.svc.Revoke(f.ctx, f.owner, f.sc, ctl.SourceID, "ctl-revoke", 1)
	if err != nil || r.Enabled || r.AccessPolicyRevision != 2 {
		t.Fatalf("control revoke: %+v %v", r, err)
	}
	// A new source at or above the bound is refused before it can be stored, enabled or not.
	for i, c := range []struct {
		apr     int64
		enabled bool
	}{{math.MaxInt64, true}, {MaxAccessPolicyRevision, true}, {MaxAccessPolicyRevision, false}} {
		cfg := dec
		cfg.SourceID, cfg.AccessPolicyRevision, cfg.Enabled = uuid.NewString(), c.apr, c.enabled
		if _, err = f.svc.CreateOrRevise(f.ctx, f.supervisor, f.sc, cfg, "new-"+strconv.Itoa(i), 0); !errors.Is(err, ErrInvalid) {
			t.Fatalf("create at %d enabled=%v: %v, want ErrInvalid", c.apr, c.enabled, err)
		}
	}
	// The highest accepted value still leaves room for one revocation.
	top := aprConfig(MaxAccessPolicyRevision - 1)
	created, err := f.svc.CreateOrRevise(f.ctx, f.supervisor, f.sc, top, "top", 0)
	if err != nil {
		t.Fatal(err)
	}
	r, err = f.svc.Revoke(f.ctx, f.owner, f.sc, top.SourceID, "top-revoke", created.Revision)
	if err != nil || r.Enabled || r.AccessPolicyRevision != MaxAccessPolicyRevision {
		t.Fatalf("revoke at the bound: %+v %v", r, err)
	}
	// WIS-720 review: the source now sits at the bound. It must stay manageable.
	again, err := f.svc.Revoke(f.ctx, f.owner, f.sc, top.SourceID, "top-revoke-again", r.Revision)
	if err != nil || again.Enabled || again.AccessPolicyRevision != MaxAccessPolicyRevision || again.Revision != r.Revision+1 {
		t.Fatalf("second revoke at the bound: %+v %v", again, err)
	}
	if n := f.count(`SELECT count(*) FROM supervisor.operational_source_audit WHERE tenant_id=$1 AND source_id=$2 AND event_type='source.revoked'`, f.sc.TenantID, top.SourceID); n != 2 {
		t.Fatalf("source.revoked audit rows=%d, want 2", n)
	}
	disabled := top
	disabled.Enabled, disabled.AccessPolicyRevision, disabled.FreshnessSeconds = false, MaxAccessPolicyRevision, 300
	revised, err := f.svc.CreateOrRevise(f.ctx, f.supervisor, f.sc, disabled, "top-disabled-edit", again.Revision)
	if err != nil || revised.Enabled || revised.AccessPolicyRevision != MaxAccessPolicyRevision || revised.FreshnessSeconds != 300 {
		t.Fatalf("disabled revision at the bound: %+v %v", revised, err)
	}
	for _, c := range []struct {
		name    string
		enabled bool
		apr     int64
		want    error
	}{
		{"re-enable at the bound", true, MaxAccessPolicyRevision, ErrInvalid},
		{"raise past the bound", false, MaxAccessPolicyRevision + 1, ErrInvalid},
		{"lower below the bound", false, MaxAccessPolicyRevision - 1, ErrAccessPolicyRegression},
	} {
		cfg := disabled
		cfg.Enabled, cfg.AccessPolicyRevision = c.enabled, c.apr
		if _, err = f.svc.CreateOrRevise(f.ctx, f.supervisor, f.sc, cfg, "top-"+c.name, revised.Revision); !errors.Is(err, c.want) {
			t.Fatalf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if rev, en, apr := f.head(top.SourceID); rev != revised.Revision || en || apr != MaxAccessPolicyRevision {
		t.Fatalf("after refusals: revision=%d enabled=%v apr=%d", rev, en, apr)
	}
}

// WIS-720 review finding 1 (High): an enabled source stored above the bound before the bound
// existed lost its plain-disable path (422 INVALID_SOURCE). A disable with the unchanged stored
// revision must still pass and intake must refuse the source afterwards; re-enabling and lowering
// stay refused.
func TestWIS720PreBoundSourceCanStillBeDisabledPostgres(t *testing.T) {
	f := newAPRFixture(t)
	cfg := aprConfig(1)
	if _, err := f.svc.CreateOrRevise(f.ctx, f.owner, f.sc, cfg, "create", 0); err != nil {
		t.Fatal(err)
	}
	// Map and approve it, so intake accepts it before the disable and the refusal is observable.
	evOut, err := evidence.New(f.runtime).CreateOrRevise(f.ctx, evidence.Actor{Issuer: f.owner.Issuer, Subject: f.owner.Subject, SessionHash: f.owner.SessionHash}, evidence.Scope{TenantID: f.sc.TenantID, SiteID: f.sc.SiteID}, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Asset identity", Text: "Synthetic reviewed asset identity"}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	entityID := uuid.NewString()
	entity, err := knowledge.New(f.runtime).CreateEntity(f.ctx, knowledge.Actor{Issuer: f.owner.Issuer, Subject: f.owner.Subject, SessionHash: f.owner.SessionHash}, knowledge.Scope{TenantID: f.sc.TenantID, SiteID: f.sc.SiteID}, knowledge.EntityInput{EntityID: entityID, Type: "asset", AssetKind: "machine", Name: "Press 1", SourceID: evOut.SourceID, SourceRevision: evOut.Revision}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := f.svc.ReviseMappings(f.ctx, f.owner, f.sc, cfg.SourceID, []Mapping{{ExternalType: "asset", ExternalKey: "press-01", EntityID: entityID, EntityRevision: entity.Revision}}, "map", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.Decide(f.ctx, f.supervisor, f.sc, cfg.SourceID, ReviewInput{Verdict: "approved", Reason: "verified"}, "approve", mapped.Revision); err != nil {
		t.Fatal(err)
	}
	f.storeAPR(cfg.SourceID, math.MaxInt64)
	if _, err = f.svc.Resolve(f.ctx, f.owner, f.sc, cfg.SourceID, "asset", "press-01"); err != nil {
		t.Fatalf("precondition: intake accepts the pre-bound source: %v", err)
	}
	for _, c := range []struct {
		name    string
		enabled bool
		apr     int64
		want    error
	}{
		{"re-enable unchanged", true, math.MaxInt64, ErrInvalid},
		{"disable lowered", false, MaxAccessPolicyRevision - 1, ErrAccessPolicyRegression},
	} {
		bad := cfg
		bad.Enabled, bad.AccessPolicyRevision = c.enabled, c.apr
		if _, err = f.svc.CreateOrRevise(f.ctx, f.supervisor, f.sc, bad, c.name, mapped.Revision); !errors.Is(err, c.want) {
			t.Fatalf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	disable := cfg
	disable.Enabled, disable.AccessPolicyRevision = false, math.MaxInt64
	out, err := f.svc.CreateOrRevise(f.ctx, f.supervisor, f.sc, disable, "disable", mapped.Revision)
	if err != nil || out.Enabled || out.AccessPolicyRevision != math.MaxInt64 || out.Revision != mapped.Revision+1 {
		t.Fatalf("disable of a pre-bound source: %+v %v", out, err)
	}
	if rev, en, apr := f.head(cfg.SourceID); rev != out.Revision || en || apr != math.MaxInt64 {
		t.Fatalf("stored after disable: revision=%d enabled=%v apr=%d", rev, en, apr)
	}
	if _, err = f.svc.Resolve(f.ctx, f.owner, f.sc, cfg.SourceID, "asset", "press-01"); !errors.Is(err, ErrUnknownMapping) {
		t.Fatalf("intake after disable: %v, want ErrUnknownMapping", err)
	}
	enable := disable
	enable.Enabled = true
	if _, err = f.svc.CreateOrRevise(f.ctx, f.supervisor, f.sc, enable, "re-enable", out.Revision); !errors.Is(err, ErrInvalid) {
		t.Fatalf("re-enable after disable: %v, want ErrInvalid", err)
	}
}

// WIS-720 review finding 1 (High): Revoke of a source stored at or above the bound returned 422
// before disabling it, cancelling its imports or recording source.revoked. It now revokes and
// keeps the stored revision, which cannot be incremented.
func TestWIS720PreBoundSourceCanStillBeRevokedPostgres(t *testing.T) {
	f := newAPRFixture(t)
	for _, apr := range []int64{math.MaxInt64, MaxAccessPolicyRevision} {
		tag := strconv.FormatInt(apr, 10)
		cfg := aprConfig(1)
		created, err := f.svc.CreateOrRevise(f.ctx, f.supervisor, f.sc, cfg, "create-"+tag, 0)
		if err != nil {
			t.Fatal(err)
		}
		f.storeAPR(cfg.SourceID, apr)
		jobID := uuid.NewString()
		f.asPreBound(`SELECT set_config('supervisor.tenant_id','`+f.sc.TenantID+`',true)`,
			`INSERT INTO supervisor.operational_import_jobs(tenant_id,site_id,id,source_id,format,file_sha256,request_sha256,idempotency_key,actor_issuer,actor_subject,source_revision,mapping_revision,start_sequence,row_count,status) VALUES('`+f.sc.TenantID+`','`+f.sc.SiteID+`','`+jobID+`','`+cfg.SourceID+`','csv',repeat('a',64),repeat('b',64),'import-`+tag+`','https://issuer.invalid','subject',1,1,1,3,'running')`)
		r, err := f.svc.Revoke(f.ctx, f.owner, f.sc, cfg.SourceID, "revoke-"+tag, created.Revision)
		if err != nil || r.Enabled || r.AccessPolicyRevision != apr || r.Revision != created.Revision+1 {
			t.Fatalf("revoke at %d: %+v %v", apr, r, err)
		}
		if rev, en, stored := f.head(cfg.SourceID); rev != r.Revision || en || stored != apr {
			t.Fatalf("stored after revoke at %d: revision=%d enabled=%v apr=%d", apr, rev, en, stored)
		}
		if n := f.count(`SELECT count(*) FROM supervisor.operational_import_jobs WHERE tenant_id=$1 AND id=$2 AND status='cancelled'`, f.sc.TenantID, jobID); n != 1 {
			t.Fatalf("running import not cancelled at %d", apr)
		}
		if n := f.count(`SELECT count(*) FROM supervisor.operational_source_audit WHERE tenant_id=$1 AND source_id=$2 AND event_type='source.revoked' AND revision=$3`, f.sc.TenantID, cfg.SourceID, r.Revision); n != 1 {
			t.Fatalf("source.revoked audit rows at %d: %d, want 1", apr, n)
		}
	}
}

// WIS-720 review finding 2 (Medium): the bound ran before the idempotency receipt, so an identical
// retry of a request accepted before the bound existed got 422 instead of its original result
// (rule 07). The receipt now replays first; a different body under the same key is still a
// conflict, and a fresh request above the bound is still refused.
func TestWIS720IdenticalRetryOfPreBoundRequestReplaysPostgres(t *testing.T) {
	f := newAPRFixture(t)
	big := aprConfig(math.MaxInt64)
	small := big
	small.AccessPolicyRevision = 1
	if _, err := f.svc.CreateOrRevise(f.ctx, f.supervisor, f.sc, small, "replay-key", 0); err != nil {
		t.Fatal(err)
	}
	// Stand in for a create of big accepted before the bound: its stored row and its receipt.
	preBoundHash := digest(struct {
		Config   Config
		Expected int64
	}{big, 0})
	f.storeAPR(big.SourceID, math.MaxInt64)
	f.asPreBound(`UPDATE supervisor.operational_source_receipts SET request_sha256='` + preBoundHash + `' WHERE tenant_id='` + f.sc.TenantID + `' AND idempotency_key='replay-key'`)

	got, err := f.svc.CreateOrRevise(f.ctx, f.supervisor, f.sc, big, "replay-key", 0)
	if err != nil || got.SourceID != big.SourceID || got.Revision != 1 || got.AccessPolicyRevision != math.MaxInt64 || !got.Enabled {
		t.Fatalf("identical retry: %+v %v, want the original result", got, err)
	}
	if rev, _, _ := f.head(big.SourceID); rev != 1 {
		t.Fatalf("replay appended a revision: head=%d", rev)
	}
	if n := f.count(`SELECT count(*) FROM supervisor.operational_source_audit WHERE tenant_id=$1 AND source_id=$2 AND event_type='mutation.replayed'`, f.sc.TenantID, big.SourceID); n != 1 {
		t.Fatalf("mutation.replayed audit rows=%d, want 1", n)
	}
	conflicting := big
	conflicting.FreshnessSeconds = 121
	if _, err = f.svc.CreateOrRevise(f.ctx, f.supervisor, f.sc, conflicting, "replay-key", 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("different body under the same key: %v, want ErrConflict", err)
	}
	if _, err = f.svc.CreateOrRevise(f.ctx, f.supervisor, f.sc, big, "fresh-key", 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("fresh enabled revision above the bound: %v, want ErrInvalid", err)
	}
}
