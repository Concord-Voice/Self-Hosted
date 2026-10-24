#!/usr/bin/env bash
#
# Self-test for check-bash-heredoc-hang.sh and its wiring (#3426).
#
# The guard only protects anyone if three things hold at once: its version
# boundaries are right, it runs FIRST at every stage, and a refusal STOPS the
# run (fail_fast). A wrong boundary or a missing fail_fast leaves the guard
# printing a correct refusal and then hanging in the next hook anyway. So the
# wiring is asserted here alongside the logic, from the PARSED config (PyYAML):
# a text grep of YAML cannot see a duplicate key or a reordered hook.
#
# GUARD / CONFIG may be overridden so mutation runs can target scratch copies
# instead of the tree.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GUARD="${GUARD:-$ROOT/scripts/check-bash-heredoc-hang.sh}"
CONFIG="${CONFIG:-$ROOT/.pre-commit-config.yaml}"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
pass=0; fail=0

ok()  { pass=$((pass + 1)); echo "  PASS: $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL: $1"; }

# decide <want-exit> <os> <major> <minor> <patch>
# A refusal must say why and how to fix it, not just exit 1: a bare exit 1
# is what a crash also produces.
decide() {
  local want="$1" label="$2 bash $3.$4.$5" out rc
  set +e; out=$(bash "$GUARD" --check "$2" "$3" "$4" "$5" 2>&1); rc=$?; set -e
  if [[ "$rc" != "$want" ]]; then
    bad "$label: exit $rc, want $want"
  elif [[ "$want" == 1 && ( "$out" != *"can hang forever"* || "$out" != *"brew upgrade bash"* || "$out" != *"#3426"* ) ]]; then
    bad "$label: refused without naming the cause, the fix and #3426"
  elif [[ "$want" == 0 && -n "$out" ]]; then
    bad "$label: passed but printed output"
  else
    ok "$label -> exit $want"
  fi
}

echo "Version boundaries:"
decide 0 darwin 3 2 57    # /bin/bash: always a temp file
decide 0 darwin 5 0 18    # before the pipe path existed
decide 1 darwin 5 1 0     # first affected release
decide 1 darwin 5 2 37    # no 5.2 patch fixes it
decide 1 darwin 5 3 15    # last affected patch
decide 0 darwin 5 3 16    # upstream fix
decide 0 darwin 5 4 0
decide 0 darwin 6 0 0
decide 0 linux-gnu 5 2 21 # Linux checks the real pipe size at runtime

# usage <want-stderr-substring> <arg>... — exit 2, and say which problem it is.
usage() {
  local want="$1" out rc; shift
  set +e; out=$(bash "$GUARD" "$@" 2>&1); rc=$?; set -e
  if [[ "$rc" == 2 && "$out" == *"$want"* ]]; then ok "'$*' -> exit 2 ($want)"
  else bad "'$*' -> exit $rc, output '$out'; want exit 2 and '$want'"; fi
}
echo "Usage errors:"
usage "usage:" --check darwin 5 1
usage "usage:" --check darwin 5 1 1 extra
usage "usage:" unexpected
usage "not a version number: x" --check darwin x 1 1
usage "not a version number: x" --check darwin 5 x 1
usage "not a version number: x" --check darwin 5 1 x
usage "not a version number: 09" --check darwin 5 3 09   # octal in (( ))

echo "Running mode, through the shebang as pre-commit runs it:"
# Limit: this can only catch a running mode that misreads the version on a
# machine whose bash is AFFECTED. On a safe bash, a running mode that always
# answered "safe" would agree with --check too, and BASH_VERSINFO is readonly,
# so no test here can fake an affected version. The boundaries above are
# pinned through --check, which shares the same affected() function.
real_bash=$(command -v bash)
mkdir "$TMP/shim"
printf '#!%s\ntouch "%s/used"\nexec "%s" "$@"\n' "$real_bash" "$TMP" "$real_bash" > "$TMP/shim/bash"
chmod +x "$TMP/shim/bash"
# shellcheck disable=SC2016  # expanded by the child bash, on purpose
read -r maj min pat < <(bash -c 'echo "${BASH_VERSINFO[0]} ${BASH_VERSINFO[1]} ${BASH_VERSINFO[2]}"')
case "$(uname -s)" in Darwin) os=darwin ;; *) os=other ;; esac
set +e
bash "$GUARD" --check "$os" "$maj" "$min" "$pat" >/dev/null 2>&1; want=$?
PATH="$TMP/shim:$PATH" "$GUARD" >/dev/null 2>&1; got=$?
set -e
if [[ -e "$TMP/used" ]]; then ok "the shebang resolves bash from PATH"
else bad "the guard did not run the PATH bash (shebang, or not executable)"; fi
if [[ "$got" == "$want" ]]; then ok "running mode agrees with --check for $os bash $maj.$min.$pat (exit $got)"
else bad "running mode exit $got, --check says $want for $os bash $maj.$min.$pat"; fi
# code_has <regex> — true when a line that is not a whole-line comment matches.
# One awk, not `grep -v | grep -q`: under pipefail an early-exiting grep -q can
# SIGPIPE the producer and turn a match into a failed pipeline.
code_has() { awk -v re="$1" '!/^[[:space:]]*#/ && $0 ~ re {f=1} END {exit !f}' "$GUARD"; }

if code_has 'OSTYPE'; then bad "the guard reads OSTYPE, which bash takes from the environment"
else ok "running mode does not read OSTYPE"; fi

echo "The guard itself carries no heredoc or here-string:"
if code_has '<<'; then bad "guard contains '<<'"
else ok "no '<<' outside whole-line comments"; fi

echo "Wiring in $(basename "$CONFIG"):"
# python3 -c, not a heredoc. Prints one OK/BAD line per check.
if ! results=$(python3 -c '
import sys, yaml
c = yaml.safe_load(open(sys.argv[1]))
g = c["repos"][0]["hooks"][0]
v = tuple(int(x) for x in str(c.get("minimum_pre_commit_version", "0")).split("."))
checks = [
    ("the guard is the first hook", g.get("id") == "bash-heredoc-hang-guard"),
    ("it runs scripts/check-bash-heredoc-hang.sh", g.get("entry") == "scripts/check-bash-heredoc-hang.sh"),
    ("fail_fast is true", g.get("fail_fast") is True),
    ("always_run is true", g.get("always_run") is True),
    ("pass_filenames is false", g.get("pass_filenames") is False),
    ("it declares no stages, so it runs at every stage", "stages" not in g),
    ("the config sets no default_stages", "default_stages" not in c),
    ("minimum_pre_commit_version >= 2.16.0 (hook-level fail_fast)", v >= (2, 16, 0)),
]
for name, good in checks:
    print(("OK " if good else "BAD ") + name)
' "$CONFIG" 2>&1); then
  bad "could not parse $(basename "$CONFIG"): $results"
else
  while IFS= read -r line; do
    case "$line" in
      "OK "*)  ok "${line#OK }" ;;
      "BAD "*) bad "${line#BAD }" ;;
      *)       bad "unexpected parser output: $line" ;;
    esac
  done < <(printf '%s\n' "$results")
fi

echo ""
echo "PASS: $pass  FAIL: $fail"
[[ "$fail" == 0 ]]
