#!/usr/bin/env bash
# Tests for scripts/release/build-selfhost-deb.sh (#187): package layout and,
# when Docker is available, install, reinstall and removal on Debian 12 and
# Ubuntu 24.04, with a stub systemctl that records what the maintainer
# scripts ask of systemd.
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
for v in 1.2.3 1.2.4; do
  python3 "$REPO/scripts/release/selfhost-bundle.py" build --public-tree "$WORK/tree" \
    --source-sha "$(git -C "$REPO" rev-parse HEAD)" --version "$v" --out "$WORK/assets-$v" >/dev/null ||
    { echo "FAIL: bundle build $v"; exit 1; }
done
# A hand-made bundle whose modes postinst must not trust.
python3 - "$WORK/assets-bad" <<'PY'
import io, os, sys, tarfile
os.makedirs(sys.argv[1])
with tarfile.open(os.path.join(sys.argv[1], 'concord-selfhost-1.2.5.tar.gz'), 'w:gz') as t:
    for name, mode in (('infrastructure/deploy/concord-selfhost', 0o4777), ('docker-compose.yml', 0o666)):
        i = tarfile.TarInfo('concord-selfhost-1.2.5/' + name); i.mode = mode; i.size = 3
        t.addfile(i, io.BytesIO(b'x\n\n'))
PY
# Just enough of an ELF header for the architecture check (e_machine at 18).
elf() { { printf '\177ELF'; head -c 14 /dev/zero; printf '%b' "$2"; head -c 44 /dev/zero; } >"$1"; }
elf "$WORK/agent" '\076\000'
elf "$WORK/agent-arm64" '\267\000'
printf '<!doctype html>\n' >"$WORK/wizard/index.html"
printf 'x' >"$WORK/wizard/assets/app.js"
chmod 0600 "$WORK/wizard/assets/app.js"

