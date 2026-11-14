//go:build integration

package httpapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestInterviewRunReaperReadBranches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	f := newInterviewRunReaperFixture(t, ctx)

	// Each stale window is pinned at the boundary: exactly at the threshold the run stays
	// running (strict <), one microsecond past it the run is reaped. The delivered window
	// includes the one-minute grace after the activity's schedule-to-close timeout. The
	// "recently delivered" cases start long before every window, so they stay running only
	// because the delivered_at IS NULL guard keeps the started_at branch from applying.
	delivered, undelivered := agent.InterviewTurnScheduleToClose+time.Minute, 30*time.Minute
	for _, tc := range []struct {
		name, purpose string
		startedAgo    time.Duration
		deliveredAgo  *time.Duration
		reaped        bool
	}{
		{"question delivered exactly at the delivered window", "questions", delivered + time.Minute, interviewReaperDuration(delivered), false},
		{"question delivered one microsecond past the delivered window", "questions", delivered + time.Minute, interviewReaperDuration(delivered + time.Microsecond), true},
		{"draft delivered exactly at the delivered window", "drafts", delivered + time.Minute, interviewReaperDuration(delivered), false},
		{"draft delivered one microsecond past the delivered window", "drafts", delivered + time.Minute, interviewReaperDuration(delivered + time.Microsecond), true},
		{"question undelivered started exactly at the undelivered window", "questions", undelivered, nil, false},
		{"question undelivered started one microsecond past the undelivered window", "questions", undelivered + time.Microsecond, nil, true},
		{"draft undelivered started exactly at the undelivered window", "drafts", undelivered, nil, false},
		{"draft undelivered started one microsecond past the undelivered window", "drafts", undelivered + time.Microsecond, nil, true},
		{"question started long ago but delivered recently stays running", "questions", 2 * time.Hour, interviewReaperDuration(time.Minute), false},
		{"draft started long ago but delivered recently stays running", "drafts", 2 * time.Hour, interviewReaperDuration(time.Minute), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer f.use(t)()
			sessionID := f.session()
			startedAt := f.now.Add(-tc.startedAgo)
			var deliveredAt *time.Time
			if tc.deliveredAgo != nil {
				deliveredAt = interviewReaperTime(f.now.Add(-*tc.deliveredAgo))
			}
			runID := f.run(sessionID, tc.purpose, &startedAt, deliveredAt)

			got, response := f.read(f.runPath(tc.purpose, sessionID, runID))
			if tc.reaped {
				f.wantState(response, got, agent.RunCompleted, recovery.StatusNeedsInput, agent.CodeActivityFailed)
				f.wantRun(runID, agent.RunCompleted, recovery.StatusNeedsInput, agent.CodeActivityFailed, &f.now)
			} else {
				f.wantState(response, got, agent.RunRunning, "", "")
				f.wantRun(runID, agent.RunRunning, "", "", nil)
			}
			f.wantAudit(sessionID, 1)
			// Reaping terminalizes the run only; the durable outbox row is never rewritten.
			if stored := f.outboxDeliveredAt(tc.purpose, runID); deliveredAt == nil && stored.Valid || deliveredAt != nil && (!stored.Valid || !stored.Time.Equal(*deliveredAt)) {
				t.Fatalf("outbox delivered_at=%v, want %v", stored, deliveredAt)
			}
		})
	}

	t.Run("reading one run reaps a stale sibling run in the same session", func(t *testing.T) {
		defer f.use(t)()
		sessionID := f.session()
		freshRun := f.run(sessionID, "questions", nil, nil)
		staleRun := f.run(sessionID, "drafts", interviewReaperTime(f.now.Add(-31*time.Minute)), nil)

		got, response := f.read(f.questionPath(sessionID, freshRun))
		f.wantState(response, got, agent.RunRunning, "", "")
		f.wantRun(freshRun, agent.RunRunning, "", "", nil)
		f.wantRun(staleRun, agent.RunCompleted, recovery.StatusNeedsInput, agent.CodeActivityFailed, &f.now)
	})

	t.Run("stale run with neither outbox row is reaped from its start time", func(t *testing.T) {
		defer f.use(t)()
		sessionID := f.session()
		runID := f.runWithoutOutbox(sessionID, "questions", f.now.Add(-31*time.Minute))

		got, response := f.read(f.questionPath(sessionID, runID))
		f.wantState(response, got, agent.RunCompleted, recovery.StatusNeedsInput, agent.CodeActivityFailed)
		f.wantAudit(sessionID, 1)
		f.wantRun(runID, agent.RunCompleted, recovery.StatusNeedsInput, agent.CodeActivityFailed, &f.now)
	})

	t.Run("already terminal stale draft run is unchanged", func(t *testing.T) {
		defer f.use(t)()
		sessionID := f.session()
		runID := f.run(sessionID, "drafts", interviewReaperTime(f.now.Add(-31*time.Minute)), nil)
		if err := agent.NewStore(f.runtime.DB).InTx(ctx, agent.RunScope{TenantID: f.tenant, SiteID: f.site}, func(tx *sql.Tx) error {
			return agent.FinishRunTx(ctx, tx, agent.RunScope{TenantID: f.tenant, SiteID: f.site}, runID, agent.Finish{State: agent.RunCompleted, OutcomeStatus: recovery.StatusRefused, OutcomeReasons: []string{"MODEL_REJECTED"}})
		}); err != nil {
			t.Fatal(err)
		}
		before := f.runRow(runID)

		got, response := f.read(f.draftPath(runID))
		f.wantState(response, got, agent.RunCompleted, recovery.StatusRefused, "MODEL_REJECTED")
		if after := f.runRow(runID); after != before {
			t.Fatalf("terminal run changed during read: before=%+v after=%+v", before, after)
		}
		f.wantAudit(sessionID, 1)
	})

	t.Run("reaping is scoped to the requested site and tenant", func(t *testing.T) {
		defer f.use(t)()
		sessionID := f.session()
		freshRun := f.run(sessionID, "questions", nil, nil)
		f.sessionInScope(f.tenant, f.otherSite, sessionID)
		f.foreignTenantSession(sessionID)
		staleAt := f.now.Add(-31 * time.Minute)
		otherSiteRun := f.runInScope(f.tenant, f.otherSite, sessionID, "questions", &staleAt, nil, false)
		otherTenantRun := f.runInScope(f.otherTenant, f.site, sessionID, "questions", &staleAt, nil, false)

		got, response := f.read(f.questionPath(sessionID, freshRun))
		f.wantState(response, got, agent.RunRunning, "", "")
		for _, runID := range []string{otherSiteRun, otherTenantRun} {
			f.wantRun(runID, agent.RunRunning, "", "", nil)
		}
	})

	t.Run("lock timeout on either run read returns 503 problem", func(t *testing.T) {
		defer f.use(t)()
		for _, purpose := range []string{"questions", "drafts"} {
			t.Run(purpose, func(t *testing.T) {
				defer f.use(t)()
				sessionID := f.session()
				runID := f.run(sessionID, purpose, interviewReaperTime(f.now.Add(-31*time.Minute)), nil)
				lock, err := f.admin.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Rollback()
				var locked string
				if err = lock.QueryRowContext(ctx, `SELECT id::text FROM supervisor.agent_runs WHERE tenant_id=$1 AND site_id=$2 AND id=$3 FOR UPDATE`, f.tenant, f.site, runID).Scan(&locked); err != nil {
					t.Fatal(err)
				}

				began := time.Now()
				response := f.call(http.MethodGet, f.runPath(purpose, sessionID, runID), "")
				elapsed := time.Since(began)
				f.wantProblem(response, http.StatusServiceUnavailable, "STORAGE_UNAVAILABLE")
				// The read waited for the 5s lock_timeout, not the 10s statement_timeout or an
				// immediate unrelated storage failure.
				if elapsed < 5*time.Second || elapsed >= 10*time.Second {
					t.Fatalf("503 after %v, want the 5s lock_timeout", elapsed)
				}
				// The holder still owns the row lock when the request returns.
				var pgErr *pgconn.PgError
				if _, err = f.admin.ExecContext(ctx, `SELECT 1 FROM supervisor.agent_runs WHERE id=$1 FOR UPDATE NOWAIT`, runID); !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
					t.Fatalf("run row lock not held by the holder after the request: %v", err)
				}
				f.wantRun(runID, agent.RunRunning, "", "", nil)
				f.wantAudit(sessionID, 0)

				// With the lock released the same read succeeds and reaps the run.
				if err = lock.Rollback(); err != nil {
					t.Fatal(err)
				}
				got, response := f.read(f.runPath(purpose, sessionID, runID))
				f.wantState(response, got, agent.RunCompleted, recovery.StatusNeedsInput, agent.CodeActivityFailed)
				f.wantRun(runID, agent.RunCompleted, recovery.StatusNeedsInput, agent.CodeActivityFailed, &f.now)
				f.wantAudit(sessionID, 1)
			})
		}
	})
}

