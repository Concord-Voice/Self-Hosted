-- Bound new permanent reservations without scanning historical messages or
-- changing the ID-only namespace created by 000206.
SET LOCAL lock_timeout = '5s';

CREATE TABLE message_id_reservation_quota (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE,
    issued BIGINT NOT NULL DEFAULT 0,
    max_issued BIGINT NOT NULL DEFAULT 5000000,
    max_per_account BIGINT NOT NULL DEFAULT 250000,
    CONSTRAINT message_id_reservation_quota_singleton CHECK (singleton),
    CONSTRAINT message_id_reservation_quota_bounds CHECK (
        issued >= 0 AND max_issued >= issued AND max_per_account >= 0
    )
);

COMMENT ON TABLE message_id_reservation_quota IS
    'Prospective global new-message issuance budget; historical backfill is outside this counter.';

INSERT INTO message_id_reservation_quota (singleton) VALUES (TRUE);

CREATE TABLE message_id_reservation_account_usage (
    user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    issued BIGINT NOT NULL DEFAULT 0,
    CONSTRAINT message_id_reservation_account_usage_nonnegative CHECK (issued >= 0)
);

COMMENT ON TABLE message_id_reservation_account_usage IS
    'Prospective new-message count for a live account; account deletion removes this counter.';

CREATE FUNCTION protect_message_id_reservation_quota() RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    -- The disposable test harness may restore a fresh singleton under the
    -- superuser-only replica setting. Production writes cannot reduce usage.
    IF current_setting('session_replication_role') = 'replica' THEN
        IF TG_OP = 'DELETE' THEN
            RETURN OLD;
        END IF;
        RETURN NEW;
    END IF;

    IF TG_OP = 'UPDATE' AND NEW.issued >= OLD.issued THEN
        RETURN NEW;
    END IF;

    RAISE EXCEPTION USING
        ERRCODE = 'integrity_constraint_violation',
        CONSTRAINT = 'message_id_reservation_quota_monotonic',
        MESSAGE = 'message ID issuance usage cannot be reduced or removed';
END;
$$;

CREATE FUNCTION reject_message_id_reservation_quota_truncate() RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF current_setting('session_replication_role') = 'replica' THEN
        RETURN NULL;
    END IF;

    RAISE EXCEPTION USING
        ERRCODE = 'integrity_constraint_violation',
        CONSTRAINT = 'message_id_reservation_quota_monotonic',
        MESSAGE = 'message ID issuance usage cannot be reduced or removed';
END;
$$;

CREATE TRIGGER message_id_reservation_quota_before_mutation
BEFORE UPDATE OR DELETE ON message_id_reservation_quota
FOR EACH ROW EXECUTE FUNCTION protect_message_id_reservation_quota();

CREATE TRIGGER message_id_reservation_quota_before_truncate
BEFORE TRUNCATE ON message_id_reservation_quota
FOR EACH STATEMENT EXECUTE FUNCTION reject_message_id_reservation_quota_truncate();

ALTER TABLE message_id_reservation_quota ENABLE ALWAYS TRIGGER message_id_reservation_quota_before_mutation;
ALTER TABLE message_id_reservation_quota ENABLE ALWAYS TRIGGER message_id_reservation_quota_before_truncate;

CREATE FUNCTION charge_message_id_reservation_quota() RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    account_limit BIGINT;
BEGIN
    -- The reservation BEFORE INSERT trigger has already claimed NEW.id. An
    -- AFTER ROW trigger runs only for an actual insert, not a same-ID retry
    -- handled by ON CONFLICT. A raised error rolls both writes back.
    UPDATE message_id_reservation_quota
    SET issued = issued + 1
    WHERE singleton AND issued < max_issued
    RETURNING max_per_account INTO account_limit;

    IF NOT FOUND THEN
        RAISE EXCEPTION USING
            ERRCODE = 'check_violation',
            CONSTRAINT = 'message_id_reservation_global_quota',
            MESSAGE = 'message storage capacity exhausted';
    END IF;

    IF account_limit = 0 THEN
        RAISE EXCEPTION USING
            ERRCODE = 'check_violation',
            CONSTRAINT = 'message_id_reservation_account_quota',
            MESSAGE = 'message storage capacity exhausted';
    END IF;

    INSERT INTO message_id_reservation_account_usage (user_id, issued)
    VALUES (NEW.user_id, 1)
    ON CONFLICT (user_id) DO UPDATE
        SET issued = message_id_reservation_account_usage.issued + 1
        WHERE message_id_reservation_account_usage.issued < account_limit;

    IF NOT FOUND THEN
        RAISE EXCEPTION USING
            ERRCODE = 'check_violation',
            CONSTRAINT = 'message_id_reservation_account_quota',
            MESSAGE = 'message storage capacity exhausted';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER message_id_reservation_quota_after_insert
AFTER INSERT ON messages
FOR EACH ROW EXECUTE FUNCTION charge_message_id_reservation_quota();

CREATE TRIGGER message_id_reservation_quota_after_insert
AFTER INSERT ON dm_messages
FOR EACH ROW EXECUTE FUNCTION charge_message_id_reservation_quota();

ALTER TABLE messages ENABLE ALWAYS TRIGGER message_id_reservation_quota_after_insert;
ALTER TABLE dm_messages ENABLE ALWAYS TRIGGER message_id_reservation_quota_after_insert;
