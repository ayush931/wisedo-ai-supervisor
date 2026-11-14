//go:build integration

package migrate

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// WIS-270: 039 backfills a populated 031 database as the non-superuser migration owner. Every
// existing source becomes its own independence group; a revision pinned by an owner 'designate'
// decision becomes approved_document, every other revision reported.
func TestSourceIndependenceBackfillFrom031(t *testing.T) {
	db := migratorDB(t)
	ctx := context.Background()
	if err := applyThrough(ctx, db, 31); err != nil {
		t.Fatal(err)
	}
	tenant, site, designated, plain := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	sameBytes := "encode(sha256('same bytes'),'hex')"
	for _, step := range []struct {
		q    string
		args []any
	}{
		{`SELECT set_config('supervisor.tenant_id',$1,true)`, []any{tenant}},
		{`INSERT INTO supervisor.tenants(id,name) VALUES($1,'backfill')`, []any{tenant}},
		{`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'backfill')`, []any{tenant, site}},
		{`INSERT INTO supervisor.evidence_heads(tenant_id,site_id,source_id,current_revision,state_revision) VALUES($1,$2,$3,2,2),($1,$2,$4,1,1)`, []any{tenant, site, designated, plain}},
		// Two sources with identical bytes: pre-039 copies are not merged retroactively.
		{`INSERT INTO supervisor.evidence_revisions(tenant_id,site_id,source_id,revision,kind,title,body,sha256,byte_length,rune_length,actor_issuer,actor_subject) VALUES
 ($1,$2,$3,1,'document','SOP','same bytes',` + sameBytes + `,10,10,'i','s'),
 ($1,$2,$3,2,'document','SOP','revised bytes',encode(sha256('revised bytes'),'hex'),13,13,'i','s'),
 ($1,$2,$4,1,'manual','note','same bytes',` + sameBytes + `,10,10,'i','s')`, []any{tenant, site, designated, plain}},
		{`INSERT INTO supervisor.evidence_authority_heads(tenant_id,site_id,source_id,current_revision) VALUES($1,$2,$3,1)`, []any{tenant, site, designated}},
		{`INSERT INTO supervisor.evidence_authority_decisions(tenant_id,site_id,source_id,revision,decision_id,source_revision,source_sha256,source_access_epoch,verdict,reason,actor_issuer,actor_subject,auth_time) VALUES($1,$2,$3,1,gen_random_uuid(),1,` + sameBytes + `,1,'designate','owner verified','i','s',now())`, []any{tenant, site, designated}},
	} {
		if _, err = tx.ExecContext(ctx, step.q, step.args...); err != nil {
			t.Fatalf("%s: %v", step.q, err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = Apply(ctx, db); err != nil {
		t.Fatalf("upgrade populated 031 as the migration owner: %v", err)
	}
	if err = Check(ctx, db); err != nil {
		t.Fatal(err)
	}
	assertTenantTablesForced(t, db)
	read, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Rollback()
	if _, err = read.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		t.Fatal(err)
	}
	rows, err := read.QueryContext(ctx, `SELECT source_id,revision,authority_class,independence_group_id FROM supervisor.evidence_revisions WHERE tenant_id=$1 ORDER BY source_id=$2 DESC,revision`, tenant, designated)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		source       string
		revision     int64
		class, group string
	}
	var got []row
	for rows.Next() {
		var r row
		if err = rows.Scan(&r.source, &r.revision, &r.class, &r.group); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []row{
		{designated, 1, "approved_document", designated},
		{designated, 2, "reported", designated},
		{plain, 1, "reported", plain},
	}
	if len(got) != len(want) {
		t.Fatalf("backfilled rows=%+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d backfilled %+v want %+v", i, got[i], want[i])
		}
	}
	// The immutability trigger is back on after the backfill, even for the owner.
	_, err = read.ExecContext(ctx, `UPDATE supervisor.evidence_revisions SET authority_class='observed' WHERE tenant_id=$1`, tenant)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "42501" {
		t.Fatalf("owner UPDATE after 039 error=%v want 42501", err)
	}
}
