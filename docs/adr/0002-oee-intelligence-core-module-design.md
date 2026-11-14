# OEE Intelligence Core Module Design

> **Current state (WIS-633, checked 2026-11-03).** The "Runtime target" below (Python 3.12, FastAPI,
> Pydantic, SQLAlchemy, Alembic) was not kept. The module is implemented in Go:
> - contracts and strict validation: `internal/oee/contracts.go` and `contract_validate.go`, with
>   `internal/strictjson`;
> - timeline normalization: `internal/oee/timeline.go`;
> - the A/P/Q/OEE/TEEP kernel and exact decimals: `kernel.go` and `values.go`;
> - readiness, loss tree and cost: `readiness.go`, `loss.go` and `cost.go`;
> - evidence sealing: `evidence_pack.go`;
> - runs, leases and persistence: `internal/oeeservice`;
> - schema: the checksummed SQL migrations in `internal/migrate` (no Alembic).
> The Python package paths, `pyproject.toml` references and `.github/workflows/release.yml` named below
> are design history and are not in this repository. Layering rules (pure kernel, inward dependencies)
> still apply. See the `oee-core-module-design` and `supervisor-oee-kernel` skills.
> The RLS setting written below as `app.tenant_id` is `supervisor.tenant_id` in the code
> (`internal/store`).

> Component scope: this ADR governs the retained OEE component and its optional
> historical MES integration. The independent AI Supervisor product is governed by
> [the current architecture](../independent-supervisor.md); MES is not a prerequisite.

**Status:** implementation contract derived from ADR 0001  
**Scope:** WIS-222 OEE, downtime, and modeled cost-of-loss intelligence core  
**Runtime target:** Python 3.12, FastAPI, strict Pydantic v2 models, SQLAlchemy 2 async, Alembic, PostgreSQL 16

**Verified MES baseline:** commit `8234ab0c37c2877a7580fe7af412bec83a5876d2` (`pyproject.toml` and `services/insights-svc/pyproject.toml`)

## 1. Purpose and governing decisions

This document defines how the retained `wisedo_plantops_oee` modules are to be built. It is subordinate to the independent product architecture in [docs/independent-supervisor.md](../independent-supervisor.md). Where this document does not define a formula, threshold, unit policy, or precedence rule, the implementation must remain unavailable until the owning product decision is approved. A developer must not fill an unresolved rule with a convenient default.

The service is an independent, tenant-scoped calculation core. It receives one bounded, versioned `OeeDatasetV1`, selects one published `CalculationPolicyV1`, runs a deterministic calculation pipeline, and seals one immutable `OeeEvidencePackV1`. MES owner services remain authoritative for source facts. Downstream consumers consume evidence; they do not calculate OEE.

The verified implementation contract is versioned REST/JSON over HTTPS, described by OpenAPI 3.1. Python 3.12 is the single supported minor and is pinned in `.python-version`, `pyproject.toml`, the runtime image, and CI. The async Redis client (`redis[hiredis]`) is available only as an infrastructure adapter for explicitly approved caches. Evidence caching remains disabled unless snapshot identity, authorization freshness, policy digest, and tenant isolation are proven. The workload is dominated by network, database, validation, and serialization I/O, not hard-real-time or PLC execution. C++ would add delivery and memory-safety cost without solving that dominant problem. Go or Rust are reserved for a later isolated worker and require a reproducible benchmark proving Python cannot meet an approved throughput or memory SLO. This baseline must be refreshed against the pinned MES commit before integration/cutover.

The following rules are non-negotiable:

- No connection to an MES database, PLC, broker, owner event stream, or caller-selected URL.
- No duplicate event consumer for a source already projected by MES.
- No mutation of MES state and no machine-control behavior.
- No caller-selected tenant, policy, authority provider, or network destination.
- No silent gap filling, last-write-wins conflict resolution, unit inference, percentage clamping, or averaging of percentages.
- No partial `READY` result. A failed, cancelled, stale, or fenced-out run cannot publish evidence.
- Pure domain modules cannot read a clock, database, network, environment, random source, or mutable singleton.
- The finalized authority registry and cutover decision remain in WIS-231/MES. This service does not decide that it is authoritative.

## 2. Module responsibilities

### 2.1 Owned by this repository

1. Strict versioned contracts for datasets, policies, and evidence packs.
2. Validation of source identity, provenance, bounds, units, time windows, and policy references.
3. Deterministic normalization of source facts into a canonical timeline.
4. Readiness evaluation and safe reason-code precedence.
5. Finalized Availability, Performance, Quality, OEE, TEEP, downtime/loss classification, and modeled cost only after the relevant product rules are approved.
6. Immutable policy revisions and publication state.
7. Calculation-run reservation, idempotency, cancellation, fencing, terminal status, and evidence metadata.
8. Read-only evidence retrieval with tenant isolation and provenance.
9. Safe metrics, traces, audit records, migration scripts, and contract fixtures.

### 2.2 Explicitly outside this repository

- Historian, production, quality, master-data, or MES persistence models.
- Owner-service ingestion, event offsets, source reconciliation, or live accumulation.
- MES finalized-authority routing, shadow comparison, cutover, and rollback flags.
- Dashboards, presentation layouts, file export, scheduling, and delivery.
- PLC, HMI, SCADA, OPC UA, MQTT, Modbus, or safety commands.
- Machine learning and predictive-maintenance inference.

## 3. Package structure

Use a modular monolith with dependencies directed inward. Domain and application code must not import FastAPI, SQLAlchemy, HTTP clients, or storage SDKs.

```text
src/wisedo_plantops_oee/
    settings.py               # pydantic-settings startup validation
    domain/
        values.py             # UUIDs, intervals, units, decimals, digests
        contracts.py          # immutable dataset, policy, and evidence values
        readiness.py          # pure status and reason precedence
        timeline.py           # canonical interval/event model
        calculation.py        # pure A/P/Q/OEE/TEEP kernel
        downtime.py           # pure taxonomy and loss classification
        cost.py               # pure governed rate/cost model
        evidence.py           # reconciliation and evidence-pack construction
    application/
        calculate.py          # orchestration and transaction sequencing
        policy_commands.py    # validate, publish, and policy reads
        evidence_queries.py   # authorized evidence reads
        cancellation.py       # cancellation and fence checks
        ports.py              # repository, clock, and storage interfaces
    contracts/
        errors.py             # RFC 9457-safe problem details
        v1/                   # Pydantic models and generated JSON Schema
            common.py         # enums, intervals, Decimal string serializers
            context.py        # RequestContext (JWT claims only; no body tenant)
            dataset.py        # OeeDatasetV1
            policy.py         # CalculationPolicyV1
            evidence.py       # OeeEvidencePackV1
            requests.py       # CalculationRequestV1
    adapters/
        http/
            api.py            # FastAPI routes and RFC 9457 error mappings
            auth.py           # verified identity and RequestContext extraction
        db/
            models.py         # SQLAlchemy mappings only
            repositories.py   # tenant-scoped persistence
            transactions.py   # RLS context (set_config) and unit of work
        cache/
            redis.py          # Async Redis client with tenant namespacing
        sources/
            dataset_verifier.py # verifies already assembled OeeDatasetV1
    observability/
        logging.py            # structlog with secret scrubbing
        metrics.py            # OpenTelemetry metrics with bounded cardinality
        tracing.py            # OpenTelemetry distributed tracing
```

`adapters/sources` does not contain MES database clients or a new owner-event consumer. The WIS-231 adapter assembles the dataset outside this repository and calls the versioned HTTPS boundary.

## 4. Core domain model

### 4.1 Request context

`RequestContext` is created only after token validation and contains the verified tenant, actor reference, permissions, authorized scope, trace ID, and request deadline. Tenant identity is never accepted from JSON, query parameters, headers, or source facts.

### 4.2 Dataset contract

`OeeDatasetV1` is immutable and bounded. It contains dataset identity and schema ID; tenant-independent source identities plus owner and revision for each fact; a half-open UTC window `[start, end)`; explicit civil-time zone and calendar references where required; source watermarks/snapshot identities and occurred/recorded timestamps; explicit units and signal/data quality; bounded facts for equipment state, counters, production, quality, downtime, and master-data references; and a canonical provenance digest.

