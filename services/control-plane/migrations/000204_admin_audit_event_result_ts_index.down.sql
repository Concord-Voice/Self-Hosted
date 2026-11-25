-- 000204_admin_audit_event_result_ts_index.down.sql
-- Drops only the index 000204 added; no data is lost. One statement, so
-- CONCURRENTLY runs outside a transaction block.
DROP INDEX CONCURRENTLY IF EXISTS idx_admin_audit_event_result_ts;
