-- WIS-279 OutcomeFeedbackV1: a person's append-only feedback on one subject of the site. The
-- subject kind is a versioned discriminator: a chat answer, a knowledge claim revision, an
-- operational source mapping revision, a case or a task. Each kind carries exactly its own
-- reference columns, and composite keys make a cross-site or dangling reference unrepresentable.
-- Feedback is data: the reason is never an instruction, and a correction never overwrites what it
-- challenges (a later reviewed candidate does that through WIS-270).
CREATE TABLE supervisor.feedback (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL,
 schema_version integer NOT NULL CHECK(schema_version=1),
 subject_kind text NOT NULL CHECK(subject_kind IN ('answer','claim','mapping','case','task')),
 -- answer: the assistant reply and the digest of its exact citations at feedback time.
 conversation_id uuid,message_id uuid,answer_citations_sha256 text CHECK(answer_citations_sha256 ~ '^[0-9a-f]{64}$'),
 -- claim: the exact claim revision.
 claim_id uuid,claim_revision bigint CHECK(claim_revision>0),
 -- mapping: the exact mapping revision of an operational source.
 source_id uuid,mapping_revision bigint CHECK(mapping_revision>0),
 -- case and task: the case (bound for a task from the task itself) and its context at feedback
 -- time; the task and an optional proposal of the same case at its current revision.
 case_id uuid,case_revision bigint CHECK(case_revision>0),context_snapshot_id uuid,
 task_id uuid,task_revision bigint CHECK(task_revision>0),proposal_id uuid,proposal_revision bigint CHECK(proposal_revision>0),
 kind text NOT NULL CHECK(kind IN ('observation','correction','usefulness')),
 verdict text NOT NULL CHECK(verdict IN ('helpful','unhelpful','uncertain','not_applicable')),
 reason text NOT NULL CHECK(char_length(reason) BETWEEN 1 AND 2000 AND btrim(reason)<>''),
 -- A correction names the exact evidence source revision it challenges, unless its subject is the
 -- claim or mapping revision it challenges.
 challenged_source_id uuid,challenged_source_revision bigint CHECK(challenged_source_revision>0),
 observation_start timestamptz,observation_end timestamptz,
 -- The author's own earlier feedback on the same subject that this one replaces: a linear chain,
 -- never a branch (rule 06). The earlier record stays.
 supersedes_feedback_id uuid CHECK(supersedes_feedback_id<>id),
 actor_id uuid NOT NULL,
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 512),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 revision bigint NOT NULL DEFAULT 1 CHECK(revision=1),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,id),
 CHECK((observation_start IS NULL)=(observation_end IS NULL) AND (observation_end IS NULL OR observation_end>observation_start)),
 CHECK((challenged_source_id IS NULL)=(challenged_source_revision IS NULL)),
 CHECK(kind<>'correction' OR subject_kind IN ('claim','mapping') OR challenged_source_id IS NOT NULL),
 CHECK(kind='correction' OR challenged_source_id IS NULL),
 -- Exactly the subject kind's own columns are set.
 CHECK((subject_kind='answer')=(conversation_id IS NOT NULL) AND (conversation_id IS NULL)=(message_id IS NULL) AND (conversation_id IS NULL)=(answer_citations_sha256 IS NULL)),
 CHECK((subject_kind='claim')=(claim_id IS NOT NULL) AND (claim_id IS NULL)=(claim_revision IS NULL)),
 CHECK((subject_kind='mapping')=(source_id IS NOT NULL) AND (source_id IS NULL)=(mapping_revision IS NULL)),
 CHECK((subject_kind IN ('case','task'))=(case_id IS NOT NULL) AND (case_id IS NULL)=(case_revision IS NULL) AND (case_id IS NULL)=(context_snapshot_id IS NULL)),
 CHECK((subject_kind='task')=(task_id IS NOT NULL) AND (task_id IS NULL)=(task_revision IS NULL)),
 CHECK(proposal_id IS NULL OR subject_kind IN ('case','task')),
 CHECK((proposal_id IS NULL)=(proposal_revision IS NULL)),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 FOREIGN KEY(tenant_id,site_id,conversation_id,message_id) REFERENCES supervisor.chat_messages(tenant_id,site_id,conversation_id,message_id),
 FOREIGN KEY(tenant_id,site_id,claim_id,claim_revision) REFERENCES supervisor.knowledge_claim_revisions(tenant_id,site_id,claim_id,revision),
 FOREIGN KEY(tenant_id,site_id,source_id,mapping_revision) REFERENCES supervisor.operational_source_mapping_revisions(tenant_id,site_id,source_id,revision),
 FOREIGN KEY(tenant_id,site_id,case_id) REFERENCES supervisor.cases(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,case_id,case_revision) REFERENCES supervisor.case_transitions(tenant_id,site_id,case_id,revision),
 FOREIGN KEY(tenant_id,site_id,task_id,case_id) REFERENCES supervisor.tasks(tenant_id,site_id,id,case_id),
 FOREIGN KEY(tenant_id,site_id,task_id,task_revision) REFERENCES supervisor.task_events(tenant_id,site_id,task_id,revision),
 FOREIGN KEY(tenant_id,site_id,context_snapshot_id) REFERENCES supervisor.plant_contexts(tenant_id,site_id,id),
 -- The proposal revision belongs to this case (066 proposal_revisions_case_key).
 FOREIGN KEY(tenant_id,site_id,proposal_id,proposal_revision,case_id) REFERENCES supervisor.proposal_revisions(tenant_id,site_id,proposal_id,revision,case_id),
 FOREIGN KEY(tenant_id,site_id,challenged_source_id,challenged_source_revision) REFERENCES supervisor.evidence_revisions(tenant_id,site_id,source_id,revision),
 -- The actor is stored as text like every append-only ledger, not bound to a membership row, so
 -- membership retention is not constrained by feedback history.
 FOREIGN KEY(tenant_id,site_id,supersedes_feedback_id) REFERENCES supervisor.feedback(tenant_id,site_id,id)
);
CREATE UNIQUE INDEX feedback_one_successor ON supervisor.feedback(tenant_id,site_id,supersedes_feedback_id) WHERE supersedes_feedback_id IS NOT NULL;
CREATE INDEX feedback_newest ON supervisor.feedback(tenant_id,site_id,created_at DESC,id DESC);
-- A successor has the same author and the same subject as what it supersedes.
CREATE FUNCTION supervisor.feedback_supersedes_check() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF NEW.supersedes_feedback_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM supervisor.feedback p
    WHERE (p.tenant_id,p.site_id,p.id)=(NEW.tenant_id,NEW.site_id,NEW.supersedes_feedback_id)
    AND (p.actor_issuer,p.actor_subject,p.subject_kind)=(NEW.actor_issuer,NEW.actor_subject,NEW.subject_kind)
    AND p.message_id IS NOT DISTINCT FROM NEW.message_id AND p.claim_id IS NOT DISTINCT FROM NEW.claim_id
    AND p.source_id IS NOT DISTINCT FROM NEW.source_id AND p.case_id IS NOT DISTINCT FROM NEW.case_id AND p.task_id IS NOT DISTINCT FROM NEW.task_id) THEN
  RAISE EXCEPTION 'feedback supersedes only its author''s feedback on the same subject' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER feedback_supersedes_check BEFORE INSERT ON supervisor.feedback FOR EACH ROW EXECUTE FUNCTION supervisor.feedback_supersedes_check();
CREATE INDEX feedback_case ON supervisor.feedback(tenant_id,site_id,case_id) WHERE case_id IS NOT NULL;
CREATE INDEX feedback_actor ON supervisor.feedback(tenant_id,site_id,actor_issuer,actor_subject);

-- The evidence a person cites, each at the exact revision current when the feedback was given.
CREATE TABLE supervisor.feedback_evidence (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,feedback_id uuid NOT NULL,
 source_id uuid NOT NULL,revision bigint NOT NULL CHECK(revision>0),
 PRIMARY KEY(tenant_id,site_id,feedback_id,source_id),
 FOREIGN KEY(tenant_id,site_id,feedback_id) REFERENCES supervisor.feedback(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,source_id,revision) REFERENCES supervisor.evidence_revisions(tenant_id,site_id,source_id,revision)
);

CREATE TABLE supervisor.feedback_receipts (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 512),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 idempotency_key uuid NOT NULL,
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 feedback_id uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id,feedback_id) REFERENCES supervisor.feedback(tenant_id,site_id,id)
);

DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['feedback','feedback_evidence','feedback_receipts'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
  -- Append-only: no update; deletion only by the owner role (WIS-290 retention), never the runtime.
  EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t||'_immutable',t);
 END LOOP;
END $$;
