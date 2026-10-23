#!/usr/bin/env bash
# lint-workflow-injection.test.sh — verifies actionlint catches P14 patterns.
#
# Issue: #836. Spec:
# [internal]specs/2026-05-20-836-workflow-injection-guard-design.md
#
# Invokes actionlint against each fixture in
# .github/scripts/tests/fixtures/workflow-injection/ and asserts the
# expected outcome. Exits 0 on full success, 1 on any failure.
#
# Run from repo root.

set -euo pipefail

FIXTURES_DIR=".github/scripts/tests/fixtures/workflow-injection"
CONFIG=".actionlint.yaml"

if ! command -v actionlint >/dev/null 2>&1; then
  echo "ERROR: actionlint not on PATH. Install: brew install actionlint"
  exit 2
fi

if [ ! -d "$FIXTURES_DIR" ]; then
  echo "ERROR: fixtures directory missing: $FIXTURES_DIR"
  exit 2
fi

FAIL=0

# Cases 01-04 must trigger an actionlint finding (exit non-zero).
for f in "$FIXTURES_DIR"/case-0{1,2,3,4}-*.yml; do
  if actionlint -config-file="$CONFIG" "$f" >/dev/null 2>&1; then
    echo "FAIL: $f was accepted by actionlint (regression — pattern not caught)"
    FAIL=1
  else
    echo "OK:   $f rejected by actionlint as expected"
  fi
done

# Case 05 must pass cleanly (safe form).
for f in "$FIXTURES_DIR"/case-05-*.yml; do
  if actionlint -config-file="$CONFIG" "$f" >/dev/null 2>&1; then
    echo "OK:   $f passed actionlint as expected (safe env-block form)"
  else
    echo "FAIL: $f rejected by actionlint (false positive on safe form)"
    actionlint -config-file="$CONFIG" "$f" || true  # show the error
    FAIL=1
  fi
done

if [ "$FAIL" -eq 0 ]; then
  echo ""
  echo "PASS: all 5 fixtures behaved as expected."
  exit 0
else
  echo ""
  echo "FAIL: one or more fixtures behaved unexpectedly. See output above."
  exit 1
fi
