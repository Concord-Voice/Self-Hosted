-- Legacy admin sessions decode a missing generation as 0. This check rejects
-- that value on every future write, including installations that ran 000197.
SET LOCAL lock_timeout = '3s';

-- Refuse a partially upgraded installation that already has a zero row. The
-- write-blocking lock keeps a zero writer from racing the check and ADD.
LOCK TABLE admin_users IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM admin_users WHERE session_generation <= 0) THEN
        RAISE EXCEPTION
            'cannot install positive admin session generation check while a nonpositive row exists'
            USING ERRCODE = 'check_violation';
    END IF;
END
$$;

ALTER TABLE admin_users
    ADD CONSTRAINT admin_users_session_generation_positive
        CHECK (session_generation > 0) NOT VALID;
