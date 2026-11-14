---
name: supervisor-case-lifecycle
description: Durable exception cases, trigger deduplication, the SupervisorCaseWorkflow Temporal orchestration and the case state machine (WIS-273). Use this skill for any work on CaseV1, case triggers, state transitions, clarifications, escalation, reopening, workflow signals, continue-as-new or runtime bounds - and whenever a shortfall needs to become durable work.
---

# Case lifecycle and durable orchestration

Source: Linear WIS-273. Implemented: the pure state machine `internal/cases/state.go` and migration
027 (PRs #65/#66/#86); triggers, reads and the cancel/resume/reopen lifecycle (migration 045,
`internal/cases/store.go`, `internal/httpapi/cases.go`); `SupervisorCaseWorkflow` and the case
outbox relay (migration 046, `internal/cases/workflow.go`, `internal/dispatch/cases.go`). Not built:
the investigation activities (WIS-275/276), the per-site/per-process concurrency queue and provider
breaker, `/close` (needs WIS-277 tasks and outcomes), and an HTTP trigger route
(service principals, WIS-290). Automatic triggering is built (migration 047,
`internal/cases/evaluate.go`, `dispatch.EvaluateCases` in the worker loop).

## Implementation map

- `cases.Service.Trigger(scope, ServiceActor, TriggerInput)` is internal: `source_event_id` is the
  deterministic `record_id` of a production_delta record the context selected
  (`plantcontext.RecordID`). The context and the previous 15-minute window (same order, line and
  process snapshot, found with `plantcontext.HeadTx`) must both be READY and eligible now
  (`plantcontext.LoadTx`) and pass `EvaluateShortfall`; otherwise `ErrNotQualified`. The site write
  lock serializes triggers, so duplicates replay (same source event, same request) or join the
  active episode (evidence added while under 100, outbox `signal`); a new case writes the case,
  transition 1, evidence, trigger record and outbox `start` in one transaction. After a terminal
  episode a trigger opens the next episode with `previous_case_id`.
- Migration 045: `case_triggers` (idempotency per source event, one opener per case),
  `case_outbox` (only `delivered_at` may be set, once), `case_receipts` (per actor and UUID key).
- Lifecycle mutations need a recently authenticated site supervisor (owner is refused), a UUID
  `Idempotency-Key` and `If-Match` equal to `expected_revision`. Cancel from executing/observing is
  refused until committed tasks can be canceled (WIS-277). Resume binds a new eligible READY
  context of the same order and line and is refused when the episode has an approved or consumed
  proposal. Reopen is allowed only on the latest episode with none active; the new episode copies
  the trigger evidence and references the old one.
- `POST /cases/{id}/clarifications` (migration 049, `cases.Service.Clarify`): an active site
  supervisor or operator (no recent sign-in needed; owner and viewer refused) answers a case in
  needs_input. The text (1..4000 characters) is appended to `case_clarifications` with its
  SHA-256 and actor, in the same transaction as the needs_input -> investigating transition and
  an outbox signal; `new_context_id` optionally binds a new eligible READY context for the same
  order and line. Completeness is judged again by the investigation (WIS-276). Any other state is
  409 `ILLEGAL_TRANSITION` and records nothing.
- Error codes: 409 `ILLEGAL_TRANSITION`, `TRANSITION_GUARD_FAILED`, `CASE_EPISODE_ACTIVE`,
  `CONTEXT_NOT_ELIGIBLE`, `IDEMPOTENCY_CONFLICT`; 412 `REVISION_CONFLICT`; 422 `INVALID_CASE_REQUEST`.
- `SupervisorCaseWorkflow` (`internal/dispatch/cases.go`, workflow ID `case:<tenant>:<case_id>`)
  only schedules: each loop re-reads the case (`ReadCaseStatus`), records every signal in
  `case_workflow_events` (`RecordCaseEvent`, deduplicated by event ID, so at-least-once delivery
  has one effect), and while the case waits on a person (needs_input, awaiting_approval) runs a
  timer to `cases.HumanWait` (24 h) measured from the transition that began the wait.
  `EscalateCaseOnDeadline` is fenced on the exact revision and the database clock; a fenced timer
  re-waits at least a minute. A terminal case drains queued signals and ends; after 1000
  processed signals it continues as new with only its reference. Activities: 30 s, at most 3
  attempts with 1 s/2 s backoff; scope or contract failures are non-retryable.
- `cases.Service.EvaluateCreatedContexts` consumes `context.created` rows of
  `plant_context_outbox` that have no `case_context_evaluations` row: a READY context raises one
  trigger per delta record through `Trigger` (actor `supervisor:evaluator`), and the outcome
  (opened, joined, already_triggered, not_qualified, not_applicable) is recorded once. A storage
  failure records nothing and stops the pass, so the context is retried on the next tick.
