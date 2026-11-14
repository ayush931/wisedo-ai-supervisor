-- Explicit human choices preserve each conflicting fact and never imply approval.
CREATE TABLE supervisor.knowledge_resolution_heads (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,conflict_id uuid NOT NULL,
 current_revision bigint NOT NULL CHECK(current_revision>0),
 PRIMARY KEY(tenant_id,site_id,conflict_id),
 FOREIGN KEY(tenant_id,site_id,conflict_id) REFERENCES supervisor.knowledge_conflicts(tenant_id,site_id,id)
);
CREATE TABLE supervisor.knowledge_resolution_decisions (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,conflict_id uuid NOT NULL,revision bigint NOT NULL CHECK(revision>0),
 resolution_id uuid NOT NULL UNIQUE,schema_version integer NOT NULL DEFAULT 1 CHECK(schema_version=1),
 decision text NOT NULL CHECK(decision IN ('prefer_left','prefer_right','neither','withdraw')),
 reason text NOT NULL CHECK(length(reason) BETWEEN 1 AND 1000 AND length(btrim(reason))>0),
 actor_issuer text NOT NULL,actor_subject text NOT NULL,auth_time timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 authority_decision_id uuid,authority_source_id uuid,authority_source_revision bigint,authority_revision bigint,
 CHECK(auth_time<=created_at AND auth_time>=created_at-interval '15 minutes'),
 CHECK((decision IN ('prefer_left','prefer_right') AND authority_decision_id IS NOT NULL AND authority_source_id IS NOT NULL AND authority_source_revision IS NOT NULL AND authority_revision IS NOT NULL)
 OR (decision IN ('neither','withdraw') AND authority_decision_id IS NULL AND authority_source_id IS NULL AND authority_source_revision IS NULL AND authority_revision IS NULL)),
 PRIMARY KEY(tenant_id,site_id,conflict_id,revision),
 UNIQUE(tenant_id,site_id,conflict_id,revision,resolution_id),
 FOREIGN KEY(tenant_id,site_id,conflict_id) REFERENCES supervisor.knowledge_resolution_heads(tenant_id,site_id,conflict_id),
 FOREIGN KEY(tenant_id,site_id,authority_source_id,authority_source_revision,authority_revision,authority_decision_id)
 REFERENCES supervisor.evidence_authority_decisions(tenant_id,site_id,source_id,source_revision,revision,decision_id)
);
ALTER TABLE supervisor.knowledge_resolution_heads ADD CONSTRAINT knowledge_resolution_current_fk
 FOREIGN KEY(tenant_id,site_id,conflict_id,current_revision) REFERENCES supervisor.knowledge_resolution_decisions(tenant_id,site_id,conflict_id,revision) DEFERRABLE INITIALLY DEFERRED;
CREATE FUNCTION supervisor.knowledge_resolution_head_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$ BEGIN
 IF ROW(NEW.tenant_id,NEW.site_id,NEW.conflict_id) IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.conflict_id) OR NEW.current_revision<>OLD.current_revision+1 THEN
 RAISE EXCEPTION 'Invalid resolution revision transition' USING ERRCODE='42501'; END IF;
 RETURN NEW; END $$;
CREATE TRIGGER knowledge_resolution_head_guard BEFORE UPDATE ON supervisor.knowledge_resolution_heads FOR EACH ROW EXECUTE FUNCTION supervisor.knowledge_resolution_head_guard();
CREATE TABLE supervisor.knowledge_review_resolution_refs (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,review_id uuid NOT NULL,claim_id uuid NOT NULL,claim_revision bigint NOT NULL,
 conflict_id uuid NOT NULL,resolution_id uuid NOT NULL,resolution_revision bigint NOT NULL,
 PRIMARY KEY(tenant_id,site_id,review_id,conflict_id),
 FOREIGN KEY(tenant_id,site_id,review_id,claim_id,claim_revision)
 REFERENCES supervisor.knowledge_claim_reviews(tenant_id,site_id,review_id,claim_id,claim_revision),
 FOREIGN KEY(tenant_id,site_id,conflict_id,resolution_revision,resolution_id)
 REFERENCES supervisor.knowledge_resolution_decisions(tenant_id,site_id,conflict_id,revision,resolution_id)
);
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['knowledge_resolution_heads','knowledge_resolution_decisions','knowledge_review_resolution_refs'] LOOP
 EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
 EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
 EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
 EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
 EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 IF t<>'knowledge_resolution_heads' THEN EXECUTE format('CREATE TRIGGER knowledge_immutable BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t); END IF;
 END LOOP; END $$;
GRANT UPDATE(current_revision) ON supervisor.knowledge_resolution_heads TO supervisor_runtime;
ALTER TABLE supervisor.knowledge_receipts DROP CONSTRAINT knowledge_receipts_operation_check;
ALTER TABLE supervisor.knowledge_receipts ADD CONSTRAINT knowledge_receipts_operation_check CHECK(operation IN ('entity','claim','review','review_revoke','resolution'));
ALTER TABLE supervisor.knowledge_audit DROP CONSTRAINT knowledge_audit_event_type_check;
ALTER TABLE supervisor.knowledge_audit ADD CONSTRAINT knowledge_audit_event_type_check CHECK(event_type IN ('entity.created','entity.revised','entity.read','entity.listed','claim.created','claim.revised','claim.read','claim.listed','mutation.replayed','conflicts.read','review.decided','review.read','review.revoked','resolution.decided','resolution.read'));
