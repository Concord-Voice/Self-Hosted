#!/usr/bin/env python3
"""Root-orchestrated #2504 selected installer-to-coturn acceptance fixture.

The host Docker CLI is used only for this test's owned project, image, volume,
and one explicitly named coturn container. Provisioning runs in a disposable
root utility container with no network or Docker socket. Its narrow Docker
adapter can describe only that consumer and writes a restart request marker;
this host runner performs a restart only for the captured owned container ID.
"""
from __future__ import annotations

import hashlib
import importlib.util
import ast
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
import uuid


ROOT = Path(__file__).resolve().parents[3]
DEPLOY = ROOT / "infrastructure/deploy"
TESTS = DEPLOY / "tests"
DOMAIN = "example.com"
# Documentation-only advertised address; TLS acceptance binds and connects via loopback.
FIXTURE_PUBLIC_IP = "203.0.113.7"
CANONICAL_IMAGE = "coturn/coturn:4.18.0-alpine@sha256:c29135c08565810d37053f384584392b7c24b69570fc81a721b878d6fe63e04e"
CONTAINER_NAME = "concordvoice-coturn"
IMAGE = os.environ.get("SELFHOST_CERT_TEST_COTURN_IMAGE", CANONICAL_IMAGE)
TOOL_IMAGE = os.environ.get("SELFHOST_CERT_TEST_IMAGE", "ubuntu:24.04")
DOCKER = os.environ.get("DOCKER_BIN", "docker")
INSPECT_TEMPLATE = r'''{{printf "{\"Name\":%q,\"Image\":%q,\"Project\":%q,\"Service\":%q,\"Status\":%q,\"Running\":%t,\"Paused\":%t,\"Restarting\":%t}" .Name .Config.Image (index .Config.Labels "com.docker.compose.project") (index .Config.Labels "com.docker.compose.service") .State.Status .State.Running .State.Paused .State.Restarting}}'''
SELECTED_PS_TEMPLATE = r'''{"ID":{{json .ID}},"Names":{{json .Names}},"Labels":{"com.docker.compose.project":{{json (.Label "com.docker.compose.project")}},"com.docker.compose.service":{{json (.Label "com.docker.compose.service")}}}}'''
OBSERVER_PROBE = r'''
import importlib.util, sys
spec = importlib.util.spec_from_file_location("selfhost_tls_observer", sys.argv[1])
if spec is None or spec.loader is None:
    raise SystemExit(2)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
try:
    module.observe_tls(sys.argv[2], sys.argv[3], int(sys.argv[4]), float(sys.argv[5]))
except module.TLSRefusalError as error:
    if str(error) in (module.HANDSHAKE_REFUSAL, module.LEAF_MISMATCH_REFUSAL):
        raise SystemExit(3)
    raise SystemExit(2)
except Exception:
    raise SystemExit(2)
'''
ast.parse(OBSERVER_PROBE, filename="<selected-consumer-observer>")


class AcceptanceFailure(Exception):
    pass


def require(condition: bool, reason: str) -> None:
    if not condition:
        raise AcceptanceFailure(reason)


def run(argv: list[str], *, input_text: str | None = None,
        env: dict[str, str] | None = None, timeout: int = 180) -> str:
    try:
        result = subprocess.run(argv, input=input_text, text=True,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                env=env, timeout=timeout, check=False)
    except Exception:
        raise AcceptanceFailure("required acceptance command could not run") from None
    if result.returncode != 0:
        raise AcceptanceFailure("required acceptance command failed")
    return result.stdout


def docker(*args: str, **kwargs) -> str:
    return run([DOCKER, *args], **kwargs)


def checked_image(image: str) -> tuple[str, str]:
    raw = docker("image", "inspect", "--format", "{{.Id}}", image)
    ident = raw.strip()
    require(re.fullmatch(r"sha256:[0-9a-f]{64}", ident) is not None,
            "required image identity was not available")
    user = docker("run", "--rm", "--network", "none", "--security-opt",
                  "no-new-privileges", "--pids-limit", "64", "--entrypoint",
                  "id", image, "-u").strip()
    group = docker("run", "--rm", "--network", "none", "--security-opt",
                   "no-new-privileges", "--pids-limit", "64", "--entrypoint",
                   "id", image, "-g").strip()
    return ident, f"{user}:{group}"


