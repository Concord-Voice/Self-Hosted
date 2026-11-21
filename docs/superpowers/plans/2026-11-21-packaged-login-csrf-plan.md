# Plan: packaged Desktop login CSRF compatibility

**Scope:** Self-Hosted ad-hoc fix · **Date:** 2026-11-21

1. Add a middleware regression table that first demonstrates the configured `app://concord` request is rejected because Fetch Metadata is evaluated before the allowed-origin check. Include configured non-Desktop origins that must remain forbidden.
2. Change the cross-site decision by the smallest amount: permit it only for a single exact `app://concord` header that the existing configured-origin checker accepts; leave the normal origin check in place.
3. Record the user-visible compatibility repair in the prerelease changelog, run formatting, focused race tests, lint, and the changelog guard, then inspect the diff before committing.

**Verification:** use `go test -race ./internal/middleware/...`, `golangci-lint run ./internal/middleware/...`, and the repository changelog verifier. No database or Redis service is required.
