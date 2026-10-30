#!/usr/bin/env bash
#
# promote-unreleased-changelog.sh — turn `## [Prerelease]` into a dated release
# section and open a fresh empty `## [Prerelease]` above it.
#
# WHY THIS SHAPE. `verify-changelog.sh` (#1857) fails a desktop version bump that
# has no matching `## [<new-version>]` section at HEAD, so any automated bump must
# produce one. It must NOT produce it by summarising commit messages: the entries
# in this file are hand-written user-facing prose ("a friend link shared outside
# Concord used to lead nowhere useful…"), and machine-generated text would both
# read wrong and overwrite an editorial voice nobody asked a script to imitate.
#
# So this promotes what humans already wrote. Nothing is authored here — the
# `## [Prerelease]` body is moved verbatim under a dated heading. The one
# exception is the `**N Total Changes** | ...` tally line, which is recounted.
#
# THE EMPTINESS CHECK IS THE RELEASE DECISION, not a guard against a corner case.
# An empty `## [Prerelease]` means nobody has written release notes, which means
# there is nothing user-facing to ship — the correct answer is "do not release",
# and exit 3 says exactly that. Do not "fix" this into a bump-anyway path; that
# would ship releases whose notes are empty by construction.
#
# THE PREAMBLE CHECK IS A DEFECT REPORT, not a release decision — which is why it
# exits 1 where the emptiness check exits 3. A section with notes but no preamble
# HAS something to ship and its notes are incomplete, so the answer is "finish the
# notes", not "do not release". The rule MIRRORS extractPreamble in
# client/desktop/src/renderer/services/system/changelog.ts: the post-update modal
# renders `preamble || body`, so a section without one shows the reader every
# bullet instead of a summary (#3139).
#
# It is not a byte-for-byte port. This side also models what the renderer PAINTS
# -- sanitizeSchema.ts's strip list, comments, images, a link's label rather than
# its destination -- which extractPreamble does not inspect, so it rejects some
# preambles the mirror accepts. That direction refuses a promotion; the opposite
# direction is #3139 itself, and is what the cross-language pin in the test suite
# holds shut.
#
# Usage:
#   promote-unreleased-changelog.sh <new-version> [iso-date] [changelog-path]
#
# Exit codes:
#   0 — promoted; CHANGELOG.md rewritten
#   3 — `## [Prerelease]` is empty: nothing to release (NOT an error)
#   1 — the file does not have the structure this script requires, OR its
#       `[Prerelease]` notes carry no preamble
#   2 — usage error

set -euo pipefail

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
  sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
  exit 0
fi

if [[ $# -lt 1 || $# -gt 3 ]]; then
  echo "ERROR: usage: promote-unreleased-changelog.sh <new-version> [iso-date] [changelog-path]" >&2
  exit 2
fi

NEW_VERSION="$1"
RELEASE_DATE="${2:-$(date -u +%Y-%m-%d)}"
CHANGELOG="${3:-CHANGELOG.md}"

if [[ ! "$NEW_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "ERROR: version must be X.Y.Z, got: $NEW_VERSION" >&2
  exit 2
fi
if [[ ! "$RELEASE_DATE" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]]; then
  echo "ERROR: date must be YYYY-MM-DD, got: $RELEASE_DATE" >&2
  exit 2
fi
if [[ ! -f "$CHANGELOG" ]]; then
  echo "ERROR: no changelog at: $CHANGELOG" >&2
  exit 2
fi

# The Python lives in a sibling FILE, not a `python3 - <<'PY'` heredoc. Bash 5.1+
# writes a heredoc no larger than its BUILD-TIME pipe-size estimate into a pipe
# before exec'ing the reader, and on macOS bash up to 5.3 patch 15 does that as
# one blocking write. The macOS kernel creates a pipe with a 512-byte buffer and
# enlarges it on a write only while system-wide pipe memory is under its limit
# (xnu bsd/kern/sys_pipe.c); past that limit the write waits for a reader, and
# the only reader is the python3 that cannot start until the write finishes. So
# any heredoc over 512 bytes can hang under load. A pre-commit run was found
# stuck in heredoc_write after ~17 min on 2026-09-23. Upstream fixed it in 5.3
# patch 16 ("accommodate macOS dynamic pipe sizing, avoiding hangs"), but a file
# argument never goes through that path on any bash. Do not inline it back.
python3 "$(dirname "$0")/promote_unreleased_changelog.py" "$NEW_VERSION" "$RELEASE_DATE" "$CHANGELOG"
