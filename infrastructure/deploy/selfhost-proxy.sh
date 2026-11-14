#!/usr/bin/env bash
# Self-host-only proxy trust: the host's published-port peer is Docker's selected bridge gateway.

selfhost_proxy_refuse() {
  printf '%s\n' "selfhost-proxy: $1" >&2
  return 1
}

selfhost_proxy_compose() {
  (cd "$DEPLOY_DIR" && $COMPOSE_CMD "$@")
}

selfhost_proxy_model() {
  python3 -c '
import json, re, sys
try:
    model = json.load(sys.stdin)
    project = model["name"]
    network = model["networks"]["concordvoice-services"]["name"]
    if not all(isinstance(v, str) and re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*", v)
               for v in (project, network)):
        raise ValueError()
    for name in ("control-plane", "media-plane"):
        nets = model["services"][name]["networks"]
        if not isinstance(nets, dict) or len(nets) < 2 or "concordvoice-data" not in nets:
            raise ValueError()
        for net, settings in nets.items():
            priority = settings.get("gw_priority", 0) if isinstance(settings, dict) else None
            if type(priority) is not int or (priority == 1) != (net == "concordvoice-services"):
                raise ValueError()
            if net != "concordvoice-services" and priority >= 1:
                raise ValueError()
    print(project, network, sep="\t")
except (KeyError, TypeError, ValueError, json.JSONDecodeError):
    sys.exit(1)
'
}

selfhost_proxy_network() {
  python3 -c '
import ipaddress, json, re, sys
try:
    n = json.load(sys.stdin)
    project, name = sys.argv[1:]
    labels = n["Labels"]
    if (n["Name"] != name or n["Driver"] != "bridge" or n.get("Internal") is True
        or labels["com.docker.compose.project"] != project
        or labels["com.docker.compose.network"] != "concordvoice-services"):
        raise ValueError()
    entries = n["IPAM"]["Config"]
    if not isinstance(entries, list) or len(entries) != 1:
        raise ValueError()
    entry = entries[0]
    gateway = ipaddress.ip_address(entry["Gateway"])
    subnet = ipaddress.ip_network(entry["Subnet"], strict=False)
    if (gateway.version != subnet.version or gateway not in subnet
        or gateway.is_unspecified or gateway.is_multicast or gateway.is_link_local):
        raise ValueError()
    containers = n["Containers"]
    if not isinstance(containers, dict):
        raise ValueError()
    if any(not isinstance(key, str) or not re.fullmatch(r"[0-9a-f]{12,64}", key)
           for key in containers):
        raise ValueError()
    print(f"{gateway}/{gateway.max_prefixlen}", "pinned" if containers else "empty",
          ",".join(containers), sep="\t")
except (AttributeError, KeyError, TypeError, ValueError, json.JSONDecodeError):
    sys.exit(1)
' "$@"
}

selfhost_proxy_members() {
  local project="$1" members="$2" id labels
  [[ -n "$members" ]] || return 0
  local -a ids
  IFS=, read -ra ids <<<"$members"
  for id in "${ids[@]}"; do
    labels="$(docker inspect "$id" --format '{{json .Config.Labels}}' 2>/dev/null)" \
      || return 1
    python3 -c '
import json, sys
try:
    labels = json.load(sys.stdin)
    if (labels["com.docker.compose.project"] != sys.argv[1]
        or labels["com.docker.compose.service"] not in
            {"nats", "coturn", "ops-agent", "control-plane", "media-plane"}):
        raise ValueError()
except (KeyError, TypeError, ValueError, json.JSONDecodeError):
    sys.exit(1)
' "$project" <<<"$labels" || return 1
  done
}

