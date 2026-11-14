-- WIS-520 digital job orders delivered to workstations. A job order is appended as immutable
-- JobOrderBodyV1 revisions with a canonical payload digest; its instruction is pinned to an
-- exact document evidence revision and hash. Job orders carry no approval of their own: a
-- revision reaches a workstation only through an approved shift plan that pins it, and the
-- current-order read refuses a pin that is no longer the head revision.
CREATE TABLE supervisor.job_order_heads (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL,
 current_revision bigint NOT NULL CHECK(current_revision>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id)
);
CREATE TABLE supervisor.job_order_revisions (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,job_order_id uuid NOT NULL,revision bigint NOT NULL CHECK(revision>0),
 payload jsonb NOT NULL,
 canonical_payload text NOT NULL CHECK(octet_length(canonical_payload) BETWEEN 1 AND 1048576),
 canonical_payload_sha256 text NOT NULL CHECK(canonical_payload_sha256=encode(sha256(convert_to(canonical_payload,'UTF8')),'hex')),
 digest_schema_version integer NOT NULL CHECK(digest_schema_version=1),
 instruction_source_id uuid NOT NULL,instruction_revision bigint NOT NULL,instruction_sha256 text NOT NULL,
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 -- The digest body is the stored payload, bound to this schema and scope; the instruction
 -- columns that carry the evidence foreign key are the payload's own pin.
 CHECK(canonical_payload::jsonb->>'schema'='JobOrderBodyV1' AND (canonical_payload::jsonb->>'schema_version')::integer=digest_schema_version),
 CHECK(canonical_payload::jsonb->>'tenant_id'=tenant_id::text AND canonical_payload::jsonb->>'site_id'=site_id::text),
 CHECK(canonical_payload::jsonb->'body'=payload AND (payload->>'schema_version')::integer=1),
 CHECK(payload->>'instruction_document_id'=instruction_source_id::text AND (payload->>'instruction_revision')::bigint=instruction_revision AND payload->>'instruction_sha256'=instruction_sha256),
 PRIMARY KEY(tenant_id,site_id,job_order_id,revision),
 FOREIGN KEY(tenant_id,site_id,job_order_id) REFERENCES supervisor.job_order_heads(tenant_id,site_id,id),
 -- The instruction is an exact stored evidence revision with the stated content hash.
 CONSTRAINT job_order_instruction_fk FOREIGN KEY(tenant_id,site_id,instruction_source_id,instruction_revision,instruction_sha256)
  REFERENCES supervisor.evidence_revisions(tenant_id,site_id,source_id,revision,sha256)
);
ALTER TABLE supervisor.job_order_heads ADD CONSTRAINT job_order_current_fk FOREIGN KEY(tenant_id,site_id,id,current_revision)
 REFERENCES supervisor.job_order_revisions(tenant_id,site_id,job_order_id,revision) DEFERRABLE INITIALLY DEFERRED;
CREATE TABLE supervisor.job_order_receipts (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,actor_issuer text NOT NULL,actor_subject text NOT NULL,
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 operation text NOT NULL CHECK(operation IN ('create','revise')),
 job_order_id uuid NOT NULL,revision bigint NOT NULL,
 PRIMARY KEY(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id,job_order_id,revision) REFERENCES supervisor.job_order_revisions(tenant_id,site_id,job_order_id,revision)
);
CREATE TABLE supervisor.job_order_audit (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),tenant_id uuid NOT NULL,site_id uuid NOT NULL,
 actor_issuer text NOT NULL,actor_subject text NOT NULL,job_order_id uuid,revision bigint,
 event_type text NOT NULL CHECK(event_type IN ('job_order.created','job_order.revised','job_order.read','job_order.listed','job_order.delivered','delivery.needs_replan','delivery.instruction_unavailable','mutation.replayed')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((event_type='job_order.listed')=(job_order_id IS NULL)),
 CHECK((event_type IN ('job_order.created','job_order.revised','job_order.delivered','delivery.needs_replan','delivery.instruction_unavailable'))<=(revision IS NOT NULL)),
 FOREIGN KEY(tenant_id,site_id,job_order_id) REFERENCES supervisor.job_order_heads(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,job_order_id,revision) REFERENCES supervisor.job_order_revisions(tenant_id,site_id,job_order_id,revision)
);
-- One row per workstation of a shift plan revision, derived from its payload by trigger
-- only. The runtime can read but never write these rows, so a delivery query can trust that
-- they equal the immutable revision they reference.
CREATE TABLE supervisor.shift_plan_assignments (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,plan_id uuid NOT NULL,revision bigint NOT NULL,
 workstation_id uuid NOT NULL,job_order_id uuid NOT NULL,job_order_revision bigint NOT NULL,
 PRIMARY KEY(tenant_id,site_id,plan_id,revision,workstation_id),
 UNIQUE(tenant_id,site_id,plan_id,revision,job_order_id),
 FOREIGN KEY(tenant_id,site_id,plan_id,revision) REFERENCES supervisor.shift_plan_revisions(tenant_id,site_id,plan_id,revision),
 CONSTRAINT shift_plan_assignment_job_order_fk FOREIGN KEY(tenant_id,site_id,job_order_id,job_order_revision) REFERENCES supervisor.job_order_revisions(tenant_id,site_id,job_order_id,revision)
);
CREATE INDEX shift_plan_assignments_workstation ON supervisor.shift_plan_assignments(tenant_id,site_id,workstation_id);
CREATE INDEX shift_plan_assignments_job_order ON supervisor.shift_plan_assignments(tenant_id,site_id,job_order_id,job_order_revision);

