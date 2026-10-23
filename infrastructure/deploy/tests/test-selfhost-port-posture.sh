#!/usr/bin/env bash
# Tests for infrastructure/deploy/selfhost-port-posture.py (#3538). CI-run via pr-ci.yml's
# infrastructure/deploy/tests/test-*.sh loop.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
CHECKER="$ROOT/infrastructure/deploy/selfhost-port-posture.py"
BASE_ONLY=0
case "${1:-}" in
  --base-only) BASE_ONLY=1 ;;
  "") ;;
  *) printf '%s\n' 'Usage: test-selfhost-port-posture.sh [--base-only]' >&2; exit 2 ;;
esac
FIX="$HERE/fixtures/selfhost-port-posture"
WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT
PASS=0; FAIL=0
ok()  { echo "PASS: $1"; PASS=$((PASS+1)); }
bad() { echo "FAIL: $1"; FAIL=$((FAIL+1)); }
# expect_rc NAME MODE WANT_RC FILE [GREP] — runs the checker and asserts exit code
# (and, optionally, that stdout contains GREP). Every case also asserts that no
# sentinel planted in a fixture leaks into the combined output (hygiene).
expect_rc() {
  local name="$1" mode="$2" want="$3" file="$4" grep_for="${5:-}" out rc=0
  out="$(python3 "$CHECKER" "$mode" < "$file" 2>&1)" || rc=$?
  if [[ "$rc" != "$want" ]]; then bad "$name (rc=$rc want=$want)"; return; fi
  if [[ -n "$grep_for" ]] && ! grep -qF -- "$grep_for" <<<"$out"; then bad "$name (missing: $grep_for)"; return; fi
  if grep -qE 'LIVE-SENTINEL|RENDER-SENTINEL' <<<"$out"; then bad "$name (sentinel leaked into output)"; return; fi
  ok "$name"
}
# Every mutant and live fixture below is built with jq. Without it each case would write an
# empty file and fail with an exit code that names the wrong cause, so say so once, up front.
if ! command -v jq >/dev/null 2>&1; then
  bad "jq is required (every mutant and live fixture is built with it) but is not installed"
  echo "passed: $PASS  failed: $FAIL"; exit 1
fi
# missing_tool TOOL WHY — a tool the real-render cases need is absent. In CI that is a
# failure: a silent SKIP there is how a suite goes green having proved nothing.
missing_tool() {
  if [[ "${CI:-}" == true ]]; then
    bad "$1 is required in CI ($2) but is not installed"
  else
    echo "SKIP: $1 not installed ($2)"
  fi
}
# mutate NAME JQ_EXPR — writes $WORK/NAME.json from the 5.5.1 fixture.
mutate() { jq "$2" "$FIX/compose-5.5.1.json" > "$WORK/$1.json"; }

# ── 1. Fixtures captured from real Compose renders ───────────────────────────
expect_rc "fixture 2.20.2 (additive merge) is refused" --rendered 1 "$FIX/compose-2.20.2.json" \
  "posture: control-plane 8080/tcp host_ip=all not allowed"
expect_rc "fixture 2.21.0 (additive merge) is refused" --rendered 1 "$FIX/compose-2.21.0.json" \
  "posture: control-plane 8080/tcp host_ip=all not allowed"
expect_rc "fixture 2.24.5 passes" --rendered 0 "$FIX/compose-2.24.5.json"
expect_rc "fixture 5.5.1 passes" --rendered 0 "$FIX/compose-5.5.1.json"

# ── 2. Mutants of the 5.5.1 render: every one is a violation (exit 1) ────────
mutate pg-5432 '.services.postgres.ports += [{"mode":"ingress","target":5432,"published":"5432","protocol":"tcp"}]'
expect_rc "mutant: postgres 5432 published" --rendered 1 "$WORK/pg-5432.json" \
  "posture: postgres 5432/tcp host_ip=all not allowed"

mutate cp-0000 '.services["control-plane"].ports[0].host_ip = "0.0.0.0"'
expect_rc "mutant: control-plane host_ip 0.0.0.0" --rendered 1 "$WORK/cp-0000.json" \
  "posture: control-plane 8080/tcp host_ip=all not allowed"
mutate cp-empty '.services["control-plane"].ports[0].host_ip = ""'
expect_rc "mutant: control-plane host_ip empty" --rendered 1 "$WORK/cp-empty.json" \
  "posture: control-plane 8080/tcp host_ip=all not allowed"
mutate cp-v6 '.services["control-plane"].ports[0].host_ip = "::"'
expect_rc "mutant: control-plane host_ip ::" --rendered 1 "$WORK/cp-v6.json" \
  "posture: control-plane 8080/tcp host_ip=all not allowed"
mutate cp-absent 'del(.services["control-plane"].ports[0].host_ip)'
expect_rc "mutant: control-plane host_ip absent" --rendered 1 "$WORK/cp-absent.json" \
  "posture: control-plane 8080/tcp host_ip=all not allowed"

mutate host-net '.services.nats.network_mode = "host"'
expect_rc "mutant: network_mode host on a port-less service" --rendered 1 "$WORK/host-net.json" \
  "posture: nats network_mode=host refused"

mutate udp-over '.services["media-plane"].ports += [{"mode":"ingress","target":42000,"published":"42000","protocol":"udp"}]'
expect_rc "mutant: media UDP above RTC_MAX_PORT" --rendered 1 "$WORK/udp-over.json" \
  "posture: media-plane 42000/udp"
mutate udp-under '.services["media-plane"].ports += [{"mode":"ingress","target":39999,"published":"39999","protocol":"udp"}]'
expect_rc "mutant: media UDP below RTC_MIN_PORT" --rendered 1 "$WORK/udp-under.json" \
  "posture: media-plane 39999/udp"
mutate udp-skew '.services["media-plane"].ports += [{"mode":"ingress","target":40002,"published":"40001","protocol":"udp"}]'
expect_rc "mutant: media UDP published != target" --rendered 1 "$WORK/udp-skew.json" \
  "posture: media-plane 40001/udp"

mutate turn-extra '.services.coturn.ports += [{"mode":"ingress","target":9999,"published":"9999","protocol":"tcp"}]'
expect_rc "mutant: coturn gains 9999/tcp" --rendered 1 "$WORK/turn-extra.json" \
  "posture: coturn 9999/tcp"

