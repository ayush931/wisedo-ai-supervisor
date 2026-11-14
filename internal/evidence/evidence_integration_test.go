//go:build integration

package evidence

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
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPostgresProvenanceAuthority(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL admin/runtime URLs required")
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
	// Immutable evidence intentionally remains until the disposable test database is destroyed.
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := admin.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if e := admin.QueryRowContext(ctx, q, args...).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	scope := Scope{uuid.NewString(), uuid.NewString()}
	other := Scope{uuid.NewString(), uuid.NewString()}
	for _, sc := range []Scope{scope, other} {
		exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic evidence test')`, sc.TenantID)
		exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'synthetic evidence line')`, sc.TenantID, sc.SiteID)
	}
	issuer := "https://evidence.example/realm"
	actor := func(role string, recent bool) Actor {
		t.Helper()
		a := Actor{issuer, uuid.NewString(), hash([]byte(uuid.NewString()))}
		age := "1 minute"
		if !recent {
			age = "1 hour"
		}
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'synthetic',now(),now(),now()+interval '1 hour',now()-$4::interval)`, a.SessionHash, a.Issuer, a.Subject, age)
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, scope.TenantID, scope.SiteID, a.Issuer, a.Subject, role)
		return a
	}
	owner, operator, viewer, reviewer, staleOwner := actor("owner", true), actor("operator", true), actor("viewer", true), actor("reviewer", true), actor("owner", false)
	s := New(runtime)
	in := Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Synthetic observation", Text: "Aé😀中Z"}
	source, err := s.CreateOrRevise(ctx, operator, scope, in, "create", 0)
	if err != nil {
		t.Fatal(err)
	}
	if source.Revision != 1 || source.StateRevision != 1 || source.AccessEpoch != 1 || source.Authority != "reported" || source.SHA256 != hash([]byte(in.Text)) || source.ByteLength != len(in.Text) || source.RuneLength != utf8.RuneCountInString(in.Text) {
		t.Fatalf("incorrect provenance %+v", source)
	}
	if got, e := s.CreateOrRevise(ctx, operator, scope, in, "create", 0); e != nil || got.Revision != 1 {
		t.Fatalf("replay=%+v %v", got, e)
	}
	changed := in
	changed.Text = "different"
	if _, e := s.CreateOrRevise(ctx, operator, scope, changed, "create", 0); !errors.Is(e, ErrConflict) {
		t.Fatalf("body conflict=%v", e)
	}
	if _, e := s.CreateOrRevise(ctx, operator, scope, in, "create", 1); !errors.Is(e, ErrConflict) {
		t.Fatalf("precondition conflict=%v", e)
	}
	for _, a := range []Actor{viewer, reviewer} {
		if _, e := s.CreateOrRevise(ctx, a, scope, Input{SourceID: uuid.NewString(), Kind: "manual", Title: "t", Text: "x"}, "denied", 0); !errors.Is(e, ErrDenied) {
			t.Fatalf("role writes=%v", e)
		}
	}
	if got, e := s.Read(ctx, viewer, scope, in.SourceID, 0); e != nil || got.Text != in.Text {
		t.Fatalf("viewer read %v", e)
	}
	span, err := s.CreateSpan(ctx, operator, scope, SpanInput{in.SourceID, 1, 1, 4}, "span", 1)
	if err != nil || span.Text != "é😀中" || span.SourceSHA256 != source.SHA256 {
		t.Fatalf("Unicode span=%+v %v", span, err)
	}
	if _, e := s.CreateSpan(ctx, operator, scope, SpanInput{in.SourceID, 1, 0, 6}, "invalid-span", 1); !errors.Is(e, ErrInvalid) {
		t.Fatalf("bounds=%v", e)
	}
	if got, e := s.ReadSpan(ctx, viewer, scope, span.ID); e != nil || got.Text != span.Text {
		t.Fatalf("span read=%v", e)
	}
	if _, e := s.Read(ctx, operator, other, in.SourceID, 1); !errors.Is(e, ErrNotFound) {
		t.Fatalf("foreign scope=%v", e)
	}
	otherSite := Scope{scope.TenantID, uuid.NewString()}
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'another site')`, otherSite.TenantID, otherSite.SiteID)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, otherSite.TenantID, otherSite.SiteID, owner.Issuer, owner.Subject)
	if _, e := s.Read(ctx, owner, otherSite, in.SourceID, 1); !errors.Is(e, ErrNotFound) {
		t.Fatalf("cross-site source=%v", e)
	}
	if _, e := s.ReadSpan(ctx, owner, otherSite, span.ID); !errors.Is(e, ErrNotFound) {
		t.Fatalf("cross-site span=%v", e)
	}
	// Only one writer wins the same state revision; no duplicate revision is produced.
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, body := range []string{"revision two a", "revision two b"} {
		wg.Add(1)
		go func(body string) {
			defer wg.Done()
			v := in
			v.Text = body
			_, e := s.CreateOrRevise(ctx, operator, scope, v, body, 1)
			results <- e
		}(body)
	}
	wg.Wait()
	close(results)
	success := 0
	for e := range results {
		if e == nil {
			success++
		} else if !errors.Is(e, ErrRevision) {
			t.Fatal(e)
		}
	}
	if success != 1 {
		t.Fatalf("concurrent successes=%d", success)
	}
	if old, e := s.Read(ctx, viewer, scope, in.SourceID, 1); e != nil || old.Text != in.Text {
		t.Fatal("immutable revision changed", e)
	}
	if page, e := s.List(ctx, viewer, scope, 1, ""); e != nil || len(page.Sources) != 1 || page.Sources[0].Revision != 2 {
		t.Fatalf("metadata list=%+v %v", page, e)
	}
	// Membership removal precedes replay and prevents formerly authorized content access.
	exec(`UPDATE supervisor.memberships SET active=false WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4`, scope.TenantID, scope.SiteID, operator.Issuer, operator.Subject)
	if _, e := s.CreateOrRevise(ctx, operator, scope, in, "create", 0); !errors.Is(e, ErrNotFound) {
		t.Fatalf("removed member replay=%v", e)
	}
	if _, e := s.Revoke(ctx, staleOwner, scope, in.SourceID, "stale proof", "revoke-stale", 2); !errors.Is(e, ErrReauthentication) {
		t.Fatalf("stale revoke=%v", e)
	}
	if _, e := s.Revoke(ctx, viewer, scope, in.SourceID, "unauthorized", "revoke-viewer", 2); !errors.Is(e, ErrDenied) {
		t.Fatalf("viewer revoke=%v", e)
	}
	revoked, err := s.Revoke(ctx, owner, scope, in.SourceID, "Source permission withdrawn", "revoke", 2)
	if err != nil || !revoked.Revoked || revoked.StateRevision != 3 || revoked.AccessEpoch != 2 {
		t.Fatalf("revoke=%+v %v", revoked, err)
	}
	if got, e := s.Revoke(ctx, owner, scope, in.SourceID, "Source permission withdrawn", "revoke", 2); e != nil || got != revoked {
		t.Fatalf("revoke replay=%+v %v", got, e)
	}
	for _, a := range []Actor{owner, viewer} {
		if _, e := s.Read(ctx, a, scope, in.SourceID, 1); !errors.Is(e, ErrNotFound) {
			t.Fatalf("revoked content=%v", e)
		}
		if _, e := s.ReadSpan(ctx, a, scope, span.ID); !errors.Is(e, ErrNotFound) {
			t.Fatalf("cached span=%v", e)
		}
	}
	if _, e := s.CreateSpan(ctx, owner, scope, SpanInput{in.SourceID, 1, 1, 4}, "span-revoked", 3); !errors.Is(e, ErrNotFound) {
		t.Fatalf("revoked new span=%v", e)
	}
	if _, e := s.CreateOrRevise(ctx, owner, scope, in, "recreate", 3); !errors.Is(e, ErrNotFound) {
		t.Fatalf("revoked source restored=%v", e)
	}
	if page, e := s.List(ctx, owner, scope, 10, ""); e != nil || len(page.Sources) != 0 {
		t.Fatalf("revoked listed=%+v %v", page, e)
	}
	if count(`SELECT count(*) FROM supervisor.evidence_revisions WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, scope.TenantID, scope.SiteID, in.SourceID) != 2 {
		t.Fatal("history lost")
	}
	if count(`SELECT count(*) FROM supervisor.evidence_revocations WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, scope.TenantID, scope.SiteID, in.SourceID) != 1 {
		t.Fatal("missing authority ledger")
	}
	var auditJSON string
	if e := admin.QueryRowContext(ctx, `SELECT string_agg(row_to_json(a)::text,'') FROM supervisor.evidence_audit a WHERE tenant_id=$1`, scope.TenantID).Scan(&auditJSON); e != nil {
		t.Fatal(e)
	}
	for _, raw := range []string{in.Text, span.Text, owner.SessionHash, operator.SessionHash} {
		if strings.Contains(auditJSON, raw) {
			t.Fatal("content or session hash leaked into audit")
		}
	}
	if count(`SELECT count(*) FROM supervisor.evidence_audit WHERE tenant_id=$1 AND event_type='source.read'`, scope.TenantID) == 0 || count(`SELECT count(*) FROM supervisor.evidence_audit WHERE tenant_id=$1 AND event_type='span.read'`, scope.TenantID) == 0 {
		t.Fatal("reads lack audit")
	}

	// Revocation ledger remains authoritative even if the mutable head projection lags.
	lagged := Input{SourceID: uuid.NewString(), Kind: "document", Title: "Synthetic ledger projection test", Text: "private text"}
	lagSource, e := s.CreateOrRevise(ctx, owner, scope, lagged, "lagged-create", 0)
	if e != nil {
		t.Fatal(e)
	}
	lagTx, e := admin.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer lagTx.Rollback()
	if _, e = lagTx.ExecContext(ctx, "SELECT set_config('supervisor.tenant_id',$1,true)", scope.TenantID); e != nil {
		t.Fatal(e)
	}
	if _, e = lagTx.ExecContext(ctx, `INSERT INTO supervisor.evidence_revocations(tenant_id,site_id,source_id,state_revision,access_epoch,actor_issuer,actor_subject,reason) VALUES($1,$2,$3,2,2,$4,$5,'projection lag fixture')`, scope.TenantID, scope.SiteID, lagged.SourceID, owner.Issuer, owner.Subject); e != nil {
		t.Fatal(e)
	}
	if e = lagTx.Commit(); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Read(ctx, owner, scope, lagged.SourceID, lagSource.Revision); !errors.Is(e, ErrNotFound) {
		t.Fatalf("stale head restored ledger-revoked source: %v", e)
	}
	if page, e := s.List(ctx, owner, scope, 10, ""); e != nil || len(page.Sources) != 0 {
		t.Fatalf("ledger revoked listed=%+v %v", page, e)
	}
	// Runtime has no history UPDATE/DELETE capability and SQL rejects forged digest/span bounds.
	tx, e := runtime.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, scope.TenantID); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.ExecContext(ctx, `UPDATE supervisor.evidence_revisions SET body='changed' WHERE tenant_id=$1`, scope.TenantID); sqlState(e) != "42501" {
		t.Fatalf("runtime history rewrite: want permission denied, got %v", e)
	}
	_ = tx.Rollback()
	tx, e = runtime.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	_, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, other.TenantID)
	if e != nil {
		t.Fatal(e)
	}
	var n int
	if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.evidence_revisions WHERE tenant_id=$1`, scope.TenantID).Scan(&n); e != nil || n != 0 {
		t.Fatalf("RLS=%d %v", n, e)
	}
	_ = tx.Rollback()
	if n = runtimeCount(t, ctx, runtime, scope.TenantID, `SELECT count(*) FROM supervisor.evidence_revisions WHERE tenant_id=$1`, scope.TenantID); n == 0 {
		t.Fatal("owning tenant sees no revisions; the RLS check above is vacuous")
	}
	tx, e = admin.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO supervisor.evidence_spans(tenant_id,site_id,id,source_id,revision,start_rune,end_rune,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,1,0,999,$5,$6)`, scope.TenantID, scope.SiteID, uuid.NewString(), in.SourceID, owner.Issuer, owner.Subject); e == nil {
		t.Fatal("DB accepted out-of-range span")
	}
	_ = tx.Rollback()
	tx, e = admin.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO supervisor.evidence_revisions(tenant_id,site_id,source_id,revision,kind,title,body,sha256,byte_length,rune_length,actor_issuer,actor_subject) VALUES($1,$2,$3,2,'document','t','x',$4,1,1,$5,$6)`, scope.TenantID, scope.SiteID, lagged.SourceID, strings.Repeat("0", 64), owner.Issuer, owner.Subject); e == nil {
		t.Fatal("DB accepted forged source hash")
	}
	_ = tx.Rollback()

	// Pagination returns metadata in a stable UUID order with no duplicates or body field.
	for _, id := range []string{uuid.NewString(), uuid.NewString(), uuid.NewString()} {
		if _, e := s.CreateOrRevise(ctx, owner, scope, Input{SourceID: id, Kind: "manual", Title: "page title", Text: "page body"}, id, 0); e != nil {
			t.Fatal(e)
		}
	}
	seenIDs := map[string]bool{}
	cursor := ""
	for {
		page, e := s.List(ctx, viewer, scope, 1, cursor)
		if e != nil {
			t.Fatal(e)
		}
		for _, item := range page.Sources {
			if seenIDs[item.SourceID] {
				t.Fatal("duplicate pagination source")
			}
			seenIDs[item.SourceID] = true
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			t.Fatal("cursor did not advance")
		}
		cursor = page.NextCursor
	}
	if len(seenIDs) != 3 {
		t.Fatalf("pagination returned %d sources", len(seenIDs))
	}

	// Read-side bounds hold even for a valid larger span inserted by a privileged importer.
	long := Input{SourceID: uuid.NewString(), Kind: "document", Title: "long span fixture", Text: strings.Repeat("界", 5000)}
	if _, e := s.CreateOrRevise(ctx, owner, scope, long, long.SourceID, 0); e != nil {
		t.Fatal(e)
	}
	longSpan := uuid.NewString()
	spanTx, e := admin.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer spanTx.Rollback()
	if _, e = spanTx.ExecContext(ctx, "SELECT set_config('supervisor.tenant_id',$1,true)", scope.TenantID); e != nil {
		t.Fatal(e)
	}
	if _, e = spanTx.ExecContext(ctx, `INSERT INTO supervisor.evidence_spans(tenant_id,site_id,id,source_id,revision,start_rune,end_rune,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,1,0,4097,$5,$6)`, scope.TenantID, scope.SiteID, longSpan, long.SourceID, owner.Issuer, owner.Subject); e != nil {
		t.Fatal(e)
	}
	if e = spanTx.Commit(); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ReadSpan(ctx, viewer, scope, longSpan); !errors.Is(e, ErrInvalid) {
		t.Fatalf("read-side span cap=%v", e)
	}
	testAtomicAuditFailure(t, ctx, admin, s, owner, scope)
	testRevocationReadOrdering(t, ctx, admin, s, owner, viewer, scope)

}

