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
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
)

// WIS-642: GET .../process-snapshots/{snapshot}/route-summary orders a process's approved
// operations by their approved precedes facts and sums their approved times exactly, from the
// snapshot only. A broken chain carries no totals; a stale snapshot is 409 SNAPSHOT_STALE; another
// site's snapshot and a non-process entity are 404; the query is strict.
func TestWIS642RouteSummaryHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("actual disposable PostgreSQL required")
	}
	control, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	name := "route_http_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = control.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, e := control.ExecContext(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	}()
	connectURL := func(raw string) string {
		u, e := url.Parse(raw)
		if e != nil {
			t.Fatal(e)
		}
		u.Path = "/" + name
		return u.String()
	}
	admin, err := sql.Open("pgx", connectURL(adminURL))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err = migrate.Apply(ctx, admin); err != nil {
		t.Fatal(err)
	}
	db, err := store.New(connectURL(runtimeURL))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.VerifyRole(ctx); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := admin.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	tenant, site, foreign := uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, origin := "https://route-http.invalid/"+uuid.NewString(), "http://127.0.0.1:58176"
	exec("INSERT INTO supervisor.tenants(id,name) VALUES($1,'route synthetic')", tenant)
	exec("INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'route synthetic'),($1,$3,'inaccessible')", tenant, site, foreign)
	type session struct{ token, hash, subject string }
	seed := func(role string) session {
		t.Helper()
		a := session{token: uuid.NewString(), subject: uuid.NewString()}
		sum := sha256.Sum256([]byte(a.token))
		a.hash = hex.EncodeToString(sum[:])
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, a.hash, a.subject, issuer, uuid.NewString())
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, tenant, site, issuer, a.subject, role)
		return a
	}
	owner, viewer := seed("owner"), seed("viewer")
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "route", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, db.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)
	get := func(a session, p string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", p, nil)
		if a.token != "" {
			r.AddCookie(&http.Cookie{Name: "supervisor", Value: a.token})
		}
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		conformsToPublished(t, r, w)
		return w
	}
	status := func(w *httptest.ResponseRecorder, want int, code string) {
		t.Helper()
		if w.Code != want || (code != "" && !strings.Contains(w.Body.String(), `"`+code+`"`)) {
			t.Fatalf("HTTP%d want %d %s: %s", w.Code, want, code, w.Body.String())
		}
	}

	sc := knowledge.Scope{TenantID: tenant, SiteID: site}
	actor := knowledge.Actor{Issuer: issuer, Subject: owner.subject, SessionHash: owner.hash}
	service, ev := knowledge.New(db.DB), evidence.New(db.DB)
	es := evidence.Scope{TenantID: tenant, SiteID: site}
	ea := evidence.Actor{Issuer: issuer, Subject: owner.subject, SessionHash: owner.hash}
	source, e := ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Bracket route", Text: "Blank 60 s, press 90.5 s, trim 120 s"}, uuid.NewString(), 0)
	if e != nil {
		t.Fatal(e)
	}
	span, e := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: source.SourceID, Revision: 1, Start: 0, End: 10}, uuid.NewString(), 1)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = ev.DecideAuthority(ctx, ea, es, source.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Owner verified the route"}, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	entity := func(typ, display string) string {
		t.Helper()
		id := uuid.NewString()
		if _, e := service.CreateEntity(ctx, actor, sc, knowledge.EntityInput{EntityID: id, Type: typ, Name: display, SourceID: source.SourceID, SourceRevision: 1}, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		return id
	}
	approve := func(c knowledge.ClaimInput) {
		t.Helper()
		c.ClaimID, c.SubjectRevision, c.EffectiveFrom, c.EvidenceClass, c.SourceSpanIDs = uuid.NewString(), 1, "2026-09-27T00:00:00Z", "report", []string{span.ID}
		if c.ObjectEntityID != "" {
			c.ObjectRevision = 1
		}
		if _, e := service.CreateClaim(ctx, actor, sc, c, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		if _, e := service.DecideReview(ctx, actor, sc, c.ClaimID, knowledge.ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Verified against the route sheet"}, uuid.NewString(), 1); e != nil {
			t.Fatal(e)
		}
	}
	snapshot := func() string {
		t.Helper()
		inventory, e := service.ReadInventory(ctx, actor, sc)
		if e != nil {
			t.Fatal(e)
		}
		s, e := service.CreateSnapshot(ctx, actor, sc, knowledge.SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Reviewed bracket route"}, uuid.NewString(), inventory.Revision)
		if e != nil {
			t.Fatal(e)
		}
		return s.SnapshotID
	}
	process := entity("process", "Bracket")
	ops := []string{entity("operation", "Blank"), entity("operation", "Press"), entity("operation", "Trim")}
	for _, op := range ops {
		approve(knowledge.ClaimInput{SubjectID: op, Predicate: "part_of", ObjectEntityID: process})
	}
	for i, v := range []string{"60", "90.5", "120"} {
		approve(knowledge.ClaimInput{SubjectID: ops[i], Predicate: "cycle_time_seconds", Literal: &knowledge.Literal{Type: "decimal", Value: json.RawMessage(`"` + v + `"`), Unit: "s"}})
	}
	// Only the first edge so far: a gap between Press and Trim.
	approve(knowledge.ClaimInput{SubjectID: ops[0], Predicate: "precedes", ObjectEntityID: ops[1]})
	gapped := snapshot()

	approve(knowledge.ClaimInput{SubjectID: ops[1], Predicate: "precedes", ObjectEntityID: ops[2]})
	coil := entity("process", "Coil slitting")
	for _, display := range []string{"Slit", "Rewind"} {
		op := entity("operation", display)
		approve(knowledge.ClaimInput{SubjectID: op, Predicate: "part_of", ObjectEntityID: coil})
		approve(knowledge.ClaimInput{SubjectID: op, Predicate: "cycle_time_seconds", Literal: &knowledge.Literal{Type: "decimal", Value: json.RawMessage(`"30"`), Unit: "s"}})
	}
	linked := snapshot()
	query := "?process_id=" + process + "&as_of=2026-10-01T00%3A00%3A00Z"
	route := func(snap string) string { return "/v1/sites/" + site + "/process-snapshots/" + snap + "/route-summary" }

	status(get(session{}, route(linked)+query), 401, "")
	// The gapped snapshot is stale now (the inventory moved past it), as the graph read refuses it.
	status(get(viewer, route(gapped)+query), 409, "SNAPSHOT_STALE")

	w := get(viewer, route(linked)+query)
	status(w, 200, "")
	var got knowledge.RouteSummary
	if e = json.Unmarshal(w.Body.Bytes(), &got); e != nil {
		t.Fatal(e)
	}
	var names []string
	for _, s := range got.Steps {
		names = append(names, s.DisplayName)
	}
	if got.Readiness != "READY" || strings.Join(names, ",") != "Blank,Press,Trim" || got.StepCount != 3 || got.SnapshotID != linked || got.ProcessName != "Bracket" {
		t.Fatalf("route %s", w.Body.String())
	}
	if got.CycleTimeSeconds.Value == nil || *got.CycleTimeSeconds.Value != "270.5" || len(got.CycleTimeSeconds.ClaimIDs) != 3 {
		t.Fatalf("cycle total %+v", got.CycleTimeSeconds)
	}
	if got.SetupTimeSeconds.Value != nil || got.SetupTimeSeconds.StepsMissing != 3 {
		t.Fatalf("setup time is missing, never zero: %+v", got.SetupTimeSeconds)
	}
	if len(got.OrderClaimIDs) != 5 || got.Steps[0].CycleTimeSeconds.SourceSpanIDs[0] != span.ID {
		t.Fatalf("citations %+v", got)
	}

	// Strict query and non-disclosing scope.
	for _, q := range []string{"", "?process_id=" + process, "?as_of=2026-10-01T00%3A00%3A00Z", query + "&process_id=" + process, query + "&extra=1", "?process_id=x&as_of=2026-10-01T00%3A00%3A00Z", "?process_id=" + process + "&as_of=2026-10-01"} {
		status(get(viewer, route(linked)+q), 422, "")
	}
	status(get(viewer, route(linked)+"?process_id="+ops[0]+"&as_of=2026-10-01T00%3A00%3A00Z"), 404, "NOT_FOUND")
	status(get(viewer, route(uuid.NewString())+query), 404, "NOT_FOUND")
	status(get(viewer, "/v1/sites/"+foreign+"/process-snapshots/"+linked+"/route-summary"+query), 404, "")

	// Negative control at the HTTP boundary: both of Coil's operations have approved times, but
	// nothing orders them, so the route is a gap and carries no total.
	w = get(viewer, route(linked)+"?process_id="+coil+"&as_of=2026-10-01T00%3A00%3A00Z")
	status(w, 200, "")
	var gap knowledge.RouteSummary
	if e = json.Unmarshal(w.Body.Bytes(), &gap); e != nil {
		t.Fatal(e)
	}
	if gap.Readiness != "NOT_READY" || strings.Join(gap.ReasonCodes, ",") != knowledge.RouteIncomplete || gap.CycleTimeSeconds.Value != nil || gap.StepCount != 2 || gap.Steps[0].Position != nil {
		t.Fatalf("gapped route %s", w.Body.String())
	}

	if _, e = ev.Revoke(ctx, ea, es, source.SourceID, "Remove the route sheet", uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	status(get(viewer, route(linked)+query), 404, "")
}
