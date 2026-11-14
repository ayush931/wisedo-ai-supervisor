# Advisory shift-plan suggestions (WIS-520)

`POST /v1/sites/{site}/shift-plans/suggestions` computes an editable draft. It
writes no plan, approval, assignment, receipt or plan-creation audit. The existing
create/revise and distinct-supervisor approval routes remain the only way to
publish a plan. Browser-session activity may still be refreshed by authentication.

Like station `profile-previews`, this POST requires the session, same-origin CSRF
proof and strict JSON, but **does not require Idempotency-Key**: there is no durable
operation to replay. Owners and supervisors may compute; operators and viewers
receive 403. Foreign sites and inaccessible job IDs return non-disclosing 404.

```json
{
  "schema_version": 1,
  "line_id": "00000000-0000-4000-8000-000000000001",
  "starts_at": "2026-10-20T08:00:00Z",
  "ends_at": "2026-10-20T16:00:00Z",
  "timezone": "UTC",
  "job_order_ids": ["00000000-0000-4000-8000-000000000002"]
}
```

Times use explicit offsets matching the IANA timezone. The window is half-open
and at most 24 hours. Unknown fields, including capacities, cycle times, actor and
tenant, are rejected with 422 `INVALID_PLAN_SELECTION`. Duplicate IDs are invalid.

The response `ShiftPlanSuggestionV1` contains:

- `method`: `greedy` or `ortools_cp_sat`; `is_optimized` is true only for a validated
  CP-SAT `OPTIMAL` result. `FEASIBLE` is labelled without claiming optimality.
- `job_orders`: the exact current revisions and bodies used (the same body/digest
  checks as `include=current`, without loading unbounded history).
- Optional `process_snapshot_id`: the current approved process cut used only for
  a conflict cross-check; omitted when no readable current snapshot exists.
- `assignments`: existing `AssignmentV1` rows, ready for human editing/submission.
- `operations`: validated timing preview. These times are advisory; the persisted
  shift-plan format has no per-assignment start/end fields.
- `unplaced_orders`: job-order IDs and stable `reason_code` values.
- `warnings` and optional `solver_status` / `fallback_reason`.

## Authoritative inputs and routing

All inputs are read under one tenant-scoped repeatable-read transaction with the
existing session/membership policy. Current station-profile bodies are digest
checked, and only profiles on the requested line can be selected.

Job orders may supply `item_id` (catalog UUID), `routing_id` (UUID) and
`step_number` (positive integer), or omit all three. Partial, empty or null links
return 422 `INVALID_JOB_ORDER`. Create/revise validates the site's current active
routing, step and matching `item_id`. `item_code` remains the human product code
or name used by operators, assignments, summaries and handovers. The optional
fields are omitted from legacy canonical payloads; old unlinked bytes and digests
are unchanged. The job-order editor selects the catalog item with the route and
allows editing the human code. There is no join on display names and no migration.

Planning reads `routing_steps.workstation_id` from the routing head's active
revision and verifies it against the canonical routing body. A linked step on a
different routing or profile line is unplaced with `ROUTING_NOT_ON_LINE`.
Items have no external-code field in the catalog, so an unlinked `item_code`
cannot identify a catalog item, including when it looks like a UUID. Such jobs
are `ROUTING_UNLINKED`: link the job to a routing step. Internally the resolver
can resolve an identified `item_id` through exactly one active route and an
unambiguous step (multi-step mappings use at most 100 current linked peers).
The write contract requires all three fields, so new jobs always supply the
explicit link. Multiple active routes are never resolved by guessing.

A readable, current approved knowledge snapshot is an optional cross-check. A
`performs` claim for the operation naming a different station during any part of
the shift yields `ROUTING_AMBIGUOUS`. Missing, stale or evidence-unavailable
snapshots are ignored with `KNOWLEDGE_CROSS_CHECK_UNAVAILABLE`.

