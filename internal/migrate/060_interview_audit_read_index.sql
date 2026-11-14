-- WIS-271 slice 5: polled agent-run reads audit at most once a minute per actor, session and
-- event (onboarding.auditRead). This index serves that NOT EXISTS lookup so a 2-second poll does
-- not scan the site's audit rows. Additive; no data change.
CREATE INDEX interview_audit_actor_event_idx ON supervisor.interview_audit(tenant_id,site_id,session_id,actor_issuer,actor_subject,event_type,created_at);
