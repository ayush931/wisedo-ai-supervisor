# Rule 05 — Human authority over consequential action

Source: `docs/independent-supervisor.md` §1 and §6, WIS-274, WIS-276, WIS-277, WIS-279.

No agent-generated text or model output can approve an action or dispatch a task.

## Required

- A proposal carries an immutable revision and a server-computed canonical payload hash.
  Approval is granted by an authenticated human with the `proposal.approve` capability.
- The approver must be a different person from whoever submitted or edited the proposal.
  Owner role does not implicitly bypass a capability grant.
- Default approval TTL is 30 minutes from immutable proposal creation. Timeout rejects execution.
- The executor independently re-evaluates eligibility predicates before dispatch: current policy
  and mode, reviewer eligibility and revocation, approval expiry, exact canonical hash, bound
  evidence revisions and freshness.
- Approval consumption and internal task creation happen in one transaction with a stable
  `action_id`. `committed` means an internal task exists, not that work was performed.
- The v1 action is `maintenance_investigation.create` with template `inspect_report_only`.
- Until WIS-290 ships a capability model, `proposal.approve` is held only by an active human
  member with the site supervisor role and a recent authentication (`internal/approvals`
  `PolicyRevision` 1). Replacing this mapping is a policy revision, not a silent widening.
- A material edit creates a new proposal revision and supersedes the previous one.

## Forbidden

- Model or service identities approving, reviewing, activating or changing promotion policy.
- A bypass endpoint that creates an approved task directly. Callers submit proposals.
- Treating Temporal retry as authorization, or regenerating an already approved payload on resume.
- Equipment adjustment, lockout bypass, quality release, production restart, PLC actuation,
  automated staff reassignment or safety override — all out of scope for v1.
