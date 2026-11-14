-- Preserve existing sessions for ordinary reads; NULL is never recent proof.
ALTER TABLE supervisor.browser_sessions ADD COLUMN auth_time timestamptz;
ALTER TABLE supervisor.oidc_logins
    ADD COLUMN reauth_session_hash varchar(64),
    ADD COLUMN reauth_issuer varchar(512),
    ADD COLUMN reauth_subject varchar(255),
    ADD COLUMN reauth_requested_at timestamptz,
    ADD CONSTRAINT complete_reauthentication_intent CHECK (
        (reauth_session_hash IS NULL AND reauth_issuer IS NULL AND reauth_subject IS NULL AND reauth_requested_at IS NULL)
        OR (reauth_session_hash IS NOT NULL AND reauth_issuer IS NOT NULL AND reauth_subject IS NOT NULL AND reauth_requested_at IS NOT NULL)
    );
