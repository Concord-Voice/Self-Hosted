#!/usr/bin/env bash
# Self-host account-key authority setup (DoR D-109, D-173, D-218, D-234).
# Sourced by concord-selfhost; relies on PROJECT_ROOT and selfhost-storage.sh's
# selfhost_storage_project_identity.
#
# `authority init` runs once, as root, on an installed stack, and nothing calls
# it yet: the control plane uses the authority only once its client
# integration ships, which then mounts the CAK and CP TLS key this writes.
# `authority upgrade` is the same for an install that already has accounts
# (D-234). Once either has run, up, stop and down manage the authority too.
# It creates ${AUTHORITY_DIR} (default /etc/concord-authority):
#   secrets/     the overlay's compose secrets, 0400, owned by their reader:
#                uid 70 for the three postgres passwords, uid 65532 for the
#                passfiles, the RSK and the TLS key
#   public/      anchors, trust, delegation-not-after-ms (read by
#                `concord-selfhost health`) and realm-id (read by up, and
#                written last: it marks a finished setup), 0644, mounted
#                read-only by the authority
#   root-keys/   product-root primaries A and B (PKCS#8 PEM, root 0600), kept
#                for `authority renew`
#   cp/          the CAK and the CP TLS key, owned by the CP image's uid 10006
#   ceremony/    the specs and the signed first delegation, for the record
#   tools/       the ceremony tool, built here and never put in an image (D-43)
# The recovery keys A' and B' go only to --export-dir, encrypted with a
# passphrase typed twice. They are never written to the host unencrypted
# (D-234). init seals the empty population, so it refuses a control plane
# that already has accounts. upgrade never seals: sealing existing accounts is
# step 3 of the activation sequence (DoR §2.17), and the control plane's side
# of it ships with the client integration.

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

# _authority_warn prints and succeeds, for a message with more steps after it:
# concord-selfhost runs under set -e, so a failing step would stop them.
_authority_warn() { printf 'selfhost-authority: %s\n' "$1" >&2; }
_authority_fail() { _authority_warn "$1"; return 1; }

verb_authority() {
  case "${1:-}" in
    init|upgrade) _authority_init "$@" ;;
    renew) shift; _authority_renew "$@" ;;
    *) _authority_fail 'usage: concord-selfhost authority init|upgrade --export-dir DIR | renew' ;;
  esac
}

# Seams the tests replace: the root check, ownership changes, and the check
# that a recreated authority stays up.
_authority_require_root() { [[ "$(id -u)" == 0 ]] || _authority_fail 'authority init, upgrade and renew must run as root'; }
_authority_chown() { chown "$@"; }
# _authority_regular FILE... — each FILE, just moved out of the stage that the
# keygen container's uid owns, is a plain file: chown and chmod would follow
# a symlink planted there.
_authority_regular() {
  local f
  for f in "$@"; do
    [[ -f "$f" && ! -L "$f" ]] || { _authority_fail "$f is not a regular file"; return 1; }
  done
}
# serve exits 1 on a trust or key it cannot use, and restart: unless-stopped
# then loops it, so "up" alone proves nothing: it must still be running, never
# restarted, 10 s later.
_authority_running() {
  local id state
  sleep 10
  id="$(_authority_compose ps -q authority)" && [[ -n "$id" ]] || return 1
  state="$(docker inspect -f '{{.State.Running}} {{.RestartCount}}' "$id")" && [[ "$state" == 'true 0' ]]
}

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

# _authority_trust_value TOOL DIR TRUST KEY — KEY from show-trust (digits only).
_authority_trust_value() {
  local v
  v="$("$1" show-trust --anchors "$2/public/anchors" --trust "$3" | sed -n "s/^$4=//p")"
  [[ "$v" =~ ^[0-9]+$ ]] || return 1
  printf '%s\n' "$v"
}

# _authority_record_not_after TOOL DIR — the trust's current not_after_ms in
# public/delegation-not-after-ms, which health reads without the root-only tool.
_authority_record_not_after() {
  local v
  v="$(_authority_trust_value "$1" "$2" "$2/public/trust" not_after_ms)" || { _authority_fail 'show-trust gave no not_after_ms'; return 1; }
  printf '%s\n' "$v" >"$2/public/delegation-not-after-ms.next" \
    && chmod 0644 "$2/public/delegation-not-after-ms.next" \
    && mv -f -- "$2/public/delegation-not-after-ms.next" "$2/public/delegation-not-after-ms"
}

