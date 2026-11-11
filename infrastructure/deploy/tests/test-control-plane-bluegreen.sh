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
# fixture_rewrite <old-line> <new-line>: the later rewrites of $src. The old
# line must occur exactly once, and as a whole line, or the run fails (and the
# cases that need the rewrite fail with it) rather than editing the wrong text.
fixture_rewrite() {
  if [[ $(grep -Fxc -- "$1" <<<"$src" || true) -ne 1 \
     || $(grep -Fc -- "$1" <<<"$src" || true) -ne 1 ]]; then
    fail "controller fixture must rewrite exactly one line: $1"
    return 0
  fi
  # bash >= 5.2 would expand `&` in the unquoted replacement to the match.
  shopt -u patsub_replacement 2>/dev/null || true
  src="${src/"$1"/$2}"
}
#   3. nginx-reload's managed lock-path admission -> the $TMP lock parent
#   4. CONTROL_PLANE_LIVE_VHOST (the live vhost the rail reads) -> $TMP/live
# shellcheck disable=SC2016
fixture_rewrite '     && [[ "$DEPLOY_TRANSACTION_LOCK_PATH" != /var/lib/concord/* ]]; then' \
  "     && [[ \"\$DEPLOY_TRANSACTION_LOCK_PATH\" != $TMP/lock/* ]]; then"
fixture_rewrite 'CONTROL_PLANE_LIVE_VHOST="/etc/nginx/sites-available/concordvoice"' \
  "CONTROL_PLANE_LIVE_VHOST=\"$TMP/live/concordvoice\""
#   5. the rail's managed-lock admission (rail-refused-no-state-dir) -> $TMP/lock
# shellcheck disable=SC2016
fixture_rewrite 'if (( CONTROL_PLANE_BLUEGREEN )) && [[ "$DEPLOY_TRANSACTION_LOCK_PATH" != /var/lib/concord/* ]]; then' \
  "if (( CONTROL_PLANE_BLUEGREEN )) && [[ \"\$DEPLOY_TRANSACTION_LOCK_PATH\" != $TMP/lock/* ]]; then"
# Rewrites 3 and 5 are the controller's two managed-lock admissions; a third
# would still name the real path in the fixture.
if grep -Fq '!= /var/lib/concord/*' <<<"$src"; then
  fail "controller fixture left a managed lock admission unrewritten"
fi
printf '%s\n' "${src/"$lock_probe"/$new_probe}" >"$CTL"
chmod +x "$CTL"

# Rail-on twin: its own project tree, so its cutover marker is separate. Its
# controller is $CTL unchanged; the rail is on through the switch in its
# deploy.env.local, written in the canonical writer's @Q form.
RAIL_PROJECT="$TMP/rail-project"
RAIL_CTL="$RAIL_PROJECT/infrastructure/deploy/concord-ctl.sh"
RAIL_ENV="$RAIL_PROJECT/infrastructure/deploy/deploy.env.local"
RAIL_MARKER="$RAIL_PROJECT/infrastructure/deploy/.control-plane-cutover.local"
PLAIN_ENV="$PROJECT/infrastructure/deploy/deploy.env.local"
PLAIN_MARKER="$PROJECT/infrastructure/deploy/.control-plane-cutover.local"
cp -R "$PROJECT" "$RAIL_PROJECT"
printf '%s\n' "CONTROL_PLANE_DEPLOY_PROFILE='control-plane-bluegreen'" >>"$RAIL_ENV"
# No-flock twin in the rail project: rewrite 6 points the rail's flock probe
# at a path that never exists.
NOFLOCK_CTL="$RAIL_PROJECT/infrastructure/deploy/concord-ctl-noflock.sh"
src="$(cat "$CTL")"
fixture_rewrite "  [[ -x /usr/bin/flock ]] || { echo 'control-plane-bluegreen: rail-refused-no-flock' >&2; exit 2; }" \
  "  [[ -x $TMP/no-flock ]] || { echo 'control-plane-bluegreen: rail-refused-no-flock' >&2; exit 2; }"
printf '%s\n' "$src" >"$NOFLOCK_CTL"
chmod +x "$NOFLOCK_CTL"
# Stop twins, one per project (rewrite 7): `stop` retires media and gates its
# compose mutation through voice-boot-containment.py, which admits only a real
# host's deploy lock and provisioning state. These copies make those two
# helpers no-ops; the rest of stop is the shipped code.
STOP_CTL="$PROJECT/infrastructure/deploy/concord-ctl-stop.sh"
RAIL_STOP_CTL="$RAIL_PROJECT/infrastructure/deploy/concord-ctl-stop.sh"
src="$(cat "$CTL")"
fixture_rewrite 'voice_enforcement_rollout_stop_media() {' 'voice_enforcement_rollout_stop_media() { return 0; }
voice_enforcement_rollout_stop_media_fixture_unused() {'
fixture_rewrite 'voice_enforcement_rollout_guard_mutation() {' 'voice_enforcement_rollout_guard_mutation() { return 0; }
voice_enforcement_rollout_guard_mutation_fixture_unused() {'
printf '%s\n' "$src" >"$STOP_CTL"
cp "$STOP_CTL" "$RAIL_STOP_CTL"
chmod +x "$STOP_CTL" "$RAIL_STOP_CTL"

# Stubs: every tool logs "<tool> <argv>" to $TMP/calls. stub_exit/stub_out/stub_err
# register rules in $TMP/rules.{exit,out,err}.<tool> as "<pattern>|<value>" (glob
# pattern on the joined argv, first match wins). Default: exit 0, no stdout.
# (The controller's flock/lockf lock itself is the host's real one, on a $TMP path.)
for tool in docker curl nginx systemctl flock nc sudo; do
  cat >"$STUBS/$tool" <<STUB
#!/usr/bin/env bash
tool="$tool"
argv="\$*"
printf '%s %s\n' "\$tool" "\$argv" >>"$TMP/calls"
if [[ -f "$TMP/rules.err.\$tool" ]]; then
  while IFS='|' read -r pat text; do
    # shellcheck disable=SC2254
    case "\$argv" in \$pat) printf '%b' "\$text" >&2; break ;; esac
  done <"$TMP/rules.err.\$tool"
fi
if [[ -f "$TMP/rules.reply.\$tool" ]]; then
  n=0
  while IFS='|' read -r pat code once text; do
    n=\$((n + 1))
    # shellcheck disable=SC2254
    case "\$argv" in
      \$pat)
        if [[ "\$once" == 1 ]]; then
          awk -v n="\$n" 'NR != n' "$TMP/rules.reply.\$tool" >"$TMP/rules.reply.\$tool.new"
          mv "$TMP/rules.reply.\$tool.new" "$TMP/rules.reply.\$tool"
        fi
        printf '%b' "\$text"
        exit "\$code" ;;
    esac
  done <"$TMP/rules.reply.\$tool"
fi
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
# stub_err <tool> <glob-pattern-on-argv> <stderr-text>: printed before any exit rule.
stub_err() { printf '%s|%s\n' "$2" "$3" >>"$TMP/rules.err.$1"; }
# stub_reply <tool> <glob-pattern-on-argv> <code> <stdout-text> [once]: stdout
# and exit code together, checked before the exit and out rules (first match
# wins). With `once` the rule is consumed by its first match.
stub_reply() { printf '%s|%s|%s|%s\n' "$2" "$3" "$([[ "${5:-}" == once ]] && echo 1 || echo 0)" "$4" >>"$TMP/rules.reply.$1"; }
# transcript: ordered argv log of every stub call, fixture root shown as <P>
transcript() { sed "s#$PROJECT/infrastructure/deploy/../..#<P>#g" "$TMP/calls"; }

