//go:build integration

package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/google/uuid"
)

func TestInterviewSessionsUpgradeFrom047AndRuntimeRLS(t *testing.T) {
	raw := os.Getenv("SUPERVISOR_TEST_ADMIN_URL")
	if raw == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL admin URL required")
	}
	root, err := sql.Open("pgx", raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	name := "interviews_migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = root.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, e := root.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	})
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	admin, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	ctx := context.Background()
	if err = applyThrough(ctx, admin, 47); err != nil {
		t.Fatal(err)
	}
	tenant, site := createSite(t, admin)
	var siteCount int
	if err = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.sites WHERE tenant_id=$1 AND id=$2`, tenant, site).Scan(&siteCount); err != nil {
		t.Fatal(err)
	}
	if siteCount != 1 {
		t.Fatalf("populated 047 site was not preserved: count=%d", siteCount)
	}
	if err = Apply(ctx, admin); err != nil {
		t.Fatalf("upgrade populated 047 database to 048: %v", err)
	}
	if err = Check(ctx, admin); err != nil {
		t.Fatal(err)
	}
	assertTenantTablesForced(t, admin)
	assertGrants(t, admin, appendOnly, "interview_answers", "interview_receipts", "interview_audit")
	if got := runtimeGrants(t, admin, "interview_sessions"); !got.Forced || got.RuntimeOwns || !got.Select || !got.Insert || got.Update || got.Delete || got.TenantPolicies != 1 {
		t.Fatalf("interview_sessions runtime grants = %+v", got)
	}
	if got := runtimeGrants(t, admin, "interview_questions"); !got.Forced || got.RuntimeOwns || !got.Select || !got.Insert || got.Update || got.Delete || got.TenantPolicies != 1 {
		t.Fatalf("interview_questions runtime grants = %+v", got)
	}
	var sessionUpdate, questionUpdate string
	if err = admin.QueryRowContext(ctx, `SELECT string_agg(column_name,',' ORDER BY column_name) FROM information_schema.column_privileges WHERE grantee='supervisor_runtime' AND table_schema='supervisor' AND table_name='interview_sessions' AND privilege_type='UPDATE'`).Scan(&sessionUpdate); err != nil {
		t.Fatal(err)
	}
	if err = admin.QueryRowContext(ctx, `SELECT string_agg(column_name,',' ORDER BY column_name) FROM information_schema.column_privileges WHERE grantee='supervisor_runtime' AND table_schema='supervisor' AND table_name='interview_questions' AND privilege_type='UPDATE'`).Scan(&questionUpdate); err != nil {
		t.Fatal(err)
	}
	if sessionUpdate != "revision,status,updated_at" || questionUpdate != "answer_source_id,answer_state,revision,updated_at" {
		t.Fatalf("unexpected runtime update grants: sessions=%q questions=%q", sessionUpdate, questionUpdate)
	}

	actor, issuer, subject := uuid.NewString(), "https://interview-migration.invalid", uuid.NewString()
	foreignTenant, foreignSite := createSite(t, admin)
	if _, err = admin.ExecContext(ctx, `INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner'),($5,$6,$3,$4,'owner')`, tenant, site, issuer, subject, foreignTenant, foreignSite); err != nil {
		t.Fatal(err)
	}
	seedForeignInterviewRows(t, ctx, admin, foreignTenant, foreignSite, issuer, subject)
	runtime := runtimeDB(t, admin)
	tx, err := runtime.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	setTenant(t, tx, tenant)
	first, second := uuid.NewString(), uuid.NewString()
	insert := `INSERT INTO supervisor.interview_sessions(tenant_id,site_id,session_id,role,participant_actor_id,participant_issuer,participant_subject,status,revision) VALUES($1,$2,$3,'owner',$4,$5,$6,'active',1)`
	if _, err = tx.ExecContext(ctx, insert, tenant, site, first, actor, issuer, subject); err != nil {
		t.Fatalf("runtime insert in tenant scope: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	foreign, err := runtime.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Rollback()
	setTenant(t, foreign, tenant)
	if _, err = foreign.ExecContext(ctx, insert, foreignTenant, foreignSite, second, actor, issuer, subject); sqlState(err) != "42501" {
		t.Fatalf("cross-tenant interview insert was not denied: %v", err)
	}
	_ = foreign.Rollback()
	foreign, err = runtime.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Rollback()
	setTenant(t, foreign, tenant)
	for _, table := range []string{"interview_sessions", "interview_questions", "interview_answers"} {
		var seeded, visible int
		if err = admin.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.`+table+` WHERE tenant_id=$1 AND site_id=$2`, foreignTenant, foreignSite).Scan(&seeded); err != nil || seeded == 0 {
			t.Fatalf("admin foreign %s seed count=%d err=%v", table, seeded, err)
		}
		if err = foreign.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.`+table+` WHERE tenant_id=$1`, foreignTenant).Scan(&visible); err != nil || visible != 0 {
			t.Fatalf("runtime cross-tenant %s read count=%d err=%v", table, visible, err)
		}
	}
	if _, err = foreign.ExecContext(ctx, `UPDATE supervisor.interview_sessions SET status='paused',revision=revision+1,updated_at=clock_timestamp() WHERE tenant_id=$1 AND site_id=$2 AND session_id=$3`, tenant, site, first); err != nil {
		t.Fatalf("valid active to paused transition: %v", err)
	}
	if _, err = foreign.ExecContext(ctx, `UPDATE supervisor.interview_sessions SET status='paused',revision=revision+1,updated_at=clock_timestamp() WHERE tenant_id=$1 AND site_id=$2 AND session_id=$3`, tenant, site, first); sqlState(err) != "23514" {
		t.Fatalf("invalid same-state transition was not rejected: %v", err)
	}
}