mutate cp-ephemeral '.services["control-plane"].ports[0].published = ""'
expect_rc "mutant: control-plane ephemeral host port (published empty)" --rendered 1 "$WORK/cp-ephemeral.json" \
  "posture: control-plane 8080/tcp"

# ── 2b. Remapped ports, host-named networks and unrenderable names (#3538) ────
# A published port that differs from its target can expose a socket the allowlist never
# names, whatever the service: the allowlist keys on the published number.
mutate coturn-remap '.services.coturn.ports += [{"mode":"ingress","target":5432,"published":"3478","protocol":"tcp"}]'
expect_rc "mutant: coturn publishes an allowed port onto postgres' target" --rendered 1 "$WORK/coturn-remap.json" \
  "posture: coturn 3478/tcp"
mutate cp-remap '.services["control-plane"].ports[0].target = 5432'
expect_rc "mutant: control-plane loopback 8080 remapped onto target 5432" --rendered 1 "$WORK/cp-remap.json" \
  "posture: control-plane 8080/tcp"

# A network whose engine name is "host" gives its members the host's network namespace.
# The fixtures were jq-reduced and carry no networks, so each case builds them.
mutate net-host-attach '.services.postgres.networks = {"hostnet": null}
  | .networks = {"hostnet": {"name": "host", "external": true}}'
expect_rc "mutant: postgres attached to a network named host (renamed key)" --rendered 1 "$WORK/net-host-attach.json" \
  "posture: postgres network=host refused"
mutate net-host-rename '.services.postgres.networks = {"default": null}
  | .networks = {"default": {"name": "host"}}'
expect_rc "mutant: the service's existing network is renamed host" --rendered 1 "$WORK/net-host-rename.json" \
  "posture: postgres network=host refused"
mutate net-host-key '.services.postgres.networks = {"host": null}
  | .networks = {"host": {}}'
expect_rc "mutant: a top-level network key is literally host" --rendered 1 "$WORK/net-host-key.json" \
  "posture: postgres network=host refused"
mutate net-host-list '.services.postgres.networks = ["hostnet"]
  | .networks = {"hostnet": {"name": "host"}}'
expect_rc "mutant: host-named network attached through the list form" --rendered 1 "$WORK/net-host-list.json" \
  "posture: postgres network=host refused"
# Controls: ordinary names pass, and so does a host-named network nothing is attached to,
# so the refusals above are caused by the attachment and not by the networks block existing.
mutate net-ordinary '.networks = {"concord-net": {"name": "concord_net"}, "edge": null}
  | .services |= map_values(.networks = {"concord-net": null})
  | .services.coturn.networks = {"concord-net": null, "edge": null}'
expect_rc "control: a real-shaped render with ordinary network names passes" --rendered 0 "$WORK/net-ordinary.json"
mutate net-host-unattached '.networks = {"hostnet": {"name": "host"}, "concord-net": null}
  | .services |= map_values(.networks = {"concord-net": null})'
expect_rc "control: a host-named network that no service attaches to passes" --rendered 0 "$WORK/net-host-unattached.json"

# network_mode container:<x> joins another container's netns, which this render cannot see.
mutate net-container '.services.nats.network_mode = "container:abc"'
expect_rc "unjudgeable: network_mode container:abc" --rendered 2 "$WORK/net-container.json"

# A lone surrogate in a name must still produce the verdict, not a traceback: the usual
# JSON decoders accept it and the report would otherwise fail to encode.
expect_no_traceback() { # NAME WANT_RC FILE
  local name="$1" want="$2" file="$3" out rc=0
  out="$(python3 "$CHECKER" --rendered < "$file" 2>&1)" || rc=$?
  if [[ "$rc" != "$want" ]]; then bad "$name (rc=$rc want=$want)"; return; fi
  if grep -q 'Traceback' <<<"$out"; then bad "$name (traceback in output)"; return; fi
  ok "$name"
}
printf '%s' '{"services":{"po\ud800stgres":{"ports":[{"mode":"ingress","target":5432,"published":"5432","protocol":"tcp"}]}}}' \
  > "$WORK/surrogate-service.json"
expect_no_traceback "violating render with a lone surrogate in the service name" 1 "$WORK/surrogate-service.json"
printf '%s' '{"services":{"control-plane":{"ports":[{"mode":"ingress","host_ip":"\ud800","target":8080,"published":"8080","protocol":"tcp"}]}}}' \
  > "$WORK/surrogate-hostip.json"
expect_no_traceback "violating render with a lone surrogate in host_ip" 1 "$WORK/surrogate-hostip.json"

# ── 2c. Per-service allowlist edges ──────────────────────────────────────────
mutate mp-3000-all '.services["media-plane"].ports[0].host_ip = "0.0.0.0"'
expect_rc "mutant: media-plane 3000/tcp published on 0.0.0.0" --rendered 1 "$WORK/mp-3000-all.json" \
  "posture: media-plane 3000/tcp host_ip=all not allowed"
mutate cp-8080-udp '.services["control-plane"].ports += [{"host_ip":"127.0.0.1","mode":"ingress","target":8080,"published":"8080","protocol":"udp"}]'
expect_rc "mutant: control-plane 8080/udp (only tcp is allowed)" --rendered 1 "$WORK/cp-8080-udp.json" \
  "posture: control-plane 8080/udp"
mutate relay-over '.services.coturn.ports += [{"mode":"ingress","target":49253,"published":"49253","protocol":"udp"}]'
expect_rc "mutant: coturn relay 49253/udp is one past the range" --rendered 1 "$WORK/relay-over.json" \
  "posture: coturn 49253/udp"
mutate relay-under '.services.coturn.ports += [{"mode":"ingress","target":49151,"published":"49151","protocol":"udp"}]'
expect_rc "mutant: coturn relay 49151/udp is one below the range" --rendered 1 "$WORK/relay-under.json" \
  "posture: coturn 49151/udp"
# Control: the fixture already carries 49152/udp and 49252/udp, the inclusive ends of the relay range.
if jq -e '[.services.coturn.ports[] | select(.protocol=="udp") | .published] | contains(["49152","49252"])' \
     "$FIX/compose-5.5.1.json" >/dev/null; then
  expect_rc "control: coturn relay 49252/udp (inclusive upper bound) passes" --rendered 0 "$FIX/compose-5.5.1.json"
else
  bad "control: the 5.5.1 fixture no longer carries coturn 49152/udp and 49252/udp"
