-- WIS-625: a designation of model-extracted text counts only when the owner confirmed they checked
-- it against the original. The decision records that confirmation. Decisions made before this rule
-- carry false, so an unconfirmed designation of an OCR revision stays ineligible until the owner
-- designates it again with the confirmation (fail closed). The default is a constant: no row scan.
ALTER TABLE supervisor.evidence_authority_decisions ADD COLUMN model_extracted_confirmed boolean NOT NULL DEFAULT false;

-- Before this rule a revision of an OCR source (POST /sources) was stored without its extraction.
-- Its text derives from the model's reading, so it gets the extraction of the latest earlier
-- extracted revision of the same source. Marking it model-extracted is the fail-closed direction
-- whatever was edited; a revision of another kind fails the kind CHECK loudly instead of passing.
-- This restores provenance, not evidence, so the immutability and per-row inventory triggers are
-- suspended for this statement only; the inventory is advanced explicitly below.
ALTER TABLE supervisor.evidence_revisions DISABLE TRIGGER evidence_immutable, DISABLE TRIGGER knowledge_inventory_changed;
UPDATE supervisor.evidence_revisions r SET (extraction,extractor_model,extractor_model_revision,extractor_prompt_sha256,extraction_run_id)=(
  SELECT p.extraction,p.extractor_model,p.extractor_model_revision,p.extractor_prompt_sha256,p.extraction_run_id FROM supervisor.evidence_revisions p
  WHERE (p.tenant_id,p.site_id,p.source_id)=(r.tenant_id,r.site_id,r.source_id) AND p.revision<r.revision AND p.extraction IS NOT NULL ORDER BY p.revision DESC LIMIT 1)
 WHERE r.extraction IS NULL AND EXISTS(SELECT 1 FROM supervisor.evidence_revisions p
  WHERE (p.tenant_id,p.site_id,p.source_id)=(r.tenant_id,r.site_id,r.source_id) AND p.revision<r.revision AND p.extraction IS NOT NULL);
ALTER TABLE supervisor.evidence_revisions ENABLE TRIGGER evidence_immutable, ENABLE TRIGGER knowledge_inventory_changed;

-- Eligibility changed for every site holding model-extracted text (unconfirmed designations,
-- restored revisions), so its knowledge inventory advances: snapshots taken before read as stale.
UPDATE supervisor.knowledge_inventory_revisions i SET revision=i.revision+1
 WHERE EXISTS(SELECT 1 FROM supervisor.evidence_revisions r WHERE (r.tenant_id,r.site_id)=(i.tenant_id,i.site_id) AND r.extraction IS NOT NULL);