# _authority_init init|upgrade ARGS...
_authority_init() {
  local verb="$1" export_dir=''
  shift
  while (( $# )); do
    case "$1" in
      --export-dir) [[ -n "${2:-}" ]] || { _authority_fail '--export-dir needs a directory'; return 1; }; export_dir="$2"; shift 2 ;;
      *) _authority_fail "usage: concord-selfhost authority $verb --export-dir DIR"; return 1 ;;
    esac
  done
  [[ -n "$export_dir" ]] || { _authority_fail "usage: concord-selfhost authority $verb --export-dir DIR"; return 1; }
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
  # The empty seal is permanent, so init refuses an install with accounts;
  # upgrade sets one up and leaves it unsealed.
  accounts="$(_authority_cp_accounts)" && [[ "$accounts" =~ ^[0-9]+$ ]] \
    || { _authority_fail "cannot count the control plane's accounts: start the stack with concord-selfhost up"; return 1; }
  [[ "$verb" == upgrade ]] || (( accounts == 0 )) \
    || { _authority_fail "the control plane has $accounts accounts; init only sets up an authority on an install with none: use concord-selfhost authority upgrade"; return 1; }

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
  ( umask 077 && _authority_init_keys "$dir" "$export_dir" "$pass" "$verb" ) \
    || {
      _authority_warn "$verb stopped. Before running it again, remove $dir, any recovery-*.p8 in $export_dir (they belong to this failed realm), and what init started:"
      printf '  docker compose --project-directory %q --project-name %q --env-file %q -f %q --profile authority-owner down -v\n' \
        "$PROJECT_ROOT" "$SELFHOST_STORAGE_PROJECT_NAME" "$PROJECT_ROOT/.env" "$PROJECT_ROOT/docker-compose.authority.yml" >&2
      return 1
    }
}

_authority_init_keys() {
  local dir="$1" export_dir="$2" pass="$3" verb="$4" stage="$1/stage" tool="$1/tools/authority-ceremony"
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
  _authority_regular "$dir/secrets/rsk" "$dir/secrets/tls-key" "$dir/cp/cak" "$dir/cp/cp-tls-key" || return 1
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
  _authority_record_not_after "$tool" "$dir" || return 1

  echo "Starting the authority database and running its owner steps..."
  _authority_compose up -d --wait authority-postgres || { _authority_fail 'the authority database did not start'; return 1; }
  _authority_compose --profile authority-owner run --rm authority-owner migrate --realm "$realm" \
    || { _authority_fail 'migrate failed'; return 1; }
  _authority_compose --profile authority-owner run --rm authority-owner replication-gate off \
    || { _authority_fail 'replication-gate off failed'; return 1; }
  # init counts again just before the seal, so an account made since the
  # first count is not sealed out. ponytail: this narrows the window but
  # cannot close it while the control plane accepts sign-ups.
  local accounts sealed=0
  if [[ "$verb" == init ]]; then
    accounts="$(_authority_cp_accounts)" && [[ "$accounts" =~ ^[0-9]+$ ]] \
      || { _authority_fail "cannot count the control plane's accounts"; return 1; }
    if (( accounts == 0 )); then
      _authority_compose --profile authority-owner run --rm authority-owner seal-legacy --cp-digest "$AUTHORITY_EMPTY_POPULATION_DIGEST" \
        || { _authority_fail 'seal-legacy failed'; return 1; }
      sealed=1
    fi
  fi
  _authority_compose up -d authority || { _authority_fail 'the authority did not start'; return 1; }
  # Last, as the mark of a finished setup: up manages only an authority that has it.
  printf '%s\n' "$realm" >"$dir/public/realm-id.next" && chmod 0644 "$dir/public/realm-id.next" \
    && mv -f -- "$dir/public/realm-id.next" "$dir/public/realm-id" || return 1

  cat <<EOF
The account-key authority is set up (realm $realm).
  Record the anchors digest somewhere other than this host:
    sha384=$anchors_sha
  Move $export_dir/recovery-a.p8 and recovery-b.p8 to separate offline media
  now, and keep the passphrase apart from both. Only they can replace the
  primary keys, which stay in $dir/root-keys for renewal.
  The first delegation expires in 180 days. Renew it before then with
  concord-selfhost authority renew; health warns from 21 days before.
EOF
  (( sealed )) || cat <<EOF
  It is not sealed. Sealing the control plane's accounts is step 3 of the
  activation sequence, which needs the control plane's authority
  integration. Until then nothing calls the authority.
EOF
}

# ── health ─────────────────────────────────────────────────────────────
readonly AUTHORITY_WARN_MS=$((21 * 86400000))

