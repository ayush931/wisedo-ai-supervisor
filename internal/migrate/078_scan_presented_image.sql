-- WIS-625 post-merge review: a scan job is replayed by its idempotency key only for the same image as
-- the vision model would read it. The embedded image's digest (original_sha256) and the title
-- alone let a re-upload under the same key with another /Rotate or placement, which changes the
-- image sent, replay the old job. presented_sha256 is the SHA-256 of the image bytes queued for
-- the model. Jobs queued before this have none and replay as they did.
ALTER TABLE supervisor.document_scan_jobs ADD COLUMN presented_sha256 text CHECK(presented_sha256 IS NULL OR presented_sha256 ~ '^[0-9a-f]{64}$');

-- The digest is set when the job is queued and never changes (rule 06: no destructive overwrite):
-- the guard counts it among the immutable columns. The function is 076's with presented_sha256
-- added to both rows.
CREATE OR REPLACE FUNCTION supervisor.document_scan_job_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'scan jobs are retained' USING ERRCODE='42501'; END IF;
 IF OLD.state<>'running' OR NEW.state='running' OR NEW.image IS NOT NULL
    OR ROW(NEW.tenant_id,NEW.site_id,NEW.job_id,NEW.uploader_issuer,NEW.uploader_subject,NEW.idempotency_key,NEW.title,NEW.original_sha256,NEW.media_type,NEW.width,NEW.height,NEW.run_id,NEW.created_at,NEW.presented_sha256)
       IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.job_id,OLD.uploader_issuer,OLD.uploader_subject,OLD.idempotency_key,OLD.title,OLD.original_sha256,OLD.media_type,OLD.width,OLD.height,OLD.run_id,OLD.created_at,OLD.presented_sha256) THEN
  RAISE EXCEPTION 'invalid scan job transition' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
