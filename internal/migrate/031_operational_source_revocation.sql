-- WIS-272 source revocation: "Revocation transaction increments access-policy revision and
-- disables source; cancels/blocks pending jobs, marks derived claims/contexts ineligible,
-- denies future retrieval/use immediately through authoritative checks."
--
-- 1. An import job can end 'cancelled' when its source is revoked while it runs. The existing
--    guard (026) already allows the running -> finished transition and fixes job identity.
ALTER TABLE supervisor.operational_import_jobs DROP CONSTRAINT operational_import_jobs_status_check;
ALTER TABLE supervisor.operational_import_jobs ADD CONSTRAINT operational_import_jobs_status_check
 CHECK(status IN ('running','completed','partial','cancelled'));

-- 2. A source's access-policy revision never decreases across its revisions. Derived contexts
--    record the access-policy revision they were built under and are ineligible once it differs;
--    lowering it again after a revocation would make revoked contexts eligible again. The
--    application rejects a decrease with 422; this trigger is the database backstop.
CREATE FUNCTION supervisor.operational_source_access_policy_guard() RETURNS trigger LANGUAGE plpgsql
 SET search_path=pg_catalog,pg_temp AS $$
DECLARE previous bigint;
BEGIN
 SELECT r.access_policy_revision INTO previous FROM supervisor.operational_source_revisions r
  WHERE (r.tenant_id,r.site_id,r.source_id)=(NEW.tenant_id,NEW.site_id,NEW.source_id) AND r.revision=NEW.revision-1;
 IF previous IS NOT NULL AND NEW.access_policy_revision<previous THEN
  RAISE EXCEPTION 'operational source access-policy revision cannot decrease' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER operational_source_access_policy_guard BEFORE INSERT ON supervisor.operational_source_revisions
 FOR EACH ROW EXECUTE FUNCTION supervisor.operational_source_access_policy_guard();
