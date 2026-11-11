#!/usr/bin/env bash
# Hermetic harness for the control-plane deploy path of concord-ctl.sh: the real
# controller runs in production mode against PATH stubs. Characterization cases
# first; rail cases append below and reuse run_ctl / transcript / stub_exit.
set -euo pipefail
fails=0
pass() { printf 'PASS: %s\n' "$1"; }
fail() { printf 'FAIL: %s\n' "$1" >&2; fails=$((fails + 1)); }

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CTL_SRC="$ROOT/infrastructure/deploy/concord-ctl.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

PROJECT="$TMP/project"
STUBS="$TMP/stubs"
CTL="$PROJECT/infrastructure/deploy/concord-ctl.sh"
mkdir -p "$PROJECT/infrastructure/deploy" "$STUBS"
for f in docker-compose.yml docker-compose.production.yml; do : >"$PROJECT/$f"; done

# Canonical writer pair (root .env + deploy.env.local): the four activity-history
# keys must agree in both; the redis/turn mode selectors are absent (= legacy).
cat >"$PROJECT/.env" <<'ENVFILE'
ACTIVITY_HISTORY_CLUSTER_ENABLED=false
CONTROL_PLANE_REPLICA_COUNT=1
ACTIVITY_HISTORY_OPERATOR_NAME=fixture-operator
ACTIVITY_HISTORY_PRIVACY_POLICY_URL=https://example.invalid/privacy
ENVFILE
cp "$PROJECT/.env" "$PROJECT/infrastructure/deploy/deploy.env.local"
: >"$TMP/calls"

# Fixture rewrites of the COPY (the shipped controller is never edited). Each
# hard-coded absolute host path is moved under $TMP:
#   1. DEPLOY_TRANSACTION_LOCK_PATH declaration -> $TMP lock file
#   2. the `[[ ! -d /var/lib/concord ]]` probe -> $TMP lock parent
lock_decl='DEPLOY_TRANSACTION_LOCK_PATH="/var/lib/concord/deploy-transaction.lock"'
lock_probe='if [[ ! -d /var/lib/concord ]]; then'
src="$(cat "$CTL_SRC")"
if [[ $(grep -Fxc "$lock_decl" <<<"$src") -ne 1 || $(grep -Fxc "$lock_probe" <<<"$src") -ne 1 ]]; then
  fail "controller fixture must rewrite exactly one lock declaration and probe"
  exit 1
fi
mkdir -p "$TMP/lock"
new_decl="DEPLOY_TRANSACTION_LOCK_PATH=\"$TMP/lock/deploy-transaction.lock\""
new_probe="if [[ ! -d $TMP/lock ]]; then"
# Replacement left unquoted: bash 3.2 keeps the quotes of a quoted replacement.
src="${src/"$lock_decl"/$new_decl}"
printf '%s\n' "${src/"$lock_probe"/$new_probe}" >"$CTL"
chmod +x "$CTL"

# Stubs: every tool logs "<tool> <argv>" to $TMP/calls. stub_exit/stub_out
# register rules in $TMP/rules.{exit,out}.<tool> as "<pattern>|<value>" (glob
# pattern on the joined argv, first match wins). Default: exit 0, no stdout.
# (The controller's flock/lockf lock itself is the host's real one, on a $TMP path.)
for tool in docker curl nginx systemctl flock nc sudo; do
  cat >"$STUBS/$tool" <<STUB
#!/usr/bin/env bash
tool="$tool"
argv="\$*"
printf '%s %s\n' "\$tool" "\$argv" >>"$TMP/calls"
if [[ -f "$TMP/rules.exit.\$tool" ]]; then
  while IFS='|' read -r pat code; do
    # shellcheck disable=SC2254
    case "\$argv" in \$pat) exit "\$code" ;; esac
  done <"$TMP/rules.exit.\$tool"
fi
if [[ -f "$TMP/rules.out.\$tool" ]]; then
  while IFS='|' read -r pat text; do
    # shellcheck disable=SC2254
    case "\$argv" in \$pat) printf '%b' "\$text"; break ;; esac
  done <"$TMP/rules.out.\$tool"
fi
exit 0
STUB
  chmod +x "$STUBS/$tool"
done

# stub_exit <tool> <glob-pattern-on-argv> <code>   (used by later rail cases)
# shellcheck disable=SC2329
stub_exit() { printf '%s|%s\n' "$2" "$3" >>"$TMP/rules.exit.$1"; }
# stub_out <tool> <glob-pattern-on-argv> <stdout-text (printf %b escapes)>
stub_out() { printf '%s|%s\n' "$2" "$3" >>"$TMP/rules.out.$1"; }
# transcript: ordered argv log of every stub call, fixture root shown as <P>
transcript() { sed "s#$PROJECT/infrastructure/deploy/../..#<P>#g" "$TMP/calls"; }

COMPOSE_MODEL='{"services":{"control-plane":{"image":"cp:fixture","depends_on":{},"environment":{"ACTIVITY_HISTORY_CLUSTER_ENABLED":"false","CONTROL_PLANE_REPLICA_COUNT":"1","ACTIVITY_HISTORY_OPERATOR_NAME":"fixture-operator","ACTIVITY_HISTORY_PRIVACY_POLICY_URL":"https://example.invalid/privacy"}},"nats":{}}}'
# reset_stubs: clear the log and rules, then restore the healthy defaults the
# controller's preflights need: a compose model, one running control-plane
# container, and a NATS container with no published host ports.
reset_stubs() {
  : >"$TMP/calls"
  rm -f "$TMP"/rules.exit.* "$TMP"/rules.out.*
  stub_out docker '*config --format json*' "$COMPOSE_MODEL\\n"
  stub_out docker '*ps --status running -q -- control-plane*' 'c0ffee000001\n'
  stub_out docker '*ps -q nats' 'ab12ab12ab12\n'
  stub_out docker 'inspect*PortBindings*' '{}|false\n'
}

