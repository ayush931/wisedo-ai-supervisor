-- WIS-272 PlantContextSnapshotV1: immutable, digest-bound production contexts per
-- order/asset/15-minute window, with exact record provenance and a supersession chain.
-- Contexts never grant authority; current eligibility is re-checked on every read.

-- Delta payloads are untrusted JSON. A malformed timestamp must never fail a query; it
-- yields NULL so the record is still selected and fails closed as INVALID_DELTA.
CREATE FUNCTION supervisor.try_utc_timestamp(value text) RETURNS timestamptz
 LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE SET search_path=pg_catalog AS $$
BEGIN
 IF value IS NULL OR value !~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?Z$' THEN RETURN NULL; END IF;
 RETURN (value::timestamp AT TIME ZONE 'UTC');
EXCEPTION WHEN others THEN RETURN NULL;
END $$;
REVOKE ALL ON FUNCTION supervisor.try_utc_timestamp(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION supervisor.try_utc_timestamp(text) TO supervisor_runtime;

-- Context assembly reads every revision of each delta chain for an order/asset. Without
-- these indexes a 10k-record window is a nested-loop table scan (~25s, measured); with
-- them it is ~0.2s. The chain index also serves intake's external_key/revision lookups.
CREATE INDEX operational_event_records_chain ON supervisor.operational_event_records(tenant_id,site_id,source_id,external_key,revision);
CREATE INDEX operational_event_records_delta_scope ON supervisor.operational_event_records(tenant_id,site_id,(payload->>'order_id'),(payload->>'asset_id'))
 WHERE event_kind='production_delta' AND quarantine_reason IS NULL;

CREATE TABLE supervisor.plant_contexts (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL,
 order_id uuid NOT NULL,asset_id uuid NOT NULL,
 window_start timestamptz NOT NULL,window_end timestamptz NOT NULL,
 process_snapshot_id uuid NOT NULL,
 process_snapshot_sha256 text NOT NULL CHECK(process_snapshot_sha256 ~ '^[0-9a-f]{64}$'),
 plan_revision_id uuid,plan_approval_id uuid,
 readiness text NOT NULL CHECK(readiness IN ('READY','LIMITED','NOT_READY')),
 snapshot jsonb NOT NULL CHECK(jsonb_typeof(snapshot)='object' AND octet_length(snapshot::text)<=4194304),
 digest text NOT NULL CHECK(digest ~ '^[0-9a-f]{64}$'),
 supersedes_context_id uuid,
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 evaluated_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 -- Disjoint UTC 15-minute windows aligned to epoch multiples of 900 seconds.
 CHECK(window_end=window_start+interval '15 minutes' AND extract(epoch FROM window_start)::bigint%900=0 AND date_trunc('second',window_start)=window_start),
 CHECK((plan_revision_id IS NULL)=(plan_approval_id IS NULL)),
 -- The stored body agrees with the indexed columns; generated fields are not hashed.
 CHECK(snapshot->>'readiness'=readiness AND snapshot->>'digest'=digest AND snapshot->>'context_id'=id::text),
 CHECK(snapshot->>'order_id'=order_id::text AND snapshot->>'asset_id'=asset_id::text AND snapshot->>'site_id'=site_id::text),
 CHECK((readiness='READY')=(snapshot->>'actual_good_units_exact' IS NOT NULL AND snapshot->>'deficit_good_units_exact' IS NOT NULL)),
 CHECK(supersedes_context_id IS DISTINCT FROM id),
 PRIMARY KEY(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 FOREIGN KEY(tenant_id,site_id,order_id) REFERENCES supervisor.knowledge_entity_heads(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,asset_id) REFERENCES supervisor.knowledge_entity_heads(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,process_snapshot_id) REFERENCES supervisor.knowledge_process_snapshots(tenant_id,site_id,snapshot_id),
 FOREIGN KEY(tenant_id,site_id,plan_approval_id,plan_revision_id) REFERENCES supervisor.plan_approvals(tenant_id,site_id,id,plan_revision_id),
 FOREIGN KEY(tenant_id,site_id,supersedes_context_id) REFERENCES supervisor.plant_contexts(tenant_id,site_id,id)
);
CREATE TABLE supervisor.plant_context_records (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,context_id uuid NOT NULL,source_id uuid NOT NULL,source_sequence bigint NOT NULL,
 PRIMARY KEY(tenant_id,site_id,context_id,source_id,source_sequence),
 FOREIGN KEY(tenant_id,site_id,context_id) REFERENCES supervisor.plant_contexts(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,source_id,source_sequence) REFERENCES supervisor.operational_event_records(tenant_id,site_id,source_id,source_sequence)
);
-- The current context for one scope; replacing it records the superseded context.
CREATE TABLE supervisor.plant_context_heads (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,order_id uuid NOT NULL,asset_id uuid NOT NULL,
 window_start timestamptz NOT NULL,process_snapshot_id uuid NOT NULL,
 context_id uuid NOT NULL,revision bigint NOT NULL CHECK(revision>0),
 PRIMARY KEY(tenant_id,site_id,order_id,asset_id,window_start,process_snapshot_id),
 FOREIGN KEY(tenant_id,site_id,context_id) REFERENCES supervisor.plant_contexts(tenant_id,site_id,id)
);
CREATE TABLE supervisor.plant_context_outbox (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL DEFAULT gen_random_uuid(),
 context_id uuid NOT NULL,event_type text NOT NULL CHECK(event_type IN ('context.created','context.superseded')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,context_id) REFERENCES supervisor.plant_contexts(tenant_id,site_id,id)
);
CREATE TABLE supervisor.plant_context_receipts (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,actor_issuer text NOT NULL,actor_subject text NOT NULL,
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 context_id uuid NOT NULL,
 PRIMARY KEY(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id,context_id) REFERENCES supervisor.plant_contexts(tenant_id,site_id,id)
);
CREATE TABLE supervisor.plant_context_audit (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),tenant_id uuid NOT NULL,site_id uuid NOT NULL,
 actor_issuer text NOT NULL,actor_subject text NOT NULL,context_id uuid NOT NULL,
 event_type text NOT NULL CHECK(event_type IN ('context.created','context.reused','context.read','mutation.replayed')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY(tenant_id,site_id,context_id) REFERENCES supervisor.plant_contexts(tenant_id,site_id,id)
);

CREATE FUNCTION supervisor.plant_context_head_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE replacement uuid;
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Context head tenant scope denied' USING ERRCODE='42501'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.revision<>1 THEN RAISE EXCEPTION 'Context head must start at one' USING ERRCODE='23514'; END IF;
 ELSE
  IF ROW(NEW.tenant_id,NEW.site_id,NEW.order_id,NEW.asset_id,NEW.window_start,NEW.process_snapshot_id)
     IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.order_id,OLD.asset_id,OLD.window_start,OLD.process_snapshot_id)
   OR NEW.revision<>OLD.revision+1 THEN
   RAISE EXCEPTION 'Invalid context head transition' USING ERRCODE='23514'; END IF;
  -- A replacement must explicitly supersede exactly the context it replaces.
  SELECT supersedes_context_id INTO replacement FROM supervisor.plant_contexts WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND id=NEW.context_id;
  IF replacement IS DISTINCT FROM OLD.context_id THEN
   RAISE EXCEPTION 'Context replacement must supersede the current context' USING ERRCODE='23514'; END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER plant_context_head_guard BEFORE INSERT OR UPDATE ON supervisor.plant_context_heads
 FOR EACH ROW EXECUTE FUNCTION supervisor.plant_context_head_guard();
CREATE FUNCTION supervisor.plant_context_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Context tenant scope denied' USING ERRCODE='42501'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER plant_context_guard BEFORE INSERT ON supervisor.plant_contexts
 FOR EACH ROW EXECUTE FUNCTION supervisor.plant_context_guard();

DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['plant_contexts','plant_context_records','plant_context_outbox','plant_context_receipts','plant_context_audit'] LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t||'_immutable',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['plant_contexts','plant_context_records','plant_context_heads','plant_context_outbox','plant_context_receipts','plant_context_audit'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
  IF t='plant_context_heads' THEN
   EXECUTE format('GRANT UPDATE(context_id,revision) ON supervisor.%I TO supervisor_runtime',t);
  END IF;
 END LOOP;
END $$;