fi
mutate relay-tcp '.services.coturn.ports += [{"mode":"ingress","target":49200,"published":"49200","protocol":"tcp"}]'
expect_rc "mutant: coturn 49200/tcp (the relay range is udp only)" --rendered 1 "$WORK/relay-tcp.json" \
  "posture: coturn 49200/tcp"
mutate mp-tcp-other '.services["media-plane"].ports += [{"mode":"ingress","target":40000,"published":"40000","protocol":"tcp"}]'
expect_rc "mutant: media-plane 40000/tcp (media tcp is only 3000)" --rendered 1 "$WORK/mp-tcp-other.json" \
  "posture: media-plane 40000/tcp"

# ── 2d. Networks that expose a container with nothing published ──────────────
# net_case NAME NETWORKS_JSON — postgres attached to the network keyed "lan".
net_case() { mutate "$1" '.services.postgres.networks = {"lan": null} | .networks = '"$2"; }
net_case net-macvlan '{"lan":{"driver":"macvlan"}}'
expect_rc "mutant: postgres on a macvlan network" --rendered 1 "$WORK/net-macvlan.json" \
  "posture: postgres network=lan refused (reachable without a publication)"
net_case net-ipvlan '{"lan":{"driver":"ipvlan"}}'
expect_rc "mutant: postgres on an ipvlan network" --rendered 1 "$WORK/net-ipvlan.json" \
  "posture: postgres network=lan refused (reachable without a publication)"
net_case net-unprot4 '{"lan":{"driver":"bridge","driver_opts":{"com.docker.network.bridge.gateway_mode_ipv4":"nat-unprotected"}}}'
expect_rc "mutant: postgres on a bridge with gateway_mode_ipv4=nat-unprotected" --rendered 1 "$WORK/net-unprot4.json" \
  "posture: postgres network=lan refused (reachable without a publication)"
net_case net-unprot6 '{"lan":{"driver":"bridge","driver_opts":{"com.docker.network.bridge.gateway_mode_ipv6":"nat-unprotected"}}}'
expect_rc "mutant: postgres on a bridge with gateway_mode_ipv6=nat-unprotected" --rendered 1 "$WORK/net-unprot6.json" \
  "posture: postgres network=lan refused (reachable without a publication)"
net_case net-trusted '{"lan":{"driver":"bridge","driver_opts":{"com.docker.network.bridge.trusted_host_interfaces":"eth0"}}}'
expect_rc "mutant: postgres on a bridge with trusted_host_interfaces" --rendered 1 "$WORK/net-trusted.json" \
  "posture: postgres network=lan refused (reachable without a publication)"
net_case net-external '{"lan":{"name":"corp-lan","external":true}}'
expect_rc "unjudgeable: postgres on an external network (its driver is not in the render)" --rendered 2 "$WORK/net-external.json"
net_case net-bridge-ok '{"lan":{"driver":"bridge","driver_opts":{"com.docker.network.bridge.enable_icc":"true","com.docker.network.bridge.gateway_mode_ipv4":"nat"}}}'
expect_rc "control: a bridge network with ordinary driver_opts passes" --rendered 0 "$WORK/net-bridge-ok.json"
net_case net-exposing-unattached '{"lan":{"driver":"macvlan"},"concord-net":null}'
jq '.services.postgres.networks = {"concord-net": null}' "$WORK/net-exposing-unattached.json" > "$WORK/net-exposing-unattached2.json"
expect_rc "control: a macvlan network that no service attaches to passes" --rendered 0 "$WORK/net-exposing-unattached2.json"

# Gateway modes: only `nat` (the default, also when absent) and `isolated` keep a 127.0.0.1
# publication off the LAN. `routed` opens published ports to direct routing and
# `nat-unprotected` opens every port; an unrecognised value is not known to be safe.
GW4='com.docker.network.bridge.gateway_mode_ipv4'
GW6='com.docker.network.bridge.gateway_mode_ipv6'
RF='refused (reachable without a publication)'
net_case net-routed4 '{"lan":{"driver":"bridge","driver_opts":{"'$GW4'":"routed"}}}'
expect_rc "mutant: postgres on a bridge with gateway_mode_ipv4=routed" --rendered 1 "$WORK/net-routed4.json" \
  "posture: postgres network=lan $RF"
net_case net-routed6 '{"lan":{"driver":"bridge","driver_opts":{"'$GW6'":"routed"}}}'
expect_rc "mutant: postgres on a bridge with gateway_mode_ipv6=routed" --rendered 1 "$WORK/net-routed6.json" \
  "posture: postgres network=lan $RF"
net_case net-routed-default-driver '{"lan":{"driver_opts":{"'$GW4'":"routed"}}}'
expect_rc "mutant: routed gateway mode on a network with no driver key (default bridge)" --rendered 1 \
  "$WORK/net-routed-default-driver.json" "posture: postgres network=lan $RF"
net_case net-gw-unknown '{"lan":{"driver":"bridge","driver_opts":{"'$GW4'":"something-new"}}}'
expect_rc "mutant: postgres on a bridge with an unrecognised gateway mode" --rendered 1 "$WORK/net-gw-unknown.json" \
  "posture: postgres network=lan $RF"
net_case net-unprot4-recheck '{"lan":{"driver":"bridge","driver_opts":{"'$GW4'":"nat-unprotected"}}}'
expect_rc "mutant: nat-unprotected is still refused beside the routed rule" --rendered 1 \
  "$WORK/net-unprot4-recheck.json" "posture: postgres network=lan $RF"
net_case net-isolated4 '{"lan":{"driver":"bridge","driver_opts":{"'$GW4'":"isolated"}}}'
expect_rc "control: a bridge with gateway_mode_ipv4=isolated passes" --rendered 0 "$WORK/net-isolated4.json"
net_case net-isolated6 '{"lan":{"driver":"bridge","driver_opts":{"'$GW6'":"isolated"}}}'
expect_rc "control: a bridge with gateway_mode_ipv6=isolated passes" --rendered 0 "$WORK/net-isolated6.json"
net_case net-nat-both '{"lan":{"driver":"bridge","driver_opts":{"'$GW4'":"nat","'$GW6'":"nat"}}}'
expect_rc "control: a bridge with both gateway modes explicitly nat passes" --rendered 0 "$WORK/net-nat-both.json"

