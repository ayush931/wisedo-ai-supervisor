//go:build integration

package evidence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/google/uuid"
)

func TestPostgresSourceAuthorityDecisions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if os.Getenv("SUPERVISOR_TEST_ADMIN_URL") == "" || os.Getenv("SUPERVISOR_TEST_DATABASE_URL") == "" {
		t.Fatal("BLOCKED: real PostgreSQL URLs required")
	}
	admin, err := sql.Open("pgx", os.Getenv("SUPERVISOR_TEST_ADMIN_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	runtime, err := sql.Open("pgx", os.Getenv("SUPERVISOR_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err = migrate.Apply(ctx, admin); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := admin.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	sc := Scope{uuid.NewString(), uuid.NewString()}
	other := Scope{uuid.NewString(), uuid.NewString()}
	for _, scope := range []Scope{sc, other} {
		exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'authority synthetic')`, scope.TenantID)
		exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'authority synthetic')`, scope.TenantID, scope.SiteID)
	}
	actor := func(role string, recent bool) Actor {
		t.Helper()
		a := Actor{"https://authority.example", uuid.NewString(), hash([]byte(uuid.NewString()))}
		age := "1 minute"
		if !recent {
			age = "1 hour"
		}
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'test',now(),now(),now()+interval '1 hour',now()-$4::interval)`, a.SessionHash, a.Issuer, a.Subject, age)
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, role)
		return a
	}
	owner, viewer, operator, stale := actor("owner", true), actor("viewer", true), actor("operator", true), actor("owner", false)
	service := New(runtime)
	src := Input{SourceID: uuid.NewString(), Kind: "document", Title: "synthetic SOP", Text: "Verified synthetic process"}
	source, err := service.CreateOrRevise(ctx, operator, sc, src, "source", 0)
	if err != nil {
		t.Fatal(err)
	}
	in := AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Owner reviewed original SOP"}
	checkErr := func(got, want error) {
		t.Helper()
		if !errors.Is(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	for _, a := range []Actor{viewer, operator} {
		_, e := service.DecideAuthority(ctx, a, sc, src.SourceID, in, "role", 0)
		checkErr(e, ErrDenied)
	}
	_, err = service.DecideAuthority(ctx, stale, sc, src.SourceID, in, "stale", 0)
	checkErr(err, ErrReauthentication)
	_, err = service.DecideAuthority(ctx, owner, other, src.SourceID, in, "scope", 0)
	checkErr(err, ErrNotFound)
	withdrawal := in
	withdrawal.Verdict = "withdraw"
	_, err = service.DecideAuthority(ctx, owner, sc, src.SourceID, withdrawal, "initial-withdraw", 0)
	checkErr(err, ErrInvalid)
	results := make(chan error, 2)
	for i := range 2 {
		go func(i int) {
			_, e := service.DecideAuthority(ctx, owner, sc, src.SourceID, in, fmt.Sprintf("decision-%d", i), 0)
			results <- e
		}(i)
	}
	winners := 0
	for range 2 {
		e := <-results
		if e == nil {
			winners++
		} else {
			checkErr(e, ErrRevision)
		}
	}
	if winners != 1 {
		t.Fatalf("CAS winners %d", winners)
	}
	first, err := service.ReadAuthority(ctx, viewer, sc, src.SourceID, 0)
	if err != nil || !first.ReviewEvidenceEligible || first.SourceSHA256 != source.SHA256 || first.SourceAccessEpoch != 1 || first.Revision != 1 || first.ActorSubject != owner.Subject {
		t.Fatalf("first=%+v %v", first, err)
	}
	// Establish a deterministic receipt, then withdraw and replay the exact designation.
	designated, err := service.DecideAuthority(ctx, owner, sc, src.SourceID, in, "designation", 1)
	if err != nil {
		t.Fatal(err)
	}
	withdrawn, err := service.DecideAuthority(ctx, owner, sc, src.SourceID, withdrawal, "withdrawal", 2)
	if err != nil || withdrawn.ReviewEvidenceEligible {
		t.Fatalf("withdrawn=%+v %v", withdrawn, err)
	}
	replayed, err := service.DecideAuthority(ctx, owner, sc, src.SourceID, in, "designation", 1)
	if err != nil || replayed.DecisionID != designated.DecisionID || replayed.CurrentRevision != 3 || replayed.ReviewEvidenceEligible {
		t.Fatalf("withdrawn replay=%+v %v", replayed, err)
	}
	changed := in
	changed.Reason = "Changed"
	_, err = service.DecideAuthority(ctx, owner, sc, src.SourceID, changed, "designation", 1)
	checkErr(err, ErrConflict)
	_, err = service.DecideAuthority(ctx, stale, sc, src.SourceID, in, "designation", 1)
	checkErr(err, ErrReauthentication)
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now()-interval '1 hour' WHERE token_hash=$1`, owner.SessionHash)
	_, err = service.DecideAuthority(ctx, owner, sc, src.SourceID, in, "designation", 1)
	checkErr(err, ErrReauthentication)
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now()-interval '1 minute' WHERE token_hash=$1`, owner.SessionHash)
	exec(`UPDATE supervisor.memberships SET role='operator' WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4`, sc.TenantID, sc.SiteID, owner.Issuer, owner.Subject)
	_, err = service.DecideAuthority(ctx, owner, sc, src.SourceID, in, "designation", 1)
	checkErr(err, ErrDenied)
	exec(`UPDATE supervisor.memberships SET role='owner' WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4`, sc.TenantID, sc.SiteID, owner.Issuer, owner.Subject)
	redesignated, err := service.DecideAuthority(ctx, owner, sc, src.SourceID, in, "redesignation", 3)
	if err != nil || !redesignated.ReviewEvidenceEligible || redesignated.DecisionID == designated.DecisionID {
		t.Fatalf("redesignation=%+v %v", redesignated, err)
	}
	src.Text = "Revised synthetic process"
	if _, err = service.CreateOrRevise(ctx, operator, sc, src, "source-revision", 1); err != nil {
		t.Fatal(err)
	}
	current, err := service.ReadAuthority(ctx, viewer, sc, src.SourceID, 0)
	if err != nil || current.ReviewEvidenceEligible {
		t.Fatalf("stale pin=%+v %v", current, err)
	}
	_, err = service.DecideAuthority(ctx, owner, sc, src.SourceID, in, "stale-source", 4)
	checkErr(err, ErrSourceChanged)
	fresh := AuthorityInput{SourceRevision: 2, ExpectedSourceStateRevision: 2, Verdict: "designate", Reason: "Reviewed revision two"}
	// A failed audit must roll back head, immutable decision and receipt together.
	blocker, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = blocker.ExecContext(ctx, `LOCK TABLE supervisor.evidence_audit IN ACCESS EXCLUSIVE MODE`); err != nil {
		blocker.Rollback()
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(ctx, 150*time.Millisecond)
	_, err = service.DecideAuthority(short, owner, sc, src.SourceID, fresh, "audit-retry", 4)
	stop()
	blocker.Rollback()
	if err == nil {
		t.Fatal("audit blockage unexpectedly committed")
	}
	current, err = service.ReadAuthority(ctx, viewer, sc, src.SourceID, 0)
	if err != nil || current.Revision != 4 {
		t.Fatalf("failed mutation changed head=%+v %v", current, err)
	}
	current, err = service.DecideAuthority(ctx, owner, sc, src.SourceID, fresh, "audit-retry", 4)
	if err != nil || current.Revision != 5 || !current.ReviewEvidenceEligible {
		t.Fatalf("same-key recovery=%+v %v", current, err)
	}
	// Runtime can neither rewrite a decision nor read another tenant's authority rows.
	tx, err := runtime.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, other.TenantID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.evidence_authority_decisions`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("RLS count=%d %v", n, err)
	}
	tx.Rollback()
	if n = runtimeCount(t, ctx, runtime, sc.TenantID, `SELECT count(*) FROM supervisor.evidence_authority_decisions`); n == 0 {
		t.Fatal("owning tenant sees no authority decisions; the RLS check above is vacuous")
	}
	tx, err = runtime.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); err != nil {
		t.Fatal(err)
	}
	_, err = tx.ExecContext(ctx, `UPDATE supervisor.evidence_authority_decisions SET reason='rewritten' WHERE source_id=$1`, src.SourceID)
	tx.Rollback()
	if sqlState(err) != "42501" {
		t.Fatalf("runtime decision rewrite: want permission denied, got %v", err)
	}
	if _, err = service.Revoke(ctx, owner, sc, src.SourceID, "Source withdrawn", "revoke", 2); err != nil {
		t.Fatal(err)
	}
	_, err = service.ReadAuthority(ctx, viewer, sc, src.SourceID, 1)
	checkErr(err, ErrNotFound)
	_, err = service.DecideAuthority(ctx, owner, sc, src.SourceID, fresh, "audit-retry", 4)
	checkErr(err, ErrNotFound)
}
