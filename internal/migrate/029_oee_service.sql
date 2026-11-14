-- WIS-229 OEE service persistence (ADR 0002 Sec. 6). PostgreSQL stores governed OEE state, not
-- owner data: policy revisions and publications, calculation runs, minimized run-source
-- provenance, sealed candidate evidence, idempotency and audit. No source-fact table exists;
-- dataset_id is a logical identity from the request (ADR 0002 Sec. 6.2).
-- Repository conventions applied to the ADR sketch: every row is site-scoped (rule 03),
-- digests are lowercase hex SHA-256 text, actors are (issuer, subject), tenant context is
-- supervisor.tenant_id, and enums are text CHECK constraints.

CREATE TABLE supervisor.oee_policy (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,policy_id uuid NOT NULL,
 policy_code text NOT NULL CHECK(length(policy_code) BETWEEN 1 AND 128),
 current_publication_id uuid,
 row_version bigint NOT NULL DEFAULT 1 CHECK(row_version>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 created_by_issuer text NOT NULL CHECK(length(created_by_issuer) BETWEEN 1 AND 255),
 created_by_subject text NOT NULL CHECK(length(created_by_subject) BETWEEN 1 AND 255),
 PRIMARY KEY(tenant_id,site_id,policy_id),
 UNIQUE(tenant_id,site_id,policy_code),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id)
);

