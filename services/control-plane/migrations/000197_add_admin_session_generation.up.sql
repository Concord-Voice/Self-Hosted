-- Bind platform-admin sessions to durable administrator authority.
-- Reset enrollment advances this generation with credential removal so old
-- sessions cannot regain authority after re-enrollment.
SET LOCAL lock_timeout = '3s';

ALTER TABLE admin_users
    ADD COLUMN session_generation BIGINT NOT NULL DEFAULT 0,
    ADD CONSTRAINT admin_users_session_generation_nonnegative
        CHECK (session_generation >= 0) NOT VALID;

COMMENT ON COLUMN admin_users.session_generation IS
    'Durable generation that every active platform-admin session must match.';
