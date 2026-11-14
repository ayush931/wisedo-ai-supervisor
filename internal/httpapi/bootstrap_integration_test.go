//go:build integration

package httpapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/bootstrap"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
)

func TestBootstrapHTTPRequiresSessionCSRFAndExactActor(t *testing.T) {
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: actual disposable PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	db, err := store.New(runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant, site, subject := uuid.NewString(), uuid.NewString(), uuid.NewString()
	defer func() {
		for _, table := range []string{"bootstrap_audit", "setup_invitations", "memberships", "sites", "tenants"} {
			key := "tenant_id"
			if table == "tenants" {
				key = "id"
			}
			if _, e := admin.ExecContext(context.Background(), "DELETE FROM supervisor."+table+" WHERE "+key+"=$1", tenant); e != nil {
				t.Error(e)
			}
		}
	}()
	issuer, origin := "https://bootstrap-test.invalid/realm", "http://127.0.0.1:58176"
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic HTTP bootstrap')", []any{tenant}},
		{"INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'synthetic HTTP site')", []any{tenant, site}},
	} {
		if _, err = admin.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	inv, err := (&bootstrap.Service{DB: admin}).Issue(ctx, bootstrap.IssueRequest{TenantID: tenant, SiteID: site, Issuer: issuer, Subject: subject, Role: "owner", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	retiredSite, retiredSubject := uuid.NewString(), uuid.NewString()
	if _, err = admin.ExecContext(ctx, "INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'pending retired-tenant site')", tenant, retiredSite); err != nil {
		t.Fatal(err)
	}
	retiredInvite, err := (&bootstrap.Service{DB: admin}).Issue(ctx, bootstrap.IssueRequest{TenantID: tenant, SiteID: retiredSite, Issuer: issuer, Subject: retiredSubject, Role: "owner", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, db.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)
	session, csrf := uuid.NewString(), uuid.NewString()
	sum := sha256.Sum256([]byte(session))
	hash := hex.EncodeToString(sum[:])
	if _, err = admin.ExecContext(ctx, `INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at) VALUES($1,$2,$3,$4,now(),now(),now()+interval '5 minutes')`, hash, subject, issuer, csrf); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = admin.ExecContext(context.Background(), "DELETE FROM supervisor.browser_sessions WHERE token_hash=$1", hash)
	}()
	body, _ := json.Marshal(map[string]string{"token": inv.Token})
	// Tenant context is resolved from the token. A body that names a tenant is rejected.
	tenantInBody, _ := json.Marshal(map[string]string{"token": inv.Token, "tenant_id": tenant, "site_id": site})
	request := func(cookie, csrfHeader, payload string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/bootstrap/redemptions", strings.NewReader(payload))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", csrfHeader)
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: "supervisor", Value: cookie})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		name, cookie, csrf, payload string
		status                      int
	}{
		{"anonymous", "", "", string(body), 401},
		{"missing CSRF", session, "", string(body), 403},
		{"actor injection", session, csrf, strings.TrimSuffix(string(body), "}") + `,"subject":"forged"}`, 422},
		{"multiple bodies", session, csrf, string(body) + ` {}`, 422},
		{"tenant named in body", session, csrf, string(tenantInBody), 422},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := request(tc.cookie, tc.csrf, tc.payload)
			if w.Code != tc.status {
				t.Fatalf("status %d want %d: %s", w.Code, tc.status, w.Body.String())
			}
		})
	}
	// An authenticated different subject cannot consume the intended owner's token.
	if _, err = admin.ExecContext(ctx, "UPDATE supervisor.browser_sessions SET subject=$1 WHERE token_hash=$2", uuid.NewString(), hash); err != nil {
		t.Fatal(err)
	}
	if w := request(session, csrf, string(body)); w.Code != 403 {
		t.Fatalf("wrong actor status %d", w.Code)
	}
	if _, err = admin.ExecContext(ctx, "UPDATE supervisor.browser_sessions SET subject=$1 WHERE token_hash=$2", subject, hash); err != nil {
		t.Fatal(err)
	}
	// The rejected tenant-in-body attempt above must not have consumed the invitation.
	w := request(session, csrf, string(body))
	if w.Code != 201 {
		t.Fatalf("redemption status %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), inv.Token) {
		t.Fatal("token exposed in response")
	}
	if w = request(session, csrf, string(body)); w.Code != 403 {
		t.Fatalf("consumed token status %d", w.Code)
	}
	sites, err := db.Sites(ctx, issuer, subject)
	if err != nil || len(sites) != 1 || sites[0].ID != site {
		t.Fatalf("membership readback sites=%v err=%v", sites, err)
	}
	if _, err = admin.ExecContext(ctx, "UPDATE supervisor.tenants SET deactivated_at=now() WHERE id=$1", tenant); err != nil {
		t.Fatal(err)
	}
	meRequest := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	meRequest.AddCookie(&http.Cookie{Name: "supervisor", Value: session})
	meResponse := httptest.NewRecorder()
	h.ServeHTTP(meResponse, meRequest)
	var me store.Me
	if meResponse.Code != http.StatusOK || json.Unmarshal(meResponse.Body.Bytes(), &me) != nil || len(me.Sites) != 0 {
		t.Fatalf("deactivated member GET /v1/me returned HTTP %d sites=%v body=%s", meResponse.Code, me.Sites, meResponse.Body.String())
	}
	if _, err = admin.ExecContext(ctx, "UPDATE supervisor.browser_sessions SET subject=$1 WHERE token_hash=$2", retiredSubject, hash); err != nil {
		t.Fatal(err)
	}
	retiredBody, _ := json.Marshal(map[string]string{"token": retiredInvite.Token})
	if w = request(session, csrf, string(retiredBody)); w.Code != http.StatusForbidden {
		t.Fatalf("deactivated tenant invitation status %d: %s", w.Code, w.Body.String())
	}
	var problem struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(w.Body.Bytes(), &problem) != nil || problem.Code != "INVALID_INVITATION" {
		t.Fatalf("deactivated invitation response=%s", w.Body.String())
	}
	var created, consumed int
	if err = admin.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2),(SELECT count(*) FROM supervisor.setup_invitations WHERE id=$3 AND consumed_at IS NOT NULL)`, tenant, retiredSite, retiredInvite.ID).Scan(&created, &consumed); err != nil || created != 0 || consumed != 0 {
		t.Fatalf("deactivated invitation side effects memberships=%d consumed=%d err=%v", created, consumed, err)
	}
}