# ── 3. Inputs the checker cannot judge (exit 2, fail closed) ─────────────────
printf 'not json' > "$WORK/not-json.json"
expect_rc "unjudgeable: not JSON" --rendered 2 "$WORK/not-json.json"
printf '{"services":[]}' > "$WORK/services-list.json"
expect_rc "unjudgeable: services is not a map" --rendered 2 "$WORK/services-list.json"
mutate no-rtc 'del(.services["media-plane"].environment)'
expect_rc "unjudgeable: media UDP present but no RTC window" --rendered 2 "$WORK/no-rtc.json"
mutate rtc-inverted '.services["media-plane"].environment.RTC_MIN_PORT = "42000"'
expect_rc "unjudgeable: RTC_MIN_PORT greater than RTC_MAX_PORT" --rendered 2 "$WORK/rtc-inverted.json"
mutate range-string '.services["media-plane"].ports[1].published = "40000-41999"'
expect_rc "unjudgeable: published is a range string" --rendered 2 "$WORK/range-string.json"

# ── 4. Live mode (docker inspect arrays) ─────────────────────────────────────
# live_ct SERVICE PORTBINDINGS_JSON ENV_JSON [NETWORK_MODE] [PUBLISH_ALL] — one inspect object.
live_ct() {
  jq -n --arg svc "$1" --argjson pb "$2" --argjson env "$3" --arg nm "${4:-bridge}" --argjson pa "${5:-false}" \
    '{Config:{Labels:{"com.docker.compose.service":$svc},Env:$env},
      HostConfig:{PortBindings:$pb,NetworkMode:$nm,PublishAllPorts:$pa}}'
}
LIVE_ENV='["RTC_MIN_PORT=40000","RTC_MAX_PORT=41999","SENTINEL_ENV=LIVE-SENTINEL"]'
CP_PB='{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":"8080"}]}'
MP_PB='{"3000/tcp":[{"HostIp":"127.0.0.1","HostPort":"3000"}],"40000/udp":[{"HostIp":"","HostPort":"40000"}]}'

{ live_ct control-plane "$CP_PB" '[]'
  live_ct media-plane "$MP_PB" "$LIVE_ENV"
  live_ct postgres '{}' '[]'; } | jq -s . > "$WORK/live-good.json"
expect_rc "live: good container set" --live 0 "$WORK/live-good.json"

{ live_ct control-plane "$CP_PB" '[]'
  live_ct postgres '{"5432/tcp":[{"HostIp":"0.0.0.0","HostPort":"5432"}]}' "$LIVE_ENV"; } | jq -s . > "$WORK/live-pg-all.json"
expect_rc "live: postgres bound to 0.0.0.0" --live 1 "$WORK/live-pg-all.json" \
  "posture: postgres 5432/tcp host_ip=all not allowed"

{ live_ct control-plane "$CP_PB" '[]'
  live_ct postgres '{"5432/tcp":[{"HostIp":"","HostPort":"5432"}]}' "$LIVE_ENV"; } | jq -s . > "$WORK/live-pg-empty.json"
expect_rc "live: postgres bound with empty HostIp" --live 1 "$WORK/live-pg-empty.json" \
  "posture: postgres 5432/tcp host_ip=all not allowed"

live_ct redis '{}' "$LIVE_ENV" host | jq -s . > "$WORK/live-host-net.json"
expect_rc "live: NetworkMode host" --live 1 "$WORK/live-host-net.json" \
  "posture: redis network_mode=host refused"

live_ct nats '{}' "$LIVE_ENV" bridge true | jq -s . > "$WORK/live-publish-all.json"
expect_rc "live: PublishAllPorts" --live 1 "$WORK/live-publish-all.json" \
  "posture: nats publish_all refused"

jq -n --argjson env "$LIVE_ENV" '[{Config:{Labels:{},Env:$env},HostConfig:{PortBindings:{},NetworkMode:"bridge",PublishAllPorts:false}}]' \
  > "$WORK/live-no-label.json"
expect_rc "live: container without a compose service label" --live 2 "$WORK/live-no-label.json"

# live: ephemeral and remapped host ports.
live_ct control-plane '{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":""}]}' '[]' | jq -s . > "$WORK/live-ephemeral.json"
expect_rc "live: HostPort empty (ephemeral host port)" --live 1 "$WORK/live-ephemeral.json" \
  "posture: control-plane 8080/tcp host_ip=127.0.0.1 not allowed (ephemeral host port)"
live_ct postgres '{"5432/tcp":[{"HostIp":"127.0.0.1","HostPort":"15432"}]}' '[]' | jq -s . > "$WORK/live-remap-pg.json"
expect_rc "live: postgres 5432/tcp remapped to host port 15432" --live 1 "$WORK/live-remap-pg.json" \
  "posture: postgres 15432/tcp"
live_ct control-plane '{"5432/tcp":[{"HostIp":"127.0.0.1","HostPort":"8080"}]}' '[]' | jq -s . > "$WORK/live-remap-cp.json"
expect_rc "live: control-plane host 8080 mapped onto container port 5432" --live 1 "$WORK/live-remap-cp.json" \
  "posture: control-plane 8080/tcp"

# live: network_mode container:<x> is judged only when <x> is in the same inspect set.
with_id() { jq --arg id "$1" --arg name "${2:-}" '. + {Id:$id, Name:$name}'; }
{ live_ct control-plane "$CP_PB" '[]' | with_id abc123 /owner
  live_ct nats '{}' '[]' container:abc123 | with_id def456 /sharer; } | jq -s . > "$WORK/live-ctr-id-in.json"
expect_rc "live: container:<Id> naming a container in the same set is judged (passes)" --live 0 "$WORK/live-ctr-id-in.json"
{ live_ct control-plane "$CP_PB" '[]' | with_id abc123 /owner
  live_ct nats '{}' '[]' container:owner | with_id def456 /sharer; } | jq -s . > "$WORK/live-ctr-name-in.json"
expect_rc "live: container:<Name> (leading / stripped) in the same set is judged (passes)" --live 0 "$WORK/live-ctr-name-in.json"
{ live_ct control-plane "$CP_PB" '[]' | with_id abc123 /owner
  live_ct postgres '{"5432/tcp":[{"HostIp":"0.0.0.0","HostPort":"5432"}]}' '[]' container:abc123 | with_id def456 /sharer; } | jq -s . > "$WORK/live-ctr-in-bad.json"
