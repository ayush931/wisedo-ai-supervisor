-- WIS-703: pending corrections never advance claim heads or knowledge inventory.
ALTER TABLE supervisor.agent_runs ADD UNIQUE(tenant_id,site_id,id,case_id,case_revision);
ALTER TABLE supervisor.feedback
 ADD COLUMN run_id uuid,
 ADD COLUMN artifact_id uuid,
 ADD COLUMN artifact_sha256 text CHECK(artifact_sha256 ~ '^[0-9a-f]{64}$'),
 ADD COLUMN terminal_sha256 text CHECK(terminal_sha256 ~ '^[0-9a-f]{64}$'),
 ADD COLUMN run_refs_sha256 text CHECK(run_refs_sha256 ~ '^[0-9a-f]{64}$'),
 ADD COLUMN process_snapshot_id uuid,
 ADD FOREIGN KEY(tenant_id,site_id,run_id) REFERENCES supervisor.agent_runs(tenant_id,site_id,id),
 ADD FOREIGN KEY(tenant_id,site_id,run_id,case_id,case_revision) REFERENCES supervisor.agent_runs(tenant_id,site_id,id,case_id,case_revision),
 ADD FOREIGN KEY(tenant_id,site_id,run_id,artifact_id) REFERENCES supervisor.agent_artifacts(tenant_id,site_id,run_id,id),
 ADD FOREIGN KEY(tenant_id,site_id,process_snapshot_id) REFERENCES supervisor.knowledge_process_snapshots(tenant_id,site_id,snapshot_id),
 ADD CHECK((artifact_id IS NULL)=(artifact_sha256 IS NULL)),
 ADD CHECK(artifact_id IS NULL OR run_id IS NOT NULL),
 ADD CHECK(run_id IS NULL OR run_refs_sha256 IS NOT NULL);
-- Replace only discriminator checks; all other 069 constraints and existing rows are retained.
DO $$ DECLARE c record; BEGIN
 FOR c IN SELECT conname FROM pg_constraint WHERE conrelid='supervisor.feedback'::regclass AND contype='c' AND pg_get_constraintdef(oid) LIKE '%subject_kind%' LOOP
  EXECUTE format('ALTER TABLE supervisor.feedback DROP CONSTRAINT %I',c.conname);
 END LOOP;
END $$;
ALTER TABLE supervisor.feedback
 ADD CHECK(subject_kind IN ('answer','investigation','claim','mapping','case','task')),
 ADD CHECK(kind<>'correction' OR subject_kind IN ('answer','investigation','claim','mapping') OR challenged_source_id IS NOT NULL),
 ADD CHECK((subject_kind='answer')=(conversation_id IS NOT NULL) AND (conversation_id IS NULL)=(message_id IS NULL) AND (conversation_id IS NULL)=(answer_citations_sha256 IS NULL)),
 ADD CHECK((subject_kind='claim')=(claim_id IS NOT NULL) AND (claim_id IS NULL)=(claim_revision IS NULL)),
 ADD CHECK((subject_kind='mapping')=(source_id IS NOT NULL) AND (source_id IS NULL)=(mapping_revision IS NULL)),
 ADD CHECK((subject_kind IN ('case','task','investigation'))=(case_id IS NOT NULL) AND (case_id IS NULL)=(case_revision IS NULL) AND (case_id IS NULL)=(context_snapshot_id IS NULL)),
 ADD CHECK((subject_kind='task')=(task_id IS NOT NULL) AND (task_id IS NULL)=(task_revision IS NULL)),
 ADD CHECK(proposal_id IS NULL OR subject_kind IN ('case','task')),
 ADD CHECK(subject_kind<>'investigation' OR (run_id IS NOT NULL AND (artifact_id IS NOT NULL OR terminal_sha256 IS NOT NULL))),
 ADD CHECK(run_id IS NULL OR subject_kind IN ('answer','investigation'));
