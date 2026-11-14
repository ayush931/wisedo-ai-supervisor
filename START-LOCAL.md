# Run wiseDo AI Supervisor locally

Run commands from the repository root. This setup runs PostgreSQL, Keycloak and
Temporal in Docker, plus the Go API/worker and built React UI on the host.
Credentials belong in the ignored `.env` file, never in this guide or Git.

## Start again after the first setup

Start Docker Desktop, then in terminal 1:

```bash
cd /Users/ayush/code/wisedo-ai-supervisor
set -a
source .env
set +a
docker compose --env-file .env -f deploy/supervisor/compose.yml up -d postgres keycloak temporal
./build/supervisor api
```

Wait for Keycloak to finish starting before launching the API. In terminal 2:

```bash
cd /Users/ayush/code/wisedo-ai-supervisor
set -a
source .env
set +a
./build/supervisor worker
```

Keep both terminals open. Visit **<http://127.0.0.1:58176>**. Use `127.0.0.1`
consistently, including for sign-in; the OIDC URLs are configured with that host.

Sign in with any of these five accounts. They all use the same password:
`SUPERVISOR_LOCAL_USER_PASSWORD` in `.env`.

| Username | Name | Site role |
| --- | --- | --- |
| `shift-lead` | Rahul Deshmukh | supervisor |
| `shift-approver` | Ananya Kulkarni | supervisor |
| `operator-1` | Sanjay Patil | operator |
| `operator-2` | Meera Joshi | operator |
| `read-only-viewer` | Kiran Shah | reviewer (read-only UI access) |

The API serves `web/dist`, so a separate frontend server is unnecessary for normal
use. For frontend development, `make web-dev` starts Vite; use the address it prints.

## First setup on a new machine

Prerequisites: Go 1.27 (see `go.mod`), Node.js 22 and npm, Make, and Docker with
Compose. The initial setup on this Mac also built successfully with Node 24.22.1.
No MES, Python runtime, cloud account or AWS training deployment is required.

1. Clone the repository, or update a clean existing checkout:

   ```bash
   git clone https://github.com/Wisedo-Technology/wisedo-ai-supervisor.git
   cd wisedo-ai-supervisor
   ```

   For an existing checkout on `main`, preserve any local work, then use:

   ```bash
   git pull --ff-only origin main
   ```

2. Create configuration only if `.env` does not already exist:

   ```bash
   cp -n .env.example .env
   chmod 600 .env
   ```

   Replace every `replace-...` placeholder with generated local credentials.
   Use distinct passwords for the admin, migration, runtime, Keycloak database,
   Temporal database and Keycloak admin. Set a client secret and demo-user password.
   Keep these pairs consistent:

   | Setting | Must match |
   | --- | --- |
   | Password in `SUPERVISOR_DATABASE_URL` | `SUPERVISOR_RUNTIME_PASSWORD` |
   | Password in `SUPERVISOR_MIGRATION_URL` | `SUPERVISOR_MIGRATION_PASSWORD` |
   | Password in `SUPERVISOR_TEST_ADMIN_URL` | `POSTGRES_ADMIN_PASSWORD` |
   | `SUPERVISOR_LOCAL_ADMIN_PASSWORD` | `KEYCLOAK_ADMIN_PASSWORD` |
   | `SUPERVISOR_LOCAL_ADMIN` | `KEYCLOAK_ADMIN_USERNAME` |

   Set the database path in `SUPERVISOR_TEST_ADMIN_URL` to `/supervisor`, using the
   postgres administrator credentials. The checked-in template currently ends it
   with `/postgres`; the seed utility writes to the migrated Supervisor database,
   so leaving that path unchanged causes `local fixture database transaction failed`.
   CI also uses `/supervisor`.

   Hexadecimal random passwords avoid shell and URL escaping problems. Otherwise,
   URL-encode passwords in connection URLs and quote shell-sensitive `.env` values.
   Changing `.env` passwords after database initialization does not change stored
   database passwords; preserve the existing working credentials.

3. Load configuration and start dependencies:

   ```bash
   set -a
   source .env
   set +a
   docker compose --env-file .env -f deploy/supervisor/compose.yml up -d postgres keycloak temporal
   docker compose --env-file .env -f deploy/supervisor/compose.yml ps
   ```

4. Build and migrate using the host binary:

   ```bash
   make supervisor-build
   ./build/supervisor migrate
   ```

   Migrations use the separate privileged migration URL. API and worker use the
   restricted runtime role. Never substitute the postgres administrator for it.

5. Once Keycloak is ready, create the base local realm, client and site:

   ```bash
   go run ./cmd/supervisor-seed
   ```

   This provisions the `supervisor-local` realm, `supervisor` client and a temporary
   owner fixture. This is a local fixture, not customer enrollment.

6. Start API and worker as shown above. Once the API is running, run the workstation
   seed from another terminal with `.env` loaded:

   ```bash
   make supervisor-demo-seed
   ```

   This adds the two supervisor accounts and two operator accounts, plus synthetic
   SOP, job order, station and current-shift data. It also provisions its pilot
   fixture accounts. The viewer account is `read-only-viewer`, created by this local
   setup. These seed utilities are for demo data and do not produce the clean
   five-account database state described above; the current reset installation was
   rebuilt with only the five listed memberships. Do not rerun the demo seed on that
   clean database unless you want its extra demo data/accounts back.

