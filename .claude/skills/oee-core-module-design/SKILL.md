---
name: oee-core-module-design
description: ADR 0002 - the layered module design for the OEE calculation core - package structure with inward dependencies, the nine-stage calculation pipeline, dataset/policy/evidence contracts, strict schema rules, retention roles and release supply-chain gates. Use this skill when structuring OEE code, defining a contract, adding a pipeline stage, or deciding what a pure domain module may touch.
---

# OEE Intelligence Core module design

Source: `docs/adr/0002-oee-intelligence-core-module-design.md`. Subordinate to
`docs/independent-supervisor.md`. The current implementation lives in Go under `internal/oee/`; the
ADR's Python package layout is the design rationale, and `docs/oee-foundation.md` maps it onto the
shipped Go files.
Governing rules: `.claude/rules/04-deterministic-arithmetic-and-digests.md`, `.claude/rules/07-api-contract-conventions.md`.

**Where this document does not define a formula, threshold, unit policy or precedence rule, the
implementation stays unavailable until the owning product decision is approved. Never fill an
unresolved rule with a convenient default.**

## Non-negotiable rules

- No connection to an MES database, PLC, broker, owner event stream or caller-selected URL.
- No duplicate event consumer for a source already projected by MES.
- No mutation of MES state and no machine-control behavior.
- No caller-selected tenant, policy, authority provider or network destination.
- No silent gap filling, last-write-wins conflict resolution, unit inference, percentage clamping or
  averaging of percentages.
- No partial `READY` result. A failed, cancelled, stale or fenced-out run cannot publish evidence.
- Pure domain modules cannot read a clock, database, network, environment, random source or mutable
  singleton.
- This service never decides that it is authoritative — that stays in WIS-231.

## Owned vs out of scope

Owned: strict versioned contracts; validation of source identity, provenance, bounds, units, windows
and policy references; deterministic normalization; readiness evaluation and reason-code precedence;
finalized A/P/Q/OEE/TEEP, loss classification and modeled cost **only after the product rules are
approved**; immutable policy revisions; run reservation, idempotency, cancellation, fencing and
terminal status; read-only evidence retrieval; safe metrics, traces, audit, migrations and fixtures.

Out of scope: MES persistence models; owner ingestion, event offsets or live accumulation; finalized
authority routing, shadow comparison, cutover and rollback flags; dashboards, layouts, export,
scheduling and delivery; PLC/HMI/SCADA/OPC UA/MQTT/Modbus and safety commands; ML and predictive
maintenance inference.

## Layering

A modular monolith with dependencies directed inward. Domain and application code must not import a
web framework, an ORM, HTTP clients or storage SDKs.

```
domain/       values · contracts · readiness · timeline · calculation · downtime · cost · evidence
application/  calculate · policy_commands · evidence_queries · cancellation · ports
contracts/    errors · v1/{common,context,dataset,policy,evidence,requests}
adapters/     http/{api,auth} · db/{models,repositories,transactions} · cache · sources
observability/ logging (secret scrubbing) · metrics (bounded cardinality) · tracing
```

`adapters/sources` verifies an already-assembled dataset. It holds no MES database client and no new
owner-event consumer.

## Core contracts

- **RequestContext** — created only after token validation; carries verified tenant, actor
  reference, permissions, authorized scope, trace ID and request deadline. Tenant identity is never
  accepted from JSON, query parameters, headers or source facts.
- **OeeDatasetV1** — immutable and bounded: dataset identity and schema ID; source identities with
  owner and revision per fact; half-open UTC window; explicit civil-time zone and calendar refs;
  source watermarks and occurred/recorded timestamps; explicit units and signal quality; bounded
  facts for state, counters, production, quality, downtime and master data; a canonical provenance
  digest. It is **not a database dump** — exact source bodies are never stored in the database, logs,
  traces or error responses.
- **CalculationPolicyV1** — an immutable published revision selected by the service for the
  authenticated tenant/scope/window, referencing approved time/calendar and DST behavior, readiness
  and reason-code precedence, normalization and overlap precedence, A/P/Q/OEE/TEEP rules, downtime
  taxonomy and loss hierarchy, unit/rounding/scale rules, cost rates and effective dates, and
  output/retention limits. A normal request cannot choose a draft or arbitrary historical policy; an
  expected revision may be supplied only as a concurrency precondition.
- **OeeEvidencePackV1** — terminal and immutable: identity, schema version, tenant scope, requested
  window, readiness status, ordered bounded reason codes, exclusions and assumptions, truth state,
  metrics only where status and policy permit, the numerators/denominators/durations/quantities
  needed for reconciliation, dataset/policy/code/taxonomy/rate digests, source snapshot provenance
  and run identity.

