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
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
)

// WIS-290 member directory: any active member of a site reads that site's active members with
// their stored display names; revoked members disappear, other sites and tenants are the
// non-disclosing 404, and GET /v1/me carries the caller's own name.
func TestSiteMemberDirectoryHTTPPostgres(t *testing.T) {
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
	issuer := "https://members-http.invalid/realm"
	tenant, other := uuid.NewString(), uuid.NewString()
	site, sibling, foreign := uuid.NewString(), uuid.NewString(), uuid.NewString()
	lead, operator, viewer, revoked, outsider := "a-lead-"+uuid.NewString(), "b-op-"+uuid.NewString(), "c-viewer-"+uuid.NewString(), "d-gone-"+uuid.NewString(), "e-out-"+uuid.NewString()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := admin.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	defer func() {
		for _, id := range []string{tenant, other} {
			for _, q := range []string{"DELETE FROM supervisor.member_profiles WHERE tenant_id=$1", "DELETE FROM supervisor.memberships WHERE tenant_id=$1", "DELETE FROM supervisor.sites WHERE tenant_id=$1", "DELETE FROM supervisor.tenants WHERE id=$1"} {
				_, _ = admin.ExecContext(context.Background(), q, id)
			}
		}
		_, _ = admin.ExecContext(context.Background(), "DELETE FROM supervisor.browser_sessions WHERE issuer=$1", issuer)
	}()
	exec("INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic directory'),($2,'synthetic other tenant')", tenant, other)
	exec("INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'site'),($1,$3,'sibling'),($4,$5,'foreign')", tenant, site, sibling, other, foreign)
	for _, m := range []struct {
		tenant, site, subject, role string
		active                      bool
	}{{tenant, site, lead, "supervisor", true}, {tenant, site, operator, "operator", true}, {tenant, site, viewer, "viewer", true}, {tenant, site, revoked, "operator", false}, {tenant, sibling, outsider, "owner", true}, {other, foreign, lead, "owner", true}} {
		exec("INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role,active) VALUES($1,$2,$3,$4,$5,$6)", m.tenant, m.site, issuer, m.subject, m.role, m.active)
	}
	// Names as login stores them; the viewer never logged in (no profile row).
	exec(`INSERT INTO supervisor.member_profiles(tenant_id,issuer,subject,display_name) VALUES($1,$2,$3,'Shift Lead'),($1,$2,$4,'Operator One'),($1,$2,$5,'Gone Operator'),($1,$2,$6,'Sibling Owner')`, tenant, issuer, lead, operator, revoked, outsider)
	sessions := map[string]string{}
	for _, subject := range []string{lead, operator} {
		token := uuid.NewString()
		sum := sha256.Sum256([]byte(token))
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour')`, hex.EncodeToString(sum[:]), subject, issuer, uuid.NewString())
		sessions[subject] = token
	}
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "test", ClientSecret: "synthetic-only", RedirectURI: "http://127.0.0.1:58176/auth/callback"}, db.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)
	get := func(as, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(&http.Cookie{Name: "supervisor", Value: sessions[as]})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	type entry struct {
		Subject     string  `json:"subject"`
		DisplayName *string `json:"display_name"`
		Role        string  `json:"role"`
	}
	type page struct {
		Members    []entry `json:"members"`
		NextCursor string  `json:"next_cursor"`
	}
	list := func(as, query string) page {
		t.Helper()
		w := get(as, "/v1/sites/"+site+"/members"+query)
		if w.Code != 200 {
			t.Fatalf("members%s as %s: HTTP %d %s", query, as, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "email") || strings.Contains(w.Body.String(), `"issuer"`) {
			t.Fatalf("directory must expose neither emails nor issuers: %s", w.Body.String())
		}
		var p page
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	render := func(p page) string {
		parts := []string{}
		for _, m := range p.Members {
			name := "<null>"
			if m.DisplayName != nil {
				name = *m.DisplayName
			}
			parts = append(parts, m.Subject[:2]+"="+name+"/"+m.Role)
		}
		return strings.Join(parts, ",")
	}

	var me struct {
		Subject     string  `json:"subject"`
		DisplayName *string `json:"display_name"`
	}
	if w := get(lead, "/v1/me"); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &me) != nil || me.DisplayName == nil || *me.DisplayName != "Shift Lead" {
		t.Fatalf("GET /v1/me: HTTP %d %s", w.Code, w.Body.String())
	}
	// Any active member reads, not only owners; revoked members and other sites are absent.
	want := "a-=Shift Lead/supervisor,b-=Operator One/operator,c-=<null>/viewer"
	for _, as := range []string{lead, operator} {
		if got := render(list(as, "")); got != want {
			t.Fatalf("directory as %s = %s, want %s", as, got, want)
		}
	}
	first := list(lead, "?limit=2")
	if render(first) != "a-=Shift Lead/supervisor,b-=Operator One/operator" || first.NextCursor == "" {
		t.Fatalf("first page %s cursor %q", render(first), first.NextCursor)
	}
	if rest := list(lead, "?limit=2&cursor="+url.QueryEscape(first.NextCursor)); render(rest) != "c-=<null>/viewer" || rest.NextCursor != "" {
		t.Fatalf("second page %s cursor %q", render(rest), rest.NextCursor)
	}
	for _, q := range []string{"?limit=0", "?limit=101", "?limit=2&limit=3", "?cursor=%25%25", "?tenant_id=" + other} {
		wantProblem(t, get(lead, "/v1/sites/"+site+"/members"+q), 422, "INVALID_REQUEST")
	}
	// Same tenant, a site the caller is not a member of; another tenant's site (where the caller
	// is an owner of a different tenant, so the UUID alone grants nothing); an unknown site.
	wantProblem(t, get(lead, "/v1/sites/"+sibling+"/members"), 404, "NOT_FOUND")
	wantProblem(t, get(operator, "/v1/sites/"+foreign+"/members"), 404, "NOT_FOUND")
	wantProblem(t, get(lead, "/v1/sites/"+uuid.NewString()+"/members"), 404, "NOT_FOUND")
	r := httptest.NewRequest("GET", "/v1/sites/"+site+"/members", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	wantProblem(t, w, 401, "AUTHENTICATION_REQUIRED")

	// Revocation takes effect on the next request: the operator disappears from the directory
	// and can no longer read it.
	exec("UPDATE supervisor.memberships SET active=false WHERE tenant_id=$1 AND site_id=$2 AND subject=$3", tenant, site, operator)
	if got := render(list(lead, "")); got != "a-=Shift Lead/supervisor,c-=<null>/viewer" {
		t.Fatalf("after revocation %s", got)
	}
	wantProblem(t, get(operator, "/v1/sites/"+site+"/members"), 404, "NOT_FOUND")
}
