-- Immutable candidate comparisons, never approval or automatic conflict resolution.
CREATE TABLE supervisor.knowledge_conflicts (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL,
 schema_version integer NOT NULL DEFAULT 1 CHECK(schema_version=1),
 left_claim_id uuid NOT NULL,left_revision bigint NOT NULL,
 right_claim_id uuid NOT NULL,right_revision bigint NOT NULL,
 kind text NOT NULL CHECK(kind IN ('value_conflict','text_review','unit_review')),
 effective_from timestamptz NOT NULL,effective_to timestamptz,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK(left_claim_id<right_claim_id),CHECK(effective_to IS NULL OR effective_from<effective_to),
 PRIMARY KEY(tenant_id,site_id,id),UNIQUE(tenant_id,site_id,left_claim_id,left_revision,right_claim_id,right_revision),
 FOREIGN KEY(tenant_id,site_id,left_claim_id,left_revision) REFERENCES supervisor.knowledge_claim_revisions(tenant_id,site_id,claim_id,revision),
 FOREIGN KEY(tenant_id,site_id,right_claim_id,right_revision) REFERENCES supervisor.knowledge_claim_revisions(tenant_id,site_id,claim_id,revision)
);
CREATE INDEX knowledge_conflicts_left ON supervisor.knowledge_conflicts(tenant_id,site_id,left_claim_id,left_revision,id);
CREATE INDEX knowledge_conflicts_right ON supervisor.knowledge_conflicts(tenant_id,site_id,right_claim_id,right_revision,id);
-- Run during the migration maintenance window: compare all existing current
-- candidates so upgraded sites do not silently omit pre-migration conflicts.
-- Owner visibility is temporary and transactional; runtime remains subject to RLS.
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['knowledge_claim_heads','knowledge_claim_revisions','knowledge_claim_entity_refs','knowledge_entity_heads','knowledge_entity_revisions','knowledge_claim_spans','evidence_heads','evidence_revocations'] LOOP
 EXECUTE format('ALTER TABLE supervisor.%I NO FORCE ROW LEVEL SECURITY',t);
 END LOOP; END $$;
