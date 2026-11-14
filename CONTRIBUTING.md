# Contributing

New feature branches must use `feature/WIS-<number>-<slug>`.
Corrective fixes must use `fix/WIS-<number>-<slug>`.
Existing branches are preserved; use these prefixes for new work.

Link the Linear issue in the PR and describe the concrete behavior, validation and
rollback. Work in the main checkout without creating git worktrees or sibling folders, and
preserve unrelated work. Changes
to an existing component must retain its contract compatibility unless the issue
explicitly approves a versioned migration.

Before implementation, the issue needs a named implementer/reviewer, selected base
SHA, contract/fixture, verification commands and rollback plan. Start with WIS-146
and WIS-290 for the independent application. Do not import MES private SDKs, query
MES databases or require another WiseDo product to boot this application.

Run `make supervisor-check` and `make supervisor-contracts` for every change, and
`make supervisor-integration CASE=<selector>` with a real PostgreSQL for database work
(see `.claude/rules/09-verification-gates-and-evidence-claims.md`). A skipped or missing
required test fails its gate. The retained Python OEE scaffolding is decommissioned;
`make check` covers only what remains of it. Additional database, browser,
actual-model and clean-install gates are added by their owning implementation tickets.
Golden fixtures are contract checks, not actual-agent or customer evidence.
Tests must protect observable behavior, failure cases and regressions, never the mere
existence of code, keys, files or values; see `.claude/rules/10-testing-standards.md`. Keep
implemented, tested, integrated, deployed and customer accepted claims separate.

## Branch protection and review (required for `main`)

`main` must be protected with these GitHub settings; a repository admin applies them:

- Changes reach `main` only through a pull request; direct pushes and force pushes are
  blocked, and the branch cannot be deleted.
- At least one approving review, from a code owner (`.github/CODEOWNERS`), who is not the
  author. Across the Supervisor backlog the implementer is Ayush Kumar and the
  independent reviewer is Anshul Rathore. New commits dismiss stale approvals.
- Required status checks, run on the exact head commit and up to date with `main`: the
  `foundation` job of `.github/workflows/supervisor.yml` (`make supervisor-check`,
  `make supervisor-build`, `make supervisor-contracts`), Code Quality & Static Analysis,
  SAST & Dependency Vulnerability Audit, and the Trivy dependency/secret/misconfiguration
  and image scans.
- Conversations must be resolved before merge.

The OEE wire contracts are generated into `contracts/` and drift fails
`make supervisor-contracts`. Provider and consumer fixtures for them live in
`internal/oee/testdata/contract-fixtures/` and run in the unit and HTTP tests.
