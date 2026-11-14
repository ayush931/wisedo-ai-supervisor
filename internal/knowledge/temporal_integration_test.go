//go:build integration

package knowledge

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/google/uuid"
)

func TestPostgresTemporalCandidateCycles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("real PostgreSQL URLs required")
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	runtime, err := sql.Open("pgx", runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := admin.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	sc := Scope{uuid.NewString(), uuid.NewString()}
	sum := sha256.Sum256([]byte(uuid.NewString()))
	actor := Actor{"https://temporal-knowledge.invalid", uuid.NewString(), hex.EncodeToString(sum[:])}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		tx, e := admin.BeginTx(cleanup, nil)
		if e != nil {
			t.Error(e)
			return
		}
		defer tx.Rollback()
		if _, e = tx.ExecContext(cleanup, "SELECT set_config('supervisor.tenant_id',$1,true)", sc.TenantID); e != nil {
			t.Error(e)
			return
		}
		if _, e = tx.ExecContext(cleanup, "SET CONSTRAINTS ALL DEFERRED"); e != nil {
			t.Error(e)
			return
		}
		for _, table := range []string{"knowledge_conflicts", "knowledge_audit", "knowledge_receipts", "knowledge_claim_spans", "knowledge_claim_entity_refs", "knowledge_claim_revisions", "knowledge_claim_heads", "knowledge_entity_revisions", "knowledge_entity_heads", "knowledge_entities", "evidence_audit", "evidence_receipts", "evidence_revocations", "evidence_spans", "evidence_revisions", "evidence_heads", "memberships", "sites", "tenants"} {
			column := "tenant_id"
			if table == "tenants" {
				column = "id"
			}
			if _, e = tx.ExecContext(cleanup, "DELETE FROM supervisor."+table+" WHERE "+column+"=$1", sc.TenantID); e != nil {
				t.Error(e)
				return
			}
		}
		if _, e = tx.ExecContext(cleanup, "DELETE FROM supervisor.browser_sessions WHERE token_hash=$1", actor.SessionHash); e != nil {
			t.Error(e)
			return
		}
		if e = tx.Commit(); e != nil {
			t.Error(e)
		}
	}()
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'temporal synthetic')`, sc.TenantID)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'temporal synthetic')`, sc.TenantID, sc.SiteID)
	exec(`INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'fixture',now(),now(),now()+interval '1 hour',now())`, actor.SessionHash, actor.Issuer, actor.Subject)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, sc.TenantID, sc.SiteID, actor.Issuer, actor.Subject)
	ev := evidence.New(runtime)
	ea := evidence.Actor{Issuer: actor.Issuer, Subject: actor.Subject, SessionHash: actor.SessionHash}
	es := evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}
	source, err := ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Temporal evidence", Text: "Synthetic observed route"}, uuid.NewString(), 0)
	if err != nil {
		t.Fatal(err)
	}
	span, err := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: source.SourceID, Revision: 1, Start: 0, End: 9}, uuid.NewString(), 1)
	if err != nil {
		t.Fatal(err)
	}
	service := New(runtime)
	entity := func() string {
		t.Helper()
		id := uuid.NewString()
		_, e := service.CreateEntity(ctx, actor, sc, EntityInput{EntityID: id, Type: "operation", Name: "Temporal operation", SourceID: source.SourceID, SourceRevision: 1}, uuid.NewString(), 0)
		if e != nil {
			t.Fatal(e)
		}
		return id
	}
	stamp := func(hour int) string { return time.Date(2026, 9, 27, hour, 0, 0, 0, time.UTC).Format(time.RFC3339) }
	claimCitation := func(subject, object string, from, to int, citation string) error {
		in := ClaimInput{ClaimID: uuid.NewString(), SubjectID: subject, SubjectRevision: 1, Predicate: "precedes", ObjectEntityID: object, ObjectRevision: 1, EffectiveFrom: stamp(from), EvidenceClass: "report", SourceSpanIDs: []string{citation}}
		if to >= 0 {
			end := stamp(to)
			in.EffectiveTo = &end
		}
		_, e := service.CreateClaim(ctx, actor, sc, in, uuid.NewString(), 0)
		return e
	}
	claim := func(subject, object string, from, to int) error {
		return claimCitation(subject, object, from, to, span.ID)
	}
	for _, tc := range []struct {
		name     string
		from, to int
		want     error
	}{
		{"disjoint reverse", 3, 4, nil},
		{"touching reverse", 2, 3, nil},
		{"overlapping reverse", 1, 3, ErrCycle},
		{"unbounded reverse", 1, -1, ErrCycle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := entity(), entity()
			if e := claim(a, b, 0, 2); e != nil {
				t.Fatal(e)
			}
			if e := claim(b, a, tc.from, tc.to); !errors.Is(e, tc.want) {
				t.Fatalf("reverse result=%v want=%v", e, tc.want)
			}
		})
	}
	t.Run("whole path intersection required", func(t *testing.T) {
		a, b, c, d := entity(), entity(), entity(), entity()
		for _, edge := range []struct {
			from, to string
			lo, hi   int
		}{{a, b, 0, 3}, {b, c, 2, 5}, {c, d, 4, 7}} {
			if e := claim(edge.from, edge.to, edge.lo, edge.hi); e != nil {
				t.Fatal(e)
			}
		}
		if e := claim(d, a, 0, 8); e != nil {
			t.Fatalf("no common path interval must be allowed: %v", e)
		}
	})
	t.Run("common path intersection blocked", func(t *testing.T) {
		a, b, c := entity(), entity(), entity()
		if e := claim(a, b, 0, 5); e != nil {
			t.Fatal(e)
		}
		if e := claim(b, c, 2, 7); e != nil {
			t.Fatal(e)
		}
		if e := claim(c, a, 3, 4); !errors.Is(e, ErrCycle) {
			t.Fatalf("common intersection result=%v", e)
		}
	})
	t.Run("distinct intervals at same vertex survive deduplication", func(t *testing.T) {
		a, b, c, d, e := entity(), entity(), entity(), entity(), entity()
		for _, edge := range []struct {
			from, to string
			lo, hi   int
		}{{a, b, 0, 2}, {a, c, 3, 5}, {b, d, 0, 2}, {c, d, 3, 5}, {d, e, 4, 5}} {
			if err := claim(edge.from, edge.to, edge.lo, edge.hi); err != nil {
				t.Fatal(err)
			}
		}
		if err := claim(e, a, 0, 10); !errors.Is(err, ErrCycle) {
			t.Fatalf("second interval path lost: %v", err)
		}
	})
	for _, mode := range []string{"stale", "revoked"} {
		t.Run(mode+" citation removes old edge only", func(t *testing.T) {
			a, b := entity(), entity()
			old := evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Old route evidence", Text: "Old route observed"}
			if _, e := ev.CreateOrRevise(ctx, ea, es, old, uuid.NewString(), 0); e != nil {
				t.Fatal(e)
			}
			oldSpan, e := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: old.SourceID, Revision: 1, Start: 0, End: 9}, uuid.NewString(), 1)
			if e != nil {
				t.Fatal(e)
			}
			if e = claimCitation(a, b, 0, 4, oldSpan.ID); e != nil {
				t.Fatal(e)
			}
			if e = claim(b, a, 1, 3); !errors.Is(e, ErrCycle) {
				t.Fatalf("current cited edge failed to prevent cycle: %v", e)
			}
			if mode == "stale" {
				old.Text = "Changed observed route"
				_, e = ev.CreateOrRevise(ctx, ea, es, old, uuid.NewString(), 1)
			} else {
				_, e = ev.Revoke(ctx, ea, es, old.SourceID, "Withdraw old route evidence", uuid.NewString(), 1)
			}
			if e != nil {
				t.Fatal(e)
			}
			if e = claim(b, a, 1, 3); e != nil {
				t.Fatalf("ineligible old edge governed cycle: %v", e)
			}
			if e = claim(a, b, 1, 3); !errors.Is(e, ErrCycle) {
				t.Fatalf("current replacement edge failed to prevent cycle: %v", e)
			}
		})
	}
	t.Run("bound counts distinct interval states not vertices", func(t *testing.T) {
		a, b, c, d := entity(), entity(), entity(), entity()
		for hour := range 999 {
			if e := claim(a, b, hour, hour+1); e != nil {
				t.Fatal(e)
			}
		}
		// The seed a and 999 different intervals at b are exactly 1000 states.
		if e := claim(c, a, 0, 1001); e != nil {
			t.Fatalf("exact interval-state budget rejected: %v", e)
		}
		if e := claim(a, b, 999, 1000); e != nil {
			t.Fatal(e)
		}
		// Only two reachable entity IDs, but 1001 distinct interval states.
		if e := claim(d, a, 0, 1001); !errors.Is(e, ErrTraversalLimit) {
			t.Fatalf("interval-state budget bypassed: %v", e)
		}
	})
}