## Nine-stage pipeline

1. **Admission** — authenticate, authorize, enforce body/fact/window/deadline bounds, reject
   caller-supplied tenant identity.
2. **Contract verification** — strict schema, identities, revisions, units, time range, provenance,
   source scope, canonical digest.
3. **Policy selection** — exactly one published effective policy. Missing, ambiguous or stale is
   `NOT_READY`.
4. **Readiness evaluation** — detect missing, stale, duplicate, conflicting, cross-tenant, ambiguous
   or unsupported facts. Do not repair them.
5. **Normalization** — canonical timeline using explicit timezone, calendar, interval, counter,
   quality and revision rules.
6. **Calculation** — approved components from explicit contributions. Zero denominators, counter
   resets, overlaps, gaps and out-of-range values follow policy reason codes, never hidden by clamping.
7. **Loss and cost** — classify through the versioned hierarchy; apply only effective,
   unit-compatible, currency-explicit rates.
8. **Evidence construction** — reconcile every output to inputs, policy, code version, exclusions
   and provenance.
9. **Atomic sealing** — commit one terminal run and immutable evidence record, or one safe
   failure/cancellation outcome.

The kernel receives time, rounding, unit conversion, taxonomy and rate values as explicit arguments.

## Strict schema rules

Every object closes with `additionalProperties: false`, including nested objects and discriminated
unions. Every collection has a maximum length; every string a maximum UTF-8 length; every numeric an
explicit minimum, maximum, precision and scale. UUID, enum, timestamp, timezone, unit, currency and
identifier formats are explicit. Decimal quantities and ratios use canonical **strings** matching a
strict regex — a bare decimal type lets raw JSON numbers through, so wire DTOs use a bounded
canonical-decimal string wire type. Windows are half-open, finite and bounded. Source identity is a
stable owner ID plus owner revision — timestamps and values are not identity substitutes. Digests are
over canonical serialization, not incidental map or database ordering; persist the schema ID,
canonicalization version and digest algorithm. Public DTOs are immutable after validation. Error
codes are finite enums with bounded safe detail.

No dynamic imports, `eval`, `exec`, shell calls, pickle deserialization, template execution or
user-supplied expressions. SQL is parameterized; repositories expose allowlisted operations only;
one database session per task with explicit cancellation propagation.

## Retention roles

A normal runtime role cannot delete rows from append-only tables. Retention cleanup runs under a
dedicated maintenance role that activates transaction-local authorization, and every retention
transaction inserts an audit record naming the rule, purged scope and row count **in the same
transaction** before committing. Purges follow strict cascade order so no orphan reference survives.

## Delivery order

1. Repository tooling, strict contracts, error model, migrations, RLS and fake datasets — no formulas.
2. Policy revision/publication and run reservation with idempotency and audit.
3. Contract verification, readiness and pure timeline interfaces.
4. Each formula/loss/cost child **only after** its golden vectors and product rules are approved.
5. Immutable evidence sealing and read-only retrieval.
6. Published OpenAPI/JSON Schema artifacts and consumer fixtures.
7. Integration through WIS-231, shadow compare, cutover through its audited authority transition.

A local fake adapter may demonstrate deterministic behavior but must be labelled non-live. No
endpoint may return fabricated finalized metrics to make the service look complete.

## Release and supply-chain gates

Locked reproducible dependency resolution, supported runtime image, SBOM, license review and
vulnerability policy; signed image and provenance attestation, non-root execution, read-only root
filesystem where compatible, dropped capabilities, seccomp sandboxing and resource limits;
dependency, secret, static-analysis, type-check, container, migration and API compatibility scans;
migration rehearsal against both an empty database and the previous production schema, explicitly
testing composite foreign keys; RLS and concurrency tests against the production-equivalent
non-owner role; golden vectors, property tests, fuzz tests, cancellation and race tests, and a
no-skipped-test gate; deployment health gates, gradual rollout, rapid rollback and a post-release
evidence replay check.

**No release is production-ready merely because the service starts.**

## Change control

Changes to source authority, tenant boundaries, contract families, finalized-result ownership,
policy selection, transaction publication or cutover semantics require a superseding ADR. Formula,
readiness, taxonomy, unit and cost changes require a new immutable policy revision plus golden
vectors. Code version and policy digest are always in evidence provenance, so a replay can never
silently acquire new semantics.
