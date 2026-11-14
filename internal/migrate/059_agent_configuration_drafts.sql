-- Slice 3 extends the existing conversation run path for agent configuration drafts.
ALTER TABLE supervisor.interview_agent_turns DROP CONSTRAINT interview_agent_turns_purpose_check;
ALTER TABLE supervisor.interview_agent_turns ADD CONSTRAINT interview_agent_turns_purpose_check CHECK(purpose IN ('questions','drafts'));

CREATE TABLE supervisor.configuration_draft_requests (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,session_id uuid NOT NULL,run_id uuid NOT NULL,
 requested_by_issuer text NOT NULL CHECK(length(requested_by_issuer) BETWEEN 1 AND 512),
 requested_by_subject text NOT NULL CHECK(length(requested_by_subject) BETWEEN 1 AND 255),
 source_ids uuid[] NOT NULL DEFAULT '{}',kinds text[] NOT NULL CHECK(cardinality(kinds) BETWEEN 1 AND 4 AND kinds <@ ARRAY['line','item','routing','workstation_profile']::text[]),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,run_id),UNIQUE(tenant_id,site_id,session_id,run_id),
 CHECK(cardinality(source_ids)<=20),
 FOREIGN KEY(tenant_id,site_id,session_id) REFERENCES supervisor.interview_sessions(tenant_id,site_id,session_id),
 FOREIGN KEY(tenant_id,site_id,run_id) REFERENCES supervisor.agent_runs(tenant_id,site_id,id)
);

CREATE TABLE supervisor.configuration_draft_request_outbox (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL,session_id uuid NOT NULL,run_id uuid NOT NULL,
 delivered_at timestamptz,
 PRIMARY KEY(tenant_id,site_id,id),UNIQUE(tenant_id,site_id,run_id),
 FOREIGN KEY(tenant_id,site_id,session_id,run_id) REFERENCES supervisor.configuration_draft_requests(tenant_id,site_id,session_id,run_id)
);

CREATE TABLE supervisor.configuration_draft_request_receipts (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 512),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),run_id uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id,run_id) REFERENCES supervisor.agent_runs(tenant_id,site_id,id)
);

CREATE TRIGGER configuration_draft_requests_immutable BEFORE UPDATE OR DELETE ON supervisor.configuration_draft_requests FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
CREATE TRIGGER configuration_draft_request_receipts_immutable BEFORE UPDATE OR DELETE ON supervisor.configuration_draft_request_receipts FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
CREATE FUNCTION supervisor.configuration_draft_request_outbox_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'configuration draft request outbox is retained' USING ERRCODE='42501'; END IF;
 IF OLD.delivered_at IS NOT NULL OR ROW(NEW.tenant_id,NEW.site_id,NEW.id,NEW.session_id,NEW.run_id) IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.id,OLD.session_id,OLD.run_id) OR NEW.delivered_at IS NULL THEN
  RAISE EXCEPTION 'invalid configuration draft request delivery update' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER configuration_draft_request_outbox_guard BEFORE UPDATE OR DELETE ON supervisor.configuration_draft_request_outbox FOR EACH ROW EXECUTE FUNCTION supervisor.configuration_draft_request_outbox_guard();

-- The run requester is an editor for separation of duties, including an unedited agent draft.
CREATE OR REPLACE FUNCTION supervisor.configuration_draft_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'configuration drafts are retained' USING ERRCODE='42501'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.status<>'draft' OR NEW.current_revision<>1 THEN RAISE EXCEPTION 'a configuration draft starts open at revision 1' USING ERRCODE='23514'; END IF;
  RETURN NEW;
 END IF;
 IF ROW(NEW.tenant_id,NEW.site_id,NEW.draft_id,NEW.kind,NEW.author_kind,NEW.run_id,NEW.created_by_issuer,NEW.created_by_subject,NEW.target_object_id,NEW.target_expected_revision,NEW.created_at)
    IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.draft_id,OLD.kind,OLD.author_kind,OLD.run_id,OLD.created_by_issuer,OLD.created_by_subject,OLD.target_object_id,OLD.target_expected_revision,OLD.created_at)
    OR OLD.status<>'draft'
    OR NOT ((NEW.status='draft' AND NEW.current_revision=OLD.current_revision+1)
         OR (NEW.status IN ('promoted','discarded') AND NEW.current_revision=OLD.current_revision)) THEN
  RAISE EXCEPTION 'invalid configuration draft transition' USING ERRCODE='23514';
 END IF;
 IF NEW.status='promoted' AND (
    (NEW.promoted_by_issuer,NEW.promoted_by_subject) IS NOT DISTINCT FROM (OLD.created_by_issuer,OLD.created_by_subject)
    OR EXISTS(SELECT 1 FROM supervisor.configuration_draft_revisions r WHERE r.tenant_id=NEW.tenant_id AND r.site_id=NEW.site_id AND r.draft_id=NEW.draft_id
       AND r.editor_issuer=NEW.promoted_by_issuer AND r.editor_subject=NEW.promoted_by_subject)
    OR EXISTS(SELECT 1 FROM supervisor.configuration_draft_requests q WHERE q.tenant_id=NEW.tenant_id AND q.site_id=NEW.site_id AND q.run_id=OLD.run_id
       AND q.requested_by_issuer=NEW.promoted_by_issuer AND q.requested_by_subject=NEW.promoted_by_subject)) THEN
  RAISE EXCEPTION 'configuration draft promoter authored or edited the draft' USING ERRCODE='42501';
 END IF;
 RETURN NEW;
END $$;

DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['configuration_draft_requests','configuration_draft_request_outbox','configuration_draft_request_receipts'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP;
 GRANT UPDATE(delivered_at) ON supervisor.configuration_draft_request_outbox TO supervisor_runtime;
END $$;