-- The head only advances by one revision; identity and creation time never change.
CREATE FUNCTION supervisor.job_order_head_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Job order tenant scope denied' USING ERRCODE='42501'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.current_revision<>1 THEN RAISE EXCEPTION 'Job order head must start at one' USING ERRCODE='23514'; END IF;
 ELSIF ROW(NEW.tenant_id,NEW.site_id,NEW.id,NEW.created_at) IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.id,OLD.created_at)
  OR NEW.current_revision<>OLD.current_revision+1 THEN
  RAISE EXCEPTION 'Invalid job order head transition' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER job_order_head_guard BEFORE INSERT OR UPDATE ON supervisor.job_order_heads
 FOR EACH ROW EXECUTE FUNCTION supervisor.job_order_head_guard();

-- Revisions are appended only as the head's new current revision: no gaps, no back-fill.
CREATE FUNCTION supervisor.job_order_revision_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE head bigint;
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Job order revision tenant scope denied' USING ERRCODE='42501'; END IF;
 SELECT current_revision INTO head FROM supervisor.job_order_heads WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND id=NEW.job_order_id;
 IF head IS DISTINCT FROM NEW.revision THEN
  RAISE EXCEPTION 'Job order revision is not the head revision' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER job_order_revision_guard BEFORE INSERT ON supervisor.job_order_revisions
 FOR EACH ROW EXECUTE FUNCTION supervisor.job_order_revision_guard();

-- Only this row trigger writes assignments. Scope is checked independently of the invoker's
-- table policy; FORCE RLS also applies to the owner.
CREATE FUNCTION supervisor.derive_shift_plan_assignments() RETURNS trigger
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,pg_temp SET row_security=on AS $$
BEGIN
 IF TG_TABLE_SCHEMA<>'supervisor' OR TG_TABLE_NAME<>'shift_plan_revisions' OR TG_LEVEL<>'ROW' OR TG_WHEN<>'AFTER' OR TG_OP<>'INSERT'
  OR current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Shift plan assignment scope denied' USING ERRCODE='42501'; END IF;
 INSERT INTO supervisor.shift_plan_assignments(tenant_id,site_id,plan_id,revision,workstation_id,job_order_id,job_order_revision)
 SELECT NEW.tenant_id,NEW.site_id,NEW.plan_id,NEW.revision,(a->>'workstation_id')::uuid,(a->>'job_order_id')::uuid,(a->>'job_order_revision')::bigint
 FROM jsonb_array_elements(NEW.payload->'assignments') a;
 RETURN NULL;
END $$;
REVOKE ALL ON FUNCTION supervisor.derive_shift_plan_assignments() FROM PUBLIC,supervisor_runtime;
CREATE TRIGGER shift_plan_assignments_derived AFTER INSERT ON supervisor.shift_plan_revisions
 FOR EACH ROW EXECUTE FUNCTION supervisor.derive_shift_plan_assignments();
