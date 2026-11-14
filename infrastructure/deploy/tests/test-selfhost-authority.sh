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

# The fake ceremony tool: copies its input to --out and records its argv, so
# a trust file is the last delegation spec. show-trust prints that spec's
# fields as the real one does.
cat >"$WORK/fake-tool" <<'TOOL'
#!/usr/bin/env bash
printf 'tool %s\n' "$*" >>"$CALLS"
verb="$1"; shift; in='' out='' trust=''
while (( $# )); do
  case "$1" in
    --spec|--signed) in="$2"; shift 2 ;;
    --out) out="$2"; shift 2 ;;
    --trust) trust="$2"; shift 2 ;;
    *) shift ;;
  esac
done
[[ "$verb" == "${TOOL_FAIL:-}" ]] && exit 1
if [[ "$verb" == show-trust ]]; then
  exec python3 -c 'import json, sys
d = json.load(open(sys.argv[1]))
for k in ("realm_id", "serial", "min_valid_serial", "not_before_ms", "not_after_ms"):
    print("%s=%s" % (k, d[k]))' "$trust"
fi
cp -- "$in" "$out" || exit 1
[[ "$verb" == make-anchors ]] && printf 'wrote %s\nsha384=%s\n' "$out" "$(openssl dgst -sha384 -r "$out" | cut -c1-96)"
exit 0
TOOL
chmod +x "$WORK/fake-tool"

# The fake docker is an executable on PATH, since init runs compose under
# env -i. It reads its settings from $WORK/state, written by each test:
# CALLS, PG_IMAGE (0 = the postgres image is missing), SUBNETS, SALT,
# which keygen mixes into its points so that a renewal's keys are new, and
# LINK=NAME:TARGET, which makes keygen leave key NAME as a symlink to TARGET,
# and COMPOSE_FAIL=WORD, which fails each compose call with WORD as an argument.
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
  'compose '*) [[ -z "${COMPOSE_FAIL:-}" || " $* " != *" $COMPOSE_FAIL "* ]]; exit ;;
