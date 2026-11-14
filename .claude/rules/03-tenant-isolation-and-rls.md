# Rule 03 — Tenant isolation and row-level security

Source: `SECURITY.md`, `docs/oee-foundation.md`, WIS-290 "Identity and tenant contract",
WIS-229, and the shared boundary paragraph in WIS-270/271/272.

## Required

- Tenant and actor come from verified authentication only. Site is path-scoped and authorized.
- Every tenant-owned row carries non-null `tenant_id` (and `site_id` where site-scoped), with
  composite foreign keys that make a cross-tenant reference unrepresentable.
- `FORCE ROW LEVEL SECURITY` on every tenant table. The runtime role is non-owner, non-superuser
  and lacks `BYPASSRLS`. Migrations run under a separate privileged role.
- Set tenant context transaction-locally (`SELECT set_config('supervisor.tenant_id', $1, true)`).
  A pooled connection must never leak the previous transaction's context.
- Membership or role revocation takes effect on the next protected request or activity dispatch,
  including already queued work.
- Reads, writes, object download URLs, exports, searches, workflows and audit views all enforce scope.

## Forbidden

- Trusting tenant or authority fields from a caller's JSON or headers — reject them with 403/422.
- Treating a UUID as an access grant, or joining operational records on display names.
- Disclosing existence: a cross-tenant resource lookup returns the non-disclosing 404.
- Inferring authority from an email domain, or shipping a default permanent support superuser.
  Break-glass access is time-bounded, customer-authorised and audited.
