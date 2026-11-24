-- Restore the NOT VALID metadata state from 000199 for an exact rollback.
SET LOCAL lock_timeout = '3s';

ALTER TABLE admin_users
    DROP CONSTRAINT admin_users_session_generation_positive,
    ADD CONSTRAINT admin_users_session_generation_positive
        CHECK (session_generation > 0) NOT VALID;
