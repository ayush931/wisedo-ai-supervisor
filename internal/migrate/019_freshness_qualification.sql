-- Explicit site policy and immutable qualification pins; no graph epoch changes.
CREATE TABLE supervisor.document_freshness_policy_heads (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,current_revision bigint NOT NULL CHECK(current_revision>0),
 PRIMARY KEY(tenant_id,site_id),FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id)
);
CREATE TABLE supervisor.document_freshness_policy_decisions (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,revision bigint NOT NULL CHECK(revision>0),
 decision_id uuid NOT NULL UNIQUE,schema_version integer NOT NULL DEFAULT 1 CHECK(schema_version=1),
 basis text NOT NULL DEFAULT 'owner_currentness_verified_at' CHECK(basis='owner_currentness_verified_at'),
 verdict text NOT NULL CHECK(verdict IN ('define','withdraw')),max_age_seconds bigint,
 reason text NOT NULL CHECK(length(reason) BETWEEN 1 AND 1000 AND length(btrim(reason))>0),
 actor_issuer text NOT NULL,actor_subject text NOT NULL,auth_time timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 withdrawn_revision bigint GENERATED ALWAYS AS (CASE WHEN verdict='withdraw' THEN revision-1 END) STORED,
 CHECK(withdrawn_revision IS NULL OR withdrawn_revision>0),
 CHECK(auth_time<=created_at AND auth_time>=created_at-interval '15 minutes'),
 CHECK((verdict='define' AND max_age_seconds IS NOT NULL AND max_age_seconds BETWEEN 1 AND 9223372036) OR (verdict='withdraw' AND max_age_seconds IS NULL)),
 PRIMARY KEY(tenant_id,site_id,revision),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.document_freshness_policy_heads(tenant_id,site_id),
 FOREIGN KEY(tenant_id,site_id,withdrawn_revision) REFERENCES supervisor.document_freshness_policy_decisions(tenant_id,site_id,revision)
);
ALTER TABLE supervisor.document_freshness_policy_heads ADD CONSTRAINT document_freshness_policy_current_fk
 FOREIGN KEY(tenant_id,site_id,current_revision) REFERENCES supervisor.document_freshness_policy_decisions(tenant_id,site_id,revision) DEFERRABLE INITIALLY DEFERRED;
CREATE FUNCTION supervisor.document_freshness_policy_head_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$ BEGIN
 IF ROW(NEW.tenant_id,NEW.site_id) IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id) OR NEW.current_revision<>OLD.current_revision+1 THEN
 RAISE EXCEPTION 'Invalid freshness policy revision' USING ERRCODE='42501'; END IF;
 RETURN NEW; END $$;
CREATE TRIGGER document_freshness_policy_head_guard BEFORE UPDATE ON supervisor.document_freshness_policy_heads FOR EACH ROW EXECUTE FUNCTION supervisor.document_freshness_policy_head_guard();
CREATE FUNCTION supervisor.document_freshness_policy_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE prior_verdict text;
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN RAISE EXCEPTION 'Freshness policy scope denied' USING ERRCODE='42501'; END IF;
 IF NEW.verdict='withdraw' THEN
 SELECT verdict INTO prior_verdict FROM supervisor.document_freshness_policy_decisions WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND revision=NEW.revision-1;
 IF prior_verdict IS DISTINCT FROM 'define' THEN RAISE EXCEPTION 'Invalid freshness policy withdrawal' USING ERRCODE='23514'; END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER document_freshness_policy_guard BEFORE INSERT ON supervisor.document_freshness_policy_decisions FOR EACH ROW EXECUTE FUNCTION supervisor.document_freshness_policy_guard();
CREATE TRIGGER knowledge_immutable BEFORE UPDATE OR DELETE ON supervisor.document_freshness_policy_decisions FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();

ALTER TABLE supervisor.knowledge_requirement_decisions ADD COLUMN freshness_policy_revision bigint,
 ADD COLUMN verification_count integer NOT NULL DEFAULT 0 CHECK(verification_count BETWEEN 0 AND 500),
 ADD CONSTRAINT requirement_freshness_policy_fk FOREIGN KEY(tenant_id,site_id,freshness_policy_revision) REFERENCES supervisor.document_freshness_policy_decisions(tenant_id,site_id,revision);
