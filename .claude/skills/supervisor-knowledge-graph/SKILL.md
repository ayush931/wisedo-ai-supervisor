---
name: supervisor-knowledge-graph
description: The approved process knowledge graph in internal/knowledge - entities, external identities, source revisions and spans, process claims, conflicts, reviews, coverage and immutable approved snapshots. Use this skill for any work on claims, predicates, provenance, corroboration, contradiction handling, graph traversal or ApprovedProcessSnapshotV1, and whenever a change could let an unreviewed fact become authoritative.
---

# Process knowledge graph

Covers `internal/knowledge/` and migrations `005`–`019`, `039`–`041` (source independence, entity
redirects, 2000-character decision reasons).
Sources: Linear WIS-270, WIS-265; `docs/independent-supervisor.md` §3; `deploy/supervisor/README.md`.
Governing rules: `.claude/rules/06-evidence-provenance-and-fail-closed.md`, `.claude/rules/03-tenant-isolation-and-rls.md`.

PostgreSQL relational entities, claims and reviews with bounded SQL traversal. No Neo4j or Graphiti
runtime in v1; an optional projection can never authorize a fact or an action.

## Tables

The specification names `entity`, `external_identity`, `source_revision`, `claim_review` and so on;
the implemented tables are `supervisor.knowledge_*` (entities and heads, claim heads/revisions/spans,
conflicts, reviews, snapshots, discovery, redirects, outbox) plus `supervisor.evidence_*` for sources
and spans (migrations 005–016, 039–041). There is no separate external-identity table; resolution is
`GET /external-identities/resolve`. Every record carries `tenant_id`,
`site_id`, UUID `id`, `created_at`, `schema_version=1`. Immutable tables deny UPDATE/DELETE to the
runtime role; mutable status and index rows are derived from events, never authority.

## Core records

- **Entity** — type is one of `site|area|asset|process|operation|order|material|tool|role|
  qualification|document|constraint`, plus the WIS-611 value-stream types `supplier|customer|location|
  inspection_point`; `asset_kind` (`line|machine`) is required only for assets.
  Unrecognized types stay source-only candidates with `in_scope=false`; they are not promoted.
- **ExternalIdentity** — unique `(tenant_id, site_id, source_id, entity_type, external_key)`. Only
  exact approved mappings auto-resolve. Fuzzy candidates need review; a merge records a redirect
  decision and preserves original IDs and history.
- **SourceRevision** — server-side SHA-256 of actual bytes, `authority_class` of
  `reported|observed|approved_document`, `independence_group_id`, `access_policy_revision`. Canonical
  event bytes use stable JSON key ordering. No cross-tenant hash dedup, no storage keys from user paths.
- **Span** — must resolve to captured bytes or the parsed artifact; the extractor version and parsed
  artifact SHA are recorded.
- **ProcessClaimV1** — `object_entity_id` XOR typed `literal`, effective interval, `evidence_class`
  of `report|observation|hypothesis|constraint`, 1–20 source spans, optional extractor and confidence.
- **Decision** — `approve|reject|revoke|resolve_conflict` with reviewer, reason and expected review
  revision.

## v1 predicates

`part_of_asset`, `located_in`, `performs`, `part_of`, `precedes`, `consumes`, `produces`,
`requires_tool`, `requires_qualification`, `responsible_role`, `follows_document`, `has_constraint`.
Literal constraints use typed properties `capacity_per_hour`, `escalation_after_seconds`,
`objective`, `actual_step`.

WIS-611 value stream (migration 070): `supplied_by` (material → supplier), `received_at` and
`stored_at` (material → location), `inspected_at` (material or operation → inspection_point),
`ships_to` (material → customer); properties `lead_time_days` (material, supplier),
`cycle_time_seconds` and `setup_time_seconds` (operation, asset), `batch_size` and `yield_ratio`
(operation; a yield is at most 1), `sample_size` and the text `inspection_method`
(inspection_point). The new properties are single-valued (two values conflict); relationships stay
many-valued. Discovery capability `process_understanding` has a nine-cell supplier-to-dispatch
template that the database guard holds exactly.

WIS-615: every `process_understanding` cell can be bound (`BindingSupported`). A witness uses only
the cell's predicates plus the linking ones (`part_of`, `consumes`, `produces`, `precedes`,
`performs`), contains each predicate the cell requires (for example `supplied_by` for
`material_suppliers`), and connects every entity it mentions to the interviewed process at its
pinned revision. Receiving inspection must be about a material and in-process inspection about an
operation; packed goods must be produced by a selected claim. Qualification is unchanged: a bound
cell is `approved` only on confirmed `document` sources, so interview-sourced witnesses stay
`candidate` (no policy) or are refused (`FRESHNESS_BLOCKED`).

Asset hierarchy is same-site and acyclic; a machine belongs to one
approved line per effective time. Approved routing is a DAG — reject self-edges and cycles.
New predicates require a schema migration and review, never tool authority.

## State machine

`candidate → approved | rejected`; `approved → revoked | superseded`; any candidate or approved claim
may become `contested`; `contested → approved | rejected` only through explicit conflict resolution.
A later edit creates a new revision and requires reapproval. Never auto-promote from model confidence.

## Corroboration and conflict

Promotion requires an authorized reviewer accepting an exact current revision from a designated
authoritative source. One such source can suffice; independent corroboration raises confidence but
is not universally required. Repeated copies, re-extractions and interviews quoting the same SOP are
one independence group. Compare claims only when effective intervals overlap and subject/predicate
match with a competing typed value on a single-valued predicate — different wording alone is not a
contradiction. Record claim-level `CONTRADICTS`, mark both contested, preserve refs and dates.

## Retrieval and snapshots

An approved snapshot holds at most 200 entities and 500 claims, with a stored canonical body of at most
8 MiB (`knowledge.MaxSnapshotBytes`, migration 072, WIS-621).
Bounded traversal: max 2 hops, 200 entities, 500 claims, 64 KiB returned text, stable ID order and
an explicit truncated flag. Snapshot creation locks touched revision heads, validates approvals,
current access, conflicts and tenant/site, then inserts snapshot plus outbox atomically. A stale or
inaccessible graph yields explicit `NOT_READY`, never cached authority.

Errors: `409 KNOWLEDGE_CONFLICT`/`UNKNOWN_MAPPING`, `422 INVALID_CLAIM`, `413 LIMIT_EXCEEDED`,
`503 KNOWLEDGE_UNAVAILABLE`.

## Claim listing

`GET /v1/sites/{site}/claims?state=candidate|approved|rejected|contested&subject_id=&cursor=` filters
current claim heads by the state of the exact current revision; any other state is 422. Precedence:
an explicit rejection is `rejected`; otherwise a revision that is a side of a recorded conflict with no
standing explicit resolution (none, or the current one withdrawn) is `contested`, even if approved;
otherwise an unrevoked approval is `approved` and anything else (including an approval with a recorded
withdrawal) is `candidate`. `review_state` in each summary is this derived state, not execution
eligibility — read `GET /claims/{id}/review` for `approval_eligible`.

## Verification

`make supervisor-integration CASE=WIS-270` (also wired as `CASE=knowledge`). Cover numeric timestamp
rejection, HTTP enum roundtrip, repeated source replay, correlated-source false corroboration,
changed SOP hash, overlapping and non-overlapping claims, multivalue predicates, cycle detection,
concurrent reviews, source revocation during snapshot creation, cross-site RLS via the non-owner
role, fresh and populated migrations, projection restart and snapshot digest reproducibility.
