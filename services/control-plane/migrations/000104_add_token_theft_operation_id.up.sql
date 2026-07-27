ALTER TABLE token_theft_pending_revocations
    ADD COLUMN operation_id UUID;

COMMENT ON COLUMN token_theft_pending_revocations.operation_id IS
    'Unique theft event identifier; NULL identifies records created before operation ownership was introduced.';
