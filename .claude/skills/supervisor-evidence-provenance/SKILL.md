---
name: supervisor-evidence-provenance
description: Evidence records, source authority, document currentness and freshness qualification in internal/evidence. Use this skill for any work on immutable evidence, source authority classes, document review expiry, currentness confirmations or evidence retrieval - and whenever code decides whether a piece of evidence is still allowed to support a decision.
---

# Evidence, authority and currentness

Covers `internal/evidence/` (`authority.go`, `currentness.go`, `evidence.go`) and migrations
`005`, `008`, `018`, `019`, `039` (authority class and independence groups).
Sources: `docs/independent-supervisor.md` §3, `docs/oee-foundation.md`, WIS-270, WIS-272, WIS-228.
Governing rule: `.claude/rules/06-evidence-provenance-and-fail-closed.md`.

## Principles

- Evidence is immutable and candidate-only. No partial `READY` ever seals.
- Every record maps to an exact source with identity, revision, hash and span. Sequence
  completeness, freshness and quarantine state are part of the record, not an afterthought.
- Authority class is `reported | observed | approved_document`. Authority comes from a reviewed
  designation, never from a source's self-declaration.
- An `independence_group_id` prevents repeated copies, re-extractions and interviews quoting the same
  SOP from counting as independent corroboration.

## Currentness and freshness

No automatic source expiry by age alone. Versioned policy supplies `max_age`; approved documents
default to a 90-day review expiry, and operational observations use the WIS-272 freshness rules
(event heartbeat 120 s, import max age 24 h, tolerated future clock skew 30 s). A currentness
confirmation is an explicit human act recorded as its own revision.

Freshness qualification decides whether a document may still back a decision. An expired or
unconfirmed document does not silently keep its authority — relevant promotion is blocked with a
stable reason code.

## Retrieval and revocation

Execution eligibility queries the authoritative revocation and access ledger on **every** retrieval.
Stored refs and policy revisions guarantee exact history; current-authority status is always checked
separately on access. A revoked source is denied immediately even if an embedding or projection cache
lags, and its content is never sent to a model.

## Evidence packs

When losses are translated into a reporting artifact (WIS-228 `OeeEvidencePackV1`), include the KPI
summary, planned and unplanned durations, downtime Pareto, loss tree, cost opportunity, data-quality
and coverage, exclusions, formula/policy/taxonomy/rate versions, exact source references, the
normalized-input digest, the result digest and a caller-supplied calculation timestamp. Keep
`modeledOpportunity`, `avoidedLoss` and `realizedSavings` separate — the core cannot infer realized
savings. Display strings must be formula-injection safe.

## Audit

Assert an audit row for each retrieval and each review. Audit queries record access without raw
prompts or source text.
