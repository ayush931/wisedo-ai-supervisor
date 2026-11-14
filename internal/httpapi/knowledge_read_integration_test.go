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
	"sort"
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

// WIS-270 GET /knowledge?entity_id=&as_of=&depth=: snapshot selection by capture
// time, explicit NOT_READY reasons, strict query parsing, scope and audit.
func TestKnowledgeHTTPReadAsOf(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
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
	name := "knowledge_read_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	tenant, site, foreignSite, otherTenant, otherSite := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, origin := "https://knowledge-read.invalid/"+uuid.NewString(), "http://127.0.0.1:58177"
	exec("INSERT INTO supervisor.tenants(id,name) VALUES($1,'read synthetic'),($2,'other tenant')", tenant, otherTenant)
	exec("INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'read synthetic'),($1,$3,'inaccessible'),($4,$5,'other tenant site')", tenant, site, foreignSite, otherTenant, otherSite)
	type session struct{ token, hash, subject string }
	seed := func(role, tn, st string) session {
		t.Helper()
		a := session{token: uuid.NewString(), subject: uuid.NewString()}
		sum := sha256.Sum256([]byte(a.token))
		a.hash = hex.EncodeToString(sum[:])
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, a.hash, a.subject, issuer, uuid.NewString())
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, tn, st, issuer, a.subject, role)
		return a
	}
	owner, viewer := seed("owner", tenant, site), seed("viewer", tenant, site)
	siblingOwner, otherOwner := seed("owner", tenant, foreignSite), seed("owner", otherTenant, otherSite)
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "knowledge-read", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, db.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)
	get := func(a session, siteID, rawQuery string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/v1/sites/"+siteID+"/knowledge?"+rawQuery, nil)
		if a.token != "" {
			r.AddCookie(&http.Cookie{Name: "supervisor", Value: a.token})
		}
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	type problemBody struct {
		Code string `json:"code"`
	}
	expectProblem := func(w *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		var p problemBody
		if w.Code != status || json.Unmarshal(w.Body.Bytes(), &p) != nil || p.Code != code {
			t.Fatalf("want HTTP%d %s, got HTTP%d: %s", status, code, w.Code, w.Body.String())
		}
	}
	type result struct {
		SchemaVersion     int    `json:"schema_version"`
		EntityID          string `json:"entity_id"`
		AsOf              string `json:"as_of"`
		Depth             int    `json:"depth"`
		Readiness         string `json:"readiness"`
		ReasonCode        string `json:"reason_code"`
		SnapshotID        string `json:"snapshot_id"`
		SHA256            string `json:"sha256"`
		CapturedAt        string `json:"captured_at"`
		InventoryRevision *int64 `json:"inventory_revision"`
		Graph             *struct {
			Truncated bool `json:"truncated"`
			Entities  []struct {
				EntityID string `json:"entity_id"`
			} `json:"entities"`
			Claims []struct {
				ClaimID string `json:"claim_id"`
			} `json:"claims"`
		} `json:"graph"`
	}
	auditCount := func() int {
		t.Helper()
		var n int
		if e := admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_audit WHERE tenant_id=$1 AND site_id=$2`, tenant, site).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	var lastTarget string
	// read performs one answered read and asserts exactly one audit row with no raw text.
	read := func(entityID string, extra url.Values) result {
		t.Helper()
		q := url.Values{"entity_id": {entityID}}
		for k, v := range extra {
			q[k] = v
		}
		before := auditCount()
		w := get(viewer, site, q.Encode())
		if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("HTTP%d: %s", w.Code, w.Body.String())
		}
		var out result
		var top map[string]json.RawMessage
		if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil || json.Unmarshal(w.Body.Bytes(), &top) != nil {
			t.Fatalf("%v: %s", e, w.Body.String())
		}
		for key := range top {
			if !strings.Contains(" schema_version entity_id as_of depth readiness reason_code snapshot_id sha256 captured_at inventory_revision graph ", " "+key+" ") {
				t.Fatalf("unexpected response field %q", key)
			}
		}
		if out.Readiness != "READY" && (out.Graph != nil || strings.Contains(w.Body.String(), `"graph"`)) {
			t.Fatalf("NOT_READY carried graph content: %s", w.Body.String())
		}
		if (out.Readiness == "READY") != (out.ReasonCode == "") || out.SchemaVersion != 1 || out.EntityID != entityID {
			t.Fatalf("readiness envelope inconsistent: %s", w.Body.String())
		}
		if after := auditCount(); after != before+1 {
			t.Fatalf("audit rows %d -> %d, want exactly one", before, after)
		}
		var row, event string
		var target sql.NullString
		if e := admin.QueryRowContext(ctx, `SELECT row_to_json(a)::text,a.event_type,a.target_id::text FROM supervisor.knowledge_audit a WHERE tenant_id=$1 AND site_id=$2 ORDER BY created_at DESC,id DESC LIMIT 1`, tenant, site).Scan(&row, &event, &target); e != nil {
			t.Fatal(e)
		}
		lastTarget = target.String
		if event != "snapshot.read" || (out.SnapshotID != "" && target.String != out.SnapshotID) || target.Valid != (out.ReasonCode != knowledge.ReasonNoSnapshot) {
			t.Fatalf("audit row %s does not identify snapshot %q", row, out.SnapshotID)
		}
		for _, raw := range []string{"Operation A then B then C", "Routing operation", "Verified route edge"} {
			if strings.Contains(row, raw) {
				t.Fatalf("audit row leaked raw text: %s", row)
			}
		}
		return out
	}
	expectNotReady := func(r result, reason, snapshotID string) {
		t.Helper()
		if r.Readiness != "NOT_READY" || r.ReasonCode != reason || r.SnapshotID != snapshotID {
			t.Fatalf("want NOT_READY/%s on %q, got %+v", reason, snapshotID, r)
		}
	}
	ids := []string{uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()}

	expectProblem(get(session{}, site, "entity_id="+ids[0]), 401, "AUTHENTICATION_REQUIRED")

	sc := knowledge.Scope{TenantID: tenant, SiteID: site}
	actor := knowledge.Actor{Issuer: issuer, Subject: owner.subject, SessionHash: owner.hash}
	service, ev := knowledge.New(db.DB), evidence.New(db.DB)
	es := evidence.Scope{TenantID: tenant, SiteID: site}
	ea := evidence.Actor{Issuer: issuer, Subject: owner.subject, SessionHash: owner.hash}
	// foreignGraph captures an approved two-entity snapshot in another site and
	// returns one of its entities; no read through our site may ever surface it.
	foreignGraph := func(tn, st string, o session) string {
		t.Helper()
		fsc, fes := knowledge.Scope{TenantID: tn, SiteID: st}, evidence.Scope{TenantID: tn, SiteID: st}
		fa, fea := knowledge.Actor{Issuer: issuer, Subject: o.subject, SessionHash: o.hash}, evidence.Actor{Issuer: issuer, Subject: o.subject, SessionHash: o.hash}
		src, e := ev.CreateOrRevise(ctx, fea, fes, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Foreign route", Text: "Foreign secret route"}, uuid.NewString(), 0)
		if e != nil {
			t.Fatal(e)
		}
		sp, e := ev.CreateSpan(ctx, fea, fes, evidence.SpanInput{SourceID: src.SourceID, Revision: 1, Start: 0, End: 7}, uuid.NewString(), 1)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = ev.DecideAuthority(ctx, fea, fes, src.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Foreign owner verified"}, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		pair := []string{uuid.NewString(), uuid.NewString()}
		for _, id := range pair {
			if _, e = service.CreateEntity(ctx, fa, fsc, knowledge.EntityInput{EntityID: id, Type: "operation", Name: "Foreign secret operation", SourceID: src.SourceID, SourceRevision: 1}, uuid.NewString(), 0); e != nil {
				t.Fatal(e)
			}
		}
		c := knowledge.ClaimInput{ClaimID: uuid.NewString(), SubjectID: pair[0], SubjectRevision: 1, Predicate: "precedes", ObjectEntityID: pair[1], ObjectRevision: 1, EffectiveFrom: "2020-01-01T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{sp.ID}}
		if _, e = service.CreateClaim(ctx, fa, fsc, c, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		if _, e = service.DecideReview(ctx, fa, fsc, c.ClaimID, knowledge.ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Foreign edge"}, uuid.NewString(), 1); e != nil {
			t.Fatal(e)
		}
		inv, e := service.ReadInventory(ctx, fa, fsc)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = service.CreateSnapshot(ctx, fa, fsc, knowledge.SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Foreign cut"}, uuid.NewString(), inv.Revision); e != nil {
			t.Fatal(e)
		}
		return pair[0]
	}
	foreign := []string{foreignGraph(tenant, foreignSite, siblingOwner), foreignGraph(otherTenant, otherSite, otherOwner)}
	empty := read(ids[0], nil)
	expectNotReady(empty, knowledge.ReasonNoSnapshot, "")
	if _, e := time.Parse(time.RFC3339Nano, empty.AsOf); e != nil || !strings.HasSuffix(empty.AsOf, "Z") || empty.Depth != 1 {
		t.Fatalf("omitted as_of/depth not resolved to server clock/default 1: %+v", empty)
	}
	// Snapshots of a sibling site and of another tenant are never selected for this site.
	for _, id := range foreign {
		expectNotReady(read(id, url.Values{"depth": {"2"}}), knowledge.ReasonNoSnapshot, "")
	}
	source, e := ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Approved route", Text: "Operation A then B then C"}, uuid.NewString(), 0)
	if e != nil {
		t.Fatal(e)
	}
	span, e := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: source.SourceID, Revision: 1, Start: 0, End: 9}, uuid.NewString(), 1)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = ev.DecideAuthority(ctx, ea, es, source.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Owner verified routing"}, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	// ids[3] is a real entity with no approved claim, so no snapshot contains it.
	for _, id := range ids {
		if _, e = service.CreateEntity(ctx, actor, sc, knowledge.EntityInput{EntityID: id, Type: "operation", Name: "Routing operation", SourceID: source.SourceID, SourceRevision: 1}, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
	}
	// claims[2] (ids[2] -> ids[4]) is approved but only effective from 2099, so it is
	// captured yet never in force at any as_of a snapshot can be selected for.
	claims := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	for i, claimID := range claims {
		object, from := ids[i+1], "2020-01-01T00:00:00Z"
		if i == 2 {
			object, from = ids[4], "2099-01-01T00:00:00Z"
		}
		c := knowledge.ClaimInput{ClaimID: claimID, SubjectID: ids[i], SubjectRevision: 1, Predicate: "precedes", ObjectEntityID: object, ObjectRevision: 1, EffectiveFrom: from, EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
		if _, e = service.CreateClaim(ctx, actor, sc, c, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		if _, e = service.DecideReview(ctx, actor, sc, claimID, knowledge.ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Verified route edge"}, uuid.NewString(), 1); e != nil {
			t.Fatal(e)
		}
	}
	// property is a literal claim on ids[1]: literal claims appear at every depth, even 0.
	property := uuid.NewString()
	literal := knowledge.ClaimInput{ClaimID: property, SubjectID: ids[1], SubjectRevision: 1, Predicate: "capacity_per_hour", Literal: &knowledge.Literal{Type: "decimal", Value: json.RawMessage(`"125.5"`), Unit: "units/hour"}, EffectiveFrom: "2020-01-01T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
	if _, e = service.CreateClaim(ctx, actor, sc, literal, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	if _, e = service.DecideReview(ctx, actor, sc, property, knowledge.ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Verified capacity"}, uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	inventory, e := service.ReadInventory(ctx, actor, sc)
	if e != nil {
		t.Fatal(e)
	}
	// Snapshots do not advance the inventory, so both cuts stay current and readable.
	first, e := service.CreateSnapshot(ctx, actor, sc, knowledge.SnapshotInput{SnapshotID: uuid.NewString(), Reason: "First routing cut"}, uuid.NewString(), inventory.Revision)
	if e != nil {
		t.Fatal(e)
	}
	second, e := service.CreateSnapshot(ctx, actor, sc, knowledge.SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Second routing cut"}, uuid.NewString(), inventory.Revision)
	if e != nil {
		t.Fatal(e)
	}
	if !first.CreatedAt.Before(second.CreatedAt.Add(-time.Microsecond)) {
		t.Fatalf("capture times not separable: %v %v", first.CreatedAt, second.CreatedAt)
	}
	at := func(v time.Time) url.Values { return url.Values{"as_of": {v.UTC().Format(time.RFC3339Nano)}} }
	expectNotReady(read(ids[0], at(first.CreatedAt.Add(-time.Microsecond))), knowledge.ReasonNoSnapshot, "")
	utc := func(v time.Time) string { return v.UTC().Format(time.RFC3339Nano) }
	for _, c := range []struct {
		name   string
		asOf   url.Values
		echoed string // "" for an omitted as_of (server clock)
		want   knowledge.Snapshot
	}{
		{"exactly first capture", at(first.CreatedAt), utc(first.CreatedAt), first},
		{"just before second", at(second.CreatedAt.Add(-time.Microsecond)), utc(second.CreatedAt.Add(-time.Microsecond)), first},
		{"first capture with offset", url.Values{"as_of": {first.CreatedAt.In(time.FixedZone("", 5*3600+1800)).Format("2006-01-02T15:04:05.999999-07:00")}}, utc(first.CreatedAt), first},
		{"exactly second capture", at(second.CreatedAt), utc(second.CreatedAt), second},
		{"omitted as_of", nil, "", second},
	} {
		got := read(ids[0], c.asOf)
		if got.Readiness != "READY" || got.SnapshotID != c.want.SnapshotID || got.SHA256 != c.want.SHA256 || got.CapturedAt != utc(c.want.CreatedAt) || got.InventoryRevision == nil || *got.InventoryRevision != inventory.Revision {
			t.Fatalf("%s: selected %+v, want snapshot %s", c.name, got, c.want.SnapshotID)
		}
		if c.echoed != "" && got.AsOf != c.echoed {
			t.Fatalf("%s: as_of %s, want UTC %s", c.name, got.AsOf, c.echoed)
		}
	}

	sorted := func(v ...string) []string { v = append([]string(nil), v...); sort.Strings(v); return v }
	for _, c := range []struct {
		root     string
		depth    string
		entities []string
		claims   []string
	}{
		{ids[1], "0", []string{ids[1]}, []string{property}},
		{ids[1], "1", []string{ids[0], ids[1], ids[2]}, []string{claims[0], claims[1], property}},
		{ids[0], "0", []string{ids[0]}, nil},
		{ids[0], "", []string{ids[0], ids[1]}, []string{claims[0], property}},
		{ids[0], "1", []string{ids[0], ids[1]}, []string{claims[0], property}},
		{ids[0], "2", []string{ids[0], ids[1], ids[2]}, []string{claims[0], claims[1], property}},
		{ids[2], "2", []string{ids[0], ids[1], ids[2]}, []string{claims[0], claims[1], property}},
		{ids[4], "2", []string{ids[4]}, nil}, // captured, but its only claim is not yet in force

	} {
		extra := url.Values{}
		if c.depth != "" {
			extra.Set("depth", c.depth)
		}
		got := read(c.root, extra)
		if got.Readiness != "READY" || got.Graph == nil || got.Graph.Truncated {
			t.Fatalf("depth %q from %s: %+v", c.depth, c.root, got)
		}
		var gotEntities, gotClaims []string
		for _, x := range got.Graph.Entities {
			gotEntities = append(gotEntities, x.EntityID)
		}
		for _, x := range got.Graph.Claims {
			gotClaims = append(gotClaims, x.ClaimID)
		}
		if strings.Join(gotEntities, ",") != strings.Join(sorted(c.entities...), ",") || strings.Join(gotClaims, ",") != strings.Join(sorted(c.claims...), ",") {
			t.Fatalf("depth %q from %s: entities %v claims %v, want %v %v", c.depth, c.root, gotEntities, gotClaims, c.entities, c.claims)
		}
	}

	// Same created_at: the greater snapshot_id wins. Clones carry the first cut's exact
	// canonical body (and so its digest) under fixed low/high identifiers.
	clone := func(id, createdAt string) {
		t.Helper()
		exec(`INSERT INTO supervisor.knowledge_process_snapshots(tenant_id,site_id,snapshot_id,inventory_revision,sha256,canonical_contents,reason,actor_issuer,actor_subject,auth_time,created_at) SELECT tenant_id,site_id,$3,inventory_revision,sha256,canonical_contents,reason,actor_issuer,actor_subject,auth_time,`+createdAt+` FROM supervisor.knowledge_process_snapshots WHERE tenant_id=$1 AND site_id=$2 AND snapshot_id=$4`, tenant, site, id, first.SnapshotID)
	}
	low, high := "00000000-0000-4000-8000-000000000001", "ffffffff-ffff-4fff-bfff-ffffffffffff"
	for _, c := range []struct{ insert, want string }{{low, first.SnapshotID}, {high, high}} {
		clone(c.insert, "(SELECT created_at FROM supervisor.knowledge_process_snapshots WHERE tenant_id=$1 AND site_id=$2 AND snapshot_id=$4)")
		if got := read(ids[0], at(first.CreatedAt)); got.Readiness != "READY" || got.SnapshotID != c.want || got.SHA256 != first.SHA256 || got.CapturedAt != utc(first.CreatedAt) {
			t.Fatalf("tie after inserting %s: %+v, want %s", c.insert, got, c.want)
		}
	}
	// Later cuts are unaffected by an earlier tie.
	if got := read(ids[0], at(second.CreatedAt)); got.SnapshotID != second.SnapshotID {
		t.Fatalf("tie leaked forward: %+v", got)
	}

	expectNotReady(read(ids[3], nil), knowledge.ReasonEntityNotInSnapshot, second.SnapshotID)
	expectNotReady(read(uuid.NewString(), nil), knowledge.ReasonEntityNotInSnapshot, second.SnapshotID)
	for _, id := range foreign {
		expectNotReady(read(id, url.Values{"depth": {"2"}}), knowledge.ReasonEntityNotInSnapshot, second.SnapshotID)
	}

	before := auditCount()
	valid := "entity_id=" + ids[0]
	for _, c := range []struct{ query, code string }{
		{"", "INVALID_REQUEST"},
		{"depth=1", "INVALID_REQUEST"},
		{valid + "&unknown=1", "INVALID_REQUEST"},
		{valid + "&entity_id=" + ids[1], "INVALID_REQUEST"},
		{valid + "&depth=1&depth=1", "INVALID_REQUEST"},
		{valid + "&as_of=2026-10-01T00%3A00%3A00Z&as_of=2026-10-01T00%3A00%3A00Z", "INVALID_REQUEST"},
		{valid + "&depth=3", "INVALID_REQUEST"},
		{valid + "&depth=-1", "INVALID_REQUEST"},
		{valid + "&depth=01", "INVALID_REQUEST"},
		{valid + "&depth=%2B1", "INVALID_REQUEST"},
		{valid + "&depth=", "INVALID_REQUEST"},
		{valid + "&as_of=", "INVALID_REQUEST"},
		{valid + "&bad=%zz", "INVALID_REQUEST"},
		{"entity_id=not-a-uuid", "INVALID_CLAIM"},
		{"entity_id=" + strings.ToUpper(ids[0]), "INVALID_CLAIM"},
		{"entity_id=", "INVALID_CLAIM"},
		{valid + "&as_of=2026-10-01T00%3A00%3A00", "INVALID_CLAIM"},
		{valid + "&as_of=1759276800", "INVALID_CLAIM"},
		{valid + "&as_of=2026-10-01", "INVALID_CLAIM"},
		{valid + "&as_of=not-time", "INVALID_CLAIM"},
		{valid + "&as_of=2026-10-01T00%3A00%3A00.1234567Z", "INVALID_CLAIM"},
		// The future has no recorded cut: as_of after the server clock is rejected.
		{valid + "&as_of=9999-12-31T23%3A59%3A59Z", "INVALID_CLAIM"},
	} {
		expectProblem(get(viewer, site, c.query), 422, c.code)
	}
	// Cross-site and cross-tenant lookups are non-disclosing, and a revoked member loses access.
	expectProblem(get(viewer, foreignSite, valid), 404, "NOT_FOUND")
	expectProblem(get(viewer, otherSite, valid), 404, "NOT_FOUND")
	exec(`UPDATE supervisor.memberships SET active=false WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4`, tenant, site, issuer, viewer.subject)
	expectProblem(get(viewer, site, valid), 404, "NOT_FOUND")
	exec(`UPDATE supervisor.memberships SET active=true WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4`, tenant, site, issuer, viewer.subject)
	if after := auditCount(); after != before {
		t.Fatalf("rejected reads wrote audit rows: %d -> %d", before, after)
	}

	// A reader that began before a capture but was serialized behind it on the site
	// lock sees that capture when as_of is omitted (clock read after the lock).
	blocker, e := admin.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback()
	var blockerPID int
	if e = blocker.QueryRowContext(ctx, `SELECT pg_backend_pid() FROM supervisor.sites WHERE tenant_id=$1 AND id=$2 FOR NO KEY UPDATE`, tenant, site).Scan(&blockerPID); e != nil {
		t.Fatal(e)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- get(viewer, site, valid) }()
	for waited := 0; ; waited++ {
		var blocked int
		if e = admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))`, blockerPID).Scan(&blocked); e != nil {
			t.Fatal(e)
		}
		if blocked > 0 {
			break
		}
		if waited > 500 {
			t.Fatal("reader never queued behind the site lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	late := "eeeeeeee-eeee-4eee-aeee-eeeeeeeeeeee"
	if _, e = blocker.ExecContext(ctx, `INSERT INTO supervisor.knowledge_process_snapshots(tenant_id,site_id,snapshot_id,inventory_revision,sha256,canonical_contents,reason,actor_issuer,actor_subject,auth_time) SELECT tenant_id,site_id,$3,inventory_revision,sha256,canonical_contents,reason,actor_issuer,actor_subject,clock_timestamp() FROM supervisor.knowledge_process_snapshots WHERE tenant_id=$1 AND site_id=$2 AND snapshot_id=$4`, tenant, site, late, second.SnapshotID); e != nil {
		t.Fatal(e)
	}
	if e = blocker.Commit(); e != nil {
		t.Fatal(e)
	}
	serialized := <-done
	var behind result
	if serialized.Code != 200 || json.Unmarshal(serialized.Body.Bytes(), &behind) != nil || behind.Readiness != "READY" || behind.SnapshotID != late {
		t.Fatalf("serialized reader missed the capture it waited for: HTTP%d %s", serialized.Code, serialized.Body.String())
	}

	// Any inventory change makes both cuts stale: they resolve but serve no content.
	if _, e = ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "New observation", Text: "Inventory changed"}, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	expectNotReady(read(ids[0], nil), knowledge.ReasonSnapshotStale, late)
	expectNotReady(read(ids[0], at(first.CreatedAt)), knowledge.ReasonSnapshotStale, high)
	// Revoking a cited source after capture is reported ahead of staleness, for every
	// cut, and discloses no snapshot identity (a direct read of it is a 404); the
	// resolved id stays only in the audit target.
	if _, e = ev.Revoke(ctx, ea, es, source.SourceID, "Remove captured source", uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	for _, c := range []struct {
		entity, target string
		asOf           url.Values
	}{{ids[0], late, nil}, {ids[0], high, at(first.CreatedAt)}, {ids[3], late, nil}} {
		got := read(c.entity, c.asOf)
		expectNotReady(got, knowledge.ReasonSnapshotUnavailable, "")
		if got.SHA256 != "" || got.CapturedAt != "" || got.InventoryRevision != nil || lastTarget != c.target {
			t.Fatalf("unavailable cut disclosed identity or lost audit target %q: %+v", lastTarget, got)
		}
	}
}
