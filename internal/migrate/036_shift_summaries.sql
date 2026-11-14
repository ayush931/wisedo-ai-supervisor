-- WIS-520 shift summaries: immutable snapshots of the deterministic ShiftSummaryV1 computed
-- over a shift plan's approved revision in force and the station records bound to the plan.
-- A snapshot stores the computed body and its canonical digest; revisions per plan are
-- contiguous and append-only, so later data adds a revision and never rewrites one.
CREATE TABLE supervisor.shift_summaries (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,plan_id uuid NOT NULL,revision bigint NOT NULL CHECK(revision>0),
 -- The approved revision whose targets the summary reports; an unapproved one is unrepresentable.
 plan_revision bigint NOT NULL,
 payload jsonb NOT NULL,
 canonical_payload text NOT NULL CHECK(octet_length(canonical_payload) BETWEEN 1 AND 8388608),
 canonical_payload_sha256 text NOT NULL CHECK(canonical_payload_sha256=encode(sha256(convert_to(canonical_payload,'UTF8')),'hex')),
 digest_schema_version integer NOT NULL CHECK(digest_schema_version=1),
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 generated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK(canonical_payload::jsonb->>'schema'='ShiftSummaryV1' AND (canonical_payload::jsonb->>'schema_version')::integer=digest_schema_version),
 CHECK(canonical_payload::jsonb->>'tenant_id'=tenant_id::text AND canonical_payload::jsonb->>'site_id'=site_id::text),
 CHECK(canonical_payload::jsonb->'body'=payload AND payload->>'plan_id'=plan_id::text AND (payload->>'plan_revision')::bigint=plan_revision),
 PRIMARY KEY(tenant_id,site_id,plan_id,revision),
 FOREIGN KEY(tenant_id,site_id,plan_id,plan_revision) REFERENCES supervisor.shift_plan_approvals(tenant_id,site_id,plan_id,revision)
);
CREATE TABLE supervisor.shift_summary_receipts (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,actor_issuer text NOT NULL,actor_subject text NOT NULL,
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 plan_id uuid NOT NULL,revision bigint NOT NULL,
 PRIMARY KEY(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id,plan_id,revision) REFERENCES supervisor.shift_summaries(tenant_id,site_id,plan_id,revision)
);
CREATE TABLE supervisor.shift_summary_audit (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),tenant_id uuid NOT NULL,site_id uuid NOT NULL,
 actor_issuer text NOT NULL,actor_subject text NOT NULL,plan_id uuid NOT NULL,revision bigint,
 event_type text NOT NULL CHECK(event_type IN ('shift_summary.generated','shift_summary.read','shift_summary.listed','mutation.replayed')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((event_type IN ('shift_summary.generated','shift_summary.read'))<=(revision IS NOT NULL)),
 FOREIGN KEY(tenant_id,site_id,plan_id) REFERENCES supervisor.shift_plan_heads(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,plan_id,revision) REFERENCES supervisor.shift_summaries(tenant_id,site_id,plan_id,revision)
);

-- Snapshot revisions are contiguous per plan: no gaps, no back-fill.
CREATE FUNCTION supervisor.shift_summary_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Shift summary tenant scope denied' USING ERRCODE='42501'; END IF;
 IF NEW.revision<>coalesce((SELECT max(revision) FROM supervisor.shift_summaries WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND plan_id=NEW.plan_id),0)+1 THEN
  RAISE EXCEPTION 'Shift summary revision is not the next revision' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER shift_summary_guard BEFORE INSERT ON supervisor.shift_summaries
 FOR EACH ROW EXECUTE FUNCTION supervisor.shift_summary_guard();

DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['shift_summaries','shift_summary_receipts','shift_summary_audit'] LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t||'_immutable',t);
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP;
END $$;
