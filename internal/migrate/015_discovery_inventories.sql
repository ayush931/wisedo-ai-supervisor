-- Requirement inventories define bounded interview work, never readiness or approval.
-- They intentionally do not advance the separate approved-graph inventory epoch.
CREATE TABLE supervisor.knowledge_discovery_inventories (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,inventory_id uuid NOT NULL,
 area_id uuid NOT NULL,process_id uuid NOT NULL,
 capability text NOT NULL CHECK(capability IN ('shortfall_investigation','internal_task_coordination')),
 PRIMARY KEY(tenant_id,site_id,inventory_id),
 UNIQUE(tenant_id,site_id,inventory_id,area_id,process_id,capability),
 FOREIGN KEY(tenant_id,site_id,area_id) REFERENCES supervisor.knowledge_entities(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,process_id) REFERENCES supervisor.knowledge_entities(tenant_id,site_id,id)
);
CREATE TABLE supervisor.knowledge_discovery_heads (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,inventory_id uuid NOT NULL,
 current_revision bigint NOT NULL CHECK(current_revision>0),
 PRIMARY KEY(tenant_id,site_id,inventory_id),
 FOREIGN KEY(tenant_id,site_id,inventory_id) REFERENCES supervisor.knowledge_discovery_inventories(tenant_id,site_id,inventory_id)
);
CREATE TABLE supervisor.knowledge_discovery_revisions (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,inventory_id uuid NOT NULL,revision bigint NOT NULL CHECK(revision>0),
 schema_version integer NOT NULL DEFAULT 1 CHECK(schema_version=1),
 template_version integer NOT NULL CHECK(template_version=1),
 area_id uuid NOT NULL,area_revision bigint NOT NULL,process_id uuid NOT NULL,process_revision bigint NOT NULL,
 capability text NOT NULL,
 reason text NOT NULL CHECK(length(reason) BETWEEN 1 AND 1000 AND length(btrim(reason))>0),
 actor_issuer text NOT NULL,actor_subject text NOT NULL,auth_time timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK(auth_time<=created_at AND auth_time>=created_at-interval '15 minutes'),
 PRIMARY KEY(tenant_id,site_id,inventory_id,revision),
 FOREIGN KEY(tenant_id,site_id,inventory_id) REFERENCES supervisor.knowledge_discovery_heads(tenant_id,site_id,inventory_id),
 FOREIGN KEY(tenant_id,site_id,inventory_id,area_id,process_id,capability) REFERENCES supervisor.knowledge_discovery_inventories(tenant_id,site_id,inventory_id,area_id,process_id,capability),
 FOREIGN KEY(tenant_id,site_id,area_id,area_revision) REFERENCES supervisor.knowledge_entity_revisions(tenant_id,site_id,id,revision),
 FOREIGN KEY(tenant_id,site_id,process_id,process_revision) REFERENCES supervisor.knowledge_entity_revisions(tenant_id,site_id,id,revision)
);
ALTER TABLE supervisor.knowledge_discovery_heads ADD CONSTRAINT knowledge_discovery_current_fk
 FOREIGN KEY(tenant_id,site_id,inventory_id,current_revision) REFERENCES supervisor.knowledge_discovery_revisions(tenant_id,site_id,inventory_id,revision) DEFERRABLE INITIALLY DEFERRED;
CREATE FUNCTION supervisor.knowledge_discovery_head_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$ BEGIN
 IF ROW(NEW.tenant_id,NEW.site_id,NEW.inventory_id) IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.inventory_id) OR NEW.current_revision<>OLD.current_revision+1 THEN
 RAISE EXCEPTION 'Invalid discovery inventory revision transition' USING ERRCODE='42501'; END IF;
 RETURN NEW; END $$;
