# Test-suite audit, October 2026

This document consolidates the five per-area audit reports produced on 2026-10-20. Rule 10
(`.claude/rules/10-testing-standards.md`) cites it as its source. It records what changed in the test
suites, why, and what remains open for review.

## 1. Scope and method

- **Date:** 2026-10-20.
- **Material reviewed:** about 26k lines of Go tests (unit and `integration`-tagged), the Playwright spec
  `web/tests/foundation.spec.ts`, and two C++ suites (`src/workstation_cpsat/tests/test_solver.cpp`,
  `src/wisedo_fixture_vision_cpp/tests/unit/test_core.cpp`).
- **Method:** five reviewers worked in parallel, one per area (knowledge, httpapi, oee, domain,
  platform). For each test file they read the production code it exercises, then classified each test or
  subtest as retain, consolidate, strengthen, remove or flag.
- **Removal criterion:** a test was removed only when the reviewer was certain that no regression could
  make it fail: an existence-only assertion, a tautology, a re-implementation of the code under test, or an
  exact duplicate. For every removal the report names the surviving test that covers the behavior.
- **Uncertain cases:** kept unchanged and flagged (section 6).
- **Production code:** none changed. A test that exposed a real defect would have been reported, not fixed.
  No production defect was found in any area.

| Area | Packages | Size before |
|---|---|---|
| knowledge | `internal/knowledge` | 22 files, 6,964 lines |
| httpapi | `internal/httpapi` (excluding `shift_plans*`) | 27 files (6 unit, 21 integration), about 6.5k lines |
| oee | `internal/oee`, `internal/oeeservice` | 19 files, about 6,035 lines |
| domain | `internal/{migrate,evidence,identity,production,approvals,plantcontext,ingestion,membership}` (excluding `032_*`) | 25 files, about 4.77k lines |
| platform | `internal/{bootstrap,store,operational,cases,onboarding,dispatch,contracts,plans,config,strictjson,canonical,session,limits}`, `cmd/**`; static review of the Playwright spec, both C++ suites and `scripts/supervisor/untested-packages.sh` | not stated |

## 2. Summary

Units differ between reports. They are reproduced as each report states them.

| Area | Removed | Consolidated | Strengthened | Retained | Flagged |
|---|---|---|---|---|---|
| knowledge | 0 | 1 test | 30 assertion sites in 13 files | all other tests (7 unit funcs, 15 integration scenario funcs) | 2 |
| httpapi | 0 | 0 | 12 files (68 integration assertions, 8 unit table rows) | 15 files unchanged | 3 |
| oee | 4 tests | 3 tests, plus 1 duplicate helper | 7 tests | all others (102 top-level passing tests in `internal/oee`) | 2 |
| domain | 0 tests (3 redundant assertion blocks, 2 duplicate helpers) | 7 | 26 | the rest | 4 |
| platform | 0 tests (1 tautological assertion) | 3 | 9 | 32 | 4 |

## 3. Removed tests

| Test | File | Reason | Surviving coverage |
|---|---|---|---|
| `TestProjectLocalShiftToUTC_CrossMidnightAndDST` | `internal/oee/timeline_test.go` | Its IST case duplicates a golden row. Its DST half only asserted `!IsZero()` on inputs the golden table checks exactly (04:00Z to 13:00Z, 9h). | `TestProjectLocalShiftToUTC_Golden` ("cross-midnight IST", "fall back day lengthens shift") |
| `TestDeduplicateOwnerFacts` | `internal/oee/timeline_test.go` | Redelivery dedup and conflicting duplicates were already covered. Its conflict case checked only `err == nil`. | `TestDeduplicateOwnerFacts_RevisionsSourcesAndRedelivery` (asserts `errors.Is(ErrConflictingDuplicate)`) |
| `TestReplayDeterminism_SameInputsProduceIdenticalHierarchyTotalsAndDigest` | `internal/oee/loss_test.go` | The result digest hashes the loss tree, both Pareto digests and the contributions, so digest equality already covers every sub-digest it compared. | `TestExplainLossesIsPureAndDigestIgnoresRecordTime`, `TestExplainLossesIsInvariantToFactOrder` |
| `TestPropertyTests_ConservationAndComponentRelationships` | `internal/oee/kernel_test.go` | It checked one scenario. The property test checks the same three identities (PPT = Run + Stop, OEE = A×P×Q, TEEP = OEE×Loading) across 200 seeds. | `TestKPIProperties`, which now also enforces a minimum number of exercised seeds (section 5) |

