#!/usr/bin/env bash
#
# npm-audit gate — CI entrypoint.
#
# Replaces a bare `npm audit --audit-level=high` step. Runs `npm audit --json`
# in the current package directory and delegates the verdict to the pure logic
# in npm-audit-gate.mjs, which fails ONLY on HIGH/CRITICAL advisories that are
# not present in the package's tracked allowlist (.audit-allowlist.json).
#
# Why: a bare `npm audit` gate is time-brittle — it queries the live advisory
# DB at run time, so a newly-published HIGH advisory red-gates every PR with
# zero code change, including fix-less advisories that authors cannot resolve.
# See scripts/security/npm-audit-gate.mjs header and
# [internal]quality-gate-definition.md "npm-audit gate".
#
# Usage (run with working-directory set to the package dir):
#   bash "$GITHUB_WORKSPACE/scripts/security/npm-audit-gate.sh" --allowlist .audit-allowlist.json
#
# Exit: 0 = pass, 1 = un-accepted HIGH/CRITICAL advisory present, 2 = usage/parse error.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ALLOWLIST=".audit-allowlist.json"

while [ $# -gt 0 ]; do
  case "$1" in
    --allowlist) ALLOWLIST="$2"; shift 2 ;;
    *) echo "npm-audit-gate: unknown argument: $1" >&2; exit 2 ;;
  esac
done

if [ ! -f "$ALLOWLIST" ]; then
  echo "npm-audit-gate: allowlist not found at $ALLOWLIST (run from the package directory)" >&2
  exit 2
fi

AUDIT_JSON="$(mktemp)"
AUDIT_ERR="$(mktemp)"
AUDIT_PROD_JSON="$(mktemp)"
AUDIT_PROD_ERR="$(mktemp)"
trap 'rm -f "$AUDIT_JSON" "$AUDIT_ERR" "$AUDIT_PROD_JSON" "$AUDIT_PROD_ERR"' EXIT

# `npm audit --json` exits non-zero when it finds any vulnerability; that is the
# expected path here (we make the gating decision ourselves), so do not let the
# non-zero exit abort the script. A genuine npm failure (network/registry/auth)
# yields either non-JSON output OR a valid `{ "error": ... }` report — both of
# which the .mjs rejects with exit 2 (fail closed). Capture stderr to a file so
# that failure is diagnosable instead of swallowed by `2>/dev/null`.
npm audit --json >"$AUDIT_JSON" 2>"$AUDIT_ERR" || true

# Second, PRODUCTION-ONLY pass. This is what makes a `"scope": "dev"` allowlist
# entry verifiable: the .mjs honours such an entry only when the advisory is
# ABSENT here, turning "dev-only, build-time" from an assertion in a free-text
# `reason` into something re-checked on every run. Same stderr-to-a-file
# treatment as above rather than `2>/dev/null`, so an unusable production report
# is diagnosable — the .mjs already fails closed for dev-scoped entries when it
# cannot verify them, and the operator needs to see why.
npm audit --omit=dev --json >"$AUDIT_PROD_JSON" 2>"$AUDIT_PROD_ERR" || true

node "$SCRIPT_DIR/npm-audit-gate.mjs" \
  --audit "$AUDIT_JSON" \
  --audit-prod "$AUDIT_PROD_JSON" \
  --allowlist "$ALLOWLIST"
rc=$?

# On a usable-report failure (exit 2), surface npm's own stderr so the operator
# can see WHY the audit produced no usable report.
if [ "$rc" -eq 2 ] && [ -s "$AUDIT_ERR" ]; then
  echo "npm-audit-gate: 'npm audit --json' did not produce a usable report — npm stderr follows:" >&2
  cat "$AUDIT_ERR" >&2
fi
# A dev-scoped exception that could not be verified blocks (exit 1), and the
# reason is almost always a failed production pass — surface that stderr too.
if [ "$rc" -eq 1 ] && [ -s "$AUDIT_PROD_ERR" ]; then
  echo "npm-audit-gate: 'npm audit --omit=dev --json' wrote to stderr; a dev-scoped exception may have blocked because it could not be verified — npm stderr follows:" >&2
  cat "$AUDIT_PROD_ERR" >&2
fi
exit "$rc"
