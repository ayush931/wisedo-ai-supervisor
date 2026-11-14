CREATE TABLE supervisor.configuration_imports (
 tenant_id uuid NOT NULL,
 site_id uuid NOT NULL,
 id uuid NOT NULL,
 actor_issuer text NOT NULL CHECK (length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK (length(actor_subject) BETWEEN 1 AND 255),
 idempotency_key text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 128),
 file_sha256 text NOT NULL CHECK (file_sha256 ~ '^[0-9a-f]{64}$'),
 report_sha256 text NOT NULL CHECK (report_sha256 ~ '^[0-9a-f]{64}$'),
 accepted_rows integer NOT NULL CHECK (accepted_rows >= 0),
 duplicate_rows integer NOT NULL CHECK (duplicate_rows >= 0),
 report jsonb NOT NULL CHECK (octet_length(report::text) <= 8388608),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (tenant_id,site_id,id),
 UNIQUE (tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY (tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id)
);

ALTER TABLE supervisor.configuration_imports ENABLE ROW LEVEL SECURITY;
ALTER TABLE supervisor.configuration_imports FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON supervisor.configuration_imports
 USING (tenant_id = nullif(current_setting('supervisor.tenant_id',true),'')::uuid)
 WITH CHECK (tenant_id = nullif(current_setting('supervisor.tenant_id',true),'')::uuid);
REVOKE ALL ON supervisor.configuration_imports FROM PUBLIC, supervisor_runtime;
GRANT SELECT, INSERT ON supervisor.configuration_imports TO supervisor_runtime;
CREATE TRIGGER configuration_imports_immutable BEFORE UPDATE OR DELETE ON supervisor.configuration_imports
 FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
