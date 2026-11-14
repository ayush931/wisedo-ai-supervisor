-- WIS-520 operator station records. An operator records lifecycle events, quantities,
-- cycle times, quality measurements and recorded dispositions, material exceptions and
-- completion evidence against the job order an approved shift plan delivers to the
-- workstation. Only an explicit, revocable station authorization lets a member record.
-- Records are immutable; a correction appends a new record that names its exact
-- predecessor, so history is never rewritten and a correction chain never branches.

-- Grants and revocations append per (workstation, grantee) with contiguous revisions that
-- alternate grant/revoke; the latest row is the authorization in force.
CREATE TABLE supervisor.station_authorizations (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,workstation_id uuid NOT NULL,
 grantee_issuer text NOT NULL CHECK(length(grantee_issuer) BETWEEN 1 AND 255),
 grantee_subject text NOT NULL CHECK(length(grantee_subject) BETWEEN 1 AND 255),
 revision bigint NOT NULL CHECK(revision>0),
 action text NOT NULL CHECK(action IN ('grant','revoke')),
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,workstation_id,grantee_issuer,grantee_subject,revision),
 -- Only a configured workstation (one with a station profile) can be authorized.
 FOREIGN KEY(tenant_id,site_id,workstation_id) REFERENCES supervisor.station_profile_heads(tenant_id,site_id,workstation_id)
);
CREATE TABLE supervisor.station_authorization_receipts (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,actor_issuer text NOT NULL,actor_subject text NOT NULL,
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 workstation_id uuid NOT NULL,grantee_issuer text NOT NULL,grantee_subject text NOT NULL,revision bigint NOT NULL,
 PRIMARY KEY(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id,workstation_id,grantee_issuer,grantee_subject,revision) REFERENCES supervisor.station_authorizations(tenant_id,site_id,workstation_id,grantee_issuer,grantee_subject,revision)
);

-- A record names the plan revision in force at its event time (the latest approval of the
-- shift containing event_at that was approved by then), the workstation and job-order
-- revision that revision assigned, and the station profile revision current at event_at.
-- The assignment and approval foreign keys make a record against an unapproved plan
-- revision, or a job order it did not assign to this workstation, unrepresentable. A record
-- whose binding was no longer the live one when it arrived is stored with late=true.
ALTER TABLE supervisor.shift_plan_assignments ADD CONSTRAINT shift_plan_assignments_record_key UNIQUE(tenant_id,site_id,plan_id,revision,workstation_id,job_order_id,job_order_revision);
CREATE TABLE supervisor.station_records (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL,
 workstation_id uuid NOT NULL,job_order_id uuid NOT NULL,job_order_revision bigint NOT NULL,
 plan_id uuid NOT NULL,plan_revision bigint NOT NULL,profile_revision bigint NOT NULL,
 kind text NOT NULL CHECK(kind IN ('acknowledged','started','paused','blocked','resumed','quantity','cycle_time','quality_measurement','quality_disposition_recorded','material_exception','completion_evidence','note')),
 -- Event (source) time is what the operator states; received time is the server clock.
 event_at timestamptz NOT NULL,
 received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 -- Manual tablet entry is the only qualified source; an adapter widens this by migration.
 source text NOT NULL CHECK(source='manual_tablet'),
 late boolean NOT NULL,
 corrects_record_id uuid,
 payload jsonb NOT NULL,
 canonical_payload text NOT NULL CHECK(octet_length(canonical_payload) BETWEEN 1 AND 1048576),
 canonical_payload_sha256 text NOT NULL CHECK(canonical_payload_sha256=encode(sha256(convert_to(canonical_payload,'UTF8')),'hex')),
 digest_schema_version integer NOT NULL CHECK(digest_schema_version=1),
 -- A tablet clock may run five minutes ahead; a late entry is at most seven days old.
 CHECK(event_at<=received_at+interval '5 minutes'),
 CONSTRAINT station_record_max_age CHECK(event_at>=received_at-interval '7 days'),
 CONSTRAINT station_record_not_self CHECK(corrects_record_id<>id),
 -- The digest body is the stored payload, bound to this schema and scope; the indexed
 -- columns are the payload's own members.
 CHECK(canonical_payload::jsonb->>'schema'='StationRecordBodyV1' AND (canonical_payload::jsonb->>'schema_version')::integer=digest_schema_version),
 CHECK(canonical_payload::jsonb->>'tenant_id'=tenant_id::text AND canonical_payload::jsonb->>'site_id'=site_id::text),
 CHECK(canonical_payload::jsonb->'body'=payload AND (payload->>'schema_version')::integer=1 AND payload->>'kind'=kind AND payload->>'workstation_id'=workstation_id::text
  AND payload->>'job_order_id'=job_order_id::text AND (payload->>'job_order_revision')::bigint=job_order_revision AND (payload->>'event_at')::timestamptz=event_at
  AND payload->>'corrects_record_id' IS NOT DISTINCT FROM corrects_record_id::text),
 -- A quality record states what the operator observed; it never releases material.
 CONSTRAINT station_record_never_releases CHECK((kind='quality_disposition_recorded')=(payload ? 'authorizes_release') AND coalesce(payload->'authorizes_release','false'::jsonb)='false'::jsonb),
 -- Lifecycle events are facts of sequence and are never corrected.
 CHECK(corrects_record_id IS NULL OR kind IN ('quantity','cycle_time','quality_measurement','quality_disposition_recorded','material_exception','completion_evidence','note')),
 PRIMARY KEY(tenant_id,site_id,id),
 UNIQUE(tenant_id,site_id,workstation_id,job_order_id,job_order_revision,plan_id,plan_revision,kind,id),
 -- One correction per record: the chain never branches and only its latest record can be
 -- corrected. The predecessor has the same workstation, job-order and plan revision and kind.
 CONSTRAINT station_record_single_correction UNIQUE(tenant_id,site_id,corrects_record_id),
 CONSTRAINT station_record_correction_fk FOREIGN KEY(tenant_id,site_id,workstation_id,job_order_id,job_order_revision,plan_id,plan_revision,kind,corrects_record_id)
  REFERENCES supervisor.station_records(tenant_id,site_id,workstation_id,job_order_id,job_order_revision,plan_id,plan_revision,kind,id),
 CONSTRAINT station_record_assignment_fk FOREIGN KEY(tenant_id,site_id,plan_id,plan_revision,workstation_id,job_order_id,job_order_revision)
  REFERENCES supervisor.shift_plan_assignments(tenant_id,site_id,plan_id,revision,workstation_id,job_order_id,job_order_revision),
 FOREIGN KEY(tenant_id,site_id,plan_id,plan_revision) REFERENCES supervisor.shift_plan_approvals(tenant_id,site_id,plan_id,revision),
 FOREIGN KEY(tenant_id,site_id,workstation_id,profile_revision) REFERENCES supervisor.station_profile_revisions(tenant_id,site_id,workstation_id,revision)
);
CREATE INDEX station_records_workstation ON supervisor.station_records(tenant_id,site_id,workstation_id,received_at,id);
CREATE INDEX station_records_lifecycle ON supervisor.station_records(tenant_id,site_id,workstation_id,job_order_id,plan_id,event_at);