COMPOSE_MODEL='{"services":{"control-plane":{"image":"cp:fixture","depends_on":{},"environment":{"ACTIVITY_HISTORY_CLUSTER_ENABLED":"false","CONTROL_PLANE_REPLICA_COUNT":"1","ACTIVITY_HISTORY_OPERATOR_NAME":"fixture-operator","ACTIVITY_HISTORY_PRIVACY_POLICY_URL":"https://example.invalid/privacy"}},"nats":{}}}'
# reset_stubs: clear the log and rules, then restore the healthy defaults the
# controller's preflights need: a compose model, one running control-plane
# container, and a NATS container with no published host ports.
reset_stubs() {
  : >"$TMP/calls"
  rm -f "$TMP"/rules.exit.* "$TMP"/rules.out.* "$TMP"/rules.err.* "$TMP"/rules.reply.*
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
NGINX_RENDER_SHA256='e6c88346ed3fdf12bf901b808e6244050591b79a63a1efd054199de51c5ae951'  # pragma: allowlist secret -- sha256 of a rendered fixture, not a secret
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

# Real-daemon contract (the only case that calls the real docker, never the
# stub). The rail starts green as a `compose run` one-off of the control-plane
# service and depends on how Compose labels, lists, stops and networks a
# one-off. If a Compose release changes any of this, this case goes red before
# a deploy does.
compose_oneoff_contract() {
  if ! docker info >/dev/null 2>&1; then
    if [[ -n "${CI:-}" ]]; then
      fail "compose_oneoff_contract needs a docker daemon under CI"
    else
      printf 'SKIP: compose_oneoff_contract (no docker daemon)\n'
    fi
    return
  fi
  local d="$TMP/oneoff" proj="cpbg$$" green="cpbg$$-green" errs='' gid='' got
  local p1=$((20000 + $$ % 20000)) p2
  p2=$((p1 + 1))
  local -a dc
  mkdir -p "$d"
  cat >"$d/base.yml" <<YML
services:
  cp:
    image: busybox:1.36
    command: ["sh", "-c", "mkdir -p /tmp/w && echo ok >/tmp/w/index.html && httpd -f -p 8080 -h /tmp/w"]
    healthcheck:
      test: ["CMD", "wget", "--spider", "-q", "http://127.0.0.1:8080/"]
      interval: 1s
    ports: ["127.0.0.1:$p1:8080"]
    networks: [svc]
networks:
  svc: {}
YML
  # Second layer, standing in for a production hardening file.
  cat >"$d/prod.yml" <<'YML'
services:
  cp:
    read_only: true
    tmpfs: ["/tmp:size=8M"]
YML
  dc=(docker compose -p "$proj" -f "$d/base.yml" -f "$d/prod.yml")

  "${dc[@]}" up -d --wait cp >"$TMP/oneoff.log" 2>&1 || errs+=' home-up-failed'
  # The exact flag set the rail uses for green. Mutation that turns this case
  # red: drop --use-aliases here (the alias assertion fails).
  gid="$("${dc[@]}" run --detach --no-deps --pull never --use-aliases --name "$green" \
    -p "127.0.0.1:$p2:8080" -e MODE=verify cp 2>>"$TMP/oneoff.log")" || errs+=' run-failed'
  [[ "$gid" =~ ^[a-f0-9]{64}$ ]] || errs+=' run-did-not-print-an-id'

  got="$(docker inspect --format '{{index .Config.Labels "com.docker.compose.service"}}|{{index .Config.Labels "com.docker.compose.oneoff"}}' "$green" 2>&1)" || true
  [[ "$got" == 'cp|True' ]] || errs+=" labels=$got"
  # The daemon-wide filter the drained check uses sees the one-off.
  docker ps --no-trunc --filter label=com.docker.compose.service=cp --format '{{.ID}}' \
    | grep -qFx "$gid" || errs+=' label-filter-misses-oneoff'
  # The own-set query the foreign-project guard uses lists the one-off.
  "${dc[@]}" ps --status running -q -- cp 2>/dev/null | grep -qFx "$gid" || errs+=' compose-ps-misses-oneoff'
  got="$(docker inspect --format '{{.HostConfig.ReadonlyRootfs}}|{{.HostConfig.RestartPolicy.Name}}|{{json .HostConfig.PortBindings}}' "$green" 2>&1)" || true
  [[ "$got" == "true|no|{\"8080/tcp\":[{\"HostIp\":\"127.0.0.1\",\"HostPort\":\"$p2\"}]}" ]] || errs+=" hostconfig=$got"
  got="$(docker inspect --format '{{range .NetworkSettings.Networks}}{{json .Aliases}}{{end}}' "$green" 2>&1)" || true
  [[ "$got" == *'"cp"'* ]] || errs+=" aliases=$got"

  # Stopping the service stops home only; home can then start beside the one-off.
  "${dc[@]}" stop -t 1 cp >>"$TMP/oneoff.log" 2>&1 || errs+=' stop-failed'
  got="$("${dc[@]}" ps --status running -q -- cp 2>/dev/null | tr '\n' ' ')"
  [[ "$got" == "$gid " ]] || errs+=" after-stop-running=$got"
  "${dc[@]}" up -d --no-deps --wait cp >>"$TMP/oneoff.log" 2>&1 || errs+=' home-restart-failed'
  got="$("${dc[@]}" ps --status running -q -- cp 2>/dev/null | wc -l | tr -d ' ')"
  [[ "$got" == 2 ]] || errs+=" after-restart-running=$got"

  docker rm -f "$green" >/dev/null 2>&1 || true
  "${dc[@]}" down -t 0 -v --remove-orphans >>"$TMP/oneoff.log" 2>&1 || true
  if [[ -z "$errs" ]]; then
    pass "compose_oneoff_contract"
  else
    fail "compose_oneoff_contract:$errs"
    tail -20 "$TMP/oneoff.log" >&2 || true
  fi
}

# ---------------------------------------------------------------------------
# Control-plane rail: nginx renderer, live-port reader, nested-flip re-entry.
# The shipped controller has the rail off; these cases turn it on only inside
# their own subshell (extracted functions) or in $RAIL_CTL.
VHOST_SRC="$ROOT/infrastructure/deploy/nginx/concordvoice.conf"
FLIP_MARKER='--concord-ctl-control-plane-flip-v1'
FLIP_REJECTED='  ✗ rejected invalid internal control-plane flip re-entry'
LOCK="$TMP/lock/deploy-transaction.lock"

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

# extract_top_fn <name>: print the column-0 function `name() {` through `}`.
extract_top_fn() {
  awk -v n="$1" '
    $0 == n "() {" { on = 1 }
    on { print }
    on && $0 == "}" { exit }
  ' "$CTL_SRC"
}

# render_with <rail 0|1> <source> <render-dir> [<port>...]: the extracted
# renderer with the default HSTS policy; returns its status.
render_with() {
  local fns
  # Variables read by the extracted render_nginx_config (dynamic scope).
  # shellcheck disable=SC2034
  local NGINX_SRC="$2" NGINX_RENDER_DIR="$3" NGINX_RENDER="$3/concordvoice.conf" CONTROL_PLANE_BLUEGREEN="$1"
  shift 3
  fns="$(extract_fn encode_hsts_for_nginx; extract_fn render_nginx_config)"
  [[ -n "$fns" ]] || return 125
  mkdir -p "$NGINX_RENDER_DIR"
  (
    eval "$fns"
    render_nginx_config "$HSTS_DEFAULT_VALUE" "$@"
  )
}

render_rail_on_8080_equals_char_hash() {
  local r=0 got=''
  render_with 1 "$VHOST_SRC" "$TMP/r8080" 8080 >/dev/null 2>&1 || r=$?
  [[ $r -ne 0 ]] || got="$(sha256_of "$TMP/r8080/concordvoice.conf")"
  # Mutation that turns this red: change the format of the rewritten server
  # line in render_nginx_config's rail branch (any byte).
  if [[ $r -eq 0 && "$got" == "$NGINX_RENDER_SHA256" ]]; then
    pass "render_rail_on_8080_equals_char_hash"
  else
    fail "render_rail_on_8080_equals_char_hash rc=$r got=$got"
  fi
}

render_rail_on_8081_changes_only_the_server_line() {
  local r=0 got want
  render_with 1 "$VHOST_SRC" "$TMP/r8080" 8080 >/dev/null 2>&1 || r=$?
  render_with 1 "$VHOST_SRC" "$TMP/r8081" 8081 >/dev/null 2>&1 || r=$?
  got="$(diff "$TMP/r8080/concordvoice.conf" "$TMP/r8081/concordvoice.conf" 2>&1 | grep '^[<>]' || true)"
  want="$(printf '%s\n' '<     server 127.0.0.1:8080;' '>     server 127.0.0.1:8081;')"
  # Mutation that turns this red: print the source server line unchanged
  # instead of the requested port.
  if [[ $r -eq 0 && "$got" == "$want" ]]; then
    pass "render_rail_on_8081_changes_only_the_server_line"
  else
    fail "render_rail_on_8081_changes_only_the_server_line rc=$r diff=$(tr '\n' ';' <<<"$got")"
  fi
}

render_rail_off_ignores_port_argument() {
  local r=0 a='' b=''
  render_with 0 "$VHOST_SRC" "$TMP/roff1" 8081 >/dev/null 2>&1 || r=$?
  render_with 0 "$VHOST_SRC" "$TMP/roff2" 9999 >/dev/null 2>&1 || r=$?
  if [[ $r -eq 0 ]]; then
    a="$(sha256_of "$TMP/roff1/concordvoice.conf")"
    b="$(sha256_of "$TMP/roff2/concordvoice.conf")"
  fi
  # Mutation that turns this red: drop the rail guard around the port check or
  # the server-line rewrite in render_nginx_config.
  if [[ $r -eq 0 && "$a" == "$NGINX_RENDER_SHA256" && "$b" == "$NGINX_RENDER_SHA256" ]]; then
    pass "render_rail_off_ignores_port_argument"
  else
    fail "render_rail_off_ignores_port_argument rc=$r 8081=$a 9999=$b"
  fi
}

# render_rejects <case> <port> <awk-program>: render a $TMP copy of the shipped
# vhost edited by <awk-program> with the rail on, into a directory that holds a
# previous render. Requires status 1, no temp file left, and the previous
# render byte-identical.
render_rejects() {
  local name="$1" port="$2" dir="$TMP/$1" r=0 left
  mkdir -p "$dir"
  awk "$3" "$VHOST_SRC" >"$dir/source.conf"
  printf 'previous render\n' >"$dir/concordvoice.conf"
  render_with 1 "$dir/source.conf" "$dir" "$port" >/dev/null 2>&1 || r=$?
  left="$(find "$dir" -name 'concordvoice.conf.tmp.*' | wc -l | tr -d ' ')"
  if [[ $r -eq 1 && "$left" == 0 && "$(cat "$dir/concordvoice.conf")" == 'previous render' ]]; then
    pass "$name"
  else
    fail "$name rc=$r temp-files=$left target=$(head -c 40 "$dir/concordvoice.conf" | tr '\n' ' ')"
  fi
}
# Awk programs below are single-quoted on purpose ($0 is awk's).
# shellcheck disable=SC2016
AWK_DROP_CP_BLOCK='$0 == "upstream control_plane {" { skip = 1 } skip { if ($0 == "}") skip = 0; next }'
# shellcheck disable=SC2016
render_rejections() {
  # Mutation: drop `cp_blocks == 1` from the rail invariant.
  render_rejects render_rejects_zero_blocks 8081 "$AWK_DROP_CP_BLOCK { print }"
  # Mutation: relax `cp_blocks == 1` to `cp_blocks >= 1`.
  render_rejects render_rejects_two_blocks 8081 '{ print }
    $0 == "upstream control_plane {" { on = 1 } on { buf = buf $0 "\n" }
    on && $0 == "}" { on = 0; printf "%s", buf }'
  # Mutation: relax `cp_servers == 1` to `cp_servers >= 1`.
  render_rejects render_rejects_two_server_lines 8081 '{ print }
    $0 == "    server 127.0.0.1:8080;" { print }'
  # Mutation: drop `!cp_bad` from the rail invariant.
  render_rejects render_rejects_backup_server_line 8081 '{ print }
    $0 == "    server 127.0.0.1:8080;" { print "    server 127.0.0.1:8081 backup;" }'
  # Mutation: widen the canonical server pattern's 127\.0\.0\.1 to any address.
  render_rejects render_rejects_other_address 8081 '
    $0 == "    server 127.0.0.1:8080;" { $0 = "    server 10.0.0.1:8080;" } { print }'
  # Mutation: match the canonical server line with `^[[:space:]]+server`.
  render_rejects render_rejects_tab_indented_server 8081 '
    $0 == "    server 127.0.0.1:8080;" { $0 = "\tserver 127.0.0.1:8080;" } { print }'
  # Mutation: drop `!in_cp` (block closed) from the rail invariant.
  render_rejects render_rejects_unclosed_block 8081 "$AWK_DROP_CP_BLOCK { print }
    END { print \"upstream control_plane {\"; print \"    server 127.0.0.1:8080;\" }"
  # Mutation: widen the canonical server pattern's (8080|8081) to [0-9]+.
  render_rejects render_rejects_source_port_9999 8081 '
    $0 == "    server 127.0.0.1:8080;" { $0 = "    server 127.0.0.1:9999;" } { print }'
  # Mutation: drop the requested-port check at the top of render_nginx_config.
  render_rejects render_rejects_port_argument_9999 9999 '{ print }'
  render_rejects render_rejects_missing_port_argument '' '{ print }'
}

# live_port <path>: the extracted reader against <path>.
live_port() {
  local fn
  fn="$(extract_top_fn control_plane_bluegreen_live_port)"
  [[ -n "$fn" ]] || return 125
  (
    export CONTROL_PLANE_LIVE_VHOST="$1"
    eval "$fn"
    control_plane_bluegreen_live_port
  )
}
# expect_live_port <case> <path> <port|refuse>: refuse means status 1, no stdout.
expect_live_port() {
  local out r=0
  out="$(live_port "$2" 2>/dev/null)" || r=$?
  if [[ "$3" == refuse && $r -eq 1 && -z "$out" ]] \
     || [[ "$3" != refuse && $r -eq 0 && "$out" == "$3" ]]; then
    pass "$1"
  else
    fail "$1 rc=$r out=$(tr '\n' ';' <<<"$out") want=$3"
  fi
}
# live_fixture <name> <awk-args...>: a $TMP copy of the shipped vhost.
live_fixture() {
  mkdir -p "$TMP/live-cases"
  awk "${@:2}" "$VHOST_SRC" >"$TMP/live-cases/$1"
  printf '%s' "$TMP/live-cases/$1"
}
# shellcheck disable=SC2016
AWK_CP_SERVER_TO='$0 == "    server 127.0.0.1:8080;" { $0 = "    server 127.0.0.1:" port ";" } { print }'
# shellcheck disable=SC2016
live_port_cases() {
  # Mutation: make the reader's missing-file branch `return 1`.
  mkdir -p "$TMP/live-none"
  expect_live_port live_port_missing_file_is_8080 "$TMP/live-none/concordvoice" 8080
  # Mutation: drop the parent test from the missing-file branch (a vhost whose
  # directory is gone reads as 8080).
  expect_live_port live_port_missing_parent_refuses "$TMP/live-gone/concordvoice" refuse
  # Mutation: print a fixed 8080 instead of the captured port.
  expect_live_port live_port_8081 "$(live_fixture p8081 -v port=8081 "$AWK_CP_SERVER_TO")" 8081
  # Mutation: widen the reader's (8080|8081) to [0-9]+.
  expect_live_port live_port_9999_refuses "$(live_fixture p9999 -v port=9999 "$AWK_CP_SERVER_TO")" refuse
  # Mutation: treat a file with no control_plane block as the home port.
  printf 'not an nginx vhost\n' >"$TMP/live-garbage"
  expect_live_port live_port_garbage_refuses "$TMP/live-garbage" refuse
  # Mutation: relax the reader's `blocks == 1` to `blocks >= 1`.
  expect_live_port live_port_two_blocks_refuses "$(live_fixture two '{ print }
    $0 == "upstream control_plane {" { on = 1 } on { buf = buf $0 "\n" }
    on && $0 == "}" { on = 0; printf "%s", buf }')" refuse
  # Mutation: strip a trailing CR from each line before matching.
  expect_live_port live_port_crlf_refuses "$(live_fixture crlf '{ printf "%s\r\n", $0 }')" refuse
  # Mutation: trim trailing whitespace before matching the server line.
  expect_live_port live_port_trailing_space_refuses "$(live_fixture trail '
    $0 == "    server 127.0.0.1:8080;" { $0 = $0 " " } { print }')" refuse
  # Mutation: test the missing-file branch with `! -f` (a directory reads as
  # 8080); as non-root also `! -r` (an unreadable file reads as 8080).
  # (Root reads a mode-000 file, so that half runs only as non-root.)
  local out r=0 bad=''
  mkdir -p "$TMP/live-dir/concordvoice"
  out="$(live_port "$TMP/live-dir/concordvoice" 2>/dev/null)" || r=$?
  [[ $r -eq 1 && -z "$out" ]] || bad+=" directory:rc=$r,out=$out"
  if [[ "$(id -u)" != 0 ]]; then
    cp "$VHOST_SRC" "$TMP/live-unreadable"
    chmod 000 "$TMP/live-unreadable"
    r=0
    out="$(live_port "$TMP/live-unreadable" 2>/dev/null)" || r=$?
    chmod 600 "$TMP/live-unreadable"
    [[ $r -eq 1 && -z "$out" ]] || bad+=" mode-000:rc=$r,out=$out"
  fi
  if [[ -z "$bad" ]]; then pass "live_port_unreadable_refuses"; else fail "live_port_unreadable_refuses:$bad"; fi
  # A vhost on 8081 in a directory the controller cannot search: neither -e nor
  # -L sees it. Mutation: drop the `-x` half of the parent test (it reads as
  # 8080 and recovery would reap a serving green).
  # Root searches a mode-0600 directory, so this cannot be simulated as root.
  if [[ "$(id -u)" == 0 ]]; then
    printf 'SKIP: live_port_unsearchable_parent_refuses (running as root; an unsearchable directory cannot be simulated)\n'
    return 0
  fi
  mkdir -p "$TMP/live-unsearchable"
  awk -v port=8081 "$AWK_CP_SERVER_TO" "$VHOST_SRC" >"$TMP/live-unsearchable/concordvoice"
  chmod 600 "$TMP/live-unsearchable"
  r=0
  out="$(live_port "$TMP/live-unsearchable/concordvoice" 2>/dev/null)" || r=$?
  chmod 700 "$TMP/live-unsearchable"
  if [[ $r -eq 1 && -z "$out" ]]; then
    pass "live_port_unsearchable_parent_refuses"
  else
    fail "live_port_unsearchable_parent_refuses rc=$r out=$(tr '\n' ';' <<<"$out") want=refuse"
  fi
}

live_port_roundtrip() {
  local r=0
  render_with 1 "$VHOST_SRC" "$TMP/rtrip" 8081 >/dev/null 2>&1 || r=$?
  if [[ $r -ne 0 ]]; then
    fail "live_port_roundtrip render rc=$r"
    return
  fi
  # Mutation: change the renderer's or the reader's server-line format.
  expect_live_port live_port_roundtrip "$TMP/rtrip/concordvoice.conf" 8081
}

live_vhost_literal_matches_nginx_dst() {
  local a b
  a="$(sed -n 's/^CONTROL_PLANE_LIVE_VHOST="\(.*\)"$/\1/p' "$CTL_SRC")"
  b="$(sed -n 's/^    NGINX_DST="\(.*\)"$/\1/p' "$CTL_SRC")"
  # Mutation: change either literal (the rail would read a file nginx-reload
  # never writes).
  if [[ -n "$a" && "$a" == "$b" && "$a" != *$'\n'* ]]; then
    pass "live_vhost_literal_matches_nginx_dst"
  else
    fail "live_vhost_literal_matches_nginx_dst live=$a nginx_dst=$b"
  fi
}

# run_ctl_at <ctl> <args...>: run_ctl against another fixture controller.
# Descriptors the caller opened (fd 9) are inherited.
run_ctl_at() {
  local ctl="$1"
  shift
  rc=0
  PATH="$STUBS:$PATH" COMPOSE_OVERRIDE=docker-compose.production.yml \
    bash "$ctl" "$@" >"$TMP/out" 2>&1 </dev/null || rc=$?
}
# need_flock <case>: the nested-flip cases need the real util-linux flock.
need_flock() {
  [[ -x /usr/bin/flock ]] && return 0
  if [[ -n "${CI:-}" ]]; then
    fail "$1 needs /usr/bin/flock under CI"
  else
    printf 'SKIP: %s (no /usr/bin/flock)\n' "$1"
  fi
  return 1
}
# flip_run <fd-mode> <ctl> <args...>: run a fixture controller as a nested
# child with descriptor 9 prepared by <fd-mode>:
#   none     the lock held on fd 8, fd 9 closed
#   other    the lock held on fd 8, fd 9 open on an unrelated file
#   unheld   fd 9 open on the lock file, nobody holds the lock
#   foreign  fd 9 open on the lock file, a background process holds the lock
#   genuine  fd 9 open on the lock file and holding it (the cutover parent)
flip_run() {
  local mode="$1" ctl="$2" holder=''
  shift 2
  rm -f "$TMP/flip.rc"
  if [[ "$mode" == foreign ]]; then
    rm -f "$TMP/hold.ready" "$TMP/hold.release"
    mkfifo "$TMP/hold.ready" "$TMP/hold.release"
    # shellcheck disable=SC2016
    /usr/bin/flock "$LOCK" bash -c 'printf "held\n" >"$1"; read -r _ <"$2"' _ \
      "$TMP/hold.ready" "$TMP/hold.release" &
    holder=$!
    read -r _ <"$TMP/hold.ready"   # returns once the holder has the lock
  fi
  (
    exec 9>&-
    case "$mode" in
      none) exec 8>"$LOCK"; /usr/bin/flock -n 8 || exit 1 ;;
      other) exec 8>"$LOCK" 9>"$TMP/not-the-lock"; /usr/bin/flock -n 8 || exit 1 ;;
      unheld|foreign) exec 9>"$LOCK" ;;
      genuine) exec 9>"$LOCK"; /usr/bin/flock -n 9 || exit 1 ;;
    esac
    run_ctl_at "$ctl" "$@"
    printf '%s\n' "$rc" >"$TMP/flip.rc"
  ) || true
  if [[ -n "$holder" ]]; then
    printf 'release\n' >"$TMP/hold.release"
    wait "$holder" || true
  fi
  rc="$(cat "$TMP/flip.rc" 2>/dev/null || printf 'setup-failed')"
}
expect_flip_rejected() {
  if [[ "$rc" == 1 ]] && grep -qFx -- "$FLIP_REJECTED" "$TMP/out"; then
    pass "$1"
  else
    fail "$1 rc=$rc: $(head -c 400 "$TMP/out" | tr '\n' ';')"
  fi
}