The dataset is not a database dump. Source contributions retained by this service are minimized to the evidence contract and digests. Exact source bodies are never stored in PostgreSQL, logs, traces, or error responses.

### 4.3 Policy contract

`CalculationPolicyV1` is an immutable, published policy revision selected by the service for the authenticated tenant/scope/window. It references or contains the approved versions of time/calendar and DST behavior, readiness and reason-code precedence, normalization and overlap precedence, Availability/Performance/Quality/OEE/TEEP rules, downtime taxonomy and loss hierarchy, unit/rounding/scale rules, modeled-cost rates/currency/effective dates/assumptions, and output/retention limits.

A normal calculation request cannot choose a draft or arbitrary historical policy. An expected policy revision may be supplied only as a concurrency precondition; a mismatch fails with a conflict.

### 4.4 Evidence contract

`OeeEvidencePackV1` is terminal and immutable. It includes evidence identity, schema version, tenant scope, requested window, readiness status, ordered bounded reason codes, exclusions and assumptions, truth state, metrics only where status and policy permit, numerators/denominators/durations/quantities needed for reconciliation, dataset/policy/calculation-code/taxonomy/rate digests, source snapshot/watermark provenance, and run identity.

`READY` describes evidence quality, not accepted authority. A ready candidate is not a finalized accepted result until WIS-231 routes it through its audited authority registry.

## 5. Calculation pipeline

The application service performs orchestration; the following stages are deterministic and side-effect free:

1. **Admission:** authenticate, authorize, enforce body/fact/window/deadline bounds, and reject caller-supplied tenant identity.
2. **Contract verification:** validate strict schema, identities, revisions, units, time range, provenance, source scope, and canonical digest.
3. **Policy selection:** load exactly one published effective policy under the tenant and scope. Missing, ambiguous, or stale policy is `NOT_READY`.
4. **Readiness evaluation:** detect missing, stale, duplicate, conflicting, cross-tenant, ambiguous, or unsupported facts. Do not repair them.
5. **Normalization:** convert facts into a canonical timeline using explicit timezone, calendar, interval, counter, quality, and revision rules.
6. **Calculation:** compute approved components from explicit contributions. Zero denominators, counter resets, overlaps, gaps, and out-of-range values follow policy reason codes; they are never hidden by clamping.
7. **Loss and cost:** classify downtime through the versioned hierarchy and apply only effective, unit-compatible, currency-explicit rates.
8. **Evidence construction:** reconcile every output to inputs, policy, code version, exclusions, and provenance.
9. **Atomic sealing:** commit one terminal run and immutable evidence record, or commit one safe failure/cancellation outcome.

The kernel must be replayable with the same dataset, policy, and code version. It receives time, rounding, unit conversion, taxonomy, and rate values as explicit arguments. It must not call `datetime.now()`, access environment configuration, or query persistence.

## 6. PostgreSQL persistence model

PostgreSQL stores governed OEE state, not owner data. Every tenant-owned table uses `tenant_id` in primary keys, unique constraints, and foreign keys. IDs are opaque UUIDs. JSONB is used only for already-validated, closed contract documents or evidence envelopes; it is not an escape hatch for arbitrary fields.

### 6.1 Tables

| Table                       | Purpose                                                             | Mutability                                        |
| --------------------------- | ------------------------------------------------------------------- | ------------------------------------------------- |
| `oee_policy`                | Stable policy identity and current publication pointer              | Pointer changes only under optimistic concurrency |
| `oee_policy_revision`       | Full immutable policy document, digest, and revision number         | Insert-only                                       |
| `oee_policy_publication`    | Append-only publish/withdraw/rollback decision                      | Insert-only                                       |
| `oee_calculation_run`       | Idempotency, lifecycle, deadline, cancellation, and execution fence | Controlled state transitions                      |
| `oee_run_source`            | Minimized source snapshot and contribution provenance               | Insert once per run/source                        |
| `oee_evidence_pack`         | Immutable evidence metadata and validated pack document             | Insert-only after terminal seal                   |
| `oee_candidate_publication` | Internal candidate publication identity for comparison              | Insert-only                                       |
| `oee_idempotency_key`       | Request fingerprint and terminal response replay                    | Expires, then is purged                           |
| `oee_audit_entry`           | Append-only security and governance audit                           | Insert-only                                       |

The finalized authority registry is not an OEE-core table. It is owned by MES/WIS-231, as are cutover and rollback decisions.

### 6.2 Schema sketch

This is the required relational shape. Exact column lengths, enum values, and policy-specific JSON schemas must be frozen with the v1 contracts before migrations are generated.

