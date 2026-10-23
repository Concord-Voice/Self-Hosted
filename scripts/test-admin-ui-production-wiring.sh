#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
failures=0

pass() {
    printf 'PASS: %s\n' "$1"
}

fail() {
    printf 'FAIL: %s\n' "$1" >&2
    failures=$((failures + 1))
}

assert_contains() {
    local file=$1
    local expected=$2
    local label=$3
    if [ -f "$ROOT/$file" ] && grep -Fq -- "$expected" "$ROOT/$file"; then
        pass "$label"
    else
        fail "$label"
    fi
}

assert_filter_path() {
    local file=$1
    local filter=$2
    local expected=$3
    local block indent='            ' item_prefix='              - '
    if [ "$file" = ".github/ci/scan-relevance.yml" ]; then
        # Two lines on purpose: Semgrep's bash parser fails on the one-line
        # form, and the protected scan treats any partial parse as a failure.
        indent=''
        item_prefix='  - '
    fi
    block=$(awk -v filter="$filter" -v indent="$indent" '
        $0 == indent filter ":" { active=1; found=1; next }
        active && $0 ~ ("^" indent "[A-Za-z0-9_-]+:$") { exit }
        active { print }
        END { if (!found) exit 2 }
    ' "$ROOT/$file") || {
        fail "$file defines the $filter filter"
        return
    }
    if grep -Fqx -- "$item_prefix'$expected'" <<< "$block"; then
        pass "$file $filter includes $expected"
    else
        fail "$file $filter includes $expected"
    fi
}

assert_filter_path ".github/workflows/build.yml" "production-docker" "client/admin/**"
assert_filter_path ".github/workflows/main-cd.yml" "services" "client/admin/**"
assert_filter_path ".github/ci/scan-relevance.yml" "services" "client/admin/**"
assert_filter_path ".github/ci/scan-relevance.yml" "production_docker" "client/admin/**"
assert_contains "docker-compose.yml" "admin_ui: ./client/admin" \
    "Compose exposes the Admin Portal named build context"
assert_contains "services/control-plane/Dockerfile" \
    "COPY --chown=appuser:appuser --from=admin-ui-builder /src/dist ./admin-ui" \
    "the runtime image receives only built Admin Portal assets"
assert_contains "services/control-plane/Dockerfile" \
    "npm ci --ignore-scripts" \
    "the Admin Portal dependency install disables lifecycle scripts"
assert_contains ".github/workflows/build.yml" \
    "test -f /home/appuser/admin-ui/index.html" \
    "production Docker CI verifies the Admin Portal entrypoint"
assert_contains ".github/workflows/build.yml" \
    "! command -v node" \
    "production Docker CI verifies Node is absent from the runtime"
assert_contains ".github/workflows/build.yml" \
    "! command -v npm" \
    "production Docker CI verifies npm is absent from the runtime"
assert_contains "client/admin/.dockerignore" "node_modules" \
    "the named context excludes host node_modules"
assert_contains "client/admin/.dockerignore" "dist" \
    "the named context excludes host build output"

if [ "$failures" -ne 0 ]; then
    printf '\n%d production wiring assertion(s) failed.\n' "$failures" >&2
    exit 1
fi

printf '\nAll Admin Portal production wiring assertions passed.\n'