def write_env(installer: Path, destination: Path, storage_mode: str) -> None:
    script = r'''
set -euo pipefail
source "$INSTALLER_SOURCE"
ENV_OUT="$1"
CFG_DOMAIN="$2"
CFG_TLS_MODE=local
CFG_TURN_REALM="$2"
CFG_PUBLIC_IP="$FIXTURE_PUBLIC_IP"
CFG_SMTP_HOST=mail.example.invalid
CFG_SMTP_PORT=587
CFG_SMTP_USER=fixture
CFG_SMTP_PASS=fixture-password
CFG_SMTP_FROM=ops@example.invalid
CFG_TRUSTED_CIDRS=127.0.0.1/32
CFG_FEEDBACK_ENABLED=no
CFG_ACTIVITY_HISTORY_OPERATOR_NAME=Test
CFG_ACTIVITY_HISTORY_PRIVACY_POLICY_URL=https://example.invalid/privacy
CFG_STORAGE_MODE="$3"
if [[ "$3" == byo-s3 ]]; then
  CFG_STORAGE_BACKEND=s3
  CFG_STORAGE_ENDPOINT=storage.example.invalid
  CFG_STORAGE_REGION=test-region
  CFG_STORAGE_ACCESS_KEY=fixture-access-key
  CFG_STORAGE_SECRET_KEY=fixture-secret-key
  CFG_STORAGE_USE_SSL=true
  CFG_STORAGE_BUCKET=fixture-bucket
fi
write_env_file >/dev/null
'''
    env = dict(os.environ)
    env["INSTALLER_SOURCE"] = str(installer)
    env["FIXTURE_PUBLIC_IP"] = FIXTURE_PUBLIC_IP
    run(["bash", "-c", script, "installer-writer", str(destination), DOMAIN,
         storage_mode], env=env)
    mode = destination.stat().st_mode & 0o777
    require(mode == 0o600, "installer-generated environment mode was not 0600")
    selector_lines = [line for line in destination.read_text(encoding="utf-8").splitlines()
                      if line.startswith("COTURN_CERTS_DIR=")]
    require(selector_lines == ['COTURN_CERTS_DIR="/opt/concord/certs/coturn"'],
            "installer did not publish exactly one canonical coturn selector")


def resolved_compose_args(project: Path) -> tuple[str, list[str]]:
    script = r'''
set -euo pipefail
source "$STORAGE_SOURCE"
selfhost_storage_resolve "$1"
printf '%s\0' "$SELFHOST_STORAGE_PROJECT_NAME"
printf '%s\0' "${SELFHOST_STORAGE_COMPOSE_ARGS[@]}"
'''
    env = dict(os.environ)
    env["STORAGE_SOURCE"] = str(DEPLOY / "selfhost-storage.sh")
    raw = run(["bash", "-c", script, "storage-resolver", str(project)],
              env=env, timeout=240)
    items = raw.split("\0")
    require(items[-1] == "", "storage resolver output shape was invalid")
    return items[0], items[1:-1]


def render_selected(project: Path, storage_mode: str) -> tuple[dict, list[str]]:
    name, compose_args = resolved_compose_args(project)
    expected_files = ["docker-compose.yml", "docker-compose.production.yml",
                      "docker-compose.selfhost.yml"]
    if storage_mode == "byo-s3":
        expected_files.append("docker-compose.byo-s3.yml")
    actual_files = [Path(compose_args[i + 1]).name for i, value in enumerate(compose_args[:-1])
                    if value == "-f"]
    require(actual_files == expected_files,
            "installer-selected Compose file order did not match storage mode")
    require(compose_args.count("--profile") == 1 and
            compose_args[compose_args.index("--profile") + 1] == "services",
            "installer-selected services profile was not preserved")
    environment = dict(os.environ)
    for key in tuple(environment):
        if key.startswith("COMPOSE_") or key in {
            "COTURN_CERTS_DIR", "SELFHOST_STORAGE_MODE", "DOMAIN", "PUBLIC_IP",
            "TURN_SECRET", "TURN_REALM", "STORAGE_BACKEND", "STORAGE_ENDPOINT",
            "STORAGE_REGION", "STORAGE_ACCESS_KEY", "STORAGE_SECRET_KEY",
            "STORAGE_USE_SSL", "STORAGE_BUCKET",
        }:
            environment.pop(key, None)
    rendered = docker("compose", *compose_args, "config", "--format", "json",
                      env=environment, timeout=240)
    try:
        model = json.loads(rendered)
    except (ValueError, TypeError):
        raise AcceptanceFailure("selected Compose render was not valid JSON") from None
    require(model.get("name") == name, "selected Compose project identity changed")
    return model, compose_args


