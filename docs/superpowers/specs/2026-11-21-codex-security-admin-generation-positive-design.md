# Positive administrator session generation

## Problem

Migration 000197 starts administrator session generations at 1 but allows 0.
Legacy Redis sessions without the generation field decode as 0. A durable row
written with generation 0 could therefore match a legacy session after the
administrator becomes active.

## Decision

Add new migrations because 000197 may already be applied. Migration 000199
adds a positive check that rejects future zero writes. Migration 000200
validates existing rows without holding an exclusive lock during the scan, and
fails if any administrator row is zero. The original nonnegative check remains
as a redundant guard. The down migrations reverse the validation state and
remove the new check.

No API, Redis format, or session-minting behavior changes. Middleware continues
to require an active administrator and an exact generation match.

## Acceptance

- PostgreSQL rejects a zero generation through the named positive constraint.
- Both migrations have symmetric down files and bounded lock waits.
- Protected PR CI runs the database-backed regression against an isolated test database.
