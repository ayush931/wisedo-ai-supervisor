//go:build integration

package evidence

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/google/uuid"
)

func TestPostgresDocumentCurrentness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: real PostgreSQL URLs required")
	}
	control, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })
	database := "currentness_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = control.ExecContext(ctx, `CREATE DATABASE `+database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 60*time.Second)
		defer stop()
		if _, e := control.ExecContext(cleanup, `DROP DATABASE IF EXISTS `+database+` WITH (FORCE)`); e != nil {
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
		t.Cleanup(func() { db.Close() })
		return db
	}
	admin, runtime := connect(adminURL), connect(runtimeURL)
	if err = migrate.Apply(ctx, admin); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := admin.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	sc, foreign := Scope{uuid.NewString(), uuid.NewString()}, Scope{uuid.NewString(), uuid.NewString()}
	for _, scope := range []Scope{sc, foreign} {
		exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'currentness fixture')`, scope.TenantID)
		exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'currentness fixture')`, scope.TenantID, scope.SiteID)
	}
	actor := func(role string) Actor {
		a := Actor{"https://currentness.invalid", uuid.NewString(), hash([]byte(uuid.NewString()))}
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'fixture',now(),now(),now()+interval '1 hour',now())`, a.SessionHash, a.Issuer, a.Subject)
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, role)
		return a
	}
	owner, supervisor, operator, viewer := actor("owner"), actor("supervisor"), actor("operator"), actor("viewer")
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, foreign.TenantID, foreign.SiteID, owner.Issuer, owner.Subject)
	service := New(runtime)
	src := Input{SourceID: uuid.NewString(), Kind: "document", Title: "Current SOP", Text: "Reviewed shopfloor procedure"}
	source, e := service.CreateOrRevise(ctx, operator, sc, src, uuid.NewString(), 0)
	if e != nil {
		t.Fatal(e)
	}
	authority, e := service.DecideAuthority(ctx, owner, sc, src.SourceID, AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Owner designated SOP"}, uuid.NewString(), 0)
	if e != nil {
		t.Fatal(e)
	}
	in := CurrentnessInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, AuthorityRevision: 1, AuthorityDecisionID: authority.DecisionID, Verdict: "confirm_current", Reason: "Owner verified this document against current work"}
	epoch := func() int64 {
		var n int64
		if err := admin.QueryRowContext(ctx, `SELECT revision FROM supervisor.knowledge_inventory_revisions WHERE tenant_id=$1 AND site_id=$2`, sc.TenantID, sc.SiteID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	graphEpoch := epoch()
	for _, a := range []Actor{supervisor, operator, viewer} {
		if _, e = service.DecideCurrentness(ctx, a, sc, src.SourceID, in, uuid.NewString(), 0); !errors.Is(e, ErrDenied) {
			t.Fatalf("nonowner accepted: %v", e)
		}
	}
	exec(`UPDATE supervisor.browser_sessions SET auth_time=NULL WHERE token_hash=$1`, owner.SessionHash)
	if _, e = service.DecideCurrentness(ctx, owner, sc, src.SourceID, in, uuid.NewString(), 0); !errors.Is(e, ErrReauthentication) {
		t.Fatalf("missing proof: %v", e)
	}
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash=$1`, owner.SessionHash)
	bad := in
	bad.AuthorityDecisionID = uuid.NewString()
	if _, e = service.DecideCurrentness(ctx, owner, sc, src.SourceID, bad, uuid.NewString(), 0); !errors.Is(e, ErrSourceChanged) {
		t.Fatalf("incorrect authority accepted: %v", e)
	}
	bad = in
	bad.Verdict = "withdraw"
	if _, e = service.DecideCurrentness(ctx, owner, sc, src.SourceID, bad, uuid.NewString(), 0); !errors.Is(e, ErrInvalid) {
		t.Fatalf("initial withdraw: %v", e)
	}
	key := uuid.NewString()
	before := time.Now()
	verified, e := service.DecideCurrentness(ctx, owner, sc, src.SourceID, in, key, 0)
	if e != nil {
		t.Fatal(e)
	}
	if verified.SchemaVersion != 1 || !verified.VerificationCurrent || verified.VerifiedAt == nil || verified.VerifiedAt.Before(before.Add(-time.Second)) || verified.VerifiedAt.After(time.Now().Add(time.Second)) || verified.SourceSHA256 != source.SHA256 || verified.SourceAccessEpoch != 1 || verified.AuthorityDecisionID != authority.DecisionID || verified.ActorSubject != owner.Subject {
		t.Fatalf("verification metadata: %+v", verified)
	}
	for _, a := range []Actor{owner, supervisor, operator, viewer} {
		v, err := service.ReadCurrentness(ctx, a, sc, src.SourceID, 0)
		if err != nil || v.VerificationID != verified.VerificationID {
			t.Fatalf("member read: %+v %v", v, err)
		}
	}
	replay, e := service.DecideCurrentness(ctx, owner, sc, src.SourceID, in, key, 0)
	if e != nil || replay.VerifiedAt == nil || !replay.VerifiedAt.Equal(*verified.VerifiedAt) || !replay.CreatedAt.Equal(verified.CreatedAt) {
		t.Fatalf("replay renewed verification: %+v %v", replay, e)
	}
	if epoch() != graphEpoch {
		t.Fatal("verification/read/replay changed graph epoch")
	}
	bad = in
	bad.Reason = "Changed reason"
	if _, e = service.DecideCurrentness(ctx, owner, sc, src.SourceID, bad, key, 0); !errors.Is(e, ErrConflict) {
		t.Fatalf("key conflict: %v", e)
	}
	if _, e = service.DecideCurrentness(ctx, owner, sc, src.SourceID, in, uuid.NewString(), 0); !errors.Is(e, ErrRevision) {
		t.Fatalf("CAS conflict: %v", e)
	}
	if _, e = service.ReadCurrentness(ctx, owner, foreign, src.SourceID, 0); !errors.Is(e, ErrNotFound) {
		t.Fatalf("foreign read: %v", e)
	}
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now()-interval '16 minutes' WHERE token_hash=$1`, owner.SessionHash)
	if _, e = service.DecideCurrentness(ctx, owner, sc, src.SourceID, in, key, 0); !errors.Is(e, ErrReauthentication) {
		t.Fatalf("replay bypassed proof: %v", e)
	}
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash=$1`, owner.SessionHash)
	// A replacement currentness decision cannot reactivate its predecessor.
	newer, e := service.DecideCurrentness(ctx, owner, sc, src.SourceID, in, uuid.NewString(), 1)
	if e != nil || newer.Revision != 2 {
		t.Fatalf("replacement: %+v %v", newer, e)
	}
	old, e := service.ReadCurrentness(ctx, viewer, sc, src.SourceID, 1)
	if e != nil || old.VerificationCurrent || old.VerifiedAt == nil || !old.VerifiedAt.Equal(*verified.VerifiedAt) {
		t.Fatalf("old verification renewed: %+v %v", old, e)
	}
	// Authority replacement invalidates exact prior pins even with identical source bytes.
	authority2, e := service.DecideAuthority(ctx, owner, sc, src.SourceID, AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "New authority review"}, uuid.NewString(), 1)
	if e != nil {
		t.Fatal(e)
	}
	old, e = service.ReadCurrentness(ctx, viewer, sc, src.SourceID, 0)
	if e != nil || old.VerificationCurrent {
		t.Fatalf("authority replacement ignored: %+v %v", old, e)
	}
	withdrawal := in
	withdrawal.Verdict = "withdraw"
	withdrawal.Reason = "Withdraw superseded verification"
	withdrawn, e := service.DecideCurrentness(ctx, owner, sc, src.SourceID, withdrawal, uuid.NewString(), 2)
	if e != nil || withdrawn.VerifiedAt != nil || withdrawn.VerificationCurrent {
		t.Fatalf("stale authority withdrawal: %+v %v", withdrawn, e)
	}
	if _, e = service.DecideCurrentness(ctx, owner, sc, src.SourceID, withdrawal, uuid.NewString(), 3); !errors.Is(e, ErrInvalid) {
		t.Fatalf("consecutive withdrawal: %v", e)
	}
	currentIn := in
	currentIn.AuthorityRevision = 2
	currentIn.AuthorityDecisionID = authority2.DecisionID
	if _, e = service.DecideCurrentness(ctx, owner, sc, src.SourceID, currentIn, uuid.NewString(), 3); e != nil {
		t.Fatal(e)
	}
	src.Text = "Updated shopfloor procedure"
	if _, e = service.CreateOrRevise(ctx, operator, sc, src, uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	old, e = service.ReadCurrentness(ctx, viewer, sc, src.SourceID, 0)
	if e != nil || old.VerificationCurrent {
		t.Fatalf("source replacement ignored: %+v %v", old, e)
	}
	withdrawal = currentIn
	withdrawal.Verdict = "withdraw"
	withdrawal.ExpectedSourceStateRevision = 2
	withdrawn, e = service.DecideCurrentness(ctx, owner, sc, src.SourceID, withdrawal, uuid.NewString(), 4)
	if e != nil || withdrawn.VerifiedAt != nil {
		t.Fatalf("stale source withdrawal: %+v %v", withdrawn, e)
	}
	// Audit failure must roll back head/history/receipt together.
	authority3, e := service.DecideAuthority(ctx, owner, sc, src.SourceID, AuthorityInput{SourceRevision: 2, ExpectedSourceStateRevision: 2, Verdict: "designate", Reason: "Revised authority"}, uuid.NewString(), 2)
	if e != nil {
		t.Fatal(e)
	}
	fresh := in
	fresh.SourceRevision = 2
	fresh.ExpectedSourceStateRevision = 2
	fresh.AuthorityRevision = 3
	fresh.AuthorityDecisionID = authority3.DecisionID
	blocker, e := admin.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback()
	if _, e = blocker.ExecContext(ctx, `LOCK TABLE supervisor.evidence_audit IN ACCESS EXCLUSIVE MODE`); e != nil {
		t.Fatal(e)
	}
	rollbackKey := uuid.NewString()
	short, stop := context.WithTimeout(ctx, 150*time.Millisecond)
	_, e = service.DecideCurrentness(short, owner, sc, src.SourceID, fresh, rollbackKey, 5)
	stop()
	if e == nil {
		t.Fatal("audit failure committed")
	}
	blocker.Rollback()
	var count int
	if e = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.evidence_currentness_decisions WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, sc.TenantID, sc.SiteID, src.SourceID).Scan(&count); e != nil || count != 5 {
		t.Fatalf("partial verification: %d %v", count, e)
	}
	if _, e = service.DecideCurrentness(ctx, owner, sc, src.SourceID, fresh, rollbackKey, 5); e != nil {
		t.Fatal(e)
	}
	for _, kind := range []string{"manual", "interview"} {
		unsupported := Input{SourceID: uuid.NewString(), Kind: kind, Title: "Unsupported currentness", Text: "Not a document"}
		if _, e = service.CreateOrRevise(ctx, operator, sc, unsupported, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		a, err := service.DecideAuthority(ctx, owner, sc, unsupported.SourceID, AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Designated report"}, uuid.NewString(), 0)
		if err != nil {
			t.Fatal(err)
		}
		bad = in
		bad.AuthorityDecisionID = a.DecisionID
		if _, e = service.DecideCurrentness(ctx, owner, sc, unsupported.SourceID, bad, uuid.NewString(), 0); !errors.Is(e, ErrInvalid) {
			t.Fatalf("%s accepted currentness: %v", kind, e)
		}
	}
	// Direct runtime INSERT cannot forge either server verification timestamp.
	for _, forged := range []string{"2000-01-01T00:00:00Z", "2099-01-01T00:00:00Z"} {
		tx, err := runtime.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); err != nil {
			t.Fatal(err)
		}
		var stamped, created time.Time
		started := time.Now()
		err = tx.QueryRowContext(ctx, `INSERT INTO supervisor.evidence_currentness_decisions(tenant_id,site_id,source_id,revision,verification_id,source_revision,source_sha256,source_access_epoch,authority_revision,authority_decision_id,verdict,reason,actor_issuer,actor_subject,auth_time,created_at,verified_at) SELECT tenant_id,site_id,source_id,7,$4,source_revision,source_sha256,source_access_epoch,authority_revision,authority_decision_id,'confirm_current','Forged timestamp regression',actor_issuer,actor_subject,now(),$5::timestamptz,$5::timestamptz FROM supervisor.evidence_currentness_decisions WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND revision=6 RETURNING created_at,verified_at`, sc.TenantID, sc.SiteID, src.SourceID, uuid.NewString(), forged).Scan(&created, &stamped)
		tx.Rollback()
		if err != nil || !stamped.Equal(created) || stamped.Before(started.Add(-time.Second)) || stamped.After(time.Now().Add(time.Second)) {
			t.Fatalf("forged server time survived: %s %s %v", created, stamped, err)
		}
	}
	// A real designated authority from another source revision cannot satisfy exact pins.
	fkTx, e := runtime.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = fkTx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); e != nil {
		t.Fatal(e)
	}
	_, e = fkTx.ExecContext(ctx, `INSERT INTO supervisor.evidence_currentness_decisions(tenant_id,site_id,source_id,revision,verification_id,source_revision,source_sha256,source_access_epoch,authority_revision,authority_decision_id,verdict,reason,actor_issuer,actor_subject,auth_time,created_at,verified_at) SELECT tenant_id,site_id,source_id,7,$4,source_revision,source_sha256,source_access_epoch,2,$5,'confirm_current','Mismatched authority regression',actor_issuer,actor_subject,now(),now(),now() FROM supervisor.evidence_currentness_decisions WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND revision=6`, sc.TenantID, sc.SiteID, src.SourceID, uuid.NewString(), authority2.DecisionID)
	fkTx.Rollback()
	if sqlState(e) != "23503" {
		t.Fatalf("exact authority FK failed to reject mismatch: %v", e)
	}
	// FORCE RLS also hides immutable history from an independently scoped runtime query.
	rls, e := runtime.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = rls.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, foreign.TenantID); e != nil {
		t.Fatal(e)
	}
	var visible int
	if e = rls.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.evidence_currentness_decisions WHERE tenant_id=$1`, sc.TenantID).Scan(&visible); e != nil || visible != 0 {
		t.Fatalf("currentness RLS leak: %d %v", visible, e)
	}
	rls.Rollback()
	if visible = runtimeCount(t, ctx, runtime, sc.TenantID, `SELECT count(*) FROM supervisor.evidence_currentness_decisions WHERE tenant_id=$1`, sc.TenantID); visible == 0 {
		t.Fatal("owning tenant sees no currentness history; the RLS check above is vacuous")
	}
	// Runtime cannot edit or delete server timestamps and immutable verification history.
	for _, q := range []string{`UPDATE supervisor.evidence_currentness_decisions SET verified_at=clock_timestamp() WHERE verification_id=$1`, `DELETE FROM supervisor.evidence_currentness_decisions WHERE verification_id=$1`} {
		tx, err := runtime.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); err != nil {
			t.Fatal(err)
		}
		_, err = tx.ExecContext(ctx, q, verified.VerificationID)
		tx.Rollback()
		if sqlState(err) != "42501" {
			t.Fatalf("runtime %q: want permission denied, got %v", q, err)
		}
	}
	if _, e = service.Revoke(ctx, owner, sc, src.SourceID, "Withdraw source access", uuid.NewString(), 2); e != nil {
		t.Fatal(e)
	}
	for _, revision := range []int64{0, 1, 3, 5} {
		if _, e = service.ReadCurrentness(ctx, viewer, sc, src.SourceID, revision); !errors.Is(e, ErrNotFound) {
			t.Fatalf("revoked read%d: %v", revision, e)
		}
	}
	if _, e = service.DecideCurrentness(ctx, owner, sc, src.SourceID, in, key, 0); !errors.Is(e, ErrNotFound) {
		t.Fatalf("revoked replay: %v", e)
	}
}