selfhost_proxy_write_env() {
  python3 -c '
import ipaddress, os, re, stat, sys, tempfile
legacy = {"172.16.0.0/12,10.0.0.0/8", "172.16.0.0/12", "172.19.0.0/16"}
try:
    if len(sys.argv) not in (3, 4) or (len(sys.argv) == 4 and sys.argv[3] != "--check"):
        raise ValueError()
    path, route = sys.argv[1:3]
    parsed = ipaddress.ip_network(route, strict=True)
    if parsed.prefixlen != parsed.max_prefixlen:
        raise ValueError()
    info = os.lstat(path)
    if not stat.S_ISREG(info.st_mode) or info.st_size > 1024 * 1024:
        raise ValueError()
    with open(path, "rb") as source:
        old = source.read()
    if not old.endswith(b"\n") or b"\r" in old or b"\0" in old:
        raise ValueError()
    lines = old.decode("utf-8").splitlines(keepends=True)
    seen = set()
    result = []
    for line in lines:
        matched = re.fullmatch(r"(TRUSTED_PROXY_CIDRS|TRUSTED_PROXIES)=(.*)\n", line)
        if matched is None:
            result.append(line)
            continue
        key, value = matched.groups()
        if key in seen:
            raise ValueError()
        seen.add(key)
        if value.startswith("\"") or value.endswith("\""):
            if len(value) < 2 or not (value.startswith("\"") and value.endswith("\"")):
                raise ValueError()
            value = value[1:-1]
        if value not in legacy:
            network = ipaddress.ip_network(value, strict=True)
            if network.prefixlen != network.max_prefixlen:
                raise ValueError()
        result.append(f"{key}=\"{route}\"\n")
    if "TRUSTED_PROXY_CIDRS" not in seen:
        raise ValueError()
    if "TRUSTED_PROXIES" not in seen:
        result.append(f"TRUSTED_PROXIES=\"{route}\"\n")
    if len(sys.argv) == 4 and sys.argv[3] == "--check":
        sys.exit(0)
    new = "".join(result).encode("utf-8")
    directory = os.path.dirname(path)
    if new == old:
        sys.exit(0)
    fd, temporary = tempfile.mkstemp(prefix=".env.proxy.", dir=directory)
    try:
        os.fchmod(fd, stat.S_IMODE(info.st_mode))
        os.fchown(fd, info.st_uid, info.st_gid)
        with os.fdopen(fd, "wb") as target:
            target.write(new)
            target.flush()
            os.fsync(target.fileno())
        if os.lstat(path).st_ino != info.st_ino:
            raise ValueError()
        os.replace(temporary, path)
        directory_fd = os.open(directory, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)
except (OSError, UnicodeError, ValueError):
    sys.exit(1)
' "$@"
}

selfhost_proxy_exact_env() {
  python3 -c '
import ipaddress, os, re, stat, sys
try:
    path = sys.argv[1]
    info = os.lstat(path)
    if not stat.S_ISREG(info.st_mode) or info.st_size > 1024 * 1024:
        raise ValueError()
    with open(path, "r", encoding="utf-8") as source:
        lines = source.readlines()
    found = {}
    for line in lines:
        match = re.fullmatch(r"(TRUSTED_PROXY_CIDRS|TRUSTED_PROXIES)=(.*)\n", line)
        if match is None:
            continue
        key, value = match.groups()
        if key in found:
            raise ValueError()
        if value.startswith("\"") or value.endswith("\""):
            if len(value) < 2 or not (value.startswith("\"") and value.endswith("\"")):
                raise ValueError()
            value = value[1:-1]
        route = ipaddress.ip_network(value, strict=True)
        if route.prefixlen != route.max_prefixlen:
            raise ValueError()
        found[key] = value
    if set(found) != {"TRUSTED_PROXY_CIDRS", "TRUSTED_PROXIES"} \
       or found["TRUSTED_PROXY_CIDRS"] != found["TRUSTED_PROXIES"]:
        raise ValueError()
    print(found["TRUSTED_PROXY_CIDRS"])
except (OSError, UnicodeError, ValueError):
    sys.exit(1)
' "$1"
}

