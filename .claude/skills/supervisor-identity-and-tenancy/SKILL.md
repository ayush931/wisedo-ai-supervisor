---
name: supervisor-identity-and-tenancy
description: OIDC sessions, bootstrap invitations, memberships, capabilities, recent-identity proof and tenant scoping for internal/identity, internal/membership, internal/bootstrap and internal/config. Use this skill for any work on sign-in, sessions, CSRF, roles, capability checks, service principals, commissioning or promotion endpoints, or whenever a change could affect who is allowed to do something.
---

# Identity, membership and tenancy

Covers `internal/identity/`, `internal/membership/`, `internal/bootstrap/`, `internal/session/`
(the shared session policy), `internal/config/`; member display names (migration `043`,
`GET /v1/me` `display_name`, `GET /v1/sites/{site}/members`).
Sources: WIS-290 "Identity and tenant contract" and "Server-enforced commissioning/promotion API",
`SECURITY.md`, `deploy/supervisor/README.md`.
Governing rules: `.claude/rules/03-tenant-isolation-and-rls.md`, `.claude/rules/05-human-authority-and-approvals.md`.

## Session contract

- OIDC authorization-code with PKCE. The backend verifies state, nonce, issuer, audience, expiry
  and allowed algorithms. Provider tokens are discarded after validation; if later retained for
  delegated APIs they are encrypted server-side.
- The browser receives an opaque Secure/HttpOnly/SameSite session cookie plus CSRF protection on
  mutations. No tokens in localStorage.
- Sessions: 30-minute idle, 8-hour absolute. Privileged approval reauthentication age ≤ 15 minutes,
  bound to the original session with its expiry preserved.

## Canonical tables

Implemented: `tenants`, `sites`, `memberships`, `browser_sessions`, `oidc_logins`,
`membership_changes`, `setup_invitations`, `member_profiles`. Users and service principals have no
table yet; a principal is its verified (issuer, subject). Site-scoped rows carry
non-null `tenant_id`/`site_id` with composite foreign keys.

## Roles

| Role | May |
| --- | --- |
| customer owner | administer site memberships, sources, activation mode; site-wide permitted views |
| supervisor | approve claims and proposals within granted capability scope |
| operator | submit evidence, acknowledge/complete own assigned tasks, submit feedback |
| viewer | read allowed scope |

The owner cannot bypass policy, enlarge its own site boundary or erase audit history. Scoped
ingestion service principals cannot approve or act. Reviewer/evaluator permission for knowledge
promotion is checked separately from the interview role.

## Bootstrap

A single-use expiring setup token supplied out of band, bound to an exact identity and site, with
private (0600) CLI output and authenticated CSRF-protected redemption. No default admin password
and no automatic first-login owner.

## Commissioning and promotion

Specified, not routed yet.

`POST /v1/sites/{site_id}/capabilities/{capability}/promotions` takes `expected_revision`,
`target_mode`, `scope_ids`, `qualified_release_id`, `coverage_revision_id`,
`commissioning_revision_id`, `customer_acceptance_id`. The server loads and verifies every
referenced record, exact code/prompt/tool/source-policy versions, same scope, freshness, absence of
revocation and the applicable WIS-280 gates, under a scope-policy lock, then persists policy epoch
plus audit plus outbox atomically. `GET /capabilities` reads back mode, epochs, blockers and
evidence refs; `/demote` disables new effects immediately and the global kill switch uses the same
authority rows.

A customer JSON asserting `passed=true` is never qualification. Site configuration may request a
mode change but never sets activation state directly.

## Revocation

Membership or role revocation takes effect on the next protected request or activity dispatch,
including already queued work. No cache keeps a revoked permission usable. Prove revocation-first
ordering with two-connection race tests.

## Verification

`make supervisor-integration CASE=identity`, plus two-tenant negative tests, role revocation,
CSRF/logout and scoped object access.
