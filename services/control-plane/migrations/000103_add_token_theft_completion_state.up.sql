ALTER TABLE token_theft_pending_revocations
    ADD COLUMN completed_at TIMESTAMPTZ;

COMMENT ON COLUMN token_theft_pending_revocations.completed_at IS
    'Refresh-token sweep committed; the row remains only until Redis deny-state cleanup succeeds.';
