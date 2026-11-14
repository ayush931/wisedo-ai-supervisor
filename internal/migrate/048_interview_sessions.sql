CREATE TABLE supervisor.interview_sessions (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,session_id uuid NOT NULL,
 role text NOT NULL CHECK(role IN ('owner','supervisor','operator')),
 participant_actor_id uuid NOT NULL,
 participant_issuer text NOT NULL CHECK(length(participant_issuer) BETWEEN 1 AND 512),
 participant_subject text NOT NULL CHECK(length(participant_subject) BETWEEN 1 AND 255),
 process_id uuid,
 inventory_id uuid,inventory_revision bigint,
 status text NOT NULL CHECK(status IN ('active','paused','submitted','closed')),
 revision bigint NOT NULL CHECK(revision>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((inventory_id IS NULL)=(inventory_revision IS NULL)),
 PRIMARY KEY(tenant_id,site_id,session_id),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 FOREIGN KEY(tenant_id,site_id,participant_issuer,participant_subject) REFERENCES supervisor.memberships(tenant_id,site_id,issuer,subject),
 FOREIGN KEY(tenant_id,site_id,process_id) REFERENCES supervisor.knowledge_entities(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,inventory_id,inventory_revision) REFERENCES supervisor.knowledge_discovery_revisions(tenant_id,site_id,inventory_id,revision)
);
CREATE FUNCTION supervisor.interview_session_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'interview sessions are retained' USING ERRCODE='42501'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.status<>'active' OR NEW.revision<>1 THEN RAISE EXCEPTION 'an interview starts active at revision 1' USING ERRCODE='23514'; END IF;
  RETURN NEW;
 END IF;
 IF ROW(NEW.tenant_id,NEW.site_id,NEW.session_id,NEW.role,NEW.participant_actor_id,NEW.participant_issuer,NEW.participant_subject,NEW.process_id,NEW.inventory_id,NEW.inventory_revision,NEW.created_at)
    IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.session_id,OLD.role,OLD.participant_actor_id,OLD.participant_issuer,OLD.participant_subject,OLD.process_id,OLD.inventory_id,OLD.inventory_revision,OLD.created_at)
    OR NEW.revision<>OLD.revision+1 THEN
  RAISE EXCEPTION 'interview identity is fixed and its revision advances by one' USING ERRCODE='23514';
 END IF;
 IF NOT ((OLD.status='active' AND NEW.status IN ('paused','submitted','closed'))
      OR (OLD.status='paused' AND NEW.status IN ('active','submitted','closed'))
      OR (OLD.status='submitted' AND NEW.status='closed')) THEN
  RAISE EXCEPTION 'interview lifecycle transition is not allowed' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER interview_session_guard BEFORE INSERT OR UPDATE OR DELETE ON supervisor.interview_sessions
 FOR EACH ROW EXECUTE FUNCTION supervisor.interview_session_guard();

CREATE TABLE supervisor.interview_questions (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,session_id uuid NOT NULL,question_id uuid NOT NULL,
 inventory_id uuid NOT NULL,inventory_revision bigint NOT NULL,requirement_id uuid NOT NULL,
 evidence_digest text NOT NULL CHECK(evidence_digest ~ '^[0-9a-f]{64}$'),
 role text NOT NULL CHECK(role IN ('owner','supervisor','operator')),
 prompt text NOT NULL CHECK(length(prompt) BETWEEN 1 AND 1000),
 source_span_ids jsonb NOT NULL DEFAULT '[]'::jsonb CHECK(jsonb_typeof(source_span_ids)='array' AND jsonb_array_length(source_span_ids)<=10),
 reason text NOT NULL CHECK(reason IN ('missing','conflict','stale')),
 answer_state text NOT NULL CHECK(answer_state IN ('unanswered','answered','dismissed')),
 snoozed_until timestamptz,
 revision bigint NOT NULL CHECK(revision>0),
 answer_source_id uuid,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,question_id),
 UNIQUE(tenant_id,site_id,session_id,question_id),
 UNIQUE(tenant_id,site_id,session_id,requirement_id,evidence_digest),
 FOREIGN KEY(tenant_id,site_id,session_id) REFERENCES supervisor.interview_sessions(tenant_id,site_id,session_id),
 FOREIGN KEY(tenant_id,site_id,inventory_id,inventory_revision,requirement_id) REFERENCES supervisor.knowledge_discovery_requirements(tenant_id,site_id,inventory_id,revision,requirement_id),
 FOREIGN KEY(tenant_id,site_id,answer_source_id) REFERENCES supervisor.evidence_heads(tenant_id,site_id,source_id)
);
CREATE FUNCTION supervisor.interview_question_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'interview questions are retained' USING ERRCODE='42501'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.answer_state<>'unanswered' OR NEW.revision<>1 OR NEW.answer_source_id IS NOT NULL THEN
   RAISE EXCEPTION 'a question starts unanswered at revision 1' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
 END IF;
 IF ROW(NEW.tenant_id,NEW.site_id,NEW.session_id,NEW.question_id,NEW.inventory_id,NEW.inventory_revision,NEW.requirement_id,NEW.evidence_digest,NEW.role,NEW.prompt,NEW.source_span_ids,NEW.reason,NEW.snoozed_until,NEW.created_at)
    IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.session_id,OLD.question_id,OLD.inventory_id,OLD.inventory_revision,OLD.requirement_id,OLD.evidence_digest,OLD.role,OLD.prompt,OLD.source_span_ids,OLD.reason,OLD.snoozed_until,OLD.created_at)
    OR NEW.revision<>OLD.revision+1
    OR NOT (NEW.answer_source_id IS NOT DISTINCT FROM OLD.answer_source_id OR OLD.answer_source_id IS NULL AND NEW.answer_source_id IS NOT NULL)
    OR NOT ((OLD.answer_state='unanswered' AND NEW.answer_state IN ('answered','dismissed'))
         OR (OLD.answer_state='answered' AND NEW.answer_state='answered')) THEN
  RAISE EXCEPTION 'invalid interview question revision' USING ERRCODE='23514';
 END IF;
 IF (NEW.answer_state='answered')<>(NEW.answer_source_id IS NOT NULL) THEN
  RAISE EXCEPTION 'answered questions require an evidence source' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER interview_question_guard BEFORE INSERT OR UPDATE OR DELETE ON supervisor.interview_questions
 FOR EACH ROW EXECUTE FUNCTION supervisor.interview_question_guard();

