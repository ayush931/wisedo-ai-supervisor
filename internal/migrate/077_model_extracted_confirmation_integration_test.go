//go:build integration

package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Review 254 P2 and the 704428f re-review (WIS-625): 077 upgrades a populated 076 database as the
// non-superuser migration owner. Every earlier designation reads as unconfirmed; a revision the
// old code re-saved from OCR text without its extraction gets that extraction back; and a site
// holding model-extracted text advances its knowledge inventory, so its snapshots read as stale.
// A site with only typed text is untouched.
func TestModelExtractedConfirmationUpgradeFrom076(t *testing.T) {
	db := migratorDB(t)
	ctx := context.Background()
	if err := applyThrough(ctx, db, 76); err != nil {
		t.Fatal(err)
	}
	tenant, ocrSite, typedSite, run, ocr, typed := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	sop := "encode(sha256('sop'),'hex')"
	for _, step := range []struct {
		q    string
		args []any
	}{
		{`SELECT set_config('supervisor.tenant_id',$1,true)`, []any{tenant}},
		{`INSERT INTO supervisor.tenants(id,name) VALUES($1,'upgrade')`, []any{tenant}},
		{`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'ocr'),($1,$3,'typed')`, []any{tenant, ocrSite, typedSite}},
		{`INSERT INTO supervisor.agent_runs(tenant_id,site_id,id,request_kind,conversation_id,request_id,trace_id,provider_name,provider_actual,model,model_revision,prompt_version,tool_version,policy_revision,invocations_left,tool_calls_left,requests_left,tokens_left,cost_left_microusd)
 VALUES($1,$2,$3,'extraction',gen_random_uuid(),gen_random_uuid(),$4,'fixture',false,'vision','r','p','t',1,1,1,1,1,1)`, []any{tenant, ocrSite, run, strings.Repeat("a", 32)}},
		{`INSERT INTO supervisor.evidence_heads(tenant_id,site_id,source_id,current_revision,state_revision) VALUES($1,$2,$3,2,2),($1,$4,$5,1,1)`, []any{tenant, ocrSite, ocr, typedSite, typed}},
		// The OCR page, then the title-only re-save the pre-fix code stored without its extraction.
		{`INSERT INTO supervisor.evidence_revisions(tenant_id,site_id,source_id,revision,kind,title,body,sha256,byte_length,rune_length,actor_issuer,actor_subject,independence_group_id,extraction,extractor_model,extractor_model_revision,extractor_prompt_sha256,extraction_run_id)
 VALUES($1,$2,$3,1,'document','Scan','sop',` + sop + `,3,3,'i','s',$3,'model_ocr','vision','r',repeat('b',64),$4)`, []any{tenant, ocrSite, ocr, run}},
		{`INSERT INTO supervisor.evidence_revisions(tenant_id,site_id,source_id,revision,kind,title,body,sha256,byte_length,rune_length,actor_issuer,actor_subject,independence_group_id) VALUES
 ($1,$2,$3,2,'document','Renamed','sop',` + sop + `,3,3,'i','s',$3),($1,$4,$5,1,'document','Typed','sop',` + sop + `,3,3,'i','s',$5)`, []any{tenant, ocrSite, ocr, typedSite, typed}},
		{`INSERT INTO supervisor.evidence_authority_heads(tenant_id,site_id,source_id,current_revision) VALUES($1,$2,$3,1),($1,$4,$5,1)`, []any{tenant, ocrSite, ocr, typedSite, typed}},
		{`INSERT INTO supervisor.evidence_authority_decisions(tenant_id,site_id,source_id,revision,decision_id,source_revision,source_sha256,source_access_epoch,verdict,reason,actor_issuer,actor_subject,auth_time) VALUES
 ($1,$2,$3,1,gen_random_uuid(),2,` + sop + `,1,'designate','owner verified','i','s',now()),($1,$4,$5,1,gen_random_uuid(),1,` + sop + `,1,'designate','owner verified','i','s',now())`, []any{tenant, ocrSite, ocr, typedSite, typed}},
	} {
		if _, err = tx.ExecContext(ctx, step.q, step.args...); err != nil {
			t.Fatalf("%s: %v", step.q, err)
		}
	}
	inventory := func(q interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}, site string) int64 {
		t.Helper()
		var n int64
		if e := q.QueryRowContext(ctx, `SELECT revision FROM supervisor.knowledge_inventory_revisions WHERE tenant_id=$1 AND site_id=$2`, tenant, site).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	ocrBefore, typedBefore := inventory(tx, ocrSite), inventory(tx, typedSite)
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = Apply(ctx, db); err != nil {
		t.Fatalf("upgrade populated 076 as the migration owner: %v", err)
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
	var confirmed int
	if err = read.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.evidence_authority_decisions WHERE tenant_id=$1 AND model_extracted_confirmed`, tenant).Scan(&confirmed); err != nil {
		t.Fatal(err)
	}
	if confirmed != 0 {
		t.Fatalf("%d pre-077 designations read as confirmed", confirmed)
	}
	rows, err := read.QueryContext(ctx, `SELECT source_id=$2,revision,coalesce(extraction,''),coalesce(extractor_model,''),coalesce(extraction_run_id::text,'') FROM supervisor.evidence_revisions WHERE tenant_id=$1 ORDER BY source_id=$2 DESC,revision`, tenant, ocr)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		ocr                   bool
		revision              int64
		extraction, model, id string
	}
	var got []row
	for rows.Next() {
		var r row
		if err = rows.Scan(&r.ocr, &r.revision, &r.extraction, &r.model, &r.id); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []row{{true, 1, "model_ocr", "vision", run}, {true, 2, "model_ocr", "vision", run}, {false, 1, "", "", ""}}
	if len(got) != len(want) {
		t.Fatalf("revisions after 077: %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("revision %d after 077: %+v want %+v", i, got[i], want[i])
		}
	}
	if o, ty := inventory(read, ocrSite), inventory(read, typedSite); o != ocrBefore+1 || ty != typedBefore {
		t.Fatalf("inventory after 077: OCR site %d (was %d), typed site %d (was %d)", o, ocrBefore, ty, typedBefore)
	}
}
