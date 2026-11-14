-- WIS-626: indexed word search over evidence span text, so chat passage ranking (WIS-620) no longer
-- re-reads each cited revision body and re-runs to_tsvector for every span on every question.
--
-- evidence_spans rows are immutable history and every write to them advances the knowledge
-- inventory (013), which would mark every approved snapshot stale; a column on that table could
-- not be backfilled. The search data is therefore a side table, one row per span, written only by
-- an AFTER INSERT trigger on evidence_spans (SECURITY DEFINER: the runtime role reads it but can
-- never write a vector of its own). A span and its revision body never change, so a row never
-- goes stale. lexemes is generated from the stored vector and carries the GIN index; ranking
-- matches exact lexemes, so no tsquery is re-parsed and the words are those WIS-620 compared.
CREATE TABLE supervisor.evidence_span_search (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,span_id uuid NOT NULL,
 source_id uuid NOT NULL,
 revision bigint NOT NULL CHECK(revision>0),
 text_bytes integer NOT NULL CHECK(text_bytes>0),
 search tsvector NOT NULL,
 lexemes text[] GENERATED ALWAYS AS (tsvector_to_array(search)) STORED,
 PRIMARY KEY(tenant_id,site_id,span_id),
 FOREIGN KEY(tenant_id,site_id,span_id,source_id,revision)
  REFERENCES supervisor.evidence_spans(tenant_id,site_id,id,source_id,revision) ON DELETE CASCADE
);
CREATE INDEX evidence_span_search_lexemes ON supervisor.evidence_span_search USING gin(lexemes);
-- Immutable like the span it describes; only the owning (retention) role may delete.
CREATE TRIGGER evidence_span_search_immutable BEFORE UPDATE OR DELETE ON supervisor.evidence_span_search
 FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();

CREATE FUNCTION supervisor.evidence_span_search_index() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
 SET search_path=pg_catalog,pg_temp SET row_security=on AS $$
DECLARE passage text;
BEGIN
 SELECT substring(r.body FROM NEW.start_rune+1 FOR NEW.end_rune-NEW.start_rune) INTO passage
 FROM supervisor.evidence_revisions r
 WHERE (r.tenant_id,r.site_id,r.source_id,r.revision)=(NEW.tenant_id,NEW.site_id,NEW.source_id,NEW.revision);
 IF passage IS NULL OR passage='' THEN RAISE EXCEPTION 'Invalid source span' USING ERRCODE='23514'; END IF;
 INSERT INTO supervisor.evidence_span_search(tenant_id,site_id,span_id,source_id,revision,text_bytes,search)
 VALUES(NEW.tenant_id,NEW.site_id,NEW.id,NEW.source_id,NEW.revision,octet_length(passage),to_tsvector('english'::regconfig,passage));
 RETURN NULL;
END $$;
REVOKE ALL ON FUNCTION supervisor.evidence_span_search_index() FROM PUBLIC,supervisor_runtime;
CREATE TRIGGER evidence_span_search_index AFTER INSERT ON supervisor.evidence_spans
 FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_span_search_index();

-- Backfill every existing span. The schema owner must see every tenant; visibility is temporary
-- and transactional (as in 007 and 009), and the runtime role stays subject to RLS throughout.
ALTER TABLE supervisor.evidence_spans NO FORCE ROW LEVEL SECURITY;
ALTER TABLE supervisor.evidence_revisions NO FORCE ROW LEVEL SECURITY;
INSERT INTO supervisor.evidence_span_search(tenant_id,site_id,span_id,source_id,revision,text_bytes,search)
 SELECT s.tenant_id,s.site_id,s.id,s.source_id,s.revision,octet_length(p.passage),to_tsvector('english'::regconfig,p.passage)
 FROM supervisor.evidence_spans s
 JOIN supervisor.evidence_revisions r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=(s.tenant_id,s.site_id,s.source_id,s.revision)
 CROSS JOIN LATERAL (SELECT substring(r.body FROM s.start_rune+1 FOR s.end_rune-s.start_rune) AS passage) p;
DO $$ BEGIN
 IF (SELECT count(*) FROM supervisor.evidence_span_search)<>(SELECT count(*) FROM supervisor.evidence_spans) THEN
  RAISE EXCEPTION 'evidence span search backfill incomplete';
 END IF;
END $$;
ALTER TABLE supervisor.evidence_spans FORCE ROW LEVEL SECURITY;
ALTER TABLE supervisor.evidence_revisions FORCE ROW LEVEL SECURITY;

ALTER TABLE supervisor.evidence_span_search ENABLE ROW LEVEL SECURITY;
ALTER TABLE supervisor.evidence_span_search FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON supervisor.evidence_span_search
 USING(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid);
REVOKE ALL ON supervisor.evidence_span_search FROM PUBLIC,supervisor_runtime;
GRANT SELECT ON supervisor.evidence_span_search TO supervisor_runtime;
