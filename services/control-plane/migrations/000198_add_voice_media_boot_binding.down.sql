-- Both objects hold only live or short-lived state: a boot stamp on a current
-- participant row and the last room-empty tombstone per channel. Code before
-- 000198 reads neither, so dropping them loses nothing it needs.
--
-- Same lock order as the up: voice_participants first, then the table that
-- references channels.
SET LOCAL lock_timeout = '5s';

ALTER TABLE voice_participants
    DROP CONSTRAINT voice_participants_media_boot_id_not_nil,
    DROP COLUMN media_boot_id;

DROP TABLE server_voice_room_terminals;
