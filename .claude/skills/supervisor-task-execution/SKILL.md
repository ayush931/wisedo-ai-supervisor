---
name: supervisor-task-execution
description: Deterministic execution of an approved action into one internal TaskV1, the task state machine, durable readback and OutcomeObservationV1 (WIS-277). Use this skill for any work on action commitment, task assignment, acknowledgement/completion, reopening, reconciliation or post-action outcome measurement - and whenever completion might be confused with recovery.
---

# Approved task execution and outcome

Source: Linear WIS-277 (Linear: Done, checked 2026-11-03). Built and integration-tested, not
customer-accepted:
- the executor: `internal/cases/execute.go` calls `approvals.Settle` and re-checks eligibility
  (`internal/approvals/dispatch.go`);
- the action ledger and tasks (migration 066);
- the task lifecycle (067) and outcome observations (068);
- `internal/tasks`;
- `GET /tasks`, `GET /tasks/{id}` and the task transition routes (`internal/httpapi/tasks.go`).
There is no route that creates a task. The web Tasks screen is not built (WIS-630).
Governing rules: `.claude/rules/05-human-authority-and-approvals.md`, `.claude/rules/06-evidence-provenance-and-fail-closed.md`.

Only the deterministic executor may consume an approved `maintenance_investigation.create`. Human
and API callers submit proposals; there is no bypass endpoint. Temporal retry is transport recovery,
not authorization.

## Atomic execution

One PostgreSQL transaction with the lock order **scope_policy → prerequisite membership/source/
version rows sorted by stable ID → case → proposal/approval → action ledger**. It locks scoped policy
state and verifies the current enabled mode and capability, reviewer eligibility and revocation,
approval expiry, the exact canonical hash, bound versions and current readiness predicates. Remote
reality cannot be transactionally locked, so freshness limits and explicit uncertainty remain
required. If evidence changed materially, mark the approval stale and request review.

A stable `action_id` is generated once per approved proposal revision.
`UNIQUE(tenant_id, proposal_id, proposal_revision, action_kind)` and `UNIQUE(tenant_id, action_id)`.
The transaction inserts the task and its initial event, marks the action committed and the approval
consumed, and writes audit plus outbox. Crash before commit → no task; crash after commit before
acknowledgement → read the same task by `action_id`. A retry never generates a new identity.

Action states: `pending → committed|rejected|canceled`. `committed` means an internal task exists,
not that work was performed. Repeated execution returns the task receipt; a conflicting payload is
409. An unavailable database is 503 with no success in the UI.

## Task API and states

`GET /tasks`, `GET /tasks/{id}`; `POST /tasks/{id}/acknowledge`, `/start`, `/block`, `/complete`,
`/cancel`, `/reopen`. Writes require `Idempotency-Key`, `If-Match` revision, a verified scoped
principal and a server timestamp.

Reads (WIS-634): an operator reads only the tasks assigned to them. Another person's task is
the non-disclosing 404 an unknown task gets, and lists leave it out; the filter is in SQL in
`tasks.Service.List`/`Get`, not in the UI. Supervisors, owners and viewers read the site's tasks.

`assigned → acknowledged → in_progress → completed`; `acknowledged|in_progress → blocked`;
`blocked → in_progress` via `/start` with a reason; any active state → `canceled`. Only the assignee
acknowledges, starts and completes their task; the site supervisor may block or cancel with a reason;
an admin role alone is insufficient. Completion takes `{expected_revision, completion_note (1..4000),
evidence_ids (1..20)}` validated as same-site immutable evidence with correct authorship — a photo is
optional, a structured report is required. Invalid transitions are 409.

Completion is never retroactively edited: a correction adds an event, and a supervisor may reopen
with a new revision and reason. `/reopen` is supervisor-only, moves `completed → in_progress` with a
correction event and a renewed observation epoch while retaining the original completion and outcome;
it resumes the same inspect/report scope and assignee. A material scope or assignee change requires
cancellation and a newly approved proposal — never a silent reassignment.

The task machine must be an approved `part_of_asset` descendant of the case line, never an arbitrary
same-site asset.

Overdue defaults: reminder 30 minutes after `due_at`, supervisor escalation 60 minutes after. No
automatic completion or cancellation.

## OutcomeObservationV1

Task/case/context IDs, baseline windows, post windows, planned and actual quantities with units,
measurement evidence IDs, eligibility flags, `status`
(`pending|observed_improvement|no_improvement|insufficient_data|not_comparable`), interpretation and
observer timestamp.

After a reported completion, evaluate the next two complete productive 15-minute windows, skipping
the partial window containing the completion. The same order, product, unit and plan-rate basis is
required; a changed basis is `not_comparable`. Both post windows with deficit ≤ 5% of expected →
`observed_improvement`; otherwise `no_improvement`. No trustworthy windows within 2 h elapsed →
`insufficient_data`. Never invent recovery during a shift break or offline period. Keep the actual
deficit values and label improvement precisely as threshold recovery, never causal proof. The
supervisor closes the case after viewing the task and the outcome; a manual closure reason cannot
overwrite the measured outcome.

## External adapters

External work orders are later capabilities needing a stable remote key, status lookup and
connector-specific qualification. Without reliable reconciliation an adapter stays read-only or
manual-export. An unknown remote commit is `uncertain`: block blind retry and reconcile or escalate
to a human. Do not implement speculative external executors in v1.

## Verification

`make supervisor-integration CASE=WIS-277` against a real database: two executors one task;
approval/policy revocation race; altered hash; stale evidence; crash boundaries; completion/cancel
race; foreign evidence; restore and retry; absent notification provider; unchanged and changed
production basis; invalid descendant; stale lineage; completion/reopen race; unchanged action ID.
