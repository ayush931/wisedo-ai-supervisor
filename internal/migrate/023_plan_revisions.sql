-- WIS-272 authoritative plan bootstrap: typed production-plan revisions captured with
-- source-span provenance, and PlanApprovalV1 supervisor decisions over an exact canonical
-- payload digest. Intake admits a production_plan only through a current approval.
CREATE TABLE supervisor.plan_revisions (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL,
 order_id uuid NOT NULL,asset_id uuid NOT NULL,plan_revision bigint NOT NULL CHECK(plan_revision>0),
 window_start timestamptz NOT NULL,window_end timestamptz NOT NULL CHECK(window_end>window_start),
 payload jsonb NOT NULL,
 canonical_payload text NOT NULL CHECK(octet_length(canonical_payload) BETWEEN 1 AND 1048576),
 canonical_payload_sha256 text NOT NULL CHECK(canonical_payload_sha256=encode(sha256(convert_to(canonical_payload,'UTF8')),'hex')),
 digest_schema_version integer NOT NULL CHECK(digest_schema_version=1),
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 -- The digest body is bound to this row: same scope, same business payload, same keys.
 CHECK((canonical_payload::jsonb->>'schema_version')::integer=digest_schema_version),
 CHECK(canonical_payload::jsonb->>'tenant_id'=tenant_id::text AND canonical_payload::jsonb->>'site_id'=site_id::text),
 CHECK(canonical_payload::jsonb->'plan'=payload),
 CHECK(payload->>'order_id'=order_id::text AND payload->>'asset_id'=asset_id::text AND (payload->>'plan_revision')::bigint=plan_revision),
 CHECK(NOT payload ? 'approval_ref'),
 PRIMARY KEY(tenant_id,site_id,id),
 UNIQUE(tenant_id,site_id,order_id,asset_id,plan_revision),
 UNIQUE(tenant_id,site_id,id,canonical_payload_sha256),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 FOREIGN KEY(tenant_id,site_id,order_id) REFERENCES supervisor.knowledge_entity_heads(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,asset_id) REFERENCES supervisor.knowledge_entity_heads(tenant_id,site_id,id)
);
CREATE TABLE supervisor.plan_revision_spans (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,plan_revision_id uuid NOT NULL,span_id uuid NOT NULL,
 PRIMARY KEY(tenant_id,site_id,plan_revision_id,span_id),
 FOREIGN KEY(tenant_id,site_id,plan_revision_id) REFERENCES supervisor.plan_revisions(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,span_id) REFERENCES supervisor.evidence_spans(tenant_id,site_id,id)
);
CREATE TABLE supervisor.plan_review_heads (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,plan_revision_id uuid NOT NULL,
 current_revision bigint NOT NULL CHECK(current_revision>0),
 PRIMARY KEY(tenant_id,site_id,plan_revision_id),
 FOREIGN KEY(tenant_id,site_id,plan_revision_id) REFERENCES supervisor.plan_revisions(tenant_id,site_id,id)
);
-- PlanApprovalV1: one immutable row per supervisor decision. id is the approval_ref.
CREATE TABLE supervisor.plan_approvals (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL,
 plan_revision_id uuid NOT NULL,review_revision bigint NOT NULL CHECK(review_revision>0),
 canonical_payload_sha256 text NOT NULL CHECK(canonical_payload_sha256 ~ '^[0-9a-f]{64}$'),
 decision text NOT NULL CHECK(decision IN ('approve','reject')),
 reason text NOT NULL CHECK(length(btrim(reason)) BETWEEN 1 AND 1000 AND reason=btrim(reason)),
 reviewer_issuer text NOT NULL CHECK(length(reviewer_issuer) BETWEEN 1 AND 255),
 reviewer_subject text NOT NULL CHECK(length(reviewer_subject) BETWEEN 1 AND 255),
 policy_revision bigint NOT NULL CHECK(policy_revision=1),
 auth_time timestamptz NOT NULL,reviewed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK(auth_time<=reviewed_at AND auth_time>=reviewed_at-interval '15 minutes'),
 PRIMARY KEY(tenant_id,site_id,id),
 UNIQUE(tenant_id,site_id,plan_revision_id,review_revision),
 UNIQUE(tenant_id,site_id,id,plan_revision_id),
 FOREIGN KEY(tenant_id,site_id,plan_revision_id) REFERENCES supervisor.plan_review_heads(tenant_id,site_id,plan_revision_id),
 FOREIGN KEY(tenant_id,site_id,plan_revision_id,canonical_payload_sha256)
  REFERENCES supervisor.plan_revisions(tenant_id,site_id,id,canonical_payload_sha256)
);
ALTER TABLE supervisor.plan_review_heads ADD CONSTRAINT plan_review_current_fk
 FOREIGN KEY(tenant_id,site_id,plan_revision_id,current_revision)
 REFERENCES supervisor.plan_approvals(tenant_id,site_id,plan_revision_id,review_revision) DEFERRABLE INITIALLY DEFERRED;