A job-order body represents one operation. Its revision supplies quantity, cycle
time, priority, due time and pinned instructions; instruction approval is checked
again. Missing cycle time, routing or instructions remain explicit refusals.

## Capacity, precedence and breaks

The current persisted plan allows one job per workstation for the whole shift,
with no numeric parallel-capacity setting in station profiles. Suggestions use
capacity 1 and the same one-job constraint, choosing jobs deterministically by
priority (1 first, unspecified last), due date, then ID. The response warns
`ONE_JOB_PER_STATION`; this does not imply measured physical capacity.

Selected jobs with the same production `order_id` follow their routing step order
as a **flow line** (product decision, 2026-10-30): parts move on one at a time, so
the steps overlap. A later step starts no earlier than one predecessor cycle after
the predecessor starts, and ends no earlier than one own cycle after the predecessor
ends. Each such suggestion warns `ROUTING_FLOW_OVERLAP_ASSUMED`: one-piece transfer
with no buffer limit is a modeling assumption, not measured line behavior, and
mid-run starvation caused by a predecessor's break is not modeled.
Applicable approved `precedes` claims between selected operations add
finish-to-start dependency edges (a claim on a routing pair makes it finish-to-start).
Cycles or ambiguous selected steps yield `ROUTING_PRECEDENCE_UNSUPPORTED`;
an unplaced selected predecessor yields `ROUTING_PREDECESSOR_UNPLACED`. An omitted
related operation warns `EXTERNAL_PRECEDENCE_NOT_SCHEDULED` instead of refusing
all planning. Under the one-job-per-station constraint, Go computes each job's
earliest start allowed by its placed predecessors (including breaks) and supplies
it as a ready-time bound to the existing solver interface. Returned actual operation
times are checked against every selected edge; a solver result that violates one
falls back to the validated greedy schedule. No C++ change is needed.
These times remain a preview; saving a plan still records whole-shift assignments.

Recorded planned breaks come from overlapping approved production-plan revisions
for the line or its stations. The shared intake approval predicate rechecks exact
payload digests, current decisions, supersession and revoked source spans.
Overlapping breaks are merged before being sent to the solver. Unavailable
approval provenance refuses the computation. No recorded breaks means an empty
known-break set, with `RECORDED_APPROVED_BREAKS_ONLY`; it does not certify that no
other downtime exists. No unreviewed machine event is treated as planned downtime.

When every station on the line has the same breaks (for example breaks recorded on
the line itself), work pauses over a break and resumes after it (product decision,
2026-10-30). Go plans in working time with the breaks removed and maps the times
back, so an operation's `starts_at`/`ends_at` span includes any break it crosses
while `duration_seconds` stays the working time. The solver interface is unchanged.
If stations have different breaks, an operation still needs an unbroken window.

Unplaced codes: `ROUTING_UNLINKED`, `ROUTING_NOT_ON_LINE`, `ROUTING_AMBIGUOUS`,
`ROUTING_PRECEDENCE_UNSUPPORTED`, `ROUTING_PREDECESSOR_UNPLACED`,
`STATION_ALREADY_ASSIGNED`, `CYCLE_TIME_MISSING`, `INSTRUCTION_UNAVAILABLE`,
`STATION_TEMPLATE_MISMATCH`, `SHIFT_CAPACITY_EXCEEDED`, `INVALID_DURATION`.
No placements returns 422 `NO_FEASIBLE_SCHEDULE`. Genuinely missing station
profiles or unavailable break provenance return 422 `PLANNER_DATA_UNAVAILABLE`.
These are planner refusals, not server failures.

## Approved onboarding cycle times (WIS-628)

A suggestion compares each selected job order with the `cycle_time_seconds` claim approved for its
operation during onboarding. Only claims in the current, non-stale process snapshot that are
effective during the shift `[starts_at, ends_at)` count. `approved_cycle_times` lists one row per
such job order, with the claim IDs and cited source spans:

