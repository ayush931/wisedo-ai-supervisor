-- WIS-276: the supervisor-confirmed maintenance assignee roster, per line. A recovery proposal may
-- name only an assignee confirmed here for the case's line (never one inferred from documents or
-- qualification text). The head is the current decision; every decision is an append-only event.
CREATE TABLE supervisor.maintenance_roster_heads (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,line_id uuid NOT NULL,assignee_actor_id uuid NOT NULL,
 assignee_issuer text NOT NULL CHECK(length(assignee_issuer) BETWEEN 1 AND 512),
 assignee_subject text NOT NULL CHECK(length(assignee_subject) BETWEEN 1 AND 255),
 confirmed boolean NOT NULL,
 revision bigint NOT NULL CHECK(revision>0),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,line_id,assignee_actor_id),
 UNIQUE(tenant_id,site_id,line_id,assignee_issuer,assignee_subject),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 FOREIGN KEY(tenant_id,site_id,line_id) REFERENCES supervisor.knowledge_entity_heads(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,assignee_issuer,assignee_subject) REFERENCES supervisor.memberships(tenant_id,site_id,issuer,subject)
);
CREATE INDEX maintenance_roster_confirmed ON supervisor.maintenance_roster_heads(tenant_id,site_id,line_id) WHERE confirmed;

CREATE TABLE supervisor.maintenance_roster_events (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,line_id uuid NOT NULL,assignee_actor_id uuid NOT NULL,
 revision bigint NOT NULL CHECK(revision>0),
 confirmed boolean NOT NULL,
 reason text NOT NULL CHECK(char_length(reason) BETWEEN 1 AND 2000),
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 512),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 auth_time timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,line_id,assignee_actor_id,revision),
 FOREIGN KEY(tenant_id,site_id,line_id,assignee_actor_id) REFERENCES supervisor.maintenance_roster_heads(tenant_id,site_id,line_id,assignee_actor_id)
);

CREATE TABLE supervisor.maintenance_roster_receipts (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 512),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 line_id uuid NOT NULL,assignee_actor_id uuid NOT NULL,revision bigint NOT NULL CHECK(revision>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id,line_id,assignee_actor_id,revision) REFERENCES supervisor.maintenance_roster_events(tenant_id,site_id,line_id,assignee_actor_id,revision)
);

CREATE FUNCTION supervisor.maintenance_roster_head_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'maintenance roster decisions are retained' USING ERRCODE='42501'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.revision<>1 THEN RAISE EXCEPTION 'a roster entry starts at revision 1' USING ERRCODE='23514'; END IF;
  RETURN NEW;
 END IF;
 IF ROW(NEW.tenant_id,NEW.site_id,NEW.line_id,NEW.assignee_actor_id,NEW.assignee_issuer,NEW.assignee_subject)
    IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.line_id,OLD.assignee_actor_id,OLD.assignee_issuer,OLD.assignee_subject)
    OR NEW.revision<>OLD.revision+1 OR NEW.confirmed=OLD.confirmed THEN
  RAISE EXCEPTION 'a roster decision changes the state and advances the revision by one' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER maintenance_roster_head_guard BEFORE INSERT OR UPDATE OR DELETE ON supervisor.maintenance_roster_heads
 FOR EACH ROW EXECUTE FUNCTION supervisor.maintenance_roster_head_guard();

-- An event must describe the head it lands on: same state, same revision.
CREATE FUNCTION supervisor.maintenance_roster_event_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE head_revision bigint; head_confirmed boolean;
BEGIN
 SELECT revision,confirmed INTO head_revision,head_confirmed FROM supervisor.maintenance_roster_heads
  WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND line_id=NEW.line_id AND assignee_actor_id=NEW.assignee_actor_id;
 IF head_revision IS DISTINCT FROM NEW.revision OR head_confirmed IS DISTINCT FROM NEW.confirmed THEN
  RAISE EXCEPTION 'a roster event must match its head' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER maintenance_roster_event_guard BEFORE INSERT ON supervisor.maintenance_roster_events
 FOR EACH ROW EXECUTE FUNCTION supervisor.maintenance_roster_event_guard();

DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['maintenance_roster_heads','maintenance_roster_events','maintenance_roster_receipts'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['maintenance_roster_events','maintenance_roster_receipts'] LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t||'_immutable',t);
 END LOOP;
 GRANT UPDATE(confirmed,revision,updated_at) ON supervisor.maintenance_roster_heads TO supervisor_runtime;
END $$;
