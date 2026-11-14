---
name: supervisor-agent-runtime
description: Scoped specialist agent runtime - DelegationEnvelopeV1, SpecialistResultV1, the tool allowlist, per-cycle budgets, breakers, cancellation and the prompt-injection boundary (WIS-275). Use this skill for any work involving model calls, agent delegation, tool exposure, token or cost budgets, traces, or whenever untrusted source text could reach a model.
---

# Scoped agent runtime

Source: Linear WIS-275 (Linear: Done, checked 2026-11-03). Built and integration-tested
(`CASE=WIS-275`), not actual-model qualified:
- the contracts (`internal/agent/contracts.go`: `DelegationEnvelopeV1` `ValidateChild`,
  `SpecialistResultV1` `ValidateFor`);
- the runtime (`runtime.go`, migration 055) with budgets, the provider breaker (`provider.go`),
  span traces (`agent.span`) and run passages (migration 071);
- the specialists: investigation, interview, configuration drafts and chat.
Providers are `fixture` and a pinned local OpenAI-compatible endpoint (`localmodel.go`), plus an
optional hosted route (WIS-623, `hosted.go`, rule 08 "Model endpoints"): Qwen on Model Studio
Singapore from the compiled `config.HostedEndpoints`, used only by a site whose owner switched it on
(`internal/modelhosting`, migration 074). `CreateRunTx` pins a run to it (provider, region, model,
revision, price version from the committed `hosted_prices.v1.json`); `callRole` re-reads the pin,
re-checks the switch inside the budget reservation and pseudonymises the request (`pseudonyms.go`:
known member, site, tenant and customer names, e-mails, phone numbers; restored only from the
request's own map, an unknown token refuses the output). Refusals before dispatch:
`HOSTED_MODEL_DISABLED`, `HOSTED_MODEL_UNAVAILABLE`, `PRICE_MISSING`; never a fallback.
Actual-model qualification (WIS-280 `MODE=actual`) has not run: it needs a hosted key file.
Governing rules: `.claude/rules/08-untrusted-input-and-model-boundary.md`, `.claude/rules/01-go-only-runtime-boundary.md`.

Go owns durable workflows, policy, budgets and tools. A hierarchical deterministic orchestrator owns
the task ledger and validated routing — and gets no tools for machine or control execution. Logical
specialist modules may share one model. Do not claim a multi-specialist advantage before WIS-280
compares it to a single-agent baseline on identical held-out cases.

## DelegationEnvelopeV1

`schema_version=1`, `run_id`, nullable `parent_run_id`, `role`, tenant/site/case IDs,
`context_snapshot_id`, `required_evidence_refs`, `allowed_tool_names`, `principal_reference`,
`policy_revision`, UTC `deadline`, `remaining_requests` / `remaining_tokens` /
`remaining_cost_microusd`, `cancellation_generation`, `hop`.

The server constructs the envelope. `principal_reference` resolves in server state and is not a
bearer token. Child authority is the intersection of parent, role and current policy.
The orchestrator envelope's `allowed_tool_names` is the run's captured tool scope (the parent term;
the orchestrator itself never calls a tool); `ValidateChild` requires child tools within that scope
and the role table, and the runtime adds current policy at dispatch. Only the executor's tool list
must be empty. No scope
tokens, credentials or system prompts travel between agents or inside model-visible content. Every
tool receives its server context separately; requested tenant or actor fields are rejected.

## SpecialistResultV1

`run_id`, `role`, `status` (`success|partial|needs_input|refused|failed`), nullable `artifact_ref`,
`cited_evidence_ids`, `missing_required_fields`, `contradictions`, `concise_rationale` ≤ 2000 chars,
nullable `failure_code`. An optional model confidence is advisory and never substitutes for evidence
or policy.

## Role matrix

| Role | Reads | Writes via application validation | Forbidden |
| --- | --- | --- | --- |
| discovery/mapping | authorized source snippets, approved ontology | candidate claims and questions | approving claims or actions |
| investigation | immutable scoped context, approved claims, deterministic measurements | investigation artifact | executing a task, editing evidence |
| proposal | validated investigation and task template | draft proposal artifact | approving or assigning a task directly |
| feedback | completed cases, outcomes, reviewed feedback | candidate memory/playbook change | self-promoting policy or prompts |
| orchestrator | typed results, task ledger | run state only | content-driven permission changes |
| executor (non-LLM) | approval, current policy, prerequisites | one approved internal task | arbitrary action or tool choice |

## Tools

Models may request only `context.get`, `evidence.get`, `claims.search` and `measurements.get`, within
captured scopes. Retrieval IDs are allowlisted against tenant/site/version at execution. A model
proposal is returned as data; deterministic code submits it for approval only after validation. No
shell, SQL, browser, arbitrary HTTP or dynamic MCP discovery. Any later MCP endpoint must have the
same schema, permission and egress boundaries.

## Budgets per investigation+proposal cycle

Max 3 specialist invocations, depth 1 (children cannot delegate), 2 independent read branches, 12
total tool calls, 6 provider requests including retries, 24 000 cumulative input+output tokens and a
USD 1.00 cost ceiling. Per request: max 2000 output and 8000 input tokens. Reserve budget atomically
before parallel calls and charge actual usage afterwards. Required IDs, policy and evidence facts
must fit — fetch bounded exact evidence or return `context_overflow`, never silently truncate.
Unknown model pricing disables a billable call until versioned prices are configured. These are
adjustable engineering guardrails, not measured customer economics.

## Failure behavior

Primary model timeout or schema failure → one narrowed retry within the same budget → a
deterministic `needs_input`/degraded record → human escalation. No unsupported diagnosis in a
fallback. A hard tenant or capability denial, or a malicious tool request, is non-retryable.
Contradictory specialist evidence becomes `needs_input`, never a majority vote. Provider breaker:
3 failures in 5 calls opens for 60 s with one half-open probe.

## Revocation, cancellation and tracing

Before each provider dispatch recheck source access, current scope and policy, cancellation and
active entitlement. Revocation winning before dispatch prevents the call; after dispatch, cancel
best-effort, discard unauthorized results and record the reason — never promise to recall bytes
already sent. Recheck again before artifact commit. Check the cancellation generation before and
after every tool call.

Structured traces carry `trace_id`/`span_id`/`run_id`/parent/role/site-scoped identity, model,
prompt and tool versions, evidence IDs and hashes, start/end/status, latency, token totals, cost,
call counts and reason-coded failures — as a redacted summary with approved evidence references,
never raw credentials or unrestricted employee text.

## Verification

`make supervisor-integration CASE=WIS-275` and `make supervisor-eval MODE=actual CASE=WIS-275`.
Table-driven: cross-tenant, forged delegation, instruction-in-source, schema-valid malicious payload,
stale scope, cancellation, unknown tool, shared-budget race, retry ceiling and contradiction.
