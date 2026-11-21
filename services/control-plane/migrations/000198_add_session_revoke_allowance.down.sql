-- Old binaries cannot enforce timestamps written only in PostgreSQL.
-- Block new writes before checking that every recorded window has expired.
SET LOCAL lock_timeout = '3s';

LOCK TABLE users IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM users
        WHERE last_session_revoke_at > clock_timestamp() - INTERVAL '24 hours'
    ) THEN
        RAISE EXCEPTION 'cannot roll back 000198 while session revocation windows remain active'
            USING ERRCODE = 'check_violation';
    END IF;
END
$$;

ALTER TABLE users
    DROP COLUMN last_session_revoke_at,
    DROP COLUMN session_revoke_auth_gen;