| `status` | Meaning |
|---|---|
| `matches` | The job order's value equals the approved value exactly (`60` equals `60.0`) |
| `differs` | The two values are different |
| `missing_on_job_order` | The job order has no cycle time, so it is still refused `CYCLE_TIME_MISSING` |
| `approved_conflict` | The approved values disagree; no value is offered |

`differs` and `missing_on_job_order` add the warning `APPROVED_CYCLE_TIME_DIFFERS`. This is
advice only. The planner always uses the job order's own value, and a supervisor changes a job
order through a normal revision. Approved `setup_time_seconds` is not used: the planner has no
setup model yet.

## Solver, connections and work bounds

`SUPERVISOR_WORKSTATION_CPSAT_PATH` defaults to the image's packaged
`/usr/local/bin/supervisor_workstation_cpsat`. A missing/unexecutable/failing binary,
search timeout/UNKNOWN, infeasible result, unsupported timestamp precision, or
rejected solver output falls back to `PlanGreedy`. Invalid solver output is never
returned. Request cancellation stops work instead of retrying it. The existing
six-second process timeout, cleared environment and Go output validation remain.
No network or database authority is passed to the child.

Limits are 50 selected job orders, 200 planner operations, 100 configured stations
on the line, and 100 merged downtime intervals. Current single-operation orders
also bound the actual operation count to 50. Reads of relevant production-plan
candidates and per-station break accumulation are bounded at 100. Excess work is
422 `PLANNER_LIMIT_EXCEEDED`.

A session advisory lock permits one solve per site across replicas; contention
is 409 `PLANNER_BUSY` with no queued solver. One dedicated connection holds that
lock through the bounded solve, but its read transaction is committed before
invoking the child: no MVCC snapshot or row locks remain open during the solve.
Session/membership are checked again in a short fresh transaction before returning.
Every exit rolls back any remaining transaction. Acquired locks are released
with an independent two-second cleanup context; a busy request never unlocks.
Failed cleanup or uncertain acquisition discards the connection rather than
returning a possible session lock to the pool.

## UI, verification and rollback

Use **Job orders** to create or revise an operation and optionally choose its
routing and step. Changing a routing selects its catalog item and resets the step.
A new item defaults to its catalog display name; an existing human code is kept
when linking a manual job. Unlinking omits all three link fields. Revision writes
retain `If-Match` and idempotency keys.

In New plan / Revise, select job orders and choose **Suggest assignments**. Inputs
are disabled while the request runs; progress and result status are announced.
Changing the line, timezone, window or selected orders clears prior suggestion
status and refusals. Unplaced codes have human-readable guidance. Replacing
nonempty rows requires confirmation. Returned revision bodies replace the form's
cached versions for those jobs, so the draft pins what was actually planned.
Rows remain editable; Create/Save remains a separate action. Creation rechecks
current revisions and instructions. Another supervisor must approve the draft.

Run `make supervisor-check`, `make supervisor-contracts`, and PostgreSQL-backed
`make supervisor-integration CASE=WIS-520`. The actual native CP-SAT HTTP test is
compiled with the additional `cpsat` build tag: set `SUPERVISOR_TEST_CPSAT=1` and
`SUPERVISOR_WORKSTATION_CPSAT_PATH` (plus the solver's library path if needed).
An explicitly selected native test without its configuration fails with `BLOCKED`.
The default integration suite does not include that optional native test.

`web/tests/shift-suggestions.spec.ts` and `job-order-routing.spec.ts` explicitly
label their API responses as stubbed. PostgreSQL HTTP tests separately prove
authority, server-side inputs, real solver/fallback behavior, read-only effects
and distinct-human approval. These checks do not establish deployed or
customer-qualified scheduling.

Rollback before any linked job revision is written may revert this fix. After
linked revisions exist, retain the new optional-field reader (or forward-fix):
older strict decoders cannot read those revisions, including immutable history.
Do not delete history to roll back. No database migration is involved.
