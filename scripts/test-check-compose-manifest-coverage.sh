#!/bin/bash
# scripts/test-check-compose-manifest-coverage.sh
#
# Test fixture for scripts/check-compose-manifest-coverage.sh
# Run manually: ./scripts/test-check-compose-manifest-coverage.sh
# Exits non-zero if any case fails.
#
# Mirrors the harness pattern from scripts/test-check-label-references.sh.
#
# See docs/superpowers/specs/2026-05-29-1246-compose-manifest-coverage-lint-design.md

set -euo pipefail
HOOK="$(cd "$(dirname "$0")" && pwd)/check-compose-manifest-coverage.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

PASSES=0
FAILS=0
FAIL_DETAILS=()

# Set up synthetic project tree in TMP:
# - Minimal docker-compose.yml with 2 build contexts (control-plane, media-plane)
# - Minimal docker-compose.production.yml with an override-only media-plane
#   stanza plus 1 build context (postgres)
# - These mirror the current base+override production shape (3 active build
#   contexts); the production file is intentionally invalid when parsed alone.
cat > "$TMP/docker-compose.yml" <<'EOF'
services:
  control-plane:
    build:
      context: ./services/control-plane
  media-plane:
    build:
      context: ./services/media-plane
  example-gated:
    build:
      context: ./services/example-gated
    profiles:
      - gated
EOF

cat > "$TMP/docker-compose.production.yml" <<'EOF'
services:
  media-plane:
    deploy:
      replicas: 1
  postgres:
    build:
      context: ./infrastructure/docker/postgres
EOF

# Initialize a git repo so the script can resolve REPO_ROOT.
(cd "$TMP" && \
    git init -q && \
    git -c user.email=t@t -c user.name=t -c commit.gpgsign=false add . >/dev/null && \
    git -c user.email=t@t -c user.name=t -c commit.gpgsign=false commit -q -m init)

# Defensive assertion: the script's `git rev-parse --show-toplevel` MUST resolve
# to $TMP (or its canonical form), not the real Concord checkout. If a future
# refactor drops the `git init` above, REPO_ROOT would resolve to the parent
# repo and tests would silently scan the real docker-compose.{yml,production.yml}
# fixtures instead of the synthetic ones, masking failures.
# Per pr-test-analyzer Gap F (rating 8).
#
# On macOS, `mktemp -d` returns `/var/folders/...` but git resolves to
# `/private/var/folders/...` (the /var → /private/var symlink). Canonicalize
# both sides via `cd ... && pwd -P` to compare physical paths.
RESOLVED_ROOT=$(cd "$TMP" && git rev-parse --show-toplevel 2>/dev/null && cd - >/dev/null; true)
RESOLVED_ROOT=$(cd "$RESOLVED_ROOT" 2>/dev/null && pwd -P || echo "")
CANONICAL_TMP=$(cd "$TMP" && pwd -P)
if [ "$RESOLVED_ROOT" != "$CANONICAL_TMP" ]; then
    echo "FATAL: test harness REPO_ROOT isolation broken — expected $CANONICAL_TMP, got '$RESOLVED_ROOT'" >&2
    echo "FATAL: this indicates the git init in TMP is missing or broken; tests would scan the real repo." >&2
    exit 99
fi

run() {
    local label="$1"
    local manifest_content="$2"
    local scope_flag="$3"      # "" or "--scope=production-active" (single arg only)
    local expect_exit="$4"     # 0, 1, or 2
    printf '%s\n' "$manifest_content" > "$TMP/manifest.txt"

    # Array-form scope_flag forwarding: $scope_flag="" passes zero args
    # (the desired default-mode invocation), $scope_flag="--scope=..." passes
    # one arg. Avoids unquoted-expansion foot-gun and trips no shellcheck SC2086.
    # Per code-reviewer MAJOR finding on the original implementation.
    #
    # The `"${scope_args[@]+"${scope_args[@]}"}"` idiom is the canonical
    # safe-expansion of a potentially-empty array under `set -u`. Plain
    # `"${scope_args[@]}"` would trip "unbound variable" on an empty array.
    local -a scope_args=()
    [ -n "$scope_flag" ] && scope_args=("$scope_flag")

    local actual_exit
    (cd "$TMP" && "$HOOK" "$TMP/manifest.txt" ${scope_args[@]+"${scope_args[@]}"}) >/dev/null 2>&1 \
        && actual_exit=0 || actual_exit=$?

    if [ "$actual_exit" = "$expect_exit" ]; then
        PASSES=$((PASSES + 1))
        printf '  \033[32m✓\033[0m %s (expected exit=%s, got %s)\n' "$label" "$expect_exit" "$actual_exit"
    else
        FAILS=$((FAILS + 1))
        printf '  \033[31m✗\033[0m %s (expected exit=%s, got %s)\n' "$label" "$expect_exit" "$actual_exit"
        FAIL_DETAILS+=("$label")
    fi
}

