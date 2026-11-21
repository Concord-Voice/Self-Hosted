-- Legacy admin sessions decode a missing generation as 0. This check rejects
-- that value on every future write, including installations that ran 000197.
SET LOCAL lock_timeout = '3s';

ALTER TABLE admin_users
    ADD CONSTRAINT admin_users_session_generation_positive
        CHECK (session_generation > 0) NOT VALID;
