//go:build integration

package migrate

import (
	"database/sql"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/cases"
	"github.com/google/uuid"
)

type caseFixture struct {
	admin, runtime     *sql.DB
	tenant, site       string
	line, order, other string
	source             string
	ctxA, ctxA2, ctxB  string
	digest             map[string]string
}

// newCaseFixture inserts the rows cases reference (entities, contexts, operational records)
// as the superuser with FK triggers off, because a fully derived plant context needs the
// whole WIS-272 pipeline. Everything the test then does to the case tables runs as the
// runtime role with every constraint and trigger active.
func newCaseFixture(t *testing.T) *caseFixture {
	t.Helper()
	admin := newOperationalMigrationDB(t)
	f := &caseFixture{admin: admin, runtime: runtimeDB(t, admin), digest: map[string]string{}}
	f.tenant, f.site = createSite(t, admin)
	f.line, f.order, f.other, f.source = uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	f.ctxA, f.ctxA2, f.ctxB = uuid.NewString(), uuid.NewString(), uuid.NewString()
	tx, err := admin.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	setTenant(t, tx, f.tenant)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`SET LOCAL session_replication_role = replica`)
	for _, id := range []string{f.line, f.order, f.other} {
		exec(`INSERT INTO supervisor.knowledge_entity_heads(tenant_id,site_id,id,current_revision) VALUES($1,$2,$3,1)`, f.tenant, f.site, id)
	}
	window := time.Date(2026, 10, 16, 6, 0, 0, 0, time.UTC)
	for i, c := range []struct{ id, asset, digest string }{
		{f.ctxA, f.line, strings.Repeat("a", 64)},
		{f.ctxA2, f.line, strings.Repeat("c", 64)},
		{f.ctxB, f.other, strings.Repeat("b", 64)},
	} {
		f.digest[c.id] = c.digest
		snapshot := `{"readiness":"NOT_READY","digest":"` + c.digest + `","context_id":"` + c.id + `","order_id":"` + f.order + `","asset_id":"` + c.asset + `","site_id":"` + f.site + `"}`
		start := window.Add(time.Duration(i) * 15 * time.Minute)
		exec(`INSERT INTO supervisor.plant_contexts(tenant_id,site_id,id,order_id,asset_id,window_start,window_end,process_snapshot_id,process_snapshot_sha256,readiness,snapshot,digest,actor_issuer,actor_subject,evaluated_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'NOT_READY',$10,$11,'test','operator',$7)`,
			f.tenant, f.site, c.id, f.order, c.asset, start, start.Add(15*time.Minute), uuid.NewString(), strings.Repeat("d", 64), snapshot, c.digest)
	}
	for seq := 1; seq <= 3; seq++ {
		exec(`INSERT INTO supervisor.operational_event_records(tenant_id,site_id,source_id,source_revision,mapping_revision,source_sequence,external_key,revision,event_kind,event_at,payload,payload_sha256,event_sha256,batch_id)
 VALUES($1,$2,$3,1,1,$4,$5,1,'production_delta',$6,'{}',$7,$7,$8)`,
			f.tenant, f.site, f.source, seq, "delta-"+uuid.NewString(), window, strings.Repeat("e", 64), uuid.NewString())
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return f
}

