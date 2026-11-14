-- WIS-272 CSV/JSON importer: one durable job per import request, with per-row outcomes.
-- A job is written as 'running' before its first chunk, so a retry with the same key resumes
-- at the same starting sequence. Chunks are persisted through the existing atomic batch intake.
CREATE TABLE supervisor.operational_import_jobs (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL,
 source_id uuid NOT NULL,
 format text NOT NULL CHECK(format IN ('csv','json')),
 file_sha256 text NOT NULL CHECK(file_sha256 ~ '^[0-9a-f]{64}$'),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 source_revision bigint NOT NULL CHECK(source_revision>0),
 mapping_revision bigint NOT NULL CHECK(mapping_revision>0),
 start_sequence bigint NOT NULL CHECK(start_sequence>0),
 row_count integer NOT NULL CHECK(row_count BETWEEN 1 AND 50000),
 status text NOT NULL CHECK(status IN ('running','completed','partial')),
 accepted_rows integer NOT NULL DEFAULT 0 CHECK(accepted_rows>=0),
 duplicate_rows integer NOT NULL DEFAULT 0 CHECK(duplicate_rows>=0),
 quarantined_rows integer NOT NULL DEFAULT 0 CHECK(quarantined_rows>=0),
 rejected_rows integer NOT NULL DEFAULT 0 CHECK(rejected_rows>=0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 finished_at timestamptz,
 CHECK(accepted_rows+duplicate_rows+quarantined_rows+rejected_rows<=row_count),
 CHECK((status='running')=(finished_at IS NULL)),
 PRIMARY KEY(tenant_id,site_id,id),
 UNIQUE(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 FOREIGN KEY(tenant_id,site_id,source_id) REFERENCES supervisor.operational_source_heads(tenant_id,site_id,source_id)
);
-- One running import per source: it owns the source stream until it finishes.
CREATE UNIQUE INDEX operational_import_one_running ON supervisor.operational_import_jobs(tenant_id,site_id,source_id)
 WHERE status='running';
CREATE TABLE supervisor.operational_import_rows (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,job_id uuid NOT NULL,
 row_index integer NOT NULL CHECK(row_index>=1),
 status text NOT NULL CHECK(status IN ('accepted','duplicate','quarantined','rejected')),
 reason text CHECK(reason IS NULL OR length(reason) BETWEEN 1 AND 128),
 source_sequence bigint CHECK(source_sequence IS NULL OR source_sequence>0),
 PRIMARY KEY(tenant_id,site_id,job_id,row_index),
 FOREIGN KEY(tenant_id,site_id,job_id) REFERENCES supervisor.operational_import_jobs(tenant_id,site_id,id)
);
-- A job's identity is fixed. Only the running->finished transition may change it.
CREATE FUNCTION supervisor.operational_import_job_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'Import jobs are immutable' USING ERRCODE='42501'; END IF;
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Import job tenant scope denied' USING ERRCODE='42501'; END IF;
 IF TG_OP='UPDATE' THEN
  IF OLD.status<>'running' OR ROW(NEW.tenant_id,NEW.site_id,NEW.id,NEW.source_id,NEW.format,NEW.file_sha256,NEW.request_sha256,NEW.idempotency_key,NEW.actor_issuer,NEW.actor_subject,NEW.source_revision,NEW.mapping_revision,NEW.start_sequence,NEW.row_count,NEW.created_at)
     IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.id,OLD.source_id,OLD.format,OLD.file_sha256,OLD.request_sha256,OLD.idempotency_key,OLD.actor_issuer,OLD.actor_subject,OLD.source_revision,OLD.mapping_revision,OLD.start_sequence,OLD.row_count,OLD.created_at)
  THEN RAISE EXCEPTION 'Import job identity is immutable' USING ERRCODE='42501'; END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER operational_import_job_guard BEFORE INSERT OR UPDATE OR DELETE ON supervisor.operational_import_jobs
 FOR EACH ROW EXECUTE FUNCTION supervisor.operational_import_job_guard();
CREATE TRIGGER operational_import_row_immutable BEFORE UPDATE OR DELETE ON supervisor.operational_import_rows
 FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['operational_import_jobs','operational_import_rows'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
 END LOOP;
 GRANT SELECT,INSERT,UPDATE(status,accepted_rows,duplicate_rows,quarantined_rows,rejected_rows,finished_at) ON supervisor.operational_import_jobs TO supervisor_runtime;
 GRANT SELECT,INSERT ON supervisor.operational_import_rows TO supervisor_runtime;
END $$;
