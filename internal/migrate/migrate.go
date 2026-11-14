// Package migrate applies immutable, ordered schema changes with a privileged role.
package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"fmt"
)

//go:embed 001_foundation.sql
var schema string

//go:embed 002_bootstrap.sql
var bootstrapSchema string

//go:embed 003_reauthentication.sql
var reauthenticationSchema string

//go:embed 004_membership.sql
var membershipSchema string

//go:embed 005_evidence.sql
var evidenceSchema string

//go:embed 006_knowledge_candidates.sql
var knowledgeSchema string

//go:embed 007_entity_revisions.sql
var entityRevisionsSchema string

//go:embed 008_source_authority.sql
var sourceAuthoritySchema string

//go:embed 009_knowledge_conflicts.sql
var knowledgeConflictsSchema string

//go:embed 010_claim_reviews.sql
var claimReviewsSchema string

//go:embed 011_review_revocations.sql
var reviewRevocationsSchema string

//go:embed 012_conflict_resolutions.sql
var conflictResolutionsSchema string

//go:embed 013_inventory_revision.sql
var inventoryRevisionSchema string

//go:embed 014_process_snapshots.sql
var processSnapshotsSchema string

//go:embed 015_discovery_inventories.sql
var discoveryInventoriesSchema string

//go:embed 016_requirement_decisions.sql
var requirementDecisionsSchema string

//go:embed 017_discovery_coverage.sql
var discoveryCoverageSchema string

//go:embed 018_document_currentness.sql
var documentCurrentnessSchema string

//go:embed 019_freshness_qualification.sql
var freshnessQualificationSchema string

//go:embed 020_operational_sources.sql
var operationalSourcesSchema string

//go:embed 021_operational_ingestion.sql
var operationalIngestionSchema string

//go:embed 022_operational_progress.sql
var operationalProgressSchema string

//go:embed 023_plan_revisions.sql
var planRevisionsSchema string

//go:embed 024_plant_contexts.sql
var plantContextsSchema string

//go:embed 025_bootstrap_token_lookup.sql
var bootstrapTokenLookupSchema string

//go:embed 028_runtime_least_privilege.sql
var runtimeLeastPrivilegeSchema string

//go:embed 029_oee_service.sql
var oeeServiceSchema string

//go:embed 030_oee_lease_and_key_expiry.sql
var oeeLeaseAndKeyExpirySchema string

//go:embed 031_operational_source_revocation.sql
var operationalSourceRevocationSchema string

//go:embed 032_shift_plans.sql
var shiftPlansSchema string

//go:embed 033_job_orders.sql
var jobOrdersSchema string

//go:embed 034_station_profiles.sql
var stationProfilesSchema string

//go:embed 035_station_records.sql
var stationRecordsSchema string

//go:embed 036_shift_summaries.sql
var shiftSummariesSchema string

//go:embed 037_station_record_correction_audit.sql
var stationRecordCorrectionAuditSchema string

//go:embed 038_shift_handovers.sql
var shiftHandoversSchema string

//go:embed 039_source_independence.sql
var sourceIndependenceSchema string

//go:embed 040_entity_redirects.sql
var entityRedirectsSchema string

//go:embed 041_knowledge_decision_reason_2000.sql
var knowledgeDecisionReasonSchema string

//go:embed 042_import_error_export_audit.sql
var importErrorExportAuditSchema string

//go:embed 043_member_profiles.sql
var memberProfilesSchema string

//go:embed 044_proposal_approvals.sql
var proposalApprovalsSchema string

//go:embed 045_case_triggers.sql
var caseTriggersSchema string

//go:embed 046_case_workflow_events.sql
var caseWorkflowEventsSchema string

//go:embed 047_case_context_evaluations.sql
var caseContextEvaluationsSchema string

//go:embed 049_case_clarifications.sql
var caseClarificationsSchema string

//go:embed 050_approval_lifecycle_events.sql
var approvalLifecycleEventsSchema string

//go:embed 048_interview_sessions.sql
var interviewSessionsSchema string

//go:embed 051_provisioning_audit.sql
var provisioningAuditSchema string

//go:embed 052_site_master_data.sql
var siteMasterDataSchema string

//go:embed 053_worker_tenant_discovery.sql
var workerTenantDiscoverySchema string

//go:embed 054_configuration_imports.sql
var configurationImportsSchema string

//go:embed 055_agent_runtime.sql
var agentRuntimeSchema string

