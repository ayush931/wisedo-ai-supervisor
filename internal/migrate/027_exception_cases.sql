-- WIS-273 exception cases. A case is one production-shortfall episode for an order on a line
-- (the line is an asset, line_id, mapped to the context's asset_id). The partial unique index
-- allows one nonterminal episode per key, so duplicate triggers cannot open a second case.
-- Terminal episodes free the key and stay as immutable history. internal/cases holds the
-- lifecycle rules; the triggers below enforce the same transition table in SQL as a fence.

-- A case cites a context by id, order, line and digest together, so it cannot point at a
-- context for another order or line, or carry a digest that context never had.
ALTER TABLE supervisor.plant_contexts ADD CONSTRAINT plant_contexts_case_binding
 UNIQUE(tenant_id,site_id,id,order_id,asset_id,digest);

CREATE TABLE supervisor.cases (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,id uuid NOT NULL,
 line_id uuid NOT NULL,order_id uuid NOT NULL,
 exception_kind text NOT NULL CHECK(exception_kind='production_shortfall'),
 episode integer NOT NULL CHECK(episode>=1),
 previous_case_id uuid,
 revision bigint NOT NULL CHECK(revision>=1),
 context_snapshot_id uuid NOT NULL,
 context_revision integer NOT NULL CHECK(context_revision=1),
 context_digest text NOT NULL CHECK(context_digest ~ '^[0-9a-f]{64}$'),
 owner_user_id uuid,
 state text NOT NULL CHECK(state IN ('detected','investigating','needs_input','proposed','awaiting_approval','executing','observing','resolved','escalated','canceled')),
 detected_at timestamptz NOT NULL,
 latest_window_end timestamptz NOT NULL,
 workflow_id text NOT NULL CHECK(workflow_id = 'case:' || tenant_id::text || ':' || id::text),
 resolution_reason text CHECK(resolution_reason IS NULL OR length(resolution_reason) BETWEEN 1 AND 2000),
 CHECK((state='resolved') = (resolution_reason IS NOT NULL)),
 -- A reopened episode names the episode it follows; the first episode names none.
 CHECK((episode=1) = (previous_case_id IS NULL)),
 PRIMARY KEY(tenant_id,site_id,id),
 UNIQUE(tenant_id,site_id,line_id,order_id,exception_kind,episode),
 UNIQUE(tenant_id,site_id,previous_case_id),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id),
 FOREIGN KEY(tenant_id,site_id,line_id) REFERENCES supervisor.knowledge_entity_heads(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,order_id) REFERENCES supervisor.knowledge_entity_heads(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,context_snapshot_id,order_id,line_id,context_digest)
  REFERENCES supervisor.plant_contexts(tenant_id,site_id,id,order_id,asset_id,digest),
 FOREIGN KEY(tenant_id,site_id,previous_case_id) REFERENCES supervisor.cases(tenant_id,site_id,id)
);
-- At most one nonterminal episode per tenant, site, line, order and exception kind.
CREATE UNIQUE INDEX cases_one_active_episode ON supervisor.cases(tenant_id,site_id,line_id,order_id,exception_kind)
 WHERE state NOT IN ('resolved','canceled');

-- The operational records that triggered the case: 1..100 per case, each a real record of the
-- same tenant and site (composite FK), never a free list of IDs.
CREATE TABLE supervisor.case_trigger_evidence (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,case_id uuid NOT NULL,
 source_id uuid NOT NULL,source_sequence bigint NOT NULL,
 PRIMARY KEY(tenant_id,site_id,case_id,source_id,source_sequence),
 FOREIGN KEY(tenant_id,site_id,case_id) REFERENCES supervisor.cases(tenant_id,site_id,id),
 FOREIGN KEY(tenant_id,site_id,source_id,source_sequence) REFERENCES supervisor.operational_event_records(tenant_id,site_id,source_id,source_sequence)
);

