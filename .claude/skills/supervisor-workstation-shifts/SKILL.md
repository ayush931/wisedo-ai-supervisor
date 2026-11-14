---
name: supervisor-workstation-shifts
description: Manual-first workstation and shift planning in internal/workstation (WIS-520) - shift plans and their approval, job orders, station profiles, station records with corrections and authorizations, shift summaries, handovers and the advisory CP-SAT planner. Use this skill for any work on shift plans, job orders, workstation profiles or records, shift summaries or handovers, the CP-SAT solver, or the shift and workstation web screens.
---

# Workstation and shift planning

Covers `internal/workstation/` (`contracts.go`, `shiftplans.go`, `joborders.go`, `profiles.go`,
`records.go`, `summary.go`/`summaries.go`, `handover.go`/`handovers.go`, `planning.go`, `cpsat.go`),
the HTTP files `internal/httpapi/shift_plans.go`, `job_orders.go`, `station_profiles.go`,
`station_records.go`, `shift_summaries.go`, `shift_handovers.go`, contracts in
`internal/contracts/workstation.go`, migrations `032`–`038` and `080` (shift runs, `shiftruns.go`,
`internal/httpapi/shift_runs.go`), the solver `src/workstation_cpsat/`,
and the web screens `web/src/shifts.tsx` and `workstation.tsx`.
Source: Linear WIS-520. Governing rules: `.claude/rules/01-go-only-runtime-boundary.md` (the CP-SAT
exception), `.claude/rules/05-human-authority-and-approvals.md`, `.claude/rules/07-api-contract-conventions.md`.

## Routes (prefix `/v1/sites/{site}`)

| Family | Routes |
| --- | --- |
| Shift plans | `POST/GET /shift-plans`, `GET /shift-plans/current`, `GET /shift-plans/{plan}`, `POST /{plan}/revisions`, `POST /{plan}/approvals` |
| Summaries | `/shift-plans/{plan}/summaries` (generate, list, get) |
| Handovers | `/shift-plans/{plan}/handovers` (generate, list, get, `/{revision}/adopt`) |
| Shift runs | `POST /shift-plans/{plan}/run` (start), `POST /{plan}/run/end`, `GET /{plan}/run` |
| Job orders | `POST/GET /job-orders`, `GET /job-orders/{job_order}`, `POST /{job_order}/revisions`, `GET /workstations/{workstation}/current-job-order` |
| Station profiles | `/workstations/{workstation}/profile`, `/profile-revisions`, `/profile-previews` |
| Station records | `/workstations/{workstation}/records`, `/records/{record}`, `/authorizations` |

Every service follows the shared pattern: verified browser session, site lock, active membership,
`Idempotency-Key` on mutations, `If-Match` on revisions, receipts and audit in one transaction.
They are published in OpenAPI and checked for routing by `TestPublishedOpenAPIRoutesAreServed`.

## Authority

- Creating and revising a shift plan needs owner or supervisor; **approving** it needs supervisor
  (owner is not a bypass), as in `shiftplans.go`. Approval supersedes the previously approved
  revision for the line.
- A handover adoption submits an *unapproved* draft plan revision; it never approves.
- A summary computed from an older plan revision is flagged as such (WIS-278 follow-up).

## Shift runs (migration 080)

- One run per plan. No row = planned; `ended_at` null = started; set = ended. Start and end are
  owner or supervisor with a recent authentication (`shiftRun` mode); the starter may end it.
  Start needs the approved `plan_revision`, at most 30 minutes before the planned start
  (`SHIFT_START_TOO_EARLY`) and before the planned end (`SHIFT_WINDOW_ENDED`).
- Station records need a started run (`SHIFT_NOT_STARTED`), decided at receipt under the site
  lock. After the end only an entry with `event_at < ended_at` is taken (`SHIFT_RUN_ENDED`
  otherwise), and it appends a summary revision; the run's cited summary never changes.
- End writes the final summary (kernel input `EndedAt`, output `run_ended_at`) and a handover
  draft in one transaction and cites both. A re-approval while started is allowed; after the
  end, approval is refused with `SHIFT_RUN_ENDED`.
- Rollback is roll-forward: keep 080, revert the record gate in `records.go`.

## CP-SAT planner (rule 01 recorded exception)

Advisory only. Go validates every request before invoking `src/workstation_cpsat` and every
response before returning it. The solver runs as a short-lived child process with stdin/stdout only,
a cleared environment and a 6-second timeout (`cpsat.go`). It never approves, persists or
dispatches. A missing or failing solver must surface as `ErrSolverUnavailable` while manual
planning keeps working.

## Verification

`make supervisor-integration CASE=WIS-520` (`./internal/workstation ./internal/migrate
./internal/httpapi`); the web specs are `web/tests/shift.spec.ts`, `workstation.spec.ts` and
`walkthrough.spec.ts`.
