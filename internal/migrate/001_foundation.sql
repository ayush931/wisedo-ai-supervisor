SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;


CREATE SCHEMA supervisor;



CREATE FUNCTION supervisor.guard_command_identity() RETURNS trigger
    LANGUAGE plpgsql
    AS $$ BEGIN
        IF ROW(NEW.id,NEW.tenant_id,NEW.site_id,NEW.actor_issuer,NEW.actor_subject,
               NEW.idempotency_key,NEW.payload_hash,NEW.payload,NEW.kind,NEW.created_at)
           IS DISTINCT FROM
           ROW(OLD.id,OLD.tenant_id,OLD.site_id,OLD.actor_issuer,OLD.actor_subject,
               OLD.idempotency_key,OLD.payload_hash,OLD.payload,OLD.kind,OLD.created_at) THEN
            RAISE EXCEPTION 'Command identity and payload are immutable' USING ERRCODE='42501';
        END IF;
        IF OLD.status <> 'accepted' AND ROW(NEW.status,NEW.completed_at)
            IS DISTINCT FROM ROW(OLD.status,OLD.completed_at) THEN
            RAISE EXCEPTION 'Terminal command cannot change' USING ERRCODE='42501';
        END IF;
        RETURN NEW;
        END $$;



CREATE FUNCTION supervisor.guard_membership_write() RETURNS trigger
    LANGUAGE plpgsql
    AS $$ BEGIN
        IF NOT pg_has_role(current_user,
            (SELECT relowner FROM pg_class WHERE oid='supervisor.memberships'::regclass),
            'MEMBER') THEN
            RAISE EXCEPTION 'Membership provisioning requires administrator' USING ERRCODE='42501';
        END IF;
        RETURN NEW;
        END $$;


SET default_tablespace = '';

SET default_table_access_method = heap;