ALTER TABLE supervisor.knowledge_requirement_decisions DROP CONSTRAINT knowledge_requirement_decisions_check1;
ALTER TABLE supervisor.knowledge_requirement_decisions ADD CONSTRAINT requirement_binding_shape CHECK(
 (verdict='bind' AND snapshot_id IS NOT NULL AND snapshot_sha256 IS NOT NULL AND snapshot_inventory_revision IS NOT NULL AND as_of IS NOT NULL AND claim_count>0
 AND ((freshness_policy_revision IS NULL AND verification_count=0) OR (freshness_policy_revision IS NOT NULL AND freshness_policy_revision>0 AND verification_count>0)))
 OR (verdict='bind' AND snapshot_id IS NULL AND snapshot_sha256 IS NULL AND snapshot_inventory_revision IS NULL AND as_of IS NULL AND claim_count=0 AND freshness_policy_revision IS NOT NULL AND freshness_policy_revision>0 AND verification_count=0)
 OR (verdict IN ('not_applicable','withdraw') AND snapshot_id IS NULL AND snapshot_sha256 IS NULL AND snapshot_inventory_revision IS NULL AND as_of IS NULL AND claim_count=0 AND freshness_policy_revision IS NULL AND verification_count=0)
);
ALTER TABLE supervisor.evidence_currentness_decisions ADD CONSTRAINT currentness_qualification_pin_key
 UNIQUE(tenant_id,site_id,source_id,revision,verification_id,source_revision,source_sha256,source_access_epoch);
CREATE TABLE supervisor.knowledge_requirement_decision_verifications (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,inventory_id uuid NOT NULL,inventory_revision bigint NOT NULL,requirement_id uuid NOT NULL,decision_revision bigint NOT NULL,
 source_id uuid NOT NULL,source_revision bigint NOT NULL,source_sha256 text NOT NULL,source_access_epoch bigint NOT NULL,
 verification_id uuid NOT NULL,verification_revision bigint NOT NULL,
 PRIMARY KEY(tenant_id,site_id,inventory_id,inventory_revision,requirement_id,decision_revision,source_id),
 FOREIGN KEY(tenant_id,site_id,inventory_id,inventory_revision,requirement_id,decision_revision) REFERENCES supervisor.knowledge_requirement_decisions(tenant_id,site_id,inventory_id,inventory_revision,requirement_id,revision),
 FOREIGN KEY(tenant_id,site_id,source_id,verification_revision,verification_id,source_revision,source_sha256,source_access_epoch)
 REFERENCES supervisor.evidence_currentness_decisions(tenant_id,site_id,source_id,revision,verification_id,source_revision,source_sha256,source_access_epoch)
);
CREATE TRIGGER knowledge_immutable BEFORE UPDATE OR DELETE ON supervisor.knowledge_requirement_decision_verifications FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
-- Replacing the existing deferred check preserves claim integrity while adding
-- exact qualification cardinality and policy-only requirement shape checks.
CREATE OR REPLACE FUNCTION supervisor.knowledge_requirement_decision_integrity() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE selected_revision bigint; selected_verdict text; expected_count integer; actual_count integer; prior_verdict text;
 expected_verifications integer; actual_verifications integer; selected_policy bigint; selected_snapshot uuid; selected_key text; policy_verdict text;
