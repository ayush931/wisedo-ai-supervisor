# Rule 02 — Branch, PR and delivery conventions

Source: `AGENTS.md`, `CONTRIBUTING.md`, the "Delivery boundary" / "Grooming baseline" blocks that
close WIS-270, WIS-271, WIS-272, WIS-273, WIS-274, WIS-275, WIS-276, WIS-277, WIS-278, WIS-279,
WIS-280, WIS-281 and WIS-290.

## Required

- New features use `feature/WIS-<number>-<slug>`; corrective fixes use `fix/WIS-<number>-<slug>`.
  The number is the owning Linear issue.
- One owned worktree per change. Preserve unrelated work and open PRs.
- Link the Linear issue in the PR and describe concrete behavior, validation and rollback.
- Before implementation the issue needs a named implementer/reviewer, selected base SHA,
  contract/fixture, verification commands and a rollback plan.
- The implementer and the independent reviewer are different people. Across the Supervisor
  backlog the owner is Ayush Kumar and the independent reviewer is Anshul Rathore.
- Record PR, SHA, the runnable command, the actual result and the rollback consequence on the
  owning issue.

## Forbidden

- Changing an existing component's contract without the issue explicitly approving a versioned migration.
- Marking an issue Done from a partial increment, vendor documentation or architecture prose.
