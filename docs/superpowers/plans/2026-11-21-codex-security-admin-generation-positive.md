# Positive administrator session generation plan

1. Add migration 000199 to enforce a positive value on future writes, then
   validate existing rows in migration 000200. Reverse each phase in its down file.
2. Extend the administrator repository test to prove a zero write fails.
3. Update the migration register and prerelease security changelog.
4. Run repository hooks and exact-head PR CI, reconcile independent migration
   and security reviews, then request the per-PR merge approval.
