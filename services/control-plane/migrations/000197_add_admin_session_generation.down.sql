-- Removing a generation after revocation would make stale sessions valid.
-- Every administrator starts at generation 1 and every reset advances it in
-- the same transaction that revokes, so a value above 1 is exactly a recorded
-- revocation. Take the write-blocking lock before checking.
SET LOCAL lock_timeout = '3s';

LOCK TABLE admin_users IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM admin_users WHERE session_generation > 1) THEN
        RAISE EXCEPTION
            'cannot roll back 000197 after an administrator session generation has advanced'
            USING ERRCODE = 'check_violation';
    END IF;
END
$$;

ALTER TABLE admin_users
    DROP CONSTRAINT admin_users_session_generation_nonnegative,
    DROP COLUMN session_generation;