type interviewRunReaperFixture struct {
	t                      *testing.T
	admin                  *sql.DB
	runtime                *store.Store
	handler                http.Handler
	now                    time.Time
	tenant, site           string
	otherTenant, otherSite string
	issuer, subject        string
	token, tokenHash       string
	csrf, origin           string
}

func (f *interviewRunReaperFixture) use(t *testing.T) func() {
	previous := f.t
	f.t = t
	return func() { f.t = previous }
}

type interviewReaperResponse struct {
	RunID          string   `json:"run_id"`
	SessionID      string   `json:"session_id"`
	State          string   `json:"state"`
	OutcomeStatus  string   `json:"outcome_status"`
	OutcomeReasons []string `json:"outcome_reasons"`
	DraftIDs       []string `json:"draft_ids"`
}

func newInterviewRunReaperFixture(t *testing.T, ctx context.Context) *interviewRunReaperFixture {
	t.Helper()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL admin and runtime URLs required")
	}
	root, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	database := "interview_reaper_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = root.ExecContext(ctx, "CREATE DATABASE "+database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, e := sql.Open("pgx", adminURL)
		if e != nil {
			t.Error(e)
			return
		}
		defer cleanup.Close()
		if _, e = cleanup.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	})
	databaseURL := func(raw string) string {
		u, e := url.Parse(raw)
		if e != nil {
			t.Fatal(e)
		}
		u.Path = "/" + database
		return u.String()
	}
	admin, err := sql.Open("pgx", databaseURL(adminURL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err = migrate.Apply(ctx, admin); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err = admin.ExecContext(ctx, `CREATE SCHEMA interview_reaper_clock`); err != nil {
		t.Fatal(err)
	}
	if _, err = admin.ExecContext(ctx, fmt.Sprintf(`CREATE FUNCTION interview_reaper_clock.clock_timestamp() RETURNS timestamptz LANGUAGE sql IMMUTABLE AS 'SELECT ''%s''::timestamptz'`, now.Format(time.RFC3339Nano))); err != nil {
		t.Fatal(err)
	}
	if _, err = admin.ExecContext(ctx, `GRANT USAGE ON SCHEMA interview_reaper_clock TO supervisor_runtime`); err != nil {
		t.Fatal(err)
	}
	runtimeDatabaseURL := databaseURL(runtimeURL)
	u, err := url.Parse(runtimeDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", "interview_reaper_clock,pg_catalog,public,supervisor")
	u.RawQuery = query.Encode()
	runtime, err := store.New(u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	var pinned time.Time
	if err = runtime.DB.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&pinned); err != nil || !pinned.Equal(now) {
		t.Fatalf("runtime clock is not pinned: got=%v err=%v want=%v", pinned, err, now)
	}
	if err = runtime.VerifyRole(ctx); err != nil {
		t.Fatal(err)
	}

	f := &interviewRunReaperFixture{t: t, admin: admin, runtime: runtime, now: now,
		tenant: uuid.NewString(), site: uuid.NewString(), otherTenant: uuid.NewString(), otherSite: uuid.NewString(),
		issuer: "https://interview-reaper.invalid/realm", subject: uuid.NewString(), token: "synthetic-interview-reaper-cookie",
		csrf: "synthetic-interview-reaper-csrf", origin: "http://127.0.0.1:58176"}
	hash := sha256.Sum256([]byte(f.token))
	f.tokenHash = hex.EncodeToString(hash[:])
	for _, q := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO supervisor.tenants(id,name) VALUES($1,'reaper fixture'),($2,'reaper foreign')`, []any{f.tenant, f.otherTenant}},
		{`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'reaper site'),($1,$3,'other site'),($4,$2,'foreign tenant site')`, []any{f.tenant, f.site, f.otherSite, f.otherTenant}},
		{`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, []any{f.tenant, f.site, f.issuer, f.subject}},
		{`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,$5::timestamptz,$5::timestamptz,$5::timestamptz+interval '1 hour',$5::timestamptz)`, []any{f.tokenHash, f.subject, f.issuer, f.csrf, f.now}},
	} {
		if _, err = admin.ExecContext(ctx, q.query, q.args...); err != nil {
			t.Fatalf("fixture SQL %q: %v", q.query, err)
		}
	}
	auth, err := identity.New(ctx, identity.Config{Issuer: f.issuer, ClientID: "interview-reaper-test", ClientSecret: "synthetic-only", RedirectURI: f.origin + "/auth/callback"}, runtime.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.handler = New(config.Config{Environment: "local", WebDir: t.TempDir()}, runtime, auth)
	return f
}