selfhost_prepare_trusted_proxy_provenance() {
  local version model identity project network network_ids network_json verdict route pinned members project_ids original_network_id original_route
  version="$(docker compose version --short 2>/dev/null)" \
    || { selfhost_proxy_refuse 'Compose version unavailable'; return 1; }
  python3 - "$version" <<'PY' || { selfhost_proxy_refuse 'Compose 2.33.1 or newer is required'; return 1; }
import re, sys
match = re.fullmatch(r"v?(\d+)\.(\d+)\.(\d+)(?:\+[A-Za-z0-9.]+|-desktop\.[0-9]+)?", sys.argv[1])
if match is None or tuple(map(int, match.groups())) < (2, 33, 1):
    sys.exit(1)
PY
  model="$(selfhost_proxy_compose config --format json 2>/dev/null)" \
    || { selfhost_proxy_refuse 'Compose model unavailable'; return 1; }
  identity="$(selfhost_proxy_model <<<"$model")" \
    || { selfhost_proxy_refuse 'application gateway model refused'; return 1; }
  IFS=$'\t' read -r project network <<<"$identity"
  selfhost_proxy_write_env "$CONFIG_ROOT/.env" 127.0.0.1/32 --check \
    || { selfhost_proxy_refuse 'proxy environment contains an unrecognized trust policy'; return 1; }
  network_ids="$(docker network ls --filter "label=com.docker.compose.project=$project" \
    --filter label=com.docker.compose.network=concordvoice-services --format '{{.ID}}' 2>/dev/null)" \
    || { selfhost_proxy_refuse 'services network unavailable'; return 1; }
  if [[ -z "$network_ids" ]]; then
    project_ids="$(docker ps -a --filter "label=com.docker.compose.project=$project" \
      --format '{{.ID}}' 2>/dev/null)" \
      || { selfhost_proxy_refuse 'project inventory unavailable'; return 1; }
    [[ -z "$project_ids" ]] \
      || { selfhost_proxy_refuse 'project containers exist without the services network'; return 1; }
  fi
  if [[ -n "$network_ids" ]]; then
    [[ "$network_ids" != *$'\n'* ]] \
      || { selfhost_proxy_refuse 'services network is ambiguous'; return 1; }
    network_json="$(docker network inspect "$network_ids" --format '{{json .}}' 2>/dev/null)" \
      || { selfhost_proxy_refuse 'services network inspection failed'; return 1; }
    verdict="$(selfhost_proxy_network "$project" "$network" <<<"$network_json")" \
      || { selfhost_proxy_refuse 'services network provenance refused'; return 1; }
    IFS=$'\t' read -r route pinned members <<<"$verdict"
    selfhost_proxy_members "$project" "$members" \
      || { selfhost_proxy_refuse 'services network membership refused'; return 1; }
  else
    pinned=empty
  fi
  if [[ "$pinned" == empty ]]; then
    selfhost_proxy_compose up --no-start --no-deps --pull missing nats >/dev/null \
      || { selfhost_proxy_refuse 'stopped network materialization failed'; return 1; }
    network_ids="$(docker network ls --filter "label=com.docker.compose.project=$project" \
      --filter label=com.docker.compose.network=concordvoice-services --format '{{.ID}}' 2>/dev/null)" \
      || { selfhost_proxy_refuse 'seeded network unavailable'; return 1; }
    [[ -n "$network_ids" && "$network_ids" != *$'\n'* ]] \
      || { selfhost_proxy_refuse 'seeded network is ambiguous'; return 1; }
    network_json="$(docker network inspect "$network_ids" --format '{{json .}}' 2>/dev/null)" \
      || { selfhost_proxy_refuse 'seeded network inspection failed'; return 1; }
    verdict="$(selfhost_proxy_network "$project" "$network" <<<"$network_json")" \
      || { selfhost_proxy_refuse 'seeded network provenance refused'; return 1; }
    IFS=$'\t' read -r route pinned members <<<"$verdict"
    selfhost_proxy_members "$project" "$members" \
      || { selfhost_proxy_refuse 'materialized network membership refused'; return 1; }
  fi
  original_network_id="$network_ids"
  original_route="$route"
  if [[ "$pinned" == empty ]]; then
    # A stopped container does not hold a Docker network. Start only the pinned
    # NATS service; no application image is built, created, or started here.
    selfhost_proxy_compose up -d --no-deps --no-build --pull missing --wait --wait-timeout 180 nats >/dev/null \
      || { selfhost_proxy_refuse 'NATS network holder failed'; return 1; }
    network_ids="$(docker network ls --filter "label=com.docker.compose.project=$project" \
      --filter label=com.docker.compose.network=concordvoice-services --format '{{.ID}}' 2>/dev/null)" \
      || { selfhost_proxy_refuse 'NATS-held network unavailable'; return 1; }
    [[ "$network_ids" == "$original_network_id" ]] \
      || { selfhost_proxy_refuse 'services network changed while starting NATS'; return 1; }
    network_json="$(docker network inspect "$network_ids" --format '{{json .}}' 2>/dev/null)" \
      || { selfhost_proxy_refuse 'NATS-held network inspection failed'; return 1; }
    verdict="$(selfhost_proxy_network "$project" "$network" <<<"$network_json")" \
      || { selfhost_proxy_refuse 'NATS-held network provenance refused'; return 1; }
    IFS=$'\t' read -r route pinned members <<<"$verdict"
    selfhost_proxy_members "$project" "$members" \
      || { selfhost_proxy_refuse 'NATS-held network membership refused'; return 1; }
    [[ "$route" == "$original_route" && "$pinned" == pinned ]] \
      || { selfhost_proxy_refuse 'NATS did not hold the measured bridge'; return 1; }
  fi
  SELFHOST_PROXY_NETWORK_ID="$network_ids"
  selfhost_proxy_write_env "$CONFIG_ROOT/.env" "$route" \
    || { selfhost_proxy_refuse 'proxy environment update refused'; return 1; }
  SELFHOST_TRUSTED_PROXY_CIDRS="$route"
  SELFHOST_TRUSTED_PROXIES="$route"
  TRUSTED_PROXY_CIDRS="$route"
  TRUSTED_PROXIES="$route"
  export TRUSTED_PROXY_CIDRS TRUSTED_PROXIES
  model="$(selfhost_proxy_compose config --format json 2>/dev/null)" \
    || { selfhost_proxy_refuse 'updated Compose model unavailable'; return 1; }
  python3 -c '
import json, sys
route = sys.argv[1]
model = json.load(sys.stdin)
if (model["services"]["control-plane"]["environment"]["TRUSTED_PROXY_CIDRS"] != route
    or model["services"]["media-plane"]["environment"]["TRUSTED_PROXIES"] != route):
    sys.exit(1)
' "$route" <<<"$model" \
    || { selfhost_proxy_refuse 'updated Compose trust model refused'; return 1; }
}