Assertion blocks and helpers removed without removing a test:

- `internal/operational/registry_test.go` `TestRegistryContractsAndMappingDigest`: `if a.EntityRevision != 3`
  could not fail (`a` is a local value). It was replaced by real checks (section 5).
- `internal/migrate` 020 `TestOperationalSourceMigrationCatalog`: the separate runtime-UPDATE check. Covered
  by `assertGrants`. The same edit fixed a scan bug in the test (`&coreSHA256` was scanned twice, so the
  availability result was overwritten).
- `internal/migrate` 022 `TestOperationalProgressMigrationCatalog`: the revisions-UPDATE check. Covered by
  `assertGrants`.
- `internal/migrate` 021 to 024 and 027: a redundant `Apply` before `Check`, because `newOperationalMigrationDB`
  already applies. Re-apply idempotency remains tested in 020, `TestMigrationReadbackAndRepeat` and
  `TestLegacyChecksumAccepted`.
- `internal/migrate` 027 helpers `casesRuntimeDB` and `caseSQLState`: byte-identical copies of `runtimeDB`
  and `sqlState` (028).
- `internal/oee/calculation_fixes_test.go` helper `hasReason`: duplicate of `kpiHasReason`. The test that
  used it, `TestScrapAboveTotalIsFlaggedNotClamped`, is kept for its unique `GoodPieces == -50` raw-value check.

## 4. Consolidated

| Test | File | Change |
|---|---|---|
| `TestAssetMembershipCandidate` into `TestRelationDomainMatrix` | `internal/knowledge/contracts_test.go` | Decode, asset/asset valid and asset/area invalid checks moved into the matrix. The matrix now also round-trips every valid predicate through the strict decoder and adds `part_of_asset asset->area` as invalid. |
| `TestGovernedReclassification_UnauthorizedAndAmbiguousFailClosed` | `internal/oee/loss_test.go` | Seven copy-pasted request literals became one table with the same 7 cases (also strengthened, section 5). |
| `TestCalculateDatasetRejectsMalformedFacts` | `internal/oee/service_contracts_test.go` | Its 7 element cases duplicated `TestVerifyDatasetRejectsMalformedElements`, and both functions share `verifyAndPrepare`. One representative case remains to prove `CalculateDataset` still verifies, a tampered-provenance case was added, and the `ErrInvalidPolicy` case is kept. |
| `TestDigestIgnoresCreationTimeButNotInputs` | `internal/plantcontext/snapshot_test.go` | A duplicate double `Assemble` became one call (also strengthened). |
| 6 per-table RLS/grant loops in 020 to 024, 027 catalog tests | `internal/migrate` | Replaced by one `assertGrants(appendOnly)` helper (also strengthened). |
| `TestOEELeaseDefaultFollowsShortDeadline` | `internal/config/config_test.go` | Calls `baseEnv(t)` instead of duplicating its setup. |
| `TestTerminalAndActive` | `internal/cases/state_test.go` | `if Terminal(Escalated)` repeated what the loop asserts for every state; the intent is now a comment. |
| `TestCaptureAcceptsOffsetTimestampsNormalized` | `internal/plans/plans_test.go` | Removed a no-op `strings.Replace(x, "", "", 1)` wrapper. |

## 5. Strengthened

### 5.1 Any error became an exact sentinel error

A check that accepted any error also passed when the input was rejected for an unrelated reason.