CREATE TABLE supervisor.knowledge_memory_candidates (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,candidate_id uuid NOT NULL,
 source_feedback_id uuid NOT NULL,proposer_issuer text NOT NULL,proposer_subject text NOT NULL,
 current_revision bigint NOT NULL DEFAULT 1 CHECK(current_revision>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,candidate_id),
 UNIQUE(tenant_id,site_id,source_feedback_id),
 FOREIGN KEY(tenant_id,site_id,source_feedback_id) REFERENCES supervisor.feedback(tenant_id,site_id,id)
);
CREATE TABLE supervisor.knowledge_memory_candidate_revisions (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,candidate_id uuid NOT NULL,revision bigint NOT NULL CHECK(revision>0),
 kind text NOT NULL DEFAULT 'process_claim' CHECK(kind='process_claim'),
 payload jsonb NOT NULL CHECK(jsonb_typeof(payload)='object' AND octet_length(payload::text)<=16384),
 sha256 text NOT NULL CHECK(sha256 ~ '^[0-9a-f]{64}$'),
 target_claim_id uuid,base_revision bigint CHECK(base_revision>0),
 actor_issuer text NOT NULL,actor_subject text NOT NULL,created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,candidate_id,revision),
 FOREIGN KEY(tenant_id,site_id,candidate_id) REFERENCES supervisor.knowledge_memory_candidates(tenant_id,site_id,candidate_id),
 FOREIGN KEY(tenant_id,site_id,target_claim_id,base_revision) REFERENCES supervisor.knowledge_claim_revisions(tenant_id,site_id,claim_id,revision),
 CHECK((target_claim_id IS NULL)=(base_revision IS NULL))
);
ALTER TABLE supervisor.knowledge_memory_candidates ADD FOREIGN KEY(tenant_id,site_id,candidate_id,current_revision)
 REFERENCES supervisor.knowledge_memory_candidate_revisions(tenant_id,site_id,candidate_id,revision) DEFERRABLE INITIALLY DEFERRED;
