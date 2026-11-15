#!/bin/bash
# protect-sensitive.sh — PreToolUse hook for Edit|Write AND Bash. Blocks (exit 2)
# edits to security-critical files unless authorized via the per-branch marker file.
# Protected regexes come from .claude/repo-config.json (.hooks.protected_patterns[]);
# absent/empty => nothing blocked (the global deny-list still covers .env/.ssh).
#
# BASH COVERAGE. This guarded Edit|Write only, so a write made THROUGH Bash —
# `python3 - <<PY`, `sed -i`, `tee`, a `>` redirect — reached a protected file
# without the hook ever running. Not theoretical: the session
# that found this had already edited scripts/test-provision-secrets-encoder.sh and
# .github/workflows/provision-secrets.yml roughly eight times that way before
# noticing the hook had never once fired.
#
# THE BASH SIDE IS A DENYLIST, NOT A PROOF. Deciding whether an arbitrary shell
# command writes a given path is undecidable; what follows recognises the
# constructs an agent actually reaches for. The Edit|Write path remains the
# complete one. Answer a miss by WIDENING this list — never by concluding the
# check is unsound and relaxing it.
#
# ROOT-RELATIVE OPERANDS, closed 2026-09-18. The patterns a repo configures are
# component-shaped, with leading AND trailing slashes -- `/rbac/`, `/auth/`,
# `/preload/`, `/middleware/security`. A path ALREADY inside the protected
# component carries no slash in front of it, so `rbac/resolver.go` missed
# `/rbac/` while `services/.../rbac/resolver.go` matched. Eight `sed -i` edits
# reached a protected rbac/ directory that way,
# with naming the same files absolutely on the very next call blocking correctly
# -- which is what proved the recognisers were fine and the SUBJECT STRING was
# not. The miss was never specific to `sed -i` or to a `cd`: it applied to every
# write shape at once. Closed in matches_protected, which now tests a relative
# subject a second time with a leading `/` attached; see the comment there for
# why BOTH forms are tested and never the rooted one alone.
#
# KNOWN LIMITATION, deliberately not "fixed": a branch whose NAME matches a
# protected pattern cannot write its own bypass marker, because the marker's
# filename contains the branch name. Exempting the marker directory would let an
# agent self-authorize, which is worse than the inconvenience. It fails closed;
# the developer creates the marker, or the branch is renamed.
#
# KNOWN GAPS, recognised and deliberately NOT closed. Coverage was frozen after
# 43 review rounds because each widening of this denylist created new adjacent
# surface -- late rounds were finding more defects in the previous round's fixes
# than in the original code. These were reported, reproduced, and left open on
# purpose. They are recorded here rather than in a tracking issue so that the
# next person to widen this file starts from what is already known:
#
#   * `case x in x) rm <protected>;; esac` at TOP level. The `case` header and
#     pattern terminator are consumed inside `$(...)` and `<(...)` bodies but not
#     for a top-level statement, so the command never reaches a segment head.
#   * `git -C a -C b <verb>`. Multiple relative `-C` options compose
#     sequentially; only the first is captured, so a target resolves against the
#     wrong directory.
#   * `install -d DIR...`. In directory mode EVERY operand is a destination;
#     only the last is collected.
#   * `ls <single-file> | xargs rm`. `ls` operands are collected only when they
#     carry a glob metacharacter, because `ls DIR` prints BASENAMES and treating
#     the directory as a target was a false block.
#   * `cat <list-file> | xargs rm`. What is deleted is the file's CONTENTS, which
#     cannot be known without reading it -- past what a PreToolUse hook should do.
#   * `curl --next` transfer boundaries. `--output-dir` is applied globally, which
#     over-collects rather than under-collects, so this errs fail-closed.
#   * `pushd DIR` / `popd`. Only `cd` moves the tracked directory, so a write
#     named relative to a pushd'd directory resolves from the wrong base:
#     `pushd services/auth; echo x > login.go` is not seen. Found while closing
#     the root-relative gap above, which covers the common spelling incidentally
#     -- the operand's own first component is protected there -- but not this
#     one, where the protected component is contributed by the BASE. `popd` is
#     untracked in the other direction and leaves the base too DEEP, which
#     over-blocks; that half fails closed. Closing this needs a directory stack
#     in the walker, a second mechanism beside the cd tracking rather than a
#     widening of it, so it is recorded here instead of chased.
#
# Widening any of these is fine. Doing it WITHOUT a regression test that fails
# with only that change reverted is what this file has repeatedly punished.
set -o pipefail
# A hook that outlives the harness timeout is skipped, which fails OPEN, and the
# Bash walk below grows with the number of segments. So the work runs under its own
# 20s deadline (the hook timeout is 30s) and running out of time blocks.
# ponytail: no timeout/gtimeout binary (stock macOS) means no deadline; install
# coreutils to get one.
if [[ -z "${PROTECT_SENSITIVE_INNER:-}" ]]; then
  DEADLINE_BIN=$(command -v timeout || command -v gtimeout || true)
  if [[ -n "$DEADLINE_BIN" ]]; then
    PROTECT_SENSITIVE_INNER=1 "$DEADLINE_BIN" 20 bash "$0" "$@"; rc=$?
    if [[ "$rc" -eq 124 ]]; then
      echo "BLOCKED: protect-sensitive.sh could not check this command within 20s. Split it into shorter commands." >&2; exit 2
    fi
    exit "$rc"
  fi
fi
if ! command -v jq >/dev/null 2>&1; then
  echo "BLOCKED: jq is required for protect-sensitive.sh but is not installed." >&2; exit 2
fi
INPUT=$(cat)
# Malformed JSON fails closed. Absent FIELDS do not: a Bash call has no file_path
# and an Edit has no command, so treating either absence as a parse failure would
# block every call of the other kind.
if ! printf '%s' "$INPUT" | jq -e . >/dev/null 2>&1; then
  echo "BLOCKED: Failed to parse hook input in protect-sensitive.sh. Failing closed." >&2; exit 2
fi
FILE_PATH=$(printf '%s' "$INPUT" | jq -r '.tool_input.file_path // empty' 2>/dev/null || true)
COMMAND=$(printf '%s' "$INPUT" | jq -r '.tool_input.command // empty' 2>/dev/null || true)
# A backslash-newline is a LINE CONTINUATION: bash joins the lines and runs one
# command. Every pass here is line-oriented -- mask_quoted reads one awk record per
# physical line -- so `rm \` and its operand landed in separate segments, the second
# of them verb-less, and the delete went unseen. Joined once, here, before anything
# else looks at the text.
#
# Inside SINGLE quotes a backslash-newline is literal data rather than a
# continuation, so joining is not faithful there. That direction is deliberate: it
# can merge two lines of a quoted string, which over-collects, whereas leaving the
# split intact drops a real command on the floor. The awk form is used because BSD
# and GNU sed disagree about multi-line handling far more than awk does.
COMMAND=$(printf '%s' "$COMMAND" | awk '{
  # Only an ODD trailing run continues the line. A line ending `\\\\` is a literal
  # backslash followed by a real separator, and joining it regardless of parity hid
  # the next command behind the previous one: `printf x \\\\` + `rm <protected>`
  # became one segment headed by printf, and the delete went unseen.
  n = 0; i = length($0)
  while (i > 0 && substr($0, i, 1) == "\\") { n++; i-- }
  if (n % 2 == 1) printf "%s", substr($0, 1, length($0) - 1); else print
}')
[ -z "$FILE_PATH" ] && [ -z "$COMMAND" ] && exit 0
REPO_ROOT="${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel 2>/dev/null || echo "")}"
# The repo root reached through symlinks, resolved once. Used to turn a physically
# resolved path back into a repo-relative one; empty when the root is unknown.
REPO_REAL=""
[ -n "$REPO_ROOT" ] && REPO_REAL=$(cd "$REPO_ROOT" 2>/dev/null && pwd -P)
RC_FILE="${REPO_ROOT:+$REPO_ROOT/}.claude/repo-config.json"; [ -f "$RC_FILE" ] || RC_FILE=".claude/repo-config.json"
# No patterns means nothing is protected, so skip the walk instead of paying for it
# on every Bash call. A marker can only widen what is allowed, so it does not matter here.
[[ -n "$(jq -r '.hooks.protected_patterns[]? // empty' "$RC_FILE" 2>/dev/null)" ]] || exit 0

# list_descendants <repo-relative-dir> — what a recursive delete of it would take.
#
# Tracked files come from `git ls-files`, which reads the INDEX: fast, and empty for
# a gitignored tree, so `rm -rf node_modules` costs nothing. But an empty result is
# ambiguous — it also means "untracked", or "not a git repo at all" — and treating
# that as "nothing protected underneath" would be a fail-open for exactly the
# recursive delete this exists to catch. So an empty result falls back to a bounded
# filesystem walk, skipped only when git positively says the path is IGNORED.
list_descendants() {
  local d="$1" root="${REPO_ROOT:-.}"
  # An operand may be ABSOLUTE — directly, or because a `cd` composed one. git wants
  # it repo-relative and `${root}/${d}` doubled the prefix into a path that exists
  # nowhere, so nothing was enumerated at all. Inside the repo it is relativized;
  # outside it there is no index to consult, so the walk runs on its own.
  case "$d" in
    "$root"/*) d="${d#"$root"/}" ;;
    /*) [ -d "$d" ] && find "$d" -maxdepth 6 2>/dev/null; return 0 ;;
    *) ;;
  esac
  git -C "$root" ls-files -- "$d" 2>/dev/null
  # ...and then the walk as well, NOT as a fallback. A nonempty index result is not
  # a complete listing: `rm -rf services` takes an untracked services/auth/login.go
  # just as surely as a tracked one, and returning early on the tracked entries hid
  # exactly that.
  #
  # The walk is skipped when git positively reports IGNORED. This is a DELIBERATE
  # fail-open, kept after being removed and measured, not an oversight:
  #
  #   rm -rf client/desktop/node_modules  ->  @types/node/crypto.d.ts   (/crypto)
  #   rm -rf client/desktop/dist          ->  dist/preload/preload.js.map (/preload/)
  #
  # Under patterns that name a component rather than a full path -- which is what the
  # consuming repo actually configures -- VENDORED and GENERATED files match the same
  # strings as the sources they are named after. Walking ignored trees therefore
  # refuses `rm -rf node_modules`, and this file argues in two places that a check
  # blocking work that routine is switched off within a day.
  #
  # What this gives up, stated plainly: a protected file living under an ignored
  # directory is deleted by an explicit recursive delete with this hook returning 0.
  # The cost is not performance -- the walk measures 0.17s on that tree. It is that
  # ignored status and generated status are indistinguishable by path, so there is no
  # rule here that keeps one property without losing the other. Narrowing the
  # patterns in the consuming repo is what would let this be reinstated.
  git -C "$root" check-ignore -q -- "$d" 2>/dev/null && return 0
  # A GLOB operand is expanded by the shell before rm ever sees it, so the literal
  # directory test below finds nothing and `rm -rf *` walked past everything. The
  # expansion happens here instead, in the repo root, with globbing enabled only for
  # that one command.
  case "$d" in
    *[*?[]*)
      ( cd "$root" 2>/dev/null || exit 0
        set +f
        for g in $d; do [ -e "$g" ] && find "$g" -maxdepth 6 2>/dev/null; done )
      return 0 ;;
    *) ;;
  esac
  [ -d "${root}/${d}" ] || return 0
  ( cd "$root" 2>/dev/null && find "$d" -maxdepth 6 2>/dev/null )
}

# var_values <token> — every path a bare $VAR / ${VAR} could hold at this point.
# The last unconditional assignment wins, as the shell does, but any CONDITIONAL
# assignment is an ADDITIONAL candidate, since the tokenizer cannot know whether the
# branch ran. Echoes the token unchanged when it is not a variable.
# expand_embedded <text> — replace every `$NAME` / `${NAME}` inside a token with its
# recorded or inherited value. Only the UNCONDITIONAL value: a token holding several
# variables would otherwise fan out combinatorially, and the leading variable's
# alternatives are already enumerated by the caller. Bounded to 8 substitutions so a
# self-referential expansion cannot spin.
expand_embedded() {
  local txt="$1" i=0 name val pre post
  # A literal `$` held in a variable: written inline, `${txt%%$*}` expands `$*` to the
  # positional parameters before the pattern is matched, and the split silently did
  # nothing at all.
  local DOL='$'
  while [ "$i" -lt 8 ]; do
    case "$txt" in
        *"$DOL"*) ;; *) break ;;
      esac
    pre="${txt%%$DOL*}"; post="${txt#*$DOL}"
    case "$post" in
      \{*) name="${post%%\}*}"; name="${name#\{}"; post="${post#*\}}" ;;
      *)   name=$(printf '%s' "$post" | grep -oE '^[A-Za-z_][A-Za-z0-9_]*')
           post="${post#"$name"}" ;;
    esac
    case "$name" in
      "" ) break ;;
      [A-Za-z_]*) case "$name" in
          *[!A-Za-z0-9_]*) break ;; *) ;;
        esac ;;
      *) break ;;
    esac
    val=$(printf '%s\n' "$VARS" | awk -F= -v n="$name" 'index($0, n "=") == 1 { sub(/^[^=]*=/, ""); v = $0 } END { print v }')
    [ -n "$val" ] || val=$(printenv "$name" 2>/dev/null) || val=""
    # Unknown: drop the reference and keep the literal remainder, which is the same
    # conservative reading the top-level suffix fallback uses.
    txt="${pre}${val}${post}"
    i=$((i + 1))
  done
  printf '%s' "$txt"
}

# brace_expand <token> — `services/{auth,public}` is TWO operands by the time the
# shell runs it. Recursive, so several groups in one token expand fully; a token with
# no `{a,b}` group is echoed unchanged. Brace expansion precedes variable expansion
# in the shell, and it does here too.
# glob_expand <token> <base> — a filesystem glob is expanded by the SHELL before the
# command runs, so `rm services/*/login.go` never presents the literal pattern to the
# verb. Matches are returned relative to the same base the caller passed, so the
# caller's resolution is unchanged. A pattern matching nothing is echoed unchanged --
# that is what the shell passes through too, with nullglob off.
glob_expand() {
  local t="$1" b="$2" pat pfx m any=0
  case "$t" in
      *[*?[]*) ;; *) printf '%s\n' "$t"; return 0 ;;
    esac
  case "$t" in
    /*) pat="$t"; pfx="" ;;
    *)  pfx="${REPO_ROOT:+${REPO_ROOT%/}/}${b:+${b%/}/}"; pat="${pfx}${t}" ;;
  esac
  for m in $pat; do
    [ -e "$m" ] || continue
    any=1
    printf '%s\n' "${m#"$pfx"}"
  done
  [ "$any" -eq 0 ] && printf '%s\n' "$t"
  return 0
}

brace_expand() {
  local t="$1" pre mid post
  case "$t" in
    *\{*,*\}*) ;;
    *) printf '%s\n' "$t"; return 0 ;;
  esac
  pre="${t%%\{*}"; mid="${t#*\{}"; mid="${mid%%\}*}"; post="${t#*\}}"
  printf '%s' "$mid" | tr ',' '\n' | while IFS= read -r p; do
    brace_expand "${pre}${p}${post}"
  done
}

var_values() {
  local t="$1" vn rest
  # `$VAR` may be a PREFIX of the operand rather than all of it — `"$d/login.go"`
  # composes a path. Treating the whole token as one variable name looked up
  # `d/login.go`, found nothing, and skipped the target as an unresolved expansion.
  case "$t" in
    \$*)
      rest="${t#\$}"
      case "$rest" in
        \{*) vn="${rest%%\}*}"; vn="${vn#\{}"; rest="${rest#*\}}" ;;
        *)   vn=$(printf '%s' "$rest" | grep -oE '^[A-Za-z_][A-Za-z0-9_]*')
             rest="${rest#"$vn"}" ;;
      esac
      # The BRACED branch takes everything up to `}`, which is not necessarily a
      # variable name: `${p[$(cmd)]}` arrives here intact. Nothing below may treat an
      # arbitrary string as a name, so anything that is not one is not an expansion
      # this hook can reason about — it is passed through as an opaque token.
      case "$vn" in
        [A-Za-z_]*) case "$vn" in
            *[!A-Za-z0-9_]*) vn="" ;; *) ;;
          esac ;;
        *) vn="" ;;
      esac
      [ -n "$vn" ] || { printf '%s\n' "$t"; return 0; } ;;
    # A literal PREFIX does not mean there is no expansion: `services/$p/login.go`
    # was returned unchanged because it does not START with `$`, so the target loop
    # accepted it verbatim and never saw services/auth/login.go.
    *) printf '%s\n' "$(expand_embedded "$t")"; return 0 ;;
  esac
  local hit=""
  # The suffix may hold FURTHER variables: `"$a/$b/login.go"` expanded only `$a` and
  # appended `/$b/login.go` verbatim. The result no longer began with `$`, so the
  # target loop accepted it as a literal and the protected path was never seen.
  rest=$(expand_embedded "$rest")
  hit=$(printf '%s\n' "$VARS" | awk -F= -v n="$vn" -v r="$rest" 'index($0, n "=") == 1 { sub(/^[^=]*=/, ""); v = $0 } END { if (v != "") print v r }')
  [ -n "$hit" ] && printf '%s\n' "$hit"
  printf '%s\n' "$VARS_ALT" | awk -F= -v n="$vn" -v r="$rest" 'index($0, n "=") == 1 { sub(/^[^=]*=/, ""); print $0 r }'
  # No assignment in the command itself does not mean the expansion is empty: the
  # variable may be INHERITED. `echo x > "$CLAUDE_PROJECT_DIR/services/auth/login.go"`
  # was skipped as unresolved while bash expanded it and wrote the protected file.
  # The hook's own environment answers that exactly when the variable is set there.
  if [ -z "$hit" ]; then
    local env_val
    # `printenv`, NOT bash's indirect expansion `${!vn}`. Indirect expansion EVALUATES
    # its argument as a parameter expression, so an array subscript containing a
    # command substitution RUNS: with vn='p[$(touch /tmp/x)]', `: "${!vn-}"` creates
    # the file. Measured on bash 5.3.15. That put a command-execution primitive inside
    # a hook that sees every Bash tool call, fed from the very command string it is
    # meant to police. The name guard above closes it too, but this is the line that
    # must never be reintroduced: printenv is a program and cannot evaluate anything.
    # It reads only EXPORTED variables, which is the correct scope — an unexported
    # shell variable of the caller was never visible to this process anyway.
    env_val=$(printenv "$vn" 2>/dev/null) || env_val=""
    if [ -n "$env_val" ]; then
      printf '%s\n' "${env_val}${rest}"
    elif [ -n "$rest" ]; then
      # Unset here too, so the value is genuinely unknown — but the LITERAL suffix is
      # not, and a pattern matching it matches wherever the prefix points. Checking it
      # is the conservative reading; dropping the whole token was fail-open.
      printf '%s\n' "$rest"
    fi
  fi
}

# norm_path <path> — collapse `.` and `..` lexically. `cd services/auth && cd
# ../public` left the running directory as `services/auth/../public`, which still
# matched a `/auth/` pattern and false-blocked a write to a directory it had left.
norm_path() {
  printf '%s' "$1" | awk '{
    abs = ($0 ~ /^\//); n = 0; m = split($0, p, "/")
    for (i = 1; i <= m; i++) {
      if (p[i] == "" || p[i] == ".") continue
      if (p[i] == ".." && n > 0 && a[n] != "..") { n--; continue }
      a[++n] = p[i]
    }
    s = ""; for (i = 1; i <= n; i++) s = s (i > 1 ? "/" : "") a[i]
    print (abs ? "/" s : s)
  }'
}