reentry_rejects_without_fd9() {
  need_flock reentry_rejects_without_fd9 || return 0
  reset_stubs
  : >"$RAIL_MARKER"
  flip_run none "$RAIL_CTL" "$FLIP_MARKER" nginx-reload --control-plane-upstream 8081
  # Mutation: invert the normalization guard (`if validate_control_plane_flip_
  # reentry "$@"; then`). No single check removal turns this red: with fd 9
  # closed the -ef test and the fd-9 flock each refuse on their own.
  expect_flip_rejected reentry_rejects_without_fd9
}
reentry_rejects_fd9_on_other_file() {
  need_flock reentry_rejects_fd9_on_other_file || return 0
  reset_stubs
  : >"$RAIL_MARKER"
  flip_run other "$RAIL_CTL" "$FLIP_MARKER" nginx-reload --control-plane-upstream 8081
  # Mutation: delete the `/dev/fd/9 -ef` check (flock -n 9 on the other file succeeds).
  expect_flip_rejected reentry_rejects_fd9_on_other_file
}
reentry_rejects_unheld_lock() {
  need_flock reentry_rejects_unheld_lock || return 0
  reset_stubs
  : >"$RAIL_MARKER"
  flip_run unheld "$RAIL_CTL" "$FLIP_MARKER" nginx-reload --control-plane-upstream 8081
  # Mutation: delete the fresh `flock -n <path>` must-fail check (flock -n 9
  # then simply takes the free lock).
  expect_flip_rejected reentry_rejects_unheld_lock
}
reentry_rejects_foreign_holder() {
  need_flock reentry_rejects_foreign_holder || return 0
  reset_stubs
  : >"$RAIL_MARKER"
  flip_run foreign "$RAIL_CTL" "$FLIP_MARKER" nginx-reload --control-plane-upstream 8081
  # Mutation: delete the final `/usr/bin/flock -n 9` (same open file description) check.
  expect_flip_rejected reentry_rejects_foreign_holder
}
reentry_rejects_inexact_argv() {
  need_flock reentry_rejects_inexact_argv || return 0
  local spec bad=''
  local -a v
  reset_stubs
  : >"$RAIL_MARKER"
  # Mutations: `$# -eq 4` -> `$# -ge 4` (trailing argument); drop the port
  # alternation (9999, 08081); drop the verb or flag comparison (the others).
  for spec in 'nginx-reload --control-plane-upstream 8081 extra' \
    'nginx-reload --control-plane-upstream 9999' 'nginx-reload --control-plane-upstream 08081' \
    'nginx-reload --control-plane-upstream' 'nginx-reload --control-plane-upstream=8081' \
    'nginx-reload --stage-bootstrap 8081' 'nginx-reload' 'sudoers-sync --control-plane-upstream 8081'; do
    read -r -a v <<<"$spec"
    flip_run genuine "$RAIL_CTL" "$FLIP_MARKER" "${v[@]}"
    if [[ "$rc" != 1 ]] || ! grep -qFx -- "$FLIP_REJECTED" "$TMP/out"; then
      bad+=" [$spec rc=$rc]"
    fi
  done
  if [[ -z "$bad" ]]; then pass "reentry_rejects_inexact_argv"; else fail "reentry_rejects_inexact_argv:$bad"; fi
}
reentry_genuine_accepted() {
  need_flock reentry_genuine_accepted || return 0
  reset_stubs
  : >"$RAIL_MARKER"
  flip_run genuine "$RAIL_CTL" "$FLIP_MARKER" nginx-reload --control-plane-upstream 8081
  # Under the stubs the child passes normalization, skips the lock (a re-acquire
  # would wait on the parent's lock and report it busy), passes the nginx-reload
  # argv check, and stops at managed_core_identity_admit: the fixture carries
  # no production identity, so the render is not reached.
  # Mutation: drop the flip flag from acquire_deploy_transaction_lock's early
  # return (busy), or from the nginx-reload argv check (usage error).
  if [[ "$rc" == 1 ]] && grep -qF 'managed-core: canonical deployment identity is missing or ambiguous' "$TMP/out" \
     && ! grep -qF -e "$FLIP_REJECTED" -e 'deploy transaction lock is busy' -e 'Usage:' "$TMP/out"; then
    pass "reentry_genuine_accepted"
  else
    fail "reentry_genuine_accepted rc=$rc: $(head -c 400 "$TMP/out" | tr '\n' ';')"
  fi
}
flip_form_refused_when_rail_off() {
  need_flock flip_form_refused_when_rail_off || return 0
  reset_stubs
  : >"$PLAIN_MARKER"
  flip_run genuine "$CTL" "$FLIP_MARKER" nginx-reload --control-plane-upstream 8081
  rm -f "$PLAIN_MARKER"
  # Mutation: delete the rail check in validate_control_plane_flip_reentry.
  expect_flip_rejected flip_form_refused_when_rail_off
}
flip_form_refused_without_marker_file() {
  need_flock flip_form_refused_without_marker_file || return 0
  reset_stubs
  rm -f "$RAIL_MARKER"
  flip_run genuine "$RAIL_CTL" "$FLIP_MARKER" nginx-reload --control-plane-upstream 8081
  # Mutation: delete the cutover-marker check in validate_control_plane_flip_reentry.
  expect_flip_rejected flip_form_refused_without_marker_file
}
flip_argument_without_reentry_is_usage_error() {
  need_flock flip_argument_without_reentry_is_usage_error || return 0
  reset_stubs
  : >"$RAIL_MARKER"
  run_ctl_at "$RAIL_CTL" nginx-reload --control-plane-upstream 8081
  # Mutation: accept the three-argument form without the flip flag (for
  # example whenever the rail is on).
  if [[ "$rc" == 2 ]] && grep -qFx 'Usage: concord-ctl.sh nginx-reload' "$TMP/out"; then
    pass "flip_argument_without_reentry_is_usage_error"
  else
    fail "flip_argument_without_reentry_is_usage_error rc=$rc: $(head -c 400 "$TMP/out" | tr '\n' ';')"
  fi
}

# ---------------------------------------------------------------------------
# Control-plane rail switch, its exit-2 refusals and the cutover-marker guard.
# The rail is on only through CONTROL_PLANE_DEPLOY_PROFILE in the deploy env
# file, and only with flock: on a host without /usr/bin/flock every rail-on
# controller run refuses by design, so those cases SKIP there (fail under CI).
T0_COMPOSE='docker compose -f <P>/docker-compose.yml -f <P>/docker-compose.production.yml --profile services'
T0_PROBE="rc=0 flag=0 profile=control-plane-single cmd=$T0_COMPOSE"
SWITCH_INVALID='control-plane-bluegreen: invalid control-plane deployment profile'
RAIL_OFF_MARKER='control-plane-bluegreen: rail-off-with-cutover-marker'
CUTOVER_INCOMPLETE='control-plane-bluegreen: cutover-incomplete'
GREEN=concordvoice-control-plane-green
# sha256 of the two migration guards, extracted by name from 778d3b7e.
GUARD_SHA256_REFRESH_DAEMON_IDS='78761343e1e4521bf3728bdd04cee884a889e910b1d47b64929f2541703248a2'  # pragma: allowlist secret -- sha256 of a rendered fixture, not a secret
GUARD_SHA256_PRIVACY_FOREIGN_CP='314d9b4d23f36c58bbe3eb12042945f762822b288d3ce343b46dfa4488053fca'  # pragma: allowlist secret -- sha256 of a rendered fixture, not a secret

# The head of the fixture controller through its COMPOSE_CMD= line (the
# test-selfhost-port-posture.sh precedent): it selects the deployment profiles
# and builds COMPOSE_CMD, and dispatches nothing. A self-host project has no
# deploy.env.local (the self-host contract refuses one), only deploy.env.
SH_PROJECT="$TMP/selfhost-project"
SH_ENV="$SH_PROJECT/infrastructure/deploy/deploy.env"
HEAD_NAME=concord-ctl-head.sh
mkdir -p "$SH_PROJECT/infrastructure/deploy"
for root in "$PROJECT" "$RAIL_PROJECT" "$SH_PROJECT"; do
  sed -n '1,/^COMPOSE_CMD=/p' "$CTL" >"$root/infrastructure/deploy/$HEAD_NAME"
done
# head_probe <root> [VAR=value...]: source <root>'s head with the stubs first on
# PATH and CONTROL_PLANE_DEPLOY_PROFILE unset unless a VAR sets it. Prints
# "rc=<n> flag=<rail> profile=<profile> cmd=<word-split COMPOSE_CMD>" (or rc
# and the refusal), fixture root shown as <P>.
head_probe() {
  local root="$1" out r=0
  shift
  # shellcheck disable=SC2016
  out="$(env -u CONTROL_PLANE_DEPLOY_PROFILE PATH="$STUBS:$PATH" "$@" bash -c 'source "$1"
    # shellcheck disable=SC2086
    printf "flag=%s profile=%s cmd=%s" "$CONTROL_PLANE_BLUEGREEN" \
      "${CONTROL_PLANE_DEPLOY_PROFILE-<unset>}" "$(echo $COMPOSE_CMD)"' \
    _ "$root/infrastructure/deploy/$HEAD_NAME" 2>&1 </dev/null)" || r=$?
  printf 'rc=%s %s' "$r" "$out" | sed "s#$root/infrastructure/deploy/../..#<P>#g"
}
# with_env_line <env-file> <line> <command...>: run <command> with <line>
# appended to <env-file>, then restore the file.
with_env_line() {
  local f="$1" line="$2"
  shift 2
  cp "$f" "$TMP/env.bak"
  printf '%s\n' "$line" >>"$f"
  "$@"
  cp "$TMP/env.bak" "$f"
}
# rail_transcript / rail_out: the call log and output with either fixture root
# shown as <P>, so a rail-off and a rail-on run compare byte for byte.
rail_transcript() {
  sed -e "s#$PROJECT/infrastructure/deploy/../..#<P>#g" \
    -e "s#$RAIL_PROJECT/infrastructure/deploy/../..#<P>#g" "$TMP/calls"
}
rail_out() { sed -e "s#$RAIL_PROJECT#<P>#g" -e "s#$PROJECT#<P>#g" "$TMP/out"; }
# refused_exactly <rc> <line>: the run exited <rc>, printed exactly <line>, and
# called no stub.
refused_exactly() {
  [[ "$rc" == "$1" && "$(cat "$TMP/out")" == "$2" && ! -s "$TMP/calls" ]]
}
# mutating_calls: stub calls that change containers or images.
mutating_calls() {
  grep -cE '^docker (compose .* )?(build|stop|up|down|run|rm|restart|kill|start)( |$)' "$TMP/calls" || true
}
# saw <line>: the last run printed <line> as a whole line.
saw() { grep -qFx -- "$1" "$TMP/out"; }
brief_out() { head -c 400 "$TMP/out" | tr '\n' ';'; }
# reset_nats: reset_stubs plus a running nats container, so `rebuild nats`
# stays scoped instead of escalating to a full rebuild.
reset_nats() {
  reset_stubs
  stub_out docker '*ps --status running -q -- nats' 'ab12ab12ab12\n'
}

