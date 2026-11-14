-- Proposal F slice 2: configuration drafts. A draft is not live configuration; a human
-- promotes it through the normal revision path. Revisions and their field citations are
-- append-only, and an agent-authored draft (slice 3) carries its run.
CREATE TABLE supervisor.configuration_drafts (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, draft_id uuid NOT NULL,
 kind text NOT NULL CHECK(kind IN ('line','item','routing','workstation_profile')),
 author_kind text NOT NULL CHECK(author_kind IN ('agent','human')),
 run_id uuid,
 created_by_issuer text CHECK(length(created_by_issuer) BETWEEN 1 AND 512),
 created_by_subject text CHECK(length(created_by_subject) BETWEEN 1 AND 255),
 target_object_id uuid,
 target_expected_revision bigint CHECK(target_expected_revision>=0),
 status text NOT NULL CHECK(status IN ('draft','promoted','discarded')),
 current_revision bigint NOT NULL CHECK(current_revision>0),
 promoted_by_issuer text CHECK(length(promoted_by_issuer) BETWEEN 1 AND 512),
 promoted_by_subject text CHECK(length(promoted_by_subject) BETWEEN 1 AND 255),
 promoted_object_id uuid,
 promoted_object_revision bigint CHECK(promoted_object_revision>0),
 promoted_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,draft_id),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 FOREIGN KEY(tenant_id,site_id,run_id) REFERENCES supervisor.agent_runs(tenant_id,site_id,id),
 CHECK((author_kind='agent')=(run_id IS NOT NULL)),
 CHECK((author_kind='human')=(created_by_issuer IS NOT NULL) AND (created_by_issuer IS NULL)=(created_by_subject IS NULL)),
 CHECK((target_object_id IS NULL)=(target_expected_revision IS NULL)),
 CHECK((status='promoted')=(promoted_by_issuer IS NOT NULL) AND (status='promoted')=(promoted_by_subject IS NOT NULL)
   AND (status='promoted')=(promoted_object_id IS NOT NULL) AND (status='promoted')=(promoted_object_revision IS NOT NULL)
   AND (status='promoted')=(promoted_at IS NOT NULL))
);

CREATE TABLE supervisor.configuration_draft_revisions (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, draft_id uuid NOT NULL,
 revision bigint NOT NULL CHECK(revision>0),
 body jsonb NOT NULL CHECK(jsonb_typeof(body)='object' AND octet_length(body::text)<=1048576),
 body_sha256 text NOT NULL CHECK(body_sha256 ~ '^[0-9a-f]{64}$'),
 edited_by_kind text NOT NULL CHECK(edited_by_kind IN ('agent','human')),
 editor_issuer text CHECK(length(editor_issuer) BETWEEN 1 AND 512),
 editor_subject text CHECK(length(editor_subject) BETWEEN 1 AND 255),
 run_id uuid,
 -- Citations of the previous revision not carried forward, each {path, span_id, reason}.
 dropped_citations jsonb NOT NULL DEFAULT '[]'::jsonb CHECK(jsonb_typeof(dropped_citations)='array' AND jsonb_array_length(dropped_citations)<=500),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,draft_id,revision),
 FOREIGN KEY(tenant_id,site_id,draft_id) REFERENCES supervisor.configuration_drafts(tenant_id,site_id,draft_id),
 FOREIGN KEY(tenant_id,site_id,run_id) REFERENCES supervisor.agent_runs(tenant_id,site_id,id),
 CHECK((edited_by_kind='agent')=(run_id IS NOT NULL)),
 CHECK((edited_by_kind='human')=(editor_issuer IS NOT NULL) AND (editor_issuer IS NULL)=(editor_subject IS NULL))
);
-- The head always names a stored revision (checked at commit, after both rows exist).
ALTER TABLE supervisor.configuration_drafts ADD CONSTRAINT configuration_draft_current_revision_fk
 FOREIGN KEY(tenant_id,site_id,draft_id,current_revision)
 REFERENCES supervisor.configuration_draft_revisions(tenant_id,site_id,draft_id,revision) DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE supervisor.configuration_draft_field_spans (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, draft_id uuid NOT NULL, revision bigint NOT NULL,
 field_path text NOT NULL CHECK(length(field_path) BETWEEN 2 AND 256 AND left(field_path,1)='/'),
 span_id uuid NOT NULL, source_id uuid NOT NULL, source_revision bigint NOT NULL CHECK(source_revision>0),
 source_access_epoch bigint NOT NULL CHECK(source_access_epoch>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,draft_id,revision,field_path,span_id),
 FOREIGN KEY(tenant_id,site_id,draft_id,revision) REFERENCES supervisor.configuration_draft_revisions(tenant_id,site_id,draft_id,revision),
 FOREIGN KEY(tenant_id,site_id,span_id) REFERENCES supervisor.evidence_spans(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,source_id,source_revision) REFERENCES supervisor.evidence_revisions(tenant_id,site_id,source_id,revision)
);

