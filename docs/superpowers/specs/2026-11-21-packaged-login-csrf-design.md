# Packaged Desktop login CSRF compatibility — design

**Scope:** Self-Hosted login CSRF middleware only · **Date:** 2026-11-21 · **Status:** approved for execution

## Problem

The packaged Desktop login request sends `Origin: app://concord` and Chromium marks it `Sec-Fetch-Site: cross-site`. The current guard rejects every cross-site request before checking the configured allowed-origin list, so a server explicitly configured for `app://concord` rejects the packaged login.

## Decision

Treat only the literal `app://concord` origin as compatible with `Sec-Fetch-Site: cross-site`, and only when that exact origin occurs once and is present in the configured allowed-origin list. Continue through the ordinary origin validator after that exception.

This is not a general relaxation for configured origins: HTTPS origins, other `app://` origins, foreign origins, missing origins, malformed/multiple origins, and wildcard configuration remain subject to the existing rejection behavior.

## Acceptance criteria

- A JSON request with `Origin: app://concord`, `Sec-Fetch-Site: cross-site`, and an `app://concord` entry in `ALLOWED_ORIGINS` reaches the handler.
- `https://app.example.test` and `app://concord-mobile` remain rejected when marked cross-site, even if configured.
- Existing login CSRF tests continue to reject all other cross-site requests and preserve origin-less native-client behavior.
- The middleware package race test and lint pass.

## Scope boundaries

No Desktop, managed deployment, configuration, router, dependency, migration, or auth-route changes are part of this fix.
