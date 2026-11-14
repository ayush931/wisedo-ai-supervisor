# Security Policy

## Supported Versions

| Version | Supported          |
| ------- | ------------------ |
| 0.1.x   | :white_check_mark: |

## Security Invariants

1. **Tenant Isolation:** Every request derives tenant context strictly from verified JWT claims via `RequestContext`. Public request payloads containing tenant identity or authority are rejected with `403/422`.
2. **Database Row Level Security (RLS):**
   - **Independent AI Supervisor (Go):** Every tenant table runs with `FORCE ROW LEVEL SECURITY`. The schema is the checksummed, additive migration set in `internal/migrate/` (`001_foundation.sql` onward). Application transactions set tenant context transaction-locally with `set_config('supervisor.tenant_id', …, true)` (`internal/store`). The runtime role is non-owner, non-superuser and lacks `BYPASSRLS`; `store.VerifyRole` checks this at boot and on `/readyz`. Unauthenticated or cross-tenant access is rejected at the database engine level.
   - **Retained OEE component (Go, `internal/oee` and `internal/oeeservice`):** Uses the same Go store, tenant context and RLS. The earlier Python OEE scaffolding and its `app.tenant_id` helper were decommissioned and are not in this repository.
3. **No Direct External Coupling:** This service never connects directly to an unvetted MES database, PLC actuator, or caller-specified network destination. All evidence enters via verified OIDC sessions and bounded ingestion APIs (`POST /v1/sites/{site}/operational-sources/{source}/batches`).
4. **Data Sanitization:** Error responses (RFC 9457 / JSON problem), diagnostics, and structured logs never expose bearer tokens, credentials, raw upstream bodies, SQL queries, DSNs, or employee personal data.

## Reporting a Vulnerability

Please report security issues privately to `security@wisedo.io`. Do not open public GitHub issues for security vulnerabilities.
