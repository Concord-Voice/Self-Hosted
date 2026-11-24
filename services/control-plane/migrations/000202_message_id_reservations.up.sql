-- Reserve every channel and direct-message identifier in one permanent global
-- namespace. The reservation table intentionally contains only the identifier:
-- it must not become a second message ledger.
SET LOCAL lock_timeout = '5s';

CREATE TABLE message_id_reservations (
    message_id UUID PRIMARY KEY
);

COMMENT ON TABLE message_id_reservations IS
    'Permanent global reservation namespace for channel and direct-message UUIDs; intentionally ID-only.';
COMMENT ON COLUMN message_id_reservations.message_id IS
    'A message UUID that must never be reused in either message table.';

-- This singleton is an activation gate, not a message record. A future
-- message-v1 writer must fail closed until the online backfill tool has set it.
CREATE TABLE message_id_reservation_rollout (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE,
    backfill_complete BOOLEAN NOT NULL DEFAULT FALSE,
    CONSTRAINT message_id_reservation_rollout_singleton_check CHECK (singleton)
);

COMMENT ON TABLE message_id_reservation_rollout IS
    'Singleton activation gate for message ID reservation backfill; false until the online verification completes.';
COMMENT ON COLUMN message_id_reservation_rollout.backfill_complete IS
    'Set only by the verified message-id-reservation-backfill operation after collision and anti-join checks.';

INSERT INTO message_id_reservation_rollout (singleton, backfill_complete) VALUES (TRUE, FALSE);

CREATE FUNCTION protect_message_id_reservations() RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    -- The disposable PostgreSQL test harness resets tables under replica mode.
    -- Application roles cannot set this superuser-only setting in production.
    IF current_setting('session_replication_role') = 'replica' THEN
        IF TG_OP = 'DELETE' THEN
            RETURN OLD;
        END IF;
        RETURN NEW;
    END IF;

    RAISE EXCEPTION USING
        ERRCODE = 'integrity_constraint_violation',
        CONSTRAINT = 'message_id_reservations_immutable',
        MESSAGE = 'message ID reservations are immutable';
END;
$$;

CREATE FUNCTION reject_message_id_reservations_truncate() RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    -- See protect_message_id_reservations for the test-only replica exception.
    IF current_setting('session_replication_role') = 'replica' THEN
        RETURN NULL;
    END IF;

    RAISE EXCEPTION USING
        ERRCODE = 'integrity_constraint_violation',
        CONSTRAINT = 'message_id_reservations_immutable',
        MESSAGE = 'message ID reservations are immutable';
END;
$$;

CREATE FUNCTION reserve_message_id_on_insert() RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    -- During the online baseline, old IDs are not yet all in the reservation
    -- table. Check the opposite source table so a new legacy write cannot
    -- create a cross-table collision before its old row is scanned.
    IF TG_TABLE_NAME = 'messages' THEN
        PERFORM 1 FROM dm_messages WHERE id = NEW.id;
    ELSIF TG_TABLE_NAME = 'dm_messages' THEN
        PERFORM 1 FROM messages WHERE id = NEW.id;
    ELSE
        RAISE EXCEPTION 'message ID reservation trigger attached to unexpected table';
    END IF;
    IF FOUND THEN
        RAISE EXCEPTION USING
            ERRCODE = 'unique_violation',
            CONSTRAINT = 'message_id_reservations_pkey',
            MESSAGE = 'message id is already reserved';
    END IF;

    INSERT INTO message_id_reservations (message_id)
    VALUES (NEW.id)
    ON CONFLICT (message_id) DO NOTHING;

    IF NOT FOUND THEN
        RAISE EXCEPTION USING
            ERRCODE = 'unique_violation',
            CONSTRAINT = 'message_id_reservations_pkey',
            MESSAGE = 'message id is already reserved';
    END IF;

    RETURN NEW;
END;
$$;

CREATE FUNCTION reserve_message_id_on_delete() RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    -- Temporary during online keyset backfill: preserve an old identifier if
    -- the row disappears before its batch is scanned. The backfill operation
    -- removes these triggers only after it proves both anti-joins are empty.
    INSERT INTO message_id_reservations (message_id)
    VALUES (OLD.id)
    ON CONFLICT (message_id) DO NOTHING;

    RETURN OLD;
