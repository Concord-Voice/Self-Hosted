-- Drain all old control-plane writers before this migration. Existing users
-- start a conservative 24-hour window; subsequent inserts default to NULL.
SET LOCAL lock_timeout = '3s';

ALTER TABLE users
    ADD COLUMN last_session_revoke_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    ADD COLUMN session_revoke_auth_gen BIGINT NOT NULL DEFAULT 0;

-- No client can observe the temporary default before this transaction commits.
-- squawk-ignore ban-drop-default
ALTER TABLE users ALTER COLUMN last_session_revoke_at DROP DEFAULT;