CREATE TABLE supervisor.oee_policy_revision (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,policy_revision_id uuid NOT NULL,policy_id uuid NOT NULL,
 revision_no integer NOT NULL CHECK(revision_no>0),
 schema_version integer NOT NULL CHECK(schema_version=1),
 effective_start timestamptz NOT NULL,
 effective_end timestamptz,
 policy_json jsonb NOT NULL CHECK(jsonb_typeof(policy_json)='object' AND octet_length(policy_json::text)<=65536),
 policy_sha256 text NOT NULL CHECK(policy_sha256 ~ '^[0-9a-f]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 created_by_issuer text NOT NULL CHECK(length(created_by_issuer) BETWEEN 1 AND 255),
 created_by_subject text NOT NULL CHECK(length(created_by_subject) BETWEEN 1 AND 255),
 PRIMARY KEY(tenant_id,site_id,policy_revision_id),
 UNIQUE(tenant_id,site_id,policy_id,policy_revision_id),
 UNIQUE(tenant_id,site_id,policy_id,revision_no),
 UNIQUE(tenant_id,site_id,policy_id,policy_sha256),
 FOREIGN KEY(tenant_id,site_id,policy_id) REFERENCES supervisor.oee_policy(tenant_id,site_id,policy_id),
 CHECK(effective_end IS NULL OR effective_start<effective_end)
);

CREATE TABLE supervisor.oee_policy_publication (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,publication_id uuid NOT NULL,policy_id uuid NOT NULL,
 policy_revision_id uuid NOT NULL,
 publication_no bigint NOT NULL CHECK(publication_no>0),
 action text NOT NULL CHECK(action IN ('PUBLISH','WITHDRAW','ROLLBACK')),
 previous_publication_id uuid,
 published_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 published_by_issuer text NOT NULL CHECK(length(published_by_issuer) BETWEEN 1 AND 255),
 published_by_subject text NOT NULL CHECK(length(published_by_subject) BETWEEN 1 AND 255),
 PRIMARY KEY(tenant_id,site_id,publication_id),
 UNIQUE(tenant_id,site_id,policy_id,publication_no),
 UNIQUE(tenant_id,site_id,policy_id,publication_id),
 CHECK((publication_no=1)=(previous_publication_id IS NULL)),
 FOREIGN KEY(tenant_id,site_id,policy_id,policy_revision_id) REFERENCES supervisor.oee_policy_revision(tenant_id,site_id,policy_id,policy_revision_id),
 FOREIGN KEY(tenant_id,site_id,policy_id,previous_publication_id) REFERENCES supervisor.oee_policy_publication(tenant_id,site_id,policy_id,publication_id)
);

ALTER TABLE supervisor.oee_policy ADD CONSTRAINT oee_policy_current_publication_fk
 FOREIGN KEY(tenant_id,site_id,policy_id,current_publication_id)
 REFERENCES supervisor.oee_policy_publication(tenant_id,site_id,policy_id,publication_id)
 DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE supervisor.oee_calculation_run (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,run_id uuid NOT NULL,
 dataset_id uuid NOT NULL,
 asset_id uuid NOT NULL,
 window_start timestamptz NOT NULL,window_end timestamptz NOT NULL CHECK(window_start<window_end),
 dataset_sha256 text NOT NULL CHECK(dataset_sha256 ~ '^[0-9a-f]{64}$'),
 policy_id uuid NOT NULL,policy_revision_id uuid NOT NULL,
 policy_sha256 text NOT NULL CHECK(policy_sha256 ~ '^[0-9a-f]{64}$'),
 requested_by_issuer text NOT NULL CHECK(length(requested_by_issuer) BETWEEN 1 AND 255),
 requested_by_subject text NOT NULL CHECK(length(requested_by_subject) BETWEEN 1 AND 255),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 status text NOT NULL CHECK(status IN ('ACCEPTED','RUNNING','SUCCEEDED','FAILED','CANCELLED')),
 result_status text CHECK(result_status IN ('READY','LIMITED','NOT_READY','INVALID')),
 truth_state text CHECK(truth_state IN ('CANDIDATE','SUPERSEDED')),
 accepted_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 started_at timestamptz,
 finished_at timestamptz,
 deadline_at timestamptz NOT NULL,
 cancel_requested_at timestamptz,
 cancel_generation bigint NOT NULL DEFAULT 0 CHECK(cancel_generation>=0),
 execution_owner text CHECK(length(execution_owner) BETWEEN 1 AND 128),
 execution_fence uuid,
 execution_lease_until timestamptz,
 failure_code text CHECK(failure_code IN ('DEADLINE_EXCEEDED','LEASE_EXPIRED','CANCELLED','INVALID_DATASET','CALCULATION_FAILED','ACCESS_REVOKED','INTERNAL')),
 row_version bigint NOT NULL DEFAULT 1 CHECK(row_version>0),
 PRIMARY KEY(tenant_id,site_id,run_id),
 FOREIGN KEY(tenant_id,site_id,policy_id,policy_revision_id) REFERENCES supervisor.oee_policy_revision(tenant_id,site_id,policy_id,policy_revision_id),
 CHECK(accepted_at<deadline_at),
 CHECK(started_at IS NULL OR accepted_at<=started_at),
 CHECK(finished_at IS NULL OR accepted_at<=finished_at),
 CHECK(finished_at IS NULL OR started_at IS NULL OR started_at<=finished_at),
 CHECK(status NOT IN ('RUNNING','SUCCEEDED') OR started_at IS NOT NULL),
 CHECK(status IN ('ACCEPTED','RUNNING') OR finished_at IS NOT NULL),
 CHECK(status<>'ACCEPTED' OR (started_at IS NULL AND finished_at IS NULL)),
 CHECK(status<>'RUNNING' OR (execution_owner IS NOT NULL AND execution_fence IS NOT NULL AND execution_lease_until IS NOT NULL)),
 CHECK((status IN ('FAILED','CANCELLED'))=(failure_code IS NOT NULL)),
 CHECK((status='SUCCEEDED' AND result_status IS NOT NULL AND truth_state IS NOT NULL)
    OR (status<>'SUCCEEDED' AND result_status IS NULL AND truth_state IS NULL))
);
CREATE INDEX oee_calculation_run_open ON supervisor.oee_calculation_run(tenant_id,site_id,status) WHERE status IN ('ACCEPTED','RUNNING');

CREATE TABLE supervisor.oee_run_source (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,run_id uuid NOT NULL,
 source_ordinal integer NOT NULL CHECK(source_ordinal>=0),
 source_key text NOT NULL CHECK(length(source_key) BETWEEN 1 AND 128),
 owner_service text NOT NULL CHECK(length(owner_service) BETWEEN 1 AND 128),
 source_revision text NOT NULL CHECK(length(source_revision) BETWEEN 1 AND 128),
 dataset_schema_id text NOT NULL CHECK(length(dataset_schema_id) BETWEEN 1 AND 128),
 consistency_mode text NOT NULL CHECK(consistency_mode IN ('SNAPSHOT','WATERMARKED','NON_REPLAYABLE')),
 snapshot_ref text CHECK(length(snapshot_ref) BETWEEN 1 AND 256),
 watermark_ref text CHECK(length(watermark_ref) BETWEEN 1 AND 256),
 contribution_sha256 text NOT NULL CHECK(contribution_sha256 ~ '^[0-9a-f]{64}$'),
 fact_count bigint NOT NULL CHECK(fact_count>=0),
 PRIMARY KEY(tenant_id,site_id,run_id,source_ordinal),
 UNIQUE(tenant_id,site_id,run_id,source_key),
 FOREIGN KEY(tenant_id,site_id,run_id) REFERENCES supervisor.oee_calculation_run(tenant_id,site_id,run_id),
 CHECK(consistency_mode='NON_REPLAYABLE' OR snapshot_ref IS NOT NULL OR watermark_ref IS NOT NULL)
);

CREATE TABLE supervisor.oee_evidence_pack (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,evidence_id uuid NOT NULL,run_id uuid NOT NULL,
 schema_version integer NOT NULL CHECK(schema_version=1),
 result_status text NOT NULL CHECK(result_status IN ('READY','LIMITED','NOT_READY','INVALID')),
 truth_state text NOT NULL CHECK(truth_state IN ('CANDIDATE','SUPERSEDED')),
 dataset_sha256 text NOT NULL CHECK(dataset_sha256 ~ '^[0-9a-f]{64}$'),
 policy_sha256 text NOT NULL CHECK(policy_sha256 ~ '^[0-9a-f]{64}$'),
 calculation_version text NOT NULL CHECK(length(calculation_version) BETWEEN 1 AND 128),
 evidence_json jsonb NOT NULL CHECK(jsonb_typeof(evidence_json)='object' AND octet_length(evidence_json::text)<=262144),
 evidence_sha256 text NOT NULL CHECK(evidence_sha256 ~ '^[0-9a-f]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 superseded_at timestamptz,
 PRIMARY KEY(tenant_id,site_id,evidence_id),
 UNIQUE(tenant_id,site_id,run_id),
 FOREIGN KEY(tenant_id,site_id,run_id) REFERENCES supervisor.oee_calculation_run(tenant_id,site_id,run_id),
 -- The stored document agrees with the indexed columns.
 CHECK(evidence_json->>'evidence_id'=evidence_id::text AND evidence_json->>'run_id'=run_id::text
   AND evidence_json->>'status'=result_status AND evidence_json->>'truth_state'=truth_state
   AND evidence_json->'digests'->>'dataset'=dataset_sha256 AND evidence_json->'policy'->>'digest'=policy_sha256)
);

-- Internal candidate publication identity for comparison (ADR 0002 Sec. 6.1). Written by the
-- WIS-231 integration; this service only defines the table.
CREATE TABLE supervisor.oee_candidate_publication (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,candidate_id uuid NOT NULL,evidence_id uuid NOT NULL,
 scope_sha256 text NOT NULL CHECK(scope_sha256 ~ '^[0-9a-f]{64}$'),
 published_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,candidate_id),
 UNIQUE(tenant_id,site_id,evidence_id),
 FOREIGN KEY(tenant_id,site_id,evidence_id) REFERENCES supervisor.oee_evidence_pack(tenant_id,site_id,evidence_id)
);

-- Request fingerprint and response replay. The key is scoped to the actor as in every other
-- Supervisor receipt table; request_sha256 binds operation, canonical body and schema version.
CREATE TABLE supervisor.oee_idempotency_key (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 operation text NOT NULL CHECK(operation IN ('policy.revise','policy.publish','calculation.create','calculation.cancel')),
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 run_id uuid,
 response_status integer CHECK(response_status BETWEEN 200 AND 299),
 response_json jsonb CHECK(response_json IS NULL OR (jsonb_typeof(response_json)='object' AND octet_length(response_json::text)<=262144)),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz NOT NULL,
 PRIMARY KEY(tenant_id,site_id,actor_issuer,actor_subject,operation,idempotency_key),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 FOREIGN KEY(tenant_id,site_id,run_id) REFERENCES supervisor.oee_calculation_run(tenant_id,site_id,run_id),
 CHECK((response_status IS NULL)=(response_json IS NULL)),
 CHECK(expires_at>created_at)
);

CREATE TABLE supervisor.oee_audit_entry (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,audit_id uuid NOT NULL DEFAULT gen_random_uuid(),
 occurred_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 action text NOT NULL CHECK(action IN ('policy.revised','policy.published','calculation.accepted','calculation.succeeded','calculation.failed','calculation.cancel_requested','calculation.cancelled','evidence.read','mutation.replayed')),
 resource_type text NOT NULL CHECK(resource_type IN ('policy','calculation','evidence')),
 resource_id uuid,
 before_sha256 text CHECK(before_sha256 ~ '^[0-9a-f]{64}$'),
 after_sha256 text CHECK(after_sha256 ~ '^[0-9a-f]{64}$'),
 reason_code text CHECK(length(reason_code) BETWEEN 1 AND 64),
 correlation_id text NOT NULL CHECK(length(correlation_id) BETWEEN 1 AND 128),
 PRIMARY KEY(tenant_id,site_id,audit_id),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id)
);

