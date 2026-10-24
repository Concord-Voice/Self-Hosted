#!/usr/bin/env bash
# check-no-unscoped-redis-flush.sh — keeps Go tests on their own Redis logical
# database.
#
# Since #2680 every `go test` process allocates a private Redis DB index via
# internal/testhelpers/redistest. Three patterns break that, and all three are
# how the original defect was written, so all three are rejected in any
# `_test.go` outside the redistest package itself:
#
#   Rule 1  FlushDB( / FlushAll( / "FLUSHDB" / "FLUSHALL"
#           A raw flush reaches whatever DB the client happens to hold. Use
#           redistest.Reset(ctx, client), which refuses to flush a database
#           this process does not own.
#   Rule 2  redis.ParseURL(
#           How a future author reintroduces a hand-rolled client on a DB
#           redistest never allocated. Use redistest.Client(t) / Options(t).
#   Rule 3  os.Getenv("REDIS_URL")
#           How they reintroduce a hand-rolled URL. Use redistest.URL(t).
#
# Rules 2 and 3 are what make "isolation by default, with no opt-in to forget"
# durable. Rule 1 alone would still admit a fresh DB-1 pin: before #2680,
# thirteen packages had independently hand-rolled their own client, which is
# direct evidence that convention alone did not hold here.
#
# KNOWN CEILING — stated rather than implied. This is a line-oriented grep, so
# three shapes are NOT closeable here and are deliberately not claimed:
#   * a command assembled at runtime — `cmd := "FLUSH" + "DB"; c.Do(ctx, cmd)`
#     (a literal spelled in one piece IS caught, which covers the accidental case)
#   * an EVALSHA carrying only a script hash, with no command name in the file
#   * a `redis.Options{...}` struct literal split across multiple lines
#   * a bare `redis.NewClient(&redis.Options{Addr: host})` with neither DB nor
#     Password — indistinguishable by grep from the legitimate miniredis idiom
#     `Addr: mr.Addr()` and the dead-address `Addr: "127.0.0.1:1"` form, both of
#     which are in the tree today and regression-tested as must-not-flag. Closing
#     it would flag those; the DB/Password discriminator is what makes rule 2
#     precise enough to be worth having.
# The guard is a strong defence against the accidental reintroduction this issue
# was actually caused by; it is not a sandbox against a determined author. Do not
# describe it as exhaustive.
#
# Exit codes:
#   0 — clean (no violation, OR out of scope, OR allowlisted, OR absent)
#   1 — at least one violation (printed file:line:matched-text), OR a file that
#       exists but could not be scanned. A guard that cannot read a file must not
#       report it clean; documenting that as a fail-open would not stop it being one.
#   2 — usage error
#
# Style precedent: scripts/check-no-pg-tcp-probes.sh.
# Refs #2680 — see [internal]specs/2026-08-16-2680-test-redis-isolation-design.md
set -u

# Allowlist — the redistest package legitimately contains every pattern above:
# it OWNS the flush, the URL parsing and the REDIS_URL read. Its test files
# additionally assert on the literals. Matched as a path prefix/suffix so both
# repo-relative and absolute invocations work.
ALLOWLIST_REGEX='(^|/)services/control-plane/internal/testhelpers/redistest/'

# All three run under `grep -inE` (case-INsensitive). That is load-bearing for
# rule 1: Redis command names are case-insensitive on the wire, so
# `c.Do(ctx, "flushdb")` is a fully functional flush that a case-sensitive grep
# walks straight past.
#
# Rule 1 covers the method calls (including the Async variants, which are real
# go-redis v9 methods and equally destructive) and the command literal in any
# quoting style — double, backtick, or single, the last being how it appears
# inside an EVAL'd Lua body.
REGEX_FLUSH='flush(db|all)(async)?\(|["`'"'"']flush(db|all)["`'"'"']'

# Rule 2 covers both spellings of a hand-rolled client: the URL form, and the
# direct-struct form carrying a DB or Password field. The bare
# `redis.NewClient(&redis.Options{Addr: mr.Addr()})` miniredis idiom sets
# neither, so it stays legal — that exemption is regression-tested.
REGEX_PARSEURL='redis\.parseurl\(|redis\.options\{[^}]*\b(db|password):'

REGEX_REDISURL_ENV='os\.(getenv|lookupenv)\("redis_url"\)'

if [ "$#" -lt 1 ]; then
  echo "usage: $0 <file> [more-files...]" >&2
  exit 2
fi

overall_exit=0

for file in "$@"; do
  rel="${file#./}"

  # Defense in depth: pre-commit already filters via `files:`, but the script is
  # also run by hand, by CI over the whole tree, and by its own test suite.
  #
  # internal/testhelpers/*.go is in scope as well as *_test.go, because
  # testhelpers/testredis.go — the file the #2680 defect actually lived in — is
  # not a _test.go file. Scanning only _test.go left the original defect site,
  # and any future sibling helper, entirely unguarded.
  case "$rel" in
    *_test.go) ;;
    *services/control-plane/internal/testhelpers/*.go) ;;
    *) continue ;;
  esac

  if echo "$rel" | grep -qE "$ALLOWLIST_REGEX"; then
    continue
  fi

  # ABSENT is skippable — pre-commit passes deleted paths in some flows.
  # PRESENT-BUT-UNREADABLE is not: it means the guard was asked to check a file
  # and could not, and reporting it clean asserts something never verified.
  # These were one silent `[ ! -r ] && continue` before, so a violating file that
  # could not be read scanned green with no output at all.
  if [ ! -e "$file" ]; then
    continue
  fi
  if [ ! -r "$file" ]; then
    echo "  ↑ cannot read $file — refusing to report it clean (#2680)" >&2
    overall_exit=1
    continue
  fi

  # scan <regex> <label> <fix-hint>
  #
  # `if grep …` was wrong: grep exits 1 for "no match" and 2 for an I/O error,
  # and `if` is false for BOTH — so an unreadable file, a directory, or a
  # vanished path was indistinguishable from a clean one, with 2>/dev/null
  # discarding the only evidence. Branch on the status explicitly.
  scan() {
    local out status
    out=$(grep -inE -- "$1" "$file")
    status=$?
    case "$status" in
      0)
        printf '%s\n' "$out"
        echo "  ↑ $2 in $file" >&2
        echo "    fix: $3" >&2
        overall_exit=1
        ;;
      1) : ;; # genuinely no match — the only clean outcome
      *)
        echo "  ↑ grep failed (exit $status) on $file — not reporting it clean (#2680)" >&2
        overall_exit=1
        ;;
    esac
  }

  scan "$REGEX_FLUSH" "unscoped Redis flush" \
    "redistest.Reset(ctx, client) — it refuses to flush a DB this process does not own (#2680)."
  scan "$REGEX_PARSEURL" "hand-rolled Redis client" \
    "redistest.Client(t), or redistest.Options(t) when you need to mutate options (#2680)."
  scan "$REGEX_REDISURL_ENV" "hand-rolled Redis URL" \
    "redistest.URL(t) — the DB index is allocated per process and must not be chosen by hand (#2680)."
done

exit "$overall_exit"
