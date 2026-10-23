#!/usr/bin/env bash
# check-no-single-return-json-assertion.sh — keeps Go tests off panic-prone
# single-return type assertions on decoded JSON (#2811).
#
# `x := body["k"].(map[string]interface{})` PANICS on a shape mismatch, and a
# panic aborts the whole package test binary: every other test in the package
# reports as failed and the assertion that actually tripped is buried in a
# stack trace. Use testhelpers.JSONField / JSONElem / JSONAs, or a plain
# two-return comma-ok.
#
# TWO LAYERS.
#   Layer 1 - type anchor. The assertion target set is closed, which
#             structurally excludes `.(type)` switches, `err.(*MyError)`, and
#             every struct/interface assertion. The worst over-matchers never
#             reach layer 2.
#   Layer 2 - legal-shape exclusion. Go permits the two-value form only as the
#             ENTIRE RHS of an assignment, so in a legal line the assertion's
#             closing paren is the last expression token. That makes this
#             exact rather than heuristic, and it is why the type must appear
#             in the layer-2 pattern too: without it,
#             `_, err := uuid.Parse(body["id"].(string))` reads as legal
#             because the line merely ENDS in `)`.
#             `range` is excluded explicitly -- a range form ends in `) {` and
#             would otherwise pass as legal. An optional trailing `// comment`
#             is permitted after the assertion: without it a perfectly safe
#             `x, ok := body["k"].(string) // note` read as a violation -- a
#             FALSE POSITIVE, which blocks correct code and teaches authors to
#             distrust the guard. (CodeRabbit, PR #2813.) Distinct from the
#             KNOWN CEILING below, which is an assertion written INSIDE a
#             comment -- still not handled.
#             EXACTLY ONE extra name, not `+`. Go's two-value assertion form has
#             exactly two LHS targets, so a `+` there also matched three-or-more
#             and let a genuine violation hide inside a wider multi-assign:
#               a, b, x := f(), g(), body["k"].(map[string]interface{})
#             which is valid Go, is still the single-return form, and still
#             panics. Caught by Gitar in review of PR #2813.
#
# THE DOUBLED BACKSLASHES IN $TYPE ARE LOAD-BEARING. awk processes escape
# sequences in string constants BEFORE the regex engine sees them, so a pattern
# passed via -v needs every backslash doubled. The single-escaped form fails
# with "awk: nonterminated character class" and then matches NOTHING -- a guard
# that reports a clean tree while enforcing nothing. Caught during authoring
# only because the self-test's must-NOT-flag cases were passing vacuously.
# If you edit $TYPE, run the self-test.
#
# KNOWN CEILING - stated, not implied. This is line-oriented; the following are
# NOT closed, and each was probed to zero occurrences in the tree at authoring:
#   * an assignment split across multiple lines
#   * a violating assertion nested inside a legal comma-ok RHS
#   * an assertion inside a trailing comment or a string literal
# Strong against the accidental reintroduction this issue was caused by; not a
# sandbox against a determined author. Do not describe it as exhaustive.
#
# Exit codes:
#   0 - clean (no violation, out of scope, allowlisted, or absent)
#   1 - at least one violation (printed file:line:text), a stale allowlist
#       entry, OR a file that exists but could not be read. A guard that cannot
#       read a file must not report it clean.
set -uo pipefail

# Overridable so the self-test can exercise the allowlist ratchet hermetically
# instead of mutating the repo's real allowlist.
ALLOWLIST="${JSON_ASSERT_ALLOWLIST:-$(dirname "$0")/json-assertion-allowlist.txt}"
TYPE='\\.\\((map\\[string\\](interface\\{\\}|any)|\\[\\](interface\\{\\}|any)|string|float64|bool|int64|int)\\)'
rc=0

