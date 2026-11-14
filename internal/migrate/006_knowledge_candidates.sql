-- Reported candidate knowledge only: no operational/approved state exists here.
CREATE TABLE supervisor.knowledge_entities (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL,
 entity_type text NOT NULL CHECK(entity_type IN ('site','area','asset','process','operation','order','material','tool','role','qualification','document','constraint')),payload jsonb NOT NULL,
 source_id uuid NOT NULL,source_revision bigint NOT NULL,
 state text NOT NULL DEFAULT 'candidate' CHECK(state='candidate'),
 actor_issuer text NOT NULL,actor_subject text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((payload->>'entity_id') IS NOT DISTINCT FROM id::text),
 CHECK((payload->>'type') IS NOT DISTINCT FROM entity_type),
 CHECK((payload->>'source_id') IS NOT DISTINCT FROM source_id::text),
 CHECK((payload->>'source_revision')::bigint IS NOT DISTINCT FROM source_revision),
 PRIMARY KEY(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,source_id,source_revision)
 REFERENCES supervisor.evidence_revisions(tenant_id,site_id,source_id,revision)
);
CREATE TABLE supervisor.knowledge_claim_heads (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL,
 current_revision bigint NOT NULL CHECK(current_revision>0),
 PRIMARY KEY(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id)
);
CREATE TABLE supervisor.knowledge_claim_revisions (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,claim_id uuid NOT NULL,
 revision bigint NOT NULL CHECK(revision>0),
 subject_id uuid NOT NULL,predicate text NOT NULL,object_entity_id uuid,
 payload jsonb NOT NULL,state text NOT NULL DEFAULT 'candidate' CHECK(state='candidate'),
 actor_issuer text NOT NULL,actor_subject text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,claim_id,revision),
 CHECK(object_entity_id IS NULL OR object_entity_id<>subject_id),
 CHECK((payload->>'claim_id') IS NOT DISTINCT FROM claim_id::text),
 CHECK((payload->>'subject_id') IS NOT DISTINCT FROM subject_id::text),
 CHECK((payload->>'predicate') IS NOT DISTINCT FROM predicate),
 CHECK((payload->>'object_entity_id') IS NOT DISTINCT FROM object_entity_id::text),
 FOREIGN KEY(tenant_id,site_id,claim_id) REFERENCES supervisor.knowledge_claim_heads(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,subject_id) REFERENCES supervisor.knowledge_entities(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,object_entity_id) REFERENCES supervisor.knowledge_entities(tenant_id,site_id,id)
);
ALTER TABLE supervisor.knowledge_claim_heads ADD CONSTRAINT knowledge_current_revision_fk
 FOREIGN KEY(tenant_id,site_id,id,current_revision)
 REFERENCES supervisor.knowledge_claim_revisions(tenant_id,site_id,claim_id,revision) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE supervisor.evidence_spans ADD CONSTRAINT evidence_span_provenance_key
 UNIQUE(tenant_id,site_id,id,source_id,revision);
