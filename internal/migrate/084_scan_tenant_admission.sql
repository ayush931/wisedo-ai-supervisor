-- WIS-715: the per-tenant scan cap counts a tenant's running scan pages created within the
-- admission window on every new upload.
CREATE INDEX document_scan_jobs_running ON supervisor.document_scan_jobs(tenant_id,created_at) WHERE state='running';
