SET LOCAL lock_timeout = '3s';

-- The admission and socket a Server Voice join consumed (#253). A redelivered
-- voice.joined whose reservation is gone is a replay when the row already holds
-- its exact admission and socket, even when two control planes clamped its
-- stamp to different receipt times. Nullable: heartbeat inserts and rows from
-- before this migration carry neither, and those keep the fail-closed eject.
ALTER TABLE voice_participants
    ADD COLUMN admission_id UUID,
    ADD COLUMN admission_socket_id TEXT;
