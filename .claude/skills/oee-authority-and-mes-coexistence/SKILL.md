---
name: oee-authority-and-mes-coexistence
description: ADR 0001 - who owns which fact, the PROVISIONAL/CANDIDATE/ACCEPTED/SUPERSEDED truth states, and the shadow-validation, cutover and rollback plan for the retained OEE core against MES (WIS-223, WIS-231). Use this skill whenever work touches OEE result authority, MES integration, shadow comparison, cutover flags, or any question of whether an OEE number is allowed to be treated as final.
---

# OEE authority, coexistence and MES cutover

Source: `docs/adr/0001-oee-authority-and-mes-integration.md` (frozen by WIS-223, merged 2026-09-19);
Linear WIS-231 (MES-owned, **outside this repository's implementation boundary**).
Governing rules: `.claude/rules/00-product-independence.md`, `.claude/rules/06-evidence-provenance-and-fail-closed.md`.

The OEE core is the sole target authority for **finalized-window** Availability, Performance,
Quality, OEE, TEEP, downtime/loss classification, modeled cost-of-loss and calculation provenance.
MES owner services remain authoritative for source facts. The existing MES `services/oee-svc` stays
a provisional live accumulator during migration — it never becomes a second accepted source of
finalized formula truth.

The integration boundary is versioned OpenAPI/JSON over HTTPS. The core does not read another
service's database, consume an owner event stream as a projection, mutate MES state, write a PLC or
expose caller-selected network destinations. C++, MQTT and gRPC are not in this module's v1.

## Authority matrix (summary)

| Datum | Owner |
| --- | --- |
| Authenticated field ingress identity | MES IoT Gateway (provenance only, not a calculated fact) |
| Equipment state, counters, time, signal quality | MES Historian |
| Work order, operation, product, declared quantity | MES Production |
| Inspection and final quality disposition | MES Quality |
| Asset, work-center, routing, ideal-cycle references | MES Master Data |
| Shift, calendar, planned exclusion, timezone | MES Master Data calendar |
| Calculation and readiness policy | **OEE core** |
| Canonical normalized timeline | **OEE core** |
| Finalized A/P/Q/OEE/TEEP | **OEE core** (`OeeEvidencePackV1`) |
| Downtime/loss hierarchy and modeled cost | **OEE core** |
| Provisional live metric | MES `services/oee-svc`, labelled `PROVISIONAL` |
| Report layout, scheduling, delivery, export | WIS-232 Reporting |
| Finalized-authority routing, shadow, cutover flag | **WIS-231 in MES** |
| PLC/HMI commands and machine safety | MES edge/command subsystem — never this module |

Modeled-cost rates are OEE-owned governed policy inputs administered by an authorized role. They are
not ledger truth, do not imply realized savings and cannot perform implicit FX conversion.

## Source conflict rule

Facts are not merged because they share timestamps or labels. Each fact carries an authoritative
owner, stable identity, revision, effective scope, occurred time, recorded time, unit and quality.
Duplicate, contradictory, cross-tenant, unversioned or ambiguously scoped facts are never silently
selected — the published policy decides `LIMITED`, `NOT_READY` or `INVALID`. It never invents facts.

## Truth state is not readiness

Readiness (`READY|LIMITED|NOT_READY|INVALID`) says whether a dataset and policy can make a governed
claim. Truth state says how a result may be used during migration:

- `PROVISIONAL` — live compatibility output from the existing MES service.
- `CANDIDATE` — immutable new-core evidence available only for shadow comparison.
- `ACCEPTED` — the pack selected by the server-owned finalized authority registry for a scope and
  authority epoch.
- `SUPERSEDED` — immutable evidence replaced by a correction, policy revision, calculation revision
  or authority transition.

A `READY` candidate is still not accepted. Only the audited finalized-evidence router attaches
accepted authority metadata. Legacy provisional output is never upgraded into an evidence pack.
**There is no `BOTH_ACCEPTED` state.**

## Shadow → cutover → rollback

`ArchitectureProposed → ContractReady → CoreCandidate → Shadow → CutoverCandidate → Authoritative
→ LegacyRetired`, with `CutoverBlocked` and `RollbackApproved` branches.

Each shadow variance record carries tenant/site/asset/window identity, old and new result identities
and versions, dataset/policy/code digests, component-level A/P/Q and duration/count contributions, a
classified cause (expected policy change, known legacy defect, source-contract gap, implementation
defect, or unresolved) and a reviewer disposition. **Unexplained material variance blocks cutover**;
tolerances and minimum observation duration are WIS-231 product decisions, not constants invented in
code.

Cutover is an audited MES routing decision that moves the finalized authority registry from `NONE`
to an exact core provider/version for a tenant/site cohort and increments the authority epoch. It is
never a request parameter. It requires WIS-222 v0.1.0 release evidence, WIS-232 consumer
compatibility, owner-contract integration gates, completed shadow criteria, a clean security and
architecture review, explicit product and integration-owner approval, and a tested rollback.

Rollback moves the registry back to `NONE` and increments the epoch. It does not relabel the legacy
value as finalized evidence, does not delete new-core runs and does not mutate sealed evidence. The
router never falls back automatically per request — it exposes a bounded unavailable/not-ready
result. Legacy retires only after the declared retention window, full consumer migration and
retained rollback evidence.

## MES producer-contract gaps WIS-231 must close first

Downtime events need tenant, plant, asset, stable event ID, source and recorded time, classification
revision and durable publication. Production completion needs work order, item/product, routing ID
and revision, effective standard-cycle seconds, count units and a transactional outbox. Quality facts
need a stable event ID, tenant/site/order/product scope, accepted/reject/scrap/rework quantities and
disposition — **one inspection is never inferred to equal one unit**. Shift and calendar contracts
need effective-dated IANA zones with DST fold/gap rules. Insights must project the canonical result,
never average percentages or implement a second formula path. Every schema must be committed with
backward-compatibility and generated-consumer tests before use.

Owner split: Anshul Rathore owns MES integration, contract acceptance, cutover and rollback. Ayush
owns the independent WIS-222 module only.