# marker_allows <path> — 0 if the per-branch marker lists it. Shared by both
# modes, so an authorization granted for a path holds however the write is spelled.
marker_allows() {
  local target="$1" branch branch_safe marker rel ap
  branch=$(git branch --show-current 2>/dev/null || true)
  [ -n "$branch" ] || return 1
  branch_safe="${branch//\//__}"
  [[ "$branch_safe" =~ ^[A-Za-z0-9._-]+$ ]] || return 1
  marker="${REPO_ROOT:+$REPO_ROOT/}.claude/state/hook-bypass/${branch_safe}.txt"
  [ -f "$marker" ] && [ ! -L "$marker" ] \
    && [ "$(wc -c < "$marker" 2>/dev/null || echo 0)" -le 16384 ] || return 1
  rel="$target"
  [ -n "$REPO_ROOT" ] && [[ "$target" = "$REPO_ROOT"/* ]] && rel="${target#"$REPO_ROOT"/}"
  while IFS= read -r ap || [ -n "$ap" ]; do
    [[ "$ap" =~ ^[[:space:]]*# ]] && continue; [ -z "$ap" ] && continue
    [[ "$ap" == /* ]] && continue; [[ "$ap" =~ (^|/)\.\.(/|$) ]] && continue
    if [ "$rel" = "$ap" ]; then
      echo "ALLOWED: $target (marker ${branch_safe}.txt, branch $branch)" >&2; return 0
    fi
  done < "$marker"
  return 1
}

# matches_protected <string> — sets MATCHED_PATTERN and returns 0 on a match,
# 1 on none, 2 on an invalid regex.
#
# Returns a status and sets a GLOBAL rather than echoing, deliberately. Written as
# `p=$(matches_protected ...)` the invalid-regex branch would `exit 2` inside a
# command-substitution SUBSHELL — the outer script would carry on with an empty
# `p`, take the no-match path, and exit 0. That turns this hook's fail-closed
# guarantee into fail-open for exactly the case it exists to catch.
#
# A RELATIVE subject is checked a second time with a leading `/` attached. The
# patterns a repo configures are component-shaped -- `/rbac/`, `/auth/`,
# `/middleware/security` -- and a path whose FIRST component is the protected one
# carries no slash in front of it, so `rbac/resolver.go` missed `/rbac/` outright
# while `services/.../rbac/resolver.go` matched. The miss is not confined to a
# `cd`: it is any operand named relative to a directory that is already inside
# the protected component, and it applies to every write shape at once --
# `sed -i`, a `>` redirect, `tee`, `rm -rf`. Eight `sed -i` edits reached
# a protected rbac/ directory that way, in a session where naming the
# same files absolutely on the very next call blocked correctly.
#
# BOTH forms are tested, never the rooted one alone. A pattern may be anchored --
# `^services/auth` is a legitimate thing to configure -- and prefixing the subject
# would break exactly those, turning a widening into a silent narrowing.
MATCHED_PATTERN=""
matches_protected() {
  local subject="$1" pattern rc rooted
  MATCHED_PATTERN=""
  [ -f "$RC_FILE" ] || return 1
  # Empty when the subject is already absolute, which is the flag the loop reads.
  case "$subject" in
      /*) rooted="" ;; *) rooted="/$subject" ;;
    esac
  while IFS= read -r pattern; do
    [ -z "$pattern" ] && continue
    [[ "$subject" =~ $pattern ]]; rc=$?
    # Only on a clean MISS. An invalid regex is rc 2 against either form, and
    # re-testing it would report the same failure twice for no gain.
    if [ "$rc" -eq 1 ] && [ -n "$rooted" ]; then
      [[ "$rooted" =~ $pattern ]]; rc=$?
    fi
    if [ "$rc" -eq 2 ]; then
      MATCHED_PATTERN="$pattern"; return 2
    elif [ "$rc" -eq 0 ]; then
      MATCHED_PATTERN="$pattern"; return 0
    fi
  done < <(jq -r '.hooks.protected_patterns[]? // empty' "$RC_FILE" 2>/dev/null)
  return 1
}

check_or_die() {  # check_or_die <subject> <label> ; blocks or returns
  local subject="$1" label="$2" rc
  matches_protected "$subject"; rc=$?
  if [ "$rc" -eq 2 ]; then
    echo "BLOCKED: invalid regex in protected_patterns: $MATCHED_PATTERN. Failing closed." >&2; exit 2
  elif [ "$rc" -eq 0 ]; then
    marker_allows "$subject" && return 0
    echo "BLOCKED: ${label} (matched: $MATCHED_PATTERN). Requires explicit user approval." >&2
    exit 2
  fi
  return 0
}

# resolve_and_check <target> <base> — resolve a relative target against the base
# that actually applies to it, then authorize and check the RESOLVED path.
#
# Authorization has to happen after resolution, not before: a marker listing
# `services/auth/login.go` was allowing `cd other && echo x > services/auth/login.go`,
# which writes `other/services/auth/login.go` — an exact-path grant extended to a
# different file. Only the effective path is checked, so a relative target under a
# known base is no longer also checked bare, which was over-blocking as well.
resolve_and_check() {
  # The RESOLVED path is normalised too, not just the base: `> services/auth/../public/x`
  # writes services/public/x, and a lexical `..` in the operand kept matching /auth/.
  local t="$1" b="$2" eff
  case "$t" in
    /*) eff="$t" ;;
    *)  eff="${b:+${b%/}/}$t" ;;
  esac
  eff=$(norm_path "$eff")
  marker_allows "$eff" && return 0
  check_or_die "$eff" "Bash command writes security-critical file: $eff"
  # A DIRECTORY operand carries no trailing separator, so a `/auth/`-shaped
  # pattern misses `rm -rf services/auth` outright. Fabricated AFTER the marker
  # decision, because a trailing separator can never equal a marker entry.
  check_or_die "${eff%/}/" "Bash command writes security-critical file: ${eff%/}/"
  # An existing symlink dereferences. The ALIASES table only knows links created
  # earlier in THIS command, and the hook sees one invocation at a time -- a link
  # made by a previous tool call, or checked into the tree, is invisible to it. One
  # hop is resolved here; a chain of links is not, which is a widening to make if a
  # real miss appears rather than a loop to write speculatively.
  # A symlink can sit in a PARENT component, not just at the end: with
  # `link -> services/auth`, the path `link/login.go` is an ordinary file and `-L`
  # on it is false, yet the write lands in the protected directory. The deepest
  # existing ancestor is resolved PHYSICALLY and the remainder re-attached.
  local pdir prest pabs preal pcand ptail
  case "$eff" in
    */*) pdir="${eff%/*}"; prest="${eff##*/}" ;;
    *)   pdir=""; prest="" ;;
  esac
  if [ -n "$pdir" ] && [ -n "$prest" ]; then
    case "$pdir" in
      /*) pabs="$pdir" ;;
      *)  pabs="${REPO_ROOT:+${REPO_ROOT%/}/}$pdir" ;;
    esac
    # Walk BACK to the deepest ancestor that exists. `mkdir -p link/new && echo x >
    # link/new/login.go` creates the intermediate directory as it runs, so the
    # immediate parent is absent when the hook looks and a whole-parent test finds
    # nothing to resolve. The tail that does not exist yet is carried along.
    ptail=""
    while [ -n "$pabs" ] && [ ! -d "$pabs" ]; do
      case "$pabs" in
        */*) ptail="${pabs##*/}${ptail:+/$ptail}"; pabs="${pabs%/*}" ;;
        *) pabs="" ;;
      esac
    done
    [ -n "$ptail" ] && prest="${ptail}/${prest}"
    if [ -d "$pabs" ]; then
      preal=$(cd "$pabs" 2>/dev/null && pwd -P) || preal=""
      if [ -n "$preal" ] && [ "$preal" != "$pabs" ]; then
        # Repo-relative when the resolved directory is still inside the repo,
        # absolute when the link points out of it. A marker can authorize either.
        pcand="${preal%/}/$prest"
        # Relativize against the PHYSICAL repo root as well as the logical one. On
        # macOS the repo root is routinely reached through a symlink itself (/var ->
        # /private/var under $TMPDIR), so `pwd -P` returns a prefix that the logical
        # root never matches -- and every relative write was then ALSO checked as an
        # absolute path, which FALSE-BLOCKED `auth/out.txt`: harmless repo-relative,
        # but a `/auth/` match once the leading slash arrives.
        case "$pcand" in
          "${REPO_ROOT%/}"/*) pcand="${pcand#"${REPO_ROOT%/}"/}" ;;
          *) if [ -n "$REPO_REAL" ]; then
               case "$pcand" in
                   "${REPO_REAL%/}"/*) pcand="${pcand#"${REPO_REAL%/}"/}" ;; *) ;;
                 esac
             fi ;;
        esac
        if [ "$pcand" != "$eff" ]; then
          marker_allows "$pcand" || check_or_die "$pcand" "Bash command writes security-critical file through a symlinked directory: $eff -> $pcand"
        fi
      fi
    fi
  fi
  local abs lnk
  # An already-absolute `eff` prefixed again produced `/repo//repo/alias`, a path that
  # exists nowhere, so `-L` never fired and the link was never followed.
  case "$eff" in
    /*) abs="$eff" ;;
    *)  abs="${REPO_ROOT:+${REPO_ROOT%/}/}$eff" ;;
  esac
  if [ -L "$abs" ]; then
    lnk=$(readlink "$abs" 2>/dev/null) || lnk=""
    if [ -n "$lnk" ]; then
      case "$lnk" in
        /*) : ;;
        # A relative link target resolves from the LINK's own directory.
        *)  case "$eff" in
              */*) lnk=$(norm_path "${eff%/*}/$lnk") ;;
              *)   lnk=$(norm_path "$lnk") ;;
            esac ;;
      esac
      marker_allows "$lnk" || check_or_die "$lnk" "Bash command writes security-critical file through a symlink: $eff -> $lnk"
    fi
  fi
}

