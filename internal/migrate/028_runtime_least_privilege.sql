-- WIS-290 runtime least privilege (rule 03). guard_membership_write() runs as the invoking
-- role and resolved pg_class through the caller's search_path. PostgreSQL searches pg_temp
-- first for relation names, so a runtime TEMP table named pg_class could shadow the catalog,
-- report runtime as the table owner and let a direct UPDATE of supervisor.memberships pass
-- (for example viewer -> owner). Pin the search path and qualify every catalog reference.
CREATE OR REPLACE FUNCTION supervisor.guard_membership_write() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$ BEGIN
        IF NOT pg_catalog.pg_has_role(current_user,
            (SELECT c.relowner FROM pg_catalog.pg_class c
              WHERE c.oid = 'supervisor.memberships'::pg_catalog.regclass),
            'MEMBER') THEN
            RAISE EXCEPTION 'Membership provisioning requires administrator' USING ERRCODE='42501';
        END IF;
        RETURN NEW;
        END $$;

-- The application never needs temporary objects. Without TEMP, no role but the owner can
-- create objects that shadow names in other sessions' functions.
DO $$ BEGIN
    EXECUTE pg_catalog.format('REVOKE TEMPORARY ON DATABASE %I FROM PUBLIC', pg_catalog.current_database());
END $$;