-- The decision persists exactly the provenance it reviewed: every span of the plan revision.
CREATE TABLE supervisor.plan_approval_spans (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,approval_id uuid NOT NULL,plan_revision_id uuid NOT NULL,span_id uuid NOT NULL,
 PRIMARY KEY(tenant_id,site_id,approval_id,span_id),
 FOREIGN KEY(tenant_id,site_id,approval_id,plan_revision_id) REFERENCES supervisor.plan_approvals(tenant_id,site_id,id,plan_revision_id),
 FOREIGN KEY(tenant_id,site_id,plan_revision_id,span_id) REFERENCES supervisor.plan_revision_spans(tenant_id,site_id,plan_revision_id,span_id)
);
CREATE TABLE supervisor.plan_receipts (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,actor_issuer text NOT NULL,actor_subject text NOT NULL,
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 operation text NOT NULL CHECK(operation IN ('plan','review')),
 plan_revision_id uuid NOT NULL,approval_id uuid,
 CHECK((operation='plan')=(approval_id IS NULL)),
 PRIMARY KEY(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id,plan_revision_id) REFERENCES supervisor.plan_revisions(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,approval_id,plan_revision_id) REFERENCES supervisor.plan_approvals(tenant_id,site_id,id,plan_revision_id)
);
CREATE TABLE supervisor.plan_audit (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),tenant_id uuid NOT NULL,site_id uuid NOT NULL,
 actor_issuer text NOT NULL,actor_subject text NOT NULL,plan_revision_id uuid NOT NULL,approval_id uuid,
 event_type text NOT NULL CHECK(event_type IN ('plan.captured','plan.read','review.decided','mutation.replayed')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((event_type='review.decided')<=(approval_id IS NOT NULL)),
 FOREIGN KEY(tenant_id,site_id,plan_revision_id) REFERENCES supervisor.plan_revisions(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,approval_id,plan_revision_id) REFERENCES supervisor.plan_approvals(tenant_id,site_id,id,plan_revision_id)
);

CREATE FUNCTION supervisor.plan_revision_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE order_type text;asset_type text;
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Plan revision tenant scope denied' USING ERRCODE='42501'; END IF;
 SELECT entity_type INTO order_type FROM supervisor.knowledge_entities WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND id=NEW.order_id;
 SELECT entity_type INTO asset_type FROM supervisor.knowledge_entities WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND id=NEW.asset_id;
 IF order_type IS DISTINCT FROM 'order' OR asset_type IS DISTINCT FROM 'asset' THEN
  RAISE EXCEPTION 'Plan entity type mismatch' USING ERRCODE='23514'; END IF;
 -- Plan revisions per order/asset are contiguous; a gap is never a valid history.
 IF NEW.plan_revision>1 AND NOT EXISTS(SELECT 1 FROM supervisor.plan_revisions p WHERE p.tenant_id=NEW.tenant_id AND p.site_id=NEW.site_id AND p.order_id=NEW.order_id AND p.asset_id=NEW.asset_id AND p.plan_revision=NEW.plan_revision-1) THEN
  RAISE EXCEPTION 'Plan revision gap' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER plan_revision_guard BEFORE INSERT ON supervisor.plan_revisions
 FOR EACH ROW EXECUTE FUNCTION supervisor.plan_revision_guard();

CREATE FUNCTION supervisor.plan_review_head_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Plan review head tenant scope denied' USING ERRCODE='42501'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.current_revision<>1 THEN RAISE EXCEPTION 'Plan review head must start at one' USING ERRCODE='23514'; END IF;
 ELSIF ROW(NEW.tenant_id,NEW.site_id,NEW.plan_revision_id) IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.plan_revision_id)
  OR NEW.current_revision<>OLD.current_revision+1 THEN
  RAISE EXCEPTION 'Invalid plan review revision transition' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER plan_review_head_guard BEFORE INSERT OR UPDATE ON supervisor.plan_review_heads
 FOR EACH ROW EXECUTE FUNCTION supervisor.plan_review_head_guard();