switch_unset_compose_cmd_identical() {
  local got probe
  got="$( (unset CONTROL_PLANE_DEPLOY_PROFILE; char_rebuild_control_plane_transcript) 2>&1)"
  cp "$TMP/out" "$TMP/t0.out"
  probe="$(head_probe "$PROJECT" COMPOSE_OVERRIDE=docker-compose.production.yml)"
  # Mutation that turns this red: default the profile to control-plane-bluegreen
  # in the production/staging branch (flag 1, or rail-refused-no-flock).
  if [[ "$got" == 'PASS: char_rebuild_control_plane_transcript' && "$probe" == "$T0_PROBE" ]]; then
    pass "switch_unset_compose_cmd_identical"
  else
    fail "switch_unset_compose_cmd_identical probe=[$probe] t0=[$(tr '\n' ';' <<<"$got")]"
  fi
}

switch_single_identical() {
  local got probe line="CONTROL_PLANE_DEPLOY_PROFILE='control-plane-single'" same=0
  got="$( (with_env_line "$PLAIN_ENV" "$line" char_rebuild_control_plane_transcript) 2>&1)"
  cmp -s "$TMP/out" "$TMP/t0.out" && same=1
  probe="$(with_env_line "$PLAIN_ENV" "$line" head_probe "$PROJECT" COMPOSE_OVERRIDE=docker-compose.production.yml)"
  # Mutation that turns this red: turn the rail on for control-plane-single too.
  if [[ "$got" == 'PASS: char_rebuild_control_plane_transcript' && $same -eq 1 && "$probe" == "$T0_PROBE" ]]; then
    pass "switch_single_identical"
  else
    fail "switch_single_identical output-identical=$same probe=[$probe] t0=[$(tr '\n' ';' <<<"$got")]"
  fi
}

switch_bluegreen_compose_cmd_identical() {
  need_flock switch_bluegreen_compose_cmd_identical || return 0
  local probe total other
  probe="$(head_probe "$RAIL_PROJECT" COMPOSE_OVERRIDE=docker-compose.production.yml)"
  reset_nats
  rm -f "$RAIL_MARKER"
  run_ctl_at "$RAIL_CTL" rebuild nats
  total="$(rail_transcript | grep -c '^docker compose ' || true)"
  other="$(rail_transcript | grep '^docker compose ' | grep -vcF "$T0_COMPOSE " || true)"
  # Mutation that turns this red: add an -f overlay (or a project flag) to
  # COMPOSE_CMD when the rail is on.
  if [[ "$probe" == "rc=0 flag=1 profile=control-plane-bluegreen cmd=$T0_COMPOSE" \
     && $rc -eq 0 && $total -ge 1 && $other -eq 0 ]]; then
    pass "switch_bluegreen_compose_cmd_identical"
  else
    fail "switch_bluegreen_compose_cmd_identical probe=[$probe] rc=$rc compose-calls=$total other-prefix=$other: $(brief_out)"
  fi
}

# Every invalid value is refused before any command runs.
switch_invalid_exits_2() {
  local v bad=''
  for v in control-plane-blue CONTROL-PLANE-BLUEGREEN control_plane_bluegreen media-bluegreen; do
    reset_stubs
    with_env_line "$PLAIN_ENV" "CONTROL_PLANE_DEPLOY_PROFILE='$v'" run_ctl rebuild control-plane
    refused_exactly 2 "$SWITCH_INVALID" || bad+=" [$v rc=$rc: $(brief_out)]"
  done
  # Mutation that turns this red: delete the profile case's `*)` refusal arm.
  if [[ -z "$bad" ]]; then pass "switch_invalid_exits_2"; else fail "switch_invalid_exits_2:$bad"; fi
}

# The value after sourcing must be exact: no padding, no literal quotes.
switch_quoted_or_padded_value_exits_2() {
  local line bad=''
  for line in "CONTROL_PLANE_DEPLOY_PROFILE=' control-plane-bluegreen'" \
    "CONTROL_PLANE_DEPLOY_PROFILE='\"control-plane-bluegreen\"'" \
    "CONTROL_PLANE_DEPLOY_PROFILE='control-plane-bluegreen '"; do
    reset_stubs
    with_env_line "$PLAIN_ENV" "$line" run_ctl rebuild control-plane
    refused_exactly 2 "$SWITCH_INVALID" || bad+=" [$line rc=$rc: $(brief_out)]"
  done
  # Mutation that turns this red: trim whitespace or quotes before the case, or
  # match with a glob such as `*control-plane-bluegreen*`.
  if [[ -z "$bad" ]]; then pass "switch_quoted_or_padded_value_exits_2"; else fail "switch_quoted_or_padded_value_exits_2:$bad"; fi
}

switch_forced_single_in_selfhost_mode() {
  local base sw inv envsw
  local -a sh=(ACTIVITY_HISTORY_CONTRACT_MODE=selfhost COMPOSE_OVERRIDE=docker-compose.production.yml COMPOSE_PROFILES=services)
  reset_stubs
  rm -f "$SH_ENV"
  base="$(head_probe "$SH_PROJECT" "${sh[@]}")"
  printf '%s\n' "CONTROL_PLANE_DEPLOY_PROFILE='control-plane-bluegreen'" >"$SH_ENV"
  sw="$(head_probe "$SH_PROJECT" "${sh[@]}")"
  printf '%s\n' "CONTROL_PLANE_DEPLOY_PROFILE='not-a-profile'" >"$SH_ENV"
  inv="$(head_probe "$SH_PROJECT" "${sh[@]}")"
  rm -f "$SH_ENV"
  envsw="$(head_probe "$SH_PROJECT" "${sh[@]}" CONTROL_PLANE_DEPLOY_PROFILE=control-plane-bluegreen)"
  # Mutation that turns this red: leave the sourced profile in place in the
  # self-host branch instead of forcing control-plane-single.
  if [[ "$base" == 'rc=0 flag=0 profile=control-plane-single cmd=docker compose --env-file <P>/.env '* \
     && "$sw" == "$base" && "$inv" == "$base" && "$envsw" == "$base" && ! -s "$TMP/calls" ]]; then
    pass "switch_forced_single_in_selfhost_mode"
  else
    fail "switch_forced_single_in_selfhost_mode base=[$base] switch=[$sw] invalid=[$inv] env=[$envsw]"
  fi
}

# The final `else` branch (an override that is neither production, staging nor
# West staging) forces control-plane-single silently, whatever the value.
switch_forced_single_outside_production() {
  local base sw inv
  base="$(head_probe "$PROJECT" COMPOSE_OVERRIDE=docker-compose.dev.yml)"
  sw="$(with_env_line "$PLAIN_ENV" "CONTROL_PLANE_DEPLOY_PROFILE='control-plane-bluegreen'" \
    head_probe "$PROJECT" COMPOSE_OVERRIDE=docker-compose.dev.yml)"
  inv="$(with_env_line "$PLAIN_ENV" "CONTROL_PLANE_DEPLOY_PROFILE='not-a-profile'" \
    head_probe "$PROJECT" COMPOSE_OVERRIDE=docker-compose.dev.yml)"
  # Mutation that turns this red: give the final else branch the production
  # branch's default-and-validate form.
  if [[ "$base" == 'rc=0 flag=0 profile=control-plane-single cmd=docker compose -f <P>/docker-compose.yml -f <P>/docker-compose.dev.yml --profile services' \
     && "$sw" == "$base" && "$inv" == "$base" ]]; then
    pass "switch_forced_single_outside_production"
  else
    fail "switch_forced_single_outside_production base=[$base] switch=[$sw] invalid=[$inv]"
  fi
}

# Checked before flock, so this runs on every host.
rail_refused_media_bluegreen() {
  reset_stubs
  with_env_line "$RAIL_ENV" "MEDIA_DEPLOY_PROFILE='media-bluegreen'" run_ctl_at "$RAIL_CTL" rebuild control-plane
  # Mutation that turns this red: delete the media-bluegreen refusal.
  if refused_exactly 2 'control-plane-bluegreen: rail-refused-media-bluegreen'; then
    pass "rail_refused_media_bluegreen"
  else
    fail "rail_refused_media_bluegreen rc=$rc: $(brief_out)"
  fi
}

# Simulated everywhere by rewrite 6 (on a host without flock it also holds natively).
rail_refused_no_flock() {
  reset_stubs
  run_ctl_at "$NOFLOCK_CTL" rebuild control-plane
  # Mutation that turns this red: delete the /usr/bin/flock refusal line.
  if refused_exactly 2 'control-plane-bluegreen: rail-refused-no-flock'; then
    pass "rail_refused_no_flock"
  else
    fail "rail_refused_no_flock rc=$rc: $(brief_out)"
  fi
}

# Without the state directory the lock falls back to the deploy directory.
rail_refused_no_state_dir() {
  need_flock rail_refused_no_state_dir || return 0
  reset_stubs
  mv "$TMP/lock" "$TMP/lock.away"
  run_ctl_at "$RAIL_CTL" rebuild control-plane
  mv "$TMP/lock.away" "$TMP/lock"
  # Mutation that turns this red: delete the no-state-dir refusal after
  # `readonly DEPLOY_TRANSACTION_LOCK_PATH` (the fallback lock is taken).
  if refused_exactly 2 'control-plane-bluegreen: rail-refused-no-state-dir'; then
    pass "rail_refused_no_state_dir"
  else
    fail "rail_refused_no_state_dir rc=$rc: $(brief_out)"
  fi
}

guards_unchanged_from_baseline() {
  local spec fn got bad=''
  for spec in "activity_history_refresh_daemon_ids:$GUARD_SHA256_REFRESH_DAEMON_IDS" \
    "privacy_defaults_refuse_foreign_control_plane:$GUARD_SHA256_PRIVACY_FOREIGN_CP"; do
    fn="${spec%%:*}"
    extract_top_fn "$fn" >"$TMP/guard.body"
    got="$(sha256_of "$TMP/guard.body")"
    [[ -s "$TMP/guard.body" && "$got" == "${spec#*:}" ]] || bad+=" $fn=$got"
  done
  # Mutation that turns this red: change any byte of either guard.
  if [[ -z "$bad" ]]; then pass "guards_unchanged_from_baseline"; else fail "guards_unchanged_from_baseline:$bad"; fi
}

# Rail on, no marker: a non-control-plane rebuild is byte-identical to rail off.
marker_absent_no_extra_calls() {
  need_flock marker_absent_no_extra_calls || return 0
  local off_t off_o off_rc
  rm -f "$PLAIN_MARKER" "$RAIL_MARKER"
  reset_nats
  run_ctl rebuild nats
  off_rc=$rc off_t="$(rail_transcript)" off_o="$(rail_out)"
  reset_nats
  run_ctl_at "$RAIL_CTL" rebuild nats
  # Mutation that turns this red: make any call from the marker guard before its
  # marker test (for example a docker ps probe for green).
  if [[ $off_rc -eq 0 && $rc -eq 0 && -n "$off_t" && "$(rail_transcript)" == "$off_t" && "$(rail_out)" == "$off_o" ]]; then
    pass "marker_absent_no_extra_calls"
  else
    fail "marker_absent_no_extra_calls off-rc=$off_rc on-rc=$rc"
    diff <(printf '%s\n' "$off_t") <(rail_transcript) >&2 || true
  fi
}

# Rail off + marker: every locked verb but sudoers-sync refuses after the lock.
marker_rail_off_refuses_rebuild() {
  local spec bad=''
  local -a v
  : >"$PLAIN_MARKER"
  for spec in 'rebuild control-plane' 'stop' 'nginx-reload'; do
    read -r -a v <<<"$spec"
    reset_stubs
    run_ctl "${v[@]}"
    [[ $rc -eq 1 && "$(mutating_calls)" == 0 ]] && saw "$RAIL_OFF_MARKER" || bad+=" [$spec rc=$rc: $(brief_out)]"
  done
  rm -f "$PLAIN_MARKER"
  # Mutation that turns this red: delete the guard call after either dispatch
  # lock (or the guard's rail-off arm).
  if [[ -z "$bad" ]]; then pass "marker_rail_off_refuses_rebuild"; else fail "marker_rail_off_refuses_rebuild:$bad"; fi
}

marker_rail_off_refuses_freshstart() {
  reset_stubs
  : >"$PLAIN_MARKER"
  rc=0
  printf 'y\n' | PATH="$STUBS:$PATH" COMPOSE_OVERRIDE=docker-compose.production.yml \
    bash "$CTL" freshstart >"$TMP/out" 2>&1 || rc=$?
  rm -f "$PLAIN_MARKER"
  # Mutation that turns this red: delete the guard call after freshstart's lock.
  if [[ $rc -eq 1 && "$(mutating_calls)" == 0 ]] && saw "$RAIL_OFF_MARKER"; then
    pass "marker_rail_off_refuses_freshstart"
  else
    fail "marker_rail_off_refuses_freshstart rc=$rc mutating=$(mutating_calls): $(brief_out)"
  fi
}

marker_rail_off_allows_sudoers_sync() {
  reset_stubs
  : >"$PLAIN_MARKER"
  run_ctl sudoers-sync
  rm -f "$PLAIN_MARKER"
  # Mutation that turns this red: drop sudoers-sync from the guard's always-allowed arm.
  if [[ $rc -eq 0 ]] && ! saw "$RAIL_OFF_MARKER" && grep -qFx 'sudo -n /usr/local/sbin/concord-sudoers-sync' "$TMP/calls"; then
    pass "marker_rail_off_allows_sudoers_sync"
  else
    fail "marker_rail_off_allows_sudoers_sync rc=$rc: $(brief_out)"
  fi
}

