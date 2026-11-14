//go:build integration

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func sqlState(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

func TestPostgresIsolationReceiptsRevocationAndRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	runtimeURL, adminURL := os.Getenv("SUPERVISOR_TEST_DATABASE_URL"), os.Getenv("SUPERVISOR_TEST_ADMIN_URL")
	if runtimeURL == "" || adminURL == "" {
		t.Fatal("real PostgreSQL admin and runtime URLs are required")
	}
	s, err := New(runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	tenant, foreign, site, otherSite := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, subject := "https://identity.example/realm", uuid.NewString()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := admin.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'test'),($2,'foreign')`, tenant, foreign)
	defer func() {
		for _, table := range []string{"outbox", "audit_events", "commands", "memberships", "sites", "tenants"} {
			key := "tenant_id"
			if table == "tenants" {
				key = "id"
			}
			if _, err := admin.ExecContext(context.Background(), fmt.Sprintf("DELETE FROM supervisor.%s WHERE %s IN ($1,$2)", table, key), tenant, foreign); err != nil {
				t.Error(err)
			}
		}
	}()
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'line'),($3,$4,'foreign')`, tenant, site, foreign, otherSite)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, tenant, site, issuer, subject)
	if err = s.VerifyRole(ctx); err != nil {
		t.Fatal(err)
	}
	privileged := &Store{DB: admin}
	if privileged.VerifyRole(ctx) == nil {
		t.Fatal("administrator accepted as runtime")
	}
	sites, err := s.Sites(ctx, issuer, subject)
	if err != nil || len(sites) != 1 || sites[0].ID != site {
		t.Fatalf("sites=%v err=%v", sites, err)
	}
	sites, err = s.Sites(ctx, "https://wrong.example", subject)
	if err != nil || len(sites) != 0 {
		t.Fatalf("wrong issuer sites=%v err=%v", sites, err)
	}
	tx, err := s.transaction(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.sites`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("scoped count=%d err=%v", count, err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.sites`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("scope leaked count=%d err=%v", count, err)
	}
	key := uuid.NewString()
	receipts := make(chan Receipt, 8)
	failures := make(chan error, 8)
	var jobs sync.WaitGroup
	for range 8 {
		jobs.Add(1)
		go func() {
			defer jobs.Done()
			r, e := s.SubmitProbe(ctx, tenant, site, issuer, subject, key, "probe")
			if e != nil {
				failures <- e
			} else {
				receipts <- r
			}
		}()
	}
	jobs.Wait()
	close(receipts)
	close(failures)
	for e := range failures {
		t.Fatal(e)
	}
	var receipt Receipt
	for r := range receipts {
		if receipt.ID != "" && receipt.ID != r.ID {
			t.Fatal("duplicate command effects")
		}
		receipt = r
	}
	if receipt.ID == "" {
		t.Fatal("no receipt")
	}
	for _, table := range []string{"commands", "audit_events", "outbox"} {
		if err = admin.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM supervisor.%s WHERE tenant_id=$1", table), tenant).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s count=%d err=%v", table, count, err)
		}
	}
	if _, err = s.SubmitProbe(ctx, tenant, site, issuer, subject, key, "changed"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed body error=%v", err)
	}
	if _, err = s.GetProbe(ctx, foreign, otherSite, issuer, subject, receipt.ID); !errors.Is(err, ErrDenied) {
		t.Fatalf("foreign access error=%v", err)
	}
	if _, err = s.GetProbe(ctx, tenant, site, issuer, subject, uuid.NewString()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing receipt error=%v", err)
	}
	// An inherited runtime login must have usable row locks, but no provisioning authority.
	role := "supervisor_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	credential := uuid.NewString()
	roleTx, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer roleTx.Rollback()
	if _, err = roleTx.ExecContext(ctx, `SELECT set_config('supervisor.test_role',$1,true),set_config('supervisor.test_credential',$2,true)`, role, credential); err != nil {
		t.Fatal(err)
	}
	if _, err = roleTx.ExecContext(ctx, `DO $$ BEGIN EXECUTE format('CREATE ROLE %I LOGIN PASSWORD %L IN ROLE supervisor_runtime',current_setting('supervisor.test_role'),current_setting('supervisor.test_credential')); END $$`); err != nil {
		t.Fatal(err)
	}
	if err = roleTx.Commit(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, e := admin.ExecContext(context.Background(), `DROP ROLE "`+role+`"`); e != nil {
			t.Error(e)
		}
	}()
	inheritedURL, err := url.Parse(runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	inheritedURL.User = url.UserPassword(role, credential)
	inherited, err := New(inheritedURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer inherited.Close()
	if err = inherited.VerifyRole(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = inherited.GetProbe(ctx, tenant, site, issuer, subject, receipt.ID); err != nil {
		t.Fatal(err)
	}
	tx, err = inherited.transaction(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE supervisor.memberships SET role='operator'`); sqlState(err) != "42501" {
		t.Fatalf("inherited role membership update must be refused by the guard: %v", err)
	}
	_ = tx.Rollback()
	tx, err = s.transaction(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE supervisor.commands SET payload='{}'::jsonb`); sqlState(err) != "42501" {
		t.Fatalf("command body mutation must be refused by the guard: %v", err)
	}
	_ = tx.Rollback()
	// A database-enforced audit failure must roll back command and outbox atomically.
	constraint := "test_audit_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	exec(fmt.Sprintf("ALTER TABLE supervisor.audit_events ADD CONSTRAINT %s CHECK(tenant_id <> '%s'::uuid) NOT VALID", constraint, tenant))
	defer func() {
		if _, e := admin.ExecContext(context.Background(), "ALTER TABLE supervisor.audit_events DROP CONSTRAINT IF EXISTS "+constraint); e != nil {
			t.Error(e)
		}
	}()
	if _, err = s.SubmitProbe(ctx, tenant, site, issuer, subject, uuid.NewString(), "audit failure"); sqlState(err) != "23514" {
		t.Fatalf("injected audit failure must surface: %v", err)
	}
	exec("ALTER TABLE supervisor.audit_events DROP CONSTRAINT " + constraint)
	if err = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.commands WHERE tenant_id=$1`, tenant).Scan(&count); err != nil || count != 1 {
		t.Fatalf("partial command count=%d err=%v", count, err)
	}
	processErrors := make(chan error, 5)
	for range 5 {
		jobs.Add(1)
		go func() {
			defer jobs.Done()
			status, e := s.ProcessProbe(ctx, tenant, receipt.ID)
			if e != nil || status != "completed" {
				processErrors <- fmt.Errorf("process status=%s err=%v", status, e)
			}
		}()
	}
	jobs.Wait()
	close(processErrors)
	for e := range processErrors {
		t.Fatal(e)
	}
	if err = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.audit_events WHERE tenant_id=$1`, tenant).Scan(&count); err != nil || count != 2 {
		t.Fatalf("duplicate audit count=%d err=%v", count, err)
	}
	pending, err := s.Pending(ctx, tenant)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	if err = s.MarkDelivered(ctx, tenant, pending[0].ID); err != nil {
		t.Fatal(err)
	}
	pending, err = s.Pending(ctx, tenant)
	if err != nil || len(pending) != 0 {
		t.Fatalf("delivered pending=%v err=%v", pending, err)
	}
	revoked, err := s.SubmitProbe(ctx, tenant, site, issuer, subject, uuid.NewString(), "revoked")
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE supervisor.memberships SET active=false WHERE tenant_id=$1`, tenant)
	if _, err = s.SubmitProbe(ctx, tenant, site, issuer, subject, key, "probe"); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked replay error=%v", err)
	}
	if status, e := s.ProcessProbe(ctx, tenant, revoked.ID); e != nil || status != "rejected" {
		t.Fatalf("revoked execution status=%s err=%v", status, e)
	}
}