CREATE TABLE supervisor.knowledge_claim_spans (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,claim_id uuid NOT NULL,claim_revision bigint NOT NULL,
 span_id uuid NOT NULL,source_id uuid NOT NULL,source_revision bigint NOT NULL,
 PRIMARY KEY(tenant_id,site_id,claim_id,claim_revision,span_id),
 FOREIGN KEY(tenant_id,site_id,claim_id,claim_revision)
 REFERENCES supervisor.knowledge_claim_revisions(tenant_id,site_id,claim_id,revision),
 FOREIGN KEY(tenant_id,site_id,span_id,source_id,source_revision)
 REFERENCES supervisor.evidence_spans(tenant_id,site_id,id,source_id,revision)
);
CREATE TABLE supervisor.knowledge_receipts (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,actor_issuer text NOT NULL,actor_subject text NOT NULL,
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
 request_hash text NOT NULL CHECK(request_hash ~ '^[0-9a-f]{64}$'),
 operation text NOT NULL CHECK(operation IN ('entity','claim')),target_id uuid NOT NULL,revision bigint NOT NULL,
 PRIMARY KEY(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id)
);
CREATE TABLE supervisor.knowledge_audit (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),tenant_id uuid NOT NULL,site_id uuid NOT NULL,
 actor_issuer text NOT NULL,actor_subject text NOT NULL,target_id uuid,revision bigint,
 event_type text NOT NULL CHECK(event_type IN ('entity.created','entity.listed','claim.created','claim.revised','claim.read','claim.listed','mutation.replayed')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id)
);
CREATE INDEX knowledge_edges ON supervisor.knowledge_claim_revisions(tenant_id,site_id,predicate,subject_id);
CREATE FUNCTION supervisor.knowledge_head_guard() RETURNS trigger LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp AS $$ BEGIN
 IF ROW(NEW.tenant_id,NEW.site_id,NEW.id) IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.id)
 OR NEW.current_revision<>OLD.current_revision+1 THEN
 RAISE EXCEPTION 'Invalid candidate revision transition' USING ERRCODE='42501'; END IF;
 RETURN NEW; END $$;
CREATE TRIGGER knowledge_head_guard BEFORE UPDATE ON supervisor.knowledge_claim_heads
 FOR EACH ROW EXECUTE FUNCTION supervisor.knowledge_head_guard();
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['knowledge_entities','knowledge_claim_heads','knowledge_claim_revisions','knowledge_claim_spans','knowledge_receipts','knowledge_audit'] LOOP
 EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
 EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
 EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
 EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
 EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 IF t<>'knowledge_claim_heads' THEN
 EXECUTE format('CREATE TRIGGER knowledge_immutable BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t);
 END IF;
 END LOOP; END $$;
GRANT UPDATE(current_revision) ON supervisor.knowledge_claim_heads TO supervisor_runtime;

-- The citations in immutable JSON and normalized composite-FK links must agree at commit.
CREATE FUNCTION supervisor.knowledge_citation_guard() RETURNS trigger LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp AS $$
DECLARE c supervisor.knowledge_claim_revisions%ROWTYPE; r bigint; n integer;
BEGIN
 IF TG_TABLE_NAME='knowledge_claim_spans' THEN r:=NEW.claim_revision; ELSE r:=NEW.revision; END IF;
 SELECT * INTO c FROM supervisor.knowledge_claim_revisions
 WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND claim_id=NEW.claim_id AND revision=r;
 IF NOT FOUND OR jsonb_typeof(c.payload->'source_span_ids') IS DISTINCT FROM 'array' THEN
 RAISE EXCEPTION 'Candidate citations required' USING ERRCODE='23514'; END IF;
 n:=jsonb_array_length(c.payload->'source_span_ids');
 IF n NOT BETWEEN 1 AND 20 OR n<>(SELECT count(*) FROM supervisor.knowledge_claim_spans s
 WHERE (s.tenant_id,s.site_id,s.claim_id,s.claim_revision)=(c.tenant_id,c.site_id,c.claim_id,c.revision))
 OR EXISTS(SELECT 1 FROM jsonb_array_elements_text(c.payload->'source_span_ids') x(id)
 WHERE NOT EXISTS(SELECT 1 FROM supervisor.knowledge_claim_spans s
 WHERE (s.tenant_id,s.site_id,s.claim_id,s.claim_revision)=(c.tenant_id,c.site_id,c.claim_id,c.revision)
 AND s.span_id=x.id::uuid)) THEN
 RAISE EXCEPTION 'Candidate citation links disagree' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE CONSTRAINT TRIGGER knowledge_citation_guard AFTER INSERT ON supervisor.knowledge_claim_revisions
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION supervisor.knowledge_citation_guard();
CREATE CONSTRAINT TRIGGER knowledge_citation_guard AFTER INSERT ON supervisor.knowledge_claim_spans
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION supervisor.knowledge_citation_guard();
