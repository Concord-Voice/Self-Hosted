#!/usr/bin/env bash
# Real-nginx proof for the control-plane blue/green flip: two stub upstreams
# (control_plane_bluegreen_upstream.py) sit on 8080 (blue) and 8081 (green)
# behind a real nginx whose `upstream control_plane` block comes from the real
# render_nginx_config and whose /health and /api/v1/ws locations are cut from
# the shipped vhost. The flip is the controller's sequence: render 8081,
# `nginx -t`, `nginx -s reload` (HUP), then SIGTERM blue.
#
# Needs nginx and python3 on PATH and loopback ports 8080, 8081 and
# LISTEN_PORT free. Locally a missing prerequisite is a SKIP; with CI set it is
# a FAIL.
set -euo pipefail
fails=0
pass() { printf 'PASS: %s\n' "$1"; }
fail() { printf 'FAIL: %s\n' "$1" >&2; fails=$((fails + 1)); }
skip_or_fail() {
  if [[ -n "${CI:-}" ]]; then
    printf 'FAIL: %s (CI is set, so this is not a skip)\n' "$1" >&2
    exit 1
  fi
  printf 'SKIP: test-control-plane-bluegreen-nginx: %s\n' "$1"
  exit 0
}

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CTL_SRC="$ROOT/infrastructure/deploy/concord-ctl.sh"
VHOST_SRC="$ROOT/infrastructure/deploy/nginx/concordvoice.conf"
PY="$ROOT/infrastructure/deploy/tests/control_plane_bluegreen_upstream.py"
LISTEN_PORT=18088
HSTS_DEFAULT_VALUE='max-age=63072000; includeSubDomains; preload'

command -v nginx >/dev/null 2>&1 || skip_or_fail "nginx is not on PATH"
command -v python3 >/dev/null 2>&1 || skip_or_fail "python3 is not on PATH"
busy="$(python3 "$PY" ports-free 8080 8081 "$LISTEN_PORT")" \
  || skip_or_fail "loopback ports already in use: $busy"

TMP="$(mktemp -d)"
# nginx workers drop to an unprivileged user when the harness runs as root;
# they must be able to traverse to the temp paths below.
chmod 0755 "$TMP"
NG="$TMP/nginx"
mkdir -p "$NG/logs" "$NG/tmp" "$TMP/render"
NGX=(nginx -p "$NG/" -c "$NG/nginx.conf")
# -e (nginx >= 1.19.5) keeps the startup error log out of /var/log/nginx.
if nginx -h 2>&1 | grep -q -- '-e filename'; then NGX+=(-e "$NG/logs/error.log"); fi
pids=()
finished=0
cleanup() {
  local pid
  : >"$TMP/stop" 2>/dev/null || true
  exec 7>&- 8>&-
  # The nginx master is not our child; it removes its pid file as it exits.
  if [[ -s "$NG/nginx.pid" ]]; then
    kill -TERM "$(cat "$NG/nginx.pid")" 2>/dev/null || true
    wait_until 5 test ! -e "$NG/nginx.pid" || true
  fi
  # Stubs drain and exit on SIGTERM; anything still running after that is killed.
  for pid in ${pids[@]+"${pids[@]}"}; do kill -TERM "$pid" 2>/dev/null || true; done
  for pid in ${pids[@]+"${pids[@]}"}; do
    wait_until 6 gone "$pid" || kill -KILL "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  done
  rm -rf "$TMP"
  if (( !finished )); then printf 'FAIL: harness aborted before the cases finished\n' >&2; fi
}
trap cleanup EXIT
# A WebSocket client that died must not take the harness down with SIGPIPE.
trap '' PIPE

die() {
  fail "$1"
  if [[ -s "$NG/logs/error.log" ]]; then tail -n 20 "$NG/logs/error.log" >&2; fi
  finished=1
  exit 1
}

# wait_until <seconds> <command...>: bounded poll until the command succeeds.
wait_until() {
  local deadline=$((SECONDS + $1))
  shift
  until "$@"; do
    (( SECONDS < deadline )) || return 1
    sleep 0.05
  done
}
gone() { ! kill -0 "$1" 2>/dev/null; }
get_is() { [[ "$(python3 "$PY" get --port "$1")" == "$2" ]]; }
has_line() { grep -qxF -- "$2" "$1" 2>/dev/null; }
last_is() { [[ "$(grep -v '^#' "$1" 2>/dev/null | tail -n 1)" == "$2" ]]; }
lines_in() { local n; n="$(grep -vc '^#' "$1" 2>/dev/null)" || true; printf '%s' "${n:-0}"; }
at_least() { (( $(lines_in "$1") >= $2 )); }
# bad_in <probe-file>: requests that were not a 200 from blue or green.
bad_in() {
  local n
  n="$(grep -v '^#' "$1" | grep -cvE '^200 (blue|green)$')" || true
  printf '%s' "${n:-0}"
}
any_bad() { (( $(bad_in "$1") + $(bad_in "$2") > 0 )); }
show_bad() { grep -v '^#' "$1" | grep -vE '^200 (blue|green)$' | sort | uniq -c | head -n 5 >&2 || true; }