selfhost_proxy_live_check() {
  local model identity project network service id labels endpoints container_env network_ids network_json verdict route pinned members
  [[ -n "${SELFHOST_TRUSTED_PROXY_CIDRS:-}" \
     && -n "${SELFHOST_PROXY_NETWORK_ID:-}" \
     && "$SELFHOST_TRUSTED_PROXY_CIDRS" == "${SELFHOST_TRUSTED_PROXIES:-}" ]] \
    || { selfhost_proxy_refuse 'proxy trust was not prepared'; return 1; }
  model="$(selfhost_proxy_compose config --format json 2>/dev/null)" \
    || { selfhost_proxy_refuse 'live Compose model unavailable'; return 1; }
  identity="$(selfhost_proxy_model <<<"$model")" \
    || { selfhost_proxy_refuse 'live gateway model refused'; return 1; }
  python3 -c '
import json, sys
try:
    model = json.load(sys.stdin)
    route = sys.argv[1]
    if (model["services"]["control-plane"]["environment"]["TRUSTED_PROXY_CIDRS"] != route
        or model["services"]["media-plane"]["environment"]["TRUSTED_PROXIES"] != route):
        raise ValueError()
except (KeyError, TypeError, ValueError, json.JSONDecodeError):
    sys.exit(1)
' "$SELFHOST_TRUSTED_PROXY_CIDRS" <<<"$model" \
    || { selfhost_proxy_refuse 'live Compose proxy trust differs from persisted route'; return 1; }
  IFS=$'\t' read -r project network <<<"$identity"
  network_ids="$(docker network ls --filter "label=com.docker.compose.project=$project" \
    --filter label=com.docker.compose.network=concordvoice-services --format '{{.ID}}' 2>/dev/null)" \
    || { selfhost_proxy_refuse 'live services network unavailable'; return 1; }
  [[ -n "$network_ids" && "$network_ids" != *$'\n'* ]] \
    || { selfhost_proxy_refuse 'live services network ambiguous'; return 1; }
  [[ "$network_ids" == "$SELFHOST_PROXY_NETWORK_ID" ]] \
    || { selfhost_proxy_refuse 'services network was recreated after proxy measurement'; return 1; }
  network_json="$(docker network inspect "$network_ids" --format '{{json .}}' 2>/dev/null)" \
    || { selfhost_proxy_refuse 'live services network inspection failed'; return 1; }
  verdict="$(selfhost_proxy_network "$project" "$network" <<<"$network_json")" \
    || { selfhost_proxy_refuse 'live services network provenance refused'; return 1; }
  IFS=$'\t' read -r route pinned members <<<"$verdict"
  selfhost_proxy_members "$project" "$members" \
    || { selfhost_proxy_refuse 'live services network membership refused'; return 1; }
  [[ "$route" == "$SELFHOST_TRUSTED_PROXY_CIDRS" && "$pinned" == pinned ]] \
    || { selfhost_proxy_refuse 'live services network gateway changed'; return 1; }
  for service in control-plane media-plane; do
    id="$(selfhost_proxy_compose ps -q -- "$service" 2>/dev/null)" \
      || { selfhost_proxy_refuse 'application container inventory unavailable'; return 1; }
    [[ -n "$id" && "$id" != *$'\n'* ]] \
      || { selfhost_proxy_refuse 'application container inventory ambiguous'; return 1; }
    labels="$(docker inspect "$id" --format '{{json .Config.Labels}}' 2>/dev/null)" \
      || { selfhost_proxy_refuse 'application container labels unavailable'; return 1; }
    endpoints="$(docker inspect "$id" --format '{{json .NetworkSettings.Networks}}' 2>/dev/null)" \
      || { selfhost_proxy_refuse 'application endpoint unavailable'; return 1; }
    container_env="$(docker inspect "$id" --format '{{json .Config.Env}}' 2>/dev/null)" \
      || { selfhost_proxy_refuse 'application proxy environment unavailable'; return 1; }
    if ! python3 - "$project" "$service" "$network" "$SELFHOST_TRUSTED_PROXY_CIDRS" \
        "$labels" "$endpoints" "$container_env" <<'PY'; then
import ipaddress, json, sys
try:
    project, service, network, route, labels_raw, endpoints_raw, env_raw = sys.argv[1:]
    labels, endpoints, environment = json.loads(labels_raw), json.loads(endpoints_raw), json.loads(env_raw)
    if (labels["com.docker.compose.project"] != project
        or labels["com.docker.compose.service"] != service):
        raise ValueError()
    key = "TRUSTED_PROXY_CIDRS" if service == "control-plane" else "TRUSTED_PROXIES"
    if not isinstance(environment, list) or environment.count(key + "=" + route) != 1 \
       or any(isinstance(item, str) and item.startswith(key + "=") and item != key + "=" + route
              for item in environment):
        raise ValueError()
    selected = endpoints[network]
    if selected["GwPriority"] != 1 or selected["Gateway"] != str(ipaddress.ip_network(route).network_address):
        raise ValueError()
    if any(type(settings.get("GwPriority", 0)) is not int or settings.get("GwPriority", 0) >= 1
           for name, settings in endpoints.items() if name != network):
        raise ValueError()
except (KeyError, TypeError, ValueError, json.JSONDecodeError):
    sys.exit(1)
PY
      selfhost_proxy_refuse 'application gateway differs from measured proxy trust'
      return 1
    fi
  done
}

