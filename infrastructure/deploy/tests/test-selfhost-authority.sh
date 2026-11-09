#!/usr/bin/env bash
# shellcheck disable=SC2015  # pass/fail one-liners: pass never fails
# Unit tests for infrastructure/deploy/selfhost-authority.sh (`authority init`).
# Docker, the ceremony tool, chown and the root check are stubs; openssl and
# python3 are real, so the keys, the exports and the specs are real files.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY="$(cd "$HERE/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf -- "$WORK"' EXIT
for c in openssl python3 od cmp; do
  command -v "$c" >/dev/null || { echo "FAIL: prerequisite missing: $c"; exit 1; }
done

fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; fails=$((fails + 1)); }

PASS='correct horse battery'
hex384() { printf '%s' "$1" | openssl dgst -sha384 -r | cut -c1-96; }

# The fake ceremony tool: copies its input to --out and records its argv.
cat >"$WORK/fake-tool" <<'TOOL'
#!/usr/bin/env bash
printf 'tool %s\n' "$*" >>"$CALLS"
verb="$1"; shift; in='' out=''
while (( $# )); do
  case "$1" in
    --spec|--signed) in="$2"; shift 2 ;;
    --out) out="$2"; shift 2 ;;
    *) shift ;;
  esac
done
[[ "$verb" == "${TOOL_FAIL:-}" ]] && exit 1
cp -- "$in" "$out" || exit 1
[[ "$verb" == make-anchors ]] && printf 'wrote %s\nsha384=%s\n' "$out" "$(openssl dgst -sha384 -r "$out" | cut -c1-96)"
exit 0
TOOL
chmod +x "$WORK/fake-tool"

# The fake docker is an executable on PATH, since init runs compose under
# env -i. It reads its settings from $WORK/state, written by each test:
# CALLS, PG_IMAGE (0 = the postgres image is missing) and SUBNETS.
mkdir "$WORK/bin"
cat >"$WORK/bin/docker" <<DOCKER
#!/usr/bin/env bash
source "$WORK/state"
hex384() { printf '%s' "\$1" | openssl dgst -sha384 -r | cut -c1-96; }
DOCKER
cat >>"$WORK/bin/docker" <<'DOCKER'
printf 'docker %s\n' "$*" >>"$CALLS"
case "$1 $2" in
  'image inspect') [[ "$PG_IMAGE" == 1 ]]; exit ;;
  'network ls') printf 'net1\nnet2\n'; exit 0 ;;
  'network inspect') printf '172.17.0.0/16 \n%s \n' "$SUBNETS"; exit 0 ;;
  'compose '*) exit 0 ;;