rc=0
# run_ctl <args...>: real fixture controller, production mode, stubs first on PATH.
run_ctl() {
  rc=0
  PATH="$STUBS:$PATH" COMPOSE_OVERRIDE=docker-compose.production.yml \
    bash "$CTL" "$@" >"$TMP/out" 2>&1 </dev/null || rc=$?
}

# Golden transcript of `rebuild control-plane` with the switch unset. <P> is the
# fixture project root. Order matters: build, foreign-project guard (ps -q),
# stop with the 45s drain, daemon-wide drained check, then up --wait.
char_rebuild_control_plane_transcript() {
  reset_stubs
  run_ctl rebuild control-plane
  local want c='docker compose -f <P>/docker-compose.yml -f <P>/docker-compose.production.yml --profile services'
  want="$(cat <<GOLDEN
$c config --format json
docker ps --all --quiet --no-trunc --filter label=com.docker.compose.service=media-plane
$c config --format json
$c config --format json
$c ps --status running -q -- control-plane
$c config --format json
$c config --format json
$c up -d --no-deps --no-build --pull missing --wait --wait-timeout 180 nats
$c ps -q nats
docker inspect --format {{json .HostConfig.PortBindings}}|{{.HostConfig.PublishAllPorts}} ab12ab12ab12
$c build control-plane
docker ps --filter label=com.docker.compose.service=control-plane --format {{.ID}}
$c ps --status running -q -- control-plane
$c stop --timeout 45 control-plane
docker ps --filter label=com.docker.compose.service=control-plane --format {{.ID}}
$c up -d --no-build --no-deps --wait --wait-timeout 180 control-plane
GOLDEN
)"
  # Mutation that turns this red: change `stop --timeout 45` to `--timeout 30`
  # in activity_history_stop_control_plane (or drop the drained check).
  if [[ $rc -eq 0 && "$(transcript)" == "$want" ]]; then
    pass "char_rebuild_control_plane_transcript"
  else
    fail "char_rebuild_control_plane_transcript rc=$rc"
    diff <(printf '%s\n' "$want") <(transcript) >&2 || true
  fi
}

# Every control-plane probe in wait-healthy targets the home port on loopback.
char_wait_healthy_curl_argv() {
  reset_stubs
  run_ctl wait-healthy --timeout 4
  local cp_probes bad
  cp_probes="$(grep -c '^curl .*127\.0\.0\.1:8080' "$TMP/calls" || true)"
  # Any curl that is not the media probe (3000) must be the exact 8080 shape.
  bad="$(grep '^curl ' "$TMP/calls" | grep -v 'http://127\.0\.0\.1:3000/' \
    | grep -cv '^curl -sf http://127\.0\.0\.1:8080/[a-z]*$' || true)"
  # Mutation that turns this red: change CP_PROBE_BASE to port 8081.
  if [[ $rc -eq 0 && "$cp_probes" -ge 1 && "$bad" -eq 0 ]]; then
    pass "char_wait_healthy_curl_argv"
  else
    fail "char_wait_healthy_curl_argv rc=$rc probes=$cp_probes offending=$bad: $(grep '^curl ' "$TMP/calls" | tr '\n' ';')"
  fi
}

# sha256 of the shipped vhost rendered with the default HSTS policy, recorded
# from an unmodified-tree run.
NGINX_RENDER_SHA256='e6c88346ed3fdf12bf901b808e6244050591b79a63a1efd054199de51c5ae951'
HSTS_DEFAULT_VALUE='max-age=63072000; includeSubDomains; preload'
# extract_fn <name>: print the function declared as `    name() {` (4-space
# indent, inside the nginx-reload arm) through its closing `    }`.
extract_fn() {
  awk -v n="$1" '
    $0 == "    " n "() {" { on = 1 }
    on { print }
    on && $0 == "    }" { exit }
  ' "$CTL_SRC"
}
char_nginx_render_hash() {
  local fns got
  fns="$(extract_fn encode_hsts_for_nginx; extract_fn render_nginx_config)"
  if ! grep -qF "HSTS_DEFAULT='$HSTS_DEFAULT_VALUE'" "$CTL_SRC" || [[ -z "$fns" ]]; then
    fail "char_nginx_render_hash could not extract the renderer or the HSTS default"
    return
  fi
  mkdir -p "$TMP/render"
  got="$( (
    # Variables read by the extracted render_nginx_config.
    export NGINX_SRC="$ROOT/infrastructure/deploy/nginx/concordvoice.conf"
    export NGINX_RENDER_DIR="$TMP/render"
    export NGINX_RENDER="$TMP/render/concordvoice.conf"
    eval "$fns"
    render_nginx_config "$HSTS_DEFAULT_VALUE" || exit 1
    if command -v sha256sum >/dev/null 2>&1; then
      sha256sum "$NGINX_RENDER" | cut -d' ' -f1
    else
      shasum -a 256 "$NGINX_RENDER" | cut -d' ' -f1
    fi
  ) 2>&1 )" || got="render failed: $got"
  # Mutation that turns this red: change the add_header Strict-Transport-Security
  # emission in render_nginx_config (or edit nginx/concordvoice.conf).
  if [[ "$got" == "$NGINX_RENDER_SHA256" ]]; then
    pass "char_nginx_render_hash"
  else
    fail "char_nginx_render_hash got=$got want=$NGINX_RENDER_SHA256"
  fi
}

char_rebuild_control_plane_transcript
char_wait_healthy_curl_argv
char_nginx_render_hash
exit $((fails > 0))
