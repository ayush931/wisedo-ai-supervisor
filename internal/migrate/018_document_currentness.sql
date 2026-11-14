-- Owner verification records document version currency, never an implicit TTL.
ALTER TABLE supervisor.evidence_authority_decisions ADD CONSTRAINT evidence_authority_currentness_pin_key
 UNIQUE(tenant_id,site_id,source_id,source_revision,source_sha256,source_access_epoch,revision,decision_id);
CREATE TABLE supervisor.evidence_currentness_heads (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,source_id uuid NOT NULL,
 current_revision bigint NOT NULL CHECK(current_revision>0),
 PRIMARY KEY(tenant_id,site_id,source_id),
 FOREIGN KEY(tenant_id,site_id,source_id) REFERENCES supervisor.evidence_heads(tenant_id,site_id,source_id)
);
CREATE TABLE supervisor.evidence_currentness_decisions (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,source_id uuid NOT NULL,revision bigint NOT NULL CHECK(revision>0),
 schema_version integer NOT NULL DEFAULT 1 CHECK(schema_version=1),verification_id uuid NOT NULL UNIQUE,
 source_revision bigint NOT NULL,source_sha256 text NOT NULL,source_access_epoch bigint NOT NULL CHECK(source_access_epoch>0),
 authority_revision bigint NOT NULL,authority_decision_id uuid NOT NULL,
 verdict text NOT NULL CHECK(verdict IN ('confirm_current','withdraw')),
 reason text NOT NULL CHECK(length(reason) BETWEEN 1 AND 1000 AND length(btrim(reason))>0),
 actor_issuer text NOT NULL,actor_subject text NOT NULL,auth_time timestamptz NOT NULL,
 created_at timestamptz NOT NULL,verified_at timestamptz,
 withdrawn_revision bigint GENERATED ALWAYS AS (CASE WHEN verdict='withdraw' THEN revision-1 END) STORED,
 CHECK(withdrawn_revision IS NULL OR withdrawn_revision>0),
 CHECK(auth_time<=created_at AND auth_time>=created_at-interval '15 minutes'),
 CHECK((verdict='confirm_current' AND verified_at IS NOT NULL AND verified_at=created_at) OR (verdict='withdraw' AND verified_at IS NULL)),
 PRIMARY KEY(tenant_id,site_id,source_id,revision),
 UNIQUE(tenant_id,site_id,source_id,revision,source_revision,source_sha256,source_access_epoch,authority_revision,authority_decision_id),
 FOREIGN KEY(tenant_id,site_id,source_id) REFERENCES supervisor.evidence_currentness_heads(tenant_id,site_id,source_id),
 FOREIGN KEY(tenant_id,site_id,source_id,source_revision,source_sha256) REFERENCES supervisor.evidence_revisions(tenant_id,site_id,source_id,revision,sha256),
 FOREIGN KEY(tenant_id,site_id,source_id,source_revision,source_sha256,source_access_epoch,authority_revision,authority_decision_id)
 REFERENCES supervisor.evidence_authority_decisions(tenant_id,site_id,source_id,source_revision,source_sha256,source_access_epoch,revision,decision_id),
 FOREIGN KEY(tenant_id,site_id,source_id,withdrawn_revision,source_revision,source_sha256,source_access_epoch,authority_revision,authority_decision_id)
 REFERENCES supervisor.evidence_currentness_decisions(tenant_id,site_id,source_id,revision,source_revision,source_sha256,source_access_epoch,authority_revision,authority_decision_id)
);
ALTER TABLE supervisor.evidence_currentness_heads ADD CONSTRAINT evidence_currentness_current_fk
 FOREIGN KEY(tenant_id,site_id,source_id,current_revision) REFERENCES supervisor.evidence_currentness_decisions(tenant_id,site_id,source_id,revision) DEFERRABLE INITIALLY DEFERRED;
CREATE FUNCTION supervisor.evidence_currentness_head_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$ BEGIN
 IF ROW(NEW.tenant_id,NEW.site_id,NEW.source_id) IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.source_id) OR NEW.current_revision<>OLD.current_revision+1 THEN
 RAISE EXCEPTION 'Invalid currentness revision transition' USING ERRCODE='42501'; END IF;
 RETURN NEW; END $$;
CREATE TRIGGER evidence_currentness_head_guard BEFORE UPDATE ON supervisor.evidence_currentness_heads FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_currentness_head_guard();
-- The database owns the timestamp even for direct runtime INSERTs. A caller cannot
-- backdate or future-date either confirmation time or its matching creation time.
CREATE FUNCTION supervisor.evidence_currentness_stamp() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE source_kind text; authority_verdict text; previous_verdict text;
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
 RAISE EXCEPTION 'Currentness scope denied' USING ERRCODE='42501'; END IF;
 NEW.created_at:=clock_timestamp();
 IF NEW.verdict='confirm_current' THEN
  SELECT r.kind INTO source_kind FROM supervisor.evidence_revisions r WHERE r.tenant_id=NEW.tenant_id AND r.site_id=NEW.site_id AND r.source_id=NEW.source_id AND r.revision=NEW.source_revision;
  SELECT d.verdict INTO authority_verdict FROM supervisor.evidence_authority_decisions d WHERE d.tenant_id=NEW.tenant_id AND d.site_id=NEW.site_id AND d.source_id=NEW.source_id AND d.revision=NEW.authority_revision AND d.decision_id=NEW.authority_decision_id;
  IF source_kind IS DISTINCT FROM 'document' OR authority_verdict IS DISTINCT FROM 'designate' THEN RAISE EXCEPTION 'Invalid document authority' USING ERRCODE='23514'; END IF;
  NEW.verified_at:=NEW.created_at;
 ELSE
  SELECT d.verdict INTO previous_verdict FROM supervisor.evidence_currentness_decisions d WHERE d.tenant_id=NEW.tenant_id AND d.site_id=NEW.site_id AND d.source_id=NEW.source_id AND d.revision=NEW.revision-1;
  IF previous_verdict IS DISTINCT FROM 'confirm_current' THEN RAISE EXCEPTION 'Invalid currentness withdrawal' USING ERRCODE='23514'; END IF;
  NEW.verified_at:=NULL;
 END IF;
 RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION supervisor.evidence_currentness_stamp() FROM PUBLIC,supervisor_runtime;
CREATE TRIGGER evidence_currentness_stamp BEFORE INSERT ON supervisor.evidence_currentness_decisions FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_currentness_stamp();
CREATE TRIGGER evidence_immutable BEFORE UPDATE OR DELETE ON supervisor.evidence_currentness_decisions FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['evidence_currentness_heads','evidence_currentness_decisions'] LOOP
 EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
 EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
 EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
 EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
 EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP;
END $$;
GRANT UPDATE(current_revision) ON supervisor.evidence_currentness_heads TO supervisor_runtime;
ALTER TABLE supervisor.evidence_audit DROP CONSTRAINT evidence_audit_event_type_check;
ALTER TABLE supervisor.evidence_audit ADD CONSTRAINT evidence_audit_event_type_check CHECK(event_type IN ('source.created','source.revised','source.read','source.listed','source.revoked','span.created','span.read','mutation.replayed','authority.decided','authority.read','currentness.decided','currentness.read'));
