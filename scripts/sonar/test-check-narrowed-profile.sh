#!/usr/bin/env bash
# scripts/sonar/test-check-narrowed-profile.sh
#
# Hermetic self-test for scripts/sonar/check-narrowed-profile.sh.
# NO network calls — a fake `curl` shim on PATH returns fixture JSON keyed by
# the API request's query string. Run manually:
#   bash scripts/sonar/test-check-narrowed-profile.sh
# Exits non-zero if any case fails.
#
# Mirrors the harness pattern from scripts/test-check-compose-manifest-coverage.sh.

set -euo pipefail

CHECK="$(cd "$(dirname "$0")" && pwd)/check-narrowed-profile.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

PASSES=0
FAILS=0
FAIL_DETAILS=()

# ── Fake curl shim ───────────────────────────────────────────────────────────
# The check's scurl helper calls `curl --fail --silent --show-error -u <tok> <url>`.
# Our shim ignores every flag and inspects the URL (last argument) to decide
# which fixture to emit. It writes the fixture for the current FIXTURE_DIR,
# selected per-case via an env var. This keeps the check unmodified (true
# black-box test of the production code path).
mkdir -p "$TMP/bin"
cat > "$TMP/bin/curl" <<'SHIM'
#!/usr/bin/env bash
# Hermetic curl stub: emit a fixture based on the requested URL.
set -euo pipefail
url=""
for a in "$@"; do url="$a"; done   # last arg is the URL

dir="${FIXTURE_DIR:?FIXTURE_DIR must be set by the test harness}"

# Detect language. qualityprofiles/search carries `language=<lang>`;
# rules/search carries no language= (it uses qprofile=<key>), so derive the
# language from our fixture key shape `qprofile=<lang>-...-key`.
lang=""
case "$url" in
  *language=js*|*qprofile=*js-*) lang="js" ;;
  *language=ts*|*qprofile=*ts-*) lang="ts" ;;
  *language=go*|*qprofile=*go-*) lang="go" ;;
esac

case "$url" in
  *"/qualityprofiles/search"*)
    f="$dir/qp_${lang}.json"
    [ -f "$f" ] || { echo "stub: missing fixture $f" >&2; exit 22; }
    cat "$f"
    ;;
  *"/rules/search"*)
    # Regression-lock the corrected selector (2026-06-17 taxonomy-drift finding):
    # modern SonarCloud taint rules dropped the `injection` tag, so the query
    # MUST scope by repositories=<lang>security and MUST NOT use tags=injection
    # (which matches 0 org-wide and made the guard pass vacuously).
    case "$url" in
      *tags=injection*) echo "stub: STALE query — rules/search must not use tags=injection; use repositories=<lang>security" >&2; exit 33 ;;
    esac
    case "$url" in
      *repositories=*security*) : ;;
      *) echo "stub: rules/search must scope by repositories=<lang>security" >&2; exit 33 ;;
    esac
    f="$dir/rules_${lang}.json"
    [ -f "$f" ] || { echo "stub: missing fixture $f" >&2; exit 22; }
    cat "$f"
    ;;
  *)
    echo "stub: unrecognized URL: $url" >&2
    exit 22
    ;;
esac
SHIM
chmod +x "$TMP/bin/curl"

# ── Fixture builders ─────────────────────────────────────────────────────────
# Case (a): stock "Sonar way" assigned → wrong profile → DRIFT (exit 1).
mkdir -p "$TMP/stock"
for lang in js ts go; do
  cat > "$TMP/stock/qp_${lang}.json" <<EOF
{ "profiles": [ { "key": "stock-${lang}-key", "name": "Sonar way", "language": "${lang}" } ] }
EOF
  # rules fixture present but never reached (wrong profile short-circuits).
  echo '{ "total": 99 }' > "$TMP/stock/rules_${lang}.json"
done

