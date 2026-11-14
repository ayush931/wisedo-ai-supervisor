---
name: supervisor-web-workspaces
description: The React/TypeScript/Vite owner, supervisor and operator workspaces under web/ (WIS-278) - routes, API client behavior, conflict and error handling, polling, roles and accessibility. Use this skill for any frontend work in this repository, including the approval dialog, task views, discovery screens, settings, Playwright specs, or whenever the UI could imply an action succeeded without a durable server receipt.
---

# Role workspaces (web/)

Covers `web/` (`src/`, `tests/`, `playwright.config.ts`).
Source: Linear WIS-278. Status checked against `web/src` on 2026-11-03:
- **Built:** overview and today (`main.tsx`, `today.tsx`, which shows proposals and approvals);
  cases (`cases.tsx`); interviews and proposed facts (`interviews.tsx`, `proposed-facts.tsx`);
  configuration import and drafts; master data, profile builder, routing editor and source mapping;
  job orders; workstation, station and shift screens (WIS-520).
- **Not built:** tasks (WIS-630), feedback and settings.
- **In review:** document upload, the value-stream checklist and the process graph (WIS-618,
  PRs #219–#221).
Governing rules: `.claude/rules/07-api-contract-conventions.md`, `.claude/rules/05-human-authority-and-approvals.md`.

React + TypeScript + Vite, static assets served same-origin by the Go backend. The session is
backend-managed; provider bearer tokens are never persisted in localStorage. **The UI is a view of
server authority, never the permission enforcer.**

## Routes

| Route | Purpose |
| --- | --- |
| `/login`, `/sites` | sign-in, allowed sites, explicit denied/expired session behavior |
| `/sites/:siteId/overview` | exceptions, source freshness, confirmed versus unverified facts, mode and coverage, observed outcomes |
| `/sites/:siteId/discovery` | resumable interviews and import progress, map/list toggle, source snippets, contradictory claims, review queue |
| `/sites/:siteId/cases/:caseId` | ordered timeline of evidence and context revisions, observations versus hypotheses, clarifications, proposals, execution receipts |
| `/sites/:siteId/approvals` | immutable proposal payload, asset/action/assignee, cited evidence, policy and source revisions, expiry, approve/reject with reason |
| `/sites/:siteId/tasks`, `/tasks/:taskId` | task states, with completion evidence clearly distinct from recovery observation |
| `/sites/:siteId/feedback` | submit feedback, reviewer queue, evaluation manifest, activation and revoke history |
| `/sites/:siteId/settings` | owner-only sources, membership and capability policy, active mode, kill switch |

The site coverage denominator is the commissioned capability inventory; unknown stays visible.
Never label modeled or hypothesized impact as realized savings. Credential values are never
returned by a read API, so never render them.

## API client behavior

Use generated TypeScript types from the `/v1` OpenAPI contracts. Tenant comes only from the
authenticated server session. Every resource carries a site and an immutable ID — no lookup by
display name. Lists use stable cursor pagination, default 50, max 100.

Send `Idempotency-Key` and `If-Match` on governed mutations and keep the same key for transport
retries of an unchanged action. The server returns the durable receipt; a toast or a model sentence
cannot fabricate an action ID.

| Response | Behavior |
| --- | --- |
| 409 / 412 | reload the current revision and ask the user to review; never silently resubmit |
| 401 | sign out to reauthentication without replaying the mutation |
| 403 | show denied |
| 404 | the backend's non-disclosing not-found |
| 429 / 503 | expose retry status; no fake cached success |
| ambiguous network failure | query the operation or resource by stable key before allowing a new submission |

## Roles

Owner: memberships, sources, mode, policy, site-wide permitted views. Supervisor: claims, operations
and approvals for granted capabilities. Operator: authorized reports, own assigned task
acknowledgement/completion, feedback. Reviewer/evaluator is checked separately for knowledge
promotion. The UI may hide controls, but every API enforces the same policy. No self-approval where
requester and approver are the same human. All approval limits come from the server.

## Live updates and accessibility

v1 uses conditional bounded polling, not a new realtime service: active case and task every 10 s,
overview every 30 s, paused while hidden, refreshed on reconnect. Show last-refreshed and source
freshness separately. Offline mutations are disabled with a retained draft — never queue approvals
or task completions for silent replay. A graph visualization always has an equivalent searchable
list or table.

Keyboard navigation, visible focus, correctly labelled fields, error summaries, non-colour-only
states, readable contrast and adequate touch targets are acceptance requirements. Approve/reject
dialogs show consequences and the current revision. Do not conflate unknown, unavailable, empty and
zero.

## Empty and error states

No sources connected → explain import/event onboarding. Ingest failed → safe row and field
diagnostics with retry. Graph conflict → needs reviewer. Context stale → proposal or action
unavailable, with the reason. Approval expired → supersede and request a new approval. Task
completed with insufficient observation → outcome pending or insufficient, not resolved.

## Verification

`make web-dev` for development, `make supervisor-contracts` for client/schema drift,
`make supervisor-e2e` for the real-backend browser gate (`npx --prefix web playwright install
chromium` first). Cover duplicate click, response lost after commit, expiry or revocation during
review, changed payload, 409/412, 401/403/not-found, reload and restart, keyboard-only path, narrow
screen and offline/reconnect. Test IDs and assertions target user-visible behavior. Mocks only
qualify UI component tests.
