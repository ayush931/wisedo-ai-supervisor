-- WIS-270 corroboration: each source revision declares an authority class and belongs to one
-- independence group. A group id is the source_id of an existing source in the same tenant+site
-- (composite FK below), so a cross-site or cross-tenant group is unrepresentable.
--
-- Backfill (documented, deterministic):
--  * independence_group_id = source_id: every pre-039 source is its own group. Identical bytes
--    stored before 039 are NOT merged retroactively; only revisions created after 039 join a
--    group by content hash.
--  * authority_class = 'approved_document' when an owner 'designate' decision pins that exact
--    (source_id, revision, sha256), else 'reported'. This records history only: a later withdrawal
--    does not rewrite the immutable revision, and live authority is still read from the ledger.
--
-- authority_class is a declared classification, never authority. Review approval keeps reading
-- the designation ledger (008) for an exact, current, non-revoked source revision.
ALTER TABLE supervisor.evidence_revisions DROP CONSTRAINT evidence_revisions_kind_check;
ALTER TABLE supervisor.evidence_revisions ADD CONSTRAINT evidence_revisions_kind_check CHECK(kind IN ('manual','interview','document','event'));
ALTER TABLE supervisor.evidence_revisions
 ADD COLUMN authority_class text NOT NULL DEFAULT 'reported' CHECK(authority_class IN ('reported','observed','approved_document')),
 ADD COLUMN independence_group_id uuid;
-- The backfill fills new columns once; it neither rewrites evidence nor changes eligibility, so
-- the immutability and inventory-epoch triggers are suspended for this statement only.
ALTER TABLE supervisor.evidence_revisions DISABLE TRIGGER evidence_immutable, DISABLE TRIGGER knowledge_inventory_changed;
UPDATE supervisor.evidence_revisions r SET independence_group_id=r.source_id,
 authority_class=CASE WHEN EXISTS(SELECT 1 FROM supervisor.evidence_authority_decisions d
  WHERE (d.tenant_id,d.site_id,d.source_id,d.source_revision,d.source_sha256)=(r.tenant_id,r.site_id,r.source_id,r.revision,r.sha256) AND d.verdict='designate')
 THEN 'approved_document' ELSE 'reported' END;
ALTER TABLE supervisor.evidence_revisions ENABLE TRIGGER evidence_immutable, ENABLE TRIGGER knowledge_inventory_changed;
-- Defaults keep a rolled-back pre-039 binary and direct inserts writing: a revision without a
-- group founds its own (the service always resolves the group first).
CREATE FUNCTION supervisor.evidence_revision_group_default() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN NEW.independence_group_id:=COALESCE(NEW.independence_group_id,NEW.source_id); RETURN NEW; END $$;
CREATE TRIGGER evidence_revision_group_default BEFORE INSERT ON supervisor.evidence_revisions FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_revision_group_default();
ALTER TABLE supervisor.evidence_revisions ALTER COLUMN independence_group_id SET NOT NULL,
 ADD CONSTRAINT evidence_revision_group_fk FOREIGN KEY(tenant_id,site_id,independence_group_id) REFERENCES supervisor.evidence_heads(tenant_id,site_id,source_id);
-- Same-site content lookup for the repeated-source-replay rule and group membership checks.
CREATE INDEX evidence_revisions_sha256_idx ON supervisor.evidence_revisions(tenant_id,site_id,sha256);
CREATE INDEX evidence_revisions_group_idx ON supervisor.evidence_revisions(tenant_id,site_id,independence_group_id);
