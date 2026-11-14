//go:build integration

package roster

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/membership"
)

type fixture struct {
	ctx                              context.Context
	admin, runtime                   *sql.DB
	s                                *Service
	tenant, site, foreign, otherSite string
	issuer                           string
	line, machine, foreignLine       string
	supervisor, staleSupervisor      Principal
	owner, operatorP, outsider       Principal
	operator, otherOperator          string // assignee subjects
}

func setup(t *testing.T) *fixture {
	t.Helper()
	if os.Getenv("SUPERVISOR_TEST_ADMIN_URL") == "" || os.Getenv("SUPERVISOR_TEST_DATABASE_URL") == "" {
		t.Fatal("BLOCKED: real PostgreSQL URLs required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	admin, err := sql.Open("pgx", os.Getenv("SUPERVISOR_TEST_ADMIN_URL"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := sql.Open("pgx", os.Getenv("SUPERVISOR_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{ctx: ctx, admin: admin, runtime: runtime, s: New(runtime), tenant: uuid.NewString(), site: uuid.NewString(), foreign: uuid.NewString(), otherSite: uuid.NewString(),
		issuer: "https://roster.invalid/" + uuid.NewString(), line: uuid.NewString(), machine: uuid.NewString(), foreignLine: uuid.NewString(),
		operator: uuid.NewString(), otherOperator: uuid.NewString()}
	t.Cleanup(func() {
		defer admin.Close()
		defer runtime.Close()
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		tx, e := admin.BeginTx(c, nil)
		if e != nil {
			t.Error(e)
			return
		}
		defer tx.Rollback()
		// Replica mode skips the append-only guards so the test scope can be removed.
		statements := []string{`SET LOCAL session_replication_role = replica`}
		for _, name := range []string{"maintenance_roster_receipts", "maintenance_roster_events", "maintenance_roster_heads", "knowledge_entity_revisions", "knowledge_entity_heads", "memberships", "sites"} {
			statements = append(statements, fmt.Sprintf("DELETE FROM supervisor.%s WHERE tenant_id IN ('%s','%s')", name, f.tenant, f.foreign))
		}
		statements = append(statements, fmt.Sprintf("DELETE FROM supervisor.tenants WHERE id IN ('%s','%s')", f.tenant, f.foreign),
			fmt.Sprintf("DELETE FROM supervisor.browser_sessions WHERE issuer='%s'", f.issuer))
		for _, s := range statements {
			if _, e = tx.ExecContext(c, s); e != nil {
				t.Error(e)
				return
			}
		}
		if e = tx.Commit(); e != nil {
			t.Error(e)
		}
	})
	f.exec(t, `INSERT INTO supervisor.tenants(id,name) VALUES($1,'roster test'),($2,'foreign')`, f.tenant, f.foreign)
	f.exec(t, `INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'roster site'),($3,$4,'foreign site')`, f.tenant, f.site, f.foreign, f.otherSite)
	member := func(tenant, site, role string) (Principal, string) {
		subject := uuid.NewString()
		f.exec(t, `INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, tenant, site, f.issuer, subject, role)
		return Principal{Issuer: f.issuer, Subject: subject, SessionHash: f.session(t, subject, 0)}, subject
	}
	f.supervisor, _ = member(f.tenant, f.site, "supervisor")
	f.staleSupervisor, _ = member(f.tenant, f.site, "supervisor")
	f.exec(t, `UPDATE supervisor.browser_sessions SET auth_time=now()-interval '20 minutes' WHERE token_hash=$1`, f.staleSupervisor.SessionHash)
	f.owner, _ = member(f.tenant, f.site, "owner")
	f.operatorP, _ = member(f.tenant, f.site, "operator")
	f.outsider, _ = member(f.foreign, f.otherSite, "supervisor")
	f.exec(t, `INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'operator'),($1,$2,$3,$5,'operator')`, f.tenant, f.site, f.issuer, f.operator, f.otherOperator)
	// The knowledge inventory: a line and a machine in this site, and a line in the foreign tenant.
	f.entity(t, f.tenant, f.site, f.line, "line")
	f.entity(t, f.tenant, f.site, f.machine, "machine")
	f.entity(t, f.foreign, f.otherSite, f.foreignLine, "line")
	return f
}

func (f *fixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.admin.ExecContext(f.ctx, q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func (f *fixture) session(t *testing.T, subject string, age time.Duration) string {
	t.Helper()
	sum := sha256.Sum256([]byte(uuid.NewString()))
	hash := hex.EncodeToString(sum[:])
	f.exec(t, `INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'fixture',now(),now(),now()+interval '1 hour',now()-make_interval(secs=>$4))`, hash, f.issuer, subject, age.Seconds())
	return hash
}

// entity plants a current asset entity revision; the evidence source behind it is outside this
// test's concern, so foreign keys are skipped for the planted rows only.
func (f *fixture) entity(t *testing.T, tenant, site, id, kind string) {
	t.Helper()
	tx, err := f.admin.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	source := uuid.NewString()
	payload := `{"entity_id":"` + id + `","type":"asset","asset_kind":"` + kind + `","source_id":"` + source + `","source_revision":1}`
	for _, s := range []string{`SET LOCAL session_replication_role = replica`,
		`INSERT INTO supervisor.knowledge_entity_heads(tenant_id,site_id,id,current_revision) VALUES('` + tenant + `','` + site + `','` + id + `',1)`,
		`INSERT INTO supervisor.knowledge_entity_revisions(tenant_id,site_id,id,revision,entity_type,payload,source_id,source_revision,actor_issuer,actor_subject) VALUES('` + tenant + `','` + site + `','` + id + `',1,'asset','` + payload + `','` + source + `',1,'test','fixture')`} {
		if _, err = tx.ExecContext(f.ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) scope() Scope { return Scope{TenantID: f.tenant, SiteID: f.site} }

func (f *fixture) confirmed(t *testing.T) []Confirmed {
	t.Helper()
	tx, err := f.runtime.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(f.ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, f.tenant); err != nil {
		t.Fatal(err)
	}
	out, err := ConfirmedTx(f.ctx, tx, f.tenant, f.site, f.line)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func decision(line, subject string, d Decision, reason string) DecisionInput {
	return DecisionInput{LineID: line, AssigneeSubject: subject, Decision: d, Reason: reason}
}

// TestWIS276RosterDecisionsReplayAndRevisions drives one line/assignee pair through confirm,
// withdraw and reconfirm, with idempotent replay, key reuse and stale preconditions.
func TestWIS276RosterDecisionsReplayAndRevisions(t *testing.T) {
	f := setup(t)
	in := decision(f.line, f.operator, DecisionConfirm, "Qualified on the press line")
	key := uuid.NewString()
	first, err := f.s.Decide(f.ctx, f.supervisor, f.scope(), in, key, 0)
	if err != nil || !first.Confirmed || first.Revision != 1 || !first.AssigneeEligible || first.DecidedBy != f.supervisor.Subject ||
		first.AssigneeID != membership.ActorID(f.issuer, f.operator).String() || first.LineID != f.line {
		t.Fatalf("confirm=%+v err=%v", first, err)
	}
	replay, err := f.s.Decide(f.ctx, f.supervisor, f.scope(), in, key, 0)
	if err != nil || replay != first {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	changed := in
	changed.Reason = "A different reason"
	if _, err = f.s.Decide(f.ctx, f.supervisor, f.scope(), changed, key, 0); !errors.Is(err, ErrIdempotency) {
		t.Fatalf("same key, other body: %v", err)
	}
	if _, err = f.s.Decide(f.ctx, f.supervisor, f.scope(), in, uuid.NewString(), 0); !errors.Is(err, ErrRevision) {
		t.Fatalf("stale If-Match 0: %v", err)
	}
	if _, err = f.s.Decide(f.ctx, f.supervisor, f.scope(), in, uuid.NewString(), 1); !errors.Is(err, ErrUnchanged) {
		t.Fatalf("confirm a confirmed entry: %v", err)
	}
	if _, err = f.s.Decide(f.ctx, f.supervisor, f.scope(), decision(f.line, f.otherOperator, DecisionWithdraw, "never confirmed"), uuid.NewString(), 0); !errors.Is(err, ErrUnchanged) {
		t.Fatalf("withdraw an absent entry: %v", err)
	}
	if got := f.confirmed(t); len(got) != 1 || got[0].AssigneeID != first.AssigneeID || got[0].ConfirmedBy != membership.ActorID(f.issuer, f.supervisor.Subject).String() {
		t.Fatalf("confirmed roster=%+v", got)
	}
	withdrawn, err := f.s.Decide(f.ctx, f.supervisor, f.scope(), decision(f.line, f.operator, DecisionWithdraw, "Moved to another line"), uuid.NewString(), 1)
	if err != nil || withdrawn.Confirmed || withdrawn.Revision != 2 || withdrawn.Reason != "Moved to another line" {
		t.Fatalf("withdraw=%+v err=%v", withdrawn, err)
	}
	if got := f.confirmed(t); len(got) != 0 {
		t.Fatalf("withdrawn assignee still offered: %+v", got)
	}
	// The original replay still returns the decision it recorded, not the current state.
	if replay, err = f.s.Decide(f.ctx, f.supervisor, f.scope(), in, key, 0); err != nil || replay != first {
		t.Fatalf("replay after withdraw=%+v err=%v", replay, err)
	}
	again, err := f.s.Decide(f.ctx, f.supervisor, f.scope(), decision(f.line, f.operator, DecisionConfirm, "Back on the line"), uuid.NewString(), 2)
	if err != nil || !again.Confirmed || again.Revision != 3 {
		t.Fatalf("reconfirm=%+v err=%v", again, err)
	}
	page, err := f.s.List(f.ctx, f.owner, f.scope(), f.line, "", DefaultPageSize)
	if err != nil || len(page.Entries) != 1 || page.Entries[0] != again || page.NextCursor != "" {
		t.Fatalf("list=%+v err=%v", page, err)
	}
	var events int
	if err = f.admin.QueryRowContext(f.ctx, `SELECT count(*) FROM supervisor.maintenance_roster_events WHERE tenant_id=$1`, f.tenant).Scan(&events); err != nil || events != 3 {
		t.Fatalf("events=%d err=%v", events, err)
	}
}

// TestWIS276RosterAuthorityAndInputs: only a recently authenticated site supervisor decides, the
// line must be a current line of this site, and a confirmed assignee an active operator or
// supervisor of it. Each refusal records nothing.
func TestWIS276RosterAuthorityAndInputs(t *testing.T) {
	f := setup(t)
	inactive := uuid.NewString()
	f.exec(t, `INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role,active) VALUES($1,$2,$3,$4,'operator',false)`, f.tenant, f.site, f.issuer, inactive)
	ok := decision(f.line, f.operator, DecisionConfirm, "Qualified")
	for name, c := range map[string]struct {
		actor Principal
		scope Scope
		in    DecisionInput
		want  error
	}{
		"operator decides":          {f.operatorP, f.scope(), ok, ErrDenied},
		"owner decides":             {f.owner, f.scope(), ok, ErrDenied},
		"stale sign-in":             {f.staleSupervisor, f.scope(), ok, ErrReauthentication},
		"foreign supervisor":        {f.outsider, f.scope(), ok, ErrNotFound},
		"forged session":            {Principal{Issuer: f.issuer, Subject: f.supervisor.Subject, SessionHash: f.owner.SessionHash}, f.scope(), ok, ErrDenied},
		"machine is not a line":     {f.supervisor, f.scope(), decision(f.machine, f.operator, DecisionConfirm, "x"), ErrLine},
		"unknown line":              {f.supervisor, f.scope(), decision(uuid.NewString(), f.operator, DecisionConfirm, "x"), ErrLine},
		"other tenant's line":       {f.supervisor, f.scope(), decision(f.foreignLine, f.operator, DecisionConfirm, "x"), ErrLine},
		"owner as assignee":         {f.supervisor, f.scope(), decision(f.line, f.owner.Subject, DecisionConfirm, "x"), ErrAssignee},
		"inactive assignee":         {f.supervisor, f.scope(), decision(f.line, inactive, DecisionConfirm, "x"), ErrAssignee},
		"unknown assignee":          {f.supervisor, f.scope(), decision(f.line, uuid.NewString(), DecisionConfirm, "x"), ErrAssignee},
		"foreign tenant's assignee": {f.supervisor, f.scope(), decision(f.line, f.outsider.Subject, DecisionConfirm, "x"), ErrAssignee},
		"blank reason":              {f.supervisor, f.scope(), decision(f.line, f.operator, DecisionConfirm, "  "), ErrInvalid},
		"reason too long":           {f.supervisor, f.scope(), decision(f.line, f.operator, DecisionConfirm, strings.Repeat("r", MaxReason+1)), ErrInvalid},
		"unknown decision":          {f.supervisor, f.scope(), decision(f.line, f.operator, "approve", "x"), ErrInvalid},
		"non-canonical line":        {f.supervisor, f.scope(), decision(strings.ToUpper(f.line), f.operator, DecisionConfirm, "x"), ErrInvalid},
	} {
		if _, err := f.s.Decide(f.ctx, c.actor, c.scope, c.in, uuid.NewString(), 0); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v want %v", name, err, c.want)
		}
	}
	var rows int
	if err := f.admin.QueryRowContext(f.ctx, `SELECT count(*) FROM supervisor.maintenance_roster_heads WHERE tenant_id=$1`, f.tenant).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("refusals recorded %d rows (err=%v)", rows, err)
	}
	// Control: the same request from the supervisor succeeds, and a supervisor may confirm
	// themselves.
	if _, err := f.s.Decide(f.ctx, f.supervisor, f.scope(), ok, uuid.NewString(), 0); err != nil {
		t.Fatalf("control: %v", err)
	}
	if _, err := f.s.Decide(f.ctx, f.supervisor, f.scope(), decision(f.line, f.supervisor.Subject, DecisionConfirm, "On call"), uuid.NewString(), 0); err != nil {
		t.Fatalf("self-confirmation: %v", err)
	}
	for name, c := range map[string]struct {
		actor Principal
		want  error
	}{"operator reads": {f.operatorP, ErrDenied}, "foreign supervisor reads": {f.outsider, ErrNotFound}} {
		if _, err := f.s.List(f.ctx, c.actor, f.scope(), "", "", DefaultPageSize); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v want %v", name, err, c.want)
		}
	}
	for _, bad := range []struct {
		line, cursor string
		limit        int
	}{{"not-a-uuid", "", 10}, {"", "%%%", 10}, {"", "", 0}, {"", "", MaxPageSize + 1}} {
		if _, err := f.s.List(f.ctx, f.owner, f.scope(), bad.line, bad.cursor, bad.limit); !errors.Is(err, ErrInvalid) {
			t.Errorf("list %+v: %v", bad, err)
		}
	}
	// Paging: two entries one at a time, in assignee order, then the end.
	page, err := f.s.List(f.ctx, f.owner, f.scope(), "", "", 1)
	if err != nil || len(page.Entries) != 1 || page.NextCursor == "" {
		t.Fatalf("page 1=%+v err=%v", page, err)
	}
	next, err := f.s.List(f.ctx, f.owner, f.scope(), "", page.NextCursor, 1)
	if err != nil || len(next.Entries) != 1 || next.NextCursor != "" || next.Entries[0].AssigneeID <= page.Entries[0].AssigneeID {
		t.Fatalf("page 2=%+v err=%v", next, err)
	}
}

// TestWIS276RosterRevocationFailsClosed: a confirmation stops counting as soon as the assignee
// loses an eligible active membership or the confirming supervisor loses the supervisor role.
func TestWIS276RosterRevocationFailsClosed(t *testing.T) {
	f := setup(t)
	if _, err := f.s.Decide(f.ctx, f.supervisor, f.scope(), decision(f.line, f.operator, DecisionConfirm, "Qualified"), uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	set := func(subject, sql string) {
		t.Helper()
		f.exec(t, `UPDATE supervisor.memberships SET `+sql+` WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4`, f.tenant, f.site, f.issuer, subject)
	}
	for name, c := range map[string]struct{ subject, revoke, restore string }{
		"assignee deactivated":       {f.operator, "active=false", "active=true"},
		"assignee made owner":        {f.operator, "role='owner'", "role='operator'"},
		"confirmer deactivated":      {f.supervisor.Subject, "active=false", "active=true"},
		"confirmer demoted operator": {f.supervisor.Subject, "role='operator'", "role='supervisor'"},
	} {
		set(c.subject, c.revoke)
		if got := f.confirmed(t); len(got) != 0 {
			t.Errorf("%s: still offered %+v", name, got)
		}
		set(c.subject, c.restore)
		if got := f.confirmed(t); len(got) != 1 {
			t.Errorf("%s restored: %+v", name, got)
		}
	}
	set(f.operator, "active=false")
	page, err := f.s.List(f.ctx, f.owner, f.scope(), f.line, "", DefaultPageSize)
	if err != nil || len(page.Entries) != 1 || !page.Entries[0].Confirmed || page.Entries[0].AssigneeEligible {
		t.Fatalf("inactive assignee must stay visible as ineligible: %+v err=%v", page, err)
	}
	// A withdrawn assignee who is no longer a member can still be withdrawn from the roster.
	if _, err = f.s.Decide(f.ctx, f.supervisor, f.scope(), decision(f.line, f.operator, DecisionWithdraw, "Left the site"), uuid.NewString(), 1); err != nil {
		t.Fatalf("withdraw inactive: %v", err)
	}
}

// TestWIS276RosterHistoryIsAppendOnlyAndTenantScoped runs as the runtime role: decisions cannot be
// rewritten or deleted, a head cannot change identity or skip a revision, and another tenant's
// context sees nothing.
func TestWIS276RosterHistoryIsAppendOnlyAndTenantScoped(t *testing.T) {
	f := setup(t)
	if _, err := f.s.Decide(f.ctx, f.supervisor, f.scope(), decision(f.line, f.operator, DecisionConfirm, "Qualified"), uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	inTenant := func(tenant, q string) error {
		tx, err := f.runtime.BeginTx(f.ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(f.ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
			return err
		}
		_, err = tx.ExecContext(f.ctx, q)
		return err
	}
	for _, q := range []string{
		`UPDATE supervisor.maintenance_roster_events SET reason='rewritten'`,
		`DELETE FROM supervisor.maintenance_roster_events`,
		`DELETE FROM supervisor.maintenance_roster_heads`,
		`DELETE FROM supervisor.maintenance_roster_receipts`,
		`UPDATE supervisor.maintenance_roster_heads SET assignee_subject='someone-else'`,
		`UPDATE supervisor.maintenance_roster_heads SET revision=revision+2,confirmed=false`,
		`UPDATE supervisor.maintenance_roster_heads SET revision=revision+1`,
	} {
		if err := inTenant(f.tenant, q); err == nil {
			t.Errorf("runtime role was allowed: %s", q)
		}
	}
	for _, table := range []string{"maintenance_roster_heads", "maintenance_roster_events", "maintenance_roster_receipts"} {
		tx, err := f.runtime.BeginTx(f.ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		if _, err = tx.ExecContext(f.ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, f.foreign); err == nil {
			err = tx.QueryRowContext(f.ctx, `SELECT count(*) FROM supervisor.`+table).Scan(&n)
		}
		_ = tx.Rollback()
		if err != nil || n != 0 {
			t.Errorf("%s visible to another tenant: n=%d err=%v", table, n, err)
		}
	}
}
