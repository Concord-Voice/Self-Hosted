-- Drops only replay evidence: without it a redelivered voice.joined ejects as it
-- did before 000195. Stop the control plane that uses these columns before
-- running this down, then start the older image; a newer binary against the
-- dropped columns fails every Server Voice join.
SET LOCAL lock_timeout = '3s';

ALTER TABLE voice_participants
    DROP COLUMN IF EXISTS admission_socket_id,
    DROP COLUMN IF EXISTS admission_id;