CREATE TRIGGER knowledge_discovery_head_guard BEFORE UPDATE ON supervisor.knowledge_discovery_heads FOR EACH ROW EXECUTE FUNCTION supervisor.knowledge_discovery_head_guard();
CREATE TABLE supervisor.knowledge_discovery_requirements (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,inventory_id uuid NOT NULL,revision bigint NOT NULL,
 requirement_key text NOT NULL CHECK(requirement_key IN ('asset_identity','asset_process_routing','responsible_role','order_window','target_rate_and_breaks','good_count_source','timestamp_unit_mapping','evidence_freshness_policy','exception_escalation_contact','recipient_authority','completion_evidence')),
 requirement_id uuid NOT NULL,first_revision bigint NOT NULL DEFAULT 1 CHECK(first_revision=1),
 definition jsonb NOT NULL CHECK(jsonb_typeof(definition)='object'),
 CHECK((definition->>'key') IS NOT DISTINCT FROM requirement_key),
 CHECK(length(definition->>'description') BETWEEN 1 AND 1000 AND definition->>'description' IS NOT NULL),
 CHECK(definition->>'interview_role' IS NOT NULL AND definition->>'interview_role' IN ('owner','supervisor','operator')),
 PRIMARY KEY(tenant_id,site_id,inventory_id,revision,requirement_key),
 UNIQUE(tenant_id,site_id,inventory_id,revision,requirement_id),
 UNIQUE(tenant_id,site_id,inventory_id,revision,requirement_key,requirement_id),
 FOREIGN KEY(tenant_id,site_id,inventory_id,revision) REFERENCES supervisor.knowledge_discovery_revisions(tenant_id,site_id,inventory_id,revision),
 -- Every subsequent revision must retain the server-issued first-revision key/ID map.
 FOREIGN KEY(tenant_id,site_id,inventory_id,first_revision,requirement_key,requirement_id) REFERENCES supervisor.knowledge_discovery_requirements(tenant_id,site_id,inventory_id,revision,requirement_key,requirement_id)
);
-- The template denominator is complete at commit, never a client-selected subset.
CREATE FUNCTION supervisor.knowledge_discovery_template_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE total integer; extra integer; capability text;
BEGIN
 SELECT r.capability INTO capability FROM supervisor.knowledge_discovery_revisions r WHERE r.tenant_id=NEW.tenant_id AND r.site_id=NEW.site_id AND r.inventory_id=NEW.inventory_id AND r.revision=NEW.revision;
 IF capability IS NULL OR current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
 RAISE EXCEPTION 'Discovery template scope denied' USING ERRCODE='42501'; END IF;
 SELECT count(*),count(*) FILTER(WHERE requirement_key IN ('recipient_authority','completion_evidence')) INTO total,extra
 FROM supervisor.knowledge_discovery_requirements WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND inventory_id=NEW.inventory_id AND revision=NEW.revision;
 IF (capability='shortfall_investigation' AND (total<>9 OR extra<>0)) OR (capability='internal_task_coordination' AND total<>11) THEN
 RAISE EXCEPTION 'Incomplete discovery template' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE CONSTRAINT TRIGGER knowledge_discovery_template_guard AFTER INSERT ON supervisor.knowledge_discovery_revisions DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION supervisor.knowledge_discovery_template_guard();
CREATE CONSTRAINT TRIGGER knowledge_discovery_requirement_template_guard AFTER INSERT ON supervisor.knowledge_discovery_requirements DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION supervisor.knowledge_discovery_template_guard();
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['knowledge_discovery_inventories','knowledge_discovery_heads','knowledge_discovery_revisions','knowledge_discovery_requirements'] LOOP
 EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
 EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
 EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
 EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
 EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 IF t<>'knowledge_discovery_heads' THEN EXECUTE format('CREATE TRIGGER knowledge_immutable BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t); END IF;
 END LOOP;
END $$;
GRANT UPDATE(current_revision) ON supervisor.knowledge_discovery_heads TO supervisor_runtime;
ALTER TABLE supervisor.knowledge_receipts DROP CONSTRAINT knowledge_receipts_operation_check;
ALTER TABLE supervisor.knowledge_receipts ADD CONSTRAINT knowledge_receipts_operation_check CHECK(operation IN ('entity','claim','review','review_revoke','resolution','snapshot','discovery'));
ALTER TABLE supervisor.knowledge_audit DROP CONSTRAINT knowledge_audit_event_type_check;
ALTER TABLE supervisor.knowledge_audit ADD CONSTRAINT knowledge_audit_event_type_check CHECK(event_type IN ('entity.created','entity.revised','entity.read','entity.listed','claim.created','claim.revised','claim.read','claim.listed','mutation.replayed','conflicts.read','review.decided','review.read','review.revoked','resolution.decided','resolution.read','inventory.read','snapshot.created','snapshot.read','discovery.created','discovery.revised','discovery.read'));
