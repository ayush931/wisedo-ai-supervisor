-- NULL remains the active state; the existing runtime grant still allows SELECT only.
ALTER TABLE supervisor.tenants ADD COLUMN deactivated_at timestamptz;

ALTER TABLE supervisor.provisioning_audit
 ADD COLUMN action text NOT NULL DEFAULT 'tenant.provisioned'
 CHECK (action IN ('tenant.provisioned','tenant.deactivated'));

CREATE FUNCTION supervisor.active_tenant_ids()
RETURNS SETOF uuid LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
SET row_security = on
AS $$ SELECT t.id FROM supervisor.tenants t WHERE t.deactivated_at IS NULL $$;
REVOKE ALL ON FUNCTION supervisor.active_tenant_ids() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION supervisor.active_tenant_ids() TO supervisor_runtime;

-- FORCE RLS applies to the non-bypass definer owner too, so only that migration role can enumerate.
DO $$ BEGIN
 EXECUTE format('CREATE POLICY tenant_discovery ON supervisor.tenants FOR SELECT TO %I USING (true)', current_user);
END $$;
