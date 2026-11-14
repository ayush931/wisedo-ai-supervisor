-- A submitted proposal gets its UUID inside approvals.Submit. Permit that UUID to be linked
-- back to the completed run once, after verifying the proposal belongs to the run.
CREATE OR REPLACE FUNCTION supervisor.agent_run_guard() RETURNS trigger LANGUAGE plpgsql
 SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN
  IF pg_has_role(current_user,(SELECT relowner FROM pg_class WHERE oid=TG_RELID),'MEMBER') THEN RETURN OLD; END IF;
  RAISE EXCEPTION 'agent runs are immutable history' USING ERRCODE='42501';
 END IF;
 IF OLD.state<>'running' THEN
  IF OLD.state='completed' AND OLD.outcome_status='proposed' AND OLD.artifact_id IS NOT NULL
   AND (OLD.proposal_id IS NULL AND NEW.proposal_id IS NOT NULL OR NEW.proposal_id=OLD.proposal_id)
   AND (to_jsonb(NEW)-'proposal_id')=(to_jsonb(OLD)-'proposal_id')
   AND EXISTS(SELECT 1 FROM supervisor.proposal_revisions p WHERE p.tenant_id=OLD.tenant_id AND p.site_id=OLD.site_id
    AND p.proposal_id=NEW.proposal_id AND p.case_id=OLD.case_id AND p.case_revision=OLD.case_revision AND p.generated_by_run_id=OLD.id) THEN
   RETURN NEW;
  END IF;
  RAISE EXCEPTION 'agent run % is terminal',OLD.id USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