- `internal/knowledge`: `contracts_test.go`, `freshness_qualification_test.go`, `discovery_contracts_test.go`,
  `requirement_contracts_test.go` now assert `ErrInvalidContract`. `coverage_integration_test.go` asserts
  `ErrDenied` for an expired session. `discovery_integration_test.go` asserts `ErrInvalidContract` for empty input.
- `internal/oee`: `timeline_test.go` (`ErrCounterDropNoEvidence`, not asserted anywhere else),
  `loss_hardening_test.go` (`ErrUnauthorizedOverride`), `loss_test.go` (denied audit status `AMBIGUOUS` for
  cases 4 to 6), `loss_review_test.go` (the padding refusal itself; production returns an unwrapped
  `fmt.Errorf`, so the test matches the message).
- `internal/production/domain_test.go`: `ErrInvalidInterval`, `ErrPlanDoesNotCoverWindow`, `ErrInvalidDelta`,
  `ErrInvalidPlan`. Missing deltas must return `ErrInvalidDelta` and nil, not an invented zero (rule 06).
- `internal/approvals/approvals_test.go`: `ErrInvalidPayload`, `ErrInvalidWindow`.
- `internal/ingestion/imports_test.go`: `ErrInvalid` and nil events.
- `internal/migrate`: `migrate_integration_test.go` and `migration_role_integration_test.go` assert the
  checksum/sequence mismatch and "schema upgrade required" errors (by message; see flag D4).
- `internal/strictjson/decode_test.go`, `internal/plans/plans_test.go`: `strictjson.ErrInvalid`.
- `internal/canonical/canonical_test.go`: `ErrNotCanonical` (also fixed a `[4]string` with 3 elements).
- `cmd/supervisor/bootstrap_test.go`: `fs.ErrExist`, so the refusal must come from `O_EXCL` before any
  database work.

### 5.2 Any error became an exact SQLSTATE

A runtime-denial check that accepted any error also passed on 42703 (column typo) or 42601 (syntax
error) in the fixture SQL. The asserted codes come from the migrations: runtime grants are SELECT and INSERT
only, triggers raise 42501 for scope and 23514 for cardinality.

- `internal/knowledge`: `store_integration_test.go` (23514, new shared `requireSQLState` helper),
  `discovery_integration_test.go` and `requirement_decisions_integration_test.go` (23514 in scope, 42501
  when the scope is switched; 42501 for history UPDATE/DELETE), `freshness_integration_test.go`,
  `reviews_integration_test.go`, `review_revocations_integration_test.go`, `resolutions_integration_test.go`,
  `snapshots_integration_test.go`, `inventory_integration_test.go` (42501).
- `internal/membership/membership_integration_test.go`: direct grant and audit delete now run inside the
  tenant context and must fail with 42501. Before, any error passed, including RLS with no context.
- `internal/evidence`: `evidence_integration_test.go`, `authority_integration_test.go`,
  `currentness_integration_test.go` (42501; shared `sqlState`/`runtimeCount` helpers).
- `internal/migrate`: `migrate_integration_test.go` `TestMissingSchemaIsNotReady` (42P01);
  `027_exception_cases_behavior` (42501 for history and evidence DELETE); `assertGrants` also asserts no
  table-level UPDATE or DELETE and exactly one `tenant_scope` policy where applicable.
- `internal/bootstrap/bootstrap_integration_test.go`: P2901 (raw redeem without session context), 42501
  (runtime membership insert, runtime invite minting), 23514 (injected audit failure).
- `internal/store/store_integration_test.go`: 42501 from the guard triggers, 23514 for the injected audit failure.

### 5.3 HTTP status became status plus problem code

428 can mean `REVISION_REQUIRED` or `REAUTHENTICATION_REQUIRED`; 409 can mean `IDEMPOTENCY_CONFLICT`,
`SOURCE_CHANGED`, `*_BLOCKED`, `*_CONFLICT`, `SOURCE_SEQUENCE_CONFLICT` or `EVENT_REVISION_CONFLICT`.
Status alone did not show which guard fired. The new helper `wantProblem(t, w, status, code)` lives in
`internal/httpapi/problem_integration_test.go`. Of the expected codes first set by convention, 30 failed;
each actual code was checked against the scenario and found semantically correct, then pinned.

