-- WIS-273 case triggers, outbox and lifecycle receipts. A trigger names one operational source
-- event (its deterministic record_id from the context's source_record_refs) and the context it
-- qualified; the trigger ledger makes a repeated source event idempotent and records which case
-- it opened or joined. The outbox is delivery plumbing for the stable workflow ID
-- case:<tenant>:<case_id>; it never schedules work by itself. Receipts make the human
-- lifecycle mutations (cancel, resume, reopen) idempotent per actor and key.

CREATE TABLE supervisor.case_triggers (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,
 source_id uuid NOT NULL,source_event_id uuid NOT NULL,
 source_sequence bigint NOT NULL,
 context_snapshot_id uuid NOT NULL,
 exception_kind text NOT NULL CHECK(exception_kind='production_shortfall'),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 case_id uuid NOT NULL,
 -- true when the trigger opened the case, false when it joined an already active episode.
 opened boolean NOT NULL,
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,source_id,source_event_id),
 FOREIGN KEY(tenant_id,site_id,case_id) REFERENCES supervisor.cases(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,source_id,source_sequence) REFERENCES supervisor.operational_event_records(tenant_id,site_id,source_id,source_sequence),
 FOREIGN KEY(tenant_id,site_id,context_snapshot_id) REFERENCES supervisor.plant_contexts(tenant_id,site_id,id)
);
-- At most one opening trigger per case.
CREATE UNIQUE INDEX case_triggers_one_opener ON supervisor.case_triggers(tenant_id,site_id,case_id) WHERE opened;

-- One row per workflow delivery: start the case workflow, or signal it with a case revision.
-- event_id is the deduplication key the workflow records in the product database.
CREATE TABLE supervisor.case_outbox (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL,
 case_id uuid NOT NULL,case_revision bigint NOT NULL CHECK(case_revision>=1),
 kind text NOT NULL CHECK(kind IN ('start','signal','cancel')),
 event_id uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 delivered_at timestamptz,
 CHECK(delivered_at IS NULL OR delivered_at>=created_at),
 PRIMARY KEY(tenant_id,site_id,id),
 UNIQUE(tenant_id,site_id,case_id,event_id),
 FOREIGN KEY(tenant_id,site_id,case_id) REFERENCES supervisor.cases(tenant_id,site_id,id)
);
CREATE INDEX case_outbox_pending ON supervisor.case_outbox(tenant_id,created_at) WHERE delivered_at IS NULL;

-- A delivered row is final; only delivered_at may be set, once.
CREATE FUNCTION supervisor.case_outbox_guard() RETURNS trigger LANGUAGE plpgsql
 SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'case outbox rows are retained' USING ERRCODE='42501'; END IF;
 IF OLD.delivered_at IS NOT NULL OR NEW.delivered_at IS NULL
    OR (NEW.tenant_id,NEW.site_id,NEW.id,NEW.case_id,NEW.case_revision,NEW.kind,NEW.event_id,NEW.created_at)
       IS DISTINCT FROM (OLD.tenant_id,OLD.site_id,OLD.id,OLD.case_id,OLD.case_revision,OLD.kind,OLD.event_id,OLD.created_at) THEN
  RAISE EXCEPTION 'a case outbox row is only marked delivered, once' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER case_outbox_guard BEFORE UPDATE OR DELETE ON supervisor.case_outbox
 FOR EACH ROW EXECUTE FUNCTION supervisor.case_outbox_guard();

CREATE TABLE supervisor.case_receipts (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 idempotency_key uuid NOT NULL,
 operation text NOT NULL CHECK(operation IN ('case.cancel','case.resume','case.reopen')),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 case_id uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id,case_id) REFERENCES supervisor.cases(tenant_id,site_id,id)
);

DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['case_triggers','case_receipts'] LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t||'_immutable',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['case_triggers','case_outbox','case_receipts'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP;
 GRANT UPDATE(delivered_at) ON supervisor.case_outbox TO supervisor_runtime;
END $$;