## Verify and troubleshoot

```bash
curl --fail http://127.0.0.1:58176/livez
curl --fail http://127.0.0.1:58176/readyz
curl --fail http://127.0.0.1:58077/realms/supervisor-local/.well-known/openid-configuration
docker compose --env-file .env -f deploy/supervisor/compose.yml ps
docker compose --env-file .env -f deploy/supervisor/compose.yml logs --tail=100 postgres keycloak temporal
```

`/readyz` checks database role and schema readiness; it does not prove an AI model
or an entire customer workflow is qualified. The worker terminal should report
that its Temporal worker started.

- **Network overlaps:** choose a free `SUPERVISOR_LOCAL_SUBNET` in `.env` before
  retrying. This Mac uses `10.253.79.0/24` because another stack uses the template's
  `10.253.77.0/24`. Leave other Docker stacks alone.
- **Keycloak not ready:** wait for its startup log, then retry seeding/API startup.
- **Port occupied:** inspect the existing listener before changing ports. If you
  change ports, update the connection URLs and OIDC/public-origin settings too.
- **Initial PostgreSQL bootstrap permission failure:** this Mac initially reported
  `010-supervisor.sh: /bin/sh: bad interpreter: Permission denied`. Only when logs
  confirm bootstrap never created any application roles/databases, restart postgres
  and run its script explicitly:

  ```bash
  docker compose --env-file .env -f deploy/supervisor/compose.yml up -d postgres
  docker compose --env-file .env -f deploy/supervisor/compose.yml exec -T postgres sh /docker-entrypoint-initdb.d/010-supervisor.sh
  docker compose --env-file .env -f deploy/supervisor/compose.yml up -d postgres keycloak temporal
  ```

  Run the command only after `postgres` is healthy and before migrations. It creates
  four roles and four databases. The script is not idempotent. Do not run it against a database that is already
  bootstrapped, and do not erase a volume to fix an unexplained startup failure.
- **Stale seeded shortfall evidence:** the fixture's heartbeat expires after about
  120 seconds. See the heartbeat instructions in `deploy/supervisor/README.md` for
  exercising approvals at human speed; startup alone does not maintain telemetry.
- **PDF uploads on this Mac:** the secure PDF child requires Linux/amd64. A
  darwin/arm64 host returns `EXTRACTION_UNAVAILABLE` for PDF parsing. Use a supported
  Linux/amd64 runtime for this feature; do not disable the parser sandbox.

## Update later

Stop API and worker first, preserve local work, then on `main`:

```bash
git pull --ff-only origin main
set -a
source .env
set +a
make supervisor-build
./build/supervisor migrate
```

Restart both processes. Preserve `.env` and the database volume. Applied migrations
are checksummed; do not edit migration history or assume an older binary supports
a newer schema.

## Stop

Press Ctrl+C in both application terminals, then:

```bash
docker compose --env-file .env -f deploy/supervisor/compose.yml down
```

This preserves data. `down -v` deletes the database volume; do not use it for a
normal shutdown.

## AI models and optional native components

The default environment disables model calls, semantic embeddings and approved-task
execution. Running the application does not automatically enable these features.
For real chat, OCR and embeddings, provision compatible model services and configure
their exact model/revision/address pins on both API and worker. See
[the model setup guide](deploy/supervisor/README.md#offline-local-models-optional-models-profile).
Its Docker bridge addresses are documented for Linux host processes; do not assume
those private container addresses are reachable from a macOS host.

Future Vision is a separate optional C++20/OpenCV edge executable; see
[its build and usage guide](src/wisedo_fixture_vision_cpp/README.md). It is not
needed to start the Supervisor, and running it alone does not establish a live
Supervisor intake integration. The optional CP-SAT solver is also separate;
manual shift planning works without it.

## Initial setup verification — 2026-11-12

GitHub `main` was pulled with `git pull --ff-only origin main`, advancing
`94dba21` to `7e3fa57`. Existing unrelated local files were preserved. Generated
credentials were saved only in `.env` with mode 600.

- `make supervisor-build`: passed after the pull (Go 1.27.4, Node 24.22.1).
- `./build/supervisor migrate`: passed against the new local database.
- The original demo setup was later removed with `docker compose down -v` and the
  clean database, Keycloak realm and five requested users were recreated.
- `./build/supervisor migrate`: passed after manually running the PostgreSQL bootstrap
  script. Keycloak and Temporal started after their roles/databases were created.
- API `/auth/login`: HTTP 302 to the local Keycloak realm (OIDC provider available).
- The clean database contains the one local site and five memberships listed above.
- The browser-control service was unavailable during this reset, so browser cookies
  and cache could not be cleared or browser sign-in rechecked.
- `make supervisor-contracts`: passed without contract drift.
- `make supervisor-check`: failed on PDF sandbox-dependent tests in
  `cmd/supervisor`, `internal/onboarding` and `cmd/supervisor-factory-sim` on
  darwin/arm64. This is not a passing full verification gate. Logs were saved to
  `/tmp/wisedo-local-check.log`; build and contract logs use
  `/tmp/wisedo-local-build.log` and `/tmp/wisedo-local-contracts.log`.

AI model services, Future Vision and the optional CP-SAT solver were not configured
in this setup. Local sign-in readiness does not establish model or customer
qualification.
