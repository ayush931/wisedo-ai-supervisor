-- WIS-520 workstation shift plans: one plan per line and half-open shift interval
-- [starts_at,ends_at), appended as immutable ShiftPlanBodyV1 revisions with a canonical
-- payload digest, and approved by a supervisor other than the plan's submitters over that
-- exact digest. Revision state is derived from the head, never stored on history rows:
-- approved = head.approved_revision; draft = head.current_revision when not approved;
-- every other revision is superseded. One head per (line, shift) and a single approved
-- pointer make "at most one approved-current plan per line and shift" structural.
CREATE TABLE supervisor.shift_plan_heads (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL,
 line_id uuid NOT NULL,starts_at timestamptz NOT NULL,ends_at timestamptz NOT NULL CHECK(ends_at>starts_at),
 current_revision bigint NOT NULL CHECK(current_revision>0),
 approved_revision bigint CHECK(approved_revision>0 AND approved_revision<=current_revision),
 state_revision bigint NOT NULL CHECK(state_revision>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,id),
 UNIQUE(tenant_id,site_id,line_id,starts_at,ends_at),
 UNIQUE(tenant_id,site_id,id,line_id,starts_at,ends_at),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id)
);
CREATE TABLE supervisor.shift_plan_revisions (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,plan_id uuid NOT NULL,revision bigint NOT NULL CHECK(revision>0),
 line_id uuid NOT NULL,starts_at timestamptz NOT NULL,ends_at timestamptz NOT NULL,
 timezone text NOT NULL CHECK(length(timezone) BETWEEN 1 AND 64),
 payload jsonb NOT NULL,
 canonical_payload text NOT NULL CHECK(octet_length(canonical_payload) BETWEEN 1 AND 1048576),
 canonical_payload_sha256 text NOT NULL CHECK(canonical_payload_sha256=encode(sha256(convert_to(canonical_payload,'UTF8')),'hex')),
 digest_schema_version integer NOT NULL CHECK(digest_schema_version=1),
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 -- The digest body is bound to this row: same schema, scope, business payload and shift.
 -- The canonical body differs from the stored request body only by UTC-normalized times.
 CHECK(canonical_payload::jsonb->>'schema'='ShiftPlanBodyV1' AND (canonical_payload::jsonb->>'schema_version')::integer=digest_schema_version),
 CHECK(canonical_payload::jsonb->>'tenant_id'=tenant_id::text AND canonical_payload::jsonb->>'site_id'=site_id::text),
 CHECK((canonical_payload::jsonb->'body')-'starts_at'-'ends_at'=payload-'starts_at'-'ends_at'),
 CHECK((canonical_payload::jsonb->'body'->>'starts_at')::timestamptz=starts_at AND (canonical_payload::jsonb->'body'->>'ends_at')::timestamptz=ends_at),
 CHECK((payload->>'starts_at')::timestamptz=starts_at AND (payload->>'ends_at')::timestamptz=ends_at),
 CHECK(payload->>'line_id'=line_id::text AND payload->>'timezone'=timezone AND (payload->>'schema_version')::integer=1),
 PRIMARY KEY(tenant_id,site_id,plan_id,revision),
 UNIQUE(tenant_id,site_id,plan_id,revision,canonical_payload_sha256),
 -- A revision can never move its plan to another line or shift.
 FOREIGN KEY(tenant_id,site_id,plan_id,line_id,starts_at,ends_at) REFERENCES supervisor.shift_plan_heads(tenant_id,site_id,id,line_id,starts_at,ends_at)
);
-- One immutable decision per approved revision, over the revision's exact digest.
CREATE TABLE supervisor.shift_plan_approvals (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,plan_id uuid NOT NULL,revision bigint NOT NULL,
 canonical_payload_sha256 text NOT NULL CHECK(canonical_payload_sha256 ~ '^[0-9a-f]{64}$'),
 approver_issuer text NOT NULL CHECK(length(approver_issuer) BETWEEN 1 AND 255),
 approver_subject text NOT NULL CHECK(length(approver_subject) BETWEEN 1 AND 255),
 policy_revision bigint NOT NULL CHECK(policy_revision=1),
 auth_time timestamptz NOT NULL,approved_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK(auth_time<=approved_at AND auth_time>=approved_at-interval '15 minutes'),
 PRIMARY KEY(tenant_id,site_id,plan_id,revision),
 FOREIGN KEY(tenant_id,site_id,plan_id,revision,canonical_payload_sha256)
  REFERENCES supervisor.shift_plan_revisions(tenant_id,site_id,plan_id,revision,canonical_payload_sha256)
);
ALTER TABLE supervisor.shift_plan_heads
 ADD CONSTRAINT shift_plan_current_fk FOREIGN KEY(tenant_id,site_id,id,current_revision)
  REFERENCES supervisor.shift_plan_revisions(tenant_id,site_id,plan_id,revision) DEFERRABLE INITIALLY DEFERRED,
 ADD CONSTRAINT shift_plan_approved_fk FOREIGN KEY(tenant_id,site_id,id,approved_revision)
  REFERENCES supervisor.shift_plan_approvals(tenant_id,site_id,plan_id,revision) DEFERRABLE INITIALLY DEFERRED;