ADAPTER = r'''#!/usr/bin/env bash
set -euo pipefail
fail() { exit 97; }
[[ -n "${OWNED_CONSUMER_ID:-}" ]] || fail
if [[ "${1:-}" == ps ]]; then
  shift
  all=0; format=""; project_filter=""; service_filter=""; name_filter=""
  while (($#)); do
    case "$1" in
      --all) [[ "$all" == 0 ]] || fail; all=1; shift ;;
      --filter)
        [[ $# -ge 2 ]] || fail
        filter="$2"; shift 2
        case "$filter" in
          label=com.docker.compose.project=*)
            [[ -z "$project_filter" ]] || fail
            project_filter="${filter#label=com.docker.compose.project=}" ;;
          label=com.docker.compose.service=*)
            [[ -z "$service_filter" ]] || fail
            service_filter="${filter#label=com.docker.compose.service=}" ;;
          name=*) [[ -z "$name_filter" ]] || fail; name_filter="${filter#name=}" ;;
          *) fail ;;
        esac ;;
      --format) [[ $# -ge 2 && -z "$format" ]] || fail; format="$2"; shift 2 ;;
      *) fail ;;
    esac
  done
  [[ "$all" == 1 && "$format" == "$EXPECTED_PS_TEMPLATE" && -n "$service_filter" ]] || fail
  # Docker returns an empty result for a valid filter that does not match the
  # actual owned labels/name. In particular, a stale project filter is absent.
  [[ "$service_filter" == coturn &&
     ( -z "$project_filter" || "$project_filter" == "${OWNED_PROJECT_NAME:-}" ) &&
     ( -z "$name_filter" || "$CONTAINER_NAME" =~ $name_filter ) ]] || exit 0
  [[ "$OWNED_CONSUMER_ID" == bootstrap-absent ]] && exit 0
  [[ "${OWNED_PROJECT_NAME:-}" =~ ^concord-2504-selected-[0-9]+$ &&
     "${OWNED_CONTAINER_NAME:-}" == "$CONTAINER_NAME" ]] || fail
  printf '{"ID":"%s","Names":"%s","Labels":{"com.docker.compose.project":"%s","com.docker.compose.service":"coturn"}}\n' \
    "$OWNED_CONSUMER_ID" "$CONTAINER_NAME" "$OWNED_PROJECT_NAME"
  exit 0
fi
if [[ $# -eq 4 && "$1" == inspect && "$2" == --format &&
      "$3" == "$EXPECTED_INSPECT_TEMPLATE" && "$4" == "$OWNED_CONSUMER_ID" ]]; then
  [[ "${OWNED_PROJECT_NAME:-}" =~ ^concord-2504-selected-[0-9]+$ &&
     "${OWNED_CONTAINER_NAME:-}" == "$CONTAINER_NAME" ]] || fail
  printf '{"Name":"/%s","Image":"coturn/coturn:4.18.0-alpine@sha256:c29135c08565810d37053f384584392b7c24b69570fc81a721b878d6fe63e04e","Project":"%s","Service":"coturn","Status":"running","Running":true,"Paused":false,"Restarting":false}\n' \
    "$CONTAINER_NAME" "$OWNED_PROJECT_NAME"
  exit 0
fi
if [[ $# -eq 2 && "$1" == restart && "$2" == "$OWNED_CONSUMER_ID" ]]; then
  [[ "${OWNED_PROJECT_NAME:-}" =~ ^concord-2504-selected-[0-9]+$ &&
     "${OWNED_CONTAINER_NAME:-}" == "$CONTAINER_NAME" ]] || fail
  printf '%s\t%s\n' "$OWNED_CONSUMER_ID" "$CONTAINER_NAME" > /state/restart-request
  chmod 0644 /state/restart-request
  exit 0
fi
fail
'''

SYSTEMCTL = r'''#!/usr/bin/env bash
[[ $# -eq 3 && "$1" == is-active && "$2" == --quiet && "$3" == nginx ]] && exit 1
exit 97
'''

