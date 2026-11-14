-- Immutable approved-knowledge cuts; no snapshot grants operational permission.
CREATE TABLE supervisor.knowledge_process_snapshots (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,snapshot_id uuid NOT NULL,
 schema_version integer NOT NULL DEFAULT 1 CHECK(schema_version=1),
 inventory_revision bigint NOT NULL CHECK(inventory_revision>=0),
 sha256 text NOT NULL CHECK(sha256 ~ '^[0-9a-f]{64}$'),
 canonical_contents text NOT NULL CHECK(octet_length(canonical_contents)<=65536 AND jsonb_typeof(canonical_contents::jsonb)='object'),
 reason text NOT NULL CHECK(length(reason) BETWEEN 1 AND 1000 AND length(btrim(reason))>0),
 actor_issuer text NOT NULL,actor_subject text NOT NULL,auth_time timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK(sha256=encode(sha256(convert_to(canonical_contents,'UTF8')),'hex')),
 CHECK(auth_time<=created_at AND auth_time>=created_at-interval '15 minutes'),
 CHECK((canonical_contents::jsonb->>'schema_version')::integer IS NOT DISTINCT FROM schema_version),
 CHECK((canonical_contents::jsonb->>'tenant_id') IS NOT DISTINCT FROM tenant_id::text),
 CHECK((canonical_contents::jsonb->>'site_id') IS NOT DISTINCT FROM site_id::text),
 CHECK((canonical_contents::jsonb->>'inventory_revision')::bigint IS NOT DISTINCT FROM inventory_revision),
 PRIMARY KEY(tenant_id,site_id,snapshot_id),
 UNIQUE(tenant_id,site_id,snapshot_id,inventory_revision,sha256),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id)
);
CREATE TABLE supervisor.knowledge_snapshot_claims (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,snapshot_id uuid NOT NULL,
 claim_id uuid NOT NULL,claim_revision bigint NOT NULL,review_id uuid NOT NULL,
 PRIMARY KEY(tenant_id,site_id,snapshot_id,claim_id),
 FOREIGN KEY(tenant_id,site_id,snapshot_id) REFERENCES supervisor.knowledge_process_snapshots(tenant_id,site_id,snapshot_id),
 FOREIGN KEY(tenant_id,site_id,review_id,claim_id,claim_revision) REFERENCES supervisor.knowledge_claim_reviews(tenant_id,site_id,review_id,claim_id,claim_revision)
);
CREATE TABLE supervisor.knowledge_snapshot_entities (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,snapshot_id uuid NOT NULL,
 entity_id uuid NOT NULL,entity_revision bigint NOT NULL,
 PRIMARY KEY(tenant_id,site_id,snapshot_id,entity_id),
 FOREIGN KEY(tenant_id,site_id,snapshot_id) REFERENCES supervisor.knowledge_process_snapshots(tenant_id,site_id,snapshot_id),
 FOREIGN KEY(tenant_id,site_id,entity_id,entity_revision) REFERENCES supervisor.knowledge_entity_revisions(tenant_id,site_id,id,revision)
);
-- Metadata-only transactional event. Delivery/acknowledgement is a later increment.
CREATE TABLE supervisor.knowledge_outbox (
 event_id uuid NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,snapshot_id uuid NOT NULL,
 schema_version integer NOT NULL DEFAULT 1 CHECK(schema_version=1),
 event_type text NOT NULL DEFAULT 'snapshot.created' CHECK(event_type='snapshot.created'),
 inventory_revision bigint NOT NULL,sha256 text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(tenant_id,site_id,snapshot_id),
 FOREIGN KEY(tenant_id,site_id,snapshot_id,inventory_revision,sha256) REFERENCES supervisor.knowledge_process_snapshots(tenant_id,site_id,snapshot_id,inventory_revision,sha256)
);
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['knowledge_process_snapshots','knowledge_snapshot_claims','knowledge_snapshot_entities','knowledge_outbox'] LOOP
 EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
 EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
 EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
 EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
 EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 EXECUTE format('CREATE TRIGGER knowledge_immutable BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t);
 END LOOP;
END $$;
ALTER TABLE supervisor.knowledge_receipts DROP CONSTRAINT knowledge_receipts_operation_check;
ALTER TABLE supervisor.knowledge_receipts ADD CONSTRAINT knowledge_receipts_operation_check CHECK(operation IN ('entity','claim','review','review_revoke','resolution','snapshot'));
ALTER TABLE supervisor.knowledge_audit DROP CONSTRAINT knowledge_audit_event_type_check;
ALTER TABLE supervisor.knowledge_audit ADD CONSTRAINT knowledge_audit_event_type_check CHECK(event_type IN ('entity.created','entity.revised','entity.read','entity.listed','claim.created','claim.revised','claim.read','claim.listed','mutation.replayed','conflicts.read','review.decided','review.read','review.revoked','resolution.decided','resolution.read','inventory.read','snapshot.created','snapshot.read'));
