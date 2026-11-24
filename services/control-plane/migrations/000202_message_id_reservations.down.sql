-- A downgrade may remove the reservation namespace only from a completely
-- unused, unverified installation. Do not delete reservations or history to
-- make this pass: that would reopen message identifiers.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

LOCK TABLE messages, dm_messages, message_id_reservations, message_id_reservation_rollout
    IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM messages)
        OR EXISTS (SELECT 1 FROM dm_messages)
        OR EXISTS (SELECT 1 FROM message_id_reservations)
        OR EXISTS (
            SELECT 1
            FROM message_id_reservation_rollout
            WHERE backfill_complete
        ) THEN
        RAISE EXCEPTION USING
            ERRCODE = 'integrity_constraint_violation',
            MESSAGE = 'refusing to remove message ID reservation state while history, reservations, or a verified baseline remain';
    END IF;
END;
$$;

DROP TRIGGER message_id_reservations_before_insert ON messages;
DROP TRIGGER message_id_reservations_before_delete ON messages;
DROP TRIGGER message_id_reservations_before_id_update ON messages;
DROP TRIGGER message_id_reservations_before_truncate ON messages;
DROP TRIGGER message_id_reservations_before_insert ON dm_messages;
DROP TRIGGER message_id_reservations_before_delete ON dm_messages;
DROP TRIGGER message_id_reservations_before_id_update ON dm_messages;
DROP TRIGGER message_id_reservations_before_truncate ON dm_messages;

DROP FUNCTION reserve_message_ids_on_truncate();
DROP FUNCTION reject_message_id_update();
DROP FUNCTION reserve_message_id_on_delete();
DROP FUNCTION reserve_message_id_on_insert();

DROP TABLE message_id_reservation_rollout;
DROP TABLE message_id_reservations;