func (f *interviewRunReaperFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.admin.ExecContext(f.t.Context(), query, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *interviewRunReaperFixture) session() string {
	f.t.Helper()
	id := uuid.NewString()
	f.sessionInScope(f.tenant, f.site, id)
	return id
}

func (f *interviewRunReaperFixture) sessionInScope(tenant, site, id string) {
	f.t.Helper()
	f.exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role)
 SELECT $1,$2,$3,$4,'owner' WHERE NOT EXISTS(SELECT 1 FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4)`, tenant, site, f.issuer, f.subject)
	f.exec(`INSERT INTO supervisor.interview_sessions(tenant_id,site_id,session_id,role,participant_actor_id,participant_issuer,participant_subject,status,revision) VALUES($1,$2,$3,'owner',$4,$5,$6,'active',1)`, tenant, site, id, uuid.NewString(), f.issuer, f.subject)
}

func (f *interviewRunReaperFixture) foreignTenantSession(id string) {
	f.t.Helper()
	subject := uuid.NewString()
	f.exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, f.otherTenant, f.site, f.issuer, subject)
	f.exec(`INSERT INTO supervisor.interview_sessions(tenant_id,site_id,session_id,role,participant_actor_id,participant_issuer,participant_subject,status,revision) VALUES($1,$2,$3,'owner',$4,$5,$6,'active',1)`, f.otherTenant, f.site, id, uuid.NewString(), f.issuer, subject)
}

func (f *interviewRunReaperFixture) run(sessionID, purpose string, startedAt, deliveredAt *time.Time) string {
	f.t.Helper()
	if startedAt == nil {
		startedAt = interviewReaperTime(f.now.Add(-5 * time.Minute))
	}
	return f.runInScope(f.tenant, f.site, sessionID, purpose, startedAt, deliveredAt, true)
}

func (f *interviewRunReaperFixture) runWithoutOutbox(sessionID, purpose string, startedAt time.Time) string {
	f.t.Helper()
	return f.runInScope(f.tenant, f.site, sessionID, purpose, &startedAt, nil, false)
}

func (f *interviewRunReaperFixture) runInScope(tenant, site, sessionID, purpose string, startedAt, deliveredAt *time.Time, withOutbox bool) string {
	f.t.Helper()
	runID := uuid.NewString()
	requestID := uuid.NewString()
	if err := agent.NewStore(f.runtime.DB).InTx(f.t.Context(), agent.RunScope{TenantID: tenant, SiteID: site}, func(tx *sql.Tx) error {
		run := agent.Run{ID: runID, RequestKind: agent.RequestConversation, ConversationID: sessionID, RequestID: requestID, State: agent.RunRunning,
			TraceID: agent.NewTraceID(), ProviderName: "fixture", Model: "fixture-model", ModelRevision: "fixture-r1",
			PromptVersion: agent.DraftPromptVersion, ToolVersion: agent.ToolVersion, PolicyRevision: 1}
		if err := agent.CreateRunTx(f.t.Context(), tx, agent.RunScope{TenantID: tenant, SiteID: site}, run, agent.InterviewLimits()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(f.t.Context(), `INSERT INTO supervisor.interview_agent_turns(tenant_id,site_id,session_id,run_id,session_revision,purpose) VALUES($1,$2,$3,$4,1,$5)`, tenant, site, sessionID, runID, purpose); err != nil {
			return err
		}
		if purpose == "drafts" {
			if _, err := tx.ExecContext(f.t.Context(), `INSERT INTO supervisor.configuration_draft_requests(tenant_id,site_id,session_id,run_id,requested_by_issuer,requested_by_subject,kinds,request_sha256) VALUES($1,$2,$3,$4,$5,$6,ARRAY['line']::text[],$7)`, tenant, site, sessionID, runID, f.issuer, f.subject, strings.Repeat("a", 64)); err != nil {
				return err
			}
			if withOutbox {
				_, err := tx.ExecContext(f.t.Context(), `INSERT INTO supervisor.configuration_draft_request_outbox(tenant_id,site_id,id,session_id,run_id,delivered_at) VALUES($1,$2,$3,$4,$5,$6)`, tenant, site, uuid.NewString(), sessionID, runID, deliveredAt)
				return err
			}
		} else if withOutbox {
			_, err := tx.ExecContext(f.t.Context(), `INSERT INTO supervisor.interview_agent_turn_outbox(tenant_id,site_id,id,session_id,run_id,delivered_at) VALUES($1,$2,$3,$4,$5,$6)`, tenant, site, uuid.NewString(), sessionID, runID, deliveredAt)
			return err
		}
		return nil
	}); err != nil {
		f.t.Fatal(err)
	}
	if startedAt != nil {
		f.exec(`UPDATE supervisor.agent_runs SET started_at=$4 WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, tenant, site, runID, *startedAt)
	}
	return runID
}