```sql
CREATE TYPE oee_run_status AS ENUM
    ('ACCEPTED', 'RUNNING', 'SUCCEEDED', 'FAILED', 'CANCELLED');
CREATE TYPE oee_result_status AS ENUM
    ('READY', 'LIMITED', 'NOT_READY', 'INVALID');
CREATE TYPE oee_truth_state AS ENUM
    ('CANDIDATE', 'SUPERSEDED');

CREATE TABLE oee_policy (
    tenant_id uuid NOT NULL,
    policy_id uuid NOT NULL,
    policy_code text NOT NULL,
    current_publication_id uuid,
    row_version bigint NOT NULL DEFAULT 1 CHECK (row_version > 0),
    created_at timestamptz NOT NULL,
    created_by_ref text NOT NULL,
    PRIMARY KEY (tenant_id, policy_id),
    UNIQUE (tenant_id, policy_code)
);

CREATE TABLE oee_policy_revision (
    tenant_id uuid NOT NULL,
    policy_revision_id uuid NOT NULL,
    policy_id uuid NOT NULL,
    revision_no integer NOT NULL CHECK (revision_no > 0),
    schema_version integer NOT NULL CHECK (schema_version = 1),
    effective_start timestamptz NOT NULL,
    effective_end timestamptz,
    policy_json jsonb NOT NULL CHECK (jsonb_typeof(policy_json) = 'object'),
    policy_sha256 bytea NOT NULL CHECK (octet_length(policy_sha256) = 32),
    created_at timestamptz NOT NULL,
    created_by_ref text NOT NULL,
    PRIMARY KEY (tenant_id, policy_revision_id),
    UNIQUE (tenant_id, policy_id, policy_revision_id),
    UNIQUE (tenant_id, policy_id, revision_no),
    UNIQUE (tenant_id, policy_id, policy_sha256),
    FOREIGN KEY (tenant_id, policy_id)
        REFERENCES oee_policy (tenant_id, policy_id),
    CHECK (effective_end IS NULL OR effective_start < effective_end)
);

CREATE TABLE oee_policy_publication (
    tenant_id uuid NOT NULL,
    publication_id uuid NOT NULL,
    policy_id uuid NOT NULL,
    policy_revision_id uuid NOT NULL,
    publication_no bigint NOT NULL CHECK (publication_no > 0),
    action text NOT NULL CHECK (action IN ('PUBLISH', 'WITHDRAW', 'ROLLBACK')),
    previous_publication_id uuid,
    published_at timestamptz NOT NULL,
    published_by_ref text NOT NULL,
    PRIMARY KEY (tenant_id, publication_id),
    UNIQUE (tenant_id, policy_id, publication_no),
    UNIQUE (tenant_id, policy_id, publication_id),
    FOREIGN KEY (tenant_id, policy_id, policy_revision_id)
        REFERENCES oee_policy_revision (tenant_id, policy_id, policy_revision_id),
    FOREIGN KEY (tenant_id, policy_id, previous_publication_id)
        REFERENCES oee_policy_publication (tenant_id, policy_id, publication_id)
);

ALTER TABLE oee_policy
    ADD CONSTRAINT oee_policy_current_publication_fk
    FOREIGN KEY (tenant_id, policy_id, current_publication_id)
    REFERENCES oee_policy_publication (tenant_id, policy_id, publication_id)
    DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE oee_calculation_run (
    tenant_id uuid NOT NULL,
    run_id uuid NOT NULL,
    request_key text NOT NULL,
    dataset_id uuid NOT NULL,
    policy_id uuid NOT NULL,
    policy_revision_id uuid NOT NULL,
    policy_digest bytea NOT NULL CHECK (octet_length(policy_digest) = 32),
    requested_by_ref text NOT NULL,
    status oee_run_status NOT NULL,
    result_status oee_result_status,
    truth_state oee_truth_state,
    request_hmac bytea NOT NULL CHECK (octet_length(request_hmac) = 32),
    request_hmac_key_version integer NOT NULL CHECK (request_hmac_key_version > 0),
    accepted_at timestamptz NOT NULL,
    started_at timestamptz,
    finished_at timestamptz,
    deadline_at timestamptz NOT NULL,
    cancel_requested_at timestamptz,
    cancel_generation bigint NOT NULL DEFAULT 0 CHECK (cancel_generation >= 0),
    execution_owner text,
    execution_fence uuid,
    execution_lease_until timestamptz,
    failure_code text,
    row_version bigint NOT NULL DEFAULT 1 CHECK (row_version > 0),
    PRIMARY KEY (tenant_id, run_id),
    UNIQUE (tenant_id, request_key),
    FOREIGN KEY (tenant_id, policy_id, policy_revision_id)
        REFERENCES oee_policy_revision (tenant_id, policy_id, policy_revision_id),
    CHECK (started_at IS NULL OR accepted_at <= started_at),
    CHECK (finished_at IS NULL OR accepted_at <= finished_at),
    CHECK (finished_at IS NULL OR started_at IS NULL OR started_at <= finished_at),
    CHECK (status NOT IN ('RUNNING', 'SUCCEEDED') OR started_at IS NOT NULL),
    CHECK (status IN ('ACCEPTED', 'RUNNING') OR finished_at IS NOT NULL),
    CHECK (status <> 'ACCEPTED' OR (started_at IS NULL AND finished_at IS NULL)),
    CHECK (
        (status = 'SUCCEEDED' AND result_status IS NOT NULL AND truth_state IS NOT NULL)
        OR (status <> 'SUCCEEDED' AND result_status IS NULL AND truth_state IS NULL)
    )
);

CREATE TABLE oee_run_source (
    tenant_id uuid NOT NULL,
    run_id uuid NOT NULL,
    source_ordinal integer NOT NULL CHECK (source_ordinal >= 0),
    owner_service text NOT NULL,
    source_revision text NOT NULL,
    dataset_schema_id text NOT NULL,
    consistency_mode text NOT NULL
        CHECK (consistency_mode IN ('SNAPSHOT', 'WATERMARKED', 'NON_REPLAYABLE')),
    snapshot_ref text,
    watermark_ref text,
    contribution_sha256 bytea NOT NULL CHECK (octet_length(contribution_sha256) = 32),
    fact_count bigint NOT NULL CHECK (fact_count >= 0),
    PRIMARY KEY (tenant_id, run_id, source_ordinal),
    FOREIGN KEY (tenant_id, run_id)
        REFERENCES oee_calculation_run (tenant_id, run_id),
    CHECK (
        consistency_mode = 'NON_REPLAYABLE'
        OR snapshot_ref IS NOT NULL OR watermark_ref IS NOT NULL
    )
);

CREATE TABLE oee_evidence_pack (
    tenant_id uuid NOT NULL,
    evidence_id uuid NOT NULL,
    run_id uuid NOT NULL,
    schema_version integer NOT NULL CHECK (schema_version = 1),
    result_status oee_result_status NOT NULL,
    truth_state oee_truth_state NOT NULL,
    dataset_digest bytea NOT NULL CHECK (octet_length(dataset_digest) = 32),
    policy_digest bytea NOT NULL CHECK (octet_length(policy_digest) = 32),
    calculation_version text NOT NULL,
    evidence_json jsonb NOT NULL CHECK (jsonb_typeof(evidence_json) = 'object'),
    evidence_sha256 bytea NOT NULL CHECK (octet_length(evidence_sha256) = 32),
    created_at timestamptz NOT NULL,
    superseded_at timestamptz,
    PRIMARY KEY (tenant_id, evidence_id),
    UNIQUE (tenant_id, run_id),
    FOREIGN KEY (tenant_id, run_id)
        REFERENCES oee_calculation_run (tenant_id, run_id)
);

CREATE TABLE oee_candidate_publication (
    tenant_id uuid NOT NULL,
    candidate_id uuid NOT NULL,
    evidence_id uuid NOT NULL,
    scope_digest bytea NOT NULL CHECK (octet_length(scope_digest) = 32),
    published_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, candidate_id),
    UNIQUE (tenant_id, evidence_id),
    FOREIGN KEY (tenant_id, evidence_id)
        REFERENCES oee_evidence_pack (tenant_id, evidence_id)
);

CREATE TABLE oee_idempotency_key (
    tenant_id uuid NOT NULL,
    operation text NOT NULL,
    idempotency_key text NOT NULL,
    request_hmac bytea NOT NULL CHECK (octet_length(request_hmac) = 32),
    key_version integer NOT NULL CHECK (key_version > 0),
    run_id uuid,
    response_status integer,
    response_json jsonb,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, operation, idempotency_key),
    FOREIGN KEY (tenant_id, run_id)
        REFERENCES oee_calculation_run (tenant_id, run_id),
    CHECK (expires_at > created_at)
);

CREATE TABLE oee_audit_entry (
    tenant_id uuid NOT NULL,
    audit_id uuid NOT NULL,
    occurred_at timestamptz NOT NULL,
    actor_ref text NOT NULL,
    action text NOT NULL,
    resource_type text NOT NULL,
    resource_id uuid,
    before_sha256 bytea,
    after_sha256 bytea,
    reason_code text,
    correlation_id text NOT NULL,
    PRIMARY KEY (tenant_id, audit_id)
);
```

Policy revisions, publications, evidence packs, candidate publications, and audit entries are append-only.

- **Ordinary Runtime Permissions and Triggers**: The runtime application role (`oee_app`) is granted only `SELECT` and `INSERT` on append-only tables, and `SELECT`, `INSERT`, `UPDATE` (constrained to the state machine) on `oee_calculation_run` and `oee_idempotency_key`. The runtime role is denied `DELETE` and `UPDATE` on all append-only tables. Database triggers enforce blanket denial of `UPDATE` unconditionally across all roles.
- **Audited Retention Exception for Deletes**: For regulated data lifecycle management (Section 17), database triggers deny `DELETE` by default but provide a narrowly authorized retention exception: deletion is permitted only when executed by a dedicated maintenance role (`oee_retention_worker`) with explicit, transaction-local authorization (`SET LOCAL app.retention_cleanup_authorized = 'true'`). Every retention deletion transaction must record an audit entry in `oee_audit_entry` within the same transaction documenting the cleanup scope, deleted row count, and retention rule. Any attempt by application roles or unauthorized sessions to delete records is rejected with an exception.
- **State Mutability**: Run status in `oee_calculation_run` is mutable only through the allowed state machine transitions; sealed terminal results, provenance sources, and evidence packs are never overwritten.

`dataset_id` is a logical identity from the accepted request, not a foreign key to owner data. The core must not create a source-fact table.

### 6.3 Tenant isolation and RLS

Enable and force row-level security on every tenant table. The runtime role must not own tables, be a superuser, or have `BYPASSRLS`.

```sql
ALTER TABLE oee_calculation_run ENABLE ROW LEVEL SECURITY;
ALTER TABLE oee_calculation_run FORCE ROW LEVEL SECURITY;

CREATE POLICY oee_calculation_run_tenant_policy
    ON oee_calculation_run
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
```

Apply the equivalent policy to every tenant-owned table. Set `app.tenant_id` with parameterized `set_config(..., true)` inside every transaction. The setting must be transaction-local so a pooled connection cannot retain one tenant after commit or rollback. Missing context denies access. Cross-tenant composite foreign keys, reads, writes, evidence retrieval, worker execution, and pool reuse require integration tests.

## 7. ACID transaction design

ACID is a property of the complete workflow, not merely a database setting.

### 7.1 Transaction A: accept and reserve

1. Start one `AsyncSession` transaction and set tenant context.
2. Lock the idempotency key or create it with `INSERT ... ON CONFLICT`.
3. If the key exists with a different request HMAC, return `409 IDEMPOTENCY_KEY_REUSED`.
4. Lock the policy pointer, verify the effective published revision and expected revision precondition.
5. Insert the run as `ACCEPTED` with the exact policy/dataset digests and deadline.
6. Store no raw restricted parameters; store only approved values or a tenant-keyed HMAC.
7. Commit. Owner calls and calculation happen after this commit.

