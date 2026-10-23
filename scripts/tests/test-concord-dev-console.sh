#!/usr/bin/env bash
# Regression test for concord-dev.sh interactive console (REPL) and the
# dispatch_verb refactor that backs it. Drives console_repl with piped (non-TTY)
# stdin in a sourced subshell, honoring the script's source-vs-execute guard.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
TARGET="$PROJECT_ROOT/scripts/concord-dev.sh"

PASS=0
FAIL=0
pass() { echo "  ✓ $1"; PASS=$((PASS + 1)); }
fail() { echo "  ✗ $1"; FAIL=$((FAIL + 1)); }

# drive_repl <stdin-lines> [log-content|__NOLOG__]
# Feeds lines to console_repl with a sandbox LOG_DIR; echoes "<exit>\n<stdout>".
drive_repl() {
  local input="$1"
  local log="${2:-DEFAULT}"
  local tmp; tmp="$(mktemp -d)"
  case "$log" in
    __NOLOG__) : ;;  # leave the dir empty so dev-code finds no log
    DEFAULT)   printf '%s\n' 'time=t level=INFO msg="DEV MODE — email verification code" to=a@b.lan code=424242' > "$tmp/control-plane.log" ;;
    *)         printf '%s\n' "$log" > "$tmp/control-plane.log" ;;
  esac
  local out rc
  out="$(printf '%s' "$input" | LOG_DIR="$tmp" bash -c 'source "$1"; console_repl' _ "$TARGET" 2>/dev/null)"
  rc=$?
  rm -rf "$tmp"
  printf '%s\n%s' "$rc" "$out"
}

# ── help lists the verbs; exit returns 0 ────────────────────────────────────
out="$(drive_repl $'help\nexit\n')"
rc="${out%%$'\n'*}"; body="${out#*$'\n'}"
if [[ "$rc" == 0 && "$body" == *"dev-code"* && "$body" == *"freshstart"* ]]; then
  pass "help lists verbs; exit returns 0"
else
  fail "help/exit [rc=$rc body=$body]"
fi

# ── EOF (no explicit exit) terminates the loop cleanly ──────────────────────
out="$(drive_repl $'help\n')"
rc="${out%%$'\n'*}"
[[ "$rc" == 0 ]] && pass "EOF ends the loop (rc 0)" || fail "EOF ends loop [rc=$rc]"

# ── a read-only verb dispatches through the REPL ────────────────────────────
out="$(drive_repl $'dev-code\nexit\n')"
rc="${out%%$'\n'*}"; body="${out#*$'\n'}"
if [[ "$rc" == 0 && "$body" == *"424242"* ]]; then
  pass "dev-code dispatches inside the REPL (code shown)"
else
  fail "dev-code via REPL [rc=$rc body=$body]"
fi

# ── verb flags pass through (dev-code with an email filter) ──────────────────
out="$(drive_repl $'dev-code nobody@nowhere.lan\nexit\n')"
rc="${out%%$'\n'*}"; body="${out#*$'\n'}"
# the fixture code is to a@b.lan, so the filter yields no match but the loop
# must still survive and exit 0 (filter parsed as an arg, not a crash)
[[ "$rc" == 0 ]] && pass "verb flags pass through to the handler" || fail "flag passthrough [rc=$rc body=$body]"

# ── a FAILING verb does not kill the session (set +e isolation) ─────────────
out="$(drive_repl $'dev-code\nexit\n' __NOLOG__)"
rc="${out%%$'\n'*}"
[[ "$rc" == 0 ]] && pass "failing verb returns to prompt (session survives)" || fail "set+e isolation [rc=$rc]"

# ── unknown command is tolerated; loop continues to exit ────────────────────
out="$(drive_repl $'bogusverb\nexit\n')"
rc="${out%%$'\n'*}"
[[ "$rc" == 0 ]] && pass "unknown command tolerated (rc 0)" || fail "unknown tolerated [rc=$rc]"

# ── blank / whitespace-only lines are no-ops ────────────────────────────────
out="$(drive_repl $'\n   \nexit\n')"
rc="${out%%$'\n'*}"
[[ "$rc" == 0 ]] && pass "blank lines ignored" || fail "blank lines [rc=$rc]"

# ── verb_console refuses a non-interactive stdin (no hang) ──────────────────
rc=0; printf '' | bash -c 'source "$1"; verb_console' _ "$TARGET" >/dev/null 2>&1 || rc=$?
[[ "$rc" == 2 ]] && pass "verb_console rejects non-TTY stdin (exit 2)" || fail "non-TTY guard [rc=$rc]"

# ── dispatch_verb refactor: known verb works, unknown returns non-zero ──────
rc=0; bash -c 'source "$1"; dispatch_verb bogus' _ "$TARGET" >/dev/null 2>&1 || rc=$?
[[ "$rc" != 0 ]] && pass "dispatch_verb: unknown verb returns non-zero" || fail "dispatch_verb unknown [rc=$rc]"

# ── a failing builtin (clear) returns to the prompt, doesn't kill the loop ───
# Locks the Gitar #1534 fix: `clear` stubbed to exit 1 (mirrors an unset TERM)
# must not terminate the session via set -e — the loop must reach `exit`.
STUB_DIR="$(mktemp -d)"; LOG_TMP="$(mktemp -d)"
printf '#!/usr/bin/env bash\nexit 1\n' > "$STUB_DIR/clear"; chmod +x "$STUB_DIR/clear"
out="$(printf '%s' $'clear\nexit\n' | PATH="$STUB_DIR:$PATH" LOG_DIR="$LOG_TMP" \
  bash -c 'source "$1"; console_repl' _ "$TARGET" 2>/dev/null; echo "rc=$?")"
rm -rf "$STUB_DIR" "$LOG_TMP"
[[ "$out" == *"rc=0"* ]] && pass "failing builtin (clear) returns to prompt, loop survives" || fail "builtin set+e guard [$out]"

echo
echo "console: $PASS passed, $FAIL failed"
[[ "$FAIL" -eq 0 ]]
