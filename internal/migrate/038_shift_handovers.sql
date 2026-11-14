-- WIS-520 next-shift handovers: immutable drafts of the deterministic ShiftHandoverV1 computed
-- from one shift summary snapshot (cited by revision and digest) and the job orders' current
-- head revisions. Generating a draft writes only these tables; no shift plan changes. A
-- separate, explicit adoption turns the draft's proposed plan into a new draft shift plan
-- revision submitted by the adopter, which still needs the usual independent approval.
CREATE TABLE supervisor.shift_handovers (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,plan_id uuid NOT NULL,revision bigint NOT NULL CHECK(revision>0),
 summary_revision bigint NOT NULL,
 payload jsonb NOT NULL,
 canonical_payload text NOT NULL CHECK(octet_length(canonical_payload) BETWEEN 1 AND 8388608),
 canonical_payload_sha256 text NOT NULL CHECK(canonical_payload_sha256=encode(sha256(convert_to(canonical_payload,'UTF8')),'hex')),
 digest_schema_version integer NOT NULL CHECK(digest_schema_version=1),
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 generated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK(canonical_payload::jsonb->>'schema'='ShiftHandoverV1' AND (canonical_payload::jsonb->>'schema_version')::integer=digest_schema_version),
 CHECK(canonical_payload::jsonb->>'tenant_id'=tenant_id::text AND canonical_payload::jsonb->>'site_id'=site_id::text),
 CHECK(canonical_payload::jsonb->'body'=payload AND payload->>'plan_id'=plan_id::text AND (payload->>'summary_revision')::bigint=summary_revision),
 PRIMARY KEY(tenant_id,site_id,plan_id,revision),
 -- The cited summary is a stored snapshot of the same plan.
 FOREIGN KEY(tenant_id,site_id,plan_id,summary_revision) REFERENCES supervisor.shift_summaries(tenant_id,site_id,plan_id,revision)
);
-- At most one adoption per draft; the adopted plan revision is a stored shift plan revision.
CREATE TABLE supervisor.shift_handover_adoptions (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,plan_id uuid NOT NULL,revision bigint NOT NULL,
 adopted_plan_id uuid NOT NULL,adopted_revision bigint NOT NULL,
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 adopted_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,plan_id,revision),
 FOREIGN KEY(tenant_id,site_id,plan_id,revision) REFERENCES supervisor.shift_handovers(tenant_id,site_id,plan_id,revision),
 FOREIGN KEY(tenant_id,site_id,adopted_plan_id,adopted_revision) REFERENCES supervisor.shift_plan_revisions(tenant_id,site_id,plan_id,revision)
);
CREATE TABLE supervisor.shift_handover_receipts (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,actor_issuer text NOT NULL,actor_subject text NOT NULL,
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 operation text NOT NULL CHECK(operation IN ('generate','adopt')),
 plan_id uuid NOT NULL,revision bigint NOT NULL,
 PRIMARY KEY(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id,plan_id,revision) REFERENCES supervisor.shift_handovers(tenant_id,site_id,plan_id,revision)
);
CREATE TABLE supervisor.shift_handover_audit (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),tenant_id uuid NOT NULL,site_id uuid NOT NULL,
 actor_issuer text NOT NULL,actor_subject text NOT NULL,plan_id uuid NOT NULL,revision bigint,adopted_plan_id uuid,
 event_type text NOT NULL CHECK(event_type IN ('shift_handover.generated','shift_handover.read','shift_handover.listed','shift_handover.adopted','mutation.replayed')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((event_type IN ('shift_handover.generated','shift_handover.read','shift_handover.adopted'))<=(revision IS NOT NULL)),
 CHECK((event_type='shift_handover.adopted')=(adopted_plan_id IS NOT NULL)),
 FOREIGN KEY(tenant_id,site_id,plan_id) REFERENCES supervisor.shift_plan_heads(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,plan_id,revision) REFERENCES supervisor.shift_handovers(tenant_id,site_id,plan_id,revision),
 FOREIGN KEY(tenant_id,site_id,adopted_plan_id) REFERENCES supervisor.shift_plan_heads(tenant_id,site_id,id)
);

-- Draft revisions are contiguous per plan: no gaps, no back-fill.
CREATE FUNCTION supervisor.shift_handover_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Shift handover tenant scope denied' USING ERRCODE='42501'; END IF;
 IF NEW.revision<>coalesce((SELECT max(revision) FROM supervisor.shift_handovers WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND plan_id=NEW.plan_id),0)+1 THEN
  RAISE EXCEPTION 'Shift handover revision is not the next revision' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER shift_handover_guard BEFORE INSERT ON supervisor.shift_handovers
 FOR EACH ROW EXECUTE FUNCTION supervisor.shift_handover_guard();

-- An adoption names the plan's latest draft and a plan revision the adopter submitted whose
-- body is exactly that draft's proposed plan, so the adopter is a submitter (and cannot
-- approve it), a superseded draft is never adopted, and nothing else is slipped in under the
-- draft's name.
CREATE FUNCTION supervisor.shift_handover_adoption_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Shift handover adoption tenant scope denied' USING ERRCODE='42501'; END IF;
 IF NEW.revision IS DISTINCT FROM (SELECT max(revision) FROM supervisor.shift_handovers WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND plan_id=NEW.plan_id) THEN
  RAISE EXCEPTION 'Shift handover adoption names a superseded draft' USING ERRCODE='23514'; END IF;
 IF NOT EXISTS(SELECT 1 FROM supervisor.shift_plan_revisions r JOIN supervisor.shift_handovers h ON (h.tenant_id,h.site_id,h.plan_id,h.revision)=(NEW.tenant_id,NEW.site_id,NEW.plan_id,NEW.revision)
  WHERE (r.tenant_id,r.site_id,r.plan_id,r.revision)=(NEW.tenant_id,NEW.site_id,NEW.adopted_plan_id,NEW.adopted_revision)
  AND r.actor_issuer=NEW.actor_issuer AND r.actor_subject=NEW.actor_subject AND jsonb_typeof(h.payload->'proposed_plan')='object' AND r.payload=h.payload->'proposed_plan') THEN
  RAISE EXCEPTION 'Shift handover adoption does not match its proposed plan' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER shift_handover_adoption_guard BEFORE INSERT ON supervisor.shift_handover_adoptions
 FOR EACH ROW EXECUTE FUNCTION supervisor.shift_handover_adoption_guard();

DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['shift_handovers','shift_handover_adoptions','shift_handover_receipts','shift_handover_audit'] LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t||'_immutable',t);
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP;
END $$;
