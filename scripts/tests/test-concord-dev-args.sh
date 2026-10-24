#!/usr/bin/env bash
# Tests for concord-dev.sh argument parsing and verb dispatch.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
CONCORD_DEV="$PROJECT_ROOT/scripts/concord-dev.sh"

if [[ ! -x "$CONCORD_DEV" ]]; then
  echo "FAIL: $CONCORD_DEV not executable"
  exit 1
fi

# Stub docker so verbs that call compose() don't actually hit Docker.
STUB_DIR=$(mktemp -d)
# DOCKER_LOG is mktemp'd later inside Test 4; include it in the trap so it
# doesn't leak into /var/folders/... across runs.
DOCKER_LOG=""
DEBUG_OUT=""
trap 'rm -rf "$STUB_DIR" "$DOCKER_LOG" "$DEBUG_OUT" 2>/dev/null || true' EXIT
cat > "$STUB_DIR/docker" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$STUB_DIR/docker"

# ── Test 1: help is printed for no args ───────────────────────────────
out=$("$CONCORD_DEV" 2>&1 || true)
if grep -q "Concord Voice — Dev Lifecycle CLI" <<<"$out"; then
  echo "PASS: help printed for no args"
else
  echo "FAIL: help should be printed for no args"
  echo "$out"
  exit 1
fi

# ── Test 2: help is printed for `help` verb ──────────────────────────
out=$("$CONCORD_DEV" help 2>&1)
if grep -q "Concord Voice — Dev Lifecycle CLI" <<<"$out"; then
  echo "PASS: help printed for 'help' verb"
else
  echo "FAIL: 'help' verb should print help"
  exit 1
fi

# ── Test 3: unknown verb exits 1 with error message ──────────────────
if "$CONCORD_DEV" nonsense 2>/dev/null; then
  echo "FAIL: unknown verb should exit non-zero"
  exit 1
else
  rc=$?
  if [[ $rc -eq 1 ]]; then
    echo "PASS: unknown verb exits with code 1"
  else
    echo "FAIL: unknown verb should exit code 1, got $rc"
    exit 1
  fi
fi

# ── Test 4: validate-config invokes docker compose config ────────────
export DOCKER_LOG=$(mktemp)
cat > "$STUB_DIR/docker" <<'STUB'
#!/usr/bin/env bash
echo "$*" >> "$DOCKER_LOG"
exit 0
STUB
chmod +x "$STUB_DIR/docker"

if PATH="$STUB_DIR:$PATH" "$CONCORD_DEV" validate-config >/dev/null 2>&1; then
  if grep -q "compose -f .*docker-compose.yml -f .*docker-compose.dev.yml config --quiet" "$DOCKER_LOG"; then
    echo "PASS: validate-config invokes 'docker compose ... config --quiet'"
  else
    echo "FAIL: validate-config did not call config --quiet with both overlays"
    cat "$DOCKER_LOG"
    exit 1
  fi
else
  echo "FAIL: validate-config exited non-zero when docker stub succeeds"
  exit 1
fi

# ── Test 5: validate-config exits non-zero when compose config fails ─
cat > "$STUB_DIR/docker" <<'STUB'
#!/usr/bin/env bash
exit 1
STUB
chmod +x "$STUB_DIR/docker"

if PATH="$STUB_DIR:$PATH" "$CONCORD_DEV" validate-config >/dev/null 2>&1; then
  echo "FAIL: validate-config should exit non-zero when docker fails"
  exit 1
else
  echo "PASS: validate-config exits non-zero when compose config fails"
fi

# ── Test 6: global --verbose flag is consumed ────────────────────────
cat > "$STUB_DIR/docker" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$STUB_DIR/docker"

DEBUG_OUT=$(mktemp)
PATH="$STUB_DIR:$PATH" "$CONCORD_DEV" --verbose validate-config > "$DEBUG_OUT" 2>&1 || true
if grep -q "+ docker compose" "$DEBUG_OUT"; then
  echo "PASS: --verbose echoes docker commands to stderr"
else
  echo "FAIL: --verbose should echo docker commands"
  echo "DEBUG: captured output was:"
  cat "$DEBUG_OUT"
  exit 1
fi
rm -f "$DEBUG_OUT"

# ── Test 7: up rejects unknown flags ─────────────────────────────────
if "$CONCORD_DEV" up --not-a-flag 2>/dev/null; then
  echo "FAIL: up should reject unknown flag --not-a-flag"
  exit 1
else
  rc=$?
  if [[ $rc -eq 2 ]]; then
    echo "PASS: up rejects unknown flag with exit code 2 (pre-flight class)"
  else
    echo "FAIL: up should exit code 2 on unknown flag, got $rc"
    exit 1
  fi
fi

# Helper: assert that running CONCORD_DEV with given args exits with the
# expected code. Avoids the classic `rc=$? after fi` foot-gun by capturing
# rc INSIDE the else branch (where the failed-condition exit value lives).
assert_exit_code() {
  local expected="$1"
  local description="$2"
  shift 2
  if "$CONCORD_DEV" "$@" </dev/null >/dev/null 2>&1; then
    echo "FAIL: $description (expected exit $expected, got 0)"
    exit 1
  else
    local rc=$?
    if [[ $rc -eq $expected ]]; then
      echo "PASS: $description"
    else
      echo "FAIL: $description (expected exit $expected, got $rc)"
      exit 1
    fi
  fi
}

# ── Tests 8-12: other verbs reject unknown args with exit 2 ─────────
# These mirror Test 7 (up). All verbs that take no args reject extras
# with exit 2 to preserve the pre-flight class taxonomy.
assert_exit_code 2 "down rejects --bogus-flag with exit 2" down --bogus-flag
assert_exit_code 2 "status rejects extra positional with exit 2" status extra-arg
assert_exit_code 2 "restart rejects extra positional with exit 2" restart extra-arg
assert_exit_code 2 "rebuild rejects extra positional with exit 2" rebuild extra-arg
# freshstart's TTY confirmation prompt is auto-skipped via </dev/null redirect
# inside assert_exit_code; arg-rejection branch fires before the prompt.
assert_exit_code 2 "freshstart rejects extra positional with exit 2" freshstart extra-arg

# ── Tests 13-16: logs + up arg validation ────────────────────────────
assert_exit_code 2 "logs unknown service exits 2" logs unknown-service-name --no-follow
assert_exit_code 2 "logs -n rejects non-numeric value with exit 2" logs control-plane -n abc --no-follow
assert_exit_code 2 "up --clients rejects non-numeric value with exit 2" up --clients abc
assert_exit_code 2 "up --clients (missing value) exits 2 with clear message" up --clients

echo ""
echo "All args tests passed."