CREATE TABLE supervisor.shift_plan_receipts (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,actor_issuer text NOT NULL,actor_subject text NOT NULL,
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 operation text NOT NULL CHECK(operation IN ('create','revise','approve')),
 plan_id uuid NOT NULL,revision bigint NOT NULL,
 PRIMARY KEY(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id,plan_id,revision) REFERENCES supervisor.shift_plan_revisions(tenant_id,site_id,plan_id,revision)
);
CREATE TABLE supervisor.shift_plan_audit (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),tenant_id uuid NOT NULL,site_id uuid NOT NULL,
 actor_issuer text NOT NULL,actor_subject text NOT NULL,plan_id uuid,revision bigint,
 event_type text NOT NULL CHECK(event_type IN ('plan.created','plan.revised','plan.approved','plan.read','plan.listed','mutation.replayed')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((event_type='plan.listed')=(plan_id IS NULL)),
 CHECK((event_type IN ('plan.created','plan.revised','plan.approved'))<=(revision IS NOT NULL)),
 FOREIGN KEY(tenant_id,site_id,plan_id) REFERENCES supervisor.shift_plan_heads(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,plan_id,revision) REFERENCES supervisor.shift_plan_revisions(tenant_id,site_id,plan_id,revision)
);

-- The head permits exactly two transitions, each advancing state_revision by one: revise
-- (current+1, approval pointer unchanged) and approve (pointer moves forward to current).
-- Shifts on one line never overlap, so "the current plan for a line" has one answer.
CREATE FUNCTION supervisor.shift_plan_head_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Shift plan tenant scope denied' USING ERRCODE='42501'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.current_revision<>1 OR NEW.approved_revision IS NOT NULL OR NEW.state_revision<>1 THEN
   RAISE EXCEPTION 'Shift plan head must start at one' USING ERRCODE='23514'; END IF;
  IF EXISTS(SELECT 1 FROM supervisor.shift_plan_heads h WHERE h.tenant_id=NEW.tenant_id AND h.site_id=NEW.site_id AND h.line_id=NEW.line_id AND h.starts_at<NEW.ends_at AND NEW.starts_at<h.ends_at) THEN
   RAISE EXCEPTION 'Shift plan overlaps an existing shift on this line' USING ERRCODE='23P01'; END IF;
 ELSIF ROW(NEW.tenant_id,NEW.site_id,NEW.id,NEW.line_id,NEW.starts_at,NEW.ends_at,NEW.created_at) IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.id,OLD.line_id,OLD.starts_at,OLD.ends_at,OLD.created_at)
  OR NEW.state_revision<>OLD.state_revision+1
  OR NOT ((NEW.current_revision=OLD.current_revision+1 AND NEW.approved_revision IS NOT DISTINCT FROM OLD.approved_revision)
   OR (NEW.current_revision=OLD.current_revision AND NEW.approved_revision=NEW.current_revision AND NEW.approved_revision IS DISTINCT FROM OLD.approved_revision)) THEN
  RAISE EXCEPTION 'Invalid shift plan head transition' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER shift_plan_head_guard BEFORE INSERT OR UPDATE ON supervisor.shift_plan_heads
 FOR EACH ROW EXECUTE FUNCTION supervisor.shift_plan_head_guard();

-- Revisions are appended only as the head's new current revision: no gaps, no back-fill.
CREATE FUNCTION supervisor.shift_plan_revision_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE head bigint;
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Shift plan revision tenant scope denied' USING ERRCODE='42501'; END IF;
 SELECT current_revision INTO head FROM supervisor.shift_plan_heads WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND id=NEW.plan_id;
 IF head IS DISTINCT FROM NEW.revision THEN
  RAISE EXCEPTION 'Shift plan revision is not the head revision' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER shift_plan_revision_guard BEFORE INSERT ON supervisor.shift_plan_revisions
 FOR EACH ROW EXECUTE FUNCTION supervisor.shift_plan_revision_guard();

CREATE FUNCTION supervisor.shift_plan_approval_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE head bigint;
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
 RETURN NEW;
END $$;
CREATE TRIGGER shift_plan_approval_guard BEFORE INSERT ON supervisor.shift_plan_approvals
 FOR EACH ROW EXECUTE FUNCTION supervisor.shift_plan_approval_guard();

DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['shift_plan_revisions','shift_plan_approvals','shift_plan_receipts','shift_plan_audit'] LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t||'_immutable',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['shift_plan_heads','shift_plan_revisions','shift_plan_approvals','shift_plan_receipts','shift_plan_audit'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP;
 GRANT UPDATE(current_revision,approved_revision,state_revision) ON supervisor.shift_plan_heads TO supervisor_runtime;
END $$;
