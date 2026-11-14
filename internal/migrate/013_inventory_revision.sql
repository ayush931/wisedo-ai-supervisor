-- This opaque site epoch identifies inventory/eligibility changes, not completeness.
CREATE TABLE supervisor.knowledge_inventory_revisions (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,revision bigint NOT NULL DEFAULT 0 CHECK(revision>=0),
 PRIMARY KEY(tenant_id,site_id),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id) ON DELETE CASCADE
);
-- Migration runs atomically as schema owner; ordinary runtime RLS remains enabled.
ALTER TABLE supervisor.sites NO FORCE ROW LEVEL SECURITY;
INSERT INTO supervisor.knowledge_inventory_revisions(tenant_id,site_id,revision)
 SELECT tenant_id,id,0 FROM supervisor.sites;
ALTER TABLE supervisor.sites FORCE ROW LEVEL SECURITY;
ALTER TABLE supervisor.knowledge_inventory_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE supervisor.knowledge_inventory_revisions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON supervisor.knowledge_inventory_revisions
 USING(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid);
REVOKE ALL ON supervisor.knowledge_inventory_revisions FROM PUBLIC,supervisor_runtime;
GRANT SELECT ON supervisor.knowledge_inventory_revisions TO supervisor_runtime;

-- Only installed row triggers can invoke this privileged mutation. Scope is checked
-- independently of the invoker's table policy; FORCE RLS also applies to the owner.
CREATE FUNCTION supervisor.advance_knowledge_inventory() RETURNS trigger
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,pg_temp SET row_security=on AS $$
DECLARE scope_tenant uuid; scope_site uuid;
BEGIN
 IF TG_OP='DELETE' THEN scope_tenant:=OLD.tenant_id; scope_site:=OLD.site_id;
 ELSE scope_tenant:=NEW.tenant_id; scope_site:=NEW.site_id; END IF;
 IF TG_TABLE_SCHEMA<>'supervisor' OR TG_LEVEL<>'ROW' OR TG_WHEN<>'AFTER'
 OR TG_OP NOT IN ('INSERT','UPDATE','DELETE') OR TG_TABLE_NAME NOT IN (
 'evidence_heads','evidence_revisions','evidence_spans','evidence_revocations',
 'evidence_authority_heads','evidence_authority_decisions',
 'knowledge_entities','knowledge_entity_heads','knowledge_entity_revisions',
 'knowledge_claim_heads','knowledge_claim_revisions','knowledge_claim_entity_refs','knowledge_claim_spans',
 'knowledge_conflicts','knowledge_claim_reviews','knowledge_review_revocations',
 'knowledge_resolution_heads','knowledge_resolution_decisions','knowledge_review_resolution_refs')
 OR current_setting('supervisor.tenant_id',true) IS DISTINCT FROM scope_tenant::text THEN
 RAISE EXCEPTION 'Inventory trigger scope denied' USING ERRCODE='42501'; END IF;
 IF TG_OP='UPDATE' THEN
  IF ROW(NEW.tenant_id,NEW.site_id) IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id) THEN
   RAISE EXCEPTION 'Inventory scope cannot change' USING ERRCODE='42501';
  END IF;
  IF NEW IS NOT DISTINCT FROM OLD THEN RETURN NEW; END IF;
 END IF;
 INSERT INTO supervisor.knowledge_inventory_revisions AS inventory(tenant_id,site_id,revision)
 VALUES(scope_tenant,scope_site,1)
 ON CONFLICT(tenant_id,site_id) DO UPDATE SET revision=inventory.revision+1;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION supervisor.advance_knowledge_inventory() FROM PUBLIC,supervisor_runtime;
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY[
 'evidence_heads','evidence_revisions','evidence_spans','evidence_revocations',
 'evidence_authority_heads','evidence_authority_decisions',
 'knowledge_entities','knowledge_entity_heads','knowledge_entity_revisions',
 'knowledge_claim_heads','knowledge_claim_revisions','knowledge_claim_entity_refs','knowledge_claim_spans',
 'knowledge_conflicts','knowledge_claim_reviews','knowledge_review_revocations',
 'knowledge_resolution_heads','knowledge_resolution_decisions','knowledge_review_resolution_refs'] LOOP
 EXECUTE format('CREATE TRIGGER knowledge_inventory_changed AFTER INSERT OR UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.advance_knowledge_inventory()',t);
 END LOOP;
END $$;
ALTER TABLE supervisor.knowledge_audit DROP CONSTRAINT knowledge_audit_event_type_check;
ALTER TABLE supervisor.knowledge_audit ADD CONSTRAINT knowledge_audit_event_type_check CHECK(event_type IN ('entity.created','entity.revised','entity.read','entity.listed','claim.created','claim.revised','claim.read','claim.listed','mutation.replayed','conflicts.read','review.decided','review.read','review.revoked','resolution.decided','resolution.read','inventory.read'));
