#!/usr/bin/env bash
# Hermetic tests for provision-production.sh control-plane-secrets: the GID 10002
# reader group for /opt/concord/etc/apple_signin.p8. No root, Docker or real
# account database: getent/groupadd/usermod are PATH stubs over fake databases,
# and the reader GID is the caller's own primary group so fchown works unprivileged.
#
# Usage: test-control-plane-secrets.sh [path/to/provision-production.sh]
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
PROVISION="${1:-$ROOT/[internal]provision-production.sh}"
HELPER="$ROOT/infrastructure/deploy/selfhost-security-event-logging.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
TMP="$(cd "$TMP" && pwd -P)" # the walk refuses symlinks, e.g. macOS /var
FAILS=0
pass() { printf 'PASS: %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; FAILS=$((FAILS + 1)); }
check() { local name="$1"; shift; if "$@"; then pass "$name"; else fail "$name"; fi; }

# ── PATH stubs over fake group/passwd databases ──────────────────────────────
BIN="$TMP/bin"; mkdir -p "$BIN"
export FAKE_GROUP="$TMP/group" FAKE_PASSWD="$TMP/passwd"
cat >"$BIN/getent" <<'SH'
#!/usr/bin/env bash
[[ -z "${GETENT_FAIL:-}" ]] || exit 2
case "$1" in group) db="$FAKE_GROUP" ;; passwd) db="$FAKE_PASSWD" ;; *) exit 2 ;; esac
if [[ $# -eq 1 ]]; then cat "$db"; else awk -F: -v n="$2" '$1 == n' "$db" | grep . ; fi
SH
cat >"$BIN/groupadd" <<'SH'
#!/usr/bin/env bash
# groupadd --system --gid GID NAME
printf '%s:x:%s:\n' "$4" "$3" >>"$FAKE_GROUP"
SH
cat >"$BIN/usermod" <<'SH'
#!/usr/bin/env bash
# usermod -aG GROUP USER
[[ -z "${USERMOD_FAIL:-}" ]] || exit 1
awk -F: -v OFS=: -v g="$2" -v u="$3" '$1 == g { $4 = ($4 == "" ? u : $4 "," u) } 1' \
  "$FAKE_GROUP" >"$FAKE_GROUP.new" && mv "$FAKE_GROUP.new" "$FAKE_GROUP"
SH
chmod +x "$BIN"/*
export PATH="$BIN:$PATH"

# ── The function under test, extracted from the real provisioner ─────────────
FN="$TMP/fn.sh"
awk '/^# ─── Control-plane secret-file reader group/,/^}$/' "$PROVISION" >"$FN"
grep -q '^install_control_plane_secrets_group() {' "$FN" \
  || { echo "FAIL: could not extract install_control_plane_secrets_group" >&2; exit 1; }
err() { printf 'ERR: %s\n' "$*" >&2; }
log() { :; }
# shellcheck source=/dev/null
source "$HELPER"
# shellcheck source=/dev/null
source "$FN"
GID="$(id -g)" UID_SELF="$(id -u)"
# Root's GID 0 would collide with the fake root group; root can chown to any GID.
[[ $EUID -ne 0 ]] || GID=54321
export CP_KEY_READER_GID="$GID" # read by the sourced function
GROUP="$CP_KEY_READER_GROUP"
KEYDIR="$TMP/opt/concord/etc"; KEY="$KEYDIR/apple_signin.p8"

reset() { # fresh databases and a 0644 key owned by "concord" (this user)
  rm -rf "$TMP/opt"; mkdir -p "$KEYDIR"
  printf 'root:x:0:\nstaff-other:x:%s:\n' "$((GID + 1))" >"$FAKE_GROUP"
  printf 'root:x:0:0::/root:/bin/sh\nconcord:x:%s:%s::/home/concord:/bin/sh\n' "$UID_SELF" "$((GID + 1))" >"$FAKE_PASSWD"
  printf 'KEY' >"$KEY"; chmod 0644 "$KEY"
  unset USERMOD_FAIL GETENT_FAIL
}
run() { install_control_plane_secrets_group "$KEY" 2>/dev/null; }
mode() { python3 -c 'import os,sys;s=os.lstat(sys.argv[1]);print(f"{s.st_gid}:{s.st_mode & 0o777:o}")' "$1"; }
members() { awk -F: -v g="$GROUP" '$1 == g {print $4}' "$FAKE_GROUP"; }
members_are() { [[ "$(members)" == "$1" ]]; }
mode_is() { [[ "$(mode "$1")" == "$2" ]]; }
untouched() { [[ "$(mode "$1")" == *":644" ]]; }
absent() { [[ ! -e "$1" && ! -L "$1" ]]; }
refused() { ! run; }
run_then_members_concord() { run && members_are concord; }
run_without_key() { run && absent "$KEY"; }

reset; printf 'stale' >"$KEY.new"; chmod 0644 "$KEY.new"
check "fresh host: creates the group and succeeds" run
check "fresh host: concord is the only member" members_are concord
check "fresh host: key tightened to GID:640" mode_is "$KEY" "$GID:640"
check "fresh host: stale apple_signin.p8.new removed" absent "$KEY.new"
check "rerun is idempotent" run_then_members_concord

reset; rm "$KEY"
check "no key yet: succeeds without creating one" run_without_key

reset; printf '%s:x:%s:concord,nginx\n' "$GROUP" "$GID" >>"$FAKE_GROUP"
check "extra member: refused" refused
check "extra member: key untouched" untouched "$KEY"

reset; printf 'intruder:x:4242:%s::/:/bin/sh\n' "$GID" >>"$FAKE_PASSWD"
check "primary-group account: refused" refused
check "primary-group account: concord not added first" members_are ''

reset; export USERMOD_FAIL=1
check "usermod failure: refused" refused
check "usermod failure: key untouched" untouched "$KEY"

reset; printf 'other:x:%s:\n' "$GID" >>"$FAKE_GROUP"
check "GID owned by another group: refused" refused

reset; printf '%s:x:%s:concord\nalias:x:%s:\n' "$GROUP" "$GID" "$GID" >>"$FAKE_GROUP"
check "duplicate GID alias: refused" refused

reset; export GETENT_FAIL=1
check "group database lookup failure: refused" refused

reset; rm "$KEY"; printf 'T' >"$TMP/target"; chmod 0644 "$TMP/target"; ln -s "$TMP/target" "$KEY"
check "symlinked key: refused" refused
check "symlinked key: target untouched" untouched "$TMP/target"

reset; ln "$KEY" "$TMP/hardlink"
check "hard-linked key: refused" refused
rm -f "$TMP/hardlink"

reset; printf 'root:x:0:0::/root:/bin/sh\nconcord:x:%s:%s::/:/bin/sh\n' "$((UID_SELF + 1))" "$((GID + 1))" >"$FAKE_PASSWD"
check "key not owned by concord: refused" refused

reset; mv "$KEYDIR" "$TMP/real-etc"; ln -s "$TMP/real-etc" "$KEYDIR"
check "symlinked key directory: refused" refused
check "symlinked key directory: key untouched" untouched "$TMP/real-etc/apple_signin.p8"

# ── POSIX ACLs (Linux with setfacl only) ─────────────────────────────────────
acl_of() { python3 -c 'import os,sys
try: os.getxattr(sys.argv[1], sys.argv[2]); print("yes")
except OSError: print("no")' "$1" "$2"; }
no_access_acl() { [[ "$(acl_of "$1" system.posix_acl_access)" == no ]]; }
reset
if command -v setfacl >/dev/null 2>&1 && setfacl -m u:65534:r "$KEY" 2>/dev/null \
  && [[ "$(acl_of "$KEY" system.posix_acl_access)" == yes ]]; then
  check "named-user ACL on key: succeeds" run
  check "named-user ACL on key: ACL removed" no_access_acl "$KEY"
  check "named-user ACL on key: mode GID:640" mode_is "$KEY" "$GID:640"
  reset; setfacl -d -m u:65534:r "$KEYDIR"
  check "default ACL on key directory: refused" refused
  check "default ACL on key directory: key untouched" untouched "$KEY"
else
  echo "SKIP: POSIX ACL cases (setfacl unavailable or unsupported here)"
fi

# ── The root route ───────────────────────────────────────────────────────────
ROUTE="$(awk '/^if \[\[ "\$\{1:-\}" == "control-plane-secrets" \]\]; then/,/^fi$/' "$PROVISION")"
check "route requires root" grep -q "EUID -ne 0" <<<"$ROUTE"
check "route requires root-owned staging" grep -q "sudoers_boundary_source_is_trusted" <<<"$ROUTE"
bootstrap_order() { # the group install must follow the recursive chown that resets it
  awk -v chown='chown -R concord:concord "$DEPLOY_DIR"' \
    '$0 == chown {c=1} c && $0 == "install_control_plane_secrets_group || exit 1" {f=1} END {exit !f}' "$PROVISION"
}
check "full bootstrap runs it after chown -R" bootstrap_order
if [[ $EUID -ne 0 ]]; then
  out="$(bash "$PROVISION" control-plane-secrets 2>&1 || true)"
  check "route refuses a non-root caller" grep -q "must be run as root" <<<"$out"
fi

if [[ $FAILS -gt 0 ]]; then
  echo "$FAILS control-plane-secrets test(s) failed." >&2
  exit 1
fi
echo "All control-plane-secrets tests passed."
