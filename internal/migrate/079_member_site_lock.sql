-- WIS-699 hardening: lock_evidence_site (005) checks the tenant but not site membership, so a
-- member of one site could take another site's row lock in the same tenant until the Go
-- membership check after it refused the request. Nothing leaked, but the other site's writers
-- waited (lock contention). lock_member_site first reads an active membership for the actor,
-- without a row lock, so the global lock order is unchanged: the authoritative membership
-- FOR SHARE read still runs after the site lock and still serializes against revocation.
-- A non-member gets false and takes no lock. Request paths with a member actor call this;
-- worker paths with no member actor keep lock_evidence_site. CREATE OR REPLACE lets the 008-baseline
-- upgrade test (internal/knowledge) install it before running the current service on that schema.
CREATE OR REPLACE FUNCTION supervisor.lock_member_site(p_tenant uuid,p_site uuid,p_issuer text,p_subject text,p_write boolean)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,pg_temp SET row_security=on AS $$
BEGIN
 IF p_tenant IS NULL OR p_site IS NULL OR p_issuer IS NULL OR p_subject IS NULL OR p_write IS NULL
 OR current_setting('supervisor.tenant_id',true) IS DISTINCT FROM p_tenant::text THEN
  RETURN false;
 END IF;
 PERFORM 1 FROM supervisor.memberships
  WHERE tenant_id=p_tenant AND site_id=p_site AND issuer=p_issuer AND subject=p_subject AND active;
 IF NOT FOUND THEN
  RETURN false;
 END IF;
 IF p_write THEN
  PERFORM 1 FROM supervisor.sites WHERE tenant_id=p_tenant AND id=p_site FOR NO KEY UPDATE;
 ELSE
  PERFORM 1 FROM supervisor.sites WHERE tenant_id=p_tenant AND id=p_site FOR KEY SHARE;
 END IF;
 RETURN FOUND;
END $$;
REVOKE ALL ON FUNCTION supervisor.lock_member_site(uuid,uuid,text,text,boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION supervisor.lock_member_site(uuid,uuid,text,text,boolean) TO supervisor_runtime;