func (f *interviewRunReaperFixture) call(method, path, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.AddCookie(&http.Cookie{Name: "supervisor", Value: f.token})
	r.Header.Set("Origin", f.origin)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSRF-Token", f.csrf)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	conformsToPublished(f.t, r, w)
	return w
}

func (f *interviewRunReaperFixture) read(path string) (interviewReaperResponse, *httptest.ResponseRecorder) {
	f.t.Helper()
	w := f.call(http.MethodGet, path, "")
	var out interviewReaperResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			f.t.Fatalf("run response %s: %v", w.Body.String(), err)
		}
	}
	return out, w
}

func (f *interviewRunReaperFixture) questionPath(sessionID, runID string) string {
	return "/v1/sites/" + f.site + "/interviews/" + sessionID + "/agent-turns/" + runID
}

func (f *interviewRunReaperFixture) draftPath(runID string) string {
	return "/v1/sites/" + f.site + "/configuration-draft-requests/" + runID
}

func (f *interviewRunReaperFixture) wantState(w *httptest.ResponseRecorder, got interviewReaperResponse, state, outcome, reason string) {
	f.t.Helper()
	if w.Code != http.StatusOK || got.State != state || got.OutcomeStatus != outcome || len(got.OutcomeReasons) != interviewReaperReasonCount(reason != "") {
		f.t.Fatalf("run response HTTP %d: %+v body=%s; want state=%s outcome=%s reason=%s", w.Code, got, w.Body.String(), state, outcome, reason)
	}
	if reason != "" && got.OutcomeReasons[0] != reason {
		f.t.Fatalf("run reason=%v, want %s", got.OutcomeReasons, reason)
	}
}

