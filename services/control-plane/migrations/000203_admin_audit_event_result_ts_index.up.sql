-- 000203_admin_audit_event_result_ts_index.up.sql
-- Serves the outer count of AuditLog.AdminsAddedSinceLastSignIn: the
-- admin_created successes, which it then keeps by row id. With only
-- idx_admin_audit_ts that count read every audit row in its window and
-- filtered on event type and result (EXPLAIN on 1M seeded rows: about 900k
-- rows removed by the filter, 160 ms). With this index it reads only the
-- admin_created successes, one per created admin (under 1 ms).
--
-- One statement, CONCURRENTLY, for the same reasons as 000202, and the same
-- dirty-state recovery: drop an INVALID index before forcing the version.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_admin_audit_event_result_ts
    ON admin_audit_log (event_type, result, ts);