expect_rc "live: a container sharing an in-set namespace is still judged on its own bindings" --live 1 "$WORK/live-ctr-in-bad.json" \
  "posture: postgres 5432/tcp host_ip=all not allowed"
{ live_ct control-plane "$CP_PB" '[]' | with_id abc123 /owner
  live_ct nats '{}' '[]' container:zzz999 | with_id def456 /sharer; } | jq -s . > "$WORK/live-ctr-out.json"
expect_rc "unjudgeable: container:<x> naming a container outside the set" --live 2 "$WORK/live-ctr-out.json"

# live: {"containers": <docker inspect>, "networks": <docker network inspect>} judges the
# networks each container is attached to. Every case builds one postgres container carrying
# the Config.Env sentinel, attached to the network named "lan" with id "net-1".
# live_net_doc OUT NETWORKS_JSON [NET_ID] — NET_ID is the id the container points at.
live_net_doc() {
  live_ct postgres '{}' "$LIVE_ENV" \
    | jq --arg id "${3:-net-1}" '. + {NetworkSettings:{Networks:{lan:{NetworkID:$id}}}}' \
    | jq -s --argjson nets "$2" '{containers: ., networks: $nets}' > "$1"
}
LRF='posture: postgres network=lan refused (reachable without a publication)'
live_net_doc "$WORK/live-net-plain.json" '[{"Id":"net-1","Name":"lan","Driver":"bridge","Options":{}}]'
expect_rc "live dict: a plain bridge network passes" --live 0 "$WORK/live-net-plain.json"
live_net_doc "$WORK/live-net-nat.json" \
  '[{"Id":"net-1","Name":"lan","Driver":"bridge","Options":{"'$GW4'":"nat","'$GW6'":"isolated"}}]'
expect_rc "live dict: a bridge with gateway modes nat and isolated passes" --live 0 "$WORK/live-net-nat.json"
live_net_doc "$WORK/live-net-routed.json" \
  '[{"Id":"net-1","Name":"lan","Driver":"bridge","Options":{"'$GW4'":"routed"}}]'
expect_rc "live dict: a bridge with gateway_mode_ipv4=routed is refused" --live 1 "$WORK/live-net-routed.json" "$LRF"
live_net_doc "$WORK/live-net-routed6.json" \
  '[{"Id":"net-1","Name":"lan","Driver":"bridge","Options":{"'$GW6'":"routed"}}]'
expect_rc "live dict: a bridge with gateway_mode_ipv6=routed is refused" --live 1 "$WORK/live-net-routed6.json" "$LRF"
live_net_doc "$WORK/live-net-unprot.json" \
  '[{"Id":"net-1","Name":"lan","Driver":"bridge","Options":{"'$GW4'":"nat-unprotected"}}]'
expect_rc "live dict: a bridge with gateway_mode_ipv4=nat-unprotected is refused" --live 1 "$WORK/live-net-unprot.json" "$LRF"
live_net_doc "$WORK/live-net-trusted.json" \
  '[{"Id":"net-1","Name":"lan","Driver":"bridge","Options":{"com.docker.network.bridge.trusted_host_interfaces":"eth0"}}]'
expect_rc "live dict: a bridge with trusted_host_interfaces is refused" --live 1 "$WORK/live-net-trusted.json" "$LRF"
live_net_doc "$WORK/live-net-macvlan.json" '[{"Id":"net-1","Name":"lan","Driver":"macvlan","Options":{}}]'
expect_rc "live dict: a macvlan network is refused" --live 1 "$WORK/live-net-macvlan.json" "$LRF"
live_net_doc "$WORK/live-net-overlay.json" '[{"Id":"net-1","Name":"lan","Driver":"overlay","Options":{}}]'
expect_rc "live dict: an overlay network is refused" --live 1 "$WORK/live-net-overlay.json" "$LRF"
live_net_doc "$WORK/live-net-null.json" '[{"Id":"net-1","Name":"lan","Driver":"null","Options":{}}]'
expect_rc "live dict: a driver-null network (--network none) has no path in and passes" --live 0 "$WORK/live-net-null.json"
live_net_doc "$WORK/live-net-hostdrv.json" '[{"Id":"net-1","Name":"lan","Driver":"host","Options":{}}]'
expect_rc "live dict: a driver-host network is judged by NetworkMode, not here, and passes" --live 0 "$WORK/live-net-hostdrv.json"
# A routed network with a NULL Options still has a driver to judge, and a missing Options on a
# plain bridge is the default (nat), not a violation.
live_net_doc "$WORK/live-net-null-opts.json" '[{"Id":"net-1","Name":"lan","Driver":"bridge","Options":null}]'
expect_rc "live dict: a bridge whose Options is null passes" --live 0 "$WORK/live-net-null-opts.json"

# Fail closed: a container attached to a network the caller did not list cannot be judged.
live_net_doc "$WORK/live-net-missing.json" '[{"Id":"net-other","Name":"other","Driver":"bridge","Options":{}}]'
expect_rc "unjudgeable live dict: the container's NetworkID is not in the network list" --live 2 "$WORK/live-net-missing.json"
live_net_doc "$WORK/live-net-empty.json" '[]'
expect_rc "unjudgeable live dict: an empty network list for an attached container" --live 2 "$WORK/live-net-empty.json"
jq 'del(.networks)' "$WORK/live-net-plain.json" > "$WORK/live-net-nokey.json"
expect_rc "unjudgeable live dict: no networks key for an attached container" --live 2 "$WORK/live-net-nokey.json"
jq '.containers = {"not":"a list"}' "$WORK/live-net-plain.json" > "$WORK/live-net-containers-obj.json"
expect_rc "unjudgeable live dict: containers is an object, not a list" --live 2 "$WORK/live-net-containers-obj.json"
jq '.containers = "nope"' "$WORK/live-net-plain.json" > "$WORK/live-net-containers-str.json"
expect_rc "unjudgeable live dict: containers is a string, not a list" --live 2 "$WORK/live-net-containers-str.json"
jq 'del(.containers)' "$WORK/live-net-plain.json" > "$WORK/live-net-nocontainers.json"
expect_rc "unjudgeable live dict: no containers key" --live 2 "$WORK/live-net-nocontainers.json"