This gives atomicity for admission and makes retries return the same run identity. Isolation uses PostgreSQL row locks and optimistic `row_version`; no process-local lock is authoritative.

### 7.2 Transaction B: claim execution

Acquire bounded capacity, then conditionally transition `ACCEPTED` to `RUNNING` with a unique execution fence, owner identity, lease expiry, and observed cancel generation. A reaper may reclaim only an expired lease with the same fence. A late executor cannot claim or publish a run it no longer owns.

### 7.3 Transaction C: seal terminal evidence

The executor stages no database evidence until calculation and output validation are complete. It then executes the atomic sealing transaction:

1. Conditionally updates `oee_calculation_run` to `SUCCEEDED` where:
    - `tenant_id = :tenant_id`
    - `run_id = :run_id`
    - `status = 'RUNNING'`
    - `execution_owner = :owner`
    - `execution_fence = :fence`
    - `cancel_generation = :cancel_gen`
    - `execution_lease_until > clock_timestamp()`
    - `deadline_at > clock_timestamp()`
2. If zero rows are updated, the transaction aborts and rolls back immediately. This ensures that if the executor experienced lock waits, scheduling delays, or database contention that caused time to cross `deadline_at` (or `execution_lease_until`), the calculation cannot succeed. The executor transitions the run to `CANCELLED` (or `FAILED` with a timeout reason code) instead.
3. If the conditional update succeeds, within the same transaction:
    - Inserts all minimized source provenance rows (`oee_run_source`).
    - Inserts exactly one immutable evidence pack (`oee_evidence_pack`) and candidate publication (`oee_candidate_publication`) when applicable.
    - Inserts the audit entry (`oee_audit_entry`).
    - Completes idempotency response metadata (`oee_idempotency_key`).
4. Commits all rows in one transaction. There is no in-memory success response before this commit.

### 7.4 Failure and cancellation

Failure commits a safe terminal `FAILED` or `CANCELLED` state with a bounded stable code and no evidence. Cancellation increments `cancel_generation` transactionally. The worker checks it at every fact batch and before sealing. Cleanup runs with a separate short deadline and cannot change a terminal run to success.

Downstream-consumer availability is independent: a sealed evidence pack remains retrievable even if a consumer is unavailable. There is no synchronous cross-service write and no state-then-broker dual write. An event/outbox table requires a future ADR and must be inserted in the same transaction as the state change.

## 8. API and error contract

Public namespace: `/v1/oee`. Use strict Pydantic models with `extra='forbid'`, bounded collections, explicit units, UTC timestamps, half-open windows, canonical Decimal strings, and generated OpenAPI 3.1/JSON Schema artifacts.

Suggested endpoints:

```text
POST /v1/oee/calculations
GET  /v1/oee/calculations/{runId}
GET  /v1/oee/evidence/{evidenceId}
POST /v1/oee/calculations/{runId}:cancel
GET  /v1/oee/policies/{policyId}
POST /v1/oee/policies/{policyId}/revisions
POST /v1/oee/policies/{policyId}/revisions/{revisionId}:publish
```

The calculation request contains the bounded `OeeDatasetV1` and an optional expected policy revision. It cannot contain a database identifier, URL, credentials, caller tenant, authority provider, formula expression, or arbitrary policy document. The service derives tenant and actor from `RequestContext`.

Use RFC 9457 `application/problem+json` with safe stable codes. Never return tokens, headers, raw upstream payloads, SQL, URLs, stack traces, or sensitive facts. Transport success does not imply business readiness: a valid response may be `NOT_READY` or `LIMITED` with reason codes and no prohibited numeric claim.

## 9. Readiness, truth, and failure behavior

| Status      | Meaning                                                                 | Numeric publication                                 |
| ----------- | ----------------------------------------------------------------------- | --------------------------------------------------- |
| `READY`     | Complete and internally consistent evidence satisfies policy            | Allowed when policy permits                         |
| `LIMITED`   | Deterministic supported result with explicit policy-approved exclusions | Allowed only with exclusions and usage restrictions |
| `NOT_READY` | Recoverable missing, stale, ambiguous, or unavailable prerequisite      | No finalized numeric claim                          |
| `INVALID`   | Contract, tenant, identity, digest, or invariant violation              | Reject or safe terminal invalid result              |

Stable reason codes must have an explicit precedence. Required behaviors include:

- missing/stale source or policy: `NOT_READY`;
- duplicate exact fact: deduplicate only by owner identity and revision;
- conflicting revisions or ambiguous mapping: `NOT_READY` or `INVALID` per policy;
- unknown unit/conversion: `INVALID` or `NOT_READY`, never inference;
- deadline/cancellation: terminal cancellation, no partial success;
- persistence failure: no visible evidence;
- cross-tenant or unauthorized input: fail before calculation side effects;
- source owner failure: bounded retry outside pure code, otherwise `NOT_READY`.

## 10. Bounds, concurrency, and operational safety

Every request has one monotonic absolute deadline captured at acceptance. Nested calls consume the remaining budget; they do not reset it. Enforce configured maximums for request bytes, facts, intervals, reason codes, source contributions, execution time, and concurrent runs before expensive work.

Use structured concurrency and bounded batches. Do not buffer unbounded source facts. Each source read is read-only, typed, bounded, and cancellable. Retry only idempotent reads and only transient failures within the same deadline. Do not retry invalid contracts or after terminal cancellation.

PostgreSQL-fenced leases are the authority for multi-replica execution ownership and capacity. An in-process semaphore may reduce local pressure but cannot enforce a deployment-wide limit. Health checks must not claim source availability; readiness checks database/config integrity, while liveness checks process health.

## 11. Security and observability

- Verify JWT signature, issuer, audience, algorithm, expiry, and token type.
- Derive tenant and scope from verified identity and enforce permissions before repository or calculation work.
- Use service identities and allowlisted deployment destinations only at the integration boundary.
- Keep IDs opaque and classify fields as `PUBLIC`, `INTERNAL`, `CONFIDENTIAL`, or `RESTRICTED`.
- Log bounded enums, opaque IDs, digests, counts, and trace IDs only. Never log source payloads, raw parameters, tokens, credentials, URLs, or employee data.
- Record calculation admission, terminal state, policy publication, evidence reads, cancellation, and administrative changes in append-only audit facts.
- Emit metrics for status/reason code, duration, fact count, source bytes, cancellation, deadline, and contract failures with bounded labels.
- Trace validation, normalization, kernel, persistence, and evidence retrieval without putting source data in span attributes.

## 12. Testing and acceptance

### Contract and repository tests

- strict unknown-field/type/unit/time/Decimal rejection;
- canonical serialization and digest stability;
- schema/OpenAPI drift and provider/consumer fixtures;
- migration from empty database and upgrade from the previous release;
- composite tenant keys, forced RLS, default deny, pool rollback/reuse, and cross-tenant FK tests;
- immutable policy/evidence/audit tests and optimistic publication conflicts;
- idempotent replay, key reuse conflict, and bounded retention cleanup.

### Pure-domain tests

- approved golden vectors for every formula and policy revision;
- property tests for interval partitioning, unit consistency, monotonic contribution accounting, and deterministic replay;
- adversarial overlap/gap, duplicate/conflict, late correction, counter reset/wrap, cross-midnight, DST, mixed product, zero denominator, scrap/rework, and over-100% performance cases;
- mutation tests proving no clock/network/database/random access in pure modules.

### Lifecycle and security tests

- cancellation during source read, normalization, persistence, and final seal;
- reaper and fence races where a late executor cannot publish success;
- source schema/provenance/digest violations;
- deadline and resource-boundary failures with no partial evidence;
- hostile tenant, URL, formula, SQL, unknown-field, and raw-payload inputs;
- no open tasks, responses, staging data, or capacity leases after failure.

CI must run formatting/lint, strict typing, compile checks, unit/property/security tests, PostgreSQL 16 integration tests, migration checks, contract compatibility checks, dependency/secret/container scans, and deterministic golden-vector tests. No skipped required test may pass the release gate.

## 13. Delivery order