//go:embed 056_agent_run_proposal_link.sql
var agentRunProposalLinkSchema string

//go:embed 057_interview_agent_turns.sql
var interviewAgentTurnsSchema string

//go:embed 058_configuration_drafts.sql
var configurationDraftsSchema string

//go:embed 059_agent_configuration_drafts.sql
var agentConfigurationDraftsSchema string

//go:embed 060_interview_audit_read_index.sql
var interviewAuditReadIndexSchema string

//go:embed 061_discovery_inventory_list.sql
var discoveryInventoryListSchema string

//go:embed 062_operations_chat.sql
var operationsChatSchema string

//go:embed 063_chat_answer_turns.sql
var chatAnswerTurnsSchema string

//go:embed 064_maintenance_roster.sql
var maintenanceRosterSchema string

//go:embed 065_agent_machine_state_refs.sql
var agentMachineStateRefsSchema string

//go:embed 066_approved_task_execution.sql
var approvedTaskExecutionSchema string

//go:embed 067_task_lifecycle.sql
var taskLifecycleSchema string

//go:embed 068_outcome_observations.sql
var outcomeObservationsSchema string

//go:embed 069_outcome_feedback.sql
var outcomeFeedbackSchema string

//go:embed 070_value_stream_vocabulary.sql
var valueStreamVocabularySchema string

//go:embed 071_agent_run_spans.sql
var agentRunSpansSchema string

//go:embed 072_snapshot_capacity.sql
var snapshotCapacitySchema string

//go:embed 073_evidence_span_search.sql
var evidenceSpanSearchSchema string

//go:embed 074_site_model_hosting.sql
var siteModelHostingSchema string

//go:embed 075_span_embeddings.sql
var spanEmbeddingsSchema string

//go:embed 076_document_scans.sql
var documentScansSchema string

//go:embed 077_model_extracted_confirmation.sql
var modelExtractedConfirmationSchema string

//go:embed 078_scan_presented_image.sql
var scanPresentedImageSchema string

//go:embed 079_member_site_lock.sql
var memberSiteLockSchema string

//go:embed 080_shift_runs.sql
var shiftRunsSchema string

//go:embed 081_mixed_document_uploads.sql
var mixedDocumentUploadsSchema string

//go:embed 082_agent_limit_profiles.sql
var agentLimitProfilesSchema string

//go:embed 083_reviewed_corrections.sql
var reviewedCorrectionsSchema string

//go:embed 084_scan_tenant_admission.sql
var scanTenantAdmissionSchema string

// MemberProfileUpsert is the WIS-290 login statement: $1 issuer, $2 subject, $3 display name
// or NULL. With the supervisor.issuer/subject GUCs set to the principal, the self_profile
// policy lets it write that principal's row in each active tenant where it is an active member.
const MemberProfileUpsert = `INSERT INTO supervisor.member_profiles(tenant_id,issuer,subject,display_name)
 SELECT DISTINCT m.tenant_id,m.issuer,m.subject,$3::text FROM supervisor.memberships m
 JOIN supervisor.active_tenant_ids() AS active(tenant_id) ON active.tenant_id=m.tenant_id
 WHERE m.issuer=$1 AND m.subject=$2 AND m.active
 ON CONFLICT(tenant_id,issuer,subject) DO UPDATE SET display_name=excluded.display_name,updated_at=clock_timestamp()`

//go:embed 026_operational_import_jobs.sql
var operationalImportJobsSchema string

//go:embed 085_operational_import_cancellations.sql
var operationalImportCancellationsSchema string

//go:embed 027_exception_cases.sql
var exceptionCasesSchema string

// relaxedForRLS names, per migration, existing forced-RLS tables whose ALTER TABLE adds a
// foreign key or CHECK that scans existing rows: 019 (knowledge_requirement_decisions) and
// 023 (operational_event_records, validated before 023 forces RLS on its own tables) and 039
// (evidence_revisions backfill reading evidence_authority_decisions, group FK to evidence_heads)
// and 042 (operational_source_audit job FK and re-added CHECKs). 042 also relaxes the referenced
// operational_import_jobs: PostgreSQL validates a new FK with one query over both tables, and that
// query fails with 42501 "query would be affected by row-level security policy" on a forced-RLS
// table even when every referencing job_id is NULL (verified by the 042 upgrade test).
var relaxedForRLS = map[int][]string{
	19: {"knowledge_requirement_decisions"},
	23: {"operational_event_records"},
	39: {"evidence_revisions", "evidence_authority_decisions", "evidence_heads"},
	42: {"operational_source_audit", "operational_import_jobs"},
	49: {"case_receipts"},
	50: {"approval_transitions", "approval_audit"},
	57: {"interview_questions", "interview_receipts", "interview_audit", "agent_runs"},
	74: {"agent_runs"},
	76: {"agent_runs", "evidence_revisions", "evidence_audit"},
	77: {"evidence_revisions", "knowledge_inventory_revisions"},
	83: {"feedback", "agent_runs", "agent_artifacts", "knowledge_process_snapshots", "knowledge_receipts", "knowledge_audit"},
	81: {"document_scan_jobs"}, // 081 links scan jobs to document_uploads (WIS-645)
	82: {"agent_runs"},
}