func (f *interviewRunReaperFixture) wantProblem(w *httptest.ResponseRecorder, status int, code string) {
	f.t.Helper()
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil || w.Code != status || problem.Code != code {
		f.t.Fatalf("problem HTTP %d code=%q body=%s err=%v; want HTTP %d code=%s", w.Code, problem.Code, w.Body.String(), err, status, code)
	}
}

func (f *interviewRunReaperFixture) wantRun(runID, state, outcome, reason string, endedAt *time.Time) {
	f.t.Helper()
	row := f.runRow(runID)
	if row.state != state || row.outcome != outcome || row.reason != reason {
		f.t.Fatalf("run %s = %s/%s/%s, want %s/%s/%s", runID, row.state, row.outcome, row.reason, state, outcome, reason)
	}
	if endedAt == nil && row.endedAt.Valid || endedAt != nil && (!row.endedAt.Valid || !row.endedAt.Time.Equal(*endedAt)) {
		f.t.Fatalf("run %s ended_at=%v, want %v", runID, row.endedAt, endedAt)
	}
}

type interviewReaperRunRow struct {
	state, outcome, reason string
	endedAt                sql.NullTime
}

func (f *interviewRunReaperFixture) runRow(runID string) interviewReaperRunRow {
	f.t.Helper()
	var row interviewReaperRunRow
	if err := f.admin.QueryRowContext(f.t.Context(), `SELECT state,COALESCE(outcome_status,''),COALESCE(outcome_reasons[1],''),ended_at FROM supervisor.agent_runs WHERE id=$1`, runID).Scan(&row.state, &row.outcome, &row.reason, &row.endedAt); err != nil {
		f.t.Fatal(err)
	}
	return row
}

