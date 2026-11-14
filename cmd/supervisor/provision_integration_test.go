//go:build integration

package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/bootstrap"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/httpapi"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/text/unicode/norm"
)

func TestProvisionPostgresEndToEnd(t *testing.T) {
	// WIS-699: the provisioning URL is checked against the environment; this suite runs against
	// a local disposable database.
	t.Setenv("SUPERVISOR_ENVIRONMENT", "local")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	migrationPassword := os.Getenv("SUPERVISOR_MIGRATION_PASSWORD")
	if adminURL == "" || runtimeURL == "" || migrationPassword == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL admin, runtime and migration credentials required")
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	runtimeDB, err := sql.Open("pgx", runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtimeDB.Close() })
	provisionURL, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	provisionURL.User = url.UserPassword("supervisor_migrator", migrationPassword)
	t.Setenv("SUPERVISOR_PROVISIONING_URL", provisionURL.String())

	requestID, secondRequestID, concurrentRequestID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	tenantInput, siteInput := "Synthetic Pro\u0301vision "+requestID, "Synthetic Detroit Pla\u0301nt "+requestID
	tenantName, siteName := norm.NFC.String(tenantInput), norm.NFC.String(siteInput)
	issuer, subject, operator := "https://provision-test.invalid/realm", uuid.NewString(), "WiseDo operations integration test"
	tmp := t.TempDir()
	firstOutput := filepath.Join(tmp, "owner.token")
	secondOutput := filepath.Join(tmp, "second-owner.token")
	args := func(id, output, tenant, site, operatorName string) []string {
		return []string{"--tenant-name", tenant, "--site-name", site, "--timezone", "America/Detroit", "--owner-issuer", issuer, "--owner-subject", subject, "--operator", operatorName, "--request-id", id, "--output", output}
	}
	var sessionHash string
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if sessionHash != "" {
			_, _ = admin.ExecContext(cleanup, `DELETE FROM supervisor.browser_sessions WHERE token_hash=$1`, sessionHash)
		}
		if _, e := admin.ExecContext(cleanup, `ALTER TABLE supervisor.provisioning_audit DISABLE TRIGGER provisioning_audit_append_only`); e != nil {
			t.Errorf("disable append-only trigger for disposable cleanup: %v", e)
		}
		defer func() {
			if _, e := admin.ExecContext(cleanup, `ALTER TABLE supervisor.provisioning_audit ENABLE TRIGGER provisioning_audit_append_only`); e != nil {
				t.Errorf("restore append-only trigger after cleanup: %v", e)
			}
		}()
		for _, id := range []string{requestID, secondRequestID, concurrentRequestID} {
			var tenantID string
			if e := admin.QueryRowContext(cleanup, `SELECT tenant_id::text FROM supervisor.provisioning_audit WHERE request_id=$1`, id).Scan(&tenantID); e != nil {
				if !errors.Is(e, sql.ErrNoRows) {
					t.Errorf("read provision cleanup scope: %v", e)
				}
				continue
			}
			for _, table := range []string{"bootstrap_audit", "memberships", "setup_invitations", "provisioning_audit"} {
				if _, e := admin.ExecContext(cleanup, "DELETE FROM supervisor."+table+" WHERE tenant_id=$1", tenantID); e != nil {
					t.Errorf("clean %s: %v", table, e)
				}
			}
			if _, e := admin.ExecContext(cleanup, `DELETE FROM supervisor.sites WHERE tenant_id=$1`, tenantID); e != nil {
				t.Error(e)
			}
			if _, e := admin.ExecContext(cleanup, `DELETE FROM supervisor.tenants WHERE id=$1`, tenantID); e != nil {
				t.Error(e)
			}
		}
	})

	if err = provision(ctx, args(requestID, firstOutput, tenantInput, siteInput, operator)); err != nil {
		t.Fatalf("first provision: %s", safeProvisionError(err))
	}
	tokenBytes, err := os.ReadFile(firstOutput)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		t.Fatal("output must contain only the invitation token")
	}
	if info, e := os.Stat(firstOutput); e != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("token file mode must be 0600, got %v %v", info, e)
	}
	var tenantID, siteID, gotOperator, osUser, gotDigest, timezone string
	if err = admin.QueryRowContext(ctx, `SELECT tenant_id::text,site_id::text,operator,os_user,request_sha256 FROM supervisor.provisioning_audit WHERE request_id=$1`, requestID).Scan(&tenantID, &siteID, &gotOperator, &osUser, &gotDigest); err != nil {
		t.Fatal(err)
	}
	currentUser, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if len(gotDigest) != 64 || gotDigest != strings.ToLower(gotDigest) || gotOperator != operator || osUser != currentUser.Username {
		t.Fatalf("audit identity/digest mismatch: operator=%q os_user=%q digest=%q err=%v", gotOperator, osUser, gotDigest, err)
	}
	var savedTenantName, savedSiteName string
	if err = admin.QueryRowContext(ctx, `SELECT t.name,s.name FROM supervisor.tenants t JOIN supervisor.sites s ON s.tenant_id=t.id WHERE t.id=$1 AND s.id=$2`, tenantID, siteID).Scan(&savedTenantName, &savedSiteName); err != nil || savedTenantName != tenantName || savedSiteName != siteName {
		t.Fatalf("names were not NFC-normalized before persistence: tenant=%q site=%q err=%v", savedTenantName, savedSiteName, err)
	}
	if err = admin.QueryRowContext(ctx, `SELECT timezone FROM supervisor.sites WHERE tenant_id=$1 AND id=$2`, tenantID, siteID).Scan(&timezone); err != nil || timezone != "America/Detroit" {
		t.Fatalf("site timezone=%q err=%v", timezone, err)
	}

	if err = provision(ctx, args(requestID, secondOutput, tenantName, siteName, "Different WiseDo operator")); !errors.Is(err, errProvisionInvitationIssued) {
		t.Fatalf("same request must fail with explicit invitation already issued: %v", err)
	}
	var sameNameTenants, sameTenantSites, sameRequestAudits int
	if err = admin.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM supervisor.tenants WHERE name=$1),(SELECT count(*) FROM supervisor.sites WHERE tenant_id=$2 AND name=$3),(SELECT count(*) FROM supervisor.provisioning_audit WHERE request_id=$4)`, tenantName, tenantID, siteName, requestID).Scan(&sameNameTenants, &sameTenantSites, &sameRequestAudits); err != nil {
		t.Fatal(err)
	}
	if sameNameTenants != 1 || sameTenantSites != 1 || sameRequestAudits != 1 {
		t.Fatalf("replayed request rows: tenants=%d sites=%d audits=%d", sameNameTenants, sameTenantSites, sameRequestAudits)
	}
	if _, e := os.Stat(secondOutput); !os.IsNotExist(e) {
		t.Fatal("failed replay left an output file")
	}
	if err = provision(ctx, args(requestID, secondOutput, tenantName, "Changed Plant "+requestID, operator)); !errors.Is(err, errProvisionRequestConflict) {
		t.Fatalf("changed request reused request ID: %v", err)
	}
	if err = provision(ctx, args(secondRequestID, secondOutput, tenantName, siteName, operator)); err != nil {
		t.Fatalf("different request ID with duplicate names: %s", safeProvisionError(err))
	}
	var secondTenant, secondSite string
	if err = admin.QueryRowContext(ctx, `SELECT tenant_id::text,site_id::text FROM supervisor.provisioning_audit WHERE request_id=$1`, secondRequestID).Scan(&secondTenant, &secondSite); err != nil {
		t.Fatal(err)
	}
	if tenantID == secondTenant || siteID == secondSite {
		t.Fatal("different request ID did not create a separate tenant and site")
	}
	if err = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.tenants WHERE name=$1`, tenantName).Scan(&sameNameTenants); err != nil || sameNameTenants != 2 {
		t.Fatalf("different request ID same names created %d tenants: %v", sameNameTenants, err)
	}

	if _, err = admin.ExecContext(ctx, `ALTER TABLE supervisor.provisioning_audit OWNER TO supervisor_migrator`); err != nil {
		t.Fatalf("set production migrator ownership for append-only test: %v", err)
	}
	migratorDB, err := sql.Open("pgx", provisionURL.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = migratorDB.Close() })
	for _, statement := range []string{
		`UPDATE supervisor.provisioning_audit SET operator=operator WHERE request_id=$1`,
		`DELETE FROM supervisor.provisioning_audit WHERE request_id=$1`,
	} {
		if _, err = migratorDB.ExecContext(ctx, statement, requestID); sqlState(err) != "55000" {
			t.Fatalf("migrator rewrite of provisioning audit was not refused by its trigger: %v", err)
		}
	}
	if _, err = runtimeDB.ExecContext(ctx, `INSERT INTO supervisor.provisioning_audit(request_id,tenant_id,site_id,operator,os_user,request_sha256) VALUES($1,$2,$3,'runtime','runtime',repeat('0',64))`, uuid.NewString(), tenantID, siteID); sqlState(err) != "42501" {
		t.Fatalf("runtime INSERT into provisioning audit must fail with 42501: %v", err)
	}

	concurrentTenant := "Synthetic Concurrent Provision " + concurrentRequestID
	concurrentSite := "Synthetic Concurrent Plant " + concurrentRequestID
	concurrentPaths := []string{filepath.Join(tmp, "concurrent-a.token"), filepath.Join(tmp, "concurrent-b.token")}
	start := make(chan struct{})
	results := make(chan error, len(concurrentPaths))
	var wg sync.WaitGroup
	for _, path := range concurrentPaths {
		path := path
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- provision(ctx, args(concurrentRequestID, path, concurrentTenant, concurrentSite, operator))
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	successes, replays := 0, 0
	for result := range results {
		if result == nil {
			successes++
		} else if errors.Is(result, errProvisionInvitationIssued) {
			replays++
		} else {
			t.Fatalf("parallel same-ID provision had unexpected outcome: %v", result)
		}
	}
	if successes != 1 || replays != 1 {
		t.Fatalf("parallel same-ID provision results: success=%d already-issued=%d", successes, replays)
	}
	var concurrentTenants, concurrentSites, concurrentAudits int
	if err = admin.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM supervisor.tenants WHERE name=$1),(SELECT count(*) FROM supervisor.sites WHERE name=$2),(SELECT count(*) FROM supervisor.provisioning_audit WHERE request_id=$3)`, concurrentTenant, concurrentSite, concurrentRequestID).Scan(&concurrentTenants, &concurrentSites, &concurrentAudits); err != nil {
		t.Fatal(err)
	}
	if concurrentTenants != 1 || concurrentSites != 1 || concurrentAudits != 1 {
		t.Fatalf("parallel same-ID rows: tenants=%d sites=%d audits=%d", concurrentTenants, concurrentSites, concurrentAudits)
	}
	outputFiles := 0
	for _, path := range concurrentPaths {
		if _, e := os.Stat(path); e == nil {
			outputFiles++
		} else if !os.IsNotExist(e) {
			t.Fatalf("stat parallel output: %v", e)
		}
	}
	if outputFiles != 1 {
		t.Fatalf("parallel same-ID provision left %d token files, want exactly one", outputFiles)
	}

	failureRequestID := uuid.NewString()
	failureTenant := "Synthetic Failed Issue " + failureRequestID
	failureSite := "Synthetic Failed Plant " + failureRequestID
	failureOutput := filepath.Join(tmp, "failed-issue.token")
	constraint := "provision_issue_failure_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	// Keep the injected failure scoped to this request so concurrent bootstrap tests can use the shared test database.
	if _, err = admin.ExecContext(ctx, "ALTER TABLE supervisor.setup_invitations ADD CONSTRAINT "+constraint+" CHECK (subject <> '"+subject+"') NOT VALID"); err != nil {
		t.Fatalf("install invitation issue failure: %v", err)
	}
	t.Cleanup(func() {
		if _, e := admin.ExecContext(context.Background(), "ALTER TABLE supervisor.setup_invitations DROP CONSTRAINT IF EXISTS "+constraint); e != nil {
			t.Errorf("drop invitation issue failure constraint: %v", e)
		}
	})
	if _, err = (bootstrap.Service{DB: admin}).Issue(ctx, bootstrap.IssueRequest{
		TenantID: tenantID, SiteID: siteID, Issuer: issuer, Subject: uuid.NewString(), Role: "owner", TTL: 15 * time.Minute,
	}); err != nil {
		t.Fatalf("provisioning failure injection affected an unrelated owner invitation: %v", err)
	}
	failureErr := provision(ctx, args(failureRequestID, failureOutput, failureTenant, failureSite, operator))
	if _, err = admin.ExecContext(ctx, "ALTER TABLE supervisor.setup_invitations DROP CONSTRAINT "+constraint); err != nil {
		t.Fatalf("remove invitation issue failure: %v", err)
	}
	if failureErr == nil {
		t.Fatal("forced invitation issuance failure unexpectedly succeeded")
	}
	if _, err = os.Stat(failureOutput); !os.IsNotExist(err) {
		t.Fatalf("failed issuance left output file: %v", err)
	}
	var failedTenants, failedSites, failedAudits int
	if err = admin.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM supervisor.tenants WHERE name=$1),(SELECT count(*) FROM supervisor.sites WHERE name=$2),(SELECT count(*) FROM supervisor.provisioning_audit WHERE request_id=$3)`, failureTenant, failureSite, failureRequestID).Scan(&failedTenants, &failedSites, &failedAudits); err != nil {
		t.Fatal(err)
	}
	if failedTenants != 0 || failedSites != 0 || failedAudits != 0 {
		t.Fatalf("failed issuance committed rows: tenants=%d sites=%d audits=%d", failedTenants, failedSites, failedAudits)
	}

	if _, err = runtimeDB.ExecContext(ctx, `SELECT * FROM supervisor.provisioning_audit LIMIT 1`); err == nil {
		t.Fatal("runtime role selected provisioning audit")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("runtime audit read must fail with 42501, got %v", err)
		}
	}
	runtimeRequestID := uuid.NewString()
	t.Setenv("SUPERVISOR_PROVISIONING_URL", runtimeURL)
	if err = provision(ctx, args(runtimeRequestID, filepath.Join(tmp, "runtime.token"), tenantName, "Runtime refusal "+requestID, operator)); !errors.Is(err, errProvisionRuntimeRole) || !strings.Contains(err.Error(), "supervisor_runtime") {
		t.Fatalf("runtime DSN was not explicitly refused: %v", err)
	}
	var runtimeRows int
	if err = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.provisioning_audit WHERE request_id=$1`, runtimeRequestID).Scan(&runtimeRows); err != nil || runtimeRows != 0 {
		t.Fatalf("runtime role caused provisioning audit writes: rows=%d err=%v", runtimeRows, err)
	}
	if _, e := os.Stat(filepath.Join(tmp, "runtime.token")); !os.IsNotExist(e) {
		t.Fatal("runtime refusal left an output file")
	}
	adminRequestID, adminOutput := uuid.NewString(), filepath.Join(tmp, "admin.token")
	t.Setenv("SUPERVISOR_PROVISIONING_URL", adminURL)
	if err = provision(ctx, args(adminRequestID, adminOutput, tenantName, "Admin refusal "+requestID, operator)); !errors.Is(err, errProvisionRole) {
		t.Fatalf("admin superuser URL was not refused: %v", err)
	}
	if _, e := os.Stat(adminOutput); !os.IsNotExist(e) {
		t.Fatal("admin role refusal left an output file")
	}

	apiDB, err := store.New(runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = apiDB.Close() })
	origin := "http://127.0.0.1:58176"
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "provision-test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, runtimeDB, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := httpapi.New(config.Config{Environment: "local", WebDir: t.TempDir()}, apiDB, auth)
	sessionToken, csrf := uuid.NewString(), uuid.NewString()
	sessionSum := sha256.Sum256([]byte(sessionToken))
	sessionHash = hex.EncodeToString(sessionSum[:])
	if _, err = admin.ExecContext(ctx, `INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, sessionHash, subject, issuer, csrf); err != nil {
		t.Fatal(err)
	}
	redemption, _ := json.Marshal(map[string]string{"token": token})
	request := httptest.NewRequest(http.MethodPost, "/v1/bootstrap/redemptions", strings.NewReader(string(redemption)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	request.Header.Set("X-CSRF-Token", csrf)
	request.AddCookie(&http.Cookie{Name: "supervisor", Value: sessionToken})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || strings.Contains(response.Body.String(), token) {
		t.Fatalf("owner invitation redemption failed or leaked token: %d %s", response.Code, response.Body.String())
	}
	getSite := func(id string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/v1/sites/"+id+"/memberships?limit=50", nil)
		r.AddCookie(&http.Cookie{Name: "supervisor", Value: sessionToken})
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if response := getSite(siteID); response.Code != http.StatusOK {
		t.Fatalf("owner could not read own site: %d %s", response.Code, response.Body.String())
	}
	if response := getSite(secondSite); response.Code != http.StatusNotFound {
		t.Fatalf("owner read another tenant site: %d %s", response.Code, response.Body.String())
	}
}

func TestProvisionDeactivateEndToEnd(t *testing.T) {
	// WIS-699: the provisioning URL is checked against the environment; this suite runs against
	// a local disposable database.
	t.Setenv("SUPERVISOR_ENVIRONMENT", "local")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	migrationPassword := os.Getenv("SUPERVISOR_MIGRATION_PASSWORD")
	if adminURL == "" || runtimeURL == "" || migrationPassword == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL admin, runtime and migration credentials required")
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	provisionURL, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	provisionURL.User = url.UserPassword("supervisor_migrator", migrationPassword)
	t.Setenv("SUPERVISOR_PROVISIONING_URL", provisionURL.String())
	provisionID, deactivateID, nextID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	tenantName := "Synthetic deactivation " + provisionID
	tmp := t.TempDir()
	if err = provision(ctx, []string{"--tenant-name", tenantName, "--site-name", "Deactivation plant", "--timezone", "America/Detroit", "--owner-issuer", "https://deactivation.invalid/realm", "--owner-subject", uuid.NewString(), "--operator", "WiseDo operations test", "--request-id", provisionID, "--output", filepath.Join(tmp, "owner.token")}); err != nil {
		t.Fatalf("provision tenant: %s", safeProvisionError(err))
	}
	var tenantID, siteID, provisionAction string
	if err = admin.QueryRowContext(ctx, `SELECT tenant_id::text,site_id::text,action FROM supervisor.provisioning_audit WHERE request_id=$1`, provisionID).Scan(&tenantID, &siteID, &provisionAction); err != nil || provisionAction != "tenant.provisioned" {
		t.Fatalf("provision audit action=%q err=%v", provisionAction, err)
	}
	fixtureTenant := uuid.NewString()
	if _, err = admin.ExecContext(ctx, `INSERT INTO supervisor.tenants(id,name) VALUES($1,'unprovisioned deactivation fixture')`, fixtureTenant); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if _, e := admin.ExecContext(cleanup, `ALTER TABLE supervisor.provisioning_audit DISABLE TRIGGER provisioning_audit_append_only`); e != nil {
			t.Error(e)
		}
		defer func() {
			if _, e := admin.ExecContext(cleanup, `ALTER TABLE supervisor.provisioning_audit ENABLE TRIGGER provisioning_audit_append_only`); e != nil {
				t.Error(e)
			}
		}()
		for _, table := range []string{"bootstrap_audit", "memberships", "setup_invitations", "provisioning_audit"} {
			if _, e := admin.ExecContext(cleanup, `DELETE FROM supervisor.`+table+` WHERE tenant_id=$1`, tenantID); e != nil {
				t.Errorf("cleanup %s: %v", table, e)
			}
		}
		if _, e := admin.ExecContext(cleanup, `DELETE FROM supervisor.sites WHERE tenant_id=$1`, tenantID); e != nil {
			t.Error(e)
		}
		for _, id := range []string{tenantID, fixtureTenant} {
			if _, e := admin.ExecContext(cleanup, `DELETE FROM supervisor.tenants WHERE id=$1`, id); e != nil {
				t.Error(e)
			}
		}
	})
	args := func(id, tenant string) []string {
		return []string{"--tenant", tenant, "--operator", "WiseDo operations deactivation test", "--request-id", id}
	}
	if err = provisionDeactivate(ctx, args(deactivateID, tenantID)); err != nil {
		t.Fatalf("deactivate tenant: %s", safeProvisionError(err))
	}
	var inactive bool
	if err = admin.QueryRowContext(ctx, `SELECT deactivated_at IS NOT NULL FROM supervisor.tenants WHERE id=$1`, tenantID).Scan(&inactive); err != nil || !inactive {
		t.Fatalf("tenant inactive=%v err=%v", inactive, err)
	}
	var action, auditSite string
	if err = admin.QueryRowContext(ctx, `SELECT action,site_id::text FROM supervisor.provisioning_audit WHERE request_id=$1`, deactivateID).Scan(&action, &auditSite); err != nil || action != "tenant.deactivated" || auditSite != siteID {
		t.Fatalf("deactivation audit action=%q site=%q want=%q err=%v", action, auditSite, siteID, err)
	}
	if err = provisionDeactivate(ctx, args(deactivateID, tenantID)); err != nil {
		t.Fatalf("same request replay: %s", safeProvisionError(err))
	}
	var count int
	if err = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.provisioning_audit WHERE tenant_id=$1 AND action='tenant.deactivated'`, tenantID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("deactivation audits=%d err=%v", count, err)
	}
	if err = provisionDeactivate(ctx, args(deactivateID, uuid.NewString())); !errors.Is(err, errProvisionRequestConflict) {
		t.Fatalf("changed tenant reused request ID: %v", err)
	}
	if err = provisionDeactivate(ctx, args(nextID, tenantID)); !errors.Is(err, errTenantAlreadyDeactivated) {
		t.Fatalf("second deactivation: %v", err)
	}
	if err = provisionDeactivate(ctx, args(uuid.NewString(), uuid.NewString())); !errors.Is(err, errTenantUnknown) {
		t.Fatalf("unknown tenant deactivation: %v", err)
	}
	if err = provisionDeactivate(ctx, args(uuid.NewString(), fixtureTenant)); !errors.Is(err, errTenantNotProvisioned) {
		t.Fatalf("fixture tenant deactivation: %v", err)
	}
	var fixtureStillActive bool
	if err = admin.QueryRowContext(ctx, `SELECT deactivated_at IS NULL FROM supervisor.tenants WHERE id=$1`, fixtureTenant).Scan(&fixtureStillActive); err != nil || !fixtureStillActive {
		t.Fatalf("unprovisioned fixture was mutated: active=%v err=%v", fixtureStillActive, err)
	}
	if err = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.provisioning_audit WHERE tenant_id=$1 AND action='tenant.deactivated'`, tenantID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("failed deactivation requests added audit rows: count=%d err=%v", count, err)
	}
	t.Setenv("SUPERVISOR_PROVISIONING_URL", runtimeURL)
	if err = provisionDeactivate(ctx, args(uuid.NewString(), tenantID)); !errors.Is(err, errProvisionRuntimeRole) {
		t.Fatalf("runtime role was accepted for deactivation: %v", err)
	}
}
