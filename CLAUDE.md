# CLAUDE.md — read this before working in this repository

This file is loaded at the start of every Claude Code session. Other coding agents read
`AGENTS.md`, which points here. Follow this order before changing code, docs or Linear.

## 1. Read these first (every session, every new task)

1. `README.md`: what the product is and how to run it.
2. `docs/independent-supervisor.md`: the product architecture, v1 scope, source contracts
   (`SourceRecordV1`, `CaseV1`, `PlantContextSnapshotV1`, `ApprovedActionV1`, `TaskV1`) and
   release gates. Read it before changing product scope.
3. `AGENTS.md` and `CONTRIBUTING.md`: branch names, PR and rollback rules.
4. `.claude/rules/00-*.md` through `11-*.md`: binding engineering rules. The key ones are
   tenant isolation (03), deterministic arithmetic and digests (04), human approval (05),
   fail-closed provenance (06), API conventions (07), verification gates (09), testing
   standards (10) and the onboarding agent (11).
5. The ADRs for the area you are changing (see the map below).
6. The Linear issue for the task. Its description and its comments are the acceptance
   criteria. Docs can lag behind Linear; when they disagree, say so and follow the issue.

## 2. Where things are

| Topic | Read |
|---|---|
| Product scope, v1 flow, architecture | `docs/independent-supervisor.md` |
| Current status of each area | Linear (source of truth); the status table in the doc was last checked 2026-11-02 and can drift (see §4) |
| OEE authority and MES coexistence | `docs/adr/0001-oee-authority-and-mes-integration.md` (status: Proposed) |
| OEE module layout | `docs/adr/0002-oee-intelligence-core-module-design.md` (design history in Python terms; see its banner) |
| OEE formulas and constraints | `docs/adr/0005-oee-calculation-industry-standard.md` (a byte-identical copy is under `docs/adr/video-embedding-docs/`) |
| OEE retained component setup | `docs/oee-foundation.md` |
| AI agent harness and LLM training | `docs/adr/agentic-ai-docs/0003-*.md`, `0004-*.md` (proposals, not built) |
| Video search | `docs/adr/video-embedding-docs/0006-*.md` (proposal) |
| Future Vision edge vision (C++) | `docs/adr/video-embedding-docs/0007-*.md`, `future-vision.md`, `fixture-inspection.md`, `match-video.md` |
| Onboarding agent, retrieval, value-stream knowledge | `docs/onboarding-agent/README.md`, rule 11, skill `supervisor-onboarding-agent` |
| Deployment and local run | `deploy/supervisor/README.md`, `deploy/README.md` |
| Security reporting | `SECURITY.md` |

Project skills in `.claude/skills/` summarize each supervisor area (for example
`supervisor-persistence-migrations`, `supervisor-production-context`, `supervisor-http-api-contracts`,
`supervisor-approvals`, `supervisor-oee-kernel`, `supervisor-workstation-shifts`). Use them as a map, then check the code and the
source document they cite.

## 3. Working rules that are easy to miss

- Work in the main checkout. Do not create git worktrees or sibling folders; this is an explicit
  user preference.
- Branch names: `feature/WIS-<n>-<slug>` for features, `fix/WIS-<n>-<slug>` for fixes.
- Do not commit, push, open PRs or change Linear without the user asking. Report results and ask.
- A feature is not Done because it compiles or because a golden fixture passes. Done needs the
  issue's acceptance criteria met and the gates run and recorded (rules 02 and 09).
- Required gates: `make supervisor-check`, `make supervisor-contracts`, and
  `make supervisor-integration CASE=<selector>` for database work. Integration tests need a real
  PostgreSQL through `SUPERVISOR_TEST_ADMIN_URL` and `SUPERVISOR_TEST_DATABASE_URL`. A skipped or
  missing required test is a failure.
- No MES, PlantOps, Sight or Flow dependency in the Supervisor (rule 00). Python components are
  allowed for methods (retrieval, parsing, OCR, rerank, eval) behind a Go-validated contract with
  no authority (rule 01, relaxed 2026-11-12).
- Migrations are additive and checksummed (`internal/migrate`). Rule 03 requires the runtime role to be
  non-owner, non-superuser and without `BYPASSRLS`; `store.VerifyRole` enforces this at boot and on
  `/readyz`, and integration tests assert it against real PostgreSQL.

## 4. Known documentation problems (verified; do not trust these as written)

- `docs/independent-supervisor.md` section 5 was re-checked against Linear on 2026-11-02 (WIS-632):
  WIS-146, WIS-270 to WIS-281 and WIS-290 are all Done. The table drifts; Linear stays the source
  of truth, so check the issue before relying on it.
- The Future Vision intake path is now written as the code route
  `POST /v1/sites/{site}/operational-sources/{source}/batches` everywhere (WIS-633). The
  C++ intake client itself is still a planned phase in ADR 0007.
- ADRs 0001, 0002, 0003, 0004, 0006 and 0007 describe Python modules, paths and stacks. Each now
  opens with a banner (WIS-633). The runtime is Go, plus the C++ edge core; the Python names are
  design history and are not in this repository. The decision text itself was not rewritten.
- ADR 0001's truth states: only CANDIDATE and SUPERSEDED exist (`truth_state`, migration 029);
  PROVISIONAL and ACCEPTED are not implemented.
- ADR 0005 zero denominators: fixed on main by PR #80. A zero denominator sets `ZERO_DENOMINATOR`
  (plus `MISSING_DATA` for availability) and publishes no value unless the policy explicitly selects
  `ZERO` (`internal/oee/kernel.go` `ratioMetric`).
