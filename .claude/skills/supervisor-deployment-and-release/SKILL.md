---
name: supervisor-deployment-and-release
description: Local rehearsal and production deployment of the Go Supervisor - compose profiles, environment variables, the separate migration role, the synthetic seed utility, the release manifest, SLOs, observability and backup/restore. Use this skill for any work on deploy/supervisor, the Dockerfile, the Makefile, CI workflows, cmd/supervisor, configuration or anything about how this product is run.
---

# Deployment, operations and release

Covers `deploy/supervisor/`, `deploy/README.md`, `Dockerfile`, `Makefile`, `.github/workflows/`,
`cmd/supervisor/`, `cmd/supervisor-seed/`, `cmd/supervisor-demo-seed/`, `cmd/supervisor-load/`,
`cmd/supervisor-contracts/`, the Dockerfile CP-SAT build stage, `internal/config/`.
Sources: `deploy/supervisor/README.md`, WIS-290 "Build, dependencies and exact command contract",
WIS-281. Governing rules: `.claude/rules/01-go-only-runtime-boundary.md`,
`.claude/rules/09-verification-gates-and-evidence-claims.md`.

## Local rehearsal

Go 1.27, Node 22, Docker Compose v2. Today no Python interpreter and no `uv` are required; a Python
method component added under rule 01 (2026-11-12) ships as its own pinned, offline-built image. All local
services bind loopback.

```bash
cp .env.example .env            # replace every password placeholder; never commit .env
docker compose --env-file .env -f deploy/supervisor/compose.yml up -d postgres keycloak temporal
docker compose --env-file .env -f deploy/supervisor/compose.yml --profile tools run --rm migrate
make supervisor-build
./build/supervisor api          # separate terminals
./build/supervisor worker
make web-dev
```

Set distinct local-only values for `POSTGRES_ADMIN_PASSWORD`, `SUPERVISOR_MIGRATION_PASSWORD`,
`SUPERVISOR_RUNTIME_PASSWORD`, `KEYCLOAK_DB_PASSWORD`, `TEMPORAL_DB_PASSWORD`,
`KEYCLOAK_ADMIN_PASSWORD`, and URL-encode password components in DSNs. Never reuse customer
credentials. Default ports: PostgreSQL 55477, Keycloak 58077, Temporal 57277; subnet
`SUPERVISOR_LOCAL_SUBNET` defaults to 10.253.77.0/24. No command prunes unrelated networks or
containers.

The migration job is a **separate privileged role**; the runtime is never a table owner. Keycloak
`start-dev` and Temporal auto-setup are local-only and are never the production profile.

`go run ./cmd/supervisor-seed` creates a clearly synthetic site, member, Keycloak realm/client and a
`foundation-reviewer` user. It refuses non-local environments and non-loopback endpoints, refuses
HTTP redirects and never prints fixture credentials. It is not customer onboarding or production
bootstrap.

CI runs on a self-hosted workstation runner (`self-hosted, linux, x64`) with its own compose project
`supervisor-ci`, ports 45477/48077/47277/48176 and subnet 10.253.78.0/24 so it never collides with a
developer stack on the same machine.

The foundation workflow runs `make supervisor-check supervisor-build`, `make supervisor-contracts`,
`make supervisor-integration` and `make supervisor-e2e`, and uploads a `supervisor-ci-evidence` artifact
(SHA, run ID, commands, toolchain versions, API/worker logs) even on failure. The secret scan in
`security.yml` uses `git grep` on tracked files and fails on an unexpected exit status; a bare
`! grep` or a path in `--exclude-dir` silently scans nothing useful. Pin every action by commit SHA.

## Production profile

Cloud pilot default: a dedicated customer deployment in one agreed AWS region, Linux container host,
managed PostgreSQL with point-in-time recovery, encrypted versioned S3 objects, Temporal Cloud and
an approved model endpoint. Keycloak, API and worker are application-owned services. Start at one
4-vCPU / 16-GiB host and report measured saturation — that is a sizing hypothesis, not guaranteed
capacity or an HA claim.

Only HTTPS is public; database, admin and metrics endpoints stay private. Secrets come from a secret
manager or instance role, never a checked-in env file. On-prem, air-gapped and multi-region are
separate release qualifications that Docker support does not imply. `make image` builds
`ghcr.io/wisedo-technology/wisedo-ai-supervisor:local` and publishes nothing.

## Dependency and manifest discipline

Freeze exact dependencies in `go.mod`/`go.sum` and `web/package-lock.json`; pin container base images
by digest and CI actions by commit. The release manifest pins the code and image digest, UI build,
package lock and SBOM, schema/migration version, Keycloak realm revision, Temporal workflow versions,
prompt/tool/model/embedding identifiers, source mapping versions and the coverage/policy snapshot,
plus region, domains, endpoints, secret references, IAM roles, backup destination, workload envelope
and retention settings. No credentials in the manifest. No floating model aliases. Generate an SBOM
and license inventory from shipped artifacts; dependency updates rerun full acceptance.

## Reliability targets

Internal pilot SLO (not a contractual SLA): 99.5% successful eligible authenticated API requests over
a rolling 30 days. Interactive non-model API p95 ≤ 750 ms at qualification load. Ingestion durably
acknowledges within 2 s p95; healthy-source processing lag p95 ≤ 60 s. Low-volume sites use an
authenticated synthetic probe in a dedicated test site, reported separately. Burn alerts: page at
14.4× over 5 min and 1 h; warn at 6× over 30 min and 6 h.

After a committed site kill switch no new action may pass the dispatch check; already committed
internal tasks stay visible and reconcile. In-flight external attempts remain explicit unknown, never
assumed canceled.

## Observability

OTEL trace IDs across HTTP, outbox, workflow, model and task. Metrics for request latency and error,
ingestion lag, queue age, workflow retry and stuck states, source freshness, pending approval age,
reconciliation backlog, per-site model usage and budget, and resource saturation. Structured logs
carry IDs and reasons, never secrets or raw customer prompts. High-cardinality tenant/case/asset IDs
belong in access-controlled traces, not unbounded metric labels. Alert on backup failure, capacity
pressure, identity outage, failed audit write and expiring credentials. Test the alerts and the
runbooks, not just dashboards.

## Upgrade

Expand/contract migrations, a backup checkpoint, a read-compatible previous build and workflow
replay/versioning. First run on staging with populated data and waiting approvals, then canary
through internal/shadow capability before the affected customer capability. Document forward-fix
versus rollback; rollback does not reverse a destructive schema migration. A failed smoke or
readiness check demotes the affected capability and pauses dispatch.
