-- WIS-611 value-stream vocabulary: an additive, versioned extension of the WIS-270 claim contract.
-- Every existing entity type, predicate, requirement key and capability keeps its meaning; this
-- only widens the sets the database accepts. Predicates and their subject/object rules are
-- enforced by the Go contract (internal/knowledge), which every write path validates.

-- Suppliers and customers at the ends of the value stream, the locations material is received and
-- stored at, and the points where quality is checked.
ALTER TABLE supervisor.knowledge_entities DROP CONSTRAINT knowledge_entities_entity_type_check;
ALTER TABLE supervisor.knowledge_entities ADD CONSTRAINT knowledge_entities_entity_type_check CHECK(entity_type IN (
 'site','area','asset','process','operation','order','material','tool','role','qualification','document','constraint',
 'supplier','customer','location','inspection_point'));

-- process_understanding: discovery walks the whole value stream, from supplier to dispatch.
ALTER TABLE supervisor.knowledge_discovery_inventories DROP CONSTRAINT knowledge_discovery_inventories_capability_check;
ALTER TABLE supervisor.knowledge_discovery_inventories ADD CONSTRAINT knowledge_discovery_inventories_capability_check
 CHECK(capability IN ('shortfall_investigation','internal_task_coordination','process_understanding'));
ALTER TABLE supervisor.knowledge_discovery_requirements DROP CONSTRAINT knowledge_discovery_requirements_requirement_key_check;
ALTER TABLE supervisor.knowledge_discovery_requirements ADD CONSTRAINT knowledge_discovery_requirements_requirement_key_check CHECK(requirement_key IN (
 'asset_identity','asset_process_routing','responsible_role','order_window','target_rate_and_breaks','good_count_source','timestamp_unit_mapping','evidence_freshness_policy','exception_escalation_contact',
 'recipient_authority','completion_evidence',
 'material_suppliers','receiving_inspection','storage_locations','item_routing','cycle_setup_times','in_process_quality','packing_finished_goods','dispatch_delivery','stage_responsibility'));

-- The template denominator is complete at commit, never a client-selected subset: each capability
-- has exactly its own key set (the earlier guard only counted keys).
CREATE OR REPLACE FUNCTION supervisor.knowledge_discovery_template_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE capability text; keys text[]; expected text[];
BEGIN
 SELECT r.capability INTO capability FROM supervisor.knowledge_discovery_revisions r WHERE r.tenant_id=NEW.tenant_id AND r.site_id=NEW.site_id AND r.inventory_id=NEW.inventory_id AND r.revision=NEW.revision;
 IF capability IS NULL OR current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
 RAISE EXCEPTION 'Discovery template scope denied' USING ERRCODE='42501'; END IF;
 SELECT coalesce(array_agg(requirement_key ORDER BY requirement_key),'{}') INTO keys
 FROM supervisor.knowledge_discovery_requirements WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND inventory_id=NEW.inventory_id AND revision=NEW.revision;
 expected := CASE capability
  WHEN 'process_understanding' THEN ARRAY['cycle_setup_times','dispatch_delivery','in_process_quality','item_routing','material_suppliers','packing_finished_goods','receiving_inspection','stage_responsibility','storage_locations']
  ELSE ARRAY['asset_identity','asset_process_routing','evidence_freshness_policy','exception_escalation_contact','good_count_source','order_window','responsible_role','target_rate_and_breaks','timestamp_unit_mapping']
   || CASE WHEN capability='internal_task_coordination' THEN ARRAY['completion_evidence','recipient_authority'] ELSE '{}'::text[] END
 END;
 IF keys IS DISTINCT FROM (SELECT array_agg(k ORDER BY k) FROM unnest(expected) k) THEN
 RAISE EXCEPTION 'Incomplete discovery template' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