marker_rail_on_refuses_restart() {
  need_flock marker_rail_on_refuses_restart || return 0
  local bad=''
  reset_stubs
  : >"$RAIL_MARKER"
  run_ctl_at "$RAIL_CTL" restart
  [[ $rc -eq 1 && "$(mutating_calls)" == 0 ]] && saw "$CUTOVER_INCOMPLETE" || bad+=" marker:rc=$rc: $(brief_out)"
  # Control: without the marker restart is not refused by the guard.
  reset_stubs
  rm -f "$RAIL_MARKER"
  run_ctl_at "$RAIL_CTL" restart
  saw "$CUTOVER_INCOMPLETE" && bad+=" no-marker:refused"
  # Mutation that turns this red: add restart to the rail-on allowed verbs (or
  # invert the marker test).
  if [[ -z "$bad" ]]; then pass "marker_rail_on_refuses_restart"; else fail "marker_rail_on_refuses_restart:$bad"; fi
}

marker_rail_on_allows_nginx_reload_and_stop() {
  need_flock marker_rail_on_allows_nginx_reload_and_stop || return 0
  local bad=''
  reset_stubs
  : >"$RAIL_MARKER"
  run_ctl_at "$RAIL_CTL" nginx-reload
  # Under the stubs nginx-reload stops at the managed identity admission, after the guard.
  { ! saw "$CUTOVER_INCOMPLETE" && grep -qF 'managed-core: canonical deployment identity is missing or ambiguous' "$TMP/out"; } \
    || bad+=" nginx-reload:rc=$rc: $(brief_out)"
  reset_stubs
  run_ctl_at "$RAIL_STOP_CTL" stop
  [[ $rc -eq 0 ]] && ! saw "$CUTOVER_INCOMPLETE" && saw 'Done.' || bad+=" stop:rc=$rc: $(brief_out)"
  rm -f "$RAIL_MARKER"
  # Mutation that turns this red: drop nginx-reload or stop from the rail-on allowed verbs.
  if [[ -z "$bad" ]]; then pass "marker_rail_on_allows_nginx_reload_and_stop"; else fail "marker_rail_on_allows_nginx_reload_and_stop:$bad"; fi
}

stop_with_marker_reaps_green_and_keeps_marker() {
  need_flock stop_with_marker_reaps_green_and_keeps_marker || return 0
  local bad='' want
  want="$(printf 'docker stop -t 45 %s\ndocker rm -f %s' "$GREEN" "$GREEN")"
  reset_stubs
  : >"$RAIL_MARKER"
  run_ctl_at "$RAIL_STOP_CTL" stop
  # Mutations: drop the reap (the last calls are compose's); remove the marker.
  [[ $rc -eq 0 && "$(tail -n 2 "$TMP/calls")" == "$want" && -e "$RAIL_MARKER" ]] \
    || bad+=" reap:rc=$rc,last=$(tail -n 2 "$TMP/calls" | tr '\n' ';')"
  # Green already gone: both calls report No such container; stop succeeds.
  # Mutation: treat a missing green as a failure.
  reset_stubs
  stub_err docker "stop -t 45 $GREEN" "Error response from daemon: No such container: $GREEN\\n"
  stub_exit docker "stop -t 45 $GREEN" 1
  stub_err docker "rm -f $GREEN" "Error response from daemon: No such container: $GREEN\\n"
  stub_exit docker "rm -f $GREEN" 1
  run_ctl_at "$RAIL_STOP_CTL" stop
  [[ $rc -eq 0 && -e "$RAIL_MARKER" ]] || bad+=" absent:rc=$rc: $(brief_out)"
  # Any other reap failure fails the stop and keeps the marker.
  # Mutation: ignore every reap failure.
  reset_stubs
  stub_err docker "stop -t 45 $GREEN" 'Cannot connect to the Docker daemon\n'
  stub_exit docker "stop -t 45 $GREEN" 1
  run_ctl_at "$RAIL_STOP_CTL" stop
  [[ $rc -eq 1 && -e "$RAIL_MARKER" ]] && saw 'Cannot connect to the Docker daemon' \
    || bad+=" daemon-error:rc=$rc: $(brief_out)"
  rm -f "$RAIL_MARKER"
  if [[ -z "$bad" ]]; then pass "stop_with_marker_reaps_green_and_keeps_marker"; else fail "stop_with_marker_reaps_green_and_keeps_marker:$bad"; fi
}

stop_without_marker_unchanged() {
  need_flock stop_without_marker_unchanged || return 0
  local off_t off_o off_rc
  rm -f "$PLAIN_MARKER" "$RAIL_MARKER"
  reset_stubs
  run_ctl_at "$STOP_CTL" stop
  off_rc=$rc off_t="$(rail_transcript)" off_o="$(rail_out)"
  reset_stubs
  run_ctl_at "$RAIL_STOP_CTL" stop
  # Mutation that turns this red: reap green whenever the rail is on (drop the
  # marker test from stop).
  if [[ $off_rc -eq 0 && $rc -eq 0 && "$(rail_transcript)" == "$off_t" && "$(rail_out)" == "$off_o" ]]; then
    pass "stop_without_marker_unchanged"
  else
    fail "stop_without_marker_unchanged off-rc=$off_rc on-rc=$rc"
    diff <(printf '%s\n' "$off_t") <(rail_transcript) >&2 || true
  fi
}

# ---------------------------------------------------------------------------
# Control-plane cutover: `rebuild control-plane` with the rail on. Rewrite 8
# replaces the cutover's one nested nginx-reload line with the ctlflip stub
# (under the stubs a real nested nginx-reload stops at the managed identity
# admission). ctlflip logs "ctlflip <port>" and rewrites the fixture live vhost
# to that port.
CUT_CTL="$RAIL_PROJECT/infrastructure/deploy/concord-ctl-cutover.sh"
LIVE_VHOST="$TMP/live/concordvoice"
# Its directory exists, as the live vhost's does on a host: the reader reads
# only a missing vhost in a searchable directory as 8080.
mkdir -p "$TMP/live"
GREEN_ID="$(printf 'fe%.0s' {1..32})"
BUILT_ID="sha256:$(printf 'b1%.0s' {1..32})"
OTHER_ID="sha256:$(printf 'a0%.0s' {1..32})"
src="$(cat "$RAIL_CTL")"
# shellcheck disable=SC2016
fixture_rewrite '  "$0" "$CONTROL_PLANE_FLIP_REENTRY_MARKER" nginx-reload --control-plane-upstream "$1"' \
  '  ctlflip "$1"'
printf '%s\n' "$src" >"$CUT_CTL"
chmod +x "$CUT_CTL"
# ctlflip <port>: with $TMP/ctlflip.sleep.<port> (seconds) it records
# $TMP/ctlflip.started and sleeps first. Like the real re-entry it refuses
# (exit 1) without the cutover marker. With $TMP/ctlflip.fail.<port> it exits
# 1: an empty file leaves the vhost alone, `applied` rewrites it to <port>
# first, and `garble` leaves it unparseable.
cat >"$STUBS/ctlflip" <<STUB
#!/usr/bin/env bash
printf 'ctlflip %s\n' "\$*" >>"$TMP/calls"
if [[ -f "$TMP/ctlflip.sleep.\$1" ]]; then
  : >"$TMP/ctlflip.started"
  sleep "\$(cat "$TMP/ctlflip.sleep.\$1")"
fi
[[ -e "$RAIL_MARKER" ]] || { echo 'ctlflip: no cutover marker' >&2; exit 1; }
fail=none
[[ ! -e "$TMP/ctlflip.fail.\$1" ]] || fail="\$(cat "$TMP/ctlflip.fail.\$1")"
[[ -n "\$fail" ]] || exit 1
mkdir -p "$TMP/live"
if [[ "\$fail" == garble ]]; then
  printf 'not an nginx vhost\n' >"$LIVE_VHOST"
  exit 1
fi
awk -v port="\$1" '\$0 == "    server 127.0.0.1:8080;" { \$0 = "    server 127.0.0.1:" port ";" } { print }' \
  "$VHOST_SRC" >"$LIVE_VHOST"
[[ "\$fail" != applied ]] || exit 1
STUB
chmod +x "$STUBS/ctlflip"

C="$T0_COMPOSE"
STATUS_PAT='*run --rm --no-deps --pull never control-plane ./main migrations status'
STATUS_RUN="$C run --rm --no-deps --pull never control-plane ./main migrations status"
GREEN_RUN="$C run --detach --no-deps --pull never --use-aliases --name $GREEN -p 127.0.0.1:8081:8080 -e MIGRATIONS_MODE=verify -e OPS_METRICS_ENABLED=false control-plane"
CP_PS="$C ps --status running -q -- control-plane"
DAEMON_PS='docker ps --filter label=com.docker.compose.service=control-plane --format {{.ID}}'
HOME_STOP="$C stop --timeout 45 control-plane"
HOME_UP="$C up -d --no-build --no-deps --wait --wait-timeout 180 control-plane"
GREEN_STOP="docker stop -t 45 $GREEN"
GREEN_RM="docker rm -f $GREEN"
GREEN_REAP="$GREEN_STOP"$'\n'"$GREEN_RM"
# The T0 golden through the build: what precedes the hook.
CUT_PREFIX="$(cat <<GOLDEN
$C config --format json
docker ps --all --quiet --no-trunc --filter label=com.docker.compose.service=media-plane
$C config --format json
$C config --format json
$CP_PS
$C config --format json
$C config --format json
$C up -d --no-deps --no-build --pull missing --wait --wait-timeout 180 nats
$C ps -q nats
docker inspect --format {{json .HostConfig.PortBindings}}|{{.HostConfig.PublishAllPorts}} ab12ab12ab12
$C build control-plane
GOLDEN
)"
CUT_HAPPY="$(cat <<GOLDEN
$CUT_PREFIX
$STATUS_RUN
$DAEMON_PS
$CP_PS
$C config --format json
docker image inspect --format {{.Id}} cp:fixture
$GREEN_REAP
$GREEN_RUN
$CP_PS
docker inspect --format {{.Image}} $GREEN_ID
curl --connect-timeout 1 --max-time 1 -sf http://127.0.0.1:8081/readyz
ctlflip 8081
$HOME_STOP
$STATUS_RUN
$HOME_UP
curl --connect-timeout 1 --max-time 1 -sf http://127.0.0.1:8080/readyz
ctlflip 8080
$GREEN_REAP
GOLDEN
)"
# The four drained calls after the build, in their original order.
DRAINED_TAIL="$(printf '%s\n' "$DAEMON_PS" "$CP_PS" "$HOME_STOP" "$DAEMON_PS" "$HOME_UP")"

# cut_prepare: clean stubs, marker, live vhost and ctlflip controls. Case rules
# go after it; run_cut_at then appends the healthy cutover defaults, which the
# case rules override (first match wins).
cut_prepare() {
  reset_stubs
  rm -f "$RAIL_MARKER" "$LIVE_VHOST" "$TMP"/ctlflip.*
}
cut_defaults() {
  stub_reply docker "$STATUS_PAT" 0 'current 12\n'
  stub_reply docker '*run --detach --no-deps *' 0 "$GREEN_ID\\n"
  stub_reply docker '*ps --status running -q -- control-plane' 0 "c0ffee000001\\n$GREEN_ID\\n"
  stub_out docker 'image inspect --format {{.Id}} cp:fixture' "$BUILT_ID\\n"
  stub_out docker "inspect --format {{.Image}} $GREEN_ID" "$BUILT_ID\\n"
}
# run_cut_at <ctl> <args...>: run_ctl_at with the cutover defaults and a short
# health wait (2s, probing every second).
run_cut_at() {
  local ctl="$1"
  shift
  cut_defaults
  rc=0
  PATH="$STUBS:$PATH" COMPOSE_OVERRIDE=docker-compose.production.yml \
    ACTIVITY_HISTORY_HEALTH_TIMEOUT=2 ACTIVITY_HISTORY_HEALTH_INTERVAL=1 \
    bash "$ctl" "$@" >"$TMP/out" 2>&1 </dev/null || rc=$?
}
has_call() { grep -qFx -- "$1" <<<"$(rail_transcript)"; }
ncalls() { grep -cFx -- "$1" <<<"$(rail_transcript)" || true; }
last_calls() { rail_transcript | tail -n "$1"; }
# lines_after <line> <n>: the <n> calls after the first <line>.
lines_after() {
  rail_transcript | awk -v l="$1" -v n="$2" 'hit && n-- > 0 { print } !hit && $0 == l { hit = 1 }'
}
# drained_ok [allow-green]: exit 0 with Done., the build, the drained calls as
# the last calls, no marker, no flip; without allow-green also no green start
# and no call naming green.
drained_ok() {
  local t
  t="$(rail_transcript)"
  [[ $rc -eq 0 && "$(tail -n 5 <<<"$t")" == "$DRAINED_TAIL" && ! -e "$RAIL_MARKER" ]] && saw 'Done.' \
    && grep -qFx -- "$C build control-plane" <<<"$t" && ! grep -q '^ctlflip ' <<<"$t" \
    && { [[ "${1:-}" == allow-green ]] || ! grep -qF -e ' run --detach ' -e "$GREEN" <<<"$t"; }
}
# named_error <name>: exit 1, the named line, then a line that names the
# recovery command.
named_error() {
  local next
  next="$(awk -v l="control-plane-bluegreen: $1" 'hit { print; exit } $0 == l { hit = 1 }' "$TMP/out")"
  [[ $rc -eq 1 && "$next" == *'rebuild control-plane'* ]]
}
cut_report() {
  if [[ "$2" == ok ]]; then pass "$1"; else fail "$1 rc=$rc: $(brief_out)"; last_calls 6 >&2; fi
}