echo "=== Case 1: current manifest state, default mode (PASS expected) ==="
run "case 1: all contexts present, default" \
    "services/control-plane
services/media-plane
infrastructure/docker" \
    "" 0

echo ""
echo "=== Case 2: current manifest state, --scope=production-active (PASS expected) ==="
run "case 2: all contexts present, --scope=production-active" \
    "services/control-plane
services/media-plane
infrastructure/docker" \
    "--scope=production-active" 0

echo ""
echo "=== Case 3: pre-#878 manifest replay, default mode (FAIL expected) ==="
# Replay the manifest state from before PR #878. At commit f9cadd93^,
# the manifest had services/control-plane/migrations (CHILD path) but
# NOT services/control-plane (parent BUILD CONTEXT). This is the
# structural bug that #878 shipped to production.
#
# Default mode: production-active control-plane + media-plane produce ERROR.
# (example-gated is profile-gated, so default-profile rendering omits it
# entirely and it never reaches the WARN branch — see #2885.)
#
# Per pr-test-analyzer Gap G: the original git-show-based fixture
# silently skipped on shallow-clone, masking the most-load-bearing
# regression test. Inline-heredoc fallback ensures cases 3/4 always
# run, even in shallow-clone CI environments. The git-show form
# remains primary so divergence from the real historical state would
# be caught by a maintainer.
PRE_878_MANIFEST=""
if PRE_878_MANIFEST=$(git -C "$(dirname "$HOOK")/.." show f9cadd93^:infrastructure/deploy/deploy-manifest.txt 2>/dev/null); then
    : # primary path: real historical content from git
else
    # Fallback: inline heredoc replicates the pre-#878 manifest state.
    # If git history is unreachable (shallow clone, history rewrite), this
    # preserves the regression-test signal. Update if PR #878 base context
    # ever changes upstream.
    PRE_878_MANIFEST="# infrastructure/deploy/deploy-manifest.txt
# Pre-#878 state — inline fallback for shallow-clone environments
docker-compose.yml
docker-compose.production.yml
infrastructure/deploy/concord-ctl.sh
infrastructure/deploy/copy-certs.sh
infrastructure/deploy/sync-releases.sh
infrastructure/deploy/health-check-cron.sh
infrastructure/deploy/backup-db.sh
infrastructure/deploy/upload-cert-to-cloudflare.sh
infrastructure/deploy/provision-production.sh
infrastructure/deploy/post-rsync.sh
infrastructure/deploy/deploy.env.example
infrastructure/deploy/tests
infrastructure/docker
services/control-plane/migrations
spa.env"
fi
run "case 3: pre-#878 replay (default)" "$PRE_878_MANIFEST" "" 1

echo ""
echo "=== Case 4: pre-#878 manifest replay, --scope=production-active (FAIL expected) ==="
# Strict mode: 2 production-active errors (control-plane + media-plane).
# (example-gated is absent from the rendered configuration either way — the
# silent-skip branch is not what excludes it; see #2885.)
run "case 4: pre-#878 replay (--scope=production-active)" "$PRE_878_MANIFEST" "--scope=production-active" 1

echo ""
echo "=== Case 13: child path does NOT satisfy parent context (FAIL expected) ==="
# THE load-bearing #878 bug-class test: manifest contains
# 'services/control-plane/migrations' (a CHILD of build context
# 'services/control-plane'). The case-statement match form
# "$entry"|"$entry"/* matches parent OR child-of-parent, but NEVER
# a child path satisfying a parent context. If a future refactor
# accidentally introduces substring matching (e.g., grep -qF), this
# test fails immediately.
run "case 13: child path 'services/control-plane/migrations' does NOT satisfy parent 'services/control-plane'" \
    "services/control-plane/migrations
