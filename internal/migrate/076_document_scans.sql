-- WIS-625 slice B1: a photo of a paper document (PNG or JPEG) is read by a pinned local vision
-- model in a bounded agent run, never in the upload request. The upload stores a scan job that
-- holds the image until the run ends; the image is cleared on every terminal state. A completed
-- job is a `document` evidence source whose revision records that its text was model-extracted,
-- by which model, revision, prompt and run. Nothing is substituted for a page the model could not
-- read: the job is quarantined with a stable reason.

-- An extraction run reads one scan job. It carries the job as conversation_id and request_id,
-- never a case, and its budget is reserved per page like every other run.
DO $$ DECLARE c text; BEGIN
 FOR c IN SELECT conname FROM pg_constraint WHERE conrelid='supervisor.agent_runs'::regclass AND contype='c' AND pg_get_constraintdef(oid) LIKE '%request_kind%' LOOP
  EXECUTE format('ALTER TABLE supervisor.agent_runs DROP CONSTRAINT %I',c);
 END LOOP;
END $$;
ALTER TABLE supervisor.agent_runs ADD CONSTRAINT agent_runs_request_kind_check CHECK(request_kind IN ('case','conversation','extraction'));
ALTER TABLE supervisor.agent_runs ADD CONSTRAINT agent_runs_request_shape_check CHECK(
 (request_kind='case' AND case_id IS NOT NULL AND case_revision IS NOT NULL AND conversation_id IS NULL AND request_id IS NULL)
 OR (request_kind IN ('conversation','extraction') AND case_id IS NULL AND case_revision IS NULL AND conversation_id IS NOT NULL AND request_id IS NOT NULL));
CREATE UNIQUE INDEX agent_runs_one_per_scan_job ON supervisor.agent_runs(tenant_id,site_id,conversation_id) WHERE request_kind='extraction';

CREATE TABLE supervisor.document_scan_jobs (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, job_id uuid NOT NULL,
 uploader_issuer text NOT NULL CHECK(length(uploader_issuer) BETWEEN 1 AND 2048),
 uploader_subject text NOT NULL CHECK(length(uploader_subject) BETWEEN 1 AND 255),
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 255),
 title text NOT NULL CHECK(length(title) BETWEEN 1 AND 200),
 original_sha256 text NOT NULL CHECK(original_sha256 ~ '^[0-9a-f]{64}$'),
 media_type text NOT NULL CHECK(media_type IN ('image/png','image/jpeg')),
 width integer NOT NULL CHECK(width BETWEEN 1 AND 20000), height integer NOT NULL CHECK(height BETWEEN 1 AND 20000),
 image bytea CHECK(image IS NULL OR octet_length(image) BETWEEN 1 AND 4194304),
 state text NOT NULL DEFAULT 'running' CHECK(state IN ('running','completed','quarantined')),
 quarantine_reason text CHECK(quarantine_reason IN ('PAGE_ILLEGIBLE','PAGE_TRUNCATED','OUTPUT_INVALID','TOOL_NOT_ALLOWED','EXTRACTION_FAILED','UPLOADER_NOT_AUTHORIZED','TEXT_TOO_LARGE','BUDGET_EXHAUSTED','RUN_CANCELED')),
 run_id uuid NOT NULL, source_id uuid,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), finished_at timestamptz,
 PRIMARY KEY(tenant_id,site_id,job_id),
 UNIQUE(tenant_id,site_id,uploader_issuer,uploader_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 FOREIGN KEY(tenant_id,site_id,run_id) REFERENCES supervisor.agent_runs(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,source_id) REFERENCES supervisor.evidence_heads(tenant_id,site_id,source_id),
 -- The image lives only while the job runs; a finished job keeps its digest, never its bytes.
 CHECK((state='running')=(image IS NOT NULL)),
 CHECK((state='running')=(finished_at IS NULL)),
 CHECK((state='completed')=(source_id IS NOT NULL)),
 CHECK((state='quarantined')=(quarantine_reason IS NOT NULL))
);
-- A job only moves from running to one terminal state, clearing its image; nothing else changes.
CREATE FUNCTION supervisor.document_scan_job_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'scan jobs are retained' USING ERRCODE='42501'; END IF;
 IF OLD.state<>'running' OR NEW.state='running' OR NEW.image IS NOT NULL
    OR ROW(NEW.tenant_id,NEW.site_id,NEW.job_id,NEW.uploader_issuer,NEW.uploader_subject,NEW.idempotency_key,NEW.title,NEW.original_sha256,NEW.media_type,NEW.width,NEW.height,NEW.run_id,NEW.created_at)
       IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.job_id,OLD.uploader_issuer,OLD.uploader_subject,OLD.idempotency_key,OLD.title,OLD.original_sha256,OLD.media_type,OLD.width,OLD.height,OLD.run_id,OLD.created_at) THEN
  RAISE EXCEPTION 'invalid scan job transition' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER document_scan_job_guard BEFORE UPDATE OR DELETE ON supervisor.document_scan_jobs FOR EACH ROW EXECUTE FUNCTION supervisor.document_scan_job_guard();

