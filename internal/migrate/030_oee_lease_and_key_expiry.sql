-- WIS-229 follow-up to 029 (which is applied and checksummed, so it is not edited).
--
-- 1. Lease renewal. The executor holds a short lease that it renews while it works, so a run
--    abandoned by a crashed or restarted process is reclaimable after one lease instead of
--    holding capacity until its deadline (ADR 0002 Sec. 7.2, 18). Only the same owner and fence
--    may extend an unexpired lease, forward only, and never past the run deadline.
-- 2. Idempotency key expiry. A key whose expires_at has passed may be rebound to a new request,
--    so the configured retention is honoured. Physical purge stays with the retention service
--    (WIS-290); until then an expired row is reused rather than deleted.

CREATE OR REPLACE FUNCTION supervisor.oee_run_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
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
 IF OLD.status='RUNNING' AND NEW.status='RUNNING' THEN
  -- Owner and fence are fixed for the life of a claim.
  IF ROW(NEW.execution_owner,NEW.execution_fence) IS DISTINCT FROM ROW(OLD.execution_owner,OLD.execution_fence) THEN
   RAISE EXCEPTION 'OEE run execution fence is fixed' USING ERRCODE='23514';
  END IF;
  -- A lease change is a renewal: of a still-held lease, forward, capped at the deadline.
  IF NEW.execution_lease_until IS DISTINCT FROM OLD.execution_lease_until
     AND (OLD.execution_lease_until<=now_ts OR NEW.execution_lease_until<=OLD.execution_lease_until OR NEW.execution_lease_until>OLD.deadline_at) THEN
   RAISE EXCEPTION 'OEE run lease may only be renewed forward while held' USING ERRCODE='23514';
  END IF;
 END IF;
 IF NEW.status='SUCCEEDED' AND (OLD.execution_lease_until<=now_ts OR OLD.deadline_at<=now_ts
    OR NEW.cancel_generation<>OLD.cancel_generation OR NEW.truth_state<>'CANDIDATE') THEN
  RAISE EXCEPTION 'OEE run cannot seal success' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION supervisor.oee_idempotency_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'OEE idempotency keys are immutable' USING ERRCODE='42501'; END IF;
 IF ROW(NEW.tenant_id,NEW.site_id,NEW.actor_issuer,NEW.actor_subject,NEW.operation,NEW.idempotency_key)
    IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.actor_issuer,OLD.actor_subject,OLD.operation,OLD.idempotency_key) THEN
  RAISE EXCEPTION 'OEE idempotency key identity is immutable' USING ERRCODE='23514';
 END IF;
 -- An expired key may be rebound to a new request with a fresh lifetime.
 IF OLD.expires_at<=clock_timestamp() THEN
  IF NEW.expires_at<=clock_timestamp() OR NEW.created_at<OLD.expires_at THEN
   RAISE EXCEPTION 'OEE idempotency key rebinding needs a fresh lifetime' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
 END IF;
 -- A live key keeps its request binding; only its response metadata may be completed once.
 IF ROW(NEW.request_sha256,NEW.created_at,NEW.expires_at) IS DISTINCT FROM ROW(OLD.request_sha256,OLD.created_at,OLD.expires_at)
    OR (OLD.run_id IS NOT NULL AND NEW.run_id IS DISTINCT FROM OLD.run_id) THEN
  RAISE EXCEPTION 'OEE idempotency binding is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;

GRANT UPDATE(request_sha256,created_at,expires_at) ON supervisor.oee_idempotency_key TO supervisor_runtime;