BEGIN
 IF TG_TABLE_NAME='knowledge_requirement_decisions' THEN selected_revision:=NEW.revision; ELSE selected_revision:=NEW.decision_revision; END IF;
 SELECT d.verdict,d.claim_count,d.verification_count,d.freshness_policy_revision,d.snapshot_id INTO selected_verdict,expected_count,expected_verifications,selected_policy,selected_snapshot
 FROM supervisor.knowledge_requirement_decisions d WHERE d.tenant_id=NEW.tenant_id AND d.site_id=NEW.site_id AND d.inventory_id=NEW.inventory_id AND d.inventory_revision=NEW.inventory_revision AND d.requirement_id=NEW.requirement_id AND d.revision=selected_revision;
 IF selected_verdict IS NULL OR current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN RAISE EXCEPTION 'Requirement decision scope denied' USING ERRCODE='42501'; END IF;
 SELECT count(*) INTO actual_count FROM supervisor.knowledge_requirement_decision_claims c WHERE c.tenant_id=NEW.tenant_id AND c.site_id=NEW.site_id AND c.inventory_id=NEW.inventory_id AND c.inventory_revision=NEW.inventory_revision AND c.requirement_id=NEW.requirement_id AND c.decision_revision=selected_revision;
 SELECT count(*) INTO actual_verifications FROM supervisor.knowledge_requirement_decision_verifications v WHERE v.tenant_id=NEW.tenant_id AND v.site_id=NEW.site_id AND v.inventory_id=NEW.inventory_id AND v.inventory_revision=NEW.inventory_revision AND v.requirement_id=NEW.requirement_id AND v.decision_revision=selected_revision;
 IF actual_count<>expected_count OR actual_verifications<>expected_verifications THEN RAISE EXCEPTION 'Incomplete requirement pins' USING ERRCODE='23514'; END IF;
 IF selected_policy IS NOT NULL THEN
 SELECT verdict INTO policy_verdict FROM supervisor.document_freshness_policy_decisions WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND revision=selected_policy;
 IF policy_verdict IS DISTINCT FROM 'define' THEN RAISE EXCEPTION 'Invalid requirement freshness policy' USING ERRCODE='23514'; END IF;
 END IF;
 IF selected_verdict='bind' THEN
 SELECT requirement_key INTO selected_key FROM supervisor.knowledge_discovery_requirements WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND inventory_id=NEW.inventory_id AND revision=NEW.inventory_revision AND requirement_id=NEW.requirement_id;
 IF selected_key IS NULL OR ((selected_snapshot IS NULL) IS DISTINCT FROM (selected_key='evidence_freshness_policy')) THEN RAISE EXCEPTION 'Invalid requirement binding shape' USING ERRCODE='23514'; END IF;
 END IF;
 IF selected_verdict='withdraw' THEN
 SELECT verdict INTO prior_verdict FROM supervisor.knowledge_requirement_decisions WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND inventory_id=NEW.inventory_id AND inventory_revision=NEW.inventory_revision AND requirement_id=NEW.requirement_id AND revision=selected_revision-1;
 IF prior_verdict IS NULL OR prior_verdict='withdraw' THEN RAISE EXCEPTION 'Invalid requirement withdrawal' USING ERRCODE='23514'; END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE CONSTRAINT TRIGGER knowledge_requirement_verification_integrity AFTER INSERT ON supervisor.knowledge_requirement_decision_verifications DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION supervisor.knowledge_requirement_decision_integrity();
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['document_freshness_policy_heads','document_freshness_policy_decisions','knowledge_requirement_decision_verifications'] LOOP
 EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
 EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
 EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
 EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
 EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP;
END $$;
GRANT UPDATE(current_revision) ON supervisor.document_freshness_policy_heads TO supervisor_runtime;
ALTER TABLE supervisor.knowledge_receipts DROP CONSTRAINT knowledge_receipts_operation_check;
ALTER TABLE supervisor.knowledge_receipts ADD CONSTRAINT knowledge_receipts_operation_check CHECK(operation IN ('entity','claim','review','review_revoke','resolution','snapshot','discovery','requirement','freshness_policy'));
ALTER TABLE supervisor.knowledge_audit DROP CONSTRAINT knowledge_audit_event_type_check;
ALTER TABLE supervisor.knowledge_audit ADD CONSTRAINT knowledge_audit_event_type_check CHECK(event_type IN ('entity.created','entity.revised','entity.read','entity.listed','claim.created','claim.revised','claim.read','claim.listed','mutation.replayed','conflicts.read','review.decided','review.read','review.revoked','resolution.decided','resolution.read','inventory.read','snapshot.created','snapshot.read','discovery.created','discovery.revised','discovery.read','requirement.decided','requirement.read','coverage.read','freshness_policy.decided','freshness_policy.read'));
