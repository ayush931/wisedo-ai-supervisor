-- WIS-520 supervisor correction audit. When an owner or supervisor corrects a record that
-- another member recorded, the correction is audited as station_record.corrected_by_supervisor
-- with the corrected record's recorder, so overriding an operator's entry stays attributable.
-- The event_type check is widened additively (drop and re-add under the same name); existing
-- rows keep satisfying it.
ALTER TABLE supervisor.station_record_audit
 ADD COLUMN predecessor_actor_issuer text CHECK(length(predecessor_actor_issuer) BETWEEN 1 AND 255),
 ADD COLUMN predecessor_actor_subject text CHECK(length(predecessor_actor_subject) BETWEEN 1 AND 255),
 DROP CONSTRAINT station_record_audit_event_type_check,
 ADD CONSTRAINT station_record_audit_event_type_check CHECK(event_type IN ('station_record.submitted','station_record.read','station_record.listed','station_record.corrected_by_supervisor',
  'station_authorization.granted','station_authorization.revoked','station_authorization.listed','mutation.replayed')),
 -- The recorder is named exactly on a supervisor correction, which always names its record.
 ADD CONSTRAINT station_record_audit_predecessor_check CHECK((event_type='station_record.corrected_by_supervisor')=(predecessor_actor_issuer IS NOT NULL)
  AND (predecessor_actor_issuer IS NULL)=(predecessor_actor_subject IS NULL) AND (event_type<>'station_record.corrected_by_supervisor' OR record_id IS NOT NULL));
