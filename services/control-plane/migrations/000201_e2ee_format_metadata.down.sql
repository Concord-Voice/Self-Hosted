ALTER TABLE messages
    DROP CONSTRAINT IF EXISTS messages_e2ee_format_check,
    DROP COLUMN IF EXISTS request_fingerprint,
    DROP COLUMN IF EXISTS e2ee_format;

ALTER TABLE dm_messages
    DROP CONSTRAINT IF EXISTS dm_messages_e2ee_format_check,
    DROP COLUMN IF EXISTS request_fingerprint,
    DROP COLUMN IF EXISTS e2ee_format;

ALTER TABLE media_files
    DROP CONSTRAINT IF EXISTS media_files_e2ee_format_check,
    DROP COLUMN IF EXISTS e2ee_format;
