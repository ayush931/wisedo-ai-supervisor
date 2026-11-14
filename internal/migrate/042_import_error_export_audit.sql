-- WIS-272 import error export: every errors.csv download leaves one audit row naming the actor,
-- the job and the number of exported rows (never the row content). The existing operational
-- source audit gains an additive event type and two nullable columns; existing rows keep NULLs
-- and every original CHECK branch is re-added unchanged, validating every existing row.
ALTER TABLE supervisor.operational_source_audit
 ADD COLUMN job_id uuid,
 ADD COLUMN exported_rows integer CHECK(exported_rows IS NULL OR exported_rows BETWEEN 0 AND 50000),
 ADD CONSTRAINT operational_source_audit_job_fkey FOREIGN KEY(tenant_id,site_id,job_id)
  REFERENCES supervisor.operational_import_jobs(tenant_id,site_id,id),
 ADD CONSTRAINT operational_source_audit_export_check CHECK(
  (event_type='import.errors_exported')=(job_id IS NOT NULL) AND (job_id IS NULL)=(exported_rows IS NULL)),
 DROP CONSTRAINT operational_source_audit_event_type_check,
 ADD CONSTRAINT operational_source_audit_event_type_check CHECK(event_type IN ('source.created','source.revised','source.read','source.revoked','mapping.revised','mapping.read','review.decided','review.read','mutation.replayed','import.errors_exported')),
 DROP CONSTRAINT operational_source_audit_check1,
 ADD CONSTRAINT operational_source_audit_check1 CHECK(
  (event_type IN ('source.created','source.revised','source.read','source.revoked') AND source_revision=revision AND mapping_revision IS NULL AND review_revision IS NULL) OR
  (event_type IN ('mapping.revised','mapping.read') AND source_revision IS NOT NULL AND mapping_revision=revision AND review_revision IS NULL) OR
  (event_type IN ('review.decided','review.read') AND source_revision IS NOT NULL AND mapping_revision IS NULL AND review_revision=revision) OR
  (event_type='mutation.replayed' AND operation IN ('source','mapping') AND source_revision=revision AND mapping_revision IS NULL AND review_revision IS NULL) OR
  (event_type='mutation.replayed' AND operation='review' AND source_revision IS NULL AND mapping_revision IS NULL AND review_revision=revision) OR
  (event_type='import.errors_exported' AND source_revision=revision AND mapping_revision IS NULL AND review_revision IS NULL));