CERTBOT = r'''#!/usr/bin/env bash
set -euo pipefail
domains=()
while (($#)); do
  if [[ "$1" == -d && $# -ge 2 ]]; then domains+=("$2"); shift 2; else shift; fi
done
[[ ${#domains[@]} -eq 4 && "${domains[0]}" == "$TEST_DOMAIN" &&
   "${domains[1]}" == "api.$TEST_DOMAIN" &&
   "${domains[2]}" == "media.$TEST_DOMAIN" &&
   "${domains[3]}" == "turn.$TEST_DOMAIN" ]] || exit 41
domain="${domains[0]}"
target="$LE_LIVE_ROOT/$domain"
install -d -m 0700 "$target"
openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 30 \
  -subj "/CN=$domain" \
  -addext "subjectAltName=DNS:$domain,DNS:api.$domain,DNS:media.$domain,DNS:turn.$domain" \
  -keyout "$target/privkey.pem" -out "$target/fullchain.pem" >/dev/null 2>&1
chmod 600 "$target/privkey.pem"
chmod 644 "$target/fullchain.pem"
'''

PROVISION = r'''#!/usr/bin/env bash
set -euo pipefail
export PATH=/fixture/stubs:/usr/sbin:/usr/bin:/sbin:/bin
export LE_LIVE_ROOT=/tmp/2504-test-source/live
export HOOK_DIR=/tmp/2504-test-hooks
export COPY_CERTS_HOOK=/fixture/copy-certs.sh
export COTURN_CERT_ROOT=/opt/concord/certs
install -d -o root -g root -m 0700 /tmp/2504-test-source
mode="$1"
case "$mode" in
  local)
    bash /fixture/provision-cert.sh local --host "$TEST_DOMAIN" --ip 127.0.0.1 --days 30
    ;;
  import)
    mkdir -m 0700 /tmp/import-input
    openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 30 \
      -subj "/CN=fixture-private-ca" \
      -addext "basicConstraints=critical,CA:TRUE" \
      -addext "keyUsage=critical,keyCertSign,cRLSign" \
      -keyout /tmp/import-input/ca-key.pem -out /tmp/import-input/ca.pem >/dev/null 2>&1
    openssl req -new -newkey rsa:2048 -sha256 -nodes \
      -subj "/CN=$TEST_DOMAIN" \
      -keyout /tmp/import-input/key.pem -out /tmp/import-input/leaf.csr >/dev/null 2>&1
    printf 'subjectAltName=DNS:%s,DNS:api.%s,DNS:media.%s,DNS:turn.%s\nbasicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\n' \
      "$TEST_DOMAIN" "$TEST_DOMAIN" "$TEST_DOMAIN" "$TEST_DOMAIN" >/tmp/import-input/leaf.ext
    openssl x509 -req -sha256 -days 30 -in /tmp/import-input/leaf.csr \
      -CA /tmp/import-input/ca.pem -CAkey /tmp/import-input/ca-key.pem \
      -CAcreateserial -extfile /tmp/import-input/leaf.ext \
      -out /tmp/import-input/leaf.pem >/dev/null 2>&1
    cat /tmp/import-input/leaf.pem /tmp/import-input/ca.pem >/tmp/import-input/fullchain.pem
    chmod 600 /tmp/import-input/key.pem
    chmod 644 /tmp/import-input/fullchain.pem
    bash /fixture/provision-cert.sh import --cert /tmp/import-input/fullchain.pem \
      --key /tmp/import-input/key.pem --host "$TEST_DOMAIN"
    ;;
  letsencrypt)
    bash /fixture/provision-cert.sh letsencrypt --domain "$TEST_DOMAIN" --email test@example.invalid
    ;;
  *) exit 42 ;;
esac >/dev/null 2>&1
source_cert="$LE_LIVE_ROOT/$TEST_DOMAIN/fullchain.pem"
[[ -s "$source_cert" ]]
sha256sum "$source_cert" | cut -d' ' -f1
'''


def observe(module_path: Path, cert_path: Path, port: int, budget: float,
            should_succeed: bool) -> None:
    result = subprocess.run([sys.executable, "-c", OBSERVER_PROBE, str(module_path),
                             str(cert_path), DOMAIN, str(port), str(budget)],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                            check=False, timeout=budget + 5)
    expected = 0 if should_succeed else 3
    require(result.returncode == expected,
            "bounded selected consumer TLS observation had the wrong result")


