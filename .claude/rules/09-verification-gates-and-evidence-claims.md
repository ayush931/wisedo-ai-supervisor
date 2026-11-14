# Rule 09 — Verification gates and honest evidence claims

Source: `AGENTS.md`, `CONTRIBUTING.md`, `docs/independent-supervisor.md` §6, WIS-280, WIS-281,
WIS-290 "Build, dependencies and exact command contract", `Makefile`.

Keep implemented, tested, integrated, deployed and customer-accepted claims separate. Golden
fixtures are contract checks, not actual-agent or customer evidence.

## Gate commands

| Target | Scope |
| --- | --- |
| `make supervisor-check` | format, lint, deterministic unit/property/security tests, frontend typecheck and build |
| `make supervisor-contracts` | regenerate OpenAPI/JSON schemas, fail on drift |
| `make supervisor-integration` | real PostgreSQL/Keycloak/Temporal; `CASE=` narrows a nonempty suite |
| `make supervisor-e2e` | browser-owned onboarding-to-task workflow against a real backend |
| `make supervisor-eval MODE=actual` | WIS-280 actual-provider qualification |
| `make supervisor-load` | fixed seed/envelope/host, latency, queue, memory, error output |
| `make supervisor-restore` | isolated backup/PITR restore with effect reconciliation, measured RPO/RTO |
| `make supervisor-release-check MODE=clean-install\|upgrade` | WIS-281 release rehearsal and manifest |

Also `go test ./internal/... ./cmd/...` for the Go suite and `make check` for retained component checks.

## Required

- A required test that is absent or skipped fails its gate. A missing target, empty selection or
  unknown selector is a failure, not a pass.
- A missing provider credential, unavailable database or missing fixture reports `BLOCKED` with a
  nonzero exit — never a green fallback.
- CI artifacts retain the SHA, the exact command, the environment manifest, the report and the
  failures with redacted data.
- Deterministic adapters are visibly labelled and never satisfy an actual-model gate.
- Freeze qualification thresholds in the manifest before comparing. A failure is never fixed by
  quietly weakening the threshold.
- Never run a destructive restore against production by default; require a disposable target allowlist.

## Release gates

Clean installation, security and multi-tenancy, deterministic calculation, human authority, and
the Future Vision edge-vision gate (`docs/independent-supervisor.md` §6) all apply.
