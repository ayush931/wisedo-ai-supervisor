-- WIS-277 approved task execution. The deterministic executor consumes an approved proposal into
-- one internal task in a single transaction: the action ledger row, the task, its first event, the
-- approval consumption and the case transitions commit together or not at all. `committed` means
-- an internal task exists, not that work was performed.

-- A proposal revision's case is part of its key, so an action cannot name a case other than the
-- one its approved proposal was generated for (WIS-277 review addendum).
ALTER TABLE supervisor.proposal_revisions ADD CONSTRAINT proposal_revisions_case_key UNIQUE(tenant_id,site_id,proposal_id,revision,case_id);

-- One action per approved proposal revision. The action ID is generated once and is the task's
-- stable identity: a retried execution finds this row, never a second one.
CREATE TABLE supervisor.approved_actions (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL,
 proposal_id uuid NOT NULL,proposal_revision bigint NOT NULL CHECK(proposal_revision>0),
 action_kind text NOT NULL CHECK(action_kind='maintenance_investigation.create'),
 canonical_payload_sha256 text NOT NULL CHECK(canonical_payload_sha256 ~ '^[0-9a-f]{64}$'),
 case_id uuid NOT NULL,
 -- The ledger stores only terminal rows (it is immutable): an action is committed with its task,
 -- or rejected with the executor's reason.
 state text NOT NULL CHECK(state IN ('committed','rejected')),
 -- A rejected action names why the executor refused it (rule 06: a specific reason).
 reason text CHECK(reason ~ '^[A-Z][A-Z0-9_]{0,63}$'),
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 512),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((state='rejected')=(reason IS NOT NULL)),
 PRIMARY KEY(tenant_id,site_id,id),
 UNIQUE(tenant_id,id),
 UNIQUE(tenant_id,site_id,id,case_id),
 UNIQUE(tenant_id,proposal_id,proposal_revision,action_kind),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 -- The action binds the exact digest of the approved revision.
 FOREIGN KEY(tenant_id,site_id,proposal_id,proposal_revision,canonical_payload_sha256)
  REFERENCES supervisor.proposal_revisions(tenant_id,site_id,proposal_id,revision,canonical_payload_sha256),
 FOREIGN KEY(tenant_id,site_id,case_id) REFERENCES supervisor.cases(tenant_id,site_id,id),
 CONSTRAINT approved_actions_proposal_case_fkey FOREIGN KEY(tenant_id,site_id,proposal_id,proposal_revision,case_id)
  REFERENCES supervisor.proposal_revisions(tenant_id,site_id,proposal_id,revision,case_id)
);

CREATE TABLE supervisor.tasks (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL,
 action_id uuid NOT NULL,case_id uuid NOT NULL,
 asset_id uuid NOT NULL,line_id uuid NOT NULL,order_id uuid NOT NULL,
 assignee_actor_id uuid NOT NULL,
 assignee_issuer text NOT NULL CHECK(length(assignee_issuer) BETWEEN 1 AND 512),
 assignee_subject text NOT NULL CHECK(length(assignee_subject) BETWEEN 1 AND 255),
 template text NOT NULL CHECK(template='inspect_report_only'),
 description text NOT NULL CHECK(char_length(description) BETWEEN 1 AND 4000),
 due_at timestamptz NOT NULL,
 state text NOT NULL CHECK(state IN ('assigned','acknowledged','in_progress','blocked','completed','canceled')),
 revision bigint NOT NULL CHECK(revision>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 acknowledged_at timestamptz,started_at timestamptz,completed_at timestamptz,
 PRIMARY KEY(tenant_id,site_id,id),
 UNIQUE(tenant_id,action_id),
 -- A task belongs to its action's case; it cannot point at another case's action.
 FOREIGN KEY(tenant_id,site_id,action_id,case_id) REFERENCES supervisor.approved_actions(tenant_id,site_id,id,case_id),
 FOREIGN KEY(tenant_id,site_id,case_id) REFERENCES supervisor.cases(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,asset_id) REFERENCES supervisor.knowledge_entity_heads(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,line_id) REFERENCES supervisor.knowledge_entity_heads(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,assignee_issuer,assignee_subject) REFERENCES supervisor.memberships(tenant_id,site_id,issuer,subject)
);
CREATE INDEX tasks_case ON supervisor.tasks(tenant_id,site_id,case_id);

CREATE TABLE supervisor.task_events (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,task_id uuid NOT NULL,
 revision bigint NOT NULL CHECK(revision>0),
 from_state text CHECK(from_state IN ('assigned','acknowledged','in_progress','blocked','completed','canceled')),
 to_state text NOT NULL CHECK(to_state IN ('assigned','acknowledged','in_progress','blocked','completed','canceled')),
 event text NOT NULL CHECK(event ~ '^[a-z][a-z_]{0,63}$'),
 reason text CHECK(char_length(reason) BETWEEN 1 AND 2000),
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 512),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((revision=1)=(from_state IS NULL)),
 PRIMARY KEY(tenant_id,site_id,task_id,revision),
 FOREIGN KEY(tenant_id,site_id,task_id) REFERENCES supervisor.tasks(tenant_id,site_id,id)
);

