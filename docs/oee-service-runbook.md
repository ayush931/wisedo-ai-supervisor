# OEE service runbook (WIS-229)

Scope: the site-scoped OEE service (`/v1/sites/{site}/oee`, `internal/oeeservice`, migration
`029_oee_service.sql`). Structure follows ADR 0002 Sec. 25: each procedure names an owner,
preconditions, commands, expected telemetry, rollback point and a verification query. Owner for
every procedure is the Supervisor backlog owner (Ayush Kumar); the independent reviewer is
Anshul Rathore. No procedure prints or needs a credential value.

Verification queries run as the migration/administrator role, which bypasses RLS. Never run
them with the runtime role expecting cross-tenant results: the runtime role sees only the tenant
set in `supervisor.tenant_id`.

## Telemetry

The service writes bounded structured log events (ADR 0002 Sec. 11, 19). Fields are opaque IDs,
enums, counts and durations; no tenant ID, fact content, policy body or SQL.

| Event | Level | Fields |
| --- | --- | --- |
| `oee run terminal` | info | `run_id`, `status`, `result_status`, `failure_code`, `fact_count`, `duration_ms`, `correlation_id` |
| `oee capacity exhausted` | warn | `open_runs`, `max_open_runs`, `correlation_id` (tenant open-run budget full) |
| `oee execution budget exhausted` | warn | `max_concurrent_executions`, `correlation_id` (this process's execution budget full) |
| `oee run execution failed` | warn | `run_id`, `correlation_id`, `error_kind` (`not_found`, `invalid`, `deadline` or `storage`) |
| `request failed` | warn | `status`, `code`, `correlation_id` for every 5xx problem |

OpenTelemetry metrics and traces are not implemented yet (WIS-229 remains open for them).

## Deploy

Preconditions: `make supervisor-check`, `make supervisor-contracts` and
`make supervisor-integration CASE=WIS-229` pass on the release SHA.

1. Apply migrations with the migration role: `make supervisor-migrate`.
2. Set limits if the defaults do not fit the host (see `docs/oee-foundation.md`):
   `SUPERVISOR_OEE_RUN_DEADLINE`, `SUPERVISOR_OEE_LEASE`, `SUPERVISOR_OEE_MAX_OPEN_RUNS`,
   `SUPERVISOR_OEE_MAX_CONCURRENT_EXECUTIONS`, `SUPERVISOR_OEE_IDEMPOTENCY_RETENTION`. An invalid
   value stops the process at startup.
3. Start the API. The routes answer 503 `OEE_SERVICE_UNAVAILABLE` if the service could not be
   constructed.

Verify:

```sql
SELECT version, sha256 FROM public.supervisor_schema_versions ORDER BY version DESC LIMIT 1;  -- 30 or later
```

## Rollback

Rollback point: a build that still contains migrations 029 and 030 but does not register the OEE
routes. Both migrations are additive and never reversed; their rows are history (rule 02, ADR 0002
Sec. 17).

Do not deploy a build from before 030: `/readyz` runs `migrate.Check`, which reports
"applied migration checksum or sequence mismatch" when the database holds a migration the build
does not know, so that build never becomes ready.

1. Build from the release SHA with the `registerOEEService(...)` line removed from
   `internal/httpapi/api.go` (or revert the feature commit while keeping the 029 and 030
   migration files and their `migrate.go` entries). The routes then return 404.
2. Runs left `ACCEPTED` or `RUNNING` are not executed. When the service returns, they are reaped
   on the next read, cancel or accepted calculation at their site (see "Stuck runs and leases").
   Sealed evidence is untouched.

## Failed migration

All pending migrations are checksummed and applied in a single transaction, so a failed 029
rolls back completely and leaves the database at its previous version. Expected telemetry: the migrate command exits nonzero with a SQLSTATE category.

1. Do not edit `029_oee_service.sql` after it has been applied anywhere: the checksum check will
   refuse the database ("applied migration checksum or sequence mismatch").
2. Fix forward with a new migration number.

## Shutdown

On SIGTERM the API stops accepting requests (10 s budget), then gives accepted OEE runs their own
10 s budget to finish. New calculations are refused with 429 while it drains; replays still answer.
When the drain budget ends, the remaining executions are cancelled and get a short window to record
a terminal outcome before the database pool closes. A run that cannot record one is reaped one lease
later, and the database refuses any late success for it.

## Stuck runs and leases

An executor holds a lease of `SUPERVISOR_OEE_LEASE` (default 15 s) and renews it three times per
lease while it works, never past the run deadline. A run whose executor disappears (crash, restart,
network partition) is therefore reclaimable one lease later, not at its deadline.

Symptom: runs read as `ACCEPTED` or `RUNNING` past their deadline or lease. Expected telemetry: no
`oee run terminal` event for the `run_id`.

```sql
SELECT site_id, status, count(*), min(deadline_at), min(execution_lease_until)
FROM supervisor.oee_calculation_run
WHERE (status = 'ACCEPTED' AND deadline_at <= now())
   OR (status = 'RUNNING' AND (execution_lease_until <= now() OR deadline_at <= now()))
GROUP BY site_id, status;
```

Resolution: no manual UPDATE is needed or allowed; the run guard trigger forbids leaving the state
machine and refuses success after the deadline or lease. Expired runs are reaped, at most 100 per
request, whenever their site serves a run read (`GET /calculations/{run}` reaps that run), a cancel
or an accepted calculation. Reaping writes one `calculation.failed` or `calculation.cancelled`
audit row per run: a run with a cancel request becomes `CANCELLED`, otherwise `FAILED` with
`DEADLINE_EXCEEDED` or `LEASE_EXPIRED`. Expired runs never count against the open-run budget, even
before they are reaped.

A client retry of a calculation (same `Idempotency-Key` and body) whose run is still `ACCEPTED`
dispatches it again; the fenced claim guarantees it runs at most once.

## Capacity exhaustion

Symptom: 429 `CAPACITY_EXHAUSTED` with `Retry-After` on a new calculation (a replay of an
existing key always answers). Two budgets produce it:

- `oee capacity exhausted`: the tenant's live runs reached `SUPERVISOR_OEE_MAX_OPEN_RUNS`. Check
  with the query above (live runs are `ACCEPTED` before their deadline or `RUNNING` under a valid
  lease). Raise the budget only within the measured envelope below.
- `oee execution budget exhausted`: this process is executing
  `SUPERVISOR_OEE_MAX_CONCURRENT_EXECUTIONS` runs (default: one per CPU). Each one holds its
  dataset in memory, so raise it only with the process memory limit in mind, or add replicas.

## Idempotency keys

A key replays its original request for `SUPERVISOR_OEE_IDEMPOTENCY_RETENTION` (default 90 days).
After that the same key may be reused for a new request: the expired row is rebound in place
(migration 030). Physical purge of expired rows belongs to the WIS-290 retention service.

## RLS drift

Every OEE table must have row level security enabled and forced, with the `tenant_scope` policy.

```sql
SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity,
       EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid = c.oid AND p.polname = 'tenant_scope') AS policy
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'supervisor' AND c.relname LIKE 'oee\_%' AND c.relkind = 'r';
```

Any `false` is an incident: stop the API, restore the policy with a new migration and audit reads
for the affected window. `TestMigrationReadbackAndRepeat` fails CI if the forced-RLS table count
changes.

## Evidence integrity and read volume

Evidence reads re-verify the stored SHA-256; a mismatch returns 503 and is never served. Each read
writes an `evidence.read` audit row. Unusual read volume (ADR 0002 Sec. 19):

```sql
SELECT site_id, actor_issuer, actor_subject, count(*)
FROM supervisor.oee_audit_entry
WHERE action = 'evidence.read' AND occurred_at > now() - interval '1 hour'
GROUP BY 1, 2, 3 ORDER BY 4 DESC LIMIT 20;
```

## Evidence retention

Not implemented. The ADR 0002 Sec. 17 retention worker role (`oee_retention_worker`) and its
audited purge do not exist yet. The runtime role cannot delete OEE rows; do not delete them
manually.

## Measured performance envelope

Measured 2026-10-17 at the WIS-229 branch head with
`go test -count=1 -tags 'integration oeeperf' -run TestOEEPerformanceEnvelope -v ./internal/oeeservice`.

Host: AMD Ryzen 7 5800HS (8 cores, 16 threads), 14 GiB RAM, Linux 7.0, Go 1.27.2. PostgreSQL 17.11
ran in a local Docker container on the same host. The application and the database share the
machine, so this is a developer-workstation figure, not a production sizing.

| Scenario | Result |
| --- | --- |
| ADR 0005 dataset (4 facts), 50 sequential accept + execute + read | p50 8.6 ms, p95 11.5 ms, max 18.0 ms |
| Same dataset, 8 concurrent workers x 25 runs | 224 runs/s, p50 31 ms, p95 61 ms |
| Maximum dataset (10,000 facts), 5 sequential runs | p50 443 ms, max 463 ms; about 303 MiB allocated per run |

Finding: a maximum-size dataset allocates about 303 MiB per run (total allocations, not peak
resident memory). With the default budget of 8 open runs per tenant, size the process memory
limit with that in mind. A profile attributes about two thirds of it to the shared canonical
serializer (`marshalCanonical`), which recomputes the dataset digest at acceptance, at calculation
and per source. Optimizing it must keep every digest byte-identical (rule 04), so it is
follow-up work.
