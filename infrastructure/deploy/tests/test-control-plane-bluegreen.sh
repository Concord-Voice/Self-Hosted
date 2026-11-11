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
printf '%s\n' "${src/"$lock_probe"/$new_probe}" >"$CTL"
chmod +x "$CTL"

# Rail-on twin: its own project tree, so its cutover marker is separate, with a
# controller that differs from $CTL only in rewrite 5, the rail selector line.
RAIL_PROJECT="$TMP/rail-project"
RAIL_CTL="$RAIL_PROJECT/infrastructure/deploy/concord-ctl.sh"
RAIL_MARKER="$RAIL_PROJECT/infrastructure/deploy/.control-plane-cutover.local"
PLAIN_MARKER="$PROJECT/infrastructure/deploy/.control-plane-cutover.local"
cp -R "$PROJECT" "$RAIL_PROJECT"
src="$(cat "$CTL")"
fixture_rewrite 'CONTROL_PLANE_BLUEGREEN=0' 'CONTROL_PLANE_BLUEGREEN=1'
printf '%s\n' "$src" >"$RAIL_CTL"

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
  expect_live_port live_port_missing_file_is_8080 "$TMP/live-none/concordvoice" 8080
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
exit $((fails > 0))
