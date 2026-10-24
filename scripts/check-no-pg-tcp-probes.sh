#!/usr/bin/env bash
# check-no-pg-tcp-probes.sh — detects TCP-only probes against PostgreSQL
# ports (5432 primary, 5433 replica). Anti-pattern that triggers
# `LOG: invalid length of startup packet` floods on the PG server,
# wasting connection slots and obscuring real issues.
#
# Use `pg_isready` (real PG protocol) instead of curl/wget/nc -z.
#
# Detection strategy — two regex families because the syntax differs:
#   Family A (HTTP-probe URL form): curl|wget targeting :5432 or :5433
#     in the URL. `:postgres:[0-9]+` would over-match legitimate URLs
#     like `curl postgres:8080/health`, so we scope by exact port.
#   Family B (nc -z space-arg form): `nc` with `-z` anywhere in flags,
#     followed by host token, then space, then 5432 or 5433. URL-form
#     regex would miss this because nc uses space-separated args.
#
# Exit codes:
#   0 — clean (no anti-pattern, OR file is allowlisted, OR file unreadable)
#   1 — at least one anti-pattern found (printed file:line:matched-text)
#   2 — usage error
#
# Style precedent: scripts/check-env-file.sh.
# Refs #755 — see [internal]specs/2026-04-27-755-postgres-startup-packet-flood-design.md
set -u

# Allowlist — files that intentionally contain the patterns (this script,
# its test fixture, the design spec, and the runbook). Matched as a
# path suffix so that both repo-relative and absolute paths are handled.
ALLOWLIST_REGEX='(^|/)scripts/check-no-pg-tcp-probes\.sh$|(^|/)scripts/test-check-no-pg-tcp-probes\.sh$|(^|/)[internal]specs/2026-04-27-755-postgres-startup-packet-flood-design\.md$|(^|/)[internal]postgres-probe-investigation\.md$'

# Family A: HTTP-probe URL form. The leading boundary covers:
#   - line-start (^) or after a shell operator (;&|)
#   - after a double-quote, single-quote, or open-paren (compose/CI YAML contexts)
#   - after a Dockerfile RUN keyword at line-start (^RUN[[:space:]]+)
# The boundary deliberately excludes bare whitespace so that comment lines
# documenting the anti-pattern (e.g. "# ... curl localhost:5432") are not
# flagged — those have curl preceded only by prose text, not a shell operator
# or recognised keyword.
# [^|;&]* prevents spanning shell pipelines or args. The port literal
# ":5432" or ":5433" with a trailing word boundary catches
# "localhost:5432" and "http://postgres:5432/health".
REGEX_HTTP_PROBE='(^RUN[[:space:]]+|^|[;&|"'"'"'(])[[:space:]]*(curl|wget)[[:space:]][^|;&]*(:5432|:5433)([/?[:space:]"$]|$)'

# Family B: nc -z space-arg form. The flag block must contain `-z` somewhere,
# but intervening flag tokens (e.g. `-w2`, `-v`) are allowed both before and
# after `-z`. This covers the established repo idiom `nc -z -w2 host port`
# (used 3x in infrastructure/deploy/ for coturn checks) and the reverse
# `nc -w2 -z host port`. After all flags comes the host token, then 5432|5433.
REGEX_NC_TCPONLY='\bnc(\s+-[a-zA-Z0-9]+)*\s+-[a-zA-Z]*z[a-zA-Z]*(\s+-[a-zA-Z0-9]+)*\s+[^[:space:]|;&]+\s+(5432|5433)\b'

if [ "$#" -lt 1 ]; then
  echo "usage: $0 <file> [more-files...]" >&2
  exit 2
fi

overall_exit=0

for file in "$@"; do
  # Allowlist by relative path. Strip leading "./" if present.
  rel="${file#./}"
  if echo "$rel" | grep -qE "$ALLOWLIST_REGEX"; then
    continue
  fi

  if [ ! -r "$file" ]; then
    # Pre-commit may invoke us with deleted files in some flows. Skip silently.
    continue
  fi

  # Family A check
  if grep -nE -- "$REGEX_HTTP_PROBE" "$file" 2>/dev/null; then
    echo "  ↑ anti-pattern: HTTP-probe (curl/wget) targeting :5432 or :5433 in $file" >&2
    echo "    fix: use pg_isready (real PG protocol) instead. See [internal]postgres-probe-investigation.md." >&2
    overall_exit=1
  fi

  # Family B check
  if grep -nE -- "$REGEX_NC_TCPONLY" "$file" 2>/dev/null; then
    echo "  ↑ anti-pattern: nc -z TCP probe against PG port in $file" >&2
    echo "    fix: use pg_isready (real PG protocol) instead. See [internal]postgres-probe-investigation.md." >&2
    overall_exit=1
  fi
done

exit "$overall_exit"
