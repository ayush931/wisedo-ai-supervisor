-- WIS-517 operations chat, slice 2: one agent conversation run answers one pending reply.
-- The row is the delivery outbox (only delivered_at may change, once) and the run's fence:
-- the run may write only while its reply is still pending.
CREATE TABLE supervisor.chat_answer_turns (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,run_id uuid NOT NULL,
 conversation_id uuid NOT NULL,question_message_id uuid NOT NULL,reply_message_id uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 delivered_at timestamptz,
 PRIMARY KEY(tenant_id,site_id,run_id),
 UNIQUE(tenant_id,site_id,reply_message_id),
 FOREIGN KEY(tenant_id,site_id,run_id) REFERENCES supervisor.agent_runs(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,conversation_id,question_message_id) REFERENCES supervisor.chat_messages(tenant_id,site_id,conversation_id,message_id),
 FOREIGN KEY(tenant_id,site_id,conversation_id,reply_message_id) REFERENCES supervisor.chat_messages(tenant_id,site_id,conversation_id,message_id),
 CHECK(delivered_at IS NULL OR delivered_at>=created_at)
);
CREATE INDEX chat_answer_turns_undelivered ON supervisor.chat_answer_turns(tenant_id,created_at) WHERE delivered_at IS NULL;
CREATE FUNCTION supervisor.chat_answer_turn_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'chat answer turns are retained' USING ERRCODE='42501'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.delivered_at IS NOT NULL THEN RAISE EXCEPTION 'a chat answer turn starts undelivered' USING ERRCODE='23514'; END IF;
  RETURN NEW;
 END IF;
 IF ROW(NEW.tenant_id,NEW.site_id,NEW.run_id,NEW.conversation_id,NEW.question_message_id,NEW.reply_message_id,NEW.created_at)
    IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.run_id,OLD.conversation_id,OLD.question_message_id,OLD.reply_message_id,OLD.created_at)
    OR OLD.delivered_at IS NOT NULL OR NEW.delivered_at IS NULL THEN
  RAISE EXCEPTION 'only delivery may be recorded, once' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER chat_answer_turn_guard BEFORE INSERT OR UPDATE OR DELETE ON supervisor.chat_answer_turns
 FOR EACH ROW EXECUTE FUNCTION supervisor.chat_answer_turn_guard();
ALTER TABLE supervisor.chat_answer_turns ENABLE ROW LEVEL SECURITY;
ALTER TABLE supervisor.chat_answer_turns FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON supervisor.chat_answer_turns USING(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid) WITH CHECK(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid);
REVOKE ALL ON supervisor.chat_answer_turns FROM PUBLIC,supervisor_runtime;
GRANT SELECT,INSERT ON supervisor.chat_answer_turns TO supervisor_runtime;
GRANT UPDATE(delivered_at) ON supervisor.chat_answer_turns TO supervisor_runtime;