cutover_happy_transcript() {
  need_flock cutover_happy_transcript || return 0
  cut_prepare
  run_cut_at "$CUT_CTL" rebuild control-plane
  rail_transcript >"$TMP/happy.calls"
  # Mutation that turns this red: drop any step, or reorder two (for example
  # inspect green's image after the readiness gate).
  if [[ $rc -eq 0 && "$(cat "$TMP/happy.calls")" == "$CUT_HAPPY" && ! -e "$RAIL_MARKER" ]] && saw 'Done.' \
     && grep -qFx '    server 127.0.0.1:8080;' "$LIVE_VHOST"; then
    pass "cutover_happy_transcript"
  else
    fail "cutover_happy_transcript rc=$rc marker=$([[ -e "$RAIL_MARKER" ]] && echo kept || echo absent): $(brief_out)"
    diff <(printf '%s\n' "$CUT_HAPPY") "$TMP/happy.calls" >&2 || true
  fi
}

cutover_gates_on_readyz_not_health() {
  need_flock cutover_gates_on_readyz_not_health || return 0
  local probes
  cut_prepare
  stub_exit curl '*/health' 22
  run_cut_at "$CUT_CTL" rebuild control-plane
  probes="$(rail_transcript | awk '/^curl / { printf "%s ", $NF }')"
  # Mutation that turns this red: call activity_history_wait_control_plane
  # without its URL at either gate (the /health default fails here).
  if [[ $rc -eq 0 && "$probes" == 'http://127.0.0.1:8081/readyz http://127.0.0.1:8080/readyz ' ]]; then
    pass "cutover_gates_on_readyz_not_health"
  else
    fail "cutover_gates_on_readyz_not_health rc=$rc probes=[$probes]: $(brief_out)"
  fi
}

# Rail off and rail on make the same calls and print the same output for any
# scope other than exactly control-plane.
cutover_only_for_exact_scope() {
  need_flock cutover_only_for_exact_scope || return 0
  local spec bad='' off_rc off_t off_o
  local -a v
  for spec in 'control-plane nats' 'control-plane media-plane'; do
    read -r -a v <<<"$spec"
    cut_prepare
    stub_out docker '*ps --status running -q -- nats' 'ab12ab12ab12\n'
    run_cut_at "$CTL" rebuild "${v[@]}"
    off_rc=$rc off_t="$(rail_transcript)" off_o="$(rail_out)"
    cut_prepare
    stub_out docker '*ps --status running -q -- nats' 'ab12ab12ab12\n'
    run_cut_at "$CUT_CTL" rebuild "${v[@]}"
    [[ $rc -eq $off_rc && -n "$off_t" && "$(rail_transcript)" == "$off_t" && "$(rail_out)" == "$off_o" ]] \
      && ! grep -qF -e ' run --detach ' -e 'migrations status' -e ctlflip <<<"$off_t" \
      || bad+=" [$spec off-rc=$off_rc on-rc=$rc]"
  done
  # Mutation that turns this red: test _has_control_plane instead of the exact
  # scope in the hook (the control-plane nats run then cuts over).
  if [[ -z "$bad" ]]; then pass "cutover_only_for_exact_scope"; else fail "cutover_only_for_exact_scope:$bad"; fi
}

cutover_not_run_with_rail_off() {
  local got
  got="$( (char_rebuild_control_plane_transcript) 2>&1)"
  # Mutation that turns this red: drop the CONTROL_PLANE_BLUEGREEN test from
  # the hook.
  if [[ "$got" == 'PASS: char_rebuild_control_plane_transcript' ]] \
     && ! grep -qF -e 'migrations status' -e ' run --detach ' -e "$GREEN" "$TMP/calls"; then
    pass "cutover_not_run_with_rail_off"
  else
    fail "cutover_not_run_with_rail_off: $(tr '\n' ';' <<<"$got")"
  fi
}

