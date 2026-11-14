-- WIS-273 automatic triggering. Every context.created row in plant_context_outbox is evaluated
-- once against the shortfall rule through the same checked, idempotent trigger path; this ledger
-- records the outcome so a context is never evaluated twice and a failed evaluation (no row) is
-- retried on the next pass. It references the case a trigger opened or joined.
CREATE TABLE supervisor.case_context_evaluations (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,context_id uuid NOT NULL,
 outcome text NOT NULL CHECK(outcome IN ('opened','joined','already_triggered','not_qualified','not_applicable')),
 case_id uuid,
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 evaluated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((outcome IN ('opened','joined'))=(case_id IS NOT NULL)),
 PRIMARY KEY(tenant_id,site_id,context_id),
 FOREIGN KEY(tenant_id,site_id,context_id) REFERENCES supervisor.plant_contexts(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,case_id) REFERENCES supervisor.cases(tenant_id,site_id,id)
);
CREATE TRIGGER case_context_evaluations_immutable BEFORE UPDATE OR DELETE ON supervisor.case_context_evaluations
 FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
ALTER TABLE supervisor.case_context_evaluations ENABLE ROW LEVEL SECURITY;
ALTER TABLE supervisor.case_context_evaluations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON supervisor.case_context_evaluations
 USING(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid);
REVOKE ALL ON supervisor.case_context_evaluations FROM PUBLIC,supervisor_runtime;
GRANT SELECT,INSERT ON supervisor.case_context_evaluations TO supervisor_runtime;
