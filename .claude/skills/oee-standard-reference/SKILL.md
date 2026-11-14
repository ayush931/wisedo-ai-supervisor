---
name: oee-standard-reference
description: ADR 0005 - the industry-standard OEE definitions this project implements - the exact A/P/Q/OEE/TEEP formulas, the Nakajima Six Big Losses framework and its mapping onto project facts, and the six non-negotiable calculation constraints. Use this skill as the reference whenever writing, reviewing or explaining an OEE calculation, a loss category, or why a number must not be averaged, clamped or defaulted.
---

# OEE calculation — industry-standard reference

Source: `docs/adr/video-embedding-docs/0005-oee-calculation-industry-standard.md`.
Companion to `oee-kernel` (implementation) and `oee-core-module-design` (structure).
Governing rule: `.claude/rules/04-deterministic-arithmetic-and-digests.md`.

## Time hierarchy

```
Total Calendar Time
└── Planned Production Time              ← schedule loss: no orders, holidays, shutdowns
    └── Operating / Run Time             ← Loss 1 breakdown, Loss 2 setup & changeover
        └── Net Operating Time           ← Loss 3 idling & minor stops, Loss 4 reduced speed
            └── Fully Productive Time    ← Loss 5 process defects, Loss 6 startup rejects
                = Good Pieces × Ideal Cycle Time
```

## The Six Big Losses (Nakajima TPM)

| Loss | OEE factor | Category | Project mapping |
| --- | --- | --- | --- |
| 1 | Availability | Unplanned stop / breakdown | `DowntimeFactV1` where `category = UNPLANNED` |
| 2 | Availability | Planned stop / setup & changeover | `DowntimeFactV1` where `category = PLANNED` |
| 3 | Performance | Micro-stops | short intervals between expected cycles (≤ 5 min) |
| 4 | Performance | Speed reduction | actual run rate slower than `ideal_cycle_time` |
| 5 | Quality | Production rejects | `QualityFactV1.scrap_units` during steady-state run |
| 6 | Quality | Startup rejects | `QualityFactV1.scrap_units` within changeover/warmup window |

Distinguishing loss 3 from loss 4 requires the taxonomy revision in
`CalculationPolicyV1.taxonomy_version`. Distinguishing loss 5 from loss 6 requires knowing whether
the quality batch falls inside a startup or changeover window. Neither distinction may be guessed.

## Six non-negotiable constraints

1. **Exact decimal arithmetic** — all calculations use exact decimals with scale ≤ 6. IEEE 754
   floating point is forbidden for governed results, because binary rounding compounds across
   shift/day/month aggregation and breaks byte-stable replay.
2. **Zero clamping** — components are never clamped into `[0, 1]`. An overshoot is diagnostic
   evidence that must be surfaced.
3. **No zero-denominator substitution** — if planned time is 0 the kernel emits `NOT_READY` with
   `MISSING_DATA`, never a fabricated `0.00%`.
4. **No percentage averaging** — OEE percentages are never averaged across shifts, lines or assets.
   Consumers aggregate raw numerators and denominators and compute the ratio over the union.
5. **No ambient system clock** — the pure domain receives immutable timestamps and clock values
   through injected ports. Reading the wall clock in domain code is prohibited.
6. **Deterministic replay** — identical dataset, policy and code version must yield a byte-identical
   evidence payload.

## Formula set

```
Availability   = Run Time / Planned Production Time
Performance    = Ideal Run Time / Run Time
Quality        = Good Count / Total Count
OEE            = Availability × Performance × Quality
TEEP           = OEE × Planned Production Time / Total Calendar Time

Planned Production Time = eligible scheduled time − policy-approved planned exclusions
Run Time                = Planned Production Time − classified stop duration
Ideal Run Time          = Σ(total-count contribution × ideal-cycle revision effective for it)
```

Worked check from the ADR: 420 minutes run time out of 480 minutes planned production gives
Availability = 0.875 (87.5%).

## Where this binds in code

`internal/oee/values.go` (exact rational arithmetic, canonical hashing, half-open UTC intervals),
`timeline.go` (ordering and collision detection), `readiness.go` (state machine and reason codes),
`kernel.go` (the formulas above), `loss.go` (taxonomy and loss tree), `service.go` (classification,
downtime cost, evidence sealing), `contracts.go` (validated DTOs).