def validate_projection(model: dict, project: Path) -> tuple[list[str], str, str]:
    service = model.get("services", {}).get("coturn")
    require(isinstance(service, dict), "selected Compose model omitted coturn")
    require(service.get("container_name") == CONTAINER_NAME,
            "selected coturn canonical container name changed")
    require(service.get("image") == CANONICAL_IMAGE,
            "production coturn image contract changed")
    command_value = service.get("command")
    if isinstance(command_value, str):
        command = shlex.split(command_value)
    elif isinstance(command_value, list) and all(isinstance(x, str) for x in command_value):
        command = command_value
    else:
        raise AcceptanceFailure("selected coturn command shape was invalid")
    require(command and command[0] == "turnserver", "selected coturn command changed")
    command = command[1:]
    require(not any(arg == "--cli" or arg.startswith("--cli=") for arg in command),
            "selected coturn command enabled CLI")
    require("--cert=/etc/coturn/certs/cert.pem" in command and
            "--pkey=/etc/coturn/certs/key.pem" in command,
            "selected coturn command did not use its selected certificate mount")
    mounts = service.get("volumes")
    require(isinstance(mounts, list), "selected coturn mount model was invalid")
    cert_mounts = [m for m in mounts if isinstance(m, dict) and
                   m.get("target") == "/etc/coturn/certs"]
    config_mounts = [m for m in mounts if isinstance(m, dict) and
                     m.get("target") == "/etc/coturn/turnserver.conf"]
    require(len(cert_mounts) == len(config_mounts) == 1,
            "selected coturn mount set was ambiguous")
    cert_mount, config_mount = cert_mounts[0], config_mounts[0]
    require(cert_mount.get("type") == "bind" and cert_mount.get("source") ==
            "/opt/concord/certs/coturn" and cert_mount.get("read_only") is True,
            "selected coturn certificate mapping was not canonical and read-only")
    source = config_mount.get("source")
    require(config_mount.get("type") == "bind" and
            config_mount.get("read_only") is True and isinstance(source, str) and
            source.endswith("/infrastructure/docker/coturn/turnserver.conf"),
            "selected coturn configuration mapping was not the public canonical file")
    ports = service.get("ports")
    require(isinstance(ports, list), "selected coturn publication model was invalid")
    tls_ports = [p for p in ports if isinstance(p, dict) and
                 p.get("target") == 5349 and p.get("protocol", "tcp") == "tcp"]
    require(len(tls_ports) == 1, "selected coturn TLS publication was ambiguous")
    return command, str(source), str(tls_ports[0].get("published", ""))


def make_project(work: Path, storage_mode: str, installer: Path) -> Path:
    parent = work / storage_mode
    parent.mkdir(mode=0o700)
    project = parent / "concordvoice"
    project.mkdir(mode=0o700)
    shutil.copyfile(ROOT / ".gitignore", project / ".gitignore")
    # Compose's control-plane model requires relative spa.env to exist. Keep
    # this runtime metadata synthetic and empty; never copy a host credential.
    (project / "spa.env").write_text("", encoding="utf-8")
    os.chmod(project / "spa.env", 0o600)
    git_environment = {key: value for key, value in os.environ.items()
                       if not key.startswith("GIT_")}
    run(["git", "init", "--quiet", str(project)], env=git_environment)
    for name in ("docker-compose.yml", "docker-compose.production.yml",
                 "docker-compose.selfhost.yml", "docker-compose.byo-s3.yml"):
        shutil.copyfile(ROOT / name, project / name)
        os.chmod(project / name, 0o600)
    config_dir = project / "infrastructure/docker/coturn"
    config_dir.mkdir(parents=True, mode=0o700)
    shutil.copyfile(DEPLOY.parent / "docker/coturn/turnserver.conf",
                    config_dir / "turnserver.conf")
    os.chmod(config_dir / "turnserver.conf", 0o600)
    env_path = project / ".env"
    write_env(installer, env_path, storage_mode)
    return project


def stage_public(work: Path) -> Path:
    stage = work / "public"
    stage.mkdir(mode=0o700)
    stubs = stage / "stubs"
    stubs.mkdir(mode=0o700)
    for source, name in ((DEPLOY / "provision-cert.sh", "provision-cert.sh"),
                         (DEPLOY / "copy-certs.sh", "copy-certs.sh"),
                         (DEPLOY.parent / "docker/coturn/turnserver.conf", "turnserver.conf")):
        shutil.copyfile(source, stage / name)
        os.chmod(stage / name, 0o644 if name == "turnserver.conf" else 0o500)
    for name, content in (("docker", ADAPTER), ("systemctl", SYSTEMCTL),
                          ("certbot", CERTBOT), ("provision-fixture", PROVISION)):
        target = stubs / name if name in {"docker", "systemctl", "certbot"} else stage / name
        target.write_text(content, encoding="utf-8")
        os.chmod(target, 0o500)
    return stage


