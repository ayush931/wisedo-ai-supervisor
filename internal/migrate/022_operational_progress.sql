-- Durable outbox consumption projection: per-source processed cursor, completeness and liveness.
-- Outbox rows stay immutable; progress is a guarded cursor plus an immutable revision history.
CREATE TABLE supervisor.operational_source_progress_heads (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, source_id uuid NOT NULL,
 processed_sequence bigint NOT NULL CHECK(processed_sequence>=0),
 current_revision bigint NOT NULL CHECK(current_revision>0),
 PRIMARY KEY(tenant_id,site_id,source_id),
 FOREIGN KEY(tenant_id,site_id,source_id) REFERENCES supervisor.operational_event_sequence_heads(tenant_id,site_id,source_id)
);
CREATE TABLE supervisor.operational_source_progress_revisions (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, source_id uuid NOT NULL,
 revision bigint NOT NULL CHECK(revision>0),
 processed_sequence bigint NOT NULL CHECK(processed_sequence>=0),
 complete_sequence bigint NOT NULL CHECK(complete_sequence BETWEEN 0 AND processed_sequence),
 quarantined_count integer NOT NULL CHECK(quarantined_count>=0),
 first_quarantined_sequence bigint CHECK((quarantined_count=0)=(first_quarantined_sequence IS NULL)),
 last_event_at timestamptz, last_event_received_at timestamptz,
 last_heartbeat_event_at timestamptz, last_heartbeat_received_at timestamptz,
 clock_quality text NOT NULL CHECK(clock_quality IN ('unknown','ok','suspect')),
 watermark_sequence bigint CHECK(watermark_sequence IS NULL OR watermark_sequence<=complete_sequence),
 watermark_event_at timestamptz,
 folded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,source_id,revision),
 FOREIGN KEY(tenant_id,site_id,source_id) REFERENCES supervisor.operational_source_progress_heads(tenant_id,site_id,source_id),
 CHECK((last_event_at IS NULL)=(last_event_received_at IS NULL)),
 CHECK((last_heartbeat_event_at IS NULL)=(last_heartbeat_received_at IS NULL)),
 CHECK((watermark_sequence IS NULL)=(watermark_event_at IS NULL)),
 CHECK(clock_quality='unknown' OR last_heartbeat_event_at IS NOT NULL)
);
ALTER TABLE supervisor.operational_source_progress_heads ADD FOREIGN KEY(tenant_id,site_id,source_id,current_revision)
 REFERENCES supervisor.operational_source_progress_revisions(tenant_id,site_id,source_id,revision) DEFERRABLE INITIALLY DEFERRED;
CREATE FUNCTION supervisor.operational_progress_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN RAISE EXCEPTION 'Progress tenant scope denied' USING ERRCODE='42501'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.processed_sequence<>0 OR NEW.current_revision<>1 THEN RAISE EXCEPTION 'Progress must start at zero' USING ERRCODE='23514'; END IF;
 ELSIF ROW(NEW.tenant_id,NEW.site_id,NEW.source_id) IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.source_id) OR NEW.current_revision<>OLD.current_revision+1 OR NEW.processed_sequence<=OLD.processed_sequence OR (SELECT count(*) FROM supervisor.operational_event_outbox o WHERE (o.tenant_id,o.site_id,o.source_id)=(OLD.tenant_id,OLD.site_id,OLD.source_id) AND o.source_sequence>OLD.processed_sequence AND o.source_sequence<=NEW.processed_sequence)<>NEW.processed_sequence-OLD.processed_sequence THEN
  RAISE EXCEPTION 'Progress must advance one revision over a contiguous durable outbox range' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER operational_progress_guard BEFORE INSERT OR UPDATE ON supervisor.operational_source_progress_heads FOR EACH ROW EXECUTE FUNCTION supervisor.operational_progress_guard();
CREATE TRIGGER operational_source_progress_revision_immutable BEFORE UPDATE OR DELETE ON supervisor.operational_source_progress_revisions FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['operational_source_progress_heads','operational_source_progress_revisions'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP;
 GRANT UPDATE(processed_sequence,current_revision) ON supervisor.operational_source_progress_heads TO supervisor_runtime;
END $$;
