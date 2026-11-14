-- WIS-274 durable approvals (rule 05). "Tables proposal_revision, approval_decision,
-- approval_state, idempotency_record and append-only audit are tenant-scoped. Composite FKs
-- prevent foreign-tenant attachment."
--
-- A proposal has immutable revisions. Each revision stores the server-built canonical
-- ApprovedActionV1 envelope and its SHA-256, which the database re-checks. approval_states is
-- the per-proposal head: the current revision, its lifecycle state and a state revision that
-- advances on every change and is the If-Match token for decisions. approval_transitions is the
-- append-only history of every state change, so a superseded or expired revision keeps its
-- final state after the head moves on.

CREATE TABLE supervisor.proposal_revisions (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,proposal_id uuid NOT NULL,
 revision bigint NOT NULL CHECK(revision>0),
 case_id uuid NOT NULL,case_revision bigint NOT NULL CHECK(case_revision>0),
 context_snapshot_id uuid NOT NULL,
 generated_by_run_id uuid NOT NULL,
 -- The human who explicitly submitted or edited this revision, if any; a model-generated
 -- revision has none. Either both are set or neither.
 submitted_by_issuer text CHECK(length(submitted_by_issuer) BETWEEN 1 AND 255),
 submitted_by_subject text CHECK(length(submitted_by_subject) BETWEEN 1 AND 255),
 issued_at timestamptz NOT NULL,expires_at timestamptz NOT NULL CHECK(expires_at>issued_at),
 input_sha256 text NOT NULL CHECK(input_sha256 ~ '^[0-9a-f]{64}$'),
 canonical_payload text NOT NULL CHECK(octet_length(canonical_payload) BETWEEN 1 AND 65536),
 canonical_payload_sha256 text NOT NULL CHECK(canonical_payload_sha256=encode(sha256(convert_to(canonical_payload,'UTF8')),'hex')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((submitted_by_issuer IS NULL)=(submitted_by_subject IS NULL)),
 -- The digest body is bound to this row: same scope, same proposal identity and revision.
 CHECK(canonical_payload::jsonb->>'schema'='ApprovedActionV1' AND (canonical_payload::jsonb->>'schema_version')::integer=1),
 CHECK(canonical_payload::jsonb->>'tenant_id'=tenant_id::text AND canonical_payload::jsonb->>'site_id'=site_id::text),
 CHECK(canonical_payload::jsonb->'body'->>'proposal_id'=proposal_id::text AND (canonical_payload::jsonb->'body'->>'proposal_revision')::bigint=revision),
 CHECK(canonical_payload::jsonb->'body'->>'case_id'=case_id::text AND (canonical_payload::jsonb->'body'->>'case_revision')::bigint=case_revision),
 CHECK(canonical_payload::jsonb->'body'->>'context_snapshot_id'=context_snapshot_id::text AND canonical_payload::jsonb->'body'->>'generated_by_run_id'=generated_by_run_id::text),
 PRIMARY KEY(tenant_id,site_id,proposal_id,revision),
 UNIQUE(tenant_id,site_id,proposal_id,revision,canonical_payload_sha256),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 FOREIGN KEY(tenant_id,site_id,case_id) REFERENCES supervisor.cases(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,context_snapshot_id) REFERENCES supervisor.plant_contexts(tenant_id,site_id,id)
);

CREATE TABLE supervisor.approval_states (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,proposal_id uuid NOT NULL,
 current_revision bigint NOT NULL CHECK(current_revision>0),
 state text NOT NULL CHECK(state IN ('pending','approved','rejected','expired','superseded','consumed','revoked','stale')),
 state_revision bigint NOT NULL CHECK(state_revision>0),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,proposal_id),
 FOREIGN KEY(tenant_id,site_id,proposal_id,current_revision) REFERENCES supervisor.proposal_revisions(tenant_id,site_id,proposal_id,revision)
);

-- One immutable row per human decision: an approval, a rejection or the revocation of an
-- approval. A reviewer can approve or reject a proposal revision only once (the spec's
-- UNIQUE(proposal_revision, reviewer)); revocation is not a review and is not counted.
CREATE TABLE supervisor.approval_decisions (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL,
 proposal_id uuid NOT NULL,proposal_revision bigint NOT NULL,
 canonical_payload_sha256 text NOT NULL CHECK(canonical_payload_sha256 ~ '^[0-9a-f]{64}$'),
 decided_state_revision bigint NOT NULL CHECK(decided_state_revision>0),
 decision text NOT NULL CHECK(decision IN ('approve','reject','revoke')),
 reason text NOT NULL CHECK(length(reason) BETWEEN 1 AND 2000 AND length(btrim(reason))>0),
 reviewer_issuer text NOT NULL CHECK(length(reviewer_issuer) BETWEEN 1 AND 255),
 reviewer_subject text NOT NULL CHECK(length(reviewer_subject) BETWEEN 1 AND 255),
 auth_time timestamptz NOT NULL,decided_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK(auth_time<=decided_at AND auth_time>=decided_at-interval '15 minutes'),
 PRIMARY KEY(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,proposal_id,proposal_revision,canonical_payload_sha256)
  REFERENCES supervisor.proposal_revisions(tenant_id,site_id,proposal_id,revision,canonical_payload_sha256)
);
CREATE UNIQUE INDEX approval_decisions_one_review_per_reviewer ON supervisor.approval_decisions
 (tenant_id,site_id,proposal_id,proposal_revision,reviewer_issuer,reviewer_subject) WHERE decision IN ('approve','reject');

