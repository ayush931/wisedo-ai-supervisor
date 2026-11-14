-- WIS-520 site-scoped, revisioned master data.
CREATE TABLE supervisor.line_heads (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, id uuid NOT NULL,
 current_revision bigint NOT NULL CHECK (current_revision > 0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (tenant_id,site_id,id),
 FOREIGN KEY (tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id)
);
CREATE TABLE supervisor.line_revisions (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, object_id uuid NOT NULL,
 revision bigint NOT NULL CHECK (revision > 0), payload jsonb NOT NULL,
 canonical_payload text NOT NULL CHECK (octet_length(canonical_payload) BETWEEN 1 AND 1048576),
 canonical_payload_sha256 text NOT NULL CHECK (canonical_payload_sha256=encode(sha256(convert_to(canonical_payload,'UTF8')),'hex')),
 digest_schema_version integer NOT NULL CHECK (digest_schema_version=1),
 actor_issuer text NOT NULL CHECK (length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK (length(actor_subject) BETWEEN 1 AND 255),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK (canonical_payload::jsonb->>'schema'='LineBodyV1'),
 CHECK ((canonical_payload::jsonb->>'schema_version')::integer=digest_schema_version),
 CHECK (canonical_payload::jsonb->>'tenant_id'=tenant_id::text AND canonical_payload::jsonb->>'site_id'=site_id::text),
 CHECK (canonical_payload::jsonb->'body'=payload AND (payload->>'schema_version')::integer=1),
 PRIMARY KEY (tenant_id,site_id,object_id,revision),
 FOREIGN KEY (tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 FOREIGN KEY (tenant_id,site_id,object_id) REFERENCES supervisor.line_heads(tenant_id,site_id,id)
);
ALTER TABLE supervisor.line_heads ADD CONSTRAINT line_current_fk
 FOREIGN KEY (tenant_id,site_id,id,current_revision)
 REFERENCES supervisor.line_revisions(tenant_id,site_id,object_id,revision) DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE supervisor.item_heads (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, id uuid NOT NULL,
 current_revision bigint NOT NULL CHECK (current_revision > 0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (tenant_id,site_id,id),
 FOREIGN KEY (tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id)
);
CREATE TABLE supervisor.item_revisions (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, object_id uuid NOT NULL,
 revision bigint NOT NULL CHECK (revision > 0), payload jsonb NOT NULL,
 canonical_payload text NOT NULL CHECK (octet_length(canonical_payload) BETWEEN 1 AND 1048576),
 canonical_payload_sha256 text NOT NULL CHECK (canonical_payload_sha256=encode(sha256(convert_to(canonical_payload,'UTF8')),'hex')),
 digest_schema_version integer NOT NULL CHECK (digest_schema_version=1),
 actor_issuer text NOT NULL CHECK (length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK (length(actor_subject) BETWEEN 1 AND 255),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK (canonical_payload::jsonb->>'schema'='ItemBodyV1'),
 CHECK ((canonical_payload::jsonb->>'schema_version')::integer=digest_schema_version),
 CHECK (canonical_payload::jsonb->>'tenant_id'=tenant_id::text AND canonical_payload::jsonb->>'site_id'=site_id::text),
 CHECK (canonical_payload::jsonb->'body'=payload AND (payload->>'schema_version')::integer=1),
 PRIMARY KEY (tenant_id,site_id,object_id,revision),
 FOREIGN KEY (tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 FOREIGN KEY (tenant_id,site_id,object_id) REFERENCES supervisor.item_heads(tenant_id,site_id,id)
);
ALTER TABLE supervisor.item_heads ADD CONSTRAINT item_current_fk
 FOREIGN KEY (tenant_id,site_id,id,current_revision)
 REFERENCES supervisor.item_revisions(tenant_id,site_id,object_id,revision) DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE supervisor.routing_heads (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, id uuid NOT NULL,
 current_revision bigint NOT NULL CHECK (current_revision > 0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (tenant_id,site_id,id),
 FOREIGN KEY (tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id)
);
CREATE TABLE supervisor.routing_revisions (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, object_id uuid NOT NULL,
 revision bigint NOT NULL CHECK (revision > 0), line_id uuid NOT NULL, item_id uuid NOT NULL,
 payload jsonb NOT NULL,
 canonical_payload text NOT NULL CHECK (octet_length(canonical_payload) BETWEEN 1 AND 1048576),
 canonical_payload_sha256 text NOT NULL CHECK (canonical_payload_sha256=encode(sha256(convert_to(canonical_payload,'UTF8')),'hex')),
 digest_schema_version integer NOT NULL CHECK (digest_schema_version=1),
 actor_issuer text NOT NULL CHECK (length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK (length(actor_subject) BETWEEN 1 AND 255),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK (canonical_payload::jsonb->>'schema'='RoutingBodyV1'),
 CHECK ((canonical_payload::jsonb->>'schema_version')::integer=digest_schema_version),
 CHECK (canonical_payload::jsonb->>'tenant_id'=tenant_id::text AND canonical_payload::jsonb->>'site_id'=site_id::text),
 CHECK (canonical_payload::jsonb->'body'=payload AND (payload->>'schema_version')::integer=1),
 CHECK (payload->>'line_id'=line_id::text AND payload->>'item_id'=item_id::text),
 PRIMARY KEY (tenant_id,site_id,object_id,revision),
 FOREIGN KEY (tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 FOREIGN KEY (tenant_id,site_id,object_id) REFERENCES supervisor.routing_heads(tenant_id,site_id,id),
 FOREIGN KEY (tenant_id,site_id,line_id) REFERENCES supervisor.line_heads(tenant_id,site_id,id),
 FOREIGN KEY (tenant_id,site_id,item_id) REFERENCES supervisor.item_heads(tenant_id,site_id,id)
);
ALTER TABLE supervisor.routing_heads ADD CONSTRAINT routing_current_fk
 FOREIGN KEY (tenant_id,site_id,id,current_revision)
 REFERENCES supervisor.routing_revisions(tenant_id,site_id,object_id,revision) DEFERRABLE INITIALLY DEFERRED;
CREATE TABLE supervisor.routing_steps (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, routing_id uuid NOT NULL,
 routing_revision bigint NOT NULL, step_number integer NOT NULL CHECK (step_number > 0),
 workstation_id uuid NOT NULL,
 PRIMARY KEY (tenant_id,site_id,routing_id,routing_revision,step_number),
 FOREIGN KEY (tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 FOREIGN KEY (tenant_id,site_id,routing_id,routing_revision)
  REFERENCES supervisor.routing_revisions(tenant_id,site_id,object_id,revision),
 FOREIGN KEY (tenant_id,site_id,workstation_id)
  REFERENCES supervisor.station_profile_heads(tenant_id,site_id,workstation_id)
);

CREATE TABLE supervisor.shift_calendar_heads (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, id uuid NOT NULL,
 current_revision bigint NOT NULL CHECK (current_revision > 0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (tenant_id,site_id,id),
 FOREIGN KEY (tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id)
);
CREATE TABLE supervisor.shift_calendar_revisions (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, object_id uuid NOT NULL,
 revision bigint NOT NULL CHECK (revision > 0), payload jsonb NOT NULL,
 canonical_payload text NOT NULL CHECK (octet_length(canonical_payload) BETWEEN 1 AND 1048576),
 canonical_payload_sha256 text NOT NULL CHECK (canonical_payload_sha256=encode(sha256(convert_to(canonical_payload,'UTF8')),'hex')),
 digest_schema_version integer NOT NULL CHECK (digest_schema_version=1),
 actor_issuer text NOT NULL CHECK (length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK (length(actor_subject) BETWEEN 1 AND 255),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK (canonical_payload::jsonb->>'schema'='ShiftCalendarBodyV1'),
 CHECK ((canonical_payload::jsonb->>'schema_version')::integer=digest_schema_version),
 CHECK (canonical_payload::jsonb->>'tenant_id'=tenant_id::text AND canonical_payload::jsonb->>'site_id'=site_id::text),
 CHECK (canonical_payload::jsonb->'body'=payload AND (payload->>'schema_version')::integer=1),
 PRIMARY KEY (tenant_id,site_id,object_id,revision),
 FOREIGN KEY (tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 FOREIGN KEY (tenant_id,site_id,object_id) REFERENCES supervisor.shift_calendar_heads(tenant_id,site_id,id)
);
ALTER TABLE supervisor.shift_calendar_heads ADD CONSTRAINT shift_calendar_current_fk
 FOREIGN KEY (tenant_id,site_id,id,current_revision)
 REFERENCES supervisor.shift_calendar_revisions(tenant_id,site_id,object_id,revision) DEFERRABLE INITIALLY DEFERRED;

-- No row means the explicit version-0 default: line validation off.
CREATE TABLE supervisor.site_master_data_setting_heads (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL,
 current_revision bigint NOT NULL CHECK (current_revision > 0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (tenant_id,site_id),
 FOREIGN KEY (tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id)
);
CREATE TABLE supervisor.site_master_data_setting_revisions (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, revision bigint NOT NULL CHECK (revision > 0),
 station_profile_line_validation_enabled boolean NOT NULL,
 payload jsonb NOT NULL,
 canonical_payload text NOT NULL CHECK (octet_length(canonical_payload) BETWEEN 1 AND 1048576),
 canonical_payload_sha256 text NOT NULL CHECK (canonical_payload_sha256=encode(sha256(convert_to(canonical_payload,'UTF8')),'hex')),
 digest_schema_version integer NOT NULL CHECK (digest_schema_version=1),
 actor_issuer text NOT NULL CHECK (length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK (length(actor_subject) BETWEEN 1 AND 255),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK (canonical_payload::jsonb->>'schema'='SiteMasterDataSettingsBodyV1'),
 CHECK ((canonical_payload::jsonb->>'schema_version')::integer=digest_schema_version),
 CHECK (canonical_payload::jsonb->>'tenant_id'=tenant_id::text AND canonical_payload::jsonb->>'site_id'=site_id::text),
 CHECK (canonical_payload::jsonb->'body'=payload AND (payload->>'schema_version')::integer=1),
 CHECK ((payload->>'station_profile_line_validation_enabled')::boolean=station_profile_line_validation_enabled),
 PRIMARY KEY (tenant_id,site_id,revision),
 FOREIGN KEY (tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 FOREIGN KEY (tenant_id,site_id) REFERENCES supervisor.site_master_data_setting_heads(tenant_id,site_id)
);
ALTER TABLE supervisor.site_master_data_setting_heads ADD CONSTRAINT site_master_data_setting_current_fk
 FOREIGN KEY (tenant_id,site_id,current_revision)
 REFERENCES supervisor.site_master_data_setting_revisions(tenant_id,site_id,revision) DEFERRABLE INITIALLY DEFERRED;

-- Shared receipt records one idempotency key across every master-data mutation.
CREATE TABLE supervisor.master_data_receipts (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL,
 actor_issuer text NOT NULL CHECK (length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK (length(actor_subject) BETWEEN 1 AND 255),
 idempotency_key text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 128),
 request_sha256 text NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
 object_kind text NOT NULL CHECK (object_kind IN ('line','item','routing','shift_calendar','site_settings')),
 object_id uuid NOT NULL, revision bigint NOT NULL CHECK (revision > 0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY (tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id)
);

CREATE FUNCTION supervisor.master_data_head_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Master data tenant scope denied' USING ERRCODE='42501';
 END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.current_revision<>1 THEN
   RAISE EXCEPTION 'Master data head must start at one' USING ERRCODE='23514';
  END IF;
 ELSIF ROW(NEW.tenant_id,NEW.site_id,NEW.id,NEW.created_at)
        IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.id,OLD.created_at)
    OR NEW.current_revision<>OLD.current_revision+1 THEN
  RAISE EXCEPTION 'Invalid master data head transition' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER line_head_guard BEFORE INSERT OR UPDATE ON supervisor.line_heads
 FOR EACH ROW EXECUTE FUNCTION supervisor.master_data_head_guard();
CREATE TRIGGER item_head_guard BEFORE INSERT OR UPDATE ON supervisor.item_heads
 FOR EACH ROW EXECUTE FUNCTION supervisor.master_data_head_guard();
CREATE TRIGGER routing_head_guard BEFORE INSERT OR UPDATE ON supervisor.routing_heads
 FOR EACH ROW EXECUTE FUNCTION supervisor.master_data_head_guard();
CREATE TRIGGER shift_calendar_head_guard BEFORE INSERT OR UPDATE ON supervisor.shift_calendar_heads
 FOR EACH ROW EXECUTE FUNCTION supervisor.master_data_head_guard();

CREATE FUNCTION supervisor.master_data_revision_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE head bigint;
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Master data tenant scope denied' USING ERRCODE='42501';
 END IF;
 EXECUTE format('SELECT current_revision FROM supervisor.%I WHERE tenant_id=$1 AND site_id=$2 AND id=$3',TG_ARGV[0])
  INTO head USING NEW.tenant_id,NEW.site_id,NEW.object_id;
 IF head IS DISTINCT FROM NEW.revision THEN
  RAISE EXCEPTION 'Master data revision is not the head revision' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER line_revision_guard BEFORE INSERT ON supervisor.line_revisions
 FOR EACH ROW EXECUTE FUNCTION supervisor.master_data_revision_guard('line_heads');
CREATE TRIGGER item_revision_guard BEFORE INSERT ON supervisor.item_revisions
 FOR EACH ROW EXECUTE FUNCTION supervisor.master_data_revision_guard('item_heads');
CREATE TRIGGER routing_revision_guard BEFORE INSERT ON supervisor.routing_revisions
 FOR EACH ROW EXECUTE FUNCTION supervisor.master_data_revision_guard('routing_heads');
CREATE TRIGGER shift_calendar_revision_guard BEFORE INSERT ON supervisor.shift_calendar_revisions
 FOR EACH ROW EXECUTE FUNCTION supervisor.master_data_revision_guard('shift_calendar_heads');

CREATE FUNCTION supervisor.site_master_data_setting_head_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Master data tenant scope denied' USING ERRCODE='42501';
 END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.current_revision<>1 THEN
   RAISE EXCEPTION 'Site setting head must start at one' USING ERRCODE='23514';
  END IF;
 ELSIF ROW(NEW.tenant_id,NEW.site_id,NEW.created_at)
        IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.created_at)
    OR NEW.current_revision<>OLD.current_revision+1 THEN
  RAISE EXCEPTION 'Invalid site setting head transition' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER site_master_data_setting_head_guard
 BEFORE INSERT OR UPDATE ON supervisor.site_master_data_setting_heads
 FOR EACH ROW EXECUTE FUNCTION supervisor.site_master_data_setting_head_guard();
CREATE FUNCTION supervisor.site_master_data_setting_revision_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE head bigint;
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Master data tenant scope denied' USING ERRCODE='42501';
 END IF;
 SELECT current_revision INTO head FROM supervisor.site_master_data_setting_heads
  WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id;
 IF head IS DISTINCT FROM NEW.revision THEN
  RAISE EXCEPTION 'Site setting is not the head revision' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER site_master_data_setting_revision_guard
 BEFORE INSERT ON supervisor.site_master_data_setting_revisions
 FOR EACH ROW EXECUTE FUNCTION supervisor.site_master_data_setting_revision_guard();

DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['line_revisions','item_revisions','routing_revisions',
  'shift_calendar_revisions','site_master_data_setting_revisions','routing_steps',
  'master_data_receipts'] LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t||'_immutable',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['line_heads','line_revisions','item_heads','item_revisions',
  'routing_heads','routing_revisions','routing_steps','shift_calendar_heads',
  'shift_calendar_revisions','site_master_data_setting_heads',
  'site_master_data_setting_revisions','master_data_receipts'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP;
 GRANT UPDATE(current_revision) ON supervisor.line_heads,supervisor.item_heads,
  supervisor.routing_heads,supervisor.shift_calendar_heads,
  supervisor.site_master_data_setting_heads TO supervisor_runtime;
END $$;



CREATE INDEX routing_revisions_line_idx ON supervisor.routing_revisions(tenant_id,site_id,line_id);
CREATE INDEX routing_revisions_item_idx ON supervisor.routing_revisions(tenant_id,site_id,item_id);
CREATE INDEX routing_steps_workstation_idx ON supervisor.routing_steps(tenant_id,site_id,workstation_id);
