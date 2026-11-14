-- WIS-614 onboarding: document passages for an agent turn. A turn may be given a few selected
-- passages (evidence spans) of a source instead of the whole source, so a document larger than one
-- model request can still be read a few passages at a time. When a run captured passages of a
-- source, evidence.get returns only those passages at the captured source revision. Rows are
-- immutable scope history, like agent_run_refs.
CREATE TABLE supervisor.agent_run_spans (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,run_id uuid NOT NULL,
 ordinal smallint NOT NULL CHECK(ordinal BETWEEN 1 AND 8),
 span_id uuid NOT NULL,
 source_id uuid NOT NULL,
 source_revision bigint NOT NULL CHECK(source_revision>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,run_id,span_id),
 UNIQUE(tenant_id,site_id,run_id,ordinal),
 FOREIGN KEY(tenant_id,site_id,run_id) REFERENCES supervisor.agent_runs(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,span_id,source_id,source_revision)
  REFERENCES supervisor.evidence_spans(tenant_id,site_id,id,source_id,revision)
);
CREATE TRIGGER agent_run_spans_immutable BEFORE UPDATE OR DELETE ON supervisor.agent_run_spans
 FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
ALTER TABLE supervisor.agent_run_spans ENABLE ROW LEVEL SECURITY;
ALTER TABLE supervisor.agent_run_spans FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON supervisor.agent_run_spans
 USING(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid);
REVOKE ALL ON supervisor.agent_run_spans FROM PUBLIC,supervisor_runtime;
GRANT SELECT,INSERT ON supervisor.agent_run_spans TO supervisor_runtime;
