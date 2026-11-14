//go:build integration

package evidence

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// WIS-270 "repeated source replay": copies of the same bytes are one independence group that a
// caller cannot split, a group never crosses a site or tenant, and the columns are immutable.
func TestPostgresWIS270SourceIndependence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL admin/runtime URLs required")
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
	otherSite := Scope{sc.TenantID, uuid.NewString()}
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'wis270 independence'),($2,'wis270 foreign')`, sc.TenantID, foreign.TenantID)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'a'),($1,$3,'b'),($4,$5,'c')`, sc.TenantID, sc.SiteID, otherSite.SiteID, foreign.TenantID, foreign.SiteID)
	owner := Actor{"https://independence.invalid", uuid.NewString(), hash([]byte(uuid.NewString()))}
	exec(`INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'synthetic',now(),now(),now()+interval '1 hour',now())`, owner.SessionHash, owner.Issuer, owner.Subject)
	for _, s := range []Scope{sc, otherSite, foreign} {
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, s.TenantID, s.SiteID, owner.Issuer, owner.Subject)
	}
	s := New(runtime)
	create := func(scope Scope, in Input, expected int64) (Source, error) {
		return s.CreateOrRevise(ctx, owner, scope, in, uuid.NewString(), expected)
	}
	must := func(scope Scope, in Input, expected int64) Source {
		t.Helper()
		out, e := create(scope, in, expected)
		if e != nil {
			t.Fatal(e)
		}
		return out
	}
	sop := "SOP-7: torque to 40 Nm, then inspect " + uuid.NewString()
	original := must(sc, Input{SourceID: uuid.NewString(), Kind: "document", Title: "SOP-7", Text: sop}, 0)
	if original.GroupID != original.SourceID || original.AuthorityClass != "reported" {
		t.Fatalf("first source must found its own reported group: %+v", original)
	}
	independent := must(sc, Input{SourceID: uuid.NewString(), Kind: "event", Title: "torque log", Text: "torque 40.1 Nm " + uuid.NewString(), AuthorityClass: "observed"}, 0)

	t.Run("identical bytes join the first group whatever the source id", func(t *testing.T) {
		copied := must(sc, Input{SourceID: uuid.NewString(), Kind: "interview", Title: "operator quoting SOP", Text: sop}, 0)
		if copied.GroupID != original.SourceID {
			t.Fatalf("replayed bytes formed group %s, want %s", copied.GroupID, original.SourceID)
		}
		// The caller cannot split a copy into an independent group.
		_, e := create(sc, Input{SourceID: uuid.NewString(), Kind: "document", Title: "copy", Text: sop, IndependenceGroupID: independent.SourceID}, 0)
		if !errors.Is(e, ErrInvalid) {
			t.Fatalf("split replay error=%v want ErrInvalid", e)
		}
		explicit := must(sc, Input{SourceID: uuid.NewString(), Kind: "document", Title: "copy", Text: sop, IndependenceGroupID: original.SourceID}, 0)
		if explicit.GroupID != original.SourceID {
			t.Fatalf("explicit matching group=%s", explicit.GroupID)
		}
	})
	t.Run("caller groups must exist in this site", func(t *testing.T) {
		paraphrase := must(sc, Input{SourceID: uuid.NewString(), Kind: "interview", Title: "paraphrase", Text: "Torque per SOP-7 " + uuid.NewString(), IndependenceGroupID: original.SourceID}, 0)
		if paraphrase.GroupID != original.SourceID {
			t.Fatalf("declared group not kept: %s", paraphrase.GroupID)
		}
		elsewhere := must(otherSite, Input{SourceID: uuid.NewString(), Kind: "document", Title: "b", Text: "site b " + uuid.NewString()}, 0)
		abroad := must(foreign, Input{SourceID: uuid.NewString(), Kind: "document", Title: "c", Text: "tenant c " + uuid.NewString()}, 0)
		for name, group := range map[string]string{"other site": elsewhere.SourceID, "other tenant": abroad.SourceID, "unknown": uuid.NewString()} {
			if _, e := create(sc, Input{SourceID: uuid.NewString(), Kind: "manual", Title: "x", Text: name + uuid.NewString(), IndependenceGroupID: group}, 0); !errors.Is(e, ErrNotFound) {
				t.Errorf("%s group error=%v want ErrNotFound", name, e)
			}
		}
		// Identical bytes in another site never join this site's group (no cross-site dedup).
		mirrored := must(otherSite, Input{SourceID: uuid.NewString(), Kind: "document", Title: "SOP-7", Text: sop}, 0)
		if mirrored.GroupID != mirrored.SourceID {
			t.Fatalf("cross-site hash dedup: %+v", mirrored)
		}
	})
	t.Run("a revision keeps its source group and class is per revision", func(t *testing.T) {
		revised := must(sc, Input{SourceID: independent.SourceID, Kind: "event", Title: "torque log", Text: "torque 39.9 Nm " + uuid.NewString()}, 1)
		if revised.Revision != 2 || revised.GroupID != independent.SourceID || revised.AuthorityClass != "reported" {
			t.Fatalf("revision group/class: %+v", revised)
		}
		first, e := s.Read(ctx, owner, sc, independent.SourceID, 1)
		if e != nil || first.AuthorityClass != "observed" {
			t.Fatalf("revision 1 class changed: %+v %v", first, e)
		}
	})
	t.Run("concurrent identical uploads land in one group", func(t *testing.T) {
		text := "Concurrent SOP " + uuid.NewString()
		type result struct {
			out Source
			err error
		}
		results := make(chan result, 2)
		for i := 0; i < 2; i++ {
			go func() {
				out, e := create(sc, Input{SourceID: uuid.NewString(), Kind: "document", Title: "race", Text: text}, 0)
				results <- result{out, e}
			}()
		}
		a, b := <-results, <-results
		if a.err != nil || b.err != nil {
			t.Fatalf("concurrent create errors: %v / %v", a.err, b.err)
		}
		if a.out.GroupID != b.out.GroupID || (a.out.GroupID != a.out.SourceID && a.out.GroupID != b.out.SourceID) {
			t.Fatalf("concurrent identical bytes split: %s vs %s", a.out.GroupID, b.out.GroupID)
		}
	})
	t.Run("runtime cannot rewrite group or class", func(t *testing.T) {
		for _, q := range []string{
			`UPDATE supervisor.evidence_revisions SET independence_group_id=$2 WHERE tenant_id=$1 AND source_id=$2`,
			`UPDATE supervisor.evidence_revisions SET authority_class='approved_document' WHERE tenant_id=$1 AND source_id=$2`,
		} {
			tx, e := runtime.BeginTx(ctx, nil)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); e != nil {
				t.Fatal(e)
			}
			_, e = tx.ExecContext(ctx, q, sc.TenantID, independent.SourceID)
			_ = tx.Rollback()
			var pg *pgconn.PgError
			if !errors.As(e, &pg) || pg.Code != "42501" {
				t.Fatalf("runtime UPDATE error=%v want 42501", e)
			}
		}
	})
}
