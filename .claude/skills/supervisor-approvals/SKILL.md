---
name: supervisor-approvals
description: Durable immutable approvals - proposal revisions, canonical payload hashing, distinct-human review, 30-minute expiry, state machine and concurrency control (WIS-274). Use this skill for any work on ApprovedActionV1, proposal decisions, revocation, staleness invalidation or the approval lock order, and whenever code could let something be authorized without a distinct human approving an exact payload.
---

# Durable approvals

Source: Linear WIS-274 (Linear: Done, checked 2026-11-03). Built and integration-tested, not
customer-accepted:
- the pure rules (`internal/approvals/approvals.go`, PR #87);
- persistence and the decision API (migration 044, `internal/approvals/store.go`,
  `internal/httpapi/proposals.go`, PR #126);
- lifecycle events (050);
- the executor's dispatch-time re-check and consumption (`internal/approvals/dispatch.go`,
  `internal/cases/execute.go`, WIS-277);
- agent-generated proposals (WIS-276);
- staleness on evidence and case change (`MarkEvidenceProposalsStale`,
  `MarkCaseProposalsStale`).
`proposal.approve` is still the `PolicyRevision` 1 mapping from rule 05, not a separate
capability grant. Check the issue for the open items before relying on this list.
Governing rules: `.claude/rules/05-human-authority-and-approvals.md`, `.claude/rules/07-api-contract-conventions.md`.

## API

Prefix `/v1/sites/{site_id}`. `GET /proposals/{id}`, `POST /proposals/{id}/decisions`,
`POST /proposals/{id}/revoke`. Decisions require an `Idempotency-Key` UUID and `If-Match` revision;
the body is `{decision: approve|reject, expected_revision, reason (1..2000)}`.

Header/body revision mismatch → 422; stale revision → 412; invalid token → 401; same-site denied
capability → 403; unavailable storage → 503; foreign resource → 404. Lists are scope-filtered and
cursor-paginated at max 100.

## Immutable proposal revision

Action kind `maintenance_investigation.create`; exact asset/line/order/task-assignee UUIDs;
controlled task template `inspect_report_only`; instructions ≤ 4000 chars; `due_at`; case, context,
config, graph and policy revisions; prerequisite versions; `issued_at` / `expires_at`;
`generated_by_run_id`.

The server builds canonical UTF-8 JSON with fixed field order, UTC-normalized times, integer and
decimal-string representations and no floats, then takes a SHA-256 that includes the schema version
and tenant/site. Commit one canonicalization fixture shared by API, runtime and executor. Never hash
unnormalized model prose as authority.

The task action is an investigation and report request. It is not an instruction to alter equipment,
bypass lockout, clear a quality hold or restart production.

## Policy and concurrency

Default: one human site supervisor holding `proposal.approve`; never a model or service identity.
If a human submitted or edited the proposal, the reviewer must be a different person. The owner role
does not implicitly bypass a capability grant. Approval TTL is 30 minutes from immutable proposal
creation — the customer may tighten it; anything beyond 24 h needs an explicit commissioned profile.
Distinct-approver configurations use `UNIQUE(proposal_revision, reviewer_user_id)` and every
requested reviewer must remain eligible before dispatch.

States: `pending → approved|rejected|expired|superseded`; `approved → consumed|revoked|expired|stale`.
There is no transition back to `pending`. A material edit creates a new revision and supersedes the
previous one.

The decision transaction acquires rows in the global order **scope_policy → prerequisite
membership/source/version rows sorted by stable ID → case → proposal/approval → action ledger**,
compares the expected revision, verifies expiry against the server clock, records an immutable
decision and updates state. On concurrent approve/reject exactly one succeeds and the other gets 412.

## Staleness

Invalidate on changed plan/order/asset identity, a bound claim correction or revocation, a changed
task payload/assignee/due date, expired measurement freshness, changed authorization/policy/mode or
a failed current eligibility predicate. Do **not** invalidate merely because unrelated new telemetry
arrived. The executor independently re-evaluates these predicates. A reason-coded stale result needs
a fresh proposal and review — never a silent substitution.

## Persistence

`proposal_revision`, `approval_decision`, `approval_state`, `idempotency_record` and an append-only
audit table, all tenant-scoped with composite foreign keys. Retain the immutable decision and hash
plus revocation and dispatch linkage; redact sensitive payload from general logs. Backup and restore
retain consumed keys and cannot duplicate actions.

## Implementation map

- `approvals.go` (pure, no clock): `transitions`/`Apply`, `CheckReviewer`, `ExpiresAt`/
  `ValidateWindow`/`CheckWindow`, `Decide` (checks in order 422 → 412 → 409 → expiry → 403),
  `CheckDispatch`, `Canonical` (the `ApprovedActionV1` envelope via `internal/canonical`).
  The committed canonicalization fixture lives in `testdata/`.
- `store.go` (`Service`): `Submit` is internal only — the server assigns proposal ID, revision,
  `issued_at` (database clock) and `expires_at`, checks the payload against the case row, and a
  material edit supersedes a pending revision (rejected or expired revisions start a fresh
  review). `Get`/`List` read; `Decide`/`Revoke` run `begin` (session, site lock, membership) →
  receipt replay → `lockHead` (case `FOR SHARE`, then state `FOR UPDATE`) → `expireIfDue`
  (committed before refusing) → the pure rule → decision row, transition, audit, receipt.
- The API `revision` (ETag, `If-Match`, `expected_revision`) is the **approval state revision**,
  which advances on every change — so a concurrent second decision gets 412. `proposal_revision`
  is the payload revision.
- Interim `proposal.approve` until the WIS-290 capability model: an active human member with the
  site **supervisor** role and auth_time within the session policy's `RecentAuth` (15 min). Owner
  and viewer are refused (403 `PERMISSION_DENIED`); a stale login is 428
  `REAUTHENTICATION_REQUIRED`; the submitter is 403 `REVIEWER_NOT_ELIGIBLE`.
- `Submit` requires the case at the payload's case revision. At decision time approval needs
  the bound case open and still bound to the proposal's context (`checkDecisionBinding`); the
  case revision is not compared, because the case advances its own revision (proposed ->
  awaiting_approval) on the normal path to approval. A replaced context or a closed case is 409
  `PROPOSAL_STALE`; rejection is always allowed. Expiry is 409 `APPROVAL_EXPIRED`.
- Reviewer eligibility counts every human who submitted any revision of the proposal
  (`earlierAuthors` -> `EditedBy`), not only the current revision's submitter.
- One live proposal per case: a new `Submit` while a pending or approved proposal is inside its
  window is `ErrActiveProposal`; a change is an edit of the live proposal.
- Canceling a case marks its approved proposals stale in the same transaction
  (`MarkCaseProposalsStale`, event `mark_stale`, audit `proposal.stale`; migration 050 also
  allows `consume` for WIS-277).
- Revoking an evidence source (`evidence.Service.Revoke`) marks every approved proposal whose
  current revision binds it (`evidence_revisions` kind `evidence` in the canonical payload) stale
  in the revocation transaction (`MarkEvidenceProposalsStale`; lock order source → case →
  proposal; audit actor is the revoker). A pending proposal cannot go stale, so approving one
  whose bound evidence is revoked or missing is 409 `PROPOSAL_STALE` (`checkBoundEvidence`);
  rejection stays possible. Unrelated sources do not invalidate anything.
- Migration 044: `proposal_revisions` (DB re-hashes `canonical_payload` and checks the envelope
  against the row), `approval_states` (trigger mirrors `transitions`), `approval_decisions`
  (partial unique index: one approve/reject per reviewer per proposal revision; `revoke` is not a
  review), `approval_transitions`, `approval_receipts`, `approval_audit`.
- The site write lock taken by `begin` already serializes decisions on one site; the state row
  lock is defence in depth.

## Verification

`make supervisor-integration CASE=WIS-274` against real PostgreSQL: populated migration, simultaneous
approval/rejection, replay, conflicting hash, same reviewer counted twice, requester self-approval,
expired/stale/revoked state, scope revocation, restart, DB outage, cross-tenant lookup and list,
forged body principal. Implemented as `TestProposalApprovalsUpgradeFrom043AndGuards`
(`internal/migrate`), `TestProposalApprovalsHTTPPostgres` and `TestProposalStorageOutageIs503`
(`internal/httpapi`); the selector is `proposals|WIS-274` in `scripts/supervisor/test.sh`.
