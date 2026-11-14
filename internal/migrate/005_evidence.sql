-- Plain-text provenance only. Claims, parser outputs and approved knowledge are later increments.
CREATE TABLE supervisor.evidence_heads (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, source_id uuid NOT NULL,
 current_revision bigint NOT NULL CHECK(current_revision>0),
 state_revision bigint NOT NULL CHECK(state_revision>0),
 access_epoch bigint NOT NULL DEFAULT 1 CHECK(access_epoch>0),
 revoked boolean NOT NULL DEFAULT false,
 PRIMARY KEY(tenant_id,site_id,source_id),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id)
);
CREATE TABLE supervisor.evidence_revisions (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, source_id uuid NOT NULL,
 revision bigint NOT NULL CHECK(revision>0),
 kind text NOT NULL CHECK(kind IN ('manual','interview','document')),
 title text NOT NULL CHECK(length(title) BETWEEN 1 AND 200),
 body text NOT NULL CHECK(octet_length(body) BETWEEN 1 AND 262144),
 sha256 text NOT NULL CHECK(sha256 = encode(sha256(convert_to(body,'UTF8')),'hex')),
 byte_length integer NOT NULL CHECK(byte_length=octet_length(body)),
 rune_length integer NOT NULL CHECK(rune_length=length(body)),
 authority text NOT NULL DEFAULT 'reported' CHECK(authority='reported'),
 actor_issuer text NOT NULL, actor_subject text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,source_id,revision),
 FOREIGN KEY(tenant_id,site_id,source_id) REFERENCES supervisor.evidence_heads(tenant_id,site_id,source_id)
);
ALTER TABLE supervisor.evidence_heads ADD CONSTRAINT evidence_current_revision_fk
 FOREIGN KEY(tenant_id,site_id,source_id,current_revision)
 REFERENCES supervisor.evidence_revisions(tenant_id,site_id,source_id,revision) DEFERRABLE INITIALLY DEFERRED;
CREATE TABLE supervisor.evidence_spans (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, id uuid NOT NULL,
 source_id uuid NOT NULL, revision bigint NOT NULL,
 start_rune integer NOT NULL CHECK(start_rune>=0), end_rune integer NOT NULL CHECK(end_rune>start_rune),
 actor_issuer text NOT NULL, actor_subject text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,source_id,revision) REFERENCES supervisor.evidence_revisions(tenant_id,site_id,source_id,revision)
);
CREATE TABLE supervisor.evidence_revocations (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, source_id uuid NOT NULL,
 state_revision bigint NOT NULL CHECK(state_revision>1), access_epoch bigint NOT NULL CHECK(access_epoch>1),
 actor_issuer text NOT NULL, actor_subject text NOT NULL,
 reason text NOT NULL CHECK(length(reason) BETWEEN 1 AND 1000),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,source_id),
 FOREIGN KEY(tenant_id,site_id,source_id) REFERENCES supervisor.evidence_heads(tenant_id,site_id,source_id)
);
CREATE TABLE supervisor.evidence_receipts (
 tenant_id uuid NOT NULL, site_id uuid NOT NULL, actor_issuer text NOT NULL, actor_subject text NOT NULL,
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
 request_hash text NOT NULL CHECK(request_hash ~ '^[0-9a-f]{64}$'),
 source_id uuid NOT NULL, result jsonb NOT NULL,
 PRIMARY KEY(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id,source_id) REFERENCES supervisor.evidence_heads(tenant_id,site_id,source_id)
);
CREATE TABLE supervisor.evidence_audit (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), tenant_id uuid NOT NULL, site_id uuid NOT NULL,
 source_id uuid, revision bigint, actor_issuer text NOT NULL, actor_subject text NOT NULL,
 event_type text NOT NULL CHECK(event_type IN ('source.created','source.revised','source.read','source.listed','source.revoked','span.created','span.read','mutation.replayed')),
 reason text CHECK(length(reason) BETWEEN 1 AND 1000),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 FOREIGN KEY(tenant_id,site_id,source_id) REFERENCES supervisor.evidence_heads(tenant_id,site_id,source_id)
);
CREATE FUNCTION supervisor.evidence_immutable() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' AND pg_has_role(current_user,(SELECT relowner FROM pg_class WHERE oid=TG_RELID),'MEMBER') THEN RETURN OLD; END IF;
 RAISE EXCEPTION 'Evidence history is immutable' USING ERRCODE='42501';
END $$;
CREATE FUNCTION supervisor.evidence_head_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF ROW(NEW.tenant_id,NEW.site_id,NEW.source_id) IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.source_id)
 OR OLD.revoked OR NEW.state_revision<>OLD.state_revision+1
 OR NOT ((NEW.current_revision=OLD.current_revision+1 AND NOT NEW.revoked AND NEW.access_epoch=OLD.access_epoch)
       OR (NEW.current_revision=OLD.current_revision AND NEW.revoked AND NEW.access_epoch=OLD.access_epoch+1)) THEN
  RAISE EXCEPTION 'Invalid evidence transition' USING ERRCODE='42501';
 END IF;
 RETURN NEW;
END $$;
CREATE FUNCTION supervisor.evidence_span_bounds() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE n integer;
BEGIN
 SELECT rune_length INTO n FROM supervisor.evidence_revisions WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND source_id=NEW.source_id AND revision=NEW.revision;
 IF n IS NULL OR NEW.end_rune>n THEN RAISE EXCEPTION 'Invalid source span' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER evidence_head_guard BEFORE UPDATE ON supervisor.evidence_heads FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_head_guard();
CREATE TRIGGER evidence_span_bounds BEFORE INSERT ON supervisor.evidence_spans FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_span_bounds();
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['evidence_heads','evidence_revisions','evidence_spans','evidence_receipts','evidence_audit','evidence_revocations'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING (tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK (tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
  IF t<>'evidence_heads' THEN
   EXECUTE format('CREATE TRIGGER evidence_immutable BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t);
  END IF;
 END LOOP;
END $$;
GRANT UPDATE(current_revision,state_revision,access_epoch,revoked) ON supervisor.evidence_heads TO supervisor_runtime;

-- Runtime has no UPDATE privilege on sites. Expose row locking, never site mutation.
CREATE FUNCTION supervisor.lock_evidence_site(p_tenant uuid,p_site uuid,p_write boolean)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,pg_temp SET row_security=on AS $$
BEGIN
 IF p_tenant IS NULL OR p_site IS NULL OR p_write IS NULL
 OR current_setting('supervisor.tenant_id',true) IS DISTINCT FROM p_tenant::text THEN
  RETURN false;
 END IF;
 IF p_write THEN
  PERFORM 1 FROM supervisor.sites WHERE tenant_id=p_tenant AND id=p_site FOR NO KEY UPDATE;
 ELSE
  PERFORM 1 FROM supervisor.sites WHERE tenant_id=p_tenant AND id=p_site FOR KEY SHARE;
 END IF;
 RETURN FOUND;
END $$;
REVOKE ALL ON FUNCTION supervisor.lock_evidence_site(uuid,uuid,boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION supervisor.lock_evidence_site(uuid,uuid,boolean) TO supervisor_runtime;