CREATE TABLE supervisor.audit_events (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    site_id uuid NOT NULL,
    command_id uuid NOT NULL,
    actor_issuer text NOT NULL,
    actor_subject text NOT NULL,
    event_type text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY supervisor.audit_events FORCE ROW LEVEL SECURITY;



CREATE TABLE supervisor.browser_sessions (
    token_hash character varying(64) NOT NULL,
    subject character varying(255) NOT NULL,
    issuer character varying(512) NOT NULL,
    csrf_token character varying(128) NOT NULL,
    created_at timestamp with time zone NOT NULL,
    last_seen_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL
);



CREATE TABLE supervisor.commands (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    site_id uuid NOT NULL,
    actor_issuer text NOT NULL,
    actor_subject text NOT NULL,
    idempotency_key text NOT NULL,
    payload_hash text NOT NULL,
    payload jsonb NOT NULL,
    kind text NOT NULL,
    status text DEFAULT 'accepted'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    completed_at timestamp with time zone,
    CONSTRAINT commands_idempotency_key_check CHECK (((length(idempotency_key) >= 1) AND (length(idempotency_key) <= 128))),
    CONSTRAINT commands_kind_check CHECK ((kind = 'foundation_probe'::text)),
    CONSTRAINT commands_status_check CHECK ((status = ANY (ARRAY['accepted'::text, 'completed'::text, 'rejected'::text])))
);

ALTER TABLE ONLY supervisor.commands FORCE ROW LEVEL SECURITY;



CREATE TABLE supervisor.identity_audit (
    id character varying(64) NOT NULL,
    event_type character varying(64) NOT NULL,
    subject character varying(255),
    created_at timestamp with time zone NOT NULL
);



CREATE TABLE supervisor.memberships (
    tenant_id uuid NOT NULL,
    site_id uuid NOT NULL,
    issuer text NOT NULL,
    subject text NOT NULL,
    role text NOT NULL,
    active boolean DEFAULT true NOT NULL,
    CONSTRAINT memberships_role_check CHECK ((role = ANY (ARRAY['owner'::text, 'supervisor'::text, 'operator'::text, 'reviewer'::text])))
);

ALTER TABLE ONLY supervisor.memberships FORCE ROW LEVEL SECURITY;



CREATE TABLE supervisor.oidc_logins (
    state_hash character varying(64) NOT NULL,
    browser_hash character varying(64) NOT NULL,
    nonce character varying(128) NOT NULL,
    verifier character varying(128) NOT NULL,
    expires_at timestamp with time zone NOT NULL
);



CREATE TABLE supervisor.outbox (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    site_id uuid NOT NULL,
    command_id uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    delivered_at timestamp with time zone
);

ALTER TABLE ONLY supervisor.outbox FORCE ROW LEVEL SECURITY;



CREATE TABLE supervisor.sites (
    tenant_id uuid NOT NULL,
    id uuid NOT NULL,
    name text NOT NULL
);

ALTER TABLE ONLY supervisor.sites FORCE ROW LEVEL SECURITY;



CREATE TABLE supervisor.tenants (
    id uuid NOT NULL,
    name text NOT NULL
);

ALTER TABLE ONLY supervisor.tenants FORCE ROW LEVEL SECURITY;



ALTER TABLE ONLY supervisor.audit_events
    ADD CONSTRAINT audit_events_pkey PRIMARY KEY (id);



ALTER TABLE ONLY supervisor.browser_sessions
    ADD CONSTRAINT browser_sessions_pkey PRIMARY KEY (token_hash);



ALTER TABLE ONLY supervisor.commands
    ADD CONSTRAINT commands_pkey PRIMARY KEY (id);



ALTER TABLE ONLY supervisor.commands
    ADD CONSTRAINT commands_tenant_id_site_id_actor_issuer_actor_subject_idemp_key UNIQUE (tenant_id, site_id, actor_issuer, actor_subject, idempotency_key);



ALTER TABLE ONLY supervisor.commands
    ADD CONSTRAINT commands_tenant_id_site_id_id_key UNIQUE (tenant_id, site_id, id);



ALTER TABLE ONLY supervisor.identity_audit
    ADD CONSTRAINT identity_audit_pkey PRIMARY KEY (id);



ALTER TABLE ONLY supervisor.memberships
    ADD CONSTRAINT memberships_pkey PRIMARY KEY (tenant_id, site_id, issuer, subject);



ALTER TABLE ONLY supervisor.oidc_logins
    ADD CONSTRAINT oidc_logins_pkey PRIMARY KEY (state_hash);



ALTER TABLE ONLY supervisor.outbox
    ADD CONSTRAINT outbox_command_id_key UNIQUE (command_id);



ALTER TABLE ONLY supervisor.outbox
    ADD CONSTRAINT outbox_pkey PRIMARY KEY (id);



ALTER TABLE ONLY supervisor.sites
    ADD CONSTRAINT sites_pkey PRIMARY KEY (tenant_id, id);



ALTER TABLE ONLY supervisor.tenants
    ADD CONSTRAINT tenants_pkey PRIMARY KEY (id);



CREATE INDEX memberships_actor ON supervisor.memberships USING btree (issuer, subject) WHERE active;



CREATE INDEX outbox_pending ON supervisor.outbox USING btree (tenant_id, created_at) WHERE (delivered_at IS NULL);



CREATE TRIGGER command_identity_immutable BEFORE UPDATE ON supervisor.commands FOR EACH ROW EXECUTE FUNCTION supervisor.guard_command_identity();



CREATE TRIGGER membership_admin_only BEFORE UPDATE ON supervisor.memberships FOR EACH ROW EXECUTE FUNCTION supervisor.guard_membership_write();



ALTER TABLE ONLY supervisor.audit_events
    ADD CONSTRAINT audit_events_tenant_id_site_id_command_id_fkey FOREIGN KEY (tenant_id, site_id, command_id) REFERENCES supervisor.commands(tenant_id, site_id, id);



ALTER TABLE ONLY supervisor.commands
    ADD CONSTRAINT commands_tenant_id_site_id_fkey FOREIGN KEY (tenant_id, site_id) REFERENCES supervisor.sites(tenant_id, id);



ALTER TABLE ONLY supervisor.memberships
    ADD CONSTRAINT memberships_tenant_id_site_id_fkey FOREIGN KEY (tenant_id, site_id) REFERENCES supervisor.sites(tenant_id, id);



ALTER TABLE ONLY supervisor.outbox
    ADD CONSTRAINT outbox_tenant_id_site_id_command_id_fkey FOREIGN KEY (tenant_id, site_id, command_id) REFERENCES supervisor.commands(tenant_id, site_id, id);



ALTER TABLE ONLY supervisor.sites
    ADD CONSTRAINT sites_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES supervisor.tenants(id);



CREATE POLICY actor_discovery ON supervisor.memberships FOR SELECT USING ((active AND (issuer = current_setting('supervisor.issuer'::text, true)) AND (subject = current_setting('supervisor.subject'::text, true))));



CREATE POLICY actor_discovery ON supervisor.sites FOR SELECT USING ((EXISTS ( SELECT 1
   FROM supervisor.memberships m
  WHERE ((m.tenant_id = sites.tenant_id) AND (m.site_id = sites.id) AND m.active AND (m.issuer = current_setting('supervisor.issuer'::text, true)) AND (m.subject = current_setting('supervisor.subject'::text, true))))));



ALTER TABLE supervisor.audit_events ENABLE ROW LEVEL SECURITY;


ALTER TABLE supervisor.commands ENABLE ROW LEVEL SECURITY;


ALTER TABLE supervisor.memberships ENABLE ROW LEVEL SECURITY;


ALTER TABLE supervisor.outbox ENABLE ROW LEVEL SECURITY;


ALTER TABLE supervisor.sites ENABLE ROW LEVEL SECURITY;


CREATE POLICY tenant_scope ON supervisor.audit_events USING ((tenant_id = (NULLIF(current_setting('supervisor.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('supervisor.tenant_id'::text, true), ''::text))::uuid));



CREATE POLICY tenant_scope ON supervisor.commands USING ((tenant_id = (NULLIF(current_setting('supervisor.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('supervisor.tenant_id'::text, true), ''::text))::uuid));



CREATE POLICY tenant_scope ON supervisor.memberships USING ((tenant_id = (NULLIF(current_setting('supervisor.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('supervisor.tenant_id'::text, true), ''::text))::uuid));



CREATE POLICY tenant_scope ON supervisor.outbox USING ((tenant_id = (NULLIF(current_setting('supervisor.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('supervisor.tenant_id'::text, true), ''::text))::uuid));



CREATE POLICY tenant_scope ON supervisor.sites USING ((tenant_id = (NULLIF(current_setting('supervisor.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('supervisor.tenant_id'::text, true), ''::text))::uuid));



CREATE POLICY tenant_scope ON supervisor.tenants USING ((id = (NULLIF(current_setting('supervisor.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((id = (NULLIF(current_setting('supervisor.tenant_id'::text, true), ''::text))::uuid));



ALTER TABLE supervisor.tenants ENABLE ROW LEVEL SECURITY;


GRANT USAGE ON SCHEMA supervisor TO supervisor_runtime;



GRANT SELECT,INSERT ON TABLE supervisor.audit_events TO supervisor_runtime;



GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE supervisor.browser_sessions TO supervisor_runtime;



GRANT SELECT,INSERT,UPDATE ON TABLE supervisor.commands TO supervisor_runtime;



GRANT INSERT ON TABLE supervisor.identity_audit TO supervisor_runtime;



GRANT SELECT,UPDATE ON TABLE supervisor.memberships TO supervisor_runtime;



GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE supervisor.oidc_logins TO supervisor_runtime;



GRANT SELECT,INSERT,UPDATE ON TABLE supervisor.outbox TO supervisor_runtime;



GRANT SELECT ON TABLE supervisor.sites TO supervisor_runtime;



GRANT SELECT ON TABLE supervisor.tenants TO supervisor_runtime;
