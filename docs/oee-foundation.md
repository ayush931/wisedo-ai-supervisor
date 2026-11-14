# Existing OEE foundation

> This is the retained OEE component, not the complete AI Supervisor runtime.
> Product scope is governed by [the independent architecture](independent-supervisor.md).
> Earlier MES integration decisions apply only to that optional integration.

## Component quickstart (local)

```bash
cp .env.example .env        # local-only placeholders, never commit .env
make install                # install frontend web dependencies
make check                  # format, lint, go vet, tests, audit
docker compose up -d postgres redis
go run ./cmd/supervisor     # runs independent Go AI Supervisor + OEE endpoints
```

Key endpoints: `POST /v1/oee/calculations`, `GET /v1/oee/calculations/{runId}`,
`POST /v1/oee/calculations/{runId}:cancel`, `GET /v1/oee/evidence/{evidenceId}`,
`GET /health/live`, `GET /health/ready`. All `/v1/oee/*` require verified
`RequestContext` (401 fail-closed in foundation, OIDC/JWKS in production) and
emit bounded RFC 9457 `application/problem+json` errors.

## OEE service (WIS-229)

The site-scoped service lives under `/v1/sites/{site}/oee` (ADR 0002 Sec. 8, rule 07). The
legacy `/v1/oee/*` routes above stay fail-closed (503).

| Route | Purpose | Who |
| --- | --- | --- |
| `POST /calculations` | Verify an `OeeDatasetV1`, select the one published policy effective for the window, reserve an `ACCEPTED` run (202). Calculation runs after commit. | owner, supervisor |
| `GET /calculations/{run}` | Run state: `ACCEPTED`, `RUNNING`, `SUCCEEDED`, `FAILED`, `CANCELLED` | any member |
| `POST /calculations/{run}:cancel` | Cancel an accepted run, or raise the cancel generation of a running one | owner, supervisor |
| `GET /evidence/{evidence}` | Sealed `CANDIDATE` evidence, digest re-verified, read audited | any member |
| `GET /policies/{policy}` | Policy, immutable revisions, current publication; `ETag` is `row_version` | any member |
| `POST /policies/{policy}/revisions` | Append an immutable policy revision | owner, supervisor |
| `POST /policies/{policy}/revisions/{revision}:publish` | Publish under `If-Match` | supervisor |

Every mutation takes `Idempotency-Key`. Errors are RFC 9457 problems with a correlation ID:
422 invalid contract, 413 over a dataset limit, 409 idempotency, lifecycle, missing/ambiguous
policy (`POLICY_NOT_READY`) or overlapping publication, 412 stale `If-Match` or expected policy
revision, 429 with `Retry-After` when the per-tenant open-run budget is exhausted.

Persistence is migration `029_oee_service.sql`, the ADR 0002 Sec. 6.2 tables with `site_id`
added, plus `030_oee_lease_and_key_expiry.sql` (lease renewal and expired-key rebinding). Facts are never stored: a run keeps the dataset digest, and `oee_run_source` keeps one
minimized row per source (identity, snapshot/watermark, contribution digest, fact count). The
run state machine, success sealing under the database clock and append-only tables are
enforced by triggers as well as by the conditional updates in `internal/oeeservice`.

Limits (provisional until the performance envelope is measured):
`SUPERVISOR_OEE_RUN_DEADLINE` (default `60s`, max `1h`), `SUPERVISOR_OEE_LEASE` (default `15s`,
at most the deadline), `SUPERVISOR_OEE_MAX_OPEN_RUNS` (default `8` live runs per tenant),
`SUPERVISOR_OEE_MAX_CONCURRENT_EXECUTIONS` (default one per CPU, per process) and
`SUPERVISOR_OEE_IDEMPOTENCY_RETENTION` (default `2160h`).
Verification: `make supervisor-integration CASE=WIS-229`.

Contract compatibility: `make supervisor-contracts` also generates TypeScript consumer types
(`contracts/typescript/oee.ts`). `TestOEEServiceHTTPPostgres` validates real responses against
the JSON Schemas and, with `SUPERVISOR_PROVIDER_FIXTURE_OUT=web/src/contracts/provider-fixtures.ts`,
records them as a TypeScript module that `tsc` (in `make supervisor-check`) typechecks against
the generated types.

Rollback: deploy a build that keeps migrations 029 and 030 but drops the `registerOEEService`
call. A build from before 030 is not a valid rollback, because its readiness check refuses a database
with an unknown migration. Procedures: [OEE service runbook](oee-service-runbook.md).

## Layout

- `internal/oee/` — pure logic, zero I/O and domain contracts:
  - `values.go` — exact rational arithmetic (`*big.Rat`), canonical hashing (`SHA-256`), half-open UTC intervals.
  - `timeline.go` — timeline segment ordering, collision detection.
  - `readiness.go` — readiness state machine (`READY`, `LIMITED`, `NOT_READY`, `INVALID`) and reason codes.
  - `service.go` — loss classification, downtime cost calculation, evidence sealing.
  - `contracts.go` — strictly validated Go DTOs (dataset, policy, calculation, evidence).
- `internal/httpapi/oee.go` — Go HTTP API handlers integrated into standard `net/http` router with RFC 9457 problem details.
- Note: Legacy Python scaffolding (`src/wisedo_plantops_oee/`, `tests/`, `migrations/`, `schemas/`) has been decommissioned in favor of the pure Go runtime.

## Production notes

- Go 1.27 (`go.mod`), PostgreSQL 16+. Forced RLS with tenant context set transaction-locally as
  `supervisor.tenant_id` (`internal/store`) is enforced; missing tenant context denies by default.
- Settings validate once at startup and fail closed in production
  (no placeholder secrets, pinned HTTPS issuer, explicit egress allowlist).
- Every tenant table uses composite `(tenant_id, …)` keys; runtime role is
  non-owner without `BYPASSRLS`; missing tenant context denies by default.
- Evidence is immutable and candidate-only; the MES-owned authority router
  (WIS-231) decides acceptance/cutover/rollback. No partial `READY` ever seals.
- See `SECURITY.md`, `deploy/README.md`, `Makefile`, `Dockerfile`, and
  `docker-compose.yml` (loopback-only dev deps) for hardening details.
