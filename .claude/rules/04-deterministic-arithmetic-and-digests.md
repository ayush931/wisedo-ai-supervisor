# Rule 04 — Deterministic arithmetic, time and digests

Source: WIS-224, WIS-226, WIS-272 "Context and shortfall algorithm", `internal/oee/values.go`,
`internal/production`, `docs/independent-supervisor.md` §6.

## Required

- Exact rational/decimal arithmetic (`math/big`) with no intermediate rounding. Expose values
  rounded half-even to the declared precision while preserving exact inputs.
- Half-open UTC intervals `[start, end)`. An IANA timezone only projects local shifts and calendars.
- Source/event time and recorded/ingestion time stay separate fields.
- Canonical serialization uses the shared schema-ordered serializer: fixed field order, UTC
  normalized times, integer and decimal-string representations, no floats. Digests are SHA-256
  over that canonical body plus schema version and tenant/site.
- Identical inputs at different creation times must produce an identical digest; any changed
  source, claim or config revision must change it. Exclude generated IDs, creation timestamps and
  the digest field itself from the hashed body.
- Formula and policy versions appear in every result and digest.
- Pure kernels perform no clock, network, database or random reads.

## Forbidden

- Zero denominators producing NaN or Infinity; every zero denominator has a published decision-table outcome.
- Silently clamping physically suspicious values — flag `LIMITED` or `NOT_READY` and retain the raw value.
- Splitting a count proportionally across a window boundary, inferring takt, interpolating uniform
  daily output, or interpreting counters without a separately qualified adapter.
- Naively averaging child OEE percentages during rollup.
- Mutating published history. Late data creates a new calculation revision.
