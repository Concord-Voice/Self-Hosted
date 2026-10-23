#!/usr/bin/env bash
# Tests for concord-dev.sh env_drift_warn() — the bootstrap_env hardening that
# warns when an existing .env is missing keys .env.example now defines. Sources
# the script (honoring its source-vs-execute guard) and calls env_drift_warn
# with sandbox files; the warning is emitted on stderr.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
CONCORD_DEV="$PROJECT_ROOT/scripts/concord-dev.sh"

PASS=0
FAIL=0
pass() { echo "  ✓ $1"; PASS=$((PASS + 1)); }
fail() { echo "  ✗ $1"; FAIL=$((FAIL + 1)); }

# run_drift <example-content> <env-content> [setup]
# Writes sandbox .env/.env.example, calls env_drift_warn, echoes its STDERR.
run_drift() {
  local ex="$1" en="$2" setup="${3:-:}"
  local tmp; tmp="$(mktemp -d)"
  printf '%s\n' "$ex" > "$tmp/.env.example"
  printf '%s\n' "$en" > "$tmp/.env"
  local err
  err="$(bash -c 'source "$1"; '"$setup"'; env_drift_warn "$2" "$3"' \
    _ "$CONCORD_DEV" "$tmp/.env" "$tmp/.env.example" 2>&1 >/dev/null)"
  rm -rf "$tmp"
  printf '%s' "$err"
}

EX=$'POSTGRES_PASSWORD=x\nJWT_SECRET=y\nPUBLIC_IP=127.0.0.1\nTURN_SECRET=z'

# ── a missing key is warned and named ──────────────────────────────────────
out="$(run_drift "$EX" $'POSTGRES_PASSWORD=x\nJWT_SECRET=y\nTURN_SECRET=z')"
if [[ "$out" == *"missing keys"* && "$out" == *"PUBLIC_IP"* ]]; then
  pass "missing key (PUBLIC_IP) warned and named"
else
  fail "missing-key warning [out=$out]"
fi

# ── all missing keys are listed ────────────────────────────────────────────
out="$(run_drift "$EX" $'POSTGRES_PASSWORD=x')"
if [[ "$out" == *"JWT_SECRET"* && "$out" == *"PUBLIC_IP"* && "$out" == *"TURN_SECRET"* ]]; then
  pass "all missing keys listed"
else
  fail "multi-missing [out=$out]"
fi

# ── no drift (.env has every example key) → silent ─────────────────────────
out="$(run_drift "$EX" "$EX")"
[[ -z "$out" ]] && pass "no drift → no warning" || fail "false-positive [out=$out]"

# ── .env superset (extra keys) → silent (drift is example→env only) ────────
out="$(run_drift "$EX" "$EX"$'\nPGADMIN_PASSWORD=extra')"
[[ -z "$out" ]] && pass ".env superset → no warning" || fail "superset false-positive [out=$out]"

# ── --quiet suppresses the warning ─────────────────────────────────────────
out="$(run_drift "$EX" $'POSTGRES_PASSWORD=x' 'QUIET=1')"
[[ -z "$out" ]] && pass "--quiet suppresses warning" || fail "quiet not honored [out=$out]"

# ── comments / blank lines in .env are not treated as keys ─────────────────
out="$(run_drift "$EX" $'# a comment\nPOSTGRES_PASSWORD=x\nJWT_SECRET=y\nPUBLIC_IP=1\nTURN_SECRET=z\n')"
[[ -z "$out" ]] && pass "comments/blanks ignored (no false drift)" || fail "comment handling [out=$out]"

# ── export/indented keys in .env are NOT counted present (Gitar #1550) ──────
# Docker Compose's env-file parser ignores `export ` and leading whitespace, so
# a key written that way in .env is unreadable by compose. The check is column-0
# only, so such a key reads as ABSENT and still warns — aligned with what compose
# actually consumes (tolerating these forms would mask the real interpolation
# failure). .env.example declares PUBLIC_IP + JWT_SECRET plainly; .env has them
# only as `export`/indented → both must warn.
out="$(run_drift $'PUBLIC_IP=1\nJWT_SECRET=2' $'export PUBLIC_IP=1\n  JWT_SECRET=2')"
if [[ "$out" == *"PUBLIC_IP"* && "$out" == *"JWT_SECRET"* ]]; then
  pass "export/indented keys in .env still warn (compose can't read them)"
else
  fail "export/indent compose-alignment [out=$out]"
fi

echo
echo "env-drift: $PASS passed, $FAIL failed"
[[ "$FAIL" -eq 0 ]]