selfhost_proxy_restart_preflight() {
  local route model identity project network network_ids network_json verdict observed pinned members
  route="$(selfhost_proxy_exact_env "$CONFIG_ROOT/.env")" \
    || { selfhost_proxy_refuse 'restart requires exact persisted proxy trust; use concord-selfhost up'; return 1; }
  model="$(selfhost_proxy_compose config --format json 2>/dev/null)" \
    || { selfhost_proxy_refuse 'restart Compose model unavailable'; return 1; }
  identity="$(selfhost_proxy_model <<<"$model")" \
    || { selfhost_proxy_refuse 'restart gateway model refused'; return 1; }
  IFS=$'\t' read -r project network <<<"$identity"
  network_ids="$(docker network ls --filter "label=com.docker.compose.project=$project" \
    --filter label=com.docker.compose.network=concordvoice-services --format '{{.ID}}' 2>/dev/null)" \
    || { selfhost_proxy_refuse 'restart services network unavailable'; return 1; }
  [[ -n "$network_ids" && "$network_ids" != *$'\n'* ]] \
    || { selfhost_proxy_refuse 'restart services network ambiguous'; return 1; }
  network_json="$(docker network inspect "$network_ids" --format '{{json .}}' 2>/dev/null)" \
    || { selfhost_proxy_refuse 'restart services network inspection failed'; return 1; }
  verdict="$(selfhost_proxy_network "$project" "$network" <<<"$network_json")" \
    || { selfhost_proxy_refuse 'restart services network provenance refused'; return 1; }
  IFS=$'\t' read -r observed pinned members <<<"$verdict"
  selfhost_proxy_members "$project" "$members" \
    || { selfhost_proxy_refuse 'restart services network membership refused'; return 1; }
  [[ "$observed" == "$route" && "$pinned" == pinned ]] \
    || { selfhost_proxy_refuse 'restart proxy route differs from live bridge; use concord-selfhost up'; return 1; }
  SELFHOST_PROXY_NETWORK_ID="$network_ids"
  SELFHOST_TRUSTED_PROXY_CIDRS="$route"
  SELFHOST_TRUSTED_PROXIES="$route"
  selfhost_proxy_live_check
}