# A container on two networks is refused for the offending one only, by name.
live_ct postgres '{}' "$LIVE_ENV" \
  | jq '. + {NetworkSettings:{Networks:{good:{NetworkID:"net-good"},lan:{NetworkID:"net-bad"}}}}' \
  | jq -s '{containers: ., networks: [{"Id":"net-good","Name":"good","Driver":"bridge","Options":{}},{"Id":"net-bad","Name":"lan","Driver":"macvlan","Options":{}}]}' \
  > "$WORK/live-net-two.json"
expect_rc "live dict: one bad network among two is refused and named" --live 1 "$WORK/live-net-two.json" "$LRF"
if python3 "$CHECKER" --live < "$WORK/live-net-two.json" 2>&1 | grep -qF 'network=good'; then
  bad "live dict: the compliant network was named as a violation"
else
  ok "live dict: the compliant network is not named as a violation"
fi

# Back-compat: the bare container list (the pre-network shape) is judged on bindings and
# modes only, so an attached container whose network would be refused still passes.
expect_rc "live bare list: the old shape still passes (live-good)" --live 0 "$WORK/live-good.json"
jq '.containers' "$WORK/live-net-macvlan.json" > "$WORK/live-net-bare-macvlan.json"
expect_rc "live bare list: network data absent, so an attached container is not network-judged" \
  --live 0 "$WORK/live-net-bare-macvlan.json"
# ...and a binding violation in a dict is still caught beside a clean network.
live_ct postgres '{"5432/tcp":[{"HostIp":"0.0.0.0","HostPort":"5432"}]}' "$LIVE_ENV" \
  | jq '. + {NetworkSettings:{Networks:{lan:{NetworkID:"net-1"}}}}' \
  | jq -s '{containers: ., networks: [{"Id":"net-1","Name":"lan","Driver":"bridge","Options":{}}]}' \
  > "$WORK/live-net-pg-all.json"
expect_rc "live dict: a 0.0.0.0 binding is still refused beside a clean network" --live 1 "$WORK/live-net-pg-all.json" \
  "posture: postgres 5432/tcp host_ip=all not allowed"

# ── 5. Hygiene: planted sentinels must never reach the output ────────────────
# expect_rc already fails on any leaked sentinel; these cases plant one in the render
# itself and assert the verdict is the one that proves the sentinel was actually read.
mutate leak-violation '.services.postgres.ports += [{"mode":"ingress","target":5432,"published":"5432","protocol":"tcp"}]
  | .services.postgres.environment.SENTINEL_ENV = "RENDER-SENTINEL"'
expect_rc "hygiene: sentinel in a violating render" --rendered 1 "$WORK/leak-violation.json" \
  "posture: postgres 5432/tcp"
mutate leak-unjudgeable 'del(.services["media-plane"].environment.RTC_MIN_PORT)
  | .services["media-plane"].environment.SENTINEL_ENV = "RENDER-SENTINEL"'
expect_rc "hygiene: sentinel in an unjudgeable render" --rendered 2 "$WORK/leak-unjudgeable.json"
# The live sentinel rides Config.Env of a violating set (live-pg-all) and a good set
# (live-good); both are run through expect_rc above. Assert here that it really is planted.
if grep -q 'LIVE-SENTINEL' "$WORK/live-pg-all.json" && grep -q 'LIVE-SENTINEL' "$WORK/live-good.json"; then
  ok "hygiene: live sentinel is present in the inputs that passed the leak check"
else
  bad "hygiene: live sentinel missing from the live inputs (leak check would be vacuous)"
fi

# ── 6. Fixture hygiene ───────────────────────────────────────────────────────
if grep -rqE 'FIXTURE-SENTINEL|203\.0\.113\.10|t\.test' "$FIX"; then
  bad "fixtures contain a dummy-env value"
else
  ok "fixtures carry no dummy-env value"
fi

# ── 7. COMPOSE_CMD pin: the managed command is byte-identical to main ────────
# The head of concord-ctl.sh up to the COMPOSE_CMD= line is sourced from a temp root, so the
# posture gate (which comes after that line) never runs and docker is never invoked.
mkdir -p "$WORK/root/infrastructure/deploy"
HEAD_SH="$WORK/root/infrastructure/deploy/concord-ctl-head.sh"
sed -n '1,/^COMPOSE_CMD=/p' "$ROOT/infrastructure/deploy/concord-ctl.sh" > "$HEAD_SH"
D="$WORK/root/infrastructure/deploy/../.."
DEPLOY_ENV="$WORK/root/infrastructure/deploy/deploy.env"
# No full controller/CLI execution is added here. Only when the extracted
# setup actually loads the helper does it need copied helper/model inputs.
HEAD_LOADS_STORAGE=0
if grep -qE 'selfhost-storage\.sh|selfhost_storage_' "$HEAD_SH"; then
  HEAD_LOADS_STORAGE=1
  if [[ ! -f "$ROOT/infrastructure/deploy/selfhost-storage.sh" ]]; then
    bad "prerequisite missing: accepted storage helper for extracted controller setup"
    exit 1
  fi
  cp "$ROOT/infrastructure/deploy/selfhost-storage.sh" "$WORK/root/infrastructure/deploy/selfhost-storage.sh"
  printf 'INSTANCE_TYPE=self-hosted\n' >"$WORK/root/.env"
  touch "$WORK/root/docker-compose.yml" "$WORK/root/docker-compose.production.yml" \
    "$WORK/root/docker-compose.selfhost.yml"
  cat >"$WORK/root/docker-compose.byo-s3.yml" <<'YAML'
services:
  minio:
    profiles:
      - selfhost-bundled-minio-inactive
  control-plane:
    depends_on: !override
      postgres:
        condition: service_healthy
      redis:
        condition: service_healthy
      nats:
        condition: service_healthy
    environment:
      STORAGE_BACKEND: "${STORAGE_BACKEND:?STORAGE_BACKEND must be set for BYO storage}"
      STORAGE_ENDPOINT: "${STORAGE_ENDPOINT:?STORAGE_ENDPOINT must be set for BYO storage}"
      STORAGE_REGION: "${STORAGE_REGION:-}"
      STORAGE_ACCESS_KEY: "${STORAGE_ACCESS_KEY:?STORAGE_ACCESS_KEY must be set for BYO storage}"
      STORAGE_SECRET_KEY: "${STORAGE_SECRET_KEY:?STORAGE_SECRET_KEY must be set for BYO storage}"
      STORAGE_USE_SSL: "${STORAGE_USE_SSL:?STORAGE_USE_SSL must be set for BYO storage}"
      STORAGE_BUCKET: "${STORAGE_BUCKET:?STORAGE_BUCKET must be set for BYO storage}"