-- The policy pointer moves only under optimistic concurrency: identity fixed, row_version
-- advances by exactly one (ADR 0002 Sec. 6.1).
CREATE FUNCTION supervisor.oee_policy_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'OEE policies are immutable' USING ERRCODE='42501'; END IF;
 IF ROW(NEW.tenant_id,NEW.site_id,NEW.policy_id,NEW.policy_code,NEW.created_at,NEW.created_by_issuer,NEW.created_by_subject)
    IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.policy_id,OLD.policy_code,OLD.created_at,OLD.created_by_issuer,OLD.created_by_subject)
    OR NEW.row_version<>OLD.row_version+1 THEN
  RAISE EXCEPTION 'OEE policy pointer may only advance' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER oee_policy_guard BEFORE UPDATE OR DELETE ON supervisor.oee_policy
 FOR EACH ROW EXECUTE FUNCTION supervisor.oee_policy_guard();

-- Run state machine (ADR 0002 Sec. 18): ACCEPTED->RUNNING->SUCCEEDED, ACCEPTED->CANCELLED|FAILED,
-- RUNNING->CANCELLED|FAILED (an expired fenced lease is reaped to FAILED). RUNNING->RUNNING
-- only records a cancel request; the fence never changes. No transition leaves a terminal
-- state. Success is sealed only under the database clock with an unexpired lease and
-- deadline. Request identity never changes.
CREATE FUNCTION supervisor.oee_run_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE now_ts timestamptz := clock_timestamp();
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'OEE runs are immutable' USING ERRCODE='42501'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.status<>'ACCEPTED' OR NEW.cancel_generation<>0 OR NEW.row_version<>1 THEN
   RAISE EXCEPTION 'OEE runs start ACCEPTED' USING ERRCODE='23514'; END IF;
  RETURN NEW;
 END IF;
 IF ROW(NEW.tenant_id,NEW.site_id,NEW.run_id,NEW.dataset_id,NEW.asset_id,NEW.window_start,NEW.window_end,NEW.dataset_sha256,NEW.policy_id,NEW.policy_revision_id,NEW.policy_sha256,NEW.requested_by_issuer,NEW.requested_by_subject,NEW.request_sha256,NEW.accepted_at,NEW.deadline_at)
    IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.run_id,OLD.dataset_id,OLD.asset_id,OLD.window_start,OLD.window_end,OLD.dataset_sha256,OLD.policy_id,OLD.policy_revision_id,OLD.policy_sha256,OLD.requested_by_issuer,OLD.requested_by_subject,OLD.request_sha256,OLD.accepted_at,OLD.deadline_at)
    OR NEW.row_version<>OLD.row_version+1 OR NEW.cancel_generation<OLD.cancel_generation THEN
  RAISE EXCEPTION 'OEE run identity is immutable' USING ERRCODE='23514';
 END IF;
 IF OLD.status IN ('SUCCEEDED','FAILED','CANCELLED') THEN
  RAISE EXCEPTION 'OEE run is terminal' USING ERRCODE='23514';
 END IF;
 IF NOT ((OLD.status='ACCEPTED' AND NEW.status IN ('RUNNING','FAILED','CANCELLED'))
      OR (OLD.status='RUNNING' AND NEW.status IN ('RUNNING','SUCCEEDED','FAILED','CANCELLED'))) THEN
  RAISE EXCEPTION 'OEE run transition % -> % is not allowed',OLD.status,NEW.status USING ERRCODE='23514';
 END IF;
 IF OLD.status='RUNNING' AND NEW.status='RUNNING'
    AND ROW(NEW.execution_owner,NEW.execution_fence,NEW.execution_lease_until) IS DISTINCT FROM ROW(OLD.execution_owner,OLD.execution_fence,OLD.execution_lease_until) THEN
  RAISE EXCEPTION 'OEE run execution fence is fixed' USING ERRCODE='23514';
 END IF;
 IF NEW.status='SUCCEEDED' AND (OLD.execution_lease_until<=now_ts OR OLD.deadline_at<=now_ts
    OR NEW.cancel_generation<>OLD.cancel_generation OR NEW.truth_state<>'CANDIDATE') THEN
  RAISE EXCEPTION 'OEE run cannot seal success' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER oee_run_guard BEFORE INSERT OR UPDATE OR DELETE ON supervisor.oee_calculation_run
 FOR EACH ROW EXECUTE FUNCTION supervisor.oee_run_guard();

