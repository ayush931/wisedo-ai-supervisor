-- Reviewed operational-source and exact knowledge-entity mapping registry.
-- Payloads retain opaque credential references only; secret values never belong here.
CREATE TABLE supervisor.operational_source_heads (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,source_id uuid NOT NULL,
 current_revision bigint NOT NULL CHECK(current_revision>0),
 PRIMARY KEY(tenant_id,site_id,source_id),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id)
);
CREATE TABLE supervisor.operational_source_mapping_heads (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,source_id uuid NOT NULL,
 current_revision bigint NOT NULL CHECK(current_revision>0),
 PRIMARY KEY(tenant_id,site_id,source_id),
 FOREIGN KEY(tenant_id,site_id,source_id) REFERENCES supervisor.operational_source_heads(tenant_id,site_id,source_id)
);
CREATE TABLE supervisor.operational_source_mapping_revisions (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,source_id uuid NOT NULL,revision bigint NOT NULL CHECK(revision>0),
 mapping_sha256 text NOT NULL CHECK(mapping_sha256 ~ '^[0-9a-f]{64}$'),
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,source_id,revision),
 UNIQUE(tenant_id,site_id,source_id,revision,mapping_sha256),
 FOREIGN KEY(tenant_id,site_id,source_id) REFERENCES supervisor.operational_source_mapping_heads(tenant_id,site_id,source_id)
);
ALTER TABLE supervisor.operational_source_mapping_heads ADD CONSTRAINT operational_source_mapping_current_fk
 FOREIGN KEY(tenant_id,site_id,source_id,current_revision)
 REFERENCES supervisor.operational_source_mapping_revisions(tenant_id,site_id,source_id,revision) DEFERRABLE INITIALLY DEFERRED;
CREATE TABLE supervisor.operational_source_mappings (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,source_id uuid NOT NULL,mapping_revision bigint NOT NULL,
 external_type text NOT NULL CHECK(external_type IN ('asset','order')),
 external_key text NOT NULL CHECK(length(external_key) BETWEEN 1 AND 512 AND external_key=btrim(external_key)),
 entity_id uuid NOT NULL,entity_revision bigint NOT NULL CHECK(entity_revision>0),
 PRIMARY KEY(tenant_id,site_id,source_id,mapping_revision,external_type,external_key),
 FOREIGN KEY(tenant_id,site_id,source_id,mapping_revision)
  REFERENCES supervisor.operational_source_mapping_revisions(tenant_id,site_id,source_id,revision),
 FOREIGN KEY(tenant_id,site_id,entity_id,entity_revision)
  REFERENCES supervisor.knowledge_entity_revisions(tenant_id,site_id,id,revision),
 UNIQUE(tenant_id,site_id,source_id,mapping_revision,external_type,entity_id,entity_revision)
);
-- PostgreSQL's built-in SHA-256 hashes the same ordered JSON shape as Go's
-- json.Marshal([]Mapping); pgcrypto is not required or installed.
CREATE FUNCTION supervisor.operational_json_string(value text) RETURNS text
 LANGUAGE sql IMMUTABLE PARALLEL SAFE SET search_path=pg_catalog AS $$
 SELECT replace(replace(replace(replace(replace(to_json(value)::text,'&',E'\\u0026'),'<',E'\\u003c'),'>',E'\\u003e'),U&'\2028',E'\\u2028'),U&'\2029',E'\\u2029')
$$;
CREATE FUNCTION supervisor.operational_mapping_digest(p_tenant uuid,p_site uuid,p_source uuid,p_revision bigint) RETURNS text
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,supervisor,pg_temp SET row_security=on AS $$
 SELECT encode(pg_catalog.sha256(convert_to(coalesce('[' || string_agg(
  '{"external_type":' || supervisor.operational_json_string(m.external_type) ||
  ',"external_key":' || supervisor.operational_json_string(m.external_key) ||
  ',"entity_id":' || supervisor.operational_json_string(m.entity_id::text) ||
  ',"entity_revision":' || m.entity_revision::text || '}',
  ',' ORDER BY m.external_type COLLATE "C",m.external_key COLLATE "C") || ']','[]'),'UTF8')),'hex')
 FROM supervisor.operational_source_mappings m
 WHERE m.tenant_id=p_tenant AND m.site_id=p_site AND m.source_id=p_source AND m.mapping_revision=p_revision