# extract_fn <name>: print the function declared as `    name() {` (4-space
# indent, inside the nginx-reload arm) through its closing `    }`.
extract_fn() {
  awk -v n="$1" '
    $0 == "    " n "() {" { on = 1 }
    on { print }
    on && $0 == "    }" { exit }
  ' "$CTL_SRC"
}
# cut_block <file> <open-line> <close-line>: print the block from the exact line
# <open-line> through the first exact <close-line> after it. Fails loudly when
# the opening line is missing or repeated, or the block never closes.
cut_block() {
  awk -v op="$2" -v cl="$3" '
    $0 == op { opens++; if (!closed) on = 1 }
    on { print }
    on && $0 == cl { on = 0; closed = 1 }
    END { if (opens != 1 || !closed) exit 1 }
  ' "$1"
}

# render_port <port>: the real render_nginx_config with the rail on, writing
# $TMP/render/concordvoice.conf with the control_plane server on <port>.
render_port() {
  local enc rnd
  enc="$(extract_fn encode_hsts_for_nginx)"
  rnd="$(extract_fn render_nginx_config)"
  [[ -n "$enc" && -n "$rnd" ]] || return 1
  (
    # Variables read by the extracted render_nginx_config.
    export NGINX_SRC="$VHOST_SRC" NGINX_RENDER_DIR="$TMP/render" \
      NGINX_RENDER="$TMP/render/concordvoice.conf" CONTROL_PLANE_BLUEGREEN=1
    eval "$enc"
    eval "$rnd"
    render_nginx_config "$HSTS_DEFAULT_VALUE" "$1"
  )
}

# Cut once from the shipped vhost: the two Upgrade-validation maps the
# WebSocket location depends on, and the /health and WebSocket locations.
# The anchors are literal vhost lines; their nginx variables must not expand.
# shellcheck disable=SC2016
UPGRADE_MAP="$(cut_block "$VHOST_SRC" 'map $http_upgrade $validated_upgrade {' '}')" \
  || die "shipped vhost: no single 'map \$http_upgrade \$validated_upgrade' block"
# shellcheck disable=SC2016
CONNECTION_MAP="$(cut_block "$VHOST_SRC" 'map $validated_upgrade $connection_upgrade {' '}')" \
  || die "shipped vhost: no single 'map \$validated_upgrade \$connection_upgrade' block"
HEALTH_LOC="$(cut_block "$VHOST_SRC" '    location = /health {' '    }')" \
  || die "shipped vhost: no single 'location = /health' block"
WS_LOC="$(cut_block "$VHOST_SRC" '    location /api/v1/ws {' '    }')" \
  || die "shipped vhost: no single 'location /api/v1/ws' block"
