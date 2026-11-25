SET LOCAL lock_timeout = '3s';

ALTER TABLE admin_users
    DROP CONSTRAINT admin_users_session_generation_positive;