Files: `knowledge_test.go` (unit table rows), `authority_integration_test.go`, `currentness_integration_test.go`,
`discovery_integration_test.go`, `evidence_integration_test.go`, `knowledge_integration_test.go`,
`membership_integration_test.go`, `operational_integration_test.go`, `resolutions_integration_test.go`,
`review_revocations_integration_test.go`, `reviews_integration_test.go`, `snapshots_integration_test.go`.

### 5.4 Tautologies and vacuous passes replaced

- `internal/operational/registry_test.go`: the tautological `EntityRevision` check became: the caller's
  slice is not reordered, a changed entity revision changes the digest, and duplicate target revisions are
  rejected (previously untested).
- `internal/production/domain_test.go` `TestCanonicalPlanPayloadDigestIsExactAndScoped` and
  `internal/approvals/approvals_test.go` `TestEveryPayloadFieldIsBound`: binding checks ignored `err`, so an
  erroring call returned `""`, which differed from the digest and passed.
- `internal/oee/kernel_test.go` `TestAggregation_MathematicalRollupNotAveraged`, `loss_test.go` setup,
  `loss_hardening_test.go`: previously ignored errors are now checked.
- `internal/oee/kernel_review_test.go` `TestKPIProperties`: fails if fewer than 50 of 200 seeds publish every
  factor, so the identity check cannot silently stop running.
- `internal/config/config_test.go` `TestConfigFailsClosed`: the fixture DSN also lacked `sslmode=verify-*`,
  so "insecure issuer" could pass with the issuer check missing. The DSN is now valid, a positive production
  control was added, and a "production Temporal without TLS" case was added.
- `internal/ingestion/batch_test.go`: a large number must survive byte-exact, not merely produce valid JSON (rule 04).

### 5.5 Positive controls for zero-row RLS checks

A cross-tenant count of zero also results when the query matches nothing at all. A positive control now
shows the same query returns rows inside the owning tenant: `internal/evidence/evidence_integration_test.go`,
`authority_integration_test.go`, `currentness_integration_test.go`, and
`internal/migrate` `027_exception_cases_behavior` `TestExceptionCaseStorageEnforcesTheLifecycle`.

### 5.6 `t.Skip` became BLOCKED

- `internal/identity/identity_integration_test.go` `TestPersistentOIDCFailurePaths`: `t.Skip` on a missing
  database became `t.Fatal("BLOCKED: ...")` (rule 09). A dead `postgresql+asyncpg` rewrite was removed.
- BLOCKED prefix added to the missing-dependency failures in `internal/evidence` (evidence, authority) and
  `internal/migrate/migrate_integration_test.go`.

### 5.7 New cases added inside existing tests

- `internal/production/domain_test.go`: a delta from another order or asset is rejected (the
  `d.OrderID != p.OrderID` path was untested).
- `internal/plantcontext/snapshot_test.go`: tenant binding of the digest (only site was checked; rule 04).
- `internal/identity/identity_test.go` `TestCSRFRequiresOriginAndSecret`: right origin with wrong token,
  right origin with missing token, empty session secret versus empty header.
- `internal/oee/readiness_test.go` `TestReadinessPrecedence`: now a table with LIMITED, LIMITED + MissingData
  → NOT_READY, and INVALID cases; asserts exact ordered codes and a non-nil `[]` for READY.

## 6. Flagged for review

All flagged tests are kept unchanged.