-- The event happened inside the shift of the plan revision it is bound to.
CREATE FUNCTION supervisor.station_record_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Station record tenant scope denied' USING ERRCODE='42501'; END IF;
 IF NOT EXISTS(SELECT 1 FROM supervisor.shift_plan_revisions WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND plan_id=NEW.plan_id AND revision=NEW.plan_revision
  AND starts_at<=NEW.event_at AND NEW.event_at<ends_at) THEN
  RAISE EXCEPTION 'Station record event is outside its shift' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER station_record_guard BEFORE INSERT ON supervisor.station_records
 FOR EACH ROW EXECUTE FUNCTION supervisor.station_record_guard();
CREATE TABLE supervisor.station_record_receipts (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,actor_issuer text NOT NULL,actor_subject text NOT NULL,
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 record_id uuid NOT NULL,
 PRIMARY KEY(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id,record_id) REFERENCES supervisor.station_records(tenant_id,site_id,id)
);
CREATE TABLE supervisor.station_record_audit (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),tenant_id uuid NOT NULL,site_id uuid NOT NULL,
 actor_issuer text NOT NULL,actor_subject text NOT NULL,workstation_id uuid NOT NULL,record_id uuid,
 event_type text NOT NULL CHECK(event_type IN ('station_record.submitted','station_record.read','station_record.listed','station_authorization.granted','station_authorization.revoked','station_authorization.listed','mutation.replayed')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((event_type IN ('station_record.submitted','station_record.read'))<=(record_id IS NOT NULL)),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 FOREIGN KEY(tenant_id,site_id,record_id) REFERENCES supervisor.station_records(tenant_id,site_id,id)
);

-- Authorization revisions are contiguous per grantee and alternate, starting with a grant.
CREATE FUNCTION supervisor.station_authorization_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE last_revision bigint; last_action text;
BEGIN
 IF current_setting('supervisor.tenant_id',true) IS DISTINCT FROM NEW.tenant_id::text THEN
  RAISE EXCEPTION 'Station authorization tenant scope denied' USING ERRCODE='42501'; END IF;
 SELECT revision,action INTO last_revision,last_action FROM supervisor.station_authorizations
  WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND workstation_id=NEW.workstation_id AND grantee_issuer=NEW.grantee_issuer AND grantee_subject=NEW.grantee_subject
  ORDER BY revision DESC LIMIT 1;
 IF NEW.revision<>coalesce(last_revision,0)+1 OR NEW.action=coalesce(last_action,'revoke') THEN
  RAISE EXCEPTION 'Invalid station authorization transition' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER station_authorization_guard BEFORE INSERT ON supervisor.station_authorizations
 FOR EACH ROW EXECUTE FUNCTION supervisor.station_authorization_guard();

DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['station_authorizations','station_authorization_receipts','station_records','station_record_receipts','station_record_audit'] LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t||'_immutable',t);
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP;
END $$;
