#!/usr/bin/env bash
# test-check-no-single-return-json-assertion.sh — locks the #2811 guard's regex.
#
# Without this, an edit that weakens the pattern produces a silently permissive
# guard with no signal anywhere (the #2680 lesson). It is not hypothetical here:
# the guard's first draft passed its type pattern to awk single-escaped, which
# errored and then matched nothing. Every must-NOT-flag case still "passed",
# because a guard that matches nothing flags nothing. That is why this suite
# asserts on BOTH directions and treats an awk error as a distinct failure.
set -uo pipefail

GUARD="$(cd "$(dirname "$0")" && pwd)/check-no-single-return-json-assertion.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
fail=0

EMPTY_AL="$TMP/empty-allowlist.txt"
: > "$EMPTY_AL"

probe() { # probe <expected-exit 0|1> <label> <line>
  # Hermetic: without an explicit empty allowlist these probes also assert the
  # REPO's allowlist is non-stale, so a genuinely stale entry would surface as a
  # confusing FAIL[comma-ok] instead of the guard's own message.
  printf '%s\n' "$3" > "$TMP/x_test.go"
  err=$(JSON_ASSERT_ALLOWLIST="$EMPTY_AL" "$GUARD" "$TMP/x_test.go" 2>&1 >/dev/null)
  got=$?
  case "$err" in
    *awk:*|*"fails closed"*)
      echo "FAIL[$2] guard errored — the fail-open shape: $err"
      fail=1
      return
      ;;
  esac
  if [ "$got" -ne "$1" ]; then
    echo "FAIL[$2] want exit $1, got $got   line: $3"
    fail=1
  fi
}

# --- MUST FLAG -------------------------------------------------------------
probe 1 single-return        'x := body["k"].(map[string]interface{})'
probe 1 any-spelling         'x := body["k"].(map[string]any)'
probe 1 slice-any            'x := body["k"].([]any)'
probe 1 range-any            '	for _, s := range h["services"].([]any) {'
probe 1 nested-arg           '	_, err := uuid.Parse(body["id"].(string))'
probe 1 nested-multi-assign  '	uid, _, _, err := auth.ValidateTicket(ctx, body["ticket"].(string))'
probe 1 assert-arg           '	assert.Equal(t, 2, conv["unread"].(float64))'
probe 1 chained              '	y := r["o"].(map[string]interface{})["id"].(string)'
probe 1 index-elem           '	c := convs[0].(map[string]interface{})'
# A single-return assertion hidden inside a WIDER multi-assign. Valid Go, still
# panics. The layer-2 quantifier must be bounded to exactly one extra name or
# this reads as a legal comma-ok. Gitar, PR #2813.
probe 1 wide-multi-assign    '	a, b, x := f(), g(), body["k"].(map[string]interface{})'
probe 1 wide-multi-assign-4  '	a, b, c, x := 1, 2, 3, body["k"].([]interface{})'

# --- MUST NOT FLAG ---------------------------------------------------------
probe 0 comma-ok             '	x, ok := body["k"].(map[string]interface{})'
probe 0 comma-ok-discard     '	x, _ := body["k"].(map[string]interface{})'
probe 0 comma-ok-named       '	x, found := body["k"].(map[string]interface{})'
probe 0 comma-ok-if          '	if x, ok := body["k"].(string); ok {'
probe 0 type-switch          '	switch v := x.(type) {'
probe 0 error-assert         '	e, ok := err.(*MyError)'
probe 0 full-line-comment    '	// x := body["k"].(map[string]interface{})'
probe 0 var-decl             '	var x map[string]interface{}'
probe 0 helper-call          '	x := testhelpers.JSONField[string](t, body, "k")'
# A trailing comment on a LEGAL comma-ok must not be a violation -- flagging it
# is a false positive that blocks correct code. (CodeRabbit, PR #2813.)
probe 0 comma-ok-trailing-c  '	x, ok := body["k"].(string) // response field'
probe 0 comma-ok-trailing-c2 '	x, ok := body["k"].(map[string]interface{}) // note'
# ...but a trailing comment must NOT launder a real violation.
probe 1 violation-trailing-c '	x := body["k"].(string) // still a violation'
probe 1 wide-multi-trailing  '	a, b, x := f(), g(), body["k"].([]interface{}) // nope'
# TWO assertions on one line: the shape test anchors only the LAST one, so the
# first hides unless the legal branch requires exactly one. Both shapes below
# are valid Go and both still panic. (CodeRabbit, PR #2813.)
probe 1 two-assertions-rhs   '	a, ok := body["a"].(string), body["b"].(string)'
probe 1 two-assertions-if    '	if x, ok := body["a"].(string); ok && body["b"].(string) == x {'
# A TYPE TOKEN INSIDE the trailing comment must not inflate the count. The
# earlier trailing-comment probes all used comments with no type token, which
# is exactly why they missed this. (Gitar, PR #2813.)
probe 0 comment-with-type    '	x, ok := body["k"].(string) // fallback to .(int)'
probe 0 comment-with-type2   '	x, ok := body["k"].(string) // see .(bool) elsewhere'