# The location logs to absolute /var/log/nginx paths; keep them under $TMP.
WS_LOC="${WS_LOC//\/var\/log\/nginx\//$NG/logs/}"
[[ "$WS_LOC" != */var/log/* ]] || die "WebSocket location still logs outside \$TMP"

# build_conf: wrap the rendered control_plane upstream and the cut blocks in a
# self-contained nginx.conf under $NG.
build_conf() {
  local upstream t
  upstream="$(cut_block "$TMP/render/concordvoice.conf" 'upstream control_plane {' '}')" || return 1
  {
    printf 'worker_processes 1;\ndaemon on;\npid %s/nginx.pid;\n' "$NG"
    printf 'error_log %s/logs/error.log warn;\nevents {\n    worker_connections 256;\n}\n' "$NG"
    printf 'http {\n    access_log off;\n'
    for t in client_body proxy fastcgi uwsgi scgi; do
      printf '    %s_temp_path %s/tmp/%s;\n' "$t" "$NG" "$t"
    done
    # Stand-ins for the vhost's top-level definitions the cut locations use.
    # The shipped api zone is 30r/s burst 20; the unthrottled probe loops run
    # thousands of requests a second, so the stand-in rate is raised until
    # limit_req cannot fire and only upstream failures can show as non-200.
    cat <<'NGINX'
    map $host $validated_host { default $host; }  # nosemgrep: generic.nginx.security.request-host-used.request-host-used -- test stand-in, loopback only
    map $http_user_agent $is_scanner { default 0; }
    map $binary_remote_addr $source_addr_bucket {
        "~^(\C{8})" $1;
        default $binary_remote_addr;
    }
    log_format timed_ws '[$time_local] "$request_method $uri $server_protocol" $status';
    limit_req_zone $source_addr_bucket zone=ws_handshake:1m rate=20r/s;
    limit_req_zone $source_addr_bucket zone=api:1m rate=1000000r/s;
NGINX
    printf '%s\n' "$UPGRADE_MAP" "$CONNECTION_MAP" "$upstream"
    printf '    server {\n        listen 127.0.0.1:%s;\n' "$LISTEN_PORT"
    printf '%s\n' "$HEALTH_LOC" "$WS_LOC"
    printf '    }\n}\n'
  } >"$NG/nginx.conf"
  grep -qxF "    server 127.0.0.1:$1;" "$NG/nginx.conf"
}

# flip <port>: the controller's sequence — render, validate, graceful reload.
flip() {
  render_port "$1" && build_conf "$1" && "${NGX[@]}" -t -q && "${NGX[@]}" -s reload
}

# Every background child closes fds 7 and 8 (the WebSocket fifo write ends):
# a child that inherited one would hold the fifo open, so closing it in the
# harness would never reach the client as EOF.
# start_stub <port> <name>: start a stub upstream and wait for its /health.
start_stub() {
  python3 "$PY" serve --port "$1" --name "$2" 2>>"$TMP/$2.log" 7>&- 8>&- &
  STUB_PID=$!
  pids+=("$STUB_PID")
  wait_until 5 get_is "$1" "200 $2"
}

# start_probes <tag>: one fresh-connection and one keep-alive /health loop.
start_probes() {
  local mode
  rm -f "$TMP/stop"
  PROBE_PIDS=()
  for mode in fresh keepalive; do
    python3 "$PY" probe --port "$LISTEN_PORT" --mode "$mode" \
      --out "$TMP/$1.$mode" --stop "$TMP/stop" 7>&- 8>&- &
    PROBE_PIDS+=("$!")
    pids+=("$!")
  done
  wait_until 5 at_least "$TMP/$1.fresh" 20 && wait_until 5 at_least "$TMP/$1.keepalive" 20
}
stop_probes() {
  local pid
  : >"$TMP/stop"
  for pid in "${PROBE_PIDS[@]}"; do wait "$pid" || return 1; done
}
# probes_green <tag>: both loops are now answered by green.
probes_green() {
  wait_until 10 last_is "$TMP/$1.fresh" '200 green' \
    && wait_until 10 last_is "$TMP/$1.keepalive" '200 green'
}

# --- Setup: blue live on 8080 behind nginx, green warm on 8081 --------------
start_stub 8080 blue || die "blue stub did not answer /health on 8080"
blue_pid=$STUB_PID
start_stub 8081 green || die "green stub did not answer /health on 8081"
render_port 8080 || die "render_nginx_config refused port 8080"
build_conf 8080 || die "rendered upstream control_plane does not name 127.0.0.1:8080"
"${NGX[@]}" -t -q || die "nginx -t rejected the 8080 config"
"${NGX[@]}" || die "nginx did not start"
wait_until 5 get_is "$LISTEN_PORT" '200 blue' || die "nginx did not proxy /health to blue"

# --- The flip ----------------------------------------------------------------
# WebSocket 1 is opened through nginx before the flip.
mkfifo "$TMP/ws1.in" "$TMP/ws2.in"
python3 "$PY" ws-client --port "$LISTEN_PORT" <"$TMP/ws1.in" >"$TMP/ws1.out" 2>&1 7>&- 8>&- &
ws1_pid=$!
pids+=("$ws1_pid")
exec 7>"$TMP/ws1.in"
ws1_hello=0 ws1_reload=0 ws1_drain=0 ws1_exit=0 blue_alive=0
wait_until 5 has_line "$TMP/ws1.out" 'hello blue' && ws1_hello=1

start_probes flip || die "the /health loops did not start through nginx"
flip 8081 || die "render 8081 / nginx -t / nginx -s reload failed"
flip_green=0
probes_green flip && flip_green=1

printf 'after-reload\n' >&7 2>/dev/null || true
wait_until 5 has_line "$TMP/ws1.out" 'echo after-reload' && ws1_reload=1
kill -TERM "$blue_pid" 2>/dev/null || true
# Blue is draining: its listener is closed, but WebSocket 1 is still its own.
kill -0 "$blue_pid" 2>/dev/null && blue_alive=1
printf 'during-drain\n' >&7 2>/dev/null || true
wait_until 5 has_line "$TMP/ws1.out" 'echo during-drain' && ws1_drain=1
exec 7>&-
wait "$ws1_pid" && ws1_exit=1
blue_exit=0
if wait_until 8 gone "$blue_pid"; then
  wait "$blue_pid" || blue_exit=$?
else
  blue_exit=timeout
fi

# WebSocket 2 is opened after the reload.
python3 "$PY" ws-client --port "$LISTEN_PORT" <"$TMP/ws2.in" >"$TMP/ws2.out" 2>&1 7>&- 8>&- &
ws2_pid=$!
pids+=("$ws2_pid")
exec 8>"$TMP/ws2.in"
ws2_hello=0 ws2_echo=0 ws2_exit=0
wait_until 5 has_line "$TMP/ws2.out" 'hello green' && ws2_hello=1
printf 'new-socket\n' >&8 2>/dev/null || true
wait_until 5 has_line "$TMP/ws2.out" 'echo new-socket' && ws2_echo=1
exec 8>&-
wait "$ws2_pid" && ws2_exit=1

# Keep both loops running on green past blue's exit, then stop them.
fresh_n="$(lines_in "$TMP/flip.fresh")"
keep_n="$(lines_in "$TMP/flip.keepalive")"
wait_until 5 at_least "$TMP/flip.fresh" $((fresh_n + 20)) || true
wait_until 5 at_least "$TMP/flip.keepalive" $((keep_n + 20)) || true
stop_probes || fail "a /health loop exited non-zero"

flip_zero_non_200() {
  local f k tail_ok=1 mode
  f="$(bad_in "$TMP/flip.fresh")"
  k="$(bad_in "$TMP/flip.keepalive")"
  for mode in fresh keepalive; do
    has_line "$TMP/flip.$mode" '200 blue' || tail_ok=0
    [[ "$(grep -v '^#' "$TMP/flip.$mode" | tail -n 5 | sort -u)" == '200 green' ]] || tail_ok=0
  done
  local detail
  detail="fresh=$(lines_in "$TMP/flip.fresh") req/$f non-200, keepalive=$(lines_in "$TMP/flip.keepalive") req/$k non-200, $(tail -n 1 "$TMP/flip.keepalive")"
  if (( f == 0 && k == 0 && tail_ok && flip_green )) && [[ "$blue_exit" == 0 ]]; then
    pass "flip_zero_non_200 ($detail)"
  else
    fail "flip_zero_non_200 ($detail, blue-then-green=$tail_ok, switched=$flip_green, blue exit=$blue_exit)"
    show_bad "$TMP/flip.fresh"
    show_bad "$TMP/flip.keepalive"
  fi
}
websocket_survives_reload() {
  if (( ws1_hello && ws1_reload && blue_alive && ws1_drain && ws1_exit )); then
    pass "websocket_survives_reload"
  else
    fail "websocket_survives_reload (hello=$ws1_hello after-reload=$ws1_reload blue-draining=$blue_alive during-drain=$ws1_drain clean-close=$ws1_exit)"
    sed 's/^/  ws1: /' "$TMP/ws1.out" >&2
  fi
}
new_websocket_lands_on_new_colour() {
  if (( ws2_hello && ws2_echo && ws2_exit )); then
    pass "new_websocket_lands_on_new_colour"
  else
    fail "new_websocket_lands_on_new_colour (hello-green=$ws2_hello echo=$ws2_echo clean-close=$ws2_exit)"
    sed 's/^/  ws2: /' "$TMP/ws2.out" >&2
  fi
}
flip_zero_non_200
websocket_survives_reload
new_websocket_lands_on_new_colour

# --- Negative control: blue dies BEFORE the reload ----------------------------
negative_control() {
  local f k
  if ! start_stub 8080 blue; then
    fail "negative_control: blue stub did not restart on 8080"
    return
  fi
  blue_pid=$STUB_PID
  if ! flip 8080 || ! wait_until 10 get_is "$LISTEN_PORT" '200 blue'; then
    fail "negative_control: could not flip nginx back to blue"
    return
  fi
  if ! start_probes neg; then
    fail "negative_control: the /health loops did not start"
    return
  fi
  kill -TERM "$blue_pid" 2>/dev/null || true
  wait_until 8 gone "$blue_pid" || true
  # Bounded: a probe that cannot see a failure times out here and fails below.
  wait_until 5 any_bad "$TMP/neg.fresh" "$TMP/neg.keepalive" || true
  if flip 8081; then probes_green neg || true; fi
  stop_probes || true
  f="$(bad_in "$TMP/neg.fresh")"
  k="$(bad_in "$TMP/neg.keepalive")"
  if (( f + k >= 1 )); then
    pass "negative_control (fresh=$f non-200, keepalive=$k non-200)"
  else
    fail "negative_control: blue killed before the reload, yet no non-200 was observed"
  fi
}
negative_control

finished=1
if (( fails )); then
  printf '%d case(s) failed\n' "$fails" >&2
  exit 1
fi
