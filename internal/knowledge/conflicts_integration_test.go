//go:build integration

package knowledge

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/google/uuid"
)

func TestPostgresCandidateConflicts(t *testing.T) {
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
	// This fixture changes SELECT policies and takes audit table locks. A separate
	// database keeps those test gates from changing plans or blocking other packages.
	database := "conflict_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	for _, raw := range []*string{&adminURL, &runtimeURL} {
		u, e := url.Parse(*raw)
		if e != nil {
			t.Fatal(e)
		}
		u.Path = "/" + database
		*raw = u.String()
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err = migrate.Apply(ctx, admin); err != nil {
		t.Fatal(err)
	}
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
	sc, other := Scope{uuid.NewString(), uuid.NewString()}, Scope{uuid.NewString(), uuid.NewString()}
	for _, s := range []Scope{sc, other} {
		exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'conflict synthetic')`, s.TenantID)
		exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'conflict synthetic')`, s.TenantID, s.SiteID)
	}
	sum := sha256.Sum256([]byte(uuid.NewString()))
	owner := Actor{"https://conflict.example", uuid.NewString(), hex.EncodeToString(sum[:])}
	exec(`INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'fixture',now(),now(),now()+interval '1 hour',now())`, owner.SessionHash, owner.Issuer, owner.Subject)
	for _, s := range []Scope{sc, other} {
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, s.TenantID, s.SiteID, owner.Issuer, owner.Subject)
	}
	ev := evidence.New(runtime)
	ea := evidence.Actor{Issuer: owner.Issuer, Subject: owner.Subject, SessionHash: owner.SessionHash}
	es := evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}
	src := evidence.Input{SourceID: uuid.NewString(), Kind: "document", Title: "Synthetic process", Text: "A synthetic source for comparisons"}
	if _, err = ev.CreateOrRevise(ctx, ea, es, src, "source", 0); err != nil {
		t.Fatal(err)
	}
	span, err := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: src.SourceID, Revision: 1, Start: 0, End: 10}, "span", 1)
	if err != nil {
		t.Fatal(err)
	}
	s := New(runtime)
	entity := func(kind string) string {
		t.Helper()
		id := uuid.NewString()
		_, e := s.CreateEntity(ctx, owner, sc, EntityInput{EntityID: id, Type: kind, Name: "Synthetic entity", SourceID: src.SourceID, SourceRevision: 1}, id, 0)
		if e != nil {
			t.Fatal(e)
		}
		return id
	}
	claim := func(subject, predicate string, literal *Literal, object string) ClaimInput {
		in := ClaimInput{ClaimID: uuid.NewString(), SubjectID: subject, SubjectRevision: 1, Predicate: predicate, Literal: literal, ObjectEntityID: object, EffectiveFrom: "2026-09-27T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
		if object != "" {
			in.ObjectRevision = 1
		}
		return in
	}
	create := func(in ClaimInput, expected int64) {
		t.Helper()
		if _, e := s.CreateClaim(ctx, owner, sc, in, uuid.NewString(), expected); e != nil {
			t.Fatal(e)
		}
	}
	list := func(in ClaimInput, want string) ConflictPage {
		t.Helper()
		out, e := s.ListConflicts(ctx, owner, sc, in.ClaimID, 0, 100, "")
		if e != nil {
			t.Fatal(e)
		}
		if want == "" && len(out.Conflicts) != 0 {
			t.Fatalf("unexpected conflicts %+v", out)
		}
		if want != "" && (len(out.Conflicts) != 1 || out.Conflicts[0].Kind != want) {
			t.Fatalf("want %s got %+v", want, out)
		}
		return out
	}
	var tracked ClaimInput
	for _, tc := range []struct{ name, predicate, kind, value1, value2, unit1, unit2, want string }{
		{"decimal", "capacity_per_hour", "decimal", `"12"`, `"13"`, "units/h", "units/h", "value_conflict"},
		{"unit", "capacity_per_hour", "decimal", `"12"`, `"12"`, "units/h", "units/s", "unit_review"},
		{"text", "actual_step", "string", `"Produce safely"`, `"Produce quickly"`, "", "", "text_review"},
		{"escaped equality", "actual_step", "string", `"caf\u00e9"`, `"café"`, "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			subject := entity("operation")
			a := claim(subject, tc.predicate, &Literal{Type: tc.kind, Value: json.RawMessage(tc.value1), Unit: tc.unit1}, "")
			b := claim(subject, tc.predicate, &Literal{Type: tc.kind, Value: json.RawMessage(tc.value2), Unit: tc.unit2}, "")
			create(a, 0)
			create(b, 0)
			out := list(a, tc.want)
			if len(out.Conflicts) > 0 {
				c := out.Conflicts[0]
				if c.SchemaVersion != 1 || c.Left.ClaimID >= c.Right.ClaimID {
					t.Fatalf("noncanonical refs %+v", c)
				}
			}
			if tc.name == "decimal" {
				tracked = a
			}
		})
	}
	t.Run("half open and unbounded", func(t *testing.T) {
		subject := entity("operation")
		a := claim(subject, "capacity_per_hour", &Literal{Type: "decimal", Value: json.RawMessage(`"1"`)}, "")
		end := "2026-09-27T01:00:00Z"
		a.EffectiveTo = &end
		b := claim(subject, "capacity_per_hour", &Literal{Type: "decimal", Value: json.RawMessage(`"2"`)}, "")
		b.EffectiveFrom = end
		create(a, 0)
		create(b, 0)
		list(a, "")
		list(b, "")
		c := b
		c.ClaimID = uuid.NewString()
		c.EffectiveFrom = "2026-09-27T02:00:00Z"
		c.Literal = &Literal{Type: "decimal", Value: json.RawMessage(`"3"`)}
		create(c, 0)
		out := list(c, "value_conflict")
		if out.Conflicts[0].EffectiveTo != nil || out.Conflicts[0].EffectiveFrom.UTC().Hour() != 2 {
			t.Fatalf("overlap %+v", out)
		}
	})
	t.Run("entity cardinality", func(t *testing.T) {
		subject := entity("operation")
		p1, p2 := entity("process"), entity("process")
		a, b := claim(subject, "part_of", nil, p1), claim(subject, "part_of", nil, p2)
		create(a, 0)
		create(b, 0)
		list(a, "value_conflict")
		m1, m2 := entity("material"), entity("material")
		a, b = claim(subject, "consumes", nil, m1), claim(subject, "consumes", nil, m2)
		create(a, 0)
		create(b, 0)
		list(a, "")
	})
	if _, err = s.ListConflicts(ctx, owner, other, tracked.ClaimID, 0, 100, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross scope %v", err)
	}
	t.Run("revoked peer is hidden without hiding target", func(t *testing.T) {
		subject := entity("operation")
		peerSource := evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: "Peer report", Text: "Peer evidence"}
		if _, e := ev.CreateOrRevise(ctx, ea, es, peerSource, "peer-source", 0); e != nil {
			t.Fatal(e)
		}
		peerSpan, e := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: peerSource.SourceID, Revision: 1, Start: 0, End: 4}, "peer-span", 1)
		if e != nil {
			t.Fatal(e)
		}
		a := claim(subject, "actual_step", &Literal{Type: "string", Value: json.RawMessage(`"a"`)}, "")
		b := claim(subject, "actual_step", &Literal{Type: "string", Value: json.RawMessage(`"b"`)}, "")
		b.SourceSpanIDs = []string{peerSpan.ID}
		create(a, 0)
		create(b, 0)
		list(a, "text_review")
		if _, e = ev.Revoke(ctx, ea, es, peerSource.SourceID, "Peer source removed", "peer-revoke", 1); e != nil {
			t.Fatal(e)
		}
		list(a, "")
		if _, e = s.ReadClaim(ctx, owner, sc, a.ClaimID, 0); e != nil {
			t.Fatalf("accessible target hidden %v", e)
		}
	})
	t.Run("latest target revision advances before empty page selection", func(t *testing.T) {
		subject := entity("operation")
		a := claim(subject, "actual_step", &Literal{Type: "string", Value: json.RawMessage(`"a"`)}, "")
		b := claim(subject, "actual_step", &Literal{Type: "string", Value: json.RawMessage(`"b"`)}, "")
		create(a, 0)
		create(b, 0)
		list(a, "text_review")
		name := "conflict_gate_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		readerURL, e := url.Parse(runtimeURL)
		if e != nil {
			t.Fatal(e)
		}
		query := readerURL.Query()
		query.Set("application_name", name)
		readerURL.RawQuery = query.Encode()
		reader, e := sql.Open("pgx", readerURL.String())
		if e != nil {
			t.Fatal(e)
		}
		defer reader.Close()
		// Restrict only this fixture connection. The claim SELECT has its old
		// statement snapshot before waiting; the following page query sees the new head.
		exec(fmt.Sprintf(`CREATE FUNCTION supervisor.%s(c uuid) RETURNS boolean LANGUAGE plpgsql AS $$ BEGIN IF current_setting('application_name')='%s' AND c='%s'::uuid THEN PERFORM pg_advisory_xact_lock(hashtextextended('%s',0)); END IF; RETURN true; END $$`, name, name, a.ClaimID, name))
		defer func() {
			cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			for _, q := range []string{fmt.Sprintf(`DROP POLICY IF EXISTS %s ON supervisor.knowledge_claim_revisions`, name), fmt.Sprintf(`DROP FUNCTION IF EXISTS supervisor.%s(uuid)`, name)} {
				if _, e := admin.ExecContext(cleanup, q); e != nil {
					t.Error(e)
				}
			}
		}()
		exec(fmt.Sprintf(`CREATE POLICY %s ON supervisor.knowledge_claim_revisions AS RESTRICTIVE FOR SELECT USING(supervisor.%s(claim_id))`, name, name))
		gate, e := admin.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer gate.Rollback()
		if _, e = gate.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, name); e != nil {
			t.Fatal(e)
		}
		type result struct {
			page ConflictPage
			err  error
		}
		done := make(chan result, 1)
		go func() {
			page, e := New(reader).ListConflicts(ctx, owner, sc, a.ClaimID, 0, 100, "")
			done <- result{page, e}
		}()
		deadline := time.Now().Add(3 * time.Second)
		for {
			var waiting bool
			e = admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event='advisory')`, name).Scan(&waiting)
			if e != nil {
				t.Fatal(e)
			}
			if waiting {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("reader did not reach deterministic claim snapshot gate")
			}
			time.Sleep(5 * time.Millisecond)
		}
		a.Literal = &Literal{Type: "string", Value: json.RawMessage(`"c"`)}
		create(a, 1)
		if e = gate.Commit(); e != nil {
			t.Fatal(e)
		}
		got := <-done
		if got.err != nil || len(got.page.Conflicts) != 1 {
			t.Fatalf("latest read returned false-empty page after revision advance: %+v %v", got.page, got.err)
		}
		c := got.page.Conflicts[0]
		if (c.Left.ClaimID == a.ClaimID && c.Left.Revision != 2) || (c.Right.ClaimID == a.ClaimID && c.Right.Revision != 2) {
			t.Fatalf("latest conflict references old target %+v", c)
		}
	})
	t.Run("bounded comparisons before insertion", func(t *testing.T) {
		subject := entity("operation")
		base := claim(subject, "capacity_per_hour", &Literal{Type: "decimal", Value: json.RawMessage(`"1"`)}, "")
		end := "2026-09-27T01:00:00Z"
		base.EffectiveTo = &end
		create(base, 0)
		seed, e := admin.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer seed.Rollback()
		if _, e = seed.ExecContext(ctx, "SELECT set_config('supervisor.tenant_id',$1,true)", sc.TenantID); e != nil {
			t.Fatal(e)
		}
		for _, q := range []string{
			`CREATE TEMP TABLE conflict_seed_ids ON COMMIT DROP AS SELECT gen_random_uuid() AS id FROM generate_series(1,1000)`,
			`INSERT INTO supervisor.knowledge_claim_heads(tenant_id,site_id,id,current_revision) SELECT $1,$2,id,1 FROM conflict_seed_ids`,
			`INSERT INTO supervisor.knowledge_claim_revisions(tenant_id,site_id,claim_id,revision,subject_id,predicate,object_entity_id,payload,actor_issuer,actor_subject) SELECT r.tenant_id,r.site_id,i.id,1,r.subject_id,r.predicate,r.object_entity_id,jsonb_set(r.payload,'{claim_id}',to_jsonb(i.id::text)),r.actor_issuer,r.actor_subject FROM supervisor.knowledge_claim_revisions r CROSS JOIN conflict_seed_ids i WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.claim_id=$3 AND r.revision=1`,
			`INSERT INTO supervisor.knowledge_claim_entity_refs(tenant_id,site_id,claim_id,claim_revision,subject_id,subject_revision,object_entity_id,object_revision) SELECT r.tenant_id,r.site_id,i.id,1,r.subject_id,r.subject_revision,r.object_entity_id,r.object_revision FROM supervisor.knowledge_claim_entity_refs r CROSS JOIN conflict_seed_ids i WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.claim_id=$3 AND r.claim_revision=1`,
			`INSERT INTO supervisor.knowledge_claim_spans(tenant_id,site_id,claim_id,claim_revision,span_id,source_id,source_revision) SELECT r.tenant_id,r.site_id,i.id,1,r.span_id,r.source_id,r.source_revision FROM supervisor.knowledge_claim_spans r CROSS JOIN conflict_seed_ids i WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.claim_id=$3 AND r.claim_revision=1`,
		} {
			var args []any
			if strings.Contains(q, "$1") {
				args = []any{sc.TenantID, sc.SiteID}
			}
			if strings.Contains(q, "$3") {
				args = append(args, base.ClaimID)
			}
			if _, e = seed.ExecContext(ctx, q, args...); e != nil {
				t.Fatal(e)
			}
		}
		if e = seed.Commit(); e != nil {
			t.Fatal(e)
		}
		next := base
		next.ClaimID = uuid.NewString()
		next.Literal = &Literal{Type: "decimal", Value: json.RawMessage(`"2"`)}
		if _, e = s.CreateClaim(ctx, owner, sc, next, "over-budget", 0); !errors.Is(e, ErrTraversalLimit) {
			t.Fatalf("comparison budget %v", e)
		}
		if _, e = s.ReadClaim(ctx, owner, sc, next.ClaimID, 0); !errors.Is(e, ErrNotFound) {
			t.Fatalf("over-budget claim persisted %v", e)
		}
		next.EffectiveFrom = end
		next.EffectiveTo = nil
		create(next, 0)
		list(next, "")
	})
	// A superseded exact revision no longer presents its immutable comparisons as active.
	tracked.Literal = &Literal{Type: "decimal", Value: json.RawMessage(`"13"`), Unit: "units/h"}
	create(tracked, 1)
	list(tracked, "")
	old, err := s.ListConflicts(ctx, owner, sc, tracked.ClaimID, 1, 100, "")
	if err != nil || len(old.Conflicts) != 0 {
		t.Fatalf("historical active pairs %+v %v", old, err)
	}
	// Failed audit must suppress the response; creation rollback must also remove conflict/receipt/head.
	subject := entity("operation")
	a := claim(subject, "actual_step", &Literal{Type: "string", Value: json.RawMessage(`"a"`)}, "")
	b := claim(subject, "actual_step", &Literal{Type: "string", Value: json.RawMessage(`"b"`)}, "")
	create(a, 0)
	blocker, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = blocker.ExecContext(ctx, `LOCK TABLE supervisor.knowledge_audit IN ACCESS EXCLUSIVE MODE`); err != nil {
		blocker.Rollback()
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(ctx, 150*time.Millisecond)
	_, err = s.CreateClaim(short, owner, sc, b, "audit-retry", 0)
	stop()
	blocker.Rollback()
	if err == nil {
		t.Fatal("blocked audit committed")
	}
	if _, err = s.ReadClaim(ctx, owner, sc, b.ClaimID, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed mutation persisted %v", err)
	}
	if _, err = s.CreateClaim(ctx, owner, sc, b, "audit-retry", 0); err != nil {
		t.Fatal(err)
	}
	list(a, "text_review")
	src.Text = "Revised synthetic source"
	if _, err = ev.CreateOrRevise(ctx, ea, es, src, "revise", 1); err != nil {
		t.Fatal(err)
	}
	list(a, "")
	if _, err = ev.Revoke(ctx, ea, es, src.SourceID, "Withdraw source", "revoke", 2); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ListConflicts(ctx, owner, sc, a.ClaimID, 0, 100, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked target leaked %v", err)
	}
}
