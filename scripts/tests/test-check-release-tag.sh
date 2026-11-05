#!/usr/bin/env bash
# Hermetic tests for check-release-tag.sh against throwaway git repositories.
set -euo pipefail
SCRIPT="$(cd "$(dirname "$0")/.." && pwd)/release/check-release-tag.sh"
WORK=$(mktemp -d)
trap 'rm -rf -- "$WORK"' EXIT
fail=0

git init -q --bare "$WORK/origin.git"
git -C "$WORK/origin.git" symbolic-ref HEAD refs/heads/main

# case NAME VERSION_CONTENT TAG ON_MAIN(yes|no) EXPECT(pass|fail) [EXPECTED_OUTPUT]
case_() {
  local name=$1 content=$2 tag=$3 on_main=$4 expect=$5 want=${6:-}
  local repo="$WORK/$name"
  git clone -q "$WORK/origin.git" "$repo" 2>/dev/null
  git -C "$repo" -c user.name=t -c user.email=t@t commit -q --allow-empty -m base
  git -C "$repo" push -q origin HEAD:main --force 2>/dev/null
  printf '%b' "$content" > "$repo/VERSION"
  git -C "$repo" add VERSION
  git -C "$repo" -c user.name=t -c user.email=t@t commit -q -m version
  [[ "$on_main" == yes ]] && git -C "$repo" push -q origin HEAD:main --force 2>/dev/null
  local out="$WORK/$name.out" rc=0
  : > "$out"
  (cd "$repo" && GITHUB_REF_NAME="$tag" GITHUB_SHA="$(git rev-parse HEAD)" GITHUB_OUTPUT="$out" \
    bash "$SCRIPT") >/dev/null 2>&1 || rc=$?
  if [[ "$expect" == pass && ( $rc -ne 0 || "$(cat "$out")" != "$want" ) ]] \
     || [[ "$expect" == fail && ( $rc -eq 0 || -s "$out" ) ]]; then
    echo "FAIL: $name (rc=$rc, output=$(cat "$out"))"; fail=1
  else
    echo "ok: $name"
  fi
}

case_ match         '0.3.0\n'   v0.3.0 yes pass version=0.3.0
case_ whitespace    ' 0.3.0 \n\n' v0.3.0 yes pass version=0.3.0
case_ mismatch      '0.3.0\n'   v0.3.1 yes fail
case_ prefix-in-file 'v0.3.0\n' v0.3.0 yes fail
case_ prerelease    '0.3.0-rc1\n' v0.3.0-rc1 yes fail
case_ leading-zero  '0.03.0\n'  v0.03.0 yes fail
case_ two-lines     '0.3.0\n0.3.1\n' v0.3.0 yes fail
case_ empty         ''          v      yes fail
case_ off-main      '0.3.0\n'   v0.3.0 no  fail
exit $fail
