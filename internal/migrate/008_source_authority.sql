-- Owner decisions establish evidence quality for an exact source revision, never tool permissions.
CREATE TABLE supervisor.evidence_authority_heads (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,source_id uuid NOT NULL,
 current_revision bigint NOT NULL CHECK(current_revision>0),
 PRIMARY KEY(tenant_id,site_id,source_id),
 FOREIGN KEY(tenant_id,site_id,source_id) REFERENCES supervisor.evidence_heads(tenant_id,site_id,source_id)
);
ALTER TABLE supervisor.evidence_revisions ADD CONSTRAINT evidence_revision_hash_key UNIQUE(tenant_id,site_id,source_id,revision,sha256);
CREATE TABLE supervisor.evidence_authority_decisions (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,source_id uuid NOT NULL,
 revision bigint NOT NULL CHECK(revision>0),decision_id uuid NOT NULL UNIQUE,
 source_revision bigint NOT NULL,source_sha256 text NOT NULL,source_access_epoch bigint NOT NULL CHECK(source_access_epoch>0),
 verdict text NOT NULL CHECK(verdict IN ('designate','withdraw')),reason text NOT NULL CHECK(length(btrim(reason)) BETWEEN 1 AND 1000),
 actor_issuer text NOT NULL,actor_subject text NOT NULL,auth_time timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK(auth_time<=created_at AND auth_time>=created_at-interval '15 minutes'),
 PRIMARY KEY(tenant_id,site_id,source_id,revision),
 FOREIGN KEY(tenant_id,site_id,source_id) REFERENCES supervisor.evidence_authority_heads(tenant_id,site_id,source_id),
 FOREIGN KEY(tenant_id,site_id,source_id,source_revision,source_sha256) REFERENCES supervisor.evidence_revisions(tenant_id,site_id,source_id,revision,sha256)
);
ALTER TABLE supervisor.evidence_authority_heads ADD CONSTRAINT evidence_authority_current_fk FOREIGN KEY(tenant_id,site_id,source_id,current_revision) REFERENCES supervisor.evidence_authority_decisions(tenant_id,site_id,source_id,revision) DEFERRABLE INITIALLY DEFERRED;
CREATE FUNCTION supervisor.evidence_authority_head_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$ BEGIN
 IF ROW(NEW.tenant_id,NEW.site_id,NEW.source_id) IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.source_id) OR NEW.current_revision<>OLD.current_revision+1 THEN RAISE EXCEPTION 'Invalid authority revision transition' USING ERRCODE='42501'; END IF;
 RETURN NEW; END $$;
CREATE TRIGGER evidence_authority_head_guard BEFORE UPDATE ON supervisor.evidence_authority_heads FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_authority_head_guard();
CREATE TRIGGER evidence_immutable BEFORE UPDATE OR DELETE ON supervisor.evidence_authority_decisions FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
ALTER TABLE supervisor.evidence_audit DROP CONSTRAINT evidence_audit_event_type_check;
ALTER TABLE supervisor.evidence_audit ADD CONSTRAINT evidence_audit_event_type_check CHECK(event_type IN ('source.created','source.revised','source.read','source.listed','source.revoked','span.created','span.read','mutation.replayed','authority.decided','authority.read'));
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['evidence_authority_heads','evidence_authority_decisions'] LOOP
 EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
 EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
 EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
 EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
 EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP; END $$;
GRANT UPDATE(current_revision) ON supervisor.evidence_authority_heads TO supervisor_runtime;
