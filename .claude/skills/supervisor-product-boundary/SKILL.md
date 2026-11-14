---
name: supervisor-product-boundary
description: Defines what the wiseDo AI Supervisor is, its v1 scope, its six operational questions and its independence boundary. Use this skill whenever work touches product scope, README/docs wording, a new capability proposal, a claim about what the Supervisor can do, or any question of whether something belongs in this repository at all - even when the request does not mention scope explicitly.
---

# Supervisor product boundary

Sources: `README.md`, `docs/independent-supervisor.md` §1, Linear WIS-146, WIS-265–WIS-269, WIS-290.
Governing rules: `.claude/rules/00-product-independence.md`, `.claude/rules/09-verification-gates-and-evidence-claims.md`.

## What the product is

A standalone, human-governed operational intelligence system for manufacturing sites. It helps a
plant team understand its approved process, detect measurable production shortfalls, investigate
using trustworthy evidence, and coordinate a bounded response with a human in control.

## The six questions

Every consequential step answers all six. If one cannot be answered, say which and stop.

1. **What happened?** — source-backed observation with immutable provenance.
2. **What process is approved?** — reviewed graph and approved production plan.
3. **What is uncertain?** — coverage gaps, freshness, contradictions, missing inputs.
4. **What could be done?** — grounded recovery alternatives with visible trade-offs.
5. **Who authorized the exact action?** — scoped human approval of an exact action hash and expiry.
6. **What actually happened afterward?** — task readback and comparable post-action measurement.

## v1 end-to-end release

One customer site, one production line/product family, one production exception workflow:
commission → discover and interview → review knowledge → ingest evidence → detect and investigate
→ approve → coordinate → observe and learn under review.

## Out of scope for v1

PLC/machine actuation, autonomous quality release, automated staff reassignment, live model weight
retraining, unreviewed memory activation, physical safety overrides. Also excluded from the first
commercial boundary: on-prem, offline models, high availability, and any vendor connector that has
not passed its own replay/auth/outage/idempotency qualification.

## Issue map

| Group | Issues |
| --- | --- |
| Product outcomes | WIS-265 onboarding, WIS-266 evidence, WIS-267 investigation/recovery, WIS-268 feedback, WIS-269 qualification |
| Implementation | WIS-270 knowledge, WIS-271 interviews, WIS-272 intake/context, WIS-273 cases, WIS-274 approvals, WIS-275 agent runtime, WIS-276 proposals, WIS-277 tasks, WIS-278 UI, WIS-279 feedback, WIS-290 foundation, WIS-520 workstation and shift planning |
| Qualification | WIS-280 agent gates, WIS-281 clean install and pilot |
| Retained OEE component | WIS-222 epic, WIS-223–WIS-231 |

## Working rules

- Read `README.md` and `docs/independent-supervisor.md` before changing product scope.
- Do not equate a repository rename, scaffolding or fixture tests with a delivered Supervisor.
  Verify the gates required by the owning Linear issue.
- When the current implementation status is relevant, state it from Linear and the code, not from
  the stale status table in `docs/independent-supervisor.md` §5 or from a module's existence.
