# Repository instructions

- This repository is `Wisedo-Technology/wisedo-ai-supervisor`, an independent product.
- Before any task, read `CLAUDE.md`. It gives the reading order (README, the architecture doc,
  the `.claude/rules/` files, the ADRs for your area and the Linear issue) and lists verified
  documentation problems. Do not rely on docs without checking them against the code and Linear.
- Read `README.md` and `docs/independent-supervisor.md` before changing product scope.
- Features use `feature/WIS-<number>-<slug>`; fixes use `fix/WIS-<number>-<slug>`.
- Preserve unrelated work and open PRs. Work in the main checkout: do not create git worktrees or
  sibling folders.
- No runtime, database, identity, SDK or deployment dependency on existing WiseDo MES.
- The legacy `wisedo_plantops_oee` Python scaffolding has been decommissioned in favor of the independent Go AI Supervisor runtime.
- Do not equate a repository rename, scaffolding or fixture tests with a delivered
  Supervisor. Verify the gates required by the owning Linear issue.
