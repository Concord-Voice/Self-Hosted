#!/usr/bin/env bash
# check-openapi-coverage.sh — CI drift gate for docs/api/openapi.yaml (#822).
#
# Fails when the spec and the live router disagree in either direction:
#   - a route registered in services/control-plane/internal/api/router.go
#     has no matching (method, path) entry in the spec, or
#   - the spec documents a route the router no longer registers.
#
# Modeled on the verify-spa-env layered-defense pattern: cheap static parse,
# runs on every PR (pr-ci `verify-openapi-coverage` job), no network.
# The parser lives in scripts/api/extract-routes.py (stdlib-only python3).
#
# Downstream consumer note (#1465): the ZAP API DAST imports this spec as
# its scan-surface definition — a stale spec silently narrows DAST coverage,
# which is why both drift directions hard-fail.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
exec python3 scripts/api/extract-routes.py --check
