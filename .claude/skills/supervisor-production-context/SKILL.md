---
name: supervisor-production-context
description: PlantContextSnapshotV1, epoch-aligned 15-minute production windows, expected/actual/deficit arithmetic and the two-window shortfall rule in internal/production. Use this skill for any work on production plans, good-unit deltas, readiness states, window boundaries, context digests or shortfall detection - and whenever a number that could trigger a case is being computed.
---

# Production context and shortfall detection

Covers `internal/production/` (plan and delta domain), `internal/plantcontext/` (`Assemble`,
`EvaluateShortfall`, persistence), `internal/httpapi/contexts.go` and migration `024`. Source: Linear WIS-272 "Context and shortfall algorithm".
Governing rules: `.claude/rules/04-deterministic-arithmetic-and-digests.md`,
`.claude/rules/06-evidence-provenance-and-fail-closed.md`.

## PlantContextSnapshotV1

`context_id`, `revision: 1`, `site_id`, `order_id`, `asset_id`, `window {start,end}`,
`approved_plan_revision`, `process_snapshot_id` and its SHA-256, `source_record_refs`
(`record_id`, `revision`, `sha256`), `mapping_revision_ids`, `source_policy_revision_ids`,
`created_at`, `readiness` (`READY|LIMITED|NOT_READY`), `reason_codes`, optional
`expected_good_units` / `actual_good_units` / `deficit_good_units`, `unit: 'count'`,
`missing_capabilities`, optional `supersedes_context_id`, `digest`.

Hash only the canonical schema, scope, input revisions, window, calculated facts and readiness.
Exclude the generated `context_id`, `created_at`, `supersedes_context_id` and the digest itself.
Snapshots are immutable at `revision = 1`; a replacement gets a new ID and digest. Two snapshots
built at different times from identical inputs must share a digest; any changed source, claim or
config revision must change it.

## Window arithmetic

- Disjoint UTC 15-minute windows aligned to epoch multiples of 900 seconds.
- Select the exact approved plan revision.
- `expected_good_units` = Σ(`target_good_units_per_second` × productive seconds) across rate/window
  intersections, excluding explicit planned breaks.
- `actual_good_units` = Σ of the latest non-conflicting corrected good-unit deltas for the same
  order/asset/unit. Intervals must be disjoint and fully cover the productive observation period,
  with explicit zero intervals.
- `deficit_good_units` = max(0, expected − actual).
- Compute with exact decimals and no intermediate rounding; expose values rounded half-even to six
  decimal places and keep the exact inputs.

Never split a count proportionally across a window boundary — missing boundary-aligned data returns
`NOT_READY`. No counter or reset interpretation without a separately qualified source adapter.

## Readiness preconditions

Authoritative watermark `complete_through >= window_end`, contiguous source sequence, evaluation
time ≥ `window_end + 120 s`, heartbeat freshness ≤ 120 s, good clock quality, approved
mappings/routing and an uncontested plan. Zero productive duration yields `NO_PLANNED_PRODUCTION`,
not an exception. Missing completeness or conflicting rates yields `NOT_READY`.

## Shortfall rule

WIS-273 opens one shortfall case only when `deficit >= max(5, 0.10 × expected)` in two consecutive
complete windows. This is a versioned engineering default the customer's planner accepts or revises
at commissioning. The context itself never infers root cause.

## Golden fixture

Rate 1 unit/s, 100 productive seconds and 800 planned-break seconds → expected 100 per window;
contiguous actual 80 → deficit 20. Two such windows qualify. Corrected actual 95 → deficit 5 and no
qualifying two-window exception. Incomplete or offline → `NOT_READY`.

## Limits and corrections

Fetch max 10k records per context; a larger window returns `422 WINDOW_TOO_LARGE` rather than
silently truncating totals. Corrections create new snapshots and notify dependent cases through the
outbox; old bodies stay immutable and their eligibility becomes superseded. Machine observations and
operator reports remain cited context, never conclusions.