WITH eligible AS MATERIALIZED (
 SELECT r.* FROM supervisor.knowledge_claim_revisions r
 JOIN supervisor.knowledge_claim_heads ch ON (ch.tenant_id,ch.site_id,ch.id,ch.current_revision)=(r.tenant_id,r.site_id,r.claim_id,r.revision)
 JOIN supervisor.knowledge_claim_entity_refs p ON (p.tenant_id,p.site_id,p.claim_id,p.claim_revision)=(r.tenant_id,r.site_id,r.claim_id,r.revision)
 JOIN supervisor.knowledge_entity_heads sh ON (sh.tenant_id,sh.site_id,sh.id,sh.current_revision)=(p.tenant_id,p.site_id,p.subject_id,p.subject_revision)
 WHERE r.predicate IN ('part_of_asset','located_in','part_of','capacity_per_hour','escalation_after_seconds','objective','actual_step')
 AND (p.object_entity_id IS NULL OR EXISTS(SELECT 1 FROM supervisor.knowledge_entity_heads oh WHERE (oh.tenant_id,oh.site_id,oh.id,oh.current_revision)=(p.tenant_id,p.site_id,p.object_entity_id,p.object_revision)))
 AND NOT EXISTS(SELECT 1 FROM supervisor.knowledge_entity_revisions e JOIN supervisor.evidence_heads h ON (h.tenant_id,h.site_id,h.source_id)=(e.tenant_id,e.site_id,e.source_id)
 WHERE e.tenant_id=p.tenant_id AND e.site_id=p.site_id AND ((e.id=p.subject_id AND e.revision=p.subject_revision) OR (e.id=p.object_entity_id AND e.revision=p.object_revision))
 AND (h.current_revision<>e.source_revision OR h.revoked OR EXISTS(SELECT 1 FROM supervisor.evidence_revocations rv WHERE (rv.tenant_id,rv.site_id,rv.source_id)=(h.tenant_id,h.site_id,h.source_id))))
 AND NOT EXISTS(SELECT 1 FROM supervisor.knowledge_claim_spans s JOIN supervisor.evidence_heads h ON (h.tenant_id,h.site_id,h.source_id)=(s.tenant_id,s.site_id,s.source_id)
 WHERE (s.tenant_id,s.site_id,s.claim_id,s.claim_revision)=(r.tenant_id,r.site_id,r.claim_id,r.revision)
 AND (h.current_revision<>s.source_revision OR h.revoked OR EXISTS(SELECT 1 FROM supervisor.evidence_revocations rv WHERE (rv.tenant_id,rv.site_id,rv.source_id)=(h.tenant_id,h.site_id,h.source_id))))
), pairs AS (
 SELECT l.tenant_id,l.site_id,l.claim_id AS left_id,l.revision AS left_rev,r.claim_id AS right_id,r.revision AS right_rev,
 CASE WHEN l.object_entity_id IS NOT NULL THEN CASE WHEN l.object_entity_id<>r.object_entity_id THEN 'value_conflict' END
 WHEN coalesce(l.payload->'literal'->>'unit','')<>coalesce(r.payload->'literal'->>'unit','') THEN 'unit_review'
 WHEN l.payload->'literal'->>'type'=r.payload->'literal'->>'type' AND l.payload->'literal'->'value'=r.payload->'literal'->'value' THEN NULL
 WHEN l.payload->'literal'->>'type'='decimal' AND r.payload->'literal'->>'type'='decimal' THEN 'value_conflict'
 ELSE 'text_review' END AS kind,
 greatest((l.payload->>'effective_from')::timestamptz,(r.payload->>'effective_from')::timestamptz) AS starts,
 least((l.payload->>'effective_to')::timestamptz,(r.payload->>'effective_to')::timestamptz) AS ends
 FROM eligible l JOIN eligible r ON l.tenant_id=r.tenant_id AND l.site_id=r.site_id AND l.subject_id=r.subject_id AND l.predicate=r.predicate AND l.claim_id<r.claim_id
)
INSERT INTO supervisor.knowledge_conflicts(tenant_id,site_id,id,left_claim_id,left_revision,right_claim_id,right_revision,kind,effective_from,effective_to)
 SELECT tenant_id,site_id,gen_random_uuid(),left_id,left_rev,right_id,right_rev,kind,starts,ends FROM pairs
 WHERE kind IS NOT NULL AND (ends IS NULL OR starts<ends);
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['knowledge_claim_heads','knowledge_claim_revisions','knowledge_claim_entity_refs','knowledge_entity_heads','knowledge_entity_revisions','knowledge_claim_spans','evidence_heads','evidence_revocations'] LOOP
 EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
 END LOOP; END $$;
ALTER TABLE supervisor.knowledge_conflicts ENABLE ROW LEVEL SECURITY;
ALTER TABLE supervisor.knowledge_conflicts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON supervisor.knowledge_conflicts USING(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid) WITH CHECK(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid);
REVOKE ALL ON supervisor.knowledge_conflicts FROM PUBLIC,supervisor_runtime;
GRANT SELECT,INSERT ON supervisor.knowledge_conflicts TO supervisor_runtime;
CREATE TRIGGER knowledge_immutable BEFORE UPDATE OR DELETE ON supervisor.knowledge_conflicts FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
ALTER TABLE supervisor.knowledge_audit DROP CONSTRAINT knowledge_audit_event_type_check;
ALTER TABLE supervisor.knowledge_audit ADD CONSTRAINT knowledge_audit_event_type_check CHECK(event_type IN ('entity.created','entity.revised','entity.read','entity.listed','claim.created','claim.revised','claim.read','claim.listed','mutation.replayed','conflicts.read'));
