#!/usr/bin/env bash
# Tests for _npm_ci_if_lock_changed in concord-dev.sh: the lock-exact install
# that `up` and `rebuild` use. npm is stubbed; it logs each call and creates
# node_modules/ the way `npm ci` does (wipe, then install).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
# shellcheck disable=SC1091
source "$PROJECT_ROOT/scripts/concord-dev.sh"
QUIET=1

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin" "$TMP/pkg"
export NPM_LOG="$TMP/npm.log" NPM_FAIL="$TMP/npm.fail"
cat >"$TMP/bin/npm" <<'STUB'
#!/usr/bin/env bash
echo "$*" >>"$NPM_LOG"
[[ -e "$NPM_FAIL" ]] && exit 1
if [[ "$1" == ci ]]; then rm -rf node_modules; mkdir node_modules; echo "{}" >node_modules/.package-lock.json; fi
exit 0
STUB
chmod +x "$TMP/bin/npm"
PATH="$TMP/bin:$PATH"

fail() { echo "FAIL: $1"; exit 1; }
calls() { [[ -f "$NPM_LOG" ]] && wc -l <"$NPM_LOG" | tr -d ' ' || echo 0; }

cd "$TMP/pkg"
echo '{"name":"p"}' >package.json
echo '{"lockfileVersion":3}' >package-lock.json

_npm_ci_if_lock_changed t --ignore-scripts -- npm rebuild || fail "cold run returned non-zero"
[[ "$(cat "$NPM_LOG")" == $'ci --ignore-scripts\nrebuild' ]] || fail "cold run did not run npm ci + post command: $(cat "$NPM_LOG")"
[[ -s node_modules/.concord-lock.sha256 ]] || fail "cold run wrote no stamp"
echo "PASS: cold tree runs npm ci and the post command, then stamps"

_npm_ci_if_lock_changed t --ignore-scripts -- npm rebuild || fail "warm run returned non-zero"
[[ "$(calls)" -eq 2 ]] || fail "warm run called npm again"
echo "PASS: warm tree with an unchanged stamp skips npm"

echo '{"name":"p","dependencies":{"x":"1"}}' >package.json
_npm_ci_if_lock_changed t || fail "manifest change run returned non-zero"
[[ "$(calls)" -eq 3 ]] || fail "package.json change without a lockfile change did not run npm ci"
echo "PASS: a package.json-only change re-runs npm ci"

echo '{"lockfileVersion":3,"x":1}' >package-lock.json
touch "$NPM_FAIL"
if _npm_ci_if_lock_changed t; then fail "failed npm ci returned zero"; fi
rm -f "$NPM_FAIL"
[[ ! -e node_modules/.concord-lock.sha256 ]] || fail "failed npm ci left a stamp"
_npm_ci_if_lock_changed t || fail "retry returned non-zero"
[[ "$(calls)" -eq 5 ]] || fail "retry after failure did not run npm ci"
echo "PASS: a failed npm ci leaves no stamp, so the next run installs again"

CONCORD_FORCE_NPM_CI=1 _npm_ci_if_lock_changed t || fail "forced run returned non-zero"
[[ "$(calls)" -eq 6 ]] || fail "CONCORD_FORCE_NPM_CI=1 did not force npm ci"
echo "PASS: CONCORD_FORCE_NPM_CI=1 (rebuild) ignores a matching stamp"

# The real verb_rebuild, with verb_down/verb_up stubbed so only its own
# forcing reaches the helper (dynamic scope through verb_up).
verb_down() { :; }
verb_up() { _npm_ci_if_lock_changed t; }
verb_rebuild || fail "verb_rebuild returned non-zero"
[[ "$(calls)" -eq 7 ]] || fail "verb_rebuild did not force npm ci through verb_up"
_npm_ci_if_lock_changed t || fail "post-rebuild up returned non-zero"
[[ "$(calls)" -eq 7 ]] || fail "plain up after rebuild did not skip"
echo "PASS: rebuild forces npm ci; the next plain up skips"

echo '{"lockfileVersion":3,"packages":{"node_modules/extra":{}}}' >node_modules/.package-lock.json
_npm_ci_if_lock_changed t || fail "hidden-lockfile change run returned non-zero"
[[ "$(calls)" -eq 8 ]] || fail "an out-of-band change to node_modules/.package-lock.json did not re-run npm ci"
echo "PASS: an out-of-band install (hidden lockfile changed) re-runs npm ci"
