#!/usr/bin/env bash
# Tests for pre-flight error paths (exit code 2 class).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
CONCORD_DEV="$PROJECT_ROOT/scripts/concord-dev.sh"

# ── Set up stub binaries ─────────────────────────────────────────────
STUB_DIR=$(mktemp -d)
trap 'rm -rf "$STUB_DIR"' EXIT

# Stub `docker` to simulate the daemon not running.
cat > "$STUB_DIR/docker" <<'STUB'
#!/usr/bin/env bash
if [[ "$1" == "info" ]]; then
  echo "Cannot connect to the Docker daemon" >&2
  exit 1
fi
exit 0
STUB
chmod +x "$STUB_DIR/docker"

# Stub `lsof` to report ports as free (test 1 + 2).
cat > "$STUB_DIR/lsof" <<'STUB'
#!/usr/bin/env bash
exit 1  # port free
STUB
chmod +x "$STUB_DIR/lsof"

# ── Test 1: missing docker daemon → exit 2 ───────────────────────────
if PATH="$STUB_DIR:$PATH" "$CONCORD_DEV" up --skip-client 2>/dev/null; then
  echo "FAIL: up should exit 2 when docker is unreachable"
  exit 1
else
  rc=$?
  if [[ $rc -eq 2 ]]; then
    echo "PASS: up exits 2 when docker daemon is unreachable"
  else
    echo "FAIL: up should exit 2 on docker fail, got $rc"
    exit 1
  fi
fi

# ── Test 2: port 8080 in use → exit 2 ───────────────────────────────
# Stub docker to succeed (so the docker check passes).
cat > "$STUB_DIR/docker" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$STUB_DIR/docker"

# Stub lsof to claim port 8080 is in use.
cat > "$STUB_DIR/lsof" <<'STUB'
#!/usr/bin/env bash
if [[ "$*" == *":8080"* ]]; then
  exit 0  # port busy
fi
exit 1
STUB
chmod +x "$STUB_DIR/lsof"

if PATH="$STUB_DIR:$PATH" "$CONCORD_DEV" up --skip-client 2>/dev/null; then
  echo "FAIL: up should exit 2 when port 8080 is in use"
  exit 1
else
  rc=$?
  if [[ $rc -eq 2 ]]; then
    echo "PASS: up exits 2 when port 8080 is busy"
  else
    echo "FAIL: up should exit 2 on port-busy, got $rc"
    exit 1
  fi
fi

echo ""
echo "All preflight tests passed."
