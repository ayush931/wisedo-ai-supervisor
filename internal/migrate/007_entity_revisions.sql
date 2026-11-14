-- Entity anchors stay immutable. New revisions and claim pins preserve old provenance.
CREATE TABLE supervisor.knowledge_entity_heads (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL,
 current_revision bigint NOT NULL CHECK(current_revision>0),
 PRIMARY KEY(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,id) REFERENCES supervisor.knowledge_entities(tenant_id,site_id,id)
);
CREATE TABLE supervisor.knowledge_entity_revisions (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL,
 revision bigint NOT NULL CHECK(revision>0),entity_type text NOT NULL,payload jsonb NOT NULL,
 source_id uuid NOT NULL,source_revision bigint NOT NULL,
 state text NOT NULL DEFAULT 'candidate' CHECK(state='candidate'),
 actor_issuer text NOT NULL,actor_subject text NOT NULL,created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((payload->>'entity_id') IS NOT DISTINCT FROM id::text),
 CHECK((payload->>'type') IS NOT DISTINCT FROM entity_type),
 CHECK((payload->>'source_id') IS NOT DISTINCT FROM source_id::text),
 CHECK((payload->>'source_revision')::bigint IS NOT DISTINCT FROM source_revision),
 PRIMARY KEY(tenant_id,site_id,id,revision),
 FOREIGN KEY(tenant_id,site_id,id) REFERENCES supervisor.knowledge_entity_heads(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,source_id,source_revision) REFERENCES supervisor.evidence_revisions(tenant_id,site_id,source_id,revision)
);
ALTER TABLE supervisor.knowledge_entity_heads ADD CONSTRAINT knowledge_entity_current_revision_fk
 FOREIGN KEY(tenant_id,site_id,id,current_revision)
 REFERENCES supervisor.knowledge_entity_revisions(tenant_id,site_id,id,revision) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE supervisor.knowledge_claim_revisions ADD CONSTRAINT knowledge_claim_subject_key
 UNIQUE(tenant_id,site_id,claim_id,revision,subject_id);
ALTER TABLE supervisor.knowledge_claim_revisions ADD CONSTRAINT knowledge_claim_object_key
 UNIQUE(tenant_id,site_id,claim_id,revision,object_entity_id);
CREATE TABLE supervisor.knowledge_claim_entity_refs (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,claim_id uuid NOT NULL,claim_revision bigint NOT NULL,
 subject_id uuid NOT NULL,subject_revision bigint NOT NULL CHECK(subject_revision>0),
 object_entity_id uuid,object_revision bigint CHECK(object_revision>0),
 CHECK((object_entity_id IS NULL)=(object_revision IS NULL)),
 PRIMARY KEY(tenant_id,site_id,claim_id,claim_revision),
 FOREIGN KEY(tenant_id,site_id,claim_id,claim_revision,subject_id)
 REFERENCES supervisor.knowledge_claim_revisions(tenant_id,site_id,claim_id,revision,subject_id),
 FOREIGN KEY(tenant_id,site_id,claim_id,claim_revision,object_entity_id)
 REFERENCES supervisor.knowledge_claim_revisions(tenant_id,site_id,claim_id,revision,object_entity_id),
 FOREIGN KEY(tenant_id,site_id,subject_id,subject_revision)
 REFERENCES supervisor.knowledge_entity_revisions(tenant_id,site_id,id,revision),
 FOREIGN KEY(tenant_id,site_id,object_entity_id,object_revision)
 REFERENCES supervisor.knowledge_entity_revisions(tenant_id,site_id,id,revision)
);
-- The schema owner must see every tenant during backfill. Runtime is a nonowner
-- and keeps RLS enforcement; transactional DDL locks hide intermediate policy state.
ALTER TABLE supervisor.knowledge_entities NO FORCE ROW LEVEL SECURITY;
ALTER TABLE supervisor.knowledge_claim_revisions NO FORCE ROW LEVEL SECURITY;
INSERT INTO supervisor.knowledge_entity_heads(tenant_id,site_id,id,current_revision)
 SELECT tenant_id,site_id,id,1 FROM supervisor.knowledge_entities;
INSERT INTO supervisor.knowledge_entity_revisions(tenant_id,site_id,id,revision,entity_type,payload,
 source_id,source_revision,state,actor_issuer,actor_subject,created_at)
 SELECT tenant_id,site_id,id,1,entity_type,payload,source_id,source_revision,state,actor_issuer,actor_subject,created_at
 FROM supervisor.knowledge_entities;
INSERT INTO supervisor.knowledge_claim_entity_refs(tenant_id,site_id,claim_id,claim_revision,
 subject_id,subject_revision,object_entity_id,object_revision)
 SELECT tenant_id,site_id,claim_id,revision,subject_id,1,object_entity_id,
 CASE WHEN object_entity_id IS NULL THEN NULL ELSE 1 END FROM supervisor.knowledge_claim_revisions;
