-- 000202_admin_audit_login_success_index.up.sql
-- Serves the inner query of AuditLog.AdminsAddedSinceLastSignIn: the newest
-- successful sign-in of one admin. Without it that lookup walked
-- idx_admin_audit_ts backwards and, on an admin's first sign-in (no matching
-- row), scanned the whole of admin_audit_log.
--
-- CONCURRENTLY, so the build does not block audit writes. golang-migrate
-- sends each file as one simple-query Exec, and PostgreSQL wraps only a
-- multi-statement query in a transaction, so this file holds exactly one
-- statement. If the build fails, the version is dirty and an INVALID index
-- may remain: see README.md § Dirty Migration State. Drop that index before
-- forcing the version; IF NOT EXISTS would otherwise keep the invalid one.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_admin_audit_login_success
    ON admin_audit_log (admin_id, ts DESC)
    WHERE event_type = 'login_success' AND result = 'success';
