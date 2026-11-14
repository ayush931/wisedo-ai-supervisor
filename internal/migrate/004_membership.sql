-- Owner-managed access changes remain explicit, recent-authenticated and auditable.
ALTER TABLE supervisor.memberships ADD COLUMN revision bigint NOT NULL DEFAULT 1 CHECK(revision > 0);
ALTER TABLE supervisor.memberships DROP CONSTRAINT memberships_role_check;
ALTER TABLE supervisor.memberships ADD CONSTRAINT memberships_role_check
    CHECK(role IN ('owner','supervisor','operator','reviewer','viewer'));

CREATE TABLE supervisor.membership_changes (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL,
    site_id uuid NOT NULL,
    actor_issuer text NOT NULL,
    actor_subject text NOT NULL,
    target_issuer text NOT NULL,
    target_subject text NOT NULL,
    idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
    request jsonb NOT NULL,
    previous jsonb,
    result jsonb NOT NULL,
    reauthenticated_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
    FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id)
);
ALTER TABLE supervisor.membership_changes ENABLE ROW LEVEL SECURITY;
ALTER TABLE supervisor.membership_changes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON supervisor.membership_changes
    USING(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid)
    WITH CHECK(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid);
REVOKE ALL ON supervisor.membership_changes FROM PUBLIC,supervisor_runtime;
GRANT SELECT ON supervisor.membership_changes TO supervisor_runtime;

CREATE FUNCTION supervisor.change_membership(
    p_session text,p_tenant uuid,p_site uuid,p_actor_issuer text,p_actor_subject text,
    p_target_issuer text,p_target_subject text,p_role text,p_active boolean,
    p_expected bigint,p_key text)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
SET row_security = on
AS $$
DECLARE
    proof supervisor.browser_sessions%ROWTYPE;
    target supervisor.memberships%ROWTYPE;
    prior supervisor.membership_changes%ROWTYPE;
    payload jsonb;
    before_value jsonb;
    after_value jsonb;
    next_revision bigint;