def run_provisioner_digest(stage: Path, cert_volume: str, state: Path,
                           mode: str, owned_id: str | None,
                           owned_project: str, owned_name: str) -> str:
    # Same isolated command as run_provisioner, with a narrow digest-only result.
    state.mkdir(mode=0o700, exist_ok=True)
    marker = state / "restart-request"
    marker.write_text("", encoding="utf-8")
    args = ["run", "--rm", "--network", "none", "--user", "0:0",
            "--security-opt", "no-new-privileges", "--pids-limit", "128",
            "--tmpfs", "/tmp:rw,nosuid,nodev,size=64m",
            "--volume", f"{stage}:/fixture:ro",
            "--volume", f"{cert_volume}:/opt/concord/certs:rw",
            "--volume", f"{state}:/state:rw",
            "--env", f"TEST_DOMAIN={DOMAIN}",
            "--env", f"OWNED_CONSUMER_ID={owned_id or 'bootstrap-absent'}",
            "--env", f"OWNED_PROJECT_NAME={owned_project}",
            "--env", f"OWNED_CONTAINER_NAME={owned_name}",
            "--env", f"CONTAINER_NAME={CONTAINER_NAME}",
            "--env", f"EXPECTED_INSPECT_TEMPLATE={INSPECT_TEMPLATE}",
            "--env", f"EXPECTED_PS_TEMPLATE={SELECTED_PS_TEMPLATE}",
            TOOL_IMAGE, "bash", "/fixture/provision-fixture", mode]
    digest = docker(*args, timeout=240).strip()
    require(re.fullmatch(r"[0-9a-f]{64}", digest) is not None,
            "provisioner source certificate digest was unavailable")
    return digest


def start_consumer(project_model: dict, project: Path, cert_volume: str,
                   stage: Path, image_id: str, command: list[str],
                   config_source: str, published: str, work: Path) -> tuple[str, int]:
    project_name = f"concord-2504-selected-{os.getpid()}"
    # Use a collision-free physical name in Docker. The adapter's explicit
    # canonical-name projection represents the selected Compose service name.
    ident = f"{project_name}-coturn"
    config_stage = stage / "turnserver.conf"
    require(config_stage.is_file(), "staged coturn config was unavailable")
    argv = ["run", "--detach", "--rm", "--name", ident,
            "--label", f"com.docker.compose.project={project_name}",
            "--label", "com.docker.compose.service=coturn",
            "--security-opt", "no-new-privileges", "--pids-limit", "128",
            "--publish", "127.0.0.1::5349/tcp",
            "--mount", f"type=volume,src={cert_volume},dst=/etc/coturn/certs,volume-subpath=coturn,readonly",
            "--volume", f"{config_stage}:/etc/coturn/turnserver.conf:ro",
            "--entrypoint", "turnserver", IMAGE, *command]
    container_id = ""
    try:
        container_id = docker(*argv).strip()
        require(re.fullmatch(r"[0-9a-f]{12,64}", container_id) is not None,
                "owned coturn consumer identity was unavailable")
        observed_image_id = docker("inspect", "--format", "{{.Image}}", container_id).strip()
        require(observed_image_id == image_id,
                "owned consumer did not use the inspected test image identity")
        mapping = docker("port", container_id, "5349/tcp").strip().splitlines()
        require(len(mapping) == 1 and mapping[0].startswith("127.0.0.1:"),
                "owned coturn TLS listener did not publish on IPv4 loopback")
        port_text = mapping[0].rsplit(":", 1)[1]
        require(port_text.isdigit() and port_text != published,
                "owned coturn TLS listener did not use a dynamic host port")
        return container_id, int(port_text)
    except Exception:
        if re.fullmatch(r"[0-9a-f]{12,64}", container_id):
            try:
                docker("rm", "--force", container_id, timeout=30)
            except AcceptanceFailure:
                pass
        raise


