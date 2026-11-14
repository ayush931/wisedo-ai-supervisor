-- WIS-290 member display names. supervisor.memberships is owner-administered (the
-- membership_admin_only trigger and the revisioned change_membership function), so the
-- IdP display name lives beside it instead of widening runtime UPDATE on memberships.
-- One row per tenant and principal, written only at login from the verified ID token.
CREATE TABLE supervisor.member_profiles (
    tenant_id uuid NOT NULL REFERENCES supervisor.tenants(id),
    issuer text NOT NULL CHECK(length(issuer) BETWEEN 1 AND 512),
    subject text NOT NULL CHECK(length(subject) BETWEEN 1 AND 255),
    -- NULL: the IdP offered no acceptable name; the UI falls back to the subject.
    display_name text CHECK(display_name IS NULL OR char_length(display_name) BETWEEN 1 AND 128),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY(tenant_id,issuer,subject)
);
ALTER TABLE supervisor.member_profiles ENABLE ROW LEVEL SECURITY;
ALTER TABLE supervisor.member_profiles FORCE ROW LEVEL SECURITY;
-- Site directory reads within the request's tenant.
CREATE POLICY tenant_scope ON supervisor.member_profiles FOR SELECT
    USING(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid);
-- Login writes only the authenticated principal's own rows, and only in tenants where it
-- holds an active membership. USING admits the existing row for ON CONFLICT DO UPDATE
-- across every tenant in one statement.
CREATE POLICY self_profile ON supervisor.member_profiles
    USING(issuer=current_setting('supervisor.issuer',true) AND subject=current_setting('supervisor.subject',true))
    WITH CHECK(issuer=current_setting('supervisor.issuer',true) AND subject=current_setting('supervisor.subject',true)
        AND EXISTS(SELECT 1 FROM supervisor.memberships m WHERE m.tenant_id=member_profiles.tenant_id
            AND m.issuer=member_profiles.issuer AND m.subject=member_profiles.subject AND m.active));
REVOKE ALL ON supervisor.member_profiles FROM PUBLIC,supervisor_runtime;
GRANT SELECT,INSERT ON supervisor.member_profiles TO supervisor_runtime;
GRANT UPDATE(display_name,updated_at) ON supervisor.member_profiles TO supervisor_runtime;
