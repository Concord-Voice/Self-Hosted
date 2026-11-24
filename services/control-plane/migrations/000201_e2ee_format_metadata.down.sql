-- Serialize the destructive rollback with writers, then refuse to remove
-- metadata once any staged reader row has populated it.
LOCK TABLE messages, dm_messages, media_files IN SHARE ROW EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM messages
        WHERE e2ee_format IS NOT NULL OR request_fingerprint IS NOT NULL
    ) OR EXISTS (
        SELECT 1 FROM dm_messages
        WHERE e2ee_format IS NOT NULL OR request_fingerprint IS NOT NULL
    ) OR EXISTS (
        SELECT 1 FROM media_files
        WHERE e2ee_format IS NOT NULL
    ) THEN
        RAISE EXCEPTION 'refusing to roll back 000201: E2EE metadata is populated';
    END IF;
END
$$;

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
