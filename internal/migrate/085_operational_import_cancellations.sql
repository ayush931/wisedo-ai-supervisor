-- WIS-719 follow-up: an owner or supervisor may cancel a running operational import (for example
-- one left running by a crashed process). Each cancellation leaves one append-only receipt naming
-- the actor and the Idempotency-Key: the audit of who ended the job, and the record a same-key
-- retry replays. A job has at most one receipt; the lease, abandon and revocation paths write none.
CREATE TABLE supervisor.operational_import_cancellations (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,job_id uuid NOT NULL,
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,job_id),
 UNIQUE(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id,job_id) REFERENCES supervisor.operational_import_jobs(tenant_id,site_id,id)
);
CREATE TRIGGER operational_import_cancellation_immutable BEFORE UPDATE OR DELETE ON supervisor.operational_import_cancellations
 FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
ALTER TABLE supervisor.operational_import_cancellations ENABLE ROW LEVEL SECURITY;
ALTER TABLE supervisor.operational_import_cancellations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON supervisor.operational_import_cancellations
 USING(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid);
REVOKE ALL ON supervisor.operational_import_cancellations FROM PUBLIC,supervisor_runtime;
GRANT SELECT,INSERT ON supervisor.operational_import_cancellations TO supervisor_runtime;
