-- Removing a generation after revocation would make stale sessions valid.
SET LOCAL lock_timeout = '3s';

LOCK TABLE admin_users IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM admin_users WHERE session_generation <> 0) THEN
        RAISE EXCEPTION
            'cannot roll back 000197 after an administrator session generation has advanced'
            USING ERRCODE = 'check_violation';
    END IF;
END
$$;

ALTER TABLE admin_users
    DROP CONSTRAINT admin_users_session_generation_nonnegative,
    DROP COLUMN session_generation;
