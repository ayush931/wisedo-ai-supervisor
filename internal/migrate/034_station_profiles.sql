-- WIS-520 station profiles: the operator screen configured per workstation, appended as
-- immutable StationProfileBodyV1 revisions with a canonical payload digest. The head row is
-- the site's workstation registry entry (one per workstation); a revision supersedes the
-- previous one at once and history is never rewritten. A profile is configuration data
-- only: a fixed template, typed fields and instruction references, never executable content.
CREATE TABLE supervisor.station_profile_heads (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,workstation_id uuid NOT NULL,
 current_revision bigint NOT NULL CHECK(current_revision>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,workstation_id),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id)
);
CREATE TABLE supervisor.station_profile_revisions (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,workstation_id uuid NOT NULL,revision bigint NOT NULL CHECK(revision>0),
 payload jsonb NOT NULL,
 canonical_payload text NOT NULL CHECK(octet_length(canonical_payload) BETWEEN 1 AND 1048576),
 canonical_payload_sha256 text NOT NULL CHECK(canonical_payload_sha256=encode(sha256(convert_to(canonical_payload,'UTF8')),'hex')),
 digest_schema_version integer NOT NULL CHECK(digest_schema_version=1),
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 -- The digest body is the stored payload, bound to this schema, scope and workstation.
 CHECK(canonical_payload::jsonb->>'schema'='StationProfileBodyV1' AND (canonical_payload::jsonb->>'schema_version')::integer=digest_schema_version),
 CHECK(canonical_payload::jsonb->>'tenant_id'=tenant_id::text AND canonical_payload::jsonb->>'site_id'=site_id::text),
 CHECK(canonical_payload::jsonb->'body'=payload AND (payload->>'schema_version')::integer=1 AND payload->>'workstation_id'=workstation_id::text),
 CHECK(payload->>'template' IN ('production','quality','material')),
 PRIMARY KEY(tenant_id,site_id,workstation_id,revision),
 FOREIGN KEY(tenant_id,site_id,workstation_id) REFERENCES supervisor.station_profile_heads(tenant_id,site_id,workstation_id)
);
ALTER TABLE supervisor.station_profile_heads ADD CONSTRAINT station_profile_current_fk FOREIGN KEY(tenant_id,site_id,workstation_id,current_revision)
 REFERENCES supervisor.station_profile_revisions(tenant_id,site_id,workstation_id,revision) DEFERRABLE INITIALLY DEFERRED;
CREATE TABLE supervisor.station_profile_receipts (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,actor_issuer text NOT NULL,actor_subject text NOT NULL,
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 workstation_id uuid NOT NULL,revision bigint NOT NULL,
 PRIMARY KEY(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id,workstation_id,revision) REFERENCES supervisor.station_profile_revisions(tenant_id,site_id,workstation_id,revision)
);
-- A preview names its workstation even when no profile exists yet, so only events that
-- carry a revision reference a stored profile revision.
CREATE TABLE supervisor.station_profile_audit (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),tenant_id uuid NOT NULL,site_id uuid NOT NULL,
 actor_issuer text NOT NULL,actor_subject text NOT NULL,workstation_id uuid,revision bigint,
 event_type text NOT NULL CHECK(event_type IN ('station_profile.created','station_profile.revised','station_profile.read','station_profile.listed','station_profile.previewed','mutation.replayed')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((event_type='station_profile.listed')=(workstation_id IS NULL)),
 CHECK((event_type IN ('station_profile.listed','station_profile.previewed'))=(revision IS NULL)),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 FOREIGN KEY(tenant_id,site_id,workstation_id,revision) REFERENCES supervisor.station_profile_revisions(tenant_id,site_id,workstation_id,revision)
);

-- The head only advances by one revision; identity and creation time never change.
CREATE FUNCTION supervisor.station_profile_head_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Station profile tenant scope denied' USING ERRCODE='42501'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.current_revision<>1 THEN RAISE EXCEPTION 'Station profile head must start at one' USING ERRCODE='23514'; END IF;
 ELSIF ROW(NEW.tenant_id,NEW.site_id,NEW.workstation_id,NEW.created_at) IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.workstation_id,OLD.created_at)
  OR NEW.current_revision<>OLD.current_revision+1 THEN
  RAISE EXCEPTION 'Invalid station profile head transition' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER station_profile_head_guard BEFORE INSERT OR UPDATE ON supervisor.station_profile_heads
 FOR EACH ROW EXECUTE FUNCTION supervisor.station_profile_head_guard();

-- Revisions are appended only as the head's new current revision: no gaps, no back-fill.
CREATE FUNCTION supervisor.station_profile_revision_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE head bigint;
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Station profile revision tenant scope denied' USING ERRCODE='42501'; END IF;
 SELECT current_revision INTO head FROM supervisor.station_profile_heads WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND workstation_id=NEW.workstation_id;
 IF head IS DISTINCT FROM NEW.revision THEN
  RAISE EXCEPTION 'Station profile revision is not the head revision' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER station_profile_revision_guard BEFORE INSERT ON supervisor.station_profile_revisions
 FOR EACH ROW EXECUTE FUNCTION supervisor.station_profile_revision_guard();

DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['station_profile_revisions','station_profile_receipts','station_profile_audit'] LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t||'_immutable',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['station_profile_heads','station_profile_revisions','station_profile_receipts','station_profile_audit'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP;
 GRANT UPDATE(current_revision) ON supervisor.station_profile_heads TO supervisor_runtime;
END $$;