// legacyChecksums are earlier bodies of a migration that a database may already have applied.
// 023 was reordered (WIS-272) so the non-superuser migration role can apply it on a fresh
// database; the resulting schema is identical, and only a superuser could have applied the old
// body. Accepting it keeps those databases migratable without a destructive reset.
var legacyChecksums = map[int]map[string]bool{
	23: {"ed9b438984a2996666c0e78a87b2a8168a82053caddeaedc4726e4fb69265f9e": true},
}

type migration struct {
	version int
	sql     string
}

var migrations = []migration{{1, schema}, {2, bootstrapSchema}, {3, reauthenticationSchema}, {4, membershipSchema}, {5, evidenceSchema}, {6, knowledgeSchema}, {7, entityRevisionsSchema}, {8, sourceAuthoritySchema}, {9, knowledgeConflictsSchema}, {10, claimReviewsSchema}, {11, reviewRevocationsSchema}, {12, conflictResolutionsSchema}, {13, inventoryRevisionSchema}, {14, processSnapshotsSchema}, {15, discoveryInventoriesSchema}, {16, requirementDecisionsSchema}, {17, discoveryCoverageSchema}, {18, documentCurrentnessSchema}, {19, freshnessQualificationSchema}, {20, operationalSourcesSchema}, {21, operationalIngestionSchema}, {22, operationalProgressSchema}, {23, planRevisionsSchema}, {24, plantContextsSchema}, {25, bootstrapTokenLookupSchema}, {26, operationalImportJobsSchema}, {27, exceptionCasesSchema}, {28, runtimeLeastPrivilegeSchema}, {29, oeeServiceSchema}, {30, oeeLeaseAndKeyExpirySchema}, {31, operationalSourceRevocationSchema}, {32, shiftPlansSchema}, {33, jobOrdersSchema}, {34, stationProfilesSchema}, {35, stationRecordsSchema}, {36, shiftSummariesSchema}, {37, stationRecordCorrectionAuditSchema}, {38, shiftHandoversSchema}, {39, sourceIndependenceSchema}, {40, entityRedirectsSchema}, {41, knowledgeDecisionReasonSchema}, {42, importErrorExportAuditSchema}, {43, memberProfilesSchema}, {44, proposalApprovalsSchema}, {45, caseTriggersSchema}, {46, caseWorkflowEventsSchema}, {47, caseContextEvaluationsSchema}, {48, interviewSessionsSchema}, {49, caseClarificationsSchema}, {50, approvalLifecycleEventsSchema}, {51, provisioningAuditSchema}, {52, siteMasterDataSchema}, {53, workerTenantDiscoverySchema}, {54, configurationImportsSchema}, {55, agentRuntimeSchema}, {56, agentRunProposalLinkSchema}, {57, interviewAgentTurnsSchema}, {58, configurationDraftsSchema}, {59, agentConfigurationDraftsSchema}, {60, interviewAuditReadIndexSchema}, {61, discoveryInventoryListSchema}, {62, operationsChatSchema}, {63, chatAnswerTurnsSchema}, {64, maintenanceRosterSchema}, {65, agentMachineStateRefsSchema}, {66, approvedTaskExecutionSchema}, {67, taskLifecycleSchema}, {68, outcomeObservationsSchema}, {69, outcomeFeedbackSchema}, {70, valueStreamVocabularySchema}, {71, agentRunSpansSchema}, {72, snapshotCapacitySchema}, {73, evidenceSpanSearchSchema}, {74, siteModelHostingSchema}, {75, spanEmbeddingsSchema}, {76, documentScansSchema}, {77, modelExtractedConfirmationSchema}, {78, scanPresentedImageSchema}, {79, memberSiteLockSchema}, {80, shiftRunsSchema}, {81, mixedDocumentUploadsSchema}, {82, agentLimitProfilesSchema}, {83, reviewedCorrectionsSchema}, {84, scanTenantAdmissionSchema}, {85, operationalImportCancellationsSchema}}

