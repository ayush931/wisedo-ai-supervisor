-- WIS-277 OutcomeObservationV1: after a reported task completion, whether production met the plan
-- again in the next two complete productive windows. One immutable observation per completion
-- epoch; a reopen and a new completion is a new epoch. The windows are kept with their exact values
-- and snapshot digests (assessed, not persisted as contexts). Observed improvement is threshold
-- recovery, never causation, and a case closure reason never overwrites it.
-- An observation names its task's own case (review of #191, minor 7): the composite key below makes
-- a mismatched task and case unrepresentable.
ALTER TABLE supervisor.tasks ADD CONSTRAINT tasks_case_key UNIQUE(tenant_id,site_id,id,case_id);
CREATE TABLE supervisor.outcome_observations (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,task_id uuid NOT NULL,epoch integer NOT NULL CHECK(epoch>0),
 case_id uuid NOT NULL,baseline_context_id uuid NOT NULL,
 status text NOT NULL CHECK(status IN ('observed_improvement','no_improvement','insufficient_data','not_comparable')),
 rule_version text NOT NULL CHECK(length(rule_version) BETWEEN 1 AND 64),
 completed_at timestamptz NOT NULL,
 windows jsonb NOT NULL CHECK(jsonb_typeof(windows)='array' AND octet_length(windows::text)<=1048576),
 interpretation text NOT NULL CHECK(char_length(interpretation) BETWEEN 1 AND 2000),
 observed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 observer_issuer text NOT NULL CHECK(length(observer_issuer) BETWEEN 1 AND 512),
 observer_subject text NOT NULL CHECK(length(observer_subject) BETWEEN 1 AND 255),
 PRIMARY KEY(tenant_id,site_id,task_id,epoch),
 FOREIGN KEY(tenant_id,site_id,task_id,epoch) REFERENCES supervisor.task_completions(tenant_id,site_id,task_id,epoch),
 FOREIGN KEY(tenant_id,site_id,case_id) REFERENCES supervisor.cases(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,task_id,case_id) REFERENCES supervisor.tasks(tenant_id,site_id,id,case_id),
 FOREIGN KEY(tenant_id,site_id,baseline_context_id) REFERENCES supervisor.plant_contexts(tenant_id,site_id,id)
);
ALTER TABLE supervisor.outcome_observations ENABLE ROW LEVEL SECURITY;
ALTER TABLE supervisor.outcome_observations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON supervisor.outcome_observations USING(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid) WITH CHECK(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid);
REVOKE ALL ON supervisor.outcome_observations FROM PUBLIC,supervisor_runtime;
GRANT SELECT,INSERT ON supervisor.outcome_observations TO supervisor_runtime;
CREATE TRIGGER outcome_observations_immutable BEFORE UPDATE OR DELETE ON supervisor.outcome_observations FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();

-- Case close (supervisor-only, after the task is terminal and its outcome recorded) is idempotent.
ALTER TABLE supervisor.case_receipts DROP CONSTRAINT case_receipts_operation_check;
ALTER TABLE supervisor.case_receipts ADD CONSTRAINT case_receipts_operation_check
 CHECK(operation IN ('case.cancel','case.resume','case.reopen','case.clarify','case.close'));