BEGIN
    IF p_session IS NULL OR p_session !~ '^[0-9a-f]{64}$' OR p_tenant IS NULL OR p_site IS NULL
       OR p_expected IS NULL OR p_expected < 0 OR p_expected >= 9223372036854775807
       OR p_active IS NULL OR p_role IS NULL
       OR p_role NOT IN ('owner','supervisor','operator','reviewer','viewer')
       OR p_key IS NULL OR length(p_key) NOT BETWEEN 1 AND 128
       OR p_target_issuer IS NULL OR length(p_target_issuer) NOT BETWEEN 1 AND 512
       OR p_target_subject IS NULL OR length(p_target_subject) NOT BETWEEN 1 AND 255
       OR p_actor_issuer IS DISTINCT FROM p_target_issuer THEN
        RAISE EXCEPTION 'Invalid membership change' USING ERRCODE='P2910';
    END IF;
    SELECT * INTO proof FROM supervisor.browser_sessions WHERE token_hash=p_session FOR SHARE;
    IF NOT FOUND OR proof.issuer IS DISTINCT FROM p_actor_issuer
       OR proof.subject IS DISTINCT FROM p_actor_subject
       OR proof.expires_at <= clock_timestamp()
       OR proof.last_seen_at <= clock_timestamp()-interval '30 minutes' THEN
        RAISE EXCEPTION 'Membership authority unavailable' USING ERRCODE='P2911';
    END IF;
    IF proof.auth_time IS NULL OR proof.auth_time > clock_timestamp()
       OR proof.auth_time < clock_timestamp()-interval '15 minutes' THEN
        RAISE EXCEPTION 'Recent authentication required' USING ERRCODE='P2912';
    END IF;
    PERFORM set_config('supervisor.tenant_id',p_tenant::text,true);
    -- Serialize administrative changes, but allow the FK KEY SHARE locks used by probe creation.
    PERFORM 1 FROM supervisor.sites WHERE tenant_id=p_tenant AND id=p_site FOR NO KEY UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'Membership authority unavailable' USING ERRCODE='P2911';
    END IF;
    PERFORM 1 FROM supervisor.memberships WHERE tenant_id=p_tenant AND site_id=p_site
        AND issuer=p_actor_issuer AND subject=p_actor_subject AND active AND role='owner' FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'Membership authority unavailable' USING ERRCODE='P2911';
    END IF;
    IF p_actor_subject=p_target_subject THEN
        RAISE EXCEPTION 'Owners cannot change their own membership' USING ERRCODE='P2910';
    END IF;
    payload := jsonb_build_object('issuer',p_target_issuer,'subject',p_target_subject,
        'role',p_role,'active',p_active,'expected_revision',p_expected);
    SELECT * INTO prior FROM supervisor.membership_changes WHERE tenant_id=p_tenant AND site_id=p_site
        AND actor_issuer=p_actor_issuer AND actor_subject=p_actor_subject AND idempotency_key=p_key;
    IF FOUND THEN
        IF prior.request IS DISTINCT FROM payload THEN
            RAISE EXCEPTION 'Idempotency conflict' USING ERRCODE='P2913';
        END IF;
        -- Recheck wall-clock proof after potentially waiting for the site lock, including replay.
        IF proof.expires_at <= clock_timestamp()
           OR proof.last_seen_at <= clock_timestamp()-interval '30 minutes'
           OR proof.auth_time < clock_timestamp()-interval '15 minutes' THEN
            RAISE EXCEPTION 'Recent authentication required' USING ERRCODE='P2912';
        END IF;
        RETURN prior.result;
    END IF;
    SELECT * INTO target FROM supervisor.memberships WHERE tenant_id=p_tenant AND site_id=p_site
        AND issuer=p_target_issuer AND subject=p_target_subject FOR UPDATE;
    IF FOUND THEN
        IF target.revision <> p_expected THEN
            RAISE EXCEPTION 'Membership revision changed' USING ERRCODE='P2914';
        END IF;
        before_value := jsonb_build_object('issuer',target.issuer,'subject',target.subject,
            'role',target.role,'active',target.active,'revision',target.revision);
        IF target.active AND target.role='owner' AND (NOT p_active OR p_role<>'owner')
           AND (SELECT count(*) FROM supervisor.memberships WHERE tenant_id=p_tenant
                AND site_id=p_site AND active AND role='owner') <= 1 THEN
            RAISE EXCEPTION 'Last active owner cannot be removed' USING ERRCODE='P2911';
        END IF;
        next_revision := target.revision+1;
    ELSE
        IF p_expected <> 0 THEN
            RAISE EXCEPTION 'Membership revision changed' USING ERRCODE='P2914';
        END IF;
        IF NOT p_active THEN
            RAISE EXCEPTION 'A new membership must be active' USING ERRCODE='P2910';
        END IF;
        next_revision := 1;
    END IF;
    IF proof.expires_at <= clock_timestamp()
       OR proof.last_seen_at <= clock_timestamp()-interval '30 minutes'
       OR proof.auth_time < clock_timestamp()-interval '15 minutes' THEN
        RAISE EXCEPTION 'Recent authentication required' USING ERRCODE='P2912';
    END IF;
    INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role,active,revision)
        VALUES(p_tenant,p_site,p_target_issuer,p_target_subject,p_role,p_active,next_revision)
        ON CONFLICT(tenant_id,site_id,issuer,subject) DO UPDATE
        SET role=excluded.role,active=excluded.active,revision=excluded.revision;
    after_value := jsonb_build_object('issuer',p_target_issuer,'subject',p_target_subject,
        'role',p_role,'active',p_active,'revision',next_revision);
    INSERT INTO supervisor.membership_changes(tenant_id,site_id,actor_issuer,actor_subject,
        target_issuer,target_subject,idempotency_key,request,previous,result,reauthenticated_at)
        VALUES(p_tenant,p_site,p_actor_issuer,p_actor_subject,p_target_issuer,p_target_subject,
            p_key,payload,before_value,after_value,proof.auth_time);
    RETURN after_value;
END;
$$;
REVOKE ALL ON FUNCTION supervisor.change_membership(text,uuid,uuid,text,text,text,text,text,boolean,bigint,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION supervisor.change_membership(text,uuid,uuid,text,text,text,text,text,boolean,bigint,text) TO supervisor_runtime;
