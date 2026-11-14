-- WIS-517 operations chat, slice 1 (renumbered 060 -> 062 after main took 060 and 061): durable site-scoped conversations and messages.
-- A conversation belongs to the member who started it; only that member reads it. A human
-- message is immutable. An assistant reply starts pending (or directly in an explicit
-- unavailable state) and reaches exactly one terminal state; it never authorizes anything.
CREATE TABLE supervisor.chat_conversations (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,conversation_id uuid NOT NULL,
 author_issuer text NOT NULL CHECK(length(author_issuer) BETWEEN 1 AND 512),
 author_subject text NOT NULL CHECK(length(author_subject) BETWEEN 1 AND 255),
 title text NOT NULL CHECK(char_length(title) BETWEEN 1 AND 200 AND title=btrim(title)),
 revision bigint NOT NULL CHECK(revision>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,conversation_id),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 FOREIGN KEY(tenant_id,site_id,author_issuer,author_subject) REFERENCES supervisor.memberships(tenant_id,site_id,issuer,subject)
);
CREATE INDEX chat_conversations_author ON supervisor.chat_conversations(tenant_id,site_id,author_issuer,author_subject,conversation_id);
CREATE FUNCTION supervisor.chat_conversation_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'chat conversations are retained' USING ERRCODE='42501'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.revision<>1 THEN RAISE EXCEPTION 'a conversation starts at revision 1' USING ERRCODE='23514'; END IF;
  RETURN NEW;
 END IF;
 IF ROW(NEW.tenant_id,NEW.site_id,NEW.conversation_id,NEW.author_issuer,NEW.author_subject,NEW.title,NEW.created_at)
    IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.conversation_id,OLD.author_issuer,OLD.author_subject,OLD.title,OLD.created_at)
    OR NEW.revision<>OLD.revision+1 THEN
  RAISE EXCEPTION 'conversation identity is fixed and its revision advances by one' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER chat_conversation_guard BEFORE INSERT OR UPDATE OR DELETE ON supervisor.chat_conversations
 FOR EACH ROW EXECUTE FUNCTION supervisor.chat_conversation_guard();

CREATE TABLE supervisor.chat_messages (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,conversation_id uuid NOT NULL,message_id uuid NOT NULL,
 seq bigint NOT NULL CHECK(seq>0),
 author_kind text NOT NULL CHECK(author_kind IN ('human','assistant')),
 request_id uuid,reply_to uuid,
 body text CHECK(body IS NULL OR (char_length(body) BETWEEN 1 AND 8000)),
 state text NOT NULL CHECK(state IN ('received','pending','answered','needs_input','unavailable','canceled','failed')),
 reason_code text CHECK(reason_code IS NULL OR reason_code ~ '^[A-Z][A-Z0-9_]{2,63}$'),
 citations jsonb NOT NULL DEFAULT '[]'::jsonb CHECK(jsonb_typeof(citations)='array' AND jsonb_array_length(citations)<=50),
 dependencies jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(dependencies)='object'),
 run_id uuid,
 revision bigint NOT NULL CHECK(revision>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,message_id),
 UNIQUE(tenant_id,site_id,conversation_id,message_id),
 UNIQUE(tenant_id,site_id,conversation_id,seq),
 UNIQUE(tenant_id,site_id,conversation_id,request_id),
 FOREIGN KEY(tenant_id,site_id,conversation_id) REFERENCES supervisor.chat_conversations(tenant_id,site_id,conversation_id),
 FOREIGN KEY(tenant_id,site_id,conversation_id,reply_to) REFERENCES supervisor.chat_messages(tenant_id,site_id,conversation_id,message_id),
 FOREIGN KEY(tenant_id,site_id,run_id) REFERENCES supervisor.agent_runs(tenant_id,site_id,id),
 -- A human message is the question as typed: 1..4000 characters, with its client request ID.
 CHECK(author_kind<>'human' OR (state='received' AND request_id IS NOT NULL AND reply_to IS NULL AND body IS NOT NULL AND char_length(body)<=4000 AND reason_code IS NULL AND run_id IS NULL AND citations='[]'::jsonb)),
 -- An assistant message answers exactly one human message; an explicit non-answer carries a reason.
 CHECK(author_kind<>'assistant' OR (state<>'received' AND request_id IS NULL AND reply_to IS NOT NULL)),
 CHECK(state NOT IN ('needs_input','unavailable','canceled','failed') OR reason_code IS NOT NULL),
 CHECK(state<>'answered' OR (body IS NOT NULL AND reason_code IS NULL)),
 CHECK(state NOT IN ('pending','unavailable','canceled','failed') OR (body IS NULL AND citations='[]'::jsonb))
);
CREATE FUNCTION supervisor.chat_message_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'chat messages are retained' USING ERRCODE='42501'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.revision<>1 OR (NEW.author_kind='assistant' AND NEW.state NOT IN ('pending','unavailable')) THEN
   RAISE EXCEPTION 'a message starts at revision 1; a reply starts pending or unavailable' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
 END IF;
 IF OLD.author_kind<>'assistant' OR OLD.state<>'pending' THEN
  RAISE EXCEPTION 'only a pending assistant reply may change' USING ERRCODE='23514';
 END IF;
 IF ROW(NEW.tenant_id,NEW.site_id,NEW.conversation_id,NEW.message_id,NEW.seq,NEW.author_kind,NEW.request_id,NEW.reply_to,NEW.created_at)
    IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.conversation_id,OLD.message_id,OLD.seq,OLD.author_kind,OLD.request_id,OLD.reply_to,OLD.created_at)
    OR NEW.revision<>OLD.revision+1 OR NEW.state='pending' THEN
  RAISE EXCEPTION 'a pending reply reaches one terminal state with its identity fixed' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER chat_message_guard BEFORE INSERT OR UPDATE OR DELETE ON supervisor.chat_messages
 FOR EACH ROW EXECUTE FUNCTION supervisor.chat_message_guard();

CREATE TABLE supervisor.chat_receipts (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 512),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
 operation text NOT NULL CHECK(operation IN ('create','post')),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 conversation_id uuid NOT NULL,result jsonb NOT NULL CHECK(jsonb_typeof(result)='object'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id,conversation_id) REFERENCES supervisor.chat_conversations(tenant_id,site_id,conversation_id)
);

CREATE TABLE supervisor.chat_audit (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL DEFAULT gen_random_uuid(),
 conversation_id uuid NOT NULL,
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 512),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 event_type text NOT NULL CHECK(event_type IN ('conversation.created','conversation.read','messages.read','message.posted','mutation.replayed')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,conversation_id) REFERENCES supervisor.chat_conversations(tenant_id,site_id,conversation_id)
);

DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['chat_conversations','chat_messages','chat_receipts','chat_audit'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['chat_receipts','chat_audit'] LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t||'_immutable',t);
 END LOOP;
 GRANT UPDATE(revision,updated_at) ON supervisor.chat_conversations TO supervisor_runtime;
 GRANT UPDATE(state,body,reason_code,citations,dependencies,run_id,revision,updated_at) ON supervisor.chat_messages TO supervisor_runtime;
END $$;