$$;
REVOKE ALL ON FUNCTION supervisor.operational_json_string(text) FROM PUBLIC,supervisor_runtime;
REVOKE ALL ON FUNCTION supervisor.operational_mapping_digest(uuid,uuid,uuid,bigint) FROM PUBLIC,supervisor_runtime;
CREATE TABLE supervisor.operational_source_revisions (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,source_id uuid NOT NULL,revision bigint NOT NULL CHECK(revision>0),
 schema_version integer NOT NULL DEFAULT 1 CHECK(schema_version=1),
 source_type text NOT NULL CHECK(source_type IN ('csv','json','event_api','manual')),
 authority_scope text NOT NULL CHECK(length(authority_scope) BETWEEN 1 AND 255 AND authority_scope=btrim(authority_scope)),
 credential_ref text CHECK(credential_ref IS NULL OR length(credential_ref) BETWEEN 1 AND 512),
 mapping_revision bigint NOT NULL CHECK(mapping_revision>0),mapping_sha256 text NOT NULL CHECK(mapping_sha256 ~ '^[0-9a-f]{64}$'),
 enabled boolean NOT NULL,access_policy_revision bigint NOT NULL CHECK(access_policy_revision>0),
 freshness_seconds bigint NOT NULL CHECK(freshness_seconds BETWEEN 1 AND 31536000),
 clock_skew_seconds bigint NOT NULL CHECK(clock_skew_seconds BETWEEN 0 AND 86400),
 completeness text NOT NULL DEFAULT 'contiguous_sequence' CHECK(completeness='contiguous_sequence'),
 config_sha256 text NOT NULL CHECK(config_sha256 ~ '^[0-9a-f]{64}$'),
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 auth_time timestamptz NOT NULL,created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK(auth_time<=created_at AND auth_time>=created_at-interval '15 minutes'),
 PRIMARY KEY(tenant_id,site_id,source_id,revision),
 UNIQUE(tenant_id,site_id,source_id,revision,config_sha256,mapping_revision,mapping_sha256),
 UNIQUE(tenant_id,site_id,source_id,revision,mapping_revision),
 FOREIGN KEY(tenant_id,site_id,source_id) REFERENCES supervisor.operational_source_heads(tenant_id,site_id,source_id),
 FOREIGN KEY(tenant_id,site_id,source_id,mapping_revision,mapping_sha256)
  REFERENCES supervisor.operational_source_mapping_revisions(tenant_id,site_id,source_id,revision,mapping_sha256)
);
ALTER TABLE supervisor.operational_source_heads ADD CONSTRAINT operational_source_current_fk
 FOREIGN KEY(tenant_id,site_id,source_id,current_revision)
 REFERENCES supervisor.operational_source_revisions(tenant_id,site_id,source_id,revision) DEFERRABLE INITIALLY DEFERRED;