services/media-plane
infrastructure/docker" \
    "" 1

echo ""
echo "=== Case 5: missing example-gated, default mode (PASS expected — exit status only) ==="
# Profile-gated service missing from manifest → WARN, but NOT ERROR.
# Exit code is 0 because no production-active service is missing.
#
# KNOWN VACUOUS (pre-existing, surfaced during #1785): this case and case 6
# pass identically whether PROFILE_GATED_CONTEXTS is populated or empty. The
# lint renders compose with DEFAULT profiles, which already excludes
# profile-gated services, so is_profile_gated() is never reached and the
# ::warning:: branch never fires. These cases therefore assert exit 0 for a
# reason unrelated to profile gating. Do not read them as coverage of the
# gating mechanism. Tracked in:
#   https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2885
run "case 5: missing example-gated, default mode (exit 0 — gating NOT exercised, #2885)" \
    "services/control-plane
services/media-plane
infrastructure/docker" \
    "" 0

echo ""
echo "=== Case 6: missing example-gated, --scope=production-active (PASS expected — exit status only) ==="
# Profile-gated service silent-skipped in strict mode.
run "case 6: missing example-gated, strict mode (exit 0 — gating NOT exercised, #2885)" \
    "services/control-plane
services/media-plane
infrastructure/docker" \
    "--scope=production-active" 0

echo ""
echo "=== Case 7: missing services/control-plane (production-active), default mode (FAIL expected) ==="
# Production-active service missing → ERROR regardless of mode.
run "case 7: missing control-plane (default → ERROR)" \
    "services/media-plane
services/example-gated
infrastructure/docker" \
    "" 1

echo ""
echo "=== Case 8: missing services/control-plane, --scope=production-active (FAIL expected) ==="
run "case 8: missing control-plane (--scope=production-active → ERROR)" \
    "services/media-plane
services/example-gated
infrastructure/docker" \
    "--scope=production-active" 1

echo ""
echo "=== Case 9: empty manifest (PASS expected, stderr WARN) ==="
run "case 9: empty manifest → graceful WARN" "" "" 0

echo ""
echo "=== Case 10: comments-only manifest (PASS expected, stderr WARN) ==="
run "case 10: comments-only manifest → graceful WARN" \
    "# only comments
# more comments
" "" 0

echo ""
echo "=== Case 11: missing manifest file (PASS expected, stderr WARN) ==="
# Test by passing a path that doesn't exist.
if (cd "$TMP" && "$HOOK" "$TMP/does-not-exist.txt") >/dev/null 2>&1; then
    actual=PASS
else
    actual=FAIL
fi
if [ "$actual" = "PASS" ]; then
    PASSES=$((PASSES + 1))
    printf '  \033[32m✓\033[0m case 11: missing manifest → graceful WARN (PASS expected, got %s)\n' "$actual"
else
    FAILS=$((FAILS + 1))
    printf '  \033[31m✗\033[0m case 11: missing manifest → graceful WARN (PASS expected, got %s)\n' "$actual"
    FAIL_DETAILS+=("case 11: missing manifest")
fi

echo ""
echo "=== Case 12: missing compose file (PASS expected, stderr WARN, exit 0) ==="
# Rename docker-compose.production.yml out of the way; the script should
# WARN about the missing file but not fail (other compose file still scanned).
mv "$TMP/docker-compose.production.yml" "$TMP/docker-compose.production.yml.bak"
run "case 12: missing compose file → graceful WARN" \
    "services/control-plane
services/media-plane
services/example-gated
infrastructure/docker" \
    "" 0
mv "$TMP/docker-compose.production.yml.bak" "$TMP/docker-compose.production.yml"

echo ""
echo "=== Case 14: invalid tab-indented Compose YAML (FAIL expected) ==="
# YAML forbids tabs for indentation. The structured Compose parser must reject
# this instead of the former grep implementation silently accepting it.
cat > "$TMP/docker-compose.yml" <<'TABCOMPOSE'
services:
  control-plane:
    build:
	context: ./services/control-plane
  media-plane:
    build:
      context: ./services/media-plane