1. Create repository tooling, strict contracts, error model, migrations, RLS, and fake datasets without formulas.
2. Implement policy revision/publication and calculation-run reservation with idempotency and audit.
3. Implement contract verification, readiness, and pure timeline interfaces.
4. Implement each formula/loss/cost child only after its golden vectors and product rules are approved.
5. Implement immutable evidence sealing and read-only retrieval.
6. Publish OpenAPI/JSON Schema artifacts and MES consumer fixtures.
7. Integrate through WIS-231, shadow compare, and cut over only through its audited authority transition.

A local fake adapter may demonstrate deterministic behavior, but it must be labelled non-live. No endpoint may return fabricated finalized metrics merely to make the service appear complete.

## 14. Change control

Changes to source authority, tenant boundaries, contract families, finalized-result ownership, policy selection, transaction publication, or cutover semantics require a superseding ADR. Formula, readiness, taxonomy, unit, and cost changes require a new immutable policy revision plus golden vectors. Code version and policy digest are always part of evidence provenance, so a replay cannot silently acquire new semantics.

## 15. Production security baseline

The following controls are release blockers, not optional hardening:

### 15.1 Identity and authorization

- Validate access tokens using a pinned issuer and audience, an explicit algorithm allowlist, signature verification, `exp`, `nbf`, `iat`, token type, and bounded clock skew. Do not accept unsigned tokens or algorithm negotiation from the token.
- Use short-lived service credentials for owner-bound calls. Prefer workload identity and mTLS; do not forward an end-user bearer token to an owner service unless the owner contract explicitly requires it.
- Authorize the requested site, line, asset, order, product, and policy scope before inserting a run or calling an owner. A denied scope must produce zero owner requests and zero calculation side effects.
- Require separate permissions for policy authoring, policy publication, evidence access, and administration. Enforce separation of duties for publication where tenant policy requires it.
- Never accept tenant, actor, role, permission, authority-provider, database, URL, credential, or destination fields from public payloads.

### 15.2 HTTP and network boundary

- Terminate TLS at an approved gateway and use TLS or mTLS for service-to-service traffic. Disable plaintext owner egress.
- Allow egress only to deployment-configured owner service identities. DNS, IP literals, redirects, proxy settings, userinfo, and caller-controlled hosts are forbidden.
- Apply request body, header, decompression, connection, and response-size limits before parsing. Reject unknown content types and ambiguous transfer encodings.
- Use a bounded per-tenant and global rate limit, concurrent-run budget, and circuit breaker. Capacity exhaustion is returned without queueing unbounded work.
- Set explicit security headers at the gateway, disable directory listing, and do not expose framework or dependency versions.

### 15.3 Database roles and connection safety

Use separate principals for migration, application read/write, read-only evidence queries, backup, and monitoring. The application role must not be a table owner, superuser, replication role, or `BYPASSRLS` role. Migrations run from a separate release job and are never executed by the web process.

At connection setup, set a safe `search_path` to trusted schemas, disable unsafe session inheritance, require TLS certificate verification, and set statement, lock, idle-in-transaction, and connection timeouts. Every transaction sets tenant context with `set_config('app.tenant_id', value, true)` and clears it by transaction end. A missing or malformed context must deny access rather than produce an unscoped query.

RLS policies must be generated and tested for every tenant table. Add a catalog test that fails when a tenant table lacks `ENABLE ROW LEVEL SECURITY`, `FORCE ROW LEVEL SECURITY`, a tenant policy, or a composite tenant-aware foreign key. Test access using the real non-owner application role; tests run as a database owner are not security evidence.

### 15.4 Secrets and cryptography

- Store signing keys, HMAC keys, database credentials, and service certificates only in the platform secret manager or workload identity system.
- Never place secrets in source, policy JSON, migrations, environment snapshots, crash dumps, traces, or database rows.
- Version tenant HMAC keys and support overlap during rotation. Record only the key version and HMAC, never key material or raw restricted parameters.
- Use a cryptographic random generator for IDs, fences, nonces, and tokens. Use SHA-256 or an approved stronger digest for canonical content; use HMAC for sensitive low-entropy parameter proofs.
- Encrypt PostgreSQL backups, replicas, logs, and any temporary evidence storage. Rotate keys with a tested re-encryption or key-encryption-key rotation procedure.

## 16. Strict schema and code-quality rules

Every public and persisted contract is versioned independently from the service build. JSON Schema and Pydantic must agree on the following rules:

- Every object uses `additionalProperties: false`; nested objects and discriminated unions are closed as well.
- Every collection has a maximum length and every string has a maximum UTF-8 length. Numeric values have explicit minimum, maximum, precision, and scale.
- UUID, enum, timestamp, timezone, unit, currency, and identifier formats are explicit. Timestamps are UTC instants; civil-time fields carry an IANA zone and calendar revision.
- Decimal quantities and ratios use canonical strings with an approved scale. Floating-point arithmetic is forbidden for governed results. Because Pydantic documents that Decimal strictness has no effect in JSON mode (numbers are parsed before conversion), wire DTOs must use an explicit bounded string wire type (`CanonicalDecimalString`) matching a strict canonical regex pattern rather than bare `Decimal` types to reject raw JSON numbers and noncanonical formats at the boundary.
- Windows are half-open, finite, and bounded. Empty windows, reversed windows, overlapping effective policies, and impossible intervals fail validation.
- Source identity is a stable owner ID plus owner revision; timestamps and values are not identity substitutes. Duplicate and conflicting revisions are handled by policy, never by arrival order.
- Digests are over canonical serialization, not incidental dictionary or database ordering. Persist the schema ID, canonicalization version, and digest algorithm.
- Public DTOs are immutable after validation. Domain values are immutable and have no implicit coercion, hidden defaults, or global mutable configuration.
- Error codes are finite enums. Error details are bounded and safe; validation must not echo arbitrary attacker-controlled payloads.

The codebase must use strict type checking, dependency injection through ports, structured concurrency, explicit cancellation propagation, and one database session per task. SQL is parameterized and repositories expose allowlisted operations only. No dynamic imports, `eval`, `exec`, shell calls, pickle deserialization, template execution, or user-supplied expressions are permitted.

## 17. Evidence, retention, and disaster recovery

Evidence is a regulated derived record, not a mutable cache. Store only the minimum source contribution needed to explain and reproduce the result. Do not store raw upstream response bodies, credentials, employee names, or unrestricted parameters.

- Define tenant-specific retention classes for policy revisions, run metadata, evidence packs, audit records, and failed-run diagnostics. Retention must have a maximum, an owner, and a deletion job with an audit record.
- **Narrowly Authorized Retention Mechanism**: Normal runtime application roles (`oee_app`) cannot delete rows from append-only tables. Retention cleanup jobs run exclusively under a dedicated maintenance role (`oee_retention_worker`), which activates transaction-local authorization (`SET LOCAL app.retention_cleanup_authorized = 'true'`). Every retention transaction must insert an audit record in `oee_audit_entry` within the same transaction documenting the rule, purged scope, and row count before committing.
- **Dependent-Record Deletion Order**: To maintain foreign key integrity and prevent orphan references, retention purges must follow strict cascade ordering:
    1. `oee_candidate_publication` (references `evidence_id`)
    2. `oee_evidence_pack` (references `run_id`)
    3. `oee_run_source` (references `run_id`)
    4. `oee_idempotency_key` (references `run_id`)
    5. `oee_calculation_run` (root entity of the execution)
    6. Expired `oee_policy_revision` may only be purged if it has passed its maximum retention window AND is not referenced by any active/historical publication or calculation run; active published policies (`current_publication_id`) cannot be purged regardless of age.
    7. Expired `oee_audit_entry` records have an extended regulatory retention window (e.g., 7 years) and may only be purged after cryptographic export and verification against long-term immutable cold storage, executed with a final summary audit record.
- Use PostgreSQL point-in-time recovery, encrypted backups, replica monitoring, and a tested restore procedure. Backups are not considered valid until a restore verifies RLS, migrations, row counts, and evidence digests.
- Define and test RPO/RTO for policies, runs, evidence, and audit separately. A restore must not create a second accepted authority or mutate sealed evidence.
- Use immutable object storage only if a later contract requires large evidence material. Stage under server-generated keys, verify size and digest, publish atomically, deny public access, and sweep orphaned objects.
- Evidence reads re-authorize the caller and return only the requested tenant/scope. Cache is disabled unless a separate decision proves snapshot identity, authorization freshness, policy digest, and tenant isolation.

