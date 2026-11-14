# Rule 07 — HTTP API and contract conventions

Source: the shared boundary paragraph in WIS-270/271/272, plus WIS-273/274/275/276/277/278/279
("Revision precondition mismatch is 412; lifecycle/idempotency conflict 409; invalid contract 422"),
WIS-229, `internal/strictjson`, `internal/httpapi`.

## Required

- Paths are prefixed `/v1/sites/{site_id}`. Identifiers are UUIDs.
- Strict JSON decoding: forbid unknown fields, reject numeric timestamps, reject NaN/Infinity.
  Timestamps are RFC3339 with explicit offset normalized to UTC. Decimals are canonical strings
  with at most six fractional digits.
- Mutations require `Idempotency-Key`; same key with the same canonical body replays the original
  result, a different body returns `409 IDEMPOTENCY_CONFLICT`.
- Revision mutations require `If-Match` and return `412 REVISION_CONFLICT` when stale.
- Errors are RFC 9457 `application/problem+json` with a code, a correlation ID and bounded safe
  detail. Lists use stable cursor pagination, default limit 50, max 100.
- Authorization is rechecked before replaying a sensitive response body.
- Idempotency binds tenant, operation, canonical body and schema/policy versions.
- Regenerate OpenAPI/JSON schemas with `make supervisor-contracts`; uncommitted drift fails the gate.

## Forbidden

- Echoing payloads, tokens, credentials, DSNs, SQL or raw upstream bodies into errors, diagnostics
  or structured logs.
- Silent partial success on batch or import endpoints — report per-row accepted/duplicate/quarantined
  status with reasons.
- Returning credential values from any read API.
