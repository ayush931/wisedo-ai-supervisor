---
name: supervisor-oee-kernel
description: The retained OEE component in internal/oee - canonical timeline normalization, the versioned A/P/Q/OEE/TEEP kernel, readiness states, the governed downtime taxonomy and loss tree, and evidence sealing (WIS-222 epic). Use this skill for any work on availability, performance, quality, TEEP, shift or counter normalization, downtime reasons, loss trees, Pareto output or anything under internal/oee.
---

# OEE and downtime intelligence core

Covers `internal/oee/` (`values.go`, `timeline.go`, `readiness.go`, `kernel.go`, `loss.go`, `cost.go`,
`evidence_pack.go`, `service.go`, `contracts.go`, `contract_validate.go`, `service_contracts.go`),
`internal/oeeservice/` (persisted calculation runs and policies), `internal/httpapi/oee.go`,
`internal/httpapi/oee_service.go` (`/v1/sites/{site}/oee/...`) and migrations `029`–`030`.
Sources: Linear WIS-222 epic with WIS-223–WIS-231; `docs/oee-foundation.md`;
`docs/adr/0001-oee-authority-and-mes-integration.md`, `0002-oee-intelligence-core-module-design.md`,
`0005-oee-calculation-industry-standard.md`.
Governing rule: `.claude/rules/04-deterministic-arithmetic-and-digests.md`.

This is the **retained component**, not the Supervisor product runtime. Its scope is governed by
`docs/independent-supervisor.md`; earlier MES integration decisions apply only to that optional
integration, which is MES-owned work under WIS-231 and outside this repository's implementation
boundary.

## Timeline normalization (WIS-226, `timeline.go`)

Convert versioned owner facts into one canonical calculation timeline without inventing data.

- Half-open UTC intervals `[start, end)`; IANA timezone only projects local shifts and calendars.
- Source/event time and recorded/ingestion time stay separate.
- Exact duplicate event IDs deduplicate; conflicting duplicates fail closed.
- Overlap precedence is policy-defined; unresolvable conflicts return `NOT_READY`.
- Gaps stay visible and reduce coverage. No implicit running or stopped fill.
- Cross-midnight shifts and DST gap/fold behaviour are deterministic.
- Planned breaks, changeovers and shutdown exclusions are versioned inputs.
- Counters handle reset, rollover, out-of-order samples and late correction only with explicit source
  evidence.
- Late data creates a new calculation revision; published history is never mutated.

Output: canonical normalized intervals and count deltas with ordered source-event references,
coverage metrics, warning and reason codes, and a deterministic digest. The same dataset and policy
must produce byte-identical normalized output.

## Calculation kernel (WIS-224, `kernel.go`)

| Quantity | Definition |
| --- | --- |
| Planned Production Time | eligible scheduled time minus policy-approved planned exclusions |
| Run Time | Planned Production Time minus classified stop duration |
| Availability | Run Time / Planned Production Time |
| Ideal Run Time | Σ(total-count contribution × the ideal-cycle revision effective for that contribution) |
| Performance | Ideal Run Time / Run Time |
| Quality | Good Count / Total Count |
| OEE | Availability × Performance × Quality |
| TEEP | OEE × Planned Production Time / Total Calendar Time |

All arithmetic uses the defined decimal precision and rounding. Raw values are retained; physically
suspicious values are flagged `LIMITED` or `NOT_READY` by policy and never silently clamped. Mixed
products, changing ideal cycles, unit conversion, parallel asset and line rollup, rework, scrap,
microstops, negative deltas and every zero denominator need a published decision-table outcome.
Aggregation is mathematically defined — child OEE percentages are not naively averaged. The kernel is
pure: no clock, network, database or random reads.

## Readiness (`readiness.go`)

`READY`, `LIMITED`, `NOT_READY`, `INVALID` with stable reason codes. Stale, malformed, ambiguous and
contradictory evidence has a stable fail-closed outcome.

## Downtime taxonomy and loss tree (WIS-227, `loss.go`)

Immutable versioned reason hierarchy with `draft`, `published`, `effective` and `retired` states;
planned versus unplanned classification with effective dates; stable loss categories for availability,
performance and quality losses. Unclassified stays visible and an unseeded taxonomy fails closed.
Reject cycles, duplicate codes, cross-tenant parents and invalid effective ranges. An operator
correction appends a new governed classification revision with actor, reason and evidence — history is
never overwritten, and reclassification creates a new result revision. Each contribution retains
source event IDs, its interval or count contribution, the policy and taxonomy revision and a digest.
Pareto output has a deterministic total order and reconciles exactly to the loss tree; every aggregate
drills down to exact contributing facts.

## Cost of loss (WIS-228)

ISO-4217 currency, decimal arithmetic, explicit unit basis, versioned rate/burden/margin assumptions
with effective dates, deterministic allocation across line/asset/product/order/reason. No implicit FX
— a cross-currency rollup requires a supplied approved FX revision. Missing, expired or conflicting
rates fail closed with bounded reason codes.

## Governed taxonomy and loss tree (WIS-227, `internal/oee/loss.go`)

Pure Go, no persistence (persistence and API stay WIS-229). `ExplainLosses` turns stoppage, speed and
quality facts plus an exact taxonomy revision into a loss tree, two Pareto analyses and a revision-1
result; `ReclassifyLoss` appends a governed revision and never overwrites. Rules enforced:

- Fail closed: empty or unseeded taxonomy, taxonomy of another tenant or site, draft or non-covering
  taxonomy, missing `RecordedAt`.
- Facts are validated, never clamped: negative or missing measurements, empty or duplicate event IDs
  and intervals outside the window are errors. A missing speed duration or quality quantity is unknown,
  not zero.
- Only published or effective reasons inside their own effective range classify; draft, retired and
  expired reasons leave the fact visible as `UNCLASSIFIED`.
- The kernel reads no clock and no randomness: caller supplies `RecordedAt`/`OccurredAt`; audit IDs are
  UUIDv5 over canonical content. Digests use the canonical serializer, bind scope, window, taxonomy
  digest, policy revision, every contribution and the predecessor digest, and exclude recorded times.
- Every result is reconciled before return (contributions, tree and both Pareto analyses sum exactly;
  each contribution appears once in the drill-down). A reason whose parent is in another
  classification is a root in its own subtree, never orphaned. Pareto groups by category and code.
- Reclassification requires owner, supervisor or operator role, justification and evidence, the exact
  taxonomy revision and digest used for the result, a reason usable when the loss occurred, and the
  caller's tenant and site. Another tenant or site is a non-disclosing `ErrContributionNotFound`; every
  denial is audited on the result it tried to touch and changes nothing.

## Service boundary (WIS-229, `internal/httpapi/oee.go`)

Typed endpoints for validate, calculate, immutable result and provenance retrieval, and
health/readiness. Persist only governed policies, taxonomies, cost assumptions, calculation manifests
and results, idempotency and audit — never a raw historian copy, owner credentials, arbitrary
SQL/code/templates/URLs or direct owner database access.

## Verification

`go test ./internal/oee/...`, `make check`. Golden reference vectors state exact intermediate and
final values; property tests cover conservation, component relationships, ordering, overlap and gap
conservation, duplicate idempotency and duration totals. Decimal and rounding tests are invariant to
locale, process timezone and input order.
