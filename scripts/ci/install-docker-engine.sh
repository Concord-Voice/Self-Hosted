#!/usr/bin/env bash
# Provision a disposable Ubuntu 24.04 amd64 CI runner, never a managed host.
# Trust identity and package set match the reviewed MinIO publisher bootstrap.
set -euo pipefail

refuse() { printf 'error: %s\n' "$1" >&2; exit 1; }
[[ $# -eq 0 && ${GITHUB_ACTIONS:-} == true && ${RUNNER_OS:-} == Linux && ${RUNNER_ARCH:-} == X64 ]] ||
  refuse 'locked Docker installation requires an amd64 Linux Actions runner'
[[ -z ${DOCKER_HOST:-} && -z ${DOCKER_CONTEXT:-} && -z ${DOCKER_TLS_VERIFY:-} && -z ${DOCKER_CERT_PATH:-} ]] ||
  refuse 'locked Docker installation refuses an overridden daemon identity'
[[ $(lsb_release -is) == Ubuntu && $(lsb_release -cs) == noble && $(dpkg --print-architecture) == amd64 ]] ||
  refuse 'locked Docker packages require Ubuntu 24.04 amd64'
[[ $(docker context show 2>/dev/null) == default ]] ||
  refuse 'locked Docker installation requires the default local context'

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
mapfile -t packages < "$root/.github/ci/docker-engine-packages.lock"
names=(docker-ce docker-ce-cli docker-ce-rootless-extras containerd.io docker-buildx-plugin docker-compose-plugin)
[[ ${#packages[@]} -eq ${#names[@]} ]] || refuse 'invalid Docker package lock'
for i in "${!names[@]}"; do
  [[ ${packages[$i]} == "${names[$i]}="* && ${packages[$i]} =~ ^[a-z0-9.-]+=[0-9:][0-9A-Za-z.:+~_-]*~ubuntu\.24\.04~noble$ ]] ||
    refuse 'invalid Docker package lock'
done

sudo apt-get update -q
sudo apt-get install -y -q --no-install-recommends --no-remove ca-certificates curl gnupg
sudo install -m 0755 -d /etc/apt/keyrings
docker_key_dir=$(mktemp -d)
trap 'rm -rf -- "$docker_key_dir"' EXIT
curl --fail --show-error --silent --location --proto '=https' --tlsv1.2 \
  https://download.docker.com/linux/ubuntu/gpg -o "$docker_key_dir/docker.asc"
# Primary identity independently reviewed from Docker docs d813acb6e172a9af209c4acdc0d6c7c03d969aa2.
if ! docker_primary=$(gpg --homedir "$docker_key_dir" --batch --with-colons --show-keys --fingerprint "$docker_key_dir/docker.asc" 2>/dev/null | awk -F: '
  $1 == "pub" { if (primary) exit 1; primary=1; count++; next }
  $1 == "fpr" && primary { print $10; primary=0 }
  END { if (primary || count != 1) exit 1 }
'); then
  refuse 'Docker signing key verification failed'
fi
[[ $docker_primary == 9DC858229FC7DD38854AE2D88D81803C0EBFCD88 ]] || refuse 'Docker signing key identity refused'
sudo install -m 0644 "$docker_key_dir/docker.asc" /etc/apt/keyrings/docker.asc
sudo tee /etc/apt/sources.list.d/docker.sources >/dev/null <<'EOF'
Types: deb
URIs: https://download.docker.com/linux/ubuntu
Suites: noble
Components: stable
Architectures: amd64
Signed-By: /etc/apt/keyrings/docker.asc 9DC858229FC7DD38854AE2D88D81803C0EBFCD88
EOF
sudo apt-get update -q
sudo apt-get install -y -q --no-install-recommends --no-remove "${packages[@]}"
sudo systemctl restart docker
sudo systemctl is-active --quiet docker
bash "$root/scripts/ci/require-docker-engine.sh"
