//go:build integration

package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/google/uuid"
)

func TestPostgresApprovedGraphAccess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("real PostgreSQL URLs required")
	}
	control, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })
	database := "graph_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = control.ExecContext(ctx, `CREATE DATABASE `+database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 60*time.Second)
		defer stop()
		if _, e := control.ExecContext(cleanup, `DROP DATABASE IF EXISTS `+database+` WITH (FORCE)`); e != nil {
			t.Error(e)
		}
	})
	connect := func(raw string) *sql.DB {
		t.Helper()
		u, e := url.Parse(raw)
		if e != nil {
			t.Fatal(e)
		}
		u.Path = "/" + database
		db, e := sql.Open("pgx", u.String())
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	admin, runtime := connect(adminURL), connect(runtimeURL)
	if err = migrate.Apply(ctx, admin); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := admin.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	sc, foreign := Scope{uuid.NewString(), uuid.NewString()}, Scope{uuid.NewString(), uuid.NewString()}
	for _, s := range []Scope{sc, foreign} {
		exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'graph synthetic')`, s.TenantID)
		exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'graph synthetic')`, s.TenantID, s.SiteID)
	}
	actor := func(role string) Actor {
		t.Helper()
		a := Actor{"https://graph.invalid", uuid.NewString(), strings.ReplaceAll(uuid.NewString(), "-", "") + strings.ReplaceAll(uuid.NewString(), "-", "")}
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'fixture',now(),now(),now()+interval '1 hour',now())`, a.SessionHash, a.Issuer, a.Subject)
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, role)
		return a
	}
	owner, supervisor, _, viewer := actor("owner"), actor("supervisor"), actor("operator"), actor("viewer")
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, foreign.TenantID, foreign.SiteID, owner.Issuer, owner.Subject)
	service, ev := New(runtime), evidence.New(runtime)
	ea := evidence.Actor{Issuer: owner.Issuer, Subject: owner.Subject, SessionHash: owner.SessionHash}
	es := evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}
	source := func() evidence.Input {
		t.Helper()
		in := evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Resolution source", Text: "Synthetic conflict evidence"}
		if _, e := ev.CreateOrRevise(ctx, ea, es, in, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		return in
	}
	origin := source()
	spanFor := func(in evidence.Input) evidence.Span {
		t.Helper()
		v, e := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: in.SourceID, Revision: 1, Start: 0, End: 9}, uuid.NewString(), 1)
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	originSpan := spanFor(origin)
	entity := EntityInput{EntityID: uuid.NewString(), Type: "operation", Name: "Approved operation", SourceID: origin.SourceID, SourceRevision: 1}
	if _, err = service.CreateEntity(ctx, owner, sc, entity, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	claim := ClaimInput{ClaimID: uuid.NewString(), SubjectID: entity.EntityID, SubjectRevision: 1, Predicate: "actual_step", Literal: &Literal{Type: "string", Value: json.RawMessage(`"Use the documented procedure"`)}, EffectiveFrom: "2026-09-27T00:00:00Z", EffectiveTo: func() *string { v := "2026-09-28T00:00:00Z"; return &v }(), EvidenceClass: "report", SourceSpanIDs: []string{originSpan.ID}}
	if _, err = service.CreateClaim(ctx, owner, sc, claim, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err = ev.DecideAuthority(ctx, ea, es, origin.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Validated document"}, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	_, err = service.DecideReview(ctx, supervisor, sc, claim.ClaimID, ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Reviewed instruction"}, uuid.NewString(), 1)
	if err != nil {
		t.Fatal(err)
	}

	capture := func() Snapshot {
		t.Helper()
		epoch, e := service.ReadInventory(ctx, owner, sc)
		if e != nil {
			t.Fatal(e)
		}
		snapshot, e := service.CreateSnapshot(ctx, owner, sc, SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Approved graph fixture"}, uuid.NewString(), epoch.Revision)
		if e != nil {
			t.Fatal(e)
		}
		return snapshot
	}
	snapshot := capture()
	query := GraphQuery{Direction: "outgoing", Roots: []string{entity.EntityID}, Predicates: []string{"actual_step"}, AsOf: "2026-09-27T12:00:00Z", Hops: 2, MaxEntities: 200, MaxClaims: 500}
	read := func(a Actor, scope Scope, id string, q GraphQuery) ([]byte, error) {
		return service.ReadGraph(ctx, a, scope, id, q)
	}
	decode := func(raw []byte) ([]SnapshotEntity, []SnapshotClaim) {
		t.Helper()
		var result struct {
			Entities []SnapshotEntity `json:"entities"`
			Claims   []SnapshotClaim  `json:"claims"`
		}
		if e := json.Unmarshal(raw, &result); e != nil {
			t.Fatal(e)
		}
		return result.Entities, result.Claims
	}
	raw, e := read(viewer, sc, snapshot.SnapshotID, query)
	if e != nil {
		t.Fatal(e)
	}
	entities, claims := decode(raw)
	if len(entities) != 1 || len(claims) != 1 || claims[0].ClaimID != claim.ClaimID {
		t.Fatalf("approved traversal entities=%+v claims=%+v", entities, claims)
	}
	// Reading requires a live session and membership, but no fresh step-up proof.
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now()-interval '1 hour' WHERE token_hash=$1`, viewer.SessionHash)
	if _, e = read(viewer, sc, snapshot.SnapshotID, query); e != nil {
		t.Fatalf("old authentication denied member read: %v", e)
	}
	boundary := query
	boundary.AsOf = "2026-09-28T00:00:00Z"
	raw, e = read(viewer, sc, snapshot.SnapshotID, boundary)
	if e != nil {
		t.Fatal(e)
	}
	_, claims = decode(raw)
	if len(claims) != 0 {
		t.Fatal("exclusive interval end leaked claim")
	}
	unknown := query
	unknown.Roots = []string{uuid.NewString()}
	if raw, e = read(viewer, sc, snapshot.SnapshotID, unknown); !errors.Is(e, ErrNotFound) || len(raw) != 0 {
		t.Fatalf("unknown root bytes=%d error=%v", len(raw), e)
	}
	if raw, e = read(owner, foreign, snapshot.SnapshotID, query); !errors.Is(e, ErrNotFound) || len(raw) != 0 {
		t.Fatalf("cross-site graph bytes=%d error=%v", len(raw), e)
	}
	if raw, e = read(viewer, sc, "invalid", query); !errors.Is(e, ErrInvalidContract) || len(raw) != 0 {
		t.Fatalf("invalid ID=%v", e)
	}
	exec(`UPDATE supervisor.memberships SET active=false WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4`, sc.TenantID, sc.SiteID, viewer.Issuer, viewer.Subject)
	if raw, e = read(viewer, sc, snapshot.SnapshotID, query); !errors.Is(e, ErrNotFound) || len(raw) != 0 {
		t.Fatalf("revoked membership bytes=%d error=%v", len(raw), e)
	}
	exec(`UPDATE supervisor.memberships SET active=true WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4`, sc.TenantID, sc.SiteID, viewer.Issuer, viewer.Subject)
	exec(`UPDATE supervisor.browser_sessions SET expires_at=now()-interval '1 second' WHERE token_hash=$1`, viewer.SessionHash)
	if raw, e = read(viewer, sc, snapshot.SnapshotID, query); !errors.Is(e, ErrDenied) || len(raw) != 0 {
		t.Fatalf("expired session bytes=%d error=%v", len(raw), e)
	}
	exec(`UPDATE supervisor.browser_sessions SET expires_at=now()+interval '1 hour' WHERE token_hash=$1`, viewer.SessionHash)
	// No computed payload escapes when the audit transaction cannot commit.
	var before, after int
	if e = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_audit WHERE tenant_id=$1 AND site_id=$2 AND target_id=$3 AND event_type='snapshot.read'`, sc.TenantID, sc.SiteID, snapshot.SnapshotID).Scan(&before); e != nil {
		t.Fatal(e)
	}
	blocker, e := admin.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback()
	if _, e = blocker.ExecContext(ctx, `LOCK TABLE supervisor.knowledge_audit IN ACCESS EXCLUSIVE MODE`); e != nil {
		t.Fatal(e)
	}
	short, stop := context.WithTimeout(ctx, 150*time.Millisecond)
	raw, e = service.ReadGraph(short, viewer, sc, snapshot.SnapshotID, query)
	stop()
	if e == nil || len(raw) != 0 {
		t.Fatalf("audit failure returned graph bytes=%d error=%v", len(raw), e)
	}
	if e = blocker.Rollback(); e != nil {
		t.Fatal(e)
	}
	if e = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_audit WHERE tenant_id=$1 AND site_id=$2 AND target_id=$3 AND event_type='snapshot.read'`, sc.TenantID, sc.SiteID, snapshot.SnapshotID).Scan(&after); e != nil {
		t.Fatal(e)
	}
	if before != after {
		t.Fatalf("failed graph audit persisted: %d -> %d", before, after)
	}
	source() // Even an unrelated committed inventory change makes retrieval stale.
	if raw, e = read(viewer, sc, snapshot.SnapshotID, query); !errors.Is(e, ErrSnapshotStale) || len(raw) != 0 {
		t.Fatalf("stale graph bytes=%d error=%v", len(raw), e)
	}
	historical, e := service.ReadSnapshot(ctx, viewer, sc, snapshot.SnapshotID)
	if e != nil || !historical.Stale {
		t.Fatalf("historical snapshot=%+v %v", historical, e)
	}
	snapshot = capture()
	if _, e = read(viewer, sc, snapshot.SnapshotID, query); e != nil {
		t.Fatal(e)
	}
	// An unapproved resolution peer is still a pinned access dependency.
	peerSource := source()
	peerSpan := spanFor(peerSource)
	otherEntity := entity
	otherEntity.EntityID = uuid.NewString()
	otherEntity.Name = "Resolved operation"
	if _, e = service.CreateEntity(ctx, owner, sc, otherEntity, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	winner := claim
	winner.ClaimID = uuid.NewString()
	winner.SubjectID = otherEntity.EntityID
	if _, e = service.CreateClaim(ctx, owner, sc, winner, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	loser := winner
	loser.ClaimID = uuid.NewString()
	loser.Literal = &Literal{Type: "string", Value: json.RawMessage(`"Different report"`)}
	loser.SourceSpanIDs = []string{peerSpan.ID}
	if _, e = service.CreateClaim(ctx, owner, sc, loser, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	conflicts, e := service.ListConflicts(ctx, owner, sc, winner.ClaimID, 1, 10, "")
	if e != nil || len(conflicts.Conflicts) != 1 {
		t.Fatalf("pair=%+v %v", conflicts, e)
	}
	pair := conflicts.Conflicts[0]
	decision := "prefer_left"
	if pair.Left.ClaimID != winner.ClaimID {
		decision = "prefer_right"
	}
	if _, e = service.DecideResolution(ctx, owner, sc, pair.ID, ResolutionInput{Decision: decision, Reason: "Preferred documented fact"}, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	if _, e = service.DecideReview(ctx, owner, sc, winner.ClaimID, ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Reviewed selected resolution"}, uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	peerSnapshot := capture()
	if _, e = read(viewer, sc, peerSnapshot.SnapshotID, query); e != nil {
		t.Fatal(e)
	}
	if _, e = ev.Revoke(ctx, ea, es, peerSource.SourceID, "Withdraw losing source", uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	if raw, e = read(viewer, sc, peerSnapshot.SnapshotID, query); !errors.Is(e, ErrNotFound) || len(raw) != 0 {
		t.Fatalf("revoked peer bytes=%d error=%v", len(raw), e)
	}
	current := capture()
	if _, e = ev.Revoke(ctx, ea, es, origin.SourceID, "Withdraw direct source", uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	if raw, e = read(viewer, sc, current.SnapshotID, query); !errors.Is(e, ErrNotFound) || len(raw) != 0 {
		t.Fatalf("revoked source bytes=%d error=%v", len(raw), e)
	}
}
