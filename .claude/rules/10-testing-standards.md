# Rule 10 — Tests protect behavior, not existence

Source: the 2026-10 test-suite audit (`docs/testing-audit-2026-10.md`), rules 03, 04, 06, 07 and 09.

A test earns its place by failing when a real regression happens. If no plausible change to
production behavior can make it fail, it is not a test.

## Required

- Assert observable behavior: returned values, persisted rows, HTTP status plus problem `code`,
  emitted events, digests, state transitions. Drive the public entry point (function, route,
  SQL as the runtime role) the way a caller does.
- Cover the failure path for every success path that matters: invalid input, wrong tenant,
  missing authority, stale revision, duplicate submission, zero denominator, unavailable source.
  Assert the specific outcome (`412 REVISION_CONFLICT`, `NOT_READY` with its reason code, the
  rejected row count), not merely `err != nil`.
- Security and authority boundaries (rules 03, 05, 07, 08) get a negative test that would pass
  if the boundary were removed — cross-tenant 404, approver equal to submitter, unknown JSON
  field, runtime role without `BYPASSRLS`.
- A bug fix lands with a regression test that fails on the pre-fix code. Name the defect or
  issue in the test name or a one-line comment.
- Prefer table-driven cases over near-duplicate test functions. One behavior, one place.
- Deterministic: no wall clock, randomness or network unless injected and pinned; integration
  tests create their own tenant/site scope and do not depend on execution order.
- Golden fixtures assert the full computed output against a reviewed expected value, and the
  fixture is regenerated only by a reviewed change (rule 09: fixtures are contract checks, not
  evidence of actual agent behavior).

## Forbidden

- Existence-only tests: asserting that a constant, struct field, config key, file, route
  registration or schema property exists, without exercising what it does.
- Tautologies: comparing a value to itself, re-implementing the code under test to compute the
  expectation, or asserting a mock returns what it was told to return.
- Asserting private implementation details (call order of internals, unexported field values,
  exact log text) when an observable outcome can be asserted instead.
- `t.Skip` in a required suite, or a test that passes when its dependency is absent (rule 09).
- Deleting or weakening a failing test to make a gate green. Fix the code, or record the
  defect and keep the test failing visibly.
- Removing a test whose value is uncertain. Keep it and record the concern for review.
