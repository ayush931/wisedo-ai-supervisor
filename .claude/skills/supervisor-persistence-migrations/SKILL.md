---
name: supervisor-persistence-migrations
description: PostgreSQL schema, versioned SQL migrations (001 onward, 071 on main as of 2026-11-03), forced RLS, the transactional outbox, the global lock order and backup/restore semantics across internal/migrate and internal/store. Use this skill whenever adding or changing a migration, a table, a unique constraint, an outbox relay, a transaction boundary or anything about restore - and before writing any SQL in this repository.
---

# Persistence, migrations and durability

Covers `internal/migrate/` and `internal/store/`.
Sources: WIS-290 "Durable workflow and persistence contract", WIS-229, `SECURITY.md`,
`deploy/supervisor/README.md`, `docs/oee-foundation.md`.
Governing rules: `.claude/rules/03-tenant-isolation-and-rls.md`, `.claude/rules/04-deterministic-arithmetic-and-digests.md`.

## Migration inventory

`001_foundation` · `002_bootstrap` · `003_reauthentication` · `004_membership` · `005_evidence` ·
`006_knowledge_candidates` · `007_entity_revisions` · `008_source_authority` ·
`009_knowledge_conflicts` · `010_claim_reviews` · `011_review_revocations` ·
`012_conflict_resolutions` · `013_inventory_revision` · `014_process_snapshots` ·
`015_discovery_inventories` · `016_requirement_decisions` · `017_discovery_coverage` ·
`018_document_currentness` · `019_freshness_qualification` · `020_operational_sources` ·
`021_operational_ingestion` · `022_operational_progress` · `023_plan_revisions` ·
`024_plant_contexts` · `025_bootstrap_token_lookup` · `026_operational_import_jobs` ·
`027_exception_cases` · `028_runtime_least_privilege` · `029_oee_service` ·
`030_oee_lease_and_key_expiry` · `031_operational_source_revocation` · `032_shift_plans` ·
`033_job_orders` · `034_station_profiles` · `035_station_records` · `036_shift_summaries` ·
`037_station_record_correction_audit` · `038_shift_handovers` · `039_source_independence` ·
`040_entity_redirects` · `041_knowledge_decision_reason_2000` · `042_import_error_export_audit` ·
`043_member_profiles` · `044_proposal_approvals` · `045_case_triggers` · `046_case_workflow_events` ·
`047_case_context_evaluations` · `048_interview_sessions` · `049_case_clarifications` ·
`050_approval_lifecycle_events` · `051_provisioning_audit` · `052_site_master_data` ·
`053_worker_tenant_discovery` · `054_configuration_imports` · `055_agent_runtime` ·
`056_agent_run_proposal_link` · `057_interview_agent_turns` · `058_configuration_drafts` ·
`059_agent_configuration_drafts` · `060_interview_audit_read_index` · `061_discovery_inventory_list` ·
`062_operations_chat` · `063_chat_answer_turns` · `064_maintenance_roster` ·
`065_agent_machine_state_refs` · `066_approved_task_execution` · `067_task_lifecycle` ·
`068_outcome_observations` · `069_outcome_feedback` · `070_value_stream_vocabulary` ·
`071_agent_run_spans` · `072_snapshot_capacity` · `073_evidence_span_search` ·
`074_site_model_hosting` · `075_span_embeddings` · `076_document_scans` ·
`077_model_extracted_confirmation` · `078_scan_presented_image` · `079_member_site_lock` · `080_shift_runs`. This list was
checked against `internal/migrate/` on 2026-11-10; the
registered list in `migrate.go` is authoritative.

Each migration is embedded and registered in order in `internal/migrate/migrate.go`; a new one
also raises the forced-RLS table count in `TestMigrationReadbackAndRepeat` (198 after 080) and
should land with an upgrade test from the previous migration on a populated database (for example
`044_proposal_approvals_integration_test.go`). A migration that adds a foreign key or CHECK to an
existing forced-RLS table must list that table in `relaxedForRLS`. Fixture rows behind the whole
ingestion pipeline (plant contexts, cases) are planted by the superuser with
`session_replication_role = replica`; everything under test then runs as the runtime role.

Migrations are checksum-verified, additive and expand/contract. They run only under the separate
privileged migration role; the runtime role is never a table owner and never has `BYPASSRLS`.
Rollback disables new writes — it does not drop claims or history, and it never claims to reverse a
destructive schema migration.

## Table requirements

Every tenant table runs `FORCE ROW LEVEL SECURITY`. Every tenant-owned foreign key includes
`tenant_id` so a cross-tenant reference is unrepresentable. Immutable tables deny UPDATE and DELETE
to the runtime role; mutable status and index rows are derived from events, not authority. Tenant
context is set transaction-locally and must not leak across pooled connections.

## Transactional contract

A transaction commits business state **plus** audit **plus** outbox before returning success. A
relay then starts or signals deterministic Temporal workflow IDs and retries safely, backed by a
unique `(tenant_id, operation_kind, idempotency_key)` constraint. The outbox is delivery plumbing,
never a second workflow scheduler. Workflow state carries IDs, revisions and minimised metadata —
never raw documents or credentials.

Retries do not imply exactly-once effects: task effects use database idempotency constraints, and
uncertain external effects enter reconciliation rather than a blind resend. Worker crash before or
after commit, a lost workflow-start acknowledgement and duplicate signals must all converge on one
business operation.

## Global lock order

All authority writers (membership, sources, mapping, policy/mode, approval/revocation, release
activation) and the executor use the same order:
**scope_policy → prerequisite membership/source/version rows sorted by stable ID → case →
proposal/approval → action ledger.** Prove with two-connection races that revocation-first prevents
dispatch, and that no deadlock results.

## Retention and deletion

Candidate defaults pending commissioning: raw telemetry 30 days; normalized facts, source documents
and decision evidence 90 days; business audit 365 days; debug logs 14 days; backups 35 days. The
retention purge worker has separately restricted permissions — a runtime DELETE denial on an
immutable table must not prevent authorized erasure. Pinning cannot silently extend approved
retention. Deletion leaves a tombstone that survives restore so expired or revoked data is never
resurrected into retrieval.

## Backup and restore

Targets to be measured, not assumed: PostgreSQL RPO ≤ 15 minutes, RTO ≤ 4 hours to read-only verified
service. Start a restore in actions-disabled mode; rebuild derived search, apply deletion and
revocation tombstones, reconcile task effect IDs, reauthenticate users and invalidate or review
pending approvals before enabling actions. Never replay all outbox rows blindly. Temporal history and
application snapshots need a documented reconciliation watermark — independent timestamps are not a
transactionally consistent backup. No destructive command defaults to production.

## Verification

`make supervisor-migrate`, then `make supervisor-integration CASE=migration` (fresh and populated),
`CASE=isolation`, `CASE=worker-crash`, plus `make supervisor-restore`.
