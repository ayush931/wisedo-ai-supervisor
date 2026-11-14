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

func TestPostgresConflictResolutions(t *testing.T) {
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
	database := "resolution_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
		exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'resolution synthetic')`, s.TenantID)
		exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'resolution synthetic')`, s.TenantID, s.SiteID)
	}
	actor := func(role string) Actor {
		t.Helper()
		a := Actor{"https://resolution.invalid", uuid.NewString(), strings.ReplaceAll(uuid.NewString(), "-", "") + strings.ReplaceAll(uuid.NewString(), "-", "")}
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
	designate := func(in evidence.Input, expected int64) {
		t.Helper()
		if _, e := ev.DecideAuthority(ctx, ea, es, in.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Owner validated source"}, uuid.NewString(), expected); e != nil {
			t.Fatal(e)
		}
	}
	approve := ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Review explicitly resolved exact claim"}
	type pair struct {
		a, b     ClaimInput
		as, bs   evidence.Input
		conflict Conflict
		prior    *Review
		priorKey string
	}
	makePair := func(approveFirst bool) pair {
		t.Helper()
		p := pair{as: source(), bs: source(), priorKey: uuid.NewString()}
		entity := uuid.NewString()
		if _, e := service.CreateEntity(ctx, owner, sc, EntityInput{EntityID: entity, Type: "operation", Name: "Resolution operation", SourceID: origin.SourceID, SourceRevision: 1}, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		for i, src := range []evidence.Input{p.as, p.bs} {
			span, e := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: src.SourceID, Revision: 1, Start: 0, End: 9}, uuid.NewString(), 1)
			if e != nil {
				t.Fatal(e)
			}
			value := `"First instruction"`
			if i == 1 {
				value = `"Second instruction"`
			}
			in := ClaimInput{ClaimID: uuid.NewString(), SubjectID: entity, SubjectRevision: 1, Predicate: "actual_step", Literal: &Literal{Type: "string", Value: json.RawMessage(value)}, EffectiveFrom: "2026-09-27T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
			if _, e = service.CreateClaim(ctx, owner, sc, in, uuid.NewString(), 0); e != nil {
				t.Fatal(e)
			}
			if i == 0 {
				p.a = in
				if approveFirst {
					designate(p.as, 0)
					r, e := service.DecideReview(ctx, owner, sc, in.ClaimID, approve, p.priorKey, 1)
					if e != nil {
						t.Fatal(e)
					}
					p.prior = &r
				}
			} else {
				p.b = in
			}
		}
		page, e := service.ListConflicts(ctx, viewer, sc, p.a.ClaimID, 1, 100, "")
		if e != nil || len(page.Conflicts) != 1 {
			t.Fatalf("conflict fixture=%+v %v", page, e)
		}
		p.conflict = page.Conflicts[0]
		return p
	}
	prefer := func(p pair, id string) ResolutionInput {
		decision := "prefer_left"
		if p.conflict.Right.ClaimID == id {
			decision = "prefer_right"
		}
		return ResolutionInput{Decision: decision, Reason: "Explicit owner resolution"}
	}
	check := func(got, want error) {
		t.Helper()
		if !errors.Is(got, want) {
			t.Fatalf("error=%v want=%v", got, want)
		}
	}
	// WIS-270 state reducer: "any candidate/approved may become contested; contested ->
	// approved/rejected only by explicit conflict resolution". The listed state and the
	// ?state= filter derive from the same ledgers.
	stateOf := func(claim ClaimInput) string {
		t.Helper()
		page, e := service.ListClaimsByState(ctx, viewer, sc, 100, "", claim.SubjectID, "")
		if e != nil {
			t.Fatal(e)
		}
		for _, c := range page.Claims {
			if c.ClaimID == claim.ClaimID {
				return c.ReviewState
			}
		}
		t.Fatalf("claim %s not listed", claim.ClaimID)
		return ""
	}
	listed := func(claim ClaimInput, state string) bool {
		t.Helper()
		page, e := service.ListClaimsByState(ctx, viewer, sc, 100, "", claim.SubjectID, state)
		if e != nil {
			t.Fatal(e)
		}
		for _, c := range page.Claims {
			if c.ClaimID == claim.ClaimID {
				return true
			}
		}
		return false
	}
	t.Run("WIS-270 contested state follows the reducer", func(t *testing.T) {
		// An approved claim becomes contested when a conflicting claim is recorded.
		approved := makePair(true)
		if stateOf(approved.a) != StateContested || stateOf(approved.b) != StateContested {
			t.Fatalf("conflict sides: %s %s", stateOf(approved.a), stateOf(approved.b))
		}
		if listed(approved.a, StateApproved) || !listed(approved.a, StateContested) {
			t.Fatal("an approval outranked an open contradiction in the state filter")
		}
		// An explicit resolution ends the contest; it does not approve anything by itself.
		p := makePair(false)
		designate(p.as, 0)
		if _, e := service.DecideResolution(ctx, supervisor, sc, p.conflict.ID, prefer(p, p.a.ClaimID), uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		if stateOf(p.a) != StateCandidate || stateOf(p.b) != StateCandidate {
			t.Fatalf("resolved sides: %s %s", stateOf(p.a), stateOf(p.b))
		}
		// Withdrawing the resolution reopens the contest.
		if _, e := service.DecideResolution(ctx, owner, sc, p.conflict.ID, ResolutionInput{Decision: "withdraw", Reason: "Reconsider resolution"}, uuid.NewString(), 1); e != nil {
			t.Fatal(e)
		}
		if stateOf(p.a) != StateContested || stateOf(p.b) != StateContested {
			t.Fatalf("withdrawn resolution: %s %s", stateOf(p.a), stateOf(p.b))
		}
		if _, e := service.ListClaimsByState(ctx, viewer, sc, 100, "", "", "disputed"); !errors.Is(e, ErrInvalidContract) {
			t.Fatalf("unknown state filter: %v", e)
		}
	})
	t.Run("authorized resolution pins fresh review without automatic approval", func(t *testing.T) {
		p := makePair(false)
		in := prefer(p, p.a.ClaimID)
		for _, a := range []Actor{operator, viewer} {
			_, e := service.DecideResolution(ctx, a, sc, p.conflict.ID, in, uuid.NewString(), 0)
			check(e, ErrDenied)
		}
		_, e := service.DecideResolution(ctx, owner, sc, p.conflict.ID, in, uuid.NewString(), 0)
		check(e, ErrResolutionBlocked)
		designate(p.as, 0)
		key := uuid.NewString()
		out, e := service.DecideResolution(ctx, supervisor, sc, p.conflict.ID, in, key, 0)
		if e != nil || !out.ResolutionEligible || out.Revision != 1 {
			t.Fatalf("resolution=%+v %v", out, e)
		}
		_, e = service.ReadReview(ctx, viewer, sc, p.a.ClaimID, 1)
		check(e, ErrNotFound)
		read, e := service.ReadResolution(ctx, viewer, sc, p.conflict.ID, 0)
		if e != nil || read.ResolutionID != out.ResolutionID {
			t.Fatalf("resolution read=%+v %v", read, e)
		}
		replay, e := service.DecideResolution(ctx, supervisor, sc, p.conflict.ID, in, key, 0)
		if e != nil || replay.ResolutionID != out.ResolutionID {
			t.Fatalf("resolution replay=%+v %v", replay, e)
		}
		changed := in
		changed.Reason = "Changed reason"
		_, e = service.DecideResolution(ctx, supervisor, sc, p.conflict.ID, changed, key, 0)
		check(e, ErrConflict)
		_, e = service.DecideResolution(ctx, owner, sc, p.conflict.ID, in, uuid.NewString(), 0)
		check(e, ErrRevision)
		_, e = service.ReadResolution(ctx, owner, foreign, p.conflict.ID, 0)
		check(e, ErrNotFound)
		exec(`UPDATE supervisor.browser_sessions SET auth_time=NULL WHERE token_hash=$1`, supervisor.SessionHash)
		_, e = service.DecideResolution(ctx, supervisor, sc, p.conflict.ID, in, key, 0)
		check(e, ErrReauthenticationRequired)
		exec(`UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash=$1`, supervisor.SessionHash)
		exec(`UPDATE supervisor.memberships SET role='operator' WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4`, sc.TenantID, sc.SiteID, supervisor.Issuer, supervisor.Subject)
		_, e = service.DecideResolution(ctx, supervisor, sc, p.conflict.ID, in, key, 0)
		check(e, ErrDenied)
		exec(`UPDATE supervisor.memberships SET role='supervisor' WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4`, sc.TenantID, sc.SiteID, supervisor.Issuer, supervisor.Subject)
		reviewed, e := service.DecideReview(ctx, owner, sc, p.a.ClaimID, approve, uuid.NewString(), 1)
		if e != nil || !reviewed.ApprovalEligible || len(reviewed.ResolutionRefs) != 1 {
			t.Fatalf("resolved review=%+v %v", reviewed, e)
		}
		ref := reviewed.ResolutionRefs[0]
		if ref.ConflictID != p.conflict.ID || ref.ResolutionID != out.ResolutionID || ref.Revision != 1 {
			t.Fatalf("wrong review resolution pin=%+v", ref)
		}
	})
	t.Run("right or left winner requires loser approval withdrawal", func(t *testing.T) {
		p := makePair(true)
		designate(p.bs, 0)
		in := prefer(p, p.b.ClaimID)
		old, e := service.ReadReview(ctx, viewer, sc, p.a.ClaimID, 1)
		if e != nil || old.ApprovalEligible {
			t.Fatalf("old approval already conflict blocked=%+v %v", old, e)
		}
		_, e = service.DecideResolution(ctx, owner, sc, p.conflict.ID, in, uuid.NewString(), 0)
		check(e, ErrResolutionBlocked)
		_, e = service.RevokeReview(ctx, owner, sc, p.a.ClaimID, RevocationInput{ClaimRevision: 1, ReviewID: p.prior.ReviewID, Reason: "Withdraw losing approval"}, uuid.NewString(), 1)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = service.DecideResolution(ctx, owner, sc, p.conflict.ID, in, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		winner, e := service.DecideReview(ctx, supervisor, sc, p.b.ClaimID, approve, uuid.NewString(), 1)
		if e != nil || !winner.ApprovalEligible {
			t.Fatalf("winner review=%+v %v", winner, e)
		}
	})
	t.Run("competing initial resolutions have one CAS winner", func(t *testing.T) {
		p := makePair(false)
		designate(p.as, 0)
		in := prefer(p, p.a.ClaimID)
		results := make(chan error, 2)
		for _, a := range []Actor{owner, supervisor} {
			go func(a Actor) {
				_, e := service.DecideResolution(ctx, a, sc, p.conflict.ID, in, uuid.NewString(), 0)
				results <- e
			}(a)
		}
		wins := 0
		for range 2 {
			if e := <-results; e == nil {
				wins++
			} else {
				check(e, ErrRevision)
			}
		}
		if wins != 1 {
			t.Fatalf("resolution CAS winners=%d", wins)
		}
	})
	t.Run("neither requires withdrawals and never authorizes either side", func(t *testing.T) {
		p := makePair(true)
		in := ResolutionInput{Decision: "neither", Reason: "Neither report accepted"}
		_, e := service.DecideResolution(ctx, owner, sc, p.conflict.ID, in, uuid.NewString(), 0)
		check(e, ErrResolutionBlocked)
		if _, e = service.RevokeReview(ctx, owner, sc, p.a.ClaimID, RevocationInput{ClaimRevision: 1, ReviewID: p.prior.ReviewID, Reason: "Withdraw prior approval"}, uuid.NewString(), 1); e != nil {
			t.Fatal(e)
		}
		if _, e = service.DecideResolution(ctx, owner, sc, p.conflict.ID, in, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		_, e = service.DecideReview(ctx, owner, sc, p.b.ClaimID, approve, uuid.NewString(), 1)
		check(e, ErrReviewBlocked)
		plain := makePair(false)
		if _, e = service.DecideResolution(ctx, owner, sc, plain.conflict.ID, in, uuid.NewString(), 0); e != nil {
			t.Fatalf("neither required positive authority: %v", e)
		}
	})
	for _, change := range []string{"withdrawal", "replacement", "authority redesignation"} {
		t.Run(change+" invalidates pinned review forever", func(t *testing.T) {
			p := makePair(false)
			designate(p.as, 0)
			in := prefer(p, p.a.ClaimID)
			first, e := service.DecideResolution(ctx, owner, sc, p.conflict.ID, in, uuid.NewString(), 0)
			if e != nil {
				t.Fatal(e)
			}
			key := uuid.NewString()
			approved, e := service.DecideReview(ctx, owner, sc, p.a.ClaimID, approve, key, 1)
			if e != nil || !approved.ApprovalEligible {
				t.Fatalf("initial pinned approval=%+v %v", approved, e)
			}
			head := int64(1)
			switch change {
			case "withdrawal":
				_, e = service.DecideResolution(ctx, owner, sc, p.conflict.ID, ResolutionInput{Decision: "withdraw", Reason: "Reconsider resolution"}, uuid.NewString(), head)
				head++
			case "replacement":
				_, e = service.DecideResolution(ctx, owner, sc, p.conflict.ID, in, uuid.NewString(), head)
				head++
			case "authority redesignation":
				designate(p.as, 1)
			}
			if e != nil {
				t.Fatal(e)
			}
			old, e := service.ReadResolution(ctx, viewer, sc, p.conflict.ID, first.Revision)
			if e != nil || old.ResolutionEligible {
				t.Fatalf("old decision still eligible=%+v %v", old, e)
			}
			read, e := service.ReadReview(ctx, viewer, sc, p.a.ClaimID, 1)
			if e != nil || read.ApprovalEligible {
				t.Fatalf("old review resurrected=%+v %v", read, e)
			}
			if _, e = service.DecideResolution(ctx, owner, sc, p.conflict.ID, in, uuid.NewString(), head); e != nil {
				t.Fatal(e)
			}
			replay, e := service.DecideReview(ctx, owner, sc, p.a.ClaimID, approve, key, 1)
			if e != nil || replay.ApprovalEligible {
				t.Fatalf("replacement head resurrected receipt=%+v %v", replay, e)
			}
		})
	}
	t.Run("old approval never gains newly resolved conflict pin", func(t *testing.T) {
		p := makePair(true)
		if _, e := service.DecideResolution(ctx, owner, sc, p.conflict.ID, prefer(p, p.a.ClaimID), uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		out, e := service.DecideReview(ctx, owner, sc, p.a.ClaimID, approve, p.priorKey, 1)
		if e != nil || out.ApprovalEligible || len(out.ResolutionRefs) != 0 {
			t.Fatalf("old review acquired new authority=%+v %v", out, e)
		}
		p.b.Literal = p.a.Literal
		if _, e = service.CreateClaim(ctx, owner, sc, p.b, uuid.NewString(), 1); e != nil {
			t.Fatal(e)
		}
		out, e = service.ReadReview(ctx, viewer, sc, p.a.ClaimID, 1)
		if e != nil || out.ApprovalEligible {
			t.Fatalf("historical peer change revived unpinned approval=%+v %v", out, e)
		}
	})
	t.Run("historical withdrawal allowed but revoked pair remains hidden", func(t *testing.T) {
		p := makePair(false)
		designate(p.as, 0)
		withdraw := ResolutionInput{Decision: "withdraw", Reason: "Withdraw stale resolution"}
		_, e := service.DecideResolution(ctx, owner, sc, p.conflict.ID, withdraw, uuid.NewString(), 0)
		check(e, ErrInvalidContract)
		if _, e = service.DecideResolution(ctx, owner, sc, p.conflict.ID, prefer(p, p.a.ClaimID), uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		p.bs.Text = "Updated losing evidence"
		if _, e = ev.CreateOrRevise(ctx, ea, es, p.bs, uuid.NewString(), 1); e != nil {
			t.Fatal(e)
		}
		_, e = service.DecideResolution(ctx, owner, sc, p.conflict.ID, prefer(p, p.a.ClaimID), uuid.NewString(), 1)
		check(e, ErrSourceChanged)
		out, e := service.DecideResolution(ctx, owner, sc, p.conflict.ID, withdraw, uuid.NewString(), 1)
		if e != nil || out.Decision != "withdraw" || out.ResolutionEligible {
			t.Fatalf("stale withdrawal=%+v %v", out, e)
		}
		if _, e = ev.Revoke(ctx, ea, es, p.bs.SourceID, "Remove peer evidence", uuid.NewString(), 2); e != nil {
			t.Fatal(e)
		}
		_, e = service.ReadResolution(ctx, viewer, sc, p.conflict.ID, 1)
		check(e, ErrNotFound)
	})
	t.Run("approval dependency budget precedes unresolved pair processing", func(t *testing.T) {
		p := makePair(false)
		designate(p.as, 0)
		seed, e := admin.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer seed.Rollback()
		if _, e = seed.ExecContext(ctx, "SELECT set_config('supervisor.tenant_id',$1,true)", sc.TenantID); e != nil {
			t.Fatal(e)
		}
		for _, q := range []string{
			`CREATE TEMP TABLE resolution_peer_ids ON COMMIT DROP AS SELECT gen_random_uuid() AS id FROM generate_series(1,1000)`,
			`INSERT INTO supervisor.knowledge_claim_heads(tenant_id,site_id,id,current_revision) SELECT $1,$2,id,1 FROM resolution_peer_ids`,
			`INSERT INTO supervisor.knowledge_claim_revisions(tenant_id,site_id,claim_id,revision,subject_id,predicate,object_entity_id,payload,actor_issuer,actor_subject) SELECT r.tenant_id,r.site_id,i.id,1,r.subject_id,r.predicate,r.object_entity_id,jsonb_set(r.payload,'{claim_id}',to_jsonb(i.id::text)),r.actor_issuer,r.actor_subject FROM supervisor.knowledge_claim_revisions r CROSS JOIN resolution_peer_ids i WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.claim_id=$3 AND r.revision=1`,
			`INSERT INTO supervisor.knowledge_claim_entity_refs(tenant_id,site_id,claim_id,claim_revision,subject_id,subject_revision,object_entity_id,object_revision) SELECT r.tenant_id,r.site_id,i.id,1,r.subject_id,r.subject_revision,r.object_entity_id,r.object_revision FROM supervisor.knowledge_claim_entity_refs r CROSS JOIN resolution_peer_ids i WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.claim_id=$3 AND r.claim_revision=1`,
			`INSERT INTO supervisor.knowledge_claim_spans(tenant_id,site_id,claim_id,claim_revision,span_id,source_id,source_revision) SELECT r.tenant_id,r.site_id,i.id,1,r.span_id,r.source_id,r.source_revision FROM supervisor.knowledge_claim_spans r CROSS JOIN resolution_peer_ids i WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.claim_id=$3 AND r.claim_revision=1`,
		} {
			var args []any
			if strings.Contains(q, "$1") {
				args = []any{sc.TenantID, sc.SiteID}
			}
			if strings.Contains(q, "$3") {
				args = append(args, p.b.ClaimID)
			}
			if _, e = seed.ExecContext(ctx, q, args...); e != nil {
				t.Fatal(e)
			}
		}
		if _, e = seed.ExecContext(ctx, `INSERT INTO supervisor.knowledge_conflicts(tenant_id,site_id,id,left_claim_id,left_revision,right_claim_id,right_revision,kind,effective_from) SELECT $1,$2,gen_random_uuid(),least($3::uuid,id),1,greatest($3::uuid,id),1,'text_review','2026-09-27T00:00:00Z'::timestamptz FROM resolution_peer_ids`, sc.TenantID, sc.SiteID, p.a.ClaimID); e != nil {
			t.Fatal(e)
		}
		if e = seed.Commit(); e != nil {
			t.Fatal(e)
		}
		key := uuid.NewString()
		_, e = service.DecideReview(ctx, owner, sc, p.a.ClaimID, approve, key, 1)
		check(e, ErrTraversalLimit)
		var n int
		if e = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_claim_reviews WHERE tenant_id=$1 AND site_id=$2 AND claim_id=$3`, sc.TenantID, sc.SiteID, p.a.ClaimID).Scan(&n); e != nil || n != 0 {
			t.Fatalf("over-budget review persisted=%d %v", n, e)
		}
		if e = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_receipts WHERE tenant_id=$1 AND site_id=$2 AND idempotency_key=$3`, sc.TenantID, sc.SiteID, key).Scan(&n); e != nil || n != 0 {
			t.Fatalf("over-budget receipt persisted=%d %v", n, e)
		}
		if e = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_review_resolution_refs WHERE tenant_id=$1 AND site_id=$2 AND claim_id=$3`, sc.TenantID, sc.SiteID, p.a.ClaimID).Scan(&n); e != nil || n != 0 {
			t.Fatalf("over-budget refs persisted=%d %v", n, e)
		}
	})
	t.Run("audit failures roll back resolution and review pins", func(t *testing.T) {
		p := makePair(false)
		designate(p.as, 0)
		in := prefer(p, p.a.ClaimID)
		key := uuid.NewString()
		blockAudit := func() *sql.Tx {
			t.Helper()
			tx, e := admin.BeginTx(ctx, nil)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = tx.ExecContext(ctx, `LOCK TABLE supervisor.knowledge_audit IN ACCESS EXCLUSIVE MODE`); e != nil {
				tx.Rollback()
				t.Fatal(e)
			}
			return tx
		}
		blocker := blockAudit()
		defer blocker.Rollback()
		short, stop := context.WithTimeout(ctx, 150*time.Millisecond)
		_, e := service.DecideResolution(short, owner, sc, p.conflict.ID, in, key, 0)
		stop()
		blocker.Rollback()
		if e == nil {
			t.Fatal("blocked audit committed resolution")
		}
		_, e = service.ReadResolution(ctx, viewer, sc, p.conflict.ID, 0)
		check(e, ErrNotFound)
		resolved, e := service.DecideResolution(ctx, owner, sc, p.conflict.ID, in, key, 0)
		if e != nil {
			t.Fatalf("resolution retry=%v", e)
		}
		blocker = blockAudit()
		defer blocker.Rollback()
		reviewKey := uuid.NewString()
		short, stop = context.WithTimeout(ctx, 150*time.Millisecond)
		_, e = service.DecideReview(short, owner, sc, p.a.ClaimID, approve, reviewKey, 1)
		stop()
		blocker.Rollback()
		if e == nil {
			t.Fatal("blocked audit committed reviewed pins")
		}
		var n int
		if e = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_review_resolution_refs WHERE tenant_id=$1 AND site_id=$2 AND resolution_id=$3`, sc.TenantID, sc.SiteID, resolved.ResolutionID).Scan(&n); e != nil || n != 0 {
			t.Fatalf("orphan resolution pins=%d %v", n, e)
		}
		_, e = service.ReadReview(ctx, viewer, sc, p.a.ClaimID, 1)
		check(e, ErrNotFound)
		if out, e := service.DecideReview(ctx, owner, sc, p.a.ClaimID, approve, reviewKey, 1); e != nil || len(out.ResolutionRefs) != 1 {
			t.Fatalf("atomic pin retry=%+v %v", out, e)
		}
	})
	t.Run("resolution storage preserves scope and immutable decisions", func(t *testing.T) {
		p := makePair(false)
		designate(p.as, 0)
		resolved, e := service.DecideResolution(ctx, owner, sc, p.conflict.ID, prefer(p, p.a.ClaimID), uuid.NewString(), 0)
		if e != nil {
			t.Fatal(e)
		}
		reviewed, e := service.DecideReview(ctx, owner, sc, p.a.ClaimID, approve, uuid.NewString(), 1)
		if e != nil {
			t.Fatal(e)
		}
		_, e = admin.ExecContext(ctx, `INSERT INTO supervisor.knowledge_review_resolution_refs(tenant_id,site_id,review_id,claim_id,claim_revision,conflict_id,resolution_id,resolution_revision) VALUES($1,$2,$3,$4,1,$5,$6,1)`, foreign.TenantID, foreign.SiteID, reviewed.ReviewID, p.a.ClaimID, p.conflict.ID, resolved.ResolutionID)
		var pg *pgconn.PgError
		if !errors.As(e, &pg) || pg.Code != "23503" {
			t.Fatalf("cross-scope resolution FK not enforced: %v", e)
		}
		tx, e := runtime.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, foreign.TenantID); e != nil {
			t.Fatal(e)
		}
		for _, table := range []string{"knowledge_resolution_heads", "knowledge_resolution_decisions", "knowledge_review_resolution_refs"} {
			var n int
			if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.`+table).Scan(&n); e != nil || n != 0 {
				t.Fatalf("foreign %s rows=%d %v", table, n, e)
			}
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
		_, e = tx.ExecContext(ctx, `UPDATE supervisor.knowledge_resolution_decisions SET reason='rewritten' WHERE resolution_id=$1`, resolved.ResolutionID)
		requireSQLState(t, e, "42501")
	})
}
