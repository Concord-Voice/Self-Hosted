#!/usr/bin/env bash
#
# strip-suppressed-sarif.sh FILE... — drop SARIF results that carry a
# `suppressions` entry, in place.
#
# Semgrep marks a `// nosemgrep:` match with a `suppressions` array, and GitHub
# Code Scanning ignores that array, so a suppressed match opens an alert. Under
# `--baseline-commit` this fires whenever a PR edits the text of an already
# annotated match, and the protected verifier then blocks on the open alert.
# Same fix as eslint-sarif.yml's "Strip in-source-suppressed results" step.
#
# Match on PRESENCE of a suppression, not on suppressions[].state: raw scanner
# output does not set `state`; GitHub only adds it when re-exporting a stored
# analysis. `. // []` tolerates a run with no results array.
#
# A file that cannot be filtered is left untouched (unfiltered upload) with a
# warning; a missing file is skipped.
set -u

for f in "$@"; do
  [ -f "$f" ] || continue
  before=$(jq '[.runs[]?.results[]?] | length' "$f" 2>/dev/null) || before='?'
  if jq '(.runs[]?.results) |= ((. // []) | map(select((.suppressions | length // 0) == 0)))' \
       "$f" > "$f.filtered"; then
    after=$(jq '[.runs[]?.results[]?] | length' "$f.filtered")
    mv "$f.filtered" "$f"
    echo "SARIF results in $f: ${before} -> ${after} (dropped in-source-suppressed)"
  else
    echo "::warning::SARIF suppression filter failed for $f; uploading unfiltered SARIF"
    rm -f "$f.filtered"
  fi
done