-- No backfill: a revision stored before this migration pins job-order IDs that could not
-- exist (job_order_revisions is created here), so derived rows would violate the job-order
-- foreign key. Such plans fail closed: approval refuses them ("assignments are not derived")
-- and delivery finds no assignment (404) until a new revision pins real job orders.

-- Approval additionally requires every assignment to be derived, every pinned job order to
-- still be its head revision, and no workstation to be booked in another approved plan whose
-- shift overlaps this one (shifts on one line never overlap, so this is across lines).
CREATE OR REPLACE FUNCTION supervisor.shift_plan_approval_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE head bigint; expected integer; derived integer;
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Shift plan approval tenant scope denied' USING ERRCODE='42501'; END IF;
 SELECT current_revision INTO head FROM supervisor.shift_plan_heads WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND id=NEW.plan_id;
 -- Only the current revision is approvable; a superseded draft never becomes current.
 IF head IS DISTINCT FROM NEW.revision THEN
  RAISE EXCEPTION 'Shift plan approval revision is not current' USING ERRCODE='23514'; END IF;
 -- The approver submitted no revision of this plan: neither its creator nor any reviser.
 IF EXISTS(SELECT 1 FROM supervisor.shift_plan_revisions WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND plan_id=NEW.plan_id AND actor_issuer=NEW.approver_issuer AND actor_subject=NEW.approver_subject) THEN
  RAISE EXCEPTION 'Shift plan approver must differ from every submitter' USING ERRCODE='42501'; END IF;
 SELECT jsonb_array_length(payload->'assignments') INTO expected FROM supervisor.shift_plan_revisions WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND plan_id=NEW.plan_id AND revision=NEW.revision;
 SELECT count(*) INTO derived FROM supervisor.shift_plan_assignments WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND plan_id=NEW.plan_id AND revision=NEW.revision;
 IF derived IS DISTINCT FROM expected THEN
  RAISE EXCEPTION 'Shift plan assignments are not derived' USING ERRCODE='23514'; END IF;
 IF EXISTS(SELECT 1 FROM supervisor.shift_plan_assignments a JOIN supervisor.job_order_heads j ON (j.tenant_id,j.site_id,j.id)=(a.tenant_id,a.site_id,a.job_order_id)
  WHERE (a.tenant_id,a.site_id,a.plan_id,a.revision)=(NEW.tenant_id,NEW.site_id,NEW.plan_id,NEW.revision) AND j.current_revision<>a.job_order_revision) THEN
  RAISE EXCEPTION 'Shift plan pins a superseded job order revision' USING ERRCODE='23514'; END IF;
 IF EXISTS(SELECT 1 FROM supervisor.shift_plan_assignments mine
  JOIN supervisor.shift_plan_heads me ON (me.tenant_id,me.site_id,me.id)=(mine.tenant_id,mine.site_id,mine.plan_id)
  JOIN supervisor.shift_plan_heads other ON other.tenant_id=me.tenant_id AND other.site_id=me.site_id AND other.id<>me.id AND other.approved_revision IS NOT NULL AND other.starts_at<me.ends_at AND me.starts_at<other.ends_at
  JOIN supervisor.shift_plan_assignments theirs ON (theirs.tenant_id,theirs.site_id,theirs.plan_id,theirs.revision)=(other.tenant_id,other.site_id,other.id,other.approved_revision) AND theirs.workstation_id=mine.workstation_id
  WHERE (mine.tenant_id,mine.site_id,mine.plan_id,mine.revision)=(NEW.tenant_id,NEW.site_id,NEW.plan_id,NEW.revision)) THEN
  RAISE EXCEPTION 'Workstation is assigned in an overlapping approved shift plan' USING ERRCODE='23P01'; END IF;
 RETURN NEW;
END $$;

DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['job_order_revisions','job_order_receipts','job_order_audit','shift_plan_assignments'] LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t||'_immutable',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['job_order_heads','job_order_revisions','job_order_receipts','job_order_audit','shift_plan_assignments'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['job_order_heads','job_order_revisions','job_order_receipts','job_order_audit'] LOOP
  EXECUTE format('GRANT INSERT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP;
 GRANT UPDATE(current_revision) ON supervisor.job_order_heads TO supervisor_runtime;
END $$;