CREATE TABLE supervisor.operational_source_review_heads (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,source_id uuid NOT NULL,
 current_revision bigint NOT NULL CHECK(current_revision>0),
 PRIMARY KEY(tenant_id,site_id,source_id),
 FOREIGN KEY(tenant_id,site_id,source_id) REFERENCES supervisor.operational_source_heads(tenant_id,site_id,source_id)
);
CREATE TABLE supervisor.operational_source_reviews (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,source_id uuid NOT NULL,revision bigint NOT NULL CHECK(revision>0),
 decision_id uuid NOT NULL UNIQUE,source_revision bigint NOT NULL,config_sha256 text NOT NULL,
 mapping_revision bigint NOT NULL,mapping_sha256 text NOT NULL,
 verdict text NOT NULL CHECK(verdict IN ('approved','rejected')),
 reason text NOT NULL CHECK(length(btrim(reason)) BETWEEN 1 AND 1000),
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 auth_time timestamptz NOT NULL,created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK(auth_time<=created_at AND auth_time>=created_at-interval '15 minutes'),
 PRIMARY KEY(tenant_id,site_id,source_id,revision),
 UNIQUE(tenant_id,site_id,source_id,revision,source_revision),
 FOREIGN KEY(tenant_id,site_id,source_id) REFERENCES supervisor.operational_source_review_heads(tenant_id,site_id,source_id),
 FOREIGN KEY(tenant_id,site_id,source_id,source_revision,config_sha256,mapping_revision,mapping_sha256)
  REFERENCES supervisor.operational_source_revisions(tenant_id,site_id,source_id,revision,config_sha256,mapping_revision,mapping_sha256)
);
ALTER TABLE supervisor.operational_source_review_heads ADD CONSTRAINT operational_source_review_current_fk
 FOREIGN KEY(tenant_id,site_id,source_id,current_revision)
 REFERENCES supervisor.operational_source_reviews(tenant_id,site_id,source_id,revision) DEFERRABLE INITIALLY DEFERRED;
CREATE TABLE supervisor.operational_source_receipts (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,actor_issuer text NOT NULL,actor_subject text NOT NULL,
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 operation text NOT NULL CHECK(operation IN ('source','mapping','review')),
 source_id uuid NOT NULL,revision bigint NOT NULL,
 source_revision bigint GENERATED ALWAYS AS (CASE WHEN operation IN ('source','mapping') THEN revision END) STORED,
 review_revision bigint GENERATED ALWAYS AS (CASE WHEN operation='review' THEN revision END) STORED,
 PRIMARY KEY(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 FOREIGN KEY(tenant_id,site_id,source_id,source_revision) REFERENCES supervisor.operational_source_revisions(tenant_id,site_id,source_id,revision),
 FOREIGN KEY(tenant_id,site_id,source_id,review_revision) REFERENCES supervisor.operational_source_reviews(tenant_id,site_id,source_id,revision)
);
CREATE TABLE supervisor.operational_source_audit (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),tenant_id uuid NOT NULL,site_id uuid NOT NULL,
 actor_issuer text NOT NULL,actor_subject text NOT NULL,source_id uuid NOT NULL,revision bigint NOT NULL,
 operation text CHECK(operation IS NULL OR operation IN ('source','mapping','review')),
 source_revision bigint,mapping_revision bigint,review_revision bigint,
 event_type text NOT NULL CHECK(event_type IN ('source.created','source.revised','source.read','source.revoked','mapping.revised','mapping.read','review.decided','review.read','mutation.replayed')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((event_type='mutation.replayed' AND operation IS NOT NULL) OR (event_type<>'mutation.replayed' AND operation IS NULL)),
 CHECK(
  (event_type IN ('source.created','source.revised','source.read','source.revoked') AND source_revision=revision AND mapping_revision IS NULL AND review_revision IS NULL) OR
  (event_type IN ('mapping.revised','mapping.read') AND source_revision IS NOT NULL AND mapping_revision=revision AND review_revision IS NULL) OR
  (event_type IN ('review.decided','review.read') AND source_revision IS NOT NULL AND mapping_revision IS NULL AND review_revision=revision) OR
  (event_type='mutation.replayed' AND operation IN ('source','mapping') AND source_revision=revision AND mapping_revision IS NULL AND review_revision IS NULL) OR
  (event_type='mutation.replayed' AND operation='review' AND source_revision IS NULL AND mapping_revision IS NULL AND review_revision=revision)
 ),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 FOREIGN KEY(tenant_id,site_id,source_id,source_revision) REFERENCES supervisor.operational_source_revisions(tenant_id,site_id,source_id,revision),
 FOREIGN KEY(tenant_id,site_id,source_id,mapping_revision) REFERENCES supervisor.operational_source_mapping_revisions(tenant_id,site_id,source_id,revision),
 FOREIGN KEY(tenant_id,site_id,source_id,source_revision,mapping_revision) REFERENCES supervisor.operational_source_revisions(tenant_id,site_id,source_id,revision,mapping_revision),
 FOREIGN KEY(tenant_id,site_id,source_id,review_revision) REFERENCES supervisor.operational_source_reviews(tenant_id,site_id,source_id,revision),
 FOREIGN KEY(tenant_id,site_id,source_id,review_revision,source_revision) REFERENCES supervisor.operational_source_reviews(tenant_id,site_id,source_id,revision,source_revision)
);

CREATE FUNCTION supervisor.operational_source_head_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='INSERT' THEN
  IF NEW.current_revision<>1 OR current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
   RAISE EXCEPTION 'Invalid operational source head creation' USING ERRCODE='42501'; END IF;
 ELSE
  IF ROW(NEW.tenant_id,NEW.site_id,NEW.source_id) IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.source_id)
   OR NEW.current_revision<>OLD.current_revision+1
   OR current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
   RAISE EXCEPTION 'Invalid operational source revision transition' USING ERRCODE='42501'; END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER operational_source_head_guard BEFORE INSERT OR UPDATE ON supervisor.operational_source_heads
 FOR EACH ROW EXECUTE FUNCTION supervisor.operational_source_head_guard();
