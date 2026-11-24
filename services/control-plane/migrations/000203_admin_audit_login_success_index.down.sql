-- 000203_admin_audit_login_success_index.down.sql
-- Drops only the index 000203 added; no data is lost. One statement, so
-- CONCURRENTLY runs outside a transaction block.
DROP INDEX CONCURRENTLY IF EXISTS idx_admin_audit_login_success;
