-- Existing sites remain valid without a timezone; supervisor provision requires one for new sites.
ALTER TABLE supervisor.sites ADD COLUMN timezone text;

CREATE TABLE supervisor.provisioning_audit (
 request_id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL,
 site_id uuid NOT NULL,
 operator text NOT NULL CHECK(length(operator) BETWEEN 1 AND 255),
 os_user text NOT NULL CHECK(length(os_user) BETWEEN 1 AND 255),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id)
);
ALTER TABLE supervisor.provisioning_audit ENABLE ROW LEVEL SECURITY;
ALTER TABLE supervisor.provisioning_audit FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON supervisor.provisioning_audit
 USING (tenant_id = nullif(current_setting('supervisor.tenant_id',true),'')::uuid)
 WITH CHECK (tenant_id = nullif(current_setting('supervisor.tenant_id',true),'')::uuid);
-- The migration role alone needs global request-id lookup for idempotency before tenant scope exists.
CREATE POLICY provisioning_admin ON supervisor.provisioning_audit TO supervisor_migrator
 USING (true) WITH CHECK (true);
REVOKE ALL ON supervisor.provisioning_audit FROM PUBLIC,supervisor_runtime;
GRANT SELECT,INSERT ON supervisor.provisioning_audit TO supervisor_migrator;

CREATE FUNCTION supervisor.reject_provisioning_audit_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'provisioning_audit is append-only' USING ERRCODE='55000';
END;
$$;
CREATE TRIGGER provisioning_audit_append_only
BEFORE UPDATE OR DELETE ON supervisor.provisioning_audit
FOR EACH ROW EXECUTE FUNCTION supervisor.reject_provisioning_audit_mutation();