CREATE FUNCTION supervisor.plan_approval_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE head bigint;submitter_issuer text;submitter_subject text;
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Plan approval tenant scope denied' USING ERRCODE='42501'; END IF;
 SELECT current_revision INTO head FROM supervisor.plan_review_heads WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND plan_revision_id=NEW.plan_revision_id;
 IF head IS DISTINCT FROM NEW.review_revision THEN
  RAISE EXCEPTION 'Plan approval revision is not current head' USING ERRCODE='23514'; END IF;
 SELECT actor_issuer,actor_subject INTO submitter_issuer,submitter_subject FROM supervisor.plan_revisions WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND id=NEW.plan_revision_id;
 -- A plan is never approved by the person who submitted it.
 IF ROW(submitter_issuer,submitter_subject) IS NOT DISTINCT FROM ROW(NEW.reviewer_issuer,NEW.reviewer_subject) THEN
  RAISE EXCEPTION 'Plan reviewer must differ from submitter' USING ERRCODE='42501'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER plan_approval_guard BEFORE INSERT ON supervisor.plan_approvals
 FOR EACH ROW EXECUTE FUNCTION supervisor.plan_approval_guard();

-- Approval spans must be exactly the plan revision's spans: checked once all rows exist.
CREATE FUNCTION supervisor.plan_approval_spans_complete() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE expected bigint;actual bigint;
BEGIN
 SELECT count(*) INTO expected FROM supervisor.plan_revision_spans WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND plan_revision_id=NEW.plan_revision_id;
 SELECT count(*) INTO actual FROM supervisor.plan_approval_spans WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND approval_id=NEW.id;
 IF expected=0 OR expected<>actual THEN RAISE EXCEPTION 'Plan approval provenance incomplete' USING ERRCODE='23514'; END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER plan_approval_spans_complete AFTER INSERT ON supervisor.plan_approvals
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION supervisor.plan_approval_spans_complete();
CREATE FUNCTION supervisor.plan_revision_spans_present() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM supervisor.plan_revision_spans WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND plan_revision_id=NEW.id) THEN
  RAISE EXCEPTION 'Plan revision requires source spans' USING ERRCODE='23514'; END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER plan_revision_spans_present AFTER INSERT ON supervisor.plan_revisions
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION supervisor.plan_revision_spans_present();


-- Intake: an admitted production_plan pins the exact approval it cited. A plan is
-- quarantined when its IDs disagree with the source's mappings (MAPPING_MISMATCH), a later
-- revision exists (SUPERSEDED_PLAN) or the source's reviewed scope excludes plans (OUT_OF_SCOPE).
ALTER TABLE supervisor.operational_event_records
 ADD COLUMN plan_revision_id uuid,ADD COLUMN plan_approval_id uuid,
 ADD CONSTRAINT operational_event_plan_approval_fk FOREIGN KEY(tenant_id,site_id,plan_approval_id,plan_revision_id)
  REFERENCES supervisor.plan_approvals(tenant_id,site_id,id,plan_revision_id),
 ADD CONSTRAINT operational_event_plan_approval_check CHECK(
  (plan_approval_id IS NULL)=(plan_revision_id IS NULL)
  AND (plan_approval_id IS NULL OR (event_kind='production_plan' AND quarantine_reason IS NULL))
  AND (event_kind<>'production_plan' OR quarantine_reason IS NOT NULL OR plan_approval_id IS NOT NULL)),
 DROP CONSTRAINT operational_event_records_quarantine_reason_check,
 ADD CONSTRAINT operational_event_records_quarantine_reason_check CHECK(quarantine_reason IS NULL OR quarantine_reason IN ('UNAPPROVED_PLAN','UNKNOWN_MAPPING','DUPLICATE_EVENT','MAPPING_MISMATCH','SUPERSEDED_PLAN','OUT_OF_SCOPE'));
ALTER TABLE supervisor.operational_event_inbox
 DROP CONSTRAINT operational_event_inbox_reason_check,
 ADD CONSTRAINT operational_event_inbox_reason_check CHECK(reason IS NULL OR reason IN ('UNAPPROVED_PLAN','UNKNOWN_MAPPING','DUPLICATE_EVENT','MAPPING_MISMATCH','SUPERSEDED_PLAN','OUT_OF_SCOPE'));

-- Enable forced RLS only after every constraint above is validated, so no scan here runs
-- against a table this migration has already forced.
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['plan_revisions','plan_revision_spans','plan_approvals','plan_approval_spans','plan_receipts','plan_audit'] LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t||'_immutable',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['plan_revisions','plan_revision_spans','plan_review_heads','plan_approvals','plan_approval_spans','plan_receipts','plan_audit'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
  IF t='plan_review_heads' THEN
   EXECUTE format('GRANT UPDATE(current_revision) ON supervisor.%I TO supervisor_runtime',t);
  END IF;
 END LOOP;
END $$;
