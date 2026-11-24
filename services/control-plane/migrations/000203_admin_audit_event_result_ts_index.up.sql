-- 000203_admin_audit_event_result_ts_index.up.sql
-- Serves the outer count of AuditLog.AdminsAddedSinceLastSignIn: the
-- admin_created successes recorded since a given time. With only
-- idx_admin_audit_ts the count read every audit row since the admin's
-- previous sign-in and filtered on event type and result, so an admin who
-- signs in rarely paid for the whole window (EXPLAIN on 1M seeded rows:
-- about 900k rows removed by the filter, 160 ms; with this index an
-- index-only scan of the matching rows, under 1 ms).
--
-- One statement, CONCURRENTLY, for the same reasons as 000202, and the same
-- dirty-state recovery: drop an INVALID index before forcing the version.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_admin_audit_event_result_ts
    ON admin_audit_log (event_type, result, ts);
