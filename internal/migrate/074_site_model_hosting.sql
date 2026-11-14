-- WIS-623 (policy: rule 08 "Model endpoints", WIS-622): the per-site switch for hosted model
-- inference. Off by default: a site without a head row never calls a hosted endpoint. Only an
-- owner of the site decides, with a reason and (to turn it on) a data processing agreement
-- reference; every decision is an append-only event, and the head is the current state.
CREATE TABLE supervisor.site_model_hosting_heads (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,
 enabled boolean NOT NULL,
 dpa_reference text CHECK(dpa_reference IS NULL OR char_length(dpa_reference) BETWEEN 1 AND 200),
 revision bigint NOT NULL CHECK(revision>0),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id),
 CHECK(NOT enabled OR dpa_reference IS NOT NULL),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id)
);

CREATE TABLE supervisor.site_model_hosting_events (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,
 revision bigint NOT NULL CHECK(revision>0),
 enabled boolean NOT NULL,
 dpa_reference text CHECK(dpa_reference IS NULL OR char_length(dpa_reference) BETWEEN 1 AND 200),
 reason text NOT NULL CHECK(char_length(reason) BETWEEN 1 AND 2000),
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 512),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 auth_time timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,revision),
 CHECK(NOT enabled OR dpa_reference IS NOT NULL),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.site_model_hosting_heads(tenant_id,site_id)
);

CREATE TABLE supervisor.site_model_hosting_receipts (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 512),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 revision bigint NOT NULL CHECK(revision>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id,revision) REFERENCES supervisor.site_model_hosting_events(tenant_id,site_id,revision)
);

CREATE FUNCTION supervisor.site_model_hosting_head_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'model hosting decisions are retained' USING ERRCODE='42501'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.revision<>1 THEN RAISE EXCEPTION 'a model hosting switch starts at revision 1' USING ERRCODE='23514'; END IF;
  RETURN NEW;
 END IF;
 IF ROW(NEW.tenant_id,NEW.site_id) IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id) OR NEW.revision<>OLD.revision+1
    OR ROW(NEW.enabled,NEW.dpa_reference) IS NOT DISTINCT FROM ROW(OLD.enabled,OLD.dpa_reference) THEN
  RAISE EXCEPTION 'a model hosting decision changes the state and advances the revision by one' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER site_model_hosting_head_guard BEFORE INSERT OR UPDATE OR DELETE ON supervisor.site_model_hosting_heads
 FOR EACH ROW EXECUTE FUNCTION supervisor.site_model_hosting_head_guard();

-- An event must describe the head it lands on: same state, same revision.
CREATE FUNCTION supervisor.site_model_hosting_event_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE head_revision bigint; head_enabled boolean; head_dpa text;
BEGIN
 SELECT revision,enabled,dpa_reference INTO head_revision,head_enabled,head_dpa FROM supervisor.site_model_hosting_heads
  WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id;
 IF head_revision IS DISTINCT FROM NEW.revision OR head_enabled IS DISTINCT FROM NEW.enabled OR head_dpa IS DISTINCT FROM NEW.dpa_reference THEN
  RAISE EXCEPTION 'a model hosting event must match its head' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER site_model_hosting_event_guard BEFORE INSERT ON supervisor.site_model_hosting_events
 FOR EACH ROW EXECUTE FUNCTION supervisor.site_model_hosting_event_guard();

DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['site_model_hosting_heads','site_model_hosting_events','site_model_hosting_receipts'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['site_model_hosting_events','site_model_hosting_receipts'] LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t||'_immutable',t);
 END LOOP;
 GRANT UPDATE(enabled,dpa_reference,revision,updated_at) ON supervisor.site_model_hosting_heads TO supervisor_runtime;
END $$;

-- A run records where it was pinned: the hosted region and the price version (both NULL for a
-- local or fixture run). Set once at creation; the run guard keeps them fixed.
ALTER TABLE supervisor.agent_runs
 ADD COLUMN provider_region text CHECK(provider_region IS NULL OR length(provider_region) BETWEEN 1 AND 64),
 ADD COLUMN price_version text CHECK(price_version IS NULL OR length(price_version) BETWEEN 1 AND 64);
