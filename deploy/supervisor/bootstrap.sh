#!/bin/sh
set -eu
# psql quoted variables preserve secret values as SQL literals.
psql --username "$POSTGRES_USER" --dbname postgres --set ON_ERROR_STOP=1 \
  --set migration_password="$SUPERVISOR_MIGRATION_PASSWORD" \
  --set runtime_password="$SUPERVISOR_RUNTIME_PASSWORD" \
  --set keycloak_password="$KEYCLOAK_DB_PASSWORD" \
  --set temporal_password="$TEMPORAL_DB_PASSWORD" <<'SQL'
CREATE ROLE supervisor_migrator LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD :'migration_password';
CREATE ROLE supervisor_runtime LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD :'runtime_password';
CREATE ROLE keycloak LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD :'keycloak_password';
CREATE ROLE temporal LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD :'temporal_password';
CREATE DATABASE supervisor OWNER supervisor_migrator;
CREATE DATABASE keycloak OWNER keycloak;
CREATE DATABASE temporal OWNER temporal;
CREATE DATABASE temporal_visibility OWNER temporal;
REVOKE CONNECT ON DATABASE supervisor, keycloak, temporal, temporal_visibility FROM PUBLIC;
REVOKE TEMPORARY ON DATABASE supervisor FROM PUBLIC;
GRANT CONNECT ON DATABASE supervisor TO supervisor_migrator, supervisor_runtime;
GRANT CONNECT ON DATABASE keycloak TO keycloak;
GRANT CONNECT ON DATABASE temporal, temporal_visibility TO temporal;
SQL
# WIS-627: pgvector is not a trusted extension, so the superuser creates it once in the
# Supervisor database; migration 075 then only finds it (CREATE EXTENSION IF NOT EXISTS).
# template1 gets it too, after the Keycloak and Temporal databases exist, so a database created
# later on this server (an upgrade or disposable integration-test database migrated by the
# non-superuser migrator) already has it.
for db in supervisor template1; do
psql --username "$POSTGRES_USER" --dbname "$db" --set ON_ERROR_STOP=1 <<'SQL'
CREATE EXTENSION IF NOT EXISTS vector SCHEMA public;
SQL
done
