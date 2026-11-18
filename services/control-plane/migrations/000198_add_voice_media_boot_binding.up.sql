-- Bind each Server Voice participant row to the media-plane boot that wrote
-- it, so a lifecycle event from one boot cannot remove or claim a live row a
-- different boot owns (VULN-110, VULN-112, VULN-209).
--
-- media_boot_id is nullable with no default. NULL means "written before this
-- change" and is never backfilled; the removal rule treats a NULL row as
-- removable by any boot. The nil UUID is refused because a zero stamp would
-- read as a boot identity no media plane can hold.
--
-- server_voice_room_terminals records the last voice.room_empty that removed
-- rows on a channel, and which control-plane replica removed them, so a peer
-- replica that observes an already-empty room can tell a peer's removal from a
-- determined no-op (VULN-208). At most one row per channel.
--
-- Heartbeats write voice_participants constantly: fail fast on the lock and
-- let the runner retry rather than queue every writer behind this migration.
SET LOCAL lock_timeout = '5s';

ALTER TABLE voice_participants ADD COLUMN media_boot_id UUID;

-- Every existing row is NULL, so validation cannot find a violating row and
-- the scan is over live participants only (a bounded table).
-- squawk-ignore constraint-missing-not-valid
ALTER TABLE voice_participants ADD CONSTRAINT voice_participants_media_boot_id_not_nil
    CHECK (media_boot_id IS NULL OR media_boot_id <> '00000000-0000-0000-0000-000000000000');

CREATE TABLE server_voice_room_terminals (
    channel_id UUID PRIMARY KEY REFERENCES channels(id) ON DELETE CASCADE,
    media_boot_id UUID NOT NULL,
    recorded_by UUID NOT NULL,
    removed_count INTEGER NOT NULL CHECK (removed_count > 0),
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

COMMENT ON COLUMN voice_participants.media_boot_id IS
    'Media-plane boot that last wrote this row; NULL for rows written before 000198.';
COMMENT ON TABLE server_voice_room_terminals IS
    'Last voice.room_empty that removed rows per channel, with the replica that removed them.';
