//go:build integration

package workstation

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
)

// Review of 704428f (WIS-625): an instruction document whose model-extracted revision was
// designated without the owner's confirmation (any designation before migration 077) is not an
// approved instruction, now or historically. A confirmed designation of the same kind of
// revision still is, so the predicate is not merely refusing OCR text.
func TestInstructionRequiresConfirmedOCRDesignationPostgres(t *testing.T) {
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: actual disposable PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	control, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	name := "ocr_instr_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = control.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	defer control.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	in := func(raw string) string {
		u, e := url.Parse(raw)
		if e != nil {
			t.Fatal(e)
		}
		u.Path = "/" + name
		return u.String()
	}
	admin, err := sql.Open("pgx", in(adminURL))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err = migrate.Apply(ctx, admin); err != nil {
		t.Fatal(err)
	}
	runtime, err := sql.Open("pgx", in(runtimeURL))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	tenant, site, run := uuid.NewString(), uuid.NewString(), uuid.NewString()
	legacy, confirmed := uuid.NewString(), uuid.NewString()
	seed, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Rollback()
	steps := []struct {
		q    string
		args []any
	}{
		{`SELECT set_config('supervisor.tenant_id',$1,true)`, []any{tenant}},
		{`INSERT INTO supervisor.tenants(id,name) VALUES($1,'ocr instructions')`, []any{tenant}},
		{`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'site')`, []any{tenant, site}},
		{`INSERT INTO supervisor.agent_runs(tenant_id,site_id,id,request_kind,conversation_id,request_id,trace_id,provider_name,provider_actual,model,model_revision,prompt_version,tool_version,policy_revision,invocations_left,tool_calls_left,requests_left,tokens_left,cost_left_microusd)
 VALUES($1,$2,$3,'extraction',gen_random_uuid(),gen_random_uuid(),$4,'fixture',false,'vision','r','p','t',1,1,1,1,1,1)`, []any{tenant, site, run, strings.Repeat("a", 32)}},
	}
	for _, s := range []struct {
		id      string
		confirm bool
	}{{legacy, false}, {confirmed, true}} {
		steps = append(steps, []struct {
			q    string
			args []any
		}{
			{`INSERT INTO supervisor.evidence_heads(tenant_id,site_id,source_id,current_revision,state_revision) VALUES($1,$2,$3,1,1)`, []any{tenant, site, s.id}},
			{`INSERT INTO supervisor.evidence_revisions(tenant_id,site_id,source_id,revision,kind,title,body,sha256,byte_length,rune_length,actor_issuer,actor_subject,extraction,extractor_model,extractor_model_revision,extractor_prompt_sha256,extraction_run_id)
 VALUES($1,$2,$3,1,'document','SOP','sop',encode(sha256('sop'),'hex'),3,3,'i','s','model_ocr','vision','r',repeat('b',64),$4)`, []any{tenant, site, s.id, run}},
			{`INSERT INTO supervisor.evidence_authority_heads(tenant_id,site_id,source_id,current_revision) VALUES($1,$2,$3,1)`, []any{tenant, site, s.id}},
			{`INSERT INTO supervisor.evidence_authority_decisions(tenant_id,site_id,source_id,revision,decision_id,source_revision,source_sha256,source_access_epoch,verdict,reason,actor_issuer,actor_subject,auth_time,model_extracted_confirmed)
 VALUES($1,$2,$3,1,$3,1,encode(sha256('sop'),'hex'),1,'designate','owner','i','s',now(),$4)`, []any{tenant, site, s.id, s.confirm}},
			{`INSERT INTO supervisor.evidence_currentness_heads(tenant_id,site_id,source_id,current_revision) VALUES($1,$2,$3,1)`, []any{tenant, site, s.id}},
			{`INSERT INTO supervisor.evidence_currentness_decisions(tenant_id,site_id,source_id,revision,verification_id,source_revision,source_sha256,source_access_epoch,authority_revision,authority_decision_id,verdict,reason,actor_issuer,actor_subject,auth_time,created_at)
 VALUES($1,$2,$3,1,gen_random_uuid(),1,encode(sha256('sop'),'hex'),1,1,$3,'confirm_current','current','i','s',now(),now())`, []any{tenant, site, s.id}},
		}...)
	}
	for _, s := range steps {
		if _, err = seed.ExecContext(ctx, s.q, s.args...); err != nil {
			t.Fatalf("%s: %v", s.q, err)
		}
	}
	if err = seed.Commit(); err != nil {
		t.Fatal(err)
	}

	tx, err := runtime.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var at time.Time
	if err = tx.QueryRowContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)::text IS NOT NULL,clock_timestamp()`, tenant).Scan(new(bool), &at); err != nil {
		t.Fatal(err)
	}
	sc := Scope{TenantID: tenant, SiteID: site}
	var digest string
	if err = tx.QueryRowContext(ctx, `SELECT encode(sha256('sop'),'hex')`).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name     string
		id       string
		approved bool
		reason   InstructionReason
	}{
		{"unconfirmed OCR designation", legacy, false, InstructionNotConfirmed},
		{"confirmed OCR designation", confirmed, true, ""},
	} {
		ref := InstructionRefV1{DocumentID: c.id, Revision: 1, SHA256: digest}
		approved, err := instructionApproved(ctx, tx, sc, ref)
		if err != nil || approved != c.approved {
			t.Fatalf("%s: instructionApproved=%v err=%v, want %v", c.name, approved, err, c.approved)
		}
		reason, err := instructionReasonAt(ctx, tx, sc, ref, at)
		if err != nil || reason != c.reason {
			t.Fatalf("%s: instructionReasonAt=%q err=%v, want %q", c.name, reason, err, c.reason)
		}
	}
}
