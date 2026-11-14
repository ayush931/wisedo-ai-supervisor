-- WIS-270 Decision contract: review, revocation and conflict-resolution reasons are
-- 1..2000 characters (length() counts characters, matching Go's rune count). Each CHECK keeps
-- its original shape and is re-added validating every existing row; the other reason columns
-- (evidence, authority, currentness, freshness, discovery, requirement, snapshot, operational,
-- plan) keep their 1000-character limits.
ALTER TABLE supervisor.knowledge_claim_reviews DROP CONSTRAINT knowledge_claim_reviews_reason_check,
 ADD CONSTRAINT knowledge_claim_reviews_reason_check CHECK(length(reason) BETWEEN 1 AND 2000 AND length(btrim(reason))>0);
ALTER TABLE supervisor.knowledge_review_revocations DROP CONSTRAINT knowledge_review_revocations_reason_check,
 ADD CONSTRAINT knowledge_review_revocations_reason_check CHECK(length(reason) BETWEEN 1 AND 2000 AND length(btrim(reason))>0);
ALTER TABLE supervisor.knowledge_resolution_decisions DROP CONSTRAINT knowledge_resolution_decisions_reason_check,
 ADD CONSTRAINT knowledge_resolution_decisions_reason_check CHECK(length(reason) BETWEEN 1 AND 2000 AND length(btrim(reason))>0);
