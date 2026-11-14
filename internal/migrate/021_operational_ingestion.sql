-- Durable, tenant/site-scoped intake. Receipt, event rows, inbox/outbox and cursor commit atomically.
ALTER TABLE supervisor.operational_source_mappings ADD CONSTRAINT operational_source_mapping_external_target UNIQUE(tenant_id,site_id,source_id,mapping_revision,external_type,external_key,entity_id,entity_revision);
CREATE TABLE supervisor.operational_event_sequence_heads (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, source_id uuid NOT NULL,
 last_sequence bigint NOT NULL CHECK(last_sequence>=0),
 PRIMARY KEY(tenant_id,site_id,source_id),
 FOREIGN KEY(tenant_id,site_id,source_id) REFERENCES supervisor.operational_source_heads(tenant_id,site_id,source_id)
);
CREATE TABLE supervisor.operational_event_batches (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, batch_id uuid NOT NULL,
 source_id uuid NOT NULL, source_revision bigint NOT NULL, mapping_revision bigint NOT NULL,
 first_sequence bigint NOT NULL CHECK(first_sequence>0), last_sequence bigint NOT NULL CHECK(last_sequence>=first_sequence),
 event_count integer NOT NULL CHECK(event_count BETWEEN 1 AND 100),
 body_sha256 text NOT NULL CHECK(body_sha256 ~ '^[0-9a-f]{64}$'),
 accepted_by_issuer text NOT NULL, accepted_by_subject text NOT NULL,
 accepted_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,batch_id), UNIQUE(tenant_id,site_id,source_id,batch_id),
 FOREIGN KEY(tenant_id,site_id,source_id,source_revision,mapping_revision)
  REFERENCES supervisor.operational_source_revisions(tenant_id,site_id,source_id,revision,mapping_revision)
);
CREATE TABLE supervisor.operational_event_records (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, source_id uuid NOT NULL,
 source_revision bigint NOT NULL, mapping_revision bigint NOT NULL,
 source_sequence bigint NOT NULL CHECK(source_sequence>0),
 external_key text NOT NULL CHECK(length(external_key) BETWEEN 1 AND 512 AND external_key=btrim(external_key)),
 revision bigint NOT NULL CHECK(revision>0),
 event_kind text NOT NULL CHECK(event_kind IN ('production_plan','production_delta','machine_state','operator_report','heartbeat','watermark')),
 event_at timestamptz NOT NULL,
 payload jsonb NOT NULL CHECK(octet_length(payload::text)<=262144),
 payload_sha256 text NOT NULL CHECK(payload_sha256 ~ '^[0-9a-f]{64}$'),
 event_sha256 text NOT NULL CHECK(event_sha256 ~ '^[0-9a-f]{64}$'),
 asset_external_key text, order_external_key text,
 asset_entity_type text GENERATED ALWAYS AS ('asset') STORED,
 order_entity_type text GENERATED ALWAYS AS ('order') STORED,
 asset_entity_id uuid, asset_entity_revision bigint,
 order_entity_id uuid, order_entity_revision bigint,
 quarantine_reason text CHECK(quarantine_reason IS NULL OR quarantine_reason IN ('UNAPPROVED_PLAN','UNKNOWN_MAPPING','DUPLICATE_EVENT')),
 batch_id uuid NOT NULL,
 PRIMARY KEY(tenant_id,site_id,source_id,source_sequence),
 FOREIGN KEY(tenant_id,site_id,source_id,batch_id) REFERENCES supervisor.operational_event_batches(tenant_id,site_id,source_id,batch_id),
 FOREIGN KEY(tenant_id,site_id,source_id,source_revision,mapping_revision) REFERENCES supervisor.operational_source_revisions(tenant_id,site_id,source_id,revision,mapping_revision),
 FOREIGN KEY(tenant_id,site_id,source_id,mapping_revision,asset_entity_type,asset_external_key,asset_entity_id,asset_entity_revision)
  REFERENCES supervisor.operational_source_mappings(tenant_id,site_id,source_id,mapping_revision,external_type,external_key,entity_id,entity_revision),
 FOREIGN KEY(tenant_id,site_id,source_id,mapping_revision,order_entity_type,order_external_key,order_entity_id,order_entity_revision)
  REFERENCES supervisor.operational_source_mappings(tenant_id,site_id,source_id,mapping_revision,external_type,external_key,entity_id,entity_revision),
 CHECK((asset_external_key IS NULL AND asset_entity_id IS NULL AND asset_entity_revision IS NULL) OR (asset_external_key IS NOT NULL AND asset_entity_id IS NOT NULL AND asset_entity_revision IS NOT NULL) OR (asset_external_key IS NOT NULL AND quarantine_reason='UNKNOWN_MAPPING' AND asset_entity_id IS NULL AND asset_entity_revision IS NULL)),
 CHECK((order_external_key IS NULL AND order_entity_id IS NULL AND order_entity_revision IS NULL) OR (order_external_key IS NOT NULL AND order_entity_id IS NOT NULL AND order_entity_revision IS NOT NULL) OR (order_external_key IS NOT NULL AND quarantine_reason='UNKNOWN_MAPPING' AND order_entity_id IS NULL AND order_entity_revision IS NULL))
);
CREATE TABLE supervisor.operational_event_validations (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, source_id uuid NOT NULL, source_sequence bigint NOT NULL,
 validation_revision bigint NOT NULL CHECK(validation_revision>0),
 status text NOT NULL CHECK(status IN ('pending','valid','late','clock_suspect','quarantined')),
 reason text, assessed_at timestamptz NOT NULL DEFAULT clock_timestamp(), assessed_by text NOT NULL,
 PRIMARY KEY(tenant_id,site_id,source_id,source_sequence,validation_revision),
 FOREIGN KEY(tenant_id,site_id,source_id,source_sequence) REFERENCES supervisor.operational_event_records(tenant_id,site_id,source_id,source_sequence)
);
CREATE TABLE supervisor.operational_event_inbox (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, source_id uuid NOT NULL, source_sequence bigint NOT NULL,
 batch_id uuid NOT NULL, state text NOT NULL CHECK(state IN ('pending','quarantined')),
 reason text CHECK(reason IS NULL OR reason IN ('UNAPPROVED_PLAN','UNKNOWN_MAPPING','DUPLICATE_EVENT')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,source_id,source_sequence),
 FOREIGN KEY(tenant_id,site_id,source_id,source_sequence) REFERENCES supervisor.operational_event_records(tenant_id,site_id,source_id,source_sequence),
 FOREIGN KEY(tenant_id,site_id,source_id,batch_id) REFERENCES supervisor.operational_event_batches(tenant_id,site_id,source_id,batch_id),
 CHECK((state='pending' AND reason IS NULL) OR (state='quarantined' AND reason IS NOT NULL))
);
CREATE TABLE supervisor.operational_event_outbox (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, source_id uuid NOT NULL, source_sequence bigint NOT NULL,
 batch_id uuid NOT NULL, event_type text NOT NULL CHECK(event_type='operational.event.received'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,source_id,source_sequence),
 FOREIGN KEY(tenant_id,site_id,source_id,source_sequence) REFERENCES supervisor.operational_event_records(tenant_id,site_id,source_id,source_sequence),
 FOREIGN KEY(tenant_id,site_id,source_id,batch_id) REFERENCES supervisor.operational_event_batches(tenant_id,site_id,source_id,batch_id)
);
CREATE TABLE supervisor.operational_event_batch_receipts (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, actor_issuer text NOT NULL, actor_subject text NOT NULL,
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'), batch_id uuid NOT NULL,
 source_id uuid NOT NULL, source_revision bigint NOT NULL, mapping_revision bigint NOT NULL,
 first_sequence bigint NOT NULL, last_sequence bigint NOT NULL, event_count integer NOT NULL CHECK(event_count BETWEEN 1 AND 100),
 accepted_at timestamptz NOT NULL,
 PRIMARY KEY(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id,source_id,batch_id) REFERENCES supervisor.operational_event_batches(tenant_id,site_id,source_id,batch_id),
 FOREIGN KEY(tenant_id,site_id,source_id,source_revision,mapping_revision) REFERENCES supervisor.operational_source_revisions(tenant_id,site_id,source_id,revision,mapping_revision)
);
CREATE FUNCTION supervisor.operational_event_cursor_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN RAISE EXCEPTION 'Event cursor tenant scope denied' USING ERRCODE='42501'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.last_sequence<>0 THEN RAISE EXCEPTION 'Event cursor must start at zero' USING ERRCODE='23514'; END IF;
 ELSIF ROW(NEW.tenant_id,NEW.site_id,NEW.source_id) IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.source_id) OR NEW.last_sequence<=OLD.last_sequence OR (SELECT count(*) FROM supervisor.operational_event_records e WHERE (e.tenant_id,e.site_id,e.source_id)=(OLD.tenant_id,OLD.site_id,OLD.source_id) AND e.source_sequence>OLD.last_sequence AND e.source_sequence<=NEW.last_sequence)<>NEW.last_sequence-OLD.last_sequence THEN
  RAISE EXCEPTION 'Event cursor must advance over a contiguous durable range' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER operational_event_cursor_guard BEFORE INSERT OR UPDATE ON supervisor.operational_event_sequence_heads FOR EACH ROW EXECUTE FUNCTION supervisor.operational_event_cursor_guard();
