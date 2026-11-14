-- Existing runs retain their original limit profile; only new, explicitly pinned runs opt in.
ALTER TABLE supervisor.agent_runs
 ADD COLUMN limit_profile_id text NOT NULL DEFAULT 'default',
 ADD COLUMN limit_profile_version integer NOT NULL DEFAULT 1,
 ADD CONSTRAINT agent_limit_profile_valid CHECK (
  limit_profile_version=1 AND (limit_profile_id='default' OR (
   limit_profile_id='wis702-local-qwen35-9b' AND request_kind='case'
   AND provider_name='local-openai-compatible' AND provider_actual
   AND model='unsloth/Qwen3.5-9B-GGUF:Q4_K_M'
   AND model_revision='sha256:03b74727a860a56338e042c4420bb3f04b2fec5734175f4cb9fa853daf52b7e8'
  )));

CREATE FUNCTION supervisor.agent_limit_profile_guard() RETURNS trigger LANGUAGE plpgsql
 SET search_path=pg_catalog,pg_temp AS $$
BEGIN
 IF ROW(NEW.limit_profile_id,NEW.limit_profile_version,NEW.request_kind,NEW.provider_name,NEW.provider_actual,NEW.model,NEW.model_revision)
  IS DISTINCT FROM ROW(OLD.limit_profile_id,OLD.limit_profile_version,OLD.request_kind,OLD.provider_name,OLD.provider_actual,OLD.model,OLD.model_revision) THEN
  RAISE EXCEPTION 'agent model and limit profile are pinned' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER agent_limit_profile_guard BEFORE UPDATE ON supervisor.agent_runs
 FOR EACH ROW EXECUTE FUNCTION supervisor.agent_limit_profile_guard();