func checksum(value string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(value))) }
func applied(ctx context.Context, query interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) (int, error) {
	rows, err := query.QueryContext(ctx, "SELECT version,sha256 FROM public.supervisor_schema_versions ORDER BY version")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var version int
		var hash string
		if err = rows.Scan(&version, &hash); err != nil {
			return 0, err
		}
		if count >= len(migrations) || version != migrations[count].version || (hash != checksum(migrations[count].sql) && !legacyChecksums[version][hash]) {
			return 0, fmt.Errorf("applied migration checksum or sequence mismatch")
		}
		count++
	}
	return count, rows.Err()
}

// Apply brings the schema to the latest migration.
func Apply(ctx context.Context, db *sql.DB) error {
	return applyThrough(ctx, db, migrations[len(migrations)-1].version)
}

// applyThrough applies pending migrations up to and including version through. Tests use it to
// build a database at an older version and then upgrade it.
func applyThrough(ctx context.Context, db *sql.DB, through int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(290001)"); err != nil {
		return err
	}
	// Every migration runs with row_security off, so an owner scan of a forced-RLS table fails
	// loudly instead of silently seeing no rows. 001 (a pg_dump body) also sets it, but only a
	// fresh install runs 001; setting it here makes fresh installs and upgrades behave alike.
	if _, err = tx.ExecContext(ctx, "SET LOCAL row_security = off"); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS public.supervisor_schema_versions(version integer PRIMARY KEY,sha256 text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now()); REVOKE ALL ON public.supervisor_schema_versions FROM PUBLIC; GRANT SELECT ON public.supervisor_schema_versions TO supervisor_runtime`); err != nil {
		return err
	}
	count, err := applied(ctx, tx)
	if err != nil {
		return err
	}
	for _, change := range migrations[count:] {
		if change.version > through {
			break
		}
		// Forced RLS blocks the owner's row scans that ADD CONSTRAINT runs to validate
		// existing rows. Relax those tables for this migration only. ALTER TABLE holds its
		// lock to transaction end, so FORCE is restored before commit.
		tables := relaxedForRLS[change.version]
		for _, table := range tables {
			if _, err = tx.ExecContext(ctx, "ALTER TABLE supervisor."+table+" NO FORCE ROW LEVEL SECURITY"); err != nil {
				return fmt.Errorf("prepare schema migration %d: %w", change.version, err)
			}
		}
		if _, err = tx.ExecContext(ctx, change.sql); err != nil {
			return fmt.Errorf("apply schema migration %d: %w", change.version, err)
		}
		for _, table := range tables {
			if _, err = tx.ExecContext(ctx, "ALTER TABLE supervisor."+table+" FORCE ROW LEVEL SECURITY"); err != nil {
				return fmt.Errorf("finalize schema migration %d: %w", change.version, err)
			}
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO public.supervisor_schema_versions(version,sha256) VALUES($1,$2)", change.version, checksum(change.sql)); err != nil {
			return err
		}
	}
	// 001 issues session-level SETs (search_path, row_security, timeouts). Reset them before
	// commit so they do not outlive this transaction on a pooled connection.
	if _, err = tx.ExecContext(ctx, "RESET ALL"); err != nil {
		return err
	}
	return tx.Commit()
}

// Check rejects old, changed, missing, gapped or unknown migration histories.
func Check(ctx context.Context, db *sql.DB) error {
	count, err := applied(ctx, db)
	if err != nil {
		return err
	}
	if count != len(migrations) {
		return fmt.Errorf("schema upgrade required")
	}
	rows, err := db.QueryContext(ctx, `SELECT s.token_hash,s.auth_time,m.revision,kir.revision,kps.canonical_contents,ko.event_id,kdr.template_version,kdq.requirement_id,krqd.claim_count,krqc.snapshot_id,ecd.verified_at FROM supervisor.browser_sessions s JOIN supervisor.commands c ON false JOIN supervisor.outbox o ON false JOIN supervisor.memberships m ON false JOIN supervisor.membership_changes a ON false JOIN supervisor.evidence_heads e ON false JOIN supervisor.evidence_revisions r ON false JOIN supervisor.evidence_spans p ON false JOIN supervisor.evidence_revocations v ON false JOIN supervisor.evidence_receipts i ON false JOIN supervisor.evidence_audit q ON false JOIN supervisor.knowledge_entities ke ON false JOIN supervisor.knowledge_claim_heads kh ON false JOIN supervisor.knowledge_claim_revisions kr ON false JOIN supervisor.knowledge_claim_spans ks ON false JOIN supervisor.knowledge_receipts ki ON false JOIN supervisor.knowledge_audit ka ON false JOIN supervisor.knowledge_conflicts kc ON false JOIN supervisor.knowledge_claim_reviews kcr ON false JOIN supervisor.knowledge_review_revocations krv ON false JOIN supervisor.knowledge_resolution_heads krh ON false JOIN supervisor.knowledge_resolution_decisions krd ON false JOIN supervisor.knowledge_review_resolution_refs krr ON false JOIN supervisor.knowledge_inventory_revisions kir ON false JOIN supervisor.knowledge_process_snapshots kps ON false JOIN supervisor.knowledge_snapshot_claims ksc ON false JOIN supervisor.knowledge_snapshot_entities kse ON false JOIN supervisor.knowledge_outbox ko ON false JOIN supervisor.knowledge_discovery_inventories kdi ON false JOIN supervisor.knowledge_discovery_heads kdh ON false JOIN supervisor.knowledge_discovery_revisions kdr ON false JOIN supervisor.knowledge_discovery_requirements kdq ON false JOIN supervisor.knowledge_requirement_decision_heads krqh ON false JOIN supervisor.knowledge_requirement_decisions krqd ON false JOIN supervisor.knowledge_requirement_decision_claims krqc ON false JOIN supervisor.evidence_currentness_heads ech ON false JOIN supervisor.evidence_currentness_decisions ecd ON false LIMIT 0`)
	if err != nil {
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	rows, err = db.QueryContext(ctx, `SELECT s.role,q.reason,a.client_answer_id,r.operation,u.event_type
 FROM supervisor.interview_sessions s
 JOIN supervisor.interview_questions q ON false
 JOIN supervisor.interview_answers a ON false
 JOIN supervisor.interview_receipts r ON false
 JOIN supervisor.interview_audit u ON false LIMIT 0`)
	if err != nil {
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	rows, err = db.QueryContext(ctx, `SELECT h.current_revision,m.revision,m.mapping_sha256,x.external_type,x.entity_revision,r.config_sha256,v.verdict,i.operation,a.event_type
 FROM supervisor.operational_source_heads h
 JOIN supervisor.operational_source_mapping_heads mh ON false
 JOIN supervisor.operational_source_mapping_revisions m ON false
 JOIN supervisor.operational_source_mappings x ON false
 JOIN supervisor.operational_source_revisions r ON false
 JOIN supervisor.operational_source_review_heads rh ON false
 JOIN supervisor.operational_source_reviews v ON false
 JOIN supervisor.operational_source_receipts i ON false
 JOIN supervisor.operational_source_audit a ON false LIMIT 0`)
	if err != nil {
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	rows, err = db.QueryContext(ctx, `SELECT h.current_revision,r.revision,p.subject_revision,p.object_revision FROM supervisor.knowledge_entity_heads h JOIN supervisor.knowledge_entity_revisions r ON false JOIN supervisor.knowledge_claim_entity_refs p ON false LIMIT 0`)
	if err != nil {
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	var bootstrapReady bool
	rows, err = db.QueryContext(ctx, `SELECT h.current_revision,d.source_revision,d.source_sha256 FROM supervisor.evidence_authority_heads h JOIN supervisor.evidence_authority_decisions d ON false LIMIT 0`)
	if err != nil {
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	err = db.QueryRowContext(ctx, `SELECT to_regclass('supervisor.setup_invitations') IS NOT NULL AND to_regclass('supervisor.bootstrap_audit') IS NOT NULL AND to_regprocedure('supervisor.redeem_setup_invitation(text,uuid,uuid)') IS NOT NULL AND to_regprocedure('supervisor.change_membership(text,uuid,uuid,text,text,text,text,text,boolean,bigint,text)') IS NOT NULL AND to_regprocedure('supervisor.lock_evidence_site(uuid,uuid,boolean)') IS NOT NULL AND to_regprocedure('supervisor.advance_knowledge_inventory()') IS NOT NULL`).Scan(&bootstrapReady)
	if err != nil {
		return err
	}
	if !bootstrapReady {
		return fmt.Errorf("bootstrap schema unavailable")
	}
	return nil
}