CREATE TABLE supervisor.interview_answers (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,session_id uuid NOT NULL,question_id uuid NOT NULL,
 revision bigint NOT NULL CHECK(revision>0),question_revision bigint NOT NULL CHECK(question_revision>0),
 client_answer_id uuid NOT NULL,source_id uuid NOT NULL,source_revision bigint NOT NULL CHECK(source_revision>0),
 source_span_id uuid NOT NULL,request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,question_id,revision),
 UNIQUE(tenant_id,site_id,client_answer_id),
 FOREIGN KEY(tenant_id,site_id,session_id,question_id) REFERENCES supervisor.interview_questions(tenant_id,site_id,session_id,question_id),
 FOREIGN KEY(tenant_id,site_id,source_id,source_revision) REFERENCES supervisor.evidence_revisions(tenant_id,site_id,source_id,revision),
 FOREIGN KEY(tenant_id,site_id,source_span_id) REFERENCES supervisor.evidence_spans(tenant_id,site_id,id)
);
CREATE FUNCTION supervisor.interview_answer_span_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE span_source uuid; span_revision bigint;
BEGIN
 SELECT source_id,revision INTO span_source,span_revision FROM supervisor.evidence_spans
  WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND id=NEW.source_span_id;
 IF span_source IS DISTINCT FROM NEW.source_id OR span_revision IS DISTINCT FROM NEW.source_revision THEN
  RAISE EXCEPTION 'interview answer span must name its evidence revision' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER interview_answer_span_guard BEFORE INSERT ON supervisor.interview_answers
 FOR EACH ROW EXECUTE FUNCTION supervisor.interview_answer_span_guard();

CREATE TABLE supervisor.interview_receipts (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 512),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
 operation text NOT NULL CHECK(operation IN ('create','patch','answer')),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 session_id uuid NOT NULL,result jsonb NOT NULL CHECK(jsonb_typeof(result)='object'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id,session_id) REFERENCES supervisor.interview_sessions(tenant_id,site_id,session_id)
);

CREATE TABLE supervisor.interview_audit (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL DEFAULT gen_random_uuid(),
 session_id uuid NOT NULL,question_id uuid,
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 512),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 event_type text NOT NULL CHECK(event_type IN ('session.created','session.read','session.listed','session.status_changed','questions.read','answer.recorded','mutation.replayed')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,session_id) REFERENCES supervisor.interview_sessions(tenant_id,site_id,session_id),
 FOREIGN KEY(tenant_id,site_id,session_id,question_id) REFERENCES supervisor.interview_questions(tenant_id,site_id,session_id,question_id)
);

DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['interview_sessions','interview_questions','interview_answers','interview_receipts','interview_audit'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['interview_answers','interview_receipts','interview_audit'] LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t||'_immutable',t);
 END LOOP;
 GRANT UPDATE(status,revision,updated_at) ON supervisor.interview_sessions TO supervisor_runtime;
 GRANT UPDATE(revision,answer_state,answer_source_id,updated_at) ON supervisor.interview_questions TO supervisor_runtime;
END $$;
