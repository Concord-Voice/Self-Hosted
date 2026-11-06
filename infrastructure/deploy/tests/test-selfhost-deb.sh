#!/usr/bin/env bash
# Tests for scripts/release/build-selfhost-deb.sh (#187): package layout and,
# when Docker is available, install, reinstall and removal on Debian 12 and
# Ubuntu 24.04 (no systemd in the container, so the unit is not started).
# shellcheck disable=SC2015  # pass/fail one-liners: pass never fails
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../../.." && pwd)"
BUILD="$REPO/scripts/release/build-selfhost-deb.sh"
WORK="$(mktemp -d)"
trap 'rm -rf -- "$WORK"' EXIT
command -v dpkg-deb >/dev/null || { echo 'FAIL: prerequisite missing: dpkg-deb'; exit 1; }

fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; fails=$((fails + 1)); }

# A tiny release tree, bundled the way release.yml does it.
mkdir -p "$WORK/tree/infrastructure/deploy" "$WORK/wizard/assets"
printf '#!/bin/sh\necho wrapper\n' >"$WORK/tree/infrastructure/deploy/concord-selfhost"
chmod 0755 "$WORK/tree/infrastructure/deploy/concord-selfhost"
printf 'services: {}\n' >"$WORK/tree/docker-compose.yml"
python3 "$REPO/scripts/release/selfhost-bundle.py" build --public-tree "$WORK/tree" \
  --source-sha "$(git -C "$REPO" rev-parse HEAD)" --version 1.2.3 --out "$WORK/assets" >/dev/null ||
  { echo 'FAIL: bundle build'; exit 1; }
printf '#!/bin/sh\necho agent\n' >"$WORK/agent"
printf '<!doctype html>\n' >"$WORK/wizard/index.html"
printf 'x' >"$WORK/wizard/assets/app.js"
chmod 0600 "$WORK/wizard/assets/app.js"

build() { "$BUILD" --version 1.2.3 --arch amd64 --bundle "$WORK/assets/concord-selfhost-1.2.3.tar.gz" \
  --agent "$WORK/agent" --wizard "$WORK/wizard" "$@"; }

deb=$(build --out "$WORK/out" 2>"$WORK/err") && [[ "$deb" == "$WORK/out/concord-selfhost_1.2.3_amd64.deb" ]] &&
  pass 'builds the package' || { fail "build: $(cat "$WORK/err")"; echo "$fails failure(s)"; exit 1; }

listing=$(dpkg-deb -c "$deb")
want() { grep -Eq "^$1 root/root +[0-9]+ [0-9-]+ [0-9:]+ \./$2\$" <<<"$listing" && pass "$2 is $1 root" || fail "$2 not $1 root"; }
want '-rwxr-xr-x' 'usr/libexec/concord-agent'
want '-rw-r--r--' 'usr/lib/systemd/system/concord-agent.service'
want '-rw-r--r--' 'usr/share/concord-selfhost/concord-selfhost-1.2.3.tar.gz'
want '-rw-r--r--' 'usr/share/concord-agent/wizard/index.html'
want '-rw-r--r--' 'usr/share/concord-agent/wizard/assets/app.js'
want '-rw-r--r--' 'usr/share/doc/concord-selfhost/copyright'
grep -q '\./opt' <<<"$listing" && fail 'package owns /opt' || pass 'package does not own /opt'
grep -vq ' root/root ' <<<"$listing" && fail 'an entry is not root-owned' || pass 'every entry is root-owned'

info=$(dpkg-deb -f "$deb")
grep -qx 'Package: concord-selfhost' <<<"$info" && grep -qx 'Version: 1.2.3' <<<"$info" &&
  grep -qx 'Architecture: amd64' <<<"$info" && ! grep -Eq '@[A-Z]+@' <<<"$info" &&
  pass 'control fields filled in' || fail "control fields: $info"
dpkg-deb --ctrl-tarfile "$deb" | tar -xO ./postinst | grep -q 'concord-selfhost-1.2.3.tar.gz' &&
  pass 'postinst names this release' || fail 'postinst does not name this release'
grep -q '^ExecStart=/usr/libexec/concord-agent$' "$REPO/infrastructure/host-agent/concord-agent.service" &&
  pass 'unit runs the packaged agent' || fail 'unit ExecStart is not /usr/libexec/concord-agent'

again=$(build --out "$WORK/out2") && cmp -s "$deb" "$again" && pass 'same inputs give the same package' ||
  fail 'package is not reproducible'

ln -s index.html "$WORK/wizard/link.html"
build --out "$WORK/out3" >/dev/null 2>"$WORK/err" && fail 'accepted a wizard with a link' ||
  { grep -q 'contain a link' "$WORK/err" && pass 'refuses a wizard with a link' || fail "link refusal: $(cat "$WORK/err")"; }
rm "$WORK/wizard/link.html"
build --out "$WORK/out3" --version 1.2 >/dev/null 2>&1 && fail 'accepted version 1.2' || pass 'refuses a bad version'
"$BUILD" --version 1.2.3 --arch amd64 --bundle "$WORK/assets/concord-selfhost-1.2.3.tar.gz" --agent "$WORK/agent" \
  --wizard "$WORK/tree" --out "$WORK/out3" >/dev/null 2>&1 && fail 'accepted a wizard with no index.html' ||
  pass 'refuses a wizard with no index.html'

if ! docker info >/dev/null 2>&1; then
  echo 'SKIP: install tests need Docker'
else
  # shellcheck disable=SC2016  # expanded in the container
  script='set -eu
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null
apt-get install -y -qq --no-install-recommends /pkg/x.deb >/tmp/log 2>&1 || { cat /tmp/log; exit 1; }
grep -q "sudo cat /run/concord-agent/setup-token" /tmp/log
getent group concord-agent >/dev/null
test "$(stat -c %U:%a /opt/concord)" = root:755
test -x /opt/concord/infrastructure/deploy/concord-selfhost
test -z "$(find /opt/concord \( ! -user root -o -perm /022 \) -print)"
echo "KEY=kept" >/opt/concord/.env
apt-get install -y -qq --reinstall /pkg/x.deb >/tmp/log 2>&1
grep -q "already exists; leaving it as it is" /tmp/log
if grep -q "setup-token" /tmp/log; then exit 1; fi
grep -qx KEY=kept /opt/concord/.env
apt-get remove -y -qq concord-selfhost >/dev/null 2>&1
test ! -e /usr/libexec/concord-agent
grep -qx KEY=kept /opt/concord/.env
echo ok'
  cp "$deb" "$WORK/x.deb"
  for image in debian:12 ubuntu:24.04; do
    if out=$(docker run --rm -v "$WORK/x.deb:/pkg/x.deb:ro" "$image" sh -c "$script" 2>&1) && [[ "$out" == *ok ]]; then
      pass "installs, reinstalls and removes on $image"
    else
      fail "install on $image: $out"
    fi
  done
fi

echo "$fails failure(s)"
[[ $fails -eq 0 ]]
