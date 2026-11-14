//go:build integration

package migrate

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/onboarding"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// WIS-271 review: onboarding.auditRead runs a NOT EXISTS lookup on every polled run read. The
// planner must be able to answer it from migration 060's index instead of scanning the site's
// interview audit rows (048 gave interview_audit only its primary key).
func TestInterviewAuditReadLookupUsesIndex(t *testing.T) {
	raw := os.Getenv("SUPERVISOR_TEST_ADMIN_URL")
	if raw == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL URL required")
	}
	db, err := sql.Open("pgx", raw)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err = Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// One busy site: 40 sessions with 50 read audits each, then fresh planner statistics. All of it
	// is rolled back with the transaction.
	tenant, site := "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"
	for _, statement := range []string{
		`INSERT INTO supervisor.tenants(id,name) VALUES('` + tenant + `','audit index synthetic')`,
		`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES('` + tenant + `','` + site + `','audit index site')`,
		`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) SELECT '` + tenant + `','` + site + `','https://issuer.invalid','subject-' || g,'owner' FROM generate_series(1,40) g`,
		`INSERT INTO supervisor.interview_sessions(tenant_id,site_id,session_id,role,participant_actor_id,participant_issuer,participant_subject,status,revision)
 SELECT '` + tenant + `','` + site + `',('00000000-0000-4000-8001-' || lpad(g::text,12,'0'))::uuid,'owner',gen_random_uuid(),'https://issuer.invalid','subject-' || g,'active',1 FROM generate_series(1,40) g`,
		`INSERT INTO supervisor.interview_audit(tenant_id,site_id,session_id,actor_issuer,actor_subject,event_type)
 SELECT tenant_id,site_id,session_id,participant_issuer,participant_subject,'agent_turn.read' FROM supervisor.interview_sessions, generate_series(1,50) WHERE tenant_id='` + tenant + `'`,
		`ANALYZE supervisor.interview_audit`,
	} {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	// EXPLAIN without ANALYZE plans auditRead's exact INSERT … WHERE NOT EXISTS without running it.
	explain := func() string {
		t.Helper()
		rows, e := tx.QueryContext(ctx, "EXPLAIN (FORMAT TEXT) "+onboarding.AuditReadSQL,
			tenant, site, "00000000-0000-4000-8001-000000000007", "https://issuer.invalid", "subject-7", "agent_turn.read")
		if e != nil {
			t.Fatal(e)
		}
		defer rows.Close()
		var lines []string
		for rows.Next() {
			var line string
			if e = rows.Scan(&line); e != nil {
				t.Fatal(e)
			}
			lines = append(lines, line)
		}
		if e = rows.Err(); e != nil {
			t.Fatal(e)
		}
		return strings.Join(lines, "\n")
	}
	plan := explain()
	rangeInIndex := false
	for _, line := range strings.Split(plan, "\n") {
		rangeInIndex = rangeInIndex || strings.Contains(line, "Index Cond") && strings.Contains(line, "created_at >")
	}
	if !strings.Contains(plan, "interview_audit_actor_event_idx") || !rangeInIndex {
		t.Fatalf("audit read lookup does not use migration 060's index:\n%s", plan)
	}
	// Negative control in the same rolled-back transaction: without the index the plan cannot name it.
	if _, err = tx.ExecContext(ctx, `DROP INDEX supervisor.interview_audit_actor_event_idx`); err != nil {
		t.Fatal(err)
	}
	if plan = explain(); strings.Contains(plan, "interview_audit_actor_event_idx") {
		t.Fatalf("plan names the dropped index:\n%s", plan)
	}
}