CREATE TABLE supervisor.knowledge_memory_candidate_reviews (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,candidate_id uuid NOT NULL,revision bigint NOT NULL,
 review_id uuid NOT NULL,verdict text NOT NULL CHECK(verdict IN ('approve','reject')),
 reason text NOT NULL CHECK(char_length(reason) BETWEEN 1 AND 2000 AND btrim(reason)<>''),
 actor_issuer text NOT NULL,actor_subject text NOT NULL,auth_time timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 claim_id uuid,claim_revision bigint,canonical_review_id uuid,
 PRIMARY KEY(tenant_id,site_id,candidate_id,revision),
 UNIQUE(tenant_id,site_id,review_id,candidate_id,revision,verdict),
 FOREIGN KEY(tenant_id,site_id,candidate_id,revision) REFERENCES supervisor.knowledge_memory_candidate_revisions(tenant_id,site_id,candidate_id,revision),
 FOREIGN KEY(tenant_id,site_id,canonical_review_id,claim_id,claim_revision) REFERENCES supervisor.knowledge_claim_reviews(tenant_id,site_id,review_id,claim_id,claim_revision),
 CHECK((verdict='approve' AND claim_id IS NOT NULL AND claim_revision IS NOT NULL AND canonical_review_id IS NOT NULL)
    OR (verdict='reject' AND claim_id IS NULL AND claim_revision IS NULL AND canonical_review_id IS NULL)),
 CHECK(auth_time<=created_at AND auth_time>=created_at-interval '15 minutes')
);
CREATE TABLE supervisor.correction_evaluation_cases (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,case_id uuid NOT NULL,
 candidate_id uuid NOT NULL,candidate_revision bigint NOT NULL,review_id uuid NOT NULL,source_feedback_id uuid NOT NULL,
 verdict text NOT NULL DEFAULT 'approve' CHECK(verdict='approve'),
 payload jsonb NOT NULL CHECK(jsonb_typeof(payload)='object' AND octet_length(payload::text)<=32768),
 sha256 text NOT NULL CHECK(sha256 ~ '^[0-9a-f]{64}$'),created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,case_id),UNIQUE(tenant_id,site_id,candidate_id,candidate_revision),
 FOREIGN KEY(tenant_id,site_id,review_id,candidate_id,candidate_revision,verdict) REFERENCES supervisor.knowledge_memory_candidate_reviews(tenant_id,site_id,review_id,candidate_id,revision,verdict),
 FOREIGN KEY(tenant_id,site_id,source_feedback_id) REFERENCES supervisor.feedback(tenant_id,site_id,id)
);
CREATE INDEX memory_candidates_newest ON supervisor.knowledge_memory_candidates(tenant_id,site_id,created_at DESC,candidate_id DESC);
CREATE INDEX correction_approvals_month ON supervisor.knowledge_memory_candidate_reviews(tenant_id,site_id,created_at) WHERE verdict='approve';
CREATE FUNCTION supervisor.memory_candidate_head_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='INSERT' THEN
  IF NOT EXISTS(SELECT 1 FROM supervisor.feedback f WHERE (f.tenant_id,f.site_id,f.id,f.actor_issuer,f.actor_subject)=(NEW.tenant_id,NEW.site_id,NEW.source_feedback_id,NEW.proposer_issuer,NEW.proposer_subject) AND f.kind='correction' AND f.subject_kind IN ('answer','investigation')) OR NEW.current_revision<>1 THEN
   RAISE EXCEPTION 'a correction starts with its feedback author' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
 END IF;
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'corrections are retained' USING ERRCODE='42501'; END IF;
 IF ROW(NEW.tenant_id,NEW.site_id,NEW.candidate_id,NEW.source_feedback_id,NEW.proposer_issuer,NEW.proposer_subject,NEW.created_at)
 IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.candidate_id,OLD.source_feedback_id,OLD.proposer_issuer,OLD.proposer_subject,OLD.created_at)
 OR NEW.current_revision<>OLD.current_revision+1
 OR EXISTS(SELECT 1 FROM supervisor.knowledge_memory_candidate_reviews WHERE (tenant_id,site_id,candidate_id)=(OLD.tenant_id,OLD.site_id,OLD.candidate_id)) THEN
  RAISE EXCEPTION 'only undecided correction heads may advance' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER memory_candidate_head_guard BEFORE INSERT OR UPDATE OR DELETE ON supervisor.knowledge_memory_candidates FOR EACH ROW EXECUTE FUNCTION supervisor.memory_candidate_head_guard();
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['knowledge_memory_candidates','knowledge_memory_candidate_revisions','knowledge_memory_candidate_reviews','correction_evaluation_cases'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
  IF t<>'knowledge_memory_candidates' THEN
   EXECUTE format('CREATE TRIGGER correction_immutable BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t);
  END IF;
 END LOOP;
END $$;
GRANT UPDATE(current_revision) ON supervisor.knowledge_memory_candidates TO supervisor_runtime;
ALTER TABLE supervisor.knowledge_receipts DROP CONSTRAINT knowledge_receipts_operation_check;
ALTER TABLE supervisor.knowledge_receipts ADD CONSTRAINT knowledge_receipts_operation_check CHECK(operation IN ('entity','claim','review','review_revoke','resolution','snapshot','discovery','requirement','freshness_policy','merge','memory_revision','memory_review'));
ALTER TABLE supervisor.knowledge_audit DROP CONSTRAINT knowledge_audit_event_type_check;
ALTER TABLE supervisor.knowledge_audit ADD CONSTRAINT knowledge_audit_event_type_check CHECK(event_type IN ('entity.created','entity.revised','entity.read','entity.listed','claim.created','claim.revised','claim.read','claim.listed','mutation.replayed','conflicts.read','review.decided','review.read','review.revoked','resolution.decided','resolution.read','inventory.read','snapshot.created','snapshot.read','discovery.created','discovery.revised','discovery.read','requirement.decided','requirement.read','coverage.read','freshness_policy.decided','freshness_policy.read','entity.merged','discovery.listed','memory.created','memory.revised','memory.read','memory.listed','memory.decided','corrections.read'));

CREATE FUNCTION supervisor.memory_review_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM supervisor.knowledge_memory_candidates c WHERE (c.tenant_id,c.site_id,c.candidate_id,c.current_revision)=(NEW.tenant_id,NEW.site_id,NEW.candidate_id,NEW.revision) AND (c.proposer_issuer,c.proposer_subject)<>(NEW.actor_issuer,NEW.actor_subject)) THEN
  RAISE EXCEPTION 'a different human reviews the current correction revision' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER memory_review_guard BEFORE INSERT ON supervisor.knowledge_memory_candidate_reviews FOR EACH ROW EXECUTE FUNCTION supervisor.memory_review_guard();
-- Only the proposer writes revisions, so the reviewer check above cannot be bypassed by another
-- person authoring a revision and then approving their own content (rule 05, enforced here too).
CREATE FUNCTION supervisor.memory_revision_author_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 -- A candidate missing from this site is left to the composite foreign key.
 IF EXISTS(SELECT 1 FROM supervisor.knowledge_memory_candidates c WHERE (c.tenant_id,c.site_id,c.candidate_id)=(NEW.tenant_id,NEW.site_id,NEW.candidate_id) AND (c.proposer_issuer,c.proposer_subject)<>(NEW.actor_issuer,NEW.actor_subject)) THEN
  RAISE EXCEPTION 'only the proposer revises a correction' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER memory_revision_author_guard BEFORE INSERT ON supervisor.knowledge_memory_candidate_revisions FOR EACH ROW EXECUTE FUNCTION supervisor.memory_revision_author_guard();

CREATE OR REPLACE FUNCTION supervisor.feedback_supersedes_check() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF NEW.supersedes_feedback_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM supervisor.feedback p
    WHERE (p.tenant_id,p.site_id,p.id)=(NEW.tenant_id,NEW.site_id,NEW.supersedes_feedback_id)
    AND (p.actor_issuer,p.actor_subject,p.subject_kind)=(NEW.actor_issuer,NEW.actor_subject,NEW.subject_kind)
    AND p.run_id IS NOT DISTINCT FROM NEW.run_id
    AND p.message_id IS NOT DISTINCT FROM NEW.message_id AND p.claim_id IS NOT DISTINCT FROM NEW.claim_id
    AND p.source_id IS NOT DISTINCT FROM NEW.source_id AND p.case_id IS NOT DISTINCT FROM NEW.case_id AND p.task_id IS NOT DISTINCT FROM NEW.task_id) THEN
  RAISE EXCEPTION 'feedback supersedes only its author''s feedback on the same subject' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