// A UUID-scoped trigger fault proves the exact write transaction cannot commit without its audit.
func testAtomicAuditFailure(t *testing.T, ctx context.Context, db *sql.DB, s *Service, a Actor, sc Scope) {
	t.Helper()
	id := uuid.NewString()
	name := "test_evidence_audit_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	q := fmt.Sprintf(`CREATE FUNCTION supervisor.%s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.source_id='%s'::uuid AND NEW.event_type='source.created' THEN RAISE EXCEPTION 'synthetic audit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER %s BEFORE INSERT ON supervisor.evidence_audit FOR EACH ROW EXECUTE FUNCTION supervisor.%s()`, name, id, name, name)
	if _, e := db.ExecContext(ctx, q); e != nil {
		t.Fatal(e)
	}
	cleanup := func() {
		if _, e := db.ExecContext(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON supervisor.evidence_audit; DROP FUNCTION IF EXISTS supervisor.%s()`, name, name)); e != nil {
			t.Error(e)
		}
	}
	defer cleanup()
	in := Input{SourceID: id, Kind: "manual", Title: "atomic audit", Text: "private source text"}
	if _, e := s.CreateOrRevise(ctx, a, sc, in, id, 0); e == nil {
		t.Fatal("audit failure committed mutation")
	}
	for _, table := range []string{"evidence_heads", "evidence_revisions", "evidence_receipts", "evidence_audit"} {
		var n int
		if e := db.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.`+table+` WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, sc.TenantID, sc.SiteID, id).Scan(&n); e != nil || n != 0 {
			t.Fatalf("partial %s rows=%d err=%v", table, n, e)
		}
	}
	cleanup()
	if _, e := s.CreateOrRevise(ctx, a, sc, in, id, 0); e != nil {
		t.Fatalf("rolled back key cannot retry: %v", e)
	}
}

func testRevocationReadOrdering(t *testing.T, ctx context.Context, db *sql.DB, s *Service, owner, viewer Actor, sc Scope) {
	t.Helper()
	in := Input{SourceID: uuid.NewString(), Kind: "manual", Title: "race fixture", Text: "secret before revocation"}
	if _, e := s.CreateOrRevise(ctx, owner, sc, in, in.SourceID, 0); e != nil {
		t.Fatal(e)
	}
	name := "test_evidence_gate_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	// A session-held advisory gate blocks the audit after Revoke already holds the source head lock.
	gate, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	var gatePID int
	if err = gate.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&gatePID); err != nil {
		t.Fatal(err)
	}
	key := int64(time.Now().UnixNano() & 0x7fffffffffffffff)
	if _, err = gate.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		t.Fatal(err)
	}
	defer gate.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
	q := fmt.Sprintf(`CREATE FUNCTION supervisor.%s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.source_id='%s'::uuid AND NEW.event_type='source.revoked' THEN PERFORM pg_advisory_xact_lock(%d); END IF; RETURN NEW; END $$; CREATE TRIGGER %s BEFORE INSERT ON supervisor.evidence_audit FOR EACH ROW EXECUTE FUNCTION supervisor.%s()`, name, in.SourceID, key, name, name)
	if _, err = db.ExecContext(ctx, q); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, e := db.ExecContext(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON supervisor.evidence_audit; DROP FUNCTION IF EXISTS supervisor.%s()`, name, name)); e != nil {
			t.Error(e)
		}
	}()
	waitBlocked := func(blocker int) int {
		t.Helper()
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			var pid int
			e := db.QueryRowContext(ctx, `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)) LIMIT 1`, blocker).Scan(&pid)
			if e == nil {
				return pid
			}
			if !errors.Is(e, sql.ErrNoRows) {
				t.Fatal(e)
			}
			select {
			case <-deadline.C:
				t.Fatal("expected database lock wait not observed")
				return 0
			case <-tick.C:
			}
		}
	}
	revokeDone := make(chan error, 1)
	go func() {
		_, e := s.Revoke(ctx, owner, sc, in.SourceID, "test access withdrawal", "race-revoke", 1)
		revokeDone <- e
	}()
	revokePID := waitBlocked(gatePID)
	readDone := make(chan error, 1)
	go func() { _, e := s.Read(ctx, viewer, sc, in.SourceID, 1); readDone <- e }()
	waitBlocked(revokePID)
	if _, err = gate.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, key); err != nil {
		t.Fatal(err)
	}
	if e := <-revokeDone; e != nil {
		t.Fatal(e)
	}
	if e := <-readDone; !errors.Is(e, ErrNotFound) {
		t.Fatalf("reader waiting behind revoke retrieved content: %v", e)
	}
}

// sqlState returns the PostgreSQL SQLSTATE of err, or "" when err is not a database error.
func sqlState(err error) string {
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		return state.SQLState()
	}
	return ""
}

// runtimeCount runs a count as the runtime role inside the given tenant context.
func runtimeCount(t *testing.T, ctx context.Context, runtime *sql.DB, tenant, query string, args ...any) int {
	t.Helper()
	tx, err := runtime.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = tx.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
