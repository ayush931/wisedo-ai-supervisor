---
name: supervisor-delivery-roadmap
description: The milestone plan, dependency order and operating modes for everything still to be built - M0/M1/M2 for the Supervisor, M4 for the retained OEE component, the fixed vertical slice, and the discovery/shadow/assisted mode ladder. Use this skill whenever planning what to build next, sequencing work, estimating, asking what blocks what, or deciding whether a capability is allowed to run yet.
---

# Delivery roadmap, milestones and operating modes

Sources: Linear project `wiseDo AI Supervisor — Human-Governed Operational Intelligence`
(start 2026-10, target 2027-06); WIS-265–WIS-269 outcome epics; WIS-267 "Fixed vertical slice";
WIS-269 "Required evidence ladder"; WIS-222 epic; `docs/independent-supervisor.md` §5.
Governing rules: `.claude/rules/02-branch-and-delivery-conventions.md`,
`.claude/rules/09-verification-gates-and-evidence-claims.md`.

## Milestones

| Milestone | Scope |
| --- | --- |
| **M0 — Scope, baseline & architecture** | WIS-146. Freeze architecture, contracts, implementation sequence and review ownership. Done. |
| **M1 — End-to-end vertical slice** | WIS-265, 266, 267, 270–278, 280, 290. The complete discovery → investigation → approval → task → outcome path. |
| **M2 — Independent deployment, security & pilot evidence** | WIS-268, 269, 279, 281. Reviewed feedback, clean install, pilot acceptance. |
| **M4 — Complete MES execution parity** (retained OEE) | WIS-222 epic: WIS-223–WIS-231. Separate from the Supervisor product. |

## Outcome epics and who owns what

| Epic | Outcome | Implementation children |
| --- | --- | --- |
| WIS-265 | Onboard customers into an approved process knowledge graph | WIS-270, WIS-271 |
| WIS-266 | Independent cross-domain plant evidence and context | WIS-272 |
| WIS-267 | Independent investigation and recovery workflow | WIS-273, 274, 275, 276, 277, 278, 290 |
| WIS-268 | Governed feedback and evaluated improvement | WIS-279 |
| WIS-269 | Qualify standalone behavior, deployment and customer value | WIS-280, WIS-281 |

A parent closes only when the end-to-end scenario consumes its children's real authenticated APIs
and persisted snapshots — never on its children's specifications.

## The fixed vertical slice (WIS-267)

Approved plan + approved good-count stream + machine-state evidence + frontline report →
deterministic shortfall → durable case → read-only investigation → cited proposal → human approval
→ one internal task → operator completion evidence → subsequent measured outcome.

Scope is v1 discrete-count production. Batch, continuous and mixed-unit conversion need separate
qualification. The agent neither controls machines nor certifies repair. Whole-site support grows
through separately commissioned area and capability profiles, not a single global switch.

**Case identity:** one active case per tenant/site/line/order/exception kind. New qualifying windows
attach evidence rather than generating repeated cases. A planned shutdown or order change closes
observation eligibility — it is not proof of recovery. Two consecutive non-qualifying windows mark
the active exception cleared but do not close the case. Recurrence while the case is active attaches
evidence; after explicit human closure it creates a new episode.

## Operating modes

| Mode | Allowed |
| --- | --- |
| `discovery_read_only` | candidate knowledge only |
| `shadow` | investigate and draft; no approval dispatch |
| `assisted` | human-approved internal task allowed |

A site or area profile carries explicit enabled capabilities and a policy revision. Mode changes are
audited human actions and are **never inferred from graph completeness**. There is no automatic
promotion. The stop control disables new investigations and actions while preserving read access,
evidence and reconciliation; existing human tasks do not disappear.

## Build order (WIS-267 §"Deliverable order")

1. WIS-290 independent foundation + WIS-272 reproducible inputs.
2. WIS-273 durable lifecycle + WIS-274 approvals + WIS-275 bounded authority.
3. WIS-276 investigation/proposal + WIS-277 executor/outcome.
4. WIS-278 role UI + WIS-279 reviewed feedback.
5. WIS-280 evaluation gates + WIS-281 clean-install customer acceptance.

## Evidence ladder (WIS-269)

1. Selected-stack integration, locks and licenses, real identity/tenant tests, durable persistence
   and recovery.
2. WIS-270–279 implement the actual end-to-end workflow. Architecture or scaffold is not delivery.
3. WIS-280 frozen 100-case release manifest, repeated actual-provider runs, deterministic
   authorization and concurrency gates, held-out evaluation, measured latency and cost.
4. WIS-281 clean install and upgrade, restore and action reconciliation, load envelope, observable
   SLOs, alerts, runbooks and user training.
5. A customer commissioning record naming buyer, sponsor, site, roles, authorized sources and
   region, pilot scope, price, support, data handling, measurable acceptance and exit procedure.
6. Shadow evidence and capability-specific promotion signed by a named customer owner. **With no
   customer available, state "software-qualified, customer-blocked" — never "accepted".**

## Release decision

Hard failure on: unauthorized effects or disclosure, stale-authority execution, duplicate internal
tasks, ungrounded action-driving claims, missing restore evidence, or required test skips.

All numerical thresholds in child issues are engineering targets until measured — never performance
claims. No committee of models substitutes for authorization or human acceptance. Single-agent and
simple relational-graph baselines must be compared before additional orchestration or datastores
become mandatory.

Measure and report separately: onboarding hours, time to first useful reviewed decision, coverage
and freshness, correct/false/missed alerts, reviewer effort, task closure time, active use,
hosting/model/support cost, and actual willingness to pay. Compare an agreed pre-pilot period with
shadow and approved-operation periods, disclosing sample size, confounders and uncertainty. Modeled
recoverable loss is not realized savings.

## Definition of complete for the slice

A repeatable fixture performs every step from authenticated ingestion to observed outcome; a second
tenant cannot read or influence any step; no model or remote WiseDo service is needed for
deterministic contract tests. Worker restart, duplicate delivery, stale approval, missing evidence
and rejected approval each have a visible safe outcome. Actual approved-model qualification is
separately required. Architecture approved, code merged, tests passed, deployed and customer
accepted remain distinct evidence states.

Gate: `make supervisor-integration CASE=WIS-267` and `make supervisor-e2e CASE=WIS-267`.
