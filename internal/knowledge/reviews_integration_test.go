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

func TestPostgresClaimReviews(t *testing.T) {
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
	database := "review_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
		exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'review synthetic')`, s.TenantID)
		exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'review synthetic')`, s.TenantID, s.SiteID)
	}
	actor := func(role string) Actor {
		t.Helper()
		a := Actor{"https://reviews.invalid", uuid.NewString(), strings.ReplaceAll(uuid.NewString(), "-", "") + strings.ReplaceAll(uuid.NewString(), "-", "")}
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'fixture',now(),now(),now()+interval '1 hour',now())`, a.SessionHash, a.Issuer, a.Subject)
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, role)
		return a
	}
	owner, supervisor, viewer, operator, reviewer := actor("owner"), actor("supervisor"), actor("viewer"), actor("operator"), actor("reviewer")
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, foreign.TenantID, foreign.SiteID, owner.Issuer, owner.Subject)
	service, ev := New(runtime), evidence.New(runtime)
	ea := evidence.Actor{Issuer: owner.Issuer, Subject: owner.Subject, SessionHash: owner.SessionHash}
	es := evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}
	source := func() evidence.Input {
		t.Helper()
		in := evidence.Input{SourceID: uuid.NewString(), Kind: "document", Title: "Review evidence", Text: "Synthetic authoritative process"}
		if _, e := ev.CreateOrRevise(ctx, ea, es, in, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		return in
	}
	origin := source()
	designate := func(in evidence.Input, sourceRevision, stateRevision, authorityRevision int64) evidence.Authority {
		t.Helper()
		out, e := ev.DecideAuthority(ctx, ea, es, in.SourceID, evidence.AuthorityInput{SourceRevision: sourceRevision, ExpectedSourceStateRevision: stateRevision, Verdict: "designate", Reason: "Owner reviewed source"}, uuid.NewString(), authorityRevision)
		if e != nil {
			t.Fatal(e)
		}
		return out
	}
	designate(origin, 1, 1, 0)
	fixture := func() (ClaimInput, evidence.Input) {
		t.Helper()
		citation := source()
		entity := uuid.NewString()
		if _, e := service.CreateEntity(ctx, owner, sc, EntityInput{EntityID: entity, Type: "operation", Name: "Reviewed operation", SourceID: origin.SourceID, SourceRevision: 1}, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		span, e := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: citation.SourceID, Revision: 1, Start: 0, End: 9}, uuid.NewString(), 1)
		if e != nil {
			t.Fatal(e)
		}
		in := ClaimInput{ClaimID: uuid.NewString(), SubjectID: entity, SubjectRevision: 1, Predicate: "actual_step", Literal: &Literal{Type: "string", Value: json.RawMessage(`"Operate safely"`)}, EffectiveFrom: "2026-09-27T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
		if _, e = service.CreateClaim(ctx, owner, sc, in, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		return in, citation
	}
	approve := ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Reviewed exact claim and evidence"}
	reject := ReviewInput{ClaimRevision: 1, Verdict: "reject", Reason: "Unsupported reported statement"}
	check := func(got, want error) {
		t.Helper()
		if !errors.Is(got, want) {
			t.Fatalf("error=%v want=%v", got, want)
		}
	}
	t.Run("authorization exact citation and immutable initial decision", func(t *testing.T) {
		in, citation := fixture()
		for _, a := range []Actor{viewer, operator, reviewer} {
			_, e := service.DecideReview(ctx, a, sc, in.ClaimID, approve, uuid.NewString(), 1)
			check(e, ErrDenied)
		}
		_, e := service.DecideReview(ctx, owner, sc, in.ClaimID, approve, uuid.NewString(), 1)
		check(e, ErrReviewBlocked)
		auth := designate(citation, 1, 1, 0)
		exec(`UPDATE supervisor.browser_sessions SET auth_time=now()-interval '1 hour' WHERE token_hash=$1`, supervisor.SessionHash)
		_, e = service.DecideReview(ctx, supervisor, sc, in.ClaimID, approve, uuid.NewString(), 1)
		check(e, ErrReauthenticationRequired)
		exec(`UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash=$1`, supervisor.SessionHash)
		key := uuid.NewString()
		out, e := service.DecideReview(ctx, supervisor, sc, in.ClaimID, approve, key, 1)
		if e != nil || !out.ApprovalEligible || out.AuthorityDecisionID != auth.DecisionID || out.ClaimRevision != 1 {
			t.Fatalf("approve=%+v %v", out, e)
		}
		replay, e := service.DecideReview(ctx, supervisor, sc, in.ClaimID, approve, key, 1)
		if e != nil || replay.ReviewID != out.ReviewID {
			t.Fatalf("replay=%+v %v", replay, e)
		}
		exec(`UPDATE supervisor.browser_sessions SET auth_time=NULL WHERE token_hash=$1`, supervisor.SessionHash)
		_, e = service.DecideReview(ctx, supervisor, sc, in.ClaimID, approve, key, 1)
		check(e, ErrReauthenticationRequired)
		exec(`UPDATE supervisor.browser_sessions SET auth_time=now() WHERE token_hash=$1`, supervisor.SessionHash)
		changed := approve
		changed.Reason = "Different reason"
		_, e = service.DecideReview(ctx, supervisor, sc, in.ClaimID, changed, key, 1)
		check(e, ErrConflict)
		_, e = service.DecideReview(ctx, owner, sc, in.ClaimID, reject, uuid.NewString(), 1)
		check(e, ErrReviewConflict)
		read, e := service.ReadReview(ctx, viewer, sc, in.ClaimID, 0)
		if e != nil || read.ReviewID != out.ReviewID {
			t.Fatalf("viewer read=%+v %v", read, e)
		}
		_, e = service.ReadReview(ctx, owner, foreign, in.ClaimID, 0)
		check(e, ErrNotFound)
		exec(`UPDATE supervisor.memberships SET role='operator' WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4`, sc.TenantID, sc.SiteID, supervisor.Issuer, supervisor.Subject)
		_, e = service.DecideReview(ctx, supervisor, sc, in.ClaimID, approve, key, 1)
		check(e, ErrDenied)
		exec(`UPDATE supervisor.memberships SET role='supervisor' WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4`, sc.TenantID, sc.SiteID, supervisor.Issuer, supervisor.Subject)
	})
	t.Run("reject without designated citation", func(t *testing.T) {
		in, _ := fixture()
		out, e := service.DecideReview(ctx, owner, sc, in.ClaimID, reject, uuid.NewString(), 1)
		if e != nil || out.Verdict != "reject" || out.ApprovalEligible {
			t.Fatalf("reject=%+v %v", out, e)
		}
	})
	t.Run("claim list filters by exact-revision review ledger state", func(t *testing.T) {
		rejected, _ := fixture()
		if _, e := service.DecideReview(ctx, owner, sc, rejected.ClaimID, reject, uuid.NewString(), 1); e != nil {
			t.Fatal(e)
		}
		pending, _ := fixture()
		has := func(page ClaimPage, id string) bool {
			for _, c := range page.Claims {
				if c.ClaimID == id {
					return true
				}
			}
			return false
		}
		for state, want := range map[string]struct{ rejected, pending bool }{"rejected": {true, false}, "candidate": {false, true}, "": {true, true}} {
			page, e := service.ListClaimsByState(ctx, viewer, sc, 100, "", "", state)
			if e != nil || has(page, rejected.ClaimID) != want.rejected || has(page, pending.ClaimID) != want.pending {
				t.Fatalf("state %q page=%+v %v", state, page, e)
			}
			for _, c := range page.Claims {
				if state != "" && c.ReviewState != state {
					t.Fatalf("state %q returned %+v", state, c)
				}
			}
		}
		if page, e := service.ListClaimsByState(ctx, viewer, foreign, 100, "", "", "rejected"); e == nil && len(page.Claims) != 0 {
			t.Fatalf("cross-tenant list leaked %+v", page)
		}
		// An unknown state is rejected; contested is a listable WIS-270 state.
		_, e := service.ListClaimsByState(ctx, viewer, sc, 100, "", "", "disputed")
		check(e, ErrInvalidContract)
		if _, e = service.ListClaimsByState(ctx, viewer, sc, 100, "", "", StateContested); e != nil {
			t.Fatalf("contested filter: %v", e)
		}
	})
	for _, change := range []string{"claim", "entity", "source"} {
		t.Run("stale "+change+" cannot be newly reviewed", func(t *testing.T) {
			in, citation := fixture()
			designate(citation, 1, 1, 0)
			var e error
			want := ErrSourceChanged
			switch change {
			case "claim":
				in.Literal.Value = json.RawMessage(`"Changed claim"`)
				_, e = service.CreateClaim(ctx, owner, sc, in, uuid.NewString(), 1)
				want = ErrRevision
			case "entity":
				_, e = service.CreateEntity(ctx, owner, sc, EntityInput{EntityID: in.SubjectID, Type: "operation", Name: "Changed entity", SourceID: origin.SourceID, SourceRevision: 1}, uuid.NewString(), 1)
			case "source":
				citation.Text = "Changed citation"
				_, e = ev.CreateOrRevise(ctx, ea, es, citation, uuid.NewString(), 1)
			}
			if e != nil {
				t.Fatal(e)
			}
			_, e = service.DecideReview(ctx, owner, sc, in.ClaimID, approve, uuid.NewString(), 1)
			check(e, want)
		})
	}
	t.Run("competing reviewers commit one decision", func(t *testing.T) {
		in, citation := fixture()
		designate(citation, 1, 1, 0)
		results := make(chan error, 2)
		for _, a := range []Actor{owner, supervisor} {
			go func(a Actor) {
				_, e := service.DecideReview(ctx, a, sc, in.ClaimID, approve, uuid.NewString(), 1)
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
			t.Fatalf("review winners=%d", wins)
		}
	})
	t.Run("authority withdrawal and redesignation cannot resurrect approval", func(t *testing.T) {
		in, citation := fixture()
		designate(citation, 1, 1, 0)
		key := uuid.NewString()
		if _, e := service.DecideReview(ctx, owner, sc, in.ClaimID, approve, key, 1); e != nil {
			t.Fatal(e)
		}
		if _, e := ev.DecideAuthority(ctx, ea, es, citation.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "withdraw", Reason: "Authority withdrawn"}, uuid.NewString(), 1); e != nil {
			t.Fatal(e)
		}
		read, e := service.ReadReview(ctx, viewer, sc, in.ClaimID, 1)
		if e != nil || read.ApprovalEligible {
			t.Fatalf("withdrawn authority remained eligible: %+v %v", read, e)
		}
		designate(citation, 1, 1, 2)
		read, e = service.DecideReview(ctx, owner, sc, in.ClaimID, approve, key, 1)
		if e != nil || read.ApprovalEligible {
			t.Fatalf("redesignation resurrected old receipt: %+v %v", read, e)
		}
		if _, e = ev.Revoke(ctx, ea, es, citation.SourceID, "Evidence removed", uuid.NewString(), 1); e != nil {
			t.Fatal(e)
		}
		_, e = service.ReadReview(ctx, viewer, sc, in.ClaimID, 1)
		check(e, ErrNotFound)
		_, e = service.DecideReview(ctx, owner, sc, in.ClaimID, approve, key, 1)
		check(e, ErrNotFound)
	})
	for _, change := range []string{"claim", "entity", "source", "new conflict"} {
		t.Run(change+" advancement invalidates approval", func(t *testing.T) {
			in, citation := fixture()
			reviewedID := in.ClaimID
			designate(citation, 1, 1, 0)
			if _, e := service.DecideReview(ctx, owner, sc, in.ClaimID, approve, uuid.NewString(), 1); e != nil {
				t.Fatal(e)
			}
			var e error
			switch change {
			case "claim":
				in.Literal.Value = json.RawMessage(`"Updated method"`)
				_, e = service.CreateClaim(ctx, owner, sc, in, uuid.NewString(), 1)
			case "entity":
				_, e = service.CreateEntity(ctx, owner, sc, EntityInput{EntityID: in.SubjectID, Type: "operation", Name: "Updated operation", SourceID: origin.SourceID, SourceRevision: 1}, uuid.NewString(), 1)
			case "source":
				citation.Text = "Updated authoritative process"
				_, e = ev.CreateOrRevise(ctx, ea, es, citation, uuid.NewString(), 1)
			case "new conflict":
				in.ClaimID = uuid.NewString()
				in.Literal.Value = json.RawMessage(`"Conflicting method"`)
				_, e = service.CreateClaim(ctx, owner, sc, in, uuid.NewString(), 0)
			}
			if e != nil {
				t.Fatal(e)
			}
			out, e := service.ReadReview(ctx, viewer, sc, reviewedID, 1)
			if e != nil || out.ApprovalEligible {
				t.Fatalf("%s retained approval: %+v %v", change, out, e)
			}
		})
	}
	t.Run("current text and unit conflicts block approval", func(t *testing.T) {
		for _, kind := range []string{"text", "unit"} {
			in, citation := fixture()
			designate(citation, 1, 1, 0)
			if kind == "unit" {
				in.Predicate = "capacity_per_hour"
				in.Literal = &Literal{Type: "decimal", Value: json.RawMessage(`"10"`), Unit: "units/h"}
				if _, e := service.CreateClaim(ctx, owner, sc, in, uuid.NewString(), 1); e != nil {
					t.Fatal(e)
				}
			}
			peer := in
			peer.ClaimID = uuid.NewString()
			if kind == "unit" {
				peer.Literal = &Literal{Type: "decimal", Value: json.RawMessage(`"10"`), Unit: "units/s"}
			} else {
				peer.Literal = &Literal{Type: "string", Value: json.RawMessage(`"Different instruction"`)}
			}
			if _, e := service.CreateClaim(ctx, owner, sc, peer, uuid.NewString(), 0); e != nil {
				t.Fatal(e)
			}
			decision, expected := approve, int64(1)
			if kind == "unit" {
				decision.ClaimRevision = 2
				expected = 2
			}
			_, e := service.DecideReview(ctx, owner, sc, in.ClaimID, decision, uuid.NewString(), expected)
			check(e, ErrReviewBlocked)
		}
	})
	// WIS-270: every retrieval and review leaves exactly one knowledge_audit row
	// naming actor, scope and subject, and no audit row carries raw text.
	t.Run("WIS-270 one audit row per retrieval and review without raw text", func(t *testing.T) {
		as := Scope{TenantID: sc.TenantID, SiteID: uuid.NewString()}
		exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'audit fixture')`, as.TenantID, as.SiteID)
		for role, a := range map[string]Actor{"owner": owner, "supervisor": supervisor, "viewer": viewer} {
			exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, as.TenantID, as.SiteID, a.Issuer, a.Subject, role)
		}
		aes := evidence.Scope{TenantID: as.TenantID, SiteID: as.SiteID}
		marker := "WIS270-AUDIT-MARKER-" + uuid.NewString()
		src := evidence.Input{SourceID: uuid.NewString(), Kind: "document", Title: marker, Text: marker + " raw source text"}
		if _, e := ev.CreateOrRevise(ctx, ea, aes, src, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		if _, e := ev.DecideAuthority(ctx, ea, aes, src.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Owner reviewed source"}, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		span, e := ev.CreateSpan(ctx, ea, aes, evidence.SpanInput{SourceID: src.SourceID, Revision: 1, Start: 0, End: len(marker)}, uuid.NewString(), 1)
		if e != nil {
			t.Fatal(e)
		}
		entity := uuid.NewString()
		if _, e = service.CreateEntity(ctx, owner, as, EntityInput{EntityID: entity, Type: "operation", Name: marker, SourceID: src.SourceID, SourceRevision: 1}, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		literal := func(v string) *Literal {
			raw, _ := json.Marshal(v)
			return &Literal{Type: "string", Value: raw}
		}
		claim := ClaimInput{ClaimID: uuid.NewString(), SubjectID: entity, SubjectRevision: 1, Predicate: "actual_step", Literal: literal(marker + " step"), EffectiveFrom: "2026-09-27T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
		if _, e = service.CreateClaim(ctx, owner, as, claim, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		type row struct {
			tenant, site, issuer, subject, event string
			target                               sql.NullString
			revision                             sql.NullInt64
		}
		ids := func() map[string]bool {
			t.Helper()
			rows, e := admin.QueryContext(ctx, `SELECT id FROM supervisor.knowledge_audit WHERE tenant_id=$1 AND site_id=$2`, as.TenantID, as.SiteID)
			if e != nil {
				t.Fatal(e)
			}
			defer rows.Close()
			out := map[string]bool{}
			for rows.Next() {
				var id string
				if e = rows.Scan(&id); e != nil {
					t.Fatal(e)
				}
				out[id] = true
			}
			if e = rows.Err(); e != nil {
				t.Fatal(e)
			}
			return out
		}
		// Every new audit row in the scope is counted, so a duplicate or extra event fails.
		exactlyOne := func(call func() error) row {
			t.Helper()
			before := ids()
			if e := call(); e != nil {
				t.Fatal(e)
			}
			rows, e := admin.QueryContext(ctx, `SELECT id,tenant_id,site_id,actor_issuer,actor_subject,event_type,target_id,revision FROM supervisor.knowledge_audit WHERE tenant_id=$1 AND site_id=$2`, as.TenantID, as.SiteID)
			if e != nil {
				t.Fatal(e)
			}
			defer rows.Close()
			var added []row
			for rows.Next() {
				var id string
				var r row
				if e = rows.Scan(&id, &r.tenant, &r.site, &r.issuer, &r.subject, &r.event, &r.target, &r.revision); e != nil {
					t.Fatal(e)
				}
				if !before[id] {
					added = append(added, r)
				}
			}
			if e = rows.Err(); e != nil {
				t.Fatal(e)
			}
			if len(added) != 1 {
				t.Fatalf("audit rows added=%d %+v", len(added), added)
			}
			return added[0]
		}
		reviewKey := uuid.NewString()
		decision := ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: marker + " review reason"}
		for _, tc := range []struct {
			name, event string
			actor       Actor
			target      string
			revision    int64
			call        func() error
		}{
			{"claim read", "claim.read", viewer, claim.ClaimID, 1, func() error {
				c, e := service.ReadClaim(ctx, viewer, as, claim.ClaimID, 1)
				if e == nil && c.ClaimID != claim.ClaimID {
					e = errors.New("wrong claim read")
				}
				return e
			}},
			{"claim list", "claim.listed", viewer, "", 0, func() error {
				p, e := service.ListClaims(ctx, viewer, as, 100, "", "")
				if e == nil && len(p.Claims) != 1 {
					e = errors.New("claim list size")
				}
				return e
			}},
			{"review decision", "review.decided", supervisor, claim.ClaimID, 1, func() error {
				_, e := service.DecideReview(ctx, supervisor, as, claim.ClaimID, decision, reviewKey, 1)
				return e
			}},
			// A replay is audited as a replay, never as a second decision.
			{"review replay", "mutation.replayed", supervisor, claim.ClaimID, 1, func() error {
				_, e := service.DecideReview(ctx, supervisor, as, claim.ClaimID, decision, reviewKey, 1)
				return e
			}},
			{"review read", "review.read", viewer, claim.ClaimID, 1, func() error {
				r, e := service.ReadReview(ctx, viewer, as, claim.ClaimID, 0)
				if e == nil && r.Reason != decision.Reason {
					e = errors.New("review read lost reason")
				}
				return e
			}},
			{"conflicts read", "conflicts.read", viewer, claim.ClaimID, 1, func() error {
				p, e := service.ListConflicts(ctx, viewer, as, claim.ClaimID, 1, 100, "")
				if e == nil && len(p.Conflicts) != 1 {
					e = errors.New("conflict list size")
				}
				return e
			}},
		} {
			if tc.event == "conflicts.read" {
				// A conflicting peer created after approval gives the read real content.
				peer := claim
				peer.ClaimID = uuid.NewString()
				peer.Literal = literal(marker + " disputed step")
				if _, e = service.CreateClaim(ctx, owner, as, peer, uuid.NewString(), 0); e != nil {
					t.Fatal(e)
				}
			}
			got := exactlyOne(tc.call)
			wantTarget := sql.NullString{String: tc.target, Valid: tc.target != ""}
			wantRevision := sql.NullInt64{Int64: tc.revision, Valid: tc.target != ""}
			if got.tenant != as.TenantID || got.site != as.SiteID || got.issuer != tc.actor.Issuer || got.subject != tc.actor.Subject || got.event != tc.event || got.target != wantTarget || got.revision != wantRevision {
				t.Fatalf("%s audit=%+v", tc.name, got)
			}
		}
		// The marker reached persistence, so its absence from audit rows is meaningful.
		var persisted, leaked int
		if e = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_claim_reviews WHERE tenant_id=$1 AND site_id=$2 AND reason LIKE '%'||$3||'%'`, as.TenantID, as.SiteID, marker).Scan(&persisted); e != nil || persisted != 1 {
			t.Fatalf("marker review rows=%d %v", persisted, e)
		}
		for _, table := range []string{"knowledge_audit", "evidence_audit"} {
			if e = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.`+table+` a WHERE tenant_id=$1 AND site_id=$2 AND to_jsonb(a)::text LIKE '%'||$3||'%'`, as.TenantID, as.SiteID, marker).Scan(&leaked); e != nil || leaked != 0 {
				t.Fatalf("%s rows with raw text=%d %v", table, leaked, e)
			}
		}
	})
	t.Run("audit failure rolls back decision and receipt", func(t *testing.T) {
		in, citation := fixture()
		designate(citation, 1, 1, 0)
		blocker, e := admin.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer blocker.Rollback()
		if _, e = blocker.ExecContext(ctx, `LOCK TABLE supervisor.knowledge_audit IN ACCESS EXCLUSIVE MODE`); e != nil {
			t.Fatal(e)
		}
		short, stop := context.WithTimeout(ctx, 150*time.Millisecond)
		key := uuid.NewString()
		_, e = service.DecideReview(short, owner, sc, in.ClaimID, approve, key, 1)
		stop()
		blocker.Rollback()
		if e == nil {
			t.Fatal("audit blockage committed review")
		}
		_, e = service.ReadReview(ctx, viewer, sc, in.ClaimID, 1)
		check(e, ErrNotFound)
		if _, e = service.DecideReview(ctx, owner, sc, in.ClaimID, approve, key, 1); e != nil {
			t.Fatalf("rolled-back idempotency key not reusable: %v", e)
		}
	})
	t.Run("runtime isolation and immutable decisions", func(t *testing.T) {
		tx, e := runtime.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, foreign.TenantID); e != nil {
			t.Fatal(e)
		}
		var count int
		if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_claim_reviews`).Scan(&count); e != nil || count != 0 {
			t.Fatalf("foreign review rows=%d %v", count, e)
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
		_, e = tx.ExecContext(ctx, `UPDATE supervisor.knowledge_claim_reviews SET reason='rewritten' WHERE tenant_id=$1`, sc.TenantID)
		requireSQLState(t, e, "42501")
	})
	t.Run("database foreign keys bind exact claim and authority scope", func(t *testing.T) {
		in, citation := fixture()
		authority := designate(citation, 1, 1, 0)
		insert := `INSERT INTO supervisor.knowledge_claim_reviews(tenant_id,site_id,review_id,claim_id,claim_revision,verdict,reason,actor_issuer,actor_subject,auth_time,authority_decision_id,authority_source_id,authority_source_revision,authority_revision) VALUES($1,$2,$3,$4,1,'approve','Synthetic FK test',$5,$6,now(),$7,$8,1,1)`
		for _, bad := range []struct {
			scope  Scope
			source string
		}{{foreign, citation.SourceID}, {sc, uuid.NewString()}} {
			_, e := admin.ExecContext(ctx, insert, bad.scope.TenantID, bad.scope.SiteID, uuid.NewString(), in.ClaimID, owner.Issuer, owner.Subject, authority.DecisionID, bad.source)
			var pg *pgconn.PgError
			if !errors.As(e, &pg) || pg.Code != "23503" {
				t.Fatalf("cross-scope provenance FK not enforced: %v", e)
			}
		}
	})
	t.Run("new target revision cannot bypass current conflicting peer", func(t *testing.T) {
		in, citation := fixture()
		designate(citation, 1, 1, 0)
		peer := in
		peer.ClaimID = uuid.NewString()
		peer.Literal = &Literal{Type: "string", Value: json.RawMessage(`"Still conflicting instruction"`)}
		if _, e := service.CreateClaim(ctx, owner, sc, peer, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		if _, e := service.CreateClaim(ctx, owner, sc, in, uuid.NewString(), 1); e != nil {
			t.Fatal(e)
		}
		fresh := approve
		fresh.ClaimRevision = 2
		_, e := service.DecideReview(ctx, owner, sc, in.ClaimID, fresh, uuid.NewString(), 2)
		check(e, ErrReviewBlocked)
	})
	for _, initiallyApproved := range []bool{true, false} {
		for _, change := range []string{"peer claim revision", "peer source revision", "peer source revocation"} {
			state := "unreviewed"
			if initiallyApproved {
				state = "approved"
			}
			t.Run("contested "+state+" remains blocked after "+change, func(t *testing.T) {
				in, citation := fixture()
				designate(citation, 1, 1, 0)
				key := uuid.NewString()
				if initiallyApproved {
					if _, e := service.DecideReview(ctx, owner, sc, in.ClaimID, approve, key, 1); e != nil {
						t.Fatal(e)
					}
				}
				peerSource := source()
				peerSpan, e := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: peerSource.SourceID, Revision: 1, Start: 0, End: 9}, uuid.NewString(), 1)
				if e != nil {
					t.Fatal(e)
				}
				peer := in
				peer.ClaimID = uuid.NewString()
				peer.SourceSpanIDs = []string{peerSpan.ID}
				peer.Literal = &Literal{Type: "string", Value: json.RawMessage(`"Conflicting peer instruction"`)}
				if _, e = service.CreateClaim(ctx, owner, sc, peer, uuid.NewString(), 0); e != nil {
					t.Fatal(e)
				}
				if initiallyApproved {
					out, e := service.ReadReview(ctx, viewer, sc, in.ClaimID, 1)
					if e != nil || out.ApprovalEligible {
						t.Fatalf("active conflict did not block: %+v %v", out, e)
					}
				}
				switch change {
				case "peer claim revision":
					peer.Literal = in.Literal
					_, e = service.CreateClaim(ctx, owner, sc, peer, uuid.NewString(), 1)
				case "peer source revision":
					peerSource.Text = "Changed peer evidence"
					_, e = ev.CreateOrRevise(ctx, ea, es, peerSource, uuid.NewString(), 1)
				case "peer source revocation":
					_, e = ev.Revoke(ctx, ea, es, peerSource.SourceID, "Peer evidence removed", uuid.NewString(), 1)
				}
				if e != nil {
					t.Fatal(e)
				}
				page, e := service.ListConflicts(ctx, viewer, sc, in.ClaimID, 1, 100, "")
				if e != nil || len(page.Conflicts) != 0 {
					t.Fatalf("inactive peer remained in active conflicts: %+v %v", page, e)
				}
				if initiallyApproved {
					out, e := service.ReadReview(ctx, viewer, sc, in.ClaimID, 1)
					if e != nil || out.ApprovalEligible {
						t.Errorf("contested approval resurrected after %s: %+v %v", change, out, e)
					}
					out, e = service.DecideReview(ctx, owner, sc, in.ClaimID, approve, key, 1)
					if e != nil || out.ApprovalEligible {
						t.Errorf("contested approval receipt resurrected after %s: %+v %v", change, out, e)
					}
				} else {
					_, e = service.DecideReview(ctx, owner, sc, in.ClaimID, approve, key, 1)
					if !errors.Is(e, ErrReviewBlocked) {
						t.Errorf("historically contested exact revision approved: %v", e)
					}
				}
				// Recovery requires an explicit new target revision and a fresh human review.
				if _, e = service.CreateClaim(ctx, owner, sc, in, uuid.NewString(), 1); e != nil {
					t.Fatal(e)
				}
				fresh := approve
				fresh.ClaimRevision = 2
				out, e := service.DecideReview(ctx, owner, sc, in.ClaimID, fresh, uuid.NewString(), 2)
				if e != nil || !out.ApprovalEligible || out.ClaimRevision != 2 {
					t.Fatalf("fresh review did not recover: %+v %v", out, e)
				}
			})
		}
	}
}
