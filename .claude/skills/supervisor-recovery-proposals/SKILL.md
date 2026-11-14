---
name: supervisor-recovery-proposals
description: Grounded investigation output and RecoveryProposalV1 - observations versus hypotheses, unknowns, bounded options, task payload and submission to approval (WIS-276). Use this skill for any work on proposal generation, citation validation, hypothesis labelling, assignee selection or shadow/assisted mode, and whenever a model's output is about to become something a human approves.
---

# Grounded recovery proposals

Source: Linear WIS-276 (Linear: Done, checked 2026-11-03). Built and integration-tested, not
customer-accepted:
- the deterministic, provider-independent core `internal/recovery` (contract, measurement
  serialization, preconditions, validation, outcome mapping, fixture pack);
- the investigation specialist on the WIS-275 runtime (`internal/agent/runtime.go`,
  `caseinput.go`), which submits a valid proposal through `approvals.Service.SubmitWithCheck` with
  no human submitter;
- the maintenance roster (migration 064).
Not found in code: a separate shadow/assisted site-mode switch. Agent work is gated by
`SUPERVISOR_AGENT_MODE_ENABLED`, and every proposal still needs human approval. Check the issue
before relying on this list.

## Implementation map (`internal/recovery`, pure: no clock, database or network)

- `Measurements` serializes expected/actual/deficit for each context from its published decimal
  strings verbatim; `MeasurementID` is a UUIDv5 over context and metric. A missing value is
  omitted, never zero.
- `Precheck`/`Preconditions` run before any provider call and return `needs_input` with sorted
  reasons: case not investigating, mode disabled, fewer than two windows, a window not READY (its
  context reason codes are carried, e.g. `INVALID_DELTA` for a counter reset, `NO_HEARTBEAT` for a
  missing source), not eligible now, other order or line, no approved plan, a plan other than the
  one approved now (`PLAN_CHANGED`), a unit other than `count`, non-consecutive windows, or a pair
  that does not meet `EvaluateShortfall`.
- `Judge` decodes untrusted output strictly (`DecodeProposal`), then `Validate` refuses it whole
  for: wrong case/context/revision, caps, invalid text, unknown measurements, any observation not
  byte-equal to the server measurement, a number stated with a unit or a percentage in free text
  that is not a server measurement, citations outside the captured evidence, uncited
  observations/hypotheses/maintenance options, option kinds outside defer/clarify/escalate/
  maintenance_investigation, a selection not offered, a task other than
  `maintenance_investigation.create`/`inspect_report_only` for the case's line/order, an assignee
  not on the supervisor-confirmed roster for the line, a maintenance selection with no confirmed
  assignee, a maintenance option without machine-fault evidence, an executable description,
  versions other than current, and bad times.
- Outcomes: provider failure -> `escalated`; schema or validation failure -> `refused`; a valid
  proposal selecting maintenance -> `proposed` (the only outcome with an action, and only after
  WIS-274 human approval); clarify/defer -> `needs_input`; escalate -> `escalated`.
Governing rules: `.claude/rules/06-evidence-provenance-and-fail-closed.md`, `.claude/rules/05-human-authority-and-approvals.md`.

Consumes the immutable `PlantContextSnapshotV1` (WIS-272) and the active `CaseV1` (WIS-273).
Shortfall arithmetic stays deterministic — the model never decides counts, readiness or source
authority.

Required preconditions: an approved plan; consistent order/product/unit and line/asset identities;
trustworthy good-count windows; approved process relationships; current permission and mode. Unknown
process ownership prompts a clarification — never infer an assignee from a random document.

## RecoveryProposalV1

UUID proposal/case/context IDs; `case_revision`/`context_revision`; `observations`
(`measurement_id`, `evidence_ids`, `metric_name`, `value_decimal`, `unit`, window start/end);
`hypotheses` (text ≤ 1000, supporting and contradicting evidence IDs); `unknowns`
(`field`, `question`, `required`); `options` (`kind: defer|clarify|escalate|
maintenance_investigation`, text, evidence IDs); `selected_option`; nullable `task_payload`;
policy/plan/graph/model/prompt/tool versions; `generated_at`/`expires_at`; rationale ≤ 2000.

Caps: 10 observations, 5 hypotheses, 10 unknowns, 3 options. Citations may reference only evidence
actually retrievable in the snapshot — invented or cross-tenant IDs reject the output. Numerical
observations must exactly match the server's measurement serialization. Uncertainty and causal
inference are explicitly labelled hypothesis.

## task_payload

`kind = maintenance_investigation.create`, `asset_id`, `line_id`, `order_id`, `assignee_id`,
`template = inspect_report_only`, description ≤ 4000, `due_at`. The assignee is chosen or confirmed
from a scoped authorized roster, not inferred from qualification text. The template requests
inspection and reporting under customer procedures — no equipment adjustment, quality release or
restart authorization. If no qualified responsible assignee is recorded, `selected_option = clarify`
and `task_payload = null`. The authoritative hash and immutable revision come from WIS-274, never
from the model.

## Flow

1. Capture context and current eligibility; insufficient evidence → `needs_input` with no provider call.
2. The investigation specialist returns evidence-linked observations and hypotheses plus at most
   three specific clarifying questions.
3. Deterministic checks reject unsupported counts, invalid units, a stale or mismatched plan, foreign
   citations and unsupported action kinds.
4. The proposal specialist produces bounded options; the exact result passes authorization and
   template validation.
5. Persist the proposal, then create the approval under the durable business identity
   `case_id:context_revision:proposal_revision`. Return the actual persisted ID; a timeout triggers a
   readback, never a fictitious success.
6. Shadow mode records a draft only; assisted mode submits for human approval. The system never
   autonomously promotes site mode.

## Judgement boundaries

A current machine fault indication may support a maintenance investigation, but absence of fault is
not proof of fault absence. If evidence points only to materials, staffing or a quality restriction,
propose clarification or escalation — do not disguise those interventions as maintenance. A changed
approved plan or a corrected source after drafting requires a new context and proposal. Irrelevant
fresh telemetry may append to the case without silently changing an approved payload.

## Fixtures and gate

Deterministic pack: two 15-minute windows, expected 75 units each, actual 60 and 60; approved
line→machine graph; machine stoppage evidence; a supervisor-confirmed maintenance assignee. Expect
deficit 15/15, a valid two-window case, a cautious hypothesis and an inspect/report proposal.
Counter-reset, missing source, mixed-unit, stale plan, no assignee, source injection, materials-only,
fabricated evidence, unsupported numeric statement and provider failure each have a specified
`needs_input`/`refused`/`escalated` output and zero action.

`make supervisor-integration CASE=WIS-276` and `make supervisor-eval MODE=actual CASE=WIS-276`
(≥ 20 cases, 100% exact cited measurements, 0 forbidden effects).
