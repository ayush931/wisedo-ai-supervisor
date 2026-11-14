---
name: supervisor-operational-intake
description: Vendor-neutral operational source registry, authenticated event intake, CSV/JSON import, the inbox/outbox progress fold and source freshness/revocation (WIS-272) across internal/operational, internal/ingestion and internal/dispatch. Use this skill for any work on SourceRecordV1, batches, watermarks, completeness, backpressure or source authority - and whenever telemetry, plans or operator reports enter the system.
---

# Operational intake and source authority

Covers `internal/operational/`, `internal/ingestion/`, `internal/dispatch/`, migrations `020`–`022`, `026` (import jobs),
`031` (revocation), `042` (CSV error-export audit).
Source: Linear WIS-272. Governing rules: `.claude/rules/06-evidence-provenance-and-fail-closed.md`,
`.claude/rules/07-api-contract-conventions.md`.

PostgreSQL inbox/outbox plus Temporal durable workflows. No LLM per telemetry event, no mandatory
broker and no MES.

## SourceConfigV1

`source_id`, `site_id`, `type` (`csv|json|event_api|manual`), `authority_scope` (record kinds plus
asset/order IDs), optional `credential_ref`, `mapping_revision`, `enabled`,
`access_policy_revision`, `freshness_seconds`, `clock_skew_seconds`,
`completeness_mode: contiguous_sequence`, `record_schema_version: 1`.

The customer approves mapping and authority. Overlapping authoritative sources for the same record
scope produce `CONFLICT`, never last-writer-wins. Credentials stay out of source bodies and prompts.
Only owner or supervisor may register or change mappings and source authority; ingestion credentials
cannot self-expand.

## SourceRecordV1

Client fields: `schema_version`, `source_id`, `external_key` (1–128 chars), `revision` ≥ 1, `kind`,
`source_sequence` ≥ 1, `event_at` RFC3339, optional `asset_external_key` / `order_external_key`,
`payload`, optional `correction_of`. The server adds `record_id`, `tenant_id`, `site_id`,
`received_at`, `sha256`, `quality` (`valid|late|clock_suspect|quarantined`),
`source_access_revision`, `mapping_revision`, `source_span_id`.

Unique `(tenant, site, source, external_key, revision)`. An identical digest is a replay; a differing
digest is `409 SOURCE_REVISION_CONFLICT`. Corrections append and name their exact predecessor — no
gaps, no branching chains. Missing or ambiguous entity mappings quarantine with `UNKNOWN_MAPPING`;
never join on names.

## Payload kinds

| Kind | Notes |
| --- | --- |
| `production_plan` | non-overlapping rate segments covering each planned productive interval, contained non-overlapping breaks, max 1000 of each; `approval_ref` resolves to an exact approved canonical payload digest. Record rate 0 explicitly for planned non-production; an absent rate is unknown. |
| `production_delta` | exact interval, same order/unit/window; corrections replace the referenced fact's contribution. Counter-mode sources need a separately qualified counter/reset adapter. |
| `machine_state` | `running|stopped|unknown` plus optional reason code. Never equate `stopped` with a bottleneck or cause. |
| `operator_report` | text 1–8000 chars from an authenticated human; an external reporter stays unverified attribution. |
| `heartbeat` | liveness only, never completeness. |
| `watermark` | authenticated assertion that everything through a sequence/time was delivered. |

## API and import semantics

`POST /operational-sources`, `/operational-sources/{id}/batches` (1–1000 records),
`/operational-sources/{id}/imports` (multipart), `GET /operational-ingestion-jobs/{id}`,
`GET /operational-ingestion-jobs/{id}/errors.csv`, `POST /operational-sources/{id}/mappings`,
`POST /operational-sources/{id}/reviews`, `GET /operational-sources/{id}/liveness`,
`POST /operational-sources/{id}/revoke`, `GET /external-identities/resolve`, plus
`POST /plan-revisions`, `POST /plan-revisions/{id}/reviews`, `GET /plan-revisions/{id}`.
The evidence-source API keeps `/sources`; the operational registry uses `/operational-sources`.

Record max 256 KiB, batch max 1 MiB streamed, CSV/JSON max 10 MiB / 50k rows, UTF-8 only. Return 202
only after durable inbox persistence, with `job_id`, `receipt_id` and accepted count — that is
receipt, not semantic acceptance. Format errors before persistence are 422. Chunks of 100 records
are atomic with per-row accepted/duplicate/quarantined status; no silent partial success. The import
cursor is immutable file SHA plus row index; the event cursor is contiguous sequence, never the max
sequence seen. Retain errored rows for authorized correction and escape formula-like cells on CSV
error export.

## Completeness protocol

Each source sequence begins at 1 and is unique and contiguous. A watermark occupies sequence N and
declares `through_sequence = N-1`. Advance completeness only when every preceding record is durable
and semantically eligible; quarantined or unresolved records block progress. The importer assigns a
deterministic stream sequence under lock and appends an authenticated watermark only after the whole
import validates durably. Import-only sources need an explicit completed range in the manifest,
reviewed against rows — never synthesize a watermark across unseen time. `complete_through` applies
only to event-time intervals explicitly represented by complete validated deltas.

## Backpressure, freshness and revocation

Envelope: 20 sources/site, 10 events/s sustained, 100 events/s for 60 s, 100k pending records or
1 GiB pending bytes per site. Beyond the limit return `429` with `Retry-After: 30`. Worker batch 100
with per-source fairness; 3 transient retries then dead-letter with a reason and idempotent manual
retry. Disk or DB saturation returns 503, never a dropped receipt.

Defaults: event-source freshness 120 s, tolerated future clock skew 30 s, import freshness from the
last successful completed import with max age 24 h, approved document review expiry 90 days. Suspect
events do not establish readiness. Late arrivals append and invalidate impacted current contexts.

Revocation increments the access-policy revision and disables the source in one transaction: pending
jobs cancel or block, derived claims and contexts become ineligible, and future retrieval is denied
immediately. Revoked content is never sent to a model.

Implemented (`operational.Service.Revoke`, migration 031): `POST /operational-sources/{id}/revoke`
with no body, If-Match on the current source revision, an Idempotency-Key and an owner/supervisor
with recent identity proof (428 otherwise). It appends a disabled revision with access-policy revision
+1 and cancels the source's running import with counts frozen from recorded rows (the importer then
records no more rows). Intake refuses the source (412 `SOURCE_REVISION_CONFLICT`), contexts built
under the old access-policy revision read `SOURCE_NO_LONGER_APPROVED`, and the access-policy
revision can never decrease (422 `ACCESS_POLICY_REGRESSION`; database trigger as backstop), so a
re-enabled source cannot revive contexts from before the revocation.

## Verification

`make supervisor-integration CASE=WIS-272` / `CASE=ingestion`. Cover fresh and populated migrations,
real HTTP parsing, stream overflow without `Content-Length`, CSV injection export, malformed
timestamps and decimals, equal-digest replay and conflict, sequence gaps, identical IDs across
tenants, wrong site, unknown mapping, interval overlaps and gaps, correction-chain branching,
watermark ahead of a missing sequence, DST, clock skew, stale heartbeat, restart at each commit
boundary, buffer full, revocation mid-job and retained-history deletion.