CREATE TRIGGER operational_source_mapping_head_guard BEFORE INSERT OR UPDATE ON supervisor.operational_source_mapping_heads
 FOR EACH ROW EXECUTE FUNCTION supervisor.operational_source_head_guard();
CREATE TRIGGER operational_source_review_head_guard BEFORE INSERT OR UPDATE ON supervisor.operational_source_review_heads
 FOR EACH ROW EXECUTE FUNCTION supervisor.operational_source_head_guard();

CREATE FUNCTION supervisor.operational_source_revision_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE expected_revision bigint;
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Operational source tenant scope denied' USING ERRCODE='42501'; END IF;
 IF TG_TABLE_NAME='operational_source_revisions' THEN
  SELECT current_revision INTO expected_revision FROM supervisor.operational_source_heads WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND source_id=NEW.source_id;
 ELSIF TG_TABLE_NAME='operational_source_mapping_revisions' THEN
  SELECT current_revision INTO expected_revision FROM supervisor.operational_source_mapping_heads WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND source_id=NEW.source_id;
 ELSE
  SELECT current_revision INTO expected_revision FROM supervisor.operational_source_review_heads WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND source_id=NEW.source_id;
 END IF;
 IF expected_revision IS DISTINCT FROM NEW.revision THEN RAISE EXCEPTION 'Operational source revision is not current head' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER operational_source_revision_guard BEFORE INSERT ON supervisor.operational_source_revisions
 FOR EACH ROW EXECUTE FUNCTION supervisor.operational_source_revision_guard();
CREATE TRIGGER operational_source_mapping_revision_guard BEFORE INSERT ON supervisor.operational_source_mapping_revisions
 FOR EACH ROW EXECUTE FUNCTION supervisor.operational_source_revision_guard();
CREATE TRIGGER operational_source_review_guard BEFORE INSERT ON supervisor.operational_source_reviews
 FOR EACH ROW EXECUTE FUNCTION supervisor.operational_source_revision_guard();
