//go:build integration

package bootstrap

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func sqlState(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

func TestPostgresInitialOwnerBootstrap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("disposable PostgreSQL admin and runtime URLs required")
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	runtime, err := sql.Open("pgx", runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err = migrate.Apply(ctx, admin); err != nil {
		t.Fatal(err)
	}
	issuer := "https://bootstrap.example/realm"
	tenant, foreign := uuid.NewString(), uuid.NewString()
	adminExec := func(query string, args ...any) {
		t.Helper()
		if _, e := admin.ExecContext(ctx, query, args...); e != nil {
			t.Fatal(e)
		}
	}
	adminExec("INSERT INTO supervisor.tenants(id,name) VALUES($1,'bootstrap-test'),($2,'bootstrap-foreign')", tenant, foreign)
	defer func() {
		for _, table := range []string{"bootstrap_audit", "setup_invitations", "memberships", "sites", "tenants"} {
			column := "tenant_id"
			if table == "tenants" {
				column = "id"
			}
			if _, e := admin.ExecContext(context.Background(), fmt.Sprintf("DELETE FROM supervisor.%s WHERE %s IN($1,$2)", table, column), tenant, foreign); e != nil {
				t.Error(e)
			}
		}
	}()
	site := func() string {
		t.Helper()
		id := uuid.NewString()
		adminExec("INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'bootstrap site')", tenant, id)
		return id
	}
	adminService, runtimeService := Service{admin}, Service{runtime}
	issue := func(siteID, subject string) Invitation {
		t.Helper()
		inv, e := adminService.Issue(ctx, IssueRequest{TenantID: tenant, SiteID: siteID, Issuer: issuer, Subject: subject, Role: "owner", TTL: 30 * time.Minute})
		if e != nil {
			t.Fatal(e)
		}
		return inv
	}
	redeem := func(inv Invitation, subject string) (Membership, error) {
		return runtimeService.Redeem(ctx, Actor{Issuer: issuer, Subject: subject}, RedeemRequest{Token: inv.Token})
	}
	count := func(query string, args ...any) int {
		t.Helper()
		var result int
		if e := admin.QueryRowContext(ctx, query, args...).Scan(&result); e != nil {
			t.Fatal(e)
		}
		return result
	}

	boundSite := site()
	subject := uuid.NewString()
	inv := issue(boundSite, subject)
	var persistedHash string
	if err = admin.QueryRowContext(ctx, "SELECT token_hash FROM supervisor.setup_invitations WHERE id=$1", inv.ID).Scan(&persistedHash); err != nil {
		t.Fatal(err)
	}
	if persistedHash != tokenHash(inv.Token) || persistedHash == inv.Token {
		t.Fatal("invitation token not hashed")
	}
	for _, attempt := range []struct {
		actor   Actor
		request RedeemRequest
	}{
		{Actor{issuer, "wrong"}, RedeemRequest{inv.Token}},
		{Actor{"https://wrong.example", subject}, RedeemRequest{inv.Token}},
		{Actor{issuer, subject}, RedeemRequest{Token: tokenHash(inv.Token)}},
		{Actor{issuer, subject}, RedeemRequest{Token: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}},
	} {
		if _, e := runtimeService.Redeem(ctx, attempt.actor, attempt.request); !errors.Is(e, ErrInvalidInvitation) {
			t.Fatalf("unbound redemption: %v", e)
		}
	}
	// The lookup policy exposes no invitation row without a declared token hash.
	var visible int
	if e := runtime.QueryRowContext(ctx, "SELECT count(*) FROM supervisor.setup_invitations").Scan(&visible); e != nil || visible != 0 {
		t.Fatalf("invitations visible without a token declaration: %d err=%v", visible, e)
	}
	var rawRole string
	if e := runtime.QueryRowContext(ctx, "SELECT supervisor.redeem_setup_invitation($1,$2::uuid,$3::uuid)", persistedHash, tenant, boundSite).Scan(&rawRole); sqlState(e) != "P2901" {
		t.Fatalf("missing authenticated context: %v", e)
	}
	if member, e := redeem(inv, subject); e != nil || member.Role != "owner" || member.SiteID != boundSite {
		t.Fatalf("valid redemption=%+v err=%v", member, e)
	}
	if _, e := redeem(inv, subject); !errors.Is(e, ErrInvalidInvitation) {
		t.Fatalf("replay accepted %v", e)
	}
	if count("SELECT count(*) FROM supervisor.bootstrap_audit WHERE invitation_id=$1", inv.ID) != 2 {
		t.Fatal("missing issuance or redemption audit")
	}
	var audit string
	if e := admin.QueryRowContext(ctx, "SELECT string_agg(row_to_json(a)::text,'') FROM supervisor.bootstrap_audit a WHERE invitation_id=$1", inv.ID).Scan(&audit); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(audit, inv.Token) || strings.Contains(audit, persistedHash) {
		t.Fatal("token leaked into audit")
	}
	if _, e := adminService.Issue(ctx, IssueRequest{tenant, boundSite, issuer, uuid.NewString(), "owner", time.Hour}); !errors.Is(e, ErrInvalidInvitation) {
		t.Fatalf("second owner invite issued %v", e)
	}

	expiredSubject := uuid.NewString()
	expired := issue(site(), expiredSubject)
	adminExec("UPDATE supervisor.setup_invitations SET created_at=now()-interval '2 hours',expires_at=now()-interval '1 hour' WHERE id=$1", expired.ID)
	if _, e := redeem(expired, expiredSubject); !errors.Is(e, ErrInvalidInvitation) {
		t.Fatalf("expired invite accepted %v", e)
	}

	concurrentSubject := uuid.NewString()
	concurrent := issue(site(), concurrentSubject)
	results := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := redeem(concurrent, concurrentSubject); results <- e }()
	}
	wg.Wait()
	close(results)
	successes := 0
	for e := range results {
		if e == nil {
			successes++
		} else if !errors.Is(e, ErrInvalidInvitation) {
			t.Fatal(e)
		}
	}
	if successes != 1 || count("SELECT count(*) FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2", tenant, concurrent.SiteID) != 1 {
		t.Fatalf("concurrent successes %d", successes)
	}

	sharedSite := site()
	firstSubject, secondSubject := uuid.NewString(), uuid.NewString()
	first, second := issue(sharedSite, firstSubject), issue(sharedSite, secondSubject)
	pair := make(chan error, 2)
	for _, candidate := range []struct {
		inv     Invitation
		subject string
	}{{first, firstSubject}, {second, secondSubject}} {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := redeem(candidate.inv, candidate.subject); pair <- e }()
	}
	wg.Wait()
	close(pair)
	successes = 0
	for e := range pair {
		if e == nil {
			successes++
		} else if !errors.Is(e, ErrInvalidInvitation) {
			t.Fatal(e)
		}
	}
	if successes != 1 || count("SELECT count(*) FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND role='owner' AND active", tenant, sharedSite) != 1 {
		t.Fatal("different invitations created multiple initial owners")
	}

	// Runtime cannot bypass the capability function to mint invitations or insert memberships.
	tx, e := runtime.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	_, e = tx.ExecContext(ctx, "SELECT set_config('supervisor.tenant_id',$1,true)", tenant)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')", tenant, site(), issuer, uuid.NewString()); sqlState(e) != "42501" {
		t.Fatalf("runtime direct membership insert must be a privilege failure: %v", e)
	}
	_ = tx.Rollback()
	if _, e = runtimeService.Issue(ctx, IssueRequest{tenant, site(), issuer, uuid.NewString(), "owner", time.Hour}); sqlState(e) != "42501" {
		t.Fatalf("runtime invite minting must be a privilege failure: %v", e)
	}
	// Existing inactive membership cannot be silently reactivated or elevated.
	inactiveSite := site()
	inactiveSubject := uuid.NewString()
	inactive := issue(inactiveSite, inactiveSubject)
	adminExec("INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role,active) VALUES($1,$2,$3,$4,'operator',false)", tenant, inactiveSite, issuer, inactiveSubject)
	if _, e = redeem(inactive, inactiveSubject); !errors.Is(e, ErrInvalidInvitation) {
		t.Fatalf("existing inactive member elevated %v", e)
	}

	// Audit failure rolls back both membership and token consumption.
	failureSubject := uuid.NewString()
	failure := issue(site(), failureSubject)
	constraint := "bootstrap_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	adminExec(fmt.Sprintf("ALTER TABLE supervisor.bootstrap_audit ADD CONSTRAINT %s CHECK (invitation_id <> '%s'::uuid OR event_type <> 'bootstrap.owner_enrolled') NOT VALID", constraint, failure.ID))
	defer func() {
		if _, e := admin.ExecContext(context.Background(), "ALTER TABLE supervisor.bootstrap_audit DROP CONSTRAINT IF EXISTS "+constraint); e != nil {
			t.Error(e)
		}
	}()
	if _, e = redeem(failure, failureSubject); sqlState(e) != "23514" {
		t.Fatalf("injected audit failure must surface: %v", e)
	}
	if count("SELECT count(*) FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2", tenant, failure.SiteID) != 0 {
		t.Fatal("partial membership committed")
	}
	if count("SELECT count(*) FROM supervisor.setup_invitations WHERE id=$1 AND consumed_at IS NULL", failure.ID) != 1 {
		t.Fatal("failed audit consumed invite")
	}
	adminExec("ALTER TABLE supervisor.bootstrap_audit DROP CONSTRAINT " + constraint)
	if _, e = redeem(failure, failureSubject); e != nil {
		t.Fatal(e)
	}
}