| # | Location | Concern |
|---|---|---|
| K1 | `internal/knowledge` integration files | Each repeats about 40 lines of setup. A shared fixture would remove several hundred lines; not done because it is a large cross-file change and fixtures differ per file. |
| K2 | `internal/knowledge` "LOCK knowledge_audit + 150ms timeout" blocks | Check only `err != nil`, because the pgx timeout error is not stable. The following row-count and retry assertions carry the guarantee. |
| H1 | `internal/httpapi/plans_integration_test.go` direct-SQL invariant block | `e == nil` → fail proves a mutation is blocked but not which trigger blocked it. Trigger texts were not pinned to avoid guessing. |
| H2 | `internal/httpapi/oee_service_integration_test.go` `conforms` | The schema checker accepts any unknown or missing `type` (for example `number`) without checking it, so that kind of schema drift is not caught. |
| H3 | `internal/httpapi/oee_test.go` `TestOEEEndpointsFailClosed` | Two cases (GET calculation 503, invalid UUID 422) check status only. Add `CALCULATION_UNAVAILABLE` and `INVALID_REQUEST`. The file was not edited because it carried concurrent WIS-520 changes. |
| O1 | `internal/oeeservice/perf_integration_test.go` `TestOEEPerformanceEnvelope` | Behind the `oeeperf` tag, logs only, no assertions. It measures the host; it is not coverage and not a gate. |
| O2 | `internal/oee/cost_test.go` `TestCostAllocationReconcilesExactly` | The total-rounding oracle uses production `roundHalfEvenScaled`, which is partly circular. Anchored by the golden `2050.00` in `TestCostGoldenAndAssumptionsVisible`; the hierarchy reconciliation checks are independent. |
| D1 | `internal/migrate/migrate_integration_test.go` `TestMigrationReadbackAndRepeat` | Hard-coded forced-RLS count (`99`) runs against the shared DB and changes with every migration. `assertTenantTablesForced` is the stronger catalog-based check. Left because WIS-520 edits this line. |
| D2 | `internal/migrate` 020 `assertRejected`, mapping-digest commit tests; `internal/evidence` forged-hash and span-bound inserts | Still accept any error. The SQLSTATE varies (FK, CHECK, trigger), so pinning needs per-case codes. |
| D3 | `internal/membership` `TestAuditFailureRollsBackMembership`; `internal/ingestion` "cancelled job reopened" | Accept any error. The row-count follow-up gives the real assurance. |
| D4 | `internal/migrate` `wantMismatch` and "schema upgrade required" checks | Compare error text from `migrate.go`. A wording change breaks them. Robust matching needs sentinel errors in `migrate.go`, which is a production change. |
| P1 | `internal/limits/limits_test.go` `TestLimitsMatchTheSpecification` | A change detector on constants. Removing it would make `untested-packages.sh` fail for the package; the ordering invariant it checks is real. |
| P2 | `web/tests/foundation.spec.ts` | The foreign-site 404 uses a random site id, so it proves non-disclosure only for a non-member site, not for a real other-tenant site. |
| P3 | `src/workstation_cpsat/tests/test_solver.cpp` | Rejections use `catch (...)`, so any exception (an unrelated `bad_alloc`, a JSON `at()` miss) counts as "rejected". Catching the specific validation exception type would be stricter. |
| P4 | `src/wisedo_fixture_vision_cpp/tests/unit/test_core.cpp` | Writes to fixed `/tmp/wisedo_test_*.{csv,json}` paths, so concurrent runs collide. Exporter checks look only for key names, not values or row counts. Board-bbox check is `width>0` only. |

Minor notes from the platform report, not counted as flags: `internal/onboarding/intake_test.go` `makeZip`
ignores the `w.Write` error; `scripts/supervisor/untested-packages.sh` checks that each Go package has a
`_test.go` file (integration-tagged files count) but not that tests run or pass, so `internal/dispatch` and
`internal/store` show "[no test files]" in the plain unit run.

## 7. Reviewed statically, not executed here

- `internal/dispatch/dispatch_integration_test.go` `TestRealTemporalLostAckRestartRetriesAndReplay`: needs
  `SUPERVISOR_TEST_TEMPORAL_ADDRESS`. Without it the test fails as BLOCKED rather than skipping. Static
  review: covers lost-ack relay dedupe, more than 5 retries and history replay.
