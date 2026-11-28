-- Removing the cap from a populated installation would restore unbounded
-- permanent reservation growth. Only an unused installation can downgrade.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

LOCK TABLE messages, dm_messages, message_id_reservations,
    message_id_reservation_account_usage, message_id_reservation_quota,
    message_id_reservation_policy
    IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM messages)
        OR EXISTS (SELECT 1 FROM dm_messages)
        OR EXISTS (SELECT 1 FROM message_id_reservations)
        OR EXISTS (SELECT 1 FROM message_id_reservation_account_usage)
        OR EXISTS (SELECT 1 FROM message_id_reservation_quota WHERE issued <> 0)
    THEN
        RAISE EXCEPTION USING
            ERRCODE = 'integrity_constraint_violation',
            MESSAGE = 'refusing to remove message ID issuance quota from a used installation';
    END IF;
END;
$$;

DROP TRIGGER message_id_reservation_quota_after_insert ON messages;
DROP TRIGGER message_id_reservation_quota_after_insert ON dm_messages;
DROP TRIGGER message_id_reservation_quota_before_mutation ON message_id_reservation_quota;
DROP TRIGGER message_id_reservation_quota_before_truncate ON message_id_reservation_quota;
DROP FUNCTION charge_message_id_reservation_quota();
DROP FUNCTION reject_message_id_reservation_quota_truncate();
DROP FUNCTION protect_message_id_reservation_quota();
DROP TABLE message_id_reservation_account_usage;
DROP TABLE message_id_reservation_quota;
DROP TABLE message_id_reservation_policy;
