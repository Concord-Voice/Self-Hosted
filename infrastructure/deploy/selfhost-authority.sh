#!/usr/bin/env bash
# Self-host account-key authority setup (DoR D-109, D-173, D-218, D-234).
# Sourced by concord-selfhost; relies on PROJECT_ROOT and selfhost-storage.sh's
# selfhost_storage_project_identity.
#
# `authority init` runs once, as root, on an installed stack, and nothing calls
# it yet: the control plane uses the authority only once its client
# integration ships, which then mounts the CAK and CP TLS key this writes.
# It creates ${AUTHORITY_DIR} (default /etc/concord-authority):
#   secrets/     the overlay's compose secrets, 0400, owned by their reader:
#                uid 70 for the three postgres passwords, uid 65532 for the
#                passfiles, the RSK and the TLS key
#   public/      anchors and trust, 0644, mounted read-only by the authority
#   root-keys/   product-root primaries A and B (PKCS#8 PEM, root 0600), kept
#                for renewing the delegation
#   cp/          the CAK and the CP TLS key, owned by the CP image's uid 10006
#   ceremony/    the specs and the signed first delegation, for the record
#   tools/       the ceremony tool, built here and never put in an image (D-43)
# The recovery keys A' and B' go only to --export-dir, encrypted with a
# passphrase typed twice. They are never written to the host unencrypted
# (D-234). init seals the empty population, so it refuses a control plane
# that already has accounts (DoR activation sequence, step 3).

readonly AUTHORITY_IMAGE=concordvoice-account-authority:local
readonly AUTHORITY_PG_IMAGE=concordvoice-postgres:16-alpine
readonly AUTHORITY_APP_UID=65532 AUTHORITY_PG_UID=70 AUTHORITY_CP_UID=10006
# SHA-384 of the empty population: a fresh install has no legacy accounts to
# seal (§2.17). cmd/authority-ceremony's tests check it against the contract.
readonly AUTHORITY_EMPTY_POPULATION_DIGEST=38b060a751ac96384cd9327eb1b1e36a21fdb71114be07434c0cc7bf63f6e1da274edebfe76f65fbd51ad2f14898b95b
# Every scope value, sorted; checked by the same test.
readonly AUTHORITY_SCOPE='add_device bind_recovery cancel_pending cancel_recovery_bind enroll_existing enroll_new erase recover replace_root revoke_root rotate_ek rotate_root status suspend unsuspend'
# DoR Tier 2 defaults: tau 10 min, delta 72 h, G 15 min, lifetime 180 d
# (under the 400-d ceiling). health warns 21 d before not_after_ms.
readonly AUTHORITY_TAU_MS=600000 AUTHORITY_DELTA_MS=259200000 AUTHORITY_G_MS=900000
readonly AUTHORITY_LIFETIME_MS=15552000000
readonly AUTHORITY_MIN_PASSPHRASE=12

_authority_fail() { printf 'selfhost-authority: %s\n' "$1" >&2; return 1; }

verb_authority() {
  case "${1:-}" in
    init) shift; _authority_init "$@" ;;
    *) _authority_fail 'usage: concord-selfhost authority init --export-dir DIR' ;;
  esac
}

# Seams the tests replace: the root check and ownership changes.
_authority_require_root() { [[ "$(id -u)" == 0 ]] || _authority_fail 'authority init must run as root'; }
_authority_chown() { chown "$@"; }

