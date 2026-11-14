-- Shift runs: one run per shift plan. A supervisor or owner starts the plan's approved
-- revision and later ends it; ending cites the final summary snapshot and the handover draft
-- written in the same transaction. No row means the plan is planned (not started). The only
-- transition is started -> ended, and the start columns never change.
CREATE TABLE supervisor.shift_runs (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,plan_id uuid NOT NULL,
 started_plan_revision bigint NOT NULL,
 started_by_issuer text NOT NULL CHECK(length(started_by_issuer) BETWEEN 1 AND 255),
 started_by_subject text NOT NULL CHECK(length(started_by_subject) BETWEEN 1 AND 255),
 start_auth_time timestamptz NOT NULL,
 started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 ended_plan_revision bigint,
 ended_by_issuer text CHECK(length(ended_by_issuer) BETWEEN 1 AND 255),
 ended_by_subject text CHECK(length(ended_by_subject) BETWEEN 1 AND 255),
 end_auth_time timestamptz,
 ended_at timestamptz,
 summary_revision bigint,
 summary_sha256 text CHECK(summary_sha256 ~ '^[0-9a-f]{64}$'),
 handover_revision bigint,
 -- Every end column is set together, or none is.
 CHECK((ended_at IS NULL)=(ended_plan_revision IS NULL) AND (ended_at IS NULL)=(ended_by_issuer IS NULL)
  AND (ended_at IS NULL)=(ended_by_subject IS NULL) AND (ended_at IS NULL)=(end_auth_time IS NULL)
  AND (ended_at IS NULL)=(summary_revision IS NULL) AND (ended_at IS NULL)=(summary_sha256 IS NULL)
  AND (ended_at IS NULL)=(handover_revision IS NULL)),
 -- The run is the half-open interval [started_at, ended_at).
 CHECK(ended_at>started_at),
 PRIMARY KEY(tenant_id,site_id,plan_id),
 FOREIGN KEY(tenant_id,site_id,plan_id) REFERENCES supervisor.shift_plan_heads(tenant_id,site_id,id),
 -- A start or end bound to an unapproved revision is unrepresentable.
 FOREIGN KEY(tenant_id,site_id,plan_id,started_plan_revision) REFERENCES supervisor.shift_plan_approvals(tenant_id,site_id,plan_id,revision),
 FOREIGN KEY(tenant_id,site_id,plan_id,ended_plan_revision) REFERENCES supervisor.shift_plan_approvals(tenant_id,site_id,plan_id,revision),
 FOREIGN KEY(tenant_id,site_id,plan_id,summary_revision) REFERENCES supervisor.shift_summaries(tenant_id,site_id,plan_id,revision),
 FOREIGN KEY(tenant_id,site_id,plan_id,handover_revision) REFERENCES supervisor.shift_handovers(tenant_id,site_id,plan_id,revision)
);
CREATE TABLE supervisor.shift_run_receipts (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,actor_issuer text NOT NULL,actor_subject text NOT NULL,
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 operation text NOT NULL CHECK(operation IN ('start','end')),
 plan_id uuid NOT NULL,
 -- The original response, replayed verbatim for the same key and request (rule 07).
 response jsonb NOT NULL CHECK(jsonb_typeof(response)='object' AND response->>'plan_id'=plan_id::text),
 PRIMARY KEY(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id,plan_id) REFERENCES supervisor.shift_runs(tenant_id,site_id,plan_id)
);
CREATE TABLE supervisor.shift_run_audit (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),tenant_id uuid NOT NULL,site_id uuid NOT NULL,
 actor_issuer text NOT NULL,actor_subject text NOT NULL,plan_id uuid NOT NULL,
 event_type text NOT NULL CHECK(event_type IN ('shift_run.started','shift_run.ended','shift_run.read','mutation.replayed')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY(tenant_id,site_id,plan_id) REFERENCES supervisor.shift_plan_heads(tenant_id,site_id,id)
);
-- A run read is audited at most once a minute per reader and plan; this finds the latest one.
CREATE INDEX shift_run_audit_reads ON supervisor.shift_run_audit(tenant_id,site_id,plan_id,actor_issuer,actor_subject,event_type,created_at);

-- A run starts on the plan's approved revision with no end columns. It is ended once, on the
-- approved revision in force then, citing a summary of this plan by its exact digest and a
-- handover drafted from that summary. Start columns never change and runs are never deleted.
CREATE FUNCTION supervisor.shift_run_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE approved bigint;
BEGIN
 IF TG_OP='DELETE' THEN
  RAISE EXCEPTION 'Shift runs are never deleted' USING ERRCODE='42501'; END IF;
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Shift run tenant scope denied' USING ERRCODE='42501'; END IF;
 SELECT approved_revision INTO approved FROM supervisor.shift_plan_heads WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND id=NEW.plan_id;
 IF TG_OP='INSERT' THEN
  IF NEW.ended_at IS NOT NULL OR NEW.ended_plan_revision IS NOT NULL OR NEW.summary_revision IS NOT NULL OR NEW.handover_revision IS NOT NULL
   OR NEW.started_plan_revision IS DISTINCT FROM approved THEN
   RAISE EXCEPTION 'Shift run must start on the approved revision' USING ERRCODE='23514'; END IF;
  RETURN NEW;
 END IF;
 IF OLD.ended_at IS NOT NULL OR NEW.ended_at IS NULL
  OR ROW(NEW.tenant_id,NEW.site_id,NEW.plan_id,NEW.started_plan_revision,NEW.started_by_issuer,NEW.started_by_subject,NEW.start_auth_time,NEW.started_at)
   IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.plan_id,OLD.started_plan_revision,OLD.started_by_issuer,OLD.started_by_subject,OLD.start_auth_time,OLD.started_at)
  OR NEW.ended_plan_revision IS DISTINCT FROM approved
  OR NOT EXISTS(SELECT 1 FROM supervisor.shift_summaries s WHERE (s.tenant_id,s.site_id,s.plan_id,s.revision)=(NEW.tenant_id,NEW.site_id,NEW.plan_id,NEW.summary_revision) AND s.canonical_payload_sha256=NEW.summary_sha256)
  OR NOT EXISTS(SELECT 1 FROM supervisor.shift_handovers h WHERE (h.tenant_id,h.site_id,h.plan_id,h.revision)=(NEW.tenant_id,NEW.site_id,NEW.plan_id,NEW.handover_revision) AND h.summary_revision=NEW.summary_revision) THEN
  RAISE EXCEPTION 'Invalid shift run transition' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER shift_run_guard BEFORE INSERT OR UPDATE OR DELETE ON supervisor.shift_runs
 FOR EACH ROW EXECUTE FUNCTION supervisor.shift_run_guard();

DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['shift_run_receipts','shift_run_audit'] LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t||'_immutable',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['shift_runs','shift_run_receipts','shift_run_audit'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP;
 GRANT UPDATE(ended_plan_revision,ended_by_issuer,ended_by_subject,end_auth_time,ended_at,summary_revision,summary_sha256,handover_revision) ON supervisor.shift_runs TO supervisor_runtime;
END $$;
