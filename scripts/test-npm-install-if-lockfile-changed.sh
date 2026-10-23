#!/usr/bin/env bash
# Regression tests for the lockfile auto-install hook (#2459).

set -euo pipefail

unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_CONFIG_COUNT GIT_CONFIG_PARAMETERS
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_NOSYSTEM=1

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HOOK="${SCRIPT_DIR}/npm-install-if-lockfile-changed.sh"
REAL_GIT="$(command -v git)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

mkdir -p "${TMP_DIR}/bin" "${TMP_DIR}/repo/client"
git -C "${TMP_DIR}/repo" init -q
git -C "${TMP_DIR}/repo" config user.email test@example.invalid
git -C "${TMP_DIR}/repo" config user.name test

printf '{"lockfileVersion": 1}\n' >"${TMP_DIR}/repo/client/package-lock.json"
git -C "${TMP_DIR}/repo" add client/package-lock.json
git -C "${TMP_DIR}/repo" commit -qm initial
from_ref="$(git -C "${TMP_DIR}/repo" rev-parse HEAD)"

printf '{"lockfileVersion": 2}\n' >"${TMP_DIR}/repo/client/package-lock.json"
git -C "${TMP_DIR}/repo" add client/package-lock.json
git -C "${TMP_DIR}/repo" commit -qm lockfile-update
to_ref="$(git -C "${TMP_DIR}/repo" rev-parse HEAD)"

cat >"${TMP_DIR}/bin/npm" <<'FAKE_NPM'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${NPM_LOG}"
FAKE_NPM
chmod +x "${TMP_DIR}/bin/npm"

cat >"${TMP_DIR}/bin/git" <<'FAKE_GIT'
#!/usr/bin/env bash
if [[ "${1:-}" == "diff" ]]; then
  printf '%s\n' "$*" >>"${GIT_LOG}"
fi
exec "${REAL_GIT}" "$@"
FAKE_GIT
chmod +x "${TMP_DIR}/bin/git"

zero_oid="0000000000000000000000000000000000000000"
if ! zero_output=$(cd "${TMP_DIR}/repo" && \
  PATH="${TMP_DIR}/bin:${PATH}" REAL_GIT="${REAL_GIT}" \
  GIT_LOG="${TMP_DIR}/git.log" NPM_LOG="${TMP_DIR}/npm.log" \
  PRE_COMMIT_FROM_REF="${zero_oid}" PRE_COMMIT_TO_REF="${to_ref}" \
  PRE_COMMIT_CHECKOUT_TYPE=1 bash "${HOOK}" client 2>&1); then
  fail "null OID worktree checkout should skip cleanly: ${zero_output}"
fi
[[ ! -e "${TMP_DIR}/npm.log" ]] || fail "null OID checkout ran npm"
[[ ! -e "${TMP_DIR}/git.log" ]] || fail "null OID checkout reached git diff"

if ! change_output=$(cd "${TMP_DIR}/repo" && \
  PATH="${TMP_DIR}/bin:${PATH}" REAL_GIT="${REAL_GIT}" \
  GIT_LOG="${TMP_DIR}/git.log" NPM_LOG="${TMP_DIR}/npm.log" \
  PRE_COMMIT_FROM_REF="${from_ref}" PRE_COMMIT_TO_REF="${to_ref}" \
  PRE_COMMIT_CHECKOUT_TYPE=1 bash "${HOOK}" client 2>&1); then
  fail "changed lockfile should run npm: ${change_output}"
fi
[[ "$(<"${TMP_DIR}/npm.log")" == "install --no-audit --no-fund" ]] ||
  fail "changed lockfile did not run the expected npm install"

echo "PASS: null OID skips cleanly and changed lockfiles run npm install"

# git exports GIT_DIR to its hooks, and in a linked worktree it names
# .git/worktrees/<name>. npm fetches a git dependency with its own `git clone`
# and `git init` in a cache directory. If the hook passes GIT_DIR through, that
# `git init` re-initialises the repository instead, and writes core.bare=true
# into the shared config, so the main checkout reads as bare. The fake npm does
# what npm's git fetch does, and the main repository must stay a work tree.
cat >"${TMP_DIR}/bin/npm" <<'FAKE_NPM'
#!/usr/bin/env bash
fetch_dir="$(mktemp -d "${NPM_FETCH_ROOT}/git-clone.XXXXXX")"
(cd "${fetch_dir}" && "${REAL_GIT}" init -q)
printf '%s\n' "${fetch_dir}" >>"${NPM_FETCH_LOG}"
FAKE_NPM
chmod +x "${TMP_DIR}/bin/npm"

git -C "${TMP_DIR}/repo" branch -q linked-base "${from_ref}"
git -C "${TMP_DIR}/repo" worktree add -q "${TMP_DIR}/linked" linked-base
linked_git_dir="$(git -C "${TMP_DIR}/linked" rev-parse --absolute-git-dir)"
git -C "${TMP_DIR}/linked" checkout -q --detach "${to_ref}"
mkdir -p "${TMP_DIR}/npm-fetch"
: >"${TMP_DIR}/npm-fetch.log"

if ! linked_output=$(cd "${TMP_DIR}/linked" && \
  PATH="${TMP_DIR}/bin:${PATH}" REAL_GIT="${REAL_GIT}" \
  GIT_DIR="${linked_git_dir}" \
  GIT_LOG="${TMP_DIR}/git.log" NPM_FETCH_ROOT="${TMP_DIR}/npm-fetch" \
  NPM_FETCH_LOG="${TMP_DIR}/npm-fetch.log" \
  PRE_COMMIT_FROM_REF="${from_ref}" PRE_COMMIT_TO_REF="${to_ref}" \
  PRE_COMMIT_CHECKOUT_TYPE=1 bash "${HOOK}" client 2>&1); then
  fail "linked-worktree checkout should run npm cleanly: ${linked_output}"
fi
[[ -s "${TMP_DIR}/npm-fetch.log" ]] || fail "linked-worktree checkout did not run npm"
[[ "$(git -C "${TMP_DIR}/repo" config --bool core.bare)" == "false" ]] ||
  fail "npm's git init rewrote core.bare in the main repository"
[[ "$(git -C "${TMP_DIR}/repo" rev-parse --is-inside-work-tree)" == "true" ]] ||
  fail "the main checkout no longer reads as a work tree"
fetch_dir="$(head -n 1 "${TMP_DIR}/npm-fetch.log")"
[[ -d "${fetch_dir}/.git" ]] ||
  fail "npm's git init did not create a repository in its own directory"

echo "PASS: npm run from a linked-worktree hook cannot re-initialise the repository"
