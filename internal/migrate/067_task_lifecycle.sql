-- WIS-277 task lifecycle: the assignee acknowledges, starts and completes a task with report
-- evidence; a site supervisor blocks, cancels or reopens it. Completion is never edited: each
-- completion is an immutable epoch with its note and the exact evidence revisions it cites, and a
-- reopen leaves it in place for the next epoch.
CREATE TABLE supervisor.task_completions (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,task_id uuid NOT NULL,
 epoch integer NOT NULL CHECK(epoch>0),
 task_revision bigint NOT NULL CHECK(task_revision>1),
 completion_note text NOT NULL CHECK(char_length(completion_note) BETWEEN 1 AND 4000),
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 512),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,task_id,epoch),
 UNIQUE(tenant_id,site_id,task_id,task_revision),
 FOREIGN KEY(tenant_id,site_id,task_id,task_revision) REFERENCES supervisor.task_events(tenant_id,site_id,task_id,revision)
);

-- Each cited evidence revision is bound exactly; a later revision of the source does not change
-- what the completion cited.
CREATE TABLE supervisor.task_completion_evidence (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,task_id uuid NOT NULL,epoch integer NOT NULL,
 source_id uuid NOT NULL,revision bigint NOT NULL CHECK(revision>0),
 PRIMARY KEY(tenant_id,site_id,task_id,epoch,source_id),
 FOREIGN KEY(tenant_id,site_id,task_id,epoch) REFERENCES supervisor.task_completions(tenant_id,site_id,task_id,epoch),
 FOREIGN KEY(tenant_id,site_id,source_id,revision) REFERENCES supervisor.evidence_revisions(tenant_id,site_id,source_id,revision)
);

CREATE TABLE supervisor.task_receipts (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 512),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 idempotency_key uuid NOT NULL,
 operation text NOT NULL CHECK(operation IN ('task.acknowledge','task.start','task.block','task.complete','task.cancel','task.reopen')),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 task_id uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key),
 FOREIGN KEY(tenant_id,site_id,task_id) REFERENCES supervisor.tasks(tenant_id,site_id,id)
);

DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['task_completions','task_completion_evidence','task_receipts'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
  EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t||'_immutable',t);
 END LOOP;
 -- The task head moves only through its guard trigger (066); identity columns stay fixed.
 GRANT UPDATE(state,revision,acknowledged_at,started_at,completed_at) ON supervisor.tasks TO supervisor_runtime;
END $$;

-- Every task head change is backed by its history (WIS-277 review): at commit, the event at the
-- new revision must exist and describe exactly this change, and a completion must have its epoch
-- with evidence. acknowledged_at and started_at are written once; completed_at is set exactly
-- while the task is completed.
ALTER TABLE supervisor.tasks ADD CONSTRAINT tasks_completed_at CHECK((state='completed')=(completed_at IS NOT NULL));
-- An event's name is tied to the exact transition it records, and the events that need a reason
-- carry one (WIS-277 review N2; internal/tasks rules). The evidence cut-off reads the latest
-- reopen, so a mislabelled event must not move it. NULL from_state is only the creation event.
ALTER TABLE supervisor.task_events ADD CONSTRAINT task_events_transition CHECK(CASE WHEN from_state IS NULL THEN event='create' AND to_state='assigned' ELSE (from_state,event,to_state) IN (
 ('assigned','acknowledge','acknowledged'),('assigned','cancel','canceled'),
 ('acknowledged','start','in_progress'),('acknowledged','block','blocked'),('acknowledged','cancel','canceled'),
 ('in_progress','complete','completed'),('in_progress','block','blocked'),('in_progress','cancel','canceled'),
 ('blocked','start','in_progress'),('blocked','cancel','canceled'),
 ('completed','reopen','in_progress')) END);
ALTER TABLE supervisor.task_events ADD CONSTRAINT task_events_reason CHECK(reason IS NOT NULL OR NOT (event IN ('block','cancel','reopen') OR (event='start' AND from_state='blocked')));
CREATE FUNCTION supervisor.task_history_check() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM supervisor.task_events e WHERE (e.tenant_id,e.site_id,e.task_id,e.revision)=(NEW.tenant_id,NEW.site_id,NEW.id,NEW.revision)
    AND e.from_state IS NOT DISTINCT FROM OLD.state AND e.to_state=NEW.state) THEN
  RAISE EXCEPTION 'a task change needs the event that describes it' USING ERRCODE='23514';
 END IF;
 IF NEW.state='completed' AND NOT EXISTS(SELECT 1 FROM supervisor.task_completions c JOIN supervisor.task_completion_evidence v ON (v.tenant_id,v.site_id,v.task_id,v.epoch)=(c.tenant_id,c.site_id,c.task_id,c.epoch)
    WHERE (c.tenant_id,c.site_id,c.task_id,c.task_revision)=(NEW.tenant_id,NEW.site_id,NEW.id,NEW.revision)) THEN
  RAISE EXCEPTION 'a completed task needs its completion with evidence' USING ERRCODE='23514';
 END IF;
 IF (OLD.acknowledged_at IS NOT NULL AND NEW.acknowledged_at IS DISTINCT FROM OLD.acknowledged_at)
    OR (OLD.started_at IS NOT NULL AND NEW.started_at IS DISTINCT FROM OLD.started_at) THEN
  RAISE EXCEPTION 'acknowledged_at and started_at are written once' USING ERRCODE='23514';
 END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER task_history_check AFTER UPDATE ON supervisor.tasks DEFERRABLE INITIALLY DEFERRED
 FOR EACH ROW EXECUTE FUNCTION supervisor.task_history_check();
