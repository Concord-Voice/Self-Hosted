#!/usr/bin/env bash
# Build the concord-selfhost .deb (#187) for Debian 12 and Ubuntu 24.04 from
# parts built elsewhere: the release bundle (selfhost-bundle.py), the agent
# binary for ARCH, and the setup wizard pages (npm run build:setup).
#
#   build-selfhost-deb.sh --version X.Y.Z --arch amd64|arm64 \
#     --bundle concord-selfhost-X.Y.Z.tar.gz --agent concord-agent \
#     --wizard client/admin/dist-setup --out DIR
#
# Output: DIR/concord-selfhost_X.Y.Z_ARCH.deb. File times come from
# SOURCE_DATE_EPOCH (default 0), so the same inputs give the same package.
set -euo pipefail

fail() { printf 'build-selfhost-deb: %s\n' "$1" >&2; exit 1; }

version='' arch='' bundle='' agent='' wizard='' out=''
while (( $# )); do
  [[ $# -ge 2 ]] || fail "$1 needs a value"
  case "$1" in
    --version) version=$2 ;;
    --arch) arch=$2 ;;
    --bundle) bundle=$2 ;;
    --agent) agent=$2 ;;
    --wizard) wizard=$2 ;;
    --out) out=$2 ;;
    *) fail "unknown option $1" ;;
  esac
  shift 2
done

[[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || fail 'version must be X.Y.Z'
[[ "$arch" == amd64 || "$arch" == arm64 ]] || fail 'arch must be amd64 or arm64'
[[ -f "$bundle" && "$(basename "$bundle")" == "concord-selfhost-$version.tar.gz" ]] ||
  fail "bundle must be concord-selfhost-$version.tar.gz"
[[ -f "$agent" ]] || fail 'agent binary not found'
# ELF e_machine: 62 is x86-64, 183 is AArch64. A wrong-arch agent installs
# fine and then restart-loops on exec, so refuse it here.
case $arch in amd64) machine=62 ;; arm64) machine=183 ;; esac
[[ "$(head -c 4 "$agent" | od -An -c | tr -d ' ')" == '177ELF' &&
  "$(od -An -tu2 -j18 -N2 --endian=little "$agent" | tr -d ' ')" == "$machine" ]] ||
  fail "agent is not an ELF binary for $arch"
[[ -f "$wizard/index.html" ]] || fail 'wizard pages not found (index.html missing)'
[[ -n "$out" ]] || fail '--out is required'

here=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
src="$here/infrastructure/host-agent"
epoch=${SOURCE_DATE_EPOCH:-0}

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
umask 022

install -D -m 0755 "$agent" "$stage/usr/libexec/concord-agent"
install -D -m 0644 "$src/concord-agent.service" "$stage/usr/lib/systemd/system/concord-agent.service"
install -D -m 0644 "$bundle" "$stage/usr/share/concord-selfhost/$(basename "$bundle")"
install -D -m 0644 "$here/LICENSE" "$stage/usr/share/doc/concord-selfhost/copyright"
mkdir -p "$stage/usr/share/concord-agent/wizard"
# The agent refuses a wizard tree with links or loose modes, so copy files only.
if [[ -n "$(find "$wizard" -type l -print -quit)" ]]; then fail 'wizard pages contain a link'; fi
cp -R "$wizard/." "$stage/usr/share/concord-agent/wizard/"
find "$stage/usr" -type d -exec chmod 0755 {} +
find "$stage/usr/share" -type f -exec chmod 0644 {} +

mkdir -m 0755 "$stage/DEBIAN"
size=$(du -sk --exclude=DEBIAN "$stage" | cut -f1)
sed -e "s/@VERSION@/$version/" -e "s/@ARCH@/$arch/" -e "s/@SIZE@/$size/" \
  "$src/deb/control" >"$stage/DEBIAN/control"
for script in postinst prerm postrm; do
  sed "s/@VERSION@/$version/" "$src/deb/$script" >"$stage/DEBIAN/$script"
  chmod 0755 "$stage/DEBIAN/$script"
done
(cd "$stage" && find usr -type f -print0 | LC_ALL=C sort -z | xargs -0 md5sum >DEBIAN/md5sums)
chmod 0644 "$stage/DEBIAN/control" "$stage/DEBIAN/md5sums"

find "$stage" -exec touch -h -d "@$epoch" {} +
mkdir -p "$out"
SOURCE_DATE_EPOCH=$epoch dpkg-deb --root-owner-group -Zxz --build "$stage" \
  "$out/concord-selfhost_${version}_${arch}.deb" >/dev/null
printf '%s\n' "$out/concord-selfhost_${version}_${arch}.deb"