def metadata(cert_volume: str) -> None:
    # Inspect inside Docker, where the hook and coturn observe volume ownership.
    result = docker("run", "--rm", "--network", "none", "--user", "0:0",
                    "--security-opt", "no-new-privileges", "--pids-limit", "64",
                    "--volume", f"{cert_volume}:/check:ro", TOOL_IMAGE, "stat", "-c",
                    "%F %u:%g:%a %s", "/check", "/check/coturn",
                    "/check/coturn/cert.pem", "/check/coturn/key.pem")
    actual = result.splitlines()
    expected = ["directory 0:0:711", "directory 0:0:711",
                "regular file 0:0:444", "regular file 65534:65533:400"]
    rows = [line.split() for line in actual]
    require(len(rows) == len(expected) and
            all(len(row) == len(want.split()) + 1 and
                row[:len(want.split())] == want.split()
                for row, want in zip(rows, expected)) and
            all(int(row[-1]) > 0 for row in rows[2:]),
            "fan-out artifact metadata did not match consumer namespace contract")


def public_certificate(cert_volume: str, destination: Path) -> Path:
    certificate = docker("run", "--rm", "--network", "none", "--security-opt",
                         "no-new-privileges", "--pids-limit", "64", "--volume",
                         f"{cert_volume}:/certs:ro", TOOL_IMAGE, "cat",
                         "/certs/coturn/cert.pem")
    destination.write_text(certificate, encoding="utf-8")
    return destination


def restart_owned(container_id: str, marker: Path) -> int:
    require(marker.is_file() and marker.read_text(encoding="utf-8").strip() ==
            f"{container_id}\t{CONTAINER_NAME}",
            "copy hook did not request the one owned coturn restart")
    # The adapter cannot invoke Docker. This is the sole restart operation and its
    # target comes from the actual container ID created by this test.
    docker("restart", container_id, timeout=30)
    marker.write_text("", encoding="utf-8")
    # Docker may assign a new dynamic host port when the owned container
    # restarts. Observe the live publication, not the pre-restart mapping.
    mapping = docker("port", container_id, "5349/tcp").strip().splitlines()
    require(len(mapping) == 1 and mapping[0].startswith("127.0.0.1:"),
            "restarted coturn TLS listener did not publish on IPv4 loopback")
    port_text = mapping[0].rsplit(":", 1)[1]
    require(port_text.isdigit() and 1 <= int(port_text) <= 65535,
            "restarted coturn TLS listener had an invalid host port")
    return int(port_text)