# _authority_health — for `concord-selfhost health`. 3 when init has not run
# (nothing to check); 0 with more than 21 days left; 2 within 21 days (a
# warning: the authority still signs); 1 when the delegation has expired or
# its record cannot be read.
_authority_health() {
  local dir na now left
  dir="$(_authority_env AUTHORITY_DIR /etc/concord-authority)" || return 1
  [[ -f "$dir/public/trust" ]] || return 3
  # An authority set up before the record existed gets it here, from the
  # root-only tool, when health runs as root and the tool is safe to run.
  ! _authority_backfill_ok "$dir" \
    || _authority_record_not_after "$dir/tools/authority-ceremony" "$dir" >/dev/null 2>&1 || :
  na="$(cat -- "$dir/public/delegation-not-after-ms" 2>/dev/null)"
  now="$(date +%s%3N)"
  if [[ ! "$na" =~ ^[0-9]+$ || ! "$now" =~ ^[0-9]{13}$ ]]; then
    echo '✗ cannot read when the authority delegation expires' >&2
    return 1
  fi
  left=$(( na - now ))
  if [[ "$left" -le 0 ]]; then
    echo '✗ the authority delegation has expired: run concord-selfhost authority renew' >&2
    return 1
  fi
  local days=$(( left / 86400000 ))
  if [[ "$left" -le "$AUTHORITY_WARN_MS" ]]; then
    printf '! the authority delegation expires in %d days: run concord-selfhost authority renew\n' "$days" >&2
    return 2
  fi
  printf 'authority delegation valid for %d more days\n' "$days"
}

# _authority_backfill_ok DIR — health may run DIR's ceremony tool: the
# record is missing, and only root or the user running health can change the
# tool or any directory above it, as init and renew require. Otherwise a
# planted tool would run as root from up, update and restore.
_authority_backfill_ok() {
  local tool="$1/tools/authority-ceremony" uid mode
  [[ ! -s "$1/public/delegation-not-after-ms" && -f "$tool" && -x "$tool" && ! -L "$tool" ]] || return 1
  _authority_safe_dir "$1/tools" 2>/dev/null || return 1
  read -r uid mode < <(stat -c '%u %A' -- "$tool") || return 1
  [[ ( "$uid" == 0 || "$uid" == "$(id -u)" ) && "${mode:5:1}${mode:8:1}" == -- ]]
}

