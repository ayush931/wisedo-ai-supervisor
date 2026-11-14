# Rule 06 — Evidence, provenance and failing closed

Source: `docs/independent-supervisor.md` §1 and §3, WIS-270, WIS-272, WIS-277, WIS-279, WIS-227.

When a prerequisite cannot be established, report the specific reason and remain read-only.
Never fill a gap with AI speculation, a zero count or a cached authority.

## Required

- Every entity has provenance to source revisions; every relationship is an explicit claim with
  provenance and resolvable source spans.
- Readiness is explicit: `READY`, `LIMITED`, `NOT_READY`, `INVALID` with stable reason codes.
  Missing actuals yield `NOT_READY`, never zero. Zero productive duration yields
  `NO_PLANNED_PRODUCTION`, not an exception.
- Ingestion into inbox/outbox is durable receipt, not semantic acceptance.
- Corrections append and identify their exact predecessor. No revision gaps, no branching
  correction chains, no destructive overwrite.
- Unclassified stays visible; an unseeded taxonomy fails closed.
- Execution eligibility queries the authoritative revocation/access ledger on every retrieval.
  Revoked content is never sent to a model and never silently returned from a lagging projection.
- Deletion leaves a non-sensitive tombstone that survives restore; citations become explicitly
  unavailable rather than silently pointing at deleted sources.

## Forbidden

- Equating a machine `stopped` state with a bottleneck or cause; absence of fault is not proof
  of fault absence.
- Presenting modeled opportunity as realized savings, or letting a manual closure reason overwrite
  a measured outcome.
- Claiming causation from task completion or co-occurring recovery. Improvement is threshold
  recovery, labelled as such.
- Conflating unknown, unavailable, empty and zero.