# scan_file FILE — prints "file:line:text" per violation. ONE definition: the
# main loop and the allowlist ratchet must agree on what a violation is, or the
# ratchet could clear an entry the scanner still flags (or vice versa).
scan_file() {
  awk -v t="$TYPE" '
    /^[[:space:]]*\/\// { next }
    $0 ~ t {
      # A legal comma-ok carries EXACTLY ONE assertion. The shape test alone
      # anchors only the LAST one, so a line with two lets the first hide:
      #   a, ok := body["a"].(string), body["b"].(string)
      #   if x, ok := body["a"].(string); ok && body["b"].(string) == x {
      # Both are valid Go, both still panic, and both read as legal without
      # this count. (CodeRabbit, PR #2813.)
      line = $0
      # Strip the trailing comment BEFORE counting. Otherwise a type token
      # written inside the comment inflates the count and a legal single
      # assertion is flagged:
      #   x, ok := body["k"].(string) // fallback to .(int)
      # That is a false positive introduced by the count itself, caught by
      # Gitar on PR #2813 -- the earlier trailing-comment probes all used
      # comments with no type token in them, so they missed it.
      # Ceiling: a "//" inside a string literal truncates the count early. That
      # can only UNDER-count, i.e. degrade to the pre-count behaviour, never
      # manufacture a false positive.
      sub(/\/\/.*$/, "", line)
      n = gsub(t, "&", line)
      legal = (n == 1) && ($0 ~ ("^[[:space:]]*(if[[:space:]]+)?[A-Za-z_][A-Za-z0-9_]*([[:space:]]*,[[:space:]]*[A-Za-z_][A-Za-z0-9_]*)[[:space:]]*:?=[[:space:]]*[^=].*" t "[[:space:]]*(;[^{]*)?\\{?[[:space:]]*(\\/\\/.*)?$"))
      if (legal && $0 !~ /:?=[[:space:]]*range[[:space:]]/) next
      printf "%s:%d:%s\n", FILENAME, FNR, $0
    }' "$1"
}

for f in "$@"; do
  # Normalize the "./" that `find .` prefixes but pre-commit does not. The
  # allowlist stores repo-relative paths WITHOUT it, and the membership test is
  # an exact match (grep -qxF), so an un-normalized "./" silently defeats every
  # allowlist entry -- the guard then reports 587 violations on a green tree.
  f="${f#./}"
  case "$f" in *_test.go) ;; *) continue ;; esac
  [ -f "$f" ] || continue   # -f not -e: a dir or device named *_test.go must not be scanned
  if [ ! -r "$f" ]; then
    echo "$f: cannot read (guard fails closed)" >&2
    rc=1
    continue
  fi
  if [ -f "$ALLOWLIST" ] && grep -qxF "$f" "$ALLOWLIST"; then
    continue
  fi
  out=$(scan_file "$f")
  awk_rc=$?
  if [ $awk_rc -ne 0 ]; then
    echo "$f: scan failed (guard fails closed)" >&2
    rc=1
    continue
  fi
  if [ -n "$out" ]; then
    printf '%s\n' "$out"
    rc=1
  fi
done

# Stale-allowlist ratchet. An entry naming a file that no longer exists would
# silently keep some future file exempt; an entry whose file is now clean would
# keep a swept package permanently unguarded. Both are errors, which is what
# forces each sweep PR to delete its own entries.
if [ -f "$ALLOWLIST" ]; then
  # `|| [ -n "$entry" ]` is load-bearing: a final line with no trailing
  # newline is dropped by `read`, but grep -qxF above still MATCHES it -- so
  # such an entry would keep suppressing violations while being exempt from
  # both ratchet halves. That is the permanent silent exemption this block
  # exists to prevent. (code-reviewer + security-reviewer, PR #2813.)
  while IFS= read -r entry || [ -n "$entry" ]; do
    [ -z "$entry" ] && continue
    case "$entry" in \#*) continue ;; esac
    if [ ! -e "$entry" ]; then
      echo "allowlist: $entry no longer exists — remove the entry"
      rc=1
      continue
    fi
    # The now-clean half of the ratchet. Without this the allowlist is a
    # one-way exemption: allowlisted files are skipped by the scanner above, so
    # a sweep PR that cleans a file but forgets its entry leaves that file
    # PERMANENTLY unguarded against reintroduction, and the guard stays green.
    # Caught in review of PR #2813 -- the comment above promised this and the
    # code did not do it.
    if [ ! -r "$entry" ]; then
      # Symmetric with the scanner above: a guard that cannot read a file must
      # not report it clean. Without this arm the ratchet silently accepts it.
      echo "allowlist: $entry cannot be read (guard fails closed)" >&2
      rc=1
      continue
    fi
    if ! entry_hits=$(scan_file "$entry"); then
      echo "allowlist: $entry scan failed (guard fails closed)" >&2
      rc=1
    elif [ -z "$entry_hits" ]; then
      echo "allowlist: $entry is now clean — remove the entry"
      rc=1
    fi
  done < "$ALLOWLIST"
fi

exit $rc