CREATE TABLE supervisor.configuration_draft_receipts (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL,
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 512),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
 operation text NOT NULL CHECK(operation IN ('create','revise','promote','discard')),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 draft_id uuid NOT NULL, revision bigint NOT NULL CHECK(revision>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id,draft_id,revision) REFERENCES supervisor.configuration_draft_revisions(tenant_id,site_id,draft_id,revision)
);

CREATE TABLE supervisor.configuration_draft_audit (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, id uuid NOT NULL DEFAULT gen_random_uuid(),
 draft_id uuid, revision bigint CHECK(revision>0),
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 512),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 event_type text NOT NULL CHECK(event_type IN ('draft.created','draft.revised','draft.promoted','draft.discarded','draft.read','draft.listed','mutation.replayed')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,draft_id) REFERENCES supervisor.configuration_drafts(tenant_id,site_id,draft_id),
 CHECK(revision IS NULL OR draft_id IS NOT NULL)
);

-- Identity is fixed; a draft advances one revision at a time while open, and promotion or
-- discard is terminal. Deletion is never allowed.
CREATE FUNCTION supervisor.configuration_draft_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
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
 -- Rule 05 backstop: the promoter is neither the author nor any human editor.
 IF NEW.status='promoted' AND (
    (NEW.promoted_by_issuer,NEW.promoted_by_subject) IS NOT DISTINCT FROM (OLD.created_by_issuer,OLD.created_by_subject)
    OR EXISTS(SELECT 1 FROM supervisor.configuration_draft_revisions r WHERE r.tenant_id=NEW.tenant_id AND r.site_id=NEW.site_id AND r.draft_id=NEW.draft_id
       AND r.editor_issuer=NEW.promoted_by_issuer AND r.editor_subject=NEW.promoted_by_subject)) THEN
  RAISE EXCEPTION 'configuration draft promoter authored or edited the draft' USING ERRCODE='42501';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER configuration_draft_guard BEFORE INSERT OR UPDATE OR DELETE ON supervisor.configuration_drafts
 FOR EACH ROW EXECUTE FUNCTION supervisor.configuration_draft_guard();

-- Revisions are gapless: each new revision follows the latest stored one.
CREATE FUNCTION supervisor.configuration_draft_revision_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF NEW.revision<>COALESCE((SELECT max(revision) FROM supervisor.configuration_draft_revisions
   WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND draft_id=NEW.draft_id),0)+1 THEN
  RAISE EXCEPTION 'configuration draft revisions are gapless' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER configuration_draft_revision_guard BEFORE INSERT ON supervisor.configuration_draft_revisions
 FOR EACH ROW EXECUTE FUNCTION supervisor.configuration_draft_revision_guard();

-- A citation must name the evidence revision its span belongs to.
CREATE FUNCTION supervisor.configuration_draft_span_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE span_source uuid; span_revision bigint;
BEGIN
 SELECT source_id,revision INTO span_source,span_revision FROM supervisor.evidence_spans
  WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND id=NEW.span_id;
 IF span_source IS DISTINCT FROM NEW.source_id OR span_revision IS DISTINCT FROM NEW.source_revision THEN
  RAISE EXCEPTION 'configuration draft span must name its evidence revision' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER configuration_draft_span_guard BEFORE INSERT ON supervisor.configuration_draft_field_spans
 FOR EACH ROW EXECUTE FUNCTION supervisor.configuration_draft_span_guard();

DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['configuration_drafts','configuration_draft_revisions','configuration_draft_field_spans','configuration_draft_receipts','configuration_draft_audit'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['configuration_draft_revisions','configuration_draft_field_spans','configuration_draft_receipts','configuration_draft_audit'] LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t||'_immutable',t);
 END LOOP;
 GRANT UPDATE(status,current_revision,promoted_by_issuer,promoted_by_subject,promoted_object_id,promoted_object_revision,promoted_at,updated_at) ON supervisor.configuration_drafts TO supervisor_runtime;
END $$;
