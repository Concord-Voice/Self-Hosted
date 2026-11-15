-- Bind administrator enrollment ceremonies to the credential-revocation epoch.
-- Resetting enrollment increments this value with credential removal, so a
-- ceremony issued before reset cannot reactivate the administrator.
SET LOCAL lock_timeout = '3s';

ALTER TABLE admin_users
    ADD COLUMN credential_generation BIGINT NOT NULL DEFAULT 1,
    ADD CONSTRAINT admin_users_credential_generation_positive
        CHECK (credential_generation > 0) NOT VALID;
