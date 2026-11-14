-- WIS-645 slice 2: a PDF with typed and scanned pages is one upload. Its typed pages are stored as
-- one document source (typed text, never model-extracted) and its scanned pages as scan jobs; the
-- upload row links both, so a reviewer sees the whole file. Typed and model-extracted text are
-- never one source (rule 11). Rows are immutable: the runtime may only insert and read them.
CREATE TABLE supervisor.document_uploads (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, upload_id uuid NOT NULL,
 uploader_issuer text NOT NULL CHECK(length(uploader_issuer) BETWEEN 1 AND 2048),
 uploader_subject text NOT NULL CHECK(length(uploader_subject) BETWEEN 1 AND 255),
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 255),
 title text NOT NULL CHECK(length(title) BETWEEN 1 AND 200),
 original_sha256 text NOT NULL CHECK(original_sha256 ~ '^[0-9a-f]{64}$'),
 typed_source_id uuid NOT NULL,
 page_count integer NOT NULL CHECK(page_count BETWEEN 2 AND 1000),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,upload_id),
 UNIQUE(tenant_id,site_id,uploader_issuer,uploader_subject,idempotency_key),
 UNIQUE(tenant_id,site_id,typed_source_id),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 FOREIGN KEY(tenant_id,site_id,typed_source_id) REFERENCES supervisor.evidence_heads(tenant_id,site_id,source_id)
);

-- A scanned page of a mixed upload names its upload; a scan-only PDF or a photo has none. The
-- link is set when the job is inserted and never changes.
ALTER TABLE supervisor.document_scan_jobs ADD COLUMN upload_id uuid,
 ADD FOREIGN KEY(tenant_id,site_id,upload_id) REFERENCES supervisor.document_uploads(tenant_id,site_id,upload_id);
CREATE INDEX document_scan_jobs_upload ON supervisor.document_scan_jobs(tenant_id,site_id,upload_id) WHERE upload_id IS NOT NULL;
-- Forced only after the foreign key above is validated: that check reads both tables in one query,
-- which forced RLS refuses (see migration 042).
ALTER TABLE supervisor.document_uploads ENABLE ROW LEVEL SECURITY;
ALTER TABLE supervisor.document_uploads FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON supervisor.document_uploads USING(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid) WITH CHECK(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid);
REVOKE ALL ON supervisor.document_uploads FROM PUBLIC,supervisor_runtime;
GRANT SELECT,INSERT ON supervisor.document_uploads TO supervisor_runtime;

-- The guard is 078's (WIS-625: presented_sha256 is immutable) with upload_id also immutable: both
-- columns stay in the row it compares, so this migration keeps 078's rule.
CREATE OR REPLACE FUNCTION supervisor.document_scan_job_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'scan jobs are retained' USING ERRCODE='42501'; END IF;
 IF OLD.state<>'running' OR NEW.state='running' OR NEW.image IS NOT NULL
    OR ROW(NEW.tenant_id,NEW.site_id,NEW.job_id,NEW.uploader_issuer,NEW.uploader_subject,NEW.idempotency_key,NEW.title,NEW.original_sha256,NEW.media_type,NEW.width,NEW.height,NEW.run_id,NEW.created_at,NEW.presented_sha256,NEW.upload_id)
       IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.job_id,OLD.uploader_issuer,OLD.uploader_subject,OLD.idempotency_key,OLD.title,OLD.original_sha256,OLD.media_type,OLD.width,OLD.height,OLD.run_id,OLD.created_at,OLD.presented_sha256,OLD.upload_id) THEN
  RAISE EXCEPTION 'invalid scan job transition' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
