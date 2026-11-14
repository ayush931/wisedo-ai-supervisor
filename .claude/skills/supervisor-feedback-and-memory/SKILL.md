---
name: supervisor-feedback-and-memory
description: Append-only outcome-linked feedback, memory candidates and reviewed knowledge releases (WIS-279). Use this skill for any work on OutcomeFeedbackV1, MemoryCandidateV1, KnowledgeReleaseV1, activation or revocation of prompts/playbooks, evaluation manifests or rollback - and whenever something would let the system learn or change its own behavior without a human reviewing it.
---

# Feedback and reviewed memory

Source: Linear WIS-279 (parent WIS-268; Linear: Done, checked 2026-11-03).
- **Built:** `OutcomeFeedbackV1` (`internal/feedback`, migration 069, and `POST`/`GET /feedback`
  plus `GET /feedback/{id}` in `internal/httpapi/feedback.go`, with integration tests).
- **Not in code:** `MemoryCandidateV1`, `KnowledgeReleaseV1` and their routes. The package comment
  calls memory candidates "the later memory-candidate increment"; the sections below on them are
  the specification, not built behaviour.
Governing rules: `.claude/rules/05-human-authority-and-approvals.md`, `.claude/rules/06-evidence-provenance-and-fail-closed.md`.

Uses the WIS-290 transaction, authorization, audit and Temporal conventions. No new memory service
and no new graph authority.

## Contracts

- **OutcomeFeedbackV1** — `id`, `site_id`, `case_id`, nullable `proposal_id`/`task_id`, `actor_id`,
  `kind` (`observation|correction|usefulness`), `verdict`
  (`helpful|unhelpful|uncertain|not_applicable`), reason 1–2000 chars, 0–20 scoped `evidence_ids`,
  paired nullable `observation_start`/`observation_end`, `context_snapshot_id`, `created_at`,
  `revision`. The server binds referenced objects to one site and case; unknown or inaccessible
  evidence cannot be stored. Freeform text never becomes an executable instruction. A correction
  identifies the claim/source revision it challenges and creates a new candidate rather than
  overwriting anything.
- **MemoryCandidateV1** — `id`, `site_id`, 1–20 `source_feedback_ids`, `kind`
  (`process_claim|playbook|prompt`), content ≤ 16 KiB plus digest, `source_snapshot_ids`,
  `proposed_by`, `review_state`, `expected_base_version`, `created_at`, `revision`. Process claims
  route through WIS-270 approval instead of duplicating its registry.
- **KnowledgeReleaseV1** — `id`, `site_id`, `kind`, immutable candidate IDs and digests, nullable
  `base_release_id`, `evaluation_manifest_id`, `proposed_by`, `reviewed_by`, `activated_by`,
  `effective_at`, `state` (`candidate|qualified|active|superseded|revoked`), `revision`. The
  evaluation manifest must match exact candidate and base digests plus model and prompt versions;
  a mutable URL is not qualification.

## API

Under `/v1/sites/{site_id}`: `POST /feedback` (201 durable; unchanged idempotent replay returns the
original receipt), `GET /feedback` and `/feedback/{id}` (operator sees authorized case and own-task
scope), `POST /memory-candidates`, `GET /memory-candidates` and `/{id}`,
`POST /memory-candidates/{id}/review` (`approve|reject`, reason, `expected_revision`),
`POST /knowledge-releases`, `/{id}/activate`, `/{id}/revoke`, `GET /knowledge-releases` and `/{id}`.

422 invalid contract or reference, 409 same key with changed body or lifecycle conflict, 412 stale
revision, 403 missing role, non-disclosing 404 for an inaccessible object. Read limits default 50,
max 100.

## State and transactional rules

Feedback is append-only; authorized redaction follows WIS-290 retention with non-sensitive tombstone
metadata instead of hidden history rewriting. Candidate: `candidate → in_review →
approved|rejected`; a change produces a new revision and invalidates the old review. An approved
candidate is not yet active memory.

A human reviewer different from the proposer reviews content. An authorized site owner or reviewer
activates a qualified release. No model or service identity may review, activate or modify promotion
policy. Activation is a compare-and-swap against `expected_base_version` with a unique active release
per `(site, kind)` and audit plus outbox in the same transaction; duplicate jobs cannot activate
twice. A revoked release is excluded from retrieval immediately by canonical-state check even if an
embedding or graph cache lags. In-flight consequential actions revalidate the required release and
policy epochs; old case history stays reproducible.

Prompt and playbook qualification requires a WIS-280 manifest with all hard assertions passing and
candidate non-regression within the frozen thresholds. Rollback activates an explicitly selected
previously qualified version through the same checks — no implicit latest fallback. Bad facts always
use WIS-270 supersession or revocation.

## Outcome semantics

Task approval, task completion, operator opinion and measured production recovery are four separate
records. Preserve `pending|observed_improvement|no_improvement|insufficient_data|not_comparable`
from WIS-277; improvement is not proof the action caused it. Rejection is not automatic negative
training data and accepted-but-unsuccessful advice is not a positive label. No cross-customer
pooling, no automatic weight updates and no automatic authority promotion.

## Qualification trust boundary

The evaluation manifest is a server-stored immutable WIS-280 record whose candidate, code, prompt,
tool and dataset hashes and gate results are verified by a designated release reviewer. Public
candidate and activation APIs can never accept a user-supplied `passed=true`.

## Verification

`make supervisor-integration CASE=feedback`, `make supervisor-eval MODE=actual CASE=memory-regression`,
`make supervisor-e2e CASE=feedback`. Cover restart and outbox replay, duplicate feedback and
activation, concurrent releases, wrong base digest, stale eval manifest, revoked retrieval,
cross-tenant references, malicious content, self-review, accepted-but-unsuccessful outcome and
rollback.