# gate_status <case> (<code> <stdout>)...: each status answer takes the
# drained path with nothing started.
gate_status() {
  local name="$1" bad=''
  need_flock "$name" || return 0
  shift
  while (( $# >= 2 )); do
    cut_prepare
    stub_reply docker "$STATUS_PAT" "$1" "$2"
    run_cut_at "$CUT_CTL" rebuild control-plane
    drained_ok || bad+=" [$1:$2 rc=$rc: $(brief_out)]"
    shift 2
  done
  if [[ -z "$bad" ]]; then pass "$name"; else fail "$name:$bad"; fi
}
gate_cases() {
  # Mutation: match the status line with `^current( [0-9]+)?$`.
  gate_status gate_status_bare_current 0 'current\n'
  # Mutation: test only the first line of the status output.
  gate_status gate_status_extra_line 0 'current 12\nextra\n'
  # Mutation (second answer): ignore the status run's exit code.
  gate_status gate_status_exit_3 3 'pending 11 12\n' 3 'current 12\n'
  gate_status gate_status_exit_64 64 '' 64 'current 12\n'
  gate_status gate_status_exit_1 1 '' 1 'current 12\n'
}

gate_live_port_unparseable() {
  need_flock gate_live_port_unparseable || return 0
  cut_prepare
  mkdir -p "$TMP/live"
  printf 'not an nginx vhost\n' >"$LIVE_VHOST"
  run_cut_at "$CUT_CTL" rebuild control-plane
  # Mutation that turns this red: read an unparseable live vhost as 8080.
  if drained_ok && ! has_call "$STATUS_RUN"; then cut_report gate_live_port_unparseable ok; else cut_report gate_live_port_unparseable bad; fi
}

gate_marker_write_failure() {
  need_flock gate_marker_write_failure || return 0
  local dir="$RAIL_PROJECT/infrastructure/deploy" left
  # Root writes into a mode-555 directory, so this cannot be simulated as root.
  if [[ "$(id -u)" == 0 ]]; then
    printf 'SKIP: gate_marker_write_failure (running as root; a read-only directory cannot be simulated)\n'
    return 0
  fi
  cut_prepare
  chmod a-w "$dir"
  run_cut_at "$CUT_CTL" rebuild control-plane
  chmod u+w "$dir"
  left="$(find "$dir" -name '.control-plane-cutover.local*' | wc -l | tr -d ' ')"
  # Mutation that turns this red: ignore the marker write's status.
  if drained_ok && [[ "$left" == 0 ]]; then cut_report gate_marker_write_failure ok; else cut_report gate_marker_write_failure bad; fi
}

gate_green_missing_from_compose_ps() {
  need_flock gate_green_missing_from_compose_ps || return 0
  cut_prepare
  stub_reply docker '*ps --status running -q -- control-plane' 0 'c0ffee000001\n'
  run_cut_at "$CUT_CTL" rebuild control-plane
  # Mutation that turns this red: skip the membership check (green's image is
  # inspected and the cutover goes on).
  if drained_ok allow-green && [[ "$(lines_after "$GREEN_RUN" 3)" == "$CP_PS"$'\n'"$GREEN_REAP" ]] \
     && ! grep -qF 'inspect --format {{.Image}}' <<<"$(rail_transcript)"; then
    cut_report gate_green_missing_from_compose_ps ok
  else
    cut_report gate_green_missing_from_compose_ps bad
  fi
}

stop_follows_flip_leg1() {
  need_flock stop_follows_flip_leg1 || return 0
  local next
  next="$(awk '$0 == "ctlflip 8081" { getline; print; exit }' "$TMP/happy.calls" 2>/dev/null || true)"
  # Mutation that turns this red: make any call between the 8081 flip and the
  # home stop (for example the status re-check first).
  if [[ "$next" == "$HOME_STOP" ]]; then pass "stop_follows_flip_leg1"; else fail "stop_follows_flip_leg1 next=[$next]"; fi
}

reap_follows_flip_leg2() {
  need_flock reap_follows_flip_leg2 || return 0
  local next
  next="$(awk '$0 == "ctlflip 8080" { getline; print; exit }' "$TMP/happy.calls" 2>/dev/null || true)"
  # Mutation that turns this red: make any call between the 8080 flip and the
  # green stop.
  if [[ "$next" == "$GREEN_STOP" ]]; then pass "reap_follows_flip_leg2"; else fail "reap_follows_flip_leg2 next=[$next]"; fi
}

# The controller runs as its own process-group leader with SIGINT at its
# default (a background job would start with it ignored); the whole group gets
# the INT while the first flip sleeps, as a terminal Ctrl-C would deliver it.
int_during_flip_still_stops() {
  need_flock int_during_flip_still_stops || return 0
  local pid i started=0
  cut_prepare
  printf '3\n' >"$TMP/ctlflip.sleep.8081"
  cut_defaults
  rc=0
  # shellcheck disable=SC2016
  PATH="$STUBS:$PATH" COMPOSE_OVERRIDE=docker-compose.production.yml \
    ACTIVITY_HISTORY_HEALTH_TIMEOUT=2 ACTIVITY_HISTORY_HEALTH_INTERVAL=1 \
    python3 -c 'import os, signal, sys
os.setpgid(0, 0)
signal.signal(signal.SIGINT, signal.SIG_DFL)
os.execvp(sys.argv[1], sys.argv[1:])' bash "$CUT_CTL" rebuild control-plane >"$TMP/out" 2>&1 </dev/null &
  pid=$!
  for ((i = 0; i < 200; i++)); do
    if [[ -e "$TMP/ctlflip.started" ]]; then started=1; break; fi
    sleep 0.1
  done
  if (( started )); then kill -INT -- "-$pid" 2>/dev/null || true; fi
  wait "$pid" || rc=$?
  # Mutation that turns this red: drop the `trap '' HUP INT TERM` before the
  # first flip (the INT kills the flip and the controller before the stop).
  if (( started )) && [[ $rc -eq 0 && "$(lines_after 'ctlflip 8081' 1)" == "$HOME_STOP" ]] && saw 'Done.'; then
    pass "int_during_flip_still_stops"
  else
    fail "int_during_flip_still_stops started=$started rc=$rc: $(brief_out)"
  fi
}

err_standby_start_failed() {
  need_flock err_standby_start_failed || return 0
  cut_prepare
  stub_reply docker '*run --detach --no-deps *' 1 ''
  run_cut_at "$CUT_CTL" rebuild control-plane
  # Mutation that turns this red: take the drained path (return 10) when the
  # green start fails.
  if named_error standby-not-ready && [[ ! -e "$RAIL_MARKER" && "$(last_calls 2)" == "$GREEN_REAP" ]] \
     && ! has_call "$HOME_STOP" && ! has_call 'ctlflip 8081'; then
    cut_report err_standby_start_failed ok
  else
    cut_report err_standby_start_failed bad
  fi
}

err_standby_image_mismatch() {
  need_flock err_standby_image_mismatch || return 0
  cut_prepare
  stub_out docker "inspect --format {{.Image}} $GREEN_ID" "$OTHER_ID\\n"
  run_cut_at "$CUT_CTL" rebuild control-plane
  # Mutation that turns this red: drop the image comparison.
  if named_error standby-image-mismatch && [[ ! -e "$RAIL_MARKER" && "$(last_calls 2)" == "$GREEN_REAP" ]] \
     && ! grep -q '^curl ' <<<"$(rail_transcript)" && ! has_call 'ctlflip 8081'; then
    cut_report err_standby_image_mismatch ok
  else
    cut_report err_standby_image_mismatch bad
  fi
}

err_standby_not_ready() {
  need_flock err_standby_not_ready || return 0
  cut_prepare
  stub_exit curl '*8081/readyz' 22
  run_cut_at "$CUT_CTL" rebuild control-plane
  # Mutation that turns this red: ignore the 8081 readiness gate's status.
  if named_error standby-not-ready && [[ ! -e "$RAIL_MARKER" && "$(last_calls 2)" == "$GREEN_REAP" ]] \
     && ! has_call "$HOME_STOP" && ! grep -q '^ctlflip ' <<<"$(rail_transcript)"; then
    cut_report err_standby_not_ready ok
  else
    cut_report err_standby_not_ready bad
  fi
}

err_flip_failed_leg1() {
  need_flock err_flip_failed_leg1 || return 0
  cut_prepare
  : >"$TMP/ctlflip.fail.8081"
  run_cut_at "$CUT_CTL" rebuild control-plane
  # Mutation that turns this red: keep the marker (or skip the reap) after a
  # failed first flip.
  if named_error flip-failed && [[ ! -e "$RAIL_MARKER" && "$(lines_after 'ctlflip 8081' 3)" == "$GREEN_REAP" ]] \
     && ! has_call "$HOME_STOP"; then
    cut_report err_flip_failed_leg1 ok
  else
    cut_report err_flip_failed_leg1 bad
  fi
}

# A failed first flip whose vhost no longer reads 8080 (reloaded to green, or
# unparseable) may route to green: green and the marker stay.
err_flip_failed_leg1_live_port_not_8080_keeps_green() {
  need_flock err_flip_failed_leg1_live_port_not_8080_keeps_green || return 0
  local mode bad=''
  for mode in applied garble; do
    cut_prepare
    printf '%s\n' "$mode" >"$TMP/ctlflip.fail.8081"
    run_cut_at "$CUT_CTL" rebuild control-plane
    { named_error flip-failed && [[ -e "$RAIL_MARKER" && "$(last_calls 1)" == 'ctlflip 8081' ]] \
      && [[ $(ncalls "$GREEN_STOP") -eq 1 ]] && ! has_call "$HOME_STOP" && grep -qF 'may be routing' "$TMP/out"; } \
      || bad+=" [$mode rc=$rc: $(brief_out)]"
    rm -f "$RAIL_MARKER"
  done
  # Mutation that turns this red: drop the live-port re-read after a failed
  # first flip (green is reaped and the marker cleared whatever nginx routes).
  if [[ -z "$bad" ]]; then pass "err_flip_failed_leg1_live_port_not_8080_keeps_green"; else fail "err_flip_failed_leg1_live_port_not_8080_keeps_green:$bad"; fi
}

err_old_colour_stop_failed() {
  need_flock err_old_colour_stop_failed || return 0
  cut_prepare
  stub_exit docker '*stop --timeout 45 control-plane' 1
  run_cut_at "$CUT_CTL" rebuild control-plane
  # Mutation that turns this red: clear the marker when the home stop fails.
  if named_error old-colour-stop-failed && [[ -e "$RAIL_MARKER" && "$(last_calls 1)" == "$HOME_STOP" ]] \
     && [[ $(ncalls "$STATUS_RUN") -eq 1 ]]; then
    cut_report err_old_colour_stop_failed ok
  else
    cut_report err_old_colour_stop_failed bad
  fi
  rm -f "$RAIL_MARKER"
}

# home_not_ready_ok <last-call>: home-not-ready, marker kept, green stopped only
# by the leftover reap, no flip back, and <last-call> was the last call.
home_not_ready_ok() {
  named_error home-not-ready && [[ -e "$RAIL_MARKER" && "$(last_calls 1)" == "$1" ]] \
    && [[ $(ncalls "$GREEN_STOP") -eq 1 ]] && ! has_call 'ctlflip 8080'
}
err_home_not_ready_status_changed() {
  need_flock err_home_not_ready_status_changed || return 0
  cut_prepare
  stub_reply docker "$STATUS_PAT" 0 'current 12\n' once
  stub_reply docker "$STATUS_PAT" 3 'pending 12 13\n'
  run_cut_at "$CUT_CTL" rebuild control-plane
  # Mutation that turns this red: drop the status re-check before home starts.
  if home_not_ready_ok "$STATUS_RUN" && ! has_call "$HOME_UP"; then
    cut_report err_home_not_ready_status_changed ok
  else
    cut_report err_home_not_ready_status_changed bad
  fi
  rm -f "$RAIL_MARKER"
}
err_home_not_ready_up_failed() {
  need_flock err_home_not_ready_up_failed || return 0
  cut_prepare
  stub_exit docker '*up -d --no-build --no-deps --wait --wait-timeout 180 control-plane' 1
  run_cut_at "$CUT_CTL" rebuild control-plane
  # Mutation that turns this red: ignore the home start's status.
  if home_not_ready_ok "$HOME_UP"; then cut_report err_home_not_ready_up_failed ok; else cut_report err_home_not_ready_up_failed bad; fi
  rm -f "$RAIL_MARKER"
}
err_home_not_ready_readyz() {
  need_flock err_home_not_ready_readyz || return 0
  cut_prepare
  stub_exit curl '*8080/readyz' 22
  run_cut_at "$CUT_CTL" rebuild control-plane
  # Mutation that turns this red: ignore the 8080 readiness gate's status.
  if home_not_ready_ok 'curl --connect-timeout 1 --max-time 1 -sf http://127.0.0.1:8080/readyz'; then
    cut_report err_home_not_ready_readyz ok
  else
    cut_report err_home_not_ready_readyz bad
  fi
  rm -f "$RAIL_MARKER"
}

err_flip_failed_leg2() {
  need_flock err_flip_failed_leg2 || return 0
  cut_prepare
  : >"$TMP/ctlflip.fail.8080"
  run_cut_at "$CUT_CTL" rebuild control-plane
  # Mutation that turns this red: reap green (or clear the marker) after a
  # failed flip back.
  if named_error flip-failed && [[ -e "$RAIL_MARKER" && "$(last_calls 1)" == 'ctlflip 8080' ]] \
     && [[ $(ncalls "$GREEN_STOP") -eq 1 ]]; then
    cut_report err_flip_failed_leg2 ok
  else
    cut_report err_flip_failed_leg2 bad
  fi
  rm -f "$RAIL_MARKER"
}

wait_healthy_rail_on_uses_live_port() {
  need_flock wait_healthy_rail_on_uses_live_port || return 0
  local probes bad
  cut_prepare
  mkdir -p "$TMP/live"
  awk -v port=8081 "$AWK_CP_SERVER_TO" "$VHOST_SRC" >"$LIVE_VHOST"
  run_cut_at "$RAIL_CTL" wait-healthy --timeout 4
  probes="$(grep -c '^curl -sf http://127\.0\.0\.1:8081/[a-z]*$' "$TMP/calls" || true)"
  bad="$(grep '^curl ' "$TMP/calls" | grep -v 'http://127\.0\.0\.1:3000/' \
    | grep -cv '^curl -sf http://127\.0\.0\.1:8081/[a-z]*$' || true)"
  rm -f "$LIVE_VHOST"
  # Mutation that turns this red: drop the rail branch before CP_PROBE_BASE.
  if [[ $rc -eq 0 && "$probes" -ge 1 && "$bad" -eq 0 ]]; then
    pass "wait_healthy_rail_on_uses_live_port"
  else
    fail "wait_healthy_rail_on_uses_live_port rc=$rc probes=$probes offending=$bad: $(brief_out)"
  fi
}

wait_healthy_rail_on_unparseable_exits_1() {
  need_flock wait_healthy_rail_on_unparseable_exits_1 || return 0
  cut_prepare
  mkdir -p "$TMP/live"
  printf 'not an nginx vhost\n' >"$LIVE_VHOST"
  run_cut_at "$RAIL_CTL" wait-healthy --timeout 4
  rm -f "$LIVE_VHOST"
  # Mutation that turns this red: fall back to 8080 when the live vhost cannot
  # be read.
  if [[ $rc -eq 1 ]] && saw 'control-plane-bluegreen: live-upstream-unparseable' && ! grep -q '^curl ' "$TMP/calls"; then
    pass "wait_healthy_rail_on_unparseable_exits_1"
  else
    fail "wait_healthy_rail_on_unparseable_exits_1 rc=$rc: $(brief_out)"
  fi
}

# ---------------------------------------------------------------------------
# Recovery of an interrupted cutover: the pre-step just before the routine
# guard (control_plane_bluegreen_recover) and the resume at the hook.
GREEN_PS="docker ps --filter name=$GREEN --format {{.Names}}"
GUARD_CONFIG="$C config --format json"
NATS_PS="$C ps --status running -q -- nats"
CURL_8081='curl --connect-timeout 1 --max-time 1 -sf http://127.0.0.1:8081/readyz'
CURL_8080='curl --connect-timeout 1 --max-time 1 -sf http://127.0.0.1:8080/readyz'
# live_at <port>: the fixture live vhost routes the control plane to <port>.
live_at() {
  mkdir -p "$TMP/live"
  awk -v port="$1" "$AWK_CP_SERVER_TO" "$VHOST_SRC" >"$LIVE_VHOST"
}
live_is() { grep -qFx "    server 127.0.0.1:$1;" "$LIVE_VHOST" 2>/dev/null; }
# green_running: `docker ps` (running only) lists a container named green.
green_running() { stub_out docker "ps --filter name=$GREEN *" "$GREEN\\n"; }
nats_running() { stub_out docker '*ps --status running -q -- nats' 'ab12ab12ab12\n'; }
# insert_after <text> <line> <lines>: <text> with <lines> after its first <line>.
insert_after() {
  INS="$3" awk -v l="$2" '{ print } !done && $0 == l { print ENVIRON["INS"]; done = 1 }' <<<"$1"
}
# The pre-step's calls follow the scope probe (the first $CP_PS) and precede
# the guard's config render. Green running: green probe, home stop.
RESUME_HEAD="$(insert_after "$CUT_PREFIX" "$CP_PS" "$GREEN_PS"$'\n'"$HOME_STOP")"
RESUME_READY="$(printf '%s\n' "$RESUME_HEAD" "$CURL_8081" "$STATUS_RUN" "$HOME_UP" "$CURL_8080" 'ctlflip 8080' "$GREEN_REAP")"
# The drained resume: foreign guard, reap, drained check, home up, gate, flip
# back (its reap finds green already removed).
RESUME_DRAINED_TAIL="$(printf '%s\n' "$DAEMON_PS" "$CP_PS" "$GREEN_REAP" "$DAEMON_PS" "$HOME_UP" "$CURL_8080" 'ctlflip 8080' "$GREEN_REAP")"
# resumed_ok: exit 0 with Done., marker cleared, nginx back on 8080.
resumed_ok() { [[ $rc -eq 0 && ! -e "$RAIL_MARKER" ]] && saw 'Done.' && live_is 8080; }

recover_no_marker_live_8080_is_noop_no_docker_call() {
  need_flock recover_no_marker_live_8080_is_noop_no_docker_call || return 0
  local off_rc off_t off_o bad=''
  cut_prepare
  nats_running
  run_cut_at "$CTL" rebuild nats
  off_rc=$rc off_t="$(rail_transcript)" off_o="$(rail_out)"
  cut_prepare
  live_at 8080
  nats_running
  run_cut_at "$CUT_CTL" rebuild nats
  [[ $off_rc -eq 0 && $rc -eq 0 && -n "$off_t" && "$(rail_transcript)" == "$off_t" && "$(rail_out)" == "$off_o" ]] \
    || bad+=" [nats off-rc=$off_rc on-rc=$rc]"
  cut_prepare
  live_at 8080
  run_cut_at "$CUT_CTL" rebuild control-plane
  [[ $rc -eq 0 && "$(rail_transcript)" == "$CUT_HAPPY" ]] || bad+=" [control-plane rc=$rc: $(brief_out)]"
  # Mutation that turns this red: query docker for green in the pre-step
  # before its marker and live-port test.
  if [[ -z "$bad" ]]; then pass "recover_no_marker_live_8080_is_noop_no_docker_call"; else fail "recover_no_marker_live_8080_is_noop_no_docker_call:$bad"; fi
}

recover_marker_unparseable_refuses() {
  need_flock recover_marker_unparseable_refuses || return 0
  cut_prepare
  : >"$RAIL_MARKER"
  mkdir -p "$TMP/live"
  printf 'not an nginx vhost\n' >"$LIVE_VHOST"
  run_cut_at "$CUT_CTL" rebuild control-plane
  # Mutation that turns this red: read an unparseable live vhost as 8080 in
  # the pre-step (green is reaped, the marker cleared and the rebuild goes on).
  if named_error recovery-ambiguous && [[ -e "$RAIL_MARKER" && "$(last_calls 1)" == "$CP_PS" \
     && "$(mutating_calls)" == 0 ]] && ! has_call "$GREEN_PS"; then
    cut_report recover_marker_unparseable_refuses ok
  else
    cut_report recover_marker_unparseable_refuses bad
  fi
  rm -f "$RAIL_MARKER"
}

recover_live_8080_reaps_and_continues() {
  need_flock recover_live_8080_reaps_and_continues || return 0
  cut_prepare
  : >"$RAIL_MARKER"
  live_at 8080
  run_cut_at "$CUT_CTL" rebuild control-plane
  # Mutation that turns this red: leave the marker in the 8080 arm (the hook
  # then resumes instead of cutting over afresh).
  if [[ "$(rail_transcript)" == "$(insert_after "$CUT_HAPPY" "$CP_PS" "$GREEN_REAP")" ]] && resumed_ok; then
    cut_report recover_live_8080_reaps_and_continues ok
  else
    cut_report recover_live_8080_reaps_and_continues bad
  fi
  rm -f "$RAIL_MARKER"
}

# Review Focus 5: a stale marker must not block a full rebuild.
recover_stale_marker_unscoped_rebuild_continues() {
  need_flock recover_stale_marker_unscoped_rebuild_continues || return 0
  local off_rc off_t got
  cut_prepare
  run_cut_at "$CTL" rebuild
  off_rc=$rc off_t="$(rail_transcript)"
  cut_prepare
  : >"$RAIL_MARKER"
  live_at 8080
  run_cut_at "$CUT_CTL" rebuild
  got="$(rail_transcript | awk -v a="$GREEN_STOP" -v b="$GREEN_RM" '
    n == 0 && $0 == a { n = 1; next } n == 1 && $0 == b { n = 2; next } { print }')"
  # Mutation that turns this red: require the exact control-plane scope in the
  # 8080 arm too (the full rebuild refuses with cutover-incomplete).
  if [[ $rc -eq $off_rc && -n "$off_t" && "$got" == "$off_t" && ! -e "$RAIL_MARKER" \
     && $(ncalls "$GREEN_STOP") -eq 1 && "$(lines_after "$GREEN_STOP" 1)" == "$GREEN_RM" ]]; then
    cut_report recover_stale_marker_unscoped_rebuild_continues ok
  else
    cut_report recover_stale_marker_unscoped_rebuild_continues bad
    diff <(printf '%s\n' "$off_t") <(printf '%s\n' "$got") >&2 || true
  fi
  rm -f "$RAIL_MARKER"
}

recover_live_8081_wrong_scope_is_cutover_incomplete() {
  need_flock recover_live_8081_wrong_scope_is_cutover_incomplete || return 0
  local bad=''
  cut_prepare
  : >"$RAIL_MARKER"
  live_at 8081
  green_running
  nats_running
  run_cut_at "$CUT_CTL" rebuild nats
  { named_error cutover-incomplete && [[ -e "$RAIL_MARKER" && "$(last_calls 1)" == "$GREEN_PS" \
    && "$(mutating_calls)" == 0 ]] && live_is 8081; } || bad+=" [nats rc=$rc: $(brief_out)]"
  cut_prepare
  live_at 8081
  green_running
  nats_running
  run_cut_at "$CUT_CTL" rebuild control-plane nats
  { named_error cutover-incomplete && [[ ! -e "$RAIL_MARKER" && "$(last_calls 1)" == "$GREEN_PS" \
    && "$(mutating_calls)" == 0 ]] && live_is 8081; } || bad+=" [control-plane nats rc=$rc: $(brief_out)]"
  rm -f "$RAIL_MARKER"
  # Mutation that turns this red: drop the exact-scope test in the
  # green-running arm (home is stopped under another scope).
  if [[ -z "$bad" ]]; then pass "recover_live_8081_wrong_scope_is_cutover_incomplete"; else fail "recover_live_8081_wrong_scope_is_cutover_incomplete:$bad"; fi
}

recover_live_8081_without_marker_writes_marker_and_resumes() {
  need_flock recover_live_8081_without_marker_writes_marker_and_resumes || return 0
  cut_prepare
  live_at 8081
  green_running
  run_cut_at "$CUT_CTL" rebuild control-plane
  # Mutation that turns this red: skip the marker write in the green-running
  # arm (the hook then cuts over afresh beside the running green).
  if [[ "$(rail_transcript)" == "$RESUME_READY" ]] && resumed_ok; then
    cut_report recover_live_8081_without_marker_writes_marker_and_resumes ok
  else
    cut_report recover_live_8081_without_marker_writes_marker_and_resumes bad
    diff <(printf '%s\n' "$RESUME_READY") <(rail_transcript) >&2 || true
  fi
  rm -f "$RAIL_MARKER"
}

recover_live_8081_green_ready_current_resumes() {
  need_flock recover_live_8081_green_ready_current_resumes || return 0
  cut_prepare
  : >"$RAIL_MARKER"
  live_at 8081
  green_running
  run_cut_at "$CUT_CTL" rebuild control-plane
  # Mutation that turns this red: ignore a present marker at the hook (the
  # cutover starts afresh).
  if [[ "$(rail_transcript)" == "$RESUME_READY" ]] && resumed_ok; then
    cut_report recover_live_8081_green_ready_current_resumes ok
  else
    cut_report recover_live_8081_green_ready_current_resumes bad
  fi
  rm -f "$RAIL_MARKER"
}

recover_live_8081_green_ready_pending_goes_drained_then_flips_home() {
  need_flock recover_live_8081_green_ready_pending_goes_drained_then_flips_home || return 0
  cut_prepare
  : >"$RAIL_MARKER"
  live_at 8081
  green_running
  stub_reply docker "$STATUS_PAT" 3 'pending 12 13\n'
  run_cut_at "$CUT_CTL" rebuild control-plane
  # Mutation that turns this red: drop the status check from the resume's
  # ready test (home starts beside green with a migration pending).
  if [[ "$(rail_transcript)" == "$(printf '%s\n' "$RESUME_HEAD" "$CURL_8081" "$STATUS_RUN" "$RESUME_DRAINED_TAIL")" ]] \
     && resumed_ok; then
    cut_report recover_live_8081_green_ready_pending_goes_drained_then_flips_home ok
  else
    cut_report recover_live_8081_green_ready_pending_goes_drained_then_flips_home bad
  fi
  rm -f "$RAIL_MARKER"
}

# "stop" then "rebuild" is the last resort: nothing serves on 8081, so any
# scope flips home first and then rebuilds as requested.
recover_live_8081_green_absent_flips_home_and_continues_any_scope() {
  need_flock recover_live_8081_green_absent_flips_home_and_continues_any_scope || return 0
  local off_rc off_t want m bad=''
  cut_prepare
  nats_running
  run_cut_at "$CTL" rebuild nats
  off_rc=$rc off_t="$(rail_transcript)"
  want="$(insert_after "$off_t" "$NATS_PS" "$GREEN_PS"$'\n''ctlflip 8080'$'\n'"$GREEN_REAP")"
  for m in marker no-marker; do
    cut_prepare
    [[ "$m" == no-marker ]] || : >"$RAIL_MARKER"
    live_at 8081
    nats_running
    run_cut_at "$CUT_CTL" rebuild nats
    { [[ $off_rc -eq 0 && "$(rail_transcript)" == "$want" ]] && resumed_ok; } || bad+=" [$m rc=$rc: $(brief_out)]"
  done
  rm -f "$RAIL_MARKER"
  # Mutations that turn this red: test the scope before the green probe (nats
  # refuses); skip writing a missing marker (the flip is refused without it).
  if [[ -z "$bad" ]]; then pass "recover_live_8081_green_absent_flips_home_and_continues_any_scope"; else fail "recover_live_8081_green_absent_flips_home_and_continues_any_scope:$bad"; fi
}

recover_live_8081_green_unready_goes_drained_then_flips_home() {
  need_flock recover_live_8081_green_unready_goes_drained_then_flips_home || return 0
  cut_prepare
  : >"$RAIL_MARKER"
  live_at 8081
  green_running
  stub_exit curl '*8081/readyz' 22
  run_cut_at "$CUT_CTL" rebuild control-plane
  # Mutation that turns this red: ignore the 8081 readiness result in the
  # resume (the ready path starts home beside an unready green).
  if [[ "$(rail_transcript | grep -vFx -- "$CURL_8081")" == "$RESUME_HEAD"$'\n'"$RESUME_DRAINED_TAIL" ]] \
     && has_call "$CURL_8081" && [[ $(ncalls "$STATUS_RUN") -eq 0 ]] && resumed_ok; then
    cut_report recover_live_8081_green_unready_goes_drained_then_flips_home ok
  else
    cut_report recover_live_8081_green_unready_goes_drained_then_flips_home bad
  fi
  rm -f "$RAIL_MARKER"
}

# The pre-step's calls come after the scope probe and before the guard's
# render, so nothing it does precedes the first-activation boundary's view.
recover_runs_before_routine_guard() {
  need_flock recover_runs_before_routine_guard || return 0
  local bad=''
  cut_prepare
  : >"$RAIL_MARKER"
  live_at 8080
  nats_running
  run_cut_at "$CUT_CTL" rebuild nats
  [[ $rc -eq 0 && "$(lines_after "$NATS_PS" 3)" == "$GREEN_REAP"$'\n'"$GUARD_CONFIG" ]] \
    || bad+=" [8080 rc=$rc: $(lines_after "$NATS_PS" 3 | tr '\n' ';')]"
  cut_prepare
  live_at 8081
  green_running
  run_cut_at "$CUT_CTL" rebuild control-plane
  [[ $rc -eq 0 && "$(lines_after "$CP_PS" 3)" == "$GREEN_PS"$'\n'"$HOME_STOP"$'\n'"$GUARD_CONFIG" ]] \
    || bad+=" [8081 rc=$rc: $(lines_after "$CP_PS" 3 | tr '\n' ';')]"
  rm -f "$RAIL_MARKER"
  # Mutation that turns this red: move the pre-step call after
  # activity_history_routine_guard.
  if [[ -z "$bad" ]]; then pass "recover_runs_before_routine_guard"; else fail "recover_runs_before_routine_guard:$bad"; fi
}

recover_home_stop_failure_keeps_marker() {
  need_flock recover_home_stop_failure_keeps_marker || return 0
  cut_prepare
  live_at 8081
  green_running
  stub_exit docker '*stop --timeout 45 control-plane' 1
  run_cut_at "$CUT_CTL" rebuild control-plane
  # Mutation that turns this red: ignore the home stop's status in the
  # green-running arm (the build and the resume run).
  if named_error old-colour-stop-failed && [[ -e "$RAIL_MARKER" && "$(last_calls 1)" == "$HOME_STOP" ]] \
     && ! has_call "$C build control-plane" && live_is 8081; then
    cut_report recover_home_stop_failure_keeps_marker ok
  else
    cut_report recover_home_stop_failure_keeps_marker bad
  fi
  rm -f "$RAIL_MARKER"
}

# A control-plane of another Compose project on this daemon: the daemon-wide
# `docker ps` lists an ID this project's `compose ps` does not.
FOREIGN_ID=f0f0f0f0f0f0
foreign_running() { stub_out docker 'ps --filter label=com.docker.compose.service=control-plane *' "$FOREIGN_ID\\n"; }
FOREIGN_REFUSAL='privacy-defaults: another Compose project runs a control-plane on this Docker daemon; refusing before stopping this one'

foreign_project_refused_before_green_start() {
  need_flock foreign_project_refused_before_green_start || return 0
  cut_prepare
  foreign_running
  run_cut_at "$CUT_CTL" rebuild control-plane
  # Mutation that turns this red: drop `privacy_defaults_refuse_foreign_control_plane
  # || exit 1` from control_plane_bluegreen_cutover (green starts and the
  # round trip runs beside the foreign control-plane).
  if [[ $rc -eq 1 && "$(rail_transcript)" == "$(printf '%s\n' "$CUT_PREFIX" "$STATUS_RUN" "$DAEMON_PS" "$CP_PS")" \
     && ! -e "$RAIL_MARKER" ]] && saw "$FOREIGN_REFUSAL" \
     && ! grep -qE -e ' run --detach ' -e '^ctlflip ' -e ' stop ' <<<"$(rail_transcript)"; then
    cut_report foreign_project_refused_before_green_start ok
  else
    cut_report foreign_project_refused_before_green_start bad
  fi
}

foreign_project_refused_on_drained_resume() {
  need_flock foreign_project_refused_on_drained_resume || return 0
  cut_prepare
  : >"$RAIL_MARKER"
  live_at 8081
  green_running
  foreign_running
  stub_exit curl '*8081/readyz' 22
  run_cut_at "$CUT_CTL" rebuild control-plane
  # Mutation that turns this red: drop `privacy_defaults_refuse_foreign_control_plane
  # || exit 1` from control_plane_bluegreen_resume (green is reaped, then the
  # drained check refuses with home and green both down).
  if [[ $rc -eq 1 && "$(rail_transcript | grep -vFx -- "$CURL_8081")" == "$(printf '%s\n' "$RESUME_HEAD" "$DAEMON_PS" "$CP_PS")" \
     && -e "$RAIL_MARKER" ]] && has_call "$CURL_8081" && saw "$FOREIGN_REFUSAL" && live_is 8081; then
    cut_report foreign_project_refused_on_drained_resume ok
  else
    cut_report foreign_project_refused_on_drained_resume bad
  fi
  rm -f "$RAIL_MARKER"
}

# The leftover-green reap before the start fails with a Docker error other than
# "No such container", once at the stop and once at the remove.
leftover_green_unremovable_keeps_home_serving() {
  need_flock leftover_green_unremovable_keeps_home_serving || return 0
  local leg pat want n bad=''
  for leg in stop rm; do
    cut_prepare
    if [[ "$leg" == stop ]]; then
      pat="${GREEN_STOP#docker }" want="$GREEN_STOP" n=1
      stub_err docker "$pat" 'Error response from daemon: cannot stop container: permission denied\n'
    else
      pat="${GREEN_RM#docker }" want="$GREEN_REAP" n=2
      stub_err docker "$pat" 'Error response from daemon: removal of container is already in progress\n'
    fi
    stub_exit docker "$pat" 1
    run_cut_at "$CUT_CTL" rebuild control-plane
    { named_error standby-not-ready \
      && saw "the previous control-plane still serves on 8080; a leftover green could not be removed; run 'rebuild control-plane' to recover." \
      && [[ ! -e "$RAIL_MARKER" && "$(last_calls "$n")" == "$want" ]] \
      && ! has_call "$HOME_STOP" && ! grep -qE -e ' run --detach ' -e '^ctlflip ' <<<"$(rail_transcript)"; } \
      || bad+=" [$leg rc=$rc: $(brief_out)]"
  done
  # Mutation that turns this red: take the drained path (return 10) when the
  # leftover green cannot be removed (home is stopped, and the drained check
  # would count the surviving green and refuse with nothing serving).
  if [[ -z "$bad" ]]; then pass "leftover_green_unremovable_keeps_home_serving"; else fail "leftover_green_unremovable_keeps_home_serving:$bad"; last_calls 6 >&2; fi
}

char_rebuild_control_plane_transcript
char_wait_healthy_curl_argv
char_nginx_render_hash
compose_oneoff_contract
render_rail_on_8080_equals_char_hash
render_rail_on_8081_changes_only_the_server_line
render_rail_off_ignores_port_argument
render_rejections
live_port_cases
live_port_roundtrip
live_vhost_literal_matches_nginx_dst
reentry_rejects_without_fd9
reentry_rejects_fd9_on_other_file
reentry_rejects_unheld_lock
reentry_rejects_foreign_holder
reentry_rejects_inexact_argv
reentry_genuine_accepted
flip_form_refused_when_rail_off
flip_form_refused_without_marker_file
flip_argument_without_reentry_is_usage_error
switch_unset_compose_cmd_identical
switch_single_identical
switch_bluegreen_compose_cmd_identical
switch_invalid_exits_2
switch_quoted_or_padded_value_exits_2
switch_forced_single_in_selfhost_mode
switch_forced_single_outside_production
rail_refused_media_bluegreen
rail_refused_no_flock
rail_refused_no_state_dir
guards_unchanged_from_baseline
marker_absent_no_extra_calls
marker_rail_off_refuses_rebuild
marker_rail_off_refuses_freshstart
marker_rail_off_allows_sudoers_sync
marker_rail_on_refuses_restart
marker_rail_on_allows_nginx_reload_and_stop
stop_with_marker_reaps_green_and_keeps_marker
stop_without_marker_unchanged
cutover_happy_transcript
cutover_gates_on_readyz_not_health
cutover_only_for_exact_scope
cutover_not_run_with_rail_off
gate_cases
gate_live_port_unparseable
gate_marker_write_failure
gate_green_missing_from_compose_ps
stop_follows_flip_leg1
reap_follows_flip_leg2
int_during_flip_still_stops
err_standby_start_failed
err_standby_image_mismatch
err_standby_not_ready
err_flip_failed_leg1
err_flip_failed_leg1_live_port_not_8080_keeps_green
err_old_colour_stop_failed
err_home_not_ready_status_changed
err_home_not_ready_up_failed
err_home_not_ready_readyz
err_flip_failed_leg2
wait_healthy_rail_on_uses_live_port
wait_healthy_rail_on_unparseable_exits_1
recover_no_marker_live_8080_is_noop_no_docker_call
recover_marker_unparseable_refuses
recover_live_8080_reaps_and_continues
recover_stale_marker_unscoped_rebuild_continues
recover_live_8081_wrong_scope_is_cutover_incomplete
recover_live_8081_without_marker_writes_marker_and_resumes
recover_live_8081_green_ready_current_resumes
recover_live_8081_green_ready_pending_goes_drained_then_flips_home
recover_live_8081_green_absent_flips_home_and_continues_any_scope
recover_live_8081_green_unready_goes_drained_then_flips_home
recover_runs_before_routine_guard
recover_home_stop_failure_keeps_marker
foreign_project_refused_before_green_start
foreign_project_refused_on_drained_resume
leftover_green_unremovable_keeps_home_serving
exit $((fails > 0))