- `web/tests/foundation.spec.ts`: needs a seeded stack. Static review: throws BLOCKED without a credential;
  checks idempotent receipt replay, CSRF 403, non-disclosing foreign 404, empty localStorage, re-auth and
  logout 401. See flag P2.
- C++ suites. `test_solver.cpp` sets `-UNDEBUG`, so its asserts run in the Release Docker build.
  `test_core.cpp` sets `-UNDEBUG` on a target built only under `BUILD_TESTING`; CI enables it in
  `.github/workflows/future-vision-cpp.yml` (`-DBUILD_TESTING=ON`, ctest), while the product Dockerfile builds
  with `BUILD_TESTING=OFF`. See flags P3 and P4.

## 8. Verification as reported

Each area ran, with Go 1.27 and the disposable test database URLs: `gofmt -l`, `go vet` with and without
`-tags=integration`, `go test -race -count=1`, and `go test -race -count=1 -tags=integration` over its packages.

| Area | Result |
|---|---|
| knowledge | gofmt empty; vet ok; unit ok (1.478s); integration ok (90.797s). Baseline before edits: unit ok, integration ok (89.175s). |
| httpapi | gofmt lists only `shift_plans_integration_test.go` (WIS-520, out of scope); vet ok; unit ok (1.360s); integration ok (44.829s; baseline 32.941s). The first run with conventional codes failed on 30 lines, which shows the new assertions bite. |
| oee | gofmt empty; vet ok (including `oeeperf`); unit ok; `internal/oee` integration ok. `internal/oeeservice` integration **BLOCKED**: `migrate.Apply` returned "applied migration checksum or sequence mismatch", twice. No `internal/oeeservice` file was changed. |
| domain | gofmt empty; vet ok; unit ok; integration ok on the shared DB in the final run, and ok on a fresh disposable DB (`audit_domain_verify`, since dropped). |
| platform | gofmt empty; vet ok; unit ok except `internal/contracts` `TestCommittedContractsMatchGenerator` (see below); integration ok for `bootstrap`, `store`, `operational` in an earlier run. In the final run `bootstrap` failed at line 48 (`migrate.Apply`, checksum or sequence mismatch), also on one rerun; line 48 precedes every edited line. `dispatch` BLOCKED (no Temporal). `untested-packages.sh` ok. |

**Shared-database interruption.** During the audit the concurrent WIS-520 work edited
`internal/migrate/032_shift_plans.sql` after it had been applied to the shared disposable database. The
applied version-32 checksum (`8570d8…`) no longer matched the file (`abc651…`), so any test calling
`migrate.Apply` on that database failed with "checksum or sequence mismatch". This explains the oeeservice
BLOCKED result and the platform `bootstrap` failure. It is environmental, not a regression from the audit.
The final gate runs are to be done on a fresh database.

**Contract drift.** `internal/contracts` `TestCommittedContractsMatchGenerator` fails on `openapi.json`,
`schemas/*ShiftPlan*V1` and `typescript/oee.ts`. The drift comes from the WIS-520 shift-plan contract changes
and failed identically before the audit edits. `make supervisor-contracts` must be run with that work.

**Fresh-database run (2026-10-20).** After the per-area runs, a new disposable PostgreSQL 17 was created and
migrated, and every integration-tagged package except `internal/dispatch` was run in one invocation:
`go test -race -count=1 -p 1 -tags=integration $(go list -tags=integration ./internal/... ./cmd/... | grep -v /internal/dispatch)`.
Result: 685 tests passed, 0 skipped, 1 failed. The single failure is the contract drift above.
`internal/oeeservice` and `internal/bootstrap` pass on the fresh database, which confirms that their earlier
failures came from the shared-database checksum.

## 9. Excluded from this audit

`internal/workstation/**` and the WIS-520 shift-plan files (`internal/httpapi/api.go`,
`internal/httpapi/shift_plans*.go`, `internal/migrate/032_*`, `internal/migrate/migrate.go`) were being edited
concurrently and were not audited. They will be audited separately.
