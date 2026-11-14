//go:build integration

package knowledge

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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

func TestPostgresApprovedSnapshots(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
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
	database := "snapshot_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
		exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'snapshot synthetic')`, s.TenantID)
		exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'snapshot synthetic')`, s.TenantID, s.SiteID)
	}
	actor := func(role string) Actor {
		t.Helper()
		a := Actor{"https://snapshot.invalid", uuid.NewString(), strings.ReplaceAll(uuid.NewString(), "-", "") + strings.ReplaceAll(uuid.NewString(), "-", "")}
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'fixture',now(),now(),now()+interval '1 hour',now())`, a.SessionHash, a.Issuer, a.Subject)
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, role)
		return a
	}
	owner, supervisor, operator, viewer := actor("owner"), actor("supervisor"), actor("operator"), actor("viewer")
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
	claim := ClaimInput{ClaimID: uuid.NewString(), SubjectID: entity.EntityID, SubjectRevision: 1, Predicate: "actual_step", Literal: &Literal{Type: "string", Value: json.RawMessage(`"Use the documented procedure"`)}, EffectiveFrom: "2026-09-27T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{originSpan.ID}}
	if _, err = service.CreateClaim(ctx, owner, sc, claim, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err = ev.DecideAuthority(ctx, ea, es, origin.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Validated document"}, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	approved, err := service.DecideReview(ctx, supervisor, sc, claim.ClaimID, ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Reviewed instruction"}, uuid.NewString(), 1)
	if err != nil {
		t.Fatal(err)
	}
	// A captured candidate does not become approved merely because a snapshot is created.
	candidate := claim
	candidate.ClaimID = uuid.NewString()
	candidate.Predicate = "capacity_per_hour"
	candidate.Literal = &Literal{Type: "decimal", Value: json.RawMessage(`"100"`), Unit: "units/hour"}
	if _, err = service.CreateClaim(ctx, owner, sc, candidate, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	epoch := func() int64 {
		t.Helper()
		v, e := service.ReadInventory(ctx, owner, sc)
		if e != nil {
			t.Fatal(e)
		}
		return v.Revision
	}
	before := epoch()
	in := SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Capture independently reviewed process"}
	key := uuid.NewString()
	for _, denied := range []Actor{operator, viewer} {
		if _, e := service.CreateSnapshot(ctx, denied, sc, in, key, before); !errors.Is(e, ErrDenied) {
			t.Fatalf("role unexpectedly allowed snapshot: %v", e)
		}
	}
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now()-interval '16 minutes' WHERE token_hash=$1`, supervisor.SessionHash)
	if _, e := service.CreateSnapshot(ctx, supervisor, sc, in, key, before); !errors.Is(e, ErrReauthenticationRequired) {
		t.Fatalf("stale identity proof=%v", e)
	}
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash=$1`, supervisor.SessionHash)
	if _, e := service.CreateSnapshot(ctx, supervisor, sc, in, key, before-1); !errors.Is(e, ErrRevision) {
		t.Fatalf("inventory CAS=%v", e)
	}
	captured, err := service.CreateSnapshot(ctx, supervisor, sc, in, key, before)
	if err != nil {
		t.Fatal(err)
	}
	if captured.SnapshotID != in.SnapshotID || captured.InventoryRevision != before || captured.CurrentInventoryRevision != before || captured.Stale || captured.SchemaVersion != 1 || len(captured.Contents.Claims) != 1 || len(captured.Contents.Entities) != 1 {
		t.Fatalf("snapshot set/epoch mismatch: %+v", captured)
	}
	if captured.Contents.Claims[0].ClaimID != claim.ClaimID || captured.Contents.Claims[0].Revision != 1 || captured.Contents.Claims[0].ReviewID != approved.ReviewID || captured.Contents.Entities[0].EntityID != entity.EntityID || captured.Contents.Entities[0].Revision != 1 {
		t.Fatal("snapshot lost exact entity/claim/review pins")
	}
	if captured.ActorIssuer != supervisor.Issuer || captured.ActorSubject != supervisor.Subject || captured.AuthenticatedAt.IsZero() || captured.CreatedAt.IsZero() {
		t.Fatal("snapshot omitted server actor proof")
	}
	canonical, e := json.Marshal(captured.Contents)
	if e != nil {
		t.Fatal(e)
	}
	digest := sha256.Sum256(canonical)
	if captured.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("canonical content digest differs")
	}
	if epoch() != before {
		t.Fatal("snapshot creation changed graph inventory")
	}
	other := SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Different capture metadata"}
	identical, err := service.CreateSnapshot(ctx, owner, sc, other, uuid.NewString(), before)
	if err != nil {
		t.Fatal(err)
	}
	if captured.SHA256 != identical.SHA256 || captured.SnapshotID == identical.SnapshotID {
		t.Fatal("metadata changed canonical content digest")
	}
	if _, e := service.CreateSnapshot(ctx, supervisor, sc, in, uuid.NewString(), before); !errors.Is(e, ErrSnapshotConflict) {
		t.Fatalf("duplicate snapshotID=%v", e)
	}
	changed := in
	changed.Reason = "changed"
	if _, e := service.CreateSnapshot(ctx, supervisor, sc, changed, key, before); !errors.Is(e, ErrConflict) {
		t.Fatalf("changed replay=%v", e)
	}
	for _, reader := range []Actor{viewer, operator, owner} {
		got, e := service.ReadSnapshot(ctx, reader, sc, in.SnapshotID)
		if e != nil || got.SHA256 != captured.SHA256 || got.Stale {
			t.Fatalf("member read=%+v %v", got, e)
		}
	}
	if _, e := service.ReadSnapshot(ctx, owner, foreign, in.SnapshotID); !errors.Is(e, ErrNotFound) {
		t.Fatalf("foreign read=%v", e)
	}
	replayed, err := service.CreateSnapshot(ctx, supervisor, sc, in, key, before)
	if err != nil || replayed.SHA256 != captured.SHA256 {
		t.Fatalf("snapshot replay=%+v %v", replayed, err)
	}
	var outboxCount int
	if e := admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_outbox WHERE tenant_id=$1 AND site_id=$2 AND snapshot_id=$3`, sc.TenantID, sc.SiteID, in.SnapshotID).Scan(&outboxCount); e != nil || outboxCount != 1 {
		t.Fatalf("snapshot outboxcount=%d %v", outboxCount, e)
	}
	// A new inventory mutation marks the capture stale without rewriting its content.
	if _, e := ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Unrelated observation", Text: "New site observation"}, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	stale, err := service.ReadSnapshot(ctx, viewer, sc, in.SnapshotID)
	if err != nil || !stale.Stale || stale.SHA256 != captured.SHA256 || stale.InventoryRevision != before || stale.CurrentInventoryRevision <= before {
		t.Fatalf("stale immutable capture=%+v %v", stale, err)
	}
	staleReplay, err := service.CreateSnapshot(ctx, supervisor, sc, in, key, before)
	if err != nil || !staleReplay.Stale || staleReplay.SHA256 != captured.SHA256 {
		t.Fatalf("historical replay=%+v %v", staleReplay, err)
	}
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now()-interval '16 minutes' WHERE token_hash=$1`, supervisor.SessionHash)
	if _, e := service.CreateSnapshot(ctx, supervisor, sc, in, key, before); !errors.Is(e, ErrReauthenticationRequired) {
		t.Fatalf("replay skipped fresh identity=%v", e)
	}
	exec(`UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash=$1`, supervisor.SessionHash)
	// An audit failure must roll back snapshot, receipt and outbox together.
	rollbackInput := SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Atomic snapshot"}
	rollbackKey := uuid.NewString()
	latest := epoch()
	blocker, e := admin.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = blocker.ExecContext(ctx, `LOCK TABLE supervisor.knowledge_audit IN ACCESS EXCLUSIVE MODE`); e != nil {
		t.Fatal(e)
	}
	short, stop := context.WithTimeout(ctx, 150*time.Millisecond)
	_, e = service.CreateSnapshot(short, owner, sc, rollbackInput, rollbackKey, latest)
	stop()
	if e == nil {
		t.Fatal("audit failure accepted snapshot")
	}
	if e = blocker.Rollback(); e != nil {
		t.Fatal(e)
	}
	for _, table := range []string{"knowledge_process_snapshots", "knowledge_outbox"} {
		var count int
		if e = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.`+table+` WHERE tenant_id=$1 AND site_id=$2 AND snapshot_id=$3`, sc.TenantID, sc.SiteID, rollbackInput.SnapshotID).Scan(&count); e != nil || count != 0 {
			t.Fatalf("rollback %s rows=%d %v", table, count, e)
		}
	}
	if _, e = service.CreateSnapshot(ctx, owner, sc, rollbackInput, rollbackKey, latest); e != nil {
		t.Fatalf("retry rolledbackkey=%v", e)
	}
	// Actual database permissions protect immutable snapshots and tenant isolation.
	attempt, e := runtime.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = attempt.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, foreign.TenantID); e != nil {
		t.Fatal(e)
	}
	var visible int
	if e = attempt.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_process_snapshots WHERE tenant_id=$1`, sc.TenantID).Scan(&visible); e != nil || visible != 0 {
		t.Fatalf("cross tenant snapshots=%d %v", visible, e)
	}
	attempt.Rollback()
	for _, query := range []string{`UPDATE supervisor.knowledge_process_snapshots SET reason='forged' WHERE snapshot_id=$1`, `DELETE FROM supervisor.knowledge_process_snapshots WHERE snapshot_id=$1`} {
		tx, e := runtime.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); e != nil {
			t.Fatal(e)
		}
		_, e = tx.ExecContext(ctx, query, in.SnapshotID)
		tx.Rollback()
		requireSQLState(t, e, "42501")
	}
	// Concurrent distinct captures share the same immutable inventory projection.
	concurrentEpoch := epoch()
	results := make(chan Snapshot, 2)
	failures := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			v, e := service.CreateSnapshot(ctx, owner, sc, SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Concurrent capture"}, uuid.NewString(), concurrentEpoch)
			results <- v
			failures <- e
		}()
	}
	var concurrentHash string
	for i := 0; i < 2; i++ {
		v, e := <-results, <-failures
		if e != nil {
			t.Fatal(e)
		}
		if concurrentHash != "" && concurrentHash != v.SHA256 {
			t.Fatal("serialized concurrent captures differ")
		}
		concurrentHash = v.SHA256
	}
	// The resolved loser's source is part of captured provenance even though the
	// loser's candidate itself is excluded from the approved claim set.
	peerSource := source()
	peerSpan := spanFor(peerSource)
	resolvedEntity := entity
	resolvedEntity.EntityID = uuid.NewString()
	if _, e = service.CreateEntity(ctx, owner, sc, resolvedEntity, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	winner := claim
	winner.ClaimID = uuid.NewString()
	winner.SubjectID = resolvedEntity.EntityID
	loser := winner
	loser.ClaimID = uuid.NewString()
	loser.SourceSpanIDs = []string{peerSpan.ID}
	loser.Literal = &Literal{Type: "string", Value: json.RawMessage(`"Disputed instruction"`)}
	for _, v := range []ClaimInput{winner, loser} {
		if _, e = service.CreateClaim(ctx, owner, sc, v, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
	}
	pairs, e := service.ListConflicts(ctx, owner, sc, winner.ClaimID, 1, 100, "")
	if e != nil || len(pairs.Conflicts) != 1 {
		t.Fatalf("pair=%+v %v", pairs, e)
	}
	preference := "prefer_left"
	if pairs.Conflicts[0].Right.ClaimID == winner.ClaimID {
		preference = "prefer_right"
	}
	if _, e = service.DecideResolution(ctx, supervisor, sc, pairs.Conflicts[0].ID, ResolutionInput{Decision: preference, Reason: "Direct document establishes winner"}, uuid.NewString(), 0); e != nil {
		t.Fatal(e)
	}
	if _, e = service.DecideReview(ctx, supervisor, sc, winner.ClaimID, ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Verified resolved winner"}, uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	peerSnapshotInput := SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Resolved process"}
	peerKey, peerEpoch := uuid.NewString(), epoch()
	peerSnapshot, e := service.CreateSnapshot(ctx, supervisor, sc, peerSnapshotInput, peerKey, peerEpoch)
	if e != nil || len(peerSnapshot.Contents.Claims) != 2 {
		t.Fatalf("resolved capture=%+v %v", peerSnapshot, e)
	}
	if _, e = ev.Revoke(ctx, ea, es, peerSource.SourceID, "Losing report removed", uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	if _, e = service.ReadSnapshot(ctx, viewer, sc, peerSnapshotInput.SnapshotID); !errors.Is(e, ErrNotFound) {
		t.Fatalf("revoked resolution peer leaked capture=%v", e)
	}
	if _, e = service.CreateSnapshot(ctx, supervisor, sc, peerSnapshotInput, peerKey, peerEpoch); !errors.Is(e, ErrNotFound) {
		t.Fatalf("revoked peer replay=%v", e)
	}
	// The pre-eligibility scan of current approved-review candidates is bounded;
	// unreviewed capture rows alone must not consume this approval scan budget.
	seed, e := admin.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer seed.Rollback()
	if _, e = seed.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); e != nil {
		t.Fatal(e)
	}
	for _, q := range []string{
		`CREATE TEMP TABLE snapshot_seed_ids ON COMMIT DROP AS SELECT gen_random_uuid() AS id FROM generate_series(1,501)`,
		`INSERT INTO supervisor.knowledge_claim_heads(tenant_id,site_id,id,current_revision) SELECT $1,$2,id,1 FROM snapshot_seed_ids`,
		`INSERT INTO supervisor.knowledge_claim_revisions(tenant_id,site_id,claim_id,revision,subject_id,predicate,object_entity_id,payload,actor_issuer,actor_subject) SELECT r.tenant_id,r.site_id,i.id,1,r.subject_id,r.predicate,r.object_entity_id,jsonb_set(r.payload,'{claim_id}',to_jsonb(i.id::text)),r.actor_issuer,r.actor_subject FROM supervisor.knowledge_claim_revisions r CROSS JOIN snapshot_seed_ids i WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.claim_id=$3 AND r.revision=1`,
		`INSERT INTO supervisor.knowledge_claim_entity_refs(tenant_id,site_id,claim_id,claim_revision,subject_id,subject_revision,object_entity_id,object_revision) SELECT r.tenant_id,r.site_id,i.id,1,r.subject_id,r.subject_revision,r.object_entity_id,r.object_revision FROM supervisor.knowledge_claim_entity_refs r CROSS JOIN snapshot_seed_ids i WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.claim_id=$3 AND r.claim_revision=1`,
		`INSERT INTO supervisor.knowledge_claim_spans(tenant_id,site_id,claim_id,claim_revision,span_id,source_id,source_revision) SELECT r.tenant_id,r.site_id,i.id,1,r.span_id,r.source_id,r.source_revision FROM supervisor.knowledge_claim_spans r CROSS JOIN snapshot_seed_ids i WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.claim_id=$3 AND r.claim_revision=1`,
		`INSERT INTO supervisor.knowledge_claim_reviews(tenant_id,site_id,review_id,claim_id,claim_revision,verdict,reason,actor_issuer,actor_subject,auth_time,authority_decision_id,authority_source_id,authority_source_revision,authority_revision) SELECT r.tenant_id,r.site_id,gen_random_uuid(),i.id,1,r.verdict,r.reason,r.actor_issuer,r.actor_subject,clock_timestamp(),r.authority_decision_id,r.authority_source_id,r.authority_source_revision,r.authority_revision FROM supervisor.knowledge_claim_reviews r CROSS JOIN snapshot_seed_ids i WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.claim_id=$3 AND r.claim_revision=1`,
	} {
		var args []any
		if strings.Contains(q, "$1") {
			args = []any{sc.TenantID, sc.SiteID}
		}
		if strings.Contains(q, "$3") {
			if strings.Contains(q, "INSERT INTO supervisor.knowledge_claim_reviews") {
				args = append(args, claim.ClaimID)
			} else {
				args = append(args, candidate.ClaimID)
			}
		}
		if _, e = seed.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	if e = seed.Commit(); e != nil {
		t.Fatal(e)
	}
	limited := SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Reject oversized inventory"}
	if _, e = service.CreateSnapshot(ctx, owner, sc, limited, uuid.NewString(), epoch()); !errors.Is(e, ErrTraversalLimit) {
		t.Fatalf("candidate cap failed open=%v", e)
	}
	if _, e = service.ReadSnapshot(ctx, owner, sc, limited.SnapshotID); !errors.Is(e, ErrNotFound) {
		t.Fatalf("oversized snapshot persisted=%v", e)
	}
	if _, e = ev.Revoke(ctx, ea, es, origin.SourceID, "Captured source removed", uuid.NewString(), 1); e != nil {
		t.Fatal(e)
	}
	if _, e = service.ReadSnapshot(ctx, viewer, sc, in.SnapshotID); !errors.Is(e, ErrNotFound) {
		t.Fatalf("revoked captured provenance read=%v", e)
	}
	if _, e = service.CreateSnapshot(ctx, supervisor, sc, in, key, before); !errors.Is(e, ErrNotFound) {
		t.Fatalf("revoked captured provenance replay=%v", e)
	}
	// WIS-621: 40 claims of 2000 characters make a body of about 90 KiB. The old 64 KiB bound
	// refused it; the documented bounds are 200 entities and 500 claims, so it is accepted whole.
	for _, tc := range []struct {
		name                    string
		entities, claims, chars int
		accepted                bool
	}{
		{"WIS-621 body over the old 64 KiB bound", 1, 40, 2000, true},
		{"entity budget", 201, 201, 10, false},
		{"historical conflict budget", 1, 2, 10, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bounded := Scope{TenantID: sc.TenantID, SiteID: uuid.NewString()}
			exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'Snapshot budget fixture')`, bounded.TenantID, bounded.SiteID)
			exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, bounded.TenantID, bounded.SiteID, owner.Issuer, owner.Subject)
			boundedEvidence := evidence.Scope{TenantID: bounded.TenantID, SiteID: bounded.SiteID}
			src, e := ev.CreateOrRevise(ctx, ea, boundedEvidence, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Budget fixture", Text: "Directly reviewed statement"}, uuid.NewString(), 0)
			if e != nil {
				t.Fatal(e)
			}
			span, e := ev.CreateSpan(ctx, ea, boundedEvidence, evidence.SpanInput{SourceID: src.SourceID, Revision: 1, Start: 0, End: 9}, uuid.NewString(), 1)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = ev.DecideAuthority(ctx, ea, boundedEvidence, src.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Fixture source verified"}, uuid.NewString(), 0); e != nil {
				t.Fatal(e)
			}
			ids := make([]string, tc.entities)
			for i := range ids {
				ids[i] = uuid.NewString()
				if _, e = service.CreateEntity(ctx, owner, bounded, EntityInput{EntityID: ids[i], Type: "operation", Name: "Bounded operation", SourceID: src.SourceID, SourceRevision: 1}, uuid.NewString(), 0); e != nil {
					t.Fatal(e)
				}
			}
			literal, _ := json.Marshal(strings.Repeat("x", tc.chars))
			var budgetClaim string
			for i := 0; i < tc.claims; i++ {
				c := ClaimInput{ClaimID: uuid.NewString(), SubjectID: ids[i%len(ids)], SubjectRevision: 1, Predicate: "actual_step", Literal: &Literal{Type: "string", Value: literal}, EffectiveFrom: "2026-09-27T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
				if _, e = service.CreateClaim(ctx, owner, bounded, c, uuid.NewString(), 0); e != nil {
					t.Fatal(e)
				}
				if _, e = service.DecideReview(ctx, owner, bounded, c.ClaimID, ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Reviewed bounded claim"}, uuid.NewString(), 1); e != nil {
					t.Fatal(e)
				}
				budgetClaim = c.ClaimID
			}
			if tc.name == "historical conflict budget" {
				seed, e := admin.BeginTx(ctx, nil)
				if e != nil {
					t.Fatal(e)
				}
				defer seed.Rollback()
				if _, e = seed.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, bounded.TenantID); e != nil {
					t.Fatal(e)
				}
				for _, q := range []string{
					`CREATE TEMP TABLE snapshot_pair_ids ON COMMIT DROP AS SELECT gen_random_uuid() AS id FROM generate_series(1,600)`,
					`INSERT INTO supervisor.knowledge_claim_heads(tenant_id,site_id,id,current_revision) SELECT $1,$2,id,1 FROM snapshot_pair_ids`,
					`INSERT INTO supervisor.knowledge_claim_revisions(tenant_id,site_id,claim_id,revision,subject_id,predicate,object_entity_id,payload,actor_issuer,actor_subject) SELECT r.tenant_id,r.site_id,i.id,1,r.subject_id,r.predicate,r.object_entity_id,jsonb_set(jsonb_set(r.payload,'{claim_id}',to_jsonb(i.id::text)),'{literal,value}','"Disputed imported claim"'::jsonb),r.actor_issuer,r.actor_subject FROM supervisor.knowledge_claim_revisions r CROSS JOIN snapshot_pair_ids i WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.claim_id=$3 AND r.revision=1`,
					`INSERT INTO supervisor.knowledge_claim_entity_refs(tenant_id,site_id,claim_id,claim_revision,subject_id,subject_revision,object_entity_id,object_revision) SELECT r.tenant_id,r.site_id,i.id,1,r.subject_id,r.subject_revision,r.object_entity_id,r.object_revision FROM supervisor.knowledge_claim_entity_refs r CROSS JOIN snapshot_pair_ids i WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.claim_id=$3 AND r.claim_revision=1`,
					`INSERT INTO supervisor.knowledge_claim_spans(tenant_id,site_id,claim_id,claim_revision,span_id,source_id,source_revision) SELECT r.tenant_id,r.site_id,i.id,1,r.span_id,r.source_id,r.source_revision FROM supervisor.knowledge_claim_spans r CROSS JOIN snapshot_pair_ids i WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.claim_id=$3 AND r.claim_revision=1`,
					`INSERT INTO supervisor.knowledge_conflicts(tenant_id,site_id,id,left_claim_id,left_revision,right_claim_id,right_revision,kind,effective_from) SELECT r.tenant_id,r.site_id,gen_random_uuid(),LEAST(r.claim_id,i.id),1,GREATEST(r.claim_id,i.id),1,'text_review','2026-09-27T00:00:00Z'::timestamptz FROM snapshot_pair_ids i CROSS JOIN supervisor.knowledge_claim_reviews r WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.verdict='approve'`,
				} {
					var args []any
					if strings.Contains(q, "$1") {
						args = []any{bounded.TenantID, bounded.SiteID}
					}
					if strings.Contains(q, "$3") {
						args = append(args, budgetClaim)
					}
					if _, e = seed.ExecContext(ctx, q, args...); e != nil {
						t.Fatal(e)
					}
				}
				if e = seed.Commit(); e != nil {
					t.Fatal(e)
				}
			}
			inventory, e := service.ReadInventory(ctx, owner, bounded)
			if e != nil {
				t.Fatal(e)
			}
			input := SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Must reject partial capture"}
			captureKey := uuid.NewString()
			if tc.accepted {
				created, e := service.CreateSnapshot(ctx, owner, bounded, input, captureKey, inventory.Revision)
				if e != nil {
					t.Fatalf("snapshot within the documented bounds refused: %v", e)
				}
				var size int
				var digest string
				if e = admin.QueryRowContext(ctx, `SELECT octet_length(canonical_contents),encode(sha256(convert_to(canonical_contents,'UTF8')),'hex') FROM supervisor.knowledge_process_snapshots WHERE tenant_id=$1 AND site_id=$2 AND snapshot_id=$3`, bounded.TenantID, bounded.SiteID, input.SnapshotID).Scan(&size, &digest); e != nil {
					t.Fatal(e)
				}
				read, e := service.ReadSnapshot(ctx, owner, bounded, input.SnapshotID)
				if e != nil || size <= 64<<10 || digest != created.SHA256 || read.SHA256 != created.SHA256 || len(read.Contents.Claims) != tc.claims {
					t.Fatalf("stored %d bytes digest %s, created %s, read %d claims (%v)", size, digest, created.SHA256, len(read.Contents.Claims), e)
				}
				return
			}
			if _, e = service.CreateSnapshot(ctx, owner, bounded, input, captureKey, inventory.Revision); !errors.Is(e, ErrTraversalLimit) {
				t.Fatalf("%s failed open: %v", tc.name, e)
			}
			var count int
			if e = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_outbox WHERE tenant_id=$1 AND site_id=$2 AND snapshot_id=$3`, bounded.TenantID, bounded.SiteID, input.SnapshotID).Scan(&count); e != nil || count != 0 {
				t.Fatalf("oversized capture emitted outbox=%d %v", count, e)
			}
			if e = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_process_snapshots WHERE tenant_id=$1 AND site_id=$2 AND snapshot_id=$3`, bounded.TenantID, bounded.SiteID, input.SnapshotID).Scan(&count); e != nil || count != 0 {
				t.Fatalf("oversized snapshotrows=%d %v", count, e)
			}
			if e = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_receipts WHERE tenant_id=$1 AND site_id=$2 AND idempotency_key=$3`, bounded.TenantID, bounded.SiteID, captureKey).Scan(&count); e != nil || count != 0 {
				t.Fatalf("oversized receipted=%d %v", count, e)
			}
		})
	}
	// WIS-270: revocation of a cited source and snapshot capture both take the site
	// write lock. A blocker forces each ordering; only two outcomes are acceptable.
	for _, revokeFirst := range []bool{false, true} {
		name := "WIS-270 snapshot commits before racing revocation"
		if revokeFirst {
			name = "WIS-270 racing revocation refuses snapshot"
		}
		t.Run(name, func(t *testing.T) {
			race := Scope{TenantID: sc.TenantID, SiteID: uuid.NewString()}
			exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'Snapshot race fixture')`, race.TenantID, race.SiteID)
			exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, race.TenantID, race.SiteID, owner.Issuer, owner.Subject)
			rs := evidence.Scope{TenantID: race.TenantID, SiteID: race.SiteID}
			src, e := ev.CreateOrRevise(ctx, ea, rs, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Race source", Text: "Directly reviewed statement"}, uuid.NewString(), 0)
			if e != nil {
				t.Fatal(e)
			}
			span, e := ev.CreateSpan(ctx, ea, rs, evidence.SpanInput{SourceID: src.SourceID, Revision: 1, Start: 0, End: 9}, uuid.NewString(), 1)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = ev.DecideAuthority(ctx, ea, rs, src.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Fixture source verified"}, uuid.NewString(), 0); e != nil {
				t.Fatal(e)
			}
			entityID := uuid.NewString()
			if _, e = service.CreateEntity(ctx, owner, race, EntityInput{EntityID: entityID, Type: "operation", Name: "Race operation", SourceID: src.SourceID, SourceRevision: 1}, uuid.NewString(), 0); e != nil {
				t.Fatal(e)
			}
			c := ClaimInput{ClaimID: uuid.NewString(), SubjectID: entityID, SubjectRevision: 1, Predicate: "actual_step", Literal: &Literal{Type: "string", Value: json.RawMessage(`"Race instruction"`)}, EffectiveFrom: "2026-09-27T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
			if _, e = service.CreateClaim(ctx, owner, race, c, uuid.NewString(), 0); e != nil {
				t.Fatal(e)
			}
			if _, e = service.DecideReview(ctx, owner, race, c.ClaimID, ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Reviewed race claim"}, uuid.NewString(), 1); e != nil {
				t.Fatal(e)
			}
			inventory, e := service.ReadInventory(ctx, owner, race)
			if e != nil {
				t.Fatal(e)
			}
			// Bounded polling on lock-wait state; the service lock_timeout is 5s.
			waiters := func(n int) {
				t.Helper()
				deadline := time.Now().Add(3 * time.Second)
				for {
					var got int
					if e := admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND state='active' AND wait_event_type='Lock' AND pid<>pg_backend_pid()`).Scan(&got); e != nil {
						t.Fatal(e)
					}
					if got >= n {
						return
					}
					if time.Now().After(deadline) {
						t.Fatalf("lock waiters=%d want %d", got, n)
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			blocker, e := admin.BeginTx(ctx, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer blocker.Rollback()
			if revokeFirst {
				// Revoke takes the site lock, then waits for the source head row.
				_, e = blocker.ExecContext(ctx, `SELECT 1 FROM supervisor.evidence_heads WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 FOR UPDATE`, race.TenantID, race.SiteID, src.SourceID)
			} else {
				// Capture takes the site lock, writes its rows, then waits on its audit insert.
				_, e = blocker.ExecContext(ctx, `LOCK TABLE supervisor.knowledge_audit IN ACCESS EXCLUSIVE MODE`)
			}
			if e != nil {
				t.Fatal(e)
			}
			in := SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Capture racing revocation"}
			key := uuid.NewString()
			type captured struct {
				v Snapshot
				e error
			}
			snapshotDone, revokeDone := make(chan captured, 1), make(chan error, 1)
			capture := func() {
				v, e := service.CreateSnapshot(ctx, owner, race, in, key, inventory.Revision)
				snapshotDone <- captured{v, e}
			}
			revoke := func() {
				_, e := ev.Revoke(ctx, ea, rs, src.SourceID, "Cited source withdrawn", uuid.NewString(), 1)
				revokeDone <- e
			}
			first, second := capture, revoke
			if revokeFirst {
				first, second = revoke, capture
			}
			go first()
			waiters(1)
			go second()
			waiters(2)
			if e = blocker.Rollback(); e != nil {
				t.Fatal(e)
			}
			got, revokeErr := <-snapshotDone, <-revokeDone
			if revokeErr != nil {
				t.Fatalf("revocation=%v", revokeErr)
			}
			count := func(table, column, value string) int {
				t.Helper()
				var n int
				if e := admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.`+table+` WHERE tenant_id=$1 AND site_id=$2 AND `+column+`=$3`, race.TenantID, race.SiteID, value).Scan(&n); e != nil {
					t.Fatal(e)
				}
				return n
			}
			if revokeFirst {
				// Outcome 2: capture is refused and leaves no snapshot, outbox or receipt.
				if !errors.Is(got.e, ErrRevision) {
					t.Fatalf("capture after revocation=%+v %v", got.v, got.e)
				}
				for _, table := range []string{"knowledge_process_snapshots", "knowledge_snapshot_claims", "knowledge_snapshot_entities", "knowledge_outbox"} {
					if n := count(table, "snapshot_id", in.SnapshotID); n != 0 {
						t.Fatalf("refused capture left %s rows=%d", table, n)
					}
				}
				if n := count("knowledge_receipts", "idempotency_key", key); n != 0 {
					t.Fatalf("refused capture left receipts=%d", n)
				}
				if _, e = service.ReadSnapshot(ctx, owner, race, in.SnapshotID); !errors.Is(e, ErrNotFound) {
					t.Fatalf("refused capture readable=%v", e)
				}
				return
			}
			// Outcome 1: capture committed with the cited claim, then revocation fails reads closed.
			if got.e != nil || len(got.v.Contents.Claims) != 1 || got.v.Contents.Claims[0].ClaimID != c.ClaimID {
				t.Fatalf("capture before revocation=%+v %v", got.v, got.e)
			}
			if n := count("knowledge_process_snapshots", "snapshot_id", in.SnapshotID); n != 1 {
				t.Fatalf("committed capture rows=%d", n)
			}
			if n := count("knowledge_outbox", "snapshot_id", in.SnapshotID); n != 1 {
				t.Fatalf("committed capture outbox=%d", n)
			}
			if _, e = service.ReadSnapshot(ctx, owner, race, in.SnapshotID); !errors.Is(e, ErrNotFound) {
				t.Fatalf("revoked capture read=%v", e)
			}
			if _, e = service.CreateSnapshot(ctx, owner, race, in, key, inventory.Revision); !errors.Is(e, ErrNotFound) {
				t.Fatalf("revoked capture replay=%v", e)
			}
		})
	}
}