# _authority_env KEY DEFAULT — KEY's value in the root .env, which the overlay
# reads too, so both agree. Only plain path or CIDR characters are accepted.
_authority_env() {
  local v
  # As compose reads it: an optional `export`, spaces around `=`, and quotes.
  v="$(sed -En "s/^[[:space:]]*(export[[:space:]]+)?$1[[:space:]]*=[[:space:]]*//p" "$PROJECT_ROOT/.env" 2>/dev/null | tail -n 1)"
  v="${v%"${v##*[![:space:]]}"}"
  [[ "$v" =~ ^\"(.*)\"$ || "$v" =~ ^\'(.*)\'$ ]] && v="${BASH_REMATCH[1]}"
  [[ -n "$v" ]] || v="$2"
  [[ "$v" =~ ^[A-Za-z0-9._/-]+$ ]] || { _authority_fail "$1 in .env has unsupported characters"; return 1; }
  printf '%s\n' "$v"
}

# _authority_compose ARGS... — the overlay alone, under the stack's project
# name so its volumes and network belong to the install. A clean environment:
# only .env sets the overlay's variables.
_authority_compose() {
  # DOCKER_HOST and DOCKER_CONTEXT pass through, so compose talks to the same
  # daemon as init's other docker calls.
  env -i PATH="$PATH" HOME="${HOME:-/root}" COMPOSE_IGNORE_ORPHANS=1 \
    ${DOCKER_HOST:+DOCKER_HOST="$DOCKER_HOST"} ${DOCKER_CONTEXT:+DOCKER_CONTEXT="$DOCKER_CONTEXT"} \
    docker compose --project-directory "$PROJECT_ROOT" --project-name "$SELFHOST_STORAGE_PROJECT_NAME" \
    --env-file "$PROJECT_ROOT/.env" -f "$PROJECT_ROOT/docker-compose.authority.yml" "$@"
}

# _authority_subnet_free CIDR IP — no Docker network already uses an
# overlapping subnet, and IP is a host address in CIDR other than its gateway.
_authority_subnet_free() {
  local -a ids used
  local out
  # Docker always has its default networks, so an empty list means docker failed.
  mapfile -t ids < <(docker network ls -q)
  (( ${#ids[@]} )) || { _authority_fail 'cannot list Docker networks'; return 1; }
  out="$(docker network inspect --format '{{range .IPAM.Config}}{{.Subnet}} {{end}}' "${ids[@]}")" \
    || { _authority_fail 'cannot inspect Docker networks'; return 1; }
  # One line per network, its subnets separated by spaces.
  read -ra used <<<"${out//$'\n'/ }"
  python3 -c '
import ipaddress, sys
try:
    want = ipaddress.ip_network(sys.argv[1], strict=True)
except ValueError:
    sys.exit("selfhost-authority: AUTHORITY_SUBNET %s is not a network address" % sys.argv[1])
try:
    ip = ipaddress.ip_address(sys.argv[2])
except ValueError:
    sys.exit("selfhost-authority: AUTHORITY_IP %s is not an address" % sys.argv[2])
hosts = list(want.hosts())
if ip not in hosts[1:]:
    sys.exit("selfhost-authority: AUTHORITY_IP %s must be a host in %s other than its gateway %s" % (ip, want, hosts[0] if hosts else "-"))
for s in sys.argv[3:]:
    n = ipaddress.ip_network(s, strict=False)
    if n.version == want.version and n.overlaps(want):
        sys.exit("selfhost-authority: AUTHORITY_SUBNET %s overlaps a Docker network; set another /29 in .env" % sys.argv[1])
' "$1" "$2" "${used[@]}"
}

# _authority_passphrase — read twice from stdin; the passphrase goes to stdout.
_authority_passphrase() {
  local a b
  printf 'Passphrase for the exported recovery keys: ' >&2
  IFS= read -rs a || { printf '\n' >&2; _authority_fail 'no passphrase'; return 1; }
  printf '\nAgain: ' >&2
  IFS= read -rs b || { printf '\n' >&2; _authority_fail 'no passphrase'; return 1; }
  printf '\n' >&2
  [[ "$a" == "$b" ]] || { _authority_fail 'the passphrases differ'; return 1; }
  (( ${#a} >= AUTHORITY_MIN_PASSPHRASE )) || { _authority_fail "the passphrase needs at least $AUTHORITY_MIN_PASSPHRASE characters"; return 1; }
  printf '%s' "$a"
}

# _authority_keygen STAGE NAME VAR — an authority-image key (DER PKCS#8,
# uid 65532, 0400) at STAGE/NAME; VAR_point and VAR_pin get its outputs.
_authority_keygen() {
  local out point pin
  out="$(docker run --rm --network none --read-only --cap-drop ALL --security-opt no-new-privileges:true \
    -v "$1:/out" "$AUTHORITY_IMAGE" keygen --out "/out/$2")" || { _authority_fail "keygen $2 failed"; return 1; }
  point="$(printf '%s\n' "$out" | sed -n 's/^point=//p')"
  pin="$(printf '%s\n' "$out" | sed -n 's/^pin=//p')"
  [[ "$point" =~ ^04[0-9a-f]{192}$ && "$pin" =~ ^[0-9a-f]{96}$ ]] || { _authority_fail "keygen $2 printed no point and pin"; return 1; }
  printf -v "${3}_point" '%s' "$point"
  printf -v "${3}_pin" '%s' "$pin"
}

# _authority_point VAR OPENSSL-PKEY-ARGS... — the key's P-384 point in VAR:
# the SPKI DER's last 97 bytes, 0x04 then X and Y.
_authority_point() {
  local var="$1" point; shift
  point="$(openssl pkey "$@" -pubout -outform DER | tail -c 97 | od -An -v -tx1 | tr -d ' \n')"
  [[ "$point" =~ ^04[0-9a-f]{192}$ ]] || return 1
  printf -v "$var" '%s' "$point"
}

# _authority_root_key FILE VAR — a product-root key (PKCS#8 PEM, 0600) and its point in VAR.
_authority_root_key() {
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-384 -out "$1" 2>/dev/null \
    || { _authority_fail "cannot generate $1"; return 1; }
  chmod 0600 "$1" || return 1
  _authority_point "$2" -in "$1" || { _authority_fail "no P-384 point for $1"; return 1; }
}

# _authority_recovery_key DEST PASSPHRASE VAR — a recovery key written only
# to DEST, scrypt-encrypted; the plaintext stays in a pipe. VAR gets the
# point read back by decrypting DEST, so the anchors hold only a key that
# the export is proven to open. p=16 makes each guess 16 times dearer:
# OpenSSL refuses a larger N (above 32 MiB).
_authority_recovery_key() {
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-384 2>/dev/null \
    | openssl pkcs8 -topk8 -v2 aes-256-cbc -scrypt -scrypt_N 16384 -scrypt_r 8 -scrypt_p 16 \
      -out "$1" -passout fd:3 3< <(printf '%s' "$2") \
    || { _authority_fail "cannot export $1"; return 1; }
  _authority_point "$3" -in "$1" -passin fd:3 3< <(printf '%s' "$2") 2>/dev/null \
    || { _authority_fail "$1 does not decrypt with the passphrase"; return 1; }
}

# _authority_cp_accounts — how many accounts the control plane has. A seam
# for the tests; needs the stack's postgres running.
_authority_cp_accounts() {
  compose exec -T postgres psql -U concord -d concord -v ON_ERROR_STOP=1 -At -c 'SELECT count(*) FROM users'
}

# _authority_safe_dir DIR — refuses unless DIR has no symlink in its path and
# only root, or the user running init, can change DIR or any directory above
# it. Otherwise another user could swap a path init later writes or runs.
# A sticky directory such as /tmp is accepted above DIR, not as DIR itself.
_authority_safe_dir() {
  local p="$1" me uid mode sticky=0
  [[ "$(cd -P -- "$p" 2>/dev/null && pwd)" == "$p" ]] \
    || { _authority_fail "$p must be an existing directory with no symlink in its path"; return 1; }
  me="$(id -u)"
  while :; do
    # %A is drwxrwxrwt: group write at 5, other write at 8, sticky (t) at 9.
    read -r uid mode < <(stat -c '%u %A' -- "$p") || { _authority_fail "cannot stat $p"; return 1; }
    [[ "$uid" == 0 || "$uid" == "$me" ]] || { _authority_fail "$p is owned by uid $uid, not root"; return 1; }
    [[ "${mode:5:1}${mode:8:1}" == -- || ( "$sticky" == 1 && "${mode:9:1}" == t ) ]] \
      || { _authority_fail "$p is writable by other users"; return 1; }
    [[ "$p" == / ]] && return 0
    sticky=1; p="${p%/*}"; p="${p:-/}"
  done
}

# _authority_secret FILE UID VALUE — VALUE in FILE, mode 0400, owned by UID.
_authority_secret() {
  printf '%s' "$3" >"$1" && chmod 0400 "$1" && _authority_chown "$2:$2" "$1"
}

_authority_init() {
  local export_dir=''
  while (( $# )); do
    case "$1" in
      --export-dir) [[ -n "${2:-}" ]] || { _authority_fail '--export-dir needs a directory'; return 1; }; export_dir="$2"; shift 2 ;;
      *) _authority_fail 'usage: concord-selfhost authority init --export-dir DIR'; return 1 ;;
    esac
  done
  [[ -n "$export_dir" ]] || { _authority_fail 'usage: concord-selfhost authority init --export-dir DIR'; return 1; }
  _authority_require_root || return 1
  local c
  for c in docker openssl python3 od cmp; do
    command -v "$c" >/dev/null || { _authority_fail "$c is required"; return 1; }
  done
  [[ -f "$PROJECT_ROOT/.env" ]] || { _authority_fail 'no .env: install the stack first'; return 1; }

  local dir subnet ip accounts
  dir="$(_authority_env AUTHORITY_DIR /etc/concord-authority)" || return 1
  subnet="$(_authority_env AUTHORITY_SUBNET 172.30.53.0/29)" || return 1
  ip="$(_authority_env AUTHORITY_IP 172.30.53.2)" || return 1
  [[ "$dir" == /* ]] || { _authority_fail 'AUTHORITY_DIR must be an absolute path'; return 1; }
  [[ ! -e "$dir" && ! -L "$dir" ]] || { _authority_fail "$dir exists: init runs once, on a host with no authority"; return 1; }
  local parent="${dir%/*}"
  _authority_safe_dir "${parent:-/}" || return 1
  [[ -d "$export_dir" && ! -L "$export_dir" ]] || { _authority_fail "--export-dir $export_dir is not a directory"; return 1; }
  export_dir="$(cd -- "$export_dir" && pwd -P)" || return 1
  _authority_safe_dir "$export_dir" || return 1
  [[ ! -e "$export_dir/recovery-a.p8" && ! -e "$export_dir/recovery-b.p8" ]] \
    || { _authority_fail "$export_dir already holds recovery-a.p8 or recovery-b.p8"; return 1; }
  docker image inspect "$AUTHORITY_PG_IMAGE" >/dev/null 2>&1 \
    || { _authority_fail "$AUTHORITY_PG_IMAGE is missing: run concord-selfhost up first"; return 1; }
  _authority_subnet_free "$subnet" "$ip" || return 1
  selfhost_storage_project_identity "$PROJECT_ROOT" || return 1
  # init seals the empty population, which is permanent: an install that
  # already has accounts needs the manual activation sequence instead.
  accounts="$(_authority_cp_accounts)" && [[ "$accounts" =~ ^[0-9]+$ ]] \
    || { _authority_fail "cannot count the control plane's accounts: start the stack with concord-selfhost up"; return 1; }
  (( accounts == 0 )) \
    || { _authority_fail "the control plane has $accounts accounts; init only sets up an authority on an install with none"; return 1; }

  local pass
  pass="$(_authority_passphrase)" || return 1

  echo "Creating $dir..."
  # Nothing below is retried in place: on a failure, remove $dir and the
  # authority volumes, then run init again.
  mkdir -m 0755 "$dir" "$dir/secrets" "$dir/public" "$dir/cp" || { _authority_fail "cannot create $dir"; return 1; }
  mkdir -m 0700 "$dir/root-keys" "$dir/ceremony" "$dir/tools" "$dir/stage" || { _authority_fail "cannot create $dir"; return 1; }
  _authority_chown "$AUTHORITY_APP_UID:$AUTHORITY_APP_UID" "$dir/stage" || { _authority_fail "cannot create $dir"; return 1; }
  # umask 077 for every file init writes: each is narrowed or widened to its
  # final mode afterwards, never readable by others in between.
  ( umask 077 && _authority_init_keys "$dir" "$export_dir" "$pass" ) \
    || {
      _authority_fail "init stopped. Before running it again, remove $dir, any recovery-*.p8 in $export_dir (they belong to this failed realm), and what init started:"
      printf '  docker compose --project-directory %q --project-name %q --env-file %q -f %q --profile authority-owner down -v\n' \
        "$PROJECT_ROOT" "$SELFHOST_STORAGE_PROJECT_NAME" "$PROJECT_ROOT/.env" "$PROJECT_ROOT/docker-compose.authority.yml" >&2
      return 1
    }
}

_authority_init_keys() {
  local dir="$1" export_dir="$2" pass="$3" stage="$1/stage" tool="$1/tools/authority-ceremony"
  local now realm su_pw app_pw owner_pw go_image anchors_sha
  # shellcheck disable=SC2034 # keygen sets a point and a pin; not every one is used
  local rsk_point rsk_pin tls_point tls_pin cak_point cak_pin cptls_point cptls_pin pa pb ra rb

  echo "Building the authority image..."
  _authority_compose build authority || { _authority_fail 'cannot build the authority image'; return 1; }
  echo "Building the ceremony tool..."
  go_image="$(sed -n 's/^FROM \(golang:[^ ]*\) AS builder$/\1/p' "$PROJECT_ROOT/services/control-plane/Dockerfile.account-authority")"
  [[ -n "$go_image" ]] || { _authority_fail 'no Go image in Dockerfile.account-authority'; return 1; }
  docker run --rm --cap-drop ALL --security-opt no-new-privileges:true \
    -v "$PROJECT_ROOT/services/control-plane:/src:ro" -v "$dir/tools:/out" -w /src \
    -e CGO_ENABLED=0 -e GOFLAGS=-mod=readonly -e GOCACHE=/tmp/go-cache -e GOMODCACHE=/tmp/go-mod \
    "$go_image" go build -trimpath -o /out/authority-ceremony ./cmd/authority-ceremony \
    || { _authority_fail 'cannot build the ceremony tool'; return 1; }
  chmod 0700 "$tool" || return 1

  echo "Generating keys..."
  _authority_keygen "$stage" rsk rsk || return 1
  _authority_keygen "$stage" tls-key tls || return 1
  _authority_keygen "$stage" cak cak || return 1
  _authority_keygen "$stage" cp-tls-key cptls || return 1
  mv -- "$stage/rsk" "$stage/tls-key" "$dir/secrets/" || return 1
  mv -- "$stage/cak" "$stage/cp-tls-key" "$dir/cp/" || return 1
  _authority_chown "$AUTHORITY_CP_UID:$AUTHORITY_CP_UID" "$dir/cp/cak" "$dir/cp/cp-tls-key" || return 1
  chmod 0400 "$dir/secrets/rsk" "$dir/secrets/tls-key" "$dir/cp/cak" "$dir/cp/cp-tls-key" || return 1
  rmdir -- "$stage" || return 1
  _authority_root_key "$dir/root-keys/primary-a.pem" pa || return 1
  _authority_root_key "$dir/root-keys/primary-b.pem" pb || return 1
  echo "Exporting the recovery keys..."
  _authority_recovery_key "$export_dir/recovery-a.p8" "$pass" ra || return 1
  _authority_recovery_key "$export_dir/recovery-b.p8" "$pass" rb || return 1

  # Hex passwords: nothing in a passfile needs escaping (`:` and `\` never occur).
  su_pw="$(openssl rand -hex 32)" && app_pw="$(openssl rand -hex 32)" && owner_pw="$(openssl rand -hex 32)" || return 1
  _authority_secret "$dir/secrets/superuser-password" "$AUTHORITY_PG_UID" "$su_pw" || return 1
  _authority_secret "$dir/secrets/app-password" "$AUTHORITY_PG_UID" "$app_pw" || return 1
  _authority_secret "$dir/secrets/owner-password" "$AUTHORITY_PG_UID" "$owner_pw" || return 1
  _authority_secret "$dir/secrets/app-pgpass" "$AUTHORITY_APP_UID" "localhost:*:authority:authority_app:$app_pw"$'\n' || return 1
  _authority_secret "$dir/secrets/owner-pgpass" "$AUTHORITY_APP_UID" "localhost:*:authority:authority_owner:$owner_pw"$'\n' || return 1

  echo "Signing the first delegation..."
  realm="$(python3 -c 'import uuid; print(uuid.uuid4())')" || return 1
  printf '{"realm_id":"%s","primary_a":"%s","primary_b":"%s","recovery_a":"%s","recovery_b":"%s"}\n' \
    "$realm" "$pa" "$pb" "$ra" "$rb" >"$dir/ceremony/anchors.json" || return 1
  anchors_sha="$("$tool" make-anchors --spec "$dir/ceremony/anchors.json" --out "$dir/public/anchors" | sed -n 's/^sha384=//p')"
  [[ "$anchors_sha" =~ ^[0-9a-f]{96}$ ]] || { _authority_fail 'make-anchors failed'; return 1; }
  now="$(date +%s%3N)" && [[ "$now" =~ ^[0-9]{13}$ ]] || { _authority_fail 'cannot read the time in milliseconds'; return 1; }
  local scope
  local -a scopes
  read -ra scopes <<<"$AUTHORITY_SCOPE"
  scope="$(printf '"%s",' "${scopes[@]}")"
  printf '{"realm_id":"%s","serial":1,"min_valid_serial":1,"product_root_gen":0,"rsk_public_keys":["%s"],"cak_public_keys":["%s"],"operator_public_keys":[],"authority_tls_keys":["%s"],"cp_tls_pins":["%s"],"not_before_ms":%s,"not_after_ms":%s,"scope":[%s],"accepted_versions":["1"],"tau_ms":%s,"delta_ms":%s,"g_ms":%s,"cutover_at_ms":0}\n' \
    "$realm" "$rsk_point" "$cak_point" "$tls_point" "$cptls_pin" "$((now - 60000))" "$((now - 60000 + AUTHORITY_LIFETIME_MS))" \
    "${scope%,}" "$AUTHORITY_TAU_MS" "$AUTHORITY_DELTA_MS" "$AUTHORITY_G_MS" >"$dir/ceremony/delegation-1.json" || return 1
  "$tool" sign-delegation --anchors "$dir/public/anchors" --first --spec "$dir/ceremony/delegation-1.json" \
    --out "$dir/ceremony/delegation-1.signed" --key-file "$dir/root-keys/primary-a.pem" \
    --key-file "$dir/root-keys/primary-b.pem" --yes </dev/null >/dev/null || { _authority_fail 'sign-delegation failed'; return 1; }
  "$tool" add-trust --anchors "$dir/public/anchors" --first --signed "$dir/ceremony/delegation-1.signed" \
    --out "$dir/public/trust" >/dev/null || { _authority_fail 'add-trust failed'; return 1; }
  # Written 0600 under the umask; the authority reads these as uid 65532.
  chmod 0644 "$dir/public/anchors" "$dir/public/trust" || return 1

  echo "Starting the authority database and running its owner steps..."
  _authority_compose up -d --wait authority-postgres || { _authority_fail 'the authority database did not start'; return 1; }
  _authority_compose --profile authority-owner run --rm authority-owner migrate --realm "$realm" \
    || { _authority_fail 'migrate failed'; return 1; }
  _authority_compose --profile authority-owner run --rm authority-owner replication-gate off \
    || { _authority_fail 'replication-gate off failed'; return 1; }
  _authority_compose --profile authority-owner run --rm authority-owner seal-legacy --cp-digest "$AUTHORITY_EMPTY_POPULATION_DIGEST" \
    || { _authority_fail 'seal-legacy failed'; return 1; }
  _authority_compose up -d authority || { _authority_fail 'the authority did not start'; return 1; }

  cat <<EOF
The account-key authority is set up (realm $realm).
  Record the anchors digest somewhere other than this host:
    sha384=$anchors_sha
  Move $export_dir/recovery-a.p8 and recovery-b.p8 to separate offline media
  now, and keep the passphrase apart from both. Only they can replace the
  primary keys, which stay in $dir/root-keys for renewal.
  The first delegation expires in 180 days. Renewal comes in a later release.
EOF
}