esac
vol='' name=''
for a in "$@"; do
  [[ "$a" == *:/out ]] && vol="${a%:/out}"
  [[ "$a" == /out/* ]] && name="${a#/out/}"
done
if [[ " $* " == *" go build "* ]]; then
  cp "$TOOL" "$vol/authority-ceremony"
else
  printf 'key %s' "$name" >"$vol/$name"
  printf 'point=04%s%s\npin=%s\n' "$(hex384 "$name")" "$(hex384 "$name.y")" "$(hex384 "$name.pin")"
fi
DOCKER
chmod +x "$WORK/bin/docker"
PATH="$WORK/bin:$PATH"

# fixture [ENV_LINE...] — a project root with a .env and the Dockerfile line init reads.
fixture() {
  ROOT="$WORK/root-$RANDOM$RANDOM"
  AUTH="$ROOT/authority"
  EXPORT="$ROOT/export"
  CALLS="$ROOT/calls"
  mkdir -p "$ROOT/services/control-plane" "$EXPORT"
  printf 'FROM golang:1.0-alpine@sha256:00 AS builder\n' >"$ROOT/services/control-plane/Dockerfile.account-authority"
  printf 'AUTHORITY_DIR=%s\n' "$AUTH" >"$ROOT/.env"
  local line
  for line in "$@"; do printf '%s\n' "$line" >>"$ROOT/.env"; done
  : >"$CALLS"
  printf 'CALLS=%q TOOL=%q PG_IMAGE=1 SUBNETS=172.18.0.0/16\n' "$CALLS" "$WORK/fake-tool" >"$WORK/state"
}
docker_set() { printf '%s\n' "$1" >>"$WORK/state"; }

# run_init STDIN ARGS... — init in a subshell with the stubs.
# shellcheck disable=SC2034,SC2329  # PROJECT_ROOT and the stubs are read by the helper
run_init() {
  local input="$1"; shift
  (
    PROJECT_ROOT="$ROOT"
    export CALLS TOOL_FAIL="${TOOL_FAIL:-}"
    # shellcheck source=../selfhost-authority.sh
    source "$DEPLOY/selfhost-authority.sh"
    _authority_require_root() { [[ "${NOT_ROOT:-}" != 1 ]] || _authority_fail 'authority init must run as root'; }
    _authority_chown() { printf 'chown %s\n' "$*" >>"$CALLS"; }
    selfhost_storage_project_identity() { SELFHOST_STORAGE_PROJECT_NAME=fixture; }
    verb_authority init "$@"
  ) <<<"$input" >"$ROOT/out" 2>&1
}

# ── The whole run ──────────────────────────────────────────────────────
fixture
if run_init "$PASS"$'\n'"$PASS" --export-dir "$EXPORT"; then
  pass "init completes"
else
  fail "init failed: $(cat "$ROOT/out")"
fi

mode() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }
ok=true
for f in superuser-password app-password owner-password app-pgpass owner-pgpass rsk tls-key; do
  [[ "$(mode "$AUTH/secrets/$f")" == 400 ]] || ok=false
done
for f in cak cp-tls-key; do [[ "$(mode "$AUTH/cp/$f")" == 400 ]] || ok=false; done
[[ "$(mode "$AUTH/public/anchors")" == 644 && "$(mode "$AUTH/public/trust")" == 644 ]] || ok=false
[[ "$(mode "$AUTH/root-keys/primary-a.pem")" == 600 && "$(mode "$AUTH/ceremony/delegation-1.json")" == 600 ]] || ok=false
$ok && pass "secrets 0400, public files 0644, everything else 0600" || fail "a file has the wrong mode"

grep -qxF "chown 70:70 $AUTH/secrets/superuser-password" "$CALLS" && grep -qxF "chown 65532:65532 $AUTH/secrets/app-pgpass" "$CALLS" \
  && grep -qxF "chown 10006:10006 $AUTH/cp/cak $AUTH/cp/cp-tls-key" "$CALLS" \
  && pass "each secret is handed to its reader's uid" || fail "ownership calls: $(grep chown "$CALLS")"

[[ ! -e "$AUTH/stage" && ! -e "$AUTH/recovery" ]] && pass "the stage and recovery directories are gone" \
  || fail "stage or recovery left behind"

app_pw="$(cat "$AUTH/secrets/app-password")"
[[ "$(cat "$AUTH/secrets/app-pgpass")" == "localhost:*:authority:authority_app:$app_pw" && "$app_pw" =~ ^[0-9a-f]{64}$ ]] \
  && pass "the app passfile carries the app password" || fail "app passfile does not match"

# The exports decrypt with the passphrase to the recovery points in the anchors.
ok=true
for k in a b; do
  [[ "$(mode "$EXPORT/recovery-$k.p8")" == 600 ]] || ok=false
  point="$(openssl pkey -in "$EXPORT/recovery-$k.p8" -passin "pass:$PASS" -pubout -outform DER | tail -c 97 | od -An -v -tx1 | tr -d ' \n')"
  [[ "$point" == "$(python3 -c "import json,sys; print(json.load(open(sys.argv[1]))['recovery_$k'])" "$AUTH/ceremony/anchors.json")" ]] || ok=false
done
$ok && pass "the exports decrypt to the anchors' recovery keys" || fail "an export does not match the anchors"
openssl pkey -in "$EXPORT/recovery-a.p8" -passin pass:wrong-passphrase -noout 2>/dev/null \
  && fail "an export opens with the wrong passphrase" || pass "an export refuses the wrong passphrase"

if python3 - "$AUTH/ceremony/delegation-1.json" "$(hex384 rsk)" "$(hex384 cp-tls-key.pin)" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
assert d["scope"] == sorted(d["scope"]) and len(d["scope"]) == 15
assert d["not_after_ms"] - d["not_before_ms"] == 15552000000
assert d["rsk_public_keys"][0].startswith("04" + sys.argv[2])
assert d["cp_tls_pins"] == [sys.argv[3]]
assert (d["serial"], d["min_valid_serial"], d["product_root_gen"]) == (1, 1, 0)
assert (d["tau_ms"], d["delta_ms"], d["g_ms"]) == (600000, 259200000, 900000)
PY
then pass "the first delegation carries the keys, the full scope and 180 days"
else fail "the first delegation spec is wrong"
fi

realm="$(python3 -c "import json,sys; print(json.load(open(sys.argv[1]))['realm_id'])" "$AUTH/ceremony/anchors.json")"
order="$(grep -oE 'compose .*' "$CALLS" | sed 's/.*docker-compose.authority.yml //')"
expected="build authority
up -d --wait authority-postgres
--profile authority-owner run --rm authority-owner migrate --realm $realm
--profile authority-owner run --rm authority-owner replication-gate off
--profile authority-owner run --rm authority-owner seal-legacy --cp-digest 38b060a751ac96384cd9327eb1b1e36a21fdb71114be07434c0cc7bf63f6e1da274edebfe76f65fbd51ad2f14898b95b
up -d authority"
[[ "$order" == "$expected" ]] && pass "compose runs build, database, owner steps, then the authority" \
  || fail "compose calls: $order"
grep -q -- "--project-name fixture" "$CALLS" && pass "compose uses the stack's project name" || fail "no project name"
grep -q "sha384=[0-9a-f]\{96\}" "$ROOT/out" && pass "init prints the anchors digest" || fail "no anchors digest"
! grep -qF "$PASS" "$ROOT/out" "$CALLS" && pass "the passphrase is never printed or passed" || fail "the passphrase leaked"

# ── Refusals before anything is written ────────────────────────────────
# refuse LABEL WANT INPUT ARGS... — init exits non-zero, says WANT, writes nothing in AUTHORITY_DIR.
refuse() {
  local label="$1" want="$2" input="$3"; shift 3
  if run_init "$input" "$@"; then
    fail "$label: accepted"
  elif ! grep -qF -- "$want" "$ROOT/out"; then
    fail "$label: refused without '$want': $(cat "$ROOT/out")"
  elif [[ -e "$AUTH/secrets" ]]; then
    fail "$label: $AUTH was written"
  else
    pass "$label"
  fi
}
good="$PASS"$'\n'"$PASS"
fixture; refuse "no --export-dir" "usage" "$good"
fixture; refuse "an unknown argument" "usage" "$good" --export-dir "$EXPORT" --force
fixture; NOT_ROOT=1 refuse "not root" "must run as root" "$good" --export-dir "$EXPORT"
fixture; rm "$ROOT/.env"; refuse "no .env" "install the stack first" "$good" --export-dir "$EXPORT"
fixture 'AUTHORITY_DIR=relative/dir'; AUTH="$ROOT/relative/dir"
refuse "a relative AUTHORITY_DIR" "absolute" "$good" --export-dir "$EXPORT"
fixture 'AUTHORITY_SUBNET=10.0.0.0/29;rm'; refuse "a .env value with shell characters" "unsupported characters" "$good" --export-dir "$EXPORT"
fixture; mkdir "$AUTH"; refuse "an existing AUTHORITY_DIR" "init runs once" "$good" --export-dir "$EXPORT"
rmdir "$AUTH"
fixture; refuse "a missing export directory" "is not a directory" "$good" --export-dir "$ROOT/nowhere"
fixture; : >"$EXPORT/recovery-a.p8"; refuse "an export directory holding a recovery key" "already holds" "$good" --export-dir "$EXPORT"
fixture; docker_set PG_IMAGE=0; refuse "no postgres image" "run concord-selfhost up first" "$good" --export-dir "$EXPORT"
fixture; docker_set SUBNETS=172.30.0.0/16; refuse "an overlapping subnet" "overlaps a Docker network" "$good" --export-dir "$EXPORT"
fixture 'AUTHORITY_SUBNET=172.30.53.1/29'; refuse "a subnet that is not a network address" "not a network address" "$good" --export-dir "$EXPORT"
fixture; refuse "passphrases that differ" "differ" "$PASS"$'\n'"$PASS!" --export-dir "$EXPORT"
fixture; refuse "a short passphrase" "at least 12" $'short\nshort' --export-dir "$EXPORT"
fixture; refuse "no passphrase" "no passphrase" '' --export-dir "$EXPORT"

# ── A failure part way says how to start again ─────────────────────────
fixture
if TOOL_FAIL=sign-delegation run_init "$good" --export-dir "$EXPORT"; then
  fail "a failed signature was accepted"
elif grep -q "sign-delegation failed" "$ROOT/out" && grep -q "remove $AUTH and what init started" "$ROOT/out" && grep -q "authority-owner down -v" "$ROOT/out" \
     && ! grep -q 'compose.* up ' "$CALLS"; then
  pass "a failed signature stops before the database, naming what to remove"
else
  fail "a failed signature: $(cat "$ROOT/out")"
fi

echo "failures: $fails"
[[ $fails -eq 0 ]]
