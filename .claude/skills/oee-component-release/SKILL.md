---
name: oee-component-release
description: The open scaffold/CI corrections (WIS-225, findings F1-F5) and the v0.1.0 release and handoff package for the retained OEE calculation component (WIS-230). Use this skill when working on OEE contracts, CI gates, branch protection, the five named security/validation findings, or preparing any OEE component release, SBOM, fixture pack or handoff claim.
---

# OEE component scaffold corrections and v0.1.0 release

Sources: Linear WIS-225 (In Progress — partially delivered, acceptance open) and WIS-230 (Todo — no
component release verified).
Governing rules: `.claude/rules/09-verification-gates-and-evidence-claims.md`, `.claude/rules/07-api-contract-conventions.md`.

The owning repository is now `wisedo-ai-supervisor`. The OEE namespace stays a component contract and
introduces no MES dependency into the independent Supervisor product.

## Open WIS-225 findings

These were raised by fresh adversarial probes against main `f560f0ea945461e2132add426c7c3c144fd076a1`
and remain acceptance criteria. The existing suite passing 66 tests does not close them.

| ID | Defect | Acceptance |
| --- | --- | --- |
| **F1** | Positional logging formats arguments *before* the recursive key-based censor, so a nested password value reaches the event string. | Redact positional mappings and sequences before interpolation. Prove JSON and console output contain no placeholder for nested password/token/credential fields while ordinary arguments still format. |
| **F2** | The request-size middleware trusts `Content-Length` with no received-byte enforcement; 2,062 streamed bytes passed a 1,024-byte cap and returned 401 instead of 413. | Enforce actual streamed bytes before unbounded body materialization. Cover absent, false and malformed `Content-Length` and the exact-limit boundary. |
| **F3** | Datetime strictness was weakened, so a numeric epoch (`1767225600`) is accepted where the schema says date-time string. | Keep ISO timestamp compatibility while rejecting numeric and boolean timestamps across every exposed contract. Regenerated schemas must match runtime behavior. |
| **F4** | A dataset with a valid populated enum passes direct model validation but fails HTTP validation with 422. | Exercise real HTTP decoding for every enum-bearing contract: valid populated requests reach the application seam, invalid enums still fail. |
| **F5** | A frozen model does not freeze nested collections — validated provenance can be cleared behind a stored digest. | Immutable nested collections, or an explicit immutable snapshot boundary before digest and calculation. |

Also open: enforce required-test no-skips with meaningful coverage of every shipped package (a named
CI step alone is insufficient); verify protected main, required reviews and exact-head checks
(GitHub reported `protected=false`); link regression RED/GREEN evidence and the reviewed merge SHA.

Always-401 authentication and fail-closed 503 seams are **intentional scaffold**, not completed
JWT/RLS/runtime behavior. Runtime auth and persistence belong to WIS-229 — do not expand a WIS-225
fix into that implementation. Corrective branches use `fix/WIS-225-<slug>`.

## Required repository shape (WIS-225)

Strict contracts and an OpenAPI 3.1 boundary; versioned migrations where governed persistence is
required; dependency lock, local developer commands, container build, CODEOWNERS, security policy
and CI. API namespace `/v1/oee`; schema identity `oee.wisedo.io/v1`.

Contracts to define and publish: `OeeDatasetV1` (immutable asset/line, window, shift/calendar,
production, quality, state interval, ideal-cycle, source-quality and provenance facts),
`CalculationPolicyV1` (effective version; formula, time, readiness, reason and cost policies),
`OeeEvidencePackV1` (status, KPIs, loss tree, assumptions, exclusions, source IDs, digests,
calculation version), the stable `READY | LIMITED | NOT_READY | INVALID` states with bounded
machine-readable reason codes, and canonical serialization and compatibility rules.

Acceptance: unknown fields and unbounded collections fail validation; units, timestamp formats, UUID
identity, decimal representation and maximum payload sizes are explicit; JSON Schema and OpenAPI
artifacts generate deterministically and are checked for drift; provider and consumer fixtures exist
without a live MES; CI runs format, lint, strict typing, unit/property/security tests, migration
checks and a no-skips gate; dependency, secret and container scans run on PRs; branch protection and
CODEOWNERS are documented; RED contract tests precede implementation.

Non-goals for WIS-225: no KPI formulas, no live owner adapters, no UI, no deployment integration.

## v0.1.0 release contents (WIS-230)

- SemVer `v0.1.0` tag from a reviewed immutable commit.
- Versioned artifact pinned by digest.
- SBOM, dependency and security scan, signed provenance where available, OpenAPI/JSON schemas and
  release notes.
- Upgrade, migration and rollback notes, an operations runbook and support boundaries.
- A multi-shift synthetic fixture covering `READY`, `LIMITED`, `NOT_READY` and `INVALID`, visibly
  labelled demonstration data.
- A standalone comparison harness against approved reference datasets. Comparison against MES
  `services/oee-svc` belongs to the optional WIS-231 integration, not here.
- An MES adapter specification and a WIS-221 `OeeEvidencePackV1` consumer fixture.

## Release acceptance

Every predecessor acceptance criterion maps to code, tests and reviewed PRs. CI runs with no skipped
or silently deselected required tests. Architecture and security reviews carry no Critical or
Important finding. A fresh artifact installs and runs from the documented commands. Exact artifact,
image, SBOM, schema and test-evidence digests are linked to Linear. The standalone component owner
accepts the versioned provider and consumer fixtures.

**Label the release "calculation core ready" only.** Make no claim of live historian integration,
production deployment, customer acceptance or realized ROI.
