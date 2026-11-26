-- Strip the v1$ prefix that 000207 added, back to bare SHA-256 digests.
-- A reader from before the versioned formats accepts only bare digests, so
-- this rollback is safe only while every stored element is bare or v1. A v2
-- element is a salted Argon2id hash: it has no bare form, and dropping or
-- keeping it would lock its owner out of backup-code sign-in under that
-- reader. Any other value has no bare form either. Either one refuses the
-- rollback.
--
-- Take the lock before checking. SHARE ROW EXCLUSIVE blocks every concurrent
-- insert, update and delete on the table (and a second copy of this
-- migration) until commit, so no v2 code can be written between the check
-- and the rewrite, while sign-in reads keep running.
SET LOCAL lock_timeout = '3s';

LOCK TABLE user_mfa_totp IN SHARE ROW EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM user_mfa_totp AS t, unnest(t.backup_codes_hash) AS e(hash)
        WHERE e.hash IS NULL
           OR NOT (e.hash ~ '^[0-9a-f]{64}$'
                   OR (left(e.hash, 3) = 'v1$' AND substr(e.hash, 4) ~ '^[0-9a-f]{64}$'))
    ) THEN
        RAISE EXCEPTION
            'cannot roll back 000207 while a backup-code hash is not a bare or v1 SHA-256 digest, such as a generated v2 code'
            USING ERRCODE = 'check_violation';
    END IF;
END
$$;

UPDATE user_mfa_totp AS t
SET backup_codes_hash = ARRAY(
    SELECT CASE WHEN left(e.hash, 3) = 'v1$' THEN substr(e.hash, 4) ELSE e.hash END
    FROM unnest(t.backup_codes_hash) WITH ORDINALITY AS e(hash, ord)
    ORDER BY e.ord
)
WHERE EXISTS (
    SELECT 1 FROM unnest(t.backup_codes_hash) AS e(hash)
    WHERE left(e.hash, 3) = 'v1$'
);