# --- ALLOWLIST RATCHET ------------------------------------------------------
# The allowlist suppresses violations, so it must not become a one-way
# exemption: a file swept clean that keeps its entry would stay permanently
# unguarded against reintroduction. Both halves of the ratchet are probed.
AL="$TMP/allowlist.txt"
SUBJ="$TMP/subject_test.go"

ratchet() { # ratchet <expected-exit> <label>
  err=$(JSON_ASSERT_ALLOWLIST="$AL" "$GUARD" "$SUBJ" 2>&1 >/dev/null)
  got=$?
  case "$err" in
    *awk:*) echo "FAIL[$2] guard errored: $err"; fail=1; return ;;
  esac
  if [ "$got" -ne "$1" ]; then
    echo "FAIL[$2] want exit $1, got $got"
    fail=1
  fi
}

# 1. dirty + allowlisted -> suppressed (this is what the allowlist is FOR)
printf 'x := body["k"].(map[string]interface{})\n' > "$SUBJ"
printf '%s\n' "$SUBJ" > "$AL"
ratchet 0 allowlist-suppresses-dirty

# 2. swept clean + entry left behind -> MUST error. Without this the allowlist
#    is a permanent exemption and the sweep PRs are unenforced.
printf 'x, ok := body["k"].(map[string]interface{})\n' > "$SUBJ"
ratchet 1 allowlist-now-clean-errors

# 3. final line with NO trailing newline -> must still be ratcheted. `read`
#    drops it but grep -qxF matches it, so it would suppress while being exempt.
printf 'x, ok := body["k"].(map[string]interface{})\n' > "$SUBJ"
printf '%s' "$SUBJ" > "$AL"          # deliberately no trailing newline
ratchet 1 allowlist-no-trailing-newline

# 4. an allowlisted entry that exists but cannot be read -> MUST fail closed,
#    symmetric with the scanner. Skipped when running as root (root reads 000).
printf '%s\n' "$SUBJ" > "$AL"
chmod 000 "$SUBJ" 2>/dev/null
if [ ! -r "$SUBJ" ]; then
  ratchet 1 allowlist-unreadable-fails-closed
fi
chmod 644 "$SUBJ" 2>/dev/null

# 5. entry naming a file that no longer exists -> MUST error
printf '%s\n' "$TMP/deleted_test.go" > "$AL"
printf 'x, ok := body["k"].(map[string]interface{})\n' > "$SUBJ"
ratchet 1 allowlist-missing-file-errors

# --- PATH-FORM AGNOSTICISM ---------------------------------------------------
# CI runs `find . ...` (paths prefixed "./"); pre-commit passes them bare. The
# allowlist stores them bare and the membership test is an exact match, so an
# un-normalized "./" silently defeats EVERY allowlist entry -- the guard then
# reports every allowlisted violation on an otherwise-green tree.
if ! (
  cd "$TMP" || exit 1
  mkdir -p rel
  printf 'x := body["k"].(map[string]interface{})\n' > rel/relative_test.go
  printf '%s\n' "rel/relative_test.go" > "$TMP/relal.txt"
  JSON_ASSERT_ALLOWLIST="$TMP/relal.txt" "$GUARD" "./rel/relative_test.go" >/dev/null 2>&1
); then
  echo "FAIL[path-form-dot-slash] a ./-prefixed path must match a bare allowlist entry"
  fail=1
fi

if [ "$fail" -eq 0 ]; then
  echo "OK: JSON-assertion guard behaves on all probes"
fi
exit $fail
