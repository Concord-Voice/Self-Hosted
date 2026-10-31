#!/usr/bin/env bash
# Tests for scripts/check-env-file.sh — validates that the linter
# rejects unquoted env values containing shell metacharacters and
# mis-quoted patterns. Coverage:
#
# Rejected metachars: <, >, ;, space, backtick, $, |, &, \
# Rejected patterns:
#   - Unclosed quote (e.g. KEY="missing close)
#   - Quoted value with trailing extra content (e.g. KEY="foo" "bar")
#
# Accepted patterns:
#   - Balanced double or single quotes around the entire value
#   - Alphanumeric / number / CIDR / comma-separated-list values
#   - Comment lines (# ...) and blank lines
#
# Refs #725 (Track 4 + post-review hardening).
set -u

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
LINTER="$SCRIPT_DIR/check-env-file.sh"

if [ ! -x "$LINTER" ]; then
  echo "FAIL: $LINTER not found or not executable"
  exit 1
fi

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

pass=0
fail=0

run_case() {
  local name="$1"
  local content="$2"
  local expect_exit="$3" # 0 for accept, 1 for reject

  local f="$TMPDIR/${name}.env"
  printf '%s\n' "$content" > "$f"

  "$LINTER" "$f" >/dev/null 2>&1
  local actual_exit=$?

  if [ "$actual_exit" = "$expect_exit" ]; then
    echo "  ok: $name"
    pass=$((pass + 1))
  else
    echo "  FAIL: $name (expected exit $expect_exit, got $actual_exit)"
    fail=$((fail + 1))
  fi
}

echo "check-env-file.sh test cases:"

# Reject patterns (expect exit 1)
run_case "reject-angle-bracket"    'SMTP_FROM=Concord Voice <noreply@x.test>' 1
run_case "reject-semicolon"        'EVIL=value; rm -rf /'                     1
run_case "reject-unquoted-space"   'LABEL=hello world'                        1
run_case "reject-backtick"         'EVIL=`id`'                                1
run_case "reject-unclosed-quote"   'BROKEN="missing close'                    1
# #725 post-review additions — metachars previously missed:
run_case "reject-dollar-var"       'EVIL=$PATH'                               1
run_case "reject-dollar-paren"     'EVIL=$(id)'                               1
run_case "reject-pipe"             'EVIL=a|b'                                 1
run_case "reject-ampersand"        'EVIL=a&b'                                 1
run_case "reject-backslash"        'EVIL=a\b'                                 1
run_case "reject-quoted-then-extra" 'KEY="foo" "bar"'                         1
run_case "reject-single-quoted-then-extra" "KEY='foo' 'bar'"                  1
# Command substitution through the key or a double-quoted value
run_case "reject-key-substitution"  'A$(touch x)=y'                             1
run_case "reject-key-lowercase"     'Ab=y'                                      1
run_case "reject-dq-dollar-paren"   'T="$(touch x)"'                            1
run_case "reject-dq-dollar-var"     'T="$HOME/x"'                               1
run_case "reject-dq-backtick"       'T="`id`"'                                  1

# Accept patterns (expect exit 0)
run_case "accept-quoted"           'SMTP_FROM="Concord Voice <noreply@x.test>"' 0
run_case "accept-single-quoted"    "KEY='literal \$PATH stays literal'"        0
run_case "accept-simple"           'DB_HOST=postgres'                          0
run_case "accept-number"           'PORT=5432'                                 0
run_case "accept-cidr"             'TRUSTED_PROXY_CIDRS=172.19.0.0/16'         0
run_case "accept-comma-list"       'ORIGINS=https://a.test,https://b.test'     0
run_case "accept-comment-line"     '# comment with < > ; space'                0
run_case "accept-blank-line"       ''                                          0

# #841 — @Q output shapes. ${VAR@Q} produces single-quoted output that
# bash cannot interpret regardless of value contents. The linter MUST
# accept these forms; failing to accept them breaks provision-secrets.yml.
#
# Accept patterns: representative @Q outputs (expect exit 0)
run_case "accept-q-simple"          "MOTD='hello world'"                       0
run_case "accept-q-dollar"          "BUDGET='cost\$100'"                       0
run_case "accept-q-multisegment"    "NAME='Joe'\\''s server'"                  0
run_case "accept-q-newline"         "MOTD=\$'line one\\nline two'"             0
run_case "accept-q-tab"             "LIST=\$'a\\tb\\tc'"                       0
run_case "accept-q-empty"           "MAYBE=''"                                 0
run_case "accept-q-backtick"        "EVIL='\`id\`'"                            0
run_case "accept-q-dollar-paren"    "EVIL='\$(id)'"                            0

# Reject patterns: malformed @Q-shaped input (expect exit 1)
run_case "reject-q-bad-escape"      "BAD='foo'bar'"                            1
run_case "reject-q-bad-ansic"       "BAD=\$'foo'bar'"                          1

# #841 review cycle — additional state-machine coverage
# Multi-segment state machine: cycle through IN_ESCAPE/IN_ESCAPE2 multiple times
run_case "accept-q-two-escapes"        "X='a'\\''b'\\''c'"                       0
# Empty leading segment
run_case "accept-q-leading-escape"     "X=''\\''foo'"                            0
# Empty trailing segment (last segment is just an escape, then close)
run_case "accept-q-trailing-escape"    "X='foo'\\'''"                            0
# Truncated mid-escape (state machine terminal IN_ESCAPE)
run_case "reject-q-truncated-escape"   "X='foo'\\"                               1
# Truncated mid-escape (state machine terminal IN_ESCAPE2)
run_case "reject-q-truncated-escape2"  "X='foo'\\'"                              1
# ANSI-C trailing-backslash (Fix #1 CRITICAL — bounds check)
run_case "reject-ansic-trailing-bs"    "BAD=\$'foo\\'"                           1
# ANSI-C value with only an escaped backslash (no trailing — valid)
run_case "accept-ansic-only-escape"    "X=\$'\\n\\t\\r'"                         0

echo ""
echo "pass: $pass, fail: $fail"
# Clamp exit to 0/1 — raw "$fail" truncates to 8 bits and could falsely
# return 0 on ≥ 256 failures. Per #725 Track 4 review (silent-failure-hunter).
if [ "$fail" -gt 0 ]; then
  exit 1
fi
exit 0