# ── Bash mode ────────────────────────────────────────────────────────────────
if [ -z "$FILE_PATH" ] && [ -n "$COMMAND" ]; then
  # git is deliberately exempt: its writes move content that was already committed,
  # and therefore already reviewable as a diff when it was authored. Two limits on
  # that, both of which the first version of this block got wrong:
  #
  #   PER SEGMENT. It matched `git` wherever a command could start and then exited
  #   the hook, so `git status; echo x > <protected>` was exempt in FULL — one token
  #   disabled the check for the rest of the line. Segments are now dropped
  #   individually and whatever remains is still inspected.
  #
  #   SAFE VERBS ONLY. `git apply`/`am` write arbitrary patch bytes that were never
  #   reviewed here, and `git mv` can move a protected file out of the path its
  #   pattern guards. The already-committed argument covers neither, so they are
  #   absent from the list below and get inspected like any other command.
  #
  # Only the git COMMAND WORDS are stripped; the rest of the segment is kept and
  # still inspected. Dropping the whole segment let `git diff > <protected>` through,
  # because the redirect went out with the git command that preceded it.
  #
  # A prefix is stripped only on a positive match, so a shape this cannot parse is
  # kept whole and inspected. The unparseable case fails closed.
  #
  # `>|` (bash force-redirect) is normalised to `>` BEFORE the split, or splitting on
  # `|` would separate the redirect from its target and the target regex would never
  # see it. The trailing space added at the end gives every line a right boundary, so
  # patterns anchored on one do not have to special-case end-of-line.
  # `stash` is absent deliberately: `git stash apply|pop` restores UNCOMMITTED bytes
  # that were never reviewed, possibly authorized on another branch. It is handled
  # with `apply`/`am` below rather than exempted here. `config` is absent because
  # `git config -f <file>` writes an arbitrary file.
  GIT_SAFE='(status|log|diff|show|branch|add|commit|push|fetch|pull|merge|rebase|checkout|switch|restore|cherry-pick|revert|reset|tag|remote|init|blame|describe|rev-parse|ls-files)'

  # Leading tokens that precede a command without being one: shell keywords and
  # group openers (`if …; then cp …` splits to a segment starting `then`), wrappers,
  # environment assignments, and options — including the few that take a separate
  # value, which must be consumed or `sudo -u root cp` stops the anchor dead at
  # `root`. Stripping these is what lets the write verbs below be ANCHORED, and the
  # anchor is what stops `grep -n "mv" <protected>` being read as a write.
  # Group openers take OPTIONAL whitespace — `(cp x prot)` has none — while word
  # tokens require it, or `then` would also match the head of `thenceforth`.
  LEAD='(([({][[:space:]]*)|((then|do|else|elif|!|sudo|env|command|nohup|time|nice|setsid|doas|(-u|-g|-I|-n|-P)[[:space:]]+[^[:space:]]+|-[^[:space:]]+|[A-Za-z_][A-Za-z0-9_]*=[^[:space:]]*)[[:space:]]+))'

  # Verbs that write or delete outright. Used only by the xargs rule below, which
  # needs to know whether the command behind a stdin feeder is a mutating one.
  # `sed` is absent as a bare verb because only `sed -i` writes; the in-place
  # spelling is matched separately where this is used, since behind `xargs` its own
  # extractor sees a program and no operand.
  MUTATING_VERBS='(rm|unlink|mv|cp|tee|truncate|dd|shred|install|touch|chmod|chown|chgrp|ln)'

  # Stripping a leading executable PATH and stripping the LEAD wrapper run are
  # ORDER-DEPENDENT, and neither pass subsumes the other: LEAD names its wrappers as
  # bare words, so `/usr/bin/env rm f` survives it untouched, while the path strip
  # rewrites only the FIRST word, so `env /bin/rm f` survives that. Alternating until
  # the text stops changing is what handles both, and it is the whole reason this is
  # a loop rather than two consecutive lines.
  #
  # The bound FAILS OPEN, and saying otherwise would be wrong: if the text has not
  # converged after 8 rounds it is left partially normalised, and a recogniser that
  # then fails to match simply does not block. That is acceptable only because each
  # round which changes anything removes at least one leading token, so 8 rounds
  # covers seven stacked path-qualified wrappers -- far past anything a person or an
  # agent writes. It is a runaway guard, not a security boundary.
  strip_lead() {
    local t prev i=0
    t=$(cat)
    prev=""
    while [ "$t" != "$prev" ] && [ "$i" -lt 8 ]; do
      prev="$t"
      # Both substitutions in ONE sed pass, applied in this order -- that is exactly
      # one round of the alternation, at half the subprocess cost. The loop runs per
      # SEGMENT, so the saving is per segment too: measured at ~145 ms on an
      # eight-segment command before this, ~70 ms after.
      # `timeout` cannot join LEAD's wrapper list, because that list is built from
      # words that are followed IMMEDIATELY by the command. `timeout` takes a
      # DURATION operand first, so stripping the bare word would leave `5 rm f` and
      # the verb one token further along than any extractor looks -- the strip would
      # read as done while achieving nothing. It gets its own rule, consuming the
      # option run and the duration together. Its value-taking options are named
      # explicitly ahead of the generic arm: `-s TERM` halts a generic option run on
      # the signal NAME, leaving the verb two tokens deep. `-k 10` looked fine only
      # because its value is numeric and the duration arm happened to absorb it --
      # an accident, not a design, so it is enumerated too rather than left resting
      # on the shape of its argument.
      t=$(printf '%s' "$t" | sed -E "s|^([[:space:]]*)/[^[:space:];&|]*/([A-Za-z0-9_.-]+)|\1\2|; s/^[[:space:]]*(${LEAD})*/ /; s/^([[:space:]]*)timeout[[:space:]]+(((-s|-k|--signal|--kill-after)[[:space:]]+[^[:space:]]+|-[^[:space:]]+|[0-9]+([.][0-9]+)?[smhd]?)[[:space:]]+)+/\1/; s/^([[:space:]]*)ionice[[:space:]]+(((-c|-n|-p|-P|-u|--class|--classdata|--pid|--pgid|--uid)([[:space:]]+|=)[^[:space:]]+|-[^[:space:]]+)[[:space:]]+)*/\1/; s/^([[:space:]]*)stdbuf[[:space:]]+(((-i|-o|-e|--input|--output|--error)([[:space:]]+|=)[^[:space:]]+|-[^[:space:]]+)[[:space:]]+)*/\1/; s/^([[:space:]]*)exec[[:space:]]+(((-a)[[:space:]]+[^[:space:]]+|-[^[:space:]]+)[[:space:]]+)*/\1/; s/^([[:space:]]*)xargs[[:space:]]+(((-a|-d|-E|-I|-i|-L|-n|-P|-s|--arg-file|--delimiter|--eof|--replace|--max-lines|--max-args|--max-procs|--max-chars|--process-slot-var)([[:space:]]+|=)[^[:space:]]+|-[^[:space:]]+)[[:space:]]+)*/\1/")
      # `xargs` leaves LEAD's bare wrapper list for the same reason `ionice` did: LEAD
      # would strip the word and then run on through its generic option arm, stopping
      # on the VALUE of a separate-argument option. `xargs -L 1 rm <protected>` halted
      # at `1`, so neither the rm extractor nor the stdin rule below ever ran.
      # `ionice` and `stdbuf` are deliberately NOT in LEAD's wrapper list, and that is
      # the whole fix rather than an aside: LEAD strips the bare word and then keeps
      # going through its generic `-[^[:space:]]+` arm, so `ionice -c 2 rm f` lost
      # `ionice` and `-c` and stalled on `2` -- leaving these rules nothing to match.
      # They take options with SEPARATE values, and the value then
      # sits where the command word should be -- `ionice -c 2 rm f` left `2 rm f`.
      # Their value-taking options are ENUMERATED rather than handled by a generic
      # "option then next word" rule, because arity is per-option: `ionice -t` and
      # `env -i` take none, so a generic rule would swallow the verb behind them and
      # convert a fail-open into a deeper one. Attached spellings (`-c2`) fall
      # through to the generic `-[^[:space:]]+` arm.
      i=$((i + 1))
    done
    printf '%s' "$t"
  }

  # Operators inside a QUOTED span are data. Splitting on them first turned
  # `printf '%s' 'safe; echo x > <protected>'` into a segment that looked executable
  # and blocked a command that only prints. They are masked to control characters
  # before the split and restored after it, so quoting survives the tokenizer.
  # Masks operators inside quoted spans, and DROPS heredoc bodies — a body is data
  # written somewhere, not syntax, so `cat > /tmp/script <<EOF` with a redirect in the
  # body was manufacturing an executable-looking segment. The interpreter check reads
  # $COMMAND directly, so a heredoc that really does write a protected path is still
  # caught there.
  mask_quoted() {
    awk '{
      # A heredoc terminator must be the ONLY thing on its line, in column zero.
      # `<<-` relaxes that for leading TABS and nothing else. Accepting arbitrary
      # surrounding whitespace for both forms ended the heredoc at an INDENTED
      # delimiter-lookalike, so the data lines after it were read as commands --
      # a false block on text that bash never executes.
      if (hd != "") {
        if ((hdash && $0 ~ ("^\t*" hd "$")) || (!hdash && $0 ~ ("^" hd "$"))) hd = ""
        print ""; next
      }
      if (match($0, /<<-?[[:space:]]*[\047\042]?[A-Za-z_][A-Za-z0-9_]*[\047\042]?/)) {
        t = substr($0, RSTART, RLENGTH)
        hdash = (substr($0, RSTART + 2, 1) == "-")
        gsub(/[<>\047\042[:space:]-]/, "", t); hd = t
      }
      q = ""; out = ""
      for (i = 1; i <= length($0); i++) {
        c = substr($0, i, 1)
        if (q == "") {
          # A backslash makes the next character DATA. `echo x \> path` performs no
          # redirect at all, yet the target extractor read `path` as one and blocked a
          # command that only prints. Masked like a quoted operator; `>` and `<` get
          # marks of their own that are never restored, since nothing downstream may
          # see them as syntax again.
          if (c == "\\") {
            nc = substr($0, i + 1, 1)
            if (nc == ";") out = out "\001"
            else if (nc == "&") out = out "\002"
            else if (nc == "|") out = out "\003"
            else if (nc == ">") out = out "\006"
            else if (nc == "<") out = out "\007"
            else out = out c nc
            i++; continue
          }
          if (c == "\047" || c == "\"") { q = c; out = out c; continue }
          # `#` at the START OF A WORD opens a comment: bash runs nothing after it,
          # yet `echo hi # > <protected>` was read as a real redirect and refused.
          # Done here because this is the only pass that knows the quote state --
          # a `#` inside quotes is data, and dropping the tail there would be a
          # FAIL-OPEN, not just a wrong answer: `echo \047 # \047 > <protected>`
          # would lose the redirect that actually happens. The word-start test is
          # what keeps a URL fragment or a `foo#bar` operand from being cut.
          # A `#` opens a comment at the start of a WORD, and an operator ends a
          # word just as whitespace does -- `echo hi;# > <protected>` performs no
          # redirect. The two conditional markers are tested by value rather than
          # inside the bracket expression, because an awk regex carrying a literal
          # control byte is exactly the portability shape that has already broken
          # this file once.
          if (c == "#") {
            pc = (out == "" ? "" : substr(out, length(out), 1))
            if (pc == "" || pc ~ /[[:space:];&|()]/ || pc == "\004" || pc == "\005") break
          }
          # `&&` / `||` open a CONDITIONAL segment, marked so a skipped assignment
          # cannot replace the value the shell actually used. The two are marked
          # SEPARATELY (\004 for `&&`, \005 for `||`) because they differ for the
          # working directory: if a later link of an `&&` chain runs at all, every
          # earlier `cd` in it ran, while a `||` branch may have been skipped
          # entirely. One shared marker made `cd a && cd b && echo x > f` treat the
          # second cd as optional and FALSE-BLOCKED it.
          # Done here rather than with sed because BSD sed drops the backslash in a
          # `\004` replacement, emitting the literal text 004 and mangling the
          # command. (`\n` in a replacement does produce a newline on BSD sed; the
          # octal escape is the part that does not.)
          if ((c == "&" || c == "|") && substr($0, i + 1, 1) == c) {
            out = out "\n" (c == "&" ? "\004" : "\005"); i++; continue
          }
          out = out c; continue
        }
        if (c == q) { q = ""; out = out c; continue }
        if (c == ";") out = out "\001"
        else if (c == "&") out = out "\002"
        else if (c == "|") out = out "\003"
        else out = out c
      }
      print out
    }'
  }

  # SCAN holds the RAW segments, split but not normalised. Normalisation happens
  # per segment inside the loop, because the variable state has to be accumulated
  # from the raw text in execution order and LEAD strips assignments away.
  # `bash -c '<cmd>'`, `sh -c` and `eval` EXECUTE their argument, so it is inlined
  # BEFORE tokenizing rather than appended to a segment afterwards. Appending gave
  # the extractors the text but not the structure: the inner `cd` never joined the
  # execution-order walk, so a relative write after it resolved from the outer
  # directory. Inlining makes the inner command an ordinary part of the stream.
  # A command substitution EXECUTES first: in `echo "$(touch <protected>)"` the touch
  # runs before echo ever does, yet the segment's leading command is `echo` and the
  # anchored write-verb extractors never see it. The bodies are appended to the scan
  # stream as segments of their own -- the same treatment `bash -c` gets, and for the
  # same reason. Both spellings; the original text is left in place as well, since a
  # substitution that merely READS still has to be seen by the read-side reasoning.
  # SINGLE-quoted spans are dropped first. A substitution inside them never executes,
  # so extracting one FALSE-BLOCKED `echo 'see $(rm <protected>)'` -- a command that
  # only prints. Double quotes are left alone: a substitution inside them does run.
  # Nested substitutions are extracted at every depth, by tracking parenthesis depth
  # rather than with `[^()]*`, which stopped at the innermost pair and left the outer
  # `touch` unseen. The OUTER body is what matters: `touch $(echo <protected>)`
  # reaches the extractors with its operand still spelled out, and the trailing `)`
  # comes off with the other closers. A flattened variant was tried alongside and
  # removed -- reverting it changed no outcome, so it was untestable weight.
  SUBSTS=$( { printf '%s' "$COMMAND" | sed -E "s/'[^']*'//g" \
                | awk '{
                    n = length($0)
                    for (i = 1; i <= n; i++) {
                      if (substr($0, i, 2) == "$(") {
                        d = 1; j = i + 2; st = j
                        while (j <= n) {
                          c = substr($0, j, 1)
                          if (c == "(") d++
                          else if (c == ")") { d--; if (d == 0) break }
                          j++
                        }
                        if (d == 0) print substr($0, st, j - st)
                        # A `case` pattern closes a parenthesis it never opened, so
                        # depth counting ends the body early: `$(case x in x) touch
                        # <protected>;; esac)` yielded only `case x in x`. Parsing
                        # shell constructs properly is out of scope for a
                        # PreToolUse hook, so a GREEDY body -- up to the last `)` in
                        # the command -- is emitted alongside the counted one. It
                        # over-collects, which costs nothing: the stream is split on
                        # `;` and `&&` afterwards, so a mutating command inside lands
                        # at the head of a segment of its own either way.
                        lp = 0
                        for (k = n; k > st; k--) if (substr($0, k, 1) == ")") { lp = k; break }
                        if (lp > st) {
                          g = substr($0, st, lp - st)
                          # `)` becomes a separator in this variant. A case PATTERN
                          # leaves the mutating command mid-segment -- `case x in x)
                          # touch <protected>` -- where the anchored write-verb
                          # extractors cannot reach it, and only the head of a segment
                          # is ever treated as a verb.
                          gsub(/\)/, ";", g); print g
                        }
                      }
                    }
                  }'
              printf '%s' "$COMMAND" | sed -E "s/'[^']*'//g" \
                | grep -oE '`[^`]*`' | tr -d '`'
              # `<(cmd)` and `>(cmd)` run their body as well, and the segment's
              # leading command is the consumer -- `cat <(touch <protected>)`.
              # Depth-counted for the same reason `$(` is: `[^()]*` stops at the
              # first `)`, so a body containing ANY parenthesised construct was
              # never emitted. `cat <(touch $(echo <protected>))` reached the
              # substitution pass as the inner `echo` alone, leaving the `touch`
              # behind the leading `cat` where no anchored extractor sees it.
              printf '%s' "$COMMAND" | sed -E "s/'[^']*'//g" \
                | awk '{
                    n = length($0)
                    for (i = 1; i <= n; i++) {
                      c2 = substr($0, i, 2)
                      if (c2 == "<(" || c2 == ">(") {
                        d = 1; j = i + 2; st = j
                        while (j <= n) {
                          c = substr($0, j, 1)
                          if (c == "(") d++
                          else if (c == ")") { d--; if (d == 0) break }
                          j++
                        }
                        if (d == 0) print substr($0, st, j - st)
                        # Same case-pattern fallback the `$(` scanner carries: a
                        # `case` pattern closes a parenthesis it never opened, so
                        # depth counting ends the body early and
                        # `<(case x in x) touch <protected>;; esac)` yielded only
                        # `case x in x`. The greedy variant over-collects, which
                        # costs nothing -- the stream is split on `;` afterwards.
                        lp = 0
                        for (k = n; k > st; k--) if (substr($0, k, 1) == ")") { lp = k; break }
                        if (lp > st) { g = substr($0, st, lp - st); gsub(/\)/, ";", g); print g }
                      }
                    }
                  }'; } 2>/dev/null || true)
  SCAN=$(printf '%s\n%s' "$COMMAND" "$SUBSTS" \
    | sed -E "s/(^|[[:space:];&|])[[:space:]]*((bash|sh|zsh|dash)[[:space:]]+-[a-zA-Z]*c|eval)[[:space:]]+'([^']*)'/\1\4/g" \
    | sed -E 's/(^|[[:space:];&|])[[:space:]]*((bash|sh|zsh|dash)[[:space:]]+-[a-zA-Z]*c|eval)[[:space:]]+"([^"]*)"/\1\4/g' \
    | sed 's/>|/>/g' \
    | sed -E 's/(-m|--message|-F)[[:space:]]*"[^"]*"/\1 /g' \
    | sed -E "s/(-m|--message|-F)[[:space:]]*'[^']*'/\1 /g" \
    | mask_quoted \
    | tr ';&|' '\n\n\n' \
    | tr '\001\002\003' ';&|' \
    | sed 's/$/ /')

  # Git global options may precede any verb, and this check sees RAW segments, so
  # it has to skip them itself or `git -C . apply` walks past.
  # Every global option, not a hand-picked few. The old list named four value-taking
  # options and three flags, so `git --no-replace-objects apply f.patch` left `apply`
  # behind an unrecognised token and the opaque-write guard never fired. Options that
  # take a SEPARATE argument stay enumerated -- consuming the argument of an unknown
  # one would eat the verb -- while every other `--flag`, `--flag=value` and `-x` is
  # matched generically, so a git release adding one cannot silently reopen this.
  GIT_GLOBAL='git[[:space:]]+(((-c|-C|--git-dir|--work-tree|--namespace|--attr-source|--super-prefix|--config-env)([[:space:]]+|=)[^[:space:]]+|--[A-Za-z][A-Za-z0-9-]*(=[^[:space:]]*)?|-[A-Za-z])[[:space:]]+)*'

  # A non-empty command MUST tokenize to something. It did not on GNU awk, where an
  # invalid bracket range aborted mask_quoted mid-pipeline: SCAN came back empty, no
  # targets were extracted, and the entire Bash side returned 0 without a word. A
  # tokenizer that dies must not read as "nothing to protect here".
  if [ -z "$(printf '%s' "$SCAN" | tr -d '[:space:]')" ]; then
    echo "BLOCKED: could not tokenize the command in protect-sensitive.sh. Failing closed." >&2
    exit 2
  fi

  # Bytes whose PATH never appears in the command: a patch file's contents, a stash
  # entry's contents. There is nothing here for a recogniser to extract, and reading
  # the patch or the stash is well past what a PreToolUse hook should attempt, so
  # these are blocked outright. Authorize with the sentinel `opaque-git-write` in the
  # branch marker. `git stash push`/`list`/`show` are unaffected.
  #
  # `patch` is the same shape and not git at all: `patch -p0 < f` names nothing.
  # ARCHIVE EXTRACTION is the same shape again: the paths written live inside the
  # archive, so `tar -xf a.tar` from the repo root can overwrite any protected file
  # and the command names none of them. Listing (`-t`) and creating (`-c`) are not
  # extraction and stay free.
  # An UNSCOPED `git clean -fd` belongs here too: with no pathspec it deletes every
  # untracked file in the tree and names none of them. Gated on a FORCE flag —
  # `-n`/`--dry-run` only lists, and without `-f` git refuses to delete at all, so
  # requiring a marker for those would be pure friction.
  # `restore` and `checkout` ARE safe verbs — but not when their source is a stash.
  # That is the same unreviewed content arriving through a different door, and the
  # safe-verb strip removes the verb before anything looks at `--source`.
  # Gated on the repo actually having protected patterns. Everything else here is
  # naturally inert without them, because it ends in matches_protected — but this
  # branch blocks on the VERB rather than on a path, so unguarded it fired in every
  # consuming repo with no .claude/repo-config.json (dev-tooling itself included),
  # breaking `git apply`, `git stash pop` and `patch` everywhere. The header's
  # promise is absent/empty patterns => nothing blocked, and it applies here too.
  # Normalised with the SAME pass the segment loop applies. This guard reads SCAN,
  # and LEAD moved into the loop two commits ago, so `sudo git apply f`,
  # `A=b git apply f` and `(git apply f)` all walked past it while bare `git apply f`
  # still blocked — fail-open in the branch that exists to catch unreviewable bytes.
  # The executable-path strip has to run here too, and for the same reason: it was
  # added to the segment loop alone, so `/usr/bin/git apply f`, `/bin/tar -xf a.tar`
  # and `/usr/bin/patch -p0` walked past the FAIL-CLOSED guard -- the higher-severity
  # branch -- while the lower-severity target extractors caught their equivalents.
  # Sharing one helper is what keeps the two sites from drifting apart again.
  OPAQUE=$(printf '%s' "$SCAN" | strip_lead)
  # A value-taking option's SEPARATE argument sits exactly where the scope and mode
  # expressions below expect option tokens, and both were defeated by it:
  #   `git clean -fd -e '*.tmp'`   -- the `-e` argument looked like a pathspec, so
  #                                   the unscoped-clean guard saw a scoped clean
  #   `tar -f archive.tar -x`      -- the archive operand sat between the options,
  #                                   so the extraction expression never reached -x
  # Each consumption is scoped to ITS OWN command, because arity is per-command and
  # getting that wrong is a live defect rather than a tidiness point: a first attempt
  # consumed `-f`'s next word everywhere, and git clean's `-f` is a FLAG, so
  # `git clean -f src` lost its pathspec and read as an unscoped clean -- turning the
  # fix into a false block on a scoped one.
  #
  # The command boundary is `[[:space:]]`, NOT `\b`. BSD `sed -E` does not support
  # `\b` in an address and matches NOTHING -- silently, with no error -- so the
  # first version of this was inert on the platform it was written on. Same family
  # as the awk bracket-range divergence this file already carries: a portability
  # difference that fails open rather than loudly.
  OPAQUE=$(printf '%s\n' "$OPAQUE" \
    | sed -E "/^[[:space:]]*${GIT_GLOBAL}clean[[:space:]]/ s/(^|[[:space:]])(-e|--exclude)([[:space:]]+|=)('[^']*'|\"[^\"]*\"|[^[:space:];&|]+)/\1/g" \
    | sed -E "/^[[:space:]]*(tar|bsdtar|gtar)[[:space:]]/ s/(^|[[:space:]])(-[a-zA-Z]*f|--file)([[:space:]]+|=)('[^']*'|\"[^\"]*\"|[^[:space:];&|]+)/\1\2/g")
  # ...minus the modes that are DOCUMENTED not to write. `patch --dry-run` says it
  # does not change files, `git apply --check` only tests applicability, and
  # `git clean -n` is a dry run — demanding a marker to VALIDATE a patch is friction
  # with no security content, and friction is what gets this switched off.
  # `-n` means DRY RUN for git clean and `--normal` for patch, which still applies
  # the patch — so the short form is recognised only for clean, never generally.
  # tar's `-O`/`--to-stdout` extracts to standard output, writing no member to disk,
  # so requiring authorization to INSPECT an archive's contents is friction with no
  # security content. Recognised bundled, since `-xOf` is how it is actually written.
  # unzip has read-only modes of its own: `-l`/`-v` list, `-t` tests, `-p`/`-c` write
  # to stdout, `-z` prints the comment. Added blanket, the extraction rule refused an
  # ARCHIVE INSPECTION -- a read -- which is the friction this file keeps warning
  # about. tar needed no equivalent: its rule matches `-x` and never `-t` or `-c`.
  # `git apply --stat`/`--numstat`/`--summary` report on a patch INSTEAD of applying
  # it, so they are reads and belong with `--check`. `--apply` combines a report mode
  # with application and is the exception the guard already carried.
  # The long forms are matched as standalone TOKENS. Unanchored, the substring test
  # was satisfied by operand text: `git apply /tmp/--check.patch` dropped its own
  # segment from OPAQUE and walked past the guard entirely.
  OPAQUE=$(printf '%s\n' "$OPAQUE" \
    | awk '!(/(^|[[:space:]])(--dry-run|--check|--stat|--numstat|--summary)([[:space:]]|=|$)/ && !/--apply/)' \
    | grep -vE "^[[:space:]]*${GIT_GLOBAL}clean([[:space:]]+-[^[:space:]]*)*[[:space:]]+-[a-zA-Z]*n" \
    | grep -vE '^[[:space:]]*unzip[^;&|]*[[:space:]]-[a-zA-Z]*[ltvzpc]' \
    | grep -vE '^[[:space:]]*(tar|bsdtar|gtar)[^;&|]*([[:space:]]-[a-zA-Z]*O|[[:space:]]--to-stdout)' || true)
  if [ -n "$(jq -r '.hooks.protected_patterns[]? // empty' "$RC_FILE" 2>/dev/null)" ] \
     && { printf '%s' "$OPAQUE" | grep -qE "^[[:space:]]*${GIT_GLOBAL}(apply|am)([[:space:]]|$)|^[[:space:]]*${GIT_GLOBAL}stash[[:space:]]+(apply|pop)([[:space:]]|$)|^[[:space:]]*patch([[:space:]]|$)|^[[:space:]]*${GIT_GLOBAL}clean([[:space:]]+-[^[:space:]]+)*[[:space:]]+(-[^[:space:]-]*f[^[:space:]]*|--force)([[:space:]]+-[^[:space:]]+)*[[:space:]]*\$" \
     || printf '%s' "$OPAQUE" | grep -qE "^[[:space:]]*(tar|bsdtar|gtar)([[:space:]]+-[^[:space:]]*)*[[:space:]]+(-[a-zA-Z]*x|--extract|--get)|^[[:space:]]*(tar|bsdtar|gtar)[[:space:]]+-?[a-zA-Z]*x[a-zA-Z]*([[:space:]]|\$)|^[[:space:]]*unzip([[:space:]]|\$)" \
     || printf '%s' "$OPAQUE" | grep -qE "^[[:space:]]*${GIT_GLOBAL}(restore|checkout)([[:space:]][^;&|]*)?(stash@\{|refs/stash([^/A-Za-z0-9_-]|\$))" \
     || printf '%s' "$OPAQUE" | grep -qE "^[[:space:]]*${GIT_GLOBAL}(restore|checkout)([[:space:]][^;&|]*)?[[:space:]]--pathspec-from-file([=[:space:]]|\$)" \
     || printf '%s' "$OPAQUE" | grep -qE '^[[:space:]]*curl([[:space:]]+[^;&|]*)?[[:space:]](-[a-zA-Z]*J[a-zA-Z]*|--remote-header-name)([[:space:]]|$)' \
     || printf '%s' "$OPAQUE" | grep -qE '^[[:space:]]*wget([[:space:]]+[^;&|]*)?[[:space:]]--content-disposition([[:space:]]|$)'; }; then
    marker_allows "opaque-git-write" || {
      echo "BLOCKED: patch/git apply/am/stash-restore/unscoped-clean/archive-extract writes bytes this hook cannot inspect. Requires explicit user approval." >&2
      exit 2
    }
  fi

  # Segments are walked IN EXECUTION ORDER with a running working directory, so a
  # `cd` applies only to writes that come AFTER it. Collecting every `cd` up front
  # and applying it to every target was retroactive: `echo x > login.go; cd
  # services/auth && cat login.go` blocked, although the only write was to an
  # unprotected root-level file and the `cd` served a read.
  # Variable state, accumulated as the walk proceeds. Resolving every target from
  # the command's FINAL assignment was fail-open: in
  # `p=<protected>; echo x > "$p"; p=safe` the redirect uses the first value, and a
  # global last-wins lookup answered `safe`. A shell assignment affects only what
  # comes after it.
  VARS=""
  VARS_ALT=""
  ALIASES=""
  CWD=""
  # Directories a CONDITIONAL `cd` may have entered. `cd a; true || cd ../b; echo x
  # > f` writes a/f, but the walk applied the second cd unconditionally and resolved
  # from b. The `&&`/`||` marker the assignment tracker already reads applies to
  # directory state too: a conditional cd ADDS a base rather than replacing one, and
  # every base is checked, which fails closed whichever branch actually ran.
  # Frames carry a `:` prefix so the repo ROOT is a non-empty line.
  CWD_ALT=""
  CWD_PENDING=""
  # Depth of the enclosing `if`/`case` block. Branches are MUTUALLY EXCLUSIVE, but
  # every one of their segments is unconditional in the `&&`/`||` sense, so textual
  # last-write-wins let a taken branch's value be replaced by an untaken one's:
  # `if true; then p=<protected>; else p=safe; fi; touch "$p"` checked only `safe`.
  # Inside a block, assignments are recorded as alternatives instead.
  BRANCH=0
  CWD_STACK=""
  INTERP_BASES=""
  while IFS= read -r raw; do
    [ -z "$raw" ] && continue
    # Assignments from the RAW segment, before LEAD strips them as leading noise.
    # Only the LEADING run counts: matching `name=value` anywhere let ordinary
    # argument text (`echo p=src/util.go > "$p"`) overwrite the tracked value and
    # redirect the check away from the protected path it had recorded. The assignment
    # builtins are stripped first, because `export NAME=VALUE` assigns before it
    # exports and the assignment is then no longer the leading word.
    # Captured before this segment's targets are checked, so a command-prefix
    # assignment (`p=x cmd > "$p"`) applies to its own command, as the shell does.
    # Control keywords are stripped alongside the assignment builtins. The match
    # below is ANCHORED, so `then p=<protected>` recorded nothing at all -- which is
    # why marking branch assignments as alternatives changed no outcome until this
    # was fixed too. The two are one defect wearing two hats.
    # The conditional marker is read off the raw segment and then removed, or the
    # anchored assignment match below never fires on a `&&`/`||` segment at all —
    # which recorded nothing rather than recording it as an alternative.
    # 0 = runs unconditionally (start of command, or after `;`)
    # 1 = `&&` link: reached only if everything before it succeeded
    # 2 = `||` link: may be skipped entirely
    case "$raw" in
      $'\004'*) cond=1 ;;
      $'\005'*) cond=2 ;;
      *) cond=0 ;;
    esac
    raw=$(printf '%s' "$raw" | tr -d '\004\005')
    # A `;` ENDS the chain, so an `&&`-guarded cd that was certain for the rest of
    # its chain becomes uncertain again: in `cd a && cd b; echo x > f` the write
    # runs even if `cd b` did not. The directory held before that cd is added back
    # as an alternative at the first unconditional segment after it.
    if [ "$cond" = 0 ] && [ -n "$CWD_PENDING" ]; then
      CWD_ALT=$(printf '%s\n%s' "$CWD_ALT" "$CWD_PENDING"); CWD_PENDING=""
    fi
    # `for NAME in WORDS` BINDS a variable the same way an assignment does, and the
    # binding was invisible: `for f in <protected>; do touch "$f"; done` resolved
    # `$f` to nothing and skipped the target. Every word is recorded as an
    # ALTERNATIVE, because the loop takes each in turn and the body runs for all of
    # them -- and because a `for` line is not a conditional, this is the one place
    # where several simultaneous values are literally correct rather than a
    # conservative over-approximation.
    # Opened before this segment's assignments are recorded and closed after, so the
    # `fi`/`esac` segment itself is still inside the block it terminates.
    case "$raw" in
      *[!A-Za-z0-9_]if[[:space:]]*|if[[:space:]]*|*[!A-Za-z0-9_]case[[:space:]]*|case[[:space:]]*)
        BRANCH=$((BRANCH + 1)) ;;
      *) ;;
    esac
    forvars=$(printf '%s' "$raw" \
      | grep -oE '^[[:space:]]*for[[:space:]]+[A-Za-z_][A-Za-z0-9_]*[[:space:]]+in[[:space:]]+[^;&|]*' \
      | sed -E 's/^[[:space:]]*for[[:space:]]+//' \
      | awk '{ n = $1; for (i = 3; i <= NF; i++) { if ($i == "do") continue; print n "=" $i } }' || true)
    if [ -n "$forvars" ]; then
      VARS_ALT=$(printf '%s\n%s' "$VARS_ALT" "$forvars")
    fi
    newvars=$(printf '%s' "$raw" \
      | sed -E 's/^[[:space:]]*((then|else|elif|do)[[:space:]]+)*(export|declare|local|readonly|typeset)?[[:space:]]*(-[^[:space:]]+[[:space:]]+)*//' \
      | grep -oE '^[[:space:]]*([A-Za-z_][A-Za-z0-9_]*=[^[:space:];&|]*[[:space:]]*)+' \
      | grep -oE '[A-Za-z_][A-Za-z0-9_]*=[^[:space:];&|]*' || true)
    # Redirections are stripped before that emptiness test: `p=<protected> > /tmp/log`
    # is an assignment WITH a redirection, which bash still persists, but the leftover
    # `> /tmp/log` made it read as a command and the value was never recorded — so a
    # later `echo x > "$p"` was checked against the stale one and allowed.
    # ...and only when the segment is assignments ALONE. A command-PREFIX assignment
    # (`p=src/util.go echo x > "$p"`) sets the variable for the command's environment,
    # but bash expands the redirection using the value from BEFORE it — so recording
    # it here pointed the check at the new path while the shell wrote the old one.
    if [ -n "$newvars" ] \
       && [ -z "$(printf '%s' "$raw" \
            | sed -E 's/^[[:space:]]*((then|else|elif|do)[[:space:]]+)*(export|declare|local|readonly|typeset)?[[:space:]]*(-[^[:space:]]+[[:space:]]+)*//' \
            | sed -E 's/^[[:space:]]*([A-Za-z_][A-Za-z0-9_]*=[^[:space:];&|]*[[:space:]]*)+//' \
            | sed -E 's/[0-9]*[<>]{1,2}&?[[:space:]]*[^[:space:];&|]*//g' \
            | tr -d '[:space:]')" ]; then
      # A segment after `&&`/`||` may not run, and the tokenizer otherwise discards
      # that. Recording it as a replacement let a SKIPPED assignment overwrite the
      # value the shell actually used. Conditional assignments are additive instead:
      # both the old and the new value are checked, which fails closed either way.
      # Either marker: unlike the working directory, an assignment inside an `&&`
      # chain is still uncertain for the rest of the COMMAND, because a `;` can
      # follow the chain and let a later write run with the earlier value.
      if [ "$cond" != 0 ] || [ "$BRANCH" -gt 0 ]; then
        VARS_ALT=$(printf '%s\n%s' "$VARS_ALT" "$newvars")
      else
        VARS=$(printf '%s\n%s' "$VARS" "$newvars")
      fi
    fi

    case "$raw" in
      *[!A-Za-z0-9_]fi[[:space:]]*|fi[[:space:]]*|*[!A-Za-z0-9_]esac[[:space:]]*|esac[[:space:]]*)
        [ "$BRANCH" -gt 0 ] && BRANCH=$((BRANCH - 1)) ;;
      *) ;;
    esac
    # A leading executable PATH hides the verb: every extractor here is anchored on a
    # bare command name, so `/bin/rm <protected>` and `/usr/bin/tee <protected>` were
    # not recognised as rm or tee at all. The directory part is stripped from the
    # first word only -- an OPERAND that happens to look like a path is untouched.
    seg=$(printf '%s' "$raw" | tr -d '\004' | strip_lead)
    [ -z "$seg" ] && continue
    # `git -C DIR` and `--work-tree=DIR` rebase relative operands for THIS segment
    # only, so they are captured before the global-option run is folded away.
    # Captured from ANYWHERE in that run rather than from the position immediately
    # after `git`: the strip below is now generic, and requiring these two to come
    # first only worked while every other global option was named in a list.
    GITBASE=""
    if printf '%s' "$seg" | grep -qE '^[[:space:]]*git[[:space:]]'; then
      GITBASE=$(printf '%s' "$seg" \
        | grep -oE '(^|[[:space:]])(-C[[:space:]]+|--work-tree[=[:space:]])[^[:space:];&|]+' \
        | head -1 | sed -E 's/.*(-C[[:space:]]+|--work-tree[=[:space:]])//')
      GITBASE="${GITBASE%\"}"; GITBASE="${GITBASE#\"}"; GITBASE="${GITBASE%\'}"; GITBASE="${GITBASE#\'}"
    fi
    # The SAME generic parser the opaque-write guard uses. This normalisation kept
    # the old four-option allowlist of its own, so `git --no-replace-objects rm
    # <protected>` left `rm` behind an unrecognised token and the anchored extractor
    # never saw the verb -- the identical defect, one copy further along.
    # Kept BEFORE the GIT_SAFE strip below, which removes the `git <verb>` prefix so
    # that a redirect trailing a read (`git diff > <protected>`) is still seen. That
    # strip also erases the verb, so any extractor that needs to know WHICH git
    # subcommand ran has to read this copy instead of `$seg`.
    PRESAFE="$seg"
    seg=$(printf '%s' "$seg" \
      | sed -E "s/^[[:space:]]*${GIT_GLOBAL}/ git /" \
      | sed -E "s/^[[:space:]]*git[[:space:]]+${GIT_SAFE}([[:space:]].*)?\$/\2/" \
      | sed 's/$/ /')

    # Candidate write TARGETS, gathered narrowly so that READING a protected file
    # stays free. `grep provision-secrets.yml x` and
    # `./scripts/test-provision-secrets-encoder.sh >/dev/null` must both pass — both
    # are routine, and a check that blocks them gets disabled within a day.
    # A redirect operator inside a QUOTED argument is data, not syntax:
    # `printf '%s\n' 'echo x > services/auth/login.go'` only prints. Redirect
    # TARGETS may themselves be quoted, so those are unquoted first and the
    # remaining quoted spans — which cannot be redirect targets — are dropped.
    # Inside `[[ ... ]]` a `>` is a lexicographic COMPARISON, not a redirect, and
    # `[[ x > <protected> ]]` writes nothing -- yet the unanchored redirect extractor
    # read the operand as an output path and refused a read-only conditional. The
    # whole construct is dropped before redirects are collected; a real redirect
    # cannot live inside `[[ ]]`, so nothing is lost by removing it wholesale.
    REDIRSEG=$(printf '%s' "$seg" | sed -E 's/\[\[[^]]*\]\]//g')
    # Unquoting a redirect TARGET is not enough on its own: the extractor below
    # stops at the first whitespace, so `> "safe dir/services/auth/login.go"`
    # became the target `safe` and the real path went unchecked. The spaces INSIDE
    # a quoted target are masked here and restored after extraction, which keeps
    # the path one token exactly as the shell treats it.
    REDIRSEG=$(printf '%s' "$REDIRSEG" | awk '{
      out = ""; n = length($0)
      for (i = 1; i <= n; i++) {
        c = substr($0, i, 1)
        if (c != ">") { out = out c; continue }
        out = out c
        if (substr($0, i + 1, 1) == ">") { i++; out = out ">" }
        j = i + 1
        while (j <= n && substr($0, j, 1) == " ") { out = out " "; j++ }
        q = substr($0, j, 1)
        if (q != "\"" && q != "\047") { i = j - 1; continue }
        k = j + 1
        while (k <= n && substr($0, k, 1) != q) {
          ch = substr($0, k, 1)
          out = out (ch == " " ? "\016" : ch)
          k++
        }
        i = k
      }
      print out
    }' | sed -E "s/'[^']*'//g; s/\"[^\"]*\"//g")

    TARGETS=$(
      # Redirects stay UNANCHORED: a redirect is legal anywhere in a segment.
      printf '%s' "$REDIRSEG" | grep -oE '>>?[[:space:]]*[^[:space:];&|)]+' \
        | sed -E 's/^>>?[[:space:]]*//' | tr '\016' ' ' \
        | awk -v r1="${REPO_ROOT:+$REPO_ROOT/}" -v r2="${REPO_REAL:+$REPO_REAL/}" '{
            # `>&2` and friends duplicate a descriptor; they name no file.
            if ($0 ~ /^&/) next
            # A repo checked out under /tmp is still the repo: its paths are never scratch.
            if ((r1 != "" && index($0, r1) == 1) || (r2 != "" && index($0, r2) == 1)) { print; next }
            # The scratch exemption keeps `> /tmp/x` free, and it was applied to the
            # RAW operand -- before `..` is collapsed. `> /tmp/../<repo>/services/
            # auth/login.go` therefore starts with `/tmp/`, was dropped here, and
            # wrote the protected file with the hook returning 0. A path containing
            # a `..` segment is not a scratch path: it is kept and normalised by
            # resolve_and_check, which decides on where it actually lands.
            if ($0 ~ /(^|\/)\.\.(\/|$)/) { print; next }
            if ($0 ~ /^(\/dev\/|\/tmp\/|\/private\/tmp\/)/) next
            print
          }'
      # Write VERBS are anchored to what LEAD left at the start of the segment.
      printf '%s' "$seg" | grep -oE '^[[:space:]]*tee\b[^;&|]*' | tr ' ' '\n' | grep -vE '^-|^tee$|^$'
      # A sed PROGRAM is not a file. `sed -i 's#/auth/#/public/#' src/util.go` emitted
      # its own substitution as a target and blocked an edit to an unprotected file,
      # so quoted spans are dropped and a bare s/y command with delimiters filtered.
      # The in-place boundary accepts a quote too, for the attached `-i''` spelling.
      # `-e SCRIPT` holds a PROGRAM. The bare-program filter below only recognises a
      # substitution (`s`/`y` followed by a delimiter), so an ADDRESS program such as
      # `-e '/auth/d'` read as a pathname and FALSE-BLOCKED an edit to an unprotected
      # file. Its argument is consumed before the quotes come off, or the unquoted
      # program would be indistinguishable from an operand.
      # The in-place option must start a WORD. Unanchored, `-[a-zA-Z]*i` followed by
      # `.` matched INSIDE an operand -- `sed -n 1p services/auth/x-ai.txt` hit the
      # `-ai.` in the filename and a plain read was refused. Found by auditing this
      # file's own cluster expressions for the per-option arity class the reviewers
      # kept reporting, rather than from a report.
      printf '%s' "$seg" | grep -oE "^[[:space:]]*(sed|perl)\b[^;&|]*[[:space:]](-[a-zA-Z]*i([[:space:]]|\.|'|\")|--in-place)[^;&|]*" \
        | sed -E "s/(^|[[:space:]])(-e|--expression)([[:space:]]+|=)('[^']*'|\"[^\"]*\"|[^[:space:]]+)/\1/g" \
        | sed -E "s/'([^']*)'/\1/g; s/\"([^\"]*)\"/\1/g" | tr ' ' '\n' | grep -vE "^-|^\$|^[sy][^[:alnum:][:space:]]"
      # $NF is the destination ONLY without -t. With `cp -t DIR SRC...` the last
      # operand is a SOURCE, so applying $NF there blocks a read.
      printf '%s' "$seg" | grep -oE '^[[:space:]]*(cp|install|ln)\b[^;&|]*' \
        | grep -vE '(^|[[:space:]])-t|--target-directory' | awk '{print $NF}'
      # A DIRECTORY destination takes the source's basename: `cp -r /tmp/src/auth
      # services/` writes `services/auth/**`, which the literal `services/` operand
      # does not name. This was keyed on the trailing separator, so the equally
      # ordinary `cp -r /tmp/auth services` -- with `services` an existing directory
      # -- named nothing and returned 0. The composition is now UNCONDITIONAL, which
      # is a superset of stat'ing the destination and needs neither the resolved base
      # nor a filesystem probe. When the destination is really a file the composed
      # child is a path that cannot exist, and it can only match a pattern in cases
      # where the destination itself already matches one.
      printf '%s' "$seg" | grep -oE '^[[:space:]]*(cp|install|ln|rsync)\b[^;&|]*' \
        | grep -vE '(^|[[:space:]])-t|--target-directory' \
        | while IFS= read -r cpline; do
            cpd=$(printf '%s' "$cpline" | awk '{print $NF}')
            cpdisdir=1
            # Case patterns inside this $( ) are written as `if` tests: bash 3.2 needs a
            # leading `(` on such patterns, and Semgrep's bash parser rejects that form.
            if [[ "$cpd" == */ ]]; then
              cpd="${cpd%/}"
            else
                  # Only when the destination REALLY IS a directory. Composing
                  # unconditionally was wrong, and the comment that justified it was
                  # wrong in a specific way worth recording: it assumed a composed
                  # child can only match when the destination already does. That
                  # holds for a directory-anchored pattern like `/auth/` and fails
                  # for a filename-shaped one. With `provision-secrets` in the
                  # pattern set, `cp scripts/provision-secrets-encoder.sh x.sh`
                  # composed `x.sh/provision-secrets-encoder.sh`, which matches --
                  # so a pure READ-copy was blocked. The repo's live patterns are
                  # mostly filename-shaped, and the test that missed it used /auth/.
                  cpabs="$cpd"
                  [[ "$cpabs" == /* ]] || cpabs="${REPO_ROOT:+${REPO_ROOT%/}/}${CWD:+${CWD%/}/}$cpd"
                  [[ -d "$cpabs" ]] || cpdisdir=0
            fi
            # A recursive copy to a destination that does NOT yet exist copies the
            # source AS that destination: `cp -r /tmp/tree services/new` creates
            # services/new/auth/login.go, with no `tree` component. Skipping the
            # non-directory case outright discarded that walk entirely -- a hole the
            # directory gate opened, not one it inherited.
            [ "$cpdisdir" = 0 ] || printf '%s' "$cpline" | awk -v d="$cpd" '{ for (i = 2; i < NF; i++) { if ($i ~ /^-/) continue; n = split($i, p, "/"); print d "/" p[n] } }'
            # A RECURSIVE copy brings the source's whole subtree with it:
            # `cp -r /tmp/tree services` creates services/tree/auth/login.go, which
            # neither `services` nor `services/tree` names. Bounded like every other
            # walk here, and only for sources that exist to be walked.
            if [[ "$cpline" == *' -'[a-zA-Z]*[rR]* || "$cpline" == *--recursive* || "$cpline" == *' -a'* ]]; then
                printf '%s' "$cpline" | tr ' ' '\n' | awk 'NR > 1 && $0 !~ /^-/ && $0 != ""' \
                  | sed '$d' \
                  | while IFS= read -r cpsrc; do
                      cpsabs="$cpsrc"
                      [[ "$cpsabs" == /* ]] || cpsabs="${REPO_ROOT:+${REPO_ROOT%/}/}${CWD:+${CWD%/}/}$cpsrc"
                      [ -d "$cpsabs" ] || continue
                      # Into `<dest>/<basename>/` when the destination exists as a
                      # directory, and directly into `<dest>/` when it does not --
                      # which is what cp itself does with a single source.
                      if [ "$cpdisdir" = 1 ]; then cppfx="${cpd}/${cpsrc##*/}"; else cppfx="$cpd"; fi
                      find "$cpsabs" -maxdepth 6 2>/dev/null \
                        | sed "s|^${cpsabs%/}/||" | grep -v "^${cpsabs}\$" \
                        | while IFS= read -r rel; do printf '%s/%s\n' "$cppfx" "$rel"; done
                    done
            fi
          done
      # `ln SRC LINK` creates a HARD link by default — an alias to the same inode, so
      # a later write through it is a write to the protected file. Only `-s` makes a
      # symlink, where the source is merely read.
      printf '%s' "$seg" | grep -oE '^[[:space:]]*ln\b[^;&|]*' \
        | grep -vE '(-[a-zA-Z]*s|--symbolic)' | tr ' ' '\n' | grep -vE '^-|^ln$|^$'
      # `rsync --remove-source-files` DELETES each source after transfer.
      printf '%s' "$seg" | grep -oE '^[[:space:]]*rsync\b[^;&|]*--remove-source-files[^;&|]*' \
        | tr ' ' '\n' | grep -vE '^-|^rsync$|^$'
      # rsync gets its own line: its destination is ALWAYS the last operand, it has
      # no target-directory form, and its `-t` means --times. Folding it in with
      # cp/install/ln made the -t filter drop the whole segment, so `rsync -t src
      # <protected>` — an ordinary flag — failed open.
      printf '%s' "$seg" | grep -oE '^[[:space:]]*rsync\b[^;&|]*' | awk '{print $NF}'
      # mv REMOVES its source, so both operands matter. cp/install sources are
      # reads and stay free — `cp <protected> /tmp/x` must not block.
      printf '%s' "$seg" | grep -oE '^[[:space:]]*(git[[:space:]]+)?mv\b[^;&|]*' \
        | grep -vE '(^|[[:space:]])-t|--target-directory' | tr ' ' '\n' | grep -vE '^-|^git$|^mv$|^$'
      printf '%s' "$seg" | grep -oE '^[[:space:]]*(git[[:space:]]+)?(cp|mv|install|ln)\b[^;&|]*(-t[[:space:]]*|--target-directory[=[:space:]])[^[:space:];&|]+' \
        | sed -E 's/.*(-t[[:space:]]*|--target-directory[=[:space:]])//'
      # `mv -t DIR SRC...` REMOVES every source as well, and the line above emits
      # only DIR — so the non-`-t` mv extractor being skipped left sources unchecked.
      printf '%s' "$seg" | grep -oE '^[[:space:]]*(git[[:space:]]+)?mv\b[^;&|]*(-t[[:space:]]|--target-directory)[^;&|]*' \
        | tr ' ' '\n' | grep -vE '^-|^git$|^mv$|^$'
      printf '%s' "$seg" | grep -oE '^[[:space:]]*dd\b[^;&|]*of=[^[:space:];&|]+' | grep -oE 'of=[^[:space:];&|]+' | sed 's/^of=//'
      # `-r RFILE` supplies a SIZE, so that file is read, not written. Collected as a
      # target it FALSE-BLOCKED `truncate -r <protected> /tmp/x`, which resizes only
      # the unprotected file. `-s` takes a size, not a path, and goes the same way.
      # `tar -cf ARCHIVE ...` creates or truncates ARCHIVE; `-r`/`-u` append to it.
      # Only EXTRACTION was treated as an archive operation, so the archive itself --
      # an ordinary file the command names outright -- was never checked.
      # Mode detection reads TARMODES, not the raw segment. An ATTACHED operand
      # (`-tfservices/auth/login.go`) is one word beginning with `-`, so scanning
      # the segment for `-[a-zA-Z]*[cru]` found the `c` of "services" INSIDE the
      # filename and read a read-only listing as an archive creation -- a false
      # block on `tar -tf<archive>`, and a self-inflicted one: the attached-operand
      # extractor added last round is what made that spelling reachable at all.
      # Truncating each short cluster at its `f` leaves only real option letters.
      # `--delete` removes members from an existing archive and `-A`/`--catenate`
      # append whole archives to it; both MUTATE the file named by `-f`, so they
      # belong with create/append/update rather than with the read-only modes.
      # tar's OLD-STYLE form takes its option cluster as a bare word: `tar cf X ...`
      # is `tar -cf X ...`. Both the mode test and the archive extraction below
      # require a leading `-`, so the dashless spelling emitted no target at all.
      # Normalised to the modern form once, here, rather than teaching every
      # expression below about a second syntax. The extraction guard already
      # accepted it via its own `-?`, so only this side was blind.
      TARSEG=$(printf '%s' "$seg" | grep -oE '^[[:space:]]*(tar|bsdtar|gtar)\b[^;&|]*' \
        | sed -E 's/^([[:space:]]*(tar|bsdtar|gtar)[[:space:]]+)([A-Za-z]+)([[:space:]]|$)/\1-\3\4/')
      TARMODES=$(printf '%s' "$TARSEG" \
        | sed -E 's/(^|[[:space:]])(-[a-eg-zA-EG-Z]*f)[^[:space:];&|]*/\1\2/g')
      if printf '%s' "$TARMODES" | grep -qE '(^|[[:space:]])-[a-zA-Z]*[cruA]|(^|[[:space:]])--(create|append|update|delete|catenate|concatenate)'; then
        printf '%s' "$TARSEG" \
          | grep -oE '(^|[[:space:]])(-[a-zA-Z]*f[[:space:]]+|--file[=[:space:]])[^[:space:];&|]+' \
          | sed -E 's/.*(-[a-zA-Z]*f[[:space:]]+|--file[=[:space:]])//'
        # ...and the ATTACHED short-option spelling `-cfARCHIVE`, which needs its own
        # extractor rather than relaxing the one above from `[[:space:]]+` to `*`: the
        # greedy `.*` in that strip would then re-anchor on a `-...f` INSIDE the
        # operand, so `tar -cf /tmp/x-conf.tar` would return `.tar` and the real path
        # would go unchecked. The option cluster is matched as "letters other than f,
        # then f" for the same reason in miniature -- a greedy `[a-zA-Z]*f` against
        # `-cfafile.tar` consumes through the `f` of `file` and returns `ile.tar`.
        printf '%s' "$TARSEG" \
          | grep -oE '(^|[[:space:]])-[a-eg-zA-EG-Z]*f[^[:space:];&|=-][^[:space:];&|]*' \
          | sed -E 's/^[[:space:]]*-[a-eg-zA-EG-Z]*f//'
      fi
      printf '%s' "$seg" | grep -oE '^[[:space:]]*truncate\b[^;&|]*' \
        | sed -E "s/(^|[[:space:]])(-r|--reference|-s|--size)([[:space:]]+|=)('[^']*'|\"[^\"]*\"|[^[:space:]]+)/\1/g" \
        | tr ' ' '\n' | grep -vE '^-|^truncate$|^$'
      # `touch` creates a missing file empty, so it can bring a protected path into
      # existence that the equivalent Write would have refused.
      # `-r FILE` is READ for its timestamps, and `-d`/`-t` carry a time, not a path.
      # Treating every non-option token as a target FALSE-BLOCKED
      # `touch -r <protected> /tmp/x`, which modifies only the unprotected file.
      printf '%s' "$seg" | grep -oE '^[[:space:]]*touch\b[^;&|]*' \
        | sed -E "s/(^|[[:space:]])(-r|--reference|-d|--date|-t)([[:space:]]+|=)('[^']*'|\"[^\"]*\"|[^[:space:]]+)/\1/g" \
        | tr ' ' '\n' | grep -vE '^-|^touch$|^$'
      # Changing a control's MODE or OWNER can make it unavailable, or make it
      # executable, without writing a byte of it. The first operand is the mode or
      # owner rather than a path -- except with `--reference`, where there is no such
      # operand and every remaining one is a target.
      printf '%s' "$seg" | grep -oE '^[[:space:]]*(chmod|chown|chgrp)\b[^;&|]*' | grep -v -- '--reference' \
        | tr ' ' '\n' | grep -vE '^-|^chmod$|^chown$|^chgrp$|^$' | awk 'NR > 1'
      printf '%s' "$seg" | grep -oE '^[[:space:]]*(chmod|chown|chgrp)\b[^;&|]*--reference[^;&|]*' \
        | sed -E 's/--reference(=|[[:space:]]+)[^[:space:]]+//g' \
        | tr ' ' '\n' | grep -vE '^-|^chmod$|^chown$|^chgrp$|^$'
      # A downloader writes wherever its output option points, and nothing else in
      # this list recognises one. Both spellings of curl's short form, since `-ox`
      # is as valid as `-o x`. Collected in the destination block below rather than
      # here: emitted here as well, they were emitted UNCONDITIONALLY, which
      # reinstated the bare name the directory-composition logic exists to suppress.
      # curl and wget both name their destination in several places, and a
      # destination DIRECTORY changes what the other options mean. Collected once
      # into a name list, then resolved, rather than as a pile of independent
      # extractors -- three rounds of patching this as separate pipelines is what
      # produced the false block corrected below.
      #
      # `-O`/`--remote-name` takes no argument: curl writes the URL's basename into
      # the current directory, so the written path is never spelled in the command.
      # `--output-dir DIR` moves that write into DIR, and curl documents it as
      # applying to `--output` too. LAST occurrence wins -- curl's manual says so.
      #
      # A RELATIVE name under an effective directory is resolved INSIDE it, and the
      # bare name is then not a written path at all. An earlier revision emitted the
      # bare name alongside the composed one, reasoning that an extra candidate can
      # only ever block more -- true, and the wrong test, because blocking more
      # includes blocking a path nothing writes. `curl --output-dir /tmp/out -o
      # services/auth/login.go` writes /tmp/out/services/auth/login.go and refusing
      # it was a FALSE BLOCK, the failure mode this file treats as worst. An
      # ABSOLUTE name is left alone: it does not live under the directory.
      CURLSEG=$(printf '%s' "$seg" | grep -oE '^[[:space:]]*curl\b[^;&|]*')
      CURLDIR=$(printf '%s' "$CURLSEG" \
        | grep -oE '(^|[[:space:]])--output-dir([[:space:]]+|=)[^[:space:];&|]+' \
        | sed -E 's/.*--output-dir([[:space:]]+|=)//' | tail -1)
      { printf '%s' "$CURLSEG" \
          | grep -oE '(^|[[:space:]])(-o|--output)([[:space:]]+|=)[^[:space:];&|]+' \
          | sed -E 's/.*(-o|--output)([[:space:]]+|=)//'
        # `-o` need not START the token: `curl -soFILE` is a valid cluster. The
        # letters allowed before it are ENUMERATED as curl's VALUELESS short flags,
        # not `[a-zA-Z]*` -- `-d` takes a value, so `-doX` means `-d oX` and reading
        # `oX` as a destination would be a false block. Same per-option arity
        # reasoning as everywhere else in this file.
        printf '%s' "$CURLSEG" \
          | grep -oE '(^|[[:space:]])-[sSkLvifgjNZR46]*o[^[:space:];&|=-][^[:space:];&|]*' \
          | sed -E 's/^[[:space:]]*-[sSkLvifgjNZR46]*o//'
        printf '%s' "$CURLSEG" \
          | grep -oE '(^|[[:space:]])-[sSkLvifgjNZR46]+o([[:space:]]+)[^[:space:];&|]+' \
          | sed -E 's/^[[:space:]]*-[sSkLvifgjNZR46]+o[[:space:]]+//'
        printf '%s' "$CURLSEG" \
          | grep -qE '(^|[[:space:]])(-[a-zA-Z]*O|--remote-name|--remote-name-all)([[:space:]]|$)' \
          && printf '%s' "$CURLSEG" | tr ' ' '\n' \
               | grep -E '://|^file:' | sed -E 's|[?#].*$||; s|.*/||' | grep -v '^$'
        :; } | awk -v d="$CURLDIR" 'NF { if (d == "" || substr($0, 1, 1) == "/") print; else print d "/" $0 }'

      # wget's short options take ATTACHED arguments as well as separate ones, so
      # `-Oservices/auth/login.go` named a document no extractor collected.
      #
      # With no output option at all wget still writes the URL's basename into the
      # working directory -- `cd services/auth && wget http://host/login.go` creates
      # a protected file naming only its URL. `-P`/`--directory-prefix` moves THAT
      # write, and only that one: wget's `-O` names a path outright and its log
      # options are not download destinations, so neither is composed with the
      # prefix. `-O` also suppresses the implicit name entirely.
      WGETSEG=$(printf '%s' "$seg" | grep -oE '^[[:space:]]*wget\b[^;&|]*')
      WGETDIR=$(printf '%s' "$WGETSEG" \
        | grep -oE '(^|[[:space:]])(-P|--directory-prefix)([[:space:]]+|=)[^[:space:];&|]+' \
        | sed -E 's/.*(-P|--directory-prefix)([[:space:]]+|=)//' | tail -1)
      printf '%s' "$WGETSEG" \
        | grep -oE '(^|[[:space:]])(-O|--output-document)([[:space:]]+|=)[^[:space:];&|]+' \
        | sed -E 's/.*(-O|--output-document)([[:space:]]+|=)//'
      # Clustered spellings, `wget -qOFILE`. As with curl the preceding letters are
      # enumerated valueless flags rather than `[a-zA-Z]*`, so an option that takes a
      # value cannot have its argument read as a destination.
      printf '%s' "$WGETSEG" \
        | grep -oE '(^|[[:space:]])-[qvbcNSkKmpEnrxdh46]*O[^[:space:];&|=-][^[:space:];&|]*' \
        | sed -E 's/^[[:space:]]*-[qvbcNSkKmpEnrxdh46]*O//'
      printf '%s' "$WGETSEG" \
        | grep -oE '(^|[[:space:]])-[qvbcNSkKmpEnrxdh46]+O[[:space:]]+[^[:space:];&|]+' \
        | sed -E 's/^[[:space:]]*-[qvbcNSkKmpEnrxdh46]+O[[:space:]]+//'
      # ...and its LOG, which is a different option in the same case-sensitive pair.
      # `-o FILE` creates or truncates that file even when the download itself fails.
      printf '%s' "$WGETSEG" \
        | grep -oE '(^|[[:space:]])(-o|-a|--output-file|--append-output)([[:space:]]+|=)[^[:space:];&|]+' \
        | sed -E 's/.*(-o|-a|--output-file|--append-output)([[:space:]]+|=)//'
      printf '%s' "$WGETSEG" \
        | grep -oE '(^|[[:space:]])-[qvbcNSkKmpEnrxdh46]*[oa][^[:space:];&|=-][^[:space:];&|]*' \
        | sed -E 's/^[[:space:]]*-[qvbcNSkKmpEnrxdh46]*[oa]//'
      # `--spider` probes without saving, so its URL basename is not a written path.
      # The suppression needs the SAME per-option arity the extractor uses. With a
      # bare `-[a-zA-Z]*O` it found the `O` in `-UOfoo` -- where `-U` takes a value
      # and `Ofoo` is a user-agent string -- suppressed the implicit destination, and
      # left no target at all.
      printf '%s' "$WGETSEG" | grep -qE '(^|[[:space:]])(-[qvbcNSkKmpEnrxdh46]*O|--output-document|--spider)' \
        || printf '%s' "$WGETSEG" | tr ' ' '\n' \
             | grep -E '://|^ftp:' | sed -E 's|[?#].*$||; s|.*/||' | grep -v '^$' \
             | awk -v d="$WGETDIR" 'NF { if (d == "" || substr($0, 1, 1) == "/") print; else print d "/" $0 }'
      # `xargs` feeds operands from STDIN, so a mutating verb behind it names no
      # target in its own segment -- `printf '%s\n' <protected> | xargs rm` deleted
      # the file with this hook returning 0. The paths are not in THIS segment, but
      # in practice they are in another one: the pipeline's producer. So every
      # non-option token in the whole command becomes a candidate here.
      #
      # Deliberately NOT a blanket opaque block. `find . -name '*.tmp' | xargs rm` is
      # routine, names nothing protected, and refusing it is exactly the friction
      # this file keeps warning about. The residual is stated rather than papered
      # over: `cat list.txt | xargs rm` cannot be decided without reading list.txt,
      # which is past what a PreToolUse hook should attempt.
      # Scoped to the PIPELINE holding the xargs, not the whole command. Reading
      # `$COMMAND` entire meant a protected path named in an unrelated read segment
      # became a write candidate: `ls tmp | xargs rm; grep TODO <protected>` was
      # refused though nothing writes that file -- read-blocking friction, which is
      # the thing this rule was deliberately shaped to avoid in the first place.
      #
      # The pipeline boundary is not available from `$raw`: the tokenizer turns `;`,
      # `&` and `|` all into newlines, so by the time a segment exists the producer
      # is a separate segment and `;` is indistinguishable from `|`. `mask_quoted`
      # still holds the distinction -- it marks `&&`/`||` and masks quoted
      # separators -- so the run is recovered by re-masking `$COMMAND` and splitting
      # on `;`, `&` and the two conditional markers -- all of which end a pipeline --
      # while leaving `|` inside the line where it belongs. The MARKERS matter: they
      # are control bytes prefixed to the following run, so a producer after `&&`
      # began with one and no `^`-anchored test could see it.
      #
      # Within that run only PRODUCERS whose operands ARE the paths are read. A
      # producer that reads a LIST from a file names its source, not its targets:
      # `cat services/auth/files-to-delete.txt | xargs rm` deletes whatever the file
      # CONTAINS, so treating the filename as a deletion target refused a command
      # that may touch nothing protected. That is the same read-blocking friction
      # this rule was shaped to avoid, one level in.
      if [[ "$raw" == *xargs* ]]; then
          printf '%s' "$seg" | grep -qE "^[[:space:]]*(${MUTATING_VERBS}([[:space:]]|\$)|sed([[:space:]]+-[a-zA-Z]*)*[[:space:]]+-[a-zA-Z]*i)" \
            && printf '%s' "$COMMAND" | mask_quoted | tr '\004\005;&' '\n\n\n\n' \
                 | awk '{
                     # A run qualifies only when some pipeline element IS an xargs
                     # command. `grep -F xargs` also matched the word as quoted DATA,
                     # so `echo \047xargs <protected>\047; printf x | xargs rm`
                     # dragged the printed path in as a deletion target.
                     n = split($0, el, "[|]")
                     ok = 0
                     for (i = 1; i <= n; i++) {
                       t = el[i]; sub(/^[[:space:]]+/, "", t)
                       if (t ~ /^xargs([[:space:]]|$)/) ok = 1
                     }
                     if (!ok) next
                     for (i = 1; i <= n; i++) {
                       t = el[i]; sub(/^[[:space:]]+/, "", t)
                       m = split(t, w, /[[:space:]]+/)
                       if (m == 0) continue
                       if (w[1] == "ls") {
                         # `ls DIR` prints BASENAMES, so the child resolves them in
                         # the CURRENT directory -- `ls services/auth | xargs rm`
                         # removes ./login.go, not the protected file, and treating
                         # the operand as a target was a false block. A GLOB is
                         # different: the shell expands it to the paths as written,
                         # so those operands are real candidates.
                         for (j = 2; j <= m; j++)
                           if (w[j] !~ /^-/ && w[j] ~ /[*?[]/) print w[j]
                         continue
                       }
                       if (w[1] == "printf" || w[1] == "echo" || w[1] == "find" || w[1] == "xargs") {
                         for (j = 2; j <= m; j++) if (w[j] !~ /^-/) print w[j]
                         continue
                       }
                       if (w[1] == "git" && m >= 2 && (w[2] == "ls-files" || w[2] == "diff"))
                         for (j = 3; j <= m; j++) if (w[j] !~ /^-/) print w[j]
                     }
                   }' | grep -vE '^-|^$'
      fi
      # Deleting a control removes it exactly as effectively as overwriting it.
      printf '%s' "$seg" | grep -oE '^[[:space:]]*(git[[:space:]]+)?(rm|unlink)\b[^;&|]*' | tr ' ' '\n' | grep -vE '^-|^git$|^rm$|^unlink$|^$'
    ) || true

    # Git SUBCOMMAND operands, kept separate because only these follow `git -C`.
    # A shell redirect is resolved by the shell against the real working directory:
    # in `git -C services diff > report.txt` the redirect writes ./report.txt, and
    # rebasing it onto `services/` would be a false block.
    GITTARGETS=$(
      printf '%s' "$seg" | grep -oE '^[[:space:]]*git[[:space:]]+(mv|rm)\b[^;&|]*' | tr ' ' '\n' | grep -vE '^-|^git$|^mv$|^rm$|^$'
      printf '%s' "$seg" | grep -oE '^[[:space:]]*git[[:space:]]+clean\b[^;&|]*' | tr ' ' '\n' | grep -vE '^-|^git$|^clean$|^$'
      printf '%s' "$seg" | grep -oE '^[[:space:]]*git[[:space:]]+config\b[^;&|]*(-f|--file)(=|[[:space:]]+)[^[:space:];&|]+' \
        | sed -E 's/.*(-f|--file)(=|[[:space:]]+)//'
      # `git checkout <treeish> -- <paths>`, `git checkout <treeish> <paths>` and
      # `git restore [--source=X] <paths>` OVERWRITE the named files from a tree.
      # This is the EASY case, not the opaque one -- the path is spelled out -- and
      # all of them returned 0. They belong in THIS list for the same reason
      # `git archive` does: `git -C services restore auth/login.go` writes
      # services/auth/login.go, and only GITTARGETS composes onto GITBASE. An
      # earlier revision put them with the shell targets and resolved every path
      # from the wrong directory.
      #
      # They read PRESAFE, not seg: the GIT_SAFE normalisation strips the
      # `git <verb>` prefix so that a redirect trailing a read (`git diff > f`) is
      # still seen, and that strip erases the verb these need.
      #
      # `git checkout <tree>` with ONE operand is a branch SWITCH -- it writes
      # whatever the diff touches while naming nothing, and refusing every branch
      # change is the friction this file keeps warning about. So the tree-ish is
      # dropped and only the operands AFTER it are collected. `git checkout -b new
      # <start-point>` therefore contributes its start-point as a candidate; that
      # over-collects, in the fail-closed direction, and is preferred to leaving
      # `git checkout HEAD~1 <protected>` open.
      printf '%s' "$PRESAFE" | grep -oE "^[[:space:]]*${GIT_GLOBAL}checkout([[:space:]]|$)[^;&|]*" \
        | sed -E "s/^[[:space:]]*${GIT_GLOBAL}checkout//" \
        | awk '{
            p = index($0, " -- ")
            if (p > 0) {
              n = split(substr($0, p + 4), a, /[[:space:]]+/)
              for (i = 1; i <= n; i++) if (a[i] != "" && substr(a[i], 1, 1) != "-") print a[i]
              next
            }
            n = split($0, a, /[[:space:]]+/); c = 0
            for (i = 1; i <= n; i++) if (a[i] != "" && substr(a[i], 1, 1) != "-") ops[++c] = a[i]
            for (i = 2; i <= c; i++) print ops[i]
          }'
      printf '%s' "$PRESAFE" | grep -oE "^[[:space:]]*${GIT_GLOBAL}restore([[:space:]]|$)[^;&|]*" \
        | sed -E "s/(^|[[:space:]])(-s|--source)([[:space:]]+|=)[^[:space:]]+/\1/g" \
        | sed -E "s/^[[:space:]]*${GIT_GLOBAL}restore//" \
        | tr ' ' '\n' | grep -vE '^-|^$'
      # `git archive -o FILE` writes the archive there. It belongs HERE rather than
      # with the shell targets: `git -C DIR` chdirs before opening that file, so
      # `git -C services archive -o auth/login.go HEAD` writes services/auth/login.go
      # -- and only this list is composed onto GITBASE.
      printf '%s' "$seg" | grep -oE '^[[:space:]]*git[[:space:]]+archive\b[^;&|]*' \
        | grep -oE '(^|[[:space:]])(-o|--output)([[:space:]]+|=)[^[:space:];&|]+' \
        | sed -E 's/.*(-o|--output)([[:space:]]+|=)//'
      printf '%s' "$seg" | grep -oE '^[[:space:]]*git[[:space:]]+archive\b[^;&|]*' \
        | grep -oE '(^|[[:space:]])-o[^[:space:];&|=-][^[:space:];&|]*' | sed -E 's/^[[:space:]]*-o//'
    ) || true

    # The unconditional directory plus every directory a conditional `cd` may have
    # entered. Frames are `:`-prefixed so the repo root survives as a real line.
    CWDLIST=$(printf ':%s\n%s' "$CWD" "$CWD_ALT")
    for list in T G; do
      case "$list" in
        T) items="$TARGETS" ;;
        G) items="$GITTARGETS" ;;
        *) ;;
      esac
      while IFS= read -r bframe; do
      case "$bframe" in
          :*) cwd0="${bframe#:}" ;; *) continue ;;
        esac
      case "$list" in
        T) base="$cwd0" ;;
        G) # A relative `-C` is resolved by git against the shell's directory, so
           # the two bases COMPOSE: `cd services && git -C auth mv x login.go`
           # writes services/auth/login.go, which neither base names alone.
           case "$GITBASE" in
             "") base="$cwd0" ;;
             /*) base="$GITBASE" ;;
             *)  base=$(norm_path "${cwd0:+${cwd0%/}/}$GITBASE") ;;
           esac ;;
        *) ;;
      esac
      while IFS= read -r t; do
        [ -z "$t" ] && continue
        t="${t%\"}"; t="${t#\"}"; t="${t%\'}"; t="${t#\'}"
        t="${t%\)}"; t="${t%\}}"
        [ -z "$t" ] && continue
        # A literal assignment earlier in the command names the real path. EVERY value
        # it could hold is checked — the last unconditional one, plus any recorded
        # behind `&&`/`||`, where the tokenizer cannot know whether the branch ran.
        while IFS= read -r tv; do
          [ -z "$tv" ] && continue
          case "$tv" in
              \$*) continue ;; *) ;;
            esac   # unresolved expansion: nothing to check
          # A symlink DEREFERENCES, so a write through the alias reaches the protected
          # file: `ln -s <protected> a; echo x > a`.
          while IFS= read -r av; do
            [ -n "$av" ] && resolve_and_check "$av" "$base"
          done <<< "$(printf '%s\n' "$ALIASES" | awk -F'\t' -v a="$tv" '$1 == a { print $2 }')"
          while IFS= read -r gv; do
            [ -n "$gv" ] && resolve_and_check "$gv" "$base"
          done <<< "$(glob_expand "$tv" "$base")"
        done <<< "$(brace_expand "$t" | while IFS= read -r bx; do var_values "$bx"; done)"
      done <<< "$items"
      done <<< "$CWDLIST"
    done

    # `rm -r <dir>` removes everything BENEATH the operand, so `rm -rf services`
    # takes services/auth/** while neither `services` nor `services/` matches a
    # `/auth/` pattern. See list_descendants for how the contents are enumerated.
    # `git clean -f <dir>` removes untracked files BENEATH the pathspec, so it is a
    # recursive delete wearing a different verb. `-d` is not the thing that makes it
    # recursive -- it only adds whole untracked DIRECTORIES to what may be removed --
    # so requiring it left `git clean -f services` walking nothing. Only the FORCED,
    # non-dry-run form: `-n`/`--dry-run` reports without removing, and blocking that
    # would refuse a read. With no pathspec at all the whole tree is in scope, which
    # the unscoped-clean guard above already refuses.
    DEL_OPERANDS=""; DEL_VERB="recursive delete"
    if printf '%s' "$seg" | grep -qE '^[[:space:]]*(git[[:space:]]+)?rm\b[^;&|]*([[:space:]]-[a-zA-Z]*[rR]|[[:space:]]--recursive)'; then
      DEL_OPERANDS=$(printf '%s' "$seg" | grep -oE '^[[:space:]]*(git[[:space:]]+)?rm\b[^;&|]*' | tr ' ' '\n' | grep -vE '^-|^git$|^rm$|^$')
    elif printf '%s' "$seg" | grep -qE "^[[:space:]]*${GIT_GLOBAL}clean\b" \
         && printf '%s' "$seg" | grep -qE '[[:space:]]-[a-zA-Z]*f|[[:space:]]--force' \
         && ! printf '%s' "$seg" | grep -qE '[[:space:]]-[a-zA-Z]*n|[[:space:]]--dry-run'; then
      DEL_OPERANDS=$(printf '%s' "$seg" | grep -oE "^[[:space:]]*${GIT_GLOBAL}clean\b[^;&|]*" | tr ' ' '\n' | grep -vE '^-|^git$|^clean$|^$')
      DEL_VERB="recursive clean"
    elif printf '%s' "$seg" | grep -qE '^[[:space:]]*(chmod|chown|chgrp)\b[^;&|]*([[:space:]]-[a-zA-Z]*R|[[:space:]]--recursive)'; then
      # A recursive mode or ownership change reaches every descendant, so a protected
      # child can be disabled through an unprotected parent: `chmod -R 000 services`
      # named only `services`, which matches no pattern. The first operand is still
      # the mode or owner rather than a path.
      DEL_OPERANDS=$(printf '%s' "$seg" | grep -oE '^[[:space:]]*(chmod|chown|chgrp)\b[^;&|]*' | grep -v -- '--reference' \
        | tr ' ' '\n' | grep -vE '^-|^chmod$|^chown$|^chgrp$|^$' | awk 'NR > 1')
      [ -n "$DEL_OPERANDS" ] || DEL_OPERANDS=$(printf '%s' "$seg" | grep -oE '^[[:space:]]*(chmod|chown|chgrp)\b[^;&|]*--reference[^;&|]*' \
        | sed -E 's/--reference(=|[[:space:]]+)[^[:space:]]+//g' | tr ' ' '\n' | grep -vE '^-|^chmod$|^chown$|^chgrp$|^$')
      DEL_VERB="recursive permission change"
    elif printf '%s' "$seg" | grep -qE "^[[:space:]]*find\b[^;&|]*[[:space:]](-delete([[:space:]]|\$)|-exec(dir)?[[:space:]]+${MUTATING_VERBS}([[:space:]]|\$))"; then
      # `find <root> ... -delete` removes matches anywhere beneath the root and names
      # none of them. `-exec`/`-execdir` running a mutating verb is the same shape --
      # `find <root> -name x -exec rm {} +` deletes just as surely, and the nested
      # `rm` never appears at a segment head where an anchored extractor could see
      # it. Both are read as "everything under the root is at risk". The roots are the operands before the first predicate; which
      # files the predicates actually select is not decidable here, so every
      # descendant of the root counts -- the conservative reading, as elsewhere.
      # `-H`, `-L`, `-P` and `-Olevel` are GLOBAL options and precede the roots, so
      # stopping at the first `-` stopped before the path had been reached at all:
      # `find -H services/auth -delete` collected nothing. They are skipped; every
      # other leading `-` really is the start of the expression.
      DEL_OPERANDS=$(printf '%s' "$seg" | grep -oE '^[[:space:]]*find\b[^;&|]*' \
        | sed -E 's/(^|[[:space:]])find([[:space:]]+(-[HLP]|-O[0-9]*))+/\1find/' | tr ' ' '\n' \
        | awk 'NR > 1 { if ($0 ~ /^-/ || $0 == "") exit; print }')
      DEL_VERB="find -delete"
    fi
    if [ -n "$DEL_OPERANDS" ]; then
      while IFS= read -r draw; do
        [ -z "$draw" ] && continue
        # Quotes come off FIRST, as the ordinary target loop does it: `"$d"` is not a
        # variable reference until they are gone, so var_values returned it verbatim.
        draw="${draw%\"}"; draw="${draw#\"}"; draw="${draw%\'}"; draw="${draw#\'}"
        [ -z "$draw" ] && continue
        case "$draw" in
            -*) continue ;; *) ;;
          esac
        # The ordinary target pass expands variables; this one walked the literal
        # `$d`, found no such directory and enumerated nothing, so `d=services;
        # rm -rf "$d"` never saw services/auth/login.go.
        while IFS= read -r d; do
        [ -z "$d" ] && continue
        case "$d" in
            \$*) continue ;; *) ;;
          esac
        while IFS= read -r bframe; do
        case "$bframe" in
            :*) cwd0="${bframe#:}" ;; *) continue ;;
          esac
        case "$d" in
          /*) dd=$(norm_path "$d") ;;
          *)  dd=$(norm_path "${cwd0:+${cwd0%/}/}$d") ;;
        esac
        # An operand that normalises to EMPTY is the repo root itself -- `norm_path`
        # collapses `.` and `./` to the empty string -- and skipping it meant the
        # most destructive spelling of all was the one never checked: `rm -rf .` and
        # `find . -delete` both returned 0 with protected files directly beneath.
        # Found by probing this branch, not reported.
        [ -n "$dd" ] || dd="."
        # The ROOT is checked as well as its descendants. `find services/auth -delete`
        # removes the directory itself, and with an EMPTY protected directory the
        # descendant walk had nothing to report, so it returned 0 while `rm -rf` on
        # the same path blocked -- the two spellings disagreed about the same write.
        # The trailing separator is fabricated for the same reason it is elsewhere: a
        # `/auth/`-shaped pattern cannot match a directory operand without one.
        if [ "$dd" != "." ]; then
          for dcand in "$dd" "${dd%/}/"; do
            matches_protected "$dcand" || continue
            marker_allows "$dcand" && continue
            echo "BLOCKED: ${DEL_VERB} of ${d} would reach security-critical ${dcand} (matched: $MATCHED_PATTERN). Requires explicit user approval." >&2
            exit 2
          done
        fi
        while IFS= read -r child; do
          [ -z "$child" ] && continue
          matches_protected "$child"; crc=$?
          if [ "$crc" -eq 2 ]; then
            echo "BLOCKED: invalid regex in protected_patterns: $MATCHED_PATTERN. Failing closed." >&2; exit 2
          fi
          [ "$crc" -eq 0 ] || continue
          marker_allows "$child" && continue
          echo "BLOCKED: ${DEL_VERB} of ${d} would reach security-critical ${child} (matched: $MATCHED_PATTERN). Requires explicit user approval." >&2
          exit 2
        done <<< "$(list_descendants "$dd")"
        done <<< "$CWDLIST"
        done <<< "$(brace_expand "$draw" | while IFS= read -r bx; do var_values "$bx"; done)"
      done <<< "$DEL_OPERANDS"
    fi

    # `cd` is applied AFTER this segment's own targets are checked: in `cd dir > f`
    # the redirection is performed in the ORIGINAL directory, and the segment used
    # to be skipped wholesale, which lost the write entirely. The operand is the
    # first non-option, non-redirect token — `$NF` picked up whatever trailed the
    # directory (`cd services/auth 2>/dev/null`) and set a bogus base, so a later
    # relative write was checked against a prefix that matched nothing.
    # Reads the LEAD-normalised segment, not the raw text: `{ cd services/auth; … }`
    # puts `cd` second and an anchored check on the raw segment never fired, so the
    # later relative write was resolved from the wrong directory. LEAD already
    # recognises group openers and wrappers; `cd` itself is not in it, so its own
    # operands survive normalisation.
    # A `cd` inside ( ) is undone when the subshell closes, so the outer directory is
    # stacked on the opener and restored on the closer. Without it,
    # `cd services/auth; (cd ../public); echo x > login.go` resolved the later write
    # from services/public, which is not where bash writes it.
    # Frames carry a `:` prefix so the ROOT frame is a non-empty line. Stored bare,
    # a root CWD pushed an empty line, the pop's own blank-line filter discarded it,
    # and the subshell's `cd` leaked into every later segment — which FALSE-BLOCKED
    # `( cd services/auth ); echo x > src/util.go`, a write nowhere near the pattern.
    case "$raw" in
      *'('*)
      CWD_STACK=$(printf '%s\n:%s:%s' "$CWD_STACK" \
        "$(printf '%s\n' "$CWD_ALT" | grep -c '^:' || true)" "$CWD") ;;
      *) ;;
    esac
    # The interpreter check runs after the walk, because it needs the heredoc bodies
    # the tokenizer drops. Record the directory active at each interpreter segment so
    # its candidates can still be resolved against it: `cd services && python3 -c
    # 'open("auth/login.go","w")'` writes services/auth/login.go.
    # `ln -s TARGET LINK` makes LINK dereference to TARGET, so a later write through
    # LINK reaches the protected file. Recorded as the walk passes it.
    if printf '%s' "$seg" | grep -qE '^[[:space:]]*ln\b[^;&|]*(-[a-zA-Z]*s|--symbolic)'; then
      ALIASES=$(printf '%s\n%s' "$ALIASES" \
        "$(printf '%s' "$seg" | grep -oE '^[[:space:]]*ln\b[^;&|]*' | tr ' ' '\n' \
           | grep -vE '^-|^ln$|^$' | awk 'NR==1{t=$0} NR==2{
                l = $0
                # A RELATIVE target is resolved from the link'"'"'s own directory, not from
                # the shell'"'"'s: `ln -s auth/login.go services/alias` dereferences to
                # services/auth/login.go. Recorded raw, it read as `auth/login.go`
                # relative to the cwd, which named nothing and let the write through.
                if (substr(t, 1, 1) != "/") {
                  d = l; sub(/\/[^\/]*$/, "", d)
                  if (d != l) t = d "/" t
                }
                printf "%s\t%s\n", l, t
              }')")
    fi

    if printf '%s' "$raw" | grep -qE '\b(python3?|node|ruby|perl)\b'; then
      INTERP_BASES=$(printf '%s\n%s' "$INTERP_BASES" "$CWD")
    fi

    if printf '%s' "$seg" | grep -qE '^[[:space:]]*cd[[:space:]]+'; then
      newd=$(printf '%s' "$seg" | sed -E 's/^[[:space:]]*cd[[:space:]]+//' \
        | awk '{for (i = 1; i <= NF; i++) if ($i !~ /^-/ && $i !~ /^[0-9]*[<>]/) { print $i; exit }}')
      newd="${newd%\"}"; newd="${newd#\"}"; newd="${newd%\'}"; newd="${newd#\'}"
      # The operand can be a variable too: `d=services/auth; cd "$d"`. Left literal it
      # composed `$d/login.go`, which matches nothing. First value only — a base has to
      # be ONE directory, unlike a target, which may legitimately be checked severally.
      newd=$(var_values "$newd" | head -1)
      if [ -n "$newd" ]; then
        case "$newd" in
          /*) nd=$(norm_path "$newd") ;;
          *)  nd=$(norm_path "${CWD:+${CWD%/}/}$newd") ;;
        esac
        if [ "$cond" = 2 ]; then
          CWD_ALT=$(printf '%s\n:%s' "$CWD_ALT" "$nd")
        else
          # A later cd applies from every base that is still possible, not only from
          # the unconditional one. `false || cd services; cd auth; echo x > login.go`
          # recorded `services` as an alternative and then composed `auth` onto CWD
          # alone, so the bases were `auth` and `services` -- never the
          # `services/auth` bash actually reaches.
          case "$newd" in
            /*) CWD_ALT="" ;;   # an absolute cd collapses every alternative
            *)  CWD_ALT=$(printf '%s\n' "$CWD_ALT" | grep '^:' \
                  | while IFS= read -r a; do
                      a="${a#:}"; printf ':%s\n' "$(norm_path "${a:+${a%/}/}$newd")"
                    done) ;;
          esac
          # Recorded for an UNCONDITIONAL cd too, not just an `&&` link: a cd can
          # simply fail. `cd services/auth; cd ../missing; echo x > login.go` leaves
          # bash in services/auth and overwrites the protected file, while the model
          # had moved on to services/missing. An `&&` chain is still exempt in
          # practice, because the flush only happens at a `;` -- and if a cd inside
          # a chain fails, nothing later in that chain runs at all.
          CWD_PENDING=$(printf '%s\n:%s' "$CWD_PENDING" "$CWD")
          CWD="$nd"
        fi
      fi
    fi
    case "$raw" in
      *')'*)
        frame=$(printf '%s\n' "$CWD_STACK" | grep '^:' | tail -1)
        if [ -n "$frame" ]; then
          frest="${frame#:}"; nalt="${frest%%:*}"; CWD="${frest#*:}"
          # Truncate to the alternative count recorded at the opener. Without it a
          # conditional cd INSIDE the subshell outlived it and false-blocked later
          # writes from a directory the shell had already left.
          # awk, not `head -n "$nalt"`: BSD head REJECTS a count of 0 ("illegal line
          # count") and an empty alternative list at the opener is the common case.
          CWD_ALT=$(printf '%s\n' "$CWD_ALT" | grep '^:' | awk -v n="$nalt" 'NR <= n')
          CWD_STACK=$(printf '%s\n' "$CWD_STACK" | grep '^:' | sed '$d')
        fi ;;
      *) ;;
    esac
  done <<< "$SCAN"

  # Interpreter bodies — the construct that opened the gap. The path sits INSIDE a
  # heredoc (`python3 - <<PY ... open(p,"w") ... PY`), so no redirect or write-verb
  # token names it. PreToolUse receives the heredoc body, so require all three: an
  # interpreter, a write MODE, and a protected path somewhere in the command.
  #
  # The mode is what separates this from a read. A bare `open(` or `.write(`
  # indicator blocked `python3 -c 'io.open(p).read()'` and `sys.stdout.write()`,
  # which is how a control like this gets switched off. Require w/a/x.
  #
  # Checked per extracted PATH TOKEN, not on the command string. marker_allows
  # compares its subject for EQUALITY against a repo-relative path, so handing it
  # the whole command made a marker structurally unable to authorize an interpreter
  # write: the same path was allowed spelled as a redirect and blocked spelled as a
  # heredoc. If the command matches a pattern but no extracted token does, the
  # extraction missed it — fall back to blocking on the command.
  #
  # Every write indicator EXCEPT shutil.copy — the one that does not consume its
  # source. Used to decide whether a command is copy-only.
  WRITE_API_STRICT='open\([^)]*,[[:space:]]*(mode[[:space:]]*=[[:space:]]*)?['"'"'"]([wax]|r[bt]*\+)|open\([^)]*,[[:space:]]*['"'"'"]>>?['"'"'"]|writeFile|appendFile|createWriteStream|write_text|write_bytes|File\.write|shutil\.move|os\.(replace|rename)|os\.(unlink|remove|rmdir)|unlinkSync|rmSync|rmdirSync|\.unlink\(\)|File\.(delete|unlink)|FileUtils\.rm'

  # Reads $COMMAND, NOT $SCAN, deliberately: a git segment must not buy an exemption
  # for an interpreter body elsewhere on the line. The cost is that a git commit
  # message quoting all three indicators now blocks — the repo's standing answer to
  # that class is `-F`/`--body-file`. Do not "tidy" this to $SCAN.
  if printf '%s' "$COMMAND" | grep -qE '\b(python3?|node|ruby|perl)\b' \
     && printf '%s' "$COMMAND" | grep -qE 'open\([^)]*,[[:space:]]*(mode[[:space:]]*=[[:space:]]*)?['"'"'"]([wax]|r[bt]*\+)|open\([^)]*,[[:space:]]*['"'"'"]>>?['"'"'"]|writeFile|appendFile|createWriteStream|write_text|write_bytes|File\.write|shutil\.(copy|move)|os\.(replace|rename)|os\.(unlink|remove|rmdir)|unlinkSync|rmSync|rmdirSync|Path\([^)]*\)\.unlink|\.unlink\(\)|File\.(delete|unlink)|FileUtils\.rm'; then
    # `shutil.copy(src, dst)` READS its source; only `move`/`replace`/`rename` and
    # the open-for-write forms consume one. When copy is the ONLY indicator present,
    # consider just its destination — sweeping every token blocked
    # `shutil.copy("<protected>", "/tmp/x")`, contradicting this file's own rule that
    # copying a protected file outward stays free.
    narrowed=0
    if printf '%s' "$COMMAND" | grep -qE 'shutil\.copy' \
       && ! printf '%s' "$COMMAND" | grep -qE "${WRITE_API_STRICT}"; then
      # The SECOND argument specifically. Stripping to the last comma isolated the
      # destination only for the two-argument shape: `copytree(src, dst, keyword=…)`
      # left the keyword and lost the destination entirely — fail-open.
      INTERP_CANDS=$(printf '%s' "$COMMAND" | grep -oE 'shutil\.copy[a-z0-9_]*\([^)]*\)' \
        | sed -E 's/^shutil\.copy[a-z0-9_]*\([^,]*,[[:space:]]*//; s/[,)].*$//; s/["'"'"'[:space:]]//g')
      narrowed=1
    else
      # `cd` operands are removed first. The sweep reads the whole command (it has to
      # — heredoc bodies live there), and a directory being ENTERED is not a path
      # being written: `cd services/auth && python3 -c 'open("/tmp/out","w")'` blocked
      # on the cd operand alone, a write pointed firmly out of the protected tree.
      INTERP_CANDS=$(printf '%s' "$COMMAND" \
        | sed -E 's/(^|[;&|[:space:]])cd[[:space:]]+[^;&|]*//g' \
        | grep -oE '[A-Za-z0-9_.@/-]+' | sort -u)
    fi
    interp_hit=0
    while IFS= read -r cand; do
      [ -z "$cand" ] && continue
      # Authorization on the CANONICAL candidate first — the fabricated `<path>/`
      # can never equal a marker entry, and checking it unconditionally re-blocks
      # what the marker just allowed. That defect was fixed once already on the
      # shell-target path; adding the variant here reintroduced it verbatim.
      # An AUTHORIZED candidate is still a hit: skipping without recording one let
      # the extraction-missed fallback re-check the whole command and block the very
      # path the marker had just allowed.
      if marker_allows "$cand"; then interp_hit=1; continue; fi
      # Both forms, as the shell-target loop does: a DIRECTORY candidate carries no
      # trailing separator, so `copytree(src, "services/auth")` missed a `/auth/`
      # pattern even after its destination was extracted correctly.
      for v in "$cand" "${cand%/}/"; do
        matches_protected "$v"; crc=$?
        if [ "$crc" -eq 2 ]; then
          echo "BLOCKED: invalid regex in protected_patterns: $MATCHED_PATTERN. Failing closed." >&2; exit 2
        fi
        [ "$crc" -eq 0 ] || continue
        interp_hit=1
        check_or_die "$v" "Bash command writes a security-critical path via an interpreter: $v"
      done
    done <<< "$INTERP_CANDS"
    # The fallback covers a MISSED extraction, not a deliberate narrowing — without
    # this guard the copy-only case fell straight back to checking the whole command
    # and blocked on the source it had just been narrowed to exclude.
    # Base composition is restricted to the WRITE API's own path argument. Composing
    # it onto every token made `cd services/auth && python3 -c 'open("/tmp/out","w")'`
    # block and report `services/auth/-c` — syntax read as a relative pathname. The
    # broad sweep above still runs, bare, which is where a non-relative protected
    # path is caught.
    while IFS= read -r wp; do
      [ -z "$wp" ] && continue
      case "$wp" in
          /*|\$*|-*) continue ;; *) ;;
        esac
      while IFS= read -r ib; do
        [ -z "$ib" ] && continue
        for v in "${ib%/}/$wp" "${ib%/}/${wp%/}/"; do
          matches_protected "$v"; crc=$?
          [ "$crc" -eq 2 ] && { echo "BLOCKED: invalid regex in protected_patterns: $MATCHED_PATTERN. Failing closed." >&2; exit 2; }
          [ "$crc" -eq 0 ] || continue
          interp_hit=1
          marker_allows "$v" && continue
          check_or_die "$v" "Bash command writes a security-critical path via an interpreter: $v"
        done
      done <<< "$INTERP_BASES"
    done <<< "$( { printf '%s' "$COMMAND" \
        | grep -oE '(open|writeFile|appendFile|createWriteStream|File\.write)\([^)]*' \
        | sed -E 's/^[^(]*\([[:space:]]*//; s/[,)].*$//; s/["'"'"'[:space:]]//g'
      # `Path(p).write_text(s)` holds its path in the RECEIVER; the first argument is
      # the CONTENT. Taking the first argument composed the base onto the string being
      # written and never onto the pathname, so `cd services && python3 -c
      # 'Path("auth/login.go").write_text("x")'` was allowed. The receiver is pulled
      # separately, and write_text/write_bytes are gone from the argument sweep above
      # — their first argument is never a path, and composing the base onto it also
      # FALSE-BLOCKED `cd services && python3 -c
      # 'Path("public/o.go").write_text("auth/login.go")'`, which writes an
      # unprotected file whose contents merely spell a relative path. (Content that
      # spells an ALREADY-matching path still blocks, via the broad token sweep
      # above; that is this check's deliberate conservatism, not this hunk's doing.)
      printf '%s' "$COMMAND" \
        | grep -oE '[A-Za-z0-9_.]*Path\([^)]*\)\.[[:space:]]*(write_text|write_bytes)' \
        | sed -E 's/^[^(]*\([[:space:]]*//; s/\).*$//; s/["'"'"'[:space:]]//g'; } )"

    [ "$interp_hit" -eq 0 ] && [ "$narrowed" -eq 0 ] \
      && check_or_die "$COMMAND" "Bash command writes a security-critical path via an interpreter"
  fi
  exit 0
fi

# ── Edit|Write mode (the complete path) ──────────────────────────────────────
[ -z "$FILE_PATH" ] && exit 0
marker_allows "$FILE_PATH" && exit 0
# Skip non-executable Claude metadata (names may contain security terms).
if [[ "$FILE_PATH" =~ /\.claude/(agents|skills|rules)/ ]] || [[ "$FILE_PATH" =~ ^\.claude/(agents|skills|rules)/ ]]; then exit 0; fi
check_or_die "$FILE_PATH" "Editing security-critical file: $FILE_PATH"
exit 0
