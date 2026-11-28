-- Bound new permanent reservations without scanning historical messages or
-- changing the ID-only namespace created by 000206.
SET LOCAL lock_timeout = '5s';

CREATE TABLE message_id_reservation_quota (
    shard SMALLINT PRIMARY KEY,
    issued BIGINT NOT NULL DEFAULT 0,
    max_issued BIGINT NOT NULL DEFAULT 78125,
    CONSTRAINT message_id_reservation_quota_shard CHECK (shard BETWEEN 0 AND 63),
    CONSTRAINT message_id_reservation_quota_bounds CHECK (
        issued >= 0 AND max_issued >= issued
    )
);

COMMENT ON TABLE message_id_reservation_quota IS
    '64 fixed prospective issuance budgets totaling 5 million new messages; historical backfill is outside these counters.';

INSERT INTO message_id_reservation_quota (shard)
SELECT shard FROM generate_series(0, 63) AS shard;

CREATE TABLE message_id_reservation_policy (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE,
    max_per_account BIGINT NOT NULL DEFAULT 250000,
    CONSTRAINT message_id_reservation_policy_singleton CHECK (singleton),
    CONSTRAINT message_id_reservation_policy_account_bound CHECK (max_per_account >= 0)
);

INSERT INTO message_id_reservation_policy (singleton) VALUES (TRUE);

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
    start_shard INTEGER;
    attempt INTEGER;
BEGIN
    -- The reservation BEFORE INSERT trigger has already claimed NEW.id. An
    -- AFTER ROW trigger runs only for an actual insert, not a same-ID retry
    -- handled by ON CONFLICT. A raised error rolls both writes back.
    SELECT max_per_account INTO account_limit
    FROM message_id_reservation_policy WHERE singleton = TRUE;

    IF NOT FOUND THEN
        RAISE EXCEPTION USING
            ERRCODE = 'check_violation',
            CONSTRAINT = 'message_id_reservation_account_quota',
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

    -- Spread independent accounts over 64 row locks. The random start is
    -- server-selected, not derived from a client-controlled message ID. Scan
    -- the other shards if this one is full, so no headroom is stranded.
    start_shard := floor(random() * 64)::INTEGER;
    FOR attempt IN 0..63 LOOP
        UPDATE message_id_reservation_quota
        SET issued = issued + 1
        WHERE shard = ((start_shard + attempt) % 64)
          AND issued < max_issued;
        IF FOUND THEN
            RETURN NEW;
        END IF;
    END LOOP;

    RAISE EXCEPTION USING
        ERRCODE = 'check_violation',
        CONSTRAINT = 'message_id_reservation_global_quota',
        MESSAGE = 'message storage capacity exhausted';
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