-- An idempotency key's request binding is fixed; only its response metadata may be completed.
CREATE FUNCTION supervisor.oee_idempotency_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'OEE idempotency keys are immutable' USING ERRCODE='42501'; END IF;
 IF ROW(NEW.tenant_id,NEW.site_id,NEW.actor_issuer,NEW.actor_subject,NEW.operation,NEW.idempotency_key,NEW.request_sha256,NEW.created_at,NEW.expires_at)
    IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.actor_issuer,OLD.actor_subject,OLD.operation,OLD.idempotency_key,OLD.request_sha256,OLD.created_at,OLD.expires_at)
    OR (OLD.run_id IS NOT NULL AND NEW.run_id IS DISTINCT FROM OLD.run_id) THEN
  RAISE EXCEPTION 'OEE idempotency binding is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER oee_idempotency_guard BEFORE UPDATE OR DELETE ON supervisor.oee_idempotency_key
 FOR EACH ROW EXECUTE FUNCTION supervisor.oee_idempotency_guard();

DO $$ DECLARE t text; BEGIN
 -- Append-only tables (ADR 0002 Sec. 6.2).
 FOREACH t IN ARRAY ARRAY['oee_policy_revision','oee_policy_publication','oee_run_source','oee_evidence_pack','oee_candidate_publication','oee_audit_entry'] LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t||'_immutable',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['oee_policy','oee_policy_revision','oee_policy_publication','oee_calculation_run','oee_run_source','oee_evidence_pack','oee_candidate_publication','oee_idempotency_key','oee_audit_entry'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP;
 GRANT UPDATE(current_publication_id,row_version) ON supervisor.oee_policy TO supervisor_runtime;
 GRANT UPDATE(status,result_status,truth_state,started_at,finished_at,cancel_requested_at,cancel_generation,execution_owner,execution_fence,execution_lease_until,failure_code,row_version) ON supervisor.oee_calculation_run TO supervisor_runtime;
 GRANT UPDATE(run_id,response_status,response_json) ON supervisor.oee_idempotency_key TO supervisor_runtime;
END $$;
