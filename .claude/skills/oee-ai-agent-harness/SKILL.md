---
name: oee-ai-agent-harness
description: ADR 0003 - the historical advisory AI agent proposal for the retained OEE component (WIS-233) - read-only evidence consumption, ReAct tool calling, grammar-guided decoding, pgvector memory, hallucination guardrails and its phased delivery order. Use this skill when work references the OEE agent, agent_* schema, evidence Q&A, confidence scoring or policy simulation, and to check what that proposal was superseded by.
---

# OEE advisory agent harness (historical proposal)

Source: `docs/adr/agentic-ai-docs/0003-oee-intelligence-ai-agent-harness.md`.
**Status: historical proposal for the retained OEE component (WIS-233).**
Runtime target in the ADR is Python 3.12 / FastAPI / LangChain / pgvector / Redis.

> Read this before using it. The independent AI Supervisor product is governed by
> `docs/independent-supervisor.md` and the Go-only directive — see
> `.claude/rules/01-go-only-runtime-boundary.md` and the `supervisor-agent-runtime` skill, which is
> the **current** agent contract (WIS-275). This ADR's Python/FastAPI/LangChain/Redis stack is not
> the Supervisor implementation. Treat it as design history for the OEE component, and as the source
> of the safety principles that WIS-275 inherited.

## Four non-negotiable principles (these did carry forward)

1. **Read-only consumer** — the agent never bypasses the deterministic calculation pipeline. It
   consumes sealed evidence only.
2. **No state mutation** — it never mutates datasets, policies or sealed evidence.
3. **No metric fabrication** — if a metric is not in a sealed evidence pack, the agent refuses.
4. **Advisory role** — outputs never override, modify or replace a `READY`/`NOT_READY`
   determination.

## Bounded-context separation

The agent sits downstream of the core as a separate bounded context with its own `agent_*`
PostgreSQL schema. It has **no direct database access** to the `oee_*` schema and reads evidence only
through `GET /v1/oee/evidence/{evidenceId}`. It cannot access raw datasets, submit calculation
requests, trigger recalculation, modify policies or golden vectors, write to the core schema, or
override a `NOT_READY`/`LIMITED` state. OIDC auth and tenant context are shared so the security
boundary propagates; the agent inherits tenant isolation strictly.

## Capabilities

Natural-language OEE diagnostics; anomaly detection against historical baselines; root-cause
analysis correlating downtime taxonomy with loss classification; multi-period trend analysis
distinguishing structural from operational change; predictive-maintenance advisory correlations;
and policy impact simulation strictly labelled simulated/advisory.

## Orchestration

A ReAct loop — Thought → Action → Observation → iterate → cite-and-answer — over a constrained tool
set: `query_evidence`, `query_evidence_trends`, `lookup_downtime_taxonomy`, `compare_metrics`,
`explain_readiness`. The system prompt embeds OEE domain knowledge (the A×P×Q definition, TEEP, the
Six Big Losses) and the advisory, evidence-bound tone.

**Grammar-guided structured decoding** is how determinism is reached with self-hosted open models:
schema-constrained logit masking against strict tool-call models, so malformed JSON is impossible and
types, enums and citation UUID patterns are exact; and native tokenizer chat templates to avoid
prompt drift.

## Persistence

`agent_conversation`, `agent_message` (role, content, tool calls and results, citations),
`agent_insight_cache` (scope, window, insight type, provenance, expiry), `agent_embedding`
(pgvector, HNSW index), `agent_audit_entry`. Embeddings cover **summaries** of evidence packs,
readiness-failure explanations and historical downtime patterns — never raw data.

## Guardrails

Every output cites the exact `evidenceId` each claim came from. Confidence is `HIGH` (exact metric
in a recent `READY` pack), `MEDIUM` (inferred across packs or older data) or `LOW` (limited evidence
or `LIMITED` readiness). A post-generation validator parses metrics out of the response and compares
them against the cited pack in memory — a mismatch blocks the response and returns a fallback
(`409 agent-hallucination-prevented` under RFC 9457). Tool calls may use only evidence IDs verified
to exist in session context. Every query, response, referenced evidence ID and tool call is audited
with tenant isolation; per-tenant and global LLM budgets and token tracking apply; JWTs are stripped
from any context passed to the model.

## Technology position

Self-hosted open-weights inference only (vLLM serving the ADR 0004 fine-tuned checkpoints).
Commercial third-party APIs are excluded to keep manufacturing data inside the VPC. `pgvector`
inside PostgreSQL rather than a new vector database. Open-source embedding models.

## Testing

Golden Q&A vectors mapping questions to expected tool calls and facts; hallucination-detection tests
that inject prompt variations and assert the validator blocks fabricated metrics; tenant-isolation
assertions; tool-calling parameter correctness; cost and latency benchmarks; adversarial
prompt-injection tests against character break, system-prompt exposure and read-only bypass.

## Delivery order

1. Agent schema and conversation models, API stubs. 2. Evidence consumer client. 3. Core tools and
the ReAct loop. 4. pgvector and embedding generation. 5. Asynchronous pre-computed insights.
6. Policy simulation. 7. Hardening — structured output validation, guardrails, audit, full testing.

## Change control

Model version, prompt definitions and tool schemas are versioned. A model or significant prompt
change requires benchmarking against the golden Q&A vectors. **Any change that would give the agent
authority — for example proposing automated policy changes — requires a superseding ADR**, because
it breaks the read-only-consumer constraint.
