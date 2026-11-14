-- WIS-275 agent runtime persistence (design §4 and §6). One run is one bounded investigation
-- cycle (request_kind=case) or one chat request (request_kind=conversation, WIS-517). The run row
-- holds the remaining budget; reservations decrement it in one statement before a provider call
-- and charges refund the unused part afterwards, so the counters bound the totals across retries.
-- Prompts and raw completions are never stored: calls keep hashes, versions, token counts and
-- reason codes; agent_artifacts keeps only output that passed strict decoding.

CREATE TABLE supervisor.agent_runs (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL,
 request_kind text NOT NULL CHECK(request_kind IN ('case','conversation')),
 case_id uuid,case_revision bigint CHECK(case_revision>0),
 conversation_id uuid,request_id uuid,
 state text NOT NULL DEFAULT 'running' CHECK(state IN ('running','completed','degraded','canceled','failed')),
 trace_id text NOT NULL CHECK(trace_id ~ '^[0-9a-f]{32}$'),
 provider_name text NOT NULL CHECK(length(provider_name) BETWEEN 1 AND 64),
 provider_actual boolean NOT NULL,
 model text NOT NULL CHECK(length(model) BETWEEN 1 AND 255),
 model_revision text NOT NULL CHECK(length(model_revision) BETWEEN 1 AND 255),
 prompt_version text NOT NULL CHECK(length(prompt_version) BETWEEN 1 AND 64),
 tool_version text NOT NULL CHECK(length(tool_version) BETWEEN 1 AND 64),
 policy_revision bigint NOT NULL CHECK(policy_revision>0),
 invocations_left integer NOT NULL CHECK(invocations_left>=0),
 tool_calls_left integer NOT NULL CHECK(tool_calls_left>=0),
 requests_left integer NOT NULL CHECK(requests_left>=0),
 tokens_left bigint NOT NULL CHECK(tokens_left>=0),
 cost_left_microusd bigint NOT NULL CHECK(cost_left_microusd>=0),
 outcome_status text CHECK(outcome_status IN ('proposed','needs_input','refused','escalated')),
 outcome_reasons text[] NOT NULL DEFAULT '{}' CHECK(cardinality(outcome_reasons)<=32),
 artifact_id uuid,proposal_id uuid,
 revoked_after_dispatch boolean NOT NULL DEFAULT false,
 revocation_reason text CHECK(revocation_reason ~ '^[A-Z][A-Z0-9_]{0,63}$'),
 started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 ended_at timestamptz,
 -- A case run is bound to one case revision; a conversation run never fabricates a case.
 CHECK((request_kind='case' AND case_id IS NOT NULL AND case_revision IS NOT NULL AND conversation_id IS NULL AND request_id IS NULL)
    OR (request_kind='conversation' AND case_id IS NULL AND case_revision IS NULL AND conversation_id IS NOT NULL AND request_id IS NOT NULL)),
 CHECK((state='running')=(ended_at IS NULL)),
 CHECK(ended_at IS NULL OR ended_at>=started_at),
 PRIMARY KEY(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 FOREIGN KEY(tenant_id,site_id,case_id) REFERENCES supervisor.cases(tenant_id,site_id,id)
);
CREATE UNIQUE INDEX agent_runs_one_per_case_revision ON supervisor.agent_runs(tenant_id,site_id,case_id,case_revision) WHERE request_kind='case';
CREATE UNIQUE INDEX agent_runs_one_per_conversation_request ON supervisor.agent_runs(tenant_id,site_id,conversation_id,request_id) WHERE request_kind='conversation';

-- The scope captured at run start: tools may only read these refs at these revisions.
CREATE TABLE supervisor.agent_run_refs (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,run_id uuid NOT NULL,
 kind text NOT NULL CHECK(kind IN ('context','measurement','snapshot','evidence')),
 ref_id text NOT NULL CHECK(length(ref_id) BETWEEN 1 AND 255),
 revision bigint NOT NULL CHECK(revision>0),
 sha256 text NOT NULL CHECK(sha256 ~ '^[0-9a-f]{64}$'),
 access_epoch bigint CHECK(access_epoch>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,run_id,kind,ref_id),
 FOREIGN KEY(tenant_id,site_id,run_id) REFERENCES supervisor.agent_runs(tenant_id,site_id,id)
);

CREATE TABLE supervisor.agent_steps (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,run_id uuid NOT NULL,
 seq integer NOT NULL CHECK(seq>0),
 span_id text NOT NULL CHECK(span_id ~ '^[0-9a-f]{16}$'),
 parent_span_id text CHECK(parent_span_id ~ '^[0-9a-f]{16}$'),
 role text NOT NULL CHECK(role IN ('discovery/mapping','investigation','proposal','feedback','orchestrator','executor')),
 hop integer NOT NULL CHECK(hop BETWEEN 0 AND 1),
 envelope_sha256 text NOT NULL CHECK(envelope_sha256 ~ '^[0-9a-f]{64}$'),
 status text NOT NULL CHECK(status IN ('completed','failed','canceled','rejected')),
 result_status text CHECK(result_status IN ('success','partial','needs_input','refused','failed')),
 reason_code text CHECK(reason_code ~ '^[A-Z][A-Z0-9_]{0,63}$'),
 started_at timestamptz NOT NULL,ended_at timestamptz NOT NULL CHECK(ended_at>=started_at),
 PRIMARY KEY(tenant_id,site_id,run_id,seq),
 UNIQUE(tenant_id,site_id,run_id,span_id),
 FOREIGN KEY(tenant_id,site_id,run_id) REFERENCES supervisor.agent_runs(tenant_id,site_id,id)
);

-- One row per provider request attempt. It is inserted 'reserved' in the transaction that
-- reserves budget, and settled exactly once by the charge.
CREATE TABLE supervisor.agent_model_calls (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,run_id uuid NOT NULL,
 seq integer NOT NULL CHECK(seq>0),attempt integer NOT NULL CHECK(attempt BETWEEN 1 AND 2),
 span_id text NOT NULL CHECK(span_id ~ '^[0-9a-f]{16}$'),
 parent_span_id text CHECK(parent_span_id ~ '^[0-9a-f]{16}$'),
 provider_actual boolean NOT NULL,
 model text NOT NULL CHECK(length(model) BETWEEN 1 AND 255),
 model_revision text NOT NULL CHECK(length(model_revision) BETWEEN 1 AND 255),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 response_sha256 text CHECK(response_sha256 ~ '^[0-9a-f]{64}$'),
 reserved_tokens bigint NOT NULL CHECK(reserved_tokens>0),
 reserved_microusd bigint NOT NULL CHECK(reserved_microusd>=0),
 input_tokens bigint CHECK(input_tokens>=0),output_tokens bigint CHECK(output_tokens>=0),
 wall_ms bigint CHECK(wall_ms>=0),
 cost_basis text NOT NULL CHECK(cost_basis IN ('local','priced')),
 charged_microusd bigint CHECK(charged_microusd>=0),
 class text CHECK(class ~ '^[a-z][a-z0-9_]{0,31}$'),
 http_status integer CHECK(http_status BETWEEN 100 AND 599),
 status text NOT NULL DEFAULT 'reserved' CHECK(status IN ('reserved','completed','failed','discarded')),
 reason_code text CHECK(reason_code ~ '^[A-Z][A-Z0-9_]{0,63}$'),
 started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 ended_at timestamptz,
 CHECK((status='reserved')=(ended_at IS NULL)),
 CHECK(cost_basis='priced' OR charged_microusd IS NULL OR charged_microusd=0),
 PRIMARY KEY(tenant_id,site_id,run_id,seq,attempt),
 UNIQUE(tenant_id,site_id,run_id,span_id),
 FOREIGN KEY(tenant_id,site_id,run_id) REFERENCES supervisor.agent_runs(tenant_id,site_id,id)
);

CREATE TABLE supervisor.agent_tool_calls (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,run_id uuid NOT NULL,
 seq integer NOT NULL CHECK(seq>0),
 span_id text NOT NULL CHECK(span_id ~ '^[0-9a-f]{16}$'),
 parent_span_id text CHECK(parent_span_id ~ '^[0-9a-f]{16}$'),
 tool text NOT NULL CHECK(length(tool) BETWEEN 1 AND 64),
 tool_version text NOT NULL CHECK(length(tool_version) BETWEEN 1 AND 64),
 args_sha256 text NOT NULL CHECK(args_sha256 ~ '^[0-9a-f]{64}$'),
 ref_ids text[] NOT NULL DEFAULT '{}' CHECK(cardinality(ref_ids)<=64),
 ref_revisions bigint[] NOT NULL DEFAULT '{}' CHECK(cardinality(ref_revisions)=cardinality(ref_ids)),
 result_sha256 text CHECK(result_sha256 ~ '^[0-9a-f]{64}$'),
 result_bytes integer CHECK(result_bytes>=0),
 status text NOT NULL CHECK(status IN ('ok','rejected','unavailable','canceled','failed')),
 code text CHECK(code ~ '^[A-Z][A-Z0-9_]{0,63}$'),
 started_at timestamptz NOT NULL,ended_at timestamptz NOT NULL CHECK(ended_at>=started_at),
 CHECK((status='ok')=(code IS NULL)),
 PRIMARY KEY(tenant_id,site_id,run_id,seq),
 UNIQUE(tenant_id,site_id,run_id,span_id),
 FOREIGN KEY(tenant_id,site_id,run_id) REFERENCES supervisor.agent_runs(tenant_id,site_id,id)
);

CREATE TABLE supervisor.agent_artifacts (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL,run_id uuid NOT NULL,
 schema text NOT NULL CHECK(length(schema) BETWEEN 1 AND 64),
 body jsonb NOT NULL CHECK(octet_length(body::text)<=1048576),
 sha256 text NOT NULL CHECK(sha256 ~ '^[0-9a-f]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,id),
 UNIQUE(tenant_id,site_id,run_id,id),
 FOREIGN KEY(tenant_id,site_id,run_id) REFERENCES supervisor.agent_runs(tenant_id,site_id,id)
);
-- The run's artifact must belong to that run. Added before RLS is forced, on empty new tables.
ALTER TABLE supervisor.agent_runs ADD FOREIGN KEY(tenant_id,site_id,id,artifact_id)
 REFERENCES supervisor.agent_artifacts(tenant_id,site_id,run_id,id);

-- A run's identity and limits are fixed; only a running run changes, and it changes once into
-- a terminal state. Counters cannot go below zero (CHECKs above).
CREATE FUNCTION supervisor.agent_run_guard() RETURNS trigger LANGUAGE plpgsql
 SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN
  -- Only the owning (retention) role may purge, as with evidence_immutable.
  IF pg_has_role(current_user,(SELECT relowner FROM pg_class WHERE oid=TG_RELID),'MEMBER') THEN RETURN OLD; END IF;
  RAISE EXCEPTION 'agent runs are immutable history' USING ERRCODE='42501';
 END IF;
 IF OLD.state<>'running' THEN RAISE EXCEPTION 'agent run % is terminal',OLD.id USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER agent_run_guard BEFORE UPDATE OR DELETE ON supervisor.agent_runs
 FOR EACH ROW EXECUTE FUNCTION supervisor.agent_run_guard();

-- A model call is settled once: reserved -> completed | failed | discarded.
CREATE FUNCTION supervisor.agent_model_call_guard() RETURNS trigger LANGUAGE plpgsql
 SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN
  -- Only the owning (retention) role may purge, as with evidence_immutable.
  IF pg_has_role(current_user,(SELECT relowner FROM pg_class WHERE oid=TG_RELID),'MEMBER') THEN RETURN OLD; END IF;
  RAISE EXCEPTION 'agent model calls are immutable history' USING ERRCODE='42501';
 END IF;
 IF OLD.status<>'reserved' THEN RAISE EXCEPTION 'agent model call is already settled' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER agent_model_call_guard BEFORE UPDATE OR DELETE ON supervisor.agent_model_calls
 FOR EACH ROW EXECUTE FUNCTION supervisor.agent_model_call_guard();

DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['agent_run_refs','agent_steps','agent_tool_calls','agent_artifacts'] LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t||'_immutable',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['agent_runs','agent_run_refs','agent_steps','agent_model_calls','agent_tool_calls','agent_artifacts'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP;
 -- 044 grants only SELECT and INSERT; the run and call rows also carry mutable counters and status.
 GRANT UPDATE(state,invocations_left,tool_calls_left,requests_left,tokens_left,cost_left_microusd,outcome_status,outcome_reasons,artifact_id,proposal_id,revoked_after_dispatch,revocation_reason,ended_at) ON supervisor.agent_runs TO supervisor_runtime;
 GRANT UPDATE(response_sha256,input_tokens,output_tokens,wall_ms,charged_microusd,class,http_status,status,reason_code,ended_at) ON supervisor.agent_model_calls TO supervisor_runtime;
END $$;
