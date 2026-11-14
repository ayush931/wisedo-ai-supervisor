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

// WIS-270 "correlated-source false corroboration" and "repeated source replay", plus the spec's
// positive single-authoritative-source-plus-reviewer and negative contested-source fixtures.
func TestPostgresWIS270Corroboration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: real PostgreSQL URLs required")
	}
	control, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })
	database := "corroboration_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	sc := Scope{uuid.NewString(), uuid.NewString()}
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'corroboration synthetic')`, sc.TenantID)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'corroboration synthetic')`, sc.TenantID, sc.SiteID)
	actor := func(role string) Actor {
		t.Helper()
		a := Actor{"https://corroboration.invalid", uuid.NewString(), strings.ReplaceAll(uuid.NewString(), "-", "") + strings.ReplaceAll(uuid.NewString(), "-", "")}
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'fixture',now(),now(),now()+interval '1 hour',now())`, a.SessionHash, a.Issuer, a.Subject)
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, role)
		return a
	}
	owner, reviewer := actor("owner"), actor("supervisor")
	service, ev := New(runtime), evidence.New(runtime)
	ea := evidence.Actor{Issuer: owner.Issuer, Subject: owner.Subject, SessionHash: owner.SessionHash}
	es := evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}
	source := func(kind, text string) evidence.Source {
		t.Helper()
		out, e := ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: kind, Title: "WIS-270 source", Text: text}, uuid.NewString(), 0)
		if e != nil {
			t.Fatal(e)
		}
		return out
	}
	span := func(s evidence.Source) string {
		t.Helper()
		out, e := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: s.SourceID, Revision: 1, Start: 0, End: 9}, uuid.NewString(), 1)
		if e != nil {
			t.Fatal(e)
		}
		return out.ID
	}
	sop := "SOP-7: torque fasteners to 40 Nm before release"
	original := source("document", sop)
	copied := source("document", sop) // a second upload of the same SOP bytes
	independent := source("event", "Torque station log: 40.1 Nm recorded at release")
	claim := func(value string, sources ...evidence.Source) ClaimInput {
		t.Helper()
		entity := uuid.NewString()
		if _, e := service.CreateEntity(ctx, owner, sc, EntityInput{EntityID: entity, Type: "operation", Name: "Torque fasteners", SourceID: original.SourceID, SourceRevision: 1}, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		spans := []string{}
		for _, s := range sources {
			spans = append(spans, span(s))
		}
		in := ClaimInput{ClaimID: uuid.NewString(), SubjectID: entity, SubjectRevision: 1, Predicate: "actual_step", Literal: &Literal{Type: "string", Value: json.RawMessage(`"` + value + `"`)}, EffectiveFrom: "2026-09-27T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: spans}
		if _, e := service.CreateClaim(ctx, owner, sc, in, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		return in
	}
	read := func(in ClaimInput) Corroboration {
		t.Helper()
		out, e := service.ReadClaim(ctx, owner, sc, in.ClaimID, 0)
		if e != nil {
			t.Fatal(e)
		}
		page, e := service.ListClaims(ctx, owner, sc, 100, "", in.SubjectID)
		if e != nil || len(page.Claims) != 1 || page.Claims[0].Corroboration != out.Corroboration {
			t.Fatalf("list corroboration differs from read %+v: %+v %v", out.Corroboration, page.Claims, e)
		}
		return out.Corroboration
	}
	approve := ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Reviewed exact claim and designated source"}

	copies := claim("Torque to 40 Nm", original, copied)
	corroborated := claim("Torque to 40 Nm", original, copied, independent)
	t.Run("correlated copies count as one independence group", func(t *testing.T) {
		if got := read(copies); got != (Corroboration{IndependentGroups: 1}) {
			t.Fatalf("two copies of one SOP = %+v, want 1 group", got)
		}
		if got := read(corroborated); got != (Corroboration{IndependentGroups: 2}) {
			t.Fatalf("copy + independent observation = %+v, want 2 groups", got)
		}
	})
	t.Run("repeated source replay does not inflate corroboration", func(t *testing.T) {
		replayed := source("interview", sop)
		if replayed.GroupID != original.SourceID {
			t.Fatalf("replayed bytes group=%s want %s", replayed.GroupID, original.SourceID)
		}
		if got := read(claim("Torque to 40 Nm", original, copied, replayed)); got.IndependentGroups != 1 {
			t.Fatalf("replay inflated corroboration: %+v", got)
		}
	})
	t.Run("positive: single designated current source plus authorized reviewer approves", func(t *testing.T) {
		single := claim("Torque to 40 Nm", original)
		if _, e := service.DecideReview(ctx, reviewer, sc, single.ClaimID, approve, uuid.NewString(), 1); !errors.Is(e, ErrReviewBlocked) {
			t.Fatalf("approval without a designated source: %v", e)
		}
		if _, e := ev.DecideAuthority(ctx, ea, es, original.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Owner verified SOP-7"}, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		if got := read(single); got != (Corroboration{IndependentGroups: 1, Authoritative: true}) {
			t.Fatalf("designated single source = %+v", got)
		}
		out, e := service.DecideReview(ctx, reviewer, sc, single.ClaimID, approve, uuid.NewString(), 1)
		if e != nil || !out.ApprovalEligible || out.AuthoritySourceID != original.SourceID {
			t.Fatalf("single authoritative source approval=%+v %v", out, e)
		}
		// Corroboration stays informational: the copies claim also cites the now-designated original.
		if got := read(copies); got != (Corroboration{IndependentGroups: 1, Authoritative: true}) {
			t.Fatalf("copies after designation = %+v", got)
		}
	})
	t.Run("negative: contested claim cannot be approved despite authoritative source", func(t *testing.T) {
		target := claim("Torque to 40 Nm", original)
		peer := target
		peer.ClaimID = uuid.NewString()
		peer.Literal = &Literal{Type: "string", Value: json.RawMessage(`"Torque to 55 Nm"`)}
		peer.SourceSpanIDs = []string{span(original)}
		if _, e := service.CreateClaim(ctx, owner, sc, peer, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		got, e := service.ReadClaim(ctx, owner, sc, target.ClaimID, 0)
		if e != nil || !got.Corroboration.Authoritative {
			t.Fatalf("contested fixture must still cite an authoritative source: %+v %v", got.Corroboration, e)
		}
		if _, e = service.DecideReview(ctx, reviewer, sc, target.ClaimID, approve, uuid.NewString(), 1); !errors.Is(e, ErrReviewBlocked) {
			t.Fatalf("contested claim approval error=%v want ErrReviewBlocked", e)
		}
	})
	t.Run("caller cannot declare approved_document", func(t *testing.T) {
		if _, e := ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "document", Title: "self-declared", Text: "Self-declared SOP " + uuid.NewString(), AuthorityClass: "approved_document"}, uuid.NewString(), 0); !errors.Is(e, evidence.ErrInvalid) {
			t.Fatalf("self-declared approved_document error=%v want ErrInvalid", e)
		}
		observed, e := ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "document", Title: "observed", Text: "Observed SOP " + uuid.NewString(), AuthorityClass: "observed"}, uuid.NewString(), 0)
		if e != nil || observed.AuthorityClass != "observed" {
			t.Fatalf("observed=%+v %v", observed, e)
		}
		if got := read(claim("Torque to 40 Nm", observed)); got != (Corroboration{IndependentGroups: 1}) {
			t.Fatalf("undesignated observed source = %+v, want not authoritative", got)
		}
	})
	t.Run("superseded cited revision stops counting", func(t *testing.T) {
		stable, moving := source("event", "Station A log "+uuid.NewString()), source("event", "Station B log "+uuid.NewString())
		in := claim("Torque to 40 Nm", stable, moving)
		if got := read(in); got.IndependentGroups != 2 {
			t.Fatalf("before revision = %+v", got)
		}
		if _, e := ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: moving.SourceID, Kind: "event", Title: "WIS-270 source", Text: "Station B corrected " + uuid.NewString()}, uuid.NewString(), 1); e != nil {
			t.Fatal(e)
		}
		if got := read(in); got.IndependentGroups != 1 {
			t.Fatalf("after cited revision superseded = %+v, want 1", got)
		}
	})
	t.Run("revoked citation fails closed; revoked founder leaves current copy counting", func(t *testing.T) {
		kept, dropped := source("event", "Station C log "+uuid.NewString()), source("event", "Station D log "+uuid.NewString())
		in := claim("Torque to 40 Nm", kept, dropped)
		if _, e := ev.Revoke(ctx, ea, es, dropped.SourceID, "Withdrawn synthetic evidence", uuid.NewString(), 1); e != nil {
			t.Fatal(e)
		}
		// Claims citing revoked evidence are not served at all (rule 06), so no count is shown.
		if _, e := service.ReadClaim(ctx, owner, sc, in.ClaimID, 0); !errors.Is(e, ErrNotFound) {
			t.Fatalf("claim citing revoked source read error=%v want ErrNotFound", e)
		}
		text := "Shared SOP-9 " + uuid.NewString()
		founder := source("document", text)
		copyOf := source("document", text)
		if copyOf.GroupID != founder.SourceID {
			t.Fatalf("copy group=%s want founder %s", copyOf.GroupID, founder.SourceID)
		}
		viaCopy := claim("Torque to 40 Nm", copyOf)
		if _, e := ev.Revoke(ctx, ea, es, founder.SourceID, "Withdrawn synthetic evidence", uuid.NewString(), 1); e != nil {
			t.Fatal(e)
		}
		if got := read(viaCopy); got.IndependentGroups != 1 {
			t.Fatalf("copy after founder revocation = %+v, want 1", got)
		}
	})
}
