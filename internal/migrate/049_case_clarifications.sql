-- WIS-273 clarifications. A clarification is attributed source evidence for a case waiting on
-- input: it is appended here, never written over an observation, in the same transaction as the
-- needs_input -> investigating transition it causes (case_revision is that transition).
CREATE TABLE supervisor.case_clarifications (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL,
 case_id uuid NOT NULL,case_revision bigint NOT NULL CHECK(case_revision>=2),
 body text NOT NULL CHECK(length(body) BETWEEN 1 AND 4000 AND length(btrim(body))>0),
 body_sha256 text NOT NULL CHECK(body_sha256=encode(sha256(convert_to(body,'UTF8')),'hex')),
 context_snapshot_id uuid,
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,id),
 UNIQUE(tenant_id,site_id,case_id,case_revision),
 FOREIGN KEY(tenant_id,site_id,case_id,case_revision) REFERENCES supervisor.case_transitions(tenant_id,site_id,case_id,revision),
 FOREIGN KEY(tenant_id,site_id,context_snapshot_id) REFERENCES supervisor.plant_contexts(tenant_id,site_id,id)
);
CREATE TRIGGER case_clarifications_immutable BEFORE UPDATE OR DELETE ON supervisor.case_clarifications
 FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
ALTER TABLE supervisor.case_clarifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE supervisor.case_clarifications FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON supervisor.case_clarifications
 USING(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid);
REVOKE ALL ON supervisor.case_clarifications FROM PUBLIC,supervisor_runtime;
GRANT SELECT,INSERT ON supervisor.case_clarifications TO supervisor_runtime;

-- Clarification receipts use the existing lifecycle receipt table.
ALTER TABLE supervisor.case_receipts DROP CONSTRAINT case_receipts_operation_check;
ALTER TABLE supervisor.case_receipts ADD CONSTRAINT case_receipts_operation_check
 CHECK(operation IN ('case.cancel','case.resume','case.reopen','case.clarify'));