CREATE TRIGGER operational_event_batch_immutable BEFORE UPDATE OR DELETE ON supervisor.operational_event_batches FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
CREATE TRIGGER operational_event_record_immutable BEFORE UPDATE OR DELETE ON supervisor.operational_event_records FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
CREATE TRIGGER operational_event_validation_immutable BEFORE UPDATE OR DELETE ON supervisor.operational_event_validations FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
CREATE TRIGGER operational_event_inbox_immutable BEFORE UPDATE OR DELETE ON supervisor.operational_event_inbox FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
CREATE TRIGGER operational_event_outbox_immutable BEFORE UPDATE OR DELETE ON supervisor.operational_event_outbox FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
CREATE TRIGGER operational_event_receipt_immutable BEFORE UPDATE OR DELETE ON supervisor.operational_event_batch_receipts FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['operational_event_sequence_heads','operational_event_batches','operational_event_records','operational_event_validations','operational_event_inbox','operational_event_outbox','operational_event_batch_receipts'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  IF t='operational_event_sequence_heads' THEN EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t); EXECUTE format('GRANT UPDATE(last_sequence) ON supervisor.%I TO supervisor_runtime',t);
  ELSIF t='operational_event_batch_receipts' THEN EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
  ELSE EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t); END IF;
 END LOOP;
END $$;
