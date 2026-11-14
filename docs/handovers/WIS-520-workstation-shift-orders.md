# Handover: WIS-520 workstation job orders, shift plans and handovers

Checked: 2026-10-20

Repository: `Wisedo-Technology/wisedo-ai-supervisor`
Branch: `feature/WIS-520-workstation-shift-orders`
Pull request: [#108](https://github.com/Wisedo-Technology/wisedo-ai-supervisor/pull/108). Open; not merged or deployed. Independent review is still needed (rule 02).

## Scope delivered on the branch

These are the backend contracts and services from the WIS-520 acceptance criteria. Everything is manual-first and works with no MES, IoT or model connectors. The tablet and supervisor UI belong to WIS-278 and are not part of this branch.

| AC | Delivered | Commit | Main evidence |
|---|---|---|---|
| 1 Shift plans | Create, revise, approve and supersede. The approver must not have submitted any revision. At most one approved plan per line and shift. | `a0eb953` | `TestShiftPlanLifecycleHTTPPostgres`, `TestShiftPlansMigrationCatalog` |
| 2 Job orders to workstations | Revisioned job orders become live through an approved plan pin. Readback returns `current`, `needs_replan` or `instruction_unavailable`, and never returns a superseded body. | `4fec4b3` | `TestJobOrderDeliveryHTTPPostgres`, `TestJobOrderGuardsPostgres` |
| 3 Station screens | Versioned station profiles use a closed set of templates and fields. Preview is side-effect free. Unsupported or executable-looking configuration is refused with its path. | `e485d3b` | `TestStationProfilesHTTPPostgres`, `TestValidateEntry`, `TestResolveFormPrecedence` |
| 4 Operator records | Station authorizations. Append-only records carry actor, `event_at`, `received_at`, station, plan revision, job revision and provenance. Corrections append a new record. Late entries bind to the plan in force at `event_at`. A quality record can never authorize release. | `1591f64`, `44d5d19` | `TestStationRecordsHTTPPostgres`, `...LateBinding...`, `...CarryForward...`, `...CorrectionAuthority...`, `...InstructionAsOf...` |
| 5 Shift summary | Pure kernel with exact arithmetic. Planned and recorded figures are kept separate, and every figure cites its records. Readiness reason codes are attached. Snapshots are immutable revisions. | `44d5d19` | `TestSummarizeShift*`, `TestShiftSummaryHTTPPostgres` |
| 6 Handover draft | Deterministic draft with a proposed next-shift plan. Generating a draft never writes a plan. Adoption creates an unapproved draft, and only the latest draft can be adopted. | `9bb27fa` | `TestDraftHandover*`, `TestShiftHandoverHTTPPostgres`, `TestShiftHandoverSupersededHTTPPostgres` |
| 7 Manual-only | The whole flow runs with no operational sources and no model calls. | `142c774` | `TestWIS520ScenarioMatrixHTTPPostgres` |
| 9 Scenario matrix | One continuous end-to-end flow. | `142c774` | `TestWIS520ScenarioMatrixHTTPPostgres` |
| 8 MES/IoT enrichment | **Not delivered.** It needs separately qualified read-only adapters (WIS-518, WIS-519). | — | — |

Migrations 032–038 are additive: 032 shift plans, 033 job orders, 034 station profiles, 035 station records, 036 shift summaries, 037 correction audit, 038 handovers. Every new table has FORCE RLS and a `tenant_scope` policy. The runtime role has SELECT and INSERT plus narrow column UPDATEs on head pointers. History tables are immutable.

The earlier advisory CP-SAT planner (`6da8fe6`: `internal/workstation/{planning,cpsat}.go`, `src/workstation_cpsat/`, the root `Dockerfile` OR-Tools stage) is still on the branch. It has no route or caller. The issue excludes optimization, and rule 01 reserves C++ for Future Vision, so it needs a human decision. Either keep it with a recorded narrow C++ exception, or move it to its own branch.

## Verification evidence (2026-10-20)

- `make supervisor-check`: passed on every slice commit (go vet, race unit suite, web build, npm audit with 0 vulnerabilities).
- `make supervisor-contracts`: no drift on every slice commit.
- `make supervisor-integration CASE=WIS-520`: 200 passed, 0 skipped at `142c774`.
- Every integration package except `internal/dispatch` (needs Temporal), on a fresh database owned by the migrator role (123 tables owned by `supervisor_migrator`): 778 passed, 0 skipped, 0 failed at the slice-6 tree.
- `TestWIS520ScenarioMatrixHTTPPostgres`: 3/3 passed with `-race`.
- GitHub Actions on PR #108: all six workflows green on `a0eb953`, `4fec4b3`, `e485d3b`, `1591f64` and `44d5d19`. On `9bb27fa`, Supervisor Go Foundation, SAST and Future Vision passed; the other three were cancelled when the next push superseded them. Re-check the PR head before merging. The `foundation` job runs the full integration gate with Temporal.

Each slice had an independent code review, and its findings were fixed before commit. The automated security review flagged two issues after `1591f64`: cross-actor corrections and a fail-open instruction check on late entries. Both are fixed in `44d5d19`.

## Known gaps and decisions

- `STALE_INPUT` (30-minute window after the shift, policy revision 1) is covered only by the unit test `TestSummarizeShiftReadiness`, because real time cannot reach it.
- Plan revisions created before migration 033 have no assignment rows. They cannot be approved or delivered until they are re-pinned. No production data exists.
- `event_at` is supplied by the client and bounded to at most 5 minutes in the future and at most 7 days old. A late entry is checked against the instruction as of `event_at` only when the job-order revision changed after `event_at`.
- Station profiles have no approval step. Owners and supervisors write them directly.
- Every mid-shift summary is a new revision because `as_of` is hashed. After the shift ends, the digest is fixed.
- Staleness counts lifecycle events (start, pause, resume) as fresh station data.
- Station writes are serialized by the site-wide write lock.
- Records: the lifecycle state is not returned. The tablet derives it from the record list.

## Resume checklist

1. Get an independent review of PR #108. The implementer and reviewer must be different people.
2. Decide what happens to the CP-SAT planner (keep it with a recorded rule-01 exception, or split it out).
3. Record PR, SHAs, commands and results on Linear WIS-520. Keep it In Progress until the review passes.
4. Before WIS-278 builds the tablet UI against these contracts, confirm the known gaps above are acceptable.
5. Production deployment and customer acceptance have not been done.

## Environment notes

- The test database is a disposable `postgres:17` container (`wis520-pg`, 127.0.0.1:55520) that `deploy/supervisor/bootstrap.sh` initializes and the migrator role migrates. `docker compose` could not create a network on this host because the address pool overlaps.
- `contracts/`, `web/node_modules` and `web/dist` had been left root-owned by a Docker run. They were renamed aside to `.contracts.root-owned`, `web/.node_modules.root-owned` and `web/.dist.root-owned` (excluded locally) and regenerated. Delete the renamed copies with sudo.
- Related branch: `fix/WIS-290-test-suite-hygiene` (`fd9ef44`) holds the test-suite audit and rule 10. It has no PR yet. It also lacks the CI fixes `4b40423` and `06cb31e`, which exist only on this branch.
