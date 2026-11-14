---
name: supervisor-http-api-contracts
description: The shared HTTP boundary in internal/httpapi and internal/strictjson - strict JSON decoding, idempotency, revision preconditions, RFC 9457 problem responses, pagination and OpenAPI contract drift. Use this skill whenever adding or changing any endpoint, request or response type, error code, or validation rule, and before touching anything under internal/httpapi.
---

# HTTP API and contract boundary

Covers `internal/httpapi/`, `internal/strictjson/`.
Sources: the shared delivery-boundary paragraph in WIS-270/271/272, the status-code paragraph in
WIS-273–WIS-279, WIS-229, `docs/oee-foundation.md`.
Governing rule: `.claude/rules/07-api-contract-conventions.md`.

## Request shape

- Paths are prefixed `/v1/sites/{site_id}`; the site is path-scoped and authorized. Tenant and actor
  come only from verified authentication, never from request JSON or a header.
- UUID identifiers. RFC3339 timestamps with an explicit offset, normalized to UTC. Half-open
  intervals. Canonical decimal strings with at most six fractional digits.
- Strict decoding through `internal/strictjson`: unknown fields forbidden, numeric timestamps
  rejected, NaN and Infinity rejected.
- Mutations carry `Idempotency-Key`; revision mutations carry `If-Match`.

## Response and status conventions

| Situation | Status |
| --- | --- |
| invalid contract or reference | 422 |
| same idempotency key, changed body, or lifecycle conflict | 409 |
| stale revision precondition | 412 |
| unauthenticated | 401 |
| same-site denied capability | 403 |
| cross-tenant or unknown resource | 404 (non-disclosing) |
| payload/stream limit exceeded | 413 |
| rate or envelope limit | 429 with `Retry-After` |
| storage or dependency unavailable | 503 |

Errors use RFC 9457 `application/problem+json` with a code, a correlation ID and bounded safe
detail. Never echo payloads, tokens, credentials, DSNs, SQL or raw upstream bodies.

Lists use stable cursor pagination with limit default 50 and max 100. Authorization is rechecked
before replaying a sensitive response body.

## Idempotency and revisions

Idempotency binds tenant, operation, canonical body and schema/policy versions. The same key with
the same canonical body replays the original result; a different body is `409 IDEMPOTENCY_CONFLICT`.
A stale `If-Match` is `412 REVISION_CONFLICT`. Concurrent duplicate requests must create exactly one
accepted operation.

## Existing endpoints

Each family registers in `internal/httpapi/api.go` through a `register*` function in its own file:
membership and `/v1/me` (`membership.go`), evidence, knowledge, operational sources, intake
(`POST /v1/sites/{site}/operational-sources/{source}/batches`) and imports, plan revisions,
plant contexts, OEE service (`/v1/sites/{site}/oee/...`), the WIS-520 workstation routes
(shift plans, job orders, station profiles and records, shift summaries and handovers), and the
WIS-274 proposals (`GET /proposals`, `GET /proposals/{id}`, `POST /proposals/{id}/decisions`,
`POST /proposals/{id}/revoke` in `proposals.go`). The retained `/v1/oee/*` routes and
`/health/live`, `/health/ready` remain. `/livez` checks the process and `/readyz` reports required
components without exposing secrets. A published route must also be routed and document 503:
`TestPublishedOpenAPIRoutesAreServed` (and `TestPublishedKnowledgeRoutesMatchRegisteredRoutes` for
the knowledge family) fail otherwise.

## Limits and cancellation

Enforce hard request, event, window, row, byte, deadline, concurrency and memory limits before
materialization. Cancellation propagates through every stage and leaves no partial published result.
Overload rejects predictably rather than exceeding the resource budget.

## Contract drift

`make supervisor-contracts` runs the contract tests, regenerates `contracts/` with
`go run ./cmd/supervisor-contracts`, then fails on `git diff` or any untracked artifact under
`contracts/`. Generation lives in `internal/contracts` (reflect-based, deterministic, closed schemas
with `additionalProperties:false`). Contract fields are described by `json` plus an optional
`contract:"sha256|decimal6|datetime|max=N"` tag; an unknown tag or unsupported kind panics instead of
publishing a half-described contract. Published today (`Catalog()` plus the `*Paths()` functions,
one file per family: `oee.go`, `knowledge.go`, `members.go`, `workstation.go`, `proposals.go`):
the `/v1/oee` and site OEE service, evidence and knowledge, `/v1/me` and members, the WIS-520
workstation routes and the WIS-274 proposals, as OpenAPI 3.1 (`contracts/openapi.json`),
`contracts/schemas/*.schema.json` and generated TypeScript (`contracts/typescript/oee.ts`, from
`typescript.go`). Operational source, intake, import, plan revision and plant context routes are
not yet published. `RequestContext` is never published — tenant and actor are not
caller-supplied. Errors are RFC 9457 `application/problem+json` (`problem` in `api.go`) with
`code`, `status`, `title` and `correlation_id`. Unit gate: `TestCommittedContractsMatchGenerator`.
`scripts/supervisor/untested-packages.sh` fails the unit gate for any Go package with no test file.