# ── up, stop and down ──────────────────────────────────────────────────
# _authority_lifecycle up|stop|down — the stack's verbs also manage the
# authority once init or upgrade has started, and do nothing before. up
# rebuilds the image, then stops serve, migrates and starts it again (DoR
# deploys: migrate refuses while serve runs), so `update` brings the
# authority along. It runs only on a finished setup, which has realm-id.
# stop and down also clear what a failed setup started. down keeps the
# authority's volumes, as the stack's down keeps its own.
_authority_lifecycle() {
  local dir realm
  dir="$(_authority_env AUTHORITY_DIR /etc/concord-authority)" || return 1
  [[ "$dir" == /* ]] || { _authority_fail 'AUTHORITY_DIR must be an absolute path'; return 1; }
  [[ -f "$dir/public/trust" ]] || return 0
  _authority_safe_dir "$dir" || return 1
  selfhost_storage_project_identity "$PROJECT_ROOT" || return 1
  case "$1" in
    up)
      realm="$(cat -- "$dir/public/realm-id" 2>/dev/null)" || realm=""
      [[ "$realm" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]] || {
        _authority_fail "$dir/public/realm-id is missing, so the authority is left alone. Either authority init or upgrade did not finish: remove $dir and the authority's volumes, then run it again. Or it ran before this release: as root, write the realm_id from $dir/ceremony/anchors.json to $dir/public/realm-id, mode 0644"
        return 1
      }
      echo "Updating the account-key authority..."
      _authority_compose build authority || { _authority_fail 'cannot build the authority image'; return 1; }
      _authority_compose stop authority || { _authority_fail 'cannot stop the authority'; return 1; }
      _authority_compose --profile authority-owner run --rm authority-owner migrate --realm "$realm" \
        || { _authority_fail 'authority migrate failed; the authority is stopped'; return 1; }
      _authority_compose up -d --no-deps --force-recreate authority || { _authority_fail 'the authority did not start'; return 1; }
      _authority_running || { _authority_fail 'the authority did not stay up'; return 1; }
      ;;
    stop) _authority_compose stop ;;
    down) _authority_compose --profile authority-owner down ;;
    *) _authority_fail 'usage: _authority_lifecycle up|stop|down'; return 1 ;;
  esac
}

# ── renew ──────────────────────────────────────────────────────────────
# `authority renew` signs delegation serial + 1 with A and B (§2.16): a new
# RSK and a new CAK, min_valid_serial = the new serial, a new 180-d window,
# and everything else as the current delegation has it. The TLS keys stay:
# they are not a validity control (D-204). Then it installs the new trust
# and keys and recreates the authority, which installs the new delegation.
# A failure before the swap leaves the old files as they were. After it,
# renew only goes forward: the authority installs the delegation in its
# database once it starts, and from then on the old RSK is not in the
# high-water delegation, so the old files cannot run it. The CP's switch to the
# new CAK after the authority shows it (§2.16 step 5) arrives with the
# client integration; until then nothing holds the old CAK.
_authority_renew() {
  (( $# == 0 )) || { _authority_fail 'usage: concord-selfhost authority renew'; return 1; }
  _authority_require_root || return 1
  local c dir
  for c in docker python3; do
    command -v "$c" >/dev/null || { _authority_fail "$c is required"; return 1; }
  done
  [[ -f "$PROJECT_ROOT/.env" ]] || { _authority_fail 'no .env: install the stack first'; return 1; }
  dir="$(_authority_env AUTHORITY_DIR /etc/concord-authority)" || return 1
  [[ "$dir" == /* ]] || { _authority_fail 'AUTHORITY_DIR must be an absolute path'; return 1; }
  [[ -f "$dir/public/trust" ]] || { _authority_fail "no authority in $dir: run concord-selfhost authority init first"; return 1; }
  _authority_safe_dir "$dir" || return 1
  [[ ! -e "$dir/stage" ]] || { _authority_fail "$dir/stage exists: a renew is running, or one stopped; remove it and try again"; return 1; }
  selfhost_storage_project_identity "$PROJECT_ROOT" || return 1
  ( umask 077 && _authority_renew_keys "$dir" )
}

_authority_renew_keys() {
  local dir="$1" stage="$1/stage" tool="$1/tools/authority-ceremony"
  local serial next now spec signed
  # shellcheck disable=SC2034 # keygen sets a point and a pin; not every one is used
  local rsk_point rsk_pin cak_point cak_pin
  [[ -x "$tool" ]] || { _authority_fail "no ceremony tool in $dir/tools"; return 1; }
  serial="$(_authority_trust_value "$tool" "$dir" "$dir/public/trust" serial)" || { _authority_fail 'show-trust failed on the current trust'; return 1; }
  next=$((serial + 1)) spec="$dir/ceremony/delegation-$next.json" signed="$dir/ceremony/delegation-$next.signed"
  [[ -f "$dir/ceremony/delegation-$serial.json" ]] || { _authority_fail "no record of delegation $serial in $dir/ceremony"; return 1; }
  [[ ! -e "$spec" && ! -e "$signed" ]] || { _authority_fail "$spec or its signed copy already exists"; return 1; }

  echo "Generating the new RSK and CAK..."
  mkdir -m 0700 "$stage" || { _authority_fail "cannot create $stage"; return 1; }
  _authority_chown "$AUTHORITY_APP_UID:$AUTHORITY_APP_UID" "$stage" || { _authority_renew_unstage "$dir" "$next"; return 1; }
  _authority_keygen "$stage" rsk rsk || { _authority_renew_unstage "$dir" "$next"; return 1; }
  _authority_keygen "$stage" cak cak || { _authority_renew_unstage "$dir" "$next"; return 1; }
  now="$(date +%s%3N)" && [[ "$now" =~ ^[0-9]{13}$ ]] \
    || { _authority_warn 'cannot read the time in milliseconds'; _authority_renew_unstage "$dir" "$next"; return 1; }
  # The current delegation's spec, with the fields a renewal changes.
  python3 -c '
import json, sys
src, dst, serial, rsk, cak, nb, life = sys.argv[1:]
d = json.load(open(src))
d.update(serial=int(serial), min_valid_serial=int(serial), rsk_public_keys=[rsk], cak_public_keys=[cak],
         not_before_ms=int(nb), not_after_ms=int(nb) + int(life))
with open(dst, "x") as f:
    json.dump(d, f, separators=(",", ":"))
    f.write("\n")
' "$dir/ceremony/delegation-$serial.json" "$spec" "$next" "$rsk_point" "$cak_point" "$((now - 60000))" "$AUTHORITY_LIFETIME_MS" \
    || { _authority_warn 'cannot write the next delegation spec'; _authority_renew_unstage "$dir" "$next"; return 1; }

  echo "Signing delegation $next..."
  "$tool" sign-delegation --anchors "$dir/public/anchors" --trust "$dir/public/trust" --spec "$spec" \
    --out "$signed" --key-file "$dir/root-keys/primary-a.pem" --key-file "$dir/root-keys/primary-b.pem" --yes </dev/null >/dev/null \
    || { _authority_warn 'sign-delegation failed'; _authority_renew_unstage "$dir" "$next"; return 1; }
  "$tool" add-trust --anchors "$dir/public/anchors" --trust "$dir/public/trust" --signed "$signed" \
    --out "$dir/public/trust.next" >/dev/null \
    || { _authority_warn 'add-trust failed'; _authority_renew_unstage "$dir" "$next"; return 1; }
  [[ "$(_authority_trust_value "$tool" "$dir" "$dir/public/trust.next" serial)" == "$next" ]] \
    || { _authority_warn "the new trust does not hold delegation $next"; _authority_renew_unstage "$dir" "$next"; return 1; }

  echo "Installing delegation $next and restarting the authority..."
  _authority_renew_place "$dir" || { _authority_warn 'cannot stage the new keys'; _authority_renew_unstage "$dir" "$next"; return 1; }
  if ! _authority_renew_swap "$dir" next prev; then
    if _authority_renew_swap "$dir" prev next; then
      _authority_renew_unstage "$dir" "$next"
    else
      _authority_warn "cannot put the old files back. Each of $dir/public/trust, $dir/secrets/rsk and $dir/cp/cak with a .prev beside it: move FILE.prev to FILE. Then remove $dir/stage and the .next files."
    fi
    return 1
  fi
  # From here on delegation $next is the authority's: no way back (see above).
  rm -f -- "$dir/public/trust.prev" "$dir/secrets/rsk.prev" "$dir/cp/cak.prev"
  _authority_record_not_after "$tool" "$dir" \
    || _authority_warn "health's expiry record was not updated; run: $tool show-trust --anchors $dir/public/anchors --trust $dir/public/trust | sed -n 's/^not_after_ms=//p' >$dir/public/delegation-not-after-ms"
  if _authority_compose up -d --no-deps --force-recreate authority && _authority_running; then
    printf 'Delegation %s is installed; it expires in 180 days.\n' "$next"
    return 0
  fi
  # A retry would sign another delegation for nothing: fix the cause instead.
  _authority_fail "delegation $next and its keys are in place, but the authority did not stay up on them. Fix the cause from its logs, then recreate it. Do not put the old files back: the authority may already hold delegation $next."
}

# _authority_renew_place DIR — the new RSK and CAK from the stage to
# FILE.next beside the live ones, with their final owners and modes.
_authority_renew_place() {
  chmod 0644 "$1/public/trust.next" || return 1
  mv -- "$1/stage/rsk" "$1/secrets/rsk.next" || return 1
  _authority_regular "$1/secrets/rsk.next" || return 1
  chmod 0400 "$1/secrets/rsk.next" || return 1
  mv -- "$1/stage/cak" "$1/cp/cak.next" || return 1
  _authority_regular "$1/cp/cak.next" || return 1
  _authority_chown "$AUTHORITY_CP_UID:$AUTHORITY_CP_UID" "$1/cp/cak.next" || return 1
  chmod 0400 "$1/cp/cak.next" || return 1
  rmdir -- "$1/stage"
}

# _authority_renew_unstage DIR SERIAL — before the install, a failure removes
# what this run made, and the live files stay as they were.
_authority_renew_unstage() {
  rm -rf -- "$1/stage" "$1/ceremony/delegation-$2.json" "$1/ceremony/delegation-$2.signed" \
    "$1/public/trust.next" "$1/secrets/rsk.next" "$1/cp/cak.next"
}

# _authority_renew_swap DIR FROM TO — each of trust, rsk and cak: the live
# file, if there is one, becomes FILE.TO and FILE.FROM becomes the live file.
# A swap back after a failure part way through therefore finds every file.
_authority_renew_swap() {
  local f
  for f in "$1/public/trust" "$1/secrets/rsk" "$1/cp/cak"; do
    [[ -e "$f.$2" ]] || continue
    [[ ! -e "$f" ]] || mv -f -- "$f" "$f.$3" || { _authority_fail "cannot swap $f"; return 1; }
    mv -f -- "$f.$2" "$f" || { _authority_fail "cannot swap $f"; return 1; }
  done
}