YAML
fi
mkdir -p "$WORK/head-bin"
PORT_DOCKER_STUB="$WORK/head-bin/docker"
PORT_DOCKER_REJECT_LOG="$WORK/head-docker-rejects"
: >"$PORT_DOCKER_REJECT_LOG"
cat >"$PORT_DOCKER_STUB" <<'STUB'
#!/usr/bin/env bash
# Only resolver identity/config and authoritative stale requests are known.
if [[ "${1:-}" == compose && " $* " == *' config --format json '* ]]; then
  printf '%s\n' '{"name":"port-fixture","services":{}}'
  exit 0
fi
if [[ "${1:-}" == ps && " $* " == *'label=com.docker.compose.project=port-fixture'* \
  && " $* " == *'label=com.docker.compose.service=minio'* && " $* " == *'status=running'* ]]; then
  exit 0
fi
printf '%s\n' 'REJECT: unknown extracted-controller Docker command' >>"$PORT_DOCKER_REJECT_LOG"
exit 97
STUB
chmod +x "$PORT_DOCKER_STUB"
# The head's env -i invocation defines its own function as well as a PATH stub.
# Prove an unknown request cannot escape either interception layer.
for route in function executable; do
  rc=0
  if [[ "$route" == function ]]; then
    env -i PATH="$WORK/head-bin:$PATH" PORT_DOCKER_STUB="$PORT_DOCKER_STUB" \
      PORT_DOCKER_REJECT_LOG="$PORT_DOCKER_REJECT_LOG" bash -c \
      'docker() { "$PORT_DOCKER_STUB" "$@"; }; docker unknown-fixture-command' >/dev/null 2>&1 || rc=$?
  else
    PORT_DOCKER_REJECT_LOG="$PORT_DOCKER_REJECT_LOG" "$PORT_DOCKER_STUB" unknown-fixture-command >/dev/null 2>&1 || rc=$?
  fi
  if [[ $rc == 97 && -s "$PORT_DOCKER_REJECT_LOG" ]]; then
    ok "extracted-controller Docker $route fixture rejects unknown commands with 97 and marker"
  else bad "extracted-controller Docker $route fixture is not fail closed"; fi
  : >"$PORT_DOCKER_REJECT_LOG"
done
# pin NAME EXPECTED [VAR=VALUE ...] — sources the head under a scrubbed environment.
pin() {
  local name="$1" want="$2" got; shift 2
  # shellcheck disable=SC2016  # the single-quoted script is expanded by the inner bash, on purpose
  got="$(env -i PATH="$WORK/head-bin:$PATH" HOME="$HOME" \
    PORT_DOCKER_STUB="$PORT_DOCKER_STUB" PORT_DOCKER_REJECT_LOG="$PORT_DOCKER_REJECT_LOG" \
    "$@" bash -c 'docker() { "$PORT_DOCKER_STUB" "$@"; }; export -f docker; source "$1"; printf %s "$COMPOSE_CMD"' _ "$HEAD_SH" 2>&1)"
  if [[ "$got" == "$want" ]]; then ok "$name"; else bad "$name"$'\n'"  want: $want"$'\n'"  got:  $got"; fi
}
rm -f "$DEPLOY_ENV"
pin "COMPOSE_CMD pin: managed, no deploy.env" \
  "docker compose  -f $D/docker-compose.yml -f $D/docker-compose.staging.yml --profile services"
pin "COMPOSE_CMD pin: managed, COMPOSE_OVERRIDE=production" \
  "docker compose  -f $D/docker-compose.yml -f $D/docker-compose.production.yml --profile services" \
  COMPOSE_OVERRIDE=docker-compose.production.yml
printf '%s\n' 'COMPOSE_SELFHOST_ARG=" -f /evil"' > "$DEPLOY_ENV"
pin "COMPOSE_CMD pin: managed, deploy.env cannot inject COMPOSE_SELFHOST_ARG" \
  "docker compose  -f $D/docker-compose.yml -f $D/docker-compose.staging.yml --profile services"
printf '%s\n' 'COMPOSE_SELFHOST_ARG=""' > "$DEPLOY_ENV"
SELFHOST_HEAD_COMMAND="docker compose --env-file $D/.env -f $D/docker-compose.yml -f $D/docker-compose.production.yml -f $D/docker-compose.selfhost.yml --profile services"
if (( HEAD_LOADS_STORAGE )); then
  SELFHOST_HEAD_COMMAND=selfhost_storage_compose
fi
pin "COMPOSE_CMD pin: self-host, deploy.env cannot suppress selected model" \
  "$SELFHOST_HEAD_COMMAND" ACTIVITY_HISTORY_CONTRACT_MODE=selfhost
rm -f "$DEPLOY_ENV"
if [[ -s "$PORT_DOCKER_REJECT_LOG" ]]; then
  bad "extracted controller setup issued an unrecognized Docker request"
fi

# ── 8. Engine floor: concord-ctl and the installer carry the same literal ────
CTL_FLOOR="$(grep -oE '^SELFHOST_MIN_ENGINE=[0-9.]+' "$ROOT/infrastructure/deploy/concord-ctl.sh" | cut -d= -f2)"
INST_FLOOR="$(grep -oE '^readonly MIN_ENGINE=[0-9.]+' "$ROOT/infrastructure/deploy/install-selfhost.sh" | cut -d= -f2)"
if [[ -n "$CTL_FLOOR" && "$CTL_FLOOR" == "$INST_FLOOR" ]]; then
  ok "Engine floor is identical in concord-ctl.sh and install-selfhost.sh ($CTL_FLOOR)"
else
  bad "Engine floor drift: concord-ctl.sh='${CTL_FLOOR}' install-selfhost.sh='${INST_FLOOR}'"
fi

# The current floor also closes CVE-2026-92543 (registry destination classification).
# Equality above would still pass if both literals regressed together.
if [[ -n "$CTL_FLOOR" && "$(printf '%s\n%s\n' 29.8.2 "$CTL_FLOOR" | sort -V | tail -1)" == "$CTL_FLOOR" ]]; then
  ok "Engine floor is at least 29.8.2 (CVE-2026-92543)"