-- Exactly the transitions in internal/cases. Revision 1 is the creation row.
CREATE TABLE supervisor.case_transitions (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,case_id uuid NOT NULL,
 revision bigint NOT NULL CHECK(revision>=1),
 from_state text NOT NULL,
 to_state text NOT NULL,
 event text NOT NULL CHECK(length(event) BETWEEN 1 AND 64),
 reason text CHECK(reason IS NULL OR length(reason) BETWEEN 1 AND 2000),
 actor_issuer text NOT NULL CHECK(length(actor_issuer) BETWEEN 1 AND 255),
 actor_subject text NOT NULL CHECK(length(actor_subject) BETWEEN 1 AND 255),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((revision=1) = (from_state='none')),
 CHECK((from_state,to_state) IN (
  ('none','detected'),
  ('detected','investigating'),('detected','needs_input'),('investigating','needs_input'),
  ('needs_input','investigating'),('investigating','proposed'),('proposed','awaiting_approval'),
  ('awaiting_approval','needs_input'),('awaiting_approval','executing'),('executing','observing'),
  ('observing','resolved'),('escalated','resolved'),('escalated','investigating'),
  ('detected','escalated'),('investigating','escalated'),('needs_input','escalated'),('proposed','escalated'),
  ('awaiting_approval','escalated'),('executing','escalated'),('observing','escalated'),
  ('detected','canceled'),('investigating','canceled'),('needs_input','canceled'),('proposed','canceled'),
  ('awaiting_approval','canceled'),('executing','canceled'),('observing','canceled'),('escalated','canceled'))),
 PRIMARY KEY(tenant_id,site_id,case_id,revision),
 FOREIGN KEY(tenant_id,site_id,case_id) REFERENCES supervisor.cases(tenant_id,site_id,id)
);

-- A new case starts detected at revision 1 with at most 100 trigger records.
CREATE FUNCTION supervisor.case_insert_guard() RETURNS trigger LANGUAGE plpgsql
 SET search_path = pg_catalog, pg_temp AS $$ BEGIN
  IF NEW.state <> 'detected' OR NEW.revision <> 1 THEN
   RAISE EXCEPTION 'a case starts detected at revision 1' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
 END $$;
CREATE TRIGGER cases_insert_guard BEFORE INSERT ON supervisor.cases
 FOR EACH ROW EXECUTE FUNCTION supervisor.case_insert_guard();

-- Every case update is one recorded transition: revision + 1, a matching transition row,
-- identity columns fixed, terminal episodes frozen, and the context replaced only when a
-- clarification or resume returns the case to investigating.
CREATE FUNCTION supervisor.case_update_guard() RETURNS trigger LANGUAGE plpgsql
 SET search_path = pg_catalog, pg_temp AS $$ BEGIN
  IF OLD.state IN ('resolved','canceled') THEN
   RAISE EXCEPTION 'terminal case episodes are immutable' USING ERRCODE='42501';
  END IF;
  IF (NEW.tenant_id,NEW.site_id,NEW.id,NEW.line_id,NEW.order_id,NEW.exception_kind,NEW.episode,NEW.previous_case_id,NEW.detected_at,NEW.workflow_id)
     IS DISTINCT FROM
     (OLD.tenant_id,OLD.site_id,OLD.id,OLD.line_id,OLD.order_id,OLD.exception_kind,OLD.episode,OLD.previous_case_id,OLD.detected_at,OLD.workflow_id) THEN
   RAISE EXCEPTION 'case identity is immutable' USING ERRCODE='42501';
  END IF;
  IF NEW.revision <> OLD.revision + 1 THEN
   RAISE EXCEPTION 'case revision must advance by one' USING ERRCODE='40001';
  END IF;
  IF NOT EXISTS (SELECT 1 FROM supervisor.case_transitions t
     WHERE t.tenant_id=NEW.tenant_id AND t.site_id=NEW.site_id AND t.case_id=NEW.id
       AND t.revision=NEW.revision AND t.from_state=OLD.state AND t.to_state=NEW.state) THEN
   RAISE EXCEPTION 'case update has no matching transition' USING ERRCODE='23514';
  END IF;
  IF (NEW.context_snapshot_id,NEW.context_digest) IS DISTINCT FROM (OLD.context_snapshot_id,OLD.context_digest)
     AND NOT (NEW.state='investigating' AND OLD.state IN ('needs_input','escalated')) THEN
   RAISE EXCEPTION 'the context changes only on clarification or resume' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
 END $$;