TABCOMPOSE
# Remove example-gated from fixture for this case (3-context shape).
cat > "$TMP/docker-compose.production.yml" <<'EOF'
services:
  media-plane:
    deploy:
      replicas: 1
  postgres:
    build:
      context: ./infrastructure/docker/postgres
EOF
run "case 14: invalid tab-indented Compose YAML" \
    "services/control-plane
services/media-plane
infrastructure/docker" \
    "" 1
# Restore default fixtures for subsequent cases.
cat > "$TMP/docker-compose.yml" <<'EOF'
services:
  control-plane:
    build:
      context: ./services/control-plane
  media-plane:
    build:
      context: ./services/media-plane
  example-gated:
    build:
      context: ./services/example-gated
    profiles:
      - gated
EOF

echo ""
echo "=== Case 15: invalid --scope value (FAIL with exit 2 expected) ==="
# Verify the --scope=* error path produces exit 2 with a script-controlled
# error message. Per pr-test-analyzer Gap D — exit-2 paths not previously tested.
run "case 15: --scope=foo → exit 2" \
    "services/control-plane
services/media-plane
services/example-gated
infrastructure/docker" \
    "--scope=foo" 2

echo ""
echo "=== Case 16: unknown flag (FAIL with exit 2 expected) ==="
# Verify the -* unknown-flag arm produces exit 2.
run "case 16: -x unknown flag → exit 2" \
    "services/control-plane
services/media-plane
services/example-gated
infrastructure/docker" \
    "-x" 2

echo ""
echo "=== Case 17: manifest with inline # comment (PASS expected) ==="
# Verify script strips trailing-of-line comments per manifest_entry%%#* logic.
# Per pr-test-analyzer Gap B.
run "case 17: manifest entries with inline comments" \
    "services/control-plane  # active production service
services/media-plane          # SFU
services/example-gated  # profile-gated (gated profile)
infrastructure/docker         # postgres + sibling docker files" \
    "" 0

echo ""
echo "=== Case 18: manifest with CRLF line endings (PASS expected) ==="
# Per pr-test-analyzer Gap E. Companion script tweak in this commit strips \r.
# Note: this case writes literal \r\n explicitly to overcome printf %s \n behavior.
printf 'services/control-plane\r\nservices/media-plane\r\nservices/example-gated\r\ninfrastructure/docker\r\n' > "$TMP/manifest.txt"
actual_exit=0
(cd "$TMP" && "$HOOK" "$TMP/manifest.txt") >/dev/null 2>&1 || actual_exit=$?
if [ "$actual_exit" = "0" ]; then
    PASSES=$((PASSES + 1))
    printf '  \033[32m✓\033[0m case 18: CRLF manifest (expected exit=0, got %s)\n' "$actual_exit"
else
    FAILS=$((FAILS + 1))
    printf '  \033[31m✗\033[0m case 18: CRLF manifest (expected exit=0, got %s)\n' "$actual_exit"
    FAIL_DETAILS+=("case 18: CRLF manifest")
fi

echo ""
echo "=== Case 19: multiple context: directives in compose file (PASS expected) ==="
# Per pr-test-analyzer Gap C — compose allows multiple build entries with separate
# context: directives across services. The grep pattern emits ALL matches.
# Verify the loop iterates correctly when several contexts appear.
cat > "$TMP/docker-compose.yml" <<'EOF'
services:
  service-a:
    build:
      context: ./services/control-plane
  media-plane:
    build:
      context: ./services/media-plane
  service-c:
    build:
      context: ./services/example-gated
    profiles:
      - gated
  service-d:
    build:
      context: ./services/control-plane
EOF
run "case 19: duplicate context: directives across services" \
    "services/control-plane
services/media-plane
infrastructure/docker" \
    "" 0
# Restore default fixture.
cat > "$TMP/docker-compose.yml" <<'EOF'
services:
  control-plane:
    build:
      context: ./services/control-plane
  media-plane:
    build:
      context: ./services/media-plane
  example-gated:
    build:
      context: ./services/example-gated
    profiles:
      - gated
EOF

