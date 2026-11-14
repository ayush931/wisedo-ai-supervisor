//go:build integration

package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestWIS626EvidenceSpanSearchMigration applies 073 over a database that already holds spans in
// two tenants, as the non-bypass migrator. It checks four things:
//   - every existing span is backfilled with the lexemes and byte size of its own text;
//   - a span written afterwards by the runtime role gets its row from the trigger;
//   - the runtime role can neither write nor change a search row itself;
//   - under forced RLS a tenant never sees another tenant's search rows.
func TestWIS626EvidenceSpanSearchMigration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := migratorDB(t)
	if err := applyThrough(ctx, db, 72); err != nil {
		t.Fatal(err)
	}
	type tenantSite struct{ tenant, site, source string }
	in := func(scope string, fn func(*sql.Tx) error) {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, scope); err != nil {
			t.Fatal(err)
		}
		if err = fn(tx); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	const text = "The curing oven runs at 180 degrees. Operators check the conveyor every hour."
	seed := func(name string) tenantSite {
		s := tenantSite{uuid.NewString(), uuid.NewString(), uuid.NewString()}
		sum := sha256.Sum256([]byte(text))
		in(s.tenant, func(tx *sql.Tx) error {
			for _, q := range []struct {
				sql  string
				args []any
			}{
				{`INSERT INTO supervisor.tenants(id,name) VALUES($1,$2)`, []any{s.tenant, name}},
				{`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'site')`, []any{s.tenant, s.site}},
				{`INSERT INTO supervisor.evidence_heads(tenant_id,site_id,source_id,current_revision,state_revision) VALUES($1,$2,$3,1,1)`, []any{s.tenant, s.site, s.source}},
				{`INSERT INTO supervisor.evidence_revisions(tenant_id,site_id,source_id,revision,kind,title,body,sha256,byte_length,rune_length,actor_issuer,actor_subject) VALUES($1,$2,$3,1,'document','SOP',$4,$5,$6,$6,'i','owner')`,
					[]any{s.tenant, s.site, s.source, text, hex.EncodeToString(sum[:]), len(text)}},
			} {
				if _, err := tx.ExecContext(ctx, q.sql, q.args...); err != nil {
					return err
				}
			}
			return nil
		})
		return s
	}
	span := func(tx *sql.Tx, s tenantSite, start, end int) (string, error) {
		id := uuid.NewString()
		_, err := tx.ExecContext(ctx, `INSERT INTO supervisor.evidence_spans(tenant_id,site_id,id,source_id,revision,start_rune,end_rune,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,1,$5,$6,'i','owner')`, s.tenant, s.site, id, s.source, start, end)
		return id, err
	}
	a, b := seed("search tenant A"), seed("search tenant B")
	var oldA, oldB string
	in(a.tenant, func(tx *sql.Tx) (err error) { oldA, err = span(tx, a, 0, 36); return })
	in(b.tenant, func(tx *sql.Tx) (err error) { oldB, err = span(tx, b, 37, 77); return })

	if err := applyThrough(ctx, db, 73); err != nil {
		t.Fatal(err)
	}

	// expect reads one search row as tenant scope and compares it with to_tsvector of the span
	// text computed here, independently of the trigger.
	expect := func(conn *sql.DB, scope, id string, start, end int) {
		t.Helper()
		var bytes, want int
		var got, lexemes []string
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, scope); err != nil {
			t.Fatal(err)
		}
		err = tx.QueryRowContext(ctx, `SELECT x.text_bytes,x.lexemes,tsvector_to_array(to_tsvector('english',substring($2::text FROM $3::int+1 FOR $4::int))),octet_length(substring($2::text FROM $3::int+1 FOR $4::int)) FROM supervisor.evidence_span_search x WHERE x.span_id=$1`,
			id, text, start, end-start).Scan(&bytes, sliceScanner{&got}, sliceScanner{&lexemes}, &want)
		if err != nil {
			t.Fatalf("search row %s: %v", id, err)
		}
		if bytes != want || !slices.Equal(got, lexemes) || len(got) == 0 {
			t.Fatalf("span %s: bytes %d want %d, lexemes %v want %v", id, bytes, want, got, lexemes)
		}
	}
	expect(db, a.tenant, oldA, 0, 36)
	expect(db, b.tenant, oldB, 37, 77)

	runtime := runtimeDB(t, db)
	var newA string
	tx, err := runtime.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, a.tenant); err != nil {
		t.Fatal(err)
	}
	if newA, err = span(tx, a, 37, 77); err != nil {
		t.Fatalf("runtime span insert: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	expect(runtime, a.tenant, newA, 37, 77)

	// The runtime role reads search rows but can never write or change one (42501).
	for name, q := range map[string]string{
		"insert": `INSERT INTO supervisor.evidence_span_search(tenant_id,site_id,span_id,source_id,revision,text_bytes,search) VALUES($1,$2,$3,$4,1,5,to_tsvector('english','forged'))`,
		"update": `UPDATE supervisor.evidence_span_search SET search=to_tsvector('english','forged') WHERE tenant_id=$1 AND site_id=$2 AND span_id=$3 AND source_id=$4`,
	} {
		tx, err := runtime.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, a.tenant); err != nil {
			t.Fatal(err)
		}
		_, err = tx.ExecContext(ctx, q, a.tenant, a.site, newA, a.source)
		_ = tx.Rollback()
		if sqlState(err) != "42501" {
			t.Fatalf("runtime %s: %v", name, err)
		}
	}

	// Rule 03: tenant A's scope sees none of tenant B's rows, even by exact ID.
	tx, err = runtime.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, a.tenant); err != nil {
		t.Fatal(err)
	}
	var visible, foreign int
	if err = tx.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER (WHERE tenant_id=$1) FROM supervisor.evidence_span_search WHERE span_id=ANY($2::uuid[])`, b.tenant, []string{oldA, oldB, newA}).Scan(&visible, &foreign); err != nil {
		t.Fatal(err)
	}
	if visible != 2 || foreign != 0 {
		t.Fatalf("tenant A sees %d rows, %d of tenant B", visible, foreign)
	}
}

// sliceScanner scans a PostgreSQL text[] into a []string.
type sliceScanner struct{ dst *[]string }

func (s sliceScanner) Scan(src any) error {
	var raw string
	switch v := src.(type) {
	case string:
		raw = v
	case []byte:
		raw = string(v)
	}
	*s.dst = parseTextArray(raw)
	return nil
}

// parseTextArray reads the simple PostgreSQL array literal of plain lexemes ({a,b,"c d"}).
func parseTextArray(raw string) []string {
	if len(raw) < 2 {
		return nil
	}
	var out []string
	cur, quoted := []rune{}, false
	for _, r := range raw[1 : len(raw)-1] {
		switch {
		case r == '"':
			quoted = !quoted
		case r == ',' && !quoted:
			out = append(out, string(cur))
			cur = cur[:0]
		default:
			cur = append(cur, r)
		}
	}
	if len(cur) > 0 || len(out) > 0 {
		out = append(out, string(cur))
	}
	return out
}
