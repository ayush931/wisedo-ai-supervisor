-- Initial owner setup only. Existing memberships are never elevated or reactivated.
CREATE TABLE supervisor.setup_invitations (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL,
    site_id uuid NOT NULL,
    token_hash text NOT NULL UNIQUE CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    issuer text NOT NULL CHECK (length(issuer) BETWEEN 1 AND 512),
    subject text NOT NULL CHECK (length(subject) BETWEEN 1 AND 255),
    role text NOT NULL CHECK (role = 'owner'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    CHECK (expires_at > created_at AND expires_at <= created_at + interval '24 hours'),
    UNIQUE (tenant_id,site_id,id),
    FOREIGN KEY (tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id)
);
CREATE TABLE supervisor.bootstrap_audit (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL,
    site_id uuid NOT NULL,
    invitation_id uuid NOT NULL,
    issuer text NOT NULL,
    subject text NOT NULL,
    database_actor text NOT NULL,
    event_type text NOT NULL CHECK (event_type IN ('bootstrap.invitation_issued','bootstrap.owner_enrolled')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (tenant_id,site_id,invitation_id)
        REFERENCES supervisor.setup_invitations(tenant_id,site_id,id)
);
ALTER TABLE supervisor.setup_invitations ENABLE ROW LEVEL SECURITY;
ALTER TABLE supervisor.setup_invitations FORCE ROW LEVEL SECURITY;
ALTER TABLE supervisor.bootstrap_audit ENABLE ROW LEVEL SECURITY;
ALTER TABLE supervisor.bootstrap_audit FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON supervisor.setup_invitations
    USING (tenant_id = nullif(current_setting('supervisor.tenant_id',true),'')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('supervisor.tenant_id',true),'')::uuid);
CREATE POLICY tenant_scope ON supervisor.bootstrap_audit
    USING (tenant_id = nullif(current_setting('supervisor.tenant_id',true),'')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('supervisor.tenant_id',true),'')::uuid);
REVOKE ALL ON supervisor.setup_invitations,supervisor.bootstrap_audit FROM PUBLIC,supervisor_runtime;

-- The application sets issuer/subject from its authenticated session, never body fields.
-- Runtime can execute only this narrow operation; it cannot mint invitations or
-- INSERT memberships. All user relations are schema-qualified and search_path fixed.
CREATE FUNCTION supervisor.redeem_setup_invitation(p_token_hash text,p_tenant uuid,p_site uuid)
RETURNS text LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
SET row_security = on
AS $$
DECLARE
    invitation supervisor.setup_invitations%ROWTYPE;
    actor_issuer text := current_setting('supervisor.issuer',true);
    actor_subject text := current_setting('supervisor.subject',true);
BEGIN
    IF nullif(actor_issuer,'') IS NULL OR nullif(actor_subject,'') IS NULL
       OR current_setting('supervisor.tenant_id',true) IS DISTINCT FROM p_tenant::text THEN
        RAISE EXCEPTION 'Invalid setup invitation' USING ERRCODE='P2901';
    END IF;
    -- ponytail: serialize initial ownership per site; use a narrower claim row if
    -- a later, separately authorized membership-invitation feature needs throughput.
    PERFORM 1 FROM supervisor.sites WHERE tenant_id=p_tenant AND id=p_site FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'Invalid setup invitation' USING ERRCODE='P2901';
    END IF;
    SELECT * INTO invitation FROM supervisor.setup_invitations
        WHERE token_hash=p_token_hash AND tenant_id=p_tenant AND site_id=p_site FOR UPDATE;
    IF NOT FOUND OR invitation.consumed_at IS NOT NULL
       OR invitation.expires_at <= clock_timestamp()
       OR invitation.issuer IS DISTINCT FROM actor_issuer
       OR invitation.subject IS DISTINCT FROM actor_subject THEN
        RAISE EXCEPTION 'Invalid setup invitation' USING ERRCODE='P2901';
    END IF;
    IF EXISTS (SELECT 1 FROM supervisor.memberships WHERE tenant_id=p_tenant AND site_id=p_site
               AND role='owner' AND active)
       OR EXISTS (SELECT 1 FROM supervisor.memberships WHERE tenant_id=p_tenant AND site_id=p_site
                  AND issuer=actor_issuer AND subject=actor_subject) THEN
        RAISE EXCEPTION 'Invalid setup invitation' USING ERRCODE='P2901';
    END IF;
    INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role,active)
        VALUES (p_tenant,p_site,actor_issuer,actor_subject,'owner',true);
    UPDATE supervisor.setup_invitations SET consumed_at=clock_timestamp() WHERE id=invitation.id;
    INSERT INTO supervisor.bootstrap_audit(tenant_id,site_id,invitation_id,issuer,subject,database_actor,event_type)
        VALUES (p_tenant,p_site,invitation.id,actor_issuer,actor_subject,session_user,'bootstrap.owner_enrolled');
    RETURN 'owner';
END;
$$;
REVOKE ALL ON FUNCTION supervisor.redeem_setup_invitation(text,uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION supervisor.redeem_setup_invitation(text,uuid,uuid) TO supervisor_runtime;