-- A task's identity, scope and assignee never change; a material change is a cancellation and a
-- newly approved proposal (no silent reassignment). A new task starts assigned at revision 1, and
-- the line and order are the case's own. Every later change advances the revision by one along an
-- allowed state pair.
CREATE FUNCTION supervisor.task_head_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE case_line uuid; case_order uuid;
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'tasks are retained' USING ERRCODE='42501'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.revision<>1 OR NEW.state<>'assigned' OR NEW.acknowledged_at IS NOT NULL OR NEW.started_at IS NOT NULL OR NEW.completed_at IS NOT NULL THEN
   RAISE EXCEPTION 'a task starts assigned at revision 1' USING ERRCODE='23514';
  END IF;
  SELECT line_id,order_id INTO case_line,case_order FROM supervisor.cases WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND id=NEW.case_id;
  IF case_line IS DISTINCT FROM NEW.line_id OR case_order IS DISTINCT FROM NEW.order_id THEN
   RAISE EXCEPTION 'a task belongs to its case line and order' USING ERRCODE='23514';
  END IF;
  -- Only a committed action carries a task (rule 05: committed means a task exists).
  IF NOT EXISTS(SELECT 1 FROM supervisor.approved_actions a WHERE a.tenant_id=NEW.tenant_id AND a.site_id=NEW.site_id AND a.id=NEW.action_id AND a.state='committed') THEN
   RAISE EXCEPTION 'a task needs a committed action' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
 END IF;
 IF ROW(NEW.tenant_id,NEW.site_id,NEW.id,NEW.action_id,NEW.case_id,NEW.asset_id,NEW.line_id,NEW.order_id,NEW.assignee_actor_id,
        NEW.assignee_issuer,NEW.assignee_subject,NEW.template,NEW.description,NEW.due_at,NEW.created_at)
    IS DISTINCT FROM ROW(OLD.tenant_id,OLD.site_id,OLD.id,OLD.action_id,OLD.case_id,OLD.asset_id,OLD.line_id,OLD.order_id,OLD.assignee_actor_id,
        OLD.assignee_issuer,OLD.assignee_subject,OLD.template,OLD.description,OLD.due_at,OLD.created_at)
    OR NEW.revision<>OLD.revision+1
    OR (OLD.state,NEW.state) NOT IN (('assigned','acknowledged'),('acknowledged','in_progress'),('in_progress','completed'),
        ('acknowledged','blocked'),('in_progress','blocked'),('blocked','in_progress'),('completed','in_progress'),
        ('assigned','canceled'),('acknowledged','canceled'),('in_progress','canceled'),('blocked','canceled')) THEN
  RAISE EXCEPTION 'a task change keeps its identity and advances one allowed step' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER task_head_guard BEFORE INSERT OR UPDATE OR DELETE ON supervisor.tasks
 FOR EACH ROW EXECUTE FUNCTION supervisor.task_head_guard();

-- An event must describe the head it lands on: same revision and state.
CREATE FUNCTION supervisor.task_event_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$
DECLARE head_revision bigint; head_state text;
BEGIN
 SELECT revision,state INTO head_revision,head_state FROM supervisor.tasks WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND id=NEW.task_id;
 IF head_revision IS DISTINCT FROM NEW.revision OR head_state IS DISTINCT FROM NEW.to_state THEN
  RAISE EXCEPTION 'a task event must match its task' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER task_event_guard BEFORE INSERT ON supervisor.task_events
 FOR EACH ROW EXECUTE FUNCTION supervisor.task_event_guard();

-- The approval audit records the executor's consumption.
ALTER TABLE supervisor.approval_audit DROP CONSTRAINT approval_audit_event_type_check;
ALTER TABLE supervisor.approval_audit ADD CONSTRAINT approval_audit_event_type_check
 CHECK(event_type IN ('proposal.submitted','proposal.read','proposal.decided','proposal.revoked','proposal.expired','proposal.stale','proposal.consumed','mutation.replayed'));

DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['approved_actions','tasks','task_events'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
  EXECUTE format('GRANT SELECT,INSERT ON supervisor.%I TO supervisor_runtime',t);
 END LOOP;
 FOREACH t IN ARRAY ARRAY['approved_actions','task_events'] LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON supervisor.%I FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable()',t||'_immutable',t);
 END LOOP;
END $$;
