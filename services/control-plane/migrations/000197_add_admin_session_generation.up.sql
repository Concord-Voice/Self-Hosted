-- Bind platform-admin sessions to durable administrator authority.
-- Reset enrollment advances this generation in the same transaction that
-- removes credentials, so old sessions cannot regain authority after
-- re-enrollment.
--
-- Every administrator, existing or new, starts at generation 1, so any value
-- above 1 records a revocation. Sessions minted before this migration carry no
-- generation and decode as 0, so they stop matching and every existing
-- administrator signs in once again. That also covers a reset run by an
-- earlier release, which left sessions alive.
SET LOCAL lock_timeout = '3s';

ALTER TABLE admin_users
    ADD COLUMN session_generation BIGINT NOT NULL DEFAULT 1,
    ADD CONSTRAINT admin_users_session_generation_nonnegative
        CHECK (session_generation >= 0) NOT VALID;

COMMENT ON COLUMN admin_users.session_generation IS
    'Durable generation that every active platform-admin session must match.';
