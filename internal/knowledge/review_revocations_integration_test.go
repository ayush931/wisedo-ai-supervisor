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
	"github.com/jackc/pgx/v5/pgconn"
)

func TestPostgresReviewRevocations(t *testing.T) {
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
	database := "review_revoke_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
		exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'revocation synthetic')`, s.TenantID)
		exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'revocation synthetic')`, s.TenantID, s.SiteID)
	}
	actor := func(role string) Actor {
		t.Helper()
		a := Actor{"https://review-revoke.invalid", uuid.NewString(), strings.ReplaceAll(uuid.NewString(), "-", "") + strings.ReplaceAll(uuid.NewString(), "-", "")}
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'fixture',now(),now(),now()+interval '1 hour',now())`, a.SessionHash, a.Issuer, a.Subject)
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, role)
		return a
	}
	owner, supervisor, operator, viewer := actor("owner"), actor("supervisor"), actor("operator"), actor("viewer")
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, foreign.TenantID, foreign.SiteID, owner.Issuer, owner.Subject)
	service, ev := New(runtime), evidence.New(runtime)
	ea := evidence.Actor{Issuer: owner.Issuer, Subject: owner.Subject, SessionHash: owner.SessionHash}
	es := evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}
	approve := ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Reviewed exact source"}
	type fixture struct {
		claim      ClaimInput
		source     evidence.Input
		review     Review
		approveKey string
	}
	makeFixture := func(verdict string) fixture {
		t.Helper()
		f := fixture{source: evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Revocation source", Text: "Synthetic original source"}, approveKey: uuid.NewString()}
		if _, e := ev.CreateOrRevise(ctx, ea, es, f.source, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		entity := uuid.NewString()
		if _, e := service.CreateEntity(ctx, owner, sc, EntityInput{EntityID: entity, Type: "operation", Name: "Review operation", SourceID: f.source.SourceID, SourceRevision: 1}, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		span, e := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: f.source.SourceID, Revision: 1, Start: 0, End: 9}, uuid.NewString(), 1)
		if e != nil {
			t.Fatal(e)
		}
		f.claim = ClaimInput{ClaimID: uuid.NewString(), SubjectID: entity, SubjectRevision: 1, Predicate: "actual_step", Literal: &Literal{Type: "string", Value: json.RawMessage(`"Original instruction"`)}, EffectiveFrom: "2026-09-27T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
		if _, e = service.CreateClaim(ctx, owner, sc, f.claim, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		if _, e = ev.DecideAuthority(ctx, ea, es, f.source.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Owner verified source"}, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		decision := approve
		decision.Verdict = verdict
		f.review, e = service.DecideReview(ctx, owner, sc, f.claim.ClaimID, decision, f.approveKey, 1)
		if e != nil {
			t.Fatal(e)
		}
		return f
	}
	input := func(f fixture) RevocationInput {
		return RevocationInput{ClaimRevision: 1, ReviewID: f.review.ReviewID, Reason: "Withdraw unsafe instruction"}
	}
	check := func(got, want error) {
		t.Helper()
		if !errors.Is(got, want) {
			t.Fatalf("error=%v want=%v", got, want)
		}
	}
	t.Run("exact withdrawal replay and original approval stay withdrawn", func(t *testing.T) {
		f := makeFixture("approve")
		var storedBefore string
		if e := admin.QueryRowContext(ctx, `SELECT row_to_json(r)::text FROM supervisor.knowledge_claim_reviews r WHERE review_id=$1`, f.review.ReviewID).Scan(&storedBefore); e != nil {
			t.Fatal(e)
		}
		in := input(f)
		for _, a := range []Actor{operator, viewer} {
			_, e := service.RevokeReview(ctx, a, sc, f.claim.ClaimID, in, uuid.NewString(), 1)
			check(e, ErrDenied)
		}
		wrong := in
		wrong.ReviewID = uuid.NewString()
		_, e := service.RevokeReview(ctx, owner, sc, f.claim.ClaimID, wrong, uuid.NewString(), 1)
		check(e, ErrReviewConflict)
		key := uuid.NewString()
		out, e := service.RevokeReview(ctx, supervisor, sc, f.claim.ClaimID, in, key, 1)
		if e != nil || out.ApprovalEligible || out.Revocation == nil || out.ReviewID != f.review.ReviewID {
			t.Fatalf("withdraw=%+v %v", out, e)
		}
		replay, e := service.RevokeReview(ctx, supervisor, sc, f.claim.ClaimID, in, key, 1)
		if e != nil || replay.Revocation == nil || replay.ApprovalEligible {
			t.Fatalf("withdraw replay=%+v %v", replay, e)
		}
		if *replay.Revocation != *out.Revocation || replay.Reason != f.review.Reason || replay.ActorSubject != f.review.ActorSubject || out.Revocation.ActorSubject != supervisor.Subject {
			t.Fatal("revocation replay or original review provenance changed")
		}
		_, e = service.RevokeReview(ctx, owner, sc, f.claim.ClaimID, in, uuid.NewString(), 1)
		check(e, ErrReviewConflict)
		changed := in
		changed.Reason = "Changed reason"
		_, e = service.RevokeReview(ctx, supervisor, sc, f.claim.ClaimID, changed, key, 1)
		check(e, ErrConflict)
		changed = in
		changed.ReviewID = uuid.NewString()
		_, e = service.RevokeReview(ctx, supervisor, sc, f.claim.ClaimID, changed, key, 1)
		check(e, ErrConflict)
		original, e := service.DecideReview(ctx, owner, sc, f.claim.ClaimID, approve, f.approveKey, 1)
		if e != nil || original.ApprovalEligible || original.Revocation == nil || original.Verdict != "approve" {
			t.Fatalf("approval replay resurrected=%+v %v", original, e)
		}
		read, e := service.ReadReview(ctx, viewer, sc, f.claim.ClaimID, 1)
		if e != nil || read.Revocation == nil || read.ApprovalEligible {
			t.Fatalf("withdrawn read=%+v %v", read, e)
		}
		var storedAfter string
		if e = admin.QueryRowContext(ctx, `SELECT row_to_json(r)::text FROM supervisor.knowledge_claim_reviews r WHERE review_id=$1`, f.review.ReviewID).Scan(&storedAfter); e != nil || storedAfter != storedBefore {
			t.Fatalf("original review row mutated: %v", e)
		}
		_, e = service.RevokeReview(ctx, owner, foreign, f.claim.ClaimID, in, uuid.NewString(), 1)
		check(e, ErrNotFound)
		exec(`UPDATE supervisor.browser_sessions SET auth_time=NULL WHERE token_hash=$1`, supervisor.SessionHash)
		_, e = service.RevokeReview(ctx, supervisor, sc, f.claim.ClaimID, in, key, 1)
		check(e, ErrReauthenticationRequired)
		exec(`UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash=$1`, supervisor.SessionHash)
		exec(`UPDATE supervisor.memberships SET role='operator' WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4`, sc.TenantID, sc.SiteID, supervisor.Issuer, supervisor.Subject)
		_, e = service.RevokeReview(ctx, supervisor, sc, f.claim.ClaimID, in, key, 1)
		check(e, ErrDenied)
		exec(`UPDATE supervisor.memberships SET role='supervisor' WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4`, sc.TenantID, sc.SiteID, supervisor.Issuer, supervisor.Subject)
	})
	t.Run("rejection and mismatched claim precondition cannot be withdrawn", func(t *testing.T) {
		f := makeFixture("reject")
		_, e := service.RevokeReview(ctx, owner, sc, f.claim.ClaimID, input(f), uuid.NewString(), 1)
		check(e, ErrInvalidContract)
		f = makeFixture("approve")
		_, e = service.RevokeReview(ctx, owner, sc, f.claim.ClaimID, input(f), uuid.NewString(), 2)
		check(e, ErrRevision)
	})
	for _, change := range []string{"claim", "entity", "source", "authority"} {
		t.Run("historical "+change+" can still be withdrawn", func(t *testing.T) {
			f := makeFixture("approve")
			var e error
			switch change {
			case "claim":
				f.claim.Literal.Value = json.RawMessage(`"New instruction"`)
				_, e = service.CreateClaim(ctx, owner, sc, f.claim, uuid.NewString(), 1)
			case "entity":
				_, e = service.CreateEntity(ctx, owner, sc, EntityInput{EntityID: f.claim.SubjectID, Type: "operation", Name: "New operation", SourceID: f.source.SourceID, SourceRevision: 1}, uuid.NewString(), 1)
			case "source":
				f.source.Text = "New source content"
				_, e = ev.CreateOrRevise(ctx, ea, es, f.source, uuid.NewString(), 1)
			case "authority":
				_, e = ev.DecideAuthority(ctx, ea, es, f.source.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "withdraw", Reason: "No longer authoritative"}, uuid.NewString(), 1)
			}
			if e != nil {
				t.Fatal(e)
			}
			out, e := service.RevokeReview(ctx, owner, sc, f.claim.ClaimID, input(f), uuid.NewString(), 1)
			if e != nil || out.Revocation == nil || out.ApprovalEligible {
				t.Fatalf("historical withdrawal=%+v %v", out, e)
			}
		})
	}
	t.Run("revoked source conceals reads and both receipts", func(t *testing.T) {
		f := makeFixture("approve")
		key := uuid.NewString()
		in := input(f)
		if _, e := service.RevokeReview(ctx, owner, sc, f.claim.ClaimID, in, key, 1); e != nil {
			t.Fatal(e)
		}
		if _, e := ev.Revoke(ctx, ea, es, f.source.SourceID, "Remove evidence", uuid.NewString(), 1); e != nil {
			t.Fatal(e)
		}
		_, e := service.ReadReview(ctx, viewer, sc, f.claim.ClaimID, 1)
		check(e, ErrNotFound)
		_, e = service.RevokeReview(ctx, owner, sc, f.claim.ClaimID, in, key, 1)
		check(e, ErrNotFound)
		_, e = service.DecideReview(ctx, owner, sc, f.claim.ClaimID, approve, f.approveKey, 1)
		check(e, ErrNotFound)
	})
	t.Run("competing withdrawals commit once", func(t *testing.T) {
		f := makeFixture("approve")
		results := make(chan error, 2)
		for _, a := range []Actor{owner, supervisor} {
			go func(a Actor) {
				_, e := service.RevokeReview(ctx, a, sc, f.claim.ClaimID, input(f), uuid.NewString(), 1)
				results <- e
			}(a)
		}
		wins := 0
		for range 2 {
			if e := <-results; e == nil {
				wins++
			} else {
				check(e, ErrReviewConflict)
			}
		}
		if wins != 1 {
			t.Fatalf("withdrawal winners=%d", wins)
		}
	})
	t.Run("audit rollback preserves approval and retry key", func(t *testing.T) {
		f := makeFixture("approve")
		key := uuid.NewString()
		in := input(f)
		blocker, e := admin.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer blocker.Rollback()
		if _, e = blocker.ExecContext(ctx, `LOCK TABLE supervisor.knowledge_audit IN ACCESS EXCLUSIVE MODE`); e != nil {
			t.Fatal(e)
		}
		short, stop := context.WithTimeout(ctx, 150*time.Millisecond)
		_, e = service.RevokeReview(short, owner, sc, f.claim.ClaimID, in, key, 1)
		stop()
		blocker.Rollback()
		if e == nil {
			t.Fatal("blocked audit committed withdrawal")
		}
		before, e := service.ReadReview(ctx, viewer, sc, f.claim.ClaimID, 1)
		if e != nil || before.Revocation != nil || !before.ApprovalEligible {
			t.Fatalf("rollback changed review=%+v %v", before, e)
		}
		after, e := service.RevokeReview(ctx, owner, sc, f.claim.ClaimID, in, key, 1)
		if e != nil || after.Revocation == nil || after.ApprovalEligible {
			t.Fatalf("same-key retry=%+v %v", after, e)
		}
	})
	t.Run("withdrawal rows have scoped references isolation and immutability", func(t *testing.T) {
		f := makeFixture("approve")
		_, e := admin.ExecContext(ctx, `INSERT INTO supervisor.knowledge_review_revocations(tenant_id,site_id,revocation_id,review_id,claim_id,claim_revision,reason,actor_issuer,actor_subject,auth_time) VALUES($1,$2,$3,$4,$5,1,'Synthetic FK check',$6,$7,now())`, foreign.TenantID, foreign.SiteID, uuid.NewString(), f.review.ReviewID, f.claim.ClaimID, owner.Issuer, owner.Subject)
		var pg *pgconn.PgError
		if !errors.As(e, &pg) || pg.Code != "23503" {
			t.Fatalf("cross-scope review FK not enforced: %v", e)
		}
		if _, e = service.RevokeReview(ctx, owner, sc, f.claim.ClaimID, input(f), uuid.NewString(), 1); e != nil {
			t.Fatal(e)
		}
		tx, e := runtime.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, foreign.TenantID); e != nil {
			t.Fatal(e)
		}
		var count int
		if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_review_revocations`).Scan(&count); e != nil || count != 0 {
			t.Fatalf("foreign withdrawal rows=%d %v", count, e)
		}
		tx.Rollback()
		tx, e = runtime.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); e != nil {
			t.Fatal(e)
		}
		_, e = tx.ExecContext(ctx, `UPDATE supervisor.knowledge_review_revocations SET reason='rewritten' WHERE review_id=$1`, f.review.ReviewID)
		requireSQLState(t, e, "42501")
	})
}
