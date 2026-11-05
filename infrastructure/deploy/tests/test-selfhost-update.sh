#!/usr/bin/env bash
# Unit tests for infrastructure/deploy/selfhost-update.sh (#185).
# Real git + SSH tag signatures; backup and the wrapper are stubs.
set -uo pipefail
# shellcheck disable=SC2015  # pass/fail one-liners: pass never fails

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY="$(cd "$HERE/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf -- "$WORK"' EXIT
command -v ssh-keygen >/dev/null || { echo 'FAIL: prerequisite missing: ssh-keygen'; exit 1; }

fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; fails=$((fails + 1)); }

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
g() { git -c user.name=t -c user.email=t@example.invalid -c commit.gpgsign=false -c tag.gpgsign=false "$@"; }
ssh-keygen -q -t ed25519 -N '' -C release -f "$WORK/release" >/dev/null
ssh-keygen -q -t ed25519 -N '' -C other -f "$WORK/other" >/dev/null
printf 'release@concord %s\n' "$(cat "$WORK/release.pub")" >"$WORK/signers"
printf '# no keys yet\n' >"$WORK/empty-signers"

# Upstream with v1.0.0 (current), v1.1.0 signed by release, v1.2.0 unsigned,
# v1.3.0 signed by another key.
g init -q -b main "$WORK/up"
echo one >"$WORK/up/f"; g -C "$WORK/up" add f; g -C "$WORK/up" commit -qm one; g -C "$WORK/up" tag v1.0.0
sign() { g -C "$WORK/up" -c gpg.format=ssh -c "user.signingkey=$WORK/$2" tag -s -m "$1" "$1"; }
echo two >"$WORK/up/f"; g -C "$WORK/up" commit -qam two; sign v1.1.0 release
echo three >"$WORK/up/f"; g -C "$WORK/up" commit -qam three; g -C "$WORK/up" tag -a -m v1.2.0 v1.2.0
echo four >"$WORK/up/f"; g -C "$WORK/up" commit -qam four; sign v1.3.0 other

fixture() {
  ROOT="$WORK/root-$RANDOM$RANDOM"
  g clone -q --no-tags "$WORK/up" "$ROOT" && g -C "$ROOT" checkout -q --detach "$(g -C "$WORK/up" rev-parse v1.0.0)"
  : >"$WORK/calls"
}

# run_update SIGNERS CLI_RC ARGS... — CLI_RC is the stub wrapper's health rc.
# shellcheck disable=SC2034  # PROJECT_ROOT, SCRIPT_DIR, BACKUP_LAST_NAME are read by the helper
run_update() {
  local signers="$1" cli_rc="$2"; shift 2
  printf '#!/usr/bin/env bash\nprintf "cli %%s\\n" "$*" >>"%s"\n[[ $1 == up ]] || exit %s\n' "$WORK/calls" "$cli_rc" >"$WORK/cli"
  chmod +x "$WORK/cli"
  (
    PROJECT_ROOT="$ROOT"; SCRIPT_DIR="$DEPLOY"
    export SELFHOST_RELEASE_SIGNERS_OVERRIDE="$signers" SELFHOST_UPDATE_CLI_OVERRIDE="$WORK/cli"
    # shellcheck source=../selfhost-update.sh
    source "$DEPLOY/selfhost-update.sh"
    # shellcheck disable=SC2329  # stub read by the sourced helper
    verb_backup() { printf 'backup\n' >>"$WORK/calls"; BACKUP_LAST_NAME=b.tar.gz; }
    verb_update "$@"
  )
}
head_is() { [[ "$(g -C "$ROOT" rev-parse HEAD)" == "$(g -C "$WORK/up" rev-parse "$1^{commit}")" ]]; }

refuse() { # LABEL SIGNERS ARGS...
  local label="$1" signers="$2"; shift 2
  run_update "$signers" 0 "$@" >/dev/null 2>&1; rc=$?
  if [[ $rc -ne 0 ]] && head_is v1.0.0 && ! grep -q backup "$WORK/calls"; then
    pass "update refuses $label without backup or switch"
  else
    fail "update $label rc=$rc calls=$(tr '\n' ' ' <"$WORK/calls")"
  fi
}

fixture
refuse "a malformed tag" "$WORK/signers" --to 'v1.1.0;rm' --confirm
refuse "without --confirm" "$WORK/signers" --to v1.1.0
refuse "while no signing key is configured" "$WORK/empty-signers" --to v1.1.0 --confirm
refuse "an unsigned tag" "$WORK/signers" --to v1.2.0 --confirm
refuse "a tag signed by a non-release key" "$WORK/signers" --to v1.3.0 --confirm
refuse "a tag that does not exist" "$WORK/signers" --to v9.0.0 --confirm
echo dirty >"$ROOT/f"
refuse "a modified checkout" "$WORK/signers" --to v1.1.0 --confirm
g -C "$ROOT" checkout -q -- f

out="$(run_update "$WORK/signers" 0 --to v1.1.0 --confirm --json 2>/dev/null)"; rc=$?
if [[ $rc -eq 0 && "$out" == '{"schema":1,"ok":true}' ]] && head_is v1.1.0 \
    && [[ "$(tr '\n' '|' <"$WORK/calls")" == 'backup|cli up|cli health|' ]]; then
  pass "a release-signed tag is backed up, switched to, then brought up and checked"
else
  fail "signed update rc=$rc out=$out calls=$(tr '\n' ' ' <"$WORK/calls")"
fi

fixture
prev="$(g -C "$ROOT" rev-parse HEAD)"
err="$(run_update "$WORK/signers" 1 --to v1.1.0 --confirm 2>&1 >/dev/null)"; rc=$?
[[ $rc -eq 1 && "$err" == *"checkout --detach $prev"* && "$err" == *'restore backups/b.tar.gz --confirm'* ]] \
  && pass "a failed health check prints the rollback commit and backup" \
  || fail "unhealthy update rc=$rc err=$err"

echo
[[ $fails -eq 0 ]] && { echo "ALL PASS"; exit 0; } || { echo "$fails FAILED"; exit 1; }