## 18. State machine and race-proofing

The only valid run transitions are:

```text
ACCEPTED -> RUNNING -> SUCCEEDED
ACCEPTED -> CANCELLED
ACCEPTED -> FAILED
RUNNING  -> CANCELLED
RUNNING  -> FAILED
RUNNING  -> FAILED  [expired fenced lease]
```

There is no transition out of a terminal state. The database update that seals success must match tenant, run ID, status (`RUNNING`), execution fence, execution owner, cancel generation, an unexpired lease (`execution_lease_until > clock_timestamp()`), and an unexpired request deadline (`deadline_at > clock_timestamp()`). Capping execution lease renewals at `deadline_at` provides defense-in-depth, but requiring `deadline_at > clock_timestamp()` directly in the conditional seal update predicate ensures that if a database lock wait causes `clock_timestamp()` to cross the deadline after the worker's application-level check, the transaction aborts and no partial or stale success can commit. Use database `clock_timestamp()`; application wall-clock time is not authoritative. A reaper, cancellation, timeout, and executor racing on the same run must result in exactly one committed terminal outcome.

The core may create only `CANDIDATE` evidence. `ACCEPTED` is assigned only by the separately owned authority router after cutover approval. A rollback increments the external authority epoch, leaves evidence immutable, and never relabels a legacy value as finalized evidence.

## 19. Production observability and incident response

Use OpenTelemetry trace context with redacted, bounded attributes. Correlation IDs, tenant-safe opaque IDs, policy/dataset/evidence digests, status, reason code, source contract version, and build digest are permitted. Raw facts, parameters, tokens, URLs, SQL, response bodies, and stack traces are forbidden in telemetry.

Alert on authentication failures, cross-tenant denial, RLS policy drift, contract-invalid source responses, digest mismatches, lease races, cancellation leaks, capacity exhaustion, deadline breaches, backup failures, migration failures, and unusual evidence-read volume. Metrics must use bounded label cardinality; tenant IDs and arbitrary resource IDs are not metric labels.

Security incidents require credential/key revocation, evidence preservation, tenant impact identification, audit export, and a documented recovery decision. A health endpoint must not disclose database details, owner availability, policy contents, or build secrets.

## 20. Release and supply-chain gates

Production release requires:

- locked and reproducible dependency resolution, supported Python/runtime image, generated SBOM, license review, and vulnerability policy;
- signed container/image and provenance attestation, non-root execution, read-only root filesystem where compatible, dropped Linux capabilities, seccomp/default sandboxing, and resource limits;
- dependency, secret, static-analysis, type-check, container, migration, and API compatibility scans;
- migration rehearsal against an empty database and the previous production schema, with rollback or forward-fix evidence, explicitly testing composite foreign keys (including policy-revision references);
- PostgreSQL RLS/concurrency tests against the production-equivalent non-owner role;
- golden vectors, property tests, fuzz tests, cancellation/race tests (including pre-start cancellation/failure before claim, and lock-wait deadline expiration crossing request deadline during seal), and no-skipped-test enforcement;
- deployment health gates, gradual rollout, rapid rollback, and a post-release evidence replay check.

No release is production-ready merely because the service starts. Production readiness requires security, migration, restore, load, failure-injection, and authority-coexistence evidence attached to the release.

## 21. Repository files and configuration

The implementation should use the following repository layout. Configuration is code-reviewed, versioned, and validated at startup. Production secrets are never committed; `.env.example` contains names and safe local placeholders only.

```text
.
├── .github/
│   ├── CODEOWNERS                     # human approval policy (ADR 0001 Sec. 18)
│   └── workflows/
│       ├── ci.yml                     # pull-request quality gates
│       ├── security.yml               # dependency, secret, Bandit scans
│       └── release.yml                # signed image and deployment promotion
├── deploy/
│   ├── docker/                        # production image support files
│   └── postgres/                      # role/bootstrap scripts, no secrets
├── docs/
│   └── adr/
│       └── 0002-oee-intelligence-core-module-design.md
├── migrations/                        # Alembic revisions
│   ├── env.py                         # async migration environment with RLS
│   └── versions/                      # migration revision scripts
├── schemas/                           # generated OpenAPI and JSON Schema
│   ├── openapi.json                   # OpenAPI 3.1 specification (/v1/oee)
│   └── v1/                            # JSON Schema contracts (oee.wisedo.io/v1)
│       ├── dataset.json
│       ├── policy.json
│       └── evidence.json
├── src/wisedo_plantops_oee/           # root application package
│   ├── __init__.py
│   ├── settings.py                    # pydantic-settings startup validation
│   ├── domain/                        # pure domain layer (zero I/O)
│   │   ├── __init__.py
│   │   ├── values.py                  # value objects: intervals, decimals, digests, units
│   │   ├── contracts.py               # pure domain models
│   │   ├── readiness.py               # pure readiness status & reason precedence
│   │   ├── timeline.py                # canonical interval & timeline normalization
│   │   ├── calculation.py             # pure A/P/Q/OEE/TEEP math kernel (WIS-226+)
│   │   ├── downtime.py                # pure taxonomy & loss hierarchy classification
│   │   ├── cost.py                    # pure financial loss rate & cost model
│   │   └── evidence.py                # pure evidence pack reconciliation
│   ├── application/                   # application orchestration & use cases
│   │   ├── __init__.py
│   │   ├── calculate.py               # CalculationApplicationService flow
│   │   ├── policy_commands.py         # validate, publish, rollback & read policy
│   │   ├── evidence_queries.py        # authorized read-only evidence queries
│   │   ├── cancellation.py            # deadline, fence, lease & cancel checks
│   │   └── ports.py                   # abstract ports (repositories, clock, UoW)
│   ├── contracts/                     # boundary contracts (DTOs)
│   │   ├── __init__.py
│   │   ├── errors.py                  # RFC 9457 Problem Details
│   │   └── v1/                        # schema identity: oee.wisedo.io/v1
│   │       ├── __init__.py
│   │       ├── common.py              # enums, TimeWindowV1, DecimalString, RatioMetricV1
│   │       ├── context.py             # RequestContext (JWT claims only; no body tenant)
│   │       ├── dataset.py             # OeeDatasetV1
│   │       ├── policy.py              # CalculationPolicyV1
│   │       ├── evidence.py            # OeeEvidencePackV1
│   │       └── requests.py            # CalculationRequestV1
│   ├── adapters/                      # I/O adapters & persistence
│   │   ├── __init__.py
│   │   ├── http/                      # inbound REST API
│   │   │   ├── __init__.py
│   │   │   ├── api.py                 # FastAPI routes & RFC 9457 error mappings
│   │   │   └── auth.py                # OIDC token verification & RequestContext
│   │   ├── db/                        # PostgreSQL 16 persistence (SQLAlchemy 2 async)
│   │   │   ├── __init__.py
│   │   │   ├── models.py              # declarative ORM models (all 9 tables)
│   │   │   ├── repositories.py        # tenant-scoped repositories
│   │   │   └── transactions.py        # Unit of Work with set_config('app.tenant_id') RLS
│   │   ├── cache/                     # caching infrastructure adapter
│   │   │   ├── __init__.py
│   │   │   └── redis.py               # async Redis client with tenant namespacing
│   │   └── sources/                   # dataset verification boundary
│   │       ├── __init__.py
│   │       └── dataset_verifier.py    # verifies incoming bounded OeeDatasetV1
│   └── observability/                 # cross-cutting telemetry
│       ├── __init__.py
│       ├── logging.py                 # structlog with secret scrubbing
│       ├── metrics.py                 # OpenTelemetry metrics with bounded labels
│       └── tracing.py                 # OpenTelemetry distributed tracing
├── tests/
│   ├── __init__.py
│   ├── conftest.py                    # shared pytest fixtures (mock JWT, test DB)
│   ├── unit/                          # pure domain and contract tests
│   ├── integration/                   # PostgreSQL, RLS, migration tests
│   ├── security/                      # hostile-input and authorization tests
│   └── golden/                        # approved calculation vectors
├── .dockerignore
├── .env.example
├── .gitignore
├── .pre-commit-config.yaml
├── .python-version
├── alembic.ini
├── docker-compose.yml
├── Dockerfile
├── Makefile
├── pyproject.toml
├── uv.lock
├── README.md
└── SECURITY.md
```