func (f *interviewRunReaperFixture) wantAudit(sessionID string, count int) {
	f.t.Helper()
	var got int
	if err := f.admin.QueryRowContext(f.t.Context(), `SELECT count(*) FROM supervisor.interview_audit WHERE tenant_id=$1 AND site_id=$2 AND session_id=$3 AND actor_issuer=$4 AND actor_subject=$5 AND event_type='agent_turn.read'`, f.tenant, f.site, sessionID, f.issuer, f.subject).Scan(&got); err != nil || got != count {
		f.t.Fatalf("agent_turn.read audit rows=%d err=%v, want %d", got, err, count)
	}
}

func (f *interviewRunReaperFixture) runPath(purpose, sessionID, runID string) string {
	if purpose == "drafts" {
		return f.draftPath(runID)
	}
	return f.questionPath(sessionID, runID)
}

func (f *interviewRunReaperFixture) outboxDeliveredAt(purpose, runID string) sql.NullTime {
	f.t.Helper()
	table := "interview_agent_turn_outbox"
	if purpose == "drafts" {
		table = "configuration_draft_request_outbox"
	}
	var deliveredAt sql.NullTime
	if err := f.admin.QueryRowContext(f.t.Context(), `SELECT delivered_at FROM supervisor.`+table+` WHERE tenant_id=$1 AND site_id=$2 AND run_id=$3`, f.tenant, f.site, runID).Scan(&deliveredAt); err != nil {
		f.t.Fatal(err)
	}
	return deliveredAt
}

func interviewReaperTime(value time.Time) *time.Time { return &value }

func interviewReaperDuration(value time.Duration) *time.Duration { return &value }

func interviewReaperReasonCount(hasReason bool) int {
	if hasReason {
		return 1
	}
	return 0
}
