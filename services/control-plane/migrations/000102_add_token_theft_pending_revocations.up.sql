CREATE TABLE token_theft_pending_revocations (
    user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE token_theft_pending_revocations IS
    'Durable recovery records for refresh-token sweeps interrupted after token-theft detection.';
COMMENT ON COLUMN token_theft_pending_revocations.user_id IS
    'User whose token-theft refresh-token sweep must complete before the Redis deny marker may clear.';
