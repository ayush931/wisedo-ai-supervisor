-- WIS-621: an approved process snapshot holds the documented 200 entities and 500 claims. The
-- 64 KiB body bound from 014 was the graph-read text budget and refused snapshots of about 45
-- claims. At the documented bounds a canonical body measures about 680 KiB typically and about
-- 4.1 MiB with every field at its contract maximum, so the stored bound is 8 MiB
-- (knowledge.MaxSnapshotBytes). Only the bound widens; every other check is unchanged and existing
-- rows already satisfy it.
ALTER TABLE supervisor.knowledge_process_snapshots DROP CONSTRAINT knowledge_process_snapshots_canonical_contents_check;
ALTER TABLE supervisor.knowledge_process_snapshots ADD CONSTRAINT knowledge_process_snapshots_canonical_contents_check
 CHECK(octet_length(canonical_contents)<=8388608 AND jsonb_typeof(canonical_contents::jsonb)='object');