# Case (b): narrowed profile assigned + zero active injection rules → clean (exit 0).
mkdir -p "$TMP/narrowed"
for lang in js ts go; do
  cat > "$TMP/narrowed/qp_${lang}.json" <<EOF
{ "profiles": [ { "key": "narrowed-${lang}-key", "name": "Concord way (${lang}) — ADR-0017 narrowed", "language": "${lang}" } ] }
EOF
  echo '{ "total": 0 }' > "$TMP/narrowed/rules_${lang}.json"
done

# Case (c): narrowed profile assigned BUT taint rules re-activated → DRIFT (exit 1).
# Exercises the active_total != 0 branch (narrowing reverted in place, not via a
# profile swap) — the primary recurrence scenario this guard exists to catch.
mkdir -p "$TMP/reactivated"
for lang in js ts go; do
  cat > "$TMP/reactivated/qp_${lang}.json" <<EOF
{ "profiles": [ { "key": "narrowed-${lang}-key", "name": "Concord way (${lang}) — ADR-0017 narrowed", "language": "${lang}" } ] }
EOF
  echo '{ "total": 9 }' > "$TMP/reactivated/rules_${lang}.json"
done

# Case (d): operational error → exit 3 (distinct from drift). Correct profile is
# assigned, but the rules/search query fails (no rules fixture → stub exits 22 →
# scurl --fail propagates → op_fail). Locks the op-error path away from drift.
mkdir -p "$TMP/operr"
for lang in js ts go; do
  cat > "$TMP/operr/qp_${lang}.json" <<EOF
{ "profiles": [ { "key": "narrowed-${lang}-key", "name": "Concord way (${lang}) — ADR-0017 narrowed", "language": "${lang}" } ] }
EOF
  # intentionally NO rules_${lang}.json → stub exits non-zero → check exits 3.
done

# ── Runner ───────────────────────────────────────────────────────────────────
run() {
  local label="$1"
  local fixture_dir="$2"
  local expect_exit="$3"

  local actual_exit
  # PATH-prepend the stub dir so `curl` resolves to our shim; pass a dummy token
  # so the SONAR_TOKEN guard passes (no real network is touched).
  (
    export PATH="$TMP/bin:$PATH"
    export FIXTURE_DIR="$fixture_dir"
    export SONAR_TOKEN="test-token-123"
    "$CHECK"
  ) >/dev/null 2>&1 && actual_exit=0 || actual_exit=$?

  if [ "$actual_exit" = "$expect_exit" ]; then
    PASSES=$((PASSES + 1))
    printf '  \033[32m✓\033[0m %s (expected exit=%s, got %s)\n' "$label" "$expect_exit" "$actual_exit"
  else
    FAILS=$((FAILS + 1))
    printf '  \033[31m✗\033[0m %s (expected exit=%s, got %s)\n' "$label" "$expect_exit" "$actual_exit"
    FAIL_DETAILS+=("$label")
  fi
}

echo "=== Case (a): stock 'Sonar way' assigned → DRIFT (exit 1 expected) ==="
run "case a: stock Sonar way profile → drift" "$TMP/stock" 1

echo ""
echo "=== Case (b): narrowed profile + zero active taint rules → clean (exit 0 expected) ==="
run "case b: narrowed profile, zero active taint rules → clean" "$TMP/narrowed" 0

echo ""
echo "=== Case (c): narrowed profile + active taint rules → DRIFT (exit 1 expected) ==="
run "case c: narrowed profile, active taint rules → drift" "$TMP/reactivated" 1

echo ""
echo "=== Case (d): rules/search API failure → operational error (exit 3 expected) ==="
run "case d: rules/search failure → op-error" "$TMP/operr" 3

echo ""
echo "──────────────────────────────────────"
echo "Results: $PASSES passed, $FAILS failed"
if [ "$FAILS" -gt 0 ]; then
  echo "Failed cases:"
  for c in "${FAIL_DETAILS[@]}"; do
    echo "  - $c"
  done
  exit 1
fi
echo "All tests passed."
