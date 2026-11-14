-- WIS-274 approval lifecycle events. The approval state machine has approved -> consumed and
-- approved -> stale (internal/approvals transitions), but 044 only allowed the events up to
-- revoke, so recording a consumption (WIS-277) or an invalidation failed the CHECK. A canceled
-- case now marks its approved proposals stale, audited as proposal.stale.
ALTER TABLE supervisor.approval_transitions DROP CONSTRAINT approval_transitions_event_check;
ALTER TABLE supervisor.approval_transitions ADD CONSTRAINT approval_transitions_event_check
 CHECK(event IN ('submit','approve','reject','expire','supersede','revoke','consume','mark_stale'));
ALTER TABLE supervisor.approval_audit DROP CONSTRAINT approval_audit_event_type_check;
ALTER TABLE supervisor.approval_audit ADD CONSTRAINT approval_audit_event_type_check
 CHECK(event_type IN ('proposal.submitted','proposal.read','proposal.decided','proposal.revoked','proposal.expired','proposal.stale','mutation.replayed'));