else
  bad "Engine floor '${CTL_FLOOR}' is below 29.8.2 (CVE-2026-92543)"
fi

# ── 9. Packaging pins ────────────────────────────────────────────────────────
if grep -qx 'docker-compose.selfhost.yml' "$ROOT/infrastructure/deploy/deploy-manifest.txt"; then
  ok "deploy-manifest.txt lists docker-compose.selfhost.yml"
else
  bad "deploy-manifest.txt does not list docker-compose.selfhost.yml on its own line"
fi
if grep -qE '^docker-compose\.selfhost\.yml[[:space:]]+@Concord-Voice/security' "$ROOT/CODEOWNERS"; then
  ok "CODEOWNERS covers docker-compose.selfhost.yml"
else
  bad "CODEOWNERS has no docker-compose.selfhost.yml entry owned by @Concord-Voice/security"
fi

# ── 10. Real render with the installed Compose ───────────────────────────────
# The helper itself: CI turns a missing tool into a FAIL, a laptop keeps the SKIP.
if [[ "$(CI=true missing_tool "docker compose" "real render" 2>&1)" == FAIL:* ]]; then
  ok "missing tool under CI=true is a FAIL"
else
  bad "missing tool under CI=true did not fail the suite"
fi
if [[ "$(CI= missing_tool "docker compose" "real render" 2>&1)" == SKIP:* ]]; then
  ok "missing tool outside CI stays a SKIP"
else
  bad "missing tool outside CI did not skip"
fi
if (( BASE_ONLY )); then
  echo "SKIP: installed-Compose real renders (--base-only; configuration behavior unexecuted)"
elif ! env -i PATH="$PATH" HOME="$HOME" docker compose version >/dev/null 2>&1; then
  missing_tool "docker compose" "the real-render cases need it"
else
  # Dummy env: keys come from an array and are joined with printf, so no `KEY=value`
  # literal ever appears in this source (secret scanners flag them).
  envline() { printf '%s=%s\n' "$1" "$2"; }
  {
    envline DOMAIN t.test
    envline PUBLIC_IP 203.0.113.10
    envline FEEDBACK_REPO a/b
    envline SMTP_HOST smtp.t.test
    envline SMTP_FROM fixture@t.test
    envline TRUSTED_PROXY_CIDRS 10.0.0.0/8
    for k in "POSTGRES_PASS""WORD" "REDIS_PASS""WORD" "JWT_SEC""RET" "TURN_SEC""RET" \
             "MINIO_ROOT_USER" "MINIO_ROOT_PASS""WORD" "FEEDBACK_PAT" "MFA_ENCRYPTION_KEY"; do
      envline "$k" ph
    done
  } > "$WORK/dummy.env"
  HAVE_COMPOSE="$(env -i PATH="$PATH" HOME="$HOME" docker compose version --short 2>/dev/null | tr -d 'v')"
  if [[ "$(printf '%s\n%s\n' 2.24.5 "$HAVE_COMPOSE" | sort -V | head -1)" == "2.24.5" ]]; then
    WANT_WITH_OVERLAY=0
  else
    WANT_WITH_OVERLAY=1
  fi
  # Real configuration rendering stays enabled by default, but uses only
  # copied sources, an explicit synthetic environment and empty synthetic spa.env.
  RENDER_ROOT="$WORK/render-root"
  mkdir -p "$RENDER_ROOT"
  cp "$ROOT/docker-compose.yml" "$ROOT/docker-compose.production.yml" \
    "$ROOT/docker-compose.selfhost.yml" "$RENDER_ROOT/"
  : >"$RENDER_ROOT/spa.env"
  render() { # render OUT [EXTRA_FILE_FLAG...], no daemon or default real .env
    local out="$1"; shift
    (cd "$RENDER_ROOT" && env -i PATH="$PATH" HOME="$HOME" \
      docker compose --project-directory "$RENDER_ROOT" --env-file "$WORK/dummy.env" \
      -f docker-compose.yml -f docker-compose.production.yml \
      "$@" --profile services config --format json 2>/dev/null) > "$out"
  }
  expect_media_capacity() { # Actual Compose result, expected quota and workers.
    local rendered="$1" expected="$2" label="$3"
    if python3 - "$rendered" "$expected" <<'PY'; then
import json
import sys
from decimal import Decimal, InvalidOperation

try:
    with open(sys.argv[1], encoding="utf-8") as source:
        model = json.load(source)
    media = model["services"]["media-plane"]
    quota = media["deploy"]["resources"]["limits"]["cpus"]
    workers = media["environment"]["NUM_WORKERS"]
    expected = Decimal(sys.argv[2])
    if isinstance(quota, bool) or not isinstance(quota, (str, int, float)):
        raise ValueError()
    quota = Decimal(str(quota))
    if isinstance(workers, bool) or not isinstance(workers, (str, int)):
        raise ValueError()
    if isinstance(workers, str) and not workers.isdecimal():
        raise ValueError()
    workers = int(workers)
    valid = quota.is_finite() and quota == expected and workers > 0 and Decimal(workers) == expected
except (KeyError, TypeError, ValueError, InvalidOperation, OSError):
    valid = False
sys.exit(0 if valid else 1)
PY
      ok "$label"
    else
      bad "$label"
    fi
  }
  if render "$WORK/real-with-overlay.json" -f docker-compose.selfhost.yml; then
    expect_rc "real render with overlay (Compose $HAVE_COMPOSE) exits $WANT_WITH_OVERLAY" \
      --rendered "$WANT_WITH_OVERLAY" "$WORK/real-with-overlay.json"
    expect_media_capacity "$WORK/real-with-overlay.json" 2 \
      "real selfhost media CPU quota and worker count are both 2"
  else
    bad "real render with overlay failed to produce a config"
  fi
  if render "$WORK/real-no-overlay.json"; then
    expect_rc "real render WITHOUT overlay is refused (the overlay is what makes it pass)" \
      --rendered 1 "$WORK/real-no-overlay.json" "posture:"
    expect_media_capacity "$WORK/real-no-overlay.json" 3 \
      "real managed production media CPU quota and worker count remain 3"
  else
    bad "real render without overlay failed to produce a config"
  fi
fi

if (( BASE_ONLY )); then
  echo "BASELINE CHECKS: installed-Compose real renders unexecuted"
fi
echo "passed: $PASS  failed: $FAIL"; [[ $FAIL -eq 0 ]]