def main() -> None:
    require(shutil.which(DOCKER) is not None, "required Docker CLI unavailable")
    docker("info", timeout=30)
    if IMAGE == CANONICAL_IMAGE:
        try:
            docker("image", "inspect", "--format", "{{.Id}}", IMAGE, timeout=30)
        except AcceptanceFailure:
            docker("pull", IMAGE, timeout=600)
    coturn_image_id, identity = checked_image(IMAGE)
    require(identity == "65534:65533", "selected coturn image identity was not 65534:65533")
    checked_image(TOOL_IMAGE)
    work = Path(tempfile.mkdtemp(prefix="concord-2504-selected-"))
    work.chmod(0o700)
    consumer_id: str | None = None
    proxy: subprocess.Popen | None = None
    cert_volume = f"concord-2504-selected-certs-{os.getpid()}-{uuid.uuid4().hex[:8]}"
    volume_created = False
    try:
        bundle_project = make_project(work, "bundled", DEPLOY / "install-selfhost.sh")
        byo_project = make_project(work, "byo-s3", DEPLOY / "install-selfhost.sh")
        bundle_model, _bundle_args = render_selected(bundle_project, "bundled")
        byo_model, byo_args = render_selected(byo_project, "byo-s3")
        command, config_source, published = validate_projection(bundle_model, bundle_project)
        byo_command, byo_config, byo_published = validate_projection(byo_model, byo_project)
        command_shape_matches = (len(command) == len(byo_command) and all(
            left == right or (left.startswith("--static-auth-secret=") and
                              right.startswith("--static-auth-secret="))
            for left, right in zip(command, byo_command)))
        require(command_shape_matches and
                config_source.endswith("/infrastructure/docker/coturn/turnserver.conf") and
                byo_config.endswith("/infrastructure/docker/coturn/turnserver.conf") and
                published == byo_published,
                "BYO selected coturn consumer projection drifted from bundled")
        require("docker-compose.byo-s3.yml" in [Path(arg).name for arg in byo_args],
                "BYO storage render omitted its fourth ordered Compose file")
        require(len([x for x in byo_args if x == "-f"]) == 4,
                "BYO storage render did not resolve four Compose files")
        require(len([x for x in _bundle_args if x == "-f"]) == 3,
                "bundled storage render did not resolve three Compose files")

        stage = stage_public(work)
        docker("volume", "create", cert_volume)
        volume_created = True
        state = work / "adapter-state"
        state.mkdir(mode=0o700)
        # The hook's fan-out is the selected runtime mount source. The product
        # selector path maps to this private test-owned fixture root.
        source_hashes: list[tuple[str, str]] = []
        runtime_project = f"concord-2504-selected-{os.getpid()}"
        runtime_name = bundle_model["services"]["coturn"]["container_name"]
        for mode in ("local", "import", "letsencrypt"):
            source_sha = run_provisioner_digest(stage, cert_volume, state, mode,
                                                consumer_id, runtime_project, runtime_name)
            source_hashes.append((mode, source_sha))
            metadata(cert_volume)
            cert_path = public_certificate(cert_volume, work / "renewed-coturn-cert.pem")
            artifact_sha = hashlib.sha256(cert_path.read_bytes()).hexdigest()
            require(artifact_sha == source_sha,
                    "selected source certificate did not match the actual fan-out artifact")
            if consumer_id is None:
                consumer_id, port = start_consumer(bundle_model, bundle_project,
                    cert_volume, stage, coturn_image_id, command, config_source,
                    published, work)
            else:
                marker = state / "restart-request"
                observe(DEPLOY / "selfhost-tls.py", cert_path,
                        port, 2.0, should_succeed=False)
                port = restart_owned(consumer_id, marker)
            if os.environ.get("SELFHOST_HOOK_MAC_BRIDGE") == "1":
                require(shutil.which("socat") is not None,
                        "required Docker Desktop loopback forwarder unavailable")
                if proxy is not None:
                    proxy.terminate()
                    proxy.wait(timeout=5)
                proxy = subprocess.Popen(
                    ["socat", f"TCP-LISTEN:{port},bind=127.0.0.1,reuseaddr,fork",
                     f"TCP:host.docker.internal:{port}"],
                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            observe(DEPLOY / "selfhost-tls.py", cert_path,
                    port, 15.0, should_succeed=True)
            if mode == "local":
                local_port = port
        require(consumer_id is not None, "owned selected coturn consumer was not created")
        print(f"selected-consumer identity mapping: runtime project={runtime_project}; container-name projection={runtime_name}; hook project={runtime_project}/coturn")
        print(f"selected-consumer image mapping: compose={CANONICAL_IMAGE}; runtime={IMAGE}; id={coturn_image_id}")
        print("selected model: bundled=3 ordered files; BYO=4 ordered files; coturn cert mount read-only")
        print("selected consumer: default UID:GID=65534:65533; dynamic IPv4 loopback TLS proof passed")
        print("certificate delivery: local, imported four-consumer SANs, deterministic Certbot; source SHA matched fan-out")
        for mode, digest in source_hashes:
            print(f"source SHA-256 ({mode}): {digest}")
    finally:
        cleanup_failure: AcceptanceFailure | None = None
        if proxy is not None:
            proxy.terminate()
            proxy.wait(timeout=5)
        if consumer_id:
            try:
                docker("rm", "--force", consumer_id, timeout=30)
            except AcceptanceFailure as failure:
                cleanup_failure = failure
        if (work / "adapter-state").exists():
            try:
                docker("run", "--rm", "--network", "none", "--user", "0:0",
                       "--security-opt", "no-new-privileges", "--pids-limit", "64",
                       "--volume", f"{work / 'adapter-state'}:/cleanup/state:rw",
                       TOOL_IMAGE, "bash", "-e", "-c",
                       "find /cleanup/state -depth -mindepth 1 -exec rm -rf -- {} +; chmod 0755 /cleanup/state")
            except AcceptanceFailure as failure:
                if cleanup_failure is None:
                    cleanup_failure = failure
        if volume_created:
            try:
                docker("volume", "rm", cert_volume, timeout=30)
            except AcceptanceFailure as failure:
                if cleanup_failure is None:
                    cleanup_failure = failure
        try:
            shutil.rmtree(work)
        except OSError:
            if cleanup_failure is None:
                cleanup_failure = AcceptanceFailure("test-owned fixture cleanup failed")
        if cleanup_failure is not None:
            raise cleanup_failure


if __name__ == "__main__":
    try:
        main()
    except AcceptanceFailure as failure:
        print(f"FAIL: {failure}", file=sys.stderr)
        raise SystemExit(1)
