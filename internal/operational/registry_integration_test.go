//go:build integration

package operational

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestOperationalRegistryPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("real PostgreSQL admin and runtime URLs required")
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	database := "operational_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.ExecContext(ctx, "CREATE DATABASE "+database); err != nil {
		t.Fatal(err)
	}
	var control, runtime *sql.DB
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if runtime != nil {
			if e := runtime.Close(); e != nil {
				t.Error(e)
			}
		}
		if control != nil {
			if e := control.Close(); e != nil {
				t.Error(e)
			}
		}
		if _, e := admin.ExecContext(cleanup, "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	})
	connect := func(raw string) *sql.DB {
		t.Helper()
		u, e := url.Parse(raw)
		if e != nil {
			t.Fatal(e)
		}
		u.Path = "/" + database
		db, e := sql.Open("pgx", u.String())
		if e != nil {
			t.Fatal(e)
		}
		return db
	}
	control = connect(adminURL)
	runtime = connect(runtimeURL)
	if err = migrate.Apply(ctx, control); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := control.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	sc := Scope{uuid.NewString(), uuid.NewString()}
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'operational registry test')`, sc.TenantID)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'test site')`, sc.TenantID, sc.SiteID)
	actor := func(role string) Actor {
		t.Helper()
		sum := sha256.Sum256([]byte(uuid.NewString()))
		a := Actor{"https://operational.example", uuid.NewString(), hex.EncodeToString(sum[:])}
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'fixture',now(),now(),now()+interval '1 hour',now())`, a.SessionHash, a.Subject, a.Issuer)
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, role)
		return a
	}
	owner, supervisor, viewer := actor("owner"), actor("supervisor"), actor("viewer")
	service := New(runtime)
	id := uuid.NewString()
	cfg := Config{SourceID: id, Type: "event_api", AuthorityScope: "production_delta:asset-1", CredentialRef: "vault://plant/key", Enabled: true, AccessPolicyRevision: 1, FreshnessSeconds: 120, ClockSkewSeconds: 30}
	created, err := service.CreateOrRevise(ctx, owner, sc, cfg, "create-source", 0)
	if err != nil {
		t.Fatal(err)
	}
	if created.Revision != 1 || created.MappingRevision != 1 || created.ReviewEligible {
		t.Fatalf("initial source=%+v", created)
	}
	// The registry only accepts an exact, already persisted knowledge entity revision.
	ev := evidence.New(runtime)
	es := evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}
	ea := evidence.Actor{Issuer: owner.Issuer, Subject: owner.Subject, SessionHash: owner.SessionHash}
	evSource := evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Asset identity", Text: "Synthetic reviewed asset identity"}
	evOut, err := ev.CreateOrRevise(ctx, ea, es, evSource, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	entityID := uuid.NewString()
	entity, err := knowledge.New(runtime).CreateEntity(ctx, knowledge.Actor{Issuer: owner.Issuer, Subject: owner.Subject, SessionHash: owner.SessionHash}, knowledge.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, knowledge.EntityInput{EntityID: entityID, Type: "asset", AssetKind: "machine", Name: "Press 1", SourceID: evOut.SourceID, SourceRevision: evOut.Revision}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := service.ReviseMappings(ctx, owner, sc, id, []Mapping{{ExternalType: "asset", ExternalKey: "press-01", EntityID: entityID, EntityRevision: entity.Revision}}, "mapping-v1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 2 || updated.MappingRevision != 2 || updated.ReviewEligible {
		t.Fatalf("mapped source=%+v", updated)
	}
	if _, err = service.ReviseMappings(ctx, viewer, sc, id, nil, "viewer-map", 2); !errors.Is(err, ErrDenied) {
		t.Fatalf("viewer revised mappings: %v", err)
	}
	if _, err = service.Decide(ctx, viewer, sc, id, ReviewInput{Verdict: "approved", Reason: "not authorized"}, "viewer-review", 2); !errors.Is(err, ErrDenied) {
		t.Fatalf("viewer approved source: %v", err)
	}
	decision, err := service.Decide(ctx, supervisor, sc, id, ReviewInput{Verdict: "approved", Reason: "mapping and source authority verified"}, "approve-v2", 2)
	if err != nil {
		t.Fatal(err)
	}
	if decision.SourceRevision != 2 || decision.MappingRevision != 2 {
		t.Fatalf("review did not pin exact revisions: %+v", decision)
	}
	read, maps, err := service.Get(ctx, owner, sc, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !read.ReviewEligible || len(maps) != 1 || maps[0].EntityRevision != entity.Revision {
		t.Fatalf("read source=%+v mappings=%+v", read, maps)
	}
	replayedMap, err := service.ReviseMappings(ctx, owner, sc, id, []Mapping{{ExternalType: "asset", ExternalKey: "press-01", EntityID: entityID, EntityRevision: entity.Revision}}, "mapping-v1", 1)
	if err != nil || replayedMap.Revision != 2 {
		t.Fatalf("mapping mutation did not replay exact revision: source=%+v err=%v", replayedMap, err)
	}
	changed, err := service.ReviseMappings(ctx, owner, sc, id, maps, "mapping-v2", 2)
	if err != nil || changed.Revision != 3 || changed.ReviewEligible {
		t.Fatalf("mapping change did not invalidate current approval: source=%+v err=%v", changed, err)
	}
	currentApproval, err := service.Decide(ctx, supervisor, sc, id, ReviewInput{Verdict: "approved", Reason: "current mapping reviewed"}, "approve-v3", 3)
	if err != nil || currentApproval.SourceRevision != 3 {
		t.Fatalf("current source approval failed: review=%+v err=%v", currentApproval, err)
	}
	if _, err = service.Decide(ctx, supervisor, sc, id, ReviewInput{Verdict: "rejected", Reason: "stale proof"}, "stale-review", 2); !errors.Is(err, ErrRevision) {
		t.Fatalf("stale source revision review accepted: %v", err)
	}
	stillCurrent, _, err := service.Get(ctx, owner, sc, id, 0)
	if err != nil || !stillCurrent.ReviewEligible || stillCurrent.Review == nil || stillCurrent.Review.Revision != currentApproval.Revision {
		t.Fatalf("stale review changed current eligible approval: source=%+v err=%v", stillCurrent, err)
	}
	laterReview, err := service.Decide(ctx, supervisor, sc, id, ReviewInput{Verdict: "rejected", Reason: "superseded after the verified approval"}, "review-v4", 3)
	if err != nil || laterReview.Revision != currentApproval.Revision+1 {
		t.Fatalf("later review failed: review=%+v err=%v", laterReview, err)
	}
	replayedReview, err := service.Decide(ctx, supervisor, sc, id, ReviewInput{Verdict: "approved", Reason: "current mapping reviewed"}, "approve-v3", 3)
	if err != nil || replayedReview.Revision != currentApproval.Revision || replayedReview.CurrentRevision != laterReview.Revision {
		t.Fatalf("replay did not distinguish operation and current review revisions: replay=%+v later=%+v err=%v", replayedReview, laterReview, err)
	}
	history, _, err := service.Get(ctx, owner, sc, id, 2)
	if err != nil || history.ReviewEligible {
		t.Fatalf("historical revision reported current approval eligibility: source=%+v err=%v", history, err)
	}
	if _, err = service.CreateOrRevise(ctx, owner, sc, cfg, "stale", 1); !errors.Is(err, ErrRevision) {
		t.Fatalf("stale source CAS accepted: %v", err)
	}
	if _, err = service.CreateOrRevise(ctx, owner, sc, cfg, "create-source", 0); err != nil {
		t.Fatalf("same idempotent request did not replay: %v", err)
	}
	conflict := cfg
	conflict.AuthorityScope = "production_delta:asset-2"
	if _, err = service.CreateOrRevise(ctx, owner, sc, conflict, "create-source", 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("idempotency key accepted changed body: %v", err)
	}
	configChanged := cfg
	configChanged.AuthorityScope = "production_delta:asset-1-revised"
	if _, err = service.CreateOrRevise(ctx, owner, sc, configChanged, "source-v4", 3); err != nil {
		t.Fatalf("source config revision failed: %v", err)
	}
	if _, _, err = service.Get(ctx, owner, sc, id, 0); err != nil {
		t.Fatalf("read after divergent source/mapping revisions failed: %v", err)
	}
	var auditedSource, auditedMapping int64
	if err = control.QueryRowContext(ctx, `SELECT source_revision,mapping_revision FROM supervisor.operational_source_audit WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND event_type='mapping.read' ORDER BY created_at DESC LIMIT 1`, sc.TenantID, sc.SiteID, id).Scan(&auditedSource, &auditedMapping); err != nil {
		t.Fatal(err)
	}
	if auditedSource != 4 || auditedMapping != 3 {
		t.Fatalf("mapping-read audit bound wrong revisions: source=%d mapping=%d want source=4 mapping=3", auditedSource, auditedMapping)
	}
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now()-interval '16 minutes' WHERE token_hash=$1`, supervisor.SessionHash)
	if _, err = service.Decide(ctx, supervisor, sc, id, ReviewInput{Verdict: "rejected", Reason: "stale proof"}, "stale-auth", 4); !errors.Is(err, ErrReauthentication) {
		t.Fatalf("stale authentication accepted: %v", err)
	}
}
