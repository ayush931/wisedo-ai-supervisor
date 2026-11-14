-- WIS-276 machine-fault evidence. A run may capture machine_state records (a recorded stoppage
-- on the case line or a machine the approved process places on it) as citable evidence. The
-- server classifies them; a stoppage supports an inspection, never a claim about cause.
ALTER TABLE supervisor.agent_run_refs DROP CONSTRAINT agent_run_refs_kind_check;
ALTER TABLE supervisor.agent_run_refs ADD CONSTRAINT agent_run_refs_kind_check
 CHECK(kind IN ('context','measurement','snapshot','evidence','machine_state'));
