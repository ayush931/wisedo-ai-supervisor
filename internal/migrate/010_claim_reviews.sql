-- Human decisions pin exact claim revisions and one directly cited authority decision.
-- The immutable verdict never grants operational permission; eligibility is recomputed.
ALTER TABLE supervisor.evidence_authority_decisions ADD CONSTRAINT evidence_authority_review_reference_key
 UNIQUE(tenant_id,site_id,source_id,source_revision,revision,decision_id);
CREATE TABLE supervisor.knowledge_claim_reviews (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,review_id uuid NOT NULL UNIQUE,
 schema_version integer NOT NULL DEFAULT 1 CHECK(schema_version=1),
 claim_id uuid NOT NULL,claim_revision bigint NOT NULL,
 verdict text NOT NULL CHECK(verdict IN ('approve','reject')),
 reason text NOT NULL CHECK(length(reason) BETWEEN 1 AND 1000 AND length(btrim(reason))>0),
 actor_issuer text NOT NULL,actor_subject text NOT NULL,auth_time timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 authority_decision_id uuid,authority_source_id uuid,authority_source_revision bigint,authority_revision bigint,
 CHECK(auth_time<=created_at AND auth_time>=created_at-interval '15 minutes'),
 CHECK((verdict='approve' AND authority_decision_id IS NOT NULL AND authority_source_id IS NOT NULL AND authority_source_revision IS NOT NULL AND authority_revision IS NOT NULL)
 OR (verdict='reject' AND authority_decision_id IS NULL AND authority_source_id IS NULL AND authority_source_revision IS NULL AND authority_revision IS NULL)),
 PRIMARY KEY(tenant_id,site_id,claim_id,claim_revision),
 FOREIGN KEY(tenant_id,site_id,claim_id,claim_revision) REFERENCES supervisor.knowledge_claim_revisions(tenant_id,site_id,claim_id,revision),
 FOREIGN KEY(tenant_id,site_id,authority_source_id,authority_source_revision,authority_revision,authority_decision_id)
 REFERENCES supervisor.evidence_authority_decisions(tenant_id,site_id,source_id,source_revision,revision,decision_id)
);
ALTER TABLE supervisor.knowledge_claim_reviews ENABLE ROW LEVEL SECURITY;
ALTER TABLE supervisor.knowledge_claim_reviews FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON supervisor.knowledge_claim_reviews USING(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid) WITH CHECK(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid);
REVOKE ALL ON supervisor.knowledge_claim_reviews FROM PUBLIC,supervisor_runtime;
GRANT SELECT,INSERT ON supervisor.knowledge_claim_reviews TO supervisor_runtime;
CREATE TRIGGER knowledge_immutable BEFORE UPDATE OR DELETE ON supervisor.knowledge_claim_reviews FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
ALTER TABLE supervisor.knowledge_receipts DROP CONSTRAINT knowledge_receipts_operation_check;
ALTER TABLE supervisor.knowledge_receipts ADD CONSTRAINT knowledge_receipts_operation_check CHECK(operation IN ('entity','claim','review'));
ALTER TABLE supervisor.knowledge_audit DROP CONSTRAINT knowledge_audit_event_type_check;
ALTER TABLE supervisor.knowledge_audit ADD CONSTRAINT knowledge_audit_event_type_check CHECK(event_type IN ('entity.created','entity.revised','entity.read','entity.listed','claim.created','claim.revised','claim.read','claim.listed','mutation.replayed','conflicts.read','review.decided','review.read'));
