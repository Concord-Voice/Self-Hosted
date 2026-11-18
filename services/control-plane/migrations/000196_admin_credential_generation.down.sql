-- Refuse rollback after a reset advanced the enrollment fence.
SET LOCAL lock_timeout = '3s';

LOCK TABLE admin_users IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM admin_users WHERE credential_generation <> 1) THEN
        RAISE EXCEPTION 'cannot remove credential generation after an enrollment reset';
    END IF;
END
$$;

ALTER TABLE admin_users
    DROP CONSTRAINT admin_users_credential_generation_positive,
    DROP COLUMN credential_generation;
