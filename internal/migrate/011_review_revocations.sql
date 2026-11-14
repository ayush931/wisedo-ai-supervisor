-- Approval withdrawal is append-only and never edits the original human verdict.
ALTER TABLE supervisor.knowledge_claim_reviews ADD CONSTRAINT knowledge_review_revocation_reference_key
 UNIQUE(tenant_id,site_id,review_id,claim_id,claim_revision);
CREATE TABLE supervisor.knowledge_review_revocations (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,revocation_id uuid NOT NULL UNIQUE,
 schema_version integer NOT NULL DEFAULT 1 CHECK(schema_version=1),
 review_id uuid NOT NULL,claim_id uuid NOT NULL,claim_revision bigint NOT NULL,
 reason text NOT NULL CHECK(length(reason) BETWEEN 1 AND 1000 AND length(btrim(reason))>0),
 actor_issuer text NOT NULL,actor_subject text NOT NULL,auth_time timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK(auth_time<=created_at AND auth_time>=created_at-interval '15 minutes'),
 PRIMARY KEY(tenant_id,site_id,review_id),
 FOREIGN KEY(tenant_id,site_id,review_id,claim_id,claim_revision)
 REFERENCES supervisor.knowledge_claim_reviews(tenant_id,site_id,review_id,claim_id,claim_revision)
);
ALTER TABLE supervisor.knowledge_review_revocations ENABLE ROW LEVEL SECURITY;
ALTER TABLE supervisor.knowledge_review_revocations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON supervisor.knowledge_review_revocations USING(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid) WITH CHECK(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid);
REVOKE ALL ON supervisor.knowledge_review_revocations FROM PUBLIC,supervisor_runtime;
GRANT SELECT,INSERT ON supervisor.knowledge_review_revocations TO supervisor_runtime;
CREATE TRIGGER knowledge_immutable BEFORE UPDATE OR DELETE ON supervisor.knowledge_review_revocations FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
ALTER TABLE supervisor.knowledge_receipts DROP CONSTRAINT knowledge_receipts_operation_check;
ALTER TABLE supervisor.knowledge_receipts ADD CONSTRAINT knowledge_receipts_operation_check CHECK(operation IN ('entity','claim','review','review_revoke'));
ALTER TABLE supervisor.knowledge_audit DROP CONSTRAINT knowledge_audit_event_type_check;
ALTER TABLE supervisor.knowledge_audit ADD CONSTRAINT knowledge_audit_event_type_check CHECK(event_type IN ('entity.created','entity.revised','entity.read','entity.listed','claim.created','claim.revised','claim.read','claim.listed','mutation.replayed','conflicts.read','review.decided','review.read','review.revoked'));