func seedForeignInterviewRows(t *testing.T, ctx context.Context, db *sql.DB, tenant, site, issuer, subject string) {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		t.Fatal(err)
	}
	sourceID, spanID, areaID, processID, inventoryID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	body := "migration interview evidence"
	hash := sha256.Sum256([]byte(body))
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.evidence_heads(tenant_id,site_id,source_id,current_revision,state_revision) VALUES($1,$2,$3,1,1)`, tenant, site, sourceID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.evidence_revisions(tenant_id,site_id,source_id,revision,kind,title,body,sha256,byte_length,rune_length,actor_issuer,actor_subject,authority_class,independence_group_id) VALUES($1,$2,$3,1,'manual','Migration interview evidence',$4,$5,$6,$6,$7,$8,'reported',$3)`, tenant, site, sourceID, body, hex.EncodeToString(hash[:]), len(body), issuer, subject); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.evidence_spans(tenant_id,site_id,id,source_id,revision,start_rune,end_rune,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,1,0,$5,$6,$7)`, tenant, site, spanID, sourceID, len(body), issuer, subject); err != nil {
		t.Fatal(err)
	}
	for _, entity := range []struct{ id, kind, name string }{{areaID, "area", "Migration area"}, {processID, "process", "Migration process"}} {
		payload, e := json.Marshal(map[string]any{"entity_id": entity.id, "type": entity.kind, "display_name": entity.name, "source_id": sourceID, "source_revision": 1})
		if e != nil {
			t.Fatal(e)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_entities(tenant_id,site_id,id,entity_type,payload,source_id,source_revision,state,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,$6,1,'candidate',$7,$8)`, tenant, site, entity.id, entity.kind, payload, sourceID, issuer, subject); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_entity_heads(tenant_id,site_id,id,current_revision) VALUES($1,$2,$3,1)`, tenant, site, entity.id); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_entity_revisions(tenant_id,site_id,id,revision,entity_type,payload,source_id,source_revision,state,actor_issuer,actor_subject) VALUES($1,$2,$3,1,$4,$5,$6,1,'candidate',$7,$8)`, tenant, site, entity.id, entity.kind, payload, sourceID, issuer, subject); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_discovery_inventories(tenant_id,site_id,inventory_id,area_id,process_id,capability) VALUES($1,$2,$3,$4,$5,'shortfall_investigation')`, tenant, site, inventoryID, areaID, processID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_discovery_heads(tenant_id,site_id,inventory_id,current_revision) VALUES($1,$2,$3,1)`, tenant, site, inventoryID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_discovery_revisions(tenant_id,site_id,inventory_id,revision,template_version,area_id,area_revision,process_id,process_revision,capability,reason,actor_issuer,actor_subject,auth_time) VALUES($1,$2,$3,1,1,$4,1,$5,1,'shortfall_investigation','RLS fixture',$6,$7,clock_timestamp())`, tenant, site, inventoryID, areaID, processID, issuer, subject); err != nil {
		t.Fatal(err)
	}
	template, err := knowledge.DiscoveryTemplate("shortfall_investigation")
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range template {
		id := uuid.NewString()
		value, e := json.Marshal(definition)
		if e != nil {
			t.Fatal(e)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_discovery_requirements(tenant_id,site_id,inventory_id,revision,requirement_key,requirement_id,definition) VALUES($1,$2,$3,1,$4,$5,$6)`, tenant, site, inventoryID, definition.Key, id, value); err != nil {
			t.Fatal(err)
		}
	}
	var requirementID string
	if err = tx.QueryRowContext(ctx, `SELECT requirement_id::text FROM supervisor.knowledge_discovery_requirements WHERE tenant_id=$1 AND site_id=$2 AND inventory_id=$3 ORDER BY requirement_key LIMIT 1`, tenant, site, inventoryID).Scan(&requirementID); err != nil {
		t.Fatal(err)
	}
	sessionID, questionID, answerID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.interview_sessions(tenant_id,site_id,session_id,role,participant_actor_id,participant_issuer,participant_subject,process_id,inventory_id,inventory_revision,status,revision) VALUES($1,$2,$3,'owner',$4,$5,$6,$7,$8,1,'active',1)`, tenant, site, sessionID, uuid.NewString(), issuer, subject, processID, inventoryID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.interview_questions(tenant_id,site_id,session_id,question_id,inventory_id,inventory_revision,requirement_id,evidence_digest,role,prompt,reason,answer_state,revision) VALUES($1,$2,$3,$4,$5,1,$6,$7,'owner','Foreign question','missing','unanswered',1)`, tenant, site, sessionID, questionID, inventoryID, requirementID, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.interview_answers(tenant_id,site_id,session_id,question_id,revision,question_revision,client_answer_id,source_id,source_revision,source_span_id,request_sha256) VALUES($1,$2,$3,$4,1,1,$5,$6,1,$7,$8)`, tenant, site, sessionID, questionID, answerID, sourceID, spanID, strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE supervisor.interview_questions SET answer_state='answered',answer_source_id=$4,revision=2 WHERE tenant_id=$1 AND site_id=$2 AND question_id=$3`, tenant, site, questionID, sourceID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