build() { "$BUILD" --version 1.2.3 --arch amd64 --bundle "$WORK/assets-1.2.3/concord-selfhost-1.2.3.tar.gz" \
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
"$BUILD" --version 1.2.3 --arch amd64 --bundle "$WORK/assets-1.2.3/concord-selfhost-1.2.3.tar.gz" --agent "$WORK/agent" \
  --wizard "$WORK/tree" --out "$WORK/out3" >/dev/null 2>&1 && fail 'accepted a wizard with no index.html' ||
  pass 'refuses a wizard with no index.html'
build --out "$WORK/out3" --arch arm64 >/dev/null 2>"$WORK/err" && fail 'accepted an amd64 agent for arm64' ||
  { grep -q 'not an ELF binary for arm64' "$WORK/err" && pass 'refuses an agent for another architecture' ||
    fail "arch refusal: $(cat "$WORK/err")"; }
"$BUILD" --version 1.2.3 --arch arm64 --bundle "$WORK/assets-1.2.3/concord-selfhost-1.2.3.tar.gz" \
  --agent "$WORK/agent-arm64" --wizard "$WORK/wizard" --out "$WORK/out3" >/dev/null 2>&1 &&
  pass 'builds an arm64 package from an arm64 agent' || fail 'arm64 build failed'

if ! docker info >/dev/null 2>&1; then
  echo 'SKIP: install tests need Docker'
else
  "$BUILD" --version 1.2.4 --arch amd64 --bundle "$WORK/assets-1.2.4/concord-selfhost-1.2.4.tar.gz" \
    --agent "$WORK/agent" --wizard "$WORK/wizard" --out "$WORK/pkg" >/dev/null &&
    "$BUILD" --version 1.2.5 --arch amd64 --bundle "$WORK/assets-bad/concord-selfhost-1.2.5.tar.gz" \
      --agent "$WORK/agent" --wizard "$WORK/wizard" --out "$WORK/pkg" >/dev/null || fail 'building the install fixtures'
  cp "$deb" "$WORK/pkg/"
  # shellcheck disable=SC2016  # expanded in the container
  script='set -eu
export DEBIAN_FRONTEND=noninteractive
# apt runs dpkg with its own PATH, so put the stub first on it.
step() { : >/tmp/sc; apt-get -o DPkg::Path=/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin --no-install-recommends "$@" >/tmp/log 2>&1 ||
  { cat /tmp/log; exit 1; }; }
called() { grep -q "$1" /tmp/sc || { echo "systemctl $1 not called"; cat /tmp/sc; exit 1; }; }
not_called() { if grep -q "$1" /tmp/sc; then echo "systemctl $1 called"; exit 1; fi; }
printf "#!/bin/sh\necho \"\$*\" >>/tmp/sc\ncase \"\$*\" in *is-enabled*) echo enabled ;; esac\n" >/usr/local/sbin/systemctl
chmod 0755 /usr/local/sbin/systemctl
wants=/etc/systemd/system/multi-user.target.wants/concord-agent.service
# deb-systemd-helper records an enable here, whether it links the unit itself
# or hands the job to systemctl (the stub, when systemd is installed).
enabled=/var/lib/systemd/deb-systemd-helper-enabled/concord-agent.service.dsh-also
mkdir -p /run/systemd/system
rm -f /usr/sbin/policy-rc.d
# Stands in for the agent reaching setup mode.
mkdir -p /run/concord-agent && : >/run/concord-agent/setup-token
apt-get update -qq >/dev/null
# First install: unpack, enable, start, print the wizard hint.
step install -y -qq /pkg/concord-selfhost_1.2.3_amd64.deb
test -e "$enabled"; called " start concord-agent.service"
grep -q "sudo cat /run/concord-agent/setup-token" /tmp/log
getent group concord-agent >/dev/null
test "$(stat -c %U:%a /opt/concord)" = root:755
test -x /opt/concord/infrastructure/deploy/concord-selfhost
test -z "$(find /opt/concord \( ! -user root -o -perm /022 \) -print)"
echo "KEY=kept" >/opt/concord/.env
# Upgrade: restart only, tree and .env left alone.
step install -y -qq /pkg/concord-selfhost_1.2.4_amd64.deb
called " restart concord-agent.service"; not_called " start "
grep -q "already exists; leaving it as it is" /tmp/log
if grep -q "setup-token" /tmp/log; then exit 1; fi
grep -qx KEY=kept /opt/concord/.env
# Upgrade during setup: no .env and the agent active, so no restart.
rm /opt/concord/.env
step install -y -qq --reinstall /pkg/concord-selfhost_1.2.4_amd64.deb
not_called restart; grep -q "setup may be in progress" /tmp/log
echo "KEY=kept" >/opt/concord/.env
# A write action running: an upgrade does not restart, and removal waits.
: >/run/concord-agent/write-in-progress
step install -y -qq --reinstall /pkg/concord-selfhost_1.2.4_amd64.deb
not_called restart; grep -q "a write action" /tmp/log
if apt-get remove -y -qq concord-selfhost >/tmp/log 2>&1; then echo "removed during a write"; exit 1; fi
grep -q "remove the package after it finishes" /tmp/log
rm /run/concord-agent/write-in-progress
# Removal: stop, mask, reload, keep /opt/concord.
step remove -y -qq concord-selfhost
called "stop concord-agent.service"; called daemon-reload; test ! -e "$wants"
test "$(readlink /etc/systemd/system/concord-agent.service)" = /dev/null
test ! -e /usr/libexec/concord-agent
grep -qx KEY=kept /opt/concord/.env
# Reinstall after removal: unmasked and enabled again.
step install -y -qq /pkg/concord-selfhost_1.2.4_amd64.deb
test ! -L /etc/systemd/system/concord-agent.service; test -e "$enabled"
called " restart concord-agent.service"
step purge -y -qq concord-selfhost
test ! -e "$enabled"; test ! -L /etc/systemd/system/concord-agent.service
grep -qx KEY=kept /opt/concord/.env
# Fresh install with no running systemd (the real systemctl, if any, works
# offline) and a bundle with unsafe modes.
rm -rf /opt/concord /run/systemd/system /usr/local/sbin/systemctl
step install -y -qq /pkg/concord-selfhost_1.2.5_amd64.deb
test -L "$wants"
grep -q "concord-agent is not in setup mode yet" /tmp/log
test -z "$(find /opt/concord \( -perm /6000 -o -perm /022 \) -print)"
echo ok'
  for image in debian:12 ubuntu:24.04; do
    if out=$(docker run --rm -v "$WORK/pkg:/pkg:ro" "$image" sh -c "$script" 2>&1) && [[ "$out" == *ok ]]; then
      pass "installs, upgrades and removes on $image"
    else
      fail "install on $image: $out"
    fi
  done
fi

echo "$fails failure(s)"
[[ $fails -eq 0 ]]
