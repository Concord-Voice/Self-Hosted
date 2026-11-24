-- Validation fails if an administrator has an existing zero generation.
-- Run separately from 000199 so the scan does not hold an exclusive lock.
SET LOCAL lock_timeout = '3s';

ALTER TABLE admin_users
    VALIDATE CONSTRAINT admin_users_session_generation_positive;