// plant inserts one row as the superuser with triggers off, to reach a guard behind another.
func (f *caseFixture) plant(t *testing.T, query, id string) {
	t.Helper()
	tx, err := f.admin.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	setTenant(t, tx, f.tenant)
	for _, s := range []string{`SET LOCAL session_replication_role = replica`} {
		if _, err = tx.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.Exec(query, f.tenant, f.site, id); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// run executes statements as the runtime role in one tenant transaction and returns the
// first error, including a commit-time (deferred) failure.
func (f *caseFixture) run(t *testing.T, statements ...func(*sql.Tx) error) error {
	t.Helper()
	tx, err := f.runtime.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`SELECT set_config('supervisor.tenant_id',$1,true)`, f.tenant); err != nil {
		t.Fatal(err)
	}
	for _, s := range statements {
		if err = s(tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func q(query string, args ...any) func(*sql.Tx) error {
	return func(tx *sql.Tx) error { _, err := tx.Exec(query, args...); return err }
}

func (f *caseFixture) newCase(id, line, ctx, digest string, episode int, previous any, evidence ...int) []func(*sql.Tx) error {
	steps := []func(*sql.Tx) error{
		q(`INSERT INTO supervisor.cases(tenant_id,site_id,id,line_id,order_id,exception_kind,episode,previous_case_id,revision,context_snapshot_id,context_revision,context_digest,state,detected_at,latest_window_end,workflow_id)
 VALUES($1,$2,$3,$4,$5,'production_shortfall',$6,$7,1,$8,1,$9,'detected',now(),now(),$10)`,
			f.tenant, f.site, id, line, f.order, episode, previous, ctx, digest, "case:"+f.tenant+":"+id),
		q(`INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,actor_issuer,actor_subject) VALUES($1,$2,$3,1,'none','detected','trigger','svc','ingestion')`, f.tenant, f.site, id),
	}
	for _, seq := range evidence {
		steps = append(steps, q(`INSERT INTO supervisor.case_trigger_evidence(tenant_id,site_id,case_id,source_id,source_sequence) VALUES($1,$2,$3,$4,$5)`, f.tenant, f.site, id, f.source, seq))
	}
	return steps
}

func (f *caseFixture) move(id string, revision int, from, to string, set string, args ...any) []func(*sql.Tx) error {
	updateArgs := append([]any{f.tenant, f.site, id, to, revision}, args...)
	return []func(*sql.Tx) error{
		q(`INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,reason,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,$6,'test','because','idp','supervisor')`, f.tenant, f.site, id, revision, from, to),
		q(`UPDATE supervisor.cases SET state=$4,revision=$5`+set+` WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, updateArgs...),
	}
}

func wantState(t *testing.T, err error, code, what string) {
	t.Helper()
	if sqlState(err) != code {
		t.Fatalf("%s: want SQLSTATE %s, got %v", what, code, err)
	}
}

func TestExceptionCaseStorageEnforcesTheLifecycle(t *testing.T) {
	f := newCaseFixture(t)
	c1 := uuid.NewString()
	if err := f.run(t, f.newCase(c1, f.line, f.ctxA, f.digest[f.ctxA], 1, nil, 1, 2)...); err != nil {
		t.Fatalf("valid case: %v", err)
	}

	// Context binding: the line, order and digest must be the cited context's.
	wantState(t, f.run(t, f.newCase(uuid.NewString(), f.other, f.ctxA, f.digest[f.ctxA], 1, nil, 1)...), "23503", "context for another line")
	wantState(t, f.run(t, f.newCase(uuid.NewString(), f.other, f.ctxB, strings.Repeat("f", 64), 1, nil, 1)...), "23503", "digest the context never had")
	// Evidence: 1..100 real records, checked at commit.
	wantState(t, f.run(t, f.newCase(uuid.NewString(), f.other, f.ctxB, f.digest[f.ctxB], 1, nil)...), "23514", "case without trigger evidence")
	wantState(t, f.run(t, f.newCase(uuid.NewString(), f.other, f.ctxB, f.digest[f.ctxB], 1, nil, 99)...), "23503", "evidence that is not a record")
	// One active episode per key.
	wantState(t, f.run(t, f.newCase(uuid.NewString(), f.line, f.ctxA, f.digest[f.ctxA], 2, c1, 3)...), "23505", "second active episode")
	// Creation always starts detected.
	bad := uuid.NewString()
	wantState(t, f.run(t, q(`INSERT INTO supervisor.cases(tenant_id,site_id,id,line_id,order_id,exception_kind,episode,revision,context_snapshot_id,context_revision,context_digest,state,detected_at,latest_window_end,workflow_id)
 VALUES($1,$2,$3,$4,$5,'production_shortfall',1,1,$6,1,$7,'investigating',now(),now(),$8)`, f.tenant, f.site, bad, f.other, f.order, f.ctxB, f.digest[f.ctxB], "case:"+f.tenant+":"+bad)), "23514", "case created past detected")

	// With trigger evidence present, only the insert guard can reject a non-detected start.
	bad2 := uuid.NewString()
	wantState(t, f.run(t,
		q(`INSERT INTO supervisor.cases(tenant_id,site_id,id,line_id,order_id,exception_kind,episode,revision,context_snapshot_id,context_revision,context_digest,state,detected_at,latest_window_end,workflow_id)
 VALUES($1,$2,$3,$4,$5,'production_shortfall',1,2,$6,1,$7,'detected',now(),now(),$8)`, f.tenant, f.site, bad2, f.other, f.order, f.ctxB, f.digest[f.ctxB], "case:"+f.tenant+":"+bad2),
		q(`INSERT INTO supervisor.case_trigger_evidence(tenant_id,site_id,case_id,source_id,source_sequence) VALUES($1,$2,$3,$4,1)`, f.tenant, f.site, bad2, f.source)), "23514", "case created at revision 2")

	// Legal transition with its history row.
	if err := f.run(t, f.move(c1, 2, "detected", "investigating", "")...); err != nil {
		t.Fatalf("detected -> investigating: %v", err)
	}
	// Fences: no update without a transition, no skipped revision, no illegal pair, no self-loop.
	wantState(t, f.run(t, q(`UPDATE supervisor.cases SET state='needs_input',revision=3 WHERE id=$1`, c1)), "23514", "update without a transition row")
	wantState(t, f.run(t, f.move(c1, 4, "investigating", "needs_input", "")...), "40001", "skipped revision")
	// The history row alone must continue the case: right revision and right from_state.
	wantState(t, f.run(t, q(`INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,actor_issuer,actor_subject) VALUES($1,$2,$3,4,'investigating','needs_input','test','idp','s')`, f.tenant, f.site, c1)), "40001", "transition row skipping a revision")
	wantState(t, f.run(t, q(`INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,actor_issuer,actor_subject) VALUES($1,$2,$3,3,'needs_input','investigating','test','idp','s')`, f.tenant, f.site, c1)), "40001", "transition row from the wrong state")
	wantState(t, f.run(t, f.move(c1, 3, "investigating", "resolved", ", resolution_reason='done'")...), "23514", "investigating -> resolved")
	wantState(t, f.run(t, q(`UPDATE supervisor.cases SET line_id=$2 WHERE id=$1`, c1, f.other)), "42501", "identity change")
	// The context changes only when a clarification or resume returns to investigating.
	wantState(t, f.run(t, f.move(c1, 3, "investigating", "needs_input", ", context_snapshot_id=$6, context_digest=$7", f.ctxA2, f.digest[f.ctxA2])...), "23514", "context swap outside clarification")
	if err := f.run(t, f.move(c1, 3, "investigating", "needs_input", "")...); err != nil {
		t.Fatal(err)
	}
	if err := f.run(t, f.move(c1, 4, "needs_input", "investigating", ", context_snapshot_id=$6, context_digest=$7", f.ctxA2, f.digest[f.ctxA2])...); err != nil {
		t.Fatalf("clarification with a new context: %v", err)
	}
	if err := f.run(t, f.move(c1, 5, "investigating", "escalated", "")...); err != nil {
		t.Fatal(err)
	}
	wantState(t, f.run(t, f.move(c1, 6, "escalated", "escalated", "")...), "23514", "escalated self-loop")
	if err := f.run(t, f.move(c1, 6, "escalated", "canceled", "")...); err != nil {
		t.Fatalf("cancel escalated: %v", err)
	}

	// Terminal episodes and history are immutable.
	wantState(t, f.run(t, f.move(c1, 7, "canceled", "detected", "")...), "23514", "leaving a terminal state")
	wantState(t, f.run(t, q(`UPDATE supervisor.cases SET owner_user_id=$2 WHERE id=$1`, c1, uuid.NewString())), "42501", "editing a terminal episode")
	wantState(t, f.run(t, q(`DELETE FROM supervisor.case_transitions WHERE case_id=$1`, c1)), "42501", "history deletion")
	wantState(t, f.run(t, q(`DELETE FROM supervisor.case_trigger_evidence WHERE case_id=$1`, c1)), "42501", "evidence deletion")

	// Reopen is a new episode that names the old one; the key is free once the old one ended.
	wantState(t, f.run(t, f.newCase(uuid.NewString(), f.line, f.ctxA2, f.digest[f.ctxA2], 2, nil, 3)...), "23514", "episode 2 without its predecessor")
	if err := f.run(t, f.newCase(uuid.NewString(), f.line, f.ctxA2, f.digest[f.ctxA2], 2, c1, 3)...); err != nil {
		t.Fatalf("reopen as episode 2: %v", err)
	}

	// Even with a history row planted past the fences, an update must advance by exactly one.
	c2 := uuid.NewString()
	if err := f.run(t, f.newCase(c2, f.other, f.ctxB, f.digest[f.ctxB], 1, nil, 1)...); err != nil {
		t.Fatal(err)
	}
	f.plant(t, `INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,actor_issuer,actor_subject) VALUES($1,$2,$3,3,'detected','investigating','planted','idp','s')`, c2)
	wantState(t, f.run(t, q(`UPDATE supervisor.cases SET state='investigating',revision=3 WHERE id=$1`, c2)), "40001", "update skipping a revision")

	// The owning tenant sees its rows; another tenant sees nothing.
	visible := func(tenant string) int {
		t.Helper()
		tx, err := f.runtime.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err = tx.Exec(`SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
			t.Fatal(err)
		}
		var n int
		if err = tx.QueryRow(`SELECT (SELECT count(*) FROM supervisor.cases)+(SELECT count(*) FROM supervisor.case_transitions)+(SELECT count(*) FROM supervisor.case_trigger_evidence)`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := visible(f.tenant); n == 0 {
		t.Fatal("owning tenant sees no case rows; the cross-tenant check below would be vacuous")
	}
	if n := visible(uuid.NewString()); n != 0 {
		t.Fatalf("cross-tenant rows visible: %d", n)
	}
}

// The SQL CHECK lists exactly the Go state machine's pairs, plus the creation row.
func TestExceptionCaseTransitionPairsMatchGo(t *testing.T) {
	db := newOperationalMigrationDB(t)
	var def string
	if err := db.QueryRow(`SELECT pg_get_constraintdef(c.oid) FROM pg_constraint c JOIN pg_class r ON r.oid=c.conrelid
 WHERE r.relname='case_transitions' AND c.contype='c' AND pg_get_constraintdef(c.oid) LIKE '%awaiting_approval%'`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range regexp.MustCompile(`from_state = '([a-z_]+)'::text\) AND \(to_state = '([a-z_]+)'::text`).FindAllStringSubmatch(def, -1) {
		got = append(got, m[1]+">"+m[2])
	}
	want := []string{"none>detected"}
	for _, p := range cases.Pairs() {
		want = append(want, string(p.From)+">"+string(p.To))
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("SQL pairs differ from internal/cases:\nsql: %v\ngo:  %v", got, want)
	}
}
