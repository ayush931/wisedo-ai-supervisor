//go:build integration

package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestWIS703PopulatedUpgradeRetainsFeedbackAndCorrectionLedgers(t *testing.T) {
	db := migratorDB(t)
	ctx := context.Background()
	if err := applyThrough(ctx, db, 81); err != nil {
		t.Fatal(err)
	}
	tenant, site, conversation, question, answer, feedbackID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	seed, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Rollback()
	steps := []struct {
		q    string
		args []any
	}{
		{`SELECT set_config('supervisor.tenant_id',$1,true)`, []any{tenant}},
		{`INSERT INTO supervisor.tenants(id,name) VALUES($1,'correction upgrade')`, []any{tenant}},
		{`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'plant')`, []any{tenant, site}},
		{`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,'i','author','supervisor')`, []any{tenant, site}},
		{`INSERT INTO supervisor.chat_conversations(tenant_id,site_id,conversation_id,author_issuer,author_subject,title,revision) VALUES($1,$2,$3,'i','author','Legacy',1)`, []any{tenant, site, conversation}},
		{`INSERT INTO supervisor.chat_messages(tenant_id,site_id,conversation_id,message_id,seq,author_kind,request_id,body,state,revision) VALUES($1,$2,$3,$4,1,'human',gen_random_uuid(),'Question','received',1)`, []any{tenant, site, conversation, question}},
		{`INSERT INTO supervisor.chat_messages(tenant_id,site_id,conversation_id,message_id,seq,author_kind,reply_to,state,revision) VALUES($1,$2,$3,$4,2,'assistant',$5,'pending',1)`, []any{tenant, site, conversation, answer, question}},
		{`UPDATE supervisor.chat_messages SET state='answered',body='Legacy answer',revision=2 WHERE message_id=$1`, []any{answer}},
		{`INSERT INTO supervisor.feedback(tenant_id,site_id,id,schema_version,subject_kind,conversation_id,message_id,answer_citations_sha256,kind,verdict,reason,actor_id,actor_issuer,actor_subject) VALUES($1,$2,$3,1,'answer',$4,$5,repeat('a',64),'observation','uncertain','Old feedback',$6,'i','author')`, []any{tenant, site, feedbackID, conversation, answer, uuid.NewString()}},
	}
	for _, step := range steps {
		if _, err = seed.ExecContext(ctx, step.q, step.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err = seed.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = Apply(ctx, db); err != nil {
		t.Fatalf("upgrade populated 081 as migrator: %v", err)
	}
	if err = Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	rt := runtimeDB(t, db)
	in := func(fn func(*sql.Tx) error) error {
		tx, e := rt.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); e != nil {
			return e
		}
		if e = fn(tx); e != nil {
			return e
		}
		return tx.Commit()
	}
	if err = in(func(tx *sql.Tx) error {
		var reason string
		var run, artifact, snapshot sql.NullString
		e := tx.QueryRowContext(ctx, `SELECT reason,run_id::text,artifact_id::text,process_snapshot_id::text FROM supervisor.feedback WHERE id=$1`, feedbackID).Scan(&reason, &run, &artifact, &snapshot)
		if e != nil {
			return e
		}
		if reason != "Old feedback" || run.Valid || artifact.Valid || snapshot.Valid {
			t.Fatal("upgrade invented provenance or changed feedback")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertTenantTablesForced(t, db)
	correctionID, candidateID := uuid.NewString(), uuid.NewString()
	if err = in(func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, `INSERT INTO supervisor.feedback(tenant_id,site_id,id,schema_version,subject_kind,conversation_id,message_id,answer_citations_sha256,kind,verdict,reason,actor_id,actor_issuer,actor_subject) VALUES($1,$2,$3,1,'answer',$4,$5,repeat('b',64),'correction','unhelpful','Incomplete',$6,'i','author')`, tenant, site, correctionID, conversation, answer, uuid.NewString())
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_memory_candidates(tenant_id,site_id,candidate_id,source_feedback_id,proposer_issuer,proposer_subject) VALUES($1,$2,$3,$4,'i','author')`, tenant, site, candidateID, correctionID)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_memory_candidate_revisions(tenant_id,site_id,candidate_id,revision,payload,sha256,actor_issuer,actor_subject) VALUES($1,$2,$3,1,'{"reason":"Incomplete"}',repeat('c',64),'i','author')`, tenant, site, candidateID)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	// Tenant RLS alone is insufficient: a revision cannot reuse a candidate from another site.
	otherSite := uuid.NewString()
	scopeTx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer scopeTx.Rollback()
	if _, err = scopeTx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err = scopeTx.ExecContext(ctx, `INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'Other scope')`, tenant, otherSite); err != nil {
		t.Fatal(err)
	}
	if err = scopeTx.Commit(); err != nil {
		t.Fatal(err)
	}
	err = in(func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_memory_candidate_revisions(tenant_id,site_id,candidate_id,revision,payload,sha256,actor_issuer,actor_subject) VALUES($1,$2,$3,2,'{"reason":"Foreign candidate"}',repeat('c',64),'i','author')`, tenant, otherSite, candidateID)
		return e
	})
	if sqlState(err) != "23503" {
		t.Fatalf("composite candidate scope FK: %v", err)
	}
	// Rule 05 backstop (review of #299): only the proposer writes revisions, so another person
	// cannot author a revision and then approve their own content as "not the proposer".
	err = in(func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_memory_candidate_revisions(tenant_id,site_id,candidate_id,revision,payload,sha256,actor_issuer,actor_subject) VALUES($1,$2,$3,2,'{"reason":"Rewritten by a reviewer"}',repeat('d',64),'i','reviewer')`, tenant, site, candidateID)
		return e
	})
	if sqlState(err) != "23514" {
		t.Fatalf("a revision by someone other than the proposer: %v, want 23514", err)
	}
	for _, table := range []string{"knowledge_memory_candidate_revisions", "knowledge_memory_candidate_reviews", "correction_evaluation_cases"} {
		for _, verb := range []string{"UPDATE " + table + " SET sha256=repeat('a',64)", "DELETE FROM " + table} {
			// Review rows have no hash; use a shared field so a missing column cannot masquerade as denial.
			if table == "knowledge_memory_candidate_reviews" && verb[:6] == "UPDATE" {
				verb = "UPDATE " + table + " SET reason='rewrite'"
			}
			statement := strings.Replace(verb, table, "supervisor."+table, 1)
			err = in(func(tx *sql.Tx) error { _, e := tx.ExecContext(ctx, statement); return e })
			if sqlState(err) != "42501" {
				t.Fatalf("%s privilege: %v", verb, err)
			}
		}
	}
	err = in(func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, `UPDATE supervisor.knowledge_memory_candidates SET proposer_subject='other' WHERE candidate_id=$1`, candidateID)
		return e
	})
	if sqlState(err) != "42501" {
		t.Fatalf("original author mutable: %v", err)
	}
	err = in(func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_memory_candidate_reviews(tenant_id,site_id,candidate_id,revision,review_id,verdict,reason,actor_issuer,actor_subject,auth_time) VALUES($1,$2,$3,1,gen_random_uuid(),'reject','Self review','i','author',clock_timestamp())`, tenant, site, candidateID)
		return e
	})
	if sqlState(err) != "23514" {
		t.Fatalf("database permits self review: %v", err)
	}
	if err = in(func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, uuid.NewString())
		if e != nil {
			return e
		}
		var n int
		e = tx.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.knowledge_memory_candidates`).Scan(&n)
		if e == nil && n != 0 {
			t.Fatal("cross-tenant candidate disclosure")
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
}