CREATE TRIGGER cases_update_guard BEFORE UPDATE ON supervisor.cases
 FOR EACH ROW EXECUTE FUNCTION supervisor.case_update_guard();

-- A transition row must continue the case's history: revision 1 for a new case, otherwise
-- exactly the next revision from the case's current state.
CREATE FUNCTION supervisor.case_transition_guard() RETURNS trigger LANGUAGE plpgsql
 SET search_path = pg_catalog, pg_temp AS $$
 DECLARE c supervisor.cases%ROWTYPE;
 BEGIN
  SELECT * INTO c FROM supervisor.cases WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND id=NEW.case_id FOR UPDATE;
  IF NEW.revision = 1 THEN
   IF c.revision <> 1 OR c.state <> 'detected' THEN
    RAISE EXCEPTION 'creation transition must match a new case' USING ERRCODE='23514';
   END IF;
  ELSIF NEW.revision <> c.revision + 1 OR NEW.from_state <> c.state THEN
   RAISE EXCEPTION 'transition does not continue the case history' USING ERRCODE='40001';
  END IF;
  RETURN NEW;
 END $$;
CREATE TRIGGER case_transitions_continuity BEFORE INSERT ON supervisor.case_transitions
 FOR EACH ROW EXECUTE FUNCTION supervisor.case_transition_guard();
CREATE TRIGGER case_transitions_immutable BEFORE UPDATE OR DELETE ON supervisor.case_transitions
 FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();

-- 1..100 trigger records per case, checked at commit so the case and its evidence can be
-- written in one transaction; evidence is append-only.
CREATE FUNCTION supervisor.case_evidence_count_guard() RETURNS trigger LANGUAGE plpgsql
 SET search_path = pg_catalog, pg_temp AS $$
 DECLARE n integer;
 BEGIN
  SELECT count(*) INTO n FROM supervisor.case_trigger_evidence
   WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND case_id=NEW.id;
  IF n NOT BETWEEN 1 AND 100 THEN
   RAISE EXCEPTION 'a case needs 1 to 100 trigger records, found %', n USING ERRCODE='23514';
  END IF;
  RETURN NULL;
 END $$;
CREATE CONSTRAINT TRIGGER cases_trigger_evidence_count AFTER INSERT ON supervisor.cases
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION supervisor.case_evidence_count_guard();
CREATE TRIGGER case_trigger_evidence_immutable BEFORE UPDATE OR DELETE ON supervisor.case_trigger_evidence
 FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();

DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['cases','case_transitions','case_trigger_evidence'] LOOP
  EXECUTE format('ALTER TABLE supervisor.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE supervisor.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON supervisor.%I USING(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''supervisor.tenant_id'',true),'''')::uuid)',t);
  EXECUTE format('REVOKE ALL ON supervisor.%I FROM PUBLIC,supervisor_runtime',t);
 END LOOP;
 GRANT SELECT,INSERT,UPDATE(state,revision,latest_window_end,resolution_reason,owner_user_id,context_snapshot_id,context_digest) ON supervisor.cases TO supervisor_runtime;
 GRANT SELECT,INSERT ON supervisor.case_transitions TO supervisor_runtime;
 GRANT SELECT,INSERT ON supervisor.case_trigger_evidence TO supervisor_runtime;
END $$;
