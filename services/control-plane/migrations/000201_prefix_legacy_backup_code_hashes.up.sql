-- Mark every legacy backup-code hash with its format version.
-- A bare lowercase 64-hex element is an unsalted SHA-256 digest; it becomes
-- 'v1$' || digest, the same digest under an explicit prefix. Array order and
-- the parallel backup_codes_used flags are unchanged, so every index keeps its
-- code and its used state. Elements in any other form (v1, v2, or a value no
-- reader accepts) are left exactly as they are. No plaintext code exists here,
-- so nothing is hashed and nothing is logged.
--
-- Rollout: every serving replica must already read bare, v1 and v2 hashes.
-- A reader from before that release rejects v1 and would refuse these codes.
-- The guarded down migration is for a schema rollback only; rolling the
-- application back to the compatible reader needs no down migration.
SET LOCAL lock_timeout = '3s';

UPDATE user_mfa_totp AS t
SET backup_codes_hash = ARRAY(
    SELECT CASE WHEN e.hash ~ '^[0-9a-f]{64}$' THEN 'v1$' || e.hash ELSE e.hash END
    FROM unnest(t.backup_codes_hash) WITH ORDINALITY AS e(hash, ord)
    ORDER BY e.ord
)
WHERE EXISTS (
    SELECT 1 FROM unnest(t.backup_codes_hash) AS e(hash)
    WHERE e.hash ~ '^[0-9a-f]{64}$'
);