CREATE FUNCTION supervisor.operational_source_mapping_target_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE target_type text;
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN RAISE EXCEPTION 'Mapping tenant scope denied' USING ERRCODE='42501'; END IF;
 SELECT entity_type INTO target_type FROM supervisor.knowledge_entity_revisions
 WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND id=NEW.entity_id AND revision=NEW.entity_revision;
 IF target_type IS DISTINCT FROM NEW.external_type THEN RAISE EXCEPTION 'Mapping target type mismatch' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER operational_source_mapping_target_guard BEFORE INSERT ON supervisor.operational_source_mappings
 FOR EACH ROW EXECUTE FUNCTION supervisor.operational_source_mapping_target_guard();
CREATE FUNCTION supervisor.operational_mapping_digest_guard() RETURNS trigger LANGUAGE plpgsql
 SECURITY DEFINER SET search_path=pg_catalog,supervisor,pg_temp SET row_security=on AS $$
DECLARE scope_tenant uuid;scope_site uuid;scope_source uuid;scope_revision bigint;expected_digest text;
BEGIN
 IF TG_TABLE_NAME='operational_source_mapping_revisions' THEN
  scope_tenant:=NEW.tenant_id;scope_site:=NEW.site_id;scope_source:=NEW.source_id;scope_revision:=NEW.revision;
 ELSE
  scope_tenant:=NEW.tenant_id;scope_site:=NEW.site_id;scope_source:=NEW.source_id;scope_revision:=NEW.mapping_revision;
 END IF;
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM scope_tenant::text THEN
  RAISE EXCEPTION 'Mapping digest tenant scope denied' USING ERRCODE='42501'; END IF;
 SELECT mapping_sha256 INTO expected_digest FROM supervisor.operational_source_mapping_revisions
 WHERE tenant_id=scope_tenant AND site_id=scope_site AND source_id=scope_source AND revision=scope_revision;
 IF expected_digest IS NULL OR expected_digest IS DISTINCT FROM supervisor.operational_mapping_digest(scope_tenant,scope_site,scope_source,scope_revision) THEN
  RAISE EXCEPTION 'Operational mapping digest mismatch' USING ERRCODE='23514'; END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER operational_source_mapping_digest_guard AFTER INSERT ON supervisor.operational_source_mapping_revisions
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION supervisor.operational_mapping_digest_guard();
CREATE CONSTRAINT TRIGGER operational_source_mapping_rows_digest_guard AFTER INSERT ON supervisor.operational_source_mappings
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION supervisor.operational_mapping_digest_guard();
CREATE TRIGGER operational_source_mapping_immutable BEFORE UPDATE OR DELETE ON supervisor.operational_source_mappings
 FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
CREATE TRIGGER operational_source_mapping_revision_immutable BEFORE UPDATE OR DELETE ON supervisor.operational_source_mapping_revisions
 FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
CREATE TRIGGER operational_source_revision_immutable BEFORE UPDATE OR DELETE ON supervisor.operational_source_revisions
 FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
CREATE TRIGGER operational_source_review_immutable BEFORE UPDATE OR DELETE ON supervisor.operational_source_reviews
 FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
CREATE TRIGGER operational_source_receipt_immutable BEFORE UPDATE OR DELETE ON supervisor.operational_source_receipts
 FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
CREATE TRIGGER operational_source_audit_immutable BEFORE UPDATE OR DELETE ON supervisor.operational_source_audit
 FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();

DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['operational_source_heads','operational_source_mapping_heads','operational_source_mapping_revisions','operational_source_mappings','operational_source_revisions','operational_source_review_heads','operational_source_reviews','operational_source_receipts','operational_source_audit'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  IF t IN ('operational_source_heads','operational_source_mapping_heads','operational_source_review_heads') THEN
   EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
   EXECUTE format('GRANT UPDATE(current_revision) ON supervisor.%I TO supervisor_runtime',t);
  ELSIF t IN ('operational_source_receipts','operational_source_audit') THEN
   EXECUTE format('GRANT INSERT ON supervisor.%I TO supervisor_runtime',t);
   EXECUTE format('GRANT SELECT ON supervisor.%I TO supervisor_runtime',t);
  ELSE
   EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
  END IF;
 END LOOP;
END $$;
