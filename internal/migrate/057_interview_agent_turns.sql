-- WIS-271 interview agent turns are explicit, participant initiated and candidate only.
ALTER TABLE supervisor.interview_questions
 ADD COLUMN prompt_origin text NOT NULL DEFAULT 'template' CHECK(prompt_origin IN ('template','agent')),
 ADD COLUMN generated_by_run_id uuid;
ALTER TABLE supervisor.interview_questions
 ADD FOREIGN KEY(tenant_id,site_id,generated_by_run_id) REFERENCES supervisor.agent_runs(tenant_id,site_id,id),
 ADD CHECK((prompt_origin='agent')=(generated_by_run_id IS NOT NULL));
CREATE OR REPLACE FUNCTION supervisor.interview_question_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'interview questions are retained' USING ERRCODE='42501'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.answer_state<>'unanswered' OR NEW.revision<>1 OR NEW.answer_source_id IS NOT NULL THEN RAISE EXCEPTION 'a question starts unanswered at revision 1' USING ERRCODE='23514'; END IF;
  RETURN NEW;
 END IF;
 IF ROW(NEW.tenant_id,NEW.site_id,NEW.session_id,NEW.question_id,NEW.inventory_id,NEW.inventory_revision,NEW.requirement_id,NEW.evidence_digest,NEW.role,NEW.prompt,NEW.source_span_ids,NEW.reason,NEW.snoozed_until,NEW.created_at,NEW.prompt_origin,NEW.generated_by_run_id)
    IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.session_id,OLD.question_id,OLD.inventory_id,OLD.inventory_revision,OLD.requirement_id,OLD.evidence_digest,OLD.role,OLD.prompt,OLD.source_span_ids,OLD.reason,OLD.snoozed_until,OLD.created_at,OLD.prompt_origin,OLD.generated_by_run_id)
    OR NEW.revision<>OLD.revision+1
    OR NOT (NEW.answer_source_id IS NOT DISTINCT FROM OLD.answer_source_id OR OLD.answer_source_id IS NULL AND NEW.answer_source_id IS NOT NULL)
    OR NOT ((OLD.answer_state='unanswered' AND NEW.answer_state IN ('answered','dismissed')) OR (OLD.answer_state='answered' AND NEW.answer_state='answered')) THEN
  RAISE EXCEPTION 'invalid interview question revision' USING ERRCODE='23514';
 END IF;
 IF (NEW.answer_state='answered')<>(NEW.answer_source_id IS NOT NULL) THEN RAISE EXCEPTION 'answered questions require an evidence source' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;

ALTER TABLE supervisor.interview_receipts DROP CONSTRAINT interview_receipts_operation_check;
ALTER TABLE supervisor.interview_receipts ADD CONSTRAINT interview_receipts_operation_check CHECK(operation IN ('create','patch','answer','agent_turn'));
ALTER TABLE supervisor.interview_audit DROP CONSTRAINT interview_audit_event_type_check;
ALTER TABLE supervisor.interview_audit ADD CONSTRAINT interview_audit_event_type_check CHECK(event_type IN ('session.created','session.read','session.listed','session.status_changed','questions.read','answer.recorded','mutation.replayed','agent_turn.started','agent_turn.replayed','agent_turn.read'));

CREATE TABLE supervisor.interview_agent_turns (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, session_id uuid NOT NULL, run_id uuid NOT NULL,
 session_revision bigint NOT NULL CHECK(session_revision>0), purpose text NOT NULL CHECK(purpose='questions'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,run_id), UNIQUE(tenant_id,site_id,session_id,run_id),
 FOREIGN KEY(tenant_id,site_id,session_id) REFERENCES supervisor.interview_sessions(tenant_id,site_id,session_id),
 FOREIGN KEY(tenant_id,site_id,run_id) REFERENCES supervisor.agent_runs(tenant_id,site_id,id)
);
CREATE TABLE supervisor.interview_agent_turn_outbox (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, id uuid NOT NULL, session_id uuid NOT NULL, run_id uuid NOT NULL,
 delivered_at timestamptz,
 PRIMARY KEY(tenant_id,site_id,id), UNIQUE(tenant_id,site_id,run_id),
 FOREIGN KEY(tenant_id,site_id,session_id,run_id) REFERENCES supervisor.interview_agent_turns(tenant_id,site_id,session_id,run_id)
);
CREATE TRIGGER interview_agent_turns_immutable BEFORE UPDATE OR DELETE ON supervisor.interview_agent_turns FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
CREATE FUNCTION supervisor.interview_agent_turn_outbox_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'interview turn outbox is retained' USING ERRCODE='42501'; END IF;
 IF OLD.delivered_at IS NOT NULL OR NEW.tenant_id<>OLD.tenant_id OR NEW.site_id<>OLD.site_id OR NEW.id<>OLD.id OR NEW.session_id<>OLD.session_id OR NEW.run_id<>OLD.run_id OR NEW.delivered_at IS NULL THEN
  RAISE EXCEPTION 'invalid interview turn delivery update' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER interview_agent_turn_outbox_guard BEFORE UPDATE OR DELETE ON supervisor.interview_agent_turn_outbox FOR EACH ROW EXECUTE FUNCTION supervisor.interview_agent_turn_outbox_guard();
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['interview_agent_turns','interview_agent_turn_outbox'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP;
 GRANT UPDATE(delivered_at) ON supervisor.interview_agent_turn_outbox TO supervisor_runtime;
END $$;
