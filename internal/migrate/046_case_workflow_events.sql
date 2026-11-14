-- WIS-273 workflow event ledger. Outbox delivery is at least once and Temporal signals can be
-- duplicated or reordered, so SupervisorCaseWorkflow records each event ID here before acting
-- on it: the product database, not workflow history, decides that an event was already seen.
CREATE TABLE supervisor.case_workflow_events (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,case_id uuid NOT NULL,
 event_id uuid NOT NULL,
 kind text NOT NULL CHECK(kind IN ('start','signal','cancel')),
 case_revision bigint NOT NULL CHECK(case_revision>=1),
 processed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,case_id,event_id),
 FOREIGN KEY(tenant_id,site_id,case_id) REFERENCES supervisor.cases(tenant_id,site_id,id)
);
CREATE TRIGGER case_workflow_events_immutable BEFORE UPDATE OR DELETE ON supervisor.case_workflow_events
 FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
ALTER TABLE supervisor.case_workflow_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE supervisor.case_workflow_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON supervisor.case_workflow_events
 USING(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid);
REVOKE ALL ON supervisor.case_workflow_events FROM PUBLIC,supervisor_runtime;
GRANT SELECT,INSERT ON supervisor.case_workflow_events TO supervisor_runtime;
