-- Human requirement decisions pin versioned evidence; they are not readiness claims.
CREATE TABLE supervisor.knowledge_requirement_decision_heads (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,inventory_id uuid NOT NULL,inventory_revision bigint NOT NULL,requirement_id uuid NOT NULL,
 current_revision bigint NOT NULL CHECK(current_revision>0),
 PRIMARY KEY(tenant_id,site_id,inventory_id,inventory_revision,requirement_id),
 FOREIGN KEY(tenant_id,site_id,inventory_id,inventory_revision,requirement_id) REFERENCES supervisor.knowledge_discovery_requirements(tenant_id,site_id,inventory_id,revision,requirement_id)
);
CREATE TABLE supervisor.knowledge_requirement_decisions (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,inventory_id uuid NOT NULL,inventory_revision bigint NOT NULL,requirement_id uuid NOT NULL,
 revision bigint NOT NULL CHECK(revision>0),decision_id uuid NOT NULL UNIQUE,
 schema_version integer NOT NULL DEFAULT 1 CHECK(schema_version=1),
 verdict text NOT NULL CHECK(verdict IN ('bind','not_applicable','withdraw')),
 reason text NOT NULL CHECK(length(reason) BETWEEN 1 AND 1000 AND length(btrim(reason))>0),
 actor_issuer text NOT NULL,actor_subject text NOT NULL,auth_time timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK(auth_time<=created_at AND auth_time>=created_at-interval '15 minutes'),
 snapshot_id uuid,snapshot_sha256 text,snapshot_inventory_revision bigint,as_of timestamptz,
 claim_count integer NOT NULL CHECK(claim_count BETWEEN 0 AND 100),
 withdrawn_revision bigint GENERATED ALWAYS AS (CASE WHEN verdict='withdraw' THEN revision-1 END) STORED,
 CHECK(withdrawn_revision IS NULL OR withdrawn_revision>0),
 CHECK((verdict='bind' AND snapshot_id IS NOT NULL AND snapshot_sha256 IS NOT NULL AND snapshot_inventory_revision IS NOT NULL AND as_of IS NOT NULL AND claim_count>0)
 OR (verdict IN ('not_applicable','withdraw') AND snapshot_id IS NULL AND snapshot_sha256 IS NULL AND snapshot_inventory_revision IS NULL AND as_of IS NULL AND claim_count=0)),
 PRIMARY KEY(tenant_id,site_id,inventory_id,inventory_revision,requirement_id,revision),
 UNIQUE(tenant_id,site_id,inventory_id,inventory_revision,requirement_id,revision,snapshot_id),
 FOREIGN KEY(tenant_id,site_id,inventory_id,inventory_revision,requirement_id) REFERENCES supervisor.knowledge_requirement_decision_heads(tenant_id,site_id,inventory_id,inventory_revision,requirement_id),
 FOREIGN KEY(tenant_id,site_id,snapshot_id,snapshot_inventory_revision,snapshot_sha256) REFERENCES supervisor.knowledge_process_snapshots(tenant_id,site_id,snapshot_id,inventory_revision,sha256),
 FOREIGN KEY(tenant_id,site_id,inventory_id,inventory_revision,requirement_id,withdrawn_revision) REFERENCES supervisor.knowledge_requirement_decisions(tenant_id,site_id,inventory_id,inventory_revision,requirement_id,revision)
);
ALTER TABLE supervisor.knowledge_requirement_decision_heads ADD CONSTRAINT knowledge_requirement_decision_current_fk
 FOREIGN KEY(tenant_id,site_id,inventory_id,inventory_revision,requirement_id,current_revision) REFERENCES supervisor.knowledge_requirement_decisions(tenant_id,site_id,inventory_id,inventory_revision,requirement_id,revision) DEFERRABLE INITIALLY DEFERRED;
CREATE FUNCTION supervisor.knowledge_requirement_decision_head_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$ BEGIN
 IF ROW(NEW.tenant_id,NEW.site_id,NEW.inventory_id,NEW.inventory_revision,NEW.requirement_id) IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.inventory_id,OLD.inventory_revision,OLD.requirement_id) OR NEW.current_revision<>OLD.current_revision+1 THEN
 RAISE EXCEPTION 'Invalid requirement decision revision transition' USING ERRCODE='42501'; END IF;
 RETURN NEW; END $$;
