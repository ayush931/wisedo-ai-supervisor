-- WIS-290 bootstrap: the server resolves an invitation's tenant and site from its token,
-- so a redemption request never supplies tenant context (rule 03). The policy below is a
-- second permissive SELECT policy that matches exactly one invitation row: the one whose
-- token hash the current transaction declares. Without that declaration it matches nothing,
-- and tenant_scope still governs every other access. The token is 256 bits of entropy.
CREATE POLICY setup_invitation_by_token ON supervisor.setup_invitations FOR SELECT
    USING (token_hash = nullif(current_setting('supervisor.redeem_token_hash',true),''));
GRANT SELECT (id, tenant_id, site_id, token_hash, expires_at, consumed_at)
    ON supervisor.setup_invitations TO supervisor_runtime;
