-- Add dormant E2EE format/provenance metadata for the S1 reader rollout.
-- All columns remain nullable and no writer is enabled by this migration.

ALTER TABLE messages
    ADD COLUMN e2ee_format TEXT,
    ADD COLUMN request_fingerprint BYTEA;

ALTER TABLE dm_messages
    ADD COLUMN e2ee_format TEXT,
    ADD COLUMN request_fingerprint BYTEA;

ALTER TABLE media_files
    ADD COLUMN e2ee_format TEXT;

ALTER TABLE messages
    ADD CONSTRAINT messages_e2ee_format_check
        CHECK ((e2ee_format IS NULL AND request_fingerprint IS NULL)
            OR (type = 'user'
                AND e2ee_format = 'message-v1'
                AND request_fingerprint IS NOT NULL
                AND octet_length(request_fingerprint) = 32)) NOT VALID;

ALTER TABLE dm_messages
    ADD CONSTRAINT dm_messages_e2ee_format_check
        CHECK (
            (e2ee_format IS NULL AND request_fingerprint IS NULL)
            OR (
                type = 'user'
                AND e2ee_format = 'message-v1'
                AND request_fingerprint IS NOT NULL
                AND octet_length(request_fingerprint) = 32
            )
        ) NOT VALID;

ALTER TABLE media_files
    ADD CONSTRAINT media_files_e2ee_format_check
        CHECK (e2ee_format IS NULL
            OR (media_tier = 2 AND e2ee_format = 'attachment-v4')) NOT VALID;

COMMENT ON COLUMN messages.e2ee_format IS
    'Stored E2EE envelope format; NULL preserves the legacy reader family.';
COMMENT ON COLUMN messages.request_fingerprint IS
    'SHA-256 fingerprint for a message-v1 create request; dormant until S1 writers activate.';
COMMENT ON COLUMN dm_messages.e2ee_format IS
    'Stored E2EE envelope format; NULL preserves the legacy reader family.';
COMMENT ON COLUMN dm_messages.request_fingerprint IS
    'SHA-256 fingerprint for a message-v1 create request; dormant until S1 writers activate.';
COMMENT ON COLUMN media_files.e2ee_format IS
    'Stored E2EE attachment format; NULL preserves the legacy reader family.';