echo ""
echo "=== Case 20: missing local additional_contexts path (FAIL expected) ==="
cat > "$TMP/docker-compose.yml" <<'EOF'
services:
  control-plane:
    build:
      context: ./services/control-plane
      additional_contexts:
        admin_ui: ./client/admin
        upstream_source: https://example.invalid/source.git
        base_image: docker-image://node:24-alpine
        sibling_service: service:media-plane
  media-plane:
    build:
      context: ./services/media-plane
EOF
run "case 20: missing local named context blocks" \
    "services/control-plane
services/media-plane
infrastructure/docker" \
    "" 1

echo ""
echo "=== Case 21: exact local additional_contexts path (PASS expected) ==="
run "case 21: exact local named context is covered" \
    "services/control-plane
services/media-plane
client/admin
infrastructure/docker" \
    "" 0

echo ""
echo "=== Case 22: parent covers local additional_contexts path (PASS expected) ==="
run "case 22: parent covers local named context" \
    "services/control-plane
services/media-plane
client
infrastructure/docker" \
    "" 0

echo ""
echo "=== Case 23: child does not cover local additional_contexts path (FAIL expected) ==="
run "case 23: child does not cover local named context" \
    "services/control-plane
services/media-plane
client/admin/src
infrastructure/docker" \
    "" 1

echo ""
echo "=== Case 24: older Compose CLI + controlled interpolation (PASS expected) ==="
# Blacksmith's CI image does not support Compose's newer no-resolution flags,
# and its older parser rejects uninterpolated short volume specs containing
# ${VAR:-default}:container:mode. Model both constraints, require the guard's
# fixed non-secret environment, and return default absolute path rendering.
FAKE_BIN="$TMP/compose-compat-bin"
mkdir -p "$FAKE_BIN"
cat > "$FAKE_BIN/docker" <<'EOF'
#!/bin/bash
set -euo pipefail
if [ "$#" -eq 2 ] && [ "$1" = "compose" ] && [ "$2" = "version" ]; then
    exit 0
fi
case " $* " in
    *" --no-env-resolution "*|*" --no-path-resolution "*)
        echo "unknown flag" >&2
        exit 125
        ;;
    *" --no-interpolate "*)
        echo 'invalid spec: ${COTURN_CERTS_DIR:-./certs}:/etc/coturn/certs:ro: too many colons' >&2
        exit 15
        ;;
esac
case " $* " in
    *" --env-file /dev/null "*) ;;
    *) echo "missing isolated env file" >&2; exit 16 ;;
esac
[ "${JWT_SECRET:-}" = "manifest-check-not-a-secret" ] || exit 17
[ "${POSTGRES_PASSWORD:-}" = "manifest-check-not-a-secret" ] || exit 18
[ "${DOMAIN:-}" = "manifest-check.invalid" ] || exit 19
root=$(pwd -P)
cat <<JSON
{"services":{"control-plane":{"build":{"context":"${root}/services/control-plane","additional_contexts":{"admin_ui":"${root}/client/admin"}}},"media-plane":{"build":{"context":"${root}/services/media-plane"}},"postgres":{"build":{"context":"${root}/infrastructure/docker/postgres"}}}}
JSON
EOF
chmod +x "$FAKE_BIN/docker"
printf '%s\n' \
    "services/control-plane" \
    "services/media-plane" \
    "client/admin" \
    "infrastructure/docker" > "$TMP/manifest.txt"
actual_exit=0
(cd "$TMP" && PATH="$FAKE_BIN:$PATH" "$HOOK" "$TMP/manifest.txt") >/dev/null 2>&1 || actual_exit=$?
if [ "$actual_exit" = "0" ]; then
    PASSES=$((PASSES + 1))
    printf '  \033[32m✓\033[0m case 24: older Compose CLI + controlled interpolation (expected exit=0, got %s)\n' "$actual_exit"
else
    FAILS=$((FAILS + 1))
    printf '  \033[31m✗\033[0m case 24: older Compose CLI + controlled interpolation (expected exit=0, got %s)\n' "$actual_exit"
    FAIL_DETAILS+=("case 24: older Compose CLI + controlled interpolation")
fi

echo ""
echo "──────────────────────────────────────"
echo "Results: $PASSES passed, $FAILS failed"
if [ "$FAILS" -gt 0 ]; then
    echo "Failed cases:"
    for case in "${FAIL_DETAILS[@]}"; do
        echo "  - $case"
    done
    exit 1
fi
echo "All tests passed."
