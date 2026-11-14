-- WIS-270 entity merge: "merging identities creates redirect decision and preserves original
-- IDs/history." A merge never rewrites an entity, claim or mapping; it records one immutable
-- human decision that redirects one entity revision to another. Readers follow redirects.
CREATE TABLE supervisor.knowledge_entity_redirects (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,
 schema_version integer NOT NULL DEFAULT 1 CHECK(schema_version=1),
 decision_id uuid NOT NULL UNIQUE,
 from_entity_id uuid NOT NULL,from_entity_revision bigint NOT NULL CHECK(from_entity_revision>0),
 to_entity_id uuid NOT NULL,to_entity_revision bigint NOT NULL CHECK(to_entity_revision>0),
 reason text NOT NULL CHECK(length(reason) BETWEEN 1 AND 2000 AND length(btrim(reason))>0),
 actor_issuer text NOT NULL,actor_subject text NOT NULL,auth_time timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK(auth_time<=created_at AND auth_time>=created_at-interval '15 minutes'),
 CHECK(from_entity_id<>to_entity_id),
 -- An entity is redirected at most once, ever: redirects form chains, never branches.
 PRIMARY KEY(tenant_id,site_id,from_entity_id),
 FOREIGN KEY(tenant_id,site_id,from_entity_id,from_entity_revision)
  REFERENCES supervisor.knowledge_entity_revisions(tenant_id,site_id,id,revision),
 FOREIGN KEY(tenant_id,site_id,to_entity_id,to_entity_revision)
  REFERENCES supervisor.knowledge_entity_revisions(tenant_id,site_id,id,revision)
);
-- Backstop for the service: tenant scope, same identity kind on both sides, and no redirect
-- into an already redirected entity (with at-most-once per source this rules out cycles).
CREATE FUNCTION supervisor.knowledge_entity_redirect_guard() RETURNS trigger LANGUAGE plpgsql
 SET search_path=pg_catalog,pg_temp AS $$
DECLARE from_type text;from_kind text;to_type text;to_kind text;
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Entity redirect tenant scope denied' USING ERRCODE='42501'; END IF;
 -- Lock both entity heads in UUID order before the redirect checks. Under READ COMMITTED the
 -- later statements then see any concurrent merge of either entity, so concurrent A->B and
 -- B->A (or a longer ring) cannot both pass the target-not-redirected check.
 PERFORM 1 FROM supervisor.knowledge_entity_heads h
  WHERE h.tenant_id=NEW.tenant_id AND h.site_id=NEW.site_id AND h.id IN (NEW.from_entity_id,NEW.to_entity_id)
  ORDER BY h.id FOR UPDATE;
 SELECT entity_type,payload->>'asset_kind' INTO from_type,from_kind FROM supervisor.knowledge_entity_revisions
  WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND id=NEW.from_entity_id AND revision=NEW.from_entity_revision;
 SELECT entity_type,payload->>'asset_kind' INTO to_type,to_kind FROM supervisor.knowledge_entity_revisions
  WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND id=NEW.to_entity_id AND revision=NEW.to_entity_revision;
 IF from_type IS NULL OR from_type IS DISTINCT FROM to_type OR from_kind IS DISTINCT FROM to_kind THEN
  RAISE EXCEPTION 'Entity redirect type mismatch' USING ERRCODE='23514'; END IF;
 IF EXISTS(SELECT 1 FROM supervisor.knowledge_entity_redirects r
  WHERE r.tenant_id=NEW.tenant_id AND r.site_id=NEW.site_id AND r.from_entity_id=NEW.to_entity_id) THEN
  RAISE EXCEPTION 'Entity redirect target is itself redirected' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
-- Merge-time chain bound and redirect-into checks walk redirects backwards by target.
CREATE INDEX knowledge_entity_redirects_target ON supervisor.knowledge_entity_redirects(tenant_id,site_id,to_entity_id);
CREATE TRIGGER knowledge_entity_redirect_guard BEFORE INSERT ON supervisor.knowledge_entity_redirects
 FOR EACH ROW EXECUTE FUNCTION supervisor.knowledge_entity_redirect_guard();
CREATE TRIGGER knowledge_immutable BEFORE UPDATE OR DELETE ON supervisor.knowledge_entity_redirects
 FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
ALTER TABLE supervisor.knowledge_entity_redirects ENABLE ROW LEVEL SECURITY;
ALTER TABLE supervisor.knowledge_entity_redirects FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON supervisor.knowledge_entity_redirects USING(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid) WITH CHECK(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid);
REVOKE ALL ON supervisor.knowledge_entity_redirects FROM PUBLIC,supervisor_runtime;
GRANT SELECT,INSERT ON supervisor.knowledge_entity_redirects TO supervisor_runtime;
ALTER TABLE supervisor.knowledge_receipts DROP CONSTRAINT knowledge_receipts_operation_check;
ALTER TABLE supervisor.knowledge_receipts ADD CONSTRAINT knowledge_receipts_operation_check CHECK(operation IN ('entity','claim','review','review_revoke','resolution','snapshot','discovery','requirement','freshness_policy','merge'));
ALTER TABLE supervisor.knowledge_audit DROP CONSTRAINT knowledge_audit_event_type_check;
ALTER TABLE supervisor.knowledge_audit ADD CONSTRAINT knowledge_audit_event_type_check CHECK(event_type IN ('entity.created','entity.revised','entity.read','entity.listed','claim.created','claim.revised','claim.read','claim.listed','mutation.replayed','conflicts.read','review.decided','review.read','review.revoked','resolution.decided','resolution.read','inventory.read','snapshot.created','snapshot.read','discovery.created','discovery.revised','discovery.read','requirement.decided','requirement.read','coverage.read','freshness_policy.decided','freshness_policy.read','entity.merged'));