esac
vol='' name=''
for a in "$@"; do
  [[ "$a" == *:/out ]] && vol="${a%:/out}"
  [[ "$a" == /out/* ]] && name="${a#/out/}"
done
if [[ " $* " == *" go build "* ]]; then
  cp "$TOOL" "$vol/authority-ceremony"
else
  if [[ "${LINK%%:*}" == "$name" ]]; then ln -s "${LINK#*:}" "$vol/$name"
  elif [[ "$name" == tls-key ]]; then
    # A real P-384 key: renew reads the live TLS key's point from the file.
    openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-384 -out "$vol/$name" 2>/dev/null
    printf 'point=%s\npin=%s\n' "$(openssl pkey -in "$vol/$name" -pubout -outform DER | tail -c 97 | od -An -v -tx1 | tr -d ' \n')" "$(hex384 "$name.pin")"
    exit 0
  else printf 'key %s%s' "$name" "${SALT:-}" >"$vol/$name"; fi
  printf 'point=04%s%s\npin=%s\n' "$(hex384 "$name${SALT:-}")" "$(hex384 "$name.y")" "$(hex384 "$name.pin")"
fi
DOCKER
chmod +x "$WORK/bin/docker"
# openssl is real, wrapped so that a passphrase on its command line fails the run.
cat >"$WORK/bin/openssl" <<OPENSSL
#!/usr/bin/env bash
[[ " \$* " != *"$PASS"* ]] || { echo 'openssl got the passphrase in argv' >&2; exit 99; }
exec $(command -v openssl) "\$@"
OPENSSL
chmod +x "$WORK/bin/openssl"
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
  printf 'CALLS=%q TOOL=%q PG_IMAGE=1 SUBNETS=172.18.0.0/16 LINK=\n' "$CALLS" "$WORK/fake-tool" >"$WORK/state"
}
docker_set() { printf '%s\n' "$1" >>"$WORK/state"; }

# run_init STDIN ARGS... — init (or VERB) in a new bash with the stubs. A new
# process keeps set -e in force as concord-selfhost runs it: a subshell under
# a caller's `if` would ignore it.
run_init() {
  local input="$1"; shift
  ROOT="$ROOT" DEPLOY="$DEPLOY" CALLS="$CALLS" TOOL_FAIL="${TOOL_FAIL:-}" SWAP_FAIL="${SWAP_FAIL:-}" FN="${FN:-verb_authority}" \
  bash -euo pipefail -c '
    # shellcheck disable=SC2034 # read by the helper
    PROJECT_ROOT="$ROOT"
    # shellcheck source=../selfhost-authority.sh
    source "$DEPLOY/selfhost-authority.sh"
    _authority_require_root() { [[ "${NOT_ROOT:-}" != 1 ]] || _authority_fail "authority init must run as root"; }
    _authority_chown() { printf "chown %s\n" "$*" >>"$CALLS"; }
    selfhost_storage_project_identity() { SELFHOST_STORAGE_PROJECT_NAME=fixture; }
    # CP_ACCOUNTS for the first count, CP_ACCOUNTS_LATER (default the same) for the one before the seal.
    _authority_cp_accounts() {
      [[ "${CP_ACCOUNTS:-0}" != fail ]] || return 1
      if [[ -e "$ROOT/counted" ]]; then printf "%s\n" "${CP_ACCOUNTS_LATER:-${CP_ACCOUNTS:-0}}"
      else : >"$ROOT/counted"; printf "%s\n" "${CP_ACCOUNTS:-0}"; fi
    }
    _authority_running() { printf "running?\n" >>"$CALLS"; [[ "${RUNNING:-1}" == 1 ]]; }
    [[ "${RECORD_FAIL:-}" != 1 ]] || _authority_record_not_after() { return 1; }
    # SWAP_FAIL=FILE: the first mv onto FILE fails.
    [[ -z "$SWAP_FAIL" ]] || mv() {
      if [[ "${*: -1}" == "$SWAP_FAIL" && ! -e "$ROOT/swap-failed" ]]; then : >"$ROOT/swap-failed"; return 1; fi
      command mv "$@"
    }
    "$FN" "${VERB:-init}" "$@"
  ' run_init "$@" <<<"$input" >"$ROOT/out" 2>&1
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

[[ ! -e "$AUTH/stage" && -z "$(find "$AUTH" -name 'recovery*')" ]] && pass "the stage is gone and no recovery key is on the host" \
  || fail "stage or a recovery key left behind"

app_pw="$(cat "$AUTH/secrets/app-password")"
[[ "$(cat "$AUTH/secrets/app-pgpass")" == "localhost:*:authority:authority_app:$app_pw" && "$app_pw" =~ ^[0-9a-f]{64}$ ]] \
  && pass "the app passfile carries the app password" || fail "app passfile does not match"

# The exports decrypt with the passphrase to the recovery points in the anchors.
export_point() {
  openssl pkey -in "$1" -passin fd:3 -pubout -outform DER 3< <(printf '%s' "$PASS") | tail -c 97 | od -An -v -tx1 | tr -d ' \n'
}
ok=true
for k in a b; do
  [[ "$(mode "$EXPORT/recovery-$k.p8")" == 600 ]] || ok=false
  point="$(export_point "$EXPORT/recovery-$k.p8")"
  [[ "$point" == "$(python3 -c "import json,sys; print(json.load(open(sys.argv[1]))['recovery_$k'])" "$AUTH/ceremony/anchors.json")" ]] || ok=false
done
$ok && pass "the exports decrypt to the anchors' recovery keys" || fail "an export does not match the anchors"
openssl pkey -in "$EXPORT/recovery-a.p8" -passin pass:wrong-passphrase -noout 2>/dev/null \
  && fail "an export opens with the wrong passphrase" || pass "an export refuses the wrong passphrase"

# Not `if python3 - <<PY`: Semgrep's bash parser refuses a heredoc in an if condition.
python3 - "$AUTH/ceremony/delegation-1.json" "$(hex384 rsk)" "$(hex384 cp-tls-key.pin)" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
assert d["scope"] == sorted(d["scope"]) and len(d["scope"]) == 15
assert d["not_after_ms"] - d["not_before_ms"] == 15552000000
assert d["rsk_public_keys"][0].startswith("04" + sys.argv[2])
assert d["cp_tls_pins"] == [sys.argv[3]]
assert (d["serial"], d["min_valid_serial"], d["product_root_gen"]) == (1, 1, 0)
assert (d["tau_ms"], d["delta_ms"], d["g_ms"]) == (600000, 259200000, 900000)
PY
spec_rc=$?
if [[ $spec_rc -eq 0 ]]; then pass "the first delegation carries the keys, the full scope and 180 days"
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

na="$(python3 -c "import json,sys; print(json.load(open(sys.argv[1]))['not_after_ms'])" "$AUTH/ceremony/delegation-1.json")"
[[ "$(cat "$AUTH/public/delegation-not-after-ms")" == "$na" && "$(mode "$AUTH/public/delegation-not-after-ms")" == 644 ]] \
  && pass "init records the delegation's expiry for health" || fail "no delegation-not-after-ms record"
[[ "$(cat "$AUTH/public/realm-id")" == "$realm" && "$(mode "$AUTH/public/realm-id")" == 644 ]] \
  && pass "init records the realm for up" || fail "no realm-id record"
grep -q "not sealed" "$ROOT/out" && fail "init reports an unsealed authority" || pass "init seals the empty population"

# ── up, stop and down, on the authority init just made ─────────────────
lifecycle_calls() { grep -oE 'compose .*' "$CALLS" | sed 's/.*docker-compose.authority.yml //'; }
: >"$CALLS"
if FN=_authority_lifecycle VERB=up run_init ''; then pass "up updates the authority"; else fail "up failed: $(cat "$ROOT/out")"; fi
expected="build authority
stop authority
--profile authority-owner run --rm authority-owner migrate --realm $realm
up -d --no-deps --force-recreate authority"
[[ "$(lifecycle_calls)" == "$expected" ]] && grep -qxF 'running?' "$CALLS" \
  && pass "up builds, stops serve, migrates, then recreates the authority and checks it stays up" || fail "up calls: $(lifecycle_calls)"
: >"$CALLS"
if RUNNING=0 FN=_authority_lifecycle VERB=up run_init ''; then fail "up accepted an authority that did not stay up"
else grep -qF 'did not stay up' "$ROOT/out" && pass "up fails when the authority does not stay up" || fail "up, not running: $(cat "$ROOT/out")"
fi
: >"$CALLS"
FN=_authority_lifecycle VERB=stop run_init '' && [[ "$(lifecycle_calls)" == stop ]] && pass "stop stops the authority" || fail "stop calls: $(lifecycle_calls)"
: >"$CALLS"
FN=_authority_lifecycle VERB=down run_init '' && [[ "$(lifecycle_calls)" == '--profile authority-owner down' ]] \
  && pass "down removes the authority's containers and keeps its volumes" || fail "down calls: $(lifecycle_calls)"
mv "$AUTH/public/realm-id" "$AUTH/public/realm-id.saved"; : >"$CALLS"
if FN=_authority_lifecycle VERB=up run_init ''; then fail "up ran without a realm"
else grep -qF 'realm-id is missing, so the authority is left alone' "$ROOT/out" && ! grep -q compose "$CALLS" && pass "up refuses, untouched, without the realm record" || fail "no realm: $(cat "$ROOT/out") calls: $(cat "$CALLS")"
fi
mv "$AUTH/public/realm-id.saved" "$AUTH/public/realm-id"
cp "$ROOT/.env" "$ROOT/.env.saved"; printf 'AUTHORITY_DIR=%s\n' rel >>"$ROOT/.env"; : >"$CALLS"
if FN=_authority_lifecycle VERB=stop run_init ''; then fail "stop accepted a relative AUTHORITY_DIR"
else grep -qF 'must be an absolute path' "$ROOT/out" && [[ ! -s "$CALLS" ]] && pass "the lifecycle refuses a relative AUTHORITY_DIR" || fail "relative: $(cat "$ROOT/out")"
fi
mv "$ROOT/.env.saved" "$ROOT/.env"

# ── renew, on the authority init just made ─────────────────────────────
jget() { python3 -c "import json,sys; print(json.load(open(sys.argv[1]))[sys.argv[2]])" "$1" "$2"; }
old_rsk="$(cat "$AUTH/secrets/rsk")"; old_tls="$(jget "$AUTH/ceremony/delegation-1.json" authority_tls_keys)"
: >"$CALLS"; docker_set SALT=2
if VERB=renew run_init ''; then pass "renew completes"; else fail "renew failed: $(cat "$ROOT/out")"; fi
d2="$AUTH/ceremony/delegation-2.json"
[[ "$(jget "$AUTH/public/trust" serial)" == 2 && "$(jget "$d2" min_valid_serial)" == 2 \
   && "$(jget "$d2" rsk_public_keys)" != "$(jget "$AUTH/ceremony/delegation-1.json" rsk_public_keys)" \
   && "$(jget "$d2" cak_public_keys)" != "$(jget "$AUTH/ceremony/delegation-1.json" cak_public_keys)" \
   && "$(jget "$d2" authority_tls_keys)" == "$old_tls" \
   && "$(jget "$d2" scope)" == "$(jget "$AUTH/ceremony/delegation-1.json" scope)" ]] \
  && pass "renew installs serial 2 with min_valid 2, a new RSK and CAK, and the same TLS keys and scope" \
  || fail "the renewed delegation is wrong: $(cat "$d2")"
[[ "$(jget "$d2" not_after_ms)" -gt "$na" && "$(cat "$AUTH/public/delegation-not-after-ms")" == "$(jget "$d2" not_after_ms)" ]] \
  && pass "renew moves the window and its health record forward" || fail "renew did not move the window"
grep -qF -- '--trust '"$AUTH/public/trust"' --spec '"$d2" "$CALLS" && grep -qF -- "--key-file $AUTH/root-keys/primary-a.pem --key-file $AUTH/root-keys/primary-b.pem --yes" "$CALLS" \
  && grep -qF 'up -d --no-deps --force-recreate authority' "$CALLS" \
  && pass "renew signs against the current trust with A and B, then recreates the authority" || fail "renew calls: $(cat "$CALLS")"
staged="$(find "$AUTH" -name '*.prev' -o -name '*.next' -o -name stage)"
[[ "$(mode "$AUTH/secrets/rsk")" == 400 && "$(mode "$AUTH/cp/cak")" == 400 && "$(mode "$AUTH/public/trust")" == 644 \
   && "$(cat "$AUTH/secrets/rsk")" != "$old_rsk" && -z "$staged" ]] && grep -qxF "chown 10006:10006 $AUTH/cp/cak.next" "$CALLS" \
  && pass "renew installs the new keys 0400 with their owners and leaves no staged files" || fail "renew files: $staged"

# The authority does not stay up: renew goes forward, since the authority may
# already hold delegation 3, and says so.
: >"$CALLS"; docker_set SALT=3
rsk2="$(cat "$AUTH/secrets/rsk")"
if RUNNING=0 VERB=renew run_init ''; then
  fail "renew accepted an authority that did not stay up"
elif grep -qF 'Do not put the old files back' "$ROOT/out" && [[ "$(jget "$AUTH/public/trust" serial)" == 3 ]] \
     && [[ "$(cat "$AUTH/secrets/rsk")" != "$rsk2" && -e "$AUTH/ceremony/delegation-3.signed" ]] \
     && [[ "$(cat "$AUTH/public/delegation-not-after-ms")" == "$(jget "$AUTH/ceremony/delegation-3.json" not_after_ms)" ]] \
     && [[ -z "$(find "$AUTH" -name '*.prev' -o -name '*.next' -o -name stage)" ]] \
     && [[ "$(grep -c 'force-recreate authority' "$CALLS")" == 1 ]]; then
  pass "renew keeps delegation 3 and its keys when the authority does not stay up, and does not roll back"
else
  fail "renew after a failed start: $(cat "$ROOT/out")"
fi
trust2="$(cat "$AUTH/public/trust")"
: >"$CALLS"
if TOOL_FAIL=sign-delegation VERB=renew run_init ''; then
  fail "renew accepted a failed signature"
elif [[ "$(cat "$AUTH/public/trust")" == "$trust2" && ! -e "$AUTH/ceremony/delegation-4.json" && ! -e "$AUTH/stage" ]] \
     && ! grep -q 'compose' "$CALLS"; then
  pass "a failed renewal signature changes nothing and restarts nothing"
else
  fail "a failed renewal signature: $(cat "$ROOT/out")"
fi
# The swap fails part way, after the live RSK has moved aside: the old trust
# and keys all come back, and nothing restarts.
rsk3="$(cat "$AUTH/secrets/rsk")"; cak3="$(cat "$AUTH/cp/cak")"
: >"$CALLS"; docker_set SALT=x
if SWAP_FAIL="$AUTH/secrets/rsk" VERB=renew run_init ''; then
  fail "renew accepted a failed swap"
elif [[ -e "$ROOT/swap-failed" && "$(cat "$AUTH/public/trust")" == "$trust2" && "$(cat "$AUTH/secrets/rsk")" == "$rsk3" \
       && "$(cat "$AUTH/cp/cak")" == "$cak3" && ! -e "$AUTH/ceremony/delegation-4.json" ]] \
     && [[ -z "$(find "$AUTH" -name '*.prev' -o -name '*.next' -o -name stage)" ]] && ! grep -q 'force-recreate' "$CALLS"; then
  pass "a swap that fails part way puts the old trust and keys back"
else
  fail "a failed swap: $(cat "$ROOT/out"); $(find "$AUTH/secrets" "$AUTH/public")"
fi
rm -f "$ROOT/swap-failed"
# A key left in the stage as a symlink: renew does not chown or chmod its
# target, and changes nothing.
printf 'outside\n' >"$ROOT/outside"; chmod 0644 "$ROOT/outside"
: >"$CALLS"; docker_set SALT=y; docker_set "LINK=cak:$ROOT/outside"
if VERB=renew run_init ''; then
  fail "renew installed a symlinked key"
elif grep -qF 'is not a regular file' "$ROOT/out" && [[ "$(mode "$ROOT/outside")" == 644 && "$(cat "$AUTH/cp/cak")" == "$cak3" ]] \
     && ! grep -qF "chown 10006:10006 $AUTH/cp/cak.next" "$CALLS" && [[ "$(cat "$AUTH/public/trust")" == "$trust2" ]]; then
  pass "renew refuses a key left in the stage as a symlink"
else
  fail "symlinked key: $(cat "$ROOT/out")"
fi
docker_set LINK=
: >"$CALLS"; docker_set SALT=4
record_before="$(cat "$AUTH/public/delegation-not-after-ms")"
if RECORD_FAIL=1 VERB=renew run_init ''; then
  if [[ "$(jget "$AUTH/public/trust" serial)" == 4 ]] && grep -qF 'Delegation 4 is installed' "$ROOT/out" \
     && grep -qF "expiry record was not updated; run: " "$ROOT/out" \
     && [[ "$(cat "$AUTH/public/delegation-not-after-ms")" == "$record_before" ]] \
     && [[ -z "$(find "$AUTH" -name '*.prev' -o -name '*.next' -o -name stage)" ]]; then
    pass "renew reports success and the manual fix when only the expiry record fails"
  else
    fail "record failure: $(cat "$ROOT/out")"
  fi
else
  fail "renew failed after installing because the expiry record failed: $(cat "$ROOT/out")"
fi
# --rotate-tls: the old and new TLS keys are listed together, the authority
# moves to the new one, and the next renew lists only that one.
tls_keys() { python3 -c "import json,sys; print(' '.join(json.load(open(sys.argv[1]))['authority_tls_keys']))" "$1"; }
tls4="$(tls_keys "$AUTH/ceremony/delegation-4.json")"; tls_key4="$(cat "$AUTH/secrets/tls-key")"
# A swap that fails on the TLS key puts every old file back.
: >"$CALLS"; docker_set SALT=z; trust4="$(cat "$AUTH/public/trust")"
if SWAP_FAIL="$AUTH/secrets/tls-key" VERB=renew run_init '' --rotate-tls; then
  fail "renew accepted a failed TLS key swap"
elif [[ "$(cat "$AUTH/public/trust")" == "$trust4" && "$(cat "$AUTH/secrets/tls-key")" == "$tls_key4" && ! -e "$AUTH/ceremony/delegation-5.json" ]] \
     && [[ -z "$(find "$AUTH" -name '*.prev' -o -name '*.next' -o -name stage)" ]]; then
  pass "a TLS key swap that fails puts the old trust and keys back"
else
  fail "a failed TLS key swap: $(cat "$ROOT/out")"
fi
rm -f "$ROOT/swap-failed"
: >"$CALLS"; docker_set SALT=5
if VERB=renew run_init '' --rotate-tls; then
  new_tls="$(tls_keys "$AUTH/ceremony/delegation-5.json")"
  if [[ "$new_tls" == "$tls4 "* && "$(wc -w <<<"$new_tls")" == 2 && "$(cat "$AUTH/secrets/tls-key")" != "$tls_key4" \
        && "$(mode "$AUTH/secrets/tls-key")" == 400 && -z "$(find "$AUTH" -name '*.prev' -o -name '*.next' -o -name stage)" ]] \
     && grep -qF 'now uses a new TLS key' "$ROOT/out" && grep -qF 'force-recreate authority' "$CALLS"; then
    pass "renew --rotate-tls lists the old and new TLS keys and installs the new key"
  else
    fail "rotate: $new_tls $(cat "$ROOT/out")"
  fi
else
  fail "renew --rotate-tls failed: $(cat "$ROOT/out")"
fi
: >"$CALLS"; docker_set SALT=6; tls_key5="$(cat "$AUTH/secrets/tls-key")"
if VERB=renew run_init ''; then
  [[ "$(tls_keys "$AUTH/ceremony/delegation-6.json")" == "${new_tls#* }" && "$(cat "$AUTH/secrets/tls-key")" == "$tls_key5" ]] \
    && ! grep -qF 'new TLS key' "$ROOT/out" \
    && pass "the next renew drops the outgoing TLS key and keeps the live one" || fail "after rotate: $(cat "$AUTH/ceremony/delegation-6.json")"
else
  fail "renew after a rotation failed: $(cat "$ROOT/out")"
fi
mkdir "$AUTH/stage"
VERB=renew run_init '' && fail "renew ran over a stage left behind" \
  || { grep -qF 'a renew is running' "$ROOT/out" && pass "renew refuses a stage left behind" || fail "stage refusal: $(cat "$ROOT/out")"; }
rmdir "$AUTH/stage"
NOT_ROOT=1 VERB=renew run_init '' && fail "renew ran as non-root" \
  || { grep -qF 'must run as root' "$ROOT/out" && pass "renew refuses non-root" || fail "root refusal: $(cat "$ROOT/out")"; }
# Renew refuses when the key in use is not the last one listed, when a
# FILE.prev is left from a failed rollback, when a TLS key was staged without
# the flag, and an empty argument.
: >"$CALLS"; saved_tls="$(cat "$AUTH/secrets/tls-key")"
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-384 -out "$ROOT/other-tls-key" 2>/dev/null
cp "$AUTH/secrets/tls-key" "$ROOT/live-tls-key"; cat "$ROOT/other-tls-key" >"$AUTH/secrets/tls-key"
VERB=renew run_init '' && fail "renew ran with a TLS key the delegation does not list" \
  || { grep -qF 'is not the last TLS key' "$ROOT/out" && [[ ! -e "$AUTH/stage" && ! -e "$AUTH/ceremony/delegation-7.json" ]] \
       && pass "renew refuses when the live TLS key is not the last one listed" || fail "tls drift: $(cat "$ROOT/out")"; }
cat "$ROOT/live-tls-key" >"$AUTH/secrets/tls-key"
printf x >"$AUTH/secrets/rsk.prev"
VERB=renew run_init '' && fail "renew ran over a .prev left behind" \
  || { grep -qF 'rsk.prev exists' "$ROOT/out" && [[ "$(cat "$AUTH/secrets/rsk.prev")" == x ]] \
       && pass "renew refuses over a FILE.prev left behind and keeps it" || fail "prev refusal: $(cat "$ROOT/out")"; }
rm -f "$AUTH/secrets/rsk.prev"
VERB=renew run_init '' '' && fail "renew took an empty argument" \
  || { grep -qF 'usage: concord-selfhost authority renew [--rotate-tls]' "$ROOT/out" && pass "renew refuses an empty argument" || fail "empty argument: $(cat "$ROOT/out")"; }
VERB=renew run_init '' --now && fail "renew took an argument" \
  || { grep -qF 'usage: concord-selfhost authority renew [--rotate-tls]' "$ROOT/out" && pass "renew refuses arguments" || fail "usage: $(cat "$ROOT/out")"; }
saved_root="$ROOT"
saved_auth="$AUTH"
fixture; VERB=renew run_init '' && fail "renew ran without an authority" \
  || { grep -qF 'run concord-selfhost authority init first' "$ROOT/out" && pass "renew refuses an install with no authority" || fail "no-authority refusal: $(cat "$ROOT/out")"; }
ROOT="$saved_root"
AUTH="$saved_auth"

# ── health: the 21-day warning ─────────────────────────────────────────
# health_rc NOT_AFTER — _authority_health's code with that record (none = no file).
# shellcheck disable=SC2034  # PROJECT_ROOT is read by the helper
health_rc() {
  rm -f "$AUTH/public/delegation-not-after-ms"
  [[ "$1" == none ]] || printf '%s\n' "$1" >"$AUTH/public/delegation-not-after-ms"
  ( PROJECT_ROOT="$ROOT"
    # shellcheck source=../selfhost-authority.sh
    source "$DEPLOY/selfhost-authority.sh"
    _authority_health ) >"$ROOT/out" 2>&1
  echo $?
}
now_ms=$(( $(date +%s) * 1000 ))
in30=$(( now_ms + 30 * 86400000 )) in20=$(( now_ms + 20 * 86400000 )) past=$(( now_ms - 1000 ))
[[ "$(health_rc "$in30")" == 0 ]] && pass "health passes with 30 days left" || fail "30 days: $(cat "$ROOT/out")"
[[ "$(health_rc "$in20")" == 2 ]] && grep -qF 'expires in 19 days' "$ROOT/out" \
  && pass "health warns with 20 days left" || fail "20 days: $(cat "$ROOT/out")"
[[ "$(health_rc "$past")" == 1 ]] && grep -qF 'has expired' "$ROOT/out" && pass "health fails once expired" || fail "expired: $(cat "$ROOT/out")"
# With the tool there, health rewrites a missing record (an authority set up
# before it existed); without it, a missing or unreadable record fails.
[[ "$(health_rc none)" == 0 && "$(cat "$AUTH/public/delegation-not-after-ms")" == "$(jget "$AUTH/ceremony/delegation-6.json" not_after_ms)" ]] \
  && pass "health backfills a missing expiry record from the trust" || fail "backfill: $(cat "$ROOT/out")"
chmod 0600 "$AUTH/tools/authority-ceremony"
[[ "$(health_rc garbage)" == 1 && "$(health_rc none)" == 1 ]] && pass "health fails on a missing or unreadable record" || fail "bad record: $(cat "$ROOT/out")"
chmod 0700 "$AUTH/tools/authority-ceremony"
# health runs the tool only when nobody but root can change it or a
# directory above it.
chmod 0702 "$AUTH/tools/authority-ceremony"
[[ "$(health_rc none)" == 1 ]] && pass "health does not run a tool that others can write" || fail "writable tool: $(cat "$ROOT/out")"
chmod 0700 "$AUTH/tools/authority-ceremony"; chmod o+w "$AUTH"
[[ "$(health_rc none)" == 1 ]] && pass "health does not run the tool from a directory that others can write" || fail "writable dir: $(cat "$ROOT/out")"
chmod o-w "$AUTH"
[[ "$(health_rc none)" == 0 ]] && pass "health backfills again once the directory is safe" || fail "safe again: $(cat "$ROOT/out")"
saved_root="$ROOT"; fixture
[[ "$(health_rc none 2>/dev/null)" == 3 ]] && pass "health has nothing to check before init" || fail "no authority: $(cat "$ROOT/out")"
ok=true
for v in up stop down; do FN=_authority_lifecycle VERB=$v run_init '' || ok=false; done
$ok && [[ ! -s "$CALLS" ]] && pass "up, stop and down leave the authority alone before init" || fail "lifecycle before init: $(cat "$CALLS" "$ROOT/out")"
ROOT="$saved_root"

# ── upgrade: an install that already has accounts ──────────────────────
fixture
if CP_ACCOUNTS=3 VERB=upgrade run_init "$PASS"$'\n'"$PASS" --export-dir "$EXPORT"; then
  pass "upgrade completes on an install with accounts"
else
  fail "upgrade failed: $(cat "$ROOT/out")"
fi
grep -qF 'replication-gate off' "$CALLS" && ! grep -qF seal-legacy "$CALLS" && grep -q 'compose.* up -d authority$' "$CALLS" \
  && grep -qF 'It is not sealed.' "$ROOT/out" && [[ -f "$AUTH/public/realm-id" ]] \
  && pass "upgrade turns the replication gate off, starts the authority unsealed and says so" || fail "upgrade: $(cat "$ROOT/out")"
# An account made between the first count and the seal: nothing is sealed.
fixture
CP_ACCOUNTS=0 CP_ACCOUNTS_LATER=1 run_init "$PASS"$'\n'"$PASS" --export-dir "$EXPORT" \
  && ! grep -qF seal-legacy "$CALLS" && grep -qF 'It is not sealed.' "$ROOT/out" \
  && pass "init does not seal when an account appears before the seal" || fail "late account: $(cat "$ROOT/out")"
# upgrade on an install with no accounts still leaves the seal to the integration.
fixture
CP_ACCOUNTS=0 VERB=upgrade run_init "$PASS"$'\n'"$PASS" --export-dir "$EXPORT" \
  && ! grep -qF seal-legacy "$CALLS" && grep -qF 'It is not sealed.' "$ROOT/out" \
  && pass "upgrade never seals, even with no accounts" || fail "upgrade, no accounts: $(cat "$ROOT/out")"
# A setup that stopped part way: up leaves it alone, stop and down still clear it.
fixture; docker_set COMPOSE_FAIL=replication-gate
if run_init "$PASS"$'\n'"$PASS" --export-dir "$EXPORT"; then fail "a failed replication-gate was accepted"
else [[ -f "$AUTH/public/trust" && ! -e "$AUTH/public/realm-id" ]] && pass "a setup that stops writes no realm-id" || fail "partial setup: $(ls "$AUTH/public")"
fi
docker_set COMPOSE_FAIL=; : >"$CALLS"
if FN=_authority_lifecycle VERB=up run_init ''; then fail "up took over a setup that did not finish"
else grep -qF 'did not finish' "$ROOT/out" && [[ ! -s "$CALLS" ]] && pass "up leaves a setup that did not finish alone" || fail "up on a partial setup: $(cat "$ROOT/out" "$CALLS")"
fi
FN=_authority_lifecycle VERB=down run_init '' && [[ "$(lifecycle_calls)" == '--profile authority-owner down' ]] \
  && pass "down still clears a setup that did not finish" || fail "down on a partial setup: $(cat "$ROOT/out")"

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
fixture; chmod 0777 "$ROOT"; refuse "an AUTHORITY_DIR under a directory others can write" "writable by other users" "$good" --export-dir "$EXPORT"
fixture; mkdir "$ROOT/real"; ln -s real "$ROOT/link"; AUTH="$ROOT/link/authority"
printf 'AUTHORITY_DIR=%s\n' "$AUTH" >"$ROOT/.env"; refuse "an AUTHORITY_DIR with a symlink in its path" "no symlink" "$good" --export-dir "$EXPORT"
fixture; refuse "a missing export directory" "is not a directory" "$good" --export-dir "$ROOT/nowhere"
fixture; : >"$EXPORT/recovery-a.p8"; refuse "an export directory holding a recovery key" "already holds" "$good" --export-dir "$EXPORT"
fixture; docker_set PG_IMAGE=0; refuse "no postgres image" "run concord-selfhost up first" "$good" --export-dir "$EXPORT"
fixture; docker_set SUBNETS=172.30.0.0/16; refuse "an overlapping subnet" "overlaps a Docker network" "$good" --export-dir "$EXPORT"
fixture 'AUTHORITY_SUBNET=172.30.53.1/29'; refuse "a subnet that is not a network address" "not a network address" "$good" --export-dir "$EXPORT"
fixture 'AUTHORITY_IP=172.30.53.1'; refuse "the gateway as AUTHORITY_IP" "other than its gateway" "$good" --export-dir "$EXPORT"
fixture 'AUTHORITY_IP=10.0.0.2'; refuse "an AUTHORITY_IP outside the subnet" "must be a host in" "$good" --export-dir "$EXPORT"
fixture "export AUTHORITY_SUBNET = '172.30.53.1/29'"; refuse "a .env line written the way compose also reads it" "not a network address" "$good" --export-dir "$EXPORT"
fixture; CP_ACCOUNTS=3 refuse "a control plane with accounts" "has 3 accounts; init only sets up an authority on an install with none: use concord-selfhost authority upgrade" "$good" --export-dir "$EXPORT"
fixture; CP_ACCOUNTS=fail refuse "a control plane that cannot be read" "cannot count the control plane's accounts" "$good" --export-dir "$EXPORT"
fixture; chmod 1777 "$EXPORT"; refuse "an export directory others can write" "writable by other users" "$good" --export-dir "$EXPORT"
fixture; refuse "passphrases that differ" "differ" "$PASS"$'\n'"$PASS!" --export-dir "$EXPORT"
fixture; refuse "a short passphrase" "at least 12" $'short\nshort' --export-dir "$EXPORT"
fixture; refuse "no passphrase" "no passphrase" '' --export-dir "$EXPORT"

# ── A failure part way says how to start again ─────────────────────────
fixture
if TOOL_FAIL=sign-delegation run_init "$good" --export-dir "$EXPORT"; then
  fail "a failed signature was accepted"
elif grep -q "sign-delegation failed" "$ROOT/out" && grep -q "remove $AUTH, any recovery-\*.p8 in $EXPORT" "$ROOT/out" && grep -q "authority-owner down -v" "$ROOT/out" \
     && ! grep -q 'compose.* up ' "$CALLS"; then
  pass "a failed signature stops before the database, naming what to remove"
else
  fail "a failed signature: $(cat "$ROOT/out")"
fi

echo "failures: $fails"
[[ $fails -eq 0 ]]
