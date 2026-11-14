//go:build integration

package membership

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type fixture struct {
	admin, runtime                   *sql.DB
	s                                *Service
	tenant, site, foreign, otherSite string
	a, b                             Actor
}

func setup(t *testing.T) *fixture {
	t.Helper()
	if os.Getenv("SUPERVISOR_TEST_ADMIN_URL") == "" || os.Getenv("SUPERVISOR_TEST_DATABASE_URL") == "" {
		t.Fatal("BLOCKED: real PostgreSQL URLs required")
	}
	admin, err := sql.Open("pgx", os.Getenv("SUPERVISOR_TEST_ADMIN_URL"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := sql.Open("pgx", os.Getenv("SUPERVISOR_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{admin: admin, runtime: runtime, s: New(runtime), tenant: uuid.NewString(), site: uuid.NewString(), foreign: uuid.NewString(), otherSite: uuid.NewString()}
	issuer := "https://identity.example/realm"
	for _, actor := range []*Actor{&f.a, &f.b} {
		sum := sha256.Sum256([]byte(uuid.NewString()))
		*actor = Actor{Issuer: issuer, Subject: uuid.NewString(), SessionHash: hex.EncodeToString(sum[:])}
	}
	t.Cleanup(func() {
		defer admin.Close()
		defer runtime.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, name := range []string{"membership_changes", "outbox", "audit_events", "commands", "memberships", "sites", "tenants"} {
			key := "tenant_id"
			if name == "tenants" {
				key = "id"
			}
			if _, e := admin.ExecContext(ctx, fmt.Sprintf("DELETE FROM supervisor.%s WHERE %s IN ($1,$2)", name, key), f.tenant, f.foreign); e != nil {
				t.Error(e)
			}
		}
		if _, e := admin.ExecContext(ctx, `DELETE FROM supervisor.browser_sessions WHERE subject IN ($1,$2)`, f.a.Subject, f.b.Subject); e != nil {
			t.Error(e)
		}
	})
	f.exec(t, `INSERT INTO supervisor.tenants(id,name) VALUES($1,'membership test'),($2,'foreign')`, f.tenant, f.foreign)
	f.exec(t, `INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'line'),($3,$4,'foreign')`, f.tenant, f.site, f.foreign, f.otherSite)
	for _, a := range []Actor{f.a, f.b} {
		f.exec(t, `INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, f.tenant, f.site, a.Issuer, a.Subject)
		f.exec(t, `INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'fixture',clock_timestamp(),clock_timestamp(),clock_timestamp()+interval '8 hours',clock_timestamp())`, a.SessionHash, a.Issuer, a.Subject)
	}
	return f
}
func (f *fixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := f.admin.ExecContext(ctx, q, args...); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) request(subject, role string, active bool, revision int64) Request {
	return Request{TenantID: f.tenant, SiteID: f.site, TargetIssuer: f.a.Issuer, TargetSubject: subject, Role: role, Active: active, ExpectedRevision: revision, IdempotencyKey: uuid.NewString()}
}
func rawChange(ctx context.Context, db *sql.DB, a Actor, r Request) error {
	var body []byte
	err := db.QueryRowContext(ctx, `SELECT supervisor.change_membership($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, a.SessionHash, r.TenantID, r.SiteID, a.Issuer, a.Subject, r.TargetIssuer, r.TargetSubject, r.Role, r.Active, r.ExpectedRevision, r.IdempotencyKey).Scan(&body)
	return translate(err)
}
func TestMembershipAuthorityRevisionsReplayAndAudit(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r := f.request(uuid.NewString(), "viewer", true, 0)
	type outcome struct {
		member Member
		err    error
	}
	concurrent := make(chan outcome, 8)
	for range 8 {
		go func() { m, e := f.s.Change(ctx, f.a, r); concurrent <- outcome{m, e} }()
	}
	var first Member
	for range 8 {
		result := <-concurrent
		if result.err != nil || result.member.Revision != 1 {
			t.Fatalf("grant=%v err=%v", result.member, result.err)
		}
		if first.Subject != "" && result.member != first {
			t.Fatal("concurrent idempotent result changed")
		}
		first = result.member
	}
	replay, err := f.s.Change(ctx, f.a, r)
	if err != nil || replay != first {
		t.Fatalf("replay=%v err=%v", replay, err)
	}
	changed := r
	changed.Role = "operator"
	if _, err = f.s.Change(ctx, f.a, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("body conflict=%v", err)
	}
	changed = r
	changed.ExpectedRevision = 1
	if _, err = f.s.Change(ctx, f.a, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("precondition conflict=%v", err)
	}
	changed = r
	changed.IdempotencyKey = uuid.NewString()
	if _, err = f.s.Change(ctx, f.a, changed); !errors.Is(err, ErrRevision) {
		t.Fatalf("stale revision=%v", err)
	}
	changed.ExpectedRevision = 1
	changed.Role = "operator"
	second, err := f.s.Change(ctx, f.a, changed)
	if err != nil || second.Revision != 2 {
		t.Fatalf("role change=%v %v", second, err)
	}
	rotated := f.a
	rotation := sha256.Sum256([]byte(uuid.NewString()))
	rotated.SessionHash = hex.EncodeToString(rotation[:])
	f.exec(t, `UPDATE supervisor.browser_sessions SET token_hash=$1 WHERE token_hash=$2`, rotated.SessionHash, f.a.SessionHash)
	f.a = rotated
	if replay, err = f.s.Change(ctx, f.a, r); err != nil || replay != first {
		t.Fatalf("rotated proof replay=%v %v", replay, err)
	}
	page, err := f.s.List(ctx, f.a, f.tenant, f.site, 1, "")
	if err != nil || len(page.Members) != 1 || page.NextCursor == "" {
		t.Fatalf("page=%v %v", page, err)
	}
	next, err := f.s.List(ctx, f.a, f.tenant, f.site, 1, page.NextCursor)
	if err != nil || len(next.Members) != 1 || next.Members[0].Subject == page.Members[0].Subject {
		t.Fatalf("next=%v %v", next, err)
	}
	wrong := r
	wrong.TenantID = f.foreign
	wrong.SiteID = f.otherSite
	if err = rawChange(ctx, f.runtime, f.a, wrong); !errors.Is(err, ErrDenied) {
		t.Fatalf("SQL cross tenant=%v", err)
	}
	forged := f.a
	forged.Subject = f.b.Subject
	if err = rawChange(ctx, f.runtime, forged, r); !errors.Is(err, ErrDenied) {
		t.Fatalf("SQL session binding=%v", err)
	}
	self := f.request(f.a.Subject, "viewer", true, 1)
	if err = rawChange(ctx, f.runtime, f.a, self); !errors.Is(err, ErrInvalid) {
		t.Fatalf("SQL self mutation=%v", err)
	}
	f.exec(t, `UPDATE supervisor.browser_sessions SET auth_time=clock_timestamp()-interval '16 minutes' WHERE token_hash=$1`, f.a.SessionHash)
	if _, err = f.s.Change(ctx, f.a, r); !errors.Is(err, ErrReauthentication) {
		t.Fatalf("Go stale proof=%v", err)
	}
	if err = rawChange(ctx, f.runtime, f.a, r); !errors.Is(err, ErrReauthentication) {
		t.Fatalf("SQL stale replay proof=%v", err)
	}
	if _, err = f.s.List(ctx, f.a, f.tenant, f.site, 10, ""); err != nil {
		t.Fatalf("read required recent proof: %v", err)
	}
	for _, expression := range []string{"NULL", "clock_timestamp()+interval '1 minute'"} {
		f.exec(t, "UPDATE supervisor.browser_sessions SET auth_time="+expression+" WHERE token_hash=$1", f.a.SessionHash)
		if err = rawChange(ctx, f.runtime, f.a, r); !errors.Is(err, ErrReauthentication) {
			t.Fatalf("SQL invalid proof=%v", err)
		}
	}
	f.exec(t, `UPDATE supervisor.browser_sessions SET auth_time=clock_timestamp() WHERE token_hash=$1`, f.a.SessionHash)
	// Runtime cannot bypass the narrow function or rewrite its append-only audit, even inside
	// its own tenant context where RLS alone would allow the row.
	for what, q := range map[string]string{
		"direct grant":   `INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,'` + f.a.Issuer + `','` + uuid.NewString() + `','owner')`,
		"audit deletion": `DELETE FROM supervisor.membership_changes WHERE tenant_id=$1 AND site_id=$2`,
	} {
		tx, e := f.runtime.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, f.tenant); e != nil {
			t.Fatal(e)
		}
		_, e = tx.ExecContext(ctx, q, f.tenant, f.site)
		_ = tx.Rollback()
		var state interface{ SQLState() string }
		if !errors.As(e, &state) || state.SQLState() != "42501" {
			t.Fatalf("%s: want permission denied, got %v", what, e)
		}
	}
	if _, err = f.s.Change(ctx, f.b, f.request(f.a.Subject, "viewer", true, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Change(ctx, f.a, r); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked owner replay=%v", err)
	}
	if _, err = f.s.List(ctx, f.a, f.tenant, f.site, 10, ""); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked owner list=%v", err)
	}
	var count int
	if err = f.admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.membership_changes WHERE tenant_id=$1`, f.tenant).Scan(&count); err != nil || count != 3 {
		t.Fatalf("audit count=%d %v", count, err)
	}
}
func TestConcurrentOwnersCannotRemoveLastOwner(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, pair := range [][2]Actor{{f.a, f.b}, {f.b, f.a}} {
		workers.Add(1)
		go func(pair [2]Actor) {
			defer workers.Done()
			<-start
			_, err := f.s.Change(ctx, pair[0], f.request(pair[1].Subject, "viewer", true, 1))
			results <- err
		}(pair)
	}
	close(start)
	workers.Wait()
	close(results)
	success, denied := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrDenied) {
			denied++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || denied != 1 {
		t.Fatalf("success=%d denied=%d", success, denied)
	}
	var count int
	if err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.memberships WHERE tenant_id=$1 AND role='owner' AND active`, f.tenant).Scan(&count); err != nil || count != 1 {
		t.Fatalf("owners=%d %v", count, err)
	}
}
func TestAuditFailureRollsBackMembership(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	name := "test_membership_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	f.exec(t, fmt.Sprintf("ALTER TABLE supervisor.membership_changes ADD CONSTRAINT %s CHECK(tenant_id <> '%s'::uuid) NOT VALID", name, f.tenant))
	defer f.exec(t, "ALTER TABLE supervisor.membership_changes DROP CONSTRAINT "+name)
	r := f.request(uuid.NewString(), "viewer", true, 0)
	if _, err := f.s.Change(ctx, f.a, r); err == nil {
		t.Fatal("audit failure committed")
	}
	var count int
	if err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.memberships WHERE tenant_id=$1 AND subject=$2`, f.tenant, r.TargetSubject).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial membership=%d %v", count, err)
	}
}

func TestRevocationSerializesWithInFlightAuthorizedOperation(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	operation, err := f.runtime.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer operation.Rollback()
	if _, err = operation.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, f.tenant); err != nil {
		t.Fatal(err)
	}
	// Same authorization lock acquired by store.SubmitProbe/ProcessProbe.
	var role string
	if err = operation.QueryRowContext(ctx, `SELECT role FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 FOR SHARE`, f.tenant, f.site, f.b.Issuer, f.b.Subject).Scan(&role); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, e := f.s.Change(ctx, f.a, f.request(f.b.Subject, "owner", false, 1)); result <- e }()
	// Wait for the membership operation to hold the site serialization lock.
	deadline := time.Now().Add(4 * time.Second)
	for {
		probe, err := f.admin.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = probe.ExecContext(ctx, `SELECT 1 FROM supervisor.sites WHERE tenant_id=$1 AND id=$2 FOR NO KEY UPDATE NOWAIT`, f.tenant, f.site)
		_ = probe.Rollback()
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("membership change never acquired site lock")
		}
		select {
		case e := <-result:
			t.Fatalf("revocation bypassed in-flight permission lock: %v", e)
		case <-time.After(10 * time.Millisecond):
		}
	}
	// An authorized FK write must still finish: NO KEY UPDATE is compatible with KEY SHARE.
	if _, err = operation.ExecContext(ctx, `INSERT INTO supervisor.commands(id,tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,payload_hash,payload,kind) VALUES($1,$2,$3,$4,$5,$6,'fixture','{"message":"lock compatibility"}'::jsonb,'foundation_probe')`, uuid.NewString(), f.tenant, f.site, f.b.Issuer, f.b.Subject, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if err = operation.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-result; err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Change(ctx, f.b, f.request(uuid.NewString(), "viewer", true, 0)); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked authority reused: %v", err)
	}
}