CREATE TRIGGER knowledge_requirement_decision_head_guard BEFORE UPDATE ON supervisor.knowledge_requirement_decision_heads FOR EACH ROW EXECUTE FUNCTION supervisor.knowledge_requirement_decision_head_guard();
ALTER TABLE supervisor.knowledge_snapshot_claims ADD CONSTRAINT knowledge_snapshot_claim_exact_reference_key UNIQUE(tenant_id,site_id,snapshot_id,claim_id,claim_revision);
CREATE TABLE supervisor.knowledge_requirement_decision_claims (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,inventory_id uuid NOT NULL,inventory_revision bigint NOT NULL,requirement_id uuid NOT NULL,
 decision_revision bigint NOT NULL,snapshot_id uuid NOT NULL,claim_id uuid NOT NULL,claim_revision bigint NOT NULL,
 PRIMARY KEY(tenant_id,site_id,inventory_id,inventory_revision,requirement_id,decision_revision,claim_id),
 FOREIGN KEY(tenant_id,site_id,inventory_id,inventory_revision,requirement_id,decision_revision,snapshot_id) REFERENCES supervisor.knowledge_requirement_decisions(tenant_id,site_id,inventory_id,inventory_revision,requirement_id,revision,snapshot_id),
 FOREIGN KEY(tenant_id,site_id,snapshot_id,claim_id,claim_revision) REFERENCES supervisor.knowledge_snapshot_claims(tenant_id,site_id,snapshot_id,claim_id,claim_revision)
);
-- Persisted exact cardinality prevents late child inserts changing an old decision.
CREATE FUNCTION supervisor.knowledge_requirement_decision_integrity() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE selected_revision bigint; selected_verdict text; expected_count integer; actual_count integer; prior_verdict text;
BEGIN
 IF TG_TABLE_NAME='knowledge_requirement_decisions' THEN selected_revision:=NEW.revision; ELSE selected_revision:=NEW.decision_revision; END IF;
 SELECT d.verdict,d.claim_count INTO selected_verdict,expected_count FROM supervisor.knowledge_requirement_decisions d
 WHERE d.tenant_id=NEW.tenant_id AND d.site_id=NEW.site_id AND d.inventory_id=NEW.inventory_id AND d.inventory_revision=NEW.inventory_revision AND d.requirement_id=NEW.requirement_id AND d.revision=selected_revision;
 IF selected_verdict IS NULL OR current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
 RAISE EXCEPTION 'Requirement decision scope denied' USING ERRCODE='42501'; END IF;
 SELECT count(*) INTO actual_count FROM supervisor.knowledge_requirement_decision_claims c
 WHERE c.tenant_id=NEW.tenant_id AND c.site_id=NEW.site_id AND c.inventory_id=NEW.inventory_id AND c.inventory_revision=NEW.inventory_revision AND c.requirement_id=NEW.requirement_id AND c.decision_revision=selected_revision;
 IF actual_count<>expected_count THEN RAISE EXCEPTION 'Incomplete requirement claim pins' USING ERRCODE='23514'; END IF;
 IF selected_verdict='withdraw' THEN
 SELECT d.verdict INTO prior_verdict FROM supervisor.knowledge_requirement_decisions d
 WHERE d.tenant_id=NEW.tenant_id AND d.site_id=NEW.site_id AND d.inventory_id=NEW.inventory_id AND d.inventory_revision=NEW.inventory_revision AND d.requirement_id=NEW.requirement_id AND d.revision=selected_revision-1;
 IF prior_verdict IS NULL OR prior_verdict='withdraw' THEN RAISE EXCEPTION 'Invalid requirement withdrawal' USING ERRCODE='23514'; END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE CONSTRAINT TRIGGER knowledge_requirement_decision_integrity AFTER INSERT ON supervisor.knowledge_requirement_decisions DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION supervisor.knowledge_requirement_decision_integrity();
CREATE CONSTRAINT TRIGGER knowledge_requirement_claim_integrity AFTER INSERT ON supervisor.knowledge_requirement_decision_claims DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION supervisor.knowledge_requirement_decision_integrity();
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['knowledge_requirement_decision_heads','knowledge_requirement_decisions','knowledge_requirement_decision_claims'] LOOP
 EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
 EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
 EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
 EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
 EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 IF t<>'knowledge_requirement_decision_heads' THEN EXECUTE format('CREATE TRIGGER knowledge_immutable BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t); END IF;
 END LOOP;
END $$;
GRANT UPDATE(current_revision) ON supervisor.knowledge_requirement_decision_heads TO supervisor_runtime;
ALTER TABLE supervisor.knowledge_receipts DROP CONSTRAINT knowledge_receipts_operation_check;
ALTER TABLE supervisor.knowledge_receipts ADD CONSTRAINT knowledge_receipts_operation_check CHECK(operation IN ('entity','claim','review','review_revoke','resolution','snapshot','discovery','requirement'));
ALTER TABLE supervisor.knowledge_audit DROP CONSTRAINT knowledge_audit_event_type_check;
ALTER TABLE supervisor.knowledge_audit ADD CONSTRAINT knowledge_audit_event_type_check CHECK(event_type IN ('entity.created','entity.revised','entity.read','entity.listed','claim.created','claim.revised','claim.read','claim.listed','mutation.replayed','conflicts.read','review.decided','review.read','review.revoked','resolution.decided','resolution.read','inventory.read','snapshot.created','snapshot.read','discovery.created','discovery.revised','discovery.read','requirement.decided','requirement.read'));