CREATE TABLE supervisor.document_scan_outbox (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, id uuid NOT NULL, job_id uuid NOT NULL, run_id uuid NOT NULL,
 delivered_at timestamptz,
 PRIMARY KEY(tenant_id,site_id,id), UNIQUE(tenant_id,site_id,job_id),
 FOREIGN KEY(tenant_id,site_id,job_id) REFERENCES supervisor.document_scan_jobs(tenant_id,site_id,job_id)
);
CREATE FUNCTION supervisor.document_scan_outbox_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'scan outbox is retained' USING ERRCODE='42501'; END IF;
 IF OLD.delivered_at IS NOT NULL OR NEW.delivered_at IS NULL
    OR ROW(NEW.tenant_id,NEW.site_id,NEW.id,NEW.job_id,NEW.run_id) IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.id,OLD.job_id,OLD.run_id) THEN
  RAISE EXCEPTION 'invalid scan delivery update' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER document_scan_outbox_guard BEFORE UPDATE OR DELETE ON supervisor.document_scan_outbox FOR EACH ROW EXECUTE FUNCTION supervisor.document_scan_outbox_guard();

DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['document_scan_jobs','document_scan_outbox'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP;
END $$;
GRANT UPDATE(image,state,quarantine_reason,source_id,finished_at) ON supervisor.document_scan_jobs TO supervisor_runtime;
GRANT UPDATE(delivered_at) ON supervisor.document_scan_outbox TO supervisor_runtime;

-- A revision whose text a model extracted says so, immutably, with what extracted it. A typed or
-- born-digital revision has none of these.
ALTER TABLE supervisor.evidence_revisions
 ADD COLUMN extraction text CHECK(extraction='model_ocr'),
 ADD COLUMN extractor_model text CHECK(length(extractor_model) BETWEEN 1 AND 255),
 ADD COLUMN extractor_model_revision text CHECK(length(extractor_model_revision) BETWEEN 1 AND 255),
 ADD COLUMN extractor_prompt_sha256 text CHECK(extractor_prompt_sha256 ~ '^[0-9a-f]{64}$'),
 ADD COLUMN extraction_run_id uuid,
 ADD CHECK((extraction IS NULL)=(extractor_model IS NULL) AND (extraction IS NULL)=(extractor_model_revision IS NULL)
  AND (extraction IS NULL)=(extractor_prompt_sha256 IS NULL) AND (extraction IS NULL)=(extraction_run_id IS NULL)),
 ADD CHECK(extraction IS NULL OR kind='document'),
 ADD FOREIGN KEY(tenant_id,site_id,extraction_run_id) REFERENCES supervisor.agent_runs(tenant_id,site_id,id);
ALTER TABLE supervisor.evidence_audit DROP CONSTRAINT evidence_audit_event_type_check;
ALTER TABLE supervisor.evidence_audit ADD CONSTRAINT evidence_audit_event_type_check CHECK(event_type IN ('source.created','source.revised','source.read','source.listed','source.revoked','span.created','span.read','mutation.replayed','authority.decided','authority.read','currentness.decided','currentness.read','scan.queued','scan.read','scan.completed','scan.quarantined'));