END;
$$;

CREATE FUNCTION reject_message_id_update() RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id THEN
        RAISE EXCEPTION USING
            ERRCODE = 'check_violation',
            CONSTRAINT = 'message_id_reservations_message_id_immutable',
            MESSAGE = 'message identifiers are immutable';
    END IF;

    RETURN NEW;
END;
$$;

CREATE FUNCTION reserve_message_ids_on_truncate() RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    -- TRUNCATE already takes ACCESS EXCLUSIVE on its target. Capture before
    -- it removes rows so an administrative truncation cannot reopen an ID.
    IF TG_TABLE_NAME = 'messages' THEN
        INSERT INTO message_id_reservations (message_id)
        SELECT id FROM messages
        ON CONFLICT (message_id) DO NOTHING;
    ELSIF TG_TABLE_NAME = 'dm_messages' THEN
        INSERT INTO message_id_reservations (message_id)
        SELECT id FROM dm_messages
        ON CONFLICT (message_id) DO NOTHING;
    ELSE
        RAISE EXCEPTION 'message ID reservation trigger attached to unexpected table';
    END IF;

    RETURN NULL;
END;
$$;

CREATE TRIGGER message_id_reservations_before_insert
BEFORE INSERT ON messages
FOR EACH ROW EXECUTE FUNCTION reserve_message_id_on_insert();

CREATE TRIGGER message_id_reservations_before_mutation
BEFORE UPDATE OR DELETE ON message_id_reservations
FOR EACH ROW EXECUTE FUNCTION protect_message_id_reservations();

CREATE TRIGGER message_id_reservations_before_truncate
BEFORE TRUNCATE ON message_id_reservations
FOR EACH STATEMENT EXECUTE FUNCTION reject_message_id_reservations_truncate();

CREATE TRIGGER message_id_reservations_before_delete
BEFORE DELETE ON messages
FOR EACH ROW EXECUTE FUNCTION reserve_message_id_on_delete();

CREATE TRIGGER message_id_reservations_before_id_update
BEFORE UPDATE OF id ON messages
FOR EACH ROW EXECUTE FUNCTION reject_message_id_update();

CREATE TRIGGER message_id_reservations_before_truncate
BEFORE TRUNCATE ON messages
FOR EACH STATEMENT EXECUTE FUNCTION reserve_message_ids_on_truncate();

CREATE TRIGGER message_id_reservations_before_insert
BEFORE INSERT ON dm_messages
FOR EACH ROW EXECUTE FUNCTION reserve_message_id_on_insert();

CREATE TRIGGER message_id_reservations_before_delete
BEFORE DELETE ON dm_messages
FOR EACH ROW EXECUTE FUNCTION reserve_message_id_on_delete();

CREATE TRIGGER message_id_reservations_before_id_update
BEFORE UPDATE OF id ON dm_messages
FOR EACH ROW EXECUTE FUNCTION reject_message_id_update();

CREATE TRIGGER message_id_reservations_before_truncate
BEFORE TRUNCATE ON dm_messages
FOR EACH STATEMENT EXECUTE FUNCTION reserve_message_ids_on_truncate();

ALTER TABLE messages ENABLE ALWAYS TRIGGER message_id_reservations_before_insert;
ALTER TABLE messages ENABLE ALWAYS TRIGGER message_id_reservations_before_delete;
ALTER TABLE messages ENABLE ALWAYS TRIGGER message_id_reservations_before_id_update;
ALTER TABLE messages ENABLE ALWAYS TRIGGER message_id_reservations_before_truncate;
ALTER TABLE dm_messages ENABLE ALWAYS TRIGGER message_id_reservations_before_insert;
ALTER TABLE dm_messages ENABLE ALWAYS TRIGGER message_id_reservations_before_delete;
ALTER TABLE dm_messages ENABLE ALWAYS TRIGGER message_id_reservations_before_id_update;
ALTER TABLE dm_messages ENABLE ALWAYS TRIGGER message_id_reservations_before_truncate;
ALTER TABLE message_id_reservations ENABLE ALWAYS TRIGGER message_id_reservations_before_mutation;
ALTER TABLE message_id_reservations ENABLE ALWAYS TRIGGER message_id_reservations_before_truncate;