Generated files must be reproducible. CI fails when generated schemas, lockfiles, or formatting differ from the source. Do not commit virtual environments, caches, local databases, coverage output, raw datasets, credentials, certificates, or production configuration.

### 21.1 `pyproject.toml` requirements

`pyproject.toml` is the single source for Python metadata, dependencies, tools, and test configuration. Use a locked resolver such as `uv`; the exact package versions are selected and reviewed during implementation.

```toml
[project]
name = "plantops-oee-intelligence"
requires-python = ">=3.12,<3.13"
dependencies = [
    "fastapi",
    "pydantic>=2,<3",
    "pydantic-settings>=2,<3",
    "sqlalchemy[asyncio]>=2,<3",
    "asyncpg",
    "redis[hiredis]>=5,<6",
    "alembic",
    "httpx",
    "pyjwt[crypto]",
    "opentelemetry-api",
    "opentelemetry-sdk",
    "structlog",
    "uvicorn[standard]",
]

[dependency-groups]
dev = [
    "mypy",
    "pytest",
    "pytest-asyncio",
    "pytest-cov",
    "hypothesis",
    "ruff",
    "pip-audit",
    "bandit",
    "pre-commit",
]

[tool.ruff]
target-version = "py312"
line-length = 100
src = ["src", "tests"]
extend-exclude = ["migrations"]

[tool.ruff.lint]
select = ["E", "F", "I", "B", "UP", "ASYNC", "A", "S", "T20"]
ignore = ["S101"]

[tool.ruff.lint.isort]
    known-first-party = ["wisedo_plantops_oee"]
force-single-line = false

[tool.ruff.format]
quote-style = "double"
indent-style = "space"
line-ending = "lf"
skip-magic-trailing-comma = false

[tool.mypy]
python_version = "3.12"
strict = true
files = ["src", "tests"]
warn_unused_configs = true
warn_unreachable = true
disallow_any_generics = true
disallow_untyped_defs = true
no_implicit_optional = true

[tool.pytest.ini_options]
testpaths = ["tests"]
addopts = "--strict-config --strict-markers --cov=wisedo_plantops_oee --cov-fail-under=90"
asyncio_mode = "strict"
markers = [
    "integration: requires PostgreSQL and service dependencies",
    "security: authorization, isolation, and hostile-input tests",
    "golden: approved deterministic calculation vectors",
]
```

The coverage threshold is a floor, not proof of correctness. Critical security, migration, race, and formula paths require named tests even when line coverage is already high.

### 21.2 Python formatting and code style

Formatting is automatic and non-negotiable: Ruff format, four spaces, UTF-8, LF line endings, 100-character lines, double quotes, sorted imports, and no unused imports. Do not hand-align tables or use formatting-only commits to hide behavioral changes.

```python
from decimal import Decimal
from typing import Annotated
from uuid import UUID

from pydantic import BaseModel, ConfigDict, Field, field_validator


# Canonical string regex for bounded decimal with up to 6 decimal places:
# strictly forbids JSON numbers, exponents, leading zeros, trailing decimals
CANONICAL_DECIMAL_PATTERN = r"^(0|[1-9]\d*)(\.\d{1,6})?$"

CanonicalDecimalString = Annotated[
    str,
    Field(
        pattern=CANONICAL_DECIMAL_PATTERN,
        description="Canonical string representation of a decimal with scale <= 6. JSON numbers are rejected.",
    ),
]


class OeeRatioWireV1(BaseModel):
    """Wire DTO enforcing strict string-only Decimal contract without JSON number coercion.

    Pydantic's Decimal type accepts numeric JSON even under strict=True. Public wire contracts
    must receive CanonicalDecimalString to guarantee JSON numbers and noncanonical forms
    are rejected at the wire boundary, then convert explicitly to immutable domain Decimals.
    """

    model_config = ConfigDict(extra="forbid", frozen=True, strict=True)

    numerator: CanonicalDecimalString
    denominator: CanonicalDecimalString
    unit: str = Field(min_length=1, max_length=32, pattern=r"^[a-z][a-z0-9._-]*$")
    policy_revision_id: UUID

    @field_validator("denominator")
    @classmethod
    def validate_positive_denominator(cls, v: str) -> str:
        d = Decimal(v)
        if d <= Decimal("0"):
            raise ValueError("Denominator must be strictly positive")
        return v

    def to_domain(self) -> "OeeRatio":
        return OeeRatio(
            numerator=Decimal(self.numerator),
            denominator=Decimal(self.denominator),
            unit=self.unit,
            policy_revision_id=self.policy_revision_id,
        )


class OeeRatio:
    """Domain model using exact immutable Decimal arithmetic (zero I/O)."""

    def __init__(
        self,
        numerator: Decimal,
        denominator: Decimal,
        unit: str,
        policy_revision_id: UUID,
    ) -> None:
        if denominator <= Decimal("0"):
            raise ValueError("Denominator must be strictly positive")
        self.numerator = numerator
        self.denominator = denominator
        self.unit = unit
        self.policy_revision_id = policy_revision_id
```

Domain code should prefer immutable value objects, explicit return types, `Decimal` for governed quantities, `Enum` values for finite states, and small functions with one responsibility. HTTP handlers translate requests into application commands; they do not contain formulas or SQL. Repositories return domain values rather than ORM objects. Comments explain invariants or tradeoffs, not obvious assignments.

### 21.3 `.pre-commit-config.yaml`

The pre-commit file runs fast local checks only. CI remains authoritative and runs the complete database, security, and race suite.

```yaml
repos:
    - repo: https://github.com/astral-sh/ruff-pre-commit
      rev: <approved-ruff-revision>
      hooks:
          - id: ruff
            args: [--fix]
          - id: ruff-format
    - repo: https://github.com/pre-commit/pre-commit-hooks
      rev: <approved-hooks-revision>
      hooks:
          - id: check-merge-conflict
          - id: check-toml
          - id: end-of-file-fixer
          - id: trailing-whitespace
          - id: detect-private-key
    - repo: https://github.com/pycqa/bandit
      rev: <approved-bandit-revision>
      hooks:
          - id: bandit
            args: [-r, src]
```

All action and hook revisions must be pinned to reviewed immutable commits in the actual repository. Floating branches such as `main` are forbidden in CI or pre-commit configuration.

## 22. Container and local runtime files

### 22.1 `Dockerfile`

Use a multi-stage, non-root image. Pin the Python 3.12 base image by digest in the implementation; the placeholder below is intentionally not a production digest.

```dockerfile
FROM python:3.12-slim@sha256:<approved-python-image-digest> AS runtime

ENV PYTHONDONTWRITEBYTECODE=1 \
        PYTHONUNBUFFERED=1 \
        PYTHONHASHSEED=random \
        PORT=8080

RUN addgroup --system --gid 10001 app \
        && adduser --system --uid 10001 --gid 10001 app

WORKDIR /app
COPY --chown=app:app pyproject.toml uv.lock ./
COPY --chown=app:app src ./src
COPY --chown=app:app migrations ./migrations
COPY --chown=app:app alembic.ini ./

RUN python -m pip install --no-cache-dir --require-hashes -r /dev/null

USER 10001:10001
EXPOSE 8080
STOPSIGNAL SIGTERM
ENTRYPOINT ["uvicorn", "wisedo_plantops_oee.adapters.http.api:app", "--host", "0.0.0.0", "--port", "8080"]
```

The `pip install` line above is a placeholder for the chosen locked-install command and must be replaced before implementation; it must install from `uv.lock` or an equivalent hash-locked export. The production image must not contain compilers, Git, package caches, test fixtures, migration credentials, or a shell required for normal operation. Run the container with a read-only root filesystem, dropped capabilities, a temporary writable directory only when required, CPU/memory/PID limits, and a seccomp/default sandbox profile.

### 22.2 `.dockerignore`

At minimum exclude `.git`, `.venv`, `__pycache__`, `.pytest_cache`, `.mypy_cache`, `.ruff_cache`, `coverage*`, `.env*`, secrets, certificates, local databases, raw datasets, and test artifacts. The build context must be checked by CI for accidental secret files.