- `dispatch.RelayCases` (worker loop in `cmd/supervisor`) delivers outbox rows with
  signal-with-start and marks them delivered only after Temporal accepts; without Temporal rows
  stay pending.
- The SDK test environment deadlocks beyond 1000 delayed callbacks, so the continue-as-new unit
  test lowers `caseSignalsPerRun`; the real-service test covers relay, two workers, escalation,
  dedupe, cancellation, history replay and Temporal outage.
- Tests: `TestCaseTriggersAndLifecycleHTTPPostgres` drives the real WIS-272 path (plan, intake,
  fold, five contexts); `TestCaseWorkflow*` (deterministic, time-skipping) and
  `TestCaseWorkflowRealTemporalPostgres`. All are in `CASE=WIS-273`, which needs Temporal.
Governing rules: `.claude/rules/05-human-authority-and-approvals.md`, `.claude/rules/03-tenant-isolation-and-rls.md`.

PostgreSQL owns business state; Temporal owns scheduling, timers and retries. No Redis, no Celery,
no second scheduler, no continuously running model loop. Bounded work is woken by source events,
approved configuration changes, human replies and durable timers.

## CaseV1

UUID `id`/`site`/`line`/`order`, `exception_kind = production_shortfall`, `episode` ≥ 1,
`revision` ≥ 1, `context_snapshot_id`, `context_revision`, nullable `owner_user_id`, `state`,
`detected_at`, `latest_window_end`, `trigger_evidence_ids` (1–100), `workflow_id`, nullable
`resolution_reason`. Context evidence is referenced, never copied into Temporal payloads.

Deduplication: a partial unique index on `(tenant_id, site_id, line_id, order_id, exception_kind)`
for non-terminal episodes.

## Triggers

`POST /cases/triggers` is scoped ingestion/service-only, with idempotency. A trigger is accepted
only after the case, evidence association and outbox persist atomically; it returns 202 with
`case_id` and revision. The outbox consumer starts or signals the stable workflow ID
`case:<tenant>:<case_id>`. Delivery is at-least-once; duplicate signals deduplicate by event ID in
the product database. SQL transition uniqueness plus expected revision and fencing prevents duplicate
effects — workflow durability alone does not imply exactly-once activities.

## States

`detected → investigating` (ready immutable context, scope enabled);
`detected|investigating → needs_input` (missing or contradictory required evidence);
`needs_input → investigating`; `investigating → proposed`; `proposed → awaiting_approval`;
`awaiting_approval → executing`; `executing → observing`; `observing → resolved` (task terminal,
eligible measured outcome and supervisor closure). Any non-terminal state may go to `escalated`
(fatal policy/validation/budget failure or wait deadline) or `canceled` (authorized cancellation).
`escalated → investigating` when a supervisor resumes with new context. `resolved|canceled` reopen
as a new episode; old history stays immutable.

A rejected, expired or stale proposal does not execute — the case goes to `needs_input` with a
reason. Task completion is not case resolution. Automatic exception clearing only records an
outcome; a supervisor closes. `escalated` remains active for deduplication until explicitly canceled
or resolved. A clarification creates source evidence; it never silently edits an observation.

## Lifecycle API

`GET /cases`, `GET /cases/{id}`, `POST /cases/{id}/clarifications`, `/cancel`, `/reopen`,
`/close` (supervisor-only, task terminal and outcome recorded, including explicit
`insufficient_data` / `not_comparable`), `/resume` (escalated, with reason and new context). All
take `expected_revision` and an idempotency key. A closure reason can never manufacture improvement
over a measured outcome.

## Runtime bounds

2 active investigations per site, 10 per process with fair queuing; one investigation cycle 180 s
wall time; model attempt timeout 45 s with max 2 attempts including schema repair and no nested
retry multiplication; transient source/DB reads max 3 attempts with 1 s/2 s backoff; DB writes
reconcile by business key before retry. Human wait 24 h → `escalated`; there is no automatic
approval. Continue-as-new after 1000 processed signals carrying minimal IDs, version and cursor.
Provider breaker: 3 failures in a rolling 5 calls opens for 60 s with one half-open probe; site
queues stay isolated.

## Recovery

Crash after SQL commit but before signal → outbox retry. Crash after an activity's business commit →
read the existing result by operation ID. Temporal missing → accept only if SQL and outbox are
durable, and show queued. SQL missing → 503 with no accepted state. Pin the worker build and replay
old histories before deployment. Already committed tasks need explicit cancellation, not a
fictitious rollback.

## Verification

`make supervisor-integration CASE=WIS-273` against real PostgreSQL and a Temporal test server:
100 duplicate triggers → one case, two workers, duplicate and out-of-order signals, cancellation
races, outbox restart, timers, old-history replay and cross-tenant IDs.