CREATE TABLE supervisor.approval_transitions (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,proposal_id uuid NOT NULL,
 state_revision bigint NOT NULL CHECK(state_revision>0),
 proposal_revision bigint NOT NULL,
 from_state text CHECK(from_state IN ('pending','approved','rejected','expired','superseded','consumed','revoked','stale')),
 to_state text NOT NULL CHECK(to_state IN ('pending','approved','rejected','expired','superseded','consumed','revoked','stale')),
 event text NOT NULL CHECK(event IN ('submit','approve','reject','expire','supersede','revoke')),
 decision_id uuid,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((event IN ('approve','reject','revoke'))=(decision_id IS NOT NULL)),
 PRIMARY KEY(tenant_id,site_id,proposal_id,state_revision,proposal_revision,to_state),
 FOREIGN KEY(tenant_id,site_id,proposal_id,proposal_revision) REFERENCES supervisor.proposal_revisions(tenant_id,site_id,proposal_id,revision),
 FOREIGN KEY(tenant_id,site_id,decision_id) REFERENCES supervisor.approval_decisions(tenant_id,site_id,id)
);

-- Idempotency record for decisions: the key binds actor, operation and the canonical request.
CREATE TABLE supervisor.approval_receipts (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
 operation text NOT NULL CHECK(operation IN ('proposal.decide','proposal.revoke')),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 proposal_id uuid NOT NULL,decision_id uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id,decision_id) REFERENCES supervisor.approval_decisions(tenant_id,site_id,id)
);

CREATE TABLE supervisor.approval_audit (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL DEFAULT gen_random_uuid(),
 -- Null actor: the server acting for a generating run (submission) or the clock (expiry).
 actor_issuer text CHECK(length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text CHECK(length(actor_subject) BETWEEN 1 AND 255),
 event_type text NOT NULL CHECK(event_type IN ('proposal.submitted','proposal.read','proposal.decided','proposal.revoked','proposal.expired','mutation.replayed')),
 proposal_id uuid NOT NULL,state_revision bigint NOT NULL CHECK(state_revision>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((actor_issuer IS NULL)=(actor_subject IS NULL)),
 PRIMARY KEY(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,proposal_id) REFERENCES supervisor.approval_states(tenant_id,site_id,proposal_id)
);

-- The head moves only along the approval state machine (internal/approvals transitions):
-- pending -> approved | rejected | expired | superseded; approved -> consumed | revoked |
-- expired | stale. A new proposal revision (a material edit) starts pending again only when the
-- previous revision is pending (it becomes superseded), rejected or expired; nothing returns to
-- pending otherwise. Every change advances state_revision by exactly one.
CREATE FUNCTION supervisor.approval_state_guard() RETURNS trigger LANGUAGE plpgsql
 SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'approval states are immutable' USING ERRCODE='42501'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.state<>'pending' OR NEW.state_revision<>1 OR NEW.current_revision<>1 THEN
   RAISE EXCEPTION 'a proposal starts pending at revision 1' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
 END IF;
 IF (NEW.tenant_id,NEW.site_id,NEW.proposal_id) IS DISTINCT FROM (OLD.tenant_id,OLD.site_id,OLD.proposal_id)
    OR NEW.state_revision<>OLD.state_revision+1 THEN
  RAISE EXCEPTION 'approval state identity is fixed and its revision advances by one' USING ERRCODE='23514';
 END IF;
 IF NEW.current_revision=OLD.current_revision THEN
  IF (OLD.state,NEW.state) NOT IN (('pending','approved'),('pending','rejected'),('pending','expired'),('pending','superseded'),
     ('approved','consumed'),('approved','revoked'),('approved','expired'),('approved','stale')) THEN
   RAISE EXCEPTION 'approval transition % -> % is not allowed',OLD.state,NEW.state USING ERRCODE='23514';
  END IF;
 ELSIF NEW.current_revision=OLD.current_revision+1 THEN
  IF NEW.state<>'pending' OR OLD.state NOT IN ('pending','rejected','expired') THEN
   RAISE EXCEPTION 'a new proposal revision may only follow a pending, rejected or expired revision' USING ERRCODE='23514';
  END IF;
 ELSE
  RAISE EXCEPTION 'proposal revisions advance by one' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER approval_state_guard BEFORE INSERT OR UPDATE OR DELETE ON supervisor.approval_states
 FOR EACH ROW EXECUTE FUNCTION supervisor.approval_state_guard();

DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['proposal_revisions','approval_decisions','approval_transitions','approval_receipts','approval_audit'] LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t||'_immutable',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['proposal_revisions','approval_states','approval_decisions','approval_transitions','approval_receipts','approval_audit'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP;
 GRANT UPDATE(current_revision,state,state_revision,updated_at) ON supervisor.approval_states TO supervisor_runtime;
END $$;