### 22.3 Local PostgreSQL

Local development may use PostgreSQL 16 in an uncommitted compose override. It must use a non-production password, bind only to loopback, use a named volume, and never be reachable from the host network or checked into CI. Local startup runs migrations explicitly; the application does not auto-create or auto-upgrade the schema.

## 23. Environment and configuration contract

Environment variables are parsed once at process startup by a strict `pydantic-settings` model. Unknown variables are ignored only when they are supplied by the platform globally; application configuration itself must reject missing, malformed, duplicated, or unsafe values. Secrets use `*_REF` names that point to a secret manager entry, not the secret value.

| Variable                        | Required | Example or rule                                      | Secret |
| ------------------------------- | -------- | ---------------------------------------------------- | ------ |
| `APP_ENV`                       | yes      | `local`, `test`, `staging`, or `production`          | no     |
| `APP_VERSION`                   | yes      | immutable Git SHA or image digest                    | no     |
| `LOG_LEVEL`                     | yes      | `INFO` in production; bounded enum                   | no     |
| `HTTP_HOST`                     | yes      | `0.0.0.0` in the container                           | no     |
| `HTTP_PORT`                     | yes      | `8080`                                               | no     |
| `DATABASE_URL_REF`              | yes      | secret-manager reference; TLS required outside local | yes    |
| `DATABASE_SCHEMA`               | yes      | fixed trusted schema name                            | no     |
| `DATABASE_POOL_MAX`             | yes      | deployment-approved bounded integer                  | no     |
| `DATABASE_STATEMENT_TIMEOUT_MS` | yes      | hard upper bound for requests                        | no     |
| `OIDC_ISSUER`                   | yes      | HTTPS allowlisted issuer                             | no     |
| `OIDC_AUDIENCE`                 | yes      | exact service audience                               | no     |
| `OIDC_JWKS_URI`                 | yes      | HTTPS issuer-owned JWKS endpoint                     | no     |
| `OIDC_CLOCK_SKEW_SECONDS`       | yes      | small bounded integer                                | no     |
| `SERVICE_IDENTITY_REF`          | yes      | workload identity or secret reference                | yes    |
| `OWNER_EGRESS_ALLOWLIST`        | yes      | deployment-owned service identities only             | no     |
| `TENANT_HMAC_KEY_REF`           | yes      | versioned secret-manager reference                   | yes    |
| `TENANT_HMAC_KEY_VERSION`       | yes      | positive integer                                     | no     |
| `MAX_REQUEST_BYTES`             | yes      | bounded deployment policy                            | no     |
| `MAX_DATASET_FACTS`             | yes      | bounded deployment policy                            | no     |
| `MAX_WINDOW_SECONDS`            | yes      | bounded deployment policy                            | no     |
| `CALCULATION_DEADLINE_SECONDS`  | yes      | bounded deployment policy                            | no     |
| `TENANT_CONCURRENCY_LIMIT`      | yes      | bounded integer                                      | no     |
| `GLOBAL_CONCURRENCY_LIMIT`      | yes      | deployment capacity limit                            | no     |
| `EVIDENCE_RETENTION_SECONDS`    | yes      | positive, policy-approved TTL                        | no     |
| `OTEL_EXPORTER_OTLP_ENDPOINT`   | yes      | private TLS telemetry endpoint                       | no     |
| `OTEL_SERVICE_NAME`             | yes      | `plantops-oee-intelligence`                          | no     |
| `OTEL_TRACES_SAMPLER`           | yes      | production-approved sampler                          | no     |

The service fails closed at startup when production uses local storage, plaintext database transport, wildcard egress, an unpinned issuer, an inline secret where a reference is required, an excessive bound, or an unsupported schema version. Environment variables cannot change tenant authority, formula semantics, RLS behavior, or accepted result ownership.

`.env.example` must contain safe local values such as `APP_ENV=local` and `DATABASE_URL_REF=local-development-only`; it must never contain real tokens, keys, certificates, or production hostnames. Production configuration is injected by the deployment platform and is not built into the image.

## 24. GitHub Actions CI/CD blueprint

### 24.1 `.github/workflows/ci.yml`

Pull requests and protected-branch pushes run the same deterministic gates:

```yaml
name: ci

on:
    pull_request:
    push:
        branches: [main]

permissions:
    contents: read
    id-token: none

jobs:
    quality:
        runs-on: ubuntu-24.04
        timeout-minutes: 20
        steps:
            - uses: actions/checkout@<pinned-commit>
            - uses: astral-sh/setup-uv@<pinned-commit>
              with:
                  version: "<pinned-version>"
                  python-version: "3.12"
            - run: uv sync --locked
            - run: uv run ruff check .
            - run: uv run ruff format --check .
            - run: uv run mypy
            - run: uv run pytest tests/unit tests/security --no-cov

    integration:
        runs-on: ubuntu-24.04
        services:
            postgres:
                image: postgres:16@sha256:<approved-postgres-image-digest>
                env:
                    POSTGRES_DB: oee_test
                    POSTGRES_USER: oee_test
                    POSTGRES_PASSWORD: oee_test_only
                ports: ["5432:5432"]
                options: >-
                    --health-cmd "pg_isready -U oee_test -d oee_test"
                    --health-interval 5s --health-timeout 5s --health-retries 20
        steps:
            - uses: actions/checkout@<pinned-commit>
            - uses: astral-sh/setup-uv@<pinned-commit>
              with:
                  version: "<pinned-version>"
                  python-version: "3.12"
            - run: uv sync --locked
            - run: uv run alembic upgrade head
            - run: uv run pytest tests/integration tests/golden --cov=wisedo_plantops_oee
```

The real workflow must pin every action by immutable commit, set minimal `GITHUB_TOKEN` permissions, use no production secrets, and upload only sanitized test reports. Database service credentials are test-only and never reused elsewhere.

### 24.2 `.github/workflows/security.yml`

Run on pull requests, protected-branch pushes, and a scheduled cadence. It must include secret scanning, dependency and license review, `pip-audit`, Bandit, SBOM generation, container scanning, IaC scanning, and API/schema compatibility checks. Findings marked blocking by the security policy fail the workflow; suppressions require an expiry, owner, and reviewed justification.

### 24.3 `.github/workflows/release.yml`

Release promotion is tag-based and environment-protected:

1. Verify the tag points to a protected commit and rerun all CI gates.
2. Build a reproducible image from the lockfile and pinned base digest.
3. Generate SBOM and SLSA-compatible provenance, scan the image, and sign the image and attestations with keyless workload identity.
4. Push only to the approved registry using short-lived credentials.
5. Run migrations as a separate protected job with the migration role.
6. Deploy progressively to staging, run smoke/RLS/replay checks, then require manual production approval.
7. Roll back application traffic independently from database forward migrations; destructive migrations require a separately approved release.

No workflow may print secrets, database URLs, JWTs, owner responses, or raw evidence. Production deployment permissions must not be available to pull requests from forks.

## 25. Developer commands and operational runbook

The `Makefile` should expose stable commands so local and CI behavior cannot drift:

```text
make install       # locked dependency installation
make format        # ruff format and safe import fixing
make lint          # ruff, mypy, and security lint
make test          # unit, property, and security tests
make test-db       # PostgreSQL integration and RLS tests
make schema        # generate OpenAPI and JSON Schema, then check git diff
make migrate       # explicitly upgrade local database
make check         # all required local pre-commit gates
make image         # build the local production-like image
```

Operational runbooks must cover deploy, rollback, failed migration, database restore, secret rotation, HMAC-key rotation, certificate rotation, owner-contract outage, RLS-policy drift, stuck lease cleanup, evidence retention, and security incident response. Each runbook names an owner, preconditions, commands, expected telemetry, rollback point, and verification query without exposing credentials.

## 26. Configuration change control

Changes to a bound, timeout, issuer, audience, egress identity, retention period, database role, image digest, or CI security gate require review and an updated configuration compatibility record. Configuration changes are versioned and auditable like code. Runtime configuration cannot activate a draft formula, bypass RLS, disable tenant authorization, weaken schema validation, or make a candidate evidence pack accepted.