DO $$ BEGIN
 IF (SELECT count(*) FROM supervisor.knowledge_entity_revisions)<>(SELECT count(*) FROM supervisor.knowledge_entities)
 OR (SELECT count(*) FROM supervisor.knowledge_entity_heads)<>(SELECT count(*) FROM supervisor.knowledge_entities)
 OR (SELECT count(*) FROM supervisor.knowledge_claim_entity_refs)<>(SELECT count(*) FROM supervisor.knowledge_claim_revisions) THEN
 RAISE EXCEPTION 'Incomplete entity provenance backfill'; END IF;
END $$;
-- Flush the deferred head-to-revision FK checks before altering table policies.
SET CONSTRAINTS ALL IMMEDIATE;
ALTER TABLE supervisor.knowledge_entities FORCE ROW LEVEL SECURITY;
ALTER TABLE supervisor.knowledge_claim_revisions FORCE ROW LEVEL SECURITY;

CREATE FUNCTION supervisor.knowledge_entity_identity_guard() RETURNS trigger LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp AS $$ DECLARE anchor supervisor.knowledge_entities%ROWTYPE; BEGIN
 SELECT * INTO anchor FROM supervisor.knowledge_entities
 WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND id=NEW.id;
 IF NOT FOUND OR NEW.entity_type IS DISTINCT FROM anchor.entity_type
 OR (NEW.payload->>'asset_kind') IS DISTINCT FROM (anchor.payload->>'asset_kind') THEN
 RAISE EXCEPTION 'Entity type and asset kind are immutable' USING ERRCODE='23514'; END IF;
 RETURN NEW; END $$;
CREATE TRIGGER knowledge_entity_identity_guard BEFORE INSERT ON supervisor.knowledge_entity_revisions
 FOR EACH ROW EXECUTE FUNCTION supervisor.knowledge_entity_identity_guard();
CREATE TRIGGER knowledge_entity_head_guard BEFORE UPDATE ON supervisor.knowledge_entity_heads
 FOR EACH ROW EXECUTE FUNCTION supervisor.knowledge_head_guard();
CREATE FUNCTION supervisor.knowledge_entity_pin_guard() RETURNS trigger LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp AS $$
DECLARE c supervisor.knowledge_claim_revisions%ROWTYPE; pins supervisor.knowledge_claim_entity_refs%ROWTYPE; r bigint;
BEGIN
 IF TG_TABLE_NAME='knowledge_claim_entity_refs' THEN r:=NEW.claim_revision; ELSE r:=NEW.revision; END IF;
 SELECT * INTO c FROM supervisor.knowledge_claim_revisions
 WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND claim_id=NEW.claim_id AND revision=r;
 SELECT * INTO pins FROM supervisor.knowledge_claim_entity_refs
 WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND claim_id=NEW.claim_id AND claim_revision=r;
 IF NOT FOUND OR pins.subject_id IS DISTINCT FROM c.subject_id
 OR pins.object_entity_id IS DISTINCT FROM c.object_entity_id
 OR (c.payload->>'subject_revision')::bigint IS DISTINCT FROM pins.subject_revision
 OR nullif((c.payload->>'object_revision')::bigint,0) IS DISTINCT FROM pins.object_revision THEN
 RAISE EXCEPTION 'Candidate entity pins disagree' USING ERRCODE='23514'; END IF;
 RETURN NEW; END $$;
CREATE CONSTRAINT TRIGGER knowledge_entity_pin_guard AFTER INSERT ON supervisor.knowledge_claim_revisions
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION supervisor.knowledge_entity_pin_guard();
CREATE CONSTRAINT TRIGGER knowledge_entity_pin_guard AFTER INSERT ON supervisor.knowledge_claim_entity_refs
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION supervisor.knowledge_entity_pin_guard();
ALTER TABLE supervisor.knowledge_audit DROP CONSTRAINT knowledge_audit_event_type_check;
ALTER TABLE supervisor.knowledge_audit ADD CONSTRAINT knowledge_audit_event_type_check
 CHECK(event_type IN ('entity.created','entity.revised','entity.read','entity.listed','claim.created','claim.revised','claim.read','claim.listed','mutation.replayed'));
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['knowledge_entity_heads','knowledge_entity_revisions','knowledge_claim_entity_refs'] LOOP
 EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
 EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
 EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
 EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
 EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 IF t<>'knowledge_entity_heads' THEN
 EXECUTE format('CREATE TRIGGER knowledge_immutable BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t);
 END IF;
 END LOOP; END $$;
GRANT UPDATE(current_revision) ON supervisor.knowledge_entity_heads TO supervisor_runtime;
